# Authentication, sessions and tenancy

Kerno merchants log in with a phone number and a one-time code (OTP). Login
returns a short-lived **access JWT** and an opaque **refresh token**. The access
token carries the tenant (`merchant_id`) every protected route works on.

Code: `pkg/auth` (service, JWT, memory repository, OTP sender),
`pkg/store/auth_store.go` (PostgreSQL repository, migrations `005_auth`,
`006_attestation_nonces`), `pkg/api/handlers_auth.go` (HTTP),
`pkg/api/middleware_auth.go` (gate, allowlist, tenant helpers),
`pkg/api/ratelimit.go`.

## Model

| Entity | Table | Notes |
|---|---|---|
| User | `users` | Identified by E.164 phone (unique). Created on first successful OTP login. |
| Membership | `merchant_memberships` | `(user_id, merchant_id)` PK, `role` ∈ `owner`, `cashier`, `operator`. |
| OTP challenge | `otp_challenges` | One per phone: `code_hash` = HMAC-SHA256(pepper, phone:code), `expires_at`, `attempts`, `last_sent_at`. |
| Refresh token | `refresh_tokens` | Stored as SHA-256 digest; `family_id` groups all rotations of one login; `revoked_at`, `replaced_by`. |

The OTP pepper is derived from `JWT_SECRET` (HMAC with a fixed label), so a
database dump alone cannot brute-force the 6-digit codes.

## Flows

1. `POST /api/v1/auth/otp/request {"phone":"+52..."}` → `202 {"status":"OTP_SENT"}`.
   Generates a 6-digit code (crypto/rand), stores only its hash, sends it via
   `auth.OTPSender`. A new request for the same phone within the resend
   interval returns `429 OTP_RATE_LIMITED` + `Retry-After`. Invalid phone → `400 INVALID_PHONE`.
2. `POST /api/v1/auth/otp/verify {"phone","code","merchant_id"?}` → `200` token pair.
   Each attempt is counted atomically *before* comparing (constant-time);
   after `OTP max attempts` the challenge stays locked (`429 OTP_LOCKED`) until
   a new code is requested, which still waits out the resend interval.
   Wrong/expired/used code → `401 OTP_INVALID`. The code is single-use.
   Tenant selection: `merchant_id` if given (must be a membership, else
   `403 NOT_A_MERCHANT_MEMBER`); otherwise the only membership; otherwise no
   tenant (new user, or several merchants → choose with refresh).
3. `POST /api/v1/auth/refresh {"refresh_token","merchant_id"?}` → `200` new pair.
   The presented token is revoked and replaced (rotation). Presenting an
   already-rotated token is treated as theft: the **whole family is revoked**
   and `401 REFRESH_TOKEN_REUSED` is returned. Concurrent rotations of the same
   token: exactly one wins (conditional `UPDATE ... WHERE revoked_at IS NULL`),
   the others trigger reuse detection. `merchant_id` re-scopes the session to
   another membership (e.g. right after creating a merchant).
4. `POST /api/v1/auth/logout {"refresh_token"}` → `204`; revokes the family.
   Access tokens are stateless and stay valid until they expire (short TTL).

Token pair response:

```json
{"access_token":"…","token_type":"Bearer","expires_in":900,
 "refresh_token":"rt_…","refresh_expires_in":2592000,
 "user_id":"usr_…","merchant_id":"…","role":"owner","memberships":[…]}
```

## Access token

HS256 JWT signed with `JWT_SECRET`. Claims: `sub` (user id), `merchant_id`
(tenant, may be empty), `role`, `typ:"access"`, `iss:"kerno"`, `jti`, `iat`,
`exp`. Validation rejects any `alg` other than HS256, a missing `exp`, and
tokens whose `typ` is not `access`.

## Lifetimes and limits

