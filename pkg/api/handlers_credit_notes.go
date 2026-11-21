package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"softpos/pkg/fiscal"
	"softpos/pkg/store"
)

// Notas de crédito (MVP-23, docs/CFDI.md): a refund or chargeback of a card
// sale enqueues a credit-note job in the same money transaction as its
// ledger movement (applyCardMoneyEvent). The worker below turns each job
// into a CFDI de egreso (TipoDeComprobante E, CfdiRelacionados TipoRelacion
// 01 with the credited invoice's UUID) when the sale's active invoice still
// covers more than the net collected, and stamps it through the PAC port
// like any invoice (PENDING retried by this worker, FAILED from the
// backoffice).

const (
	serieCreditNote = "E"
	// creditNoteGrace: a job whose sale has no active invoice waits this long
	// before it is resolved NOT_REQUIRED, so an invoice being created with
	// amounts read just before the movement committed is still credited.
	creditNoteGrace      = 5 * time.Minute
	creditNoteMaxBackoff = time.Hour
	creditNoteBatch      = 500
)

func (s *Server) creditJobRepo() (store.CreditNoteJobRepository, bool) {
	repo, ok := s.moneyStore().(store.CreditNoteJobRepository)
	return repo, ok
}

// creditNoteKey is the invoice idempotency key of a job (one credit note
// per refund / chargeback).
func creditNoteKey(j store.CreditNoteJob) string {
	return "cn:" + j.SourceKind + ":" + j.SaleID + ":" + j.SourceID
}

// creditNoteDescripcion is the concepto description, from the key.
func creditNoteDescripcion(key string) string {
	if strings.HasPrefix(key, "cn:"+store.CreditSourceChargeback+":") {
		return "Contracargo de venta"
	}
	return "Devolución de venta"
}

// creditSplit divides the credited gross into base and IVA in the
// proportion of the credited invoice line (half-up); the last credit of a
// line takes exactly what is left of its base and IVA, so the credit notes
// of a line add up to it to the cent. A split outside the SAT IVA limits
// falls back to the RESICO 16 % split of the credited gross.
func creditSplit(credit, lineBase, lineIVA, prevBase, prevIVA int64) (base, iva int64) {
	lineGross := lineBase + lineIVA
	if credit == lineGross-prevBase-prevIVA {
		base, iva = lineBase-prevBase, lineIVA-prevIVA
	} else {
		base = (2*credit*lineBase + lineGross) / (2 * lineGross)
		iva = credit - base
	}
	if base <= 0 || iva < 0 || !fiscal.IVAWithinSATLimits(base, iva) {
		base, iva = fiscal.IVAFromGross(credit)
	}
	return base, iva
}

func creditBackoff(attempts int) time.Duration {
	d := time.Minute
	for i := 1; i < attempts && d < creditNoteMaxBackoff; i++ {
		d *= 2
	}
	return min(d, creditNoteMaxBackoff)
}

// ProcessCreditNotes resolves the due credit-note jobs (oldest first; a
// sale's jobs strictly in order) and retries credit notes whose stamping is
// PENDING. It returns how many credit notes were issued or retried.
func (s *Server) ProcessCreditNotes(ctx context.Context, now time.Time) (int, error) {
	repo, ok := s.creditJobRepo()
	if !ok {
		return 0, nil
	}
	jobs, err := repo.ListCreditNoteJobs(ctx, store.CreditNoteJobQuery{Statuses: []string{store.CreditJobPending}, Limit: creditNoteBatch})
	if err != nil {
		return 0, err
	}
	n := 0
	blocked := map[string]bool{}
	issuedNow := map[string]bool{}
	for _, j := range jobs {
		sale := j.MerchantID + "\x00" + j.SaleID
		if blocked[sale] {
			continue
		}
		if j.NextAttemptAt.After(now) {
			blocked[sale] = true // a later movement of the sale waits for this one
			continue
		}
		issued, resolved, id, err := s.processCreditNoteJob(ctx, repo, j, now)
		issuedNow[id] = true
		if err != nil {
			slog.Error("credit note job failed", "job_id", j.ID, "merchant_id", j.MerchantID, "sale_id", j.SaleID, "error", err)
		}
		if !resolved {
			blocked[sale] = true
		}
		if issued {
			n++
		}
	}
	pending, err := s.invoiceRepo().ListInvoices(ctx, store.InvoiceQuery{Kind: store.InvoiceCreditNote, Statuses: []string{store.InvoicePending}, Limit: creditNoteBatch})
	if err != nil {
		return n, err
	}
	for _, inv := range pending {
		if issuedNow[inv.ID] || (inv.StampLeaseUntil != nil && inv.StampLeaseUntil.After(now)) {
			continue
		}
		if _, err := s.RetryInvoice(ctx, inv.MerchantID, inv.ID); err != nil && !errors.Is(err, errInvoiceNotRetryable) {
			slog.Warn("credit note stamping retry failed", "invoice_id", inv.ID, "error", err)
			continue
		}
		n++
	}
	return n, nil
}

