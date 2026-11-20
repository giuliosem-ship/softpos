package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"softpos/pkg/acquirer"
	"softpos/pkg/ledger"
	"softpos/pkg/store"
)

// Ledger accounts of the processor flow (created at boot by cmd/server).
const (
	AccountAcquirerReceivable = "ASSET:ACQUIRER_RECEIVABLE"
	// AccountAcquirerFees is the processor's fee expense (Kerno's cost).
	AccountAcquirerFees = "EXPENSE:ACQUIRER_FEES"
	// AccountBankOperating is Kerno's bank account receiving processor payouts.
	AccountBankOperating = "ASSET:BANK_OPERATING"
)

// MaxCardPaymentCents bounds one card payment (1,000,000.00 MXN).
const MaxCardPaymentCents int64 = 100_000_000

// SetAcquirerProvider installs the processor adapter (ACQUIRER_PROVIDER).
// Without one the card-payment endpoints answer 503 ACQUIRER_NOT_CONFIGURED.
func (s *Server) SetAcquirerProvider(p acquirer.Provider) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.acquirer = p
}

// AcquirerProvider returns the configured processor adapter (nil if none).
func (s *Server) AcquirerProvider() acquirer.Provider {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.acquirer
}

// SetLegacyCardFlowEnabled toggles the legacy client-trusted EMV endpoints
// (/payments/emv/authorize, /payments/settle, /payments/emv/reverse): they
// "approve" a payment from a TLV the phone sends, so cmd/server enables them
// only with DEV_MODE (demo); otherwise they answer 410 LEGACY_CARD_FLOW_DISABLED.
func (s *Server) SetLegacyCardFlowEnabled(enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.legacyCardFlow = enabled
}

func (s *Server) legacyCardOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.mu.RLock()
		on := s.legacyCardFlow
		s.mu.RUnlock()
		if !on {
			writeError(w, http.StatusGone, "LEGACY_CARD_FLOW_DISABLED")
			return
		}
		h(w, r)
	}
}

func newCardID(prefix string) (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(b), nil
}

// CardPaymentIntentRequest: POST /api/v1/card-payments/intents. The merchant
// comes from the token; amount_cents is validated on the server.
type CardPaymentIntentRequest struct {
	MerchantID     string `json:"merchant_id,omitempty"` // optional; must equal the token's tenant
	AmountCents    int64  `json:"amount_cents"`
	Currency       string `json:"currency,omitempty"`  // "MXN" (default; the only one accepted)
	Reference      string `json:"reference,omitempty"` // merchant reference, <= 64 chars
	IsPersonaMoral bool   `json:"is_persona_moral,omitempty"`
}

// CardPaymentIntentResponse is what the Android app needs to start the
// processor's Tap to Pay SDK. client_secret is a short-lived, single-intent
// session token for the SDK; Kerno does not store it.
type CardPaymentIntentResponse struct {
	CardPaymentID     string                  `json:"card_payment_id"`
	Status            store.CardPaymentStatus `json:"status"`
	AmountCents       int64                   `json:"amount_cents"`
	Currency          string                  `json:"currency"`
	Provider          string                  `json:"provider"`
	ProviderPaymentID string                  `json:"provider_payment_id"`
	ClientSecret      string                  `json:"client_secret,omitempty"`
	ExpiresAt         string                  `json:"expires_at,omitempty"`
	CreatedAt         string                  `json:"created_at"`
}

func intentRequestHash(req CardPaymentIntentRequest) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%d|%s|%s|%t", req.AmountCents, req.Currency, req.Reference, req.IsPersonaMoral)))
	return hex.EncodeToString(h[:])
}

var (
	errProviderIDMismatch = errors.New("card payment already bound to another provider payment id")
)

