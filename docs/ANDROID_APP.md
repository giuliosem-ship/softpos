# Kerno Android app (merchant)

Module: `mobile/android/app` — a real Android application (Compose, Material 3).
Code package `mx.softpos.app`; store id `mx.kerno.softpos` (`.debug` suffix in debug).

## Toolchain

| Piece | Version | Why |
|---|---|---|
| Gradle wrapper | 8.14.3 | AGP 8.x does not support Gradle 9. |
| Android Gradle Plugin | 8.13.2 | Last 8.x line; defaults to build-tools 35.0.0. AGP 9 needs Kotlin ≥ 2.2 (built-in Kotlin) — upgrade both together later. |
| Kotlin / Compose compiler | 2.0.21 | Unchanged from the catalog. |
| compileSdk / targetSdk | 35 | Installed platform. Play will require 36 in 2026-08+ for new uploads: bump before the first release. |
| minSdk | 28 (Android 9) | Floor of the Tap to Pay on Android SDKs evaluated and of hardware-backed Keystore AES-GCM. |
| JDK | 17 | |

## Build and run

```bash
cd mobile/android
export JAVA_HOME=/opt/homebrew/opt/openjdk@17   # any JDK 17
echo "sdk.dir=$HOME/Library/Android/sdk" > local.properties   # git-ignored
./gradlew :app:testDebugUnitTest   # JVM unit tests (JUnit 5)
./gradlew :app:lintDebug
./gradlew :app:assembleDebug       # app/build/outputs/apk/debug/app-debug.apk
./gradlew :app:installDebug        # emulator/device
```

