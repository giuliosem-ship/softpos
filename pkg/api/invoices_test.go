package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"softpos/pkg/acquirer"
	"softpos/pkg/auth"
	"softpos/pkg/fiscal"
	"softpos/pkg/fiscal/fiscaltest"
	"softpos/pkg/pac"
	"softpos/pkg/store"
)

const invoiceCLABE = "646180123400000007"

type captureMailer struct {
	mu   sync.Mutex
	sent []Mail
}

func (c *captureMailer) Send(_ context.Context, m Mail) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, m)
	return nil
}

type invoiceFixture struct {
	s      *Server
	mock   *acquirer.MockProvider
	sat    *pac.MockSAT
	mailer *captureMailer
}

// setupInvoiceServer: approved RESICO PF merchant (cardMerchant) with CSD,
// fiscal settings (CP 06000), CLABE and webhooks for card/SPEI/CoDi sales.
func setupInvoiceServer(t *testing.T) invoiceFixture {
	t.Helper()
	s, mock := setupWebhookCardServer(t)
	s.SetCoDiWebhookSecret(testCoDiWebhookSecret)
	if err := s.RegisterMerchantCLABE(invoiceCLABE, cardMerchant, "GARM850101ABC"); err != nil {
		t.Fatal(err)
	}
	c, err := fiscaltest.NewCSD(fiscaltest.CSDOptions{RFC: "GARM850101ABC"})
	if err != nil {
		t.Fatal(err)
	}
	enc, _ := s.vault().StoreCSDBytes(cardMerchant, c.CertDER, c.EncryptedKey, c.Password)
	b, _ := json.Marshal(enc)
	if err := s.merchantRepo.SaveMerchantCSD(context.Background(), cardMerchant, b); err != nil {
		t.Fatal(err)
	}
	if err := s.SeedFiscalSettings(context.Background(), cardMerchant, "06000"); err != nil {
		t.Fatal(err)
	}
	sat := pac.NewMockSAT()
	s.SetPACClient(pac.NewMultiPACOrchestrator(pac.NewMockPACClientWithSAT(false, "", sat), pac.NewMockPACClientWithSAT(false, "", sat), time.Second))
	mailer := &captureMailer{}
	s.SetMailer(mailer)
	return invoiceFixture{s: s, mock: mock, sat: sat, mailer: mailer}
}

func (f invoiceFixture) speiSale(t *testing.T, clave string, cents int64) string {
	t.Helper()
	if rec := serve(f.s, jsonReq(http.MethodPost, "/api/v1/webhooks/stp/abono", signedAbono(t, f.s, clave, invoiceCLABE, cents))); rec.Code != http.StatusOK {
		t.Fatalf("abono: %d %s", rec.Code, rec.Body.String())
	}
	return "spei_40012:" + clave
}

func pmReceptor() map[string]string {
	return map[string]string{"rfc": "EKU9003173C9", "nombre": "Escuela Kemper Urgate", "regimen_fiscal": "601",
		"codigo_postal": "26015", "uso_cfdi": "G03", "email": "facturas@example.com"}
}

func createInvoice(t *testing.T, s *Server, role auth.Role, key string, body any) (int, InvoiceResponse, string) {
	t.Helper()
	req := jsonReq(http.MethodPost, "/api/v1/merchants/"+cardMerchant+"/invoices", body)
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	rec := serve(s, asRole(t, s, cardMerchant, role, req))
	var out InvoiceResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out, rec.Body.String()
}

func cancelInvoice(t *testing.T, s *Server, role auth.Role, id string, body any) (int, InvoiceResponse, string) {
	t.Helper()
	rec := serve(s, asRole(t, s, cardMerchant, role, jsonReq(http.MethodPost, "/api/v1/merchants/"+cardMerchant+"/invoices/"+id+"/cancel", body)))
	var out InvoiceResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out, rec.Body.String()
}