// processCreditNoteJob decides one job. resolved=false means it waits (no
// stamped invoice yet, CSD missing, ...) and was rescheduled.
func (s *Server) processCreditNoteJob(ctx context.Context, repo store.CreditNoteJobRepository, j store.CreditNoteJob, now time.Time) (issued, resolved bool, invoiceID string, err error) {
	wait := func(reason string) (bool, bool, string, error) {
		j.Attempts++
		j.NextAttemptAt, j.LastError = now.Add(creditBackoff(j.Attempts)), reason
		return false, false, "", repo.UpdateCreditNoteJob(ctx, j)
	}
	notRequired := func(reason string) (bool, bool, string, error) {
		j.Status, j.LastError = store.CreditJobNotRequired, reason
		slog.Info("credit note not required", "job_id", j.ID, "sale_id", j.SaleID, "source", j.SourceKind, "reason", reason)
		return false, true, "", repo.UpdateCreditNoteJob(ctx, j)
	}
	invoices := s.invoiceRepo()
	key := creditNoteKey(j)
	if existing, found, err := invoices.FindInvoiceByIdempotencyKey(ctx, j.MerchantID, key); err != nil {
		return wait("INVOICES_UNAVAILABLE: " + err.Error())
	} else if found { // created by an earlier attempt that did not record it
		j.Status, j.CreditNoteInvoiceID, j.RelatedInvoiceID, j.CreditedCents, j.LastError =
			store.CreditJobIssued, existing.ID, existing.RelatedInvoiceID, existing.SubtotalCents+existing.IVACents, ""
		return false, true, "", repo.UpdateCreditNoteJob(ctx, j)
	}
	links, err := invoices.SaleInvoiceLinks(ctx, j.MerchantID, []string{j.SaleID})
	if err != nil {
		return wait("INVOICES_UNAVAILABLE: " + err.Error())
	}
	var active *store.SaleInvoiceLink
	for i := range links {
		if links[i].Active {
			active = &links[i]
		}
	}
	if active == nil {
		if now.Sub(j.CreatedAt) < creditNoteGrace {
			return wait("NO_INVOICE_YET")
		}
		return notRequired("sale not invoiced: a later invoice uses the net amount")
	}
	if active.Status != store.InvoiceStamped {
		return wait("INVOICE_NOT_STAMPED: " + active.Status)
	}
	credited, found, err := invoices.GetInvoice(ctx, j.MerchantID, active.InvoiceID)
	if err != nil || !found {
		return wait(fmt.Sprintf("INVOICE_UNAVAILABLE: %v", err))
	}
	var prevBase, prevIVA int64
	for _, l := range links {
		if l.Kind == store.InvoiceCreditNote && l.RelatedInvoiceID == credited.ID && l.Status != store.InvoiceCanceled {
			prevBase += l.BaseCents
			prevIVA += l.IVACents
		}
	}
	lineGross := active.BaseCents + active.IVACents
	credit := min(j.AmountCents, lineGross-prevBase-prevIVA-j.NetAfterCents)
	if credit <= 0 {
		return notRequired("the invoice already reflects this movement")
	}
	base, iva := creditSplit(credit, active.BaseCents, active.IVACents, prevBase, prevIVA)

	emisor, err := s.emisorProfile(ctx, j.MerchantID)
	if err != nil {
		return wait("FISCAL_PROFILE_INCOMPLETE")
	}
	signer, err := s.activeSigner(j.MerchantID, now)
	if err != nil {
		return wait("CSD_UNAVAILABLE: " + err.Error())
	}
	rec := store.InvoiceRecord{ID: newInvoiceID(), MerchantID: j.MerchantID, Kind: store.InvoiceCreditNote, Status: store.InvoicePending,
		IdempotencyKey: key, RequestHash: requestHash([]any{key, base, iva}), Serie: serieCreditNote, FormaPago: "04",
		Receptor: credited.Receptor, GlobalPeriod: credited.GlobalPeriod, RelatedInvoiceID: credited.ID, StampAttempts: 1}
	lines := []saleLine{{InvoiceLine: fiscal.InvoiceLine{SaleID: j.SaleID, BaseCents: base, IVACents: iva}}}
	spec, err := s.creditNoteSpec(rec, emisor, credited, lines, now)
	if err != nil {
		return wait("CFDI_BUILD_FAILED: " + err.Error())
	}
	_, tot, err := fiscal.BuildInvoiceComprobante(spec)
	if err != nil {
		return wait("CFDI_BUILD_FAILED: " + err.Error())
	}
	rec.SubtotalCents, rec.IVACents, rec.ISRRetenidoCents, rec.TotalCents = tot.SubtotalCents, tot.IVACents, tot.ISRRetenidoCents, tot.TotalCents
	lease := now.Add(stampLease)
	rec.StampLeaseUntil = &lease
	created, err := invoices.CreateInvoice(ctx, rec, storeLines(lines))
	fresh := err == nil
	if err != nil && !errors.Is(err, store.ErrInvoiceExists) {
		return wait("CREDIT_NOTE_NOT_CREATED: " + err.Error())
	}
	j.Status, j.CreditNoteInvoiceID, j.RelatedInvoiceID, j.CreditedCents, j.LastError = store.CreditJobIssued, created.ID, credited.ID, credit, ""
	if err := repo.UpdateCreditNoteJob(ctx, j); err != nil {
		return false, false, "", err
	}
	slog.Info("credit note created", "job_id", j.ID, "invoice_id", created.ID, "credited_invoice_id", credited.ID,
		"sale_id", j.SaleID, "source", j.SourceKind, "credited_cents", credit)
	if fresh {
		s.stampInvoice(ctx, created, signer, now)
	}
	return true, true, created.ID, nil
}

