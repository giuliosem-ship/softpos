package main

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"softpos/pkg/api"
	"softpos/pkg/auth"
	"softpos/pkg/payouts"
	"softpos/pkg/store"
	"softpos/pkg/store/pgtest"
	"softpos/pkg/stp"
)

const pgPayoutCLABE = "032180000118359719"

// setupPGPayoutMerchant builds a PostgreSQL-backed server with a merchant
// funded with $1,000.00 (signed STP abono) and a verified settlement CLABE.
func setupPGPayoutMerchant(t *testing.T, m string) (*api.Server, *sql.DB, string) {
	t.Helper()
	cfg := Config{SQLDriver: "pgx", DatabaseURL: pgtest.URL(t), STPHMACSecret: "test_secret", JWTSecret: testJWTSecret, VaultMasterKeyHex: testVaultKeyHex}
	srv, db, _, err := BuildServer(context.Background(), cfg)
	if err != nil {
		t.Fatalf("BuildServer: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := srv.RegisterMerchantCLABE("646180123400000007", m, "GARM850101ABC"); err != nil {
		t.Fatal(err)
	}
	owner, _ := srv.AuthService().MintAccessToken("usr_"+m, m, auth.RoleOwner)
	operator, _ := srv.AuthService().MintAccessToken("usr_op_"+m, m, auth.RoleOperator)
	signer := stp.NewSTPSigner(nil, nil, cfg.STPHMACSecret)
	canonical := signer.BuildAbonoCanonicalString("646180123400000007", 100000, "SPEIPGIDEM01", "40012", "012180001234567890")
	if rec := doJSON(t, srv, "", http.MethodPost, "/api/v1/webhooks/stp/abono", map[string]any{
		"id": 1, "claveRastreo": "SPEIPGIDEM01", "monto": json.Number("1000.00"), "institucionOrdenante": "40012",
		"cuentaOrdenante": "012180001234567890", "cuentaBeneficiario": "646180123400000007", "firma": signer.SignHMAC(canonical),
	}); rec.Code != http.StatusOK {
		t.Fatalf("abono: %d %s", rec.Code, rec.Body.String())
	}
	acctPath := "/api/v1/merchants/" + m + "/settlement-account"
	if rec := doJSON(t, srv, owner, http.MethodPut, acctPath, map[string]any{"clabe": pgPayoutCLABE}); rec.Code != http.StatusOK {
		t.Fatalf("register settlement account: %d %s", rec.Code, rec.Body.String())
	}
	if rec := doJSON(t, srv, operator, http.MethodPost, acctPath+"/verify", map[string]any{"clabe": pgPayoutCLABE, "status": "VERIFIED"}); rec.Code != http.StatusOK {
		t.Fatalf("verify: %d %s", rec.Code, rec.Body.String())
	}
	if got := availableBalance(t, srv, owner, m); got != 100000 {
		t.Fatalf("funded available=%d", got)
	}
	return srv, db, owner
}

func payoutWithKey(t *testing.T, srv *api.Server, tok, key string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	b, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/disbursements", bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Idempotency-Key", key)
	rec := httptest.NewRecorder()
	srv.Router().ServeHTTP(rec, req)
	return rec
}

