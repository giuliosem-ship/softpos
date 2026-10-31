package store

import (
	"context"
	"testing"
)

func TestSQLOutboxRepository_SaveAndFetchPending(t *testing.T) {
	db, cleanup := setupTestDB(t, "test_outbox_save_fetch")
	defer cleanup()

	repo := NewSQLOutboxRepository(db)
	ctx := context.Background()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx failed: %v", err)
	}

	payload := map[string]any{
		"payment_id":  "emv_tx_5001",
		"merchant_id": "merch_01",
		"gross_cents": 150000,
	}

	if err := repo.SaveEventTx(ctx, tx, "cfdi.stamp.requested", "emv_tx_5001", payload); err != nil {
		_ = tx.Rollback()
		t.Fatalf("SaveEventTx failed: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("Commit failed: %v", err)
	}

	// Fetch pending events
	events, err := repo.FetchPendingEventsWithLock(ctx, 10)
	if err != nil {
		t.Fatalf("FetchPendingEventsWithLock failed: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 pending event, got: %d", len(events))
	}

	event := events[0]
	if event.EventType != "cfdi.stamp.requested" || event.AggregateID != "emv_tx_5001" || event.Status != "PENDING" {
		t.Errorf("unexpected event: %+v", event)
	}
}

func TestSQLOutboxRepository_MarkPublishedAndFailed(t *testing.T) {
	db, cleanup := setupTestDB(t, "test_outbox_publish_fail")
	defer cleanup()

	repo := NewSQLOutboxRepository(db)
	ctx := context.Background()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx failed: %v", err)
	}

	if err := repo.SaveEventTx(ctx, tx, "webhook.merchant.dispatched", "wh_901", map[string]string{"event": "PAYMENT_CONFIRMED"}); err != nil {
		_ = tx.Rollback()
		t.Fatalf("SaveEventTx failed: %v", err)
	}
	_ = tx.Commit()

	events, _ := repo.FetchPendingEventsWithLock(ctx, 10)
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	eventID := events[0].ID

	// 1. Mark failed 4 times (retry_count should increment and remain PENDING since max_retries = 5)
	for i := 1; i <= 4; i++ {
		if err := repo.MarkFailed(ctx, eventID, "temporary network timeout"); err != nil {
			t.Fatalf("MarkFailed failed on attempt %d: %v", i, err)
		}
	}

	pending, _ := repo.FetchPendingEventsWithLock(ctx, 10)
	if len(pending) != 1 || pending[0].RetryCount != 4 || pending[0].Status != "PENDING" {
		t.Errorf("expected event to still be pending with retry_count=4: %+v", pending)
	}

	// 2. Mark failed 5th time -> should reach max_retries (5) and transition to FAILED
	if err := repo.MarkFailed(ctx, eventID, "permanent PAC error"); err != nil {
		t.Fatalf("5th MarkFailed failed: %v", err)
	}

	// Now it should NOT appear in pending
	pendingAfterMax, _ := repo.FetchPendingEventsWithLock(ctx, 10)
	if len(pendingAfterMax) != 0 {
		t.Errorf("expected 0 pending events after max retries, got: %d", len(pendingAfterMax))
	}

	// 3. Mark another event as published
	tx2, _ := db.BeginTx(ctx, nil)
	_ = repo.SaveEventTx(ctx, tx2, "cfdi.stamp.requested", "emv_tx_5002", map[string]string{"status": "ok"})
	_ = tx2.Commit()

	events2, _ := repo.FetchPendingEventsWithLock(ctx, 10)
	if len(events2) != 1 {
		t.Fatalf("expected 1 event, got: %d", len(events2))
	}

	if err := repo.MarkPublished(ctx, events2[0].ID); err != nil {
		t.Fatalf("MarkPublished failed: %v", err)
	}

	pendingFinal, _ := repo.FetchPendingEventsWithLock(ctx, 10)
	if len(pendingFinal) != 0 {
		t.Errorf("expected 0 pending events after publish, got: %d", len(pendingFinal))
	}
}