| Setting | Default | Env |
|---|---|---|
| Access token TTL | 15 min | `AUTH_ACCESS_TTL` (Go duration) |
| Refresh token TTL | 30 days | `AUTH_REFRESH_TTL` |
| OTP TTL | 5 min | — |
| OTP resend interval | 60 s | — |
| OTP max attempts | 5 | — |

## Tenancy (MVP-05)

`Server.Router()` wraps the mux with `authGate`: every route requires a valid
access token **unless it is on the explicit public allowlist** (default deny,
including unknown paths). The middleware puts `merchant_id`, `user_id` and
`role` from the token into the request context.

Public allowlist (`publicRoutes` in `pkg/api/middleware_auth.go`):

| Route | Why public |
|---|---|
| `GET /healthz`, `GET /`, `GET /pos/*` | Health check and static Web POS |
| `POST /api/v1/auth/otp/request`, `/otp/verify`, `/refresh`, `/logout` | Login flow |
| `GET`/`POST /api/v1/auth/attestation-nonce`, `POST /api/v1/auth/attestation/verify` | Play Integrity handshake |
| `POST /api/v1/auth/dev-session` | `404` unless `DEV_MODE` |
| `POST /api/v1/webhooks/stp/abono`, `POST /api/v1/webhooks/codi/confirm`, `POST /api/v1/webhooks/acquirer/{provider}` | Authenticated by their own signatures |

Rules for protected handlers:

- The tenant **always comes from the token**. A `merchant_id` in the body or
  path that differs from it → `403 TENANT_MISMATCH` / `403` anti-IDOR. Tokens
  without a selected merchant → `403 MERCHANT_SCOPE_REQUIRED`.
- Resources looked up by their own id that belong to another tenant
  (payments in settle/reverse, disbursements by `clave_rastreo`, CEP
  reconcile) answer `404` as if they did not exist.
- CoDi QR always pays into the tenant's registered CLABE; a different
  `seller_clabe` → `403 SELLER_CLABE_NOT_OWNED`, none registered → `409`.
- Idempotency keys (client `Idempotency-Key` or derived) are prefixed with the
  tenant, so one merchant can never replay another's cached response.
- Roles: `POST /api/v1/disbursements` and `PUT
  /api/v1/merchants/{id}/settlement-account` require `owner`; refunds of
  settled payments (`POST /api/v1/payments/emv/reverse` on a `SETTLED` payment)
  require `owner` unless `ALLOW_CASHIER_REFUNDS=true` (voids of not-yet-settled
  payments stay open to cashiers); `POST
  /api/v1/merchants/{id}/settlement-account/verify` requires `operator` (or the
  owner in `DEV_MODE`, for the demo); `POST /api/v1/rbf/loans` requires `owner`
  or `operator`; other tenant routes accept any member. See
  `docs/MONEY_FLOWS.md`.
- `POST /api/v1/merchants` requires a token of an existing user; the **server
  generates the id** (`mer_…`; a client `id` is ignored), validates the RFC
  format (`400 INVALID_RFC`), creates a `DRAFT` onboarding (`kyb_status` in
  the response) and the caller becomes `owner`. The virtual collection CLABE
  is assigned by the server only when an operator approves the KYB (a client
  `virtual_clabe` is ignored); until then money operations answer `403
  MERCHANT_NOT_APPROVED` (`docs/KYB.md`). Then `POST /api/v1/auth/refresh
  {"refresh_token","merchant_id"}` returns a token scoped to the new merchant.

Rate limiting: `/api/v1/auth/*` is limited per client IP (fixed window,
`AUTH_RATE_LIMIT_PER_MINUTE`, default 30) → `429 RATE_LIMITED` + `Retry-After`.
The IP is the TCP peer (`X-Forwarded-For` is ignored); behind a proxy put the
limit in the gateway. CORS/TLS are also gateway concerns.

## Device attestation (Play Integrity)

