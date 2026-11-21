package api

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"log/slog"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"softpos/pkg/auth"
	"softpos/pkg/cfdipdf"
	"softpos/pkg/fiscal"
	"softpos/pkg/store"
)

// Sale receipt + autofactura (MVP-26, docs/CFDI.md "Comprobante de venta").
//
// Every confirmed sale (card capture, SPEI abono, CoDi confirmation) has one
// public receipt at /r/{token}. The token is
// base64url(id[12] || expiry[8] || HMAC-SHA256(RECEIPT_TOKEN_SECRET,
// "kerno-receipt-v1" || id || expiry)[:16]): unguessable (96 random bits),
// signed (forged or altered tokens are rejected before any lookup), expiring
// (RECEIPT_TTL, default 90 days) and free of tenant data (the id is random;
// sale_receipts maps it to the sale). The end customer can request an
// individual CFDI from it (autofactura) within the merchant's window.

const (
	receiptTokenContext          = "kerno-receipt-v1"
	receiptIDBytes               = 12
	receiptMACBytes              = 16
	DefaultReceiptTTL            = 90 * 24 * time.Hour
	DefaultReceiptRateLimitPerIP = 60 // public /r/* requests per minute
	receiptPathPrefix            = "/r/"
)

// SMSSender sends a text message (receipt links). Only LogSMSSender exists
// until an SMS / WhatsApp provider is contracted.
type SMSSender interface {
	SendSMS(ctx context.Context, to, body string) error
}

// LogSMSSender logs the message metadata (masked phone) instead of sending it.
type LogSMSSender struct{}

func (LogSMSSender) SendSMS(_ context.Context, to, body string) error {
	masked := to
	if len(masked) > 4 {
		masked = strings.Repeat("*", len(masked)-4) + masked[len(masked)-4:]
	}
	slog.Info("sms not sent (log sender)", "to", masked, "chars", len([]rune(body)))
	return nil
}

// SetSMSSender replaces the SMS port (default LogSMSSender).
func (s *Server) SetSMSSender(sender SMSSender) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sms = sender
}

func (s *Server) smsSender() SMSSender {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.sms == nil {
		return LogSMSSender{}
	}
	return s.sms
}

// SetReceiptTokenSecret sets the HMAC secret of receipt tokens
// (RECEIPT_TOKEN_SECRET). Without it receipts are disabled (503
// RECEIPTS_NOT_CONFIGURED and no receipt_url).
func (s *Server) SetReceiptTokenSecret(secret []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.receiptSecret = append([]byte(nil), secret...)
}

// SetReceiptOptions sets the public base URL of receipt links (e.g.
// https://pagos.kerno.mx; "" = relative /r/…) and the token lifetime
// (<= 0 keeps DefaultReceiptTTL).
func (s *Server) SetReceiptOptions(baseURL string, ttl time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.receiptBaseURL, s.receiptTTL = strings.TrimRight(strings.TrimSpace(baseURL), "/"), ttl
}

// SetReceiptRepository replaces the in-memory receipt store.
func (s *Server) SetReceiptRepository(repo store.ReceiptRepository) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.receipts = repo
}

func (s *Server) receiptConfig() (secret []byte, baseURL string, ttl time.Duration, repo store.ReceiptRepository) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ttl = s.receiptTTL
	if ttl <= 0 {
		ttl = DefaultReceiptTTL
	}
	return s.receiptSecret, s.receiptBaseURL, ttl, s.receipts
}

func receiptMAC(secret, id []byte, exp uint64) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(receiptTokenContext))
	m.Write(id)
	var b [8]byte
	binary.BigEndian.PutUint64(b[:], exp)
	m.Write(b[:])
	return m.Sum(nil)[:receiptMACBytes]
}

