package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"softpos/pkg/payouts"
	"softpos/pkg/store"
	"softpos/pkg/stp"
)

// countingSTP accepts every order and counts registraOrden calls.
type countingSTP struct{ calls atomic.Int64 }

func (c *countingSTP) RegisterOrder(_ context.Context, o stp.STPOrderRequest) (*stp.STPOrderResponse, error) {
	n := c.calls.Add(1)
	return &stp.STPOrderResponse{STPOrderID: 900 + n, ClaveRastreo: o.ClaveRastreo}, nil
}

func payoutReq(t *testing.T, s *Server, m, key string, body map[string]any) *http.Request {
	t.Helper()
	req := asTenant(t, s, m, jsonReq("POST", "/api/v1/disbursements", body))
	if key == "" {
		req.Header.Del("Idempotency-Key")
	} else {
		req.Header.Set("Idempotency-Key", key)
	}
	return req
}

func setupPayoutMerchant(t *testing.T, m string, cents int64) *Server {
	t.Helper()
	s, _ := setupTestServer(t)
	_ = s.RegisterMerchant(m, "GARM850101ABC", "M", true)
	if err := s.SeedVerifiedSettlementAccount(context.Background(), m, settlementCLABE); err != nil {
		t.Fatal(err)
	}
	fundMerchant(t, s, m, cents)
	return s
}

func TestDisbursement_RequiresIdempotencyKey(t *testing.T) {
	const m = "merch_payout_nokey"
	s := setupPayoutMerchant(t, m, 50000)
	rec := serve(s, payoutReq(t, s, m, "", map[string]any{"amount_cents": 1000}))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "MISSING_IDEMPOTENCY_KEY") {
		t.Fatalf("want 400 MISSING_IDEMPOTENCY_KEY, got %d %s", rec.Code, rec.Body.String())
	}
	rec = serve(s, payoutReq(t, s, m, strings.Repeat("k", 256), map[string]any{"amount_cents": 1000}))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "INVALID_IDEMPOTENCY_KEY") {
		t.Fatalf("want 400 INVALID_IDEMPOTENCY_KEY, got %d %s", rec.Code, rec.Body.String())
	}
	if got := availableCents(t, s, m); got != 50000 {
		t.Fatalf("no funds may move without a key, available=%d", got)
	}
}

func TestDisbursement_SameKeySameBodyReplaysWithoutPayingTwice(t *testing.T) {
	const m = "merch_payout_replay"
	s := setupPayoutMerchant(t, m, 50000)
	cs := &countingSTP{}
	s.SetSTPClient(cs)
	body := map[string]any{"amount_cents": 20000, "concept": "retiro"}

	first := serve(s, payoutReq(t, s, m, "k-1", body))
	if first.Code != http.StatusOK {
		t.Fatalf("first payout: %d %s", first.Code, first.Body.String())
	}
	second := serve(s, payoutReq(t, s, m, "k-1", body))
	if second.Code != http.StatusOK || second.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay: %d %s (replayed=%q)", second.Code, second.Body.String(), second.Header().Get("Idempotent-Replayed"))
	}
	var a, b DisbursementResponse
	_ = json.Unmarshal(first.Body.Bytes(), &a)
	_ = json.Unmarshal(second.Body.Bytes(), &b)
	if a.ClaveRastreo == "" || a.ClaveRastreo != b.ClaveRastreo || a.STPOrderID != b.STPOrderID || b.RemainingBalanceCents != 30000 {
		t.Fatalf("replay must return the first result: %+v vs %+v", a, b)
	}
	if cs.calls.Load() != 1 || availableCents(t, s, m) != 30000 || len(memEvents(t, s, "disbursement.requested")) != 1 {
		t.Fatalf("paid more than once: stp calls=%d available=%d", cs.calls.Load(), availableCents(t, s, m))
	}
}

func TestDisbursement_SameKeyDifferentBodyIs409(t *testing.T) {
	const m = "merch_payout_conflict"
	s := setupPayoutMerchant(t, m, 50000)
	if rec := serve(s, payoutReq(t, s, m, "k-2", map[string]any{"amount_cents": 10000})); rec.Code != http.StatusOK {
		t.Fatalf("first payout: %d %s", rec.Code, rec.Body.String())
	}
	rec := serve(s, payoutReq(t, s, m, "k-2", map[string]any{"amount_cents": 15000}))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "IDEMPOTENCY_KEY_REUSED") {
		t.Fatalf("want 409 IDEMPOTENCY_KEY_REUSED, got %d %s", rec.Code, rec.Body.String())
	}
	if got := availableCents(t, s, m); got != 40000 {
		t.Fatalf("available=%d", got)
	}
}

