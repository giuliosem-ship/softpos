package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

func testPayout(clave, merchant, key string) PayoutRecord {
	return PayoutRecord{ClaveRastreo: clave, MerchantID: merchant, IdempotencyKey: key, RequestHash: "hash-" + key,
		AmountCents: 1500, DestinationCLABE: "032180000118359719", FechaOperacion: "20261120"}
}

// TestPGIntegration_PayoutRecordsIdempotentAndStateMachine: the
// (merchant, Idempotency-Key) pair is unique, concurrent duplicates resolve to
// the first row, status changes are compare-and-set and recovery lists only
// stale RESERVED / PENDING_RECONCILIATION payouts.
func TestPGIntegration_PayoutRecordsIdempotentAndStateMachine(t *testing.T) {
	db := openMigratedPG(t)
	ms := NewSQLMoneyStore(db)
	ctx := context.Background()

	// 10 concurrent creates with the same key: exactly one row, all callers see it.
	var mu sync.Mutex
	created, claves := 0, map[string]bool{}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			err := ms.InTx(ctx, func(tx MoneyTx) error {
				p, ok, err := tx.CreatePayout(ctx, testPayout(fmt.Sprintf("KRNPG%02d", i), "m1", "same"))
				if err != nil {
					return err
				}
				mu.Lock()
				defer mu.Unlock()
				if ok {
					created++
				}
				claves[p.ClaveRastreo] = true
				return nil
			})
			if err != nil {
				t.Errorf("create %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()
	if created != 1 || len(claves) != 1 || countPayouts(t, db) != 1 {
		t.Fatalf("created=%d distinct=%d rows=%d", created, len(claves), countPayouts(t, db))
	}
	// Same key for another merchant is independent.
	if err := ms.InTx(ctx, func(tx MoneyTx) error {
		_, ok, err := tx.CreatePayout(ctx, testPayout("KRNPGOTHER", "m2", "same"))
		if err == nil && !ok {
			err = errors.New("m2 must get its own payout")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}

	var clave string
	for c := range claves {
		clave = c
	}
	// Compare-and-set transitions.
	if err := ms.InTx(ctx, func(tx MoneyTx) error {
		if err := tx.SetPayoutRemainingBalance(ctx, clave, 777); err != nil {
			return err
		}
		return tx.SetPayoutStatus(ctx, clave, PayoutReserved, PayoutPendingReconciliation, 0, "timeout")
	}); err != nil {
		t.Fatal(err)
	}
	err := ms.InTx(ctx, func(tx MoneyTx) error {
		return tx.SetPayoutStatus(ctx, clave, PayoutReserved, PayoutDispatched, 1, "")
	})
	if !errors.Is(err, ErrPayoutStateConflict) {
		t.Fatalf("stale transition: want ErrPayoutStateConflict, got %v", err)
	}
	var got PayoutRecord
	_ = ms.InTx(ctx, func(tx MoneyTx) error {
		got, _, err = tx.GetPayoutForUpdate(ctx, clave)
		return err
	})
	if got.Status != PayoutPendingReconciliation || got.LastError != "timeout" || got.RemainingBalanceCents != 777 || got.RequestHash != "hash-same" {
		t.Fatalf("payout %+v", got)
	}

	// Recovery listing: only stale recoverable payouts.
	repo := NewSQLPayoutRepository(db)
	if list, err := repo.ListPayoutsForRecovery(ctx, time.Now().Add(-time.Minute), 10); err != nil || len(list) != 0 {
		t.Fatalf("fresh payouts must not be listed: %d %v", len(list), err)
	}
	_, _ = db.Exec(`UPDATE payouts SET updated_at = NOW() - INTERVAL '1 hour'`)
	_ = ms.InTx(ctx, func(tx MoneyTx) error {
		return tx.SetPayoutStatus(ctx, "KRNPGOTHER", PayoutReserved, PayoutDispatched, 9, "")
	})
	list, err := repo.ListPayoutsForRecovery(ctx, time.Now().Add(-time.Minute), 10)
	if err != nil || len(list) != 1 || list[0].ClaveRastreo != clave {
		t.Fatalf("recovery list: %+v %v", list, err)
	}
}

func countPayouts(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM payouts`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}
