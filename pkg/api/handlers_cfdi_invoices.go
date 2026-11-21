package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"softpos/pkg/auth"
	"softpos/pkg/cfdipdf"
	"softpos/pkg/fiscal"
	"softpos/pkg/kyb"
	"softpos/pkg/pac"
	"softpos/pkg/store"
)

// Invoice lifecycle (MVP-22): invoices from sales (amounts from the posted
// split, never from the client), stamping through the PAC port, PDF / XML,
// cancellation (motivos 01-04) with receptor acceptance polling, and the
// automatic factura global. See docs/CFDI.md.

const (
	serieIndividual   = "K"
	serieGlobal       = "G"
	maxInvoiceSales   = 100
	stampLease        = 2 * time.Minute
	resealAfter       = 70 * time.Hour // SAT: Fecha within 72 h of the stamp
	defaultInvoicePg  = 50
	maxInvoicePg      = 100
	globalSalesPage   = 1000
	maxGlobalConcepts = 5000 // conceptos per factura global part
)

// SetInvoiceRepository replaces the in-memory invoice store.
func (s *Server) SetInvoiceRepository(repo store.InvoiceRepository) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.invoices = repo
}

func (s *Server) invoiceRepo() store.InvoiceRepository {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.invoices
}

func (s *Server) pacClient() pac.PACClient {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.pacOrchestrator
}

func newInvoiceID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return "inv_" + hex.EncodeToString(b)
}

// InvoiceCancellation is the cancellation state of an invoice.
type InvoiceCancellation struct {
	Motivo           string `json:"motivo"`
	FolioSustitucion string `json:"folio_sustitucion,omitempty"`
	Status           string `json:"status"` // REQUESTED | ACCEPTED | REJECTED
	RequestedAt      string `json:"requested_at,omitempty"`
	ResolvedAt       string `json:"resolved_at,omitempty"`
}

// InvoiceResponse is the invoice shape of the API.
type InvoiceResponse struct {
	InvoiceID         string          `json:"invoice_id"`
	Kind              string          `json:"kind"` // INDIVIDUAL | GLOBAL | CREDIT_NOTE
	Status            string          `json:"status"`
	UUID              string          `json:"uuid,omitempty"`
	Serie             string          `json:"serie,omitempty"`
	Folio             string          `json:"folio,omitempty"`
	TotalCents        int64           `json:"total_cents"`
	SubtotalCents     int64           `json:"subtotal_cents"`
	IVACents          int64           `json:"iva_cents"`
	ISRRetenidoCents  int64           `json:"isr_retenido_cents"`
	Receptor          json.RawMessage `json:"receptor"`
	SaleIDs           []string        `json:"sale_ids"`
	CreatedAt         string          `json:"created_at"`
	StampedAt         string          `json:"stamped_at,omitempty"`
	Error             string          `json:"error,omitempty"`
	GlobalPeriod      string          `json:"global_period,omitempty"`
	GlobalPart        int             `json:"global_part,omitempty"` // GLOBAL: part of the period (1, 2, … when split)
	ReplacesInvoiceID string          `json:"replaces_invoice_id,omitempty"`
	// RelatedInvoiceID: the invoice a CREDIT_NOTE credits (TipoRelacion 01).
	RelatedInvoiceID string               `json:"related_invoice_id,omitempty"`
	Cancellation     *InvoiceCancellation `json:"cancellation,omitempty"`
}

func rfc3339(t *time.Time) string {
	if t == nil {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func invoiceResponse(r store.InvoiceRecord) InvoiceResponse {
	out := InvoiceResponse{InvoiceID: r.ID, Kind: r.Kind, Status: r.Status, UUID: r.UUID, Serie: r.Serie,
		TotalCents: r.TotalCents, SubtotalCents: r.SubtotalCents, IVACents: r.IVACents, ISRRetenidoCents: r.ISRRetenidoCents,
		Receptor: r.Receptor, SaleIDs: r.SaleIDs, CreatedAt: r.CreatedAt.UTC().Format(time.RFC3339), StampedAt: rfc3339(r.StampedAt),
		Error: r.ErrorCode, GlobalPeriod: r.GlobalPeriod, ReplacesInvoiceID: r.ReplacesInvoiceID, RelatedInvoiceID: r.RelatedInvoiceID}
	if r.Kind == store.InvoiceGlobal {
		out.GlobalPart = max(r.GlobalPart, 1)
	}
	if r.Folio > 0 {
		out.Folio = strconv.FormatInt(r.Folio, 10)
	}
	if out.SaleIDs == nil {
		out.SaleIDs = []string{}
	}
	if r.CancelStatus != "" {
		out.Cancellation = &InvoiceCancellation{Motivo: r.CancelMotivo, FolioSustitucion: r.CancelFolioSustitucion, Status: r.CancelStatus,
			RequestedAt: rfc3339(r.CancelRequestedAt), ResolvedAt: rfc3339(r.CancelResolvedAt)}
	}
	return out
}

// saleLine is an invoiceable sale with its amounts and c_FormaPago.
type saleLine struct {
	fiscal.InvoiceLine
	formaPago string
	gross     int64
	createdAt time.Time
}

// saleError is a per-sale rejection (code + HTTP status).
type saleError struct {
	code   string
	saleID string
	status int
}

func (e *saleError) Error() string { return e.code + " " + e.saleID }

// invoiceableLine computes a sale's invoice amounts from what was posted:
// card captures use their settlement record (base/IVA minus refunds), SPEI
// abonos and CoDi confirmations the 16 % IVA-inclusive split the ledger uses.
func (s *Server) invoiceableLine(ctx context.Context, rec store.SaleRecord) (saleLine, error) {
	l := saleLine{InvoiceLine: fiscal.InvoiceLine{SaleID: rec.ID}, createdAt: rec.CreatedAt}
	notInvoiceable := &saleError{code: "SALE_NOT_INVOICEABLE", saleID: rec.ID, status: http.StatusUnprocessableEntity}
	switch rec.Kind {
	case store.SaleCard:
		if rec.Status != "CAPTURED" && rec.Status != "PARTIALLY_REFUNDED" {
			return l, notInvoiceable
		}
		var st store.SettlementRecord
		var found bool
		if err := s.moneyStore().InTx(ctx, func(tx store.MoneyTx) error {
			var err error
			st, found, err = tx.GetSettlementForUpdate(ctx, rec.ID)
			return err
		}); err != nil {
			return l, err
		}
		if !found {
			return l, notInvoiceable
		}
		l.gross, l.BaseCents, l.IVACents = settlementInvoiceSplit(st)
		l.formaPago = "04" // tarjeta (crédito/débito not distinguished by the processor port yet)
	case store.SaleSPEI, store.SaleCoDi:
		if (rec.Kind == store.SaleSPEI && rec.Status != "RECEIVED") || (rec.Kind == store.SaleCoDi && rec.Status != "CONFIRMED") {
			return l, notInvoiceable
		}
		l.gross = rec.AmountCents
		l.BaseCents, l.IVACents = fiscal.IVAFromGross(rec.AmountCents)
		l.formaPago = "03" // transferencia electrónica de fondos
	default:
		return l, notInvoiceable
	}
	if l.BaseCents <= 0 || l.IVACents < 0 {
		return l, notInvoiceable
	}
	return l, nil
}

// settlementInvoiceSplit is the net (gross, base, IVA) of a card sale: the
// posted split minus the proportional refunds. Subtracting two rounded
// splits can leave the IVA one cent outside the SAT limits for its base
// (the PAC rejects the CFDI); then the net gross is re-split with the RESICO
// formula (fiscal.IVAFromGross), so the total still equals what was paid and
// the base differs from the ledger by at most one cent.
func settlementInvoiceSplit(st store.SettlementRecord) (gross, base, iva int64) {
	gross = st.GrossCents - st.RefundedGrossCents
	base = st.BaseCents - st.RefundedBaseCents
	iva = gross - base
	if !fiscal.IVAWithinSATLimits(base, iva) {
		base, iva = fiscal.IVAFromGross(gross)
	}
	return gross, base, iva
}

// formaPagoFor is the c_FormaPago of the largest amount (SAT rule for
// several payment means under PUE).
func formaPagoFor(lines []saleLine) string {
	best, amount := "99", int64(-1)
	for _, l := range lines {
		if l.gross > amount {
			best, amount = l.formaPago, l.gross
		}
	}
	return best
}

func requestHash(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// CreateInvoiceRequest is POST /api/v1/merchants/{id}/invoices.
type CreateInvoiceRequest struct {
	SaleIDs  []string        `json:"sale_ids"`
	Receptor fiscal.Receptor `json:"receptor"`
	// ReplacesInvoiceID (optional) issues this invoice as the substitute of a
	// STAMPED one (TipoRelacion 04), taking over its sales; then cancel the
	// old one with motivo 01 and folio_sustitucion = this UUID.
	ReplacesInvoiceID string `json:"replaces_invoice_id,omitempty"`
}

func writeInvoiceError(w http.ResponseWriter, err error) {
	var se *saleError
	var si *store.SaleInvoicedError
	var fp errFiscalProfile
	switch {
	case errors.As(err, &se):
		writeJSON(w, se.status, map[string]string{"error": se.code, "sale_id": se.saleID})
	case errors.As(err, &si):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "SALE_ALREADY_INVOICED", "sale_id": si.SaleID, "invoice_id": si.InvoiceID})
	case errors.As(err, &fp):
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "FISCAL_PROFILE_INCOMPLETE", "missing": fp.missing})
	case errors.Is(err, errCSDNotFound):
		writeError(w, http.StatusUnprocessableEntity, "CSD_NOT_FOUND")
	case errors.Is(err, errCSDExpired):
		writeError(w, http.StatusUnprocessableEntity, "CSD_EXPIRED")
	default:
		slog.Error("invoice operation failed", "error", err)
		writeError(w, http.StatusServiceUnavailable, "INVOICES_UNAVAILABLE")
	}
}

