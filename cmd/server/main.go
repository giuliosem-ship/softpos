package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
	// Embedded IANA zoneinfo: RESICO months are computed in
	// America/Mexico_City and the runtime image ships no tzdata package.
	_ "time/tzdata"

	"softpos/migrations"
	"softpos/pkg/acquirer"
	"softpos/pkg/api"
	"softpos/pkg/auth"
	"softpos/pkg/fiscal/fiscaltest"
	"softpos/pkg/identity"
	"softpos/pkg/kyb"
	"softpos/pkg/ledger"
	"softpos/pkg/outbox"
	"softpos/pkg/payouts"
	"softpos/pkg/store"
	"softpos/pkg/stp"
)

// Config holds runtime configuration loaded from environment variables.
type Config struct {
	Port              int
	DatabaseURL       string
	SQLDriver         string
	MigrationsDir     string
	RedisAddr         string
	RedisPassword     string
	VaultMasterKeyHex string // VAULT_MASTER_KEY_HEX: active 32-byte CSD vault master key
	VaultKeyID        string // VAULT_MASTER_KEY_ID: id stored in envelopes (default "v1")
	VaultPreviousKeys string // VAULT_PREVIOUS_KEYS: "id:hex,..." retired keys kept for decryption
	STPHMACSecret     string
	STPBaseURL        string
	SeedDemoMerchant  bool
	StaticDir         string

	// DevMode (DEV_MODE=true) enables explicit local-development conveniences:
	// ephemeral JWT secret when JWT_SECRET is unset, OTP codes revealed in logs.
	// The production guard rejects it.
	DevMode bool
	// Auth (MVP-04)
	JWTSecret      string        // JWT_SECRET (>= 32 bytes); required unless DevMode
	AuthAccessTTL  time.Duration // AUTH_ACCESS_TTL, default 15m
	AuthRefreshTTL time.Duration // AUTH_REFRESH_TTL, default 720h
	// AuthRateLimitPerMinute is the per-IP budget for /api/v1/auth/*
	// (AUTH_RATE_LIMIT_PER_MINUTE, default 30; 0 keeps the default).
	AuthRateLimitPerMinute int
	// Play Integrity verification for /api/v1/auth/attestation/verify.
	PlayIntegritySecret  string // PLAY_INTEGRITY_SECRET; empty disables verification (503)
	PlayIntegrityPackage string // PLAY_INTEGRITY_PACKAGE, default DefaultPlayIntegrityPackage
	// CoDiWebhookSecret signs POST /api/v1/webhooks/codi/confirm (CODI_WEBHOOK_SECRET).
	// Empty outside DEV_MODE disables the webhook (503).
	CoDiWebhookSecret string

	// DefaultPlatformFeeBps is the tariff for merchants without their own
	// (DEFAULT_PLATFORM_FEE_BPS, 0..10000; unset keeps api.DefaultPlatformFeeBps).
	DefaultPlatformFeeBps int64
	HasDefaultFeeBps      bool
	// AllowCashierRefunds (ALLOW_CASHIER_REFUNDS=true) lets cashiers refund
	// settled payments; by default refunds are owner-only.
	AllowCashierRefunds bool

	// Outbox dispatcher (only runs when DatabaseURL is set).
	OutboxPollInterval time.Duration // OUTBOX_POLL_INTERVAL, default 2s
	OutboxBatchSize    int           // OUTBOX_BATCH_SIZE, default 50
	OutboxPublisher    string        // OUTBOX_PUBLISHER: "log" (default) | "noop"
	// OutboxPublishTimeout bounds each publish (OUTBOX_PUBLISH_TIMEOUT, default 30s).
	OutboxPublishTimeout time.Duration

	// AcquirerProvider selects the card processor adapter (ACQUIRER_PROVIDER):
	// "mock" (tests / DEV_MODE; rejected in production) or empty = card
	// payments disabled (503). DEV_MODE defaults it to "mock".
	AcquirerProvider string
	// AcquirerWebhookSecret signs POST /api/v1/webhooks/acquirer/{provider}
	// (ACQUIRER_WEBHOOK_SECRET, >= 32 bytes in production). DEV_MODE uses an
	// ephemeral random secret when unset; otherwise the webhook answers 503.
	AcquirerWebhookSecret string
	// AcquirerReconcileInterval is how often captures not settled by the
	// processor are flagged (ACQUIRER_RECONCILE_INTERVAL, default 1h; runs
	// only with DatabaseURL).
	AcquirerReconcileInterval time.Duration

	// KYCProvider selects the identity verification provider used at KYB
	// submission (KYC_PROVIDER): "mock" (tests / DEV_MODE; rejected in
	// production) or empty = no automatic identity check (operators review
	// the documents manually). DEV_MODE defaults it to "mock".
	KYCProvider string
	// BootstrapOperatorPhone (BACKOFFICE_BOOTSTRAP_OPERATOR_PHONE, E.164)
	// is made a platform operator at boot (backoffice access, docs/BACKOFFICE.md).
	// More operators are added with cmd/operatorctl; never through the API.
	BootstrapOperatorPhone string

	// Payout recovery (only runs when DatabaseURL is set).
	PayoutReconcileInterval time.Duration // PAYOUT_RECONCILE_INTERVAL, default 1m
	PayoutReconcileAfter    time.Duration // PAYOUT_RECONCILE_AFTER, default 10m (min 1m)

	// Refund request sweep (MVP-29, docs/ACQUIRER_INTEGRATION.md): refunds
	// stuck in REQUESTED are re-queried at the processor.
	RefundSweepInterval      time.Duration // REFUND_SWEEP_INTERVAL, default 5m
	RefundSweepAfter         time.Duration // REFUND_SWEEP_AFTER, default 15m (min 2m)
	RefundSweepAlertAttempts int           // REFUND_SWEEP_ALERT_ATTEMPTS, default 5

	// SAT 69-B list ingestion (MVP-20, docs/KYB.md). SAT69BURL
	// (SAT_69B_URL, e.g. the SAT's Listado_Completo_69-B.csv) enables the
	// download worker; empty = lists are loaded only with cmd/operatorctl.
	SAT69BURL             string
	SAT69BRefreshInterval time.Duration // SAT_69B_REFRESH_INTERVAL, default 24h
	SAT69BMaxAge          time.Duration // SAT_69B_MAX_AGE, default 7 days (older list -> UNKNOWN)
	SAT69BMaxShrinkPct    int           // SAT_69B_MAX_SHRINK_PCT, default 10 (0..100)
	// SAT69BMaxChangePct (SAT_69B_MAX_CHANGE_PCT, default 10, 1..100; 100
	// disables) refuses a load where more than pct% of the RFCs present in
	// both lists changed situation (MVP-27).
	SAT69BMaxChangePct int

	// CFDI workers (MVP-22, docs/CFDI.md): automatic factura global and
	// polling of cancellations waiting for the receptor's acceptance.
	CFDIGlobalInvoiceInterval time.Duration // CFDI_GLOBAL_INVOICE_INTERVAL, default 1h
	CFDICancelPollInterval    time.Duration // CFDI_CANCEL_POLL_INTERVAL, default 15m
}

