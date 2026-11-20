package store

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"strings"
	"time"
)

// Migration represents a database migration script.
type Migration struct {
	Version string
	Name    string
	UpSQL   string
	DownSQL string
}

// RunMigrations applies all pending *.up.sql migrations found in migrationsDir.
// It is a thin wrapper over RunMigrationsFS for on-disk migration directories.
func RunMigrations(ctx context.Context, db *sql.DB, migrationsDir string) error {
	if _, err := os.Stat(migrationsDir); err != nil {
		return fmt.Errorf("failed to read migrations dir: %w", err)
	}
	return RunMigrationsFS(ctx, db, os.DirFS(migrationsDir))
}

// LoadMigrations returns the *.up.sql migrations in fsys sorted by version.
// The version is the filename prefix before the first underscore (e.g. "001").
// Duplicate versions are rejected.
func LoadMigrations(fsys fs.FS) ([]Migration, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, fmt.Errorf("failed to read migrations dir: %w", err)
	}

	var migrations []Migration
	seen := make(map[string]string)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		version, _, ok := strings.Cut(name, "_")
		if !ok || version == "" {
			return nil, fmt.Errorf("migration %s: filename must be NNN_description.up.sql", name)
		}
		if prev, dup := seen[version]; dup {
			return nil, fmt.Errorf("duplicate migration version %s (%s, %s)", version, prev, name)
		}
		seen[version] = name

		content, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, fmt.Errorf("failed to read migration file %s: %w", name, err)
		}
		m := Migration{Version: version, Name: name, UpSQL: string(content)}
		downName := strings.TrimSuffix(name, ".up.sql") + ".down.sql"
		if down, err := fs.ReadFile(fsys, downName); err == nil {
			m.DownSQL = string(down)
		}
		migrations = append(migrations, m)
	}
	sort.Slice(migrations, func(i, j int) bool { return migrations[i].Version < migrations[j].Version })
	return migrations, nil
}

// RunMigrationsFS applies every pending migration from fsys, each in its own
// transaction, recording applied versions in schema_migrations. Re-running is
// idempotent: already-recorded versions are skipped.
func RunMigrationsFS(ctx context.Context, db *sql.DB, fsys fs.FS) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version VARCHAR(64) PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		);
	`)
	if err != nil {
		return fmt.Errorf("failed to create schema_migrations table: %w", err)
	}

	migrations, err := LoadMigrations(fsys)
	if err != nil {
		return err
	}

	for _, m := range migrations {
		var exists bool
		err := db.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE version = $1)", m.Version).Scan(&exists)
		if err != nil {
			return fmt.Errorf("failed to check migration status for %s: %w", m.Version, err)
		}
		if exists {
			continue
		}

		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("failed to begin tx for migration %s: %w", m.Name, err)
		}

		for _, stmt := range splitSQLStatements(m.UpSQL) {
			if strings.TrimSpace(stmt) == "" {
				continue
			}
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				_ = tx.Rollback()
				return fmt.Errorf("failed to execute migration statement in %s: %w (stmt: %s)", m.Name, err, stmt)
			}
		}

		if _, err := tx.ExecContext(ctx, "INSERT INTO schema_migrations (version, applied_at) VALUES ($1, $2)", m.Version, time.Now().UTC()); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("failed to record migration %s: %w", m.Version, err)
		}

		if err := tx.Commit(); err != nil {
			return fmt.Errorf("failed to commit migration %s: %w", m.Version, err)
		}
	}

	return nil
}

func splitSQLStatements(sqlScript string) []string {
	var stmts []string
	var current strings.Builder

	lines := strings.Split(sqlScript, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "--") {
			continue
		}
		current.WriteString(line)
		current.WriteString("\n")
		if strings.HasSuffix(trimmed, ";") {
			stmts = append(stmts, current.String())
			current.Reset()
		}
	}

	rem := strings.TrimSpace(current.String())
	if rem != "" {
		stmts = append(stmts, rem)
	}
	return stmts
}