var (
	errCSDNotFound = errors.New("merchant CSD not configured")
	errCSDExpired  = errors.New("merchant CSD expired")
)

// activeSigner returns the merchant's CSD signer, refusing expired CSDs.
func (s *Server) activeSigner(merchantID string, now time.Time) (*fiscal.CSDSigner, error) {
	if _, found, err := s.merchantRepo.GetMerchantCSD(context.Background(), merchantID); err != nil {
		return nil, err
	} else if !found {
		return nil, errCSDNotFound
	}
	signer, err := s.merchantSigner(merchantID)
	if err != nil {
		return nil, err
	}
	if c := signer.Certificate(); now.After(c.NotAfter) || now.Before(c.NotBefore) {
		return nil, errCSDExpired
	}
	return signer, nil
}

// retainISR: a persona moral receptor withholds 1.25 % ISR from a RESICO
// persona física emisor (Art. 113-J LISR).
func retainISR(emisor emisorProfile, receptorRFC string) bool {
	return emisor.Regimen == "626" && len([]rune(emisor.RFC)) == 13 && len([]rune(receptorRFC)) == 12
}

// HandleCreateInvoice: POST /api/v1/merchants/{id}/invoices (owner or
// cashier; Idempotency-Key required).
func (s *Server) HandleCreateInvoice(w http.ResponseWriter, r *http.Request) {
	tenant, ok := s.requireActiveTenant(w, r, r.PathValue("id"))
	if !ok || !requireRole(w, r, auth.RoleOwner, auth.RoleCashier) {
		return
	}
	key := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	switch {
	case key == "":
		writeError(w, http.StatusBadRequest, "MISSING_IDEMPOTENCY_KEY")
		return
	case len(key) > 200:
		writeError(w, http.StatusBadRequest, "IDEMPOTENCY_KEY_TOO_LONG")
		return
	}
	var req CreateInvoiceRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON")
		return
	}
	seen := map[string]bool{}
	for i, id := range req.SaleIDs {
		req.SaleIDs[i] = strings.TrimSpace(id)
		if req.SaleIDs[i] == "" || len(req.SaleIDs[i]) > 160 || seen[req.SaleIDs[i]] {
			writeError(w, http.StatusBadRequest, "INVALID_SALE_IDS")
			return
		}
		seen[req.SaleIDs[i]] = true
	}
	if len(req.SaleIDs) == 0 || len(req.SaleIDs) > maxInvoiceSales {
		writeError(w, http.StatusBadRequest, "INVALID_SALE_IDS")
		return
	}
	rec, err := s.issueIndividualInvoice(r.Context(), tenant, key, req)
	var re *invoiceRequestError
	if errors.As(err, &re) {
		writeJSON(w, re.status, re.body)
		return
	}
	if err != nil {
		writeInvoiceError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, invoiceResponse(rec))
}

// invoiceRequestError is a request-level rejection of issueIndividualInvoice.
type invoiceRequestError struct {
	status int
	body   any
}

func (e *invoiceRequestError) Error() string { return fmt.Sprintf("invoice request rejected (%d)", e.status) }

