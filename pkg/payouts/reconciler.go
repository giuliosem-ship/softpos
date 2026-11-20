package payouts

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"softpos/pkg/store"
	"softpos/pkg/stp"
)

// ReconcilerConfig tunes the payout recovery loop. Zero values use defaults.
type ReconcilerConfig struct {
	// Interval between passes (default 1m).
	Interval time.Duration
	// After is how long a payout must sit in RESERVED / PENDING_RECONCILIATION
	// before recovery touches it (default 10m). It must exceed the handler's
	// registraOrden timeout so an in-flight request is never raced.
	After time.Duration
	// BatchSize caps payouts per pass (default 50).
	BatchSize int
	// QueryTimeout bounds each STP status query (default 15s).
	QueryTimeout time.Duration
	Logger       *slog.Logger
	Now          func() time.Time
}

const (
	DefaultReconcileInterval = time.Minute
	DefaultReconcileAfter    = 10 * time.Minute
	DefaultReconcileBatch    = 50
	DefaultQueryTimeout      = 15 * time.Second
)

func (c ReconcilerConfig) withDefaults() ReconcilerConfig {
	if c.Interval <= 0 {
		c.Interval = DefaultReconcileInterval
	}
	if c.After <= 0 {
		c.After = DefaultReconcileAfter
	}
	if c.BatchSize <= 0 {
		c.BatchSize = DefaultReconcileBatch
	}
	if c.QueryTimeout <= 0 {
		c.QueryTimeout = DefaultQueryTimeout
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	return c
}

// Reconciler resolves stale payouts against STP: confirmed orders are
// finalized (SETTLED / DISPATCHED), definitively rejected or never-registered
// orders get a compensating journal (FAILED), and anything still unknown is
// kept reserved and logged at ERROR level (alert).
type Reconciler struct {
	repo  store.PayoutRepository
	money store.MoneyStore
	stp   stp.OrderStatusClient
	cfg   ReconcilerConfig
}

func NewReconciler(repo store.PayoutRepository, money store.MoneyStore, client stp.OrderStatusClient, cfg ReconcilerConfig) *Reconciler {
	return &Reconciler{repo: repo, money: money, stp: client, cfg: cfg.withDefaults()}
}

// Result summarizes one recovery pass.
type Result struct {
	Settled     int
	Dispatched  int
	Compensated int
	Unknown     int // kept reserved, alert logged
	Skipped     int // already finalized by someone else
}

// Outcome of reconciling one payout.
type Outcome string

const (
	OutcomeSettled     Outcome = "SETTLED"     // STP liquidated it
	OutcomeDispatched  Outcome = "DISPATCHED"  // STP accepted it
	OutcomeCompensated Outcome = "COMPENSATED" // rejected / never registered: funds returned (FAILED)
	OutcomeUnknown     Outcome = "UNKNOWN"     // still unknown: funds stay reserved, alert logged
	OutcomeSkipped     Outcome = "SKIPPED"     // already finalized by someone else
)

// RunOnce processes one batch of stale payouts.
func (r *Reconciler) RunOnce(ctx context.Context) (Result, error) {
	var res Result
	list, err := r.repo.ListPayoutsForRecovery(ctx, r.cfg.Now().Add(-r.cfg.After), r.cfg.BatchSize)
	if err != nil {
		return res, err
	}
	for _, p := range list {
		if ctx.Err() != nil {
			return res, ctx.Err()
		}
		switch r.ReconcilePayout(ctx, p) {
		case OutcomeSettled:
			res.Settled++
		case OutcomeDispatched:
			res.Dispatched++
		case OutcomeCompensated:
			res.Compensated++
		case OutcomeSkipped:
			res.Skipped++
		default:
			res.Unknown++
		}
	}
	return res, nil
}

// ReconcilePayout resolves one RESERVED / PENDING_RECONCILIATION payout
// against the STP order status (also used by the backoffice "retry"). Every
// state change is compare-and-set on the locked payout row, so it is safe to
// run concurrently with the background loop.
func (r *Reconciler) ReconcilePayout(ctx context.Context, p store.PayoutRecord) Outcome {
	qctx, cancel := context.WithTimeout(ctx, r.cfg.QueryTimeout)
	st, qerr := r.stp.QueryOrderStatus(qctx, p.ClaveRastreo, p.FechaOperacion)
	cancel()
	if qerr != nil {
		r.alert(ctx, p, "STP status query failed", qerr)
		return OutcomeUnknown
	}
	var outcome string
	var ferr error
	var done Outcome
	switch st.State {
	case stp.OrderLiquidated:
		outcome, ferr = r.finalize(ctx, p, store.PayoutSettled, st.STPOrderID, "disbursement.settled")
		done = OutcomeSettled
	case stp.OrderAccepted:
		outcome, ferr = r.finalize(ctx, p, store.PayoutDispatched, st.STPOrderID, "disbursement.dispatched")
		done = OutcomeDispatched
	case stp.OrderRejected, stp.OrderNotFound:
		outcome, ferr = r.compensate(ctx, p, st)
		done = OutcomeCompensated
	default:
		r.alert(ctx, p, "STP order state still unknown", fmt.Errorf("state %q: %s", st.State, st.Detail))
		return OutcomeUnknown
	}
	if ferr != nil {
		r.alert(ctx, p, "payout recovery transaction failed", ferr)
		return OutcomeUnknown
	}
	if outcome == "skipped" {
		return OutcomeSkipped
	}
	return done
}

func (r *Reconciler) alert(ctx context.Context, p store.PayoutRecord, msg string, err error) {
	r.cfg.Logger.Log(ctx, slog.LevelError, "ALERT payout unresolved: "+msg+"; funds stay reserved",
		"clave_rastreo", p.ClaveRastreo, "merchant_id", p.MerchantID, "amount_cents", p.AmountCents,
		"status", p.Status, "age", r.cfg.Now().Sub(p.CreatedAt).Round(time.Second), "error", err)
}

func recoverable(s store.PayoutStatus) bool {
	return s == store.PayoutReserved || s == store.PayoutPendingReconciliation
}

// finalize records an order STP confirmed (no money movement: the
// reservation already debited the merchant).
func (r *Reconciler) finalize(ctx context.Context, p store.PayoutRecord, to store.PayoutStatus, orderID int64, event string) (string, error) {
	outcome := "skipped"
	err := r.money.InTx(ctx, func(tx store.MoneyTx) error {
		cur, found, err := tx.GetPayoutForUpdate(ctx, p.ClaveRastreo)
		if err != nil || !found || !recoverable(cur.Status) {
			return err
		}
		if err := tx.SetPayoutStatus(ctx, cur.ClaveRastreo, cur.Status, to, orderID, ""); err != nil {
			return err
		}
		outcome = "done"
		return tx.EnqueueEvent(ctx, event, cur.ClaveRastreo, map[string]any{
			"clave_rastreo": cur.ClaveRastreo, "merchant_id": cur.MerchantID, "amount_cents": cur.AmountCents,
			"stp_order_id": orderID, "recovered_from": string(cur.Status),
		})
	})
	if err != nil {
		return "", err
	}
	if outcome == "done" {
		r.cfg.Logger.Info("payout recovered", "clave_rastreo", p.ClaveRastreo, "from", p.Status, "to", to)
	}
	return outcome, nil
}

// compensate returns the reserved funds of an order STP rejected or never
// registered, in one transaction with the status change.
func (r *Reconciler) compensate(ctx context.Context, p store.PayoutRecord, st stp.OrderStatus) (string, error) {
	reason := "STP order " + string(st.State)
	if st.Detail != "" {
		reason += ": " + st.Detail
	}
	outcome := "skipped"
	err := r.money.InTx(ctx, func(tx store.MoneyTx) error {
		cur, found, err := tx.GetPayoutForUpdate(ctx, p.ClaveRastreo)
		if err != nil || !found || !recoverable(cur.Status) {
			return err
		}
		jnl, err := CompensationJournal(cur, r.cfg.Now().UTC(), "recovery compensation")
		if err != nil {
			return err
		}
		if err := tx.PostJournal(ctx, jnl); err != nil {
			if errors.Is(err, store.ErrDuplicateJournal) {
				return fmt.Errorf("compensation journal already posted for %s: %w", cur.ClaveRastreo, err)
			}
			return err
		}
		if err := tx.SetPayoutStatus(ctx, cur.ClaveRastreo, cur.Status, store.PayoutFailed, 0, reason); err != nil {
			return err
		}
		outcome = "done"
		return tx.EnqueueEvent(ctx, "disbursement.failed", cur.ClaveRastreo, map[string]any{
			"clave_rastreo": cur.ClaveRastreo, "merchant_id": cur.MerchantID, "amount_cents": cur.AmountCents,
			"error": reason, "recovered_from": string(cur.Status),
		})
	})
	if err != nil {
		return "", err
	}
	if outcome == "done" {
		r.cfg.Logger.Warn("payout compensated by recovery", "clave_rastreo", p.ClaveRastreo, "from", p.Status, "reason", reason)
	}
	return outcome, nil
}

// Run executes passes every Interval until ctx is cancelled.
func (r *Reconciler) Run(ctx context.Context) error {
	r.cfg.Logger.Info("payout reconciler started", "interval", r.cfg.Interval, "after", r.cfg.After)
	defer r.cfg.Logger.Info("payout reconciler stopped")
	t := time.NewTimer(0)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		res, err := r.RunOnce(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			r.cfg.Logger.Error("payout reconciler pass failed", "error", err)
		} else if res != (Result{}) {
			r.cfg.Logger.Info("payout reconciler pass", "settled", res.Settled, "dispatched", res.Dispatched,
				"compensated", res.Compensated, "unknown", res.Unknown, "skipped", res.Skipped)
		}
		t.Reset(r.cfg.Interval)
	}
}