// TestPGIntegration_PayoutIdempotencyKey: concurrent retries with one key pay
// once, a reused key with another body is 409, a missing key is 400.
func TestPGIntegration_PayoutIdempotencyKey(t *testing.T) {
	const m = "merch_pg_idem"
	srv, db, owner := setupPGPayoutMerchant(t, m)

	bodies := make([]string, 10)
	codes := runConcurrently(10, func(i int) int {
		rec := payoutWithKey(t, srv, owner, "retry-1", map[string]any{"amount_cents": 30000})
		bodies[i] = rec.Body.String()
		return rec.Code
	})
	claves := map[string]bool{}
	for i, c := range codes {
		if c != http.StatusOK && c != http.StatusAccepted {
			t.Fatalf("request %d: %d %s", i, c, bodies[i])
		}
		var r struct {
			ClaveRastreo string `json:"clave_rastreo"`
		}
		_ = json.Unmarshal([]byte(bodies[i]), &r)
		claves[r.ClaveRastreo] = true
	}
	if len(claves) != 1 || availableBalance(t, srv, owner, m) != 70000 {
		t.Fatalf("same key must pay once: claves=%v available=%d", claves, availableBalance(t, srv, owner, m))
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM payouts WHERE merchant_id = $1`, m); n != 1 {
		t.Fatalf("payout rows=%d", n)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM outbox_events WHERE event_type = 'disbursement.requested'`); n != 1 {
		t.Fatalf("disbursement.requested events=%d", n)
	}
	if rec := payoutWithKey(t, srv, owner, "retry-1", map[string]any{"amount_cents": 30001}); rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "IDEMPOTENCY_KEY_REUSED") {
		t.Fatalf("reused key with another body: %d %s", rec.Code, rec.Body.String())
	}
	if rec := payoutWithKey(t, srv, owner, "", map[string]any{"amount_cents": 100}); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing key: %d %s", rec.Code, rec.Body.String())
	}
	if rec := payoutWithKey(t, srv, owner, "retry-2", map[string]any{"amount_cents": 30000}); rec.Code != http.StatusOK {
		t.Fatalf("new key: %d %s", rec.Code, rec.Body.String())
	}
	if got := availableBalance(t, srv, owner, m); got != 40000 {
		t.Fatalf("available=%d", got)
	}
	var status string
	_ = db.QueryRow(`SELECT status FROM payouts WHERE idempotency_key = 'retry-2'`).Scan(&status)
	if status != string(store.PayoutDispatched) {
		t.Fatalf("status=%s", status)
	}
}

type pgAmbiguousSTP struct{}

func (pgAmbiguousSTP) RegisterOrder(context.Context, stp.STPOrderRequest) (*stp.STPOrderResponse, error) {
	return nil, fmt.Errorf("%w: i/o timeout", stp.ErrSTPNetworkError)
}