func TestDisbursement_KeyIsScopedPerMerchant(t *testing.T) {
	const a, b = "merch_payout_scope_a", "merch_payout_scope_b"
	s := setupPayoutMerchant(t, a, 50000)
	_ = s.RegisterMerchant(b, "GARM850101ABD", "B", true)
	_ = s.SeedVerifiedSettlementAccount(context.Background(), b, settlementCLABE)
	fundMerchant(t, s, b, 50000)
	body := map[string]any{"amount_cents": 10000}
	if rec := serve(s, payoutReq(t, s, a, "shared", body)); rec.Code != http.StatusOK {
		t.Fatalf("a: %d %s", rec.Code, rec.Body.String())
	}
	rec := serve(s, payoutReq(t, s, b, "shared", body))
	if rec.Code != http.StatusOK || rec.Header().Get("Idempotent-Replayed") != "" {
		t.Fatalf("b must get its own payout: %d %s", rec.Code, rec.Body.String())
	}
	if availableCents(t, s, a) != 40000 || availableCents(t, s, b) != 40000 {
		t.Fatalf("a=%d b=%d", availableCents(t, s, a), availableCents(t, s, b))
	}
}

func TestDisbursement_ConcurrentSameKeyDebitsOnce(t *testing.T) {
	const m = "merch_payout_samekey_race"
	s := setupPayoutMerchant(t, m, 100000)
	cs := &countingSTP{}
	s.SetSTPClient(cs)
	var wg sync.WaitGroup
	codes := make([]int, 10)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i] = serve(s, payoutReq(t, s, m, "race-key", map[string]any{"amount_cents": 30000})).Code
		}(i)
	}
	wg.Wait()
	for _, c := range codes {
		if c != http.StatusOK && c != http.StatusAccepted {
			t.Fatalf("unexpected status %d (%v)", c, codes)
		}
	}
	if cs.calls.Load() != 1 || availableCents(t, s, m) != 70000 {
		t.Fatalf("same key must debit once: stp calls=%d available=%d", cs.calls.Load(), availableCents(t, s, m))
	}
}

func TestDisbursement_ReplayOfUnknownAndFailedOutcomes(t *testing.T) {
	const m = "merch_payout_replay_outcomes"
	s := setupPayoutMerchant(t, m, 50000)
	s.SetSTPClient(ambiguousSTP{})
	body := map[string]any{"amount_cents": 20000}
	if rec := serve(s, payoutReq(t, s, m, "unknown-1", body)); rec.Code != http.StatusAccepted {
		t.Fatalf("want 202, got %d", rec.Code)
	}
	s.SetSTPClient(&countingSTP{}) // a retry must not re-dispatch
	rec := serve(s, payoutReq(t, s, m, "unknown-1", body))
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), "PENDING_RECONCILIATION") {
		t.Fatalf("replay of unknown outcome: %d %s", rec.Code, rec.Body.String())
	}

	s.SetSTPClient(failingSTP{})
	if rec := serve(s, payoutReq(t, s, m, "failed-1", body)); rec.Code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d", rec.Code)
	}
	rec = serve(s, payoutReq(t, s, m, "failed-1", body))
	if rec.Code != http.StatusBadGateway || rec.Header().Get("Idempotent-Replayed") != "true" {
		t.Fatalf("replay of failure: %d %s", rec.Code, rec.Body.String())
	}
	// 50000 - 20000 reserved (unknown) ; the failed one was compensated exactly once.
	if got := availableCents(t, s, m); got != 30000 {
		t.Fatalf("available=%d", got)
	}
}

// TestDisbursement_CrashAfterReservationIsRecovered: the process dies after
// the funds reservation commits and before registraOrden (the payout row is
// left RESERVED). The reconciler then asks STP: never registered ->
// compensating journal and FAILED.
func TestDisbursement_CrashAfterReservationIsRecovered(t *testing.T) {
	const m = "merch_payout_crash"
	s := setupPayoutMerchant(t, m, 50000)
	ms := s.moneyStore().(*store.MemoryMoneyStore)
	// Simulate the crash window: reserve exactly like the handler does, then stop.
	p := store.PayoutRecord{ClaveRastreo: "KRNCRASH0001", MerchantID: m, IdempotencyKey: "crash-1", RequestHash: "h",
		AmountCents: 20000, DestinationCLABE: settlementCLABE, FechaOperacion: "20261120"}
	if err := ms.InTx(context.Background(), func(tx store.MoneyTx) error {
		if _, _, err := tx.CreatePayout(context.Background(), p); err != nil {
			return err
		}
		j, err := payouts.ReservationJournal(p, time.Now())
		if err != nil {
			return err
		}
		return tx.PostJournal(context.Background(), j)
	}); err != nil {
		t.Fatal(err)
	}
	if got := availableCents(t, s, m); got != 30000 {
		t.Fatalf("reserved: available=%d", got)
	}
	ms.SetPayoutUpdatedAt(p.ClaveRastreo, time.Now().Add(-time.Hour))
	rec := payouts.NewReconciler(ms, ms, stp.NewMockSTPClient(nil, ""), payouts.ReconcilerConfig{})
	res, err := rec.RunOnce(context.Background())
	if err != nil || res.Compensated != 1 {
		t.Fatalf("recovery: %+v %v", res, err)
	}
	if got := availableCents(t, s, m); got != 50000 {
		t.Fatalf("funds must return after recovery, available=%d", got)
	}
	if got, _ := ms.Payout(p.ClaveRastreo); got.Status != store.PayoutFailed {
		t.Fatalf("status=%s", got.Status)
	}
}
