package payouts

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"softpos/pkg/fiscal"
	"softpos/pkg/store"
)

// AcquirerSettlementConfig tunes the processor settlement watcher.
type AcquirerSettlementConfig struct {
	Interval  time.Duration // ACQUIRER_RECONCILE_INTERVAL, default 1h
	GraceDays int           // days after the expected settlement date before flagging, default 1
	BatchSize int           // default 500
	Now       func() time.Time
	Logger    *slog.Logger
}

func (c AcquirerSettlementConfig) withDefaults() AcquirerSettlementConfig {
	if c.Interval <= 0 {
		c.Interval = time.Hour
	}
	if c.GraceDays <= 0 {
		c.GraceDays = 1
	}
	if c.BatchSize <= 0 {
		c.BatchSize = 500
	}
	if c.Now == nil {
		c.Now = func() time.Time { return time.Now().UTC() }
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return c
}

// AcquirerSettlementWatcher flags captured card payments that the processor
// has not settled to Kerno by their expected date (+ grace) as NOT_SETTLED
// reconciliation items, with an ERROR alert. Payout lines themselves are
// reconciled when the payout.settled webhook arrives (pkg/api). Each payment
// is flagged once (dedupe key notsettled:<card_payment_id>).
type AcquirerSettlementWatcher struct {
	repo store.AcquirerRepository
	cfg  AcquirerSettlementConfig
	loc  *time.Location
}

func NewAcquirerSettlementWatcher(repo store.AcquirerRepository, cfg AcquirerSettlementConfig) *AcquirerSettlementWatcher {
	loc, err := time.LoadLocation(fiscal.MexicoCityZone)
	if err != nil {
		loc = time.UTC
	}
	return &AcquirerSettlementWatcher{repo: repo, cfg: cfg.withDefaults(), loc: loc}
}

// RunOnce flags overdue settlements and returns how many new items were created.
func (w *AcquirerSettlementWatcher) RunOnce(ctx context.Context) (int, error) {
	// Settlement dates are calendar days in Mexico City.
	cutoff := w.cfg.Now().In(w.loc).AddDate(0, 0, -w.cfg.GraceDays).Format("2006-01-02")
	list, err := w.repo.ListUnsettledCaptures(ctx, cutoff, w.cfg.BatchSize)
	if err != nil {
		return 0, err
	}
	created := 0
	for _, cp := range list {
		ok, err := w.repo.InsertReconciliationItem(ctx, store.ReconciliationItem{
			DedupeKey:         "notsettled:" + cp.ID,
			Provider:          cp.Provider,
			CardPaymentID:     cp.ID,
			ProviderPaymentID: cp.ProviderPaymentID,
			Kind:              store.ReconNotSettled,
			ExpectedCents:     cp.CapturedCents - cp.ProviderFeeCents,
			Detail:            fmt.Sprintf("capture expected in a processor payout on %s, none received", cp.ProviderSettlementDate),
		})
		if err != nil {
			return created, err
		}
		if ok {
			created++
			w.cfg.Logger.Error("ALERT acquirer reconciliation discrepancy", "kind", store.ReconNotSettled, "provider", cp.Provider,
				"card_payment_id", cp.ID, "merchant_id", cp.MerchantID, "expected_settlement_date", cp.ProviderSettlementDate,
				"expected_cents", cp.CapturedCents-cp.ProviderFeeCents)
		}
	}
	return created, nil
}

// Run executes RunOnce every Interval until ctx is cancelled.
func (w *AcquirerSettlementWatcher) Run(ctx context.Context) error {
	w.cfg.Logger.Info("acquirer settlement watcher started", "interval", w.cfg.Interval, "grace_days", w.cfg.GraceDays)
	defer w.cfg.Logger.Info("acquirer settlement watcher stopped")
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		if _, err := w.RunOnce(ctx); err != nil && !errors.Is(err, context.Canceled) {
			w.cfg.Logger.Error("acquirer settlement watcher pass failed", "error", err)
		}
		t.Reset(w.cfg.Interval)
	}
}
