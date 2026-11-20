package api

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

const testCoDiWebhookSecret = "codi_webhook_test_secret_32_bytes_min!"

// signedCoDiRequest builds a CoDi webhook request signed at ts.
func signedCoDiRequest(secret string, ts time.Time, body string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/codi/confirm", strings.NewReader(body))
	req.Header.Set(WebhookTimestampHeader, strconv.FormatInt(ts.Unix(), 10))
	req.Header.Set(WebhookSignatureHeader, SignWebhook([]byte(secret), ts.Unix(), []byte(body)))
	return req
}

func codiPayload(tracking string) string {
	return `{"tracking_key":"` + tracking + `","seller_clabe":"646180123400000007","buyer_clabe":"012180000118359719",` +
		`"amount_cents":11600,"timestamp":"2026-11-20T12:00:00Z","status":"APROBADO","banxico_approval_code":"AUT001"}`
}

func setupCoDiWebhookServer(t *testing.T) *Server {
	t.Helper()
	s, _ := setupTestServer(t)
	_ = s.RegisterMerchantCLABE("646180123400000007", "merch_codi_sig", "GARM850101ABC")
	s.SetCoDiWebhookSecret(testCoDiWebhookSecret)
	return s
}

func TestCoDiWebhook_ValidSignatureCredits(t *testing.T) {
	s := setupCoDiWebhookServer(t)
	rec := serve(s, signedCoDiRequest(testCoDiWebhookSecret, time.Now(), codiPayload("BNXSIG0001")))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "CONFIRMED") {
		t.Fatalf("valid signature: want 200 CONFIRMED, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestCoDiWebhook_RejectsBadOrMissingSignature(t *testing.T) {
	s := setupCoDiWebhookServer(t)
	body := codiPayload("BNXSIG0002")
	cases := map[string]*http.Request{
		"no headers":   httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/codi/confirm", strings.NewReader(body)),
		"wrong secret": signedCoDiRequest("another_secret_that_is_32_bytes_long!!", time.Now(), body),
	}
	tampered := signedCoDiRequest(testCoDiWebhookSecret, time.Now(), body)
	tampered.Body = httptest.NewRequest(http.MethodPost, "/", strings.NewReader(strings.Replace(body, "11600", "99999900", 1))).Body
	cases["tampered body"] = tampered
	badHex := signedCoDiRequest(testCoDiWebhookSecret, time.Now(), body)
	badHex.Header.Set(WebhookSignatureHeader, "v1=zz")
	cases["malformed signature"] = badHex
	noPrefix := signedCoDiRequest(testCoDiWebhookSecret, time.Now(), body)
	noPrefix.Header.Set(WebhookSignatureHeader, strings.TrimPrefix(noPrefix.Header.Get(WebhookSignatureHeader), "v1="))
	cases["missing scheme"] = noPrefix
	shiftedTS := signedCoDiRequest(testCoDiWebhookSecret, time.Now(), body)
	shiftedTS.Header.Set(WebhookTimestampHeader, strconv.FormatInt(time.Now().Unix()+1, 10)) // signature bound to timestamp
	cases["timestamp swapped"] = shiftedTS

	for name, req := range cases {
		rec := serve(s, req)
		if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "INVALID_SIGNATURE") {
			t.Errorf("%s: want 401 INVALID_SIGNATURE, got %d %s", name, rec.Code, rec.Body.String())
		}
	}
	// Nothing was credited.
	if s.GetMerchantMonthlySalesCents("merch_codi_sig") != 0 {
		t.Fatal("rejected webhooks must not credit the merchant")
	}
}

func TestCoDiWebhook_RejectsStaleTimestamp(t *testing.T) {
	s := setupCoDiWebhookServer(t)
	for _, skew := range []time.Duration{-(WebhookTolerance + time.Minute), WebhookTolerance + time.Minute} {
		rec := serve(s, signedCoDiRequest(testCoDiWebhookSecret, time.Now().Add(skew), codiPayload("BNXSIG0003")))
		if rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "STALE_TIMESTAMP") {
			t.Errorf("skew %v: want 401 STALE_TIMESTAMP, got %d %s", skew, rec.Code, rec.Body.String())
		}
	}
}

