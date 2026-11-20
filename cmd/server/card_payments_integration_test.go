package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

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
	good.AcquirerProvider = "mock"
	if err := ValidateProductionConfig(good, "production"); !errors.Is(err, ErrMockAcquirerInProduction) {
		t.Fatalf("mock in production: want ErrMockAcquirerInProduction, got %v", err)
	}
}
