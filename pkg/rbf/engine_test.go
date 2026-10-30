package rbf_test

import (
	"errors"
	"testing"

	"softpos/pkg/ledger"
	"softpos/pkg/rbf"
)

func setupRBFLedger(t *testing.T) (*ledger.LedgerEngine, *rbf.Engine) {
	t.Helper()
	le := ledger.NewLedgerEngine()
	_, _ = le.CreateAccount("ASSET:ACQUIRER_RECEIVABLE", ledger.AccountAsset, "Acquirer Receivable")
	_, _ = le.CreateAccount("REVENUE:FEES", ledger.AccountRevenue, "Platform Fees")
	return le, rbf.NewEngine(le)
}

func TestOriginateLoan_AndLedgerPosting(t *testing.T) {
	le, engine := setupRBFLedger(t)

	// Originate $10,000.00 MXN (1,000,000 cents) with 15% fixed fee (1500 bps) and 10% holdback (1000 bps)
	loan, err := engine.OriginateLoan("merch_01", 1_000_000, 1500, 1000)
	if err != nil {
		t.Fatalf("OriginateLoan failed: %v", err)
	}

	if loan.PrincipalCents != 1_000_000 {
		t.Errorf("PrincipalCents = %d; want 1000000", loan.PrincipalCents)
	}
	if loan.FeeCents != 150_000 {
		t.Errorf("FeeCents = %d; want 150000", loan.FeeCents)
	}
	if loan.RepaymentCapCents != 1_150_000 {
		t.Errorf("RepaymentCapCents = %d; want 1150000", loan.RepaymentCapCents)
	}
	if loan.Status != rbf.LoanActive {
		t.Errorf("Status = %s; want ACTIVE", loan.Status)
	}

	// Verify Ledger balances:
	// Merchant available should have $10,000.00 MXN (1,000,000 cents)
	availBal, err := le.GetBalance("LIABILITY:MERCHANT:merch_01:AVAILABLE")
	if err != nil || availBal.AmountCents != 1_000_000 {
		t.Errorf("Available balance = %d, err = %v; want 1000000", availBal.AmountCents, err)
	}

	// RBF Receivable should reflect full repayment cap ($11,500.00 MXN = 1,150,000 cents)
	rbfBal, err := le.GetBalance("ASSET:RBF_LOANS_RECEIVABLE:merch_01")
	if err != nil || rbfBal.AmountCents != 1_150_000 {
		t.Errorf("RBF Receivable balance = %d, err = %v; want 1150000", rbfBal.AmountCents, err)
	}

	// Duplicate active loan for same merchant must be rejected
	_, err = engine.OriginateLoan("merch_01", 500_000, 1000, 1000)
	if !errors.Is(err, rbf.ErrActiveLoanExists) {
		t.Errorf("expected ErrActiveLoanExists, got %v", err)
	}
}

func TestHoldbackSweep_AndRepaymentCapEnforcement(t *testing.T) {
	_, engine := setupRBFLedger(t)

	// Loan: $1,000.00 principal (100,000 cents), 10% fee (10,000 cents) -> Cap = 110,000 cents
	// Holdback = 20% (2000 bps) per sale
	loan, err := engine.OriginateLoan("merch_cap", 100_000, 1000, 2000)
	if err != nil {
		t.Fatalf("OriginateLoan failed: %v", err)
	}
	if loan.RepaymentCapCents != 110_000 {
		t.Fatalf("expected cap 110000, got %d", loan.RepaymentCapCents)
	}

	// Sale 1: $3,000.00 gross (300,000 cents) -> 20% holdback = 60,000 cents
	sweep1 := engine.CalculateSweep("merch_cap", 300_000, 250_000)
	if sweep1 != 60_000 {
		t.Fatalf("expected sweep1 = 60000, got %d", sweep1)
	}
	updated, err := engine.ApplyRepayment("merch_cap", sweep1)
	if err != nil {
		t.Fatalf("ApplyRepayment 1 failed: %v", err)
	}
	if updated.RemainingCents() != 50_000 || updated.Status != rbf.LoanActive {
		t.Errorf("expected 50000 remaining and ACTIVE, got %d (%s)", updated.RemainingCents(), updated.Status)
	}

	// Sale 2: $4,000.00 gross (400,000 cents) -> 20% would be 80,000 cents,
	// MUST BE CAPPED at the remaining 50,000 cents!
	sweep2 := engine.CalculateSweep("merch_cap", 400_000, 320_000)
	if sweep2 != 50_000 {
		t.Fatalf("expected sweep2 capped at 50000, got %d", sweep2)
	}
	paidOff, err := engine.ApplyRepayment("merch_cap", sweep2)
	if err != nil {
		t.Fatalf("ApplyRepayment 2 failed: %v", err)
	}
	if paidOff.Status != rbf.LoanPaidOff || paidOff.RemainingCents() != 0 {
		t.Errorf("expected PAID_OFF and 0 remaining, got %s and %d", paidOff.Status, paidOff.RemainingCents())
	}

	// Sale 3 after payoff: sweep must be 0
	sweep3 := engine.CalculateSweep("merch_cap", 100_000, 80_000)
	if sweep3 != 0 {
		t.Errorf("expected 0 sweep after loan payoff, got %d", sweep3)
	}
}
