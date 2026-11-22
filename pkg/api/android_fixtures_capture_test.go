package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"softpos/pkg/auth"
)

// TestCaptureAndroidFixtures writes the response bodies the Android contract
// tests decode (mobile/android/app/src/test/resources/contract, batch M:
// sale detail + receipt, invoiced sales, credit notes, memberships with
// merchant_name, the 401 of the auth gate). It only runs when
// ANDROID_FIXTURES_DIR is set, so the fixtures are always captured from the
// real handlers instead of written by hand:
//
//	ANDROID_FIXTURES_DIR=$PWD/mobile/android/app/src/test/resources/contract \
//	  go test -count=1 -run TestCaptureAndroidFixtures ./pkg/api/
//
// Opaque token values in the token pair are replaced with placeholders.
func TestCaptureAndroidFixtures(t *testing.T) {
	dir := os.Getenv("ANDROID_FIXTURES_DIR")
	if dir == "" {
		t.Skip("ANDROID_FIXTURES_DIR not set")
	}
	write := func(name string, rec *httptest.ResponseRecorder, wantCode int) {
		t.Helper()
		if rec.Code != wantCode {
			t.Fatalf("%s: got %d want %d: %s", name, rec.Code, wantCode, rec.Body.String())
		}
		if err := os.WriteFile(filepath.Join(dir, name), rec.Body.Bytes(), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	get := func(s *Server, role auth.Role, path string) *httptest.ResponseRecorder {
		return serve(s, asRole(t, s, cardMerchant, role, jsonReq(http.MethodGet, path, nil)))
	}
	post := func(s *Server, role auth.Role, path string, body any) *httptest.ResponseRecorder {
		return serve(s, asRole(t, s, cardMerchant, role, jsonReq(http.MethodPost, path, body)))
	}
	salePath := func(id string) string { return "/api/v1/merchants/" + cardMerchant + "/sales/" + id }

	// --- AND-39: sale detail with the receipt link, and sending it.
	f := setupReceiptServer(t)
	s := f.s
	card := capturedCardPayment(t, s, f.mock, "fx-card", 11600)
	write("sale_detail_card_receipt_200.json", get(s, auth.RoleCashier, salePath(card.CardPaymentID)), http.StatusOK)
	spei := f.speiSale(t, "FXSPEI0001", 5800)
	write("sale_detail_spei_200.json", get(s, auth.RoleCashier, salePath(spei)), http.StatusOK)
	_, pending, _ := createIntent(t, s, cardMerchant, "fx-pending", map[string]any{"amount_cents": 1000})
	write("sale_detail_unconfirmed_200.json", get(s, auth.RoleCashier, salePath(pending.CardPaymentID)), http.StatusOK)
	write("sale_not_found_404.json", get(s, auth.RoleCashier, salePath("nope")), http.StatusNotFound)

	sms := &captureSMS{}
	s.SetSMSSender(sms)
	write("receipt_send_200.json", post(s, auth.RoleCashier, salePath(card.CardPaymentID)+"/receipt",
		map[string]string{"email": "cliente@example.com", "phone": "+525512345678"}), http.StatusOK)
	write("receipt_send_invalid_422.json", post(s, auth.RoleOwner, salePath(card.CardPaymentID)+"/receipt",
		map[string]string{"email": "not-an-email", "phone": "5512345678"}), http.StatusUnprocessableEntity)
	write("receipt_send_not_confirmed_409.json", post(s, auth.RoleOwner, salePath(pending.CardPaymentID)+"/receipt",
		map[string]string{"email": "a@example.com"}), http.StatusConflict)
	write("receipt_send_forbidden_403.json", post(s, auth.RoleOperator, salePath(card.CardPaymentID)+"/receipt",
		map[string]string{"email": "a@example.com"}), http.StatusForbidden)
	s.SetSMSSender(failingSMS{})
	write("receipt_send_port_failed_502.json", post(s, auth.RoleOwner, salePath(card.CardPaymentID)+"/receipt",
		map[string]string{"phone": "+525512345678"}), http.StatusBadGateway)
	s.SetReceiptOptions("https://pagos.kerno.mx", 0)
	write("sale_detail_card_receipt_absolute_200.json", get(s, auth.RoleCashier, salePath(card.CardPaymentID)), http.StatusOK)
	s.SetReceiptTokenSecret(nil)
	write("receipt_send_not_configured_503.json", post(s, auth.RoleOwner, salePath(card.CardPaymentID)+"/receipt",
		map[string]string{"email": "a@example.com"}), http.StatusServiceUnavailable)

	// --- AND-40: invoiced sales (individual + credit note, global, free).
	g := setupInvoiceServer(t)
	s = g.s
	inv := capturedCardPayment(t, s, g.mock, "fx-inv", 11600)
	inGlobal := g.speiSale(t, "FXGLOBAL01", 5800)
	_, invoice, body := createInvoice(t, s, auth.RoleOwner, "fx-inv", map[string]any{"sale_ids": []string{inv.CardPaymentID}, "receptor": pmReceptor()})
	if invoice.Status != "STAMPED" {
		t.Fatalf("invoice: %s", body)
	}
	if n, _ := s.RunGlobalInvoices(context.Background(), time.Now().AddDate(0, 1, 2)); n != 1 {
		t.Fatal("global not issued")
	}
	g.deliverOK(t, refundEvent(inv, "re_fx", 1160))
	if n, _ := s.ProcessCreditNotes(context.Background(), time.Now()); n != 1 {
		t.Fatal("credit note not issued")
	}
	_ = g.speiSale(t, "FXFREE0001", 1000)
	write("sales_invoiced_page.json", get(s, auth.RoleCashier, "/api/v1/merchants/"+cardMerchant+"/sales"), http.StatusOK)
	write("sale_detail_invoiced_credit_note_200.json", get(s, auth.RoleCashier, salePath(inv.CardPaymentID)), http.StatusOK)
	write("sale_detail_in_global_200.json", get(s, auth.RoleCashier, salePath(inGlobal)), http.StatusOK)
	write("invoice_cancel_has_credit_notes_409.json", post(s, auth.RoleOwner,
		"/api/v1/merchants/"+cardMerchant+"/invoices/"+invoice.InvoiceID+"/cancel", map[string]string{"motivo": "02"}), http.StatusConflict)

	// --- AND-41: memberships with merchant_name; the auth gate's 401.
	a, _ := setupTestServer(t)
	sender := withTestAuth(t, a)
	tp := httpLogin(t, a, sender, "+525512340001", "")
	for _, m := range []map[string]any{
		{"rfc": "GARM850101ABC", "name": "Tacos Don Memo", "is_resico": true},
		{"rfc": "GARM850101ABC", "name": "Tortas La Güera", "is_resico": true},
	} {
		if rec := serve(a, withBearer(jsonReq(http.MethodPost, "/api/v1/merchants", m), tp.AccessToken)); rec.Code != http.StatusCreated {
			t.Fatalf("register: %d %s", rec.Code, rec.Body.String())
		}
	}
	rec := postJSON(t, a.Router(), "/api/v1/auth/refresh", map[string]string{"refresh_token": tp.RefreshToken})
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh: %d %s", rec.Code, rec.Body.String())
	}
	redacted := redactTokens(rec.Body.Bytes())
	rec.Body.Reset()
	rec.Body.Write(redacted)
	write("auth_refresh_memberships_200.json", rec, http.StatusOK)
	// Any rejected access token (bad signature, expired, wrong iss/aud since
	// MVP-31) gets the same body from the gate.
	write("auth_unauthorized_401.json", serve(a, withBearer(jsonReq(http.MethodGet, "/api/v1/merchants/x/sales", nil), "not-a-jwt")), http.StatusUnauthorized)
}

type failingSMS struct{}

func (failingSMS) SendSMS(context.Context, string, string) error {
	return errors.New("sms provider down")
}

var tokenValueRe = regexp.MustCompile(`"(access_token|refresh_token)":"[^"]*"`)

func redactTokens(b []byte) []byte {
	out := tokenValueRe.ReplaceAllFunc(b, func(m []byte) []byte {
		var kv map[string]string
		_ = json.Unmarshal(append(append([]byte("{"), m...), '}'), &kv)
		for k := range kv {
			return []byte(`"` + k + `":"` + k + `_captured"`)
		}
		return m
	})
	return out
}