// creditNoteSpec is the CFDI de egreso spec of a credit note crediting
// `credited`: same receptor (UsoCFDI G02 "Devoluciones, descuentos o
// bonificaciones" when the catalog allows it for the receptor's régimen),
// the factura global's InformacionGlobal when it credits a global, ISR
// retention when the credited invoice withheld it.
func (s *Server) creditNoteSpec(rec store.InvoiceRecord, emisor emisorProfile, credited store.InvoiceRecord, lines []saleLine, now time.Time) (fiscal.InvoiceSpec, error) {
	if credited.UUID == "" {
		return fiscal.InvoiceSpec{}, errors.New("credited invoice has no UUID")
	}
	var receptor fiscal.Receptor
	_ = json.Unmarshal(credited.Receptor, &receptor)
	var global *fiscal.CFDIInformacionGlobal
	if credited.Kind == store.InvoiceGlobal {
		global = globalInfo(credited.GlobalPeriod)
	} else {
		g02 := receptor
		g02.UsoCFDI = "G02"
		if len(fiscal.ValidateReceptor(g02)) == 0 {
			receptor = g02
		}
	}
	spec := s.invoiceSpec(rec, emisor, receptor, lines, "", global, now)
	spec.Egreso, spec.CreditNoteOf = true, []string{credited.UUID}
	spec.Descripcion = creditNoteDescripcion(rec.IdempotencyKey)
	spec.RetainISR = credited.ISRRetenidoCents > 0
	return spec, nil
}

// ---------------------------------------------------------------------------
// Backoffice: credit-note jobs (flagged chargeback reversals, waiting jobs)

func (s *Server) handleBOCreditNoteJobs(w http.ResponseWriter, r *http.Request, _ string) {
	limit, ok := boLimit(w, r, 100, 1000)
	if !ok {
		return
	}
	repo, ok := s.creditJobRepo()
	if !ok {
		writeError(w, http.StatusServiceUnavailable, "CREDIT_NOTES_UNAVAILABLE")
		return
	}
	q := store.CreditNoteJobQuery{MerchantID: r.URL.Query().Get("merchant_id"), Flagged: r.URL.Query().Get("flagged") == "true", Limit: limit}
	if v := strings.TrimSpace(r.URL.Query().Get("status")); v != "" {
		q.Statuses = strings.Split(strings.ToUpper(v), ",")
	}
	jobs, err := repo.ListCreditNoteJobs(r.Context(), q)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "CREDIT_NOTES_UNAVAILABLE")
		return
	}
	type boJob struct {
		ID                  int64  `json:"id"`
		MerchantID          string `json:"merchant_id"`
		SaleID              string `json:"sale_id"`
		SourceKind          string `json:"source_kind"`
		SourceID            string `json:"source_id"`
		AmountCents         int64  `json:"amount_cents"`
		Status              string `json:"status"`
		CreditNoteInvoiceID string `json:"credit_note_invoice_id,omitempty"`
		RelatedInvoiceID    string `json:"related_invoice_id,omitempty"`
		CreditedCents       int64  `json:"credited_cents"`
		Flag                string `json:"flag,omitempty"`
		Attempts            int    `json:"attempts"`
		LastError           string `json:"last_error,omitempty"`
		CreatedAt           string `json:"created_at"`
	}
	out := make([]boJob, 0, len(jobs))
	for _, j := range jobs {
		out = append(out, boJob{ID: j.ID, MerchantID: j.MerchantID, SaleID: j.SaleID, SourceKind: j.SourceKind, SourceID: j.SourceID,
			AmountCents: j.AmountCents, Status: j.Status, CreditNoteInvoiceID: j.CreditNoteInvoiceID, RelatedInvoiceID: j.RelatedInvoiceID,
			CreditedCents: j.CreditedCents, Flag: j.Flag, Attempts: j.Attempts, LastError: j.LastError, CreatedAt: j.CreatedAt.Format(time.RFC3339)})
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": out})
}
