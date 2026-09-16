package ledger

import (
	"testing"
	"time"
)

func TestMoneyArithmetic(t *testing.T) {
	m1 := NewMoney(1000, "MXN")
	m2 := NewMoney(500, "MXN")

	// Addition
	m3, err := m1.Add(m2)
	if err != nil {
		t.Fatalf("unexpected error adding money: %v", err)
	}
	if m3.AmountCents != 1500 || m3.Currency != "MXN" {
		t.Errorf("expected 1500 MXN, got %v %s", m3.AmountCents, m3.Currency)
	}

	// Subtraction
	m4, err := m1.Sub(m2)
	if err != nil {
		t.Fatalf("unexpected error subtracting money: %v", err)
	}
	if m4.AmountCents != 500 {
		t.Errorf("expected 500 cents, got %v", m4.AmountCents)
	}

	// Currency Mismatch Error
	usd := NewMoney(100, "USD")
	if _, err := m1.Add(usd); err != ErrCurrencyMismatch {
		t.Errorf("expected ErrCurrencyMismatch, got %v", err)
	}

	// Formatting String
	if m1.String() != "$10.00 MXN" {
		t.Errorf("expected '$10.00 MXN', got '%s'", m1.String())
	}
}

func TestBalancedJournalEntry(t *testing.T) {
	entries := []Entry{
		{
			AccountID: "ASSET:ACQUIRER_RECEIVABLE",
			Direction: Debit,
			Money:     NewMoney(10000, "MXN"), // $100.00 MXN
		},
		{
			AccountID: "REVENUE:FEES",
			Direction: Credit,
			Money:     NewMoney(336, "MXN"), // $3.36 MXN
		},
		{
			AccountID: "LIABILITY:MERCHANT:123:TAX_ESCROW",
			Direction: Credit,
			Money:     NewMoney(1600, "MXN"), // $16.00 MXN
		},
		{
			AccountID: "LIABILITY:MERCHANT:123:AVAILABLE",
			Direction: Credit,
			Money:     NewMoney(8064, "MXN"), // $80.64 MXN
		},
	}

	journal, err := NewJournalEntry("tx_001", time.Now(), "SoftPOS $100 MXN", entries, nil)
	if err != nil {
		t.Fatalf("unexpected error creating balanced journal: %v", err)
	}

	if !journal.IsBalanced() {
		t.Errorf("expected journal to be balanced")
	}

	if journal.TotalDebit().AmountCents != 10000 {
		t.Errorf("expected total debit 10000, got %d", journal.TotalDebit().AmountCents)
	}
	if journal.TotalCredit().AmountCents != 10000 {
		t.Errorf("expected total credit 10000, got %d", journal.TotalCredit().AmountCents)
	}
}

func TestUnbalancedJournalEntryFails(t *testing.T) {
	entries := []Entry{
		{
			AccountID: "ASSET:ACQUIRER_RECEIVABLE",
			Direction: Debit,
			Money:     NewMoney(10000, "MXN"),
		},
		{
			AccountID: "LIABILITY:MERCHANT:123:AVAILABLE",
			Direction: Credit,
			Money:     NewMoney(9000, "MXN"), // Unbalanced by 1,000 cents
		},
	}

	_, err := NewJournalEntry("tx_unbalanced", time.Now(), "Unbalanced", entries, nil)
	if err != ErrUnbalancedJournal {
		t.Errorf("expected ErrUnbalancedJournal, got %v", err)
	}
}

func TestEmptyJournalEntryFails(t *testing.T) {
	_, err := NewJournalEntry("tx_empty", time.Now(), "Empty", []Entry{}, nil)
	if err != ErrEmptyJournal {
		t.Errorf("expected ErrEmptyJournal, got %v", err)
	}
}

func TestAccountDefinition(t *testing.T) {
	acc := Account{
		ID:   "acc_123",
		Code: "ASSET:ACQUIRER_RECEIVABLE",
		Name: "Acquirer Receivable",
		Type: AccountAsset,
	}
	if acc.Type != AccountAsset {
		t.Errorf("expected ASSET, got %v", acc.Type)
	}
	if acc.Code != "ASSET:ACQUIRER_RECEIVABLE" {
		t.Errorf("expected code ASSET:ACQUIRER_RECEIVABLE, got %s", acc.Code)
	}
}
