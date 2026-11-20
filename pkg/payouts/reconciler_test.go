package payouts

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"softpos/pkg/ledger"
	"softpos/pkg/payments"
	"softpos/pkg/store"
	"softpos/pkg/stp"
)

const merchant = "merch_recon"

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

type fixture struct {
	le   *ledger.LedgerEngine
	ms   *store.MemoryMoneyStore
	now  time.Time
	logs *syncBuffer
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	le := ledger.NewLedgerEngine()
	for code, typ := range map[string]ledger.AccountType{
		ConcentratorAccount:        ledger.AccountAsset,
		AvailableAccount(merchant): ledger.AccountLiability,
	} {
		if _, err := le.CreateAccount(code, typ, code); err != nil {
			t.Fatal(err)
		}
	}
	// Fund the merchant: concentrator (asset) up, available (liability) up.
	fund, _ := ledger.NewJournalEntry("fund", time.Now(), "fund", []ledger.Entry{
		{AccountID: ConcentratorAccount, Direction: ledger.Debit, Money: ledger.NewMoney(100000, "MXN")},
		{AccountID: AvailableAccount(merchant), Direction: ledger.Credit, Money: ledger.NewMoney(100000, "MXN")},
	}, nil)
	if err := le.PostTransaction(fund); err != nil {
		t.Fatal(err)
	}
	merchants := store.NewMemoryMerchantRepository()
	ms := store.NewMemoryMoneyStore(le, func() payments.PaymentProcessor { return nil }, func() store.MerchantRepository { return merchants })
	return &fixture{le: le, ms: ms, now: time.Now(), logs: &syncBuffer{}}
}

func (f *fixture) available(t *testing.T) int64 {
	t.Helper()
	b, err := f.le.GetBalance(AvailableAccount(merchant))
	if err != nil {
		t.Fatal(err)
	}
	return b.AmountCents
}

