package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"softpos/pkg/api"
	"softpos/pkg/identity"
	"softpos/pkg/ledger"
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
}

func LoadConfigFromEnv() Config {
	port := 8080
	if val := os.Getenv("PORT"); val != "" {
		if p, err := strconv.Atoi(val); err == nil && p > 0 {
			port = p
		}
	}

	migrationsDir := os.Getenv("MIGRATIONS_DIR")
	if migrationsDir == "" {
		if _, err := os.Stat("migrations"); err == nil {
			migrationsDir = "migrations"
		} else if _, err := os.Stat("../../migrations"); err == nil {
			migrationsDir = "../../migrations"
		} else {
			migrationsDir = "migrations"
		}
	}

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

	hmacSecret := os.Getenv("STP_HMAC_SECRET")
	if hmacSecret == "" {
		hmacSecret = "softpos_dev_secret_key_12345678"
	}

	sqlDriver := os.Getenv("SQL_DRIVER")
	if sqlDriver == "" {
		sqlDriver = "postgres"
	}

	return Config{
		Port:              port,
		DatabaseURL:       os.Getenv("DATABASE_URL"),
		SQLDriver:         sqlDriver,
		MigrationsDir:     migrationsDir,
		RedisAddr:         os.Getenv("REDIS_ADDR"),
		RedisPassword:     os.Getenv("REDIS_PASSWORD"),
		VaultMasterKeyHex: os.Getenv("VAULT_MASTER_KEY_HEX"),
		STPHMACSecret:     hmacSecret,
		STPBaseURL:        os.Getenv("STP_BASE_URL"),
		SeedDemoMerchant:  seedDemo,
		StaticDir:         staticDir,
	}
}

// BuildServer instantiates and connects all infrastructure, repositories, and domain services.
func BuildServer(ctx context.Context, cfg Config) (*api.Server, *sql.DB, *store.RedisClient, error) {
	ledgerEngine := ledger.NewLedgerEngine()
	blacklistService := identity.NewBlacklistService()

	var db *sql.DB
	var redisClient *store.RedisClient

	// 1. Initialize PostgreSQL persistence and migrations if DATABASE_URL is configured
	if cfg.DatabaseURL != "" {
		var err error
		db, err = sql.Open(cfg.SQLDriver, cfg.DatabaseURL)
		if err != nil {
			return nil, nil, nil, fmt.Errorf("failed to open database connection: %w", err)
		}

		// Execute migrations
		if err := store.RunMigrations(ctx, db, cfg.MigrationsDir); err != nil {
			slog.Warn("database migrations warning", "error", err)
		}

		// Wire SQL repositories with pessimistic locking into domain engines
		sqlLedger := store.NewSQLLedgerRepository(db)
		ledgerEngine.SetRepository(sqlLedger)

		sqlBlacklist := store.NewSQLBlacklistRepository(db)
		blacklistService.SetRepository(sqlBlacklist)
	}

	// 2. Initialize STP Signer
	stpSigner := stp.NewSTPSigner(nil, nil, cfg.STPHMACSecret)

	// 3. Create HTTP Server
	server := api.NewServer(ledgerEngine, blacklistService, stpSigner)
	server.SetStaticDir(cfg.StaticDir)

	if db != nil {
		sqlMerchantRepo := store.NewSQLMerchantRepository(db)
		server.SetMerchantRepository(sqlMerchantRepo)

		sqlRBFRepo := store.NewSQLRBFRepository(db)
		server.SetRBFRepository(sqlRBFRepo)

		sqlEMVRepo := store.NewSQLEMVPaymentRepository(db)
		server.SetEMVPaymentRepository(sqlEMVRepo)

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
		_ = server.RegisterMerchantCLABE(demoCLABE, demoID, demoRFC)

		// Generate and register demo CSD for instant CFDI 4.0 stamping
		privDER, certDER, err := generateSelfSignedCSD()
		if err == nil {
			_ = server.RegisterMerchantCSD(demoID, privDER, certDER)
		}
	}

	return server, db, redisClient, nil
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
		slog.Info("SoftPOS server shutdown completed successfully")
	}
}
