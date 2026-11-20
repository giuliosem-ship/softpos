package store

import (
	"strings"

	// Register the pgx database/sql driver under the name "pgx".
	_ "github.com/jackc/pgx/v5/stdlib"
)

// PostgresDriverName is the database/sql driver name registered by pgx/v5/stdlib.
const PostgresDriverName = "pgx"

// NormalizeDriverName maps empty and conventional PostgreSQL aliases ("postgres",
// "postgresql") to the registered pgx driver. Any other name (e.g. the
// "mockpostgres" test driver) is returned unchanged.
func NormalizeDriverName(name string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "postgres", "postgresql", "pgx", "pgx/v5":
		return PostgresDriverName
	default:
		return name
	}
}