// issueIndividualInvoice is the invoicing service behind POST …/invoices and
// the receipt's autofactura: validates the receptor, replays an idempotency
// key, computes the amounts from what was posted, enforces one active
// invoice per sale and stamps. req.SaleIDs must already be validated (1–100
// unique ids). Errors: *invoiceRequestError (HTTP status + body),
// *saleError, *store.SaleInvoicedError, errFiscalProfile, CSD errors.
func (s *Server) issueIndividualInvoice(ctx context.Context, tenant, key string, req CreateInvoiceRequest) (store.InvoiceRecord, error) {
	req.Receptor = req.Receptor.Normalize()
	if bad := fiscal.ValidateReceptor(req.Receptor); len(bad) > 0 {
		return store.InvoiceRecord{}, &invoiceRequestError{http.StatusUnprocessableEntity, map[string]any{"error": "RECEPTOR_INVALID", "fields": bad}}
	}
	reused := &invoiceRequestError{http.StatusConflict, map[string]string{"error": "IDEMPOTENCY_KEY_REUSED"}}
	repo := s.invoiceRepo()
	hash := requestHash(req)
	if existing, found, err := repo.FindInvoiceByIdempotencyKey(ctx, tenant, key); err != nil {
		return store.InvoiceRecord{}, err
	} else if found {
		if existing.RequestHash != hash {
			return store.InvoiceRecord{}, reused
		}
		return existing, nil
	}
	now := nowUTC()
	emisor, err := s.emisorProfile(ctx, tenant)
	if err != nil {
		return store.InvoiceRecord{}, err
	}
	signer, err := s.activeSigner(tenant, now)
	if err != nil {
		return store.InvoiceRecord{}, err
	}

	var replaced store.InvoiceRecord
	if req.ReplacesInvoiceID != "" {
		old, found, err := repo.GetInvoice(ctx, tenant, req.ReplacesInvoiceID)
		if err != nil {
			return store.InvoiceRecord{}, err
		}
		covered := map[string]bool{}
		for _, id := range old.SaleIDs {
			covered[id] = true
		}
		valid := found && old.Status == store.InvoiceStamped && old.Kind == store.InvoiceIndividual
		for _, id := range req.SaleIDs {
			valid = valid && covered[id]
		}
		if !valid {
			return store.InvoiceRecord{}, &invoiceRequestError{http.StatusUnprocessableEntity, map[string]string{"error": "REPLACEMENT_INVALID",
				"detail": "replaces_invoice_id must be a STAMPED individual invoice covering every sale_id"}}
		}
		replaced = old
	}

	sales, ok := s.salesRepo()
	if !ok {
		return store.InvoiceRecord{}, &invoiceRequestError{http.StatusServiceUnavailable, map[string]string{"error": "SALES_UNAVAILABLE"}}
	}
	var lines []saleLine
	for _, id := range req.SaleIDs {
		recs, err := sales.ListSales(ctx, store.SalesQuery{MerchantID: tenant, ID: id, Limit: 1})
		if err != nil {
			return store.InvoiceRecord{}, err
		}
		if len(recs) == 0 {
			return store.InvoiceRecord{}, &saleError{code: "SALE_NOT_FOUND", saleID: id, status: http.StatusUnprocessableEntity}
		}
		l, err := s.invoiceableLine(ctx, recs[0])
		if err != nil {
			return store.InvoiceRecord{}, err
		}
		lines = append(lines, l)
	}
	holders, err := repo.ActiveInvoicesForSales(ctx, tenant, req.SaleIDs)
	if err != nil {
		return store.InvoiceRecord{}, err
	}
	for _, id := range req.SaleIDs {
		if h, taken := holders[id]; taken && h != replaced.ID {
			return store.InvoiceRecord{}, &store.SaleInvoicedError{SaleID: id, InvoiceID: h}
		}
	}

	receptorJSON, _ := json.Marshal(req.Receptor)
	rec := store.InvoiceRecord{ID: newInvoiceID(), MerchantID: tenant, Kind: store.InvoiceIndividual, Status: store.InvoicePending,
		IdempotencyKey: key, RequestHash: hash, Serie: serieIndividual, FormaPago: formaPagoFor(lines), Receptor: receptorJSON,
		ReplacesInvoiceID: replaced.ID, StampAttempts: 1}
	spec := s.invoiceSpec(rec, emisor, req.Receptor, lines, replaced.UUID, nil, now)
	_, tot, err := fiscal.BuildInvoiceComprobante(spec)
	if err != nil {
		return store.InvoiceRecord{}, err
	}
	rec.SubtotalCents, rec.IVACents, rec.ISRRetenidoCents, rec.TotalCents = tot.SubtotalCents, tot.IVACents, tot.ISRRetenidoCents, tot.TotalCents
	lease := now.Add(stampLease)
	rec.StampLeaseUntil = &lease
	created, err := repo.CreateInvoice(ctx, rec, storeLines(lines))
	if errors.Is(err, store.ErrInvoiceExists) {
		if created.RequestHash != hash {
			return store.InvoiceRecord{}, reused
		}
		return created, nil
	}
	if err != nil {
		return store.InvoiceRecord{}, err
	}
	return s.stampInvoice(ctx, created, signer, now), nil
}

func storeLines(lines []saleLine) []store.InvoiceSaleLine {
	out := make([]store.InvoiceSaleLine, len(lines))
	for i, l := range lines {
		out[i] = store.InvoiceSaleLine{SaleID: l.SaleID, BaseCents: l.BaseCents, IVACents: l.IVACents}
	}
	return out
}

// invoiceSpec assembles the CFDI spec of an invoice record.
func (s *Server) invoiceSpec(rec store.InvoiceRecord, emisor emisorProfile, receptor fiscal.Receptor, lines []saleLine, replacesUUID string, global *fiscal.CFDIInformacionGlobal, now time.Time) fiscal.InvoiceSpec {
	spec := fiscal.InvoiceSpec{Serie: rec.Serie, Folio: strconv.FormatInt(max(rec.Folio, 1), 10), Fecha: now, FormaPago: rec.FormaPago,
		LugarExpedicion: emisor.LugarExpedicion,
		Emisor:          fiscal.CFDIEmisor{Rfc: emisor.RFC, Nombre: emisor.Nombre, RegimenFiscal: emisor.Regimen},
		ClaveProdServ:   emisor.Settings.ClaveProdServ, ClaveUnidad: emisor.Settings.ClaveUnidad, Descripcion: emisor.Settings.Descripcion,
		Global: global}
	if global != nil {
		spec.Receptor = fiscal.PublicoEnGeneral(emisor.LugarExpedicion)
	} else {
		spec.Receptor = fiscal.CFDIReceptor{Rfc: receptor.RFC, Nombre: receptor.Nombre, DomicilioFiscalReceptor: receptor.CodigoPostal,
			RegimenFiscalReceptor: receptor.RegimenFiscal, UsoCFDI: receptor.UsoCFDI}
		spec.RetainISR = retainISR(emisor, receptor.RFC)
	}
	if replacesUUID != "" {
		spec.Replaces = []string{replacesUUID}
	}
	for _, l := range lines {
		spec.Lines = append(spec.Lines, l.InvoiceLine)
	}
	return spec
}

// globalInfo parses a stored global period ("M2026-10" / "D2026-11-19").
func globalInfo(period string) *fiscal.CFDIInformacionGlobal {
	if len(period) < 8 {
		return nil
	}
	t, err := time.Parse("2006-01", period[1:8])
	if err != nil {
		return nil
	}
	p := "04"
	if period[0] == 'D' {
		p = "01"
	}
	return &fiscal.CFDIInformacionGlobal{Periodicidad: p, Meses: fmt.Sprintf("%02d", int(t.Month())), Anio: t.Year()}
}

