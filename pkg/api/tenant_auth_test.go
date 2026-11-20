package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"softpos/pkg/auth"
	"softpos/pkg/identity"
)

func serve(s *Server, req *http.Request) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.Router().ServeHTTP(rec, req)
	return rec
}

func jsonReq(method, path string, body any) *http.Request {
	var buf bytes.Buffer
	if body != nil {
		_ = json.NewEncoder(&buf).Encode(body)
	}
	req := httptest.NewRequest(method, path, &buf)
	withPayoutKey(req)
	return req
}

func TestAuthGate_DefaultDenyAndPublicAllowlist(t *testing.T) {
	s, _ := setupTestServer(t)

	protected := []struct{ method, path string }{
		{"GET", "/api/v1/merchants/m1"},
		{"POST", "/api/v1/merchants"},
		{"GET", "/api/v1/merchants/m1/balances"},
		{"GET", "/api/v1/merchants/m1/rbf"},
		{"POST", "/api/v1/disbursements"},
		{"GET", "/api/v1/disbursements/ABC"},
		{"POST", "/api/v1/disbursements/ABC/reconcile-cep"},
		{"POST", "/api/v1/payments/emv/authorize"},
		{"POST", "/api/v1/payments/emv/reverse"},
		{"POST", "/api/v1/payments/codi/qr"},
		{"POST", "/api/v1/payments/settle"},
		{"POST", "/api/v1/invoices/cfdi"},
		{"POST", "/api/v1/invoices/global"},
		{"POST", "/api/v1/rbf/loans"},
		{"GET", "/api/v1/not-a-route"},
	}
	for _, p := range protected {
		rec := serve(s, jsonReq(p.method, p.path, map[string]any{}))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without token: want 401, got %d", p.method, p.path, rec.Code)
		}
	}

	// Forged / expired / wrong-secret tokens are rejected too.
	forged, _ := GenerateTestJWT("m1", "some_other_secret_that_is_32_bytes!!", time.Hour)
	if rec := serve(s, withBearer(jsonReq("GET", "/api/v1/merchants/m1/balances", nil), forged)); rec.Code != http.StatusUnauthorized {
		t.Errorf("forged token: want 401, got %d", rec.Code)
	}

	public := []struct{ method, path string }{
		{"GET", "/healthz"},
		{"GET", "/pos/"},
		{"POST", "/api/v1/auth/otp/request"},
		{"POST", "/api/v1/auth/otp/verify"},
		{"POST", "/api/v1/auth/refresh"},
		{"POST", "/api/v1/auth/logout"},
		{"GET", "/api/v1/auth/attestation-nonce"},
		{"POST", "/api/v1/webhooks/stp/abono"},
		{"POST", "/api/v1/webhooks/codi/confirm"},
	}
	for _, p := range public {
		rec := serve(s, jsonReq(p.method, p.path, map[string]any{}))
		if rec.Code == http.StatusUnauthorized && rec.Header().Get("WWW-Authenticate") != "" {
			t.Errorf("%s %s must be public, got JWT challenge", p.method, p.path)
		}
	}
}

