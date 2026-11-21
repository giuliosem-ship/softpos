package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"softpos/pkg/auth"
	"softpos/pkg/store"
)

const testReceiptSecret = "receipt_token_test_secret_32_bytes!!"

type captureSMS struct {
	mu   sync.Mutex
	sent []string
}

func (c *captureSMS) SendSMS(_ context.Context, to, body string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent = append(c.sent, to+"|"+body)
	return nil
}

func setupReceiptServer(t *testing.T) invoiceFixture {
	t.Helper()
	f := setupInvoiceServer(t)
	f.s.SetReceiptTokenSecret([]byte(testReceiptSecret))
	return f
}

func saleDetail(t *testing.T, s *Server, saleID string) (int, SaleDetail, string) {
	t.Helper()
	rec := serve(s, asRole(t, s, cardMerchant, auth.RoleCashier, jsonReq(http.MethodGet, "/api/v1/merchants/"+cardMerchant+"/sales/"+saleID, nil)))
	var d SaleDetail
	_ = json.Unmarshal(rec.Body.Bytes(), &d)
	return rec.Code, d, rec.Body.String()
}

func publicGet(s *Server, path string, jsonAccept bool) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if jsonAccept {
		req.Header.Set("Accept", "application/json")
	}
	return serve(s, req)
}

func TestReceiptToken_SignedExpiringOpaque(t *testing.T) {
	secret := []byte(testReceiptSecret)
	id := "0123456789abcdef01234567"
	exp := time.Now().Add(time.Hour).Truncate(time.Second)
	tok, err := encodeReceiptToken(secret, id, exp)
	if err != nil {
		t.Fatal(err)
	}
	if got, gotExp, ok := decodeReceiptToken(secret, tok, time.Now()); !ok || got != id || !gotExp.Equal(exp) {
		t.Fatalf("round trip: %v %s %s", ok, got, gotExp)
	}
	if _, _, ok := decodeReceiptToken([]byte("another_secret_of_32_bytes_long!!!!"), tok, time.Now()); ok {
		t.Fatal("token verified with another secret")
	}
	if _, _, ok := decodeReceiptToken(secret, tok, exp.Add(time.Second)); ok {
		t.Fatal("expired token accepted")
	}
	raw, _ := base64.RawURLEncoding.DecodeString(tok)
	for i := range raw {
		bad := append([]byte(nil), raw...)
		bad[i] ^= 0x01
		if _, _, ok := decodeReceiptToken(secret, base64.RawURLEncoding.EncodeToString(bad), time.Now()); ok {
			t.Fatalf("tampered byte %d accepted", i)
		}
	}
	for _, junk := range []string{"", "x", strings.Repeat("A", 200), tok + "A"} {
		if _, _, ok := decodeReceiptToken(secret, junk, time.Now()); ok {
			t.Fatalf("junk token %q accepted", junk)
		}
	}
	if _, _, ok := decodeReceiptToken(nil, tok, time.Now()); ok {
		t.Fatal("no secret configured must reject every token")
	}
}

