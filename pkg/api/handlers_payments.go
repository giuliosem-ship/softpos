package api

import (
	"encoding/json"
	"net/http"
	"time"

	"softpos/pkg/fiscal"
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

	if req.GrossAmountCents <= 0 {
		http.Error(w, `{"error":"INVALID_AMOUNT"}`, http.StatusBadRequest)
		return
	}

	// Idempotency check
	idempKey := r.Header.Get("Idempotency-Key")
	if idempKey == "" {
		idempKey = "pay:settle:" + req.PaymentID
	}
	if rec, exists := s.idempotency.Get(idempKey); exists {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rec.StatusCode)
		_, _ = w.Write(rec.Body)
		return
	}

	// 1. Calculate RESICO Tax Split
	taxSplit, err := fiscal.CalculateResicoTaxSplit(req.GrossAmountCents, 0, req.IsPersonaMoral)
	if err != nil {
		http.Error(w, `{"error":"TAX_CALCULATION_ERROR"}`, http.StatusBadRequest)
		return
	}

	// 2. Post atomic Double-Entry Ledger Split
	_, err = s.ledger.RecordSplitPayment(
		req.GrossAmountCents,
		req.MerchantID,
		req.PlatformFeeCents,
		taxSplit.TotalTaxEscrowCents,
		req.RBFSweepCents,
	)
	if err != nil {
		http.Error(w, `{"error":"LEDGER_SPLIT_FAILED"}`, http.StatusInternalServerError)
		return
	}

	netAvailable := req.GrossAmountCents - (req.PlatformFeeCents + taxSplit.TotalTaxEscrowCents + req.RBFSweepCents)

	resp := SettlePaymentResponse{
		PaymentID:                 req.PaymentID,
		GrossAmountCents:          req.GrossAmountCents,
		BaseAmountCents:           taxSplit.BaseAmountCents,
		IVAAmountCents:            taxSplit.IVAAmountCents,
		ISRRate:                   taxSplit.ISRRate,
		ISREscrowCents:            taxSplit.ISREscrowCents,
		TaxEscrowCents:            taxSplit.TotalTaxEscrowCents,
		PlatformFeeCents:          req.PlatformFeeCents,
		RBFSweepCents:             req.RBFSweepCents,
		NetAvailableCents:         netAvailable,
		IsPersonaMoralWithholding: taxSplit.IsPersonaMoralWithholding,
	}

	respBytes, _ := json.Marshal(resp)
	s.idempotency.Set(idempKey, http.StatusOK, respBytes, 24*time.Hour)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(respBytes)
}
