package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"softpos/pkg/fiscal"
	"softpos/pkg/payments"
)

type SettlePaymentRequest struct {
	PaymentID        string `json:"payment_id"`
	MerchantID       string `json:"merchant_id"`
	GrossAmountCents int64  `json:"gross_amount_cents"`
	CustomerRFC      string `json:"customer_rfc"`
	IsPersonaMoral   bool   `json:"is_persona_moral"`
	PlatformFeeCents int64  `json:"platform_fee_cents"`
	RBFSweepCents    int64  `json:"rbf_sweep_cents"`
}

type SettlePaymentResponse struct {
	PaymentID                 string  `json:"payment_id"`
	GrossAmountCents          int64   `json:"gross_amount_cents"`
	BaseAmountCents           int64   `json:"base_amount_cents"`
	IVAAmountCents            int64   `json:"iva_amount_cents"`
	ISRRate                   float64 `json:"isr_rate"`
	ISREscrowCents            int64   `json:"isr_escrow_cents"`
	TaxEscrowCents            int64   `json:"tax_escrow_cents"`
	PlatformFeeCents          int64   `json:"platform_fee_cents"`
	RBFSweepCents             int64   `json:"rbf_sweep_cents"`
	NetAvailableCents         int64   `json:"net_available_cents"`
	IsPersonaMoralWithholding bool    `json:"is_persona_moral_withholding"`
}

func (s *Server) HandlePaymentSettle(w http.ResponseWriter, r *http.Request) {
	var req SettlePaymentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"INVALID_JSON"}`, http.StatusBadRequest)
		return
	}

	tenant, ok := requireTenant(w, r, req.MerchantID)
	if !ok {
		return
	}
	req.MerchantID = tenant

	if req.GrossAmountCents <= 0 {
		http.Error(w, `{"error":"INVALID_AMOUNT"}`, http.StatusBadRequest)
		return
	}

	// Idempotency check (scoped by tenant)
	idempKey := r.Header.Get("Idempotency-Key")
	if idempKey == "" {
		idempKey = "pay:settle:" + req.PaymentID
	}
	idempKey = tenantIdempotencyKey(tenant, idempKey)
	if rec, exists := s.idempotency.Get(idempKey); exists {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rec.StatusCode)
		_, _ = w.Write(rec.Body)
		return
	}

	// If payment was previously authorized in EMV PaymentEngine, verify it can transition to SETTLED
	if existingPayment, err := s.paymentEngine.GetPayment(req.PaymentID); err == nil && existingPayment != nil {
		if existingPayment.MerchantID != tenant {
			http.Error(w, `{"error":"PAYMENT_NOT_FOUND"}`, http.StatusNotFound)
			return
		}
		if existingPayment.Status != payments.PaymentAuthorized {
			http.Error(w, `{"error":"INVALID_PAYMENT_STATE"}`, http.StatusConflict)
			return
		}
	}

	// 1. Query merchant's accumulated monthly base sales and calculate RESICO Tax Split
	accumulatedMonthlySalesCents, err := s.merchantMonthlySalesCents(req.MerchantID)
	if err != nil {
		http.Error(w, `{"error":"MONTHLY_SALES_UNAVAILABLE"}`, http.StatusServiceUnavailable)
		return
	}
	taxSplit, err := fiscal.CalculateResicoTaxSplit(req.GrossAmountCents, accumulatedMonthlySalesCents, req.IsPersonaMoral)
	if err != nil {
		http.Error(w, `{"error":"TAX_CALCULATION_ERROR"}`, http.StatusBadRequest)
		return
	}

	// 2. Determine RBF Sweep Cents (automatic holdback from active RBF loan if not explicitly set, or capped by active loan)
	rbfSweepCents := req.RBFSweepCents
	maxDeductible := req.GrossAmountCents - req.PlatformFeeCents - taxSplit.TotalTaxEscrowCents
	hasActiveLoan := false
	if loan, err := s.rbfEngine.GetMerchantLoan(req.MerchantID); err == nil && loan != nil && loan.RemainingCents() > 0 {
		hasActiveLoan = true
		if rbfSweepCents == 0 {
			rbfSweepCents = s.rbfEngine.CalculateSweep(req.MerchantID, req.GrossAmountCents, maxDeductible)
		} else if rbfSweepCents > loan.RemainingCents() {
			rbfSweepCents = loan.RemainingCents()
		}
	}

	// 3. Post atomic Double-Entry Ledger Split
	_, err = s.ledger.RecordSplitPayment(
		req.GrossAmountCents,
		req.MerchantID,
		req.PlatformFeeCents,
		taxSplit.TotalTaxEscrowCents,
		rbfSweepCents,
	)
	if err != nil {
		http.Error(w, `{"error":"LEDGER_SPLIT_FAILED"}`, http.StatusInternalServerError)
		return
	}

	// 4. Transition EMV payment state if tracked and apply RBF loan repayment
	_, err = s.paymentEngine.Settle(req.PaymentID)
	if err != nil && !errors.Is(err, payments.ErrPaymentNotFound) {
		http.Error(w, `{"error":"PAYMENT_STATE_TRANSITION_FAILED"}`, http.StatusConflict)
		return
	}

	if hasActiveLoan && rbfSweepCents > 0 {
		_, _ = s.rbfEngine.ApplyRepayment(req.MerchantID, rbfSweepCents)
	}

	// 5. Atomically record the base amount into the merchant's monthly accumulated sales
	_ = s.AddMerchantMonthlySalesCents(req.MerchantID, taxSplit.BaseAmountCents) // logged on failure

	netAvailable := req.GrossAmountCents - (req.PlatformFeeCents + taxSplit.TotalTaxEscrowCents + rbfSweepCents)

	resp := SettlePaymentResponse{
		PaymentID:                 req.PaymentID,
		GrossAmountCents:          req.GrossAmountCents,
		BaseAmountCents:           taxSplit.BaseAmountCents,
		IVAAmountCents:            taxSplit.IVAAmountCents,
		ISRRate:                   taxSplit.ISRRate,
		ISREscrowCents:            taxSplit.ISREscrowCents,
		TaxEscrowCents:            taxSplit.TotalTaxEscrowCents,
		PlatformFeeCents:          req.PlatformFeeCents,
		RBFSweepCents:             rbfSweepCents,
		NetAvailableCents:         netAvailable,
		IsPersonaMoralWithholding: taxSplit.IsPersonaMoralWithholding,
	}

	respBytes, _ := json.Marshal(resp)
	s.idempotency.Set(idempKey, http.StatusOK, respBytes, 24*time.Hour)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(respBytes)
}
