package ledger

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	ErrAccountNotFound = errors.New("account not found")
	ErrDuplicateAccount = errors.New("duplicate account code")
	ErrInvalidAmount    = errors.New("amount must be non-negative")
	ErrDeductionsExceedGross = errors.New("total deductions exceed gross amount")
)

// Repository defines the interface for persisting double-entry accounts and transactions (e.g. PostgreSQL with pessimistic locking).
type Repository interface {
	CreateAccount(ctx context.Context, acc Account) error
	GetAccount(ctx context.Context, code string) (Account, error)
	GetBalance(ctx context.Context, accountCode string) (Money, error)
	PostTransaction(ctx context.Context, journal *JournalEntry) error
}

type LedgerEngine struct {
	mu               sync.RWMutex
	repo             Repository
	accounts         map[string]Account
	entriesByAccount map[string][]Entry
	journals         []*JournalEntry
}

func NewLedgerEngine() *LedgerEngine {
	return &LedgerEngine{
		accounts:         make(map[string]Account),
		entriesByAccount: make(map[string][]Entry),
		journals:         []*JournalEntry{},
	}
}

func NewLedgerEngineWithRepository(repo Repository) *LedgerEngine {
	le := NewLedgerEngine()
	le.repo = repo
	return le
}

func (le *LedgerEngine) SetRepository(repo Repository) {
	le.mu.Lock()
	defer le.mu.Unlock()
	le.repo = repo
}

func (le *LedgerEngine) CreateAccount(code string, accType AccountType, name string) (Account, error) {
	le.mu.Lock()
	defer le.mu.Unlock()

	if _, exists := le.accounts[code]; exists {
		return Account{}, ErrDuplicateAccount
	}

	account := Account{
		ID:   code,
		Code: code,
		Name: name,
		Type: accType,
	}

	if le.repo != nil {
		if err := le.repo.CreateAccount(context.Background(), account); err != nil {
			return Account{}, err
		}
	}

	le.accounts[code] = account
	return account, nil
}

func (le *LedgerEngine) GetAccount(accountCode string) (Account, error) {
	le.mu.RLock()
	defer le.mu.RUnlock()

	if le.repo != nil {
		return le.repo.GetAccount(context.Background(), accountCode)
	}

	account, exists := le.accounts[accountCode]
	if !exists {
		return Account{}, ErrAccountNotFound
	}
	return account, nil
}

func (le *LedgerEngine) GetBalance(accountCode string) (Money, error) {
	le.mu.RLock()
	defer le.mu.RUnlock()

	if le.repo != nil {
		return le.repo.GetBalance(context.Background(), accountCode)
	}

	account, exists := le.accounts[accountCode]
	if !exists {
		return Money{}, ErrAccountNotFound
	}

	var balance int64
	for _, entry := range le.entriesByAccount[accountCode] {
		if entry.Direction == Debit {
			balance += entry.Money.AmountCents
		} else if entry.Direction == Credit {
			balance -= entry.Money.AmountCents
		}
	}

	if account.Type == AccountAsset || account.Type == AccountExpense {
		return NewMoney(balance, "MXN"), nil
	}
	return NewMoney(-balance, "MXN"), nil
}

func (le *LedgerEngine) PostTransaction(journal *JournalEntry) error {
	if journal == nil {
		return errors.New("journal entry cannot be nil")
	}
	if !journal.IsBalanced() {
		return ErrUnbalancedJournal
	}

	le.mu.Lock()
	defer le.mu.Unlock()

	if le.repo != nil {
		if err := le.repo.PostTransaction(context.Background(), journal); err != nil {
			return err
		}
	} else {
		// Validate all accounts exist before mutating state (atomicity)
		for _, entry := range journal.Entries {
			if _, exists := le.accounts[entry.AccountID]; !exists {
				return ErrAccountNotFound
			}
		}
	}

	for _, entry := range journal.Entries {
		le.entriesByAccount[entry.AccountID] = append(le.entriesByAccount[entry.AccountID], entry)
	}
	le.journals = append(le.journals, journal)
	return nil
}

func (le *LedgerEngine) RecordSplitPayment(grossMxnCents int64, merchantID string, platformFeeCents int64, taxEscrowCents int64, rbfSweepCents int64) (*JournalEntry, error) {
	if grossMxnCents < 0 || platformFeeCents < 0 || taxEscrowCents < 0 || rbfSweepCents < 0 {
		return nil, ErrInvalidAmount
	}
	totalDeductions := platformFeeCents + taxEscrowCents + rbfSweepCents
	if totalDeductions > grossMxnCents {
		return nil, ErrDeductionsExceedGross
	}
	netAvailableCents := grossMxnCents - totalDeductions

	entries := []Entry{
		{AccountID: "ASSET:ACQUIRER_RECEIVABLE", Direction: Debit, Money: NewMoney(grossMxnCents, "MXN")},
		{AccountID: "REVENUE:FEES", Direction: Credit, Money: NewMoney(platformFeeCents, "MXN")},
		{AccountID: fmt.Sprintf("LIABILITY:MERCHANT:%s:TAX_ESCROW", merchantID), Direction: Credit, Money: NewMoney(taxEscrowCents, "MXN")},
		{AccountID: fmt.Sprintf("ASSET:RBF_LOANS_RECEIVABLE:%s", merchantID), Direction: Credit, Money: NewMoney(rbfSweepCents, "MXN")},
		{AccountID: fmt.Sprintf("LIABILITY:MERCHANT:%s:AVAILABLE", merchantID), Direction: Credit, Money: NewMoney(netAvailableCents, "MXN")},
	}

	journalID := fmt.Sprintf("split_tx_%d", time.Now().UnixNano())
	journal, err := NewJournalEntry(journalID, time.Now(), "Split Payment", entries, nil)
	if err != nil {
		return nil, err
	}

	if err := le.PostTransaction(journal); err != nil {
		return nil, err
	}
	return journal, nil
}
