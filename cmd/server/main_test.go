package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	_ "softpos/pkg/store" // register mockpostgres driver
)

func TestLoadConfigFromEnv(t *testing.T) {
	t.Setenv("PORT", "9090")
	t.Setenv("DATABASE_URL", "postgres://test:test@localhost:5432/testdb")
	t.Setenv("REDIS_ADDR", "localhost:6379")
	t.Setenv("STP_HMAC_SECRET", "custom_secret_key")

	cfg := LoadConfigFromEnv()
	if cfg.Port != 9090 {
		t.Errorf("expected port 9090, got %d", cfg.Port)
	}
	if cfg.DatabaseURL != "postgres://test:test@localhost:5432/testdb" {
		t.Errorf("unexpected database URL: %s", cfg.DatabaseURL)
	}
	if cfg.RedisAddr != "localhost:6379" {
		t.Errorf("unexpected redis addr: %s", cfg.RedisAddr)
	}
	if cfg.STPHMACSecret != "custom_secret_key" {
		t.Errorf("unexpected hmac secret: %s", cfg.STPHMACSecret)
	}
}

func TestBuildServer_InMemoryAndMockDB(t *testing.T) {
	ctx := context.Background()

	// 1. In-memory build
	cfgMem := Config{
		Port:             8080,
		SeedDemoMerchant: true,
		STPHMACSecret:    "test_secret",
		JWTSecret:        testJWTSecret,
	}
	srvMem, dbMem, rdMem, err := BuildServer(ctx, cfgMem)
	if err != nil {
		t.Fatalf("BuildServer in-memory failed: %v", err)
	}
	if dbMem != nil || rdMem != nil {
		t.Errorf("expected nil DB and Redis for in-memory config")
	}

	// Verify /healthz
	recHealth := httptest.NewRecorder()
	srvMem.Router().ServeHTTP(recHealth, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if recHealth.Code != http.StatusOK {
		t.Errorf("expected 200 on /healthz, got %d", recHealth.Code)
	}

	// Verify demo merchant seeded
	m, ok := srvMem.GetMerchant("merch_demo_01")
	if !ok || m.RFC != "TAQ850101XYZ" {
		t.Errorf("expected demo merchant seeded, got %+v", m)
	}

	// 2. Build with mock SQL database driver
	tmpDir := t.TempDir()
	cfgSQL := Config{
		Port:             8081,
		SQLDriver:        "mockpostgres",
		DatabaseURL:      "main_test_db",
		MigrationsDir:    tmpDir,
		SeedDemoMerchant: true,
		STPHMACSecret:    "test_secret",
		JWTSecret:        testJWTSecret,
	}
	srvSQL, dbSQL, _, err := BuildServer(ctx, cfgSQL)
	if err != nil {
		t.Fatalf("BuildServer with mock DB failed: %v", err)
	}
	defer func() {
		if dbSQL != nil {
			_ = dbSQL.Close()
		}
	}()

	recGet := httptest.NewRecorder()
	srvSQL.Router().ServeHTTP(recGet, httptest.NewRequest(http.MethodGet, "/api/v1/merchants/merch_demo_01", nil))
	if recGet.Code != http.StatusOK {
		t.Fatalf("expected 200 on get demo merchant, got %d", recGet.Code)
	}

	var merchResp map[string]any
	_ = json.Unmarshal(recGet.Body.Bytes(), &merchResp)
	if merchResp["id"] != "merch_demo_01" {
		t.Errorf("expected merch_demo_01, got %v", merchResp["id"])
	}
}

func TestServerLifecycle_GracefulShutdown(t *testing.T) {
	ctx := context.Background()
	cfg := Config{
		Port:             0, // ephemeral
		SeedDemoMerchant: true,
		STPHMACSecret:    "test_secret",
		JWTSecret:        testJWTSecret,
	}

	srv, _, _, err := BuildServer(ctx, cfg)
	if err != nil {
		t.Fatalf("BuildServer failed: %v", err)
	}

	testServer := httptest.NewServer(srv.Router())
	defer testServer.Close()

	res, err := http.Get(testServer.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz failed: %v", err)
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", res.StatusCode)
	}
}

const testJWTSecret = "cmd_server_test_jwt_secret_32_bytes_min"

func TestBuildServer_RequiresJWTSecretUnlessDevMode(t *testing.T) {
	if _, _, _, err := BuildServer(context.Background(), Config{STPHMACSecret: "test_secret"}); !errors.Is(err, ErrMissingJWTSecret) {
		t.Fatalf("expected ErrMissingJWTSecret, got %v", err)
	}
	if _, _, _, err := BuildServer(context.Background(), Config{STPHMACSecret: "test_secret", DevMode: true}); err != nil {
		t.Fatalf("DEV_MODE without JWT_SECRET must boot with an ephemeral secret, got %v", err)
	}
	if _, _, _, err := BuildServer(context.Background(), Config{STPHMACSecret: "test_secret", JWTSecret: "too_short"}); err == nil {
		t.Fatal("short JWT secret must be rejected")
	}
}

func TestLoadConfigFromEnv_AuthSettings(t *testing.T) {
	t.Setenv("DEV_MODE", "true")
	t.Setenv("JWT_SECRET", testJWTSecret)
	t.Setenv("AUTH_ACCESS_TTL", "5m")
	t.Setenv("AUTH_REFRESH_TTL", "bogus")
	cfg := LoadConfigFromEnv()
	if !cfg.DevMode || cfg.JWTSecret != testJWTSecret || cfg.AuthAccessTTL != 5*time.Minute || cfg.AuthRefreshTTL != 0 {
		t.Fatalf("unexpected auth config: %+v", cfg)
	}
}
