package api

import (
	"context"

	"softpos/pkg/acquirer"
	"softpos/pkg/store"
)

// applyCardMoneyEvent handles refunds and chargebacks (MVP-15).
func (s *Server) applyCardMoneyEvent(_ context.Context, _ store.MoneyTx, _ *store.CardPaymentRecord, _ acquirer.Event, _ *cardEventEffects) (bool, error) {
	return false, errUnsupportedEvent
}

// applyAcquirerPayout reconciles a processor payout (MVP-16).
func (s *Server) applyAcquirerPayout(_ context.Context, _ string, _ acquirer.Event) ([]byte, error) {
	return nil, errUnsupportedEvent
}
