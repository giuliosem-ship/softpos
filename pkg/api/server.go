package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"sync"
	"time"

	"softpos/pkg/auth"
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
	paymentEngine     payments.PaymentProcessor
	codiService       *payments.CoDiService
	pacOrchestrator   pac.PACClient
	rbfEngine         *rbf.Engine
	csdVault          *identity.CSDVaultService
	idempotency       *IdempotencyStore
	merchantRepo      store.MerchantRepository
	rbfRepo           *store.SQLRBFRepository
	emvPaymentRepo    *store.SQLEMVPaymentRepository
	outboxRepo        *store.SQLOutboxRepository
	auth              *auth.Service
	authLimiter       *RateLimiter
	integrityVerifier *identity.PlayIntegrityVerifier
	devMode           bool
	staticDir         string
	mu                sync.RWMutex
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
		merchantRepo:      store.NewMemoryMerchantRepository(),
		router:            http.NewServeMux(),
	}
	s.auth = newEphemeralAuthService()
	s.authLimiter = NewRateLimiter(DefaultAuthRateLimitPerMinute, time.Minute)

	s.routes()
	return s
}

// newEphemeralAuthService builds the default auth service: in-memory users and
// a random per-process JWT secret (never a hardcoded key). cmd/server replaces
// it with SetAuthService using JWT_SECRET and the SQL repository.
func newEphemeralAuthService() *auth.Service {
	secret := make([]byte, 32)
	if _, err := rand.Read(secret); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	svc, err := auth.NewService(auth.NewMemoryRepository(), auth.LogSender{}, auth.Config{JWTSecret: secret})
	if err != nil {
		panic(err)
	}
	return svc
}

// SetAuthService replaces the auth service (JWT secret, user repository, OTP sender).
func (s *Server) SetAuthService(svc *auth.Service) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.auth = svc
}

// SetAuthRateLimit sets the per-IP request budget per minute for
// /api/v1/auth/* (<= 0 disables it). Call at boot.
func (s *Server) SetAuthRateLimit(perMinute int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.authLimiter = NewRateLimiter(perMinute, time.Minute)
}

// SetPlayIntegrityVerifier enables POST /api/v1/auth/attestation/verify.
func (s *Server) SetPlayIntegrityVerifier(v *identity.PlayIntegrityVerifier) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.integrityVerifier = v
}

// SetDevMode enables explicit local-development endpoints (demo session).
func (s *Server) SetDevMode(enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.devMode = enabled
}

// AuthService returns the configured auth service.
func (s *Server) AuthService() *auth.Service { return s.authService() }

func (s *Server) authService() *auth.Service {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.auth
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

// SetMerchantRepository replaces the default in-memory merchant store (e.g. with
// store.SQLMerchantRepository when DATABASE_URL is configured). Call at boot.
func (s *Server) SetMerchantRepository(repo store.MerchantRepository) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.merchantRepo = repo
}

func (s *Server) SetRBFRepository(repo *store.SQLRBFRepository) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rbfRepo = repo
}

// SetEMVPaymentRepository makes card payment state persistent: the payment
// state machine is switched to store.SQLPaymentEngine over repo.
func (s *Server) SetEMVPaymentRepository(repo *store.SQLEMVPaymentRepository) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.emvPaymentRepo = repo
	s.paymentEngine = store.NewSQLPaymentEngine(repo)
}

// SetPaymentProcessor overrides the card payment state machine.
func (s *Server) SetPaymentProcessor(p payments.PaymentProcessor) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.paymentEngine = p
}

// SetCoDiRepository makes CoDi requests/confirmations persistent.
func (s *Server) SetCoDiRepository(repo payments.CoDiRepository) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.codiService = payments.NewCoDiServiceWithRepository(repo)
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
// The merchant must already be registered.
func (s *Server) RegisterMerchantCSD(merchantID string, keyDER []byte, certDER []byte) error {
	enc, err := s.csdVault.StoreCSD(merchantID, certDER, keyDER, "vault-managed")
	if err != nil {
		return err
	}
	encBytes, err := json.Marshal(enc)
	if err != nil {
		return fmt.Errorf("failed to encode encrypted CSD: %w", err)
	}
	return s.merchantRepo.SaveMerchantCSD(context.Background(), merchantID, encBytes)
}