// TestInvoices_CreateFromSalesStampDocsAndIdempotency: amounts come from the
// posted split (card settlement, SPEI 16 % split), never from the client.
func TestInvoices_CreateFromSalesStampDocsAndIdempotency(t *testing.T) {
	f := setupInvoiceServer(t)
	s := f.s
	card := capturedCardPayment(t, s, f.mock, "inv-card-1", 11600).CardPaymentID
	spei := f.speiSale(t, "SPEIINV0001", 50000)

	if code, _, _ := createInvoice(t, s, auth.RoleCashier, "", map[string]any{"sale_ids": []string{card}, "receptor": pmReceptor()}); code != http.StatusBadRequest {
		t.Fatalf("missing idempotency key: %d", code)
	}
	bad := pmReceptor()
	bad["regimen_fiscal"], bad["uso_cfdi"], bad["codigo_postal"] = "612", "D01", "123"
	code, _, body := createInvoice(t, s, auth.RoleCashier, "k-bad", map[string]any{"sale_ids": []string{card}, "receptor": bad})
	if code != http.StatusUnprocessableEntity || !strings.Contains(body, `"RECEPTOR_INVALID"`) || !strings.Contains(body, `"regimen_fiscal":"NOT_ALLOWED_FOR_PERSONA_TYPE"`) || !strings.Contains(body, `"codigo_postal"`) {
		t.Fatalf("receptor validation: %d %s", code, body)
	}
	if code, _, body := createInvoice(t, s, auth.RoleCashier, "k-nosale", map[string]any{"sale_ids": []string{"nope"}, "receptor": pmReceptor()}); code != http.StatusUnprocessableEntity || !strings.Contains(body, "SALE_NOT_FOUND") {
		t.Fatalf("unknown sale: %d %s", code, body)
	}

	req := map[string]any{"sale_ids": []string{card, spei}, "receptor": pmReceptor(), "total_cents": 1}
	code, inv, body := createInvoice(t, s, auth.RoleCashier, "k-1", req)
	if code != http.StatusCreated || inv.Status != "STAMPED" || inv.UUID == "" || inv.Serie != "K" || inv.Folio != "1" || inv.Kind != "INDIVIDUAL" {
		t.Fatalf("create: %d %s", code, body)
	}
	speiBase, speiIVA := fiscal.IVAFromGross(50000)
	cardBase, cardIVA := fiscal.IVAFromGross(11600)
	isr := (cardBase*125+5000)/10000 + (speiBase*125+5000)/10000 // PM receptor of a RESICO PF: 1.25 % ISR
	if inv.SubtotalCents != cardBase+speiBase || inv.IVACents != cardIVA+speiIVA || inv.ISRRetenidoCents != isr || inv.TotalCents != 61600-isr {
		t.Fatalf("amounts must come from the posted split: %+v", inv)
	}
	if len(inv.SaleIDs) != 2 || !strings.Contains(string(inv.Receptor), `"rfc":"EKU9003173C9"`) || !strings.Contains(string(inv.Receptor), `"nombre":"ESCUELA KEMPER URGATE"`) {
		t.Fatalf("receptor / sales: %s", body)
	}
	// Idempotent replay; same key with another body -> 409.
	if code, again, _ := createInvoice(t, s, auth.RoleCashier, "k-1", req); code != http.StatusCreated || again.InvoiceID != inv.InvoiceID {
		t.Fatalf("replay: %d %+v", code, again)
	}
	if code, _, body := createInvoice(t, s, auth.RoleCashier, "k-1", map[string]any{"sale_ids": []string{card}, "receptor": pmReceptor()}); code != http.StatusConflict || !strings.Contains(body, "IDEMPOTENCY_KEY_REUSED") {
		t.Fatalf("key reuse: %d %s", code, body)
	}
	// A sale is in at most one active invoice.
	if code, _, body := createInvoice(t, s, auth.RoleOwner, "k-2", map[string]any{"sale_ids": []string{spei}, "receptor": pmReceptor()}); code != http.StatusConflict || !strings.Contains(body, "SALE_ALREADY_INVOICED") || !strings.Contains(body, inv.InvoiceID) {
		t.Fatalf("double invoicing: %d %s", code, body)
	}

	// XML: sealed (sello verifies), stamped, CFDI 4.0 data.
	rec := serve(s, asRole(t, s, cardMerchant, auth.RoleCashier, httptest.NewRequest(http.MethodGet, "/api/v1/merchants/"+cardMerchant+"/invoices/"+inv.InvoiceID+"/xml", nil)))
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/xml") {
		t.Fatalf("xml: %d", rec.Code)
	}
	x := rec.Body.String()
	for _, want := range []string{`UUID="` + inv.UUID + `"`, `Rfc="EKU9003173C9"`, `TotalImpuestosRetenidos="`, `NoIdentificacion="` + spei + `"`, `FormaPago="03"`, `LugarExpedicion="06000"`} {
		if !strings.Contains(x, want) {
			t.Errorf("xml misses %s", want)
		}
	}
	rec = serve(s, asTenant(t, s, cardMerchant, httptest.NewRequest(http.MethodGet, "/api/v1/merchants/"+cardMerchant+"/invoices/"+inv.InvoiceID+"/pdf", nil)))
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "application/pdf" || !strings.HasPrefix(rec.Body.String(), "%PDF") || !strings.Contains(rec.Body.String(), inv.UUID) {
		t.Fatalf("pdf: %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	if len(f.mailer.sent) != 1 || f.mailer.sent[0].To != "facturas@example.com" || len(f.mailer.sent[0].Attachments) != 2 {
		t.Fatalf("email: %+v", f.mailer.sent)
	}
	// Listing (any member) and tenant isolation.
	rec = serve(s, asRole(t, s, cardMerchant, auth.RoleCashier, httptest.NewRequest(http.MethodGet, "/api/v1/merchants/"+cardMerchant+"/invoices?limit=1", nil)))
	var page InvoicePage
	_ = json.Unmarshal(rec.Body.Bytes(), &page)
	if rec.Code != http.StatusOK || len(page.Items) != 1 || page.Items[0].InvoiceID != inv.InvoiceID {
		t.Fatalf("list: %d %s", rec.Code, rec.Body.String())
	}
	if rec := serve(s, asTenant(t, s, "merch_other", httptest.NewRequest(http.MethodGet, "/api/v1/merchants/"+cardMerchant+"/invoices/"+inv.InvoiceID, nil))); rec.Code != http.StatusForbidden {
		t.Fatalf("other tenant: %d", rec.Code)
	}
}

