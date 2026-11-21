package store

import (
	"context"
	"testing"
	"time"
)

// TestPGIntegration_SampleOps (MVP-30): the metrics sample reads outbox,
// payout, reconciliation, refund and 69-B state with plain aggregates.
func TestPGIntegration_SampleOps(t *testing.T) {
	db := openMigratedPG(t)
	ctx := context.Background()
	snap, err := SampleOps(ctx, db, time.Now())
	if err != nil || len(snap.OutboxByStatus) != 0 || snap.SAT69BLoaded || snap.RefundsRequested != 0 {
		t.Fatalf("empty: %+v %v", snap, err)
	}
	for _, q := range []string{
		`INSERT INTO outbox_events (event_type, aggregate_id, payload_json, status, retry_count, max_retries, last_error, created_at, updated_at)
			VALUES ('a', 'x', '{}', 'PENDING', 0, 5, '', NOW() - INTERVAL '10 minutes', NOW()),
			       ('b', 'x', '{}', 'FAILED', 5, 5, 'boom', NOW(), NOW()),
			       ('c', 'x', '{}', 'PUBLISHED', 0, 5, '', NOW() - INTERVAL '1 day', NOW())`,
		`INSERT INTO acquirer_reconciliation_items (dedupe_key, provider, kind) VALUES ('k1', 'mock', 'NOT_SETTLED'), ('k2', 'mock', 'FEE_MISMATCH')`,
		`UPDATE acquirer_reconciliation_items SET status = 'RESOLVED' WHERE dedupe_key = 'k2'`,
	} {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	snap, err = SampleOps(ctx, db, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if snap.OutboxByStatus["PENDING"] != 1 || snap.OutboxByStatus["FAILED"] != 1 || snap.OutboxByStatus["PUBLISHED"] != 0 ||
		snap.OutboxOldestPendingAge < 590 || snap.ReconciliationOpen[ReconNotSettled] != 1 || snap.ReconciliationOpen[ReconFeeMismatch] != 0 {
		t.Fatalf("sample: %+v", snap)
	}
}
