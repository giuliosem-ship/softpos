package rbf

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"softpos/pkg/ledger"
)

var (
	ErrInvalidPrincipal     = errors.New("principal amount must be positive")
	ErrInvalidFeeRate       = errors.New("fixed fee basis points cannot be negative")
	ErrInvalidHoldbackRate  = errors.New("holdback basis points must be between 1 and 5000")
	ErrActiveLoanExists     = errors.New("merchant already has an active RBF loan")
	ErrLoanNotFound         = errors.New("no RBF loan found for merchant")
	ErrInvalidRepayment     = errors.New("repayment amount is invalid or exceeds remaining cap")
	ErrMissingMerchantID    = errors.New("merchant ID is required")
)

type LoanStatus string

const (
	LoanActive  LoanStatus = "ACTIVE"
	LoanPaidOff LoanStatus = "PAID_OFF"
)

// RBFLoan represents a Revenue-Based Financing advance with a fixed fee and automatic sales holdback.
type RBFLoan struct {
	ID                string     `json:"loan_id"`
	MerchantID        string     `json:"merchant_id"`
	PrincipalCents    int64      `json:"principal_cents"`
	FixedFeeBps       int64      `json:"fixed_fee_bps"`
	FeeCents          int64      `json:"fee_cents"`
	RepaymentCapCents int64      `json:"repayment_cap_cents"`
	HoldbackBps       int64      `json:"holdback_bps"`
	RepaidCents       int64      `json:"repaid_cents"`
	Status            LoanStatus `json:"status"`
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
}

// RemainingCents returns the remaining balance required to reach the repayment cap.
func (l *RBFLoan) RemainingCents() int64 {
	rem := l.RepaymentCapCents - l.RepaidCents
	if rem < 0 {
		return 0
	}
	return rem
}

// Engine manages the lifecycle of Revenue-Based Financing loans and holdback sweeps.
type Engine struct {
	mu            sync.RWMutex
	ledger        *ledger.LedgerEngine
	activeByMerch map[string]*RBFLoan
	loansByID     map[string]*RBFLoan
	historyByMerch map[string][]*RBFLoan
}

func NewEngine(ledgerEngine *ledger.LedgerEngine) *Engine {
	return &Engine{
		ledger:         ledgerEngine,
		activeByMerch:  make(map[string]*RBFLoan),
		loansByID:      make(map[string]*RBFLoan),
		historyByMerch: make(map[string][]*RBFLoan),
	}
}

