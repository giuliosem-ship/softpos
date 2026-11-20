# Authentication, sessions and tenancy

Kerno merchants log in with a phone number and a one-time code (OTP). Login
returns a short-lived **access JWT** and an opaque **refresh token**. The access
token carries the tenant (`merchant_id`) every protected route works on.

Code: `pkg/auth` (service, JWT, memory repository, OTP sender),
`pkg/store/auth_store.go` (PostgreSQL repository, migration `005_auth`),
`pkg/api/handlers_auth.go` (HTTP).

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
   after `OTP max attempts` the challenge is deleted (`429 OTP_LOCKED`).
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

## Configuration

| Env | Meaning |
|---|---|
| `JWT_SECRET` | ≥ 32 bytes. Required unless `DEV_MODE=true`; the production guard rejects missing/short values. |
| `DEV_MODE` | `true`/`1` enables explicit local-dev conveniences: ephemeral random JWT secret if `JWT_SECRET` is unset and OTP codes printed in the log. Rejected in production. |

OTP delivery: only `auth.LogSender` exists (no SMS/WhatsApp provider yet).
Outside `DEV_MODE` it logs a masked phone and **does not reveal the code**, so
real logins need a provider implementing `auth.OTPSender`.
