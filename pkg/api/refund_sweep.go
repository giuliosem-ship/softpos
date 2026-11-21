package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"softpos/pkg/acquirer"
	"softpos/pkg/store"
)

// Refund request sweep (MVP-29). A refund stays REQUESTED (holding its amount
// against the refundable remainder) when the provider call's outcome is
// unknown (timeout, crash between the commit and the call). The sweep
// re-queries the provider through acquirer.Provider.GetRefund and resolves
// it; see docs/ACQUIRER_INTEGRATION.md "Barrido de devoluciones".

// RefundSweepConfig tunes one sweep pass.
type RefundSweepConfig struct {
	// After is how long a refund must stay untouched in REQUESTED before it
	// is re-queried (default 15m; also the spacing between attempts).
	After time.Duration
	// AlertAttempts: after this many inconclusive lookups an ALERT is logged
	// on every further attempt (default 5).
	AlertAttempts int
	BatchSize     int // default 50
}

// Refund sweep defaults.
const (
	DefaultRefundSweepAfter         = 15 * time.Minute
	MinRefundSweepAfter             = 2 * time.Minute // well beyond the 30 s provider call of the handler
	DefaultRefundSweepAlertAttempts = 5
)

// RefundSweepResult counts the outcomes of one pass.
type RefundSweepResult struct {
	Claimed   int
	Succeeded int // applied through the refund webhook path (ledger reversed once)
	Pending   int // the provider has it: PENDING, the webhook finishes it
	Failed    int // the provider rejected it: FAILED, hold released
	Expired   int // the provider never received it: FAILED (expired), hold released
	Resolved  int // resolved concurrently by the webhook / a merchant retry: nothing done
	Unknown   int // provider unavailable / not conclusive: retried later
	Alerted   int // unknown after AlertAttempts attempts
}

// Outcome labels (metrics, logs).
const (
	RefundSweepSucceeded = "succeeded"
	RefundSweepPending   = "pending"
	RefundSweepFailed    = "failed"
	RefundSweepExpired   = "expired"
	RefundSweepUnknown   = "unknown"
	RefundSweepResolved  = "already_resolved"
)

// SweepStaleRefunds runs one refund sweep pass at now.
func (s *Server) SweepStaleRefunds(ctx context.Context, now time.Time, cfg RefundSweepConfig) (RefundSweepResult, error) {
	var res RefundSweepResult
	prov := s.AcquirerProvider()
	if prov == nil {
		return res, nil
	}
	if cfg.After <= 0 {
		cfg.After = DefaultRefundSweepAfter
	}
	if cfg.AlertAttempts <= 0 {
		cfg.AlertAttempts = DefaultRefundSweepAlertAttempts
	}
	var claimed []store.StaleCardRefund
	err := s.moneyStore().InTx(ctx, func(tx store.MoneyTx) error {
		var err error
		claimed, err = tx.ClaimStaleCardRefunds(ctx, now.Add(-cfg.After), now, cfg.BatchSize)
		return err
	})
	if err != nil {
		return res, fmt.Errorf("claim stale refunds: %w", err)
	}
	res.Claimed = len(claimed)
	for _, rf := range claimed {
		outcome, err := s.resolveStaleRefund(ctx, prov, rf)
		if err != nil {
			slog.Error("refund sweep: resolution failed", "refund_id", rf.ID, "error", err)
			outcome = RefundSweepUnknown
		}
		switch outcome {
		case RefundSweepSucceeded:
			res.Succeeded++
		case RefundSweepPending:
			res.Pending++
		case RefundSweepFailed:
			res.Failed++
		case RefundSweepExpired:
			res.Expired++
		case RefundSweepResolved:
			res.Resolved++
		default:
			res.Unknown++
			if rf.SweepAttempts >= cfg.AlertAttempts {
				res.Alerted++
				slog.Error("ALERT refund request unresolved", "refund_id", rf.ID, "card_payment_id", rf.CardPaymentID,
					"attempts", rf.SweepAttempts, "amount_cents", rf.AmountCents)
			}
		}
	}
	return res, nil
}

