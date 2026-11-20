package payouts

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"softpos/pkg/ledger"
	"softpos/pkg/payments"
	"softpos/pkg/store"
)

func seedCapture(t *testing.T, ms *store.MemoryMoneyStore, id, settlementDate, payoutID string) {
	t.Helper()
	ctx := context.Background()
	err := ms.InTx(ctx, func(tx store.MoneyTx) error {
		cp, _, err := tx.CreateCardPayment(ctx, store.CardPaymentRecord{
			ID: id, MerchantID: merchant, Provider: "mock", ProviderPaymentID: "pi_" + id,
			IntentIdempotencyKey: id, RequestHash: "h", AmountCents: 10000,
		})
		if err != nil {
			return err
		}
		cp.Status, cp.CapturedCents, cp.ProviderFeeCents = store.CardCaptured, 10000, 250
		cp.ProviderSettlementDate, cp.ProviderPayoutID = settlementDate, payoutID
		return tx.UpdateCardPayment(ctx, cp)
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestAcquirerSettlementWatcher_FlagsOverdueCapturesOnce(t *testing.T) {
	ms := store.NewMemoryMoneyStore(ledger.NewLedgerEngine(), func() payments.PaymentProcessor { return nil },
		func() store.MerchantRepository { return store.NewMemoryMerchantRepository() })
	seedCapture(t, ms, "cp_overdue", "2026-11-18", "")
	seedCapture(t, ms, "cp_due_yesterday", "2026-11-19", "") // within the 1-day grace
	seedCapture(t, ms, "cp_paid", "2026-11-10", "po_1")
	seedCapture(t, ms, "cp_future", "2026-11-22", "")

	logs := &syncBuffer{}
	// 2026-11-20 10:00 in Mexico City (16:00 UTC).
	now := func() time.Time { return time.Date(2026, 11, 20, 16, 0, 0, 0, time.UTC) }
	w := NewAcquirerSettlementWatcher(ms, AcquirerSettlementConfig{Now: now, Logger: slog.New(slog.NewTextHandler(logs, nil))})
	n, err := w.RunOnce(context.Background())
	if err != nil || n != 1 {
		t.Fatalf("first pass: n=%d err=%v", n, err)
	}
	if n, _ := w.RunOnce(context.Background()); n != 0 {
		t.Fatalf("second pass must not duplicate: %d", n)
	}
	items, _ := ms.ListReconciliationItems(context.Background(), "OPEN", 0)
	if len(items) != 1 || items[0].Kind != store.ReconNotSettled || items[0].CardPaymentID != "cp_overdue" || items[0].ExpectedCents != 9750 {
		t.Fatalf("items: %+v", items)
	}
	if !strings.Contains(logs.String(), "ALERT acquirer reconciliation discrepancy") || !strings.Contains(logs.String(), "level=ERROR") {
		t.Fatalf("alert log missing: %s", logs.String())
	}

	// Dates are Mexico City calendar days: 2026-11-21 04:00 UTC is still the
	// 20th at 22:00 local, so the capture due on the 19th is not overdue yet.
	evening := func() time.Time { return time.Date(2026, 11, 21, 4, 0, 0, 0, time.UTC) }
	w2 := NewAcquirerSettlementWatcher(ms, AcquirerSettlementConfig{Now: evening, Logger: slog.New(slog.NewTextHandler(logs, nil))})
	if n, _ := w2.RunOnce(context.Background()); n != 0 {
		t.Fatalf("20th local evening: %d", n)
	}
	nextDay := func() time.Time { return time.Date(2026, 11, 21, 6, 30, 0, 0, time.UTC) } // 00:30 local on the 21st
	w3 := NewAcquirerSettlementWatcher(ms, AcquirerSettlementConfig{Now: nextDay, Logger: slog.New(slog.NewTextHandler(logs, nil))})
	if n, _ := w3.RunOnce(context.Background()); n != 1 {
		t.Fatalf("21st local: %d", n)
	}
}

func TestAcquirerSettlementWatcher_RunStops(t *testing.T) {
	ms := store.NewMemoryMoneyStore(ledger.NewLedgerEngine(), func() payments.PaymentProcessor { return nil },
		func() store.MerchantRepository { return store.NewMemoryMerchantRepository() })
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	w := NewAcquirerSettlementWatcher(ms, AcquirerSettlementConfig{Interval: 10 * time.Millisecond, Logger: slog.New(slog.NewTextHandler(&syncBuffer{}, nil))})
	go func() { _ = w.Run(ctx); close(done) }()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not stop")
	}
}

// Captures already flagged must not fill the batch: with more overdue captures
// than BatchSize, every run reaches new ones instead of re-listing the same.
func TestAcquirerSettlementWatcher_BatchDoesNotStarveOnFlaggedCaptures(t *testing.T) {
	ms := store.NewMemoryMoneyStore(ledger.NewLedgerEngine(), func() payments.PaymentProcessor { return nil },
		func() store.MerchantRepository { return store.NewMemoryMerchantRepository() })
	seedCapture(t, ms, "cp_a", "2026-11-10", "")
	seedCapture(t, ms, "cp_b", "2026-11-11", "")
	now := func() time.Time { return time.Date(2026, 11, 20, 16, 0, 0, 0, time.UTC) }
	w := NewAcquirerSettlementWatcher(ms, AcquirerSettlementConfig{Now: now, BatchSize: 1, Logger: slog.New(slog.NewTextHandler(&syncBuffer{}, nil))})
	total := 0
	for i := 0; i < 3; i++ {
		n, err := w.RunOnce(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		total += n
	}
	if total != 2 {
		t.Fatalf("flagged %d of 2 overdue captures", total)
	}
}
