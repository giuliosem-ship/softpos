package ledger

import (
	"sync"
	"testing"
	"time"
)

func TestCreateAccountAndDuplicates(t *testing.T) {
	engine := NewLedgerEngine()

	acc, err := engine.CreateAccount("ASSET:STP_CONCENTRATOR", AccountAsset, "STP Concentrator Account")
	if err != nil {
		t.Fatalf("unexpected error creating account: %v", err)
	}
	if acc.Code != "ASSET:STP_CONCENTRATOR" || acc.Type != AccountAsset {
		t.Errorf("account properties mismatch: %+v", acc)
	}

	// Duplicate
	_, err = engine.CreateAccount("ASSET:STP_CONCENTRATOR", AccountAsset, "Duplicate")
	if err != ErrDuplicateAccount {
		t.Errorf("expected ErrDuplicateAccount, got %v", err)
	}
}

func TestGetAccountNotFound(t *testing.T) {
	engine := NewLedgerEngine()
	_, err := engine.GetAccount("NON_EXISTENT")
	if err != ErrAccountNotFound {
		t.Errorf("expected ErrAccountNotFound, got %v", err)
	}

	_, err = engine.GetBalance("NON_EXISTENT")
	if err != ErrAccountNotFound {
		t.Errorf("expected ErrAccountNotFound on GetBalance, got %v", err)
	}
}

func TestPostTransactionAndNormalBalances(t *testing.T) {
	engine := NewLedgerEngine()

	// 1. Setup accounts
	_, _ = engine.CreateAccount("ASSET:CASH", AccountAsset, "Cash")
	_, _ = engine.CreateAccount("LIABILITY:MERCHANT:1:AVAILABLE", AccountLiability, "Merchant Available")
	_, _ = engine.CreateAccount("REVENUE:FEES", AccountRevenue, "Fee Revenue")

	// 2. Initial state: balance should be 0 MXN
	bal, err := engine.GetBalance("ASSET:CASH")
	if err != nil || bal.AmountCents != 0 {
		t.Fatalf("expected initial balance 0, got %v, err: %v", bal, err)
	}

	// 3. Post a transaction: Debit Cash $100, Credit Merchant $97, Credit Fees $3
	entries := []Entry{
		{AccountID: "ASSET:CASH", Direction: Debit, Money: NewMoney(10000, "MXN")},
		{AccountID: "LIABILITY:MERCHANT:1:AVAILABLE", Direction: Credit, Money: NewMoney(9700, "MXN")},
		{AccountID: "REVENUE:FEES", Direction: Credit, Money: NewMoney(300, "MXN")},
	}
	journal, err := NewJournalEntry("tx_01", time.Now(), "Deposit & split", entries, nil)
	if err != nil {
		t.Fatalf("failed to build journal: %v", err)
	}

	if err := engine.PostTransaction(journal); err != nil {
		t.Fatalf("failed to post transaction: %v", err)
	}

	// Asset normal balance = Debits - Credits -> +10000
	cashBal, _ := engine.GetBalance("ASSET:CASH")
	if cashBal.AmountCents != 10000 {
		t.Errorf("expected cash 10000, got %d", cashBal.AmountCents)
	}

	// Liability normal balance = Credits - Debits -> +9700
	merchBal, _ := engine.GetBalance("LIABILITY:MERCHANT:1:AVAILABLE")
	if merchBal.AmountCents != 9700 {
		t.Errorf("expected merchant liability 9700, got %d", merchBal.AmountCents)
	}

	// Revenue normal balance = Credits - Debits -> +300
	feeBal, _ := engine.GetBalance("REVENUE:FEES")
	if feeBal.AmountCents != 300 {
		t.Errorf("expected fees 300, got %d", feeBal.AmountCents)
	}
}

func TestPostTransactionUnknownAccountFails(t *testing.T) {
	engine := NewLedgerEngine()
	_, _ = engine.CreateAccount("ASSET:CASH", AccountAsset, "Cash")

	entries := []Entry{
		{AccountID: "ASSET:CASH", Direction: Debit, Money: NewMoney(1000, "MXN")},
		{AccountID: "UNKNOWN:ACCOUNT", Direction: Credit, Money: NewMoney(1000, "MXN")},
	}
	journal, _ := NewJournalEntry("tx_missing", time.Now(), "Missing Account", entries, nil)

	err := engine.PostTransaction(journal)
	if err != ErrAccountNotFound {
		t.Errorf("expected ErrAccountNotFound, got %v", err)
	}
}

