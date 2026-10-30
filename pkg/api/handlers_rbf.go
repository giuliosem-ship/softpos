package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"softpos/pkg/rbf"
)

type OriginateRBFLoanRequest struct {
	MerchantID     string `json:"merchant_id"`
	PrincipalCents int64  `json:"principal_cents"`
	FixedFeeBps    int64  `json:"fixed_fee_bps"`
	HoldbackBps    int64  `json:"holdback_bps"`
}

type RBFLoanResponse struct {
	LoanID            string         `json:"loan_id"`
	MerchantID        string         `json:"merchant_id"`
	PrincipalCents    int64          `json:"principal_cents"`
	FixedFeeBps       int64          `json:"fixed_fee_bps"`
	FeeCents          int64          `json:"fee_cents"`
	RepaymentCapCents int64          `json:"repayment_cap_cents"`
	HoldbackBps       int64          `json:"holdback_bps"`
	RepaidCents       int64          `json:"repaid_cents"`
	RemainingCents    int64          `json:"remaining_cents"`
	Status            rbf.LoanStatus `json:"status"`
}

func toRBFLoanResponse(loan *rbf.RBFLoan) RBFLoanResponse {
	return RBFLoanResponse{
		LoanID:            loan.ID,
		MerchantID:        loan.MerchantID,
		PrincipalCents:    loan.PrincipalCents,
		FixedFeeBps:       loan.FixedFeeBps,
		FeeCents:          loan.FeeCents,
		RepaymentCapCents: loan.RepaymentCapCents,
		HoldbackBps:       loan.HoldbackBps,
		RepaidCents:       loan.RepaidCents,
		RemainingCents:    loan.RemainingCents(),
		Status:            loan.Status,
	}
}

func (s *Server) HandleOriginateRBFLoan(w http.ResponseWriter, r *http.Request) {
	var req OriginateRBFLoanRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"INVALID_JSON"}`, http.StatusBadRequest)
		return
	}

	if _, exists := s.GetMerchant(req.MerchantID); !exists {
		http.Error(w, `{"error":"MERCHANT_NOT_FOUND"}`, http.StatusNotFound)
		return
	}

	loan, err := s.rbfEngine.OriginateLoan(req.MerchantID, req.PrincipalCents, req.FixedFeeBps, req.HoldbackBps)
	if err != nil {
		if errors.Is(err, rbf.ErrActiveLoanExists) {
			http.Error(w, `{"error":"ACTIVE_LOAN_EXISTS"}`, http.StatusConflict)
			return
		}
		http.Error(w, `{"error":"INVALID_LOAN_PARAMETERS"}`, http.StatusBadRequest)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(toRBFLoanResponse(loan))
}

func (s *Server) HandleGetMerchantRBFLoan(w http.ResponseWriter, r *http.Request) {
	merchantID := r.PathValue("id")
	if merchantID == "" {
		http.Error(w, `{"error":"MISSING_MERCHANT_ID"}`, http.StatusBadRequest)
		return
	}

	loan, err := s.rbfEngine.GetMerchantLoan(merchantID)
	if err != nil {
		http.Error(w, `{"error":"RBF_LOAN_NOT_FOUND"}`, http.StatusNotFound)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(toRBFLoanResponse(loan))
}