func LoadConfigFromEnv() Config {
	port := 8080
	if val := os.Getenv("PORT"); val != "" {
		if p, err := strconv.Atoi(val); err == nil && p > 0 {
			port = p
		}
	}

	// Empty MIGRATIONS_DIR means "use the migrations embedded in the binary".
	migrationsDir := os.Getenv("MIGRATIONS_DIR")

	staticDir := os.Getenv("STATIC_DIR")
	if staticDir == "" {
		if _, err := os.Stat("web/pos"); err == nil {
			staticDir = "web/pos"
		} else if _, err := os.Stat("../../web/pos"); err == nil {
			staticDir = "../../web/pos"
		} else {
			staticDir = "web/pos"
		}
	}

	seedDemo := true
	if val := os.Getenv("SEED_DEMO_MERCHANT"); val == "false" || val == "0" {
		seedDemo = false
	}

	// No default here: BuildServer only falls back to the dev STP secret in DEV_MODE.
	hmacSecret := os.Getenv("STP_HMAC_SECRET")

	sqlDriver := store.NormalizeDriverName(os.Getenv("SQL_DRIVER"))

	var outboxPoll time.Duration
	if val := os.Getenv("OUTBOX_POLL_INTERVAL"); val != "" {
		if d, err := time.ParseDuration(val); err == nil && d > 0 {
			outboxPoll = d
		} else {
			slog.Warn("invalid OUTBOX_POLL_INTERVAL, using default", "value", val)
		}
	}
	var outboxBatch int
	if val := os.Getenv("OUTBOX_BATCH_SIZE"); val != "" {
		if n, err := strconv.Atoi(val); err == nil && n > 0 {
			outboxBatch = n
		} else {
			slog.Warn("invalid OUTBOX_BATCH_SIZE, using default", "value", val)
		}
	}

	devMode := false
	if val := strings.ToLower(os.Getenv("DEV_MODE")); val == "true" || val == "1" {
		devMode = true
	}

	authRate := 0
	if val := os.Getenv("AUTH_RATE_LIMIT_PER_MINUTE"); val != "" {
		if n, err := strconv.Atoi(val); err == nil && n > 0 {
			authRate = n
		} else {
			slog.Warn("invalid AUTH_RATE_LIMIT_PER_MINUTE, using default", "value", val)
		}
	}
	var feeBps int64
	hasFee := false
	if val := os.Getenv("DEFAULT_PLATFORM_FEE_BPS"); val != "" {
		if n, err := strconv.ParseInt(val, 10, 64); err == nil && n >= 0 && n <= 10000 {
			feeBps, hasFee = n, true
		} else {
			slog.Warn("invalid DEFAULT_PLATFORM_FEE_BPS, using default", "value", val)
		}
	}
	integrityPkg := os.Getenv("PLAY_INTEGRITY_PACKAGE")
	if integrityPkg == "" {
		integrityPkg = DefaultPlayIntegrityPackage
	}

	shrinkPct := 0
	if val := os.Getenv("SAT_69B_MAX_SHRINK_PCT"); val != "" {
		if n, err := strconv.Atoi(val); err == nil && n > 0 && n <= 100 {
			shrinkPct = n
		} else {
			slog.Warn("invalid SAT_69B_MAX_SHRINK_PCT, using default", "value", val)
		}
	}

	changePct := 0
	if val := os.Getenv("SAT_69B_MAX_CHANGE_PCT"); val != "" {
		if n, err := strconv.Atoi(val); err == nil && n > 0 && n <= 100 {
			changePct = n
		} else {
			slog.Warn("invalid SAT_69B_MAX_CHANGE_PCT, using default", "value", val)
		}
	}

	alertAttempts := 0
	if val := os.Getenv("REFUND_SWEEP_ALERT_ATTEMPTS"); val != "" {
		if n, err := strconv.Atoi(val); err == nil && n > 0 {
			alertAttempts = n
		} else {
			slog.Warn("invalid REFUND_SWEEP_ALERT_ATTEMPTS, using default", "value", val)
		}
	}

	cashierRefunds := false
	if val := strings.ToLower(os.Getenv("ALLOW_CASHIER_REFUNDS")); val == "true" || val == "1" {
		cashierRefunds = true
	}

	return Config{
		AllowCashierRefunds:       cashierRefunds,
		AcquirerProvider:          strings.ToLower(strings.TrimSpace(os.Getenv("ACQUIRER_PROVIDER"))),
		AcquirerWebhookSecret:     os.Getenv("ACQUIRER_WEBHOOK_SECRET"),
		AcquirerReconcileInterval: parseDurationEnv("ACQUIRER_RECONCILE_INTERVAL"),
		KYCProvider:               strings.ToLower(strings.TrimSpace(os.Getenv("KYC_PROVIDER"))),
		BootstrapOperatorPhone:    strings.TrimSpace(os.Getenv("BACKOFFICE_BOOTSTRAP_OPERATOR_PHONE")),
		DefaultPlatformFeeBps:     feeBps,
		HasDefaultFeeBps:          hasFee,
		AuthRateLimitPerMinute:    authRate,
		PlayIntegritySecret:       os.Getenv("PLAY_INTEGRITY_SECRET"),
		CoDiWebhookSecret:         os.Getenv("CODI_WEBHOOK_SECRET"),
		PlayIntegrityPackage:      integrityPkg,
		DevMode:                   devMode,
		JWTSecret:                 os.Getenv("JWT_SECRET"),
		AuthAccessTTL:             parseDurationEnv("AUTH_ACCESS_TTL"),
		AuthRefreshTTL:            parseDurationEnv("AUTH_REFRESH_TTL"),
		OutboxPollInterval:        outboxPoll,
		OutboxBatchSize:           outboxBatch,
		OutboxPublisher:           os.Getenv("OUTBOX_PUBLISHER"),
		OutboxPublishTimeout:      parseDurationEnv("OUTBOX_PUBLISH_TIMEOUT"),
		PayoutReconcileInterval:   parseDurationEnv("PAYOUT_RECONCILE_INTERVAL"),
		PayoutReconcileAfter:      parseDurationEnv("PAYOUT_RECONCILE_AFTER"),
		RefundSweepInterval:       parseDurationEnv("REFUND_SWEEP_INTERVAL"),
		RefundSweepAfter:          parseDurationEnv("REFUND_SWEEP_AFTER"),
		RefundSweepAlertAttempts:  alertAttempts,
		SAT69BURL:                 strings.TrimSpace(os.Getenv("SAT_69B_URL")),
		SAT69BRefreshInterval:     parseDurationEnv("SAT_69B_REFRESH_INTERVAL"),
		SAT69BMaxAge:              parseDurationEnv("SAT_69B_MAX_AGE"),
		SAT69BMaxShrinkPct:        shrinkPct,
		SAT69BMaxChangePct:        changePct,
		CFDIGlobalInvoiceInterval: parseDurationEnv("CFDI_GLOBAL_INVOICE_INTERVAL"),
		CFDICancelPollInterval:    parseDurationEnv("CFDI_CANCEL_POLL_INTERVAL"),
		Port:                      port,
		DatabaseURL:               os.Getenv("DATABASE_URL"),
		SQLDriver:                 sqlDriver,
		MigrationsDir:             migrationsDir,
		RedisAddr:                 os.Getenv("REDIS_ADDR"),
		RedisPassword:             os.Getenv("REDIS_PASSWORD"),
		VaultMasterKeyHex:         os.Getenv("VAULT_MASTER_KEY_HEX"),
		VaultKeyID:                os.Getenv("VAULT_MASTER_KEY_ID"),
		VaultPreviousKeys:         os.Getenv("VAULT_PREVIOUS_KEYS"),
		STPHMACSecret:             hmacSecret,
		STPBaseURL:                os.Getenv("STP_BASE_URL"),
		SeedDemoMerchant:          seedDemo,
		StaticDir:                 staticDir,
	}
}

