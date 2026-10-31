package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"sync"
	"time"

	"softpos/pkg/identity"
	"softpos/pkg/ledger"
	"softpos/pkg/pac"
	"softpos/pkg/payments"
	"softpos/pkg/rbf"
	"softpos/pkg/store"
	"softpos/pkg/stp"
)

const defaultVaultMasterKeyHex = "0102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f20"

type Merchant struct {
	ID       string `json:"id"`
	RFC      string `json:"rfc"`
	Name     string `json:"name"`
	IsRESICO bool   `json:"is_resico"`
	CLABE    string `json:"clabe,omitempty"`
}

type Server struct {
	ledger            *ledger.LedgerEngine
	blacklist         *identity.BlacklistService
	stpSigner         *stp.STPSigner
	stpClient         stp.STPClient
	cepReconciler     *stp.CEPReconciler
	paymentEngine     *payments.PaymentEngine
	codiService       *payments.CoDiService
	pacOrchestrator   pac.PACClient
	rbfEngine         *rbf.Engine
	csdVault          *identity.CSDVaultService
	idempotency       *IdempotencyStore
	merchantRepo      *store.SQLMerchantRepository
	rbfRepo           *store.SQLRBFRepository
	emvPaymentRepo    *store.SQLEMVPaymentRepository
	outboxRepo        *store.SQLOutboxRepository
	staticDir         string
	mu                sync.RWMutex
	merchants         map[string]Merchant
	clabeToMerchant   map[string]string
	merchantToCLABE   map[string]string
	encryptedCSDs     map[string]identity.EncryptedCSD
	monthlySalesCents map[string]int64
	router            *http.ServeMux
}

func NewServer(
	ledgerEngine *ledger.LedgerEngine,
	blacklistService *identity.BlacklistService,
	stpSigner *stp.STPSigner,
) *Server {
	vault, _ := identity.NewCSDVaultService(defaultVaultMasterKeyHex)
	primaryPAC := pac.NewMockPACClient(false, "")
	fallbackPAC := pac.NewMockPACClient(false, "")

	s := &Server{
		ledger:            ledgerEngine,
		blacklist:         blacklistService,
		stpSigner:         stpSigner,
		stpClient:         stp.NewMockSTPClient(stpSigner, ""),
		cepReconciler:     stp.NewCEPReconciler(stp.NewMockCEPProvider()),
		paymentEngine:     payments.NewPaymentEngine(),
		codiService:       payments.NewCoDiService(),
		pacOrchestrator:   pac.NewMultiPACOrchestrator(primaryPAC, fallbackPAC, 1500*time.Millisecond),
		rbfEngine:         rbf.NewEngine(ledgerEngine),
		csdVault:          vault,
		idempotency:       NewIdempotencyStore(),
		merchants:         make(map[string]Merchant),
		clabeToMerchant:   make(map[string]string),
		merchantToCLABE:   make(map[string]string),
		encryptedCSDs:     make(map[string]identity.EncryptedCSD),
		monthlySalesCents: make(map[string]int64),
		router:            http.NewServeMux(),
	}

	s.routes()
	return s
}

func (s *Server) SetSTPClient(client stp.STPClient) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stpClient = client
}

func (s *Server) SetCEPProvider(provider stp.CEPProvider) {
	s.cepReconciler.SetProvider(provider)
}

func (s *Server) SetPACClient(client pac.PACClient) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pacOrchestrator = client
}

func (s *Server) SetMerchantRepository(repo *store.SQLMerchantRepository) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.merchantRepo = repo
}

func (s *Server) SetRBFRepository(repo *store.SQLRBFRepository) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rbfRepo = repo
}

func (s *Server) SetEMVPaymentRepository(repo *store.SQLEMVPaymentRepository) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.emvPaymentRepo = repo
}

func (s *Server) SetOutboxRepository(repo *store.SQLOutboxRepository) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.outboxRepo = repo
}

func (s *Server) SetIdempotencyStore(idemp *IdempotencyStore) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.idempotency = idemp
}

func (s *Server) IdempotencyStore() *IdempotencyStore {
	return s.idempotency
}

func (s *Server) SetStaticDir(dir string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.staticDir = dir
}

