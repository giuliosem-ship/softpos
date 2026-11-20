package api

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"softpos/pkg/auth"
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
	RefundCents int64  `json:"refund_cents,omitempty"` // 0 = the whole remaining amount
}

type EMVReverseResponse struct {
	PaymentID                string                 `json:"payment_id"`
	MerchantID               string                 `json:"merchant_id"`
	AmountCents              int64                  `json:"amount_cents"`
	Status                   payments.PaymentStatus `json:"status"`
	RefundCents              int64                  `json:"refund_cents,omitempty"`
	RefundedTotalCents       int64                  `json:"refunded_total_cents,omitempty"`
	RemainingRefundableCents int64                  `json:"remaining_refundable_cents"`
	ReversedTaxEscrowCents   int64                  `json:"reversed_tax_escrow_cents,omitempty"`
	ReversedPlatformFeeCents int64                  `json:"reversed_platform_fee_cents,omitempty"`
	UpdatedAt                string                 `json:"updated_at"`
}

// SetCashierRefundsAllowed lets cashiers refund settled payments
// (ALLOW_CASHIER_REFUNDS). Default: refunds are owner-only; voids of
// not-yet-settled payments stay open to cashiers.
func (s *Server) SetCashierRefundsAllowed(allowed bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cashierRefunds = allowed
}

// refundSplit is the proportional reversal of a settlement for one refund.
type refundSplit struct {
	amount, base, taxEscrow, fee, available int64
}

// proportionalRefund computes the reversal of refundCents against rec using
// cumulative targets (target_x = x * refunded_total / gross, half-up), so the
// sum of partial reversals equals the full reversal exactly.
func proportionalRefund(rec store.SettlementRecord, refundCents int64) refundSplit {
	total := rec.RefundedGrossCents + refundCents
	target := func(x int64) int64 {
		if total == rec.GrossCents {
			return x
		}
		return ratioHalfUp(x, total, rec.GrossCents)
	}
	sp := refundSplit{
		amount:    refundCents,
		base:      target(rec.BaseCents) - rec.RefundedBaseCents,
		taxEscrow: target(rec.TaxEscrowCents) - rec.RefundedTaxEscrowCents,
		fee:       target(rec.PlatformFeeCents) - rec.RefundedFeeCents,
	}
	sp.available = refundCents - sp.taxEscrow - sp.fee
	if sp.available < 0 { // rounding guard on near-total deductions
		sp.taxEscrow += sp.available
		sp.available = 0
	}
	return sp
}

// postRefundReversal reverses amount of a settlement inside tx
// (proportionalRefund): journal refund_<paymentID>_<refunded_total> (Dr
// available / tax escrow / fees, Cr acquirer receivable), RESICO accumulator
// of the settlement month and the settlement refund columns. Shared by the
// legacy EMV reverse and processor refund webhooks; amount must be within the
// remaining refundable amount.
func postRefundReversal(ctx context.Context, tx store.MoneyTx, tenant, paymentID string, rec store.SettlementRecord, amount int64, description string) (store.SettlementRecord, refundSplit, error) {
	if amount <= 0 || amount > rec.GrossCents-rec.RefundedGrossCents {
		return rec, refundSplit{}, errRefundExceedsBalance
	}
	// Lock order (docs/MONEY_FLOWS.md): the RESICO month row before the
	// ledger accounts, as settles and CoDi confirmations do; otherwise
	// concurrent settles and refunds of one merchant deadlock.
	if _, err := tx.MonthlySalesForUpdate(ctx, tenant, rec.YearMonth); err != nil {
		return rec, refundSplit{}, err
	}
	sp := proportionalRefund(rec, amount)
	rec.RefundedGrossCents += sp.amount
	rec.RefundedBaseCents += sp.base
	rec.RefundedTaxEscrowCents += sp.taxEscrow
	rec.RefundedFeeCents += sp.fee

	var entries []ledger.Entry
	add := func(account string, dir ledger.Direction, cents int64) {
		if cents > 0 {
			entries = append(entries, ledger.Entry{AccountID: account, Direction: dir, Money: ledger.NewMoney(cents, "MXN")})
		}
	}
	add(fmt.Sprintf("LIABILITY:MERCHANT:%s:AVAILABLE", tenant), ledger.Debit, sp.available)
	add(fmt.Sprintf("LIABILITY:MERCHANT:%s:TAX_ESCROW", tenant), ledger.Debit, sp.taxEscrow)
	add("REVENUE:FEES", ledger.Debit, sp.fee)
	add("ASSET:ACQUIRER_RECEIVABLE", ledger.Credit, sp.amount)
	jnl, err := ledger.NewJournalEntry(fmt.Sprintf("refund_%s_%d", paymentID, rec.RefundedGrossCents), nowUTC(), description, entries,
		map[string]string{"payment_id": paymentID, "merchant_id": tenant})
	if err != nil {
		return rec, refundSplit{}, err
	}
	if err := tx.PostJournal(ctx, jnl); err != nil {
		return rec, refundSplit{}, err
	}
	if err := tx.AddMonthlySales(ctx, tenant, rec.YearMonth, -sp.base); err != nil {
		return rec, refundSplit{}, err
	}
	if err := tx.UpdateSettlementRefunds(ctx, rec); err != nil {
		return rec, refundSplit{}, err
	}
	return rec, sp, nil
}