func parseDurationEnv(name string) time.Duration {
	val := os.Getenv(name)
	if val == "" {
		return 0
	}
	d, err := time.ParseDuration(val)
	if err != nil || d <= 0 {
		slog.Warn("invalid duration, using default", "var", name, "value", val)
		return 0
	}
	return d
}

// DefaultPlayIntegrityPackage is the Android applicationId of the release
// app (debug builds are "mx.kerno.softpos.debug": set PLAY_INTEGRITY_PACKAGE
// per environment).
const DefaultPlayIntegrityPackage = "mx.kerno.softpos"

// demoSettlementCLABE is the demo merchant's (fake) bank account for payouts.
const demoSettlementCLABE = "032180000118359719"

// ErrMissingJWTSecret is returned when JWT_SECRET is unset outside DEV_MODE.
var ErrMissingJWTSecret = errors.New("JWT_SECRET is required (>= 32 bytes) unless DEV_MODE=true")

// buildAuthService creates the auth service over repo using the configured
// JWT secret. Without JWT_SECRET it only proceeds in explicit dev mode, with a
// random per-process secret (tokens die on restart).
func buildAuthService(cfg Config, repo auth.Repository) (*auth.Service, error) {
	secret := []byte(cfg.JWTSecret)
	if len(secret) == 0 {
		if !cfg.DevMode {
			return nil, ErrMissingJWTSecret
		}
		secret = make([]byte, 32)
		if _, err := rand.Read(secret); err != nil {
			return nil, err
		}
		slog.Warn("DEV_MODE: JWT_SECRET not set, using an ephemeral random secret")
	}
	svc, err := auth.NewService(repo, auth.LogSender{RevealCode: cfg.DevMode}, auth.Config{
		JWTSecret:  secret,
		AccessTTL:  cfg.AuthAccessTTL,
		RefreshTTL: cfg.AuthRefreshTTL,
	})
	if err != nil {
		return nil, fmt.Errorf("auth service: %w", err)
	}
	return svc, nil
}

