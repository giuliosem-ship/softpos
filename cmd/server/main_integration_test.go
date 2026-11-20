package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"softpos/pkg/store/pgtest"
)

// TestPGIntegration_BuildServerTwiceOnRealPostgres boots the server against a
// real PostgreSQL (TEST_DATABASE_URL), then boots it again on the same schema to
// prove migrations are idempotent and seeded state survives a restart.
func TestPGIntegration_BuildServerTwiceOnRealPostgres(t *testing.T) {
	dsn := pgtest.URL(t)
	cfg := Config{
		Port:             0,
		SQLDriver:        "postgres", // alias normalized to pgx
		DatabaseURL:      dsn,
		SeedDemoMerchant: true,
		STPHMACSecret:    "test_secret",
		JWTSecret:        testJWTSecret,
	}

	for boot := 1; boot <= 2; boot++ {
		srv, db, _, err := BuildServer(context.Background(), cfg)
		if err != nil {
			t.Fatalf("boot %d: BuildServer: %v", boot, err)
		}
		rec := httptest.NewRecorder()
		srv.Router().ServeHTTP(rec, asMerchant(t, srv, "merch_demo_01", httptest.NewRequest(http.MethodGet, "/api/v1/merchants/merch_demo_01", nil)))
		if rec.Code != http.StatusOK {
			t.Fatalf("boot %d: expected demo merchant 200, got %d: %s", boot, rec.Code, rec.Body.String())
		}
		var versions int
		if err := db.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&versions); err != nil || versions == 0 {
			t.Fatalf("boot %d: schema_migrations count=%d err=%v", boot, versions, err)
		}
		_ = db.Close()
	}
}

func TestBuildServer_FailsFastOnUnreachableDatabase(t *testing.T) {
	cfg := Config{
		SQLDriver:     "pgx",
		DatabaseURL:   "postgres://nobody:nothing@127.0.0.1:1/none?sslmode=disable&connect_timeout=1",
		STPHMACSecret: "test_secret",
		JWTSecret:     testJWTSecret,
	}
	if _, _, _, err := BuildServer(context.Background(), cfg); err == nil {
		t.Fatalf("expected BuildServer to fail on unreachable database")
	}
}