// TestReceipts_CardReceiptAndAutofactura: the sale detail returns the
// receipt link; the public receipt shows the masked PAN, brand, auth code,
// amount, date and merchant name/RFC (no tenant ids); the end customer
// requests their CFDI from it (idempotent; one active invoice per sale).
func TestReceipts_CardReceiptAndAutofactura(t *testing.T) {
	f := setupReceiptServer(t)
	s := f.s
	intent := capturedCardPayment(t, s, f.mock, "rcpt-card", 11600)
	code, d, body := saleDetail(t, s, intent.CardPaymentID)
	if code != http.StatusOK || !strings.HasPrefix(d.ReceiptURL, "/r/") || d.ReceiptExpiresAt == "" || d.Card == nil ||
		d.Card.MaskedPAN != "**** 4242" || d.Card.Brand != "visa" || d.Card.AuthCode == "" {
		t.Fatalf("sale detail: %d %s", code, body)
	}
	if _, again, _ := saleDetail(t, s, intent.CardPaymentID); again.ReceiptURL != d.ReceiptURL {
		t.Fatal("one receipt per sale: the link is stable")
	}
	tokenPath := d.ReceiptURL

	rec := publicGet(s, tokenPath, true) // no JWT: public route
	var v ReceiptView
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	if rec.Code != http.StatusOK || v.MerchantName == "" || v.MerchantRFC != "GARM850101ABC" || v.Amount != "116.00" || v.Card == nil ||
		v.Card.MaskedPAN != "**** 4242" || v.Status != "APPROVED" || !v.Autofactura.Available || v.Autofactura.URL != tokenPath+"/invoice" || v.Date == "" {
		t.Fatalf("receipt: %d %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), cardMerchant) || regexp.MustCompile(`\d{13,19}`).MatchString(rec.Body.String()) {
		t.Fatalf("receipt leaks the merchant id or a PAN: %s", rec.Body.String())
	}
	if rec.Header().Get("Cache-Control") != "no-store" || rec.Header().Get("Referrer-Policy") != "no-referrer" {
		t.Fatalf("receipt headers: %v", rec.Header())
	}
	html := publicGet(s, tokenPath, false)
	if html.Code != http.StatusOK || !strings.HasPrefix(html.Header().Get("Content-Type"), "text/html") ||
		!strings.Contains(html.Header().Get("Content-Security-Policy"), "default-src 'none'") ||
		!strings.Contains(html.Body.String(), `action="`+tokenPath+`/invoice"`) || !strings.Contains(html.Body.String(), "**** 4242") {
		t.Fatalf("receipt html: %d %s", html.Code, html.Body.String())
	}

	post := func(body any) (int, map[string]any, string) {
		rec := serve(s, jsonReq(http.MethodPost, tokenPath+"/invoice", body))
		out := map[string]any{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out, rec.Body.String()
	}
	bad := pmReceptor()
	bad["rfc"] = "XAXX010101000"
	if code, _, body := post(bad); code != http.StatusUnprocessableEntity || !strings.Contains(body, "RECEPTOR_INVALID") {
		t.Fatalf("invalid receptor: %d %s", code, body)
	}
	code, inv, body := post(pmReceptor())
	if code != http.StatusCreated || inv["status"] != "STAMPED" || inv["uuid"] == nil || inv["pdf_url"] != tokenPath+"/invoice/pdf" {
		t.Fatalf("autofactura: %d %s", code, body)
	}
	if code, again, _ := post(pmReceptor()); code != http.StatusCreated || again["invoice_id"] != inv["invoice_id"] {
		t.Fatalf("idempotent replay: %d %v", code, again)
	}
	other := pmReceptor()
	other["uso_cfdi"] = "G01"
	if code, _, body := post(other); code != http.StatusConflict || !strings.Contains(body, "SALE_ALREADY_INVOICED") || strings.Contains(body, "inv_") {
		t.Fatalf("second invoice of the sale: %d %s", code, body)
	}
	if rec := publicGet(s, tokenPath+"/invoice/pdf", false); rec.Code != http.StatusOK || !strings.HasPrefix(rec.Body.String(), "%PDF") {
		t.Fatalf("pdf: %d", rec.Code)
	}
	if rec := publicGet(s, tokenPath+"/invoice/xml", false); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `UsoCFDI="G03"`) {
		t.Fatalf("xml: %d", rec.Code)
	}
	rec = publicGet(s, tokenPath, true)
	_ = json.Unmarshal(rec.Body.Bytes(), &v)
	if v.Invoice == nil || v.Invoice.UUID != inv["uuid"] || v.Autofactura.Available || v.Autofactura.Reason != "ALREADY_INVOICED" {
		t.Fatalf("receipt after invoicing: %s", rec.Body.String())
	}
	// The merchant sees it as an individual invoice of the sale.
	if _, d2, _ := saleDetail(t, s, intent.CardPaymentID); d2.InvoiceKind == nil || *d2.InvoiceKind != "INDIVIDUAL" {
		t.Fatal("autofactura invoice not on the sale")
	}

	// Forged / unknown tokens look the same: 404.
	tok := strings.TrimPrefix(tokenPath, "/r/")
	raw, _ := base64.RawURLEncoding.DecodeString(tok)
	raw[0] ^= 0xff
	for _, p := range []string{"/r/" + base64.RawURLEncoding.EncodeToString(raw), "/r/AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA", "/r/x"} {
		if rec := publicGet(s, p, true); rec.Code != http.StatusNotFound {
			t.Fatalf("%s: %d", p, rec.Code)
		}
	}
	// Expired receipt: the old link dies, the sale detail renews it.
	id, _, _ := decodeReceiptToken([]byte(testReceiptSecret), tok, time.Now())
	s.receipts.(*store.MemoryReceiptRepository).ExpireReceipt(id, time.Now().Add(-time.Minute))
	if rec := publicGet(s, tokenPath, true); rec.Code != http.StatusNotFound {
		t.Fatalf("expired receipt: %d", rec.Code)
	}
	s.SetReceiptOptions("https://pagos.example.mx", 48*time.Hour) // also: absolute links
	if _, d3, _ := saleDetail(t, s, intent.CardPaymentID); !strings.HasPrefix(d3.ReceiptURL, "https://pagos.example.mx/r/") || strings.HasSuffix(d3.ReceiptURL, tokenPath) {
		t.Fatal("expired receipt must be renewed with a new link")
	}
}

// TestReceipts_AutofacturaWindowGlobalAndForm: a sale in a factura global
// is 409; the merchant can disable autofactura; the window is the sale's
// calendar month (CDMX); the HTML form posts urlencoded data.
func TestReceipts_AutofacturaWindowGlobalAndForm(t *testing.T) {
	f := setupReceiptServer(t)
	s := f.s
	spei := f.speiSale(t, "RCPTSPEI01", 11600)
	_, d, body := saleDetail(t, s, spei)
	if d.ReceiptURL == "" {
		t.Fatalf("spei receipt: %s", body)
	}
	// HTML form submission (application/x-www-form-urlencoded).
	form := url.Values{"rfc": {"EKU9003173C9"}, "nombre": {"Escuela Kemper Urgate"}, "regimen_fiscal": {"601"}, "codigo_postal": {"26015"}, "uso_cfdi": {"G03"}}
	req := httptest.NewRequest(http.MethodPost, d.ReceiptURL+"/invoice", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	rec := serve(s, req)
	if rec.Code != http.StatusCreated || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") || !strings.Contains(rec.Body.String(), "STAMPED") {
		t.Fatalf("form autofactura: %d %s", rec.Code, rec.Body.String())
	}

	inGlobal := f.speiSale(t, "RCPTSPEI02", 5800)
	_, d2, _ := saleDetail(t, s, inGlobal)
	if n, _ := s.RunGlobalInvoices(context.Background(), time.Now().AddDate(0, 1, 2)); n != 1 {
		t.Fatal("global")
	}
	if rec := serve(s, jsonReq(http.MethodPost, d2.ReceiptURL+"/invoice", pmReceptor())); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "SALE_ALREADY_INVOICED") {
		t.Fatalf("sale in a global: %d %s", rec.Code, rec.Body.String())
	}

	free := f.speiSale(t, "RCPTSPEI03", 2000)
	_, d3, _ := saleDetail(t, s, free)
	put := serve(s, asTenant(t, s, cardMerchant, jsonReq(http.MethodPut, "/api/v1/merchants/"+cardMerchant+"/fiscal-settings", map[string]string{"autofactura": "DISABLED"})))
	if put.Code != http.StatusOK || !strings.Contains(put.Body.String(), `"autofactura":"DISABLED"`) {
		t.Fatalf("disable autofactura: %d %s", put.Code, put.Body.String())
	}
	if rec := serve(s, jsonReq(http.MethodPost, d3.ReceiptURL+"/invoice", pmReceptor())); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), `"reason":"DISABLED"`) {
		t.Fatalf("disabled: %d %s", rec.Code, rec.Body.String())
	}
	if put := serve(s, asTenant(t, s, cardMerchant, jsonReq(http.MethodPut, "/api/v1/merchants/"+cardMerchant+"/fiscal-settings", map[string]string{"autofactura": "SOMETIMES"}))); put.Code != http.StatusUnprocessableEntity {
		t.Fatalf("invalid autofactura setting: %d", put.Code)
	}
	_ = serve(s, asTenant(t, s, cardMerchant, jsonReq(http.MethodPut, "/api/v1/merchants/"+cardMerchant+"/fiscal-settings", map[string]string{"autofactura": "SAME_MONTH"})))

	// Window: the sale's calendar month in America/Mexico_City.
	sales, _ := s.salesRepo()
	list, _ := sales.ListSales(context.Background(), store.SalesQuery{MerchantID: cardMerchant, ID: free, Limit: 1})
	deadline := autofacturaDeadline(list[0])
	for at, want := range map[time.Time]string{deadline.Add(-time.Second): "", deadline: "WINDOW_CLOSED"} {
		af, err := s.autofacturaState(context.Background(), store.ReceiptRecord{}, list[0], at)
		if err != nil || af.Reason != want || af.Available != (want == "") {
			t.Fatalf("window at %s: %+v %v", at, af, err)
		}
	}
	if l := deadline.In(time.FixedZone("CDMX", -6*3600)); l.Day() != 1 || l.Hour() != 0 {
		t.Fatalf("deadline must be local midnight of the 1st: %s", l)
	}
}

