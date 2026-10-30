package api

import (
	"encoding/json"
	"fmt"
	"net/http"
)

type MerchantBalancesResponse struct {
	MerchantID     string `json:"merchant_id"`
	AvailableCents int64  `json:"available_cents"`
	TaxEscrowCents int64  `json:"tax_escrow_cents"`
	RBFDebtCents   int64  `json:"rbf_debt_cents"`
	Currency       string `json:"currency"`
}

func (s *Server) HandleGetMerchantBalances(w http.ResponseWriter, r *http.Request) {
	merchantID := r.PathValue("id")
	if merchantID == "" {
		http.Error(w, `{"error":"MISSING_MERCHANT_ID"}`, http.StatusBadRequest)
		return
	}

	availBal, _ := s.ledger.GetBalance(fmt.Sprintf("LIABILITY:MERCHANT:%s:AVAILABLE", merchantID))
	escrowBal, _ := s.ledger.GetBalance(fmt.Sprintf("LIABILITY:MERCHANT:%s:TAX_ESCROW", merchantID))
	rbfBal, _ := s.ledger.GetBalance(fmt.Sprintf("ASSET:RBF_LOANS_RECEIVABLE:%s", merchantID))

	resp := MerchantBalancesResponse{
		MerchantID:     merchantID,
		AvailableCents: availBal.AmountCents,
		TaxEscrowCents: escrowBal.AmountCents,
		RBFDebtCents:   rbfBal.AmountCents,
		Currency:       "MXN",
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}
