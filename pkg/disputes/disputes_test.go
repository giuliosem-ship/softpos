package disputes

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"softpos/pkg/ledger"
)

func setupTestLedger(t *testing.T, mid string, initialBalanceCents int64) *ledger.LedgerEngine {
	le := ledger.NewLedgerEngine()

	availAcc := fmt.Sprintf("LIABILITY:MERCHANT:%s:AVAILABLE", mid)
	escrowAcc := fmt.Sprintf("LIABILITY:MERCHANT:%s:DISPUTE_ESCROW", mid)
	reserveAcc := fmt.Sprintf("LIABILITY:MERCHANT:%s:ROLLING_RESERVE", mid)
	clearingAcc := "ASSET:SETTLEMENT:ISSUER_CLEARING"
	fundingAcc := "ASSET:BANK:CASH"

	_, err := le.CreateAccount(availAcc, ledger.AccountLiability, "Merchant Available Funds")
	if err != nil {
		t.Fatalf("CreateAccount available failed: %v", err)
	}
	_, err = le.CreateAccount(escrowAcc, ledger.AccountLiability, "Dispute Escrow")
	if err != nil {
		t.Fatalf("CreateAccount escrow failed: %v", err)
	}
	_, err = le.CreateAccount(reserveAcc, ledger.AccountLiability, "Rolling Reserve")
	if err != nil {
		t.Fatalf("CreateAccount reserve failed: %v", err)
	}
	_, err = le.CreateAccount(clearingAcc, ledger.AccountAsset, "Issuer Clearing Settlement")
	if err != nil {
		t.Fatalf("CreateAccount clearing failed: %v", err)
	}
	_, err = le.CreateAccount(fundingAcc, ledger.AccountAsset, "Bank Cash Asset")
	if err != nil {
		t.Fatalf("CreateAccount funding failed: %v", err)
	}

	// Fund initial merchant available balance
	if initialBalanceCents > 0 {
		initialJournal, err := ledger.NewJournalEntry(
			"INIT-01",
			time.Now(),
			"Initial Merchant Balance",
			[]ledger.Entry{
				{AccountID: fundingAcc, Direction: ledger.Debit, Money: ledger.NewMoney(initialBalanceCents, "MXN")},
				{AccountID: availAcc, Direction: ledger.Credit, Money: ledger.NewMoney(initialBalanceCents, "MXN")},
			},
			nil,
		)
		if err != nil {
			t.Fatalf("NewJournalEntry init failed: %v", err)
		}
		if err := le.PostTransaction(initialJournal); err != nil {
			t.Fatalf("PostTransaction init failed: %v", err)
		}
	}

	return le
}

