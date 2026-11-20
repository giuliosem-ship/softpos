package api

import (
	"context"
	"time"

	"softpos/pkg/payments"
	"softpos/pkg/store"
)

// DefaultPlatformFeeBps is the platform tariff applied when a merchant has no
// tariff of its own (DEFAULT_PLATFORM_FEE_BPS overrides it at boot).
const DefaultPlatformFeeBps int64 = 200 // 2.00%

// bpsHalfUp returns amount * bps / 10000 rounded half-up (non-negative inputs).
func bpsHalfUp(amountCents, bps int64) int64 {
	return (amountCents*bps + 5000) / 10000
}

// ratioHalfUp returns value * num / den rounded half-up (den > 0, non-negative inputs).
func ratioHalfUp(value, num, den int64) int64 {
	return (value*num*2 + den) / (den * 2)
}

func nowUTC() time.Time { return time.Now().UTC() }

// SetMoneyStore replaces the unit of work used by every money-moving handler
// (store.SQLMoneyStore when DATABASE_URL is configured). Call at boot.
func (s *Server) SetMoneyStore(ms store.MoneyStore) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.money = ms
}

func (s *Server) moneyStore() store.MoneyStore {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.money
}

// newMemoryMoneyStore wires the in-memory unit of work to the server's current
// ledger, payment engine and merchant repository.
func (s *Server) newMemoryMoneyStore() *store.MemoryMoneyStore {
	return store.NewMemoryMoneyStore(s.ledger,
		func() payments.PaymentProcessor {
			s.mu.RLock()
			defer s.mu.RUnlock()
			return s.paymentEngine
		},
		func() store.MerchantRepository {
			s.mu.RLock()
			defer s.mu.RUnlock()
			return s.merchantRepo
		})
}

// SetDefaultPlatformFeeBps sets the tariff used for merchants without their own.
func (s *Server) SetDefaultPlatformFeeBps(bps int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.defaultFeeBps = bps
}

// platformFeeBps resolves the merchant tariff (merchant override or default).
func (s *Server) platformFeeBps(ctx context.Context, merchantID string) (int64, error) {
	bps, set, err := s.merchantRepo.GetMerchantPlatformFeeBps(ctx, merchantID)
	if err != nil {
		return 0, err
	}
	if set {
		return bps, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.defaultFeeBps, nil
}

func (s *Server) hasActiveRBFLoan(merchantID string) bool {
	loan, err := s.rbfEngine.GetMerchantLoan(merchantID)
	return err == nil && loan != nil && loan.RemainingCents() > 0
}

// rbfSweepCents derives the holdback from the in-memory RBF engine (never from
// the client); 0 without an active loan.
func (s *Server) rbfSweepCents(merchantID string, grossCents, maxDeductibleCents int64) int64 {
	if !s.hasActiveRBFLoan(merchantID) {
		return 0
	}
	return s.rbfEngine.CalculateSweep(merchantID, grossCents, maxDeductibleCents)
}