// encodeReceiptToken signs (id, expiry).
func encodeReceiptToken(secret []byte, idHex string, exp time.Time) (string, error) {
	id, err := hex.DecodeString(idHex)
	if err != nil || len(id) != receiptIDBytes {
		return "", errors.New("invalid receipt id")
	}
	e := uint64(exp.Unix())
	buf := make([]byte, 0, receiptIDBytes+8+receiptMACBytes)
	buf = append(buf, id...)
	buf = binary.BigEndian.AppendUint64(buf, e)
	buf = append(buf, receiptMAC(secret, id, e)...)
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// decodeReceiptToken verifies the signature and the expiry.
func decodeReceiptToken(secret []byte, tok string, now time.Time) (idHex string, exp time.Time, ok bool) {
	if len(secret) == 0 || len(tok) > 64 {
		return "", time.Time{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil || len(raw) != receiptIDBytes+8+receiptMACBytes {
		return "", time.Time{}, false
	}
	id, e, mac := raw[:receiptIDBytes], binary.BigEndian.Uint64(raw[receiptIDBytes:receiptIDBytes+8]), raw[receiptIDBytes+8:]
	if !hmac.Equal(mac, receiptMAC(secret, id, e)) || e > 1<<40 {
		return "", time.Time{}, false
	}
	exp = time.Unix(int64(e), 0).UTC()
	if !now.Before(exp) {
		return "", time.Time{}, false
	}
	return hex.EncodeToString(id), exp, true
}

var (
	errReceiptsNotConfigured = errors.New("receipt tokens not configured")
	errSaleNotConfirmed      = errors.New("sale not confirmed")
)

// saleConfirmed reports whether money was collected for the sale.
func saleConfirmed(rec store.SaleRecord) bool {
	switch rec.Kind {
	case store.SaleCard:
		return store.CardPaymentStatus(rec.Status).Captured()
	case store.SaleSPEI:
		return rec.Status == "RECEIVED"
	case store.SaleCoDi:
		return rec.Status == "CONFIRMED"
	}
	return false
}

// ensureReceipt returns the receipt of a confirmed sale and its public URL.
func (s *Server) ensureReceipt(ctx context.Context, sale store.SaleRecord) (store.ReceiptRecord, string, error) {
	secret, base, ttl, repo := s.receiptConfig()
	if len(secret) == 0 || repo == nil {
		return store.ReceiptRecord{}, "", errReceiptsNotConfigured
	}
	if !saleConfirmed(sale) {
		return store.ReceiptRecord{}, "", errSaleNotConfirmed
	}
	id := make([]byte, receiptIDBytes)
	if _, err := rand.Read(id); err != nil {
		return store.ReceiptRecord{}, "", err
	}
	rec, err := repo.EnsureReceipt(ctx, store.ReceiptRecord{ID: hex.EncodeToString(id), MerchantID: sale.MerchantID, SaleID: sale.ID,
		ExpiresAt: nowUTC().Add(ttl)})
	if err != nil {
		return store.ReceiptRecord{}, "", err
	}
	tok, err := encodeReceiptToken(secret, rec.ID, rec.ExpiresAt)
	if err != nil {
		return store.ReceiptRecord{}, "", err
	}
	return rec, base + receiptPathPrefix + tok, nil
}

// ---------------------------------------------------------------------------
// Receipt view

// ReceiptCard is the card part of a receipt (never the full PAN).
type ReceiptCard struct {
	Brand     string `json:"brand,omitempty"`
	MaskedPAN string `json:"masked_pan,omitempty"` // "**** 4242"
	AuthCode  string `json:"auth_code,omitempty"`
}

// ReceiptAutofactura tells the end customer whether and until when they can
// request their CFDI from the receipt.
type ReceiptAutofactura struct {
	Available bool   `json:"available"`
	Reason    string `json:"reason,omitempty"` // DISABLED | WINDOW_CLOSED | SALE_NOT_INVOICEABLE | ALREADY_INVOICED
	Deadline  string `json:"deadline,omitempty"`
	URL       string `json:"url,omitempty"`
}

// ReceiptInvoice is the CFDI requested through the receipt.
type ReceiptInvoice struct {
	Status string `json:"status"`
	UUID   string `json:"uuid,omitempty"`
	PDFURL string `json:"pdf_url,omitempty"`
	XMLURL string `json:"xml_url,omitempty"`
	Error  string `json:"error,omitempty"`
}

// ReceiptView is GET /r/{token}.
type ReceiptView struct {
	MerchantName  string             `json:"merchant_name"`
	MerchantRFC   string             `json:"merchant_rfc,omitempty"`
	Folio         string             `json:"folio"`
	Kind          string             `json:"kind"` // card | spei | codi
	Status        string             `json:"status"`
	AmountCents   int64              `json:"amount_cents"`
	Amount        string             `json:"amount"`
	RefundedCents int64              `json:"refunded_cents,omitempty"`
	Currency      string             `json:"currency"`
	Date          string             `json:"date"`
	DateLocal     string             `json:"date_local"`
	Card          *ReceiptCard       `json:"card,omitempty"`
	Reference     string             `json:"reference,omitempty"`
	Autofactura   ReceiptAutofactura `json:"autofactura"`
	Invoice       *ReceiptInvoice    `json:"invoice,omitempty"`
	ExpiresAt     string             `json:"expires_at"`
}

// autofacturaDeadline: the end of the sale's calendar month in
// America/Mexico_City (the SAT expects a nominative CFDI in the month of
// the sale; afterwards the sale goes to the factura global).
func autofacturaDeadline(sale store.SaleRecord) time.Time {
	loc := fiscal.MexicoCityLocation()
	l := sale.CreatedAt.In(loc)
	return time.Date(l.Year(), l.Month(), 1, 0, 0, 0, 0, loc).AddDate(0, 1, 0)
}

// autofacturaState evaluates the merchant's autofactura window for a sale.
func (s *Server) autofacturaState(ctx context.Context, rec store.ReceiptRecord, sale store.SaleRecord, now time.Time) (ReceiptAutofactura, error) {
	deadline := autofacturaDeadline(sale)
	out := ReceiptAutofactura{Deadline: deadline.UTC().Format(time.RFC3339)}
	fs, _, err := s.fiscalSettingsRepo().GetFiscalSettings(ctx, sale.MerchantID)
	if err != nil {
		return out, err
	}
	if fs.Autofactura == store.AutofacturaDisabled {
		out.Reason, out.Deadline = "DISABLED", ""
		return out, nil
	}
	if !now.Before(deadline) {
		out.Reason = "WINDOW_CLOSED"
		return out, nil
	}
	if _, err := s.invoiceableLine(ctx, sale); err != nil {
		var se *saleError
		if errors.As(err, &se) {
			out.Reason = "SALE_NOT_INVOICEABLE"
			return out, nil
		}
		return out, err
	}
	holders, err := s.invoiceRepo().ActiveInvoicesForSales(ctx, sale.MerchantID, []string{sale.ID})
	if err != nil {
		return out, err
	}
	if _, taken := holders[sale.ID]; taken {
		out.Reason = "ALREADY_INVOICED"
		return out, nil
	}
	out.Available = true
	return out, nil
}

func receiptStatus(sale store.SaleRecord) string {
	if sale.Kind != store.SaleCard {
		return "APPROVED"
	}
	switch store.CardPaymentStatus(sale.Status) {
	case store.CardCaptured, store.CardChargebackReversed:
		return "APPROVED"
	default:
		return sale.Status
	}
}

func (s *Server) receiptView(ctx context.Context, rec store.ReceiptRecord, sale store.SaleRecord, url string, now time.Time) (ReceiptView, error) {
	v := ReceiptView{Folio: sale.ID, Kind: sale.Kind, Status: receiptStatus(sale), AmountCents: sale.AmountCents,
		Amount: fiscal.FormatCents(sale.AmountCents), Currency: "MXN", Date: sale.CreatedAt.UTC().Format(time.RFC3339),
		DateLocal: sale.CreatedAt.In(fiscal.MexicoCityLocation()).Format("2006-01-02 15:04") + " (hora de la Ciudad de México)",
		ExpiresAt: rec.ExpiresAt.UTC().Format(time.RFC3339)}
	if m, ok := s.GetMerchant(sale.MerchantID); ok {
		v.MerchantName, v.MerchantRFC = m.Name, m.RFC
	}
	if p, _ := s.emisorProfile(ctx, sale.MerchantID); p.RFC != "" {
		v.MerchantRFC = p.RFC // KYB RFC (the CFDI emisor)
	}
	switch sale.Kind {
	case store.SaleCard:
		cp, _, err := s.loadCardPayment(ctx, sale.MerchantID, sale.ID)
		if err != nil {
			return v, err
		}
		v.AmountCents, v.Amount, v.RefundedCents = cp.CapturedCents, fiscal.FormatCents(cp.CapturedCents), cp.RefundedCents
		c := &ReceiptCard{Brand: cp.CardBrand, AuthCode: cp.AuthCode}
		if cp.CardLast4 != "" {
			c.MaskedPAN = "**** " + cp.CardLast4
		}
		v.Card = c
	case store.SaleSPEI:
		v.Reference = sale.Reference // clave de rastreo
	}
	af, err := s.autofacturaState(ctx, rec, sale, now)
	if err != nil {
		return v, err
	}
	if af.Available {
		af.URL = url + "/invoice"
	}
	v.Autofactura = af
	if rec.InvoiceID != "" {
		if inv, found, err := s.invoiceRepo().GetInvoice(ctx, sale.MerchantID, rec.InvoiceID); err == nil && found {
			v.Invoice = receiptInvoice(inv, url)
		}
	}
	return v, nil
}

func receiptInvoice(inv store.InvoiceRecord, url string) *ReceiptInvoice {
	out := &ReceiptInvoice{Status: inv.Status, UUID: inv.UUID, Error: inv.ErrorCode}
	if inv.UUID != "" && len(inv.XML) > 0 {
		out.PDFURL, out.XMLURL = url+"/invoice/pdf", url+"/invoice/xml"
	}
	return out
}

// publicReceipt resolves the token of a public request (404 for unknown,
// forged or expired tokens alike).
func (s *Server) publicReceipt(w http.ResponseWriter, r *http.Request) (store.ReceiptRecord, store.SaleRecord, string, bool) {
	secret, base, _, repo := s.receiptConfig()
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if len(secret) == 0 || repo == nil {
		writeError(w, http.StatusServiceUnavailable, "RECEIPTS_NOT_CONFIGURED")
		return store.ReceiptRecord{}, store.SaleRecord{}, "", false
	}
	tok := r.PathValue("token")
	now := nowUTC()
	id, exp, ok := decodeReceiptToken(secret, tok, now)
	var rec store.ReceiptRecord
	var found bool
	var err error
	if ok {
		rec, found, err = repo.GetReceipt(r.Context(), id)
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "RECEIPTS_UNAVAILABLE")
		return rec, store.SaleRecord{}, "", false
	}
	if !ok || !found || !rec.ExpiresAt.Equal(exp) {
		writeError(w, http.StatusNotFound, "RECEIPT_NOT_FOUND")
		return rec, store.SaleRecord{}, "", false
	}
	sales, okRepo := s.salesRepo()
	if !okRepo {
		writeError(w, http.StatusServiceUnavailable, "RECEIPTS_UNAVAILABLE")
		return rec, store.SaleRecord{}, "", false
	}
	list, err := sales.ListSales(r.Context(), store.SalesQuery{MerchantID: rec.MerchantID, ID: rec.SaleID, Limit: 1})
	if err != nil || len(list) == 0 {
		writeError(w, http.StatusNotFound, "RECEIPT_NOT_FOUND")
		return rec, store.SaleRecord{}, "", false
	}
	return rec, list[0], base + receiptPathPrefix + tok, true
}

func wantsJSON(r *http.Request) bool {
	return r.URL.Query().Get("format") == "json" || strings.Contains(r.Header.Get("Accept"), "application/json")
}

const receiptCSP = "default-src 'none'; style-src 'unsafe-inline'; form-action 'self'; base-uri 'none'; frame-ancestors 'none'"

// HandleGetReceipt: GET /r/{token} (public, rate limited): JSON
// (?format=json or Accept: application/json) or a simple HTML page with the
// autofactura form.
func (s *Server) HandleGetReceipt(w http.ResponseWriter, r *http.Request) {
	rec, sale, url, ok := s.publicReceipt(w, r)
	if !ok {
		return
	}
	v, err := s.receiptView(r.Context(), rec, sale, url, nowUTC())
	if err != nil {
		slog.Error("receipt view failed", "receipt_id", rec.ID, "error", err)
		writeError(w, http.StatusServiceUnavailable, "RECEIPTS_UNAVAILABLE")
		return
	}
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, v)
		return
	}
	renderReceiptHTML(w, http.StatusOK, receiptPage{View: v, Action: url + "/invoice"})
}

