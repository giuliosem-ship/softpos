# Kerno (SoftPOS) — plataforma de cobro y apartado fiscal para PyMEs en México

Objetivo del producto: un comercio PyME (inicialmente RESICO persona física) cobra con tarjeta contactless en su Android y por SPEI, ve cuánto es suyo y cuánto es de impuestos, recibe su dinero en su cuenta y emite CFDI 4.0.

## Estado actual (2026-11-20)

**Prototipo con la Fase 1 del MVP (cimientos) cerrada. No mueve dinero real.**

- **Hecho (Fase 1, MVP-01..13):** persistencia PostgreSQL con migraciones embebidas; login OTP + JWT con tenant tomado del token; webhooks firmados; posteo de dinero atómico y exactly-once (pago + asiento + acumulado RESICO + outbox en una transacción); payouts solo a la CLABE de liquidación verificada, con `Idempotency-Key` obligatoria y reconciliador de recuperación; outbox con leases; secretos fuera del repo (`.env.example`); CI en GitHub Actions.
- **No existe todavía:** procesador de tarjetas real (la "autorización" EMV es simulada), PAC real (solo `MockPACClient`), STP/CEP reales (el servidor usa mocks), proveedor OTP real, KMS real, app Android distribuible (el módulo es Kotlin/JVM, sin APK), conciliación diaria (`cmd/reconciler` es un stub), backoffice.
- Módulos de fase 2 implementados pero **no integrados** al servidor: `iso8583`, `crypto` (DUKPT/HSM simulado), `clearing` (STEC/MIF), `disputes`, `rbf`.

El inventario completo, los gaps por funcionalidad y el plan por fases están en [`docs/KERNO_MVP_GAP_ANALYSIS.md`](docs/KERNO_MVP_GAP_ANALYSIS.md) (backlog vivo). Estado del loop: [`tools/loop/HANDOFF.md`](tools/loop/HANDOFF.md).

## Estructura

- `cmd/server` — servidor HTTP: configuración por entorno (`.env.example`), guard de producción, migraciones, dispatcher del outbox y reconciliador de payouts.
- `pkg/` — dominio: `ledger` (partida doble en centavos `int64`), `fiscal` (RESICO, CFDI 4.0), `stp` (CLABE, firmas, cliente y mocks), `payments` (EMV TLV, CoDi), `payouts`, `outbox`, `auth`, `identity` (RFC/CURP, 69-B, bóveda CSD), `store` (PostgreSQL + equivalentes en memoria), `api` (rutas REST).
- `migrations/` — esquema SQL versionado (embebido en el binario).
- `web/pos/` — Web POS de demostración (simulación de tap, QR SPEI/CoDi, desglose fiscal). Web NFC no lee tarjetas EMV: es solo demo.
- `mobile/android/` — lógica Kotlin/JVM de la futura app (sin Android Gradle Plugin todavía).
- `docs/` — `PERSISTENCE.md`, `AUTH.md`, `MONEY_FLOWS.md`, gap analysis y especificaciones objetivo.

## Ejecución

### Local (Go 1.27.1)
```bash
# Modo desarrollo: secreto JWT efímero, códigos OTP en el log y sesión demo
DEV_MODE=true go run ./cmd/server
open http://localhost:8080/pos/
```

Fuera de `DEV_MODE` el servidor exige `JWT_SECRET` (≥ 32 bytes) y `VAULT_MASTER_KEY_HEX` (llave de 32 bytes de la bóveda CSD; ver [`docs/AUTH.md`](docs/AUTH.md)). Todas las rutas `/api/v1/*` requieren `Authorization: Bearer <token>` salvo la lista pública (salud, Web POS, login OTP, webhooks firmados). Con `APP_ENV=production` el guard rechaza secretos ausentes, débiles, de ejemplo o de dev.

Sin `DATABASE_URL` el servidor corre en memoria (el estado se pierde al reiniciar). Con `DATABASE_URL` usa PostgreSQL (driver `pgx`) y aplica las migraciones embebidas al arrancar; ver [`docs/PERSISTENCE.md`](docs/PERSISTENCE.md). Flujos de dinero (settle, devoluciones, abonos, payouts y su recuperación): [`docs/MONEY_FLOWS.md`](docs/MONEY_FLOWS.md). Métricas Prometheus (`METRICS_ADDR`) y reglas de alerta: [`docs/OBSERVABILITY.md`](docs/OBSERVABILITY.md).

### Docker Compose (PostgreSQL + Redis + servidor)
```bash
cp .env.example .env   # .env está en .gitignore; reemplaza cada CHANGE_ME (openssl rand -hex 32)
docker compose up --build
```

`docker-compose.yml` no contiene secretos ni activa `DEV_MODE`: toda la configuración sale de `.env`. `.env.example` documenta cada variable que lee el servidor.

## Validación

La misma que corre el CI (`.github/workflows/ci.yml`) y que se exige antes de cada push (ver [`AGENTS.md`](AGENTS.md) §2):

```bash
go vet ./...
go test -race -count=1 ./...
TEST_DATABASE_URL=postgres://… go test -race -count=1 -run PGIntegration ./...   # PostgreSQL 16 real
node --test web/pos/softpos_sdk.test.js
(cd mobile/android && JAVA_HOME=<jdk17> ./gradlew :app:test)
```

## Proceso de desarrollo

El backlog (`tools/loop/tasks.json`) se ejecuta por lotes con Claude Code: subagente implementador (RED → GREEN) y subagente auditor independiente (AUDIT) por lote, un commit por tarea. El orquestador multi-modelo de LM Studio (`tools/loop/loop_runner.py`) queda como herramienta legacy opcional.