// RegisterMerchantCSD encrypts and stores a merchant's CSD certificate and private key in the envelope-encrypted vault.
func (s *Server) RegisterMerchantCSD(merchantID string, keyDER []byte, certDER []byte) error {
	enc, err := s.csdVault.StoreCSD(merchantID, certDER, keyDER, "vault-managed")
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.encryptedCSDs[merchantID] = enc
	s.mu.Unlock()

	if s.merchantRepo != nil {
		encBytes, _ := json.Marshal(enc)
		_ = s.merchantRepo.SaveMerchantCSD(context.Background(), merchantID, encBytes)
	}
	return nil
}

func (s *Server) getMerchantCSD(merchantID string) (identity.DecryptedCSD, error) {
	s.mu.RLock()
	enc, ok := s.encryptedCSDs[merchantID]
	s.mu.RUnlock()
	if !ok && s.merchantRepo != nil {
		csdBytes, found, err := s.merchantRepo.GetMerchantCSD(context.Background(), merchantID)
		if err == nil && found {
			var loaded identity.EncryptedCSD
			if json.Unmarshal(csdBytes, &loaded) == nil {
				s.mu.Lock()
				s.encryptedCSDs[merchantID] = loaded
				s.mu.Unlock()
				enc = loaded
				ok = true
			}
		}
	}
	if !ok {
		return identity.DecryptedCSD{}, errors.New("merchant CSD not registered")
	}
	return s.csdVault.DecryptCSDForSigning(enc)
}

// RegisterMerchant ensures ledger accounts are created before committing merchant state (Prepare-Commit pattern).
func (s *Server) RegisterMerchant(id string, rfc string, name string, isRESICO bool) error {
	availCode := fmt.Sprintf("LIABILITY:MERCHANT:%s:AVAILABLE", id)
	escrowCode := fmt.Sprintf("LIABILITY:MERCHANT:%s:TAX_ESCROW", id)
	rbfCode := fmt.Sprintf("ASSET:RBF_LOANS_RECEIVABLE:%s", id)

	// Prepare phase: validate and create merchant-specific accounts first
	if _, err := s.ledger.CreateAccount(availCode, ledger.AccountLiability, name+" Available"); err != nil && !errors.Is(err, ledger.ErrDuplicateAccount) {
		return err
	}
	if _, err := s.ledger.CreateAccount(escrowCode, ledger.AccountLiability, name+" Tax Escrow"); err != nil && !errors.Is(err, ledger.ErrDuplicateAccount) {
		return err
	}
	if _, err := s.ledger.CreateAccount(rbfCode, ledger.AccountAsset, name+" RBF Loans Receivable"); err != nil && !errors.Is(err, ledger.ErrDuplicateAccount) {
		return err
	}

	// Commit phase: persist in memory and optional SQL repository
	s.mu.Lock()
	s.merchants[id] = Merchant{
		ID:       id,
		RFC:      rfc,
		Name:     name,
		IsRESICO: isRESICO,
	}
	s.mu.Unlock()

	if s.merchantRepo != nil {
		_ = s.merchantRepo.SaveMerchant(context.Background(), store.MerchantRecord{
			ID:       id,
			RFC:      rfc,
			Name:     name,
			IsRESICO: isRESICO,
		})
	}
	return nil
}

func (s *Server) RegisterMerchantCLABE(clabe string, merchantID string, rfc string) error {
	if err := s.RegisterMerchant(merchantID, rfc, "Merchant "+merchantID, true); err != nil {
		return err
	}

	s.mu.Lock()
	s.clabeToMerchant[clabe] = merchantID
	s.merchantToCLABE[merchantID] = clabe
	s.mu.Unlock()

	if s.merchantRepo != nil {
		_ = s.merchantRepo.SetMerchantCLABE(context.Background(), merchantID, clabe)
	}
	return nil
}