func TestDisputeLifecycle_WonFlow(t *testing.T) {
	ctx := context.Background()
	mid := "MERCHANT_01"
	initialBalance := int64(1000000) // $10,000.00 MXN
	disputeAmount := int64(250000)   // $2,500.00 MXN

	le := setupTestLedger(t, mid, initialBalance)
	engine := NewDisputeEngine(le)

	// 1. Open Dispute
	disp, err := engine.OpenDispute(ctx, "DISP-001", "TX-12345", mid, disputeAmount, "FRAUD_SUSPECTED")
	if err != nil {
		t.Fatalf("OpenDispute failed: %v", err)
	}
	if disp.Status != DisputeStatusOpened {
		t.Fatalf("Dispute status = %s, want %s", disp.Status, DisputeStatusOpened)
	}

	// Verify funds moved from Available to Escrow
	availBal, _ := le.GetBalance(fmt.Sprintf("LIABILITY:MERCHANT:%s:AVAILABLE", mid))
	escrowBal, _ := le.GetBalance(fmt.Sprintf("LIABILITY:MERCHANT:%s:DISPUTE_ESCROW", mid))

	if availBal.AmountCents != (initialBalance - disputeAmount) {
		t.Errorf("Available balance = %d, want %d", availBal.AmountCents, initialBalance-disputeAmount)
	}
	if escrowBal.AmountCents != disputeAmount {
		t.Errorf("Escrow balance = %d, want %d", escrowBal.AmountCents, disputeAmount)
	}

	// 2. Request Evidence
	if err := engine.RequestEvidence("DISP-001"); err != nil {
		t.Fatalf("RequestEvidence failed: %v", err)
	}
	disp, _ = engine.GetDispute("DISP-001")
	if disp.Status != DisputeStatusDocsRequired {
		t.Errorf("Dispute status = %s, want %s", disp.Status, DisputeStatusDocsRequired)
	}

	// 3. Submit Representment
	evidence := []string{"receipt_signed.pdf", "emv_contactless_log.json"}
	if err := engine.SubmitRepresentment("DISP-001", evidence); err != nil {
		t.Fatalf("SubmitRepresentment failed: %v", err)
	}
	disp, _ = engine.GetDispute("DISP-001")
	if disp.Status != DisputeStatusRepresentment {
		t.Errorf("Dispute status = %s, want %s", disp.Status, DisputeStatusRepresentment)
	}

	// 4. Resolve as WON (merchant wins dispute)
	if err := engine.ResolveDispute(ctx, "DISP-001", true); err != nil {
		t.Fatalf("ResolveDispute WON failed: %v", err)
	}
	disp, _ = engine.GetDispute("DISP-001")
	if disp.Status != DisputeStatusWon {
		t.Errorf("Dispute status = %s, want %s", disp.Status, DisputeStatusWon)
	}

	// Verify funds returned to Available from Escrow
	availBal, _ = le.GetBalance(fmt.Sprintf("LIABILITY:MERCHANT:%s:AVAILABLE", mid))
	escrowBal, _ = le.GetBalance(fmt.Sprintf("LIABILITY:MERCHANT:%s:DISPUTE_ESCROW", mid))

	if availBal.AmountCents != initialBalance {
		t.Errorf("Final Available balance = %d, want original %d", availBal.AmountCents, initialBalance)
	}
	if escrowBal.AmountCents != 0 {
		t.Errorf("Final Escrow balance = %d, want 0", escrowBal.AmountCents)
	}
}

func TestDisputeLifecycle_LostFlow(t *testing.T) {
	ctx := context.Background()
	mid := "MERCHANT_02"
	initialBalance := int64(500000) // $5,000.00 MXN
	disputeAmount := int64(150000)  // $1,500.00 MXN

	le := setupTestLedger(t, mid, initialBalance)
	engine := NewDisputeEngine(le)

	// Open dispute
	_, err := engine.OpenDispute(ctx, "DISP-002", "TX-99999", mid, disputeAmount, "UNRECOGNIZED_TRANSACTION")
	if err != nil {
		t.Fatalf("OpenDispute failed: %v", err)
	}

	// Resolve as LOST (customer won dispute, funds routed to issuer clearing)
	if err := engine.ResolveDispute(ctx, "DISP-002", false); err != nil {
		t.Fatalf("ResolveDispute LOST failed: %v", err)
	}

	disp, _ := engine.GetDispute("DISP-002")
	if disp.Status != DisputeStatusLost {
		t.Errorf("Dispute status = %s, want %s", disp.Status, DisputeStatusLost)
	}

	availBal, _ := le.GetBalance(fmt.Sprintf("LIABILITY:MERCHANT:%s:AVAILABLE", mid))
	escrowBal, _ := le.GetBalance(fmt.Sprintf("LIABILITY:MERCHANT:%s:DISPUTE_ESCROW", mid))
	clearingBal, _ := le.GetBalance("ASSET:SETTLEMENT:ISSUER_CLEARING")

	if availBal.AmountCents != (initialBalance - disputeAmount) {
		t.Errorf("Available balance = %d, want %d", availBal.AmountCents, initialBalance-disputeAmount)
	}
	if escrowBal.AmountCents != 0 {
		t.Errorf("Escrow balance = %d, want 0", escrowBal.AmountCents)
	}
	// Clearing account asset reduced by disputeAmount (credit normal in double-entry)
	if clearingBal.AmountCents != -disputeAmount {
		t.Errorf("Clearing balance = %d, want %d", clearingBal.AmountCents, -disputeAmount)
	}
}

