package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"softpos/migrations"
	"softpos/pkg/api"
	"softpos/pkg/auth"
	"softpos/pkg/identity"
	"softpos/pkg/ledger"
	"softpos/pkg/outbox"
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
	VaultMasterKeyHex string
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
	PlayIntegrityPackage string // PLAY_INTEGRITY_PACKAGE, default "mx.softpos.app"
	// CoDiWebhookSecret signs POST /api/v1/webhooks/codi/confirm (CODI_WEBHOOK_SECRET).
	// Empty outside DEV_MODE disables the webhook (503).
	CoDiWebhookSecret string

	// Outbox dispatcher (only runs when DatabaseURL is set).
	OutboxPollInterval time.Duration // OUTBOX_POLL_INTERVAL, default 2s
	OutboxBatchSize    int           // OUTBOX_BATCH_SIZE, default 50
	OutboxPublisher    string        // OUTBOX_PUBLISHER: "log" (default) | "noop"
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
	integrityPkg := os.Getenv("PLAY_INTEGRITY_PACKAGE")
	if integrityPkg == "" {
		integrityPkg = "mx.softpos.app"
	}

	return Config{
		AuthRateLimitPerMinute: authRate,
		PlayIntegritySecret:    os.Getenv("PLAY_INTEGRITY_SECRET"),
		CoDiWebhookSecret:      os.Getenv("CODI_WEBHOOK_SECRET"),
		PlayIntegrityPackage:   integrityPkg,
		DevMode:                devMode,
		JWTSecret:              os.Getenv("JWT_SECRET"),
		AuthAccessTTL:          parseDurationEnv("AUTH_ACCESS_TTL"),
		AuthRefreshTTL:         parseDurationEnv("AUTH_REFRESH_TTL"),
		OutboxPollInterval:     outboxPoll,
		OutboxBatchSize:        outboxBatch,
		OutboxPublisher:        os.Getenv("OUTBOX_PUBLISHER"),
		Port:                   port,
		DatabaseURL:            os.Getenv("DATABASE_URL"),
		SQLDriver:              sqlDriver,
		MigrationsDir:          migrationsDir,
		RedisAddr:              os.Getenv("REDIS_ADDR"),
		RedisPassword:          os.Getenv("REDIS_PASSWORD"),
		VaultMasterKeyHex:      os.Getenv("VAULT_MASTER_KEY_HEX"),
		STPHMACSecret:          hmacSecret,
		STPBaseURL:             os.Getenv("STP_BASE_URL"),
		SeedDemoMerchant:       seedDemo,
		StaticDir:              staticDir,
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
	}

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
	server.SetDevMode(cfg.DevMode)
	if codiSecret != "" {
		server.SetCoDiWebhookSecret(codiSecret)
	}
	if cfg.AuthRateLimitPerMinute > 0 {
		server.SetAuthRateLimit(cfg.AuthRateLimitPerMinute)
	}
	if cfg.PlayIntegritySecret != "" {
		server.SetPlayIntegrityVerifier(identity.NewPlayIntegrityVerifier(cfg.PlayIntegrityPackage, cfg.PlayIntegritySecret, 5*time.Minute))
	}

	if db != nil {
		sqlMerchantRepo := store.NewSQLMerchantRepository(db)
		server.SetMerchantRepository(sqlMerchantRepo)

		sqlRBFRepo := store.NewSQLRBFRepository(db)
		server.SetRBFRepository(sqlRBFRepo)

		// Card payment state machine persists in emv_payments (SQLPaymentEngine).
		sqlEMVRepo := store.NewSQLEMVPaymentRepository(db)
		server.SetEMVPaymentRepository(sqlEMVRepo)

		server.SetCoDiRepository(store.NewSQLCoDiRepository(db))

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

	// 6. Seed demo merchant if enabled
	if cfg.SeedDemoMerchant {
		demoID := "merch_demo_01"
		demoRFC := "TAQ850101XYZ"
		demoCLABE := "646180123400000007"
		if err := server.RegisterMerchantCLABE(demoCLABE, demoID, demoRFC); err != nil {
			slog.Warn("demo merchant seeding failed", "error", err)
		}

		// Generate and register demo CSD for instant CFDI 4.0 stamping
		privDER, certDER, err := generateSelfSignedCSD()
		if err == nil {
			_ = server.RegisterMerchantCSD(demoID, privDER, certDER)
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
		PollInterval: cfg.OutboxPollInterval,
		BatchSize:    cfg.OutboxBatchSize,
	})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = d.Run(ctx)
	}()
	return func() { <-done }, nil
}

func generateSelfSignedCSD() ([]byte, []byte, error) {
	privKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, nil, err
	}
	privDER, err := x509.MarshalPKCS8PrivateKey(privKey)
	if err != nil {
		return nil, nil, err
	}
	serial, _ := new(big.Int).SetString("30001000000400002434", 10)
	tpl := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: "TAQUERIA SOFTPOS DEMO"},
		NotBefore:    time.Now().Add(-1 * time.Hour),
		NotAfter:     time.Now().Add(365 * 24 * time.Hour),
	}
	certDER, err := x509.CreateCertificate(rand.Reader, &tpl, &tpl, &privKey.PublicKey, privKey)
	if err != nil {
		return nil, nil, err
	}
	return privDER, certDER, nil
}

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
		go func() { waitDispatcher(); close(dispatcherDone) }()
		select {
		case <-dispatcherDone:
		case <-shutdownCtx.Done():
			slog.Warn("outbox dispatcher did not stop before shutdown deadline")
		}
		slog.Info("SoftPOS server shutdown completed successfully")
	}
}