func TestTenantFromToken_AntiIDOR(t *testing.T) {
	s, _ := setupTestServer(t)
	const a, b = "merch_tenant_a", "merch_tenant_b"
	_ = s.RegisterMerchant(a, "GARM850101ABC", "A", true)
	_ = s.RegisterMerchant(b, "XAXX010101000", "B", true)
	_ = s.RegisterMerchantCLABE("646180123400000007", a, "GARM850101ABC")
	_ = s.RegisterMerchantCLABE("646180123400000010", b, "XAXX010101000")
	_, _ = s.ledger.RecordSplitPayment(100000, b, 0, 0, 0) // B has funds

	// B authorizes and settles a payment of its own.
	tlv := "9F02060000000250005F2A0204849F2701809F2608AABBCCDDEEFF00119F360200145A0841523133333390125F24022812"
	if rec := serve(s, asTenant(t, s, b, jsonReq("POST", "/api/v1/payments/emv/authorize", map[string]any{"payment_id": "pay_b", "tlv_hex": tlv}))); rec.Code != http.StatusOK {
		t.Fatalf("B authorize: %d %s", rec.Code, rec.Body.String())
	}

	forbidden := []struct {
		name, method, path string
		body               any
		want               int
	}{
		{"settle for B", "POST", "/api/v1/payments/settle", map[string]any{"payment_id": "x1", "merchant_id": b, "gross_amount_cents": 1000}, http.StatusForbidden},
		{"settle B's payment", "POST", "/api/v1/payments/settle", map[string]any{"payment_id": "pay_b", "gross_amount_cents": 25000}, http.StatusNotFound},
		{"reverse B's payment", "POST", "/api/v1/payments/emv/reverse", map[string]any{"payment_id": "pay_b"}, http.StatusNotFound},
		{"authorize for B", "POST", "/api/v1/payments/emv/authorize", map[string]any{"payment_id": "x2", "merchant_id": b, "tlv_hex": tlv}, http.StatusForbidden},
		{"disburse B's funds", "POST", "/api/v1/disbursements", map[string]any{"merchant_id": b, "amount_cents": 100, "destination_clabe": "032180000118359719", "beneficiary_name": "X"}, http.StatusForbidden},
		{"B's balances", "GET", "/api/v1/merchants/" + b + "/balances", nil, http.StatusForbidden},
		{"B's profile", "GET", "/api/v1/merchants/" + b, nil, http.StatusForbidden},
		{"B's rbf", "GET", "/api/v1/merchants/" + b + "/rbf", nil, http.StatusForbidden},
		{"loan for B", "POST", "/api/v1/rbf/loans", map[string]any{"merchant_id": b, "principal_cents": 100000, "fixed_fee_bps": 1000, "holdback_bps": 1000}, http.StatusForbidden},
		{"CFDI as B", "POST", "/api/v1/invoices/cfdi", map[string]any{"merchant_id": b, "receptor_rfc": "XAXX010101000", "gross_amount_cents": 1000}, http.StatusForbidden},
		{"global CFDI as B", "POST", "/api/v1/invoices/global", map[string]any{"merchant_id": b, "total_gross_cents": 1000}, http.StatusForbidden},
		{"QR to B's CLABE", "POST", "/api/v1/payments/codi/qr", map[string]any{"seller_clabe": "646180123400000010", "amount_cents": 1000, "concept": "x", "reference": "R1"}, http.StatusForbidden},
		{"QR for B", "POST", "/api/v1/payments/codi/qr", map[string]any{"merchant_id": b, "amount_cents": 1000, "concept": "x", "reference": "R1"}, http.StatusForbidden},
	}
	for _, f := range forbidden {
		rec := serve(s, asTenant(t, s, a, jsonReq(f.method, f.path, f.body)))
		if rec.Code != f.want {
			t.Errorf("A -> %s: want %d, got %d %s", f.name, f.want, rec.Code, rec.Body.String())
		}
	}

	// B's balance is untouched by A's attempts; B can still settle its payment.
	if rec := serve(s, asTenant(t, s, b, jsonReq("POST", "/api/v1/payments/settle", map[string]any{"payment_id": "pay_b", "gross_amount_cents": 25000}))); rec.Code != http.StatusOK {
		t.Fatalf("B settle own payment: %d %s", rec.Code, rec.Body.String())
	}

	// A token without merchant scope cannot use tenant routes.
	unscoped, _ := s.AuthService().MintAccessToken("usr_new", "", "")
	if rec := serve(s, withBearer(jsonReq("POST", "/api/v1/payments/settle", map[string]any{"payment_id": "z", "gross_amount_cents": 100}), unscoped)); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "MERCHANT_SCOPE_REQUIRED") {
		t.Errorf("unscoped token: want 403 MERCHANT_SCOPE_REQUIRED, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestTenantFromToken_DisbursementLookupAndRoles(t *testing.T) {
	s, _ := setupTestServer(t)
	const a, b = "merch_disb_a", "merch_disb_b"
	_ = s.RegisterMerchant(a, "GARM850101ABC", "A", true)
	_ = s.RegisterMerchant(b, "GARM850101ABC", "B", true)
	_, _ = s.ledger.RecordSplitPayment(500000, b, 0, 0, 0)
	if err := s.SeedVerifiedSettlementAccount(context.Background(), b, "032180000118359719"); err != nil {
		t.Fatal(err)
	}

	body := map[string]any{"amount_cents": 1000, "destination_clabe": "032180000118359719", "beneficiary_name": "B Owner", "concept": "payout"}
	// Cashier of B cannot move money out.
	if rec := serve(s, asRole(t, s, b, auth.RoleCashier, jsonReq("POST", "/api/v1/disbursements", body))); rec.Code != http.StatusForbidden || !strings.Contains(rec.Body.String(), "ROLE_FORBIDDEN") {
		t.Fatalf("cashier disbursement: want 403 ROLE_FORBIDDEN, got %d %s", rec.Code, rec.Body.String())
	}
	rec := serve(s, asTenant(t, s, b, jsonReq("POST", "/api/v1/disbursements", body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("owner disbursement: %d %s", rec.Code, rec.Body.String())
	}
	var disb DisbursementResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &disb)

	// A cannot read or reconcile B's disbursement (looks nonexistent).
	for _, path := range []string{"/api/v1/disbursements/" + disb.ClaveRastreo} {
		if rec := serve(s, asTenant(t, s, a, jsonReq("GET", path, nil))); rec.Code != http.StatusNotFound {
			t.Errorf("A GET B's disbursement: want 404, got %d", rec.Code)
		}
	}
	if rec := serve(s, asTenant(t, s, a, jsonReq("POST", "/api/v1/disbursements/"+disb.ClaveRastreo+"/reconcile-cep", nil))); rec.Code != http.StatusNotFound {
		t.Errorf("A reconcile B's disbursement: want 404, got %d", rec.Code)
	}
	if rec := serve(s, asTenant(t, s, b, jsonReq("GET", "/api/v1/disbursements/"+disb.ClaveRastreo, nil))); rec.Code != http.StatusOK {
		t.Errorf("B GET own disbursement: want 200, got %d", rec.Code)
	}
}

func TestIdempotencyKeysAreScopedByTenant(t *testing.T) {
	s, _ := setupTestServer(t)
	const a, b = "merch_idem_a", "merch_idem_b"
	_ = s.RegisterMerchant(a, "GARM850101ABC", "A", true)
	_ = s.RegisterMerchant(b, "GARM850101ABC", "B", true)
	tlv := "9F02060000000250005F2A0204849F2701809F2608AABBCCDDEEFF00119F360200145A0841523133333390125F24022812"
	reqA := asTenant(t, s, a, jsonReq("POST", "/api/v1/payments/emv/authorize", map[string]any{"payment_id": "idem_a", "tlv_hex": tlv}))
	reqA.Header.Set("Idempotency-Key", "same-key")
	if rec := serve(s, reqA); rec.Code != http.StatusOK {
		t.Fatalf("A authorize: %d %s", rec.Code, rec.Body.String())
	}
	reqB := asTenant(t, s, b, jsonReq("POST", "/api/v1/payments/emv/authorize", map[string]any{"payment_id": "idem_b", "tlv_hex": tlv}))
	reqB.Header.Set("Idempotency-Key", "same-key")
	rec := serve(s, reqB)
	var resp EMVAuthorizeResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if rec.Code != http.StatusOK || resp.PaymentID != "idem_b" || resp.MerchantID != b {
		t.Fatalf("B must not receive A's cached response: %d %+v", rec.Code, resp)
	}
}

func TestAuthRateLimitPerIP(t *testing.T) {
	s, _ := setupTestServer(t)
	s.SetAuthRateLimit(3)
	send := func(ip string) int {
		req := jsonReq("POST", "/api/v1/auth/otp/request", map[string]string{"phone": "bad"})
		req.RemoteAddr = ip + ":1234"
		return serve(s, req).Code
	}
	for i := 0; i < 3; i++ {
		if code := send("198.51.100.7"); code == http.StatusTooManyRequests {
			t.Fatalf("request %d limited too early", i)
		}
	}
	req := jsonReq("POST", "/api/v1/auth/otp/request", map[string]string{"phone": "bad"})
	req.RemoteAddr = "198.51.100.7:1234"
	rec := serve(s, req)
	if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") == "" {
		t.Fatalf("4th request: want 429 + Retry-After, got %d", rec.Code)
	}
	if code := send("198.51.100.8"); code == http.StatusTooManyRequests {
		t.Fatal("another IP must have its own budget")
	}
	// Non-auth routes are not affected by the auth limiter.
	for i := 0; i < 5; i++ {
		r := httptest.NewRequest("GET", "/healthz", nil)
		r.RemoteAddr = "198.51.100.7:1234"
		if serve(s, r).Code != http.StatusOK {
			t.Fatal("healthz must not be rate limited")
		}
	}
}

func TestRateLimiter_WindowResets(t *testing.T) {
	l := NewRateLimiter(2, time.Minute)
	now := time.Unix(1_700_000_000, 0)
	l.now = func() time.Time { return now }
	l.Allow("k")
	l.Allow("k")
	if ok, retry := l.Allow("k"); ok || retry == "" {
		t.Fatal("third request in window must be limited")
	}
	now = now.Add(61 * time.Second)
	if ok, _ := l.Allow("k"); !ok {
		t.Fatal("budget must reset after the window")
	}
	if ok, _ := NewRateLimiter(0, time.Minute).Allow("k"); !ok {
		t.Fatal("limit 0 disables limiting")
	}
}

func TestAttestationNonceAndVerify(t *testing.T) {
	s, _ := setupTestServer(t)
	getNonce := func(method string) AttestationNonceResponse {
		rec := serve(s, jsonReq(method, "/api/v1/auth/attestation-nonce", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("%s attestation-nonce: %d", method, rec.Code)
		}
		var n AttestationNonceResponse
		_ = json.Unmarshal(rec.Body.Bytes(), &n)
		if len(n.Nonce) < 40 || n.TTLSeconds != 300 || n.IssuedAtMs <= 0 {
			t.Fatalf("unexpected nonce response: %+v", n)
		}
		return n
	}
	n1 := getNonce("GET")
	n2 := getNonce("POST")
	if n1.Nonce == n2.Nonce {
		t.Fatal("nonces must be unique")
	}

	// Not configured -> 503.
	if rec := serve(s, jsonReq("POST", "/api/v1/auth/attestation/verify", map[string]string{"nonce": n1.Nonce, "integrity_token": "x"})); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured verifier: want 503, got %d", rec.Code)
	}

	const pkg, secret = "mx.softpos.app", "play_integrity_test_secret"
	s.SetPlayIntegrityVerifier(identity.NewPlayIntegrityVerifier(pkg, secret, 5*time.Minute))
	token := func(nonce string) string {
		tok, _ := identity.CreatePlayIntegrityTestToken(identity.PlayIntegrityVerdict{
			PackageName:              pkg,
			DeviceRecognitionVerdict: []string{"MEETS_DEVICE_INTEGRITY"},
			Nonce:                    nonce,
			TimestampMillis:          time.Now().UnixMilli(),
		}, secret)
		return tok
	}
	verify := func(nonce, tok string) *httptest.ResponseRecorder {
		return serve(s, jsonReq("POST", "/api/v1/auth/attestation/verify", map[string]string{"nonce": nonce, "integrity_token": tok}))
	}
	// n1 was burned by the 503 attempt? No: the verifier check happens first, so n1 is still valid.
	if rec := verify(n1.Nonce, token(n1.Nonce)); rec.Code != http.StatusOK {
		t.Fatalf("valid attestation: want 200, got %d %s", rec.Code, rec.Body.String())
	}
	if rec := verify(n1.Nonce, token(n1.Nonce)); rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "ATTESTATION_NONCE_INVALID") {
		t.Fatalf("replayed nonce: want 401 ATTESTATION_NONCE_INVALID, got %d %s", rec.Code, rec.Body.String())
	}
	// Token bound to another nonce fails and still burns n2.
	if rec := verify(n2.Nonce, token("other-nonce")); rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "ATTESTATION_FAILED") {
		t.Fatalf("nonce mismatch: want 401 ATTESTATION_FAILED, got %d %s", rec.Code, rec.Body.String())
	}
	if rec := verify(n2.Nonce, token(n2.Nonce)); rec.Code != http.StatusUnauthorized {
		t.Fatalf("burned nonce reused: want 401, got %d", rec.Code)
	}
	if rec := verify("never-issued", token("never-issued")); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unknown nonce: want 401, got %d", rec.Code)
	}
}

func TestDevSession_OnlyInDevMode(t *testing.T) {
	s, _ := setupTestServer(t)
	const m = "merch_demo_dev"
	_ = s.RegisterMerchant(m, "TAQ850101XYZ", "Demo", true)
	if rec := serve(s, jsonReq("POST", "/api/v1/auth/dev-session", map[string]string{"merchant_id": m})); rec.Code != http.StatusNotFound {
		t.Fatalf("dev-session outside DEV_MODE: want 404, got %d", rec.Code)
	}
	s.SetDevMode(true)
	rec := serve(s, jsonReq("POST", "/api/v1/auth/dev-session", map[string]string{"merchant_id": m}))
	if rec.Code != http.StatusOK {
		t.Fatalf("dev-session: %d %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		AccessToken string `json:"access_token"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if rec := serve(s, withBearer(jsonReq("GET", "/api/v1/merchants/"+m+"/balances", nil), resp.AccessToken)); rec.Code != http.StatusOK {
		t.Fatalf("dev token must work on own merchant: %d", rec.Code)
	}
	if rec := serve(s, jsonReq("POST", "/api/v1/auth/dev-session", map[string]string{"merchant_id": "nope"})); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown merchant: want 404, got %d", rec.Code)
	}
}
