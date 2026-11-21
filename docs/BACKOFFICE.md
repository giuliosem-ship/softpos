# Internal backoffice

Kerno operators review merchant onboardings (KYB), block merchants, search
payments, inspect a merchant's ledger, retry stuck payouts and work the
processor reconciliation report. Code: `pkg/api/handlers_backoffice.go`,
`pkg/api/backoffice_static.go`, `web/backoffice/` (UI), `cmd/operatorctl`,
`pkg/store/backoffice.go` (audit log, payout and journal queries), migration
`014_backoffice`.

## Who is an operator

A **platform operator** is a user (phone) listed in `platform_operators`. It is
global — not a membership of a merchant — and it is created only by trusted
tooling, **never by a public endpoint**:

| Way | How |
|---|---|
| Bootstrap at boot | `BACKOFFICE_BOOTSTRAP_OPERATOR_PHONE=+52…` — `cmd/server` creates the user if needed and grants it (idempotent, `created_by = bootstrap:env`). |
| CLI | `DATABASE_URL=… go run ./cmd/operatorctl add|remove|check +52…` (database already migrated; `created_by = operatorctl:<os user>`). The same tool loads a SAT 69-B file: `operatorctl load-69b <file.csv> [--override-shrink] [--override-changes]` (audit `sat69b.load_file`). |

**Segregation of duties:** an operator who is also a member of a merchant
(e.g. its owner) cannot approve that merchant's onboarding nor unblock it
(`403 OPERATOR_IS_MERCHANT_MEMBER`); another operator must.

Removal takes effect on the next request: every backoffice call re-checks
`platform_operators`, and a refresh of a backoffice session fails with `403`.

Login is the normal OTP flow (`docs/AUTH.md`) selecting the pseudo-tenant
`merchant_id = "kerno:backoffice"` (`auth.BackofficeScope`) in
`/api/v1/auth/otp/verify` (or `/refresh`): only platform operators get it,
with role `operator`; anyone else gets `403 NOT_A_MERCHANT_MEMBER`. A
backoffice token is not a merchant token: merchant routes answer `403`
(path anti-IDOR, `MERCHANT_NOT_APPROVED`), and merchant tokens — including
per-merchant `operator` memberships — answer `403 BACKOFFICE_FORBIDDEN` on
the backoffice API. (`GET /api/v1/acquirer/reconciliation` keeps accepting
any `operator` role, as before.)

## API — `/api/v1/backoffice/*` (platform operator only, `Cache-Control: no-store`)