// TestInvoices_CancellationMotivosAndAcceptance covers motivo 02 with
// receptor acceptance (poller), a rejected request, and motivo 01 with a
// stamped replacement that takes over the sales.
func TestInvoices_CancellationMotivosAndAcceptance(t *testing.T) {
	f := setupInvoiceServer(t)
	s := f.s
	big := f.speiSale(t, "SPEICAN0001", 500000) // > $1,000: needs the receptor's acceptance
	small := f.speiSale(t, "SPEICAN0002", 50000)

	_, inv, body := createInvoice(t, s, auth.RoleOwner, "c-1", map[string]any{"sale_ids": []string{big}, "receptor": pmReceptor()})
	if inv.Status != "STAMPED" {
		t.Fatalf("create: %s", body)
	}
	if code, _, _ := cancelInvoice(t, s, auth.RoleCashier, inv.InvoiceID, map[string]string{"motivo": "02"}); code != http.StatusForbidden {
		t.Fatalf("cashier cancel: %d", code)
	}
	for _, c := range []map[string]string{{"motivo": "09"}, {"motivo": "01"}, {"motivo": "02", "folio_sustitucion": "X"}, {"motivo": "04"}} {
		if code, _, body := cancelInvoice(t, s, auth.RoleOwner, inv.InvoiceID, c); code != http.StatusUnprocessableEntity || !strings.Contains(body, "CANCEL_INVALID") {
			t.Fatalf("invalid cancel %v: %d %s", c, code, body)
		}
	}
	code, out, body := cancelInvoice(t, s, auth.RoleOwner, inv.InvoiceID, map[string]string{"motivo": "02"})
	if code != http.StatusAccepted || out.Status != "CANCEL_REQUESTED" || out.Cancellation == nil || out.Cancellation.Status != "REQUESTED" {
		t.Fatalf("cancel request: %d %s", code, body)
	}
	if code, _, _ := cancelInvoice(t, s, auth.RoleOwner, inv.InvoiceID, map[string]string{"motivo": "02"}); code != http.StatusConflict {
		t.Fatalf("second cancel: %d", code)
	}
	// Receptor rejects: back to STAMPED with the rejection recorded.
	f.sat.ResolveCancellation(inv.UUID, false)
	if n, err := s.PollCancellations(context.Background()); err != nil || n != 1 {
		t.Fatalf("poll: %d %v", n, err)
	}
	got, _, _ := s.invoiceRepo().GetInvoice(context.Background(), cardMerchant, inv.InvoiceID)
	if got.Status != store.InvoiceStamped || got.CancelStatus != store.CancelRejected {
		t.Fatalf("rejected: %+v", got)
	}
	// Second request, accepted.
	if code, _, body := cancelInvoice(t, s, auth.RoleOwner, inv.InvoiceID, map[string]string{"motivo": "03"}); code != http.StatusAccepted {
		t.Fatalf("re-request: %d %s", code, body)
	}
	if n, _ := s.PollCancellations(context.Background()); n != 0 {
		t.Fatal("nothing to resolve while pending")
	}
	f.sat.ResolveCancellation(inv.UUID, true)
	if n, _ := s.PollCancellations(context.Background()); n != 1 {
		t.Fatal("accepted cancellation not resolved")
	}
	got, _, _ = s.invoiceRepo().GetInvoice(context.Background(), cardMerchant, inv.InvoiceID)
	if got.Status != store.InvoiceCanceled || got.CancelStatus != store.CancelAccepted || got.CancelResolvedAt == nil {
		t.Fatalf("canceled: %+v", got)
	}
	// A canceled invoice releases its sales.
	if code, again, body := createInvoice(t, s, auth.RoleOwner, "c-2", map[string]any{"sale_ids": []string{big}, "receptor": pmReceptor()}); code != http.StatusCreated || again.Status != "STAMPED" {
		t.Fatalf("re-invoice after cancel: %d %s", code, body)
	}

	// Motivo 01: replacement first (takes over the sale), then cancel.
	_, orig, _ := createInvoice(t, s, auth.RoleOwner, "c-3", map[string]any{"sale_ids": []string{small}, "receptor": pmReceptor()})
	fixed := pmReceptor()
	fixed["uso_cfdi"] = "G01"
	code, repl, body := createInvoice(t, s, auth.RoleOwner, "c-4", map[string]any{"sale_ids": []string{small}, "receptor": fixed, "replaces_invoice_id": orig.InvoiceID})
	if code != http.StatusCreated || repl.Status != "STAMPED" || repl.ReplacesInvoiceID != orig.InvoiceID {
		t.Fatalf("replacement: %d %s", code, body)
	}
	rx, _, _ := s.invoiceRepo().GetInvoice(context.Background(), cardMerchant, repl.InvoiceID)
	if !strings.Contains(string(rx.XML), `TipoRelacion="04"`) || !strings.Contains(string(rx.XML), `UUID="`+orig.UUID+`"`) {
		t.Fatal("replacement must relate the original UUID (TipoRelacion 04)")
	}
	if code, _, body := cancelInvoice(t, s, auth.RoleOwner, orig.InvoiceID, map[string]string{"motivo": "01", "folio_sustitucion": "00000000-0000-4000-8000-000000000000"}); code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown replacement: %d %s", code, body)
	}
	code, out, body = cancelInvoice(t, s, auth.RoleOwner, orig.InvoiceID, map[string]string{"motivo": "01", "folio_sustitucion": strings.ToLower(repl.UUID)})
	if code != http.StatusOK || out.Status != "CANCELED" || out.Cancellation.FolioSustitucion != repl.UUID {
		t.Fatalf("motivo 01 (<= $1,000, no acceptance): %d %s", code, body)
	}
	holders, _ := s.invoiceRepo().ActiveInvoicesForSales(context.Background(), cardMerchant, []string{small})
	if holders[small] != repl.InvoiceID {
		t.Fatalf("the replacement must keep the sale: %v", holders)
	}
}