// HandleCreateCardPaymentIntent: POST /api/v1/card-payments/intents (any
// member). Header Idempotency-Key is required: a retry returns the same card
// payment and the same provider intent (the provider call is idempotent on
// kerno_intent_<card_payment_id>). Nothing is approved here; the final status
// arrives only through the processor's signed webhooks.
func (s *Server) HandleCreateCardPaymentIntent(w http.ResponseWriter, r *http.Request) {
	prov := s.AcquirerProvider()
	if prov == nil {
		writeError(w, http.StatusServiceUnavailable, "ACQUIRER_NOT_CONFIGURED")
		return
	}
	var req CardPaymentIntentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON")
		return
	}
	tenant, ok := requireTenant(w, r, req.MerchantID)
	if !ok {
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
	if req.Currency == "" {
		req.Currency = "MXN"
	}
	switch {
	case req.AmountCents <= 0 || req.AmountCents > MaxCardPaymentCents:
		writeError(w, http.StatusBadRequest, "INVALID_AMOUNT")
		return
	case req.Currency != "MXN":
		writeError(w, http.StatusBadRequest, "UNSUPPORTED_CURRENCY")
		return
	case len(req.Reference) > 64:
		writeError(w, http.StatusBadRequest, "REFERENCE_TOO_LONG")
		return
	}
	if _, found := s.GetMerchant(tenant); !found {
		writeError(w, http.StatusNotFound, "MERCHANT_NOT_FOUND")
		return
	}
	id, err := newCardID("cp_")
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ID_GENERATION_FAILED")
		return
	}
	ctx := r.Context()
	var cp store.CardPaymentRecord
	created := false
	err = s.moneyStore().InTx(ctx, func(tx store.MoneyTx) error {
		var err error
		cp, created, err = tx.CreateCardPayment(ctx, store.CardPaymentRecord{
			ID:                   id,
			MerchantID:           tenant,
			Provider:             prov.Name(),
			IntentIdempotencyKey: idemKey,
			RequestHash:          intentRequestHash(req),
			AmountCents:          req.AmountCents,
			Currency:             req.Currency,
			Reference:            req.Reference,
			PersonaMoral:         req.IsPersonaMoral,
			Status:               store.CardCreated,
		})
		if err != nil {
			return err
		}
		if !created && cp.RequestHash != intentRequestHash(req) {
			return errIdempotencyKeyReused
		}
		return nil
	})
	if errors.Is(err, errIdempotencyKeyReused) {
		writeError(w, http.StatusConflict, "IDEMPOTENCY_KEY_REUSED")
		return
	}
	if err != nil {
		slog.Error("card payment intent not stored", "merchant_id", tenant, "error", err)
		writeError(w, http.StatusInternalServerError, "CARD_PAYMENT_STORE_FAILED")
		return
	}

	resp := CardPaymentIntentResponse{
		CardPaymentID: cp.ID, Status: cp.Status, AmountCents: cp.AmountCents, Currency: cp.Currency,
		Provider: cp.Provider, ProviderPaymentID: cp.ProviderPaymentID, CreatedAt: cp.CreatedAt.UTC().Format(time.RFC3339),
	}
	code := http.StatusCreated
	if !created {
		code = http.StatusOK
		w.Header().Set("Idempotent-Replayed", "true")
	}
	if cp.Provider != prov.Name() {
		writeError(w, http.StatusConflict, "ACQUIRER_CHANGED")
		return
	}
	if cp.Status != store.CardCreated { // the tap already happened: no new SDK session
		writeJSON(w, code, resp)
		return
	}

	// Provider call outside the transaction (idempotent on the card payment id),
	// detached from the client connection so a disconnect cannot leave the
	// provider intent half-created.
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	intent, err := prov.CreatePaymentIntent(pctx, acquirer.IntentRequest{
		IdempotencyKey: "kerno_intent_" + cp.ID,
		MerchantID:     tenant,
		CardPaymentID:  cp.ID,
		AmountCents:    cp.AmountCents,
		Currency:       cp.Currency,
		Reference:      cp.Reference,
	})
	if err != nil {
		slog.Warn("acquirer intent creation failed", "card_payment_id", cp.ID, "error", err)
		if errors.Is(err, acquirer.ErrProviderRejected) {
			writeError(w, http.StatusBadGateway, "ACQUIRER_REJECTED")
			return
		}
		// Unknown outcome: the row stays CREATED; retrying with the same
		// Idempotency-Key re-asks the provider for the same intent.
		writeError(w, http.StatusBadGateway, "ACQUIRER_UNAVAILABLE")
		return
	}
	if cp.ProviderPaymentID != intent.ProviderPaymentID {
		err = s.moneyStore().InTx(pctx, func(tx store.MoneyTx) error {
			cur, found, err := tx.GetCardPaymentForUpdate(pctx, cp.ID)
			if err != nil {
				return err
			}
			if !found {
				return store.ErrCardPaymentConflict
			}
			if cur.ProviderPaymentID != "" && cur.ProviderPaymentID != intent.ProviderPaymentID {
				return errProviderIDMismatch
			}
			if cur.ProviderPaymentID == intent.ProviderPaymentID {
				return nil
			}
			cur.ProviderPaymentID = intent.ProviderPaymentID
			cp = cur
			return tx.UpdateCardPayment(pctx, cur)
		})
		if err != nil {
			slog.Error("acquirer intent id not stored", "card_payment_id", cp.ID, "error", err)
			writeError(w, http.StatusInternalServerError, "CARD_PAYMENT_STORE_FAILED")
			return
		}
	}
	resp.Status = cp.Status
	resp.ProviderPaymentID = intent.ProviderPaymentID
	resp.ClientSecret = intent.ClientSecret
	if !intent.ExpiresAt.IsZero() {
		resp.ExpiresAt = intent.ExpiresAt.UTC().Format(time.RFC3339)
	}
	writeJSON(w, code, resp)
}

