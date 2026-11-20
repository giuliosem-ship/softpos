package api

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"softpos/pkg/fiscal"
	"softpos/pkg/ledger"
	"softpos/pkg/payments"
	"softpos/pkg/store"
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
	tenant, ok := requireTenant(w, r, req.MerchantID)
	if !ok {
		return
	}
	req.MerchantID = tenant
	if req.PaymentID == "" || req.TLVHex == "" {
		http.Error(w, `{"error":"MISSING_REQUIRED_FIELDS"}`, http.StatusBadRequest)
		return
	}

	idempKey := r.Header.Get("Idempotency-Key")
	if idempKey == "" {
		idempKey = "emv:auth:" + req.PaymentID
	}
	idempKey = tenantIdempotencyKey(tenant, idempKey)
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

	tenant, ok := requireTenant(w, r, "")
	if !ok {
		return
	}
	ctx := r.Context()
	var updated *payments.Payment
	err := s.moneyStore().InTx(ctx, func(tx store.MoneyTx) error {
		p, err := tx.GetPaymentForUpdate(ctx, req.PaymentID)
		if err != nil {
			return err
		}
		if p.MerchantID != tenant {
			return payments.ErrPaymentNotFound
		}
		switch p.Status {
		case payments.PaymentAuthorized:
			if err := tx.SetPaymentStatus(ctx, p.ID, payments.PaymentAuthorized, payments.PaymentVoided); err != nil {
				return err
			}
			p.Status = payments.PaymentVoided
			updated = p
			return tx.EnqueueEvent(ctx, "payment.voided", p.ID, map[string]any{"payment_id": p.ID, "merchant_id": tenant})
		case payments.PaymentSettled:
			refundAmt := req.RefundCents
			if refundAmt == 0 {
				refundAmt = p.AmountCents
			}
			if refundAmt <= 0 || refundAmt > p.AmountCents {
				return errRefundExceedsBalance
			}
			availCode := fmt.Sprintf("LIABILITY:MERCHANT:%s:AVAILABLE", tenant)
			jnl, err := ledger.NewJournalEntry("refund_"+p.ID, nowUTC(), "EMV Refund "+p.ID, []ledger.Entry{
				{AccountID: availCode, Direction: ledger.Debit, Money: ledger.NewMoney(refundAmt, "MXN")},
				{AccountID: "ASSET:ACQUIRER_RECEIVABLE", Direction: ledger.Credit, Money: ledger.NewMoney(refundAmt, "MXN")},
			}, map[string]string{"payment_id": p.ID})
			if err != nil {
				return err
			}
			if err := tx.PostJournal(ctx, jnl); err != nil {
				return err
			}
			if err := tx.SetPaymentStatus(ctx, p.ID, payments.PaymentSettled, payments.PaymentRefunded); err != nil {
				return err
			}
			p.Status = payments.PaymentRefunded
			updated = p
			return tx.EnqueueEvent(ctx, "payment.refunded", p.ID, map[string]any{"payment_id": p.ID, "merchant_id": tenant, "refund_cents": refundAmt})
		default:
			return payments.ErrInvalidStateTransition
		}
	})
	if err != nil {
		writeMoneyError(w, "reverse", err)
		return
	}
	updated.UpdatedAt = nowUTC()

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

	tenant, ok := requireTenant(w, r, req.MerchantID)
	if !ok {
		return
	}
	req.MerchantID = tenant

	// The QR always pays into the tenant's own registered CLABE.
	clabe, found := s.GetMerchantCLABE(tenant)
	if !found || clabe == "" {
		writeError(w, http.StatusConflict, "MERCHANT_CLABE_NOT_ASSIGNED")
		return
	}
	if req.SellerCLABE != "" && req.SellerCLABE != clabe {
		writeError(w, http.StatusForbidden, "SELLER_CLABE_NOT_OWNED")
		return
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

// maxWebhookBody bounds webhook payloads read into memory.
const maxWebhookBody = 1 << 20

// HandleCoDiWebhookConfirm: POST /api/v1/webhooks/codi/confirm. Public route,
// authenticated by an HMAC-SHA256 signature over timestamp + raw body
// (VerifyWebhookSignature); stale timestamps and replayed signatures are rejected.
func (s *Server) HandleCoDiWebhookConfirm(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	secret := s.codiWebhookSecret
	s.mu.RUnlock()
	if len(secret) == 0 {
		writeError(w, http.StatusServiceUnavailable, "WEBHOOK_NOT_CONFIGURED")
		return
	}

	bodyBytes, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		http.Error(w, `{"error":"INVALID_BODY"}`, http.StatusBadRequest)
		return
	}

	sig, err := VerifyWebhookSignature(secret, r.Header.Get(WebhookTimestampHeader), r.Header.Get(WebhookSignatureHeader), bodyBytes, time.Now(), WebhookTolerance)
	if errors.Is(err, ErrWebhookStale) {
		writeError(w, http.StatusUnauthorized, "STALE_TIMESTAMP")
		return
	}
	if err != nil {
		writeError(w, http.StatusUnauthorized, "INVALID_SIGNATURE")
		return
	}
	replayKey := "webhook:codi:sig:" + hex.EncodeToString(sig)
	if _, seen := s.idempotency.Get(replayKey); seen {
		writeError(w, http.StatusConflict, "WEBHOOK_REPLAYED")
		return
	}
	s.idempotency.Set(replayKey, http.StatusAccepted, []byte(`{}`), 2*WebhookTolerance)

	conf, err := payments.ValidateCoDiConfirmation(bodyBytes)
	if err != nil {
		http.Error(w, `{"error":"INVALID_CODI_CONFIRMATION"}`, http.StatusBadRequest)
		return
	}

	merchant, exists := s.GetMerchantByCLABE(conf.SellerCLABE)
	if !exists {
		http.Error(w, `{"error":"UNKNOWN_SELLER_CLABE"}`, http.StatusNotFound)
		return
	}

	// Validates and upserts the confirmation (idempotent; safe on retries).
	if err := s.codiService.ConfirmPayment(conf); err != nil {
		http.Error(w, `{"error":"CODI_CONFIRM_FAILED"}`, http.StatusBadRequest)
		return
	}

	// Exactly-once credit: the tracking key is claimed inside the same
	// transaction as the journal, the RESICO accumulator and the outbox event.
	ctx := r.Context()
	opKey := "codi:confirm:" + conf.TrackingKey
	if s.hasActiveRBFLoan(merchant.ID) {
		s.rbfMu.Lock()
		defer s.rbfMu.Unlock()
	}
	var respBytes []byte
	var rbfSweepCents int64
	fresh := false
	err = s.moneyStore().InTx(ctx, func(tx store.MoneyTx) error {
		fresh, rbfSweepCents = false, 0
		claimed, _, prior, err := tx.ClaimOperation(ctx, opKey)
		if err != nil {
			return err
		}
		if !claimed {
			respBytes = prior
			return nil
		}
		ym := currentYearMonth()
		accSales, err := tx.MonthlySalesForUpdate(ctx, merchant.ID, ym)
		if err != nil {
			return err
		}
		// RESICO tax split (0 platform fee on CoDi/SPEI)
		taxSplit, err := fiscal.CalculateResicoTaxSplit(conf.AmountCents, accSales, false)
		if err != nil {
			return err
		}
		rbfSweepCents = s.rbfSweepCents(merchant.ID, conf.AmountCents, conf.AmountCents-taxSplit.TotalTaxEscrowCents)
		jnl, err := ledger.NewSplitPaymentJournal("codi_"+conf.TrackingKey, nowUTC(), conf.AmountCents, merchant.ID, 0, taxSplit.TotalTaxEscrowCents, rbfSweepCents)
		if err != nil {
			return err
		}
		jnl.Description = "CoDi confirmation " + conf.TrackingKey
		jnl.Metadata = map[string]string{"tracking_key": conf.TrackingKey, "merchant_id": merchant.ID}
		if err := tx.PostJournal(ctx, jnl); err != nil {
			return err
		}
		if err := tx.AddMonthlySales(ctx, merchant.ID, ym, taxSplit.BaseAmountCents); err != nil {
			return err
		}
		resp := CoDiWebhookResponse{
			Status:            "CONFIRMED",
			TrackingKey:       conf.TrackingKey,
			MerchantID:        merchant.ID,
			GrossAmountCents:  conf.AmountCents,
			TaxEscrowCents:    taxSplit.TotalTaxEscrowCents,
			RBFSweepCents:     rbfSweepCents,
			NetAvailableCents: conf.AmountCents - taxSplit.TotalTaxEscrowCents - rbfSweepCents,
		}
		if respBytes, err = json.Marshal(resp); err != nil {
			return err
		}
		if err := tx.EnqueueEvent(ctx, "codi.confirmed", conf.TrackingKey, resp); err != nil {
			return err
		}
		fresh = true
		return tx.CompleteOperation(ctx, opKey, http.StatusOK, respBytes)
	})
	if err != nil {
		writeMoneyError(w, "codi_confirm", err)
		return
	}
	if fresh && rbfSweepCents > 0 {
		if _, err := s.rbfEngine.ApplyRepayment(merchant.ID, rbfSweepCents); err != nil {
			slog.Error("rbf repayment not applied after CoDi confirmation", "tracking_key", conf.TrackingKey, "error", err)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(respBytes)
}

// HandleDevCoDiSimulate: POST /api/v1/dev/codi/simulate-confirm (DEV_MODE
// only, authenticated). Signs a CoDi confirmation for the caller's own CLABE
// with the server secret and processes it, so the Web POS demo can simulate a
// payment without holding the webhook secret.
func (s *Server) HandleDevCoDiSimulate(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	dev, secret := s.devMode, s.codiWebhookSecret
	s.mu.RUnlock()
	if !dev {
		http.NotFound(w, r)
		return
	}
	tenant, ok := requireTenant(w, r, "")
	if !ok {
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWebhookBody))
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_BODY")
		return
	}
	var probe struct {
		SellerCLABE string `json:"seller_clabe"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON")
		return
	}
	if clabe, found := s.GetMerchantCLABE(tenant); !found || clabe != probe.SellerCLABE {
		writeError(w, http.StatusForbidden, "SELLER_CLABE_NOT_OWNED")
		return
	}
	ts := time.Now().Unix()
	signed, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, "/api/v1/webhooks/codi/confirm", bytes.NewReader(body))
	signed.Header.Set(WebhookTimestampHeader, strconv.FormatInt(ts, 10))
	signed.Header.Set(WebhookSignatureHeader, SignWebhook(secret, ts, body))
	s.HandleCoDiWebhookConfirm(w, signed)
}