func (s *Server) getMerchantCSD(merchantID string) (identity.DecryptedCSD, error) {
	csdBytes, found, err := s.merchantRepo.GetMerchantCSD(context.Background(), merchantID)
	if err != nil {
		return identity.DecryptedCSD{}, err
	}
	if !found {
		return identity.DecryptedCSD{}, errors.New("merchant CSD not registered")
	}
	var enc identity.EncryptedCSD
	if err := json.Unmarshal(csdBytes, &enc); err != nil {
		return identity.DecryptedCSD{}, fmt.Errorf("corrupt encrypted CSD: %w", err)
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

	// Commit phase: persist the merchant profile in the configured repository
	return s.merchantRepo.SaveMerchant(context.Background(), store.MerchantRecord{
		ID:       id,
		RFC:      rfc,
		Name:     name,
		IsRESICO: isRESICO,
	})
}

// RegisterMerchantCLABE assigns a virtual CLABE to a merchant, registering a
// placeholder merchant profile only when the merchant does not exist yet.
func (s *Server) RegisterMerchantCLABE(clabe string, merchantID string, rfc string) error {
	if _, exists := s.GetMerchant(merchantID); !exists {
		if err := s.RegisterMerchant(merchantID, rfc, "Merchant "+merchantID, true); err != nil {
			return err
		}
	}
	return s.merchantRepo.SetMerchantCLABE(context.Background(), merchantID, clabe)
}

func merchantFromRecord(rec store.MerchantRecord) Merchant {
	return Merchant{
		ID:       rec.ID,
		RFC:      rec.RFC,
		Name:     rec.Name,
		IsRESICO: rec.IsRESICO,
		CLABE:    rec.VirtualCLABE,
	}
}

func (s *Server) GetMerchant(id string) (Merchant, bool) {
	rec, found, err := s.merchantRepo.GetMerchant(context.Background(), id)
	if err != nil {
		slog.Error("merchant lookup failed", "merchant_id", id, "error", err)
		return Merchant{}, false
	}
	if !found {
		return Merchant{}, false
	}
	return merchantFromRecord(rec), true
}

func (s *Server) GetMerchantByCLABE(clabe string) (Merchant, bool) {
	rec, found, err := s.merchantRepo.GetMerchantByCLABE(context.Background(), clabe)
	if err != nil {
		slog.Error("merchant CLABE lookup failed", "error", err)
		return Merchant{}, false
	}
	if !found {
		return Merchant{}, false
	}
	return merchantFromRecord(rec), true
}

func (s *Server) GetMerchantCLABE(merchantID string) (string, bool) {
	clabe, found, err := s.merchantRepo.GetMerchantCLABE(context.Background(), merchantID)
	if err != nil {
		slog.Error("merchant CLABE query failed", "merchant_id", merchantID, "error", err)
		return "", false
	}
	return clabe, found
}

func currentYearMonth() string {
	return time.Now().UTC().Format("2006-01")
}

// merchantMonthlySalesCents returns the accumulated base sales for the current month, propagating storage errors.
func (s *Server) merchantMonthlySalesCents(merchantID string) (int64, error) {
	return s.merchantRepo.GetMonthlySalesCents(context.Background(), merchantID, currentYearMonth())
}

// GetMerchantMonthlySalesCents returns the accumulated monthly base sales (in cents) for a merchant in the current calendar month.
func (s *Server) GetMerchantMonthlySalesCents(merchantID string) int64 {
	cents, err := s.merchantMonthlySalesCents(merchantID)
	if err != nil {
		slog.Error("monthly sales lookup failed", "merchant_id", merchantID, "error", err)
		return 0
	}
	return cents
}

// AddMerchantMonthlySalesCents atomically increments a merchant's accumulated monthly base sales (in cents).
func (s *Server) AddMerchantMonthlySalesCents(merchantID string, baseAmountCents int64) error {
	if baseAmountCents <= 0 {
		return nil
	}
	_, err := s.merchantRepo.AddMonthlySalesCents(context.Background(), merchantID, currentYearMonth(), baseAmountCents)
	if err != nil {
		slog.Error("monthly sales increment failed", "merchant_id", merchantID, "amount_cents", baseAmountCents, "error", err)
	}
	return err
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
	merchantID, ok := requireTenant(w, r, r.PathValue("id"))
	if !ok {
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
	// ID is ignored: the server generates merchant ids (F1 "el id lo manda el cliente").
	ID           string `json:"id,omitempty"`
	RFC          string `json:"rfc"`
	Name         string `json:"name"`
	IsRESICO     bool   `json:"is_resico"`
	VirtualCLABE string `json:"virtual_clabe,omitempty"`
}

func newMerchantID() (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "mer_" + hex.EncodeToString(b), nil
}

// HandleRegisterMerchant: POST /api/v1/merchants. Requires an authenticated
// user; the server generates the merchant id and makes the caller its owner.
// Use POST /api/v1/auth/refresh with merchant_id to get a token for it.
func (s *Server) HandleRegisterMerchant(w http.ResponseWriter, r *http.Request) {
	userID, ok := UserIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "UNAUTHORIZED")
		return
	}
	authSvc := s.authService()
	if _, found, err := authSvc.Repository().GetUser(r.Context(), userID); err != nil {
		writeError(w, http.StatusServiceUnavailable, "USER_LOOKUP_FAILED")
		return
	} else if !found {
		writeError(w, http.StatusForbidden, "USER_ACCOUNT_REQUIRED")
		return
	}

	var req RegisterMerchantRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"INVALID_JSON"}`, http.StatusBadRequest)
		return
	}
	if req.RFC == "" || req.Name == "" {
		http.Error(w, `{"error":"MISSING_REQUIRED_FIELDS"}`, http.StatusBadRequest)
		return
	}
	merchantID, err := newMerchantID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "ID_GENERATION_FAILED")
		return
	}

	if err := s.RegisterMerchant(merchantID, req.RFC, req.Name, req.IsRESICO); err != nil {
		http.Error(w, fmt.Sprintf(`{"error":"REGISTRATION_FAILED","detail":%q}`, err.Error()), http.StatusBadRequest)
		return
	}

	if err := authSvc.AddMembership(r.Context(), userID, merchantID, auth.RoleOwner); err != nil {
		slog.Error("owner membership failed", "merchant_id", merchantID, "error", err)
		writeError(w, http.StatusInternalServerError, "OWNER_MEMBERSHIP_FAILED")
		return
	}

	if req.VirtualCLABE != "" {
		if err := s.RegisterMerchantCLABE(req.VirtualCLABE, merchantID, req.RFC); err != nil {
			http.Error(w, fmt.Sprintf(`{"error":"CLABE_ASSIGNMENT_FAILED","detail":%q}`, err.Error()), http.StatusConflict)
			return
		}
	}

	writeJSON(w, http.StatusCreated, map[string]any{
		"status":      "CREATED",
		"merchant_id": merchantID,
		"role":        auth.RoleOwner,
	})
}

func (s *Server) routes() {
	// Health and System Status
	s.router.HandleFunc("GET /healthz", s.HandleHealthz)

	// Authentication: OTP login, token refresh/rotation, logout
	s.router.HandleFunc("POST /api/v1/auth/otp/request", s.HandleOTPRequest)
	s.router.HandleFunc("POST /api/v1/auth/otp/verify", s.HandleOTPVerify)
	s.router.HandleFunc("POST /api/v1/auth/refresh", s.HandleRefresh)
	s.router.HandleFunc("POST /api/v1/auth/logout", s.HandleLogout)
	s.router.HandleFunc("GET /api/v1/auth/attestation-nonce", s.HandleAttestationNonce)
	s.router.HandleFunc("POST /api/v1/auth/attestation-nonce", s.HandleAttestationNonce)
	s.router.HandleFunc("POST /api/v1/auth/attestation/verify", s.HandleAttestationVerify)
	s.router.HandleFunc("POST /api/v1/auth/dev-session", s.HandleDevSession)

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

// Router returns the HTTP handler: recovery -> logging -> auth gate (JWT on
// every route outside the public allowlist, per-IP limit on /api/v1/auth/*).
func (s *Server) Router() http.Handler {
	return RecoveryMiddleware(LoggingMiddleware(s.authGate(s.router)))
}