// CardPaymentResponse is GET /api/v1/card-payments/{id}: the server-side
// truth the app polls after the tap (status changes only via webhooks).
type CardPaymentResponse struct {
	CardPaymentID     string                  `json:"card_payment_id"`
	MerchantID        string                  `json:"merchant_id"`
	Provider          string                  `json:"provider"`
	ProviderPaymentID string                  `json:"provider_payment_id"`
	Status            store.CardPaymentStatus `json:"status"`
	AmountCents       int64                   `json:"amount_cents"`
	Currency          string                  `json:"currency"`
	Reference         string                  `json:"reference,omitempty"`
	CapturedCents     int64                   `json:"captured_cents"`
	RefundedCents     int64                   `json:"refunded_cents"`
	ChargebackCents   int64                   `json:"chargeback_cents"`
	DeclineCode       string                  `json:"decline_code,omitempty"`
	Final             bool                    `json:"final"`
	Settlement        *SettlePaymentResponse  `json:"settlement,omitempty"`
	CreatedAt         string                  `json:"created_at"`
	UpdatedAt         string                  `json:"updated_at"`
}

// finalCardStatus: the app can stop polling (no further status change is
// expected from the tap; refunds/chargebacks may still follow).
func finalCardStatus(st store.CardPaymentStatus) bool {
	return st != store.CardCreated && st != store.CardAuthorized
}

func cardPaymentResponse(cp store.CardPaymentRecord, settlement *store.SettlementRecord) CardPaymentResponse {
	out := CardPaymentResponse{
		CardPaymentID: cp.ID, MerchantID: cp.MerchantID, Provider: cp.Provider, ProviderPaymentID: cp.ProviderPaymentID,
		Status: cp.Status, AmountCents: cp.AmountCents, Currency: cp.Currency, Reference: cp.Reference,
		CapturedCents: cp.CapturedCents, RefundedCents: cp.RefundedCents, ChargebackCents: cp.ChargebackCents,
		DeclineCode: cp.DeclineCode, Final: finalCardStatus(cp.Status),
		CreatedAt: cp.CreatedAt.UTC().Format(time.RFC3339), UpdatedAt: cp.UpdatedAt.UTC().Format(time.RFC3339),
	}
	if settlement != nil {
		sr := settleResponseFromRecord(*settlement)
		out.Settlement = &sr
	}
	return out
}

// loadCardPayment returns a tenant's card payment and its settlement record.
func (s *Server) loadCardPayment(ctx context.Context, tenant, id string) (store.CardPaymentRecord, *store.SettlementRecord, error) {
	var cp store.CardPaymentRecord
	var settlement *store.SettlementRecord
	err := s.moneyStore().InTx(ctx, func(tx store.MoneyTx) error {
		var found bool
		var err error
		cp, found, err = tx.GetCardPaymentForUpdate(ctx, id)
		if err != nil {
			return err
		}
		if !found || cp.MerchantID != tenant { // foreign payments look nonexistent
			return errCardPaymentNotFound
		}
		if cp.Status.Captured() {
			rec, ok, err := tx.GetSettlementForUpdate(ctx, cp.ID)
			if err != nil {
				return err
			}
			if ok {
				settlement = &rec
			}
		}
		return nil
	})
	return cp, settlement, err
}

var errCardPaymentNotFound = errors.New("card payment not found")