// TestInvoices_GlobalInvoiceWorker issues the monthly factura global for the
// sales not invoiced individually; those sales cannot be invoiced later.
func TestInvoices_GlobalInvoiceWorker(t *testing.T) {
	f := setupInvoiceServer(t)
	s := f.s
	card := capturedCardPayment(t, s, f.mock, "glob-card", 23200).CardPaymentID
	s1 := f.speiSale(t, "SPEIGLOB001", 11600)
	s2 := f.speiSale(t, "SPEIGLOB002", 5800)
	// An intent never captured (no money) is not invoiceable.
	if code, _, body := createIntent(t, s, cardMerchant, "glob-uncaptured", map[string]any{"amount_cents": 1000}); code != http.StatusCreated {
		t.Fatalf("intent: %d %s", code, body)
	}
	if code, _, body := createInvoice(t, s, auth.RoleOwner, "g-ind", map[string]any{"sale_ids": []string{s2}, "receptor": pmReceptor()}); code != http.StatusCreated {
		t.Fatalf("individual: %d %s", code, body)
	}

	now := time.Now()
	if n, err := s.RunGlobalInvoices(context.Background(), now); err != nil || n != 0 {
		t.Fatalf("current month is open: %d %v", n, err)
	}
	nextMonth := now.AddDate(0, 1, 5)
	n, err := s.RunGlobalInvoices(context.Background(), nextMonth)
	if err != nil || n != 1 {
		t.Fatalf("global run: %d %v", n, err)
	}
	globals, _ := s.invoiceRepo().ListInvoices(context.Background(), store.InvoiceQuery{MerchantID: cardMerchant, Kind: store.InvoiceGlobal})
	if len(globals) != 1 || globals[0].Status != store.InvoiceStamped || globals[0].Serie != "G" {
		t.Fatalf("globals: %+v", globals)
	}
	g := globals[0]
	ids := strings.Join(g.SaleIDs, ",")
	if !strings.Contains(ids, card) || !strings.Contains(ids, s1) || strings.Contains(ids, s2) || len(g.SaleIDs) != 2 {
		t.Fatalf("global must cover the captured, non-invoiced sales: %v", g.SaleIDs)
	}
	month := fiscal.ResicoYearMonth(now)
	xml := string(g.XML)
	for _, want := range []string{`Periodicidad="04"`, `Meses="` + month[5:] + `"`, `Rfc="XAXX010101000"`, `DomicilioFiscalReceptor="06000"`, `ClaveProdServ="01010101"`, `ClaveUnidad="ACT"`} {
		if !strings.Contains(xml, want) {
			t.Errorf("global xml misses %s", want)
		}
	}
	if g.GlobalPeriod != "M"+month {
		t.Fatalf("period %s", g.GlobalPeriod)
	}
	// Rerun: nothing new. A sale already in the global cannot be invoiced.
	if n, _ := s.RunGlobalInvoices(context.Background(), nextMonth); n != 0 {
		t.Fatal("global issued twice")
	}
	if code, _, body := createInvoice(t, s, auth.RoleOwner, "g-late", map[string]any{"sale_ids": []string{s1}, "receptor": pmReceptor()}); code != http.StatusConflict || !strings.Contains(body, g.ID) {
		t.Fatalf("sale in global: %d %s", code, body)
	}
	// Motivo 04 is for globals; it releases the sales (a new global is issued).
	if code, out, body := cancelInvoice(t, s, auth.RoleOwner, g.ID, map[string]string{"motivo": "04"}); code != http.StatusOK || out.Status != "CANCELED" {
		t.Fatalf("cancel global: %d %s", code, body)
	}
	if n, _ := s.RunGlobalInvoices(context.Background(), nextMonth); n != 1 {
		t.Fatal("re-issue after canceling the global")
	}

	// DAILY periodicity: yesterday's sales.
	fs, _, _ := s.fiscalSettingsRepo().GetFiscalSettings(context.Background(), cardMerchant)
	fs.GlobalPeriodicity = store.GlobalDaily
	_ = s.fiscalSettingsRepo().SaveFiscalSettings(context.Background(), fs)
	s3 := f.speiSale(t, "SPEIGLOB003", 2000)
	if n, _ := s.RunGlobalInvoices(context.Background(), now.AddDate(0, 0, 1)); n != 1 {
		t.Fatal("daily global")
	}
	daily, _ := s.invoiceRepo().ListInvoices(context.Background(), store.InvoiceQuery{MerchantID: cardMerchant, Kind: store.InvoiceGlobal, Limit: 1})
	if !strings.Contains(string(daily[0].XML), `Periodicidad="01"`) || len(daily[0].SaleIDs) != 1 || daily[0].SaleIDs[0] != s3 {
		t.Fatalf("daily: %v", daily[0].SaleIDs)
	}
}

