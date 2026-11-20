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

## What is persisted (PostgreSQL mode)

`cmd/server.BuildServer` wires these when `DATABASE_URL` is set; without it the
same interfaces use in-memory implementations.

| State | Interface | SQL implementation | In-memory implementation |
|---|---|---|---|
| Ledger accounts / journals / balances | `ledger.Repository` | `store.SQLLedgerRepository` | `ledger.LedgerEngine` maps |
| Merchants, virtual CLABEs, encrypted CSDs, RESICO monthly sales | `store.MerchantRepository` | `store.SQLMerchantRepository` | `store.MemoryMerchantRepository` |
| Card payment state machine | `payments.PaymentProcessor` | `store.SQLPaymentEngine` (`emv_payments`) | `payments.PaymentEngine` |
| CoDi requests / confirmations | `payments.CoDiRepository` | `store.SQLCoDiRepository` (`codi_requests`, `codi_confirmations`) | maps inside `payments.CoDiService` |
| Idempotency keys | `api.IdempotencyStore` backend | `store.SQLIdempotencyRepository` (Redis if `REDIS_ADDR`) | map |
| SAT 69-B blacklist | `identity` repository | `store.SQLBlacklistRepository` | map |
| Users, memberships, OTP challenges, refresh tokens, attestation nonces (`docs/AUTH.md`) | `auth.Repository` | `store.SQLAuthRepository` (migrations 005, 006) | `auth.MemoryRepository` |
| Money unit of work: settlements, exactly-once keys, merchant tariff (`docs/MONEY_FLOWS.md`) | `store.MoneyStore` | `store.SQLMoneyStore` (`payment_settlements`, `money_operations`, `merchants.platform_fee_bps`, migration 007) | `store.MemoryMoneyStore` |

Notes:
- The raw EMV TLV payload is **not** stored (it can contain the clear PAN /
  track 2); `emv_payments.tlv_payload_hex` is written empty.
- Storage errors are no longer swallowed: registering a merchant/CLABE/CSD
  returns the error (`ErrDuplicateCLABE` -> HTTP 409 on `POST /merchants`), and a
  failed monthly-sales read returns 503 `MONTHLY_SALES_UNAVAILABLE` instead of
  silently using 0 (which would pick the wrong RESICO bracket).
- Still in memory (later batches): RBF loans (`rbf.Engine`; `SQLRBFRepository`
  exists but is not wired), disbursement/CEP tracking, SAT ingestion state.
- Payment state change, ledger journal, RESICO monthly accumulator,
  settlement record, webhook exactly-once key and outbox event are written in
  **one transaction** (`store.SQLMoneyStore`, see `docs/MONEY_FLOWS.md`);
  posting errors roll back and return 5xx.

## Outbox dispatcher

With a database configured, `cmd/server` starts `pkg/outbox.Dispatcher` in a
goroutine; on SIGTERM it is stopped after the HTTP server drains.

- `store.SQLOutboxRepository.ClaimDueEvents` claims due events with
  `FOR UPDATE SKIP LOCKED` and marks them `PROCESSING` (safe with several
  replicas). Events stuck in `PROCESSING` longer than the lease (5 min) are
  reclaimed.
- On publisher error (or panic) `MarkRetry` increments `retry_count`, stores
  `last_error` and sets `next_attempt_at = now + 1s * 2^retry_count` (capped at
  5 min, migration 004). At `max_retries` (default 5) the event becomes
  `FAILED` (dead letter).
- Delivery is at-least-once: publishers must be idempotent on the event ID.
- Publishers: `log` (default; logs metadata, never the payload) and `noop`.
  Real targets implement `outbox.Publisher`.
- Producers (MVP-07): `payment.settled`, `payment.voided`, `payment.refunded`,
  `codi.confirmed`, `spei.abono_received` are enqueued with `SaveEventTx`
  inside the money transaction (see `docs/MONEY_FLOWS.md`).

## Environment variables

| Variable | Default | Purpose |
|---|---|---|
| `DATABASE_URL` | empty (in-memory mode) | e.g. `postgres://user:pass@host:5432/softpos?sslmode=require` |
| `SQL_DRIVER` | `pgx` | database/sql driver name |
| `MIGRATIONS_DIR` | empty (embedded) | on-disk migrations override |
| `OUTBOX_POLL_INTERVAL` | `2s` | dispatcher poll interval (Go duration) |
| `OUTBOX_BATCH_SIZE` | `50` | events claimed per pass |
| `OUTBOX_PUBLISHER` | `log` | `log` or `noop` |
| `DEFAULT_PLATFORM_FEE_BPS` | `200` | platform tariff for merchants without `platform_fee_bps` (0..10000) |
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
