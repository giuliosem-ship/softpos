# Engineering Loop Handoff Checkpoint
**Actualizado:** 2026-11-21 — Fase 1 cerrada (MVP-01..13, Lotes A–D); Fase 2 en curso: Lotes E (procesador, MVP-14..16), F (app Android, AND-30..32) y G (KYB + backoffice + ventas, MVP-17..19) aplicados y auditados; Lote K (MVP-23..26, CFDI posterior a la venta y comprobante) aplicado, auditoría pendiente
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

## Fase 2 — Lote H (app Android, AND-33..35)
- Contrato real de cobro con tarjeta (`/api/v1/card-payments/*`), devoluciones, selector de comercio y onboarding KYB en la app; auditoría `[AND-33-audit]`/`[AND-35-audit]` (llave de idempotencia en 5xx de resultado desconocido, polling acotado por tiempo, CI falla si el APK release conserva el simulador). Detalle en `docs/ANDROID_APP.md`.

## Fase 2/3 — Lote I (69-B y CFDI 4.0, MVP-20..22)
- MVP-20: ingesta real de la lista 69-B del SAT (parser Latin-1, staging + swap atómico, migración 015, protección de encogimiento 10 % con override auditado, lista sin verificar > `SAT_69B_MAX_AGE` → `UNKNOWN`, worker `SAT_69B_URL`, backoffice y `operatorctl load-69b`, re-revisión y bloqueo de comercios aprobados listados; `docs/KYB.md`).
- MVP-21: cadena original validada contra el XSLT oficial (golden tests con `xsltproc`), sello sobre la cadena, carga de CSD (`.key` PKCS#8 cifrado), catálogos SAT embebidos, ajustes fiscales del comercio (migración 016).
- MVP-22: facturas desde ventas (montos del split posteado), timbrado por el puerto PAC, cancelación 01–04 con *polling* de aceptación, PDF con QR en Go puro, `Mailer`, factura global automática, reintentos en backoffice (migración 017; `docs/CFDI.md`).
- Auditoría (`[MVP-22-audit]`): la consulta de estado de cancelaciones no mandaba el RFC del emisor (con un PAC real ninguna cancelación pendiente se resolvía; el mock SAT ahora exige la expresión re/rr/tt/id); una venta con devolución parcial podía quedar con IVA un centavo fuera de los límites del SAT (rechazo del PAC). Pendientes nuevos en el gap analysis: notas de crédito, zona horaria de `Fecha` por CP, robustez de la global (> 5,000 ventas, ventanas), formato `tt` del QR, bloqueo 69-B sin notificación/desbloqueo, sobre del CSD sin AAD; decisiones fiscales a confirmar por el contador listadas en F7.

## Fase 3 — Lote K (CFDI posterior a la venta y comprobante, MVP-23..26)
- MVP-23: notas de crédito automáticas: una devolución (`payment.refunded`) o contracargo (`chargeback.opened`) encola un `credit_note_jobs` en la misma transacción del ledger (migración 018); el worker (`CFDI_CREDIT_NOTE_INTERVAL`) emite un CFDI de egreso (tipo `E`, relación 01, `84111506`/`ACT`, IVA/ISR proporcional half-up, remanente exacto) por el puerto PAC solo si la factura activa cubre más que lo cobrado; `PENDING` lo reintenta el worker, `FAILED` el backoffice; `chargeback.reversed` no re-factura: se marca para el operador (`GET /api/v1/backoffice/credit-notes?flagged=true`).
- MVP-24: factura global en partes (`global_part`, una viva por periodo y parte), reconciliación de todas las ventas sin factura hasta `CFDI_GLOBAL_MAX_AGE` (90 días; ventas tardías y liberadas por motivo 04 entran en una parte nueva), `ALERT` para las más viejas; `Fecha` en la zona horaria del CP de `LugarExpedicion` (tabla de rangos, límites en `docs/CFDI.md`).
- MVP-25: `GET …/sales` y el nuevo `GET …/sales/{sale_id}` exponen `invoice_id`/`invoice_kind`/`invoice_status` (null si no hay) y notas de crédito; las membresías del login/refresh traen `merchant_name`.
- MVP-26: comprobante de venta público `/r/{token}` (token HMAC con expiración, sin datos del tenant, `RECEIPT_TOKEN_SECRET` en el guard de producción), PAN enmascarado/marca/autorización desde el webhook (migración 019), autofactura del cliente final por el servicio de facturación (ventana del mes de la venta o deshabilitada), envío por `Mailer` y el puerto `SMSSender` (log).
- Validación local: `go vet ./...` limpio; `go test -race -count=1 ./...` 21/21 paquetes OK; `TEST_DATABASE_URL=… go test -race -count=1 -run PGIntegration ./...` contra PostgreSQL 16 (`embedded-postgres`) 43/43; golden tests `xsltproc` 14 variantes + egresos construidos; `node --test web/pos/softpos_sdk.test.js` 8/8. Tests Kotlin no ejecutados (sin cambios en Android).
- Auditoría del Lote K: doble crédito tras sustitución motivo 01 (cancelar con notas de crédito vivas ahora es `409 INVOICE_HAS_CREDIT_NOTES`), egreso `FAILED` timbrado contra factura cancelada, carrera con `chargeback.reversed`, devolución entre la lectura y la creación de la global, centavo de ISR en créditos parciales, token del comprobante en logs y formulario público sin límite de tamaño.
- Decisiones a confirmar por el contador (F7 del gap analysis): egreso vs. cancelación con sustitución, 84111506/ACT y `InformacionGlobal` en el egreso de una global, `UsoCFDI` G02, ventana de autofactura = mes calendario, globales tardías hasta 90 días.

## Fase 3 — Lote L (operación y cumplimiento, MVP-27..31)
- MVP-27: ciclo del bloqueo 69-B (`blocked_source`, eventos `merchant.blocked_69b`/`unblocked_69b` en la misma transacción, desbloqueo automático solo de bloqueos `SAT_69B`, excepciones documentadas `sat69b_exceptions`, guarda de cambios `REJECTED_CHANGES`); migración 020.
- MVP-28: `operator_audit_log` append-only por triggers y expediente KYB `ON DELETE RESTRICT` (migración 021).
- MVP-29: barrido de devoluciones `REQUESTED` (`Provider.GetRefund`, `SKIP LOCKED`, resolución por el camino del webhook, expiración que libera la retención).
- MVP-30: métricas Prometheus en `METRICS_ADDR` con `METRICS_TOKEN`, `docs/OBSERVABILITY.md` y `deploy/prometheus/kerno-alerts.yml`.
- MVP-31: `iss`/`aud` del JWT, tope diario de OTP por número entre instancias y allowlist CORS.
- Auditoría: la guarda de cambios 69-B no contaba RFCs reemplazados (un archivo del mismo tamaño habría desbloqueado a todos), un operador no podía bloquear a un comercio ya bloqueado por 69-B, y el barrido podía expirar una devolución que el comercio reenviaba.

## Siguiente lote — Lote M, app Android (AND-39..41) — punto de arranque para un contexto nuevo
Estado al 2026-11-21: `origin/main` = `eff6847` (Lotes A–L publicados y auditados). Backend listo para este lote; solo falta la app (`mobile/android`, paquete `mx.softpos.app`, applicationId `mx.kerno.softpos`).

- **AND-39 Comprobante de venta:** en el detalle de venta usar `GET /api/v1/merchants/{id}/sales/{sale_id}` (`receipt_url`, `receipt_expires_at`, `card` con marca/`masked_pan`/`auth_code`) para compartir el enlace (share sheet de Android) y enviarlo con `POST /api/v1/merchants/{id}/sales/{sale_id}/receipt` `{"email"?,"phone"? (E.164)}` (owner/cashier). Contrato: `docs/CFDI.md` (§ comprobante/autofactura, ~l. 380–440) y `docs/MONEY_FLOWS.md` (~l. 267). Nunca mostrar/guardar más que los últimos 4 dígitos.
- **AND-40 Ventas facturadas:** la lista y el detalle de ventas traen `invoice_id`, `invoice_kind` (`INDIVIDUAL`/`GLOBAL`/null), `invoice_status` y `credit_notes`. Marcar las ventas facturadas o incluidas en una global, deshabilitar de antemano "Facturar" en esas (hoy se descubre con `409 SALE_ALREADY_INVOICED`) y mostrar las notas de crédito. Cancelar una factura con notas de crédito vivas ahora responde `409 INVOICE_HAS_CREDIT_NOTES`: mapear ese código.
- **AND-41 Nombre del comercio:** el login/refresh devuelve `memberships[].merchant_name` (`docs/AUTH.md` ~l. 84); usarlo en el selector de comercio (fallback al id si viene vacío). Desde el Lote L los access tokens llevan `aud`: un 401 tras el deploy se recupera con el refresh existente (verificar que la app lo haga).
- Al terminar, quitar del gap analysis la fila F9 "Ventas incluidas en factura global sin marca en la app" y actualizar `docs/ANDROID_APP.md`.

**Proceso (igual que Lotes F/H/J):** worktree propio (`git worktree add -b feat/android-m ../SoftPOS-wt-android-m main`), subagente implementador (spec → RED → GREEN) y luego un auditor independiente que corrige cada defecto con prueba que falla primero. Fixtures de contrato capturados del backend real (ver `docs/ANDROID_APP.md` § Contract tests), no escritos a mano. Commits en inglés `feat(android): ... [AND-39]` con `Co-Authored-By`. Validación: `cd mobile/android && JAVA_HOME=/opt/homebrew/opt/openjdk@17 ./gradlew :app:testDebugUnitTest :app:lintDebug :app:assembleDebug :app:assembleRelease` (el release no debe contener strings dev/mock) + `go build ./...` del backend. Merge a `main`, poda del gap analysis en el mismo lote y push por SHA (`git push origin <sha>:main` tras `git merge-base --is-ancestor origin/main <sha>`).

## Pendientes sin terceros (después del Lote M)
- Devoluciones `PENDING` sin webhook final: extender el barrido de MVP-29 (fila F10).
- Re-postear el apartado ISR del split cuando la factura sale a persona moral (fila F8 "Retención en el split").
- Exportación de `operator_audit_log` a almacenamiento WORM y roles separados de migración/app (F11).
- Worker de notas de crédito sin leader lock (seguro solo por llaves de idempotencia); índice de ventas sin factura para el worker de la global.
- Ventas en contracargo revertido nunca se facturan; hold para ventas liberadas por motivo 04 que se quieran facturar nominativas (decisión del contador).
- `gofmt` de archivos previos (incl. `pkg/api/server.go`), sobre al CSD con `merchant_id` como AAD, test con latencia de 300 ms posiblemente flaky (`TestSTPAbonoWebhook_SuccessAndIdempotency`).

## Pendientes que dependen del usuario o de terceros
Procesador + SDK Tap to Pay, STP real, PAC real, proveedor KYC, SMS/WhatsApp/email, KMS, revisión del contador (lista de decisiones en F7 del gap analysis), asesoría legal (figura, PLD, datos personales, custodia de fondos), infraestructura/CD y rotar los secretos que siguen en el historial de git (`docker-compose.yml` anterior al Lote D).

## Qué sigue — Fase 2 (gap analysis §7)
- Rieles reales: adaptador del procesador elegido (el flujo backend ya existe sobre el puerto) + su SDK en la app, STP real (abonos, `registraOrden`, consulta de estado de órdenes para el reconciliador de payouts, CEP, devoluciones).
- Payout automático diario y conciliación STP/banco vs. ledger (la del procesador ya existe).
- Publicador real del outbox; proveedor OTP real; KMS real para la bóveda; verificación real de titularidad de la CLABE de liquidación.
- En paralelo (Fase 3): SDK del procesador en la app y adaptador del PAC real (el ciclo CFDI ya existe sobre `pac.PACClient`) + validación fiscal por contador.