Debug talks to `http://10.0.2.2:8080` (the host's backend from the emulator);
cleartext is allowed only to `10.0.2.2`, `localhost` and `127.0.0.1`, and only in
the debug network security config. Release uses `https://api.kerno.mx`
(placeholder), HTTPS only, R8 minify + resource shrinking, **unsigned**: no
signing keys live in the repo (debug uses the default debug keystore).

Log in with a phone number; with the backend in `DEV_MODE` the OTP code is
printed in the server log.

CI (`.github/workflows/ci.yml`, job `kotlin`) runs
`:app:testDebugUnitTest :app:lintDebug :app:assembleDebug`.

## Architecture

- `MainActivity` → `KernoApp` (Compose). FLAG_SECURE is set app-wide through
  `WindowSecurityGuard` (Android adapters in `ui/security/AndroidWindowSecurity.kt`).
- Manual DI: `AppContainer` (one per process, owned by `KernoApplication`).
- Navigation (navigation-compose): no session → login; session without a
  merchant and several memberships → merchant picker; otherwise bottom bar
  *Inicio* (balances), *Cobrar*, *Ventas* (list + detail with refunds),
  *Perfil* (profile, CLABE, switch merchant, logout). The merchant app is
  keyed on user + merchant, so switching tenant rebuilds every ViewModel.
- ViewModels are `androidx.lifecycle.ViewModel`s with injected APIs and an IO
  dispatcher, unit-tested with fakes (`src/test`). API clients are blocking
  OkHttp + kotlinx.serialization, called on the IO dispatcher.
- Money is `Long` cents end to end (`MoneyFormatter` formats without floats);
  the keypad reuses `PosTerminalViewModel`, the QR reuses `CodiQrGenerator`.

### Auth (`auth/`, contract in `docs/AUTH.md`)

- `HttpAuthApi`: `/api/v1/auth/otp/request`, `/otp/verify`, `/refresh`, `/logout`
  over a client **without** the bearer interceptor.
- `SessionManager`: persists the token pair, refreshes **single-flight**
  (concurrent 401s rotate the refresh token once; a second use would trip the
  backend's reuse detection and revoke the family), 4xx on refresh ends the
  session, network/5xx keep it. Logout revokes the family server-side and
  wipes the device.
- `AuthInterceptor` adds `Authorization: Bearer` and refreshes proactively 30 s
  before expiry; `TokenAuthenticator` refreshes once on 401 and retries once.
- Storage: `EncryptedTokenStore` keeps only AES-256-GCM ciphertext in private
  SharedPreferences; the key is generated in the Android Keystore
  (`AndroidKeystoreKeys`). Interfaces (`TokenStore`, `SecretCipher`,
  `KeyValueStore`) make it JVM-testable. Undecryptable blobs are dropped (user
  logs in again). Backups/device transfer exclude app data.

### Charge (card contract in `docs/ACQUIRER_INTEGRATION.md` §2)

- **Tarjeta (Tap)**: `POST /api/v1/card-payments/intents {amount_cents, currency:"MXN"}`
  with an `Idempotency-Key` that is stable per attempt (no `is_persona_moral`:
  the backend derives it) → `TapToPayProvider.collect(intent)` starts the
  processor SDK with `provider_payment_id` + `client_secret` (never logged) →
  poll `GET /api/v1/card-payments/{id}` until `final:true` (backoff 0.5 s ×1.5,
  capped at 3 s, 60 s timeout → "en confirmación", check *Ventas*). The SDK
  result is informative only; the screen follows the backend status:
  `CAPTURED` (or later refund/chargeback states) = approved, `DECLINED` shows
  the `decline_code`, `CANCELED` back to the keypad.
- Retries: a network error or `502 ACQUIRER_UNAVAILABLE` keeps the key, so
  the retry replays the same intent (one charge). Any other answer
  (`409 IDEMPOTENCY_KEY_REUSED`, `503 ACQUIRER_NOT_CONFIGURED`, a KYB `403`,
  a decline, a cancel) drops it and the next attempt is a new intent.
- *Cancelar cobro* while waiting for the card stops the SDK and calls
  `POST /api/v1/card-payments/{id}/cancel` (`200` canceled / `202` requested;
  the webhook moves the status). An SDK failure also cancels the intent.
- **SPEI QR**: reads the merchant's `clabe` (`GET /api/v1/merchants/{id}`),
  draws a QR with `{v, type:"SPEI", clabe, beneficiary, amount_cents, reference}`
  (7-digit numeric reference, 15 min expiry) plus the CLABE and reference as text.
  This is a Kerno payload, not CoDi/DiMo; payment is credited when the STP
  abono arrives.

### Sales

`GET /api/v1/merchants/{id}/sales?limit=50&before=<cursor>` →
`{"items":[{id, kind:"card"|"spei"|"codi", status, amount_cents, created_at, card_payment_id?}], "next_cursor"}`.
The list asks for the next page (`before=next_cursor`) when its end is
reached; `null` = last page; duplicates across pages are dropped; *Actualizar*
starts over from the first page.

### Refunds and sale detail (`docs/ACQUIRER_INTEGRATION.md` §4)

The detail of a card sale reads `GET /api/v1/card-payments/{card_payment_id}`
and shows captured / refunded / chargeback amounts, the settlement split
(fee, tax set-aside, net) and the decline code. Owners (session role
`owner`; hidden for cashiers/operators, the backend enforces it too) can
refund the whole refundable remainder (`{}`) or a typed amount
(`MoneyParser`, Long cents, ≤ 2 decimals, ≤ refundable), after a
confirmation dialog: `POST /api/v1/card-payments/{id}/refunds` with an
`Idempotency-Key` stable per attempt and amount (kept on network errors and
`502 ACQUIRER_UNAVAILABLE`, dropped on any other answer). The `202 PENDING`
request is shown as such; the money moves when the processor's
`payment.refunded` arrives (in debug the DEV simulator plays it with
`{"outcome":"refund","refund_id"}`). Statuses `PARTIALLY_REFUNDED`,
`REFUNDED`, `CHARGEBACK` (amount debited from the available balance) and
`CHARGEBACK_REVERSED` have their own labels.

### Merchant picker

The token pair's `memberships` (`merchant_id`, `role`) are stored in the
encrypted session. When login returns no `merchant_id` and there are several
memberships, the picker is shown before the app; *Perfil → Cambiar de
comercio* opens it too. Choosing calls `POST /api/v1/auth/refresh
{refresh_token, merchant_id}` under the same single-flight lock as the 401
refresh; only a `401` ends the session (`403 NOT_A_MERCHANT_MEMBER`, 429 or
5xx keep the current one). Merchants are listed by id and role: the backend
has no endpoint that names merchants outside the current tenant.

## What is mocked or provisional

| Item | Status |
|---|---|
| `SalesApi` | Contract defined with the backend batch that builds `/merchants/{id}/sales` in parallel; the client follows it exactly. |
| `TapToPayProvider` | Debug: `MockTapToPayProvider` waits 1.5 s and calls `POST /api/v1/dev/card-payments/{id}/simulate {"outcome":"tap"}` (cancel → `"cancel"`), so a debug build against a `DEV_MODE` backend with the mock acquirer runs the real intent → webhook → ledger → polling path. Outcomes follow the backend mock scenarios: amounts ending in **51** centavos are declined, **52** make the processor time out (`502`, retry with the same key). Without `DEV_MODE` the simulate call fails and the charge ends as a tap failure (intent canceled). Release: `UnavailableTapToPayProvider` (card button disabled; `BuildConfig.DEV_TAP_SIMULATOR=false`, no simulator wired) so a release build can never fake a processor event. |
| Legacy EMV client | `SoftPosApiClient.authorizeEmv` (`/api/v1/payments/emv/authorize`, `410` without `DEV_MODE`) was removed. The pure fake of the card backend lives only in unit tests (`FakeCardBackend`). |
| Play Integrity | Not wired into the app yet (`PlayIntegrityClient` is still pure logic). |

## Next steps

1. Processor **Tap to Pay on Android SDK**: implement `TapToPayProvider` with it
   (device checks, attestation, permissions); the card contract is already final.
2. Real **Play Integrity**: Google SDK token + `/api/v1/auth/attestation/verify`,
   bound to login. Note the backend default `PLAY_INTEGRITY_PACKAGE` is still
   `mx.softpos.app`; the app id is now `mx.kerno.softpos`.
3. **Signing and distribution**: upload key in CI secrets, Play App Signing,
   internal/closed track, forced minimum version; bump targetSdk to 36.
4. **Crash reporting / telemetry** (Crashlytics or Sentry) with PII scrubbing.
5. Remaining F9 screens: KYB onboarding, CFDI invoicing; F10 sale receipt.
