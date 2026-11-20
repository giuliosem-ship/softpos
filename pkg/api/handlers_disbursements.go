package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"softpos/pkg/auth"
	"softpos/pkg/payouts"
	"softpos/pkg/store"
	"softpos/pkg/stp"
)

// DisbursementRequest pays out to the merchant's registered and VERIFIED
// settlement CLABE. destination_clabe is optional; if sent it must equal the
// registered account (403 DESTINATION_CLABE_NOT_ALLOWED).
type DisbursementRequest struct {
	MerchantID       string `json:"merchant_id,omitempty"`
	AmountCents      int64  `json:"amount_cents"`
	DestinationCLABE string `json:"destination_clabe,omitempty"`
	BeneficiaryName  string `json:"beneficiary_name,omitempty"`
	Concept          string `json:"concept"`
}

type DisbursementResponse struct {
	STPOrderID            int64  `json:"stp_order_id"`
	ClaveRastreo          string `json:"clave_rastreo"`
	Status                string `json:"status"`     // "DISPATCHED" (or "SETTLED" on a replay after recovery)
	CEPStatus             string `json:"cep_status"` // "PENDING_CEP"
	AmountCents           int64  `json:"amount_cents"`
	DestinationCLABE      string `json:"destination_clabe"`
	RemainingBalanceCents int64  `json:"remaining_balance_cents"`
	Timestamp             string `json:"timestamp"`
}