// sealedXML returns the invoice's sealed XML: the stored one while its
// Fecha is recent (a retry must resend the very same CFDI so the PAC can
// answer with the existing timbre), otherwise a fresh one sealed now.
func (s *Server) sealedXML(ctx context.Context, rec store.InvoiceRecord, signer *fiscal.CSDSigner, now time.Time) ([]byte, *time.Time, error) {
	if len(rec.XML) > 0 && rec.SealedAt != nil && now.Sub(*rec.SealedAt) < resealAfter {
		return rec.XML, rec.SealedAt, nil
	}
	emisor, err := s.emisorProfile(ctx, rec.MerchantID)
	if err != nil {
		return nil, nil, err
	}
	stored, err := s.invoiceRepo().InvoiceLines(ctx, rec.ID)
	if err != nil {
		return nil, nil, err
	}
	lines := make([]saleLine, len(stored))
	for i, l := range stored {
		lines[i] = saleLine{InvoiceLine: fiscal.InvoiceLine{SaleID: l.SaleID, BaseCents: l.BaseCents, IVACents: l.IVACents}}
	}
	var receptor fiscal.Receptor
	_ = json.Unmarshal(rec.Receptor, &receptor)
	replacesUUID := ""
	if rec.ReplacesInvoiceID != "" {
		if old, found, err := s.invoiceRepo().GetInvoice(ctx, rec.MerchantID, rec.ReplacesInvoiceID); err == nil && found {
			replacesUUID = old.UUID
		}
	}
	var global *fiscal.CFDIInformacionGlobal
	if rec.Kind == store.InvoiceGlobal {
		global = globalInfo(rec.GlobalPeriod)
	}
	spec := s.invoiceSpec(rec, emisor, receptor, lines, replacesUUID, global, now)
	if rec.Kind == store.InvoiceCreditNote {
		credited, found, err := s.invoiceRepo().GetInvoice(ctx, rec.MerchantID, rec.RelatedInvoiceID)
		if err != nil || !found {
			return nil, nil, fmt.Errorf("credited invoice %s not found: %v", rec.RelatedInvoiceID, err)
		}
		if spec, err = s.creditNoteSpec(rec, emisor, credited, lines, now); err != nil {
			return nil, nil, err
		}
	}
	comp, tot, err := fiscal.BuildInvoiceComprobante(spec)
	if err != nil {
		return nil, nil, err
	}
	if tot.TotalCents != rec.TotalCents || tot.IVACents != rec.IVACents || tot.ISRRetenidoCents != rec.ISRRetenidoCents {
		return nil, nil, fmt.Errorf("invoice %s totals changed (%d != %d)", rec.ID, tot.TotalCents, rec.TotalCents)
	}
	if _, err := signer.Seal(comp); err != nil {
		return nil, nil, err
	}
	doc, err := fiscal.BuildCFDI40XML(comp)
	if err != nil {
		return nil, nil, err
	}
	at := now.UTC()
	return doc, &at, nil
}

// stampInvoice seals (or reuses) the XML and stamps it. The invoice must be
// PENDING with the caller's lease. Outcome: STAMPED; FAILED (deterministic
// SAT/PAC or build error, sales released); PENDING with error
// PAC_UNAVAILABLE (retry from the backoffice / worker).
func (s *Server) stampInvoice(ctx context.Context, rec store.InvoiceRecord, signer *fiscal.CSDSigner, now time.Time) store.InvoiceRecord {
	repo := s.invoiceRepo()
	fail := func(code, detail string, op store.LinkOp, status string) store.InvoiceRecord {
		rec.Status, rec.ErrorCode, rec.ErrorDetail, rec.StampLeaseUntil = status, code, detail, nil
		out, err := repo.UpdateInvoice(ctx, rec, store.InvoicePending, op)
		if err != nil {
			slog.Error("invoice state not saved", "invoice_id", rec.ID, "status", status, "error", err)
			return rec
		}
		if status == store.InvoiceFailed {
			slog.Error("ALERT CFDI stamping failed", "invoice_id", rec.ID, "merchant_id", rec.MerchantID, "code", code, "detail", detail)
		}
		return out
	}
	if rec.Kind == store.InvoiceCreditNote {
		if err := s.creditedStillStamped(ctx, rec); err != nil {
			return fail("CREDITED_INVOICE_NOT_STAMPED", err.Error(), store.LinksRelease, store.InvoiceFailed)
		}
	}
	doc, sealedAt, err := s.sealedXML(ctx, rec, signer, now)
	if err != nil {
		return fail("CFDI_BUILD_FAILED", err.Error(), store.LinksRelease, store.InvoiceFailed)
	}
	if !sealedAt.Equal(ptrTime(rec.SealedAt)) {
		rec.XML, rec.SealedAt = doc, sealedAt
		if saved, err := repo.UpdateInvoice(ctx, rec, store.InvoicePending, store.LinksKeep); err == nil {
			rec = saved
		} else {
			slog.Error("sealed XML not saved", "invoice_id", rec.ID, "error", err)
			return rec
		}
	}
	resp, err := s.pacClient().Stamp(ctx, doc)
	switch {
	case errors.Is(err, pac.ErrSATValidationError), errors.Is(err, pac.ErrMalformedXML):
		code := "SAT_VALIDATION_ERROR"
		detail := err.Error()
		if resp != nil && resp.ErrorCode != "" {
			code, detail = resp.ErrorCode, resp.ErrorMessage
		}
		return fail(code, detail, store.LinksRelease, store.InvoiceFailed)
	case err != nil || resp == nil || resp.Timbre == nil:
		slog.Warn("PAC unavailable; invoice stays PENDING", "invoice_id", rec.ID, "error", err)
		return fail("PAC_UNAVAILABLE", fmt.Sprint(err), store.LinksKeep, store.InvoicePending)
	}
	stamped, err := pac.InjectTimbreFiscal(doc, resp.Timbre)
	if err != nil {
		return fail("TFD_INJECTION_FAILED", err.Error(), store.LinksKeep, store.InvoicePending)
	}
	at := resp.Timbre.FechaTimbrado.UTC()
	rec.Status, rec.UUID, rec.XML, rec.StampedAt, rec.StampLeaseUntil, rec.ErrorCode, rec.ErrorDetail =
		store.InvoiceStamped, strings.ToUpper(resp.Timbre.UUID), stamped, &at, nil, "", ""
	out, err := repo.UpdateInvoice(ctx, rec, store.InvoicePending, store.LinksKeep)
	if err != nil {
		slog.Error("ALERT stamped CFDI not saved", "invoice_id", rec.ID, "uuid", resp.Timbre.UUID, "error", err)
		return rec
	}
	slog.Info("CFDI stamped", "invoice_id", out.ID, "merchant_id", out.MerchantID, "uuid", out.UUID, "kind", out.Kind)
	return s.emailInvoice(ctx, out)
}

func ptrTime(t *time.Time) time.Time {
	if t == nil {
		return time.Time{}
	}
	return *t
}

// emailInvoice sends the PDF + XML to the receptor's email (best effort).
func (s *Server) emailInvoice(ctx context.Context, rec store.InvoiceRecord) store.InvoiceRecord {
	var receptor fiscal.Receptor
	if err := json.Unmarshal(rec.Receptor, &receptor); err != nil || receptor.Email == "" || rec.Status != store.InvoiceStamped {
		return rec
	}
	pdf, err := cfdipdf.Render(rec.XML)
	if err != nil {
		slog.Error("invoice PDF not rendered for email", "invoice_id", rec.ID, "error", err)
		return rec
	}
	name := fmt.Sprintf("%s-%s%d", rec.UUID, rec.Serie, rec.Folio)
	err = s.mailSender().Send(ctx, Mail{To: receptor.Email, Subject: fmt.Sprintf("Factura %s%d (CFDI %s)", rec.Serie, rec.Folio, rec.UUID),
		Body: "Adjuntamos la representación impresa (PDF) y el XML de su factura electrónica.",
		Attachments: []MailAttachment{{Name: name + ".pdf", ContentType: "application/pdf", Data: pdf},
			{Name: name + ".xml", ContentType: "application/xml", Data: rec.XML}}})
	if err != nil {
		slog.Error("invoice email failed", "invoice_id", rec.ID, "error", err)
		return rec
	}
	now := nowUTC()
	rec.EmailedAt = &now
	if out, err := s.invoiceRepo().UpdateInvoice(ctx, rec, store.InvoiceStamped, store.LinksKeep); err == nil {
		return out
	}
	return rec
}

