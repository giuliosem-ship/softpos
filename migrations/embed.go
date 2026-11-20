// Package migrations embeds the versioned SQL schema migrations so the server
// binary carries a single source of truth for the database schema.
//
// Files follow the pattern NNN_description.up.sql / NNN_description.down.sql.
// The NNN prefix is the version recorded in the schema_migrations table.
package migrations

import "embed"

// FS holds every *.sql migration file in this directory.
//
//go:embed *.sql
var FS embed.FS