func (s *Server) HandleDisbursement(w http.ResponseWriter, r *http.Request) {
	var req DisbursementRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON")
		return
	}

	tenant, ok := requireTenant(w, r, req.MerchantID)
	if !ok {
		return
	}
	if !requireRole(w, r, auth.RoleOwner) { // cashiers cannot move money out
		return
	}
	if !s.requireActiveMerchant(w, r, tenant) { // approved KYB, not blocked
		return
	}
	// Every payout needs a merchant-scoped Idempotency-Key: a retried request
	// replays the stored outcome instead of paying twice.
	idemKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idemKey == "" {
		writeError(w, http.StatusBadRequest, "MISSING_IDEMPOTENCY_KEY")
		return
	}
	if len(idemKey) > maxIdempotencyKeyLen {
		writeError(w, http.StatusBadRequest, "INVALID_IDEMPOTENCY_KEY")
		return
	}

	if req.AmountCents <= 0 {
		writeError(w, http.StatusBadRequest, "INVALID_AMOUNT")
		return
	}
	ctx := r.Context()

	merchant, exists := s.GetMerchant(tenant)
	if !exists {
		writeError(w, http.StatusNotFound, "MERCHANT_NOT_FOUND")
		return
	}

	// 1. Destination = the registered, ownership-verified settlement CLABE only.
	acct, found, err := s.merchantRepo.GetSettlementAccount(ctx, tenant)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "SETTLEMENT_ACCOUNT_UNAVAILABLE")
		return
	}
	if !found {
		writeError(w, http.StatusConflict, "SETTLEMENT_ACCOUNT_NOT_REGISTERED")
		return
	}
	if req.DestinationCLABE != "" && req.DestinationCLABE != acct.CLABE {
		writeError(w, http.StatusForbidden, "DESTINATION_CLABE_NOT_ALLOWED")
		return
	}
	if acct.Status != store.SettlementVerified {
		writeError(w, http.StatusConflict, "SETTLEMENT_ACCOUNT_NOT_VERIFIED")
		return
	}
	if !stp.ValidateCLABE(acct.CLABE) {
		writeError(w, http.StatusConflict, "INVALID_DESTINATION_CLABE")
		return
	}

	// 2. Regulatory Compliance Screen (SAT Article 69-B EFOS)
	if _, err := s.blacklist.ValidateMerchantEligibility(merchant.RFC); err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error":  "SAT_69B_SANCTION_HOLD",
			"reason": "Merchant RFC is blacklisted under SAT Article 69-B",
		})
		return
	}

	now := nowUTC()
	claveRastreo, err := newClaveRastreo()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "CLAVE_RASTREO_UNAVAILABLE")
		return
	}
	beneficiary := req.BeneficiaryName
	if beneficiary == "" {
		beneficiary = merchant.Name
	}
	payout := store.PayoutRecord{
		ClaveRastreo:     claveRastreo,
		MerchantID:       tenant,
		IdempotencyKey:   idemKey,
		RequestHash:      disbursementRequestHash(req),
		AmountCents:      req.AmountCents,
		DestinationCLABE: acct.CLABE,
		BeneficiaryName:  beneficiary,
		BeneficiaryRFC:   merchant.RFC,
		Concept:          req.Concept,
		FechaOperacion:   now.Format("20060102"),
		Status:           store.PayoutReserved,
	}
	accountID := payouts.AvailableAccount(tenant)

	// 3. Payout row (idempotency) + balance check + debit under the ledger row
	// lock, in one transaction: two concurrent payouts cannot both spend the
	// same balance, and a duplicate key never debits twice. Funds are reserved
	// before calling STP; the row lets the payout reconciler finish the job if
	// this process dies before or during registraOrden.
	var replay *store.PayoutRecord
	err = s.moneyStore().InTx(ctx, func(tx store.MoneyTx) error {
		existing, created, err := tx.CreatePayout(ctx, payout)
		if err != nil {
			return err
		}
		if !created {
			if existing.RequestHash != payout.RequestHash {
				return errIdempotencyKeyReused
			}
			replay = &existing
			return nil
		}
		bals, err := tx.LockAccountBalances(ctx, accountID, payouts.ConcentratorAccount)
		if err != nil {
			return err
		}
		if bals[accountID] < req.AmountCents {
			return errInsufficientFunds
		}
		jnl, err := payouts.ReservationJournal(payout, now)
		if err != nil {
			return err
		}
		if err := tx.PostJournal(ctx, jnl); err != nil {
			return err
		}
		payout.RemainingBalanceCents = bals[accountID] - req.AmountCents
		if err := tx.SetPayoutRemainingBalance(ctx, claveRastreo, payout.RemainingBalanceCents); err != nil {
			return err
		}
		return tx.EnqueueEvent(ctx, "disbursement.requested", claveRastreo, map[string]any{
			"clave_rastreo": claveRastreo, "merchant_id": tenant, "amount_cents": req.AmountCents, "destination_clabe": acct.CLABE,
		})
	})
	if errors.Is(err, errIdempotencyKeyReused) {
		writeError(w, http.StatusConflict, "IDEMPOTENCY_KEY_REUSED")
		return
	}
	if err != nil {
		writeMoneyError(w, "disbursement", err)
		return
	}
	if replay != nil {
		w.Header().Set("Idempotent-Replayed", "true")
		writePayoutOutcome(w, *replay)
		return
	}
	remaining := payout.RemainingBalanceCents

	// 4. Sign and dispatch outbound order via STP registraOrden client. The
	// call is detached from the client connection: a disconnect must not
	// abort an order that may already have reached STP.
	s.mu.RLock()
	stpClient := s.stpClient
	s.mu.RUnlock()
	dctx, cancelDispatch := context.WithTimeout(context.WithoutCancel(ctx), stpDispatchTimeout)
	defer cancelDispatch()
	stpResp, err := stpClient.RegisterOrder(dctx, stp.STPOrderRequest{
		ClaveRastreo:        claveRastreo,
		FechaOperacion:      payout.FechaOperacion,
		AmountCents:         req.AmountCents,
		NombreBeneficiario:  beneficiary,
		CuentaBeneficiario:  acct.CLABE,
		RfcCurpBeneficiario: merchant.RFC,
		ConceptoPago:        req.Concept,
	})
	cctx := context.WithoutCancel(ctx)
	if err != nil && dispatchOutcomeUnknown(err) {
		// The order may have been registered (timeout, reset, 5xx): returning
		// the funds could pay the merchant twice. Keep the reservation and
		// leave it to the payout reconciler (STP order status).
		dispatchErr := err
		if eerr := s.moneyStore().InTx(cctx, func(tx store.MoneyTx) error {
			if err := tx.SetPayoutStatus(cctx, claveRastreo, store.PayoutReserved, store.PayoutPendingReconciliation, 0, dispatchErr.Error()); err != nil {
				return err
			}
			return tx.EnqueueEvent(cctx, "disbursement.dispatch_unknown", claveRastreo, map[string]any{
				"clave_rastreo": claveRastreo, "merchant_id": tenant, "amount_cents": req.AmountCents, "error": dispatchErr.Error(),
			})
		}); eerr != nil {
			slog.Error("disbursement dispatch outcome unknown and not recorded; payout stays RESERVED for the reconciler", "clave_rastreo", claveRastreo, "error", eerr)
		}
		slog.Error("disbursement dispatch outcome unknown; funds stay reserved pending reconciliation", "clave_rastreo", claveRastreo, "error", err)
		s.cepReconciler.TrackDisbursement(stp.DisbursementRecord{
			ClaveRastreo:          claveRastreo,
			MerchantID:            tenant,
			AmountCents:           req.AmountCents,
			DestinationCLABE:      acct.CLABE,
			BeneficiaryName:       beneficiary,
			Concept:               req.Concept,
			FechaOperacion:        payout.FechaOperacion,
			Status:                stp.StatusPendingCEP,
			RemainingBalanceCents: remaining,
			CreatedAt:             now,
		})
		payout.Status = store.PayoutPendingReconciliation
		writePayoutOutcome(w, payout)
		return
	}
	if err != nil {
		// Definitive rejection (STP answered, or the order never left):
		// compensate the reservation in the same transaction as the status
		// change; if that fails the payout stays RESERVED and the reconciler
		// resolves it (logged loudly).
		dispatchErr := err
		cerr := s.moneyStore().InTx(cctx, func(tx store.MoneyTx) error {
			if err := tx.SetPayoutStatus(cctx, claveRastreo, store.PayoutReserved, store.PayoutFailed, 0, dispatchErr.Error()); err != nil {
				return err
			}
			jnl, err := payouts.CompensationJournal(payout, nowUTC(), "dispatch failed")
			if err != nil {
				return err
			}
			if err := tx.PostJournal(cctx, jnl); err != nil {
				return err
			}
			return tx.EnqueueEvent(cctx, "disbursement.failed", claveRastreo, map[string]any{
				"clave_rastreo": claveRastreo, "merchant_id": tenant, "amount_cents": req.AmountCents, "error": dispatchErr.Error(),
			})
		})
		if cerr != nil {
			slog.Error("disbursement compensation failed; payout left to the reconciler", "clave_rastreo", claveRastreo, "error", cerr)
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error":  "STP_DISPATCH_FAILED",
			"detail": err.Error(),
		})
		return
	}

	if eerr := s.moneyStore().InTx(cctx, func(tx store.MoneyTx) error {
		return tx.SetPayoutStatus(cctx, claveRastreo, store.PayoutReserved, store.PayoutDispatched, stpResp.STPOrderID, "")
	}); eerr != nil {
		// STP has the order; the reconciler will confirm it from STP.
		slog.Error("disbursement dispatched but status not recorded; payout stays RESERVED for the reconciler", "clave_rastreo", claveRastreo, "error", eerr)
	}

	// 5. Track disbursement for automatic Banxico CEP reconciliation
	s.cepReconciler.TrackDisbursement(stp.DisbursementRecord{
		ClaveRastreo:          claveRastreo,
		STPOrderID:            stpResp.STPOrderID,
		MerchantID:            tenant,
		AmountCents:           req.AmountCents,
		DestinationCLABE:      acct.CLABE,
		BeneficiaryName:       beneficiary,
		Concept:               req.Concept,
		FechaOperacion:        payout.FechaOperacion,
		Status:                stp.StatusPendingCEP,
		RemainingBalanceCents: remaining,
		CreatedAt:             now,
	})

	payout.Status = store.PayoutDispatched
	payout.STPOrderID = stpResp.STPOrderID
	payout.CreatedAt = now
	writePayoutOutcome(w, payout)
}