func TestCoDiWebhook_RejectsReplay(t *testing.T) {
	s := setupCoDiWebhookServer(t)
	req := signedCoDiRequest(testCoDiWebhookSecret, time.Now(), codiPayload("BNXSIG0004"))
	ts, sig := req.Header.Get(WebhookTimestampHeader), req.Header.Get(WebhookSignatureHeader)
	if rec := serve(s, req); rec.Code != http.StatusOK {
		t.Fatalf("first delivery: %d %s", rec.Code, rec.Body.String())
	}
	replay := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/codi/confirm", strings.NewReader(codiPayload("BNXSIG0004")))
	replay.Header.Set(WebhookTimestampHeader, ts)
	replay.Header.Set(WebhookSignatureHeader, sig)
	if rec := serve(s, replay); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "WEBHOOK_REPLAYED") {
		t.Fatalf("replay: want 409 WEBHOOK_REPLAYED, got %d %s", rec.Code, rec.Body.String())
	}
	// A legitimate re-delivery re-signed with a new timestamp is idempotent on tracking_key.
	resent := signedCoDiRequest(testCoDiWebhookSecret, time.Now().Add(time.Second), codiPayload("BNXSIG0004"))
	if rec := serve(s, resent); rec.Code != http.StatusOK {
		t.Fatalf("re-signed retry: want cached 200, got %d", rec.Code)
	}
	if got := s.GetMerchantMonthlySalesCents("merch_codi_sig"); got != 10000 {
		t.Fatalf("payment must be credited once (base 10000), got %d", got)
	}
}

func TestCoDiWebhook_NotConfiguredIsRefused(t *testing.T) {
	s, _ := setupTestServer(t)
	rec := serve(s, signedCoDiRequest(testCoDiWebhookSecret, time.Now(), codiPayload("BNXSIG0005")))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("no secret configured: want 503, got %d", rec.Code)
	}
}

func TestDevCoDiSimulate_OnlyDevModeAndOwnCLABE(t *testing.T) {
	s := setupCoDiWebhookServer(t)
	body := codiPayload("BNXDEVSIM01")
	if rec := serve(s, asTenant(t, s, "merch_codi_sig", httptest.NewRequest(http.MethodPost, "/api/v1/dev/codi/simulate-confirm", strings.NewReader(body)))); rec.Code != http.StatusNotFound {
		t.Fatalf("outside DEV_MODE: want 404, got %d", rec.Code)
	}
	s.SetDevMode(true)
	if rec := serve(s, httptest.NewRequest(http.MethodPost, "/api/v1/dev/codi/simulate-confirm", strings.NewReader(body))); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated simulate: want 401, got %d", rec.Code)
	}
	_ = s.RegisterMerchant("merch_other", "XAXX010101000", "Other", true)
	if rec := serve(s, asTenant(t, s, "merch_other", httptest.NewRequest(http.MethodPost, "/api/v1/dev/codi/simulate-confirm", strings.NewReader(body)))); rec.Code != http.StatusForbidden {
		t.Fatalf("simulate for a foreign CLABE: want 403, got %d", rec.Code)
	}
	rec := serve(s, asTenant(t, s, "merch_codi_sig", httptest.NewRequest(http.MethodPost, "/api/v1/dev/codi/simulate-confirm", strings.NewReader(body))))
	if rec.Code != http.StatusOK {
		t.Fatalf("dev simulate: want 200, got %d %s", rec.Code, rec.Body.String())
	}
}

func TestVerifyWebhookSignature_Unit(t *testing.T) {
	secret := []byte(testCoDiWebhookSecret)
	now := time.Unix(1_800_000_000, 0)
	body := []byte(`{"a":1}`)
	sig := SignWebhook(secret, now.Unix(), body)
	if _, err := VerifyWebhookSignature(secret, strconv.FormatInt(now.Unix(), 10), sig, body, now.Add(WebhookTolerance), WebhookTolerance); err != nil {
		t.Fatalf("edge of window must pass: %v", err)
	}
	if _, err := VerifyWebhookSignature(nil, strconv.FormatInt(now.Unix(), 10), sig, body, now, WebhookTolerance); err == nil {
		t.Fatal("empty secret must never verify")
	}
	if _, err := VerifyWebhookSignature(secret, "abc", sig, body, now, WebhookTolerance); err == nil {
		t.Fatal("non-numeric timestamp must fail")
	}
}