1. `GET`/`POST /api/v1/auth/attestation-nonce` → `{"nonce","issued_at_ms","ttl_seconds":300}`
   (matches Android `AttestationNonce`). 32 random bytes, stored as SHA-256
   (`attestation_nonces`, migration 006), single use.
2. `POST /api/v1/auth/attestation/verify {"nonce","integrity_token"}` (token may
   also come in `X-Play-Integrity-Token`). The nonce is burned first, then the
   token is checked with `identity.PlayIntegrityVerifier` (package, nonce,
   freshness, device integrity) → `200 ATTESTED` | `401 ATTESTATION_NONCE_INVALID`
   | `401 ATTESTATION_FAILED` | `503 ATTESTATION_NOT_CONFIGURED`.

The verifier still checks the simulated HMAC-signed token format; the real
Google decryption/verification API and binding the verdict to login are pending
(F9 "Play Integrity real").

## Dev-mode demo session

`POST /api/v1/auth/dev-session {"merchant_id"}` (only with `DEV_MODE=true`)
returns an owner access token for an existing merchant through a synthetic
user. The Web POS demo (`web/pos/app.js`) calls it at start-up;
`SoftPOSClient.setAccessToken`/`verifyOTP` attach `Authorization` to every call.

## Signed webhooks (MVP-06)

Webhooks are public routes authenticated by signatures, never by JWT.

**CoDi confirmation** — `POST /api/v1/webhooks/codi/confirm`:

```
X-Kerno-Timestamp: <unix seconds>
X-Kerno-Signature: v1=<hex(HMAC-SHA256(CODI_WEBHOOK_SECRET, timestamp + "." + raw_body))>
```

- The MAC covers the **raw body bytes** (max 1 MiB) and the timestamp; the
  comparison is constant-time (`hmac.Equal`).
- Timestamp must be within ±5 min of the server clock → else `401 STALE_TIMESTAMP`.
- Missing/malformed/wrong signature → `401 INVALID_SIGNATURE`; nothing is credited.
- Each accepted signature is remembered for 10 min in the idempotency store
  (Redis/PostgreSQL when configured): the same signature again → `409 WEBHOOK_REPLAYED`.
  Provider retries must be re-signed with a fresh timestamp; they are then
  idempotent on `tracking_key` (cached response, credited once).
- No `CODI_WEBHOOK_SECRET` → `503 WEBHOOK_NOT_CONFIGURED` (fails closed).
- `pkg/api.SignWebhook` / `VerifyWebhookSignature` implement the scheme.

**STP abono** — `POST /api/v1/webhooks/stp/abono` keeps STP's own scheme
(RSA, or HMAC over the canonical string, plus idempotency on `claveRastreo`).
The well-known dev HMAC secret is only used when `DEV_MODE=true`: configuring
it without `DEV_MODE` makes the server refuse to start, and without
`STP_HMAC_SECRET` HMAC-signed abonos are rejected.

**Card processor** — `POST /api/v1/webhooks/acquirer/{provider}` (one path
segment; the only prefix-matched public route): same `X-Kerno-Timestamp` /
`X-Kerno-Signature` scheme with `ACQUIRER_WEBHOOK_SECRET` (or the adapter's own
`WebhookVerifier`), exactly-once by the provider event id instead of a
signature replay cache (see `docs/ACQUIRER_INTEGRATION.md`).
`POST /api/v1/dev/card-payments/{id}/simulate` (DEV_MODE + mock acquirer,
own payments) signs simulated processor events server-side.