func (s *Server) resolveStaleRefund(ctx context.Context, prov acquirer.Provider, rf store.StaleCardRefund) (string, error) {
	if rf.Provider != prov.Name() || rf.ProviderPaymentID == "" {
		return RefundSweepUnknown, nil
	}
	pctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	st, err := prov.GetRefund(pctx, acquirer.RefundLookup{ProviderPaymentID: rf.ProviderPaymentID,
		IdempotencyKey: "kerno_refund_" + rf.ID, KernoRefundID: rf.ID})
	switch {
	case errors.Is(err, acquirer.ErrRefundNotFound):
		return s.finishStaleRefund(ctx, rf, store.RefundFailed, "", "EXPIRED: refund request never reached the provider", "card_payment.refund_expired", RefundSweepExpired)
	case err != nil:
		slog.Warn("refund sweep: provider lookup not conclusive", "refund_id", rf.ID, "attempts", rf.SweepAttempts, "error", err)
		return RefundSweepUnknown, nil
	}
	switch st.Status {
	case acquirer.RefundStatusSucceeded:
		if st.ProviderRefundID == "" {
			return RefundSweepUnknown, nil
		}
		amount := st.AmountCents
		if amount <= 0 {
			amount = rf.AmountCents
		}
		// Same path as the webhook: the refund is applied exactly once per
		// provider refund id (claimSub), so the real webhook arriving later
		// (or concurrently) does not post it again.
		return s.applySweepEvent(ctx, acquirer.Event{ID: "sweep_" + rf.ID + "_succeeded", Type: acquirer.EventRefunded,
			ProviderPaymentID: rf.ProviderPaymentID, KernoPaymentID: rf.CardPaymentID, RefundID: st.ProviderRefundID,
			KernoRefundID: rf.ID, AmountCents: amount, OccurredAt: time.Now().UTC()}, rf.Provider, RefundSweepSucceeded)
	case acquirer.RefundStatusFailed:
		if st.ProviderRefundID != "" {
			return s.applySweepEvent(ctx, acquirer.Event{ID: "sweep_" + rf.ID + "_failed", Type: acquirer.EventRefundFailed,
				ProviderPaymentID: rf.ProviderPaymentID, KernoPaymentID: rf.CardPaymentID, RefundID: st.ProviderRefundID,
				KernoRefundID: rf.ID, OccurredAt: time.Now().UTC()}, rf.Provider, RefundSweepFailed)
		}
		return s.finishStaleRefund(ctx, rf, store.RefundFailed, "", "provider reported the refund failed", "card_payment.refund_failed", RefundSweepFailed)
	case acquirer.RefundStatusPending:
		return s.finishStaleRefund(ctx, rf, store.RefundPending, st.ProviderRefundID, "", "", RefundSweepPending)
	}
	return RefundSweepUnknown, nil
}

// applySweepEvent applies a provider-confirmed outcome through the webhook
// path; "ignored" (already applied by the real webhook) is already_resolved.
func (s *Server) applySweepEvent(ctx context.Context, e acquirer.Event, provider, outcome string) (string, error) {
	out, err := s.ApplyAcquirerEvent(ctx, provider, e)
	if err != nil {
		return RefundSweepUnknown, err
	}
	var res AcquirerEventResult
	if json.Unmarshal(out, &res) == nil && res.Result != "applied" {
		return RefundSweepResolved, nil
	}
	return outcome, nil
}

// finishStaleRefund moves a still-REQUESTED refund to status under the same
// lock order as the webhook (card payment, then refund). A refund the
// webhook already resolved is left alone.
func (s *Server) finishStaleRefund(ctx context.Context, rf store.StaleCardRefund, status store.CardRefundStatus, providerRefundID, lastError, eventType, outcome string) (string, error) {
	changed := false
	err := s.moneyStore().InTx(ctx, func(tx store.MoneyTx) error {
		changed = false
		cp, found, err := tx.GetCardPaymentForUpdate(ctx, rf.CardPaymentID)
		if err != nil {
			return err
		}
		if !found {
			return errCardPaymentNotFound
		}
		cur, found, err := tx.GetCardRefundForUpdate(ctx, rf.ID)
		if err != nil {
			return err
		}
		if !found || cur.Status != store.RefundRequested {
			return nil
		}
		cur.Status, cur.LastError = status, lastError
		if providerRefundID != "" {
			cur.ProviderRefundID = providerRefundID
		}
		if err := tx.UpdateCardRefund(ctx, cur); err != nil {
			return err
		}
		changed = true
		if eventType == "" {
			return nil
		}
		return tx.EnqueueEvent(ctx, eventType, cp.ID, map[string]any{
			"card_payment_id": cp.ID, "merchant_id": cp.MerchantID, "refund_id": cur.ID, "amount_cents": cur.AmountCents, "reason": lastError,
		})
	})
	if err != nil {
		return RefundSweepUnknown, err
	}
	if !changed {
		return RefundSweepResolved, nil // resolved concurrently (webhook / merchant retry)
	}
	if status == store.RefundFailed {
		slog.Warn("refund sweep released a refund hold", "refund_id", rf.ID, "outcome", outcome, "amount_cents", rf.AmountCents)
	}
	return outcome, nil
}
