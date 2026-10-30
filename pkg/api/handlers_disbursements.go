package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"softpos/pkg/ledger"
	"softpos/pkg/stp"
)

type DisbursementRequest struct {
	MerchantID       string `json:"merchant_id"`
	AmountCents      int64  `json:"amount_cents"`
	DestinationCLABE string `json:"destination_clabe"`
	BeneficiaryName  string `json:"beneficiary_name"`
	Concept          string `json:"concept"`
}

type DisbursementResponse struct {
	STPOrderID            int64  `json:"stp_order_id"`
	ClaveRastreo          string `json:"clave_rastreo"`
	Status                string `json:"status"`     // "DISPATCHED"
	CEPStatus             string `json:"cep_status"` // "PENDING_CEP"
	AmountCents           int64  `json:"amount_cents"`
	RemainingBalanceCents int64  `json:"remaining_balance_cents"`
	Timestamp             string `json:"timestamp"`
}

func (s *Server) HandleDisbursement(w http.ResponseWriter, r *http.Request) {
	var req DisbursementRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"INVALID_JSON"}`, http.StatusBadRequest)
		return
	}

	if req.AmountCents <= 0 {
		http.Error(w, `{"error":"INVALID_AMOUNT"}`, http.StatusBadRequest)
		return
	}

	// 1. Validate Destination CLABE
	if !stp.ValidateCLABE(req.DestinationCLABE) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "INVALID_DESTINATION_CLABE",
		})
		return
	}

	// 2. Regulatory Compliance Screen (SAT Article 69-B EFOS)
	merchant, exists := s.GetMerchant(req.MerchantID)
	if !exists {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "MERCHANT_NOT_FOUND",
		})
		return
	}

	_, err := s.blacklist.ValidateMerchantEligibility(merchant.RFC)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error":  "SAT_69B_SANCTION_HOLD",
			"reason": "Merchant RFC is blacklisted under SAT Article 69-B",
		})
		return
	}

	// 3. Balance verification
	accountID := fmt.Sprintf("LIABILITY:MERCHANT:%s:AVAILABLE", req.MerchantID)
	balance, err := s.ledger.GetBalance(accountID)
	if err != nil || balance.AmountCents < req.AmountCents {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error": "INSUFFICIENT_FUNDS",
		})
		return
	}

	now := time.Now().UTC()
	claveRastreo := fmt.Sprintf("SPEI%d", now.UnixNano())

	// 4. Sign and dispatch outbound order via STP registraOrden client
	s.mu.RLock()
	stpClient := s.stpClient
	s.mu.RUnlock()

	stpResp, err := stpClient.RegisterOrder(r.Context(), stp.STPOrderRequest{
		ClaveRastreo:        claveRastreo,
		FechaOperacion:      now.Format("20060102"),
		AmountCents:         req.AmountCents,
		NombreBeneficiario:  req.BeneficiaryName,
		CuentaBeneficiario:  req.DestinationCLABE,
		RfcCurpBeneficiario: merchant.RFC,
		ConceptoPago:        req.Concept,
	})
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"error":  "STP_DISPATCH_FAILED",
			"detail": err.Error(),
		})
		return
	}

	// 5. Double-Entry Posting: Debit Merchant Available, Credit STP Concentrator
	entries := []ledger.Entry{
		{AccountID: accountID, Direction: ledger.Debit, Money: ledger.NewMoney(req.AmountCents, "MXN")},
		{AccountID: "ASSET:STP_CONCENTRATOR", Direction: ledger.Credit, Money: ledger.NewMoney(req.AmountCents, "MXN")},
	}

	journal, err := ledger.NewJournalEntry(
		fmt.Sprintf("disb_%s", claveRastreo),
		now,
		fmt.Sprintf("Disbursement SPEI to %s", req.DestinationCLABE),
		entries,
		map[string]string{
			"clave_rastreo": claveRastreo,
			"stp_order_id":  fmt.Sprintf("%d", stpResp.STPOrderID),
		},
	)
	if err != nil {
		http.Error(w, `{"error":"LEDGER_JOURNAL_ERROR"}`, http.StatusInternalServerError)
		return
	}

	if err := s.ledger.PostTransaction(journal); err != nil {
		http.Error(w, `{"error":"LEDGER_POSTING_FAILED"}`, http.StatusInternalServerError)
		return
	}

	remainingBal, _ := s.ledger.GetBalance(accountID)

	// 6. Track disbursement for automatic Banxico CEP reconciliation
	s.cepReconciler.TrackDisbursement(stp.DisbursementRecord{
		ClaveRastreo:          claveRastreo,
		STPOrderID:            stpResp.STPOrderID,
		MerchantID:            req.MerchantID,
		AmountCents:           req.AmountCents,
		DestinationCLABE:      req.DestinationCLABE,
		BeneficiaryName:       req.BeneficiaryName,
		Concept:               req.Concept,
		FechaOperacion:        now.Format("20060102"),
		Status:                stp.StatusPendingCEP,
		RemainingBalanceCents: remainingBal.AmountCents,
		CreatedAt:             now,
	})

	resp := DisbursementResponse{
		STPOrderID:            stpResp.STPOrderID,
		ClaveRastreo:          claveRastreo,
		Status:                "DISPATCHED",
		CEPStatus:             string(stp.StatusPendingCEP),
		AmountCents:           req.AmountCents,
		RemainingBalanceCents: remainingBal.AmountCents,
		Timestamp:             now.Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) HandleGetDisbursement(w http.ResponseWriter, r *http.Request) {
	clave := r.PathValue("clave")
	if clave == "" {
		http.Error(w, `{"error":"MISSING_CLAVE_RASTREO"}`, http.StatusBadRequest)
		return
	}

	rec, err := s.cepReconciler.GetDisbursement(clave)
	if err != nil {
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
