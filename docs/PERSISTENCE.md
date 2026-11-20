# Persistence (PostgreSQL)

The server runs in two modes:

| Mode | When | State |
|---|---|---|
| **In-memory** (dev/test) | `DATABASE_URL` empty | Maps inside the process; lost on restart. |
| **PostgreSQL** | `DATABASE_URL` set | SQL repositories in `pkg/store`; survives restarts. |

## Driver

`pkg/store/driver.go` registers `github.com/jackc/pgx/v5/stdlib` under the
database/sql name **`pgx`**. `SQL_DRIVER` defaults to `pgx`; the aliases
`postgres` / `postgresql` are normalized to `pgx`. Any other value (e.g. the
`mockpostgres` fake driver used by unit tests) is passed through unchanged.

At boot the server pings the database and **exits if it is unreachable or a
migration fails** (previously a migration failure was only a warning).

## Migrations

- Single source of truth: `migrations/NNN_description.up.sql` (+ `.down.sql`).
  The files are embedded into the binary (`migrations/embed.go`), so the server
  needs no files on disk.
- `store.RunMigrationsFS` applies pending versions in order, one transaction per
  file, and records each `NNN` in `schema_migrations(version, applied_at)`.
  Re-runs are no-ops. Duplicate versions are rejected.
- `MIGRATIONS_DIR` (optional) overrides the embedded set with an on-disk
  directory.
- `docker-compose.yml` no longer mounts SQL into `docker-entrypoint-initdb.d`;
  the app applies every migration itself.
- Not yet handled: concurrent runners from several replicas (no advisory lock);
  down migrations are stored but never executed automatically.

## Environment variables

| Variable | Default | Purpose |
|---|---|---|
| `DATABASE_URL` | empty (in-memory mode) | e.g. `postgres://user:pass@host:5432/softpos?sslmode=require` |
| `SQL_DRIVER` | `pgx` | database/sql driver name |
| `MIGRATIONS_DIR` | empty (embedded) | on-disk migrations override |
| `TEST_DATABASE_URL` | empty | enables real-Postgres integration tests |

## Running locally

```bash
docker compose up --build          # postgres + redis + server
# or, with your own Postgres:
DATABASE_URL='postgres://user:pass@localhost:5432/softpos?sslmode=disable' go run ./cmd/server
```

## Integration tests (real PostgreSQL)

Tests named `TestPGIntegration_*` run only when `TEST_DATABASE_URL` is set and
are skipped otherwise, so `go test ./...` stays offline. The helper
`pkg/store/pgtest` creates a fresh schema per test (bound via `search_path`)
and drops it on cleanup, so any database you point it at is left untouched.

```bash
TEST_DATABASE_URL='postgres://user:pass@localhost:5432/softpos_test?sslmode=disable' \
  go test -race -count=1 -run PGIntegration -v ./...
```

No Docker? Any throwaway Postgres 16 works; e.g. a tiny program outside the repo
using `github.com/fergusstrange/embedded-postgres` that starts a server on a
spare port and prints its URL.
