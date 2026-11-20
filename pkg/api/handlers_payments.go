package api

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"softpos/pkg/fiscal"
	"softpos/pkg/ledger"
	"softpos/pkg/payments"
	"softpos/pkg/store"
)

// SettlePaymentRequest settles an AUTHORIZED card payment. Every amount is
// derived on the server: gross = the authorized amount, platform fee = the
// merchant tariff (bps), RBF sweep = the active loan holdback. The
// gross_amount_cents field is optional and only cross-checked; client fee/RBF
// amounts are no longer accepted (unknown JSON fields are ignored).
type SettlePaymentRequest struct {
	PaymentID        string `json:"payment_id"`
	MerchantID       string `json:"merchant_id,omitempty"`
	GrossAmountCents int64  `json:"gross_amount_cents,omitempty"`
	CustomerRFC      string `json:"customer_rfc,omitempty"`
	IsPersonaMoral   bool   `json:"is_persona_moral"`
}

type SettlePaymentResponse struct {
	PaymentID                 string `json:"payment_id"`
	GrossAmountCents          int64  `json:"gross_amount_cents"`
	BaseAmountCents           int64  `json:"base_amount_cents"`
	IVAAmountCents            int64  `json:"iva_amount_cents"`
	ISRRateBps                int64  `json:"isr_rate_bps"`
	ISREscrowCents            int64  `json:"isr_escrow_cents"`
	TaxEscrowCents            int64  `json:"tax_escrow_cents"`
	PlatformFeeBps            int64  `json:"platform_fee_bps"`
	PlatformFeeCents          int64  `json:"platform_fee_cents"`
	RBFSweepCents             int64  `json:"rbf_sweep_cents"`
	NetAvailableCents         int64  `json:"net_available_cents"`
	IsPersonaMoralWithholding bool   `json:"is_persona_moral_withholding"`
}

func settleResponseFromRecord(rec store.SettlementRecord) SettlePaymentResponse {
	return SettlePaymentResponse{
		PaymentID:                 rec.PaymentID,
		GrossAmountCents:          rec.GrossCents,
		BaseAmountCents:           rec.BaseCents,
		IVAAmountCents:            rec.IVACents,
		ISRRateBps:                rec.ISRRateBps,
		ISREscrowCents:            rec.ISRCents,
		TaxEscrowCents:            rec.TaxEscrowCents,
		PlatformFeeBps:            rec.PlatformFeeBps,
		PlatformFeeCents:          rec.PlatformFeeCents,
		RBFSweepCents:             rec.RBFSweepCents,
		NetAvailableCents:         rec.NetCents,
		IsPersonaMoralWithholding: rec.PersonaMoral,
	}
}