// RetryInvoice re-stamps a PENDING or FAILED invoice (backoffice / worker).
func (s *Server) RetryInvoice(ctx context.Context, merchantID, id string) (store.InvoiceRecord, error) {
	now := nowUTC()
	rec, claimed, err := s.invoiceRepo().ClaimStamping(ctx, merchantID, id, now.Add(stampLease))
	if err != nil {
		return rec, err
	}
	if !claimed {
		return rec, errInvoiceNotRetryable
	}
	signer, err := s.activeSigner(rec.MerchantID, now)
	if err != nil {
		rec.Status, rec.ErrorCode, rec.ErrorDetail, rec.StampLeaseUntil = store.InvoicePending, "CSD_UNAVAILABLE", err.Error(), nil
		out, uerr := s.invoiceRepo().UpdateInvoice(ctx, rec, store.InvoicePending, store.LinksKeep)
		if uerr != nil {
			return rec, uerr
		}
		return out, err
	}
	return s.stampInvoice(ctx, rec, signer, now), nil
}

var errInvoiceNotRetryable = errors.New("invoice not retryable")

// ---------------------------------------------------------------------------
// Read endpoints

func (s *Server) loadInvoice(w http.ResponseWriter, r *http.Request) (store.InvoiceRecord, bool) {
	tenant, ok := requireTenant(w, r, r.PathValue("id"))
	if !ok {
		return store.InvoiceRecord{}, false
	}
	rec, found, err := s.invoiceRepo().GetInvoice(r.Context(), tenant, r.PathValue("invoice"))
	if err != nil {
		writeInvoiceError(w, err)
		return rec, false
	}
	if !found {
		writeError(w, http.StatusNotFound, "INVOICE_NOT_FOUND")
		return rec, false
	}
	return rec, true
}

// InvoicePage is GET /api/v1/merchants/{id}/invoices.
type InvoicePage struct {
	Items      []InvoiceResponse `json:"items"`
	NextCursor *string           `json:"next_cursor"`
}

// HandleListInvoices: GET /api/v1/merchants/{id}/invoices?limit=50&before= (any member).
func (s *Server) HandleListInvoices(w http.ResponseWriter, r *http.Request) {
	tenant, ok := requireTenant(w, r, r.PathValue("id"))
	if !ok {
		return
	}
	limit, cursor, ok := parseSalesPage(w, r, maxInvoicePg)
	if !ok {
		return
	}
	recs, err := s.invoiceRepo().ListInvoices(r.Context(), store.InvoiceQuery{MerchantID: tenant, Before: cursor, Limit: limit + 1})
	if err != nil {
		writeInvoiceError(w, err)
		return
	}
	page := InvoicePage{Items: []InvoiceResponse{}}
	if len(recs) > limit {
		recs = recs[:limit]
		c := encodeSalesCursor(store.SaleRecord{ID: recs[len(recs)-1].ID, CreatedAt: recs[len(recs)-1].CreatedAt})
		page.NextCursor = &c
	}
	for _, rec := range recs {
		page.Items = append(page.Items, invoiceResponse(rec))
	}
	writeJSON(w, http.StatusOK, page)
}

// HandleGetInvoice: GET /api/v1/merchants/{id}/invoices/{invoice}.
func (s *Server) HandleGetInvoice(w http.ResponseWriter, r *http.Request) {
	if rec, ok := s.loadInvoice(w, r); ok {
		writeJSON(w, http.StatusOK, invoiceResponse(rec))
	}
}

func stampedDoc(w http.ResponseWriter, rec store.InvoiceRecord) bool {
	if rec.UUID == "" || len(rec.XML) == 0 {
		writeError(w, http.StatusConflict, "INVOICE_NOT_STAMPED")
		return false
	}
	return true
}

// HandleInvoiceXML: GET …/invoices/{invoice}/xml (application/xml).
func (s *Server) HandleInvoiceXML(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.loadInvoice(w, r)
	if !ok || !stampedDoc(w, rec) {
		return
	}
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.xml"`, rec.UUID))
	_, _ = w.Write(rec.XML)
}

// HandleInvoicePDF: GET …/invoices/{invoice}/pdf (application/pdf).
func (s *Server) HandleInvoicePDF(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.loadInvoice(w, r)
	if !ok || !stampedDoc(w, rec) {
		return
	}
	pdf, err := cfdipdf.Render(rec.XML)
	if err != nil {
		slog.Error("invoice PDF render failed", "invoice_id", rec.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "PDF_RENDER_FAILED")
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`inline; filename="%s.pdf"`, rec.UUID))
	_, _ = w.Write(pdf)
}

// ---------------------------------------------------------------------------
// Cancellation

// CancelInvoiceRequest is POST …/invoices/{invoice}/cancel.
type CancelInvoiceRequest struct {
	Motivo           string `json:"motivo"`
	FolioSustitucion string `json:"folio_sustitucion,omitempty"`
}