// HandleGetCardPayment: GET /api/v1/card-payments/{id} (tenant-scoped).
func (s *Server) HandleGetCardPayment(w http.ResponseWriter, r *http.Request) {
	tenant, ok := requireTenant(w, r, "")
	if !ok {
		return
	}
	cp, settlement, err := s.loadCardPayment(r.Context(), tenant, r.PathValue("id"))
	if errors.Is(err, errCardPaymentNotFound) {
		writeError(w, http.StatusNotFound, "CARD_PAYMENT_NOT_FOUND")
		return
	}
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "CARD_PAYMENT_UNAVAILABLE")
		return
	}
	writeJSON(w, http.StatusOK, cardPaymentResponse(cp, settlement))
}

// HandleCancelCardPayment: POST /api/v1/card-payments/{id}/cancel (any
// member). A payment the provider never saw is canceled at once; otherwise
// the cancellation is requested from the provider (202) and the status moves
// to CANCELED only when its payment.canceled webhook arrives.
func (s *Server) HandleCancelCardPayment(w http.ResponseWriter, r *http.Request) {
	prov := s.AcquirerProvider()
	if prov == nil {
		writeError(w, http.StatusServiceUnavailable, "ACQUIRER_NOT_CONFIGURED")
		return
	}
	tenant, ok := requireTenant(w, r, "")
	if !ok {
		return
	}
	ctx := r.Context()
	id := r.PathValue("id")
	var cp store.CardPaymentRecord
	local := false
	err := s.moneyStore().InTx(ctx, func(tx store.MoneyTx) error {
		var found bool
		var err error
		cp, found, err = tx.GetCardPaymentForUpdate(ctx, id)
		if err != nil {
			return err
		}
		if !found || cp.MerchantID != tenant {
			return errCardPaymentNotFound
		}
		if cp.Status != store.CardCreated && cp.Status != store.CardAuthorized {
			return errCardInvalidState
		}
		if cp.ProviderPaymentID == "" {
			local = true
			cp.Status = store.CardCanceled
			if err := tx.UpdateCardPayment(ctx, cp); err != nil {
				return err
			}
			return tx.EnqueueEvent(ctx, "card_payment.canceled", cp.ID, map[string]string{"card_payment_id": cp.ID, "merchant_id": tenant})
		}
		return nil
	})
	if err != nil {
		writeCardError(w, "cancel", err)
		return
	}
	if local {
		writeJSON(w, http.StatusOK, cardPaymentResponse(cp, nil))
		return
	}
	pctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if _, err := prov.Cancel(pctx, acquirer.CancelRequest{IdempotencyKey: "kerno_cancel_" + cp.ID, ProviderPaymentID: cp.ProviderPaymentID}); err != nil {
		if errors.Is(err, acquirer.ErrProviderRejected) {
			writeError(w, http.StatusConflict, "ACQUIRER_REJECTED")
			return
		}
		writeError(w, http.StatusBadGateway, "ACQUIRER_UNAVAILABLE")
		return
	}
	resp := cardPaymentResponse(cp, nil)
	writeJSON(w, http.StatusAccepted, map[string]any{"card_payment": resp, "cancel_requested": true})
}

var errCardInvalidState = errors.New("card payment is not in a state that allows this operation")

// writeCardError maps card-payment unit-of-work errors.
func writeCardError(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, errCardPaymentNotFound):
		writeError(w, http.StatusNotFound, "CARD_PAYMENT_NOT_FOUND")
	case errors.Is(err, errCardInvalidState):
		writeError(w, http.StatusConflict, "INVALID_PAYMENT_STATE")
	case errors.Is(err, errIdempotencyKeyReused):
		writeError(w, http.StatusConflict, "IDEMPOTENCY_KEY_REUSED")
	case errors.Is(err, errRefundRoleForbidden):
		writeError(w, http.StatusForbidden, "ROLE_FORBIDDEN")
	default:
		writeMoneyError(w, op, err)
	}
}

// ----------------------------------------------------------------------------
// Processor events -> card payment state machine + ledger
// ----------------------------------------------------------------------------

var (
	// errEventOutOfOrder: the event needs an earlier one that has not arrived
	// (e.g. a refund before the capture). Nothing is consumed; the provider
	// retries the delivery later.
	errEventOutOfOrder = errors.New("acquirer event arrived before the event it depends on")
	// errEventConflict: the event contradicts the recorded state (capture of a
	// canceled payment, amounts above what was captured). Alerted, not applied.
	errEventConflict = errors.New("acquirer event conflicts with the recorded payment state")
	// errUnknownCardPayment: no card payment matches the event.
	errUnknownCardPayment = errors.New("acquirer event for an unknown card payment")
	// errUnsupportedEvent: the event type is not handled by this server version.
	errUnsupportedEvent = errors.New("unsupported acquirer event type")
)

