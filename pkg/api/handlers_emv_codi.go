package api

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"softpos/pkg/fiscal"
	"softpos/pkg/ledger"
	"softpos/pkg/payments"
)

type EMVAuthorizeRequest struct {
	PaymentID  string `json:"payment_id"`
	MerchantID string `json:"merchant_id"`
	TLVHex     string `json:"tlv_hex"`
}

type EMVAuthorizeResponse struct {
	PaymentID      string                  `json:"payment_id"`
	MerchantID     string                  `json:"merchant_id"`
	AmountCents    int64                   `json:"amount_cents"`
	CurrencyCode   string                  `json:"currency_code"`
	CryptogramType payments.CryptogramType `json:"cryptogram_type"`
	CryptogramHex  string                  `json:"cryptogram_hex"`
	ATC            int                     `json:"atc"`
	MaskedPAN      string                  `json:"masked_pan"`
	Status         payments.PaymentStatus  `json:"status"`
	CreatedAt      string                  `json:"created_at"`
}

func (s *Server) HandleEMVAuthorize(w http.ResponseWriter, r *http.Request) {
	var req EMVAuthorizeRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"INVALID_JSON"}`, http.StatusBadRequest)
		return
	}
	if req.PaymentID == "" || req.MerchantID == "" || req.TLVHex == "" {
		http.Error(w, `{"error":"MISSING_REQUIRED_FIELDS"}`, http.StatusBadRequest)
		return
	}

	idempKey := r.Header.Get("Idempotency-Key")
	if idempKey == "" {
		idempKey = "emv:auth:" + req.PaymentID
	}
	if rec, exists := s.idempotency.Get(idempKey); exists {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rec.StatusCode)
		_, _ = w.Write(rec.Body)
		return
	}

	rawTLV, err := hex.DecodeString(req.TLVHex)
	if err != nil {
		http.Error(w, `{"error":"INVALID_TLV_HEX"}`, http.StatusBadRequest)
		return
	}

	emvTags, err := payments.ParseEMVTags(rawTLV)
	if err != nil {
		status := http.StatusUnprocessableEntity
		errCode := "EMV_VALIDATION_FAILED"
		switch {
		case errors.Is(err, payments.ErrInvalidTLV):
			status = http.StatusBadRequest
			errCode = "INVALID_EMV_TLV"
		case errors.Is(err, payments.ErrUnsupportedCryptogram):
			errCode = "CRYPTOGRAM_DECLINED"
		case errors.Is(err, payments.ErrExpiredCard):
			errCode = "EXPIRED_CARD"
		case errors.Is(err, payments.ErrInvalidCurrency):
			errCode = "INVALID_CURRENCY"
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": errCode})
		return
	}

	payment, err := s.paymentEngine.Authorize(req.PaymentID, req.MerchantID, emvTags)
	if err != nil {
		http.Error(w, `{"error":"AUTHORIZATION_FAILED"}`, http.StatusConflict)
		return
	}

	resp := EMVAuthorizeResponse{
		PaymentID:      payment.ID,
		MerchantID:     payment.MerchantID,
		AmountCents:    payment.AmountCents,
		CurrencyCode:   emvTags.CurrencyCode,
		CryptogramType: emvTags.CryptogramType,
		CryptogramHex:  emvTags.CryptogramHex,
		ATC:            emvTags.ATC,
		MaskedPAN:      payment.MaskedPAN,
		Status:         payment.Status,
		CreatedAt:      payment.CreatedAt.Format(time.RFC3339),
	}

	respBytes, _ := json.Marshal(resp)
	s.idempotency.Set(idempKey, http.StatusOK, respBytes, 24*time.Hour)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(respBytes)
}

type EMVReverseRequest struct {
	PaymentID   string `json:"payment_id"`
	RefundCents int64  `json:"refund_cents,omitempty"`
}

type EMVReverseResponse struct {
	PaymentID   string                 `json:"payment_id"`
	MerchantID  string                 `json:"merchant_id"`
	AmountCents int64                  `json:"amount_cents"`
	Status      payments.PaymentStatus `json:"status"`
	UpdatedAt   string                 `json:"updated_at"`
}

func (s *Server) HandleEMVReverse(w http.ResponseWriter, r *http.Request) {
	var req EMVReverseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"INVALID_JSON"}`, http.StatusBadRequest)
		return
	}
	if req.PaymentID == "" {
		http.Error(w, `{"error":"MISSING_PAYMENT_ID"}`, http.StatusBadRequest)
		return
	}

	existing, err := s.paymentEngine.GetPayment(req.PaymentID)
	if err != nil {
		http.Error(w, `{"error":"PAYMENT_NOT_FOUND"}`, http.StatusNotFound)
		return
	}

	var updated *payments.Payment
	switch existing.Status {
	case payments.PaymentAuthorized:
		updated, err = s.paymentEngine.Void(req.PaymentID)
		if err != nil {
			http.Error(w, `{"error":"VOID_FAILED"}`, http.StatusConflict)
			return
		}
	case payments.PaymentSettled:
		refundAmt := req.RefundCents
		if refundAmt == 0 {
			refundAmt = existing.AmountCents
		}
		updated, err = s.paymentEngine.Refund(req.PaymentID, refundAmt)
		if err != nil {
			http.Error(w, `{"error":"REFUND_FAILED"}`, http.StatusBadRequest)
			return
		}

		// Post reversing journal entry on merchant available & acquirer receivable if accounts exist
		availCode := fmt.Sprintf("LIABILITY:MERCHANT:%s:AVAILABLE", existing.MerchantID)
		entries := []ledger.Entry{
			{AccountID: availCode, Direction: ledger.Debit, Money: ledger.NewMoney(refundAmt, "MXN")},
			{AccountID: "ASSET:ACQUIRER_RECEIVABLE", Direction: ledger.Credit, Money: ledger.NewMoney(refundAmt, "MXN")},
		}
		if jnl, jErr := ledger.NewJournalEntry(
			fmt.Sprintf("refund_%s_%d", req.PaymentID, time.Now().UnixNano()),
			time.Now().UTC(),
			fmt.Sprintf("EMV Refund %s", req.PaymentID),
			entries,
			nil,
		); jErr == nil {
			_ = s.ledger.PostTransaction(jnl)
		}
	default:
		http.Error(w, `{"error":"INVALID_PAYMENT_STATE"}`, http.StatusConflict)
		return
	}

	resp := EMVReverseResponse{
		PaymentID:   updated.ID,
		MerchantID:  updated.MerchantID,
		AmountCents: updated.AmountCents,
		Status:      updated.Status,
		UpdatedAt:   updated.UpdatedAt.Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

type CoDiCreateQRRequest struct {
	MerchantID  string `json:"merchant_id"`
	SellerCLABE string `json:"seller_clabe,omitempty"`
	AmountCents int64  `json:"amount_cents"`
	Concept     string `json:"concept"`
	Reference   string `json:"reference"`
	TTLSeconds  int64  `json:"ttl_seconds,omitempty"`
}

type CoDiCreateQRResponse struct {
	QRPayload   string `json:"qr_payload"`
	MerchantID  string `json:"merchant_id"`
	SellerCLABE string `json:"seller_clabe"`
	AmountCents int64  `json:"amount_cents"`
	Reference   string `json:"reference"`
	ExpiresAt   string `json:"expires_at"`
}

func (s *Server) HandleCoDiCreateQR(w http.ResponseWriter, r *http.Request) {
	var req CoDiCreateQRRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"INVALID_JSON"}`, http.StatusBadRequest)
		return
	}

	clabe := req.SellerCLABE
	if clabe == "" && req.MerchantID != "" {
		clabe, _ = s.GetMerchantCLABE(req.MerchantID)
	}

	ttl := 15 * time.Minute
	if req.TTLSeconds > 0 {
		ttl = time.Duration(req.TTLSeconds) * time.Second
	}

	qrPayload, err := s.codiService.CreatePaymentRequest(clabe, req.AmountCents, req.Concept, req.Reference, ttl)
	if err != nil {
		http.Error(w, `{"error":"INVALID_CODI_REQUEST"}`, http.StatusBadRequest)
		return
	}

	resp := CoDiCreateQRResponse{
		QRPayload:   qrPayload,
		MerchantID:  req.MerchantID,
		SellerCLABE: clabe,
		AmountCents: req.AmountCents,
		Reference:   req.Reference,
		ExpiresAt:   time.Now().UTC().Add(ttl).Format(time.RFC3339),
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(resp)
}

type CoDiWebhookResponse struct {
	Status            string `json:"status"`
	TrackingKey       string `json:"tracking_key"`
	MerchantID        string `json:"merchant_id"`
	GrossAmountCents  int64  `json:"gross_amount_cents"`
	TaxEscrowCents    int64  `json:"tax_escrow_cents"`
	RBFSweepCents     int64  `json:"rbf_sweep_cents"`
	NetAvailableCents int64  `json:"net_available_cents"`
}

func (s *Server) HandleCoDiWebhookConfirm(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, `{"error":"INVALID_BODY"}`, http.StatusBadRequest)
		return
	}

	conf, err := payments.ValidateCoDiConfirmation(bodyBytes)
	if err != nil {
		http.Error(w, `{"error":"INVALID_CODI_CONFIRMATION"}`, http.StatusBadRequest)
		return
	}

	idempKey := "codi:confirm:" + conf.TrackingKey
	if rec, exists := s.idempotency.Get(idempKey); exists {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(rec.StatusCode)
		_, _ = w.Write(rec.Body)
		return
	}

	merchant, exists := s.GetMerchantByCLABE(conf.SellerCLABE)
	if !exists {
		http.Error(w, `{"error":"UNKNOWN_SELLER_CLABE"}`, http.StatusNotFound)
		return
	}

	if err := s.codiService.ConfirmPayment(conf); err != nil {
		http.Error(w, `{"error":"CODI_CONFIRM_FAILED"}`, http.StatusBadRequest)
		return
	}

	// Calculate RESICO tax split (0 platform fee on Banxico CoDi QR)
	accSales := s.GetMerchantMonthlySalesCents(merchant.ID)
	taxSplit, err := fiscal.CalculateResicoTaxSplit(conf.AmountCents, accSales, false)
	if err != nil {
		http.Error(w, `{"error":"TAX_SPLIT_FAILED"}`, http.StatusInternalServerError)
		return
	}

	maxDeductible := conf.AmountCents - taxSplit.TotalTaxEscrowCents
	rbfSweepCents := s.rbfEngine.CalculateSweep(merchant.ID, conf.AmountCents, maxDeductible)

	if _, err := s.ledger.RecordSplitPayment(conf.AmountCents, merchant.ID, 0, taxSplit.TotalTaxEscrowCents, rbfSweepCents); err != nil {
		http.Error(w, `{"error":"LEDGER_SPLIT_FAILED"}`, http.StatusInternalServerError)
		return
	}

	if rbfSweepCents > 0 {
		_, _ = s.rbfEngine.ApplyRepayment(merchant.ID, rbfSweepCents)
	}
	s.AddMerchantMonthlySalesCents(merchant.ID, taxSplit.BaseAmountCents)

	netAvailable := conf.AmountCents - taxSplit.TotalTaxEscrowCents - rbfSweepCents
	resp := CoDiWebhookResponse{
		Status:            "CONFIRMED",
		TrackingKey:       conf.TrackingKey,
		MerchantID:        merchant.ID,
		GrossAmountCents:  conf.AmountCents,
		TaxEscrowCents:    taxSplit.TotalTaxEscrowCents,
		RBFSweepCents:     rbfSweepCents,
		NetAvailableCents: netAvailable,
	}

	respBytes, _ := json.Marshal(resp)
	s.idempotency.Set(idempKey, http.StatusOK, respBytes, 24*time.Hour)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(respBytes)
}