var (
	// ErrMissingVaultKey is returned when VAULT_MASTER_KEY_HEX is unset outside DEV_MODE.
	ErrMissingVaultKey = errors.New("VAULT_MASTER_KEY_HEX is required (32 bytes hex) unless DEV_MODE=true")
	// ErrDevVaultKeyOutsideDevMode is returned when the public dev vault key is configured without DEV_MODE.
	ErrDevVaultKeyOutsideDevMode = errors.New("VAULT_MASTER_KEY_HEX is the public dev key or a trivial key; set a real random key or DEV_MODE=true")
)

// buildVaultKeyProvider returns the CSD vault master-key provider from the
// environment keyring. The well-known dev key is only accepted with DEV_MODE
// (it keeps dev databases' existing CSD envelopes decryptable). A KMS-backed
// identity.KMSKeyProvider plugs in here once a KMS is chosen.
func buildVaultKeyProvider(cfg Config) (identity.KeyProvider, error) {
	keyHex, keyID := cfg.VaultMasterKeyHex, cfg.VaultKeyID
	switch {
	case keyHex == "" && !cfg.DevMode:
		return nil, ErrMissingVaultKey
	case keyHex == "":
		slog.Warn("DEV_MODE: VAULT_MASTER_KEY_HEX not set, using the public dev vault key")
		keyHex = defaultVaultMasterKeyHex
		if keyID == "" {
			keyID = "dev"
		}
	case insecureVaultKey(keyHex) && !cfg.DevMode:
		return nil, ErrDevVaultKeyOutsideDevMode
	}
	if keyID == "" {
		keyID = "v1"
	}
	previous, err := identity.ParseKeyring(cfg.VaultPreviousKeys)
	if err != nil {
		return nil, fmt.Errorf("VAULT_PREVIOUS_KEYS: %w", err)
	}
	p, err := identity.NewStaticKeyProvider(keyID, keyHex, previous)
	if err != nil {
		return nil, fmt.Errorf("vault key: %w", err)
	}
	return p, nil
}

// ErrDevSTPSecretOutsideDevMode is returned when the well-known dev STP HMAC
// secret is configured without DEV_MODE (anyone could forge STP abonos).
var ErrDevSTPSecretOutsideDevMode = errors.New("STP_HMAC_SECRET is the public dev default; set a real secret or DEV_MODE=true")

// resolveWebhookSecrets applies the dev-mode fallbacks: the dev STP HMAC secret
// and an ephemeral CoDi webhook secret are only used with DEV_MODE.
func resolveWebhookSecrets(cfg Config) (stpSecret, codiSecret string, err error) {
	stpSecret, codiSecret = cfg.STPHMACSecret, cfg.CoDiWebhookSecret
	if stpSecret == defaultSTPHMACSecret && !cfg.DevMode {
		return "", "", ErrDevSTPSecretOutsideDevMode
	}
	if stpSecret == "" {
		if cfg.DevMode {
			stpSecret = defaultSTPHMACSecret
			slog.Warn("DEV_MODE: using the public dev STP HMAC secret")
		} else {
			slog.Warn("STP_HMAC_SECRET not set: HMAC-signed STP webhooks will be rejected (RSA only)")
		}
	}
	if codiSecret == "" {
		if cfg.DevMode {
			b := make([]byte, 32)
			if _, err := rand.Read(b); err != nil {
				return "", "", err
			}
			codiSecret = hex.EncodeToString(b)
			slog.Warn("DEV_MODE: CODI_WEBHOOK_SECRET not set, using an ephemeral random secret")
		} else {
			slog.Warn("CODI_WEBHOOK_SECRET not set: CoDi confirmation webhook disabled (503)")
		}
	}
	return stpSecret, codiSecret, nil
}

// buildAcquirerProvider resolves ACQUIRER_PROVIDER (DEV_MODE defaults to the
// mock). nil means card payments are disabled.
func buildAcquirerProvider(cfg Config) (acquirer.Provider, error) {
	name := cfg.AcquirerProvider
	if name == "" && cfg.DevMode {
		name = acquirer.MockProviderName
	}
	if name == "" {
		slog.Warn("ACQUIRER_PROVIDER not set: card payments disabled (503)")
		return nil, nil
	}
	p, err := acquirer.New(name)
	if err != nil {
		return nil, fmt.Errorf("ACQUIRER_PROVIDER: %w", err)
	}
	if name == acquirer.MockProviderName {
		slog.Warn("ACQUIRER_PROVIDER=mock: card payments use the deterministic mock processor (no real money)")
	}
	return p, nil
}

// resolveAcquirerWebhookSecret returns ACQUIRER_WEBHOOK_SECRET, or an
// ephemeral random secret in DEV_MODE (used by the dev card simulation).
func resolveAcquirerWebhookSecret(cfg Config) (string, error) {
	if cfg.AcquirerWebhookSecret != "" || !cfg.DevMode {
		if cfg.AcquirerWebhookSecret == "" {
			slog.Warn("ACQUIRER_WEBHOOK_SECRET not set: processor webhooks disabled (503)")
		}
		return cfg.AcquirerWebhookSecret, nil
	}
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	slog.Warn("DEV_MODE: ACQUIRER_WEBHOOK_SECRET not set, using an ephemeral random secret")
	return hex.EncodeToString(b), nil
}

// ErrUnknownKYCProvider is returned for an unsupported KYC_PROVIDER.
var ErrUnknownKYCProvider = errors.New("unknown KYC_PROVIDER (want mock or empty)")

// buildKYCProvider resolves KYC_PROVIDER (DEV_MODE defaults to the mock).
// nil means identity checks stay UNKNOWN and operators verify manually.
func buildKYCProvider(cfg Config) (kyb.KYCProvider, error) {
	name := cfg.KYCProvider
	if name == "" && cfg.DevMode {
		name = "mock"
	}
	switch name {
	case "":
		slog.Warn("KYC_PROVIDER not set: KYB identity checks are manual (operator review)")
		return nil, nil
	case "mock":
		slog.Warn("KYC_PROVIDER=mock: identity checks use the deterministic mock (no real verification)")
		return kyb.MockKYCProvider{}, nil
	}
	return nil, fmt.Errorf("%w: %q", ErrUnknownKYCProvider, name)
}