// AcquirerEventResult is the stored (and replayed) outcome of one event.
type AcquirerEventResult struct {
	EventID       string                  `json:"event_id"`
	EventType     acquirer.EventType      `json:"event_type"`
	Result        string                  `json:"result"` // applied | ignored
	CardPaymentID string                  `json:"card_payment_id,omitempty"`
	Status        store.CardPaymentStatus `json:"status,omitempty"`
	PayoutID      string                  `json:"payout_id,omitempty"`
	Discrepancies int                     `json:"discrepancies,omitempty"`
}

// cardEventEffects carries post-commit work of an applied event.
type cardEventEffects struct {
	merchantID    string
	rbfSweepCents int64
	alerts        []string
}

// ApplyAcquirerEvent applies one verified processor event exactly once
// (money_operations key acq:<provider>:evt:<event id>) and returns the stored
// result. Duplicates replay the first result; events that regress the state
// machine are recorded as ignored.
func (s *Server) ApplyAcquirerEvent(ctx context.Context, provider string, e acquirer.Event) ([]byte, error) {
	if err := e.Validate(); err != nil {
		return nil, err
	}
	if e.Type == acquirer.EventPayoutSettled {
		return s.applyAcquirerPayout(ctx, provider, e)
	}
	// RBF state is in process memory: serialize captures of merchants with an
	// active loan (same rule as the legacy settle).
	if e.Type == acquirer.EventCaptured {
		if merchantID := s.cardPaymentMerchant(ctx, provider, e); merchantID != "" && s.hasActiveRBFLoan(merchantID) {
			s.rbfMu.Lock()
			defer s.rbfMu.Unlock()
		}
	}
	var out []byte
	var fx cardEventEffects
	err := s.moneyStore().InTx(ctx, func(tx store.MoneyTx) error {
		out, fx = nil, cardEventEffects{}
		cp, found, err := tx.GetCardPaymentByProviderForUpdate(ctx, provider, e.ProviderPaymentID)
		if err != nil {
			return err
		}
		if !found && e.KernoPaymentID != "" {
			// The provider id may not be stored yet (intent call outcome lost).
			cp, found, err = tx.GetCardPaymentForUpdate(ctx, e.KernoPaymentID)
			if err != nil {
				return err
			}
			if found && (cp.Provider != provider || (cp.ProviderPaymentID != "" && cp.ProviderPaymentID != e.ProviderPaymentID)) {
				found = false
			}
			if found {
				cp.ProviderPaymentID = e.ProviderPaymentID
			}
		}
		if !found {
			return errUnknownCardPayment
		}
		opKey := "acq:" + provider + ":evt:" + e.ID
		claimed, _, prior, err := tx.ClaimOperation(ctx, opKey)
		if err != nil {
			return err
		}
		if !claimed {
			out = prior
			return nil
		}
		fx.merchantID = cp.MerchantID
		applied, err := s.applyCardEvent(ctx, tx, &cp, e, &fx)
		if err != nil {
			return err
		}
		if err := tx.UpdateCardPayment(ctx, cp); err != nil {
			return err
		}
		res := AcquirerEventResult{EventID: e.ID, EventType: e.Type, Result: "ignored", CardPaymentID: cp.ID, Status: cp.Status}
		if applied {
			res.Result = "applied"
		}
		if out, err = json.Marshal(res); err != nil {
			return err
		}
		return tx.CompleteOperation(ctx, opKey, http.StatusOK, out)
	})
	if err != nil {
		return nil, err
	}
	for _, a := range fx.alerts {
		slog.Error("ALERT acquirer event", "provider", provider, "event_id", e.ID, "event_type", e.Type, "detail", a)
	}
	if fx.rbfSweepCents > 0 {
		if _, err := s.rbfEngine.ApplyRepayment(fx.merchantID, fx.rbfSweepCents); err != nil {
			slog.Error("rbf repayment not applied after card capture", "event_id", e.ID, "error", err)
		}
	}
	return out, nil
}

// cardPaymentMerchant looks up the merchant of an event's payment (no lock
// kept; only used to decide the RBF serialization).
func (s *Server) cardPaymentMerchant(ctx context.Context, provider string, e acquirer.Event) string {
	var merchant string
	_ = s.moneyStore().InTx(ctx, func(tx store.MoneyTx) error {
		cp, found, err := tx.GetCardPaymentByProviderForUpdate(ctx, provider, e.ProviderPaymentID)
		if err == nil && !found && e.KernoPaymentID != "" {
			cp, found, err = tx.GetCardPaymentForUpdate(ctx, e.KernoPaymentID)
		}
		if err == nil && found {
			merchant = cp.MerchantID
		}
		return nil
	})
	return merchant
}