// TestReceipts_SendByEmailAndSMS: owners and cashiers send the receipt link
// through the Mailer and SMS ports.
func TestReceipts_SendByEmailAndSMS(t *testing.T) {
	f := setupReceiptServer(t)
	s := f.s
	sms := &captureSMS{}
	s.SetSMSSender(sms)
	intent := capturedCardPayment(t, s, f.mock, "rcpt-send", 11600)
	send := func(role auth.Role, saleID string, body any) *httptest.ResponseRecorder {
		return serve(s, asRole(t, s, cardMerchant, role, jsonReq(http.MethodPost, "/api/v1/merchants/"+cardMerchant+"/sales/"+saleID+"/receipt", body)))
	}
	rec := send(auth.RoleCashier, intent.CardPaymentID, map[string]string{"email": "cliente@example.com", "phone": "+525512345678"})
	var out SendReceiptResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if rec.Code != http.StatusOK || !out.Sent["email"] || !out.Sent["sms"] || !strings.HasPrefix(out.ReceiptURL, "/r/") {
		t.Fatalf("send: %d %s", rec.Code, rec.Body.String())
	}
	if len(f.mailer.sent) != 1 || f.mailer.sent[0].To != "cliente@example.com" || !strings.Contains(f.mailer.sent[0].Body, out.ReceiptURL) ||
		!strings.Contains(f.mailer.sent[0].Body, "**** 4242") || !strings.Contains(f.mailer.sent[0].Body, "116.00") {
		t.Fatalf("receipt email: %+v", f.mailer.sent)
	}
	if len(sms.sent) != 1 || !strings.HasPrefix(sms.sent[0], "+525512345678|") || !strings.Contains(sms.sent[0], out.ReceiptURL) {
		t.Fatalf("receipt sms: %v", sms.sent)
	}
	for _, b := range []map[string]string{{}, {"email": "not-an-email"}, {"phone": "5512345678"}} {
		if rec := send(auth.RoleOwner, intent.CardPaymentID, b); rec.Code != http.StatusUnprocessableEntity {
			t.Fatalf("invalid destination %v: %d", b, rec.Code)
		}
	}
	_, pending, _ := createIntent(t, s, cardMerchant, "rcpt-pending", map[string]any{"amount_cents": 1000})
	if rec := send(auth.RoleOwner, pending.CardPaymentID, map[string]string{"email": "a@example.com"}); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "SALE_NOT_CONFIRMED") {
		t.Fatalf("unconfirmed sale: %d %s", rec.Code, rec.Body.String())
	}
	if _, d, _ := saleDetail(t, s, pending.CardPaymentID); d.ReceiptURL != "" {
		t.Fatal("no receipt for an unconfirmed sale")
	}
	if rec := send(auth.RoleOperator, intent.CardPaymentID, map[string]string{"email": "a@example.com"}); rec.Code != http.StatusForbidden {
		t.Fatalf("operator role: %d", rec.Code)
	}

	s.SetReceiptTokenSecret(nil)
	if rec := send(auth.RoleOwner, intent.CardPaymentID, map[string]string{"email": "a@example.com"}); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("not configured: %d", rec.Code)
	}
	if rec := publicGet(s, out.ReceiptURL, true); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("public receipt without secret: %d", rec.Code)
	}
}

// TestReceipts_PublicRoutesAndRateLimit: only the receipt routes are public
// and they are rate limited per IP.
func TestReceipts_PublicRoutesAndRateLimit(t *testing.T) {
	f := setupReceiptServer(t)
	s := f.s
	for _, c := range []struct{ method, path string }{
		{http.MethodPost, "/r/tok"}, {http.MethodGet, "/r/tok/invoice"}, {http.MethodGet, "/r/tok/other"},
		{http.MethodGet, "/r/tok/invoice/zip"}, {http.MethodGet, "/r/"}, {http.MethodDelete, "/r/tok"},
	} {
		if rec := serve(s, httptest.NewRequest(c.method, c.path, nil)); rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s must not be public: %d", c.method, c.path, rec.Code)
		}
	}
	s.mu.Lock()
	s.receiptLimiter = NewRateLimiter(2, time.Minute)
	s.mu.Unlock()
	codes := []int{}
	for i := 0; i < 3; i++ {
		codes = append(codes, publicGet(s, "/r/unknown", true).Code)
	}
	if codes[0] != http.StatusNotFound || codes[1] != http.StatusNotFound || codes[2] != http.StatusTooManyRequests {
		t.Fatalf("rate limit: %v", codes)
	}
}
