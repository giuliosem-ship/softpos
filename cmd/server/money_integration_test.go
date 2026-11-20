package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"softpos/pkg/api"
	"softpos/pkg/auth"
	"softpos/pkg/store/pgtest"
	"softpos/pkg/stp"
)

func countRows(t *testing.T, db *sql.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := db.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return n
}

func availableBalance(t *testing.T, srv *api.Server, tok, merchantID string) int64 {
	t.Helper()
	rec := doJSON(t, srv, tok, http.MethodGet, "/api/v1/merchants/"+merchantID+"/balances", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("balances: %d %s", rec.Code, rec.Body.String())
	}
	var b api.MerchantBalancesResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &b)
	return b.AvailableCents
}

// runConcurrently fires n copies of fn at once and returns their status codes.
func runConcurrently(n int, fn func(i int) int) []int {
	codes := make([]int, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			codes[i] = fn(i)
		}(i)
	}
	close(start)
	wg.Wait()
	return codes
}

// TestPGIntegration_ConcurrentMoneyPostingIsExactlyOnce: concurrent settles of
// one payment and concurrent duplicate STP / CoDi webhooks credit the merchant
// exactly once on real PostgreSQL (single transaction + row locks + unique keys).
func TestPGIntegration_ConcurrentMoneyPostingIsExactlyOnce(t *testing.T) {
	cfg := Config{
		SQLDriver:         "pgx",
		DatabaseURL:       pgtest.URL(t),
		STPHMACSecret:     "test_secret",
		CoDiWebhookSecret: "money_codi_webhook_secret_32_bytes!!",
		JWTSecret:         testJWTSecret,
	}
	srv, db, _, err := BuildServer(context.Background(), cfg)
	if err != nil {
		t.Fatalf("BuildServer: %v", err)
	}
	defer func() { _ = db.Close() }()

	const merchantID, clabe = "merch_pg_money", "646180123400000007"
	if err := srv.RegisterMerchantCLABE(clabe, merchantID, "GARM850101ABC"); err != nil {
		t.Fatalf("register: %v", err)
	}
	tok, _ := srv.AuthService().MintAccessToken("usr_pg_money", merchantID, auth.RoleOwner)

	// 1. Concurrent settles of the same authorized payment.
	if rec := doJSON(t, srv, tok, http.MethodPost, "/api/v1/payments/emv/authorize", map[string]any{
		"payment_id": "pay_pg_race", "tlv_hex": validEMVTLVHex,
	}); rec.Code != http.StatusOK {
		t.Fatalf("authorize: %d %s", rec.Code, rec.Body.String())
	}
	bodies := make([]string, 10)
	codes := runConcurrently(10, func(i int) int {
		rec := doJSON(t, srv, tok, http.MethodPost, "/api/v1/payments/settle", map[string]any{"payment_id": "pay_pg_race"})
		bodies[i] = rec.Body.String()
		return rec.Code
	})
	for i, c := range codes {
		if c != http.StatusOK || bodies[i] != bodies[0] {
			t.Fatalf("settle %d: %d %s (first: %s)", i, c, bodies[i], bodies[0])
		}
	}
	var settled api.SettlePaymentResponse
	_ = json.Unmarshal([]byte(bodies[0]), &settled)
	if settled.GrossAmountCents != 25000 || settled.PlatformFeeCents != 500 {
		t.Fatalf("server-derived split expected (25000 gross, 200 bps fee), got %+v", settled)
	}
	if got := availableBalance(t, srv, tok, merchantID); got != settled.NetAvailableCents {
		t.Fatalf("available after 10 concurrent settles = %d, want %d", got, settled.NetAvailableCents)
	}
	if got := srv.GetMerchantMonthlySalesCents(merchantID); got != settled.BaseAmountCents {
		t.Fatalf("monthly sales = %d, want %d", got, settled.BaseAmountCents)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM payment_settlements WHERE payment_id = $1`, "pay_pg_race"); n != 1 {
		t.Fatalf("payment_settlements rows = %d", n)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM outbox_events WHERE event_type = 'payment.settled' AND aggregate_id = $1`, "pay_pg_race"); n != 1 {
		t.Fatalf("payment.settled events = %d", n)
	}
	afterSettle := settled.NetAvailableCents

	// 2. Concurrent duplicate STP abonos (same claveRastreo).
	signer := stp.NewSTPSigner(nil, nil, cfg.STPHMACSecret)
	canonical := signer.BuildAbonoCanonicalString(clabe, 50000, "SPEIPGRACE01", "40012", "012180001234567890")
	abono := map[string]any{
		"id": 77, "fechaOperacion": 20261120, "institucionOrdenante": "40012", "institucionBeneficiaria": "646",
		"claveRastreo": "SPEIPGRACE01", "monto": json.Number("500.00"), "cuentaOrdenante": "012180001234567890",
		"cuentaBeneficiario": clabe, "firma": signer.SignHMAC(canonical),
	}
	for i, c := range runConcurrently(10, func(int) int {
		return doJSON(t, srv, "", http.MethodPost, "/api/v1/webhooks/stp/abono", abono).Code
	}) {
		if c != http.StatusOK {
			t.Fatalf("abono %d: %d", i, c)
		}
	}
	if got := availableBalance(t, srv, tok, merchantID); got != afterSettle+50000 {
		t.Fatalf("available after duplicate abonos = %d, want %d", got, afterSettle+50000)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM outbox_events WHERE event_type = 'spei.abono_received'`); n != 1 {
		t.Fatalf("spei.abono_received events = %d", n)
	}
	afterAbono := afterSettle + 50000

	// 3. Concurrent CoDi confirmations of one tracking key, each re-signed
	// (distinct timestamps -> distinct signatures, like provider retries).
	codiBody := `{"tracking_key":"BNXPGRACE0001","seller_clabe":"` + clabe + `","buyer_clabe":"012180000118359719",` +
		`"amount_cents":11600,"timestamp":"2026-11-20T12:00:00Z","status":"APROBADO","banxico_approval_code":"AUT7"}`
	codiResp := make([]string, 10)
	for i, c := range runConcurrently(10, func(i int) int {
		ts := time.Now().Add(-time.Duration(i) * time.Second).Unix()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/webhooks/codi/confirm", strings.NewReader(codiBody))
		req.Header.Set(api.WebhookTimestampHeader, strconv.FormatInt(ts, 10))
		req.Header.Set(api.WebhookSignatureHeader, api.SignWebhook([]byte(cfg.CoDiWebhookSecret), ts, []byte(codiBody)))
		rec := httptest.NewRecorder()
		srv.Router().ServeHTTP(rec, req)
		codiResp[i] = rec.Body.String()
		return rec.Code
	}) {
		if c != http.StatusOK || codiResp[i] != codiResp[0] {
			t.Fatalf("codi %d: %d %s", i, c, codiResp[i])
		}
	}
	var codi api.CoDiWebhookResponse
	_ = json.Unmarshal([]byte(codiResp[0]), &codi)
	if got := availableBalance(t, srv, tok, merchantID); got != afterAbono+codi.NetAvailableCents {
		t.Fatalf("available after duplicate CoDi = %d, want %d", got, afterAbono+codi.NetAvailableCents)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM outbox_events WHERE event_type = 'codi.confirmed'`); n != 1 {
		t.Fatalf("codi.confirmed events = %d", n)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM money_operations`); n != 2 {
		t.Fatalf("money_operations rows = %d, want 2", n)
	}
}