// applyCardEvent moves the state machine of a locked card payment and posts
// the ledger effects. It returns applied=false for events that would regress
// the state (out-of-order deliveries are tolerated by ignoring them).
func (s *Server) applyCardEvent(ctx context.Context, tx store.MoneyTx, cp *store.CardPaymentRecord, e acquirer.Event, fx *cardEventEffects) (bool, error) {
	preTap := cp.Status == store.CardCreated || cp.Status == store.CardAuthorized
	switch e.Type {
	case acquirer.EventAuthorized:
		if cp.Status != store.CardCreated && cp.Status != store.CardDeclined {
			return false, nil
		}
		cp.Status, cp.DeclineCode = store.CardAuthorized, ""
		return true, tx.EnqueueEvent(ctx, "card_payment.authorized", cp.ID, cardPaymentResponse(*cp, nil))

	case acquirer.EventDeclined:
		// A declined attempt does not end the intent (the buyer may tap again).
		if !preTap {
			return false, nil
		}
		cp.Status, cp.DeclineCode = store.CardDeclined, e.DeclineCode
		return true, tx.EnqueueEvent(ctx, "card_payment.declined", cp.ID, cardPaymentResponse(*cp, nil))

	case acquirer.EventCanceled:
		if cp.Status.Captured() {
			fx.alerts = append(fx.alerts, "payment.canceled for captured card payment "+cp.ID+" ignored")
			return false, nil
		}
		if cp.Status == store.CardCanceled {
			return false, nil
		}
		cp.Status = store.CardCanceled
		return true, tx.EnqueueEvent(ctx, "card_payment.canceled", cp.ID, cardPaymentResponse(*cp, nil))

	case acquirer.EventCaptured:
		if cp.Status.Captured() {
			return false, nil
		}
		if cp.Status == store.CardCanceled || e.AmountCents > cp.AmountCents {
			return false, fmt.Errorf("%w: capture of %d on %s payment %s (amount %d)", errEventConflict, e.AmountCents, cp.Status, cp.ID, cp.AmountCents)
		}
		feeBps, err := s.platformFeeBps(ctx, cp.MerchantID)
		if err != nil {
			return false, err
		}
		rec, err := s.postSettlement(ctx, tx, cp.MerchantID, cp.ID, e.AmountCents, cp.PersonaMoral, feeBps, "Card capture "+cp.ID)
		if err != nil {
			return false, err
		}
		if e.FeeCents > 0 {
			jnl, err := ledger.NewJournalEntry("acq_fee_"+cp.ID, nowUTC(), "Processor fee "+cp.ID, []ledger.Entry{
				{AccountID: AccountAcquirerFees, Direction: ledger.Debit, Money: ledger.NewMoney(e.FeeCents, "MXN")},
				{AccountID: AccountAcquirerReceivable, Direction: ledger.Credit, Money: ledger.NewMoney(e.FeeCents, "MXN")},
			}, map[string]string{"payment_id": cp.ID, "provider": cp.Provider})
			if err != nil {
				return false, err
			}
			if err := tx.PostJournal(ctx, jnl); err != nil {
				return false, err
			}
		}
		cp.Status, cp.DeclineCode = store.CardCaptured, ""
		cp.CapturedCents, cp.ProviderFeeCents, cp.ProviderSettlementDate = e.AmountCents, e.FeeCents, e.SettlementDate
		fx.rbfSweepCents = rec.RBFSweepCents
		return true, tx.EnqueueEvent(ctx, "card_payment.captured", cp.ID, cardPaymentResponse(*cp, &rec))
	}
	return s.applyCardMoneyEvent(ctx, tx, cp, e, fx)
}

// claimSub claims a secondary exactly-once key (a refund or chargeback may be
// notified by several events with different ids).
func claimSub(ctx context.Context, tx store.MoneyTx, key string) (bool, error) {
	claimed, _, _, err := tx.ClaimOperation(ctx, key)
	if err != nil {
		return false, err
	}
	if claimed {
		return true, tx.CompleteOperation(ctx, key, http.StatusOK, []byte(`{}`))
	}
	return false, nil
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }
