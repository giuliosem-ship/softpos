package api

import (
	"encoding/json"
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
	ClaveRastreo          string `json:"clave_rastreo"`
	Status                string `json:"status"` // "DISPATCHED"
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

	// 4. Double-Entry Posting: Debit Merchant Available, Credit STP Concentrator
	entries := []ledger.Entry{
		{AccountID: accountID, Direction: ledger.Debit, Money: ledger.NewMoney(req.AmountCents, "MXN")},
		{AccountID: "ASSET:STP_CONCENTRATOR", Direction: ledger.Credit, Money: ledger.NewMoney(req.AmountCents, "MXN")},
	}

	claveRastreo := fmt.Sprintf("SPEI%d%s", time.Now().UnixNano(), req.MerchantID)
	journal, err := ledger.NewJournalEntry(
		fmt.Sprintf("disb_%s", claveRastreo),
		time.Now(),
		fmt.Sprintf("Disbursement SPEI to %s", req.DestinationCLABE),
		entries,
		nil,
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

	resp := DisbursementResponse{
		ClaveRastreo:          claveRastreo,
		Status:                "DISPATCHED",
		AmountCents:           req.AmountCents,
		RemainingBalanceCents: remainingBal.AmountCents,
		Timestamp:             time.Now().UTC().Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}