// HandleReceiptInvoice: POST /r/{token}/invoice (public, rate limited). The
// end customer submits receptor data (JSON or an HTML form) and gets an
// individual CFDI through the invoicing service. Idempotent per receipt and
// receptor data; 409 SALE_ALREADY_INVOICED when the sale is in an active
// invoice (individual or factura global); 422 AUTOFACTURA_NOT_AVAILABLE
// outside the merchant's window.
func (s *Server) HandleReceiptInvoice(w http.ResponseWriter, r *http.Request) {
	rec, sale, url, ok := s.publicReceipt(w, r)
	if !ok {
		return
	}
	form := strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded")
	reply := func(status int, body map[string]any) {
		if form {
			renderReceiptHTML(w, status, receiptPage{Result: body, Back: url})
			return
		}
		writeJSON(w, status, body)
	}
	var receptor fiscal.Receptor
	if form {
		if err := r.ParseForm(); err != nil {
			reply(http.StatusBadRequest, map[string]any{"error": "INVALID_FORM"})
			return
		}
		receptor = fiscal.Receptor{RFC: r.PostFormValue("rfc"), Nombre: r.PostFormValue("nombre"), RegimenFiscal: r.PostFormValue("regimen_fiscal"),
			CodigoPostal: r.PostFormValue("codigo_postal"), UsoCFDI: r.PostFormValue("uso_cfdi"), Email: r.PostFormValue("email")}
	} else if err := json.NewDecoder(io.LimitReader(r.Body, 8<<10)).Decode(&receptor); err != nil {
		reply(http.StatusBadRequest, map[string]any{"error": "INVALID_JSON"})
		return
	}
	receptor = receptor.Normalize()
	ctx := r.Context()
	key := "autofactura:" + rec.ID + ":" + requestHash(receptor)[:16]
	req := CreateInvoiceRequest{SaleIDs: []string{sale.ID}, Receptor: receptor}
	if existing, found, err := s.invoiceRepo().FindInvoiceByIdempotencyKey(ctx, sale.MerchantID, key); err == nil && found {
		reply(http.StatusCreated, publicInvoiceBody(existing, url))
		return
	}
	if active, code, err := s.merchantActive(ctx, sale.MerchantID); err != nil || !active {
		slog.Warn("autofactura refused: merchant not active", "merchant_id", sale.MerchantID, "code", code, "error", err)
		reply(http.StatusUnprocessableEntity, map[string]any{"error": "AUTOFACTURA_NOT_AVAILABLE", "reason": "MERCHANT_UNAVAILABLE"})
		return
	}
	af, err := s.autofacturaState(ctx, rec, sale, nowUTC())
	if err != nil {
		reply(http.StatusServiceUnavailable, map[string]any{"error": "AUTOFACTURA_UNAVAILABLE"})
		return
	}
	switch {
	case af.Reason == "ALREADY_INVOICED":
		reply(http.StatusConflict, map[string]any{"error": "SALE_ALREADY_INVOICED"})
		return
	case !af.Available:
		reply(http.StatusUnprocessableEntity, map[string]any{"error": "AUTOFACTURA_NOT_AVAILABLE", "reason": af.Reason})
		return
	}
	inv, err := s.issueIndividualInvoice(ctx, sale.MerchantID, key, req)
	var re *invoiceRequestError
	var si *store.SaleInvoicedError
	var se *saleError
	var fp errFiscalProfile
	switch {
	case errors.As(err, &re):
		body := map[string]any{"error": "AUTOFACTURA_REJECTED"}
		if m, ok := re.body.(map[string]any); ok {
			body = m
		}
		reply(re.status, body)
		return
	case errors.As(err, &si):
		reply(http.StatusConflict, map[string]any{"error": "SALE_ALREADY_INVOICED"})
		return
	case errors.As(err, &se):
		reply(http.StatusUnprocessableEntity, map[string]any{"error": "AUTOFACTURA_NOT_AVAILABLE", "reason": "SALE_NOT_INVOICEABLE"})
		return
	case errors.As(err, &fp), errors.Is(err, errCSDNotFound), errors.Is(err, errCSDExpired):
		slog.Warn("autofactura refused: merchant fiscal setup", "merchant_id", sale.MerchantID, "error", err)
		reply(http.StatusServiceUnavailable, map[string]any{"error": "AUTOFACTURA_UNAVAILABLE"})
		return
	case err != nil:
		slog.Error("autofactura failed", "receipt_id", rec.ID, "error", err)
		reply(http.StatusServiceUnavailable, map[string]any{"error": "AUTOFACTURA_UNAVAILABLE"})
		return
	}
	_, _, _, repo := s.receiptConfig()
	if err := repo.SetReceiptInvoice(ctx, rec.ID, inv.ID); err != nil {
		slog.Error("receipt invoice not recorded", "receipt_id", rec.ID, "invoice_id", inv.ID, "error", err)
	}
	slog.Info("autofactura issued", "merchant_id", sale.MerchantID, "sale_id", sale.ID, "invoice_id", inv.ID, "status", inv.Status)
	reply(http.StatusCreated, publicInvoiceBody(inv, url))
}