// reserve creates a payout in status, debited like the handler does, last
// updated age ago.
func (f *fixture) reserve(t *testing.T, clave string, status store.PayoutStatus, age time.Duration) store.PayoutRecord {
	t.Helper()
	p := store.PayoutRecord{ClaveRastreo: clave, MerchantID: merchant, IdempotencyKey: "k-" + clave, RequestHash: "h",
		AmountCents: 10000, DestinationCLABE: "032180000118359719", FechaOperacion: "20261120"}
	ctx := context.Background()
	if err := f.ms.InTx(ctx, func(tx store.MoneyTx) error {
		if _, _, err := tx.CreatePayout(ctx, p); err != nil {
			return err
		}
		j, err := ReservationJournal(p, f.now)
		if err != nil {
			return err
		}
		if err := tx.PostJournal(ctx, j); err != nil {
			return err
		}
		if status != store.PayoutReserved {
			return tx.SetPayoutStatus(ctx, clave, store.PayoutReserved, status, 0, "")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.ms.SetPayoutUpdatedAt(clave, f.now.Add(-age))
	return p
}

func (f *fixture) reconciler(client stp.OrderStatusClient) *Reconciler {
	return NewReconciler(f.ms, f.ms, client, ReconcilerConfig{
		After:  10 * time.Minute,
		Now:    func() time.Time { return f.now },
		Logger: slog.New(slog.NewTextHandler(f.logs, nil)),
	})
}

func (f *fixture) status(t *testing.T, clave string) store.PayoutRecord {
	t.Helper()
	p, ok := f.ms.Payout(clave)
	if !ok {
		t.Fatalf("payout %s missing", clave)
	}
	return p
}

func (f *fixture) events(eventType string) int {
	n := 0
	for _, e := range f.ms.Events() {
		if e.EventType == eventType {
			n++
		}
	}
	return n
}

func TestReconciler_FinalizesEachSTPOutcome(t *testing.T) {
	f := newFixture(t)
	f.reserve(t, "KRNLIQ", store.PayoutPendingReconciliation, time.Hour)
	f.reserve(t, "KRNACC", store.PayoutReserved, time.Hour)
	f.reserve(t, "KRNREJ", store.PayoutPendingReconciliation, time.Hour)
	f.reserve(t, "KRNNF", store.PayoutReserved, time.Hour)
	f.reserve(t, "KRNUNK", store.PayoutPendingReconciliation, time.Hour)
	f.reserve(t, "KRNERR", store.PayoutReserved, time.Hour)
	if got := f.available(t); got != 40000 {
		t.Fatalf("after 6 reservations available=%d", got)
	}
	m := stp.NewMockOrderStatusClient()
	m.Set("KRNLIQ", stp.OrderStatus{State: stp.OrderLiquidated, STPOrderID: 11})
	m.Set("KRNACC", stp.OrderStatus{State: stp.OrderAccepted, STPOrderID: 12})
	m.Set("KRNREJ", stp.OrderStatus{State: stp.OrderRejected, Detail: "cuenta inexistente"})
	m.Set("KRNNF", stp.OrderStatus{State: stp.OrderNotFound})
	m.Set("KRNUNK", stp.OrderStatus{State: stp.OrderUnknown})
	m.SetError("KRNERR", stp.ErrOrderStatusUnavailable)

	res, err := f.reconciler(m).RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if res != (Result{Settled: 1, Dispatched: 1, Compensated: 2, Unknown: 2}) {
		t.Fatalf("result %+v", res)
	}
	for clave, want := range map[string]store.PayoutStatus{
		"KRNLIQ": store.PayoutSettled, "KRNACC": store.PayoutDispatched, "KRNREJ": store.PayoutFailed,
		"KRNNF": store.PayoutFailed, "KRNUNK": store.PayoutPendingReconciliation, "KRNERR": store.PayoutReserved,
	} {
		if got := f.status(t, clave).Status; got != want {
			t.Errorf("%s: status %s want %s", clave, got, want)
		}
	}
	if f.status(t, "KRNLIQ").STPOrderID != 11 || !strings.Contains(f.status(t, "KRNREJ").LastError, "cuenta inexistente") {
		t.Errorf("order id / reason not recorded")
	}
	// Two compensations returned 20000; unknown ones stay reserved.
	if got := f.available(t); got != 60000 {
		t.Fatalf("available=%d want 60000", got)
	}
	if f.events("disbursement.settled") != 1 || f.events("disbursement.dispatched") != 1 || f.events("disbursement.failed") != 2 {
		t.Fatalf("events: %+v", f.ms.Events())
	}
	if logs := f.logs.String(); strings.Count(logs, "ALERT payout unresolved") != 2 || !strings.Contains(logs, "KRNERR") {
		t.Fatalf("unknown outcomes must raise alerts:\n%s", logs)
	}

	// Second pass: finalized payouts are no longer candidates; nothing is
	// compensated twice; the unknown ones are retried.
	m.Set("KRNUNK", stp.OrderStatus{State: stp.OrderRejected})
	res, err = f.reconciler(m).RunOnce(context.Background())
	if err != nil || res != (Result{Compensated: 1, Unknown: 1}) {
		t.Fatalf("second pass %+v %v", res, err)
	}
	if m.Calls("KRNREJ") != 1 || m.Calls("KRNUNK") != 2 {
		t.Fatalf("finalized payouts must not be re-queried: rej=%d unk=%d", m.Calls("KRNREJ"), m.Calls("KRNUNK"))
	}
	if got := f.available(t); got != 70000 {
		t.Fatalf("available=%d want 70000", got)
	}
}

func TestReconciler_LeavesYoungAndFinalPayoutsAlone(t *testing.T) {
	f := newFixture(t)
	f.reserve(t, "KRNYOUNG", store.PayoutReserved, time.Minute) // dispatch may still be in flight
	f.reserve(t, "KRNDONE", store.PayoutDispatched, time.Hour)
	m := stp.NewMockOrderStatusClient()
	m.Set("KRNYOUNG", stp.OrderStatus{State: stp.OrderNotFound})
	m.Set("KRNDONE", stp.OrderStatus{State: stp.OrderNotFound})
	res, err := f.reconciler(m).RunOnce(context.Background())
	if err != nil || res != (Result{}) {
		t.Fatalf("%+v %v", res, err)
	}
	if m.Calls("KRNYOUNG")+m.Calls("KRNDONE") != 0 || f.available(t) != 80000 {
		t.Fatalf("young/final payouts touched")
	}
}

// staleRepo returns a snapshot taken before another worker finalized the payout.
type staleRepo struct{ list []store.PayoutRecord }

func (s staleRepo) ListPayoutsForRecovery(context.Context, time.Time, int) ([]store.PayoutRecord, error) {
	return s.list, nil
}

func TestReconciler_SkipsPayoutFinalizedConcurrently(t *testing.T) {
	f := newFixture(t)
	p := f.reserve(t, "KRNRACE", store.PayoutPendingReconciliation, time.Hour)
	snapshot, _ := f.ms.ListPayoutsForRecovery(context.Background(), f.now, 10)
	// The handler / another reconciler finalizes it after the scan.
	if err := f.ms.InTx(context.Background(), func(tx store.MoneyTx) error {
		return tx.SetPayoutStatus(context.Background(), p.ClaveRastreo, store.PayoutPendingReconciliation, store.PayoutDispatched, 5, "")
	}); err != nil {
		t.Fatal(err)
	}
	m := stp.NewMockOrderStatusClient()
	m.Set("KRNRACE", stp.OrderStatus{State: stp.OrderRejected})
	r := NewReconciler(staleRepo{snapshot}, f.ms, m, ReconcilerConfig{Now: func() time.Time { return f.now }, Logger: slog.New(slog.NewTextHandler(f.logs, nil))})
	res, err := r.RunOnce(context.Background())
	if err != nil || res != (Result{Skipped: 1}) {
		t.Fatalf("%+v %v", res, err)
	}
	if f.status(t, "KRNRACE").Status != store.PayoutDispatched || f.available(t) != 90000 {
		t.Fatalf("a finalized payout must never be compensated")
	}
}

func TestReconciler_RunStopsOnCancel(t *testing.T) {
	f := newFixture(t)
	f.reserve(t, "KRNRUN", store.PayoutReserved, time.Hour)
	m := stp.NewMockOrderStatusClient()
	m.Set("KRNRUN", stp.OrderStatus{State: stp.OrderLiquidated, STPOrderID: 1})
	ctx, cancel := context.WithCancel(context.Background())
	r := NewReconciler(f.ms, f.ms, m, ReconcilerConfig{Interval: 5 * time.Millisecond, Now: func() time.Time { return f.now }})
	done := make(chan error, 1)
	go func() { done <- r.Run(ctx) }()
	deadline := time.Now().Add(2 * time.Second)
	for f.status(t, "KRNRUN").Status != store.PayoutSettled {
		if time.Now().After(deadline) {
			t.Fatal("reconciler did not run")
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop")
	}
}

func TestCompensationJournal_IsIdempotentByID(t *testing.T) {
	f := newFixture(t)
	p := f.reserve(t, "KRNDUP", store.PayoutReserved, time.Hour)
	j1, _ := CompensationJournal(p, f.now, "x")
	j2, _ := CompensationJournal(p, f.now, "y")
	if err := f.ms.InTx(context.Background(), func(tx store.MoneyTx) error { return tx.PostJournal(context.Background(), j1) }); err != nil {
		t.Fatal(err)
	}
	err := f.ms.InTx(context.Background(), func(tx store.MoneyTx) error { return tx.PostJournal(context.Background(), j2) })
	if !errors.Is(err, store.ErrDuplicateJournal) {
		t.Fatalf("second compensation must be rejected, got %v", err)
	}
}
