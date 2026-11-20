package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"softpos/pkg/auth"
	"softpos/pkg/ledger"
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
	Status                string `json:"status"`     // "DISPATCHED"
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
	claveRastreo := fmt.Sprintf("SPEI%d", now.UnixNano())
	beneficiary := req.BeneficiaryName
	if beneficiary == "" {
		beneficiary = merchant.Name
	}
	accountID := fmt.Sprintf("LIABILITY:MERCHANT:%s:AVAILABLE", tenant)
	debit := func(journalID, description string, amount int64, fromAvailable bool) *ledger.JournalEntry {
		avail := ledger.Entry{AccountID: accountID, Direction: ledger.Debit, Money: ledger.NewMoney(amount, "MXN")}
		conc := ledger.Entry{AccountID: "ASSET:STP_CONCENTRATOR", Direction: ledger.Credit, Money: ledger.NewMoney(amount, "MXN")}
		if !fromAvailable { // compensation: put the money back
			avail.Direction, conc.Direction = ledger.Credit, ledger.Debit
		}
		j, _ := ledger.NewJournalEntry(journalID, now, description, []ledger.Entry{avail, conc},
			map[string]string{"clave_rastreo": claveRastreo, "merchant_id": tenant})
		return j
	}

	// 3. Balance check + debit under the ledger row lock, in one transaction
	// (two concurrent payouts cannot both spend the same balance). Funds are
	// reserved before calling STP and returned if the dispatch fails.
	var remaining int64
	err = s.moneyStore().InTx(ctx, func(tx store.MoneyTx) error {
		bals, err := tx.LockAccountBalances(ctx, accountID, "ASSET:STP_CONCENTRATOR")
		if err != nil {
			return err
		}
		if bals[accountID] < req.AmountCents {
			return errInsufficientFunds
		}
		if err := tx.PostJournal(ctx, debit("disb_"+claveRastreo, "Disbursement SPEI to "+acct.CLABE, req.AmountCents, true)); err != nil {
			return err
		}
		remaining = bals[accountID] - req.AmountCents
		return tx.EnqueueEvent(ctx, "disbursement.requested", claveRastreo, map[string]any{
			"clave_rastreo": claveRastreo, "merchant_id": tenant, "amount_cents": req.AmountCents, "destination_clabe": acct.CLABE,
		})
	})
	if err != nil {
		writeMoneyError(w, "disbursement", err)
		return
	}

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
		FechaOperacion:      now.Format("20060102"),
		AmountCents:         req.AmountCents,
		NombreBeneficiario:  beneficiary,
		CuentaBeneficiario:  acct.CLABE,
		RfcCurpBeneficiario: merchant.RFC,
		ConceptoPago:        req.Concept,
	})
	if err != nil && dispatchOutcomeUnknown(err) {
		// The order may have been registered (timeout, reset, 5xx): returning
		// the funds could pay the merchant twice. Keep the reservation and
		// leave it to CEP/STP reconciliation.
		cctx := context.WithoutCancel(ctx)
		if eerr := s.moneyStore().InTx(cctx, func(tx store.MoneyTx) error {
			return tx.EnqueueEvent(cctx, "disbursement.dispatch_unknown", claveRastreo, map[string]any{
				"clave_rastreo": claveRastreo, "merchant_id": tenant, "amount_cents": req.AmountCents, "error": err.Error(),
			})
		}); eerr != nil {
			slog.Error("disbursement dispatch outcome unknown and event not recorded", "clave_rastreo", claveRastreo, "error", eerr)
		}
		slog.Error("disbursement dispatch outcome unknown; funds stay reserved pending reconciliation", "clave_rastreo", claveRastreo, "error", err)
		s.cepReconciler.TrackDisbursement(stp.DisbursementRecord{
			ClaveRastreo:          claveRastreo,
			MerchantID:            tenant,
			AmountCents:           req.AmountCents,
			DestinationCLABE:      acct.CLABE,
			BeneficiaryName:       beneficiary,
			Concept:               req.Concept,
			FechaOperacion:        now.Format("20060102"),
			Status:                stp.StatusPendingCEP,
			RemainingBalanceCents: remaining,
			CreatedAt:             now,
		})
		writeJSON(w, http.StatusAccepted, map[string]any{
			"status":        "PENDING_RECONCILIATION",
			"clave_rastreo": claveRastreo,
			"amount_cents":  req.AmountCents,
		})
		return
	}
	if err != nil {
		// Definitive rejection (STP answered, or the order never left):
		// compensate the reservation; if even that fails the journal stays and
		// must be resolved by reconciliation (logged loudly).
		cctx := context.WithoutCancel(ctx)
		dispatchErr := err
		cerr := s.moneyStore().InTx(cctx, func(tx store.MoneyTx) error {
			if err := tx.PostJournal(cctx, debit("disb_rev_"+claveRastreo, "Disbursement dispatch failed "+claveRastreo, req.AmountCents, false)); err != nil {
				return err
			}
			return tx.EnqueueEvent(cctx, "disbursement.failed", claveRastreo, map[string]any{
				"clave_rastreo": claveRastreo, "merchant_id": tenant, "amount_cents": req.AmountCents, "error": dispatchErr.Error(),
			})
		})
		if cerr != nil {
			slog.Error("disbursement compensation failed; manual reconciliation required", "clave_rastreo", claveRastreo, "error", cerr)
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{
			"error":  "STP_DISPATCH_FAILED",
			"detail": err.Error(),
		})
		return
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
		FechaOperacion:        now.Format("20060102"),
		Status:                stp.StatusPendingCEP,
		RemainingBalanceCents: remaining,
		CreatedAt:             now,
	})

	writeJSON(w, http.StatusOK, DisbursementResponse{
		STPOrderID:            stpResp.STPOrderID,
		ClaveRastreo:          claveRastreo,
		Status:                "DISPATCHED",
		CEPStatus:             string(stp.StatusPendingCEP),
		AmountCents:           req.AmountCents,
		DestinationCLABE:      acct.CLABE,
		RemainingBalanceCents: remaining,
		Timestamp:             now.Format(time.RFC3339),
	})
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