// publicInvoiceBody is what the end customer sees of their invoice.
func publicInvoiceBody(inv store.InvoiceRecord, url string) map[string]any {
	ri := receiptInvoice(inv, url)
	out := map[string]any{"invoice_id": inv.ID, "status": inv.Status, "total_cents": inv.TotalCents}
	if ri.UUID != "" {
		out["uuid"] = ri.UUID
	}
	if ri.PDFURL != "" {
		out["pdf_url"], out["xml_url"] = ri.PDFURL, ri.XMLURL
	}
	if inv.ErrorCode != "" {
		out["error"] = inv.ErrorCode
	}
	return out
}

// HandleReceiptInvoiceDoc: GET /r/{token}/invoice/{doc} (pdf | xml) of the
// CFDI requested through this receipt.
func (s *Server) HandleReceiptInvoiceDoc(w http.ResponseWriter, r *http.Request) {
	rec, sale, _, ok := s.publicReceipt(w, r)
	if !ok {
		return
	}
	doc := r.PathValue("doc")
	if (doc != "pdf" && doc != "xml") || rec.InvoiceID == "" {
		writeError(w, http.StatusNotFound, "INVOICE_NOT_FOUND")
		return
	}
	inv, found, err := s.invoiceRepo().GetInvoice(r.Context(), sale.MerchantID, rec.InvoiceID)
	if err != nil || !found {
		writeError(w, http.StatusNotFound, "INVOICE_NOT_FOUND")
		return
	}
	if !stampedDoc(w, inv) {
		return
	}
	if doc == "xml" {
		w.Header().Set("Content-Type", "application/xml; charset=utf-8")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.xml"`, inv.UUID))
		_, _ = w.Write(inv.XML)
		return
	}
	pdf, err := cfdipdf.Render(inv.XML)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "PDF_RENDER_FAILED")
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`inline; filename="%s.pdf"`, inv.UUID))
	_, _ = w.Write(pdf)
}

