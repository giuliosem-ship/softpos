// Package pgtest provides helpers for integration tests that run against a real
// PostgreSQL server. Tests are skipped unless TEST_DATABASE_URL is set, so the
// default `go test ./...` stays fast and offline.
//
// Each call to URL/Open creates a fresh schema (dropped on test cleanup) and
// returns a connection string pinned to it via search_path, so tests are
// isolated from each other and from any existing data.
package pgtest

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// EnvVar is the environment variable holding the base PostgreSQL URL.
const EnvVar = "TEST_DATABASE_URL"

// DriverName is the database/sql driver used for integration tests.
const DriverName = "pgx"

var (
	seq        atomic.Int64
	nonIdentRe = regexp.MustCompile(`[^a-z0-9_]`)
)

// URL creates an isolated schema and returns a connection URL bound to it.
// It skips the test when TEST_DATABASE_URL is not set.
func URL(t testing.TB) string {
	t.Helper()
	base := os.Getenv(EnvVar)
	if base == "" {
		t.Skipf("%s not set; skipping PostgreSQL integration test", EnvVar)
	}

	name := strings.ToLower(t.Name())
	name = nonIdentRe.ReplaceAllString(name, "_")
	if len(name) > 30 {
		name = name[:30]
	}
	schema := fmt.Sprintf("it_%d_%d_%s", time.Now().UnixNano()%1_000_000_000, seq.Add(1), name)

	admin, err := sql.Open(DriverName, base)
	if err != nil {
		t.Fatalf("pgtest: open admin connection: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if _, err := admin.ExecContext(ctx, `CREATE SCHEMA "`+schema+`"`); err != nil {
		_ = admin.Close()
		t.Fatalf("pgtest: create schema %s: %v", schema, err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_, _ = admin.ExecContext(ctx, `DROP SCHEMA IF EXISTS "`+schema+`" CASCADE`)
		_ = admin.Close()
	})

	u, err := url.Parse(base)
	if err != nil || u.Scheme == "" {
		// key=value DSN form
		return base + " search_path=" + schema
	}
	q := u.Query()
	q.Set("search_path", schema)
	u.RawQuery = q.Encode()
	return u.String()
}

// Open returns a *sql.DB connected to a fresh isolated schema. The pool is
// closed on test cleanup.
func Open(t testing.TB) *sql.DB {
	t.Helper()
	dsn := URL(t)
	db, err := sql.Open(DriverName, dsn)
	if err != nil {
		t.Fatalf("pgtest: open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("pgtest: ping: %v", err)
	}
	return db
}