`POST /api/v1/dev/codi/simulate-confirm` (DEV_MODE only, authenticated,
caller's own CLABE only) signs a confirmation server-side so the Web POS demo
can simulate a CoDi payment without holding the secret.

## Configuration

| Env | Meaning |
|---|---|
| `JWT_SECRET` | ≥ 32 bytes. Required unless `DEV_MODE=true`; the production guard rejects missing/short values. |
| `DEV_MODE` | `true`/`1` enables explicit local-dev conveniences: ephemeral random JWT secret if `JWT_SECRET` is unset, OTP codes printed in the log, `POST /api/v1/auth/dev-session`. Rejected in production. |
| `AUTH_RATE_LIMIT_PER_MINUTE` | Per-IP budget for `/api/v1/auth/*` (default 30). |
| `PLAY_INTEGRITY_SECRET` | Enables attestation verification; empty → `503`. |
| `PLAY_INTEGRITY_PACKAGE` | Expected Android package (default `mx.softpos.app`). |
| `CODI_WEBHOOK_SECRET` | HMAC secret for the CoDi webhook (≥ 32 bytes in production). Unset → webhook 503; in `DEV_MODE` an ephemeral random one is used. |
| `ACQUIRER_WEBHOOK_SECRET` | HMAC secret of the processor webhook (≥ 32 bytes in production when `ACQUIRER_PROVIDER` is set). Unset → webhook 503; in `DEV_MODE` an ephemeral random one is used. |
| `STP_HMAC_SECRET` | STP HMAC secret. No default; the dev default is accepted only with `DEV_MODE`. Required in production. |
| `VAULT_MASTER_KEY_HEX` | Active 32-byte (64 hex) master key of the CSD vault. Required unless `DEV_MODE=true` (then the public dev key is used, so old dev envelopes keep decrypting); the dev key (in any spelling) and trivial keys (all bytes equal, e.g. all zeros) are refused without `DEV_MODE`. |
| `VAULT_MASTER_KEY_ID` | Key id written into new envelopes (default `v1`; `dev` for the dev key). |
| `VAULT_PREVIOUS_KEYS` | Retired keys still accepted for decryption: `id:hex,id:hex` (rotation). |
| `ALLOW_CASHIER_REFUNDS` | `true` lets cashiers refund settled payments (default: owner-only). |

Production guard (`APP_ENV=production`) refuses to start with `DEV_MODE`,
`JWT_SECRET` < 32 bytes, `CODI_WEBHOOK_SECRET` < 32 bytes, a missing/default
or < 32-byte `STP_HMAC_SECRET`, a missing/default/malformed
`VAULT_MASTER_KEY_HEX`, a malformed `VAULT_PREVIOUS_KEYS`, a
`PLAY_INTEGRITY_SECRET` set but < 32 bytes, a missing `DATABASE_URL` or one
with `sslmode=disable`, `REDIS_ADDR` without a ≥ 16-byte `REDIS_PASSWORD`, any
secret still holding the `.env.example` `CHANGE_ME` placeholder, or demo
seeding. The full list of variables (secret or not) is `ServerEnvVars` in
`cmd/server/config_guard.go`; tests keep it equal to the variables the code
reads, require `.env.example` to document each one and require a guard case
for every secret (MVP-11). `docker-compose.yml` holds no secrets: it loads
`.env` (git-ignored, copied from `.env.example`).

CSD vault keys (MVP-09): `NewServer` no longer has a hardcoded key (it starts
with an ephemeral random one); `cmd/server` installs an
`identity.KeyProvider` built from the variables above
(`identity.StaticKeyProvider`). Each envelope stores the `KeyID` that wrapped
its DEK (AES-256-GCM, key id as AAD); envelopes without `KeyID` are the legacy
format and are opened with any held key. Rotation: set the new key as active,
move the old one to `VAULT_PREVIOUS_KEYS`, re-wrap stored CSDs with
`CSDVaultService.RewrapCSD`, then drop the old key. `identity.KMSKeyProvider`
adapts any KMS behind the `identity.KMSClient` interface (Encrypt/Decrypt); no
cloud SDK is wired yet.

OTP delivery: only `auth.LogSender` exists (no SMS/WhatsApp provider yet).
Outside `DEV_MODE` it logs a masked phone and **does not reveal the code**, so
real logins need a provider implementing `auth.OTPSender`.
