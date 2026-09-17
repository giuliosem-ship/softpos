package api

import (
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

func (s *Server) RegisterMerchant(id string, rfc string, name string, isRESICO bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.merchants[id] = Merchant{
		ID:       id,
		RFC:      rfc,
		Name:     name,
		IsRESICO: isRESICO,
	}

	// Ensure ledger accounts exist
	_, _ = s.ledger.CreateAccount(fmt.Sprintf("LIABILITY:MERCHANT:%s:AVAILABLE", id), ledger.AccountLiability, name+" Available")
	_, _ = s.ledger.CreateAccount(fmt.Sprintf("LIABILITY:MERCHANT:%s:TAX_ESCROW", id), ledger.AccountLiability, name+" Tax Escrow")
}

func (s *Server) RegisterMerchantCLABE(clabe string, merchantID string, rfc string) {
	s.RegisterMerchant(merchantID, rfc, "Merchant "+merchantID, true)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.clabeToMerchant[clabe] = merchantID
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