// maxIdempotencyKeyLen matches payouts.idempotency_key VARCHAR(255).
const maxIdempotencyKeyLen = 255

var errIdempotencyKeyReused = errors.New("idempotency key reused with a different request")

// disbursementRequestHash fingerprints the client-controlled fields of a
// payout request; the same Idempotency-Key with a different fingerprint is a
// 409 instead of a replay.
func disbursementRequestHash(req DisbursementRequest) string {
	b, _ := json.Marshal([]any{req.AmountCents, req.DestinationCLABE, req.BeneficiaryName, req.Concept})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// newClaveRastreo returns a unique SPEI tracking key: "KRN" + 24 uppercase
// hex chars (27 <= 30, alphanumeric). Random, so concurrent requests can never
// collide (time-based keys did on coarse clocks).
func newClaveRastreo() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "KRN" + strings.ToUpper(hex.EncodeToString(b)), nil
}

// writePayoutOutcome renders a payout's current state (fresh or replayed).
func writePayoutOutcome(w http.ResponseWriter, p store.PayoutRecord) {
	switch p.Status {
	case store.PayoutDispatched, store.PayoutSettled:
		ts := p.CreatedAt
		if ts.IsZero() {
			ts = nowUTC()
		}
		writeJSON(w, http.StatusOK, DisbursementResponse{
			STPOrderID:            p.STPOrderID,
			ClaveRastreo:          p.ClaveRastreo,
			Status:                string(p.Status),
			CEPStatus:             string(stp.StatusPendingCEP),
			AmountCents:           p.AmountCents,
			DestinationCLABE:      p.DestinationCLABE,
			RemainingBalanceCents: p.RemainingBalanceCents,
			Timestamp:             ts.UTC().Format(time.RFC3339),
		})
	case store.PayoutFailed:
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error":         "STP_DISPATCH_FAILED",
			"detail":        p.LastError,
			"clave_rastreo": p.ClaveRastreo,
		})
	default: // RESERVED (in flight / interrupted) or PENDING_RECONCILIATION
		writeJSON(w, http.StatusAccepted, map[string]any{
			"status":        string(store.PayoutPendingReconciliation),
			"clave_rastreo": p.ClaveRastreo,
			"amount_cents":  p.AmountCents,
		})
	}
}

