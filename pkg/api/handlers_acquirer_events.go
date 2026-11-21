package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"softpos/pkg/acquirer"
	"softpos/pkg/auth"
	"softpos/pkg/ledger"
	"softpos/pkg/store"
)

// SetAcquirerWebhookSecret sets the HMAC secret of
// POST /api/v1/webhooks/acquirer/{provider} (ACQUIRER_WEBHOOK_SECRET). Without
// it (and without a provider-specific verifier) the webhook answers 503.
func (s *Server) SetAcquirerWebhookSecret(secret string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acquirerSecret = []byte(secret)
}

// acquirerWebhookPrefix is the public route prefix of processor webhooks.
const acquirerWebhookPrefix = "/api/v1/webhooks/acquirer/"

// HandleAcquirerWebhook: POST /api/v1/webhooks/acquirer/{provider}. Public
// route authenticated by the signature (Kerno scheme: HMAC-SHA256 over
// "<X-Kerno-Timestamp>.<raw body>" with ACQUIRER_WEBHOOK_SECRET, ±5 min
// window; or the adapter's own WebhookVerifier). Each event is applied
// exactly once by its provider event id; 200 tells the provider to stop
// retrying, 409 EVENT_OUT_OF_ORDER / 404 CARD_PAYMENT_NOT_FOUND ask it to
// retry later (nothing was consumed).
func (s *Server) HandleAcquirerWebhook(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("provider")
	prov := s.AcquirerProvider()
	if prov == nil || prov.Name() != name {
		writeError(w, http.StatusNotFound, "UNKNOWN_ACQUIRER")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY")
		return
	}
	if v, ok := prov.(acquirer.WebhookVerifier); ok {
		if err := v.VerifyWebhook(r.Header, body, time.Now()); err != nil {
			writeError(w, http.StatusUnauthorized, "INVALID_SIGNATURE")
			return
		}
	} else {
		s.mu.RLock()
		secret := s.acquirerSecret
		s.mu.RUnlock()
		if len(secret) == 0 {
			writeError(w, http.StatusServiceUnavailable, "WEBHOOK_NOT_CONFIGURED")
			return
		}
		_, err := VerifyWebhookSignature(secret, r.Header.Get(WebhookTimestampHeader), r.Header.Get(WebhookSignatureHeader), body, time.Now(), WebhookTolerance)
		if errors.Is(err, ErrWebhookStale) {
			writeError(w, http.StatusUnauthorized, "STALE_TIMESTAMP")
			return
		}
		if err != nil {
			writeError(w, http.StatusUnauthorized, "INVALID_SIGNATURE")
			return
		}
	}
	evt, err := prov.ParseWebhookEvent(body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_EVENT")
		return
	}
	out, err := s.ApplyAcquirerEvent(r.Context(), name, evt)
	if err != nil {
		writeAcquirerEventError(w, name, evt, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(out)
}

func writeAcquirerEventError(w http.ResponseWriter, provider string, e acquirer.Event, err error) {
	switch {
	case errors.Is(err, errEventOutOfOrder):
		slog.Warn("acquirer event out of order, provider will retry", "provider", provider, "event_id", e.ID, "type", e.Type, "error", err)
		writeError(w, http.StatusConflict, "EVENT_OUT_OF_ORDER")
	case errors.Is(err, errUnknownCardPayment):
		slog.Warn("acquirer event for unknown card payment", "provider", provider, "event_id", e.ID, "provider_payment_id", e.ProviderPaymentID)
		writeError(w, http.StatusNotFound, "CARD_PAYMENT_NOT_FOUND")
	case errors.Is(err, errEventConflict):
		slog.Error("ALERT acquirer event conflicts with recorded state", "provider", provider, "event_id", e.ID, "type", e.Type, "error", err)
		writeError(w, http.StatusUnprocessableEntity, "EVENT_CONFLICT")
	case errors.Is(err, errUnsupportedEvent):
		writeError(w, http.StatusUnprocessableEntity, "UNSUPPORTED_EVENT")
	case errors.Is(err, acquirer.ErrInvalidEvent):
		writeError(w, http.StatusBadRequest, "INVALID_EVENT")
	default:
		writeMoneyError(w, "acquirer_event", err)
	}
}

// applyCardMoneyEvent handles refunds and chargebacks of a locked card payment.
func (s *Server) applyCardMoneyEvent(ctx context.Context, tx store.MoneyTx, cp *store.CardPaymentRecord, e acquirer.Event, _ *cardEventEffects) (bool, error) {
	switch e.Type {
	case acquirer.EventRefunded:
		if !cp.Status.Captured() {
			return false, fmt.Errorf("%w: refund before capture of %s", errEventOutOfOrder, cp.ID)
		}
		fresh, err := claimSub(ctx, tx, "acq:"+cp.Provider+":refund:"+cp.ID+":"+e.RefundID)
		if err != nil || !fresh {
			return false, err
		}
		// Disputed funds were already taken back by the chargeback: refund +
		// chargeback never exceed the capture.
		if remaining := cp.CapturedCents - cp.RefundedCents - cp.ChargebackCents; e.AmountCents > remaining {
			return false, fmt.Errorf("%w: refund %d exceeds remaining %d of %s", errEventConflict, e.AmountCents, remaining, cp.ID)
		}
		rec, found, err := tx.GetSettlementForUpdate(ctx, cp.ID)
		if err != nil {
			return false, err
		}
		if !found {
			return false, errSettlementMissing
		}
		rec, sp, err := postRefundReversal(ctx, tx, cp.MerchantID, cp.ID, rec, e.AmountCents, "Card refund "+cp.ID+" "+e.RefundID)
		if err != nil {
			return false, err
		}
		cp.RefundedCents += e.AmountCents
		if cp.Status.Refundable() {
			cp.Status = store.CardPartiallyRefunded
			if cp.RefundedCents == cp.CapturedCents {
				cp.Status = store.CardRefunded
			}
		}
		if req, ok, err := refundRequestOf(ctx, tx, cp, e); err != nil {
			return false, err
		} else if ok && req.Status != store.RefundSucceeded {
			req.Status, req.ProviderRefundID = store.RefundSucceeded, e.RefundID
			if err := tx.UpdateCardRefund(ctx, req); err != nil {
				return false, err
			}
		}
		return true, tx.EnqueueEvent(ctx, "card_payment.refunded", cp.ID, map[string]any{
			"card_payment_id": cp.ID, "merchant_id": cp.MerchantID, "refund_id": e.RefundID,
			"refund_cents": sp.amount, "refunded_total_cents": rec.RefundedGrossCents,
			"reversed_tax_escrow_cents": sp.taxEscrow, "reversed_platform_fee_cents": sp.fee, "status": cp.Status,
		})

	case acquirer.EventRefundFailed:
		req, ok, err := refundRequestOf(ctx, tx, cp, e)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, fmt.Errorf("%w: refund.failed for unknown refund %s", errEventOutOfOrder, e.RefundID)
		}
		if req.Status != store.RefundRequested && req.Status != store.RefundPending {
			return false, nil
		}
		req.Status, req.LastError, req.ProviderRefundID = store.RefundFailed, "provider reported refund.failed", e.RefundID
		if err := tx.UpdateCardRefund(ctx, req); err != nil {
			return false, err
		}
		return true, tx.EnqueueEvent(ctx, "card_payment.refund_failed", cp.ID, map[string]any{
			"card_payment_id": cp.ID, "merchant_id": cp.MerchantID, "refund_id": req.ID, "amount_cents": req.AmountCents,
		})

	case acquirer.EventChargebackOpened:
		if !cp.Status.Captured() {
			return false, fmt.Errorf("%w: chargeback before capture of %s", errEventOutOfOrder, cp.ID)
		}
		fresh, err := claimSub(ctx, tx, "acq:"+cp.Provider+":chargeback:"+cp.ID+":"+e.ChargebackID+":opened")
		if err != nil || !fresh {
			return false, err
		}
		if e.AmountCents > cp.CapturedCents-cp.RefundedCents-cp.ChargebackCents {
			return false, fmt.Errorf("%w: chargeback %d exceeds disputable amount of %s", errEventConflict, e.AmountCents, cp.ID)
		}
		// The processor takes the disputed funds back from Kerno (netted from
		// a payout): the merchant's available balance bears it and may go
		// negative (which blocks payouts) if it was already withdrawn.
		avail := merchantAvailable(cp.MerchantID)
		if err := postTransfer(ctx, tx, "chargeback_"+cp.ID+"_"+e.ChargebackID, "Chargeback "+e.ChargebackID+" on "+cp.ID,
			avail, AccountAcquirerReceivable, e.AmountCents, cp); err != nil {
			return false, err
		}
		cp.ChargebackCents += e.AmountCents
		cp.Status = store.CardChargeback
		bal, err := tx.LockAccountBalances(ctx, avail)
		if err != nil {
			return false, err
		}
		// Refunds still in flight (REQUESTED / PENDING) are not netted here:
		// the processor already took the disputed funds, and a refund it
		// still completes beyond the remainder arrives as EVENT_CONFLICT for
		// operator review (never posted twice). Flag it now so the operator
		// can cancel the refund at the processor (docs/ACQUIRER_INTEGRATION.md).
		open, err := tx.OpenRefundCents(ctx, cp.ID)
		if err != nil {
			return false, err
		}
		if open > 0 && open > cp.CapturedCents-cp.RefundedCents-cp.ChargebackCents {
			slog.Error("ALERT chargeback opened with refunds in flight beyond the remaining amount", "card_payment_id", cp.ID,
				"chargeback_id", e.ChargebackID, "open_refund_cents", open)
		}
		return true, tx.EnqueueEvent(ctx, "card_payment.chargeback_opened", cp.ID, map[string]any{
			"card_payment_id": cp.ID, "merchant_id": cp.MerchantID, "chargeback_id": e.ChargebackID,
			"amount_cents": e.AmountCents, "available_after_cents": bal[avail], "open_refund_cents": open,
		})

	case acquirer.EventChargebackReversed:
		fresh, err := claimSub(ctx, tx, "acq:"+cp.Provider+":chargeback:"+cp.ID+":"+e.ChargebackID+":reversed")
		if err != nil || !fresh {
			return false, err
		}
		if cp.Status != store.CardChargeback || cp.ChargebackCents < e.AmountCents {
			// Rolls the claim back too: the opened event has not been applied yet.
			return false, fmt.Errorf("%w: chargeback.reversed before chargeback.opened of %s", errEventOutOfOrder, cp.ID)
		}
		if err := postTransfer(ctx, tx, "chargeback_rev_"+cp.ID+"_"+e.ChargebackID, "Chargeback won "+e.ChargebackID+" on "+cp.ID,
			AccountAcquirerReceivable, merchantAvailable(cp.MerchantID), e.AmountCents, cp); err != nil {
			return false, err
		}
		cp.ChargebackCents -= e.AmountCents
		if cp.ChargebackCents == 0 {
			cp.Status = store.CardChargebackReversed
		}
		return true, tx.EnqueueEvent(ctx, "card_payment.chargeback_reversed", cp.ID, map[string]any{
			"card_payment_id": cp.ID, "merchant_id": cp.MerchantID, "chargeback_id": e.ChargebackID, "amount_cents": e.AmountCents,
		})
	}
	return false, errUnsupportedEvent
}

// refundRequestOf locks the refund request a refund event refers to: by the
// provider refund id, or, when that id was not stored yet (provider call timed
// out or its answer is still being written), by the echoed Kerno refund id.
func refundRequestOf(ctx context.Context, tx store.MoneyTx, cp *store.CardPaymentRecord, e acquirer.Event) (store.CardRefundRecord, bool, error) {
	req, ok, err := tx.GetCardRefundByProviderForUpdate(ctx, cp.ID, e.RefundID)
	if err != nil || ok || e.KernoRefundID == "" {
		return req, ok, err
	}
	req, ok, err = tx.GetCardRefundForUpdate(ctx, e.KernoRefundID)
	if err != nil || !ok {
		return store.CardRefundRecord{}, false, err
	}
	if req.CardPaymentID != cp.ID || (req.ProviderRefundID != "" && req.ProviderRefundID != e.RefundID) {
		return store.CardRefundRecord{}, false, nil
	}
	return req, true, nil
}

func merchantAvailable(merchantID string) string {
	return "LIABILITY:MERCHANT:" + merchantID + ":AVAILABLE"
}

// postTransfer posts a two-line journal Dr debit / Cr credit.
func postTransfer(ctx context.Context, tx store.MoneyTx, journalID, description, debit, credit string, cents int64, cp *store.CardPaymentRecord) error {
	jnl, err := ledger.NewJournalEntry(journalID, nowUTC(), description, []ledger.Entry{
		{AccountID: debit, Direction: ledger.Debit, Money: ledger.NewMoney(cents, "MXN")},
		{AccountID: credit, Direction: ledger.Credit, Money: ledger.NewMoney(cents, "MXN")},
	}, map[string]string{"payment_id": cp.ID, "merchant_id": cp.MerchantID, "provider": cp.Provider})
	if err != nil {
		return err
	}
	return tx.PostJournal(ctx, jnl)
}

// ----------------------------------------------------------------------------
// Refund requests
// ----------------------------------------------------------------------------

// CardRefundRequest: POST /api/v1/card-payments/{id}/refunds.
type CardRefundRequest struct {
	AmountCents int64 `json:"amount_cents,omitempty"` // 0 = the whole refundable remainder
}

// CardRefundResponse describes a refund request. The ledger moves only when
// the processor's refund webhook arrives (status SUCCEEDED).
type CardRefundResponse struct {
	RefundID         string                 `json:"refund_id"`
	CardPaymentID    string                 `json:"card_payment_id"`
	AmountCents      int64                  `json:"amount_cents"`
	Status           store.CardRefundStatus `json:"status"`
	ProviderRefundID string                 `json:"provider_refund_id,omitempty"`
}

func refundResponse(r store.CardRefundRecord) CardRefundResponse {
	return CardRefundResponse{RefundID: r.ID, CardPaymentID: r.CardPaymentID, AmountCents: r.AmountCents, Status: r.Status, ProviderRefundID: r.ProviderRefundID}
}

// HandleCreateCardRefund: POST /api/v1/card-payments/{id}/refunds. Owner only
// (cashiers too with ALLOW_CASHIER_REFUNDS). Header Idempotency-Key required.
// The request is stored (holding its amount against the refundable remainder),
// sent to the processor and answered 202; funds move on the webhook.
func (s *Server) HandleCreateCardRefund(w http.ResponseWriter, r *http.Request) {
	prov := s.AcquirerProvider()
	if prov == nil {
		writeError(w, http.StatusServiceUnavailable, "ACQUIRER_NOT_CONFIGURED")
		return
	}
	tenant, ok := s.requireActiveTenant(w, r, "")
	if !ok {
		return
	}
	s.mu.RLock()
	cashierRefunds := s.cashierRefunds
	s.mu.RUnlock()
	if role := RoleFromContext(r.Context()); role != auth.RoleOwner && !(cashierRefunds && role == auth.RoleCashier) {
		writeError(w, http.StatusForbidden, "ROLE_FORBIDDEN")
		return
	}
	idemKey := r.Header.Get("Idempotency-Key")
	if idemKey == "" {
		writeError(w, http.StatusBadRequest, "MISSING_IDEMPOTENCY_KEY")
		return
	}
	if len(idemKey) > maxIdempotencyKeyLen {
		writeError(w, http.StatusBadRequest, "IDEMPOTENCY_KEY_TOO_LONG")
		return
	}
	var req CardRefundRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "INVALID_JSON")
		return
	}
	if req.AmountCents < 0 {
		writeError(w, http.StatusBadRequest, "INVALID_AMOUNT")
		return
	}
	refundID, err := newCardID("cr_")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ID_GENERATION_FAILED")
		return
	}
	ctx := r.Context()
	id := r.PathValue("id")
	var cp store.CardPaymentRecord
	var rf store.CardRefundRecord
	created := false
	err = s.moneyStore().InTx(ctx, func(tx store.MoneyTx) error {
		created = false
		var found bool
		var err error
		cp, found, err = tx.GetCardPaymentForUpdate(ctx, id)
		if err != nil {
			return err
		}
		if !found || cp.MerchantID != tenant {
			return errCardPaymentNotFound
		}
		// A retry replays the stored request whatever the payment state is now.
		if prior, ok, err := tx.GetCardRefundByKeyForUpdate(ctx, cp.ID, idemKey); err != nil {
			return err
		} else if ok {
			if req.AmountCents != 0 && req.AmountCents != prior.AmountCents {
				return errIdempotencyKeyReused
			}
			rf = prior
			return nil
		}
		if !cp.Status.Refundable() {
			return errCardInvalidState
		}
		open, err := tx.OpenRefundCents(ctx, cp.ID)
		if err != nil {
			return err
		}
		remaining := cp.CapturedCents - cp.RefundedCents - cp.ChargebackCents - open
		amount := req.AmountCents
		if amount == 0 {
			amount = remaining
		}
		if amount <= 0 || amount > remaining {
			return errRefundExceedsBalance
		}
		rf, created, err = tx.CreateCardRefund(ctx, store.CardRefundRecord{
			ID: refundID, CardPaymentID: cp.ID, MerchantID: tenant, IdempotencyKey: idemKey, AmountCents: amount, Status: store.RefundRequested,
		})
		if err != nil {
			return err
		}
		if !created { // concurrent request with the same key won
			return nil
		}
		return tx.EnqueueEvent(ctx, "card_payment.refund_requested", cp.ID, refundResponse(rf))
	})
	if err != nil {
		writeCardError(w, "card_refund", err)
		return
	}
	if !created {
		w.Header().Set("Idempotent-Replayed", "true")
		if rf.Status != store.RefundRequested {
			writeJSON(w, http.StatusAccepted, refundResponse(rf))
			return
		}
	}

	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	res, perr := prov.Refund(pctx, acquirer.RefundRequest{IdempotencyKey: "kerno_refund_" + rf.ID, ProviderPaymentID: cp.ProviderPaymentID, AmountCents: rf.AmountCents, KernoRefundID: rf.ID})
	if perr != nil && !errors.Is(perr, acquirer.ErrProviderRejected) {
		// Unknown outcome: stays REQUESTED (amount held); a retry with the
		// same Idempotency-Key re-sends the same provider request.
		slog.Warn("acquirer refund outcome unknown", "refund_id", rf.ID, "error", perr)
		writeError(w, http.StatusBadGateway, "ACQUIRER_UNAVAILABLE")
		return
	}
	err = s.moneyStore().InTx(pctx, func(tx store.MoneyTx) error {
		cur, found, err := tx.GetCardRefundForUpdate(pctx, rf.ID)
		if err != nil {
			return err
		}
		if !found {
			return store.ErrCardPaymentConflict
		}
		rf = cur
		if cur.Status != store.RefundRequested {
			return nil
		}
		if perr != nil { // definitive rejection: release the hold
			cur.Status, cur.LastError = store.RefundFailed, perr.Error()
		} else {
			cur.Status, cur.ProviderRefundID = store.RefundPending, res.ProviderRefundID
		}
		rf = cur
		return tx.UpdateCardRefund(pctx, cur)
	})
	if err != nil {
		slog.Error("card refund state not stored", "refund_id", rf.ID, "error", err)
		writeError(w, http.StatusInternalServerError, "CARD_REFUND_STORE_FAILED")
		return
	}
	if rf.Status == store.RefundFailed {
		writeJSON(w, http.StatusBadGateway, map[string]any{"error": "ACQUIRER_REJECTED", "refund": refundResponse(rf)})
		return
	}
	writeJSON(w, http.StatusAccepted, refundResponse(rf))
}

