package store

import (
	"context"
	"errors"
	"testing"
	"time"

	"softpos/pkg/rbf"
)

func TestSQLRBFRepository_SaveAndGetLoan(t *testing.T) {
	db, cleanup := setupTestDB(t, "test_rbf_save_get")
	defer cleanup()

	repo := NewSQLRBFRepository(db)
	ctx := context.Background()

	loan := &rbf.RBFLoan{
		ID:                "rbf_merch_01",
		MerchantID:        "merch_01",
		PrincipalCents:    1000000, // $10,000.00 MXN
		FixedFeeBps:       1000,    // 10%
		FeeCents:          100000,  // $1,000.00 MXN
		RepaymentCapCents: 1100000, // $11,000.00 MXN
		HoldbackBps:       1500,    // 15%
		RepaidCents:       0,
		Status:            rbf.LoanActive,
		CreatedAt:         time.Now().UTC(),
		UpdatedAt:         time.Now().UTC(),
	}

	if err := repo.SaveLoan(ctx, loan); err != nil {
		t.Fatalf("SaveLoan failed: %v", err)
	}

	fetched, found, err := repo.GetLoan(ctx, "merch_01")
	if err != nil {
		t.Fatalf("GetLoan failed: %v", err)
	}
	if !found || fetched == nil {
		t.Fatalf("expected loan to be found")
	}

	if fetched.MerchantID != "merch_01" || fetched.PrincipalCents != 1000000 || fetched.RemainingCents() != 1100000 {
		t.Errorf("unexpected loan data: %+v", fetched)
	}
}

func TestSQLRBFRepository_ApplyRepaymentWithLock(t *testing.T) {
	db, cleanup := setupTestDB(t, "test_rbf_repayment_lock")
	defer cleanup()

	repo := NewSQLRBFRepository(db)
	ctx := context.Background()

	loan := &rbf.RBFLoan{
		ID:                "rbf_merch_02",
		MerchantID:        "merch_02",
		PrincipalCents:    500000,
		FixedFeeBps:       1000,
		FeeCents:          50000,
		RepaymentCapCents: 550000,
		HoldbackBps:       1000,
		RepaidCents:       0,
		Status:            rbf.LoanActive,
		CreatedAt:         time.Now().UTC(),
		UpdatedAt:         time.Now().UTC(),
	}
	if err := repo.SaveLoan(ctx, loan); err != nil {
		t.Fatalf("SaveLoan failed: %v", err)
	}

	// 1. First partial repayment: $1,500.00 MXN (150,000 cents)
	updated, err := repo.ApplyRepaymentWithLock(ctx, "merch_02", 150000)
	if err != nil {
		t.Fatalf("ApplyRepaymentWithLock failed: %v", err)
	}
	if updated.RepaidCents != 150000 || updated.RemainingCents() != 400000 || updated.Status != rbf.LoanActive {
		t.Errorf("unexpected state after partial repayment: %+v", updated)
	}

	// 2. Final repayment: $4,000.00 MXN (400,000 cents)
	finalLoan, err := repo.ApplyRepaymentWithLock(ctx, "merch_02", 400000)
	if err != nil {
		t.Fatalf("final repayment failed: %v", err)
	}
	if finalLoan.RepaidCents != 550000 || finalLoan.RemainingCents() != 0 || finalLoan.Status != rbf.LoanPaidOff {
		t.Errorf("expected loan to be PAID_OFF, got: %+v", finalLoan)
	}

	// 3. Additional repayment should fail with ErrInvalidRepayment
	_, err = repo.ApplyRepaymentWithLock(ctx, "merch_02", 1000)
	if !errors.Is(err, rbf.ErrInvalidRepayment) {
		t.Errorf("expected ErrInvalidRepayment on paid-off loan, got: %v", err)
	}
}

func TestSQLRBFRepository_LoanNotFoundAndInvalidRepayments(t *testing.T) {
	db, cleanup := setupTestDB(t, "test_rbf_errors")
	defer cleanup()

	repo := NewSQLRBFRepository(db)
	ctx := context.Background()

	_, found, err := repo.GetLoan(ctx, "non_existent")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if found {
		t.Errorf("expected loan not found")
	}

	_, err = repo.ApplyRepaymentWithLock(ctx, "non_existent", 500)
	if !errors.Is(err, rbf.ErrLoanNotFound) {
		t.Errorf("expected ErrLoanNotFound, got: %v", err)
	}

	_, err = repo.ApplyRepaymentWithLock(ctx, "non_existent", -500)
	if !errors.Is(err, rbf.ErrInvalidRepayment) {
		t.Errorf("expected ErrInvalidRepayment on negative repayment, got: %v", err)
	}
}
