package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"softpos/pkg/acquirer"
	"softpos/pkg/api"
	"softpos/pkg/auth"
	"softpos/pkg/store"
	"softpos/pkg/store/pgtest"
)

// cardIntent posts a card payment intent with an Idempotency-Key.
func cardIntent(t *testing.T, srv *api.Server, tok, key string, amount int64) (int, api.CardPaymentIntentResponse) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"amount_cents": amount})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/card-payments/intents", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Idempotency-Key", key)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	var out api.CardPaymentIntentResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func buildCardServer(t *testing.T, extra func(*Config)) (*api.Server, string, string) {
	t.Helper()
	cfg := Config{
		SQLDriver:         "pgx",
		DatabaseURL:       pgtest.URL(t),
		STPHMACSecret:     "test_secret",
		JWTSecret:         testJWTSecret,
		VaultMasterKeyHex: testVaultKeyHex,
		AcquirerProvider:  "mock",
	}
	if extra != nil {
		extra(&cfg)
	}
	srv, db, _, err := BuildServer(context.Background(), cfg)
	if err != nil {
		t.Fatalf("BuildServer: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	const merchantID = "merch_pg_card"
	if err := srv.RegisterMerchantCLABE("646180123400000007", merchantID, "GARM850101ABC"); err != nil {
		t.Fatalf("register: %v", err)
	}
	tok, _ := srv.AuthService().MintAccessToken("usr_pg_card", merchantID, auth.RoleOwner)
	return srv, merchantID, tok
}

// TestPGIntegration_CardIntentAndCaptureAreExactlyOnce: concurrent intents
// with one Idempotency-Key create one card payment; concurrent deliveries of
// the capture confirmation post the settlement split exactly once.
func TestPGIntegration_CardIntentAndCaptureAreExactlyOnce(t *testing.T) {
	srv, merchantID, tok := buildCardServer(t, nil)

	ids := make([]string, 8)
	codes := runConcurrently(8, func(i int) int {
		code, out := cardIntent(t, srv, tok, "pg-tap-1", 11600)
		ids[i] = out.CardPaymentID
		return code
	})
	created := 0
	for i, c := range codes {
		if c == http.StatusCreated {
			created++
		} else if c != http.StatusOK {
			t.Fatalf("intent %d: status %d", i, c)
		}
		if ids[i] != ids[0] || ids[i] == "" {
			t.Fatalf("concurrent intents must share one card payment: %v", ids)
		}
	}
	if created != 1 {
		t.Fatalf("exactly one 201 expected, got %d (%v)", created, codes)
	}
	_, intent := cardIntent(t, srv, tok, "pg-tap-1", 11600)

	mock := srv.AcquirerProvider().(*acquirer.MockProvider)
	ev := mock.SimulateTap(intent.ProviderPaymentID, intent.CardPaymentID, 11600)
	codes = runConcurrently(6, func(i int) int {
		if _, err := srv.ApplyAcquirerEvent(context.Background(), "mock", ev[1]); err != nil {
			t.Errorf("apply capture %d: %v", i, err)
			return 500
		}
		return 200
	})
	_ = codes
	if _, err := srv.ApplyAcquirerEvent(context.Background(), "mock", ev[0]); err != nil { // late authorized
		t.Fatalf("late authorized: %v", err)
	}
	rec := doJSON(t, srv, tok, http.MethodGet, "/api/v1/card-payments/"+intent.CardPaymentID, nil)
	var got api.CardPaymentResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if rec.Code != http.StatusOK || got.Status != store.CardCaptured || got.Settlement == nil {
		t.Fatalf("get: %d %s", rec.Code, rec.Body.String())
	}
	if avail := availableBalance(t, srv, tok, merchantID); avail != got.Settlement.NetAvailableCents {
		t.Fatalf("available %d, want %d (posted once)", avail, got.Settlement.NetAvailableCents)
	}
	// Unknown provider ids are rejected without consuming anything.
	if _, err := srv.ApplyAcquirerEvent(context.Background(), "mock", acquirer.Event{ID: "e_x", Type: acquirer.EventAuthorized, ProviderPaymentID: "mock_pi_zz"}); err == nil {
		t.Fatal("unknown payment must fail")
	}
}

// TestBuildServer_AcquirerSelection: DEV_MODE defaults to the mock and keeps
// the legacy EMV demo; otherwise the legacy flow is off and an unknown
// provider fails the boot.
func TestBuildServer_AcquirerSelection(t *testing.T) {
	dev, _, _, err := BuildServer(context.Background(), Config{DevMode: true})
	if err != nil {
		t.Fatal(err)
	}
	if dev.AcquirerProvider() == nil || dev.AcquirerProvider().Name() != "mock" {
		t.Fatal("DEV_MODE must default to the mock acquirer")
	}
	plain, _, _, err := BuildServer(context.Background(), Config{STPHMACSecret: "test_secret", JWTSecret: testJWTSecret, VaultMasterKeyHex: testVaultKeyHex})
	if err != nil {
		t.Fatal(err)
	}
	if plain.AcquirerProvider() != nil {
		t.Fatal("no ACQUIRER_PROVIDER outside DEV_MODE: card payments disabled")
	}
	tok, _ := plain.AuthService().MintAccessToken("u", "m", auth.RoleOwner)
	if rec := doJSON(t, plain, tok, http.MethodPost, "/api/v1/payments/emv/authorize", map[string]any{"payment_id": "p"}); rec.Code != http.StatusGone {
		t.Fatalf("legacy EMV outside DEV_MODE: want 410, got %d", rec.Code)
	}
	if rec := doJSON(t, plain, tok, http.MethodPost, "/api/v1/card-payments/intents", map[string]any{"amount_cents": 100}); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("card intents without provider: want 503, got %d", rec.Code)
	}
	if _, _, _, err := BuildServer(context.Background(), Config{DevMode: true, AcquirerProvider: "acme"}); !errors.Is(err, acquirer.ErrUnknownProvider) {
		t.Fatalf("unknown provider: want ErrUnknownProvider, got %v", err)
	}
}

func TestValidateProductionConfig_RejectsMockAcquirer(t *testing.T) {
	good := Config{
		DatabaseURL:       "postgres://softpos:Str0ngRandomPassw0rd@db.prod.internal:5432/softpos?sslmode=require",
		VaultMasterKeyHex: "a1b2c3d4e5f67890123456789abcdef0123456789abcdef0123456789abcdef0",
		STPHMACSecret:     "prod_stp_hmac_secret_very_long_and_random_string_xyz",
		JWTSecret:         "prod_jwt_secret_with_at_least_32_random_bytes",
		CoDiWebhookSecret: "prod_codi_webhook_secret_with_32_random_bytes",
	}
	if err := ValidateProductionConfig(good, "production"); err != nil {
		t.Fatalf("baseline: %v", err)
	}
	good.AcquirerProvider, good.AcquirerWebhookSecret = "mock", "prod_acquirer_webhook_secret_32_random_bytes"
	if err := ValidateProductionConfig(good, "production"); !errors.Is(err, ErrMockAcquirerInProduction) {
		t.Fatalf("mock in production: want ErrMockAcquirerInProduction, got %v", err)
	}
}

const pgAcquirerSecret = "pg_acquirer_webhook_secret_32_bytes_long!"

func deliverSigned(t *testing.T, srv *api.Server, e acquirer.Event) int {
	t.Helper()
	body, _ := json.Marshal(e)
	ts := time.Now().Unix()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/acquirer/mock", bytes.NewReader(body))
	req.Header.Set(api.WebhookTimestampHeader, strconv.FormatInt(ts, 10))
	req.Header.Set(api.WebhookSignatureHeader, api.SignWebhook([]byte(pgAcquirerSecret), ts, body))
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	return rec.Code
}

// TestPGIntegration_AcquirerWebhooksConcurrentDuplicatesAndOutOfOrder:
// signed webhooks delivered concurrently and out of order on real
// PostgreSQL: duplicates post once, a refund racing its capture is either
// applied after it or rejected as retryable (never lost, never doubled), and
// duplicate chargebacks debit once.
func TestPGIntegration_AcquirerWebhooksConcurrentDuplicatesAndOutOfOrder(t *testing.T) {
	srv, merchantID, tok := buildCardServer(t, func(c *Config) { c.AcquirerWebhookSecret = pgAcquirerSecret })
	_, intent := cardIntent(t, srv, tok, "pg-wh-1", 20000)
	mock := srv.AcquirerProvider().(*acquirer.MockProvider)
	tap := mock.SimulateTap(intent.ProviderPaymentID, intent.CardPaymentID, 20000)
	refund := acquirer.Event{ID: "evt_pg_rf", Type: acquirer.EventRefunded, ProviderPaymentID: intent.ProviderPaymentID, RefundID: "re_pg", AmountCents: 5000}

	// 8 concurrent deliveries: capture x4, authorized x2, refund x2.
	codes := runConcurrently(8, func(i int) int {
		switch {
		case i < 4:
			return deliverSigned(t, srv, tap[1])
		case i < 6:
			return deliverSigned(t, srv, tap[0])
		default:
			return deliverSigned(t, srv, refund)
		}
	})
	for i, c := range codes[:6] {
		if c != http.StatusOK {
			t.Fatalf("delivery %d: %d", i, c)
		}
	}
	for _, c := range codes[6:] {
		if c != http.StatusOK && c != http.StatusConflict {
			t.Fatalf("refund delivery: want 200 or 409 (retry), got %d", c)
		}
	}
	// The provider keeps retrying until 200.
	if c := deliverSigned(t, srv, refund); c != http.StatusOK {
		t.Fatalf("refund retry: %d", c)
	}
	rec := doJSON(t, srv, tok, http.MethodGet, "/api/v1/card-payments/"+intent.CardPaymentID, nil)
	var got api.CardPaymentResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got.Status != store.CardPartiallyRefunded || got.RefundedCents != 5000 || got.CapturedCents != 20000 {
		t.Fatalf("state: %s", rec.Body.String())
	}
	afterRefund := availableBalance(t, srv, tok, merchantID)
	if afterRefund >= got.Settlement.NetAvailableCents || afterRefund <= 0 {
		t.Fatalf("one partial refund expected: available %d net %d", afterRefund, got.Settlement.NetAvailableCents)
	}

	opened := acquirer.Event{ID: "evt_pg_cb", Type: acquirer.EventChargebackOpened, ProviderPaymentID: intent.ProviderPaymentID, ChargebackID: "cb_pg", AmountCents: 3000}
	codes = runConcurrently(5, func(i int) int {
		e := opened
		if i%2 == 1 {
			e.ID = "evt_pg_cb_resent" // same chargeback, another event id
		}
		return deliverSigned(t, srv, e)
	})
	for _, c := range codes {
		if c != http.StatusOK {
			t.Fatalf("chargeback deliveries: %v", codes)
		}
	}
	if got := availableBalance(t, srv, tok, merchantID); got != afterRefund-3000 {
		t.Fatalf("chargeback must debit once: %d, want %d", got, afterRefund-3000)
	}
}