// TestPGIntegration_PayoutRecoveryResolvesUnknownAndCrashedPayouts: one payout
// gets an unknown STP outcome (timeout), another is left RESERVED as if the
// process died before registraOrden. The reconciler, against real
// PostgreSQL, finalizes the confirmed one, compensates the never-registered
// one exactly once and keeps an unknown one reserved.
func TestPGIntegration_PayoutRecoveryResolvesUnknownAndCrashedPayouts(t *testing.T) {
	const m = "merch_pg_recovery"
	srv, db, owner := setupPGPayoutMerchant(t, m)
	ctx := context.Background()

	srv.SetSTPClient(pgAmbiguousSTP{})
	rec := payoutWithKey(t, srv, owner, "timeout-1", map[string]any{"amount_cents": 20000})
	if rec.Code != http.StatusAccepted {
		t.Fatalf("unknown outcome: %d %s", rec.Code, rec.Body.String())
	}
	var unknown struct {
		ClaveRastreo string `json:"clave_rastreo"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &unknown)
	rec = payoutWithKey(t, srv, owner, "timeout-2", map[string]any{"amount_cents": 5000})
	var stillUnknown struct {
		ClaveRastreo string `json:"clave_rastreo"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &stillUnknown)

	// Crash window: reservation committed, registraOrden never sent.
	crashed := store.PayoutRecord{ClaveRastreo: "KRNPGCRASH01", MerchantID: m, IdempotencyKey: "crash-1", RequestHash: "h",
		AmountCents: 10000, DestinationCLABE: pgPayoutCLABE, FechaOperacion: time.Now().Format("20060102")}
	ms := store.NewSQLMoneyStore(db)
	if err := ms.InTx(ctx, func(tx store.MoneyTx) error {
		if _, _, err := tx.CreatePayout(ctx, crashed); err != nil {
			return err
		}
		j, err := payouts.ReservationJournal(crashed, time.Now())
		if err != nil {
			return err
		}
		return tx.PostJournal(ctx, j)
	}); err != nil {
		t.Fatal(err)
	}
	if got := availableBalance(t, srv, owner, m); got != 65000 {
		t.Fatalf("all three reserved: available=%d", got)
	}
	var st string
	_ = db.QueryRow(`SELECT status FROM payouts WHERE clave_rastreo = $1`, unknown.ClaveRastreo).Scan(&st)
	if st != string(store.PayoutPendingReconciliation) {
		t.Fatalf("unknown payout status=%s", st)
	}
	if _, err := db.Exec(`UPDATE payouts SET updated_at = NOW() - INTERVAL '1 hour'`); err != nil {
		t.Fatal(err)
	}

	statuses := stp.NewMockOrderStatusClient()
	statuses.Set(unknown.ClaveRastreo, stp.OrderStatus{State: stp.OrderLiquidated, STPOrderID: 4242})
	statuses.Set(crashed.ClaveRastreo, stp.OrderStatus{State: stp.OrderNotFound})
	statuses.Set(stillUnknown.ClaveRastreo, stp.OrderStatus{State: stp.OrderUnknown})
	r := payouts.NewReconciler(store.NewSQLPayoutRepository(db), ms, statuses, payouts.ReconcilerConfig{})
	res, err := r.RunOnce(ctx)
	if err != nil || res != (payouts.Result{Settled: 1, Compensated: 1, Unknown: 1}) {
		t.Fatalf("recovery: %+v %v", res, err)
	}
	// Crashed payout's 10000 returned; the liquidated and unknown ones stay debited.
	if got := availableBalance(t, srv, owner, m); got != 75000 {
		t.Fatalf("available after recovery=%d want 75000", got)
	}
	var orderID int64
	_ = db.QueryRow(`SELECT status, stp_order_id FROM payouts WHERE clave_rastreo = $1`, unknown.ClaveRastreo).Scan(&st, &orderID)
	if st != string(store.PayoutSettled) || orderID != 4242 {
		t.Fatalf("liquidated payout: %s %d", st, orderID)
	}
	_ = db.QueryRow(`SELECT status FROM payouts WHERE clave_rastreo = $1`, crashed.ClaveRastreo).Scan(&st)
	if st != string(store.PayoutFailed) {
		t.Fatalf("crashed payout: %s", st)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM ledger_journals WHERE id = $1`, payouts.CompensationJournalID(crashed.ClaveRastreo)); n != 1 {
		t.Fatalf("compensation journals=%d", n)
	}
	if n := countRows(t, db, `SELECT COUNT(*) FROM outbox_events WHERE event_type IN ('disbursement.settled', 'disbursement.failed')`); n != 2 {
		t.Fatalf("recovery events=%d", n)
	}

	// Idempotent: a second pass only re-checks the unknown one.
	res, err = r.RunOnce(ctx)
	if err != nil || res != (payouts.Result{Unknown: 1}) || availableBalance(t, srv, owner, m) != 75000 {
		t.Fatalf("second pass: %+v %v available=%d", res, err, availableBalance(t, srv, owner, m))
	}
	// A replay of the recovered request now reports the final state.
	srv.SetSTPClient(stp.NewMockSTPClient(nil, ""))
	rec = payoutWithKey(t, srv, owner, "timeout-1", map[string]any{"amount_cents": 20000})
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"status":"SETTLED"`) {
		t.Fatalf("replay after recovery: %d %s", rec.Code, rec.Body.String())
	}
}

// TestPGIntegration_PayoutReconcilerWorkerRuns: the cmd/server worker uses the
// server's STP client (MockSTPClient: accepted orders liquidated, others not
// found) and stops on cancel.
func TestPGIntegration_PayoutReconcilerWorkerRuns(t *testing.T) {
	const m = "merch_pg_worker"
	srv, db, owner := setupPGPayoutMerchant(t, m)
	crashed := store.PayoutRecord{ClaveRastreo: "KRNPGWORKER1", MerchantID: m, IdempotencyKey: "w-1", RequestHash: "h",
		AmountCents: 10000, DestinationCLABE: pgPayoutCLABE, FechaOperacion: time.Now().Format("20060102")}
	ctx := context.Background()
	if err := store.NewSQLMoneyStore(db).InTx(ctx, func(tx store.MoneyTx) error {
		if _, _, err := tx.CreatePayout(ctx, crashed); err != nil {
			return err
		}
		j, _ := payouts.ReservationJournal(crashed, time.Now())
		return tx.PostJournal(ctx, j)
	}); err != nil {
		t.Fatal(err)
	}
	_, _ = db.Exec(`UPDATE payouts SET updated_at = NOW() - INTERVAL '1 hour'`)
	wctx, cancel := context.WithCancel(ctx)
	wait := StartPayoutReconciler(wctx, Config{PayoutReconcileInterval: 10 * time.Millisecond, PayoutReconcileAfter: time.Minute}, db, srv)
	deadline := time.Now().Add(5 * time.Second)
	for availableBalance(t, srv, owner, m) != 100000 {
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("worker did not compensate the crashed payout")
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	wait()
}