// HandleCancelInvoice: owner only. 200 CANCELED (no receptor acceptance
// needed) or 202 CANCEL_REQUESTED (receptor has 72 h; the poller resolves it).
func (s *Server) HandleCancelInvoice(w http.ResponseWriter, r *http.Request) {
	if !requireRole(w, r, auth.RoleOwner) {
		return
	}
	rec, ok := s.loadInvoice(w, r)
	if !ok {
		return
	}
	var req CancelInvoiceRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON")
		return
	}
	req.Motivo, req.FolioSustitucion = strings.TrimSpace(req.Motivo), strings.ToUpper(strings.TrimSpace(req.FolioSustitucion))
	ctx := r.Context()
	invalid := func(field, reason string) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "CANCEL_INVALID", "fields": map[string]string{field: reason}})
	}
	if !fiscal.ValidMotivoCancelacion(req.Motivo) {
		invalid("motivo", fiscal.ReasonUnknownCode)
		return
	}
	if rec.Status != store.InvoiceStamped {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "INVOICE_NOT_CANCELABLE", "status": rec.Status})
		return
	}
	switch req.Motivo {
	case "01":
		if req.FolioSustitucion == "" {
			invalid("folio_sustitucion", fiscal.ReasonRequired)
			return
		}
		repl, found, err := s.invoiceRepo().FindInvoiceByUUID(ctx, rec.MerchantID, req.FolioSustitucion)
		if err == nil && !found {
			repl, found, err = s.invoiceRepo().GetInvoice(ctx, rec.MerchantID, strings.ToLower(req.FolioSustitucion))
		}
		if err != nil {
			writeInvoiceError(w, err)
			return
		}
		if !found || repl.Status != store.InvoiceStamped || repl.ReplacesInvoiceID != rec.ID {
			invalid("folio_sustitucion", "REPLACEMENT_NOT_STAMPED")
			return
		}
		req.FolioSustitucion = repl.UUID
	default:
		if req.FolioSustitucion != "" {
			invalid("folio_sustitucion", "ONLY_FOR_MOTIVO_01")
			return
		}
		if req.Motivo == "04" && rec.Kind != store.InvoiceGlobal {
			invalid("motivo", "MOTIVO_04_ONLY_FOR_GLOBAL")
			return
		}
	}
	// SAT: a CFDI with related vigente CFDI (its notas de crédito,
	// TipoRelacion 01) cannot be canceled; cancel them first. Without this a
	// replacement built from the net amount plus the old egreso still vigente
	// would credit the refund twice (MVP-23 audit).
	if rec.Kind != store.InvoiceCreditNote {
		notes, err := s.invoiceRepo().ListInvoices(ctx, store.InvoiceQuery{MerchantID: rec.MerchantID, Kind: store.InvoiceCreditNote,
			RelatedInvoiceID: rec.ID, Statuses: []string{store.InvoicePending, store.InvoiceStamped, store.InvoiceCancelRequested}, Limit: 1})
		if err != nil {
			writeInvoiceError(w, err)
			return
		}
		if len(notes) > 0 {
			writeJSON(w, http.StatusConflict, map[string]string{"error": "INVOICE_HAS_CREDIT_NOTES", "credit_note_id": notes[0].ID})
			return
		}
	}
	now := nowUTC()
	signer, err := s.activeSigner(rec.MerchantID, now)
	if err != nil {
		writeInvoiceError(w, err)
		return
	}
	// Claim the cancellation first (STAMPED -> CANCEL_REQUESTED) so two
	// concurrent requests never both call the PAC.
	claim := rec
	claim.Status, claim.CancelMotivo, claim.CancelFolioSustitucion, claim.CancelStatus = store.InvoiceCancelRequested, req.Motivo, req.FolioSustitucion, store.CancelRequested
	claim.CancelRequestedAt, claim.CancelResolvedAt = &now, nil
	claimed, err := s.invoiceRepo().UpdateInvoice(ctx, claim, store.InvoiceStamped, store.LinksKeep)
	if errors.Is(err, store.ErrInvoiceConflict) {
		writeError(w, http.StatusConflict, "INVOICE_NOT_CANCELABLE")
		return
	}
	if err != nil {
		writeInvoiceError(w, err)
		return
	}
	var receptor fiscal.Receptor
	_ = json.Unmarshal(rec.Receptor, &receptor)
	receptorRFC := receptor.RFC
	if rec.Kind == store.InvoiceGlobal {
		receptorRFC = fiscal.RFCPublicoEnGeneral
	}
	csd, err := s.getMerchantCSD(rec.MerchantID)
	var key []byte
	if err == nil {
		key, err = plainCSDKey(csd)
		csd.Wipe()
	}
	var resp *pac.CancelResponse
	if err == nil {
		resp, err = s.pacClient().Cancel(ctx, pac.CancelRequest{EmisorRFC: fiscal.CertificateRFC(signer.Certificate()), ReceptorRFC: receptorRFC,
			UUID: rec.UUID, TotalCents: rec.TotalCents, Motivo: req.Motivo, FolioSustitucion: req.FolioSustitucion,
			CertDER: signer.Certificate().Raw, KeyPKCS8: key})
	}
	clear(key)
	revert := func() {
		back := claimed
		back.Status, back.CancelStatus, back.CancelMotivo, back.CancelFolioSustitucion, back.CancelRequestedAt = store.InvoiceStamped, "", "", "", nil
		if _, uerr := s.invoiceRepo().UpdateInvoice(ctx, back, store.InvoiceCancelRequested, store.LinksKeep); uerr != nil {
			slog.Error("ALERT cancellation claim not reverted", "invoice_id", rec.ID, "error", uerr)
		}
	}
	switch {
	case errors.Is(err, pac.ErrSATValidationError) || (err == nil && resp != nil && resp.State == pac.CancelRejected):
		revert()
		code := "CANCEL_REJECTED"
		if resp != nil && resp.ErrorCode != "" {
			code = resp.ErrorCode
		}
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"error": "CANCEL_REJECTED", "detail": code})
		return
	case err != nil || resp == nil:
		revert()
		slog.Warn("PAC cancellation unavailable", "invoice_id", rec.ID, "error", err)
		writeError(w, http.StatusBadGateway, "PAC_UNAVAILABLE")
		return
	}
	user, _ := UserIDFromContext(ctx)
	slog.Info("CFDI cancellation requested", "invoice_id", rec.ID, "uuid", rec.UUID, "motivo", req.Motivo, "state", resp.State, "user", user)
	if resp.State == pac.CancelCanceled {
		out, err := s.resolveCancellation(ctx, claimed, true)
		if err != nil {
			writeInvoiceError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, invoiceResponse(out))
		return
	}
	writeJSON(w, http.StatusAccepted, invoiceResponse(claimed))
}

// resolveCancellation moves a CANCEL_REQUESTED invoice to CANCELED (sales
// released) or back to STAMPED with the cancellation REJECTED.
func (s *Server) resolveCancellation(ctx context.Context, rec store.InvoiceRecord, accepted bool) (store.InvoiceRecord, error) {
	now := nowUTC()
	rec.CancelResolvedAt = &now
	op := store.LinksKeep
	if accepted {
		rec.Status, rec.CancelStatus, op = store.InvoiceCanceled, store.CancelAccepted, store.LinksRelease
	} else {
		rec.Status, rec.CancelStatus = store.InvoiceStamped, store.CancelRejected
	}
	return s.invoiceRepo().UpdateInvoice(ctx, rec, store.InvoiceCancelRequested, op)
}

// PollCancellations asks the PAC for the SAT status of every
// CANCEL_REQUESTED invoice and resolves the ones the receptor answered (or
// whose 72 h expired). It returns how many were resolved.
func (s *Server) PollCancellations(ctx context.Context) (int, error) {
	recs, err := s.invoiceRepo().ListInvoices(ctx, store.InvoiceQuery{Statuses: []string{store.InvoiceCancelRequested}, Limit: 1000})
	if err != nil {
		return 0, err
	}
	resolved := 0
	for _, rec := range recs {
		// The SAT query expression needs re, rr, tt and id of the stamped CFDI.
		emisorRFC, receptorRFC := stampedParties(rec.XML)
		st, err := s.pacClient().Status(ctx, pac.StatusRequest{EmisorRFC: emisorRFC, ReceptorRFC: receptorRFC, UUID: rec.UUID, TotalCents: rec.TotalCents})
		if err != nil || st == nil {
			slog.Warn("CFDI status query failed", "invoice_id", rec.ID, "error", err)
			continue
		}
		var accepted bool
		switch {
		case st.Estado == pac.EstadoCancelado:
			accepted = true
		case st.EstatusCancelacion == pac.CancelacionRechazada:
			accepted = false
		default:
			continue
		}
		if _, err := s.resolveCancellation(ctx, rec, accepted); err != nil {
			slog.Error("cancellation not resolved", "invoice_id", rec.ID, "error", err)
			continue
		}
		resolved++
		slog.Info("CFDI cancellation resolved", "invoice_id", rec.ID, "uuid", rec.UUID, "accepted", accepted)
	}
	return resolved, nil
}

// stampedParties returns the emisor and receptor RFCs of a stamped CFDI.
func stampedParties(doc []byte) (emisorRFC, receptorRFC string) {
	var c struct {
		Emisor struct {
			Rfc string `xml:"Rfc,attr"`
		} `xml:"Emisor"`
		Receptor struct {
			Rfc string `xml:"Rfc,attr"`
		} `xml:"Receptor"`
	}
	_ = xml.Unmarshal(doc, &c)
	return c.Emisor.Rfc, c.Receptor.Rfc
}

