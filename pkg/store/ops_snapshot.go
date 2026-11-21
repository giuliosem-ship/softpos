package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// OpsSnapshot is the operational state sampled from PostgreSQL for the
// metrics gauges (MVP-30, docs/OBSERVABILITY.md). Counts are global (no
// per-merchant breakdown); ages are in seconds, 0 when there is nothing.
type OpsSnapshot struct {
	OutboxByStatus          map[string]int64 // PENDING, PROCESSING, FAILED
	OutboxOldestPendingAge  float64          // oldest PENDING/PROCESSING event
	PayoutsUnresolved       map[string]int64 // RESERVED, PENDING_RECONCILIATION
	PayoutsOldestUnresolved float64
	ReconciliationOpen      map[string]int64 // OPEN items by kind
	RefundsRequested        int64
	RefundsOldestRequested  float64
	SAT69BLoaded            bool
	SAT69BRows              int64
	SAT69BVerifiedAge       float64
}

// Enumerations the gauges always export (a value that disappears is 0).
var (
	OpsOutboxStatuses = []string{"PENDING", "PROCESSING", "FAILED"}
	OpsPayoutStatuses = []string{string(PayoutReserved), string(PayoutPendingReconciliation)}
	OpsReconKinds     = []string{ReconMissingPayment, ReconAmountMismatch, ReconFeeMismatch, ReconDuplicateSettlement, ReconPayoutTotalMismatch, ReconNotSettled}
)

// SampleOps reads the operational snapshot (a handful of indexed
// aggregate queries).
func SampleOps(ctx context.Context, db *sql.DB, now time.Time) (OpsSnapshot, error) {
	snap := OpsSnapshot{OutboxByStatus: map[string]int64{}, PayoutsUnresolved: map[string]int64{}, ReconciliationOpen: map[string]int64{}}
	group := func(query string, into map[string]int64) error {
		rows, err := db.QueryContext(ctx, query)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var k string
			var n int64
			if err := rows.Scan(&k, &n); err != nil {
				return err
			}
			into[k] = n
		}
		return rows.Err()
	}
	age := func(query string) (float64, error) {
		var t sql.NullTime
		if err := db.QueryRowContext(ctx, query).Scan(&t); err != nil {
			return 0, err
		}
		if !t.Valid {
			return 0, nil
		}
		return max(now.Sub(t.Time).Seconds(), 0), nil
	}
	var err error
	if err = group(`SELECT status, COUNT(*) FROM outbox_events WHERE status IN ('PENDING', 'PROCESSING', 'FAILED') GROUP BY status`, snap.OutboxByStatus); err != nil {
		return snap, fmt.Errorf("sample outbox: %w", err)
	}
	if snap.OutboxOldestPendingAge, err = age(`SELECT MIN(created_at) FROM outbox_events WHERE status IN ('PENDING', 'PROCESSING')`); err != nil {
		return snap, fmt.Errorf("sample outbox age: %w", err)
	}
	if err = group(`SELECT status, COUNT(*) FROM payouts WHERE status IN ('RESERVED', 'PENDING_RECONCILIATION') GROUP BY status`, snap.PayoutsUnresolved); err != nil {
		return snap, fmt.Errorf("sample payouts: %w", err)
	}
	if snap.PayoutsOldestUnresolved, err = age(`SELECT MIN(updated_at) FROM payouts WHERE status IN ('RESERVED', 'PENDING_RECONCILIATION')`); err != nil {
		return snap, fmt.Errorf("sample payout age: %w", err)
	}
	if err = group(`SELECT kind, COUNT(*) FROM acquirer_reconciliation_items WHERE status = 'OPEN' GROUP BY kind`, snap.ReconciliationOpen); err != nil {
		return snap, fmt.Errorf("sample reconciliation: %w", err)
	}
	if err = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM card_payment_refunds WHERE status = 'REQUESTED'`).Scan(&snap.RefundsRequested); err != nil {
		return snap, fmt.Errorf("sample refunds: %w", err)
	}
	if snap.RefundsOldestRequested, err = age(`SELECT MIN(created_at) FROM card_payment_refunds WHERE status = 'REQUESTED'`); err != nil {
		return snap, fmt.Errorf("sample refund age: %w", err)
	}
	var verified sql.NullTime
	err = db.QueryRowContext(ctx, `SELECT row_count, verified_at FROM sat_blacklist_meta WHERE id = 1`).Scan(&snap.SAT69BRows, &verified)
	switch {
	case err == sql.ErrNoRows:
	case err != nil:
		return snap, fmt.Errorf("sample 69-B meta: %w", err)
	case verified.Valid:
		snap.SAT69BLoaded, snap.SAT69BVerifiedAge = true, max(now.Sub(verified.Time).Seconds(), 0)
	}
	return snap, nil
}