// OriginateLoan grants a new RBF capital advance to a merchant, records the double-entry journal in the ledger,
// and activates automatic percentage holdbacks on future sales up to RepaymentCapCents.
func (e *Engine) OriginateLoan(merchantID string, principalCents int64, fixedFeeBps int64, holdbackBps int64) (*RBFLoan, error) {
	if merchantID == "" {
		return nil, ErrMissingMerchantID
	}
	if principalCents <= 0 {
		return nil, ErrInvalidPrincipal
	}
	if fixedFeeBps < 0 {
		return nil, ErrInvalidFeeRate
	}
	if holdbackBps <= 0 || holdbackBps > 5000 {
		return nil, ErrInvalidHoldbackRate
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	if existing, ok := e.activeByMerch[merchantID]; ok && existing.Status == LoanActive {
		return nil, ErrActiveLoanExists
	}

	// Pure integer basis-point calculation with half-up rounding: (principal * bps + 5000) / 10000
	feeCents := (principalCents*fixedFeeBps + 5000) / 10000
	repaymentCapCents := principalCents + feeCents

	now := time.Now().UTC()
	loanID := fmt.Sprintf("rbf_%s_%d", merchantID, now.UnixNano())

	if e.ledger != nil {
		rbfAccount := fmt.Sprintf("ASSET:RBF_LOANS_RECEIVABLE:%s", merchantID)
		availAccount := fmt.Sprintf("LIABILITY:MERCHANT:%s:AVAILABLE", merchantID)
		feesAccount := "REVENUE:FEES"

		_, _ = e.ledger.CreateAccount(rbfAccount, ledger.AccountAsset, merchantID+" RBF Loans Receivable")
		_, _ = e.ledger.CreateAccount(availAccount, ledger.AccountLiability, merchantID+" Available")
		_, _ = e.ledger.CreateAccount(feesAccount, ledger.AccountRevenue, "Platform Fee Revenue")

		entries := []ledger.Entry{
			{
				AccountID: rbfAccount,
				Direction: ledger.Debit,
				Money:     ledger.NewMoney(repaymentCapCents, "MXN"),
			},
			{
				AccountID: availAccount,
				Direction: ledger.Credit,
				Money:     ledger.NewMoney(principalCents, "MXN"),
			},
		}
		if feeCents > 0 {
			entries = append(entries, ledger.Entry{
				AccountID: feesAccount,
				Direction: ledger.Credit,
				Money:     ledger.NewMoney(feeCents, "MXN"),
			})
		}

		journal, err := ledger.NewJournalEntry(
			fmt.Sprintf("jnl_%s", loanID),
			now,
			fmt.Sprintf("RBF Loan Origination %s", loanID),
			entries,
			map[string]string{
				"loan_id":     loanID,
				"merchant_id": merchantID,
			},
		)
		if err != nil {
			return nil, err
		}
		if err := e.ledger.PostTransaction(journal); err != nil {
			return nil, err
		}
	}

	loan := &RBFLoan{
		ID:                loanID,
		MerchantID:        merchantID,
		PrincipalCents:    principalCents,
		FixedFeeBps:       fixedFeeBps,
		FeeCents:          feeCents,
		RepaymentCapCents: repaymentCapCents,
		HoldbackBps:       holdbackBps,
		RepaidCents:       0,
		Status:            LoanActive,
		CreatedAt:         now,
		UpdatedAt:         now,
	}

	e.activeByMerch[merchantID] = loan
	e.loansByID[loanID] = loan
	e.historyByMerch[merchantID] = append(e.historyByMerch[merchantID], loan)

	copyLoan := *loan
	return &copyLoan, nil
}

// GetActiveLoan returns a copy of the merchant's active RBF loan, or the most recent paid-off loan if none is active.
func (e *Engine) GetMerchantLoan(merchantID string) (*RBFLoan, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	if active, ok := e.activeByMerch[merchantID]; ok {
		cp := *active
		return &cp, nil
	}

	history := e.historyByMerch[merchantID]
	if len(history) == 0 {
		return nil, ErrLoanNotFound
	}
	latest := history[len(history)-1]
	cp := *latest
	return &cp, nil
}

// CalculateSweep computes the automatic holdback amount in cents for a given gross sale,
// strictly capped by both the loan's remaining repayment cap and maxDeductibleCents.
func (e *Engine) CalculateSweep(merchantID string, grossSaleCents int64, maxDeductibleCents int64) int64 {
	if grossSaleCents <= 0 || maxDeductibleCents <= 0 {
		return 0
	}

	e.mu.RLock()
	defer e.mu.RUnlock()

	loan, ok := e.activeByMerch[merchantID]
	if !ok || loan.Status != LoanActive {
		return 0
	}

	remaining := loan.RemainingCents()
	if remaining <= 0 {
		return 0
	}

	sweep := (grossSaleCents*loan.HoldbackBps + 5000) / 10000
	if sweep > remaining {
		sweep = remaining
	}
	if sweep > maxDeductibleCents {
		sweep = maxDeductibleCents
	}
	if sweep < 0 {
		return 0
	}
	return sweep
}

// ApplyRepayment records a repayment sweep against the merchant's active loan and transitions it to PAID_OFF
// once RepaidCents reaches RepaymentCapCents.
func (e *Engine) ApplyRepayment(merchantID string, sweepCents int64) (*RBFLoan, error) {
	if sweepCents < 0 {
		return nil, ErrInvalidRepayment
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	loan, ok := e.activeByMerch[merchantID]
	if !ok || loan.Status != LoanActive {
		return nil, ErrLoanNotFound
	}

	if sweepCents > loan.RemainingCents() {
		return nil, ErrInvalidRepayment
	}

	loan.RepaidCents += sweepCents
	loan.UpdatedAt = time.Now().UTC()

	if loan.RepaidCents >= loan.RepaymentCapCents {
		loan.Status = LoanPaidOff
		delete(e.activeByMerch, merchantID)
	}

	cp := *loan
	return &cp, nil
}
