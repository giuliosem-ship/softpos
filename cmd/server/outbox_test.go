package main

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"softpos/migrations"
	"softpos/pkg/store"
	"softpos/pkg/store/pgtest"
)

func TestLoadConfigFromEnv_OutboxSettings(t *testing.T) {
	t.Setenv("OUTBOX_POLL_INTERVAL", "250ms")
	t.Setenv("OUTBOX_BATCH_SIZE", "7")
	t.Setenv("OUTBOX_PUBLISHER", "noop")
	cfg := LoadConfigFromEnv()
	if cfg.OutboxPollInterval != 250*time.Millisecond || cfg.OutboxBatchSize != 7 || cfg.OutboxPublisher != "noop" {
		t.Fatalf("unexpected outbox config: %+v", cfg)
	}

	t.Setenv("OUTBOX_POLL_INTERVAL", "bogus")
	t.Setenv("OUTBOX_BATCH_SIZE", "-1")
	cfg = LoadConfigFromEnv()
	if cfg.OutboxPollInterval != 0 || cfg.OutboxBatchSize != 0 {
		t.Fatalf("invalid values must fall back to defaults (zero), got %+v", cfg)
	}
}

func TestLoadConfigFromEnv_PayoutRecoveryAndPublishTimeout(t *testing.T) {
	t.Setenv("OUTBOX_PUBLISH_TIMEOUT", "5s")
	t.Setenv("PAYOUT_RECONCILE_INTERVAL", "30s")
	t.Setenv("PAYOUT_RECONCILE_AFTER", "15m")
	cfg := LoadConfigFromEnv()
	if cfg.OutboxPublishTimeout != 5*time.Second || cfg.PayoutReconcileInterval != 30*time.Second || cfg.PayoutReconcileAfter != 15*time.Minute {
		t.Fatalf("unexpected config: %+v", cfg)
	}
}

func TestStartPayoutReconciler_NoDBIsNoop(t *testing.T) {
	StartPayoutReconciler(context.Background(), Config{}, nil, nil)() // must not block
}

func TestStartOutboxDispatcher_NoDBIsNoop(t *testing.T) {
	wait, err := StartOutboxDispatcher(context.Background(), Config{}, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	wait() // must not block
}

func TestStartOutboxDispatcher_RejectsUnknownPublisher(t *testing.T) {
	db, err := sql.Open("mockpostgres", "outbox_unknown_publisher")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := StartOutboxDispatcher(context.Background(), Config{OutboxPublisher: "kafka"}, db); err == nil {
		t.Fatalf("expected error for unknown publisher")
	}
}

// TestPGIntegration_OutboxDispatcherPublishesAndShutsDown runs the real
// dispatcher wiring against PostgreSQL: an enqueued event gets PUBLISHED and the
// dispatcher exits promptly when its context is cancelled.
func TestPGIntegration_OutboxDispatcherPublishesAndShutsDown(t *testing.T) {
	db := pgtest.Open(t)
	ctx := context.Background()
	if err := store.RunMigrationsFS(ctx, db, migrations.FS); err != nil {
		t.Fatalf("migrations: %v", err)
	}
	tx, _ := db.BeginTx(ctx, nil)
	if err := store.NewSQLOutboxRepository(db).SaveEventTx(ctx, tx, "payment.settled", "pay_1", map[string]int64{"amount_cents": 100}); err != nil {
		t.Fatalf("SaveEventTx: %v", err)
	}
	_ = tx.Commit()

	runCtx, cancel := context.WithCancel(ctx)
	wait, err := StartOutboxDispatcher(runCtx, Config{OutboxPollInterval: 10 * time.Millisecond, OutboxPublisher: "noop"}, db)
	if err != nil {
		t.Fatalf("StartOutboxDispatcher: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		var status string
		_ = db.QueryRow(`SELECT status FROM outbox_events WHERE aggregate_id = 'pay_1'`).Scan(&status)
		if status == "PUBLISHED" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("event not published in time (status=%q)", status)
		}
		time.Sleep(10 * time.Millisecond)
	}

	cancel()
	stopped := make(chan struct{})
	go func() { wait(); close(stopped) }()
	select {
	case <-stopped:
	case <-time.After(2 * time.Second):
		t.Fatalf("dispatcher did not stop after cancel")
	}
}