// ---------------------------------------------------------------------------
// Factura global

// globalPeriod is one closed period of a merchant's factura global.
type globalPeriod struct {
	key        string // M2026-10 | D2026-11-19
	start, end time.Time
}

// globalPeriodOf is the period (America/Mexico_City boundaries) that t falls in.
func globalPeriodOf(periodicity string, t time.Time) globalPeriod {
	loc := fiscal.MexicoCityLocation()
	local := t.In(loc)
	if periodicity == store.GlobalDaily {
		start := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
		return globalPeriod{key: "D" + start.Format("2006-01-02"), start: start, end: start.AddDate(0, 0, 1)}
	}
	start := time.Date(local.Year(), local.Month(), 1, 0, 0, 0, 0, loc)
	return globalPeriod{key: "M" + start.Format("2006-01"), start: start, end: start.AddDate(0, 1, 0)}
}

// DefaultGlobalInvoiceMaxAge bounds the reconciliation of un-invoiced sales
// (CFDI_GLOBAL_MAX_AGE): the SAT expects the factura global within 72 h of
// the period end (RMF 2.7.1.21); later parts are still issued (and logged)
// for sales up to this age; older ones are alerted for manual handling.
const DefaultGlobalInvoiceMaxAge = 90 * 24 * time.Hour

// SetGlobalInvoiceMaxAge sets how old an un-invoiced sale may be to still
// get into an automatic factura global (<= 0 keeps the default).
func (s *Server) SetGlobalInvoiceMaxAge(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.globalMaxAge = d
}

// SetGlobalInvoiceMaxConcepts sets the conceptos per factura global part
// (<= 0 keeps maxGlobalConcepts; tests and tuning).
func (s *Server) SetGlobalInvoiceMaxConcepts(n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.globalMaxConcepts = n
}

func (s *Server) globalLimits() (maxAge time.Duration, maxConcepts int) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	maxAge, maxConcepts = s.globalMaxAge, s.globalMaxConcepts
	if maxAge <= 0 {
		maxAge = DefaultGlobalInvoiceMaxAge
	}
	if maxConcepts <= 0 {
		maxConcepts = maxGlobalConcepts
	}
	return maxAge, maxConcepts
}

// RunGlobalInvoices, per approved merchant with a CSD: retries its PENDING
// globals, then reconciles every eligible sale of a closed period that is in
// no active invoice and is at most the max age old (late captures, sales
// freed by a motivo 04 cancellation or by a FAILED stamp...) into new
// factura global parts of its period, at most maxGlobalConcepts conceptos
// each; eligible sales older than the window without invoice are alerted.
// It returns the invoices created or retried.
func (s *Server) RunGlobalInvoices(ctx context.Context, now time.Time) (int, error) {
	repo, _ := s.kybStore()
	recs, err := repo.ListKYB(ctx, store.KYBQuery{Status: kyb.StatusApproved, Limit: 100000})
	if err != nil {
		return 0, err
	}
	n := 0
	for _, k := range recs {
		if k.Blocked {
			continue
		}
		if _, found, err := s.merchantRepo.GetMerchantCSD(ctx, k.MerchantID); err != nil || !found {
			continue
		}
		emisor, err := s.emisorProfile(ctx, k.MerchantID)
		if err != nil {
			slog.Warn("factura global skipped: fiscal profile incomplete", "merchant_id", k.MerchantID, "error", err)
			continue
		}
		m, err := s.reconcileGlobals(ctx, k.MerchantID, emisor, now)
		if err != nil {
			slog.Error("factura global failed", "merchant_id", k.MerchantID, "error", err)
		}
		n += m
	}
	return n, nil
}

func (s *Server) reconcileGlobals(ctx context.Context, merchantID string, emisor emisorProfile, now time.Time) (int, error) {
	n := 0
	pending, err := s.invoiceRepo().ListInvoices(ctx, store.InvoiceQuery{MerchantID: merchantID, Kind: store.InvoiceGlobal,
		Statuses: []string{store.InvoicePending}, Limit: 1000})
	if err != nil {
		return 0, err
	}
	for _, g := range pending {
		if g.StampLeaseUntil != nil && g.StampLeaseUntil.After(now) {
			continue
		}
		if _, err := s.RetryInvoice(ctx, merchantID, g.ID); err == nil {
			n++
		} else if !errors.Is(err, errInvoiceNotRetryable) {
			slog.Warn("factura global retry failed", "merchant_id", merchantID, "invoice_id", g.ID, "error", err)
		}
	}

	maxAge, maxConcepts := s.globalLimits()
	periodicity := emisor.Settings.GlobalPeriodicity
	windowStart, open := now.Add(-maxAge), globalPeriodOf(periodicity, now).start
	byPeriod := map[string][]saleLine{}
	periods := map[string]globalPeriod{}
	if windowStart.Before(open) {
		err = s.eachUninvoicedSale(ctx, merchantID, windowStart, open, 0, func(l saleLine) {
			p := globalPeriodOf(periodicity, l.createdAt)
			periods[p.key] = p
			byPeriod[p.key] = append(byPeriod[p.key], l)
		})
		if err != nil {
			return n, err
		}
	}
	keys := make([]string, 0, len(byPeriod))
	for k := range byPeriod {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		lines := byPeriod[k]
		sort.Slice(lines, func(i, j int) bool {
			if !lines[i].createdAt.Equal(lines[j].createdAt) {
				return lines[i].createdAt.Before(lines[j].createdAt)
			}
			return lines[i].SaleID < lines[j].SaleID
		})
		for i := 0; i < len(lines); i += maxConcepts {
			out, err := s.issueGlobalPart(ctx, merchantID, emisor, periods[k], lines[i:min(i+maxConcepts, len(lines))], now)
			if err != nil {
				slog.Error("factura global part failed", "merchant_id", merchantID, "period", k, "error", err)
				continue
			}
			if out != nil {
				n++
			}
		}
	}

	// Eligible sales older than the window that still have no invoice.
	var stale []string
	staleBefore := windowStart
	if open.Before(staleBefore) {
		staleBefore = open
	}
	if err := s.eachUninvoicedSale(ctx, merchantID, time.Time{}, staleBefore, staleSalesScan, func(l saleLine) {
		stale = append(stale, l.SaleID)
	}); err != nil {
		return n, err
	}
	if len(stale) > 0 {
		slog.Error("ALERT eligible sales older than the factura global window have no invoice", "merchant_id", merchantID,
			"count", len(stale), "max_age", maxAge.String(), "sale_ids", strings.Join(stale[:min(len(stale), 20)], ","))
	}
	return n, nil
}

// staleSalesScan bounds the alert scan to the newest sales before the window.
const staleSalesScan = 1000