// BuildServer instantiates and connects all infrastructure, repositories, and domain services.
func BuildServer(ctx context.Context, cfg Config) (*api.Server, *sql.DB, *store.RedisClient, error) {
	stpSecret, codiSecret, err := resolveWebhookSecrets(cfg)
	if err != nil {
		return nil, nil, nil, err
	}

	ledgerEngine := ledger.NewLedgerEngine()
	blacklistService := identity.NewBlacklistService()

	var db *sql.DB
	var redisClient *store.RedisClient

	// 1. Initialize PostgreSQL persistence and migrations if DATABASE_URL is configured
	if cfg.DatabaseURL != "" {
		db, err = sql.Open(store.NormalizeDriverName(cfg.SQLDriver), cfg.DatabaseURL)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to open database connection: %w", err)
		}
		if err := db.PingContext(ctx); err != nil {
			_ = db.Close()
			return nil, nil, nil, fmt.Errorf("failed to connect to database: %w", err)
		}

		// Apply versioned migrations (embedded by default, MIGRATIONS_DIR overrides).
		var migErr error
		if cfg.MigrationsDir != "" {
			migErr = store.RunMigrations(ctx, db, cfg.MigrationsDir)
		} else {
			migErr = store.RunMigrationsFS(ctx, db, migrations.FS)
		}
		if migErr != nil {
			_ = db.Close()
			return nil, nil, nil, fmt.Errorf("database migrations failed: %w", migErr)
		}

		// Wire SQL repositories with pessimistic locking into domain engines
		sqlLedger := store.NewSQLLedgerRepository(db)
		ledgerEngine.SetRepository(sqlLedger)

		sqlBlacklist := store.NewSQLBlacklistRepository(db)
		blacklistService.SetRepository(sqlBlacklist)
	} else {
		// In-memory 69-B list (lost on restart): loadable through the backoffice.
		blacklistService.SetRepository(identity.NewMemoryList69BStore())
	}
	blacklistService.SetMaxAge(cfg.SAT69BMaxAge)

	// 2. Initialize STP Signer
	stpSigner := stp.NewSTPSigner(nil, nil, stpSecret)

	// 3. Create HTTP Server
	server := api.NewServer(ledgerEngine, blacklistService, stpSigner)
	server.SetStaticDir(cfg.StaticDir)

	var authRepo auth.Repository = auth.NewMemoryRepository()
	if db != nil {
		authRepo = store.NewSQLAuthRepository(db)
	}
	authSvc, err := buildAuthService(cfg, authRepo)
	if err != nil {
		if db != nil {
			_ = db.Close()
		}
		return nil, nil, nil, err
	}
	server.SetAuthService(authSvc)
	vaultKeys, err := buildVaultKeyProvider(cfg)
	if err == nil {
		err = server.SetCSDKeyProvider(vaultKeys)
	}
	if err != nil {
		if db != nil {
			_ = db.Close()
		}
		return nil, nil, nil, err
	}
	server.SetDevMode(cfg.DevMode)
	if cfg.SAT69BURL != "" {
		server.SetSAT69BSource(cfg.SAT69BURL, identity.HTTP69BFetcher(cfg.SAT69BURL, nil), cfg.SAT69BMaxShrinkPct)
	} else {
		slog.Warn("SAT_69B_URL not set: the 69-B list is only loaded with cmd/operatorctl load-69b (KYB lookups stay UNKNOWN until then)")
	}
	server.SetSAT69BMaxChangePct(cfg.SAT69BMaxChangePct)
	if cfg.HasDefaultFeeBps {
		server.SetDefaultPlatformFeeBps(cfg.DefaultPlatformFeeBps)
	}
	server.SetCashierRefundsAllowed(cfg.AllowCashierRefunds)
	// Legacy client-trusted EMV approve/settle/reverse: demo only.
	server.SetLegacyCardFlowEnabled(cfg.DevMode)
	if prov, err := buildAcquirerProvider(cfg); err != nil {
		if db != nil {
			_ = db.Close()
		}
		return nil, nil, nil, err
	} else if prov != nil {
		server.SetAcquirerProvider(prov)
		secret, err := resolveAcquirerWebhookSecret(cfg)
		if err != nil {
			if db != nil {
				_ = db.Close()
			}
			return nil, nil, nil, err
		}
		server.SetAcquirerWebhookSecret(secret)
	}
	if codiSecret != "" {
		server.SetCoDiWebhookSecret(codiSecret)
	}
	kycProv, err := buildKYCProvider(cfg)
	if err != nil {
		if db != nil {
			_ = db.Close()
		}
		return nil, nil, nil, err
	}
	server.SetKYCProvider(kycProv)
	if cfg.AuthRateLimitPerMinute > 0 {
		server.SetAuthRateLimit(cfg.AuthRateLimitPerMinute)
	}
	if cfg.PlayIntegritySecret != "" {
		server.SetPlayIntegrityVerifier(identity.NewPlayIntegrityVerifier(cfg.PlayIntegrityPackage, cfg.PlayIntegritySecret, 5*time.Minute))
	}

	if db != nil {
		sqlMerchantRepo := store.NewSQLMerchantRepository(db)
		server.SetMerchantRepository(sqlMerchantRepo)

		// Merchant onboarding (KYB) and encrypted KYC document blobs (bytea).
		sqlKYB := store.NewSQLKYBRepository(db)
		server.SetKYBRepository(sqlKYB, sqlKYB)
		server.SetOperatorAuditRepository(store.NewSQLAuditRepository(db))
		server.SetFiscalSettingsRepository(store.NewSQLFiscalSettingsRepository(db))
		server.SetInvoiceRepository(store.NewSQLInvoiceRepository(db))

		sqlRBFRepo := store.NewSQLRBFRepository(db)
		server.SetRBFRepository(sqlRBFRepo)

		// Card payment state machine persists in emv_payments (SQLPaymentEngine).
		sqlEMVRepo := store.NewSQLEMVPaymentRepository(db)
		server.SetEMVPaymentRepository(sqlEMVRepo)

		server.SetCoDiRepository(store.NewSQLCoDiRepository(db))

		// Payment state + journal + RESICO accumulator + outbox in one transaction.
		server.SetMoneyStore(store.NewSQLMoneyStore(db))

		sqlOutboxRepo := store.NewSQLOutboxRepository(db)
		server.SetOutboxRepository(sqlOutboxRepo)

		sqlIdemp := store.NewSQLIdempotencyRepository(db)
		server.IdempotencyStore().SetBackend(sqlIdemp)
	}

	// 4. Initialize Redis if REDIS_ADDR is configured
	if cfg.RedisAddr != "" {
		redisClient = store.NewRedisClient(cfg.RedisAddr, cfg.RedisPassword)
		if err := redisClient.Ping(ctx); err == nil {
			slog.Info("connected to Redis for distributed idempotency cache", "addr", cfg.RedisAddr)
			redisStore := store.NewRedisIdempotencyStore(redisClient)
			server.IdempotencyStore().SetBackend(redisStore)
		} else {
			slog.Warn("redis ping failed, continuing with database/in-memory fallback", "error", err)
		}
	}

	// 5. Pre-create global platform ledger accounts
	_, _ = ledgerEngine.CreateAccount("ASSET:STP_CONCENTRATOR", ledger.AccountAsset, "STP Concentrator Bank Account")
	_, _ = ledgerEngine.CreateAccount("ASSET:ACQUIRER_RECEIVABLE", ledger.AccountAsset, "Acquirer Card Receivable")
	_, _ = ledgerEngine.CreateAccount("REVENUE:FEES", ledger.AccountRevenue, "Platform SoftPOS Fee Revenue")
	_, _ = ledgerEngine.CreateAccount("ASSET:RBF_LOANS_RECEIVABLE", ledger.AccountAsset, "Platform RBF Loans")
	_, _ = ledgerEngine.CreateAccount(api.AccountAcquirerFees, ledger.AccountExpense, "Card Processor Fees")
	_, _ = ledgerEngine.CreateAccount(api.AccountBankOperating, ledger.AccountAsset, "Kerno Operating Bank Account (processor payouts)")

	// Backoffice bootstrap operator (idempotent).
	if cfg.BootstrapOperatorPhone != "" {
		u, err := authSvc.GrantPlatformOperator(ctx, cfg.BootstrapOperatorPhone, "bootstrap:env")
		if err != nil {
			if db != nil {
				_ = db.Close()
			}
			return nil, nil, nil, fmt.Errorf("BACKOFFICE_BOOTSTRAP_OPERATOR_PHONE: %w", err)
		}
		slog.Info("backoffice bootstrap operator ensured", "user_id", u.ID)
	}

	// 6. Seed demo merchant if enabled
	if cfg.SeedDemoMerchant {
		demoID := "merch_demo_01"
		demoRFC := demoMerchantRFC
		demoCLABE := "646180123400000007"
		if err := server.RegisterMerchantCLABE(demoCLABE, demoID, demoRFC); err != nil {
			slog.Warn("demo merchant seeding failed", "error", err)
		}
		// The demo merchant is provisioned with an approved onboarding (also
		// when it was seeded before migration 012 created merchant_kyb).
		if err := server.EnsureMerchantApproved(ctx, demoID); err != nil {
			slog.Warn("demo merchant KYB approval failed", "error", err)
		}
		// DEV_MODE only: a pre-verified settlement account so the demo can
		// exercise payouts (ownership verification has no real provider yet).
		if cfg.DevMode {
			if err := server.SeedVerifiedSettlementAccount(ctx, demoID, demoSettlementCLABE); err != nil {
				slog.Warn("demo settlement account seeding failed", "error", err)
			}
		}

		// Generate and register demo CSD for instant CFDI 4.0 stamping
		privDER, certDER, err := generateSelfSignedCSD()
		if err == nil {
			_ = server.RegisterMerchantCSD(demoID, privDER, certDER)
		}
		// CFDI lugar de expedición (the provisioned demo KYB has no address).
		if err := server.SeedFiscalSettings(ctx, demoID, "06000"); err != nil {
			slog.Warn("demo fiscal settings seeding failed", "error", err)
		}
	}

	return server, db, redisClient, nil
}

