package main

import (
	"bytes"
	"context"
	"database/sql"
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
	"softpos/pkg/payouts"
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
	srv, m, tok, _ := buildCardServerDB(t, extra)
	return srv, m, tok
}

func buildCardServerDB(t *testing.T, extra func(*Config)) (*api.Server, string, string, *sql.DB) {
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
	return srv, merchantID, tok, db
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

// TestPGIntegration_AcquirerPayoutReconciliation: processor payouts on real
// PostgreSQL. Concurrent deliveries of one payout (same and different event
// ids) move ASSET:ACQUIRER_RECEIVABLE to the bank once; a mismatched payout
// produces the discrepancy report the operator endpoint serves; the
// settlement watcher flags overdue captures once even when passes overlap.
func TestPGIntegration_AcquirerPayoutReconciliation(t *testing.T) {
	srv, _, tok, db := buildCardServerDB(t, func(c *Config) { c.AcquirerWebhookSecret = pgAcquirerSecret })
	mock := srv.AcquirerProvider().(*acquirer.MockProvider)
	capture := func(key string, amount int64) api.CardPaymentIntentResponse {
		_, intent := cardIntent(t, srv, tok, key, amount)
		for _, e := range mock.SimulateTap(intent.ProviderPaymentID, intent.CardPaymentID, amount) {
			if c := deliverSigned(t, srv, e); c != http.StatusOK {
				t.Fatalf("tap %s: %d", e.Type, c)
			}
		}
		return intent
	}
	a, b, c := capture("pg-po-a", 10000), capture("pg-po-b", 30000), capture("pg-po-c", 7000)
	feeA, feeB := mock.FeeCents(10000), mock.FeeCents(30000)
	payout := acquirer.Event{ID: "evt_pg_po1", Type: acquirer.EventPayoutSettled, Payout: &acquirer.Payout{
		PayoutID: "pg_po_1", AmountCents: 40000 - feeA - feeB, SettlementDate: "2026-11-21",
		Items: []acquirer.PayoutItem{
			{ProviderPaymentID: b.ProviderPaymentID, Kind: acquirer.PayoutItemCapture, AmountCents: 30000, FeeCents: feeB},
			{ProviderPaymentID: a.ProviderPaymentID, Kind: acquirer.PayoutItemCapture, AmountCents: 10000, FeeCents: feeA},
		}}}
	codes := runConcurrently(6, func(i int) int {
		e := payout
		if i%2 == 1 {
			e.ID = "evt_pg_po1_resent"
		}
		return deliverSigned(t, srv, e)
	})
	for _, code := range codes {
		if code != http.StatusOK {
			t.Fatalf("payout deliveries: %v", codes)
		}
	}
	balance := func(code string) int64 {
		var v int64
		if err := db.QueryRow(`SELECT balance_cents FROM ledger_accounts WHERE code = $1`, code).Scan(&v); err != nil {
			t.Fatalf("balance %s: %v", code, err)
		}
		return v
	}
	if got := balance(api.AccountBankOperating); got != 40000-feeA-feeB {
		t.Fatalf("bank must be credited once: %d", got)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM acquirer_payouts WHERE payout_id = 'pg_po_1' AND status = 'RECONCILED'`); n != 1 {
		t.Fatalf("payout rows: %d", n)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM card_payments WHERE provider_payout_id = 'pg_po_1'`); n != 2 {
		t.Fatalf("settled captures: %d", n)
	}

	// Mismatch: C with a different fee, an unknown line, wrong total.
	bad := acquirer.Event{ID: "evt_pg_po2", Type: acquirer.EventPayoutSettled, Payout: &acquirer.Payout{
		PayoutID: "pg_po_2", AmountCents: 1, SettlementDate: "2026-11-22",
		Items: []acquirer.PayoutItem{
			{ProviderPaymentID: c.ProviderPaymentID, Kind: acquirer.PayoutItemCapture, AmountCents: 7000, FeeCents: 999},
			{ProviderPaymentID: "mock_pi_ghost", Kind: acquirer.PayoutItemCapture, AmountCents: 500},
		}}}
	if code := deliverSigned(t, srv, bad); code != http.StatusOK {
		t.Fatalf("mismatched payout: %d", code)
	}
	opTok, _ := srv.AuthService().MintAccessToken("usr_ops", "merch_pg_card", auth.RoleOperator)
	rec := doJSON(t, srv, opTok, http.MethodGet, "/api/v1/acquirer/reconciliation", nil)
	var rep struct {
		Items []api.ReconciliationItemResponse `json:"items"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &rep)
	kinds := map[string]bool{}
	for _, it := range rep.Items {
		kinds[it.Kind] = true
	}
	if rec.Code != http.StatusOK || len(rep.Items) != 3 || !kinds[store.ReconFeeMismatch] || !kinds[store.ReconMissingPayment] || !kinds[store.ReconPayoutTotalMismatch] {
		t.Fatalf("report: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, srv, tok, http.MethodGet, "/api/v1/acquirer/reconciliation", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("owner must not read the report: %d", rec.Code)
	}

	// Overdue capture: a fourth payment never paid out.
	d := capture("pg-po-d", 4000)
	if _, err := db.Exec(`UPDATE card_payments SET provider_settlement_date = '2026-01-02' WHERE id = $1`, d.CardPaymentID); err != nil {
		t.Fatal(err)
	}
	w := payouts.NewAcquirerSettlementWatcher(store.NewSQLMoneyStore(db), payouts.AcquirerSettlementConfig{})
	created := make([]int, 3)
	runConcurrently(3, func(i int) int {
		n, err := w.RunOnce(context.Background())
		if err != nil {
			t.Errorf("watcher: %v", err)
		}
		created[i] = n
		return 0
	})
	if created[0]+created[1]+created[2] != 1 {
		t.Fatalf("overdue capture flagged %v times", created)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM acquirer_reconciliation_items WHERE kind = 'NOT_SETTLED' AND card_payment_id = $1`, d.CardPaymentID); n != 1 {
		t.Fatalf("NOT_SETTLED rows: %d", n)
	}

	// The worker wiring starts and stops with its context.
	ctx, cancel := context.WithCancel(context.Background())
	wait := StartAcquirerSettlementWatcher(ctx, Config{AcquirerReconcileInterval: 10 * time.Millisecond}, db)
	cancel()
	wait()
}
