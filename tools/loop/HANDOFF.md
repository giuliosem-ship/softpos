# Engineering Loop Handoff Checkpoint
**Actualizado:** 2026-11-20 — **Fase 1 del MVP cerrada** (MVP-01..13, Lotes A–D)
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

## Qué sigue — Fase 2 (gap analysis §7)
- Rieles reales: procesador con Tap to Pay on Android (backend + webhooks firmados), STP real (abonos, `registraOrden`, consulta de estado de órdenes para el reconciliador de payouts, CEP, devoluciones).
- Payout automático diario y conciliador real (procesador y STP vs. ledger; `cmd/reconciler` sigue siendo un stub).
- Publicador real del outbox; proveedor OTP real; KMS real para la bóveda; verificación real de titularidad de la CLABE de liquidación.
- En paralelo (Fase 3): app Android real (AGP + Compose + SDK del procesador) y PAC real con CFDI validado.
