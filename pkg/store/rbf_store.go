package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"softpos/pkg/rbf"
)

var (
	ErrLoanNotFound     = rbf.ErrLoanNotFound
	ErrActiveLoanExists = rbf.ErrActiveLoanExists
	ErrInvalidRepayment = rbf.ErrInvalidRepayment
)

// SQLRBFRepository provides PostgreSQL persistence and row-level pessimistic locking for RBF loans.
type SQLRBFRepository struct {
	db *sql.DB
}

func NewSQLRBFRepository(db *sql.DB) *SQLRBFRepository {
	return &SQLRBFRepository{db: db}
}

// SaveLoan inserts or updates an RBF loan record.
func (r *SQLRBFRepository) SaveLoan(ctx context.Context, loan *rbf.RBFLoan) error {
	now := time.Now().UTC()
	createdAt := loan.CreatedAt
	if createdAt.IsZero() {
		createdAt = now
	}
	updatedAt := loan.UpdatedAt
	if updatedAt.IsZero() {
		updatedAt = now
	}

	query := `
		INSERT INTO rbf_loans (
			merchant_id, principal_cents, fee_cents, repayment_cap_cents,
			holdback_bps, repaid_cents, remaining_cents, status, created_at, updated_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		ON CONFLICT (merchant_id) DO UPDATE SET
			principal_cents = EXCLUDED.principal_cents,
			fee_cents = EXCLUDED.fee_cents,
			repayment_cap_cents = EXCLUDED.repayment_cap_cents,
			holdback_bps = EXCLUDED.holdback_bps,
			repaid_cents = EXCLUDED.repaid_cents,
			remaining_cents = EXCLUDED.remaining_cents,
			status = EXCLUDED.status,
			updated_at = EXCLUDED.updated_at
	`

	_, err := r.db.ExecContext(ctx, query,
		loan.MerchantID,
		loan.PrincipalCents,
		loan.FeeCents,
		loan.RepaymentCapCents,
		loan.HoldbackBps,
		loan.RepaidCents,
		loan.RemainingCents(),
		string(loan.Status),
		createdAt,
		updatedAt,
	)
	if err != nil {
		return fmt.Errorf("failed to save RBF loan: %w", err)
	}
	return nil
}

// GetLoan retrieves an RBF loan by merchant ID.
func (r *SQLRBFRepository) GetLoan(ctx context.Context, merchantID string) (*rbf.RBFLoan, bool, error) {
	query := `
		SELECT merchant_id, principal_cents, fee_cents, repayment_cap_cents,
		       holdback_bps, repaid_cents, remaining_cents, status, created_at, updated_at
		FROM rbf_loans
		WHERE merchant_id = $1
	`
	var loan rbf.RBFLoan
	var statusStr string
	var remainingCents int64

	err := r.db.QueryRowContext(ctx, query, merchantID).Scan(
		&loan.MerchantID,
		&loan.PrincipalCents,
		&loan.FeeCents,
		&loan.RepaymentCapCents,
		&loan.HoldbackBps,
		&loan.RepaidCents,
		&remainingCents,
		&statusStr,
		&loan.CreatedAt,
		&loan.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("failed to get RBF loan: %w", err)
	}

	loan.ID = fmt.Sprintf("rbf_%s", loan.MerchantID)
	loan.Status = rbf.LoanStatus(statusStr)
	return &loan, true, nil
}

// ApplyRepaymentWithLock locks the merchant's loan row (SELECT ... FOR UPDATE),
// deducts the repayment from remaining balance, updates repaid_cents, transitions status if paid off, and commits atomically.
func (r *SQLRBFRepository) ApplyRepaymentWithLock(ctx context.Context, merchantID string, repaymentCents int64) (*rbf.RBFLoan, error) {
	if repaymentCents <= 0 {
		return nil, ErrInvalidRepayment
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	query := `
		SELECT merchant_id, principal_cents, fee_cents, repayment_cap_cents,
		       holdback_bps, repaid_cents, remaining_cents, status, created_at, updated_at
		FROM rbf_loans
		WHERE merchant_id = $1
		FOR UPDATE
	`

	var loan rbf.RBFLoan
	var statusStr string
	var remainingCents int64

	err = tx.QueryRowContext(ctx, query, merchantID).Scan(
		&loan.MerchantID,
		&loan.PrincipalCents,
		&loan.FeeCents,
		&loan.RepaymentCapCents,
		&loan.HoldbackBps,
		&loan.RepaidCents,
		&remainingCents,
		&statusStr,
		&loan.CreatedAt,
		&loan.UpdatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrLoanNotFound
		}
		return nil, fmt.Errorf("failed to query loan with lock: %w", err)
	}

	loan.Status = rbf.LoanStatus(statusStr)
	if loan.Status != rbf.LoanActive || remainingCents <= 0 || repaymentCents > remainingCents {
		return nil, ErrInvalidRepayment
	}

	newRepaid := loan.RepaidCents + repaymentCents
	newRemaining := remainingCents - repaymentCents
	newStatus := rbf.LoanActive
	if newRemaining == 0 {
		newStatus = rbf.LoanPaidOff
	}
	now := time.Now().UTC()

	updateQuery := `
		UPDATE rbf_loans
		SET repaid_cents = $1, remaining_cents = $2, status = $3, updated_at = $4
		WHERE merchant_id = $5
	`
	_, err = tx.ExecContext(ctx, updateQuery, newRepaid, newRemaining, string(newStatus), now, merchantID)
	if err != nil {
		return nil, fmt.Errorf("failed to update repaid balance: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("failed to commit repayment: %w", err)
	}

	loan.RepaidCents = newRepaid
	loan.Status = newStatus
	loan.UpdatedAt = now
	return &loan, nil
}