func (s *Server) GetMerchant(id string) (Merchant, bool) {
	s.mu.RLock()
	m, ok := s.merchants[id]
	s.mu.RUnlock()
	if ok {
		return m, true
	}

	if s.merchantRepo != nil {
		rec, found, err := s.merchantRepo.GetMerchant(context.Background(), id)
		if err == nil && found {
			merchant := Merchant{
				ID:       rec.ID,
				RFC:      rec.RFC,
				Name:     rec.Name,
				IsRESICO: rec.IsRESICO,
				CLABE:    rec.VirtualCLABE,
			}
			s.mu.Lock()
			s.merchants[id] = merchant
			if rec.VirtualCLABE != "" {
				s.clabeToMerchant[rec.VirtualCLABE] = id
				s.merchantToCLABE[id] = rec.VirtualCLABE
			}
			s.mu.Unlock()
			return merchant, true
		}
	}
	return Merchant{}, false
}

func (s *Server) GetMerchantByCLABE(clabe string) (Merchant, bool) {
	s.mu.RLock()
	mID, ok := s.clabeToMerchant[clabe]
	var m Merchant
	if ok {
		m = s.merchants[mID]
	}
	s.mu.RUnlock()
	if ok {
		return m, true
	}

	if s.merchantRepo != nil {
		rec, found, err := s.merchantRepo.GetMerchantByCLABE(context.Background(), clabe)
		if err == nil && found {
			merchant := Merchant{
				ID:       rec.ID,
				RFC:      rec.RFC,
				Name:     rec.Name,
				IsRESICO: rec.IsRESICO,
				CLABE:    rec.VirtualCLABE,
			}
			s.mu.Lock()
			s.merchants[rec.ID] = merchant
			s.clabeToMerchant[clabe] = rec.ID
			s.merchantToCLABE[rec.ID] = clabe
			s.mu.Unlock()
			return merchant, true
		}
	}
	return Merchant{}, false
}

func (s *Server) GetMerchantCLABE(merchantID string) (string, bool) {
	s.mu.RLock()
	clabe, ok := s.merchantToCLABE[merchantID]
	s.mu.RUnlock()
	if ok {
		return clabe, true
	}

	if s.merchantRepo != nil {
		c, found, err := s.merchantRepo.GetMerchantCLABE(context.Background(), merchantID)
		if err == nil && found {
			s.mu.Lock()
			s.merchantToCLABE[merchantID] = c
			s.clabeToMerchant[c] = merchantID
			s.mu.Unlock()
			return c, true
		}
	}
	return "", false
}

func monthlySalesKey(merchantID string) string {
	return fmt.Sprintf("%s:%s", merchantID, time.Now().UTC().Format("2006-01"))
}

// GetMerchantMonthlySalesCents returns the accumulated monthly base sales (in cents) for a merchant in the current calendar month.
func (s *Server) GetMerchantMonthlySalesCents(merchantID string) int64 {
	if s.merchantRepo != nil {
		yearMonth := time.Now().UTC().Format("2006-01")
		cents, err := s.merchantRepo.GetMonthlySalesCents(context.Background(), merchantID, yearMonth)
		if err == nil && cents > 0 {
			return cents
		}
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.monthlySalesCents[monthlySalesKey(merchantID)]
}

// AddMerchantMonthlySalesCents atomically increments a merchant's accumulated monthly base sales (in cents).
func (s *Server) AddMerchantMonthlySalesCents(merchantID string, baseAmountCents int64) {
	if baseAmountCents <= 0 {
		return
	}
	if s.merchantRepo != nil {
		yearMonth := time.Now().UTC().Format("2006-01")
		_, _ = s.merchantRepo.AddMonthlySalesCents(context.Background(), merchantID, yearMonth, baseAmountCents)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := monthlySalesKey(merchantID)
	s.monthlySalesCents[key] += baseAmountCents
}

func (s *Server) HandleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":    "UP",
		"timestamp": time.Now().UTC().Format(time.RFC3339),
		"service":   "SoftPOS Hybrid Fiscal Platform",
	})
}

func (s *Server) HandleGetMerchant(w http.ResponseWriter, r *http.Request) {
	merchantID := r.PathValue("id")
	if merchantID == "" {
		http.Error(w, `{"error":"MISSING_MERCHANT_ID"}`, http.StatusBadRequest)
		return
	}
	m, exists := s.GetMerchant(merchantID)
	if !exists {
		http.Error(w, `{"error":"MERCHANT_NOT_FOUND"}`, http.StatusNotFound)
		return
	}
	clabe, _ := s.GetMerchantCLABE(merchantID)
	m.CLABE = clabe

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(m)
}