// TestInvoices_PACFailuresAndBackofficeRetry: PAC outage -> PENDING; SAT
// rejection -> FAILED (sales released); operators list and retry (audited).
func TestInvoices_PACFailuresAndBackofficeRetry(t *testing.T) {
	f := setupInvoiceServer(t)
	s := f.s
	down := pac.NewMultiPACOrchestrator(pac.NewMockPACClientWithSAT(true, "", f.sat), pac.NewMockPACClientWithSAT(true, "", f.sat), 100*time.Millisecond)
	s.SetPACClient(down)
	sale := f.speiSale(t, "SPEIPAC0001", 11600)
	code, inv, body := createInvoice(t, s, auth.RoleOwner, "p-1", map[string]any{"sale_ids": []string{sale}, "receptor": pmReceptor()})
	if code != http.StatusCreated || inv.Status != "PENDING" || inv.Error != "PAC_UNAVAILABLE" || inv.UUID != "" {
		t.Fatalf("pac down: %d %s", code, body)
	}
	if rec := serve(s, asTenant(t, s, cardMerchant, httptest.NewRequest(http.MethodGet, "/api/v1/merchants/"+cardMerchant+"/invoices/"+inv.InvoiceID+"/pdf", nil))); rec.Code != http.StatusConflict {
		t.Fatalf("pdf of a pending invoice: %d", rec.Code)
	}
	tok, _ := operatorToken(t, s, "+525500000077")
	rec := boCall(s, tok, http.MethodGet, "/api/v1/backoffice/invoices", nil)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), inv.InvoiceID) {
		t.Fatalf("backoffice list: %d %s", rec.Code, rec.Body.String())
	}
	s.SetPACClient(pac.NewMultiPACOrchestrator(pac.NewMockPACClientWithSAT(false, "", f.sat), nil, time.Second))
	rec = boCall(s, tok, http.MethodPost, "/api/v1/backoffice/invoices/"+inv.InvoiceID+"/retry", nil)
	var out InvoiceResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusOK || out.Status != "STAMPED" || out.UUID == "" {
		t.Fatalf("retry: %d %s", rec.Code, rec.Body.String())
	}
	if got := auditActions(t, s, cardMerchant); len(got) == 0 || got[len(got)-1] != "invoice.retry" {
		t.Fatalf("audit: %v", got)
	}
	if rec := boCall(s, tok, http.MethodPost, "/api/v1/backoffice/invoices/"+inv.InvoiceID+"/retry", nil); rec.Code != http.StatusConflict {
		t.Fatalf("retry of a stamped invoice: %d", rec.Code)
	}

	// Deterministic SAT rejection: FAILED and the sale is free again.
	s.SetPACClient(pac.NewMultiPACOrchestrator(pac.NewMockPACClientWithSAT(false, "CFDI40145", f.sat), nil, time.Second))
	sale2 := f.speiSale(t, "SPEIPAC0002", 11600)
	_, failed, body := createInvoice(t, s, auth.RoleOwner, "p-2", map[string]any{"sale_ids": []string{sale2}, "receptor": pmReceptor()})
	if failed.Status != "FAILED" || failed.Error != "CFDI40145" {
		t.Fatalf("sat rejection: %s", body)
	}
	if holders, _ := s.invoiceRepo().ActiveInvoicesForSales(context.Background(), cardMerchant, []string{sale2}); len(holders) != 0 {
		t.Fatal("a FAILED invoice must release its sales")
	}
	s.SetPACClient(pac.NewMultiPACOrchestrator(pac.NewMockPACClientWithSAT(false, "", f.sat), nil, time.Second))
	if rec := boCall(s, tok, http.MethodPost, "/api/v1/backoffice/invoices/"+failed.InvoiceID+"/retry", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"STAMPED"`) {
		t.Fatalf("retry failed: %d %s", rec.Code, rec.Body.String())
	}
}

func TestInvoices_RequireApprovedMerchantAndCSD(t *testing.T) {
	s, _ := setupTestServer(t)
	draftMerchant(t, s, "m_draft_inv")
	req := jsonReq(http.MethodPost, "/api/v1/merchants/m_draft_inv/invoices", map[string]any{"sale_ids": []string{"x"}, "receptor": pmReceptor()})
	req.Header.Set("Idempotency-Key", "k")
	if rec := serve(s, asTenant(t, s, "m_draft_inv", req)); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "MERCHANT_NOT_APPROVED") {
		t.Fatalf("draft merchant: %d %s", rec.Code, rec.Body.String())
	}
	_ = s.RegisterMerchant("m_nocsd", "GARM850101ABC", "Sin CSD", true)
	_ = s.SeedFiscalSettings(context.Background(), "m_nocsd", "06000")
	req = jsonReq(http.MethodPost, "/api/v1/merchants/m_nocsd/invoices", map[string]any{"sale_ids": []string{"x"}, "receptor": pmReceptor()})
	req.Header.Set("Idempotency-Key", "k")
	if rec := serve(s, asTenant(t, s, "m_nocsd", req)); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "CSD_NOT_FOUND") {
		t.Fatalf("no CSD: %d %s", rec.Code, rec.Body.String())
	}
}