// ----------------------------------------------------------------------------
// DEV_MODE simulation
// ----------------------------------------------------------------------------

// DevCardSimulateRequest: POST /api/v1/dev/card-payments/{id}/simulate.
type DevCardSimulateRequest struct {
	// Outcome: "tap" (default; the mock scenario of the amount: approve ->
	// authorized + captured, ...51 -> declined), "cancel", or "refund"
	// (completes the pending refund request refund_id).
	Outcome  string `json:"outcome,omitempty"`
	RefundID string `json:"refund_id,omitempty"`
}

// HandleDevCardSimulate (DEV_MODE + mock acquirer only, authenticated, own
// payments only): builds the processor webhook events of a simulated tap /
// cancel / refund, signs them with the server secret and delivers them to the
// webhook handler, so the app and the Web POS demo can complete a card
// payment without a processor.
func (s *Server) HandleDevCardSimulate(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	dev, secret := s.devMode, s.acquirerSecret
	s.mu.RUnlock()
	mock, isMock := s.AcquirerProvider().(*acquirer.MockProvider)
	if !dev || !isMock || len(secret) == 0 {
		http.NotFound(w, r)
		return
	}
	tenant, ok := requireTenant(w, r, "")
	if !ok {
		return
	}
	var req DevCardSimulateRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "INVALID_JSON")
		return
	}
	cp, _, err := s.loadCardPayment(r.Context(), tenant, r.PathValue("id"))
	if err != nil {
		writeError(w, http.StatusNotFound, "CARD_PAYMENT_NOT_FOUND")
		return
	}
	if cp.ProviderPaymentID == "" {
		writeError(w, http.StatusConflict, "NO_PROVIDER_INTENT")
		return
	}
	var events []acquirer.Event
	switch req.Outcome {
	case "", "tap":
		events = mock.SimulateTap(cp.ProviderPaymentID, cp.ID, cp.AmountCents)
	case "cancel":
		events = []acquirer.Event{{ID: "evt_cancel_" + cp.ProviderPaymentID, Type: acquirer.EventCanceled, ProviderPaymentID: cp.ProviderPaymentID}}
	case "refund":
		var rf store.CardRefundRecord
		var found bool
		_ = s.moneyStore().InTx(r.Context(), func(tx store.MoneyTx) error {
			rf, found, _ = tx.GetCardRefundForUpdate(r.Context(), req.RefundID)
			return nil
		})
		if !found || rf.CardPaymentID != cp.ID || rf.ProviderRefundID == "" {
			writeError(w, http.StatusNotFound, "REFUND_NOT_FOUND")
			return
		}
		events = []acquirer.Event{{ID: "evt_refund_" + rf.ProviderRefundID, Type: acquirer.EventRefunded,
			ProviderPaymentID: cp.ProviderPaymentID, RefundID: rf.ProviderRefundID, KernoRefundID: rf.ID, AmountCents: rf.AmountCents}}
	default:
		writeError(w, http.StatusBadRequest, "INVALID_OUTCOME")
		return
	}
	results := make([]json.RawMessage, 0, len(events))
	for _, e := range events {
		body, _ := json.Marshal(e)
		ts := time.Now().Unix()
		signed, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, acquirerWebhookPrefix+mock.Name(), bytes.NewReader(body))
		signed.SetPathValue("provider", mock.Name())
		signed.Header.Set(WebhookTimestampHeader, strconv.FormatInt(ts, 10))
		signed.Header.Set(WebhookSignatureHeader, SignWebhook(secret, ts, body))
		rec := &bufferedResponse{header: http.Header{}}
		s.HandleAcquirerWebhook(rec, signed)
		if rec.code != http.StatusOK {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(rec.code)
			_, _ = w.Write(rec.body.Bytes())
			return
		}
		results = append(results, json.RawMessage(strings.TrimSpace(rec.body.String())))
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": results})
}

// bufferedResponse is a minimal in-process http.ResponseWriter.
type bufferedResponse struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func (b *bufferedResponse) Header() http.Header { return b.header }
func (b *bufferedResponse) WriteHeader(code int) {
	if b.code == 0 {
		b.code = code
	}
}
func (b *bufferedResponse) Write(p []byte) (int, error) {
	if b.code == 0 {
		b.code = http.StatusOK
	}
	return b.body.Write(p)
}