type RegisterMerchantRequest struct {
	ID           string `json:"id"`
	RFC          string `json:"rfc"`
	Name         string `json:"name"`
	IsRESICO     bool   `json:"is_resico"`
	VirtualCLABE string `json:"virtual_clabe,omitempty"`
}

func (s *Server) HandleRegisterMerchant(w http.ResponseWriter, r *http.Request) {
	var req RegisterMerchantRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"INVALID_JSON"}`, http.StatusBadRequest)
		return
	}
	if req.ID == "" || req.RFC == "" || req.Name == "" {
		http.Error(w, `{"error":"MISSING_REQUIRED_FIELDS"}`, http.StatusBadRequest)
		return
	}

	if err := s.RegisterMerchant(req.ID, req.RFC, req.Name, req.IsRESICO); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"REGISTRATION_FAILED","detail":%q}`, err.Error()), http.StatusBadRequest)
		return
	}

	if req.VirtualCLABE != "" {
		_ = s.RegisterMerchantCLABE(req.VirtualCLABE, req.ID, req.RFC)
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"status":      "CREATED",
		"merchant_id": req.ID,
	})
}

func (s *Server) routes() {
	// Health and System Status
	s.router.HandleFunc("GET /healthz", s.HandleHealthz)

	// Merchant Catalog
	s.router.HandleFunc("GET /api/v1/merchants/{id}", s.HandleGetMerchant)
	s.router.HandleFunc("POST /api/v1/merchants", s.HandleRegisterMerchant)

	// STP Inbound & Outbound Disbursements + Banxico CEP Reconciliation
	s.router.HandleFunc("POST /api/v1/webhooks/stp/abono", s.HandleSTPAbono)
	s.router.HandleFunc("POST /api/v1/disbursements", s.HandleDisbursement)
	s.router.HandleFunc("GET /api/v1/disbursements/{clave}", s.HandleGetDisbursement)
	s.router.HandleFunc("POST /api/v1/disbursements/{clave}/reconcile-cep", s.HandleReconcileCEP)

	// Payments: EMV Contactless, CoDi QR & Tax-Split Settlement
	s.router.HandleFunc("POST /api/v1/payments/emv/authorize", s.HandleEMVAuthorize)
	s.router.HandleFunc("POST /api/v1/payments/emv/reverse", s.HandleEMVReverse)
	s.router.HandleFunc("POST /api/v1/payments/codi/qr", s.HandleCoDiCreateQR)
	s.router.HandleFunc("POST /api/v1/webhooks/codi/confirm", s.HandleCoDiWebhookConfirm)
	s.router.HandleFunc("POST /api/v1/payments/settle", s.HandlePaymentSettle)

	// CFDI 4.0 Invoicing & Multi-PAC Stamping
	s.router.HandleFunc("POST /api/v1/invoices/cfdi", s.HandleStampIndividualCFDI)
	s.router.HandleFunc("POST /api/v1/invoices/global", s.HandleStampFacturaGlobal)

	// Revenue-Based Financing (RBF) & Merchant Balances
	s.router.HandleFunc("POST /api/v1/rbf/loans", s.HandleOriginateRBFLoan)
	s.router.HandleFunc("GET /api/v1/merchants/{id}/rbf", s.HandleGetMerchantRBFLoan)
	s.router.HandleFunc("GET /api/v1/merchants/{id}/balances", s.HandleGetMerchantBalances)

	// Serve Static POS Web App dynamically
	s.router.HandleFunc("GET /pos/", func(w http.ResponseWriter, r *http.Request) {
		s.mu.RLock()
		dir := s.staticDir
		s.mu.RUnlock()
		if dir == "" {
			if _, err := os.Stat("web/pos"); err == nil {
				dir = "web/pos"
			} else if _, err := os.Stat("../../web/pos"); err == nil {
				dir = "../../web/pos"
			}
		}
		http.StripPrefix("/pos/", http.FileServer(http.Dir(dir))).ServeHTTP(w, r)
	})
	s.router.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/pos/", http.StatusFound)
	})
}

func (s *Server) Router() http.Handler {
	return RecoveryMiddleware(LoggingMiddleware(s.router))
}