// stpDispatchTimeout bounds a registraOrden call detached from the request.
const stpDispatchTimeout = 30 * time.Second

// dispatchOutcomeUnknown reports whether a registraOrden error leaves it
// unknown whether STP registered the order (transport failure after sending,
// timeout, 5xx, unreadable response). Rejections and validation/signing
// errors are definitive.
func dispatchOutcomeUnknown(err error) bool {
	return errors.Is(err, stp.ErrSTPNetworkError) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

func (s *Server) HandleGetDisbursement(w http.ResponseWriter, r *http.Request) {
	clave := r.PathValue("clave")
	if clave == "" {
		http.Error(w, `{"error":"MISSING_CLAVE_RASTREO"}`, http.StatusBadRequest)
		return
	}

	tenant, ok := requireTenant(w, r, "")
	if !ok {
		return
	}
	rec, err := s.cepReconciler.GetDisbursement(clave)
	if err != nil || rec.MerchantID != tenant { // foreign records look nonexistent
		http.Error(w, `{"error":"DISBURSEMENT_NOT_FOUND"}`, http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(rec)
}

func (s *Server) HandleReconcileCEP(w http.ResponseWriter, r *http.Request) {
	clave := r.PathValue("clave")
	if clave == "" {
		http.Error(w, `{"error":"MISSING_CLAVE_RASTREO"}`, http.StatusBadRequest)
		return
	}

	tenant, ok := requireTenant(w, r, "")
	if !ok {
		return
	}
	if owned, err := s.cepReconciler.GetDisbursement(clave); err != nil || owned.MerchantID != tenant {
		http.Error(w, `{"error":"DISBURSEMENT_NOT_FOUND"}`, http.StatusNotFound)
		return
	}

	rec, err := s.cepReconciler.ReconcileByClaveRastreo(r.Context(), clave)
	if err != nil {
		if errors.Is(err, stp.ErrDisbursementNotFound) {
			http.Error(w, `{"error":"DISBURSEMENT_NOT_FOUND"}`, http.StatusNotFound)
			return
		}
		if errors.Is(err, stp.ErrCEPAmountMismatch) || errors.Is(err, stp.ErrCEPAccountMismatch) || errors.Is(err, stp.ErrInvalidCEPSignature) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error":  "CEP_RECONCILIATION_DISCREPANCY",
				"detail": err.Error(),
				"record": rec,
			})
			return
		}
		http.Error(w, `{"error":"CEP_FETCH_FAILED"}`, http.StatusBadGateway)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(rec)
}