// ---------------------------------------------------------------------------
// Merchant side: send the receipt

// SendReceiptRequest is POST /api/v1/merchants/{id}/sales/{sale_id}/receipt.
type SendReceiptRequest struct {
	Email string `json:"email,omitempty"`
	Phone string `json:"phone,omitempty"` // E.164, e.g. +525512345678
}

// SendReceiptResponse reports where the receipt went.
type SendReceiptResponse struct {
	ReceiptURL string          `json:"receipt_url"`
	ExpiresAt  string          `json:"expires_at"`
	Sent       map[string]bool `json:"sent"`
}

// HandleSendReceipt: owner or cashier. Sends the receipt link by email
// (Mailer port) and/or SMS (SMSSender port).
func (s *Server) HandleSendReceipt(w http.ResponseWriter, r *http.Request) {
	tenant, ok := requireTenant(w, r, r.PathValue("id"))
	if !ok || !requireRole(w, r, auth.RoleOwner, auth.RoleCashier) {
		return
	}
	var req SendReceiptRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON")
		return
	}
	req.Email, req.Phone = strings.TrimSpace(req.Email), strings.TrimSpace(req.Phone)
	bad := map[string]string{}
	if req.Email != "" {
		if a, err := mail.ParseAddress(req.Email); err != nil || a.Address != req.Email || len(req.Email) > 254 {
			bad["email"] = fiscal.ReasonInvalidFormat
		}
	}
	if req.Phone != "" && !auth.ValidPhone(req.Phone) {
		bad["phone"] = fiscal.ReasonInvalidFormat
	}
	if req.Email == "" && req.Phone == "" {
		bad["email"] = fiscal.ReasonRequired
	}
	if len(bad) > 0 {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": "RECEIPT_DESTINATION_INVALID", "fields": bad})
		return
	}
	sale, ok := s.loadSale(w, r, tenant, r.PathValue("sale_id"))
	if !ok {
		return
	}
	ctx := r.Context()
	rec, url, err := s.ensureReceipt(ctx, sale)
	if !writeReceiptError(w, err) {
		return
	}
	v, err := s.receiptView(ctx, rec, sale, url, nowUTC())
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "RECEIPTS_UNAVAILABLE")
		return
	}
	text := receiptText(v, url)
	out := SendReceiptResponse{ReceiptURL: url, ExpiresAt: rec.ExpiresAt.UTC().Format(time.RFC3339), Sent: map[string]bool{}}
	failed := false
	if req.Email != "" {
		err := s.mailSender().Send(ctx, Mail{To: req.Email, Subject: "Comprobante de venta - " + v.MerchantName, Body: text})
		out.Sent["email"], failed = err == nil, failed || err != nil
		if err != nil {
			slog.Error("receipt email failed", "receipt_id", rec.ID, "error", err)
		}
	}
	if req.Phone != "" {
		err := s.smsSender().SendSMS(ctx, req.Phone, fmt.Sprintf("%s: comprobante de su compra por $%s MXN: %s", v.MerchantName, v.Amount, url))
		out.Sent["sms"], failed = err == nil, failed || err != nil
		if err != nil {
			slog.Error("receipt sms failed", "receipt_id", rec.ID, "error", err)
		}
	}
	status := http.StatusOK
	if failed {
		status = http.StatusBadGateway
	}
	writeJSON(w, status, out)
}

// writeReceiptError maps ensureReceipt errors; true = no error.
func writeReceiptError(w http.ResponseWriter, err error) bool {
	switch {
	case err == nil:
		return true
	case errors.Is(err, errReceiptsNotConfigured):
		writeError(w, http.StatusServiceUnavailable, "RECEIPTS_NOT_CONFIGURED")
	case errors.Is(err, errSaleNotConfirmed):
		writeError(w, http.StatusConflict, "SALE_NOT_CONFIRMED")
	default:
		slog.Error("receipt not created", "error", err)
		writeError(w, http.StatusServiceUnavailable, "RECEIPTS_UNAVAILABLE")
	}
	return false
}

// receiptText is the plain-text receipt (email body).
func receiptText(v ReceiptView, url string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Comprobante de venta\n%s", v.MerchantName)
	if v.MerchantRFC != "" {
		fmt.Fprintf(&b, " (RFC %s)", v.MerchantRFC)
	}
	fmt.Fprintf(&b, "\nImporte: $%s %s\nFecha: %s\nFolio: %s\n", v.Amount, v.Currency, v.DateLocal, v.Folio)
	if v.Card != nil {
		fmt.Fprintf(&b, "Tarjeta: %s %s\nAutorización: %s\n", strings.ToUpper(v.Card.Brand), v.Card.MaskedPAN, v.Card.AuthCode)
	}
	if v.Reference != "" {
		fmt.Fprintf(&b, "Referencia: %s\n", v.Reference)
	}
	fmt.Fprintf(&b, "\nConsulte su comprobante")
	if v.Autofactura.Available {
		fmt.Fprintf(&b, " y solicite su factura")
	}
	fmt.Fprintf(&b, " en: %s\n", url)
	return b.String()
}

// ---------------------------------------------------------------------------
// HTML

type receiptPage struct {
	View   ReceiptView
	Action string
	Result map[string]any
	Back   string
}

var receiptTemplate = template.Must(template.New("receipt").Funcs(template.FuncMap{"upper": strings.ToUpper}).Parse(`<!doctype html>
<html lang="es"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="robots" content="noindex"><title>Comprobante de venta</title>
<style>body{font-family:system-ui,sans-serif;max-width:32rem;margin:1rem auto;padding:0 1rem;color:#111}
dl{display:grid;grid-template-columns:auto 1fr;gap:.25rem 1rem}dt{color:#555}label{display:block;margin:.5rem 0 .1rem}
input{width:100%;padding:.4rem;box-sizing:border-box}button{margin-top:1rem;padding:.6rem 1rem}.muted{color:#555}</style></head><body>
{{if .Result}}<h1>Factura</h1>
{{with .Result}}{{if .error}}<p>No fue posible emitir la factura: <strong>{{.error}}</strong> {{.reason}}</p>
{{else}}<p>Factura {{.status}}{{if .uuid}} — UUID {{.uuid}}{{end}}.</p>{{if .pdf_url}}<p><a href="{{.pdf_url}}">PDF</a> · <a href="{{.xml_url}}">XML</a></p>{{end}}{{end}}{{end}}
<p><a href="{{.Back}}">Volver al comprobante</a></p>
{{else}}{{with .View}}<h1>Comprobante de venta</h1>
<p><strong>{{.MerchantName}}</strong>{{if .MerchantRFC}}<br><span class="muted">RFC {{.MerchantRFC}}</span>{{end}}</p>
<dl><dt>Importe</dt><dd>${{.Amount}} {{.Currency}}</dd><dt>Fecha</dt><dd>{{.DateLocal}}</dd><dt>Estado</dt><dd>{{.Status}}</dd>
{{with .Card}}<dt>Tarjeta</dt><dd>{{upper .Brand}} {{.MaskedPAN}}</dd><dt>Autorización</dt><dd>{{.AuthCode}}</dd>{{end}}
{{if .Reference}}<dt>Referencia</dt><dd>{{.Reference}}</dd>{{end}}<dt>Folio</dt><dd>{{.Folio}}</dd></dl>
{{with .Invoice}}<p>Factura: {{.Status}}{{if .UUID}} — UUID {{.UUID}}{{end}}{{if .PDFURL}} · <a href="{{.PDFURL}}">PDF</a> · <a href="{{.XMLURL}}">XML</a>{{end}}</p>{{end}}
{{end}}{{if .View.Autofactura.Available}}<h2>Solicitar factura</h2>
<form method="post" action="{{.Action}}">
<label for="rfc">RFC</label><input id="rfc" name="rfc" required maxlength="13">
<label for="nombre">Nombre o razón social</label><input id="nombre" name="nombre" required maxlength="254">
<label for="regimen_fiscal">Régimen fiscal (clave)</label><input id="regimen_fiscal" name="regimen_fiscal" required maxlength="3">
<label for="codigo_postal">Código postal fiscal</label><input id="codigo_postal" name="codigo_postal" required maxlength="5">
<label for="uso_cfdi">Uso del CFDI (clave)</label><input id="uso_cfdi" name="uso_cfdi" required maxlength="4">
<label for="email">Correo (opcional)</label><input id="email" name="email" type="email" maxlength="254">
<button type="submit">Facturar</button></form>
<p class="muted">Disponible hasta {{.View.Autofactura.Deadline}}.</p>
{{else}}<p class="muted">Autofactura no disponible{{with .View.Autofactura.Reason}} ({{.}}){{end}}.</p>{{end}}{{end}}
</body></html>`))

func renderReceiptHTML(w http.ResponseWriter, status int, page receiptPage) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Security-Policy", receiptCSP)
	w.WriteHeader(status)
	if err := receiptTemplate.Execute(w, page); err != nil {
		slog.Error("receipt page render failed", "error", err)
	}
}