// HandlePaymentSettle: POST /api/v1/payments/settle. Payment status change,
// split journal, RESICO accumulator, settlement record and the
// payment.settled outbox event commit in one transaction. Settling an already
// settled payment returns the stored result (idempotent).
func (s *Server) HandlePaymentSettle(w http.ResponseWriter, r *http.Request) {
	var req SettlePaymentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_JSON")
		return
	}
	tenant, ok := requireTenant(w, r, req.MerchantID)
	if !ok {
		return
	}
	if req.PaymentID == "" {
		writeError(w, http.StatusBadRequest, "MISSING_PAYMENT_ID")
		return
	}
	if req.GrossAmountCents < 0 {
		writeError(w, http.StatusBadRequest, "INVALID_AMOUNT")
		return
	}
	ctx := r.Context()
	feeBps, err := s.platformFeeBps(ctx, tenant)
	if err != nil {
		writeError(w, http.StatusServiceUnavailable, "MERCHANT_TARIFF_UNAVAILABLE")
		return
	}

	// RBF state is still in process memory (phase 2): serialize settles of
	// merchants with an active loan so concurrent sweeps cannot exceed the cap.
	if s.hasActiveRBFLoan(tenant) {
		s.rbfMu.Lock()
		defer s.rbfMu.Unlock()
	}

	var rec store.SettlementRecord
	var fresh bool
	err = s.moneyStore().InTx(ctx, func(tx store.MoneyTx) error {
		fresh = false
		p, err := tx.GetPaymentForUpdate(ctx, req.PaymentID)
		if err != nil {
			return err
		}
		if p.MerchantID != tenant { // foreign payments look nonexistent
			return payments.ErrPaymentNotFound
		}
		if req.GrossAmountCents != 0 && req.GrossAmountCents != p.AmountCents {
			return errAmountMismatch
		}
		switch p.Status {
		case payments.PaymentAuthorized:
		case payments.PaymentSettled, payments.PaymentRefunded:
			prev, found, err := tx.GetSettlementForUpdate(ctx, p.ID)
			if err != nil {
				return err
			}
			if !found {
				return payments.ErrInvalidStateTransition
			}
			rec = prev
			return nil
		default:
			return payments.ErrInvalidStateTransition
		}

		gross := p.AmountCents
		ym := currentYearMonth()
		accumulated, err := tx.MonthlySalesForUpdate(ctx, tenant, ym)
		if err != nil {
			return err
		}
		split, err := fiscal.CalculateResicoTaxSplit(gross, accumulated, req.IsPersonaMoral)
		if err != nil {
			return err
		}
		fee := bpsHalfUp(gross, feeBps)
		if fee+split.TotalTaxEscrowCents > gross {
			return ledger.ErrDeductionsExceedGross
		}
		rbfSweep := s.rbfSweepCents(tenant, gross, gross-fee-split.TotalTaxEscrowCents)

		journalID := "settle_" + p.ID
		jnl, err := ledger.NewSplitPaymentJournal(journalID, nowUTC(), gross, tenant, fee, split.TotalTaxEscrowCents, rbfSweep)
		if err != nil {
			return err
		}
		jnl.Description = "EMV settlement " + p.ID
		jnl.Metadata = map[string]string{"payment_id": p.ID, "merchant_id": tenant}
		if err := tx.PostJournal(ctx, jnl); err != nil {
			return err
		}
		if err := tx.SetPaymentStatus(ctx, p.ID, payments.PaymentAuthorized, payments.PaymentSettled); err != nil {
			return err
		}
		if err := tx.AddMonthlySales(ctx, tenant, ym, split.BaseAmountCents); err != nil {
			return err
		}
		rec = store.SettlementRecord{
			PaymentID:        p.ID,
			MerchantID:       tenant,
			GrossCents:       gross,
			BaseCents:        split.BaseAmountCents,
			IVACents:         split.IVAAmountCents,
			ISRCents:         split.ISREscrowCents,
			ISRRateBps:       split.ISRRateBps,
			TaxEscrowCents:   split.TotalTaxEscrowCents,
			PlatformFeeBps:   feeBps,
			PlatformFeeCents: fee,
			RBFSweepCents:    rbfSweep,
			NetCents:         gross - fee - split.TotalTaxEscrowCents - rbfSweep,
			PersonaMoral:     split.IsPersonaMoralWithholding,
			YearMonth:        ym,
			JournalID:        journalID,
		}
		if err := tx.InsertSettlement(ctx, rec); err != nil {
			return err
		}
		fresh = true
		return tx.EnqueueEvent(ctx, "payment.settled", p.ID, settleResponseFromRecord(rec))
	})
	if err != nil {
		writeMoneyError(w, "settle", err)
		return
	}
	if fresh && rec.RBFSweepCents > 0 {
		if _, err := s.rbfEngine.ApplyRepayment(tenant, rec.RBFSweepCents); err != nil {
			slog.Error("rbf repayment not applied after settle", "payment_id", rec.PaymentID, "error", err)
		}
	}
	writeJSON(w, http.StatusOK, settleResponseFromRecord(rec))
}

var (
	errAmountMismatch       = errors.New("amount does not match the authorized payment")
	errInsufficientFunds    = errors.New("insufficient funds")
	errRefundExceedsBalance = errors.New("refund exceeds remaining refundable amount")
)

// writeMoneyError maps unit-of-work failures to HTTP responses. Anything not
// recognized is a posting failure: the transaction was rolled back -> 500.
func writeMoneyError(w http.ResponseWriter, op string, err error) {
	switch {
	case errors.Is(err, payments.ErrPaymentNotFound):
		writeError(w, http.StatusNotFound, "PAYMENT_NOT_FOUND")
	case errors.Is(err, payments.ErrInvalidStateTransition):
		writeError(w, http.StatusConflict, "INVALID_PAYMENT_STATE")
	case errors.Is(err, errAmountMismatch):
		writeError(w, http.StatusConflict, "AMOUNT_MISMATCH")
	case errors.Is(err, errInsufficientFunds):
		writeError(w, http.StatusBadRequest, "INSUFFICIENT_FUNDS")
	case errors.Is(err, errRefundExceedsBalance):
		writeError(w, http.StatusBadRequest, "REFUND_EXCEEDS_REMAINING")
	case errors.Is(err, ledger.ErrDeductionsExceedGross):
		writeError(w, http.StatusUnprocessableEntity, "DEDUCTIONS_EXCEED_GROSS")
	case errors.Is(err, fiscal.ErrInvalidTaxAmount):
		writeError(w, http.StatusBadRequest, "TAX_CALCULATION_ERROR")
	case errors.Is(err, store.ErrMerchantNotFound):
		writeError(w, http.StatusNotFound, "MERCHANT_NOT_FOUND")
	default:
		slog.Error("money posting failed, transaction rolled back", "op", op, "error", err)
		writeError(w, http.StatusInternalServerError, "LEDGER_POSTING_FAILED")
	}
}