func TestRollingReserve_RetainAndReleaseMature(t *testing.T) {
	ctx := context.Background()
	mid := "MERCHANT_RISKY"
	initialBalance := int64(1000000) // $10,000.00 MXN

	le := setupTestLedger(t, mid, initialBalance)
	engine := NewDisputeEngine(le)

	// 1. Retain 500 bps (5%) of $2,000.00 MXN gross = $100.00 MXN (10000 cents) for 180 days
	grossCents := int64(200000)
	reserveBps := int64(500)
	holdDays := 180

	res, err := engine.RetainRollingReserve(ctx, mid, grossCents, reserveBps, holdDays)
	if err != nil {
		t.Fatalf("RetainRollingReserve failed: %v", err)
	}
	if res.ReservedCents != 10000 {
		t.Fatalf("ReservedCents = %d, want 10000", res.ReservedCents)
	}

	availBal, _ := le.GetBalance(fmt.Sprintf("LIABILITY:MERCHANT:%s:AVAILABLE", mid))
	reserveBal, _ := le.GetBalance(fmt.Sprintf("LIABILITY:MERCHANT:%s:ROLLING_RESERVE", mid))

	if availBal.AmountCents != (initialBalance - 10000) {
		t.Errorf("Available balance = %d, want %d", availBal.AmountCents, initialBalance-10000)
	}
	if reserveBal.AmountCents != 10000 {
		t.Errorf("Reserve balance = %d, want 10000", reserveBal.AmountCents)
	}

	// 2. Try release before maturity -> 0 released
	nowBefore := time.Now().Add(30 * 24 * time.Hour) // 30 days later
	releasedCents, err := engine.ReleaseMatureReserves(ctx, nowBefore)
	if err != nil {
		t.Fatalf("ReleaseMatureReserves early failed: %v", err)
	}
	if releasedCents != 0 {
		t.Errorf("Expected 0 released early, got %d", releasedCents)
	}

	// 3. Release after maturity (181 days later)
	nowAfter := time.Now().Add(181 * 24 * time.Hour)
	releasedCents, err = engine.ReleaseMatureReserves(ctx, nowAfter)
	if err != nil {
		t.Fatalf("ReleaseMatureReserves after maturity failed: %v", err)
	}
	if releasedCents != 10000 {
		t.Errorf("Expected 10000 released, got %d", releasedCents)
	}

	availBal, _ = le.GetBalance(fmt.Sprintf("LIABILITY:MERCHANT:%s:AVAILABLE", mid))
	reserveBal, _ = le.GetBalance(fmt.Sprintf("LIABILITY:MERCHANT:%s:ROLLING_RESERVE", mid))

	if availBal.AmountCents != initialBalance {
		t.Errorf("Available balance restored = %d, want %d", availBal.AmountCents, initialBalance)
	}
	if reserveBal.AmountCents != 0 {
		t.Errorf("Reserve balance after release = %d, want 0", reserveBal.AmountCents)
	}
}

func TestDisputeEngine_ConcurrentAccess(t *testing.T) {
	ctx := context.Background()
	mid := "MERCHANT_CONC"
	initialBalance := int64(100000000) // $1,000,000.00 MXN

	le := setupTestLedger(t, mid, initialBalance)
	engine := NewDisputeEngine(le)

	const goroutines = 15
	var wg sync.WaitGroup
	wg.Add(goroutines)

	for i := 0; i < goroutines; i++ {
		dispID := fmt.Sprintf("DISP-C-%03d", i)
		go func(id string, amount int64) {
			defer wg.Done()
			_, err := engine.OpenDispute(ctx, id, "TX-CONC", mid, amount, "CONCURRENT_TEST")
			if err != nil {
				t.Errorf("Concurrent OpenDispute failed: %v", err)
				return
			}
			_ = engine.RequestEvidence(id)
			_ = engine.SubmitRepresentment(id, []string{"doc.pdf"})
			_ = engine.ResolveDispute(ctx, id, true)
		}(dispID, int64(1000*(i+1)))
	}

	wg.Wait()

	// Verify all returned to available
	availBal, err := le.GetBalance(fmt.Sprintf("LIABILITY:MERCHANT:%s:AVAILABLE", mid))
	if err != nil {
		t.Fatalf("GetBalance failed: %v", err)
	}
	if availBal.AmountCents != initialBalance {
		t.Errorf("Final available balance = %d, want original %d", availBal.AmountCents, initialBalance)
	}
}
