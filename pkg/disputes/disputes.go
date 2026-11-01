package disputes

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"softpos/pkg/ledger"
)

var (
	ErrDisputeNotFound          = errors.New("dispute record not found")
	ErrDisputeAlreadyExists      = errors.New("dispute ID already exists")
	ErrInvalidAmount             = errors.New("dispute amount must be greater than zero")
	ErrInvalidStateTransition    = errors.New("invalid dispute lifecycle state transition")
	ErrDisputeAlreadyResolved    = errors.New("dispute has already been resolved")
	ErrInsufficientBalance       = errors.New("insufficient merchant available balance")
)

type DisputeStatus string

const (
	DisputeStatusOpened        DisputeStatus = "CHARGEBACK_OPENED"
	DisputeStatusDocsRequired  DisputeStatus = "DOCS_REQUIRED"
	DisputeStatusRepresentment DisputeStatus = "REPRESENTMENT_SUBMITTED"
	DisputeStatusWon           DisputeStatus = "WON"
	DisputeStatusLost          DisputeStatus = "LOST"
)

type Dispute struct {
	ID             string
	TransactionID  string
	MID            string
	AmountCents    int64
	Currency       string
	ReasonCode     string
	Status         DisputeStatus
	OpenedAt       time.Time
	UpdatedAt      time.Time
	ResolvedAt     *time.Time
	EvidenceDocs   []string
}

type RollingReserve struct {
	ID            string
	MID           string
	GrossCents    int64
	ReserveBps    int64
	ReservedCents int64
	CreatedAt     time.Time
	MatureAt      time.Time
	Released      bool
	ReleasedAt    *time.Time
}

type DisputeEngine struct {
	ledger   *ledger.LedgerEngine
	mu       sync.RWMutex
	disputes map[string]*Dispute
	reserves map[string]*RollingReserve
}

func NewDisputeEngine(ledgerEngine *ledger.LedgerEngine) *DisputeEngine {
	return &DisputeEngine{
		ledger:   ledgerEngine,
		disputes: make(map[string]*Dispute),
		reserves: make(map[string]*RollingReserve),
	}
}

func roundHalfUp(numerator, denominator int64) int64 {
	if numerator < 0 {
		return (numerator - denominator/2) / denominator
	}
	return (numerator + denominator/2) / denominator
}

// OpenDispute opens a new chargeback dispute, freezing the merchant's available balance to dispute escrow.
func (e *DisputeEngine) OpenDispute(ctx context.Context, id, txID, mid string, amountCents int64, reasonCode string) (*Dispute, error) {
	if amountCents <= 0 {
		return nil, ErrInvalidAmount
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	if _, exists := e.disputes[id]; exists {
		return nil, ErrDisputeAlreadyExists
	}

	availAcc := fmt.Sprintf("LIABILITY:MERCHANT:%s:AVAILABLE", mid)
	escrowAcc := fmt.Sprintf("LIABILITY:MERCHANT:%s:DISPUTE_ESCROW", mid)
	now := time.Now()

	// Post double-entry journal to isolate funds in escrow
	journal, err := ledger.NewJournalEntry(
		fmt.Sprintf("DISP-OPEN-%s", id),
		now,
		fmt.Sprintf("Dispute %s frozen to escrow", id),
		[]ledger.Entry{
			{AccountID: availAcc, Direction: ledger.Debit, Money: ledger.NewMoney(amountCents, "MXN")},
			{AccountID: escrowAcc, Direction: ledger.Credit, Money: ledger.NewMoney(amountCents, "MXN")},
		},
		map[string]string{
			"dispute_id": id,
			"tx_id":      txID,
			"mid":        mid,
		},
	)
	if err != nil {
		return nil, err
	}

	if err := e.ledger.PostTransaction(journal); err != nil {
		return nil, err
	}

	disp := &Dispute{
		ID:            id,
		TransactionID: txID,
		MID:           mid,
		AmountCents:   amountCents,
		Currency:      "MXN",
		ReasonCode:    reasonCode,
		Status:        DisputeStatusOpened,
		OpenedAt:      now,
		UpdatedAt:     now,
		EvidenceDocs:  make([]string, 0),
	}
	e.disputes[id] = disp
	return disp, nil
}

// RequestEvidence marks the dispute as requiring documentation from merchant.
func (e *DisputeEngine) RequestEvidence(id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	disp, exists := e.disputes[id]
	if !exists {
		return ErrDisputeNotFound
	}
	if disp.Status != DisputeStatusOpened {
		return ErrInvalidStateTransition
	}

	disp.Status = DisputeStatusDocsRequired
	disp.UpdatedAt = time.Now()
	return nil
}

// SubmitRepresentment submits counter-evidence against the chargeback.
func (e *DisputeEngine) SubmitRepresentment(id string, evidenceDocs []string) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	disp, exists := e.disputes[id]
	if !exists {
		return ErrDisputeNotFound
	}
	if disp.Status != DisputeStatusOpened && disp.Status != DisputeStatusDocsRequired {
		return ErrInvalidStateTransition
	}

	disp.Status = DisputeStatusRepresentment
	disp.EvidenceDocs = append(disp.EvidenceDocs, evidenceDocs...)
	disp.UpdatedAt = time.Now()
	return nil
}

