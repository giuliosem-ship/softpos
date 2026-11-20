package main

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"time"
	// Settlement calendar days are computed in America/Mexico_City.
	_ "time/tzdata"

	"softpos/pkg/payouts"
	"softpos/pkg/store"
)

// main runs one processor-settlement reconciliation pass (cron / CronJob):
// captured card payments the processor has not paid out by their expected
// settlement date (+1 day) become NOT_SETTLED reconciliation items with an
// ERROR alert. The same check runs continuously inside cmd/server
// (ACQUIRER_RECONCILE_INTERVAL); payout lines are reconciled when the
// processor's payout.settled webhook arrives. Needs DATABASE_URL (SQL_DRIVER
// optional, default pgx).
func main() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		slog.Error("DATABASE_URL is required")
		os.Exit(2)
	}
	db, err := sql.Open(store.NormalizeDriverName(os.Getenv("SQL_DRIVER")), dsn)
	if err != nil {
		slog.Error("open database", "error", err)
		os.Exit(1)
	}
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		slog.Error("connect database", "error", err)
		os.Exit(1)
	}

	w := payouts.NewAcquirerSettlementWatcher(store.NewSQLMoneyStore(db), payouts.AcquirerSettlementConfig{})
	n, err := w.RunOnce(ctx)
	_ = db.Close()
	if err != nil {
		slog.Error("acquirer settlement reconciliation failed", "error", err)
		os.Exit(1)
	}
	slog.Info("acquirer settlement reconciliation done", "new_discrepancies", n)
}