| Route | Purpose | Audit action |
|---|---|---|
| `GET me` | operator id (UI session check) | — |
| `GET merchants?kyb_status=&blocked=true\|false&limit=` | onboardings, newest update first (`kyb_status`, `reason`, 69-B, identity check, block flag) | — |
| `GET merchants/{merchant}` | profile, full KYB (application, checks, identity verdict and provider reference, processor sub-merchant, decision, block), documents (metadata incl. superseded), settlement account | `kyb.view` (written **before** serving; audit failure → `503`) |
| `GET merchants/{merchant}/documents/{document}` | decrypted file, sniffed `Content-Type`, `Content-Disposition: attachment`, `nosniff` | `document.download` (before serving, fail closed) |
| `POST merchants/{merchant}/kyb/approve {"note"?}` | `IN_REVIEW`/`SUBMITTED` → `APPROVED`: assigns the virtual CLABE if missing, onboards the sub-merchant at the processor (`docs/KYB.md`); `409 KYB_INVALID_TRANSITION`, `409 RFC_LISTED_69B` (69-B lookup found the RFC listed), `502 ACQUIRER_ONBOARDING_FAILED` (stays in review, retry) | `kyb.approve` |
| `POST merchants/{merchant}/kyb/reject {"reason"}` | → `REJECTED` with the reason shown to the owner (`400 REASON_REQUIRED`) | `kyb.reject` |
| `POST merchants/{merchant}/block {"reason"}` | any status; every money operation of the merchant → `403 MERCHANT_BLOCKED` (`409 ALREADY_BLOCKED`) | `merchant.block` |
| `POST merchants/{merchant}/unblock {"reason"?}` | lifts the block (`409 NOT_BLOCKED`). If the RFC is still `PRESUNTO`/`DEFINITIVO` in the 69-B list the `reason` is required (`400 REASON_REQUIRED`) and a documented exception for that situation is recorded (`"sat69b_exception": true`; the next loads respect it until the situation changes); `409 SAT69B_STATUS_UNKNOWN` when a merchant recorded as listed cannot be checked (stale list). See `docs/KYB.md` "69-B block lifecycle" | `merchant.unblock` (+ `sat69b.exception`) |
| `GET merchants/{merchant}/ledger?limit=` | balances (available, tax escrow, RBF) + journal lines of the merchant accounts, newest first | `ledger.view` |
| `GET payments?merchant_id=&id=&kind=card\|spei\|codi&from=&to=&limit=&before=` | cross-merchant search over the sales view (`docs/MONEY_FLOWS.md`); dates RFC 3339 or `YYYY-MM-DD` (Mexico City day) | — |
| `GET payouts?merchant_id=&status=&limit=` | payouts newest first | — |
| `POST payouts/{clave}/retry` | runs the payout reconciler (`payouts.Reconciler.ReconcilePayout`) on one payout `PENDING_RECONCILIATION` (or `RESERVED` older than 10 min, so an in-flight `registraOrden` is never raced): STP status → `SETTLED` / `DISPATCHED` / compensated `FAILED` / still `UNKNOWN`; `409 PAYOUT_NOT_RETRYABLE` otherwise | `payout.retry` |
| `GET reconciliation?status=&limit=` | processor reconciliation report (same as `GET /api/v1/acquirer/reconciliation`) | — |
| `POST reconciliation/{item}/resolve {"note"}` | resolve a difference (same as the acquirer endpoint) | `reconciliation.resolve` |
| `GET audit-log?merchant_id=&action=&limit=` | the audit log, newest first | — |
| `GET invoices?status=FAILED,PENDING&merchant_id=&limit=` | CFDI stampings to look at (default failed + pending), with attempts and error detail (`docs/CFDI.md`) | — |
| `POST invoices/{invoice}/retry` | re-stamps a `PENDING` / `FAILED` invoice through the PAC (a `FAILED` one re-links its sales first): `409 INVOICE_NOT_RETRYABLE`, `409 SALE_ALREADY_INVOICED`, `422 RETRY_FAILED` (CSD unavailable) | `invoice.retry` |
| `GET sat69b` | SAT 69-B list status: loaded, `stale`, version, source, published date, row / malformed counts, fetched / verified times, max age, configured source, last 20 load attempts (`docs/KYB.md`) | — |
| `POST sat69b/sync {"override_shrink"?, "override_changes"?, "reason"?}` | downloads `SAT_69B_URL` now and swaps the list in; re-checks approved merchants (listed ones blocked, 69-B blocks of RFCs no longer listed lifted). `409 LIST_SHRANK` (shrink protection; retry with `override_shrink: true` and a `reason`, `400 REASON_REQUIRED` without it), `409 LIST_CHANGED` (change protection; `override_changes: true` and a `reason`), `422 SAT69B_INVALID_FILE`, `502 SAT69B_FETCH_FAILED`, `503 SAT69B_SOURCE_NOT_CONFIGURED` | `sat69b.sync` (every attempt, with result) |

Merchant-scoped operator actions outside the prefix are audited too:
`POST /api/v1/merchants/{id}/settlement-account/verify`
(`settlement_account.review`) and `POST
/api/v1/acquirer/reconciliation/{item}/resolve` (`reconciliation.resolve`).

## Audit log (`operator_audit_log`)

Append-only rows: `operator_id` (user id), `action`, `merchant_id`, `target`
(document / payout / reconciliation item), `before_json`, `after_json`,
`detail` (note / reason / outcome), `created_at`. Decisions record a state
summary (status, reason, block, processor sub-merchant, virtual CLABE) — not
the personal data itself. Reads of personal data (`kyb.view`,
`document.download`) are written before the data is served and fail closed;
for mutations the row is written after the change, and a failed write is
logged as `ERROR ALERT operator audit write failed`.

## UI — `/backoffice/`

Static files in `web/backoffice/` (plain HTML/JS/CSS, no build step, no
third-party code), served by `cmd/server` at `/backoffice/` (public GET; the
directory is `web/backoffice` next to `STATIC_DIR`). Headers:
`Content-Security-Policy: default-src 'none'; script-src 'self'; style-src
'self'; img-src 'self' blob: data:; connect-src 'self'; frame-src blob:;
base-uri 'none'; form-action 'none'; frame-ancestors 'none'`,
`X-Frame-Options: DENY`, `nosniff`, `Referrer-Policy: no-referrer`,
`Cache-Control: no-store`. No inline script or style; every API value is
rendered with `textContent`. The access token stays in memory and the
refresh token in `sessionStorage` (per tab).

Screens: operator OTP login; merchants by KYB status / block with detail,
document download, approve / reject / block / unblock and ledger; payment
search; payouts with "retry reconciliation"; processor reconciliation
(resolve); audit log.

## Pending

Per-operator permissions (all platform operators can do everything), 2FA /
hardware keys for operators, IP allowlist / VPN in front of `/backoffice/`,
correcting journals for reconciliation differences, export of the audit log
to immutable storage. CFDI stamping retries exist (above) but run against
the mock PAC until a real one is contracted.
