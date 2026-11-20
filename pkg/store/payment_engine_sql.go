package store

import (
	"context"
	"fmt"
	"time"

	"softpos/pkg/payments"
)

// SQLPaymentEngine implements payments.PaymentProcessor on top of
// SQLEMVPaymentRepository, so card payment state survives restarts and is
// shared by every server instance. State transitions use SELECT ... FOR UPDATE.
type SQLPaymentEngine struct {
	repo *SQLEMVPaymentRepository
}

var _ payments.PaymentProcessor = (*SQLPaymentEngine)(nil)

func NewSQLPaymentEngine(repo *SQLEMVPaymentRepository) *SQLPaymentEngine {
	return &SQLPaymentEngine{repo: repo}
}

func recordToPayment(rec *EMVPaymentRecord) *payments.Payment {
	return &payments.Payment{
		ID:          rec.PaymentID,
		MerchantID:  rec.MerchantID,
		AmountCents: rec.AmountCents,
		Status:      rec.Status,
		MaskedPAN:   rec.MaskedPAN,
		CreatedAt:   rec.CreatedAt,
		UpdatedAt:   rec.UpdatedAt,
	}
}

// Authorize inserts a new AUTHORIZED payment. The raw TLV payload is not
// persisted (it may carry the clear PAN / track 2); only masked data is stored.
func (e *SQLPaymentEngine) Authorize(id string, merchantID string, emv payments.EMVTags) (*payments.Payment, error) {
	if err := payments.ValidateAuthorization(id, merchantID, emv); err != nil {
		return nil, err
	}
	now := time.Now().UTC()
	rec := EMVPaymentRecord{
		PaymentID:      id,
		MerchantID:     merchantID,
		AmountCents:    emv.AmountCents,
		Currency:       "MXN",
		Status:         payments.PaymentAuthorized,
		MaskedPAN:      emv.PANMasked,
		CryptogramType: emv.CryptogramType,
		CryptogramHex:  emv.CryptogramHex,
		ATC:            emv.ATC,
		CreatedAt:      now,
		UpdatedAt:      now,
	}
	if err := e.repo.InsertPayment(context.Background(), rec); err != nil {
		return nil, err
	}
	return recordToPayment(&rec), nil
}

func (e *SQLPaymentEngine) transition(id string, from, to payments.PaymentStatus) (*payments.Payment, error) {
	if err := e.repo.TransitionStatus(context.Background(), id, from, to); err != nil {
		return nil, err
	}
	return e.GetPayment(id)
}

func (e *SQLPaymentEngine) Settle(id string) (*payments.Payment, error) {
	return e.transition(id, payments.PaymentAuthorized, payments.PaymentSettled)
}

func (e *SQLPaymentEngine) Void(id string) (*payments.Payment, error) {
	return e.transition(id, payments.PaymentAuthorized, payments.PaymentVoided)
}

func (e *SQLPaymentEngine) Refund(id string, refundCents int64) (*payments.Payment, error) {
	p, err := e.GetPayment(id)
	if err != nil {
		return nil, err
	}
	if refundCents <= 0 || refundCents > p.AmountCents {
		return nil, payments.ErrInvalidAmount
	}
	return e.transition(id, payments.PaymentSettled, payments.PaymentRefunded)
}

func (e *SQLPaymentEngine) GetPayment(id string) (*payments.Payment, error) {
	rec, found, err := e.repo.GetPayment(context.Background(), id)
	if err != nil {
		return nil, fmt.Errorf("failed to load payment: %w", err)
	}
	if !found {
		return nil, payments.ErrPaymentNotFound
	}
	return recordToPayment(rec), nil
}