// eachUninvoicedSale calls fn for every invoiceable sale created in
// [from, to) that is in no active invoice (newest first; limit 0 = all).
func (s *Server) eachUninvoicedSale(ctx context.Context, merchantID string, from, to time.Time, limit int, fn func(saleLine)) error {
	sales, ok := s.salesRepo()
	if !ok {
		return errSalesUnavailable
	}
	var before *store.SalesCursor
	seen := 0
	for {
		page, err := sales.ListSales(ctx, store.SalesQuery{MerchantID: merchantID, From: from, To: to, Before: before, Limit: globalSalesPage})
		if err != nil {
			return err
		}
		ids := make([]string, len(page))
		for i, r := range page {
			ids[i] = r.ID
		}
		holders, err := s.invoiceRepo().ActiveInvoicesForSales(ctx, merchantID, ids)
		if err != nil {
			return err
		}
		for _, rec := range page {
			if _, taken := holders[rec.ID]; taken {
				continue
			}
			l, err := s.invoiceableLine(ctx, rec)
			if err != nil {
				var se *saleError
				if errors.As(err, &se) {
					continue // refunded / declined / uncaptured sales are not invoiced
				}
				return err
			}
			fn(l)
		}
		seen += len(page)
		if len(page) < globalSalesPage || (limit > 0 && seen >= limit) {
			return nil
		}
		last := page[len(page)-1]
		before = &store.SalesCursor{CreatedAt: last.CreatedAt, ID: last.ID}
	}
}

// issueGlobalPart issues one factura global part for lines (sales of period
// p in no active invoice). The idempotency key is derived from the period and
// the part's sale set, so reruns never duplicate it: a FAILED part waits for
// the backoffice retry, a CANCELED one (motivo 04) moves to the next suffix.
func (s *Server) issueGlobalPart(ctx context.Context, merchantID string, emisor emisorProfile, p globalPeriod, lines []saleLine, now time.Time) (*store.InvoiceRecord, error) {
	saleIDs := make([]string, len(lines))
	for i, l := range lines {
		saleIDs[i] = l.SaleID
	}
	base := "global:" + p.key + ":" + requestHash(saleIDs)[:16]
	key := base
	for i := 1; ; i++ {
		existing, found, err := s.invoiceRepo().FindInvoiceByIdempotencyKey(ctx, merchantID, key)
		if err != nil {
			return nil, err
		}
		if !found {
			break
		}
		if existing.Status != store.InvoiceCanceled {
			return nil, nil // live (another worker) or FAILED (backoffice retry)
		}
		key = fmt.Sprintf("%s:%d", base, i)
	}
	part := 1
	prior, err := s.invoiceRepo().ListInvoices(ctx, store.InvoiceQuery{MerchantID: merchantID, Kind: store.InvoiceGlobal, GlobalPeriod: p.key, Limit: 1000})
	if err != nil {
		return nil, err
	}
	for _, g := range prior {
		part = max(part, g.GlobalPart+1)
	}
	signer, err := s.activeSigner(merchantID, now)
	if err != nil {
		return nil, err
	}
	global := globalInfo(p.key)
	receptorJSON, _ := json.Marshal(map[string]string{"rfc": fiscal.RFCPublicoEnGeneral, "nombre": "PUBLICO EN GENERAL",
		"regimen_fiscal": "616", "codigo_postal": emisor.LugarExpedicion, "uso_cfdi": "S01"})
	rec := store.InvoiceRecord{ID: newInvoiceID(), MerchantID: merchantID, Kind: store.InvoiceGlobal, Status: store.InvoicePending,
		IdempotencyKey: key, RequestHash: requestHash(saleIDs), Serie: serieGlobal, FormaPago: formaPagoFor(lines),
		Receptor: receptorJSON, GlobalPeriod: p.key, GlobalPart: part, StampAttempts: 1}
	_, tot, err := fiscal.BuildInvoiceComprobante(s.invoiceSpec(rec, emisor, fiscal.Receptor{}, lines, "", global, now))
	if err != nil {
		return nil, err
	}
	rec.SubtotalCents, rec.IVACents, rec.TotalCents = tot.SubtotalCents, tot.IVACents, tot.TotalCents
	lease := now.Add(stampLease)
	rec.StampLeaseUntil = &lease
	created, err := s.invoiceRepo().CreateInvoice(ctx, rec, storeLines(lines))
	if errors.Is(err, store.ErrGlobalInvoiceExists) || errors.Is(err, store.ErrInvoiceExists) {
		return nil, nil // a concurrent run took the part number: next run
	}
	var si *store.SaleInvoicedError
	if errors.As(err, &si) {
		return nil, nil // a sale was invoiced meanwhile: next run
	}
	if err != nil {
		return nil, err
	}
	if late := now.Sub(p.end); late > 72*time.Hour {
		slog.Warn("factura global issued after the 72 h SAT window", "merchant_id", merchantID, "period", p.key, "part", part, "late", late.Truncate(time.Hour))
	}
	out := s.stampInvoice(ctx, created, signer, now)
	return &out, nil
}

// ---------------------------------------------------------------------------
// Backoffice: failed / pending stampings

func (s *Server) handleBOInvoices(w http.ResponseWriter, r *http.Request, _ string) {
	limit, ok := boLimit(w, r, 50, 200)
	if !ok {
		return
	}
	statuses := []string{store.InvoiceFailed, store.InvoicePending}
	if v := strings.TrimSpace(r.URL.Query().Get("status")); v != "" {
		statuses = strings.Split(strings.ToUpper(v), ",")
	}
	recs, err := s.invoiceRepo().ListInvoices(r.Context(), store.InvoiceQuery{MerchantID: r.URL.Query().Get("merchant_id"), Statuses: statuses, Limit: limit})
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "INVOICES_UNAVAILABLE")
		return
	}
	type boInvoice struct {
		MerchantID    string `json:"merchant_id"`
		StampAttempts int    `json:"stamp_attempts"`
		ErrorDetail   string `json:"error_detail,omitempty"`
		InvoiceResponse
	}
	out := make([]boInvoice, 0, len(recs))
	for _, rec := range recs {
		out = append(out, boInvoice{MerchantID: rec.MerchantID, StampAttempts: rec.StampAttempts, ErrorDetail: rec.ErrorDetail, InvoiceResponse: invoiceResponse(rec)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}

func (s *Server) handleBORetryInvoice(w http.ResponseWriter, r *http.Request, op string) {
	ctx := r.Context()
	id := r.PathValue("invoice")
	before, found, err := s.invoiceRepo().GetInvoice(ctx, "", id)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "INVOICES_UNAVAILABLE")
		return
	}
	if !found {
		writeError(w, http.StatusNotFound, "INVOICE_NOT_FOUND")
		return
	}
	out, err := s.RetryInvoice(ctx, "", id)
	var si *store.SaleInvoicedError
	switch {
	case errors.Is(err, errInvoiceNotRetryable):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "INVOICE_NOT_RETRYABLE", "status": before.Status})
		return
	case errors.As(err, &si):
		writeJSON(w, http.StatusConflict, map[string]string{"error": "SALE_ALREADY_INVOICED", "sale_id": si.SaleID, "invoice_id": si.InvoiceID})
		return
	}
	_ = s.audit(ctx, op, "invoice.retry", before.MerchantID, id, map[string]any{"status": before.Status, "error": before.ErrorCode},
		map[string]any{"status": out.Status, "uuid": out.UUID, "error": out.ErrorCode}, fmt.Sprint(err))
	if err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "RETRY_FAILED", "detail": err.Error(), "invoice": invoiceResponse(out)})
		return
	}
	writeJSON(w, http.StatusOK, invoiceResponse(out))
}