// HandleEMVReverse: POST /api/v1/payments/emv/reverse. AUTHORIZED -> VOIDED
// (nothing was posted). SETTLED -> full or partial refund that reverses,
// proportionally and in one transaction, the merchant available balance, the
// tax set-aside (IVA/ISR), the platform fee and the RESICO accumulator of the
// settlement month. Refunds can never exceed the remaining refundable amount;
// the RBF holdback is not reversed (the loan repayment stands). An optional
// Idempotency-Key makes retries replay the first result.
func (s *Server) HandleEMVReverse(w http.ResponseWriter, r *http.Request) {
	var req EMVReverseRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON")
		return
	}
	if req.PaymentID == "" {
		writeError(w, http.StatusBadRequest, "MISSING_PAYMENT_ID")
		return
	}
	if req.RefundCents < 0 {
		writeError(w, http.StatusBadRequest, "INVALID_AMOUNT")
		return
	}
	tenant, ok := requireTenant(w, r, "")
	if !ok {
		return
	}
	s.mu.RLock()
	cashierRefunds := s.cashierRefunds
	s.mu.RUnlock()
	role := RoleFromContext(r.Context())

	ctx := r.Context()
	opKey := ""
	if k := r.Header.Get("Idempotency-Key"); k != "" {
		opKey = tenantIdempotencyKey(tenant, "reverse:"+req.PaymentID+":"+k)
	}
	var respBytes []byte
	err := s.moneyStore().InTx(ctx, func(tx store.MoneyTx) error {
		p, err := tx.GetPaymentForUpdate(ctx, req.PaymentID)
		if err != nil {
			return err
		}
		if p.MerchantID != tenant { // foreign payments look nonexistent
			return payments.ErrPaymentNotFound
		}
		if p.Status == payments.PaymentSettled && role != auth.RoleOwner && !(cashierRefunds && role == auth.RoleCashier) {
			return errRefundRoleForbidden
		}
		if opKey != "" {
			claimed, _, prior, err := tx.ClaimOperation(ctx, opKey)
			if err != nil {
				return err
			}
			if !claimed {
				respBytes = prior
				return nil
			}
		}
		resp := EMVReverseResponse{PaymentID: p.ID, MerchantID: tenant, AmountCents: p.AmountCents, UpdatedAt: nowUTC().Format(time.RFC3339)}

		switch p.Status {
		case payments.PaymentAuthorized:
			if req.RefundCents != 0 && req.RefundCents != p.AmountCents {
				return errPartialVoid
			}
			if err := tx.SetPaymentStatus(ctx, p.ID, payments.PaymentAuthorized, payments.PaymentVoided); err != nil {
				return err
			}
			resp.Status = payments.PaymentVoided
			if err := tx.EnqueueEvent(ctx, "payment.voided", p.ID, resp); err != nil {
				return err
			}
		case payments.PaymentSettled:
			rec, found, err := tx.GetSettlementForUpdate(ctx, p.ID)
			if err != nil {
				return err
			}
			if !found {
				return errSettlementMissing
			}
			remaining := rec.GrossCents - rec.RefundedGrossCents
			amount := req.RefundCents
			if amount == 0 {
				amount = remaining
			}
			if amount <= 0 || amount > remaining {
				return errRefundExceedsBalance
			}
			var sp refundSplit
			rec, sp, err = postRefundReversal(ctx, tx, tenant, p.ID, rec, amount, "EMV refund "+p.ID)
			if err != nil {
				return err
			}
			resp.Status = payments.PaymentSettled
			if rec.RefundedGrossCents == rec.GrossCents {
				if err := tx.SetPaymentStatus(ctx, p.ID, payments.PaymentSettled, payments.PaymentRefunded); err != nil {
					return err
				}
				resp.Status = payments.PaymentRefunded
			}
			resp.RefundCents = sp.amount
			resp.RefundedTotalCents = rec.RefundedGrossCents
			resp.RemainingRefundableCents = rec.GrossCents - rec.RefundedGrossCents
			resp.ReversedTaxEscrowCents = sp.taxEscrow
			resp.ReversedPlatformFeeCents = sp.fee
			if err := tx.EnqueueEvent(ctx, "payment.refunded", p.ID, resp); err != nil {
				return err
			}
		default:
			return payments.ErrInvalidStateTransition
		}
		if respBytes, err = json.Marshal(resp); err != nil {
			return err
		}
		if opKey != "" {
			return tx.CompleteOperation(ctx, opKey, http.StatusOK, respBytes)
		}
		return nil
	})
	if errors.Is(err, errRefundRoleForbidden) {
		writeError(w, http.StatusForbidden, "ROLE_FORBIDDEN")
		return
	}
	if errors.Is(err, errPartialVoid) {
		writeError(w, http.StatusBadRequest, "PARTIAL_VOID_NOT_SUPPORTED")
		return
	}
	if errors.Is(err, errSettlementMissing) {
		writeError(w, http.StatusConflict, "SETTLEMENT_RECORD_MISSING")
		return
	}
	if err != nil {
		writeMoneyError(w, "reverse", err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(respBytes)
}

var (
	errRefundRoleForbidden = errors.New("refunds are owner-only")
	errPartialVoid         = errors.New("an authorized payment can only be voided in full")
	errSettlementMissing   = errors.New("settled payment has no settlement record")
)

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
