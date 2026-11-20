# Engineering Loop Handoff Checkpoint
**Actualizado:** 2026-11-20 — Fase 1 cerrada (MVP-01..13, Lotes A–D); Fase 2 en curso: Lotes E (procesador, MVP-14..16), F (app Android, AND-30..32) y G (KYB + backoffice + ventas, MVP-17..19) aplicados y auditados
**Estado real:** prototipo con cimientos de MVP. No hay procesador de tarjetas, PAC ni STP reales: los rieles externos siguen simulados (ver `docs/KERNO_MVP_GAP_ANALYSIS.md`).

## Cómo se trabaja ahora
- El loop lo ejecuta Claude Code por lotes: subagente implementador (RED → GREEN) + subagente auditor independiente (AUDIT), un commit por tarea (`[MVP-NN]`, correcciones de auditoría `[MVP-NN-audit]`). Detalle en `AGENTS.md` §2.
- `tools/loop/loop_runner.py` (LM Studio, Qwen/Gemma) queda como herramienta legacy opcional.
- Validación completa antes de push: `go vet`, `go test -race`, integración PostgreSQL (`-run PGIntegration`), `node --test` y tests Kotlin; después, CI verde en GitHub.

## Fase 1 — resumen (cimientos seguros)
| Lote | Tareas | Resultado |
|---|---|---|
| A | MVP-01..03 | Driver `pgx`, migraciones versionadas embebidas, todo el estado de comercios/pagos/CoDi/CSD/acumulado/ledger en PostgreSQL, tests contra Postgres 16 real, dispatcher del outbox con backoff y apagado ordenado. |
| B | MVP-04..06 | Usuarios por teléfono, OTP, JWT de acceso + refresh rotativo, JWT montado en `/api/v1/*`, tenant desde el token (anti-IDOR), webhooks CoDi/STP firmados, `DEV_MODE` explícito. |
| C | MVP-07..09 | `settle` con montos del servidor y posteo atómico (`store.MoneyStore`), webhooks exactly-once, payouts solo a la CLABE verificada, reversa proporcional en devoluciones, llave de la bóveda CSD desde el entorno. |
| D | MVP-10..13 | CI en GitHub Actions + toolchain Go 1.27.1 fijado (Dockerfile no-root, wrapper Gradle 9.8.2); secretos fuera de `docker-compose.yml` (`.env.example`, guard que cubre todo secreto leído); payouts con `Idempotency-Key` obligatoria y reconciliador de recuperación (resultado desconocido / caída); outbox con dueño de lease, reclamo = intento y timeout por publicación; mes RESICO en `America/Mexico_City`; docs y proceso alineados. |

## Validación al cierre de la Fase 1 (Lote D, local)
- `go vet ./...` limpio; `go test -race -count=1 ./...`: todos los paquetes OK.
- `TEST_DATABASE_URL=… go test -race -count=1 -run PGIntegration ./...` contra PostgreSQL 16 (`embedded-postgres`): 26/26.
- `node --test web/pos/softpos_sdk.test.js`: 8/8.
- `cd mobile/android && ./gradlew :app:test` (JDK 17, Gradle 9.8.2): 78/78.
- No ejecutado localmente: `docker build` / `docker compose up` (sin Docker) ni el workflow de GitHub Actions (su primera corrida es la validación real).

## Fase 2 — Lote E (backend del procesador, camino A)
- MVP-14: puerto `acquirer.Provider` + `MockProvider`, `card_payments` (migración 010), `POST /api/v1/card-payments/intents` / `GET /{id}` / `POST /{id}/cancel`; captura confirmada por el procesador → split existente + comisión del procesador en `MoneyStore`; endpoints EMV legacy solo con `DEV_MODE`.
- MVP-15: webhook firmado `POST /api/v1/webhooks/acquirer/{provider}` (exactly-once, tolerante a desorden), devoluciones vía procesador (`POST /{id}/refunds`, el ledger se mueve con el webhook), contracargos en ledger + outbox, `ACQUIRER_WEBHOOK_SECRET` en el guard.
- MVP-16: conciliación de payouts del procesador (`ASSET:ACQUIRER_RECEIVABLE` → `ASSET:BANK_OPERATING`, reporte de diferencias para `operator`, alertas), vigilante de capturas no liquidadas (worker + `cmd/reconciler`).
- Validación: `go vet` limpio, `go test -race` OK, PGIntegration 29/29, `node --test` 8/8. Contrato para Android en `docs/ACQUIRER_INTEGRATION.md` §2.

## Fase 2 — Lote F (app Android, AND-30..32)
- Módulo Android real (Compose, sesión cifrada con Keystore, saldo/cobrar/ventas/perfil); detalle y estado en `docs/ANDROID_APP.md` (su auditoría va aparte).

## Fase 2 — Lote G (KYB, backoffice, ventas)
- MVP-17: KYB `DRAFT → SUBMITTED → IN_REVIEW → APPROVED/REJECTED` (migración 012), documentos cifrados con la bóveda, checks (dígito verificador RFC, solo RESICO PF, 69-B, `kyb.KYCProvider` mock); sin aprobación o bloqueado no hay movimientos de dinero (`docs/KYB.md`).
- MVP-18: backoffice de operadores de plataforma (`kerno:backoffice`, migración 014, bitácora `operator_audit_log`, UI en `/backoffice/` con CSP estricta; `docs/BACKOFFICE.md`).
- MVP-19: `GET /api/v1/merchants/{id}/sales` (migración 013 con backfill), `PLAY_INTEGRITY_PACKAGE=mx.kerno.softpos`, ventana de gracia de 30 s del refresh.
- Auditoría (`[MVP-17-audit]`/`[MVP-18-audit]`): RBF tras la aprobación y solo `operator`; CLABE virtual asignada una sola vez; no se aprueba un RFC 69-B `LISTED`; un operador no aprueba/desbloquea su propio comercio; 69-B `UNKNOWN` sin lista cargada. Pendientes nuevos en el gap analysis (ingesta 69-B, documento subido durante el envío, bitácora mutable a nivel BD).
- Validación: `go vet` limpio, `go test -race` 20/20 paquetes, PGIntegration 36/36, `node --test` 8/8.

## Qué sigue — Fase 2 (gap analysis §7)
- Rieles reales: adaptador del procesador elegido (el flujo backend ya existe sobre el puerto) + su SDK en la app, STP real (abonos, `registraOrden`, consulta de estado de órdenes para el reconciliador de payouts, CEP, devoluciones).
- Payout automático diario y conciliación STP/banco vs. ledger (la del procesador ya existe).
- Publicador real del outbox; proveedor OTP real; KMS real para la bóveda; verificación real de titularidad de la CLABE de liquidación.
- En paralelo (Fase 3): app Android real (AGP + Compose + SDK del procesador) y PAC real con CFDI validado.