// ResolveDispute finalizes a dispute.
// If won: releases escrow back to merchant available balance.
// If lost: liquidates escrow against issuer clearing asset.
func (e *DisputeEngine) ResolveDispute(ctx context.Context, id string, won bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()

	disp, exists := e.disputes[id]
	if !exists {
		return ErrDisputeNotFound
	}
	if disp.Status == DisputeStatusWon || disp.Status == DisputeStatusLost {
		return ErrDisputeAlreadyResolved
	}

	now := time.Now()
	availAcc := fmt.Sprintf("LIABILITY:MERCHANT:%s:AVAILABLE", disp.MID)
	escrowAcc := fmt.Sprintf("LIABILITY:MERCHANT:%s:DISPUTE_ESCROW", disp.MID)
	clearingAcc := "ASSET:SETTLEMENT:ISSUER_CLEARING"

	var journal *ledger.JournalEntry
	var err error

	if won {
		journal, err = ledger.NewJournalEntry(
			fmt.Sprintf("DISP-RESOLVE-WON-%s", id),
			now,
			fmt.Sprintf("Dispute %s won: returned to available", id),
			[]ledger.Entry{
				{AccountID: escrowAcc, Direction: ledger.Debit, Money: ledger.NewMoney(disp.AmountCents, "MXN")},
				{AccountID: availAcc, Direction: ledger.Credit, Money: ledger.NewMoney(disp.AmountCents, "MXN")},
			},
			map[string]string{"dispute_id": id, "resolution": "WON"},
		)
	} else {
		journal, err = ledger.NewJournalEntry(
			fmt.Sprintf("DISP-RESOLVE-LOST-%s", id),
			now,
			fmt.Sprintf("Dispute %s lost: routed to issuer clearing", id),
			[]ledger.Entry{
				{AccountID: escrowAcc, Direction: ledger.Debit, Money: ledger.NewMoney(disp.AmountCents, "MXN")},
				{AccountID: clearingAcc, Direction: ledger.Credit, Money: ledger.NewMoney(disp.AmountCents, "MXN")},
			},
			map[string]string{"dispute_id": id, "resolution": "LOST"},
		)
	}

	if err != nil {
		return err
	}

	if err := e.ledger.PostTransaction(journal); err != nil {
		return err
	}

	if won {
		disp.Status = DisputeStatusWon
	} else {
		disp.Status = DisputeStatusLost
	}
	disp.UpdatedAt = now
	disp.ResolvedAt = &now

	return nil
}

// RetainRollingReserve retains a configured percentage (bps) of gross volume for high-risk merchants.
func (e *DisputeEngine) RetainRollingReserve(ctx context.Context, mid string, grossCents, reserveBps int64, holdDays int) (*RollingReserve, error) {
	if grossCents <= 0 || reserveBps <= 0 || holdDays <= 0 {
		return nil, ErrInvalidAmount
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	reservedCents := roundHalfUp(grossCents*reserveBps, 10000)
	now := time.Now()
	matureAt := now.Add(time.Duration(holdDays) * 24 * time.Hour)
	reserveID := fmt.Sprintf("RES-%s-%d", mid, now.UnixNano())

	availAcc := fmt.Sprintf("LIABILITY:MERCHANT:%s:AVAILABLE", mid)
	reserveAcc := fmt.Sprintf("LIABILITY:MERCHANT:%s:ROLLING_RESERVE", mid)

	journal, err := ledger.NewJournalEntry(
		fmt.Sprintf("JOURNAL-%s", reserveID),
		now,
		fmt.Sprintf("Rolling reserve retained (%d bps)", reserveBps),
		[]ledger.Entry{
			{AccountID: availAcc, Direction: ledger.Debit, Money: ledger.NewMoney(reservedCents, "MXN")},
			{AccountID: reserveAcc, Direction: ledger.Credit, Money: ledger.NewMoney(reservedCents, "MXN")},
		},
		map[string]string{"reserve_id": reserveID, "mid": mid},
	)
	if err != nil {
		return nil, err
	}

	if err := e.ledger.PostTransaction(journal); err != nil {
		return nil, err
	}

	reserve := &RollingReserve{
		ID:            reserveID,
		MID:           mid,
		GrossCents:    grossCents,
		ReserveBps:    reserveBps,
		ReservedCents: reservedCents,
		CreatedAt:     now,
		MatureAt:      matureAt,
		Released:      false,
	}
	e.reserves[reserveID] = reserve

	return reserve, nil
}

// ReleaseMatureReserves releases all mature reserves past the given cutoff timestamp back to merchant available balance.
func (e *DisputeEngine) ReleaseMatureReserves(ctx context.Context, now time.Time) (int64, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	var totalReleased int64

	for _, res := range e.reserves {
		if !res.Released && (now.After(res.MatureAt) || now.Equal(res.MatureAt)) {
			availAcc := fmt.Sprintf("LIABILITY:MERCHANT:%s:AVAILABLE", res.MID)
			reserveAcc := fmt.Sprintf("LIABILITY:MERCHANT:%s:ROLLING_RESERVE", res.MID)

			journal, err := ledger.NewJournalEntry(
				fmt.Sprintf("RELEASE-%s", res.ID),
				now,
				fmt.Sprintf("Mature rolling reserve released: %s", res.ID),
				[]ledger.Entry{
					{AccountID: reserveAcc, Direction: ledger.Debit, Money: ledger.NewMoney(res.ReservedCents, "MXN")},
					{AccountID: availAcc, Direction: ledger.Credit, Money: ledger.NewMoney(res.ReservedCents, "MXN")},
				},
				map[string]string{"reserve_id": res.ID, "mid": res.MID},
			)
			if err != nil {
				return totalReleased, err
			}

			if err := e.ledger.PostTransaction(journal); err != nil {
				return totalReleased, err
			}

			res.Released = true
			res.ReleasedAt = &now
			totalReleased += res.ReservedCents
		}
	}

	return totalReleased, nil
}

// GetDispute returns a dispute by its ID.
func (e *DisputeEngine) GetDispute(id string) (*Dispute, error) {
	e.mu.RLock()
	defer e.mu.RUnlock()

	disp, exists := e.disputes[id]
	if !exists {
		return nil, ErrDisputeNotFound
	}
	return disp, nil
}
