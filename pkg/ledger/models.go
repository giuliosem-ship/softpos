package ledger

import (
	"errors"
	"fmt"
	"time"
)

// Sentinel errors
var (
	ErrCurrencyMismatch = errors.New("currency mismatch")
	ErrUnbalancedJournal = errors.New("unbalanced journal")
	ErrEmptyJournal     = errors.New("empty journal")
)

// Money represents an amount of money with a specific currency.
type Money struct {
	AmountCents int64
	Currency    string
}

// NewMoney creates a new Money instance.
func NewMoney(amountCents int64, currency string) Money {
	return Money{
		AmountCents: amountCents,
		Currency:    currency,
	}
}

// Add returns the sum of two Money instances.
func (m Money) Add(other Money) (Money, error) {
	if m.Currency != other.Currency {
		return Money{}, ErrCurrencyMismatch
	}
	return Money{
		AmountCents: m.AmountCents + other.AmountCents,
		Currency:    m.Currency,
	}, nil
}

// Sub returns the difference between two Money instances.
func (m Money) Sub(other Money) (Money, error) {
	if m.Currency != other.Currency {
		return Money{}, ErrCurrencyMismatch
	}
	return Money{
		AmountCents: m.AmountCents - other.AmountCents,
		Currency:    m.Currency,
	}, nil
}

// String formats the Money instance as a string.
func (m Money) String() string {
	return fmt.Sprintf("$%0.2f %s", float64(m.AmountCents)/100, m.Currency)
}

// AccountType represents the type of an account.
type AccountType string

const (
	AccountAsset    AccountType = "ASSET"
	AccountLiability AccountType = "LIABILITY"
	AccountEquity   AccountType = "EQUITY"
	AccountRevenue  AccountType = "REVENUE"
	AccountExpense  AccountType = "EXPENSE"
)

// Direction represents the direction of an entry.
type Direction string

const (
	Debit  Direction = "DEBIT"
	Credit Direction = "CREDIT"
)

// Account represents an account in the ledger.
type Account struct {
	ID   string
	Code string
	Name string
	Type AccountType
}

// Entry represents a single entry in a journal.
type Entry struct {
	AccountID string
	Direction Direction
	Money     Money
}

// JournalEntry represents a journal entry with multiple entries.
type JournalEntry struct {
	ID        string
	Timestamp time.Time
	Description string
	Entries   []Entry
	Metadata  map[string]string
}

// NewJournalEntry creates a new JournalEntry instance.
func NewJournalEntry(id string, timestamp time.Time, description string, entries []Entry, metadata map[string]string) (*JournalEntry, error) {
	if len(entries) == 0 {
		return nil, ErrEmptyJournal
	}

	currency := entries[0].Money.Currency
	for _, entry := range entries {
		if entry.Money.Currency != currency {
			return nil, ErrCurrencyMismatch
		}
	}

	if !isBalanced(entries) {
		return nil, ErrUnbalancedJournal
	}

	return &JournalEntry{
		ID:        id,
		Timestamp: timestamp,
		Description: description,
		Entries:   entries,
		Metadata:  metadata,
	}, nil
}

// IsBalanced checks if the journal entry is balanced.
func (j JournalEntry) IsBalanced() bool {
	return isBalanced(j.Entries)
}

// TotalDebit calculates the total debit amount.
func (j JournalEntry) TotalDebit() Money {
	return sumEntries(j.Entries, Debit)
}

// TotalCredit calculates the total credit amount.
func (j JournalEntry) TotalCredit() Money {
	return sumEntries(j.Entries, Credit)
}

func isBalanced(entries []Entry) bool {
	totalDebit := sumEntries(entries, Debit)
	totalCredit := sumEntries(entries, Credit)
	return totalDebit.AmountCents == totalCredit.AmountCents
}

func sumEntries(entries []Entry, direction Direction) Money {
	if len(entries) == 0 {
		return Money{}
	}
	currency := entries[0].Money.Currency
	var totalCents int64
	for _, entry := range entries {
		if entry.Direction == direction {
			totalCents += entry.Money.AmountCents
		}
	}
	return NewMoney(totalCents, currency)
}