// newOutboxPublisher selects the outbox publisher by name. Real brokers/webhooks
// plug in here in later batches.
func newOutboxPublisher(name string) (outbox.Publisher, error) {
	switch name {
	case "", "log":
		return outbox.LogPublisher{}, nil
	case "noop":
		return outbox.NoopPublisher{}, nil
	default:
		return nil, fmt.Errorf("unknown OUTBOX_PUBLISHER %q (want log|noop)", name)
	}
}

// StartOutboxDispatcher launches the outbox dispatcher in a goroutine when a
// database is configured. It stops when ctx is cancelled; the returned wait
// function blocks until the dispatcher has exited. With db == nil it is a no-op.
func StartOutboxDispatcher(ctx context.Context, cfg Config, db *sql.DB) (wait func(), err error) {
	if db == nil {
		return func() {}, nil
	}
	pub, err := newOutboxPublisher(cfg.OutboxPublisher)
	if err != nil {
		return nil, err
	}
	d := outbox.NewDispatcher(store.NewSQLOutboxRepository(db), pub, outbox.Config{
		PollInterval:   cfg.OutboxPollInterval,
		BatchSize:      cfg.OutboxBatchSize,
		PublishTimeout: cfg.OutboxPublishTimeout,
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = d.Run(ctx)
	}()
	return func() { <-done }, nil
}

// minPayoutReconcileAfter keeps recovery well behind an in-flight
// registraOrden (the handler waits up to 30s for STP).
const minPayoutReconcileAfter = time.Minute

// StartPayoutReconciler launches the payout recovery loop (stale RESERVED /
// PENDING_RECONCILIATION payouts resolved against STP order status) when a
// database is configured; with db == nil it is a no-op. The returned wait
// function blocks until the loop has exited after ctx is cancelled.
func StartPayoutReconciler(ctx context.Context, cfg Config, db *sql.DB, statuses stp.OrderStatusClient) (wait func()) {
	if db == nil || statuses == nil {
		return func() {}
	}
	after := cfg.PayoutReconcileAfter
	if after != 0 && after < minPayoutReconcileAfter {
		slog.Warn("PAYOUT_RECONCILE_AFTER below minimum, using minimum", "value", after, "min", minPayoutReconcileAfter)
		after = minPayoutReconcileAfter
	}
	r := payouts.NewReconciler(store.NewSQLPayoutRepository(db), store.NewSQLMoneyStore(db), statuses, payouts.ReconcilerConfig{
		Interval: cfg.PayoutReconcileInterval,
		After:    after,
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = r.Run(ctx)
	}()
	return func() { <-done }
}

// StartAcquirerSettlementWatcher launches the processor settlement watcher
// (captures not paid out by their expected date -> NOT_SETTLED
// reconciliation items + ERROR alerts) when a database is configured; with
// db == nil it is a no-op. The returned wait function blocks until the loop
// has exited after ctx is cancelled.
func StartAcquirerSettlementWatcher(ctx context.Context, cfg Config, db *sql.DB) (wait func()) {
	if db == nil {
		return func() {}
	}
	w := payouts.NewAcquirerSettlementWatcher(store.NewSQLMoneyStore(db), payouts.AcquirerSettlementConfig{
		Interval: cfg.AcquirerReconcileInterval,
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = w.Run(ctx)
	}()
	return func() { <-done }
}

// defaultRefundSweepInterval is how often the refund sweep runs.
const defaultRefundSweepInterval = 5 * time.Minute

// StartRefundSweeper launches the refund request sweep (MVP-29) when a card
// processor is configured: every REFUND_SWEEP_INTERVAL, refunds stuck in
// REQUESTED for REFUND_SWEEP_AFTER are re-queried at the processor and
// resolved (applied through the webhook path, PENDING, FAILED or expired).
// The returned wait function blocks until the loop exits after ctx is
// cancelled.
func StartRefundSweeper(ctx context.Context, cfg Config, server *api.Server) (wait func()) {
	if server == nil || server.AcquirerProvider() == nil {
		return func() {}
	}
	interval := cfg.RefundSweepInterval
	if interval <= 0 {
		interval = defaultRefundSweepInterval
	}
	after := cfg.RefundSweepAfter
	if after != 0 && after < api.MinRefundSweepAfter {
		slog.Warn("REFUND_SWEEP_AFTER below minimum, using minimum", "value", after, "min", api.MinRefundSweepAfter)
		after = api.MinRefundSweepAfter
	}
	sweepCfg := api.RefundSweepConfig{After: after, AlertAttempts: cfg.RefundSweepAlertAttempts}
	done := make(chan struct{})
	go func() {
		defer close(done)
		runEvery(ctx, interval, func(ctx context.Context) {
			res, err := server.SweepStaleRefunds(ctx, time.Now(), sweepCfg)
			if err != nil {
				slog.Error("refund sweep failed", "error", err)
				return
			}
			if res.Claimed > 0 {
				slog.Info("refund sweep", "claimed", res.Claimed, "succeeded", res.Succeeded, "pending", res.Pending,
					"failed", res.Failed, "expired", res.Expired, "already_resolved", res.Resolved, "unknown", res.Unknown, "alerted", res.Alerted)
			}
		})
	}()
	return func() { <-done }
}

// CFDI worker defaults.
const (
	defaultCFDIGlobalInterval = time.Hour
	defaultCFDICancelPoll     = 15 * time.Minute
)

// runEvery calls fn now and then every interval until ctx is cancelled.
func runEvery(ctx context.Context, interval time.Duration, fn func(context.Context)) {
	fn(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			fn(ctx)
		}
	}
}

// StartCFDIWorkers launches the factura global worker (issues each closed
// period's global CFDI and retries PENDING ones) and the cancellation
// poller (resolves CANCEL_REQUESTED invoices from the SAT status). The
// returned wait function blocks until both loops exit after ctx is cancelled.
func StartCFDIWorkers(ctx context.Context, cfg Config, server *api.Server) (wait func()) {
	if server == nil {
		return func() {}
	}
	globalEvery, pollEvery := cfg.CFDIGlobalInvoiceInterval, cfg.CFDICancelPollInterval
	if globalEvery <= 0 {
		globalEvery = defaultCFDIGlobalInterval
	}
	if pollEvery <= 0 {
		pollEvery = defaultCFDICancelPoll
	}
	done := make(chan struct{}, 2)
	go func() {
		defer func() { done <- struct{}{} }()
		runEvery(ctx, globalEvery, func(ctx context.Context) {
			n, err := server.RunGlobalInvoices(ctx, time.Now())
			if err != nil {
				slog.Error("factura global run failed", "error", err)
			} else if n > 0 {
				slog.Info("factura global run", "invoices", n)
			}
		})
	}()
	go func() {
		defer func() { done <- struct{}{} }()
		runEvery(ctx, pollEvery, func(ctx context.Context) {
			if n, err := server.PollCancellations(ctx); err != nil {
				slog.Error("CFDI cancellation poll failed", "error", err)
			} else if n > 0 {
				slog.Info("CFDI cancellations resolved", "count", n)
			}
		})
	}()
	return func() { <-done; <-done }
}

// defaultSAT69BRefresh is how often the 69-B worker re-downloads the list.
const defaultSAT69BRefresh = 24 * time.Hour

// StartSAT69BWorker downloads and loads the SAT 69-B list at boot and then
// every SAT_69B_REFRESH_INTERVAL (only with SAT_69B_URL). Each successful
// load re-checks approved merchants. The returned wait function blocks until
// the loop has exited after ctx is cancelled.
func StartSAT69BWorker(ctx context.Context, cfg Config, server *api.Server) (wait func()) {
	if cfg.SAT69BURL == "" || server == nil {
		return func() {}
	}
	interval := cfg.SAT69BRefreshInterval
	if interval <= 0 {
		interval = defaultSAT69BRefresh
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		run := func() {
			runCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
			defer cancel()
			res, retro, err := server.SyncSAT69B(runCtx, "system:69b-worker", identity.Load69BOptions{})
			if err != nil {
				slog.Error("SAT 69-B sync failed; previous list kept", "error", err, "status", res.Load.Status)
				return
			}
			slog.Info("SAT 69-B sync", "status", res.Load.Status, "rows", res.Load.RowCount, "malformed", res.Malformed,
				"retroscreen_checked", retro.Checked, "retroscreen_blocked", len(retro.Blocked))
		}
		run()
		t := time.NewTicker(interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				run()
			}
		}
	}()
	return func() { <-done }
}

// generateSelfSignedCSD returns a self-signed, SAT-shaped demo CSD (never a
// real SAT certificate) for the demo merchant: plaintext PKCS#8 key and DER
// certificate.
func generateSelfSignedCSD() ([]byte, []byte, error) {
	c, err := fiscaltest.NewCSD(fiscaltest.CSDOptions{RFC: demoMerchantRFC, Name: "TAQUERIA SOFTPOS DEMO",
		Serial: "30001000000400002434", NotAfter: time.Now().Add(365 * 24 * time.Hour)})
	if err != nil {
		return nil, nil, err
	}
	return c.PlainKey, c.CertDER, nil
}

// demoMerchantRFC is the RFC of the seeded demo merchant.
const demoMerchantRFC = "TAQ850101XYZ"

func main() {
	cfg := LoadConfigFromEnv()

	env := os.Getenv("APP_ENV")
	if env == "" {
		env = os.Getenv("ENV")
	}
	if env == "" {
		env = "development"
	}
	if err := ValidateProductionConfig(cfg, env); err != nil {
		slog.Error("production configuration security violation", "error", err)
		os.Exit(1)
	}

	slog.Info("starting SoftPOS Hybrid Fiscal & Payment Platform server",
		"port", cfg.Port,
		"has_db", cfg.DatabaseURL != "",
		"has_redis", cfg.RedisAddr != "",
	)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	server, db, redisClient, err := BuildServer(ctx, cfg)
	if err != nil {
		slog.Error("fatal initialization error", "error", err)
		os.Exit(1)
	}
	defer func() {
		if db != nil {
			_ = db.Close()
		}
		if redisClient != nil {
			_ = redisClient.Close()
		}
	}()

	dispatcherCtx, stopDispatcher := context.WithCancel(context.Background())
	defer stopDispatcher()
	waitDispatcher, err := StartOutboxDispatcher(dispatcherCtx, cfg, db)
	if err != nil {
		slog.Error("fatal initialization error", "error", err)
		os.Exit(1)
	}
	waitPayouts := StartPayoutReconciler(dispatcherCtx, cfg, db, server)
	waitAcquirer := StartAcquirerSettlementWatcher(dispatcherCtx, cfg, db)
	wait69B := StartSAT69BWorker(dispatcherCtx, cfg, server)
	waitCFDI := StartCFDIWorkers(dispatcherCtx, cfg, server)
	waitRefunds := StartRefundSweeper(dispatcherCtx, cfg, server)

	httpServer := &http.Server{
		Addr:         fmt.Sprintf(":%d", cfg.Port),
		Handler:      server.Router(),
		ReadTimeout:  15 * time.Second,
		WriteTimeout: 15 * time.Second,
		IdleTimeout:  60 * time.Second,
	}

	// Graceful shutdown handling
	serverErrors := make(chan error, 1)
	go func() {
		slog.Info("HTTP server listening", "addr", httpServer.Addr, "pos_url", fmt.Sprintf("http://localhost:%d/pos/", cfg.Port))
		if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErrors <- err
		}
	}()

	shutdown := make(chan os.Signal, 1)
	signal.Notify(shutdown, os.Interrupt, syscall.SIGTERM)

	select {
	case err := <-serverErrors:
		slog.Error("server listener failure", "error", err)
		os.Exit(1)
	case sig := <-shutdown:
		slog.Info("received termination signal, shutting down gracefully", "signal", sig.String())
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shutdownCancel()

		if err := httpServer.Shutdown(shutdownCtx); err != nil {
			_ = httpServer.Close()
			slog.Error("graceful shutdown failed, forced close", "error", err)
		}

		// Stop the outbox dispatcher after HTTP drains so in-flight requests can still enqueue events.
		stopDispatcher()
		dispatcherDone := make(chan struct{})
		go func() {
			waitDispatcher()
			waitPayouts()
			waitAcquirer()
			wait69B()
			waitCFDI()
			waitRefunds()
			close(dispatcherDone)
		}()
		select {
		case <-dispatcherDone:
		case <-shutdownCtx.Done():
			slog.Warn("outbox dispatcher did not stop before shutdown deadline")
		}
		slog.Info("SoftPOS server shutdown completed successfully")
	}
}