func TestRecordSplitPayment(t *testing.T) {
	engine := NewLedgerEngine()

	merchantID := "merch_mx_001"
	_, _ = engine.CreateAccount("ASSET:ACQUIRER_RECEIVABLE", AccountAsset, "Acquirer Receivable")
	_, _ = engine.CreateAccount("REVENUE:FEES", AccountRevenue, "Platform Fees")
	_, _ = engine.CreateAccount("LIABILITY:MERCHANT:"+merchantID+":TAX_ESCROW", AccountLiability, "Tax Escrow")
	_, _ = engine.CreateAccount("ASSET:RBF_LOANS_RECEIVABLE", AccountAsset, "RBF Loan Receivable")
	_, _ = engine.CreateAccount("LIABILITY:MERCHANT:"+merchantID+":AVAILABLE", AccountLiability, "Merchant Available")

	// Gross $100.00 (10000 cents)
	// Fees: $3.36 (336 cents)
	// Tax Escrow: $16.00 (1600 cents)
	// RBF Sweep: $10.00 (1000 cents)
	// Net Available: 10000 - (336 + 1600 + 1000) = 7064 cents ($70.64 MXN)
	journal, err := engine.RecordSplitPayment(10000, merchantID, 336, 1600, 1000)
	if err != nil {
		t.Fatalf("unexpected error recording split payment: %v", err)
	}
	if journal == nil {
		t.Fatal("expected journal entry, got nil")
	}

	// Verify balances
	acquirerBal, _ := engine.GetBalance("ASSET:ACQUIRER_RECEIVABLE")
	if acquirerBal.AmountCents != 10000 {
		t.Errorf("expected acquirer balance 10000, got %d", acquirerBal.AmountCents)
	}

	feeBal, _ := engine.GetBalance("REVENUE:FEES")
	if feeBal.AmountCents != 336 {
		t.Errorf("expected fee balance 336, got %d", feeBal.AmountCents)
	}

	escrowBal, _ := engine.GetBalance("LIABILITY:MERCHANT:" + merchantID + ":TAX_ESCROW")
	if escrowBal.AmountCents != 1600 {
		t.Errorf("expected escrow balance 1600, got %d", escrowBal.AmountCents)
	}

	// RBF Receivable is an Asset with credit entry -> balance = Debits - Credits = -1000
	rbfBal, _ := engine.GetBalance("ASSET:RBF_LOANS_RECEIVABLE")
	if rbfBal.AmountCents != -1000 {
		t.Errorf("expected RBF balance -1000, got %d", rbfBal.AmountCents)
	}

	availBal, _ := engine.GetBalance("LIABILITY:MERCHANT:" + merchantID + ":AVAILABLE")
	if availBal.AmountCents != 7064 {
		t.Errorf("expected merchant available 7064, got %d", availBal.AmountCents)
	}
}

func TestRecordSplitPaymentValidations(t *testing.T) {
	engine := NewLedgerEngine()

	// Negative amount
	_, err := engine.RecordSplitPayment(-100, "m1", 10, 10, 0)
	if err != ErrInvalidAmount {
		t.Errorf("expected ErrInvalidAmount for negative gross, got %v", err)
	}

	_, err = engine.RecordSplitPayment(1000, "m1", -10, 10, 0)
	if err != ErrInvalidAmount {
		t.Errorf("expected ErrInvalidAmount for negative fee, got %v", err)
	}

	// Deductions exceed gross
	_, err = engine.RecordSplitPayment(1000, "m1", 800, 300, 0)
	if err != ErrDeductionsExceedGross {
		t.Errorf("expected ErrDeductionsExceedGross, got %v", err)
	}
}

func TestLedgerEngineConcurrency(t *testing.T) {
	engine := NewLedgerEngine()
	_, _ = engine.CreateAccount("ASSET:CONCURRENT_CASH", AccountAsset, "Concurrent Cash")
	_, _ = engine.CreateAccount("LIABILITY:CONCURRENT_PAYABLE", AccountLiability, "Concurrent Payable")

	var wg sync.WaitGroup
	numRoutines := 20
	txPerRoutine := 10

	for i := 0; i < numRoutines; i++ {
		wg.Add(1)
		go func(routineID int) {
			defer wg.Done()
			for j := 0; j < txPerRoutine; j++ {
				entries := []Entry{
					{AccountID: "ASSET:CONCURRENT_CASH", Direction: Debit, Money: NewMoney(10, "MXN")},
					{AccountID: "LIABILITY:CONCURRENT_PAYABLE", Direction: Credit, Money: NewMoney(10, "MXN")},
				}
				jnl, err := NewJournalEntry(time.Now().Format(time.RFC3339Nano), time.Now(), "concurrent tx", entries, nil)
				if err == nil {
					_ = engine.PostTransaction(jnl)
				}
				_, _ = engine.GetBalance("ASSET:CONCURRENT_CASH")
			}
		}(i)
	}

	wg.Wait()

	expectedCents := int64(numRoutines * txPerRoutine * 10)
	bal, err := engine.GetBalance("ASSET:CONCURRENT_CASH")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if bal.AmountCents != expectedCents {
		t.Errorf("expected %d cents after concurrent postings, got %d", expectedCents, bal.AmountCents)
	}
}
