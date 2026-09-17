package api

import (
	"errors"
	"fmt"
	"net/http"
	"sync"

	"softpos/pkg/identity"
	"softpos/pkg/ledger"
	"softpos/pkg/stp"
)

type Merchant struct {
	ID       string
	RFC      string
	Name     string
	IsRESICO bool
}

type Server struct {
	ledger          *ledger.LedgerEngine
	blacklist       *identity.BlacklistService
	stpSigner       *stp.STPSigner
	idempotency     *IdempotencyStore
	mu              sync.RWMutex
	merchants       map[string]Merchant
	clabeToMerchant map[string]string
	router          *http.ServeMux
}

func NewServer(
	ledgerEngine *ledger.LedgerEngine,
	blacklistService *identity.BlacklistService,
	stpSigner *stp.STPSigner,
) *Server {
	s := &Server{
		ledger:          ledgerEngine,
		blacklist:       blacklistService,
		stpSigner:       stpSigner,
		idempotency:     NewIdempotencyStore(),
		merchants:       make(map[string]Merchant),
		clabeToMerchant: make(map[string]string),
		router:          http.NewServeMux(),
	}

	s.routes()
	return s
}

// RegisterMerchant ensures ledger accounts are created before committing merchant state (Prepare-Commit pattern).
func (s *Server) RegisterMerchant(id string, rfc string, name string, isRESICO bool) error {
	availCode := fmt.Sprintf("LIABILITY:MERCHANT:%s:AVAILABLE", id)
	escrowCode := fmt.Sprintf("LIABILITY:MERCHANT:%s:TAX_ESCROW", id)

	// Prepare phase: validate and create accounts first
	if _, err := s.ledger.CreateAccount(availCode, ledger.AccountLiability, name+" Available"); err != nil && !errors.Is(err, ledger.ErrDuplicateAccount) {
		return err
	}
	if _, err := s.ledger.CreateAccount(escrowCode, ledger.AccountLiability, name+" Tax Escrow"); err != nil && !errors.Is(err, ledger.ErrDuplicateAccount) {
		return err
	}

	// Commit phase: persist in memory
	s.mu.Lock()
	defer s.mu.Unlock()

	s.merchants[id] = Merchant{
		ID:       id,
		RFC:      rfc,
		Name:     name,
		IsRESICO: isRESICO,
	}
	return nil
}

func (s *Server) RegisterMerchantCLABE(clabe string, merchantID string, rfc string) error {
	if err := s.RegisterMerchant(merchantID, rfc, "Merchant "+merchantID, true); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.clabeToMerchant[clabe] = merchantID
	return nil
}

func (s *Server) GetMerchant(id string) (Merchant, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	m, ok := s.merchants[id]
	return m, ok
}

func (s *Server) GetMerchantByCLABE(clabe string) (Merchant, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	mID, ok := s.clabeToMerchant[clabe]
	if !ok {
		return Merchant{}, false
	}
	m, ok := s.merchants[mID]
	return m, ok
}

func (s *Server) routes() {
	s.router.HandleFunc("POST /api/v1/webhooks/stp/abono", s.HandleSTPAbono)
	s.router.HandleFunc("POST /api/v1/payments/settle", s.HandlePaymentSettle)
	s.router.HandleFunc("POST /api/v1/disbursements", s.HandleDisbursement)
	s.router.HandleFunc("GET /api/v1/merchants/{id}/balances", s.HandleGetMerchantBalances)
}

func (s *Server) Router() http.Handler {
	return RecoveryMiddleware(LoggingMiddleware(s.router))
}
