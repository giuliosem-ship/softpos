package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"softpos/pkg/payments"
)

var (
	ErrPaymentNotFound        = payments.ErrPaymentNotFound
	ErrInvalidStateTransition = payments.ErrInvalidStateTransition
)

// EMVPaymentRecord represents a persisted contactless card transaction.
type EMVPaymentRecord struct {
	PaymentID      string
	MerchantID     string
	AmountCents    int64
	Currency       string
	Status         payments.PaymentStatus
	MaskedPAN      string
	CryptogramType payments.CryptogramType
	CryptogramHex  string
	ATC            int
	TLVPayloadHex  string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

// SQLEMVPaymentRepository provides PostgreSQL persistence for contactless EMV card payments.
type SQLEMVPaymentRepository struct {
	db *sql.DB
}

func NewSQLEMVPaymentRepository(db *sql.DB) *SQLEMVPaymentRepository {
	return &SQLEMVPaymentRepository{db: db}
}

// SavePayment inserts or updates an EMV payment record.
func (r *SQLEMVPaymentRepository) SavePayment(ctx context.Context, record EMVPaymentRecord) error {
	now := time.Now().UTC()
	createdAt := record.CreatedAt
	if createdAt.IsZero() {
		createdAt = now
	}
	updatedAt := record.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = now
	}
	curr := record.Currency
	if curr == "" {
		curr = "MXN"
	}

	query := `
		INSERT INTO emv_payments (
			id, merchant_id, amount_cents, currency, status,
			masked_pan, cryptogram_type, cryptogram_hex, atc,
			tlv_payload_hex, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (id) DO UPDATE SET
			status = EXCLUDED.status,
			updated_at = EXCLUDED.updated_at
	`

	_, err := r.db.ExecContext(ctx, query,
		record.PaymentID,
		record.MerchantID,
		record.AmountCents,
		curr,
		string(record.Status),
		record.MaskedPAN,
		string(record.CryptogramType),
		record.CryptogramHex,
		record.ATC,
		record.TLVPayloadHex,
		createdAt,
		updatedAt,
	)
	if err != nil {
		return fmt.Errorf("failed to save EMV payment: %w", err)
	}
	return nil
}

// GetPayment retrieves an EMV payment record by its ID.
func (r *SQLEMVPaymentRepository) GetPayment(ctx context.Context, paymentID string) (*EMVPaymentRecord, bool, error) {
	query := `
		SELECT id, merchant_id, amount_cents, currency, status,
		       masked_pan, cryptogram_type, cryptogram_hex, atc,
		       tlv_payload_hex, created_at, updated_at
		FROM emv_payments
		WHERE id = $1
	`

	var rec EMVPaymentRecord
	var statusStr, cryptoType string

	err := r.db.QueryRowContext(ctx, query, paymentID).Scan(
		&rec.PaymentID,
		&rec.MerchantID,
		&rec.AmountCents,
		&rec.Currency,
		&statusStr,
		&rec.MaskedPAN,
		&cryptoType,
		&rec.CryptogramHex,
		&rec.ATC,
		&rec.TLVPayloadHex,
		&rec.CreatedAt,
		&rec.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("failed to get EMV payment: %w", err)
	}

	rec.Status = payments.PaymentStatus(statusStr)
	rec.CryptogramType = payments.CryptogramType(cryptoType)
	return &rec, true, nil
}

// TransitionStatus validates and executes an atomic payment state transition (e.g. AUTHORIZED -> SETTLED).
func (r *SQLEMVPaymentRepository) TransitionStatus(
	ctx context.Context,
	paymentID string,
	from payments.PaymentStatus,
	to payments.PaymentStatus,
) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var currentStatus string
	querySelect := `SELECT status FROM emv_payments WHERE id = $1 FOR UPDATE`
	err = tx.QueryRowContext(ctx, querySelect, paymentID).Scan(&currentStatus)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrPaymentNotFound
		}
		return fmt.Errorf("failed to lock payment for status transition: %w", err)
	}

	if payments.PaymentStatus(currentStatus) != from {
		return ErrInvalidStateTransition
	}

	now := time.Now().UTC()
	queryUpdate := `UPDATE emv_payments SET status = $1, updated_at = $2 WHERE id = $3`
	_, err = tx.ExecContext(ctx, queryUpdate, string(to), now, paymentID)
	if err != nil {
		return fmt.Errorf("failed to update payment status: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("failed to commit payment status transition: %w", err)
	}
	return nil
}
