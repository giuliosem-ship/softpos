# Kerno (SoftPOS) — Estado actual y análisis de gaps para MVP

**Fecha de revisión:** 2026-11-19
**Commit revisado:** `4dacefd` (`main`, sincronizado con `origin/main`, 67 commits desde 2026-09-15)
**Última actualización:** 2026-11-20 — Lote G (MVP-17..19) aplicado y auditado
**Alcance:** backend Go (`cmd/`, `pkg/`, `migrations/`), app Android (`mobile/android`), Web POS (`web/pos`), docs y orquestador (`tools/loop`).

---

## 1. Resumen ejecutivo

Kerno tiene una **base de dominio amplia y bien testeada** (61/61 tareas del backlog cerradas, ~10k LOC Go de producción + ~5.6k LOC de tests, cobertura 60–89 % por paquete, todas las suites en verde). Lo que **no** tiene todavía es un producto que pueda cobrar dinero real de punta a punta: los módulos existen como librerías aisladas, pero la integración, la persistencia, la seguridad perimetral, la app móvil y los conectores con terceros reales (adquirente, PAC, STP) están incompletos o simulados.

| Dimensión | Estado | Comentario |
|---|---|---|
| Lógica de dominio (ledger, RESICO, CLABE, CFDI XML, ISO 8583, DUKPT, STEC, MIF, disputas) | 🟢 Avanzado | Librerías puras con tests unitarios sólidos. |
| Integración en el servidor HTTP | 🟡 Parcial | Solo ledger, pagos EMV/CoDi, CFDI, STP, RBF están ruteados. ISO 8583, HSM/DUKPT, clearing y disputas **no están conectados** a ninguna ruta. |
| Persistencia | 🟡 Parcial | Lote A: driver `pgx`, migraciones versionadas embebidas y comercios, pagos EMV, CoDi, CSD, acumulado mensual, ledger e idempotencia en PostgreSQL con `DATABASE_URL` (probado contra Postgres 16 real). Lote C: pago + asiento + acumulado RESICO + evento de outbox en una sola transacción (`store.MoneyStore`, ver `docs/MONEY_FLOWS.md`). Lote D: payouts persistidos (`payouts`, migración 009) y outbox con leases por dueño. Falta: publicador real del outbox y RBF en DB. Sin DB todo sigue en memoria (modo dev). |
| Seguridad perimetral | 🟡 Parcial | Lote B: login OTP, JWT montado en todas las rutas `/api/v1/*` salvo una lista pública explícita, tenant tomado del token (anti-IDOR), webhook CoDi firmado y secretos de dev solo con `DEV_MODE` (ver `docs/AUTH.md`). Lote C: montos de `settle` y CLABE de dispersión ya no vienen del cliente; llave de la bóveda desde el entorno. Lote G: alta con KYB (`docs/KYB.md`) — ningún comercio mueve dinero sin aprobación de un operador —, bloqueo de comercios, operadores de plataforma con bitácora de auditoría (`docs/BACKOFFICE.md`), `is_persona_moral` ya no viene del cliente y ventana de gracia del refresh token. Falta: proveedor OTP real, CORS/TLS/gateway, KMS real y verificación real de titularidad de la CLABE de liquidación. |
| Cobro con tarjeta real | 🔴 Bloqueante | Lote E (camino A): puerto `acquirer.Provider` con adaptador mock, modelo persistente `card_payments`, intents para el SDK Tap to Pay, webhooks firmados del procesador (captura → split en ledger, devoluciones, contracargos) y conciliación de sus liquidaciones; los endpoints EMV que "aprueban" al parsear TLV quedan solo con `DEV_MODE`. Falta elegir y contratar el procesador, su adaptador real y su SDK en la app. |
| App Android | 🟡 Parcial | Lote F (AND-30..32): módulo Android real (AGP, Compose Material 3, `MainActivity`, genera APK debug), login OTP con sesión cifrada bajo Android Keystore y pantallas de saldo/apartado, cobrar (teclado → tap mock / QR SPEI), ventas y perfil/CLABE (ver `docs/ANDROID_APP.md`). Falta: pasar del contrato de tarjeta provisional y del endpoint EMV legacy a `/api/v1/card-payments/*`, el SDK del procesador, Play Integrity real, pantallas de onboarding KYB (el backend y su contrato existen desde el Lote G)/devoluciones/facturar y la distribución en Play. |
| Integraciones externas | 🔴 Bloqueante | Procesador de tarjetas: solo `acquirer.MockProvider`. PAC: solo `MockPACClient`. STP/CEP: existe cliente HTTP, pero el servidor siempre usa el mock. |
| Operación (CI/CD, observabilidad, conciliación) | 🟡 Parcial | Lote D: CI en GitHub Actions (Go vet + race, integración Postgres 16, Web POS, Kotlin, imagen Docker), toolchain Go 1.27.1 fijado, secretos fuera del repo y reconciliador de payouts activo en `cmd/server`. Lote E: conciliación de liquidaciones del procesador (reporte de diferencias para operador, alertas `ERROR`, worker en `cmd/server` y `cmd/reconciler` de una pasada). Lote G: backoffice interno mínimo (revisión KYB, bloqueo, búsqueda de pagos, ledger por comercio, reintento de payouts, conciliación del procesador, bitácora; UI en `/backoffice/`). Falta: despliegue (CD) e infraestructura, conciliación contra el estado de cuenta STP/bancario y métricas/alertas enrutadas. |

**Conclusión:** el proyecto está en etapa de *"motor de dominio prototipado"*, no de MVP. Estimo que lo construido cubre ~25–30 % del trabajo de un MVP que cobre dinero real, y gran parte del código más sofisticado (switch ISO 8583, DUKPT, STEC, MIF) **no es necesario para el MVP** si se cobra con tarjeta a través de un proveedor certificado.

---

## 2. Validación ejecutada en esta revisión

| Verificación | Resultado |
|---|---|
| `go test -race -count=1 ./...` (Go 1.24 local, directiva bajada temporalmente desde 1.27.1 en una copia) | ✅ 14/14 paquetes OK, sin data races |
| Cobertura (`go test -cover`) | api 62 %, stp 61 %, payments 67 %, rbf 71 %, fiscal 75 %, pac 79 %, store 79 %, ledger 80 %, identity 83 %, disputes 82 %, iso8583 84 %, clearing 88 %, crypto 89 %, cmd/server 60 % |
| `node --test web/pos/softpos_sdk.test.js` | ✅ 7/7 |
| Tests Android | ✅ (Lote F + auditoría del Lote E, 2026-11-20) 123/123 con `./gradlew :app:testDebugUnitTest :app:assembleDebug` (Gradle 8.14.3, AGP, JDK 17); genera `app-debug.apk`. |
| CI (`.github/workflows/ci.yml`) | Workflow creado en el Lote D y validado sintácticamente (YAML); su primera corrida en GitHub y el `docker build` no se ejecutaron localmente (sin runner ni Docker). |
| Arranque con `DATABASE_URL` | ✅ (Lote A, 2026-11-20; migraciones 005–008 en Lotes B y C, 009 en el Lote D) arranca, aplica migraciones 001–004 y conserva el estado tras reiniciar contra PostgreSQL 16 real (`embedded-postgres`, `TEST_DATABASE_URL=… go test -run PGIntegration ./...`). Si la DB no responde o una migración falla, aborta con error explícito. `docker compose up` no se re-ejecutó (sin Docker en el entorno). |
| `POST /api/v1/disbursements` y `GET /api/v1/merchants/{id}/balances` sin token | ✅ (Lote B, 2026-11-20) `401` sin token y `403` con token de otro comercio; una dispersión exige rol `owner`. |

---

## 3. Inventario de lo construido

**Backend Go (`pkg/`)**
- `ledger`: partida doble con `int64` centavos, splits (comisión, apartado fiscal, barrido RBF); repositorio SQL con `SELECT … FOR UPDATE`.
- `fiscal`: RESICO PF (tramos ISR 1.0–2.5 %, IVA 16 %, retención PM), XML CFDI 4.0, cadena original, sello, factura global diaria.
- `pac`: interfaz + orquestador multi-PAC con failover — **solo implementación mock**.
- `stp`: generador/validador CLABE, firma canónica RSA/HMAC, cliente `registraOrden` HTTP, parser CEP Banxico, conciliador.
- `payments`: parser TLV EMV, motor de estados (authorize/settle/void/refund), CoDi QR (formato JSON propio).
- `identity`: RFC/CURP, parser CSF, lista 69-B + worker de ingesta, bóveda CSD AES-GCM envolvente, verificador Play Integrity.
- `iso8583`, `crypto` (DUKPT, traducción PIN, HSM simulado), `clearing` (STEC, MIF), `disputes` (contracargos, reserva dinámica), `rbf`: implementados y testeados, **no integrados**.
- `store`: driver `pgx`, repos SQL (ledger, comercios, RBF, pagos EMV, CoDi, outbox, idempotencia) con implementaciones en memoria equivalentes para modo sin DB, runner de migraciones versionadas (`schema_migrations`, embebidas en el binario), tests de integración contra Postgres real (`pkg/store/pgtest`), cliente Redis RESP propio.
- `outbox`: dispatcher con `FOR UPDATE SKIP LOCKED`, backoff exponencial, lease con dueño (reclamo = intento, resultado solo del dueño), timeout por publicación y apagado ordenado; arranca desde `cmd/server`.
- `payouts`: asientos de reserva/compensación de dispersiones y reconciliador de recuperación contra el estado de la orden en STP (arranca desde `cmd/server`); vigilante de liquidaciones del procesador (`AcquirerSettlementWatcher`, también en `cmd/reconciler`).
- `acquirer` (Lote E): puerto agnóstico `Provider` hacia el procesador certificado (intent, estado, captura, devolución, cancelación con llaves de idempotencia; eventos de webhook normalizados) y `MockProvider` determinista; ver `docs/ACQUIRER_INTEGRATION.md`.
- `api`: rutas REST + servidor estático del Web POS; idempotencia con clave por tenant; middleware JWT montado con lista pública, rate limit por IP en `/auth/*`, webhook CoDi con firma HMAC.
- `auth`: usuarios por teléfono, membresías usuario↔comercio con rol, OTP (hash, expiración, intentos, reenvío), access JWT HS256 y refresh opaco rotativo con detección de reuso; repos SQL y en memoria.

**Android (`mobile/android`)** — 25 clases Kotlin con lógica pura y descriptores de UI (calculadora de splits, CVM, teclado aleatorio, ISO 9564, TLV, orquestador de tap, cliente API, SAF cifrada, guard de ventana). Ninguna depende del SDK de Android; los puntos de contacto con el SO son interfaces (`IsoDepTransceiver`, `SecureWindow`, `AccessibilityScanner`).

**Web POS (`web/pos`)** — demo funcional con simulación de tap, CoDi y desglose fiscal, más páginas de narrativa (overview, economics, roadmap).

**Tooling** — CI en GitHub Actions; loop por lotes con Claude Code (implementador + auditor independiente, RED→GREEN→AUDIT); el orquestador multi-modelo de LM Studio queda como legacy opcional.

---

## 4. Definición del MVP

**Propuesta de valor mínima:** *un comercio PyME (inicialmente RESICO persona física) se da de alta desde su Android, cobra con tarjeta contactless en su teléfono y por transferencia SPEI, ve cuánto es suyo y cuánto es de impuestos, recibe su dinero en su cuenta y puede emitir CFDI 4.0 válido.*

### Decisión estratégica previa (condiciona todo el MVP)

Para cobrar con tarjeta hay dos caminos:

- **A. Proveedor certificado (recomendado para MVP):** integrar el SDK de un procesador que ya tenga Tap to Pay on Android en México. Adyen lo habilitó en México en noviembre de 2025; Clip y Mercado Pago también operan Tap to Pay en Android en el país. El proveedor asume kernel EMV L2, certificación PCI MPoC, conexión a PROSA/E-Global, liquidación con marcas y contracargos. Kerno opera como plataforma/agregador sobre ese adquirente.
- **B. Stack propio:** requiere kernel EMV L2 certificado, listado PCI MPoC por un laboratorio reconocido (servicio de atestación y monitoreo incluido), adquirente patrocinador, conexión ISO 8583 al switch, HSM físico/cloud, y certificación de marcas. Son de 12 a 24 meses y presupuesto de certificación considerable.

Este análisis **asume el camino A**. Con eso, `iso8583`, `crypto` (DUKPT/PIN/HSM), `clearing`, el teclado PIN propio, `IsoDepEmvReader` y la SAF offline salen del MVP y quedan como activos para una fase 2 (adquirencia propia).

### Funcionalidades 100 % requeridas para el MVP

| # | Funcionalidad | ¿Por qué es imprescindible? |
|---|---|---|
| F1 | Alta y KYB/KYC del comercio | Sin alta verificada no hay contrato con el adquirente ni obligaciones PLD cumplidas. |
| F2 | Autenticación, sesiones y aislamiento multi-tenant | Sin esto cualquiera puede mover el dinero de cualquier comercio. |
| F3 | Cobro con tarjeta contactless (Tap to Phone) | Es el producto. |
| F4 | Cobro por transferencia SPEI (CLABE/QR) | Segundo riel, barato, y es donde ya hay más código útil (STP). |
| F5 | Ledger persistente, saldos y conciliación | Fuente de verdad del dinero; requisito para liquidar sin errores. |
| F6 | Liquidación (payout) al comercio vía SPEI | El comercio tiene que recibir su dinero. |
| F7 | Apartado fiscal RESICO/IVA | Diferenciador central del producto. |
| F8 | Facturación CFDI 4.0 (individual y global) con PAC real | Diferenciador central + obligación fiscal. |
| F9 | App Android del comercio (UI real, distribuible) | Canal único del MVP (iOS congelado). |
| F10 | Devoluciones, cancelaciones y comprobante de venta | Operación mínima esperada por comercio y marcas. |
| F11 | Plataforma y operación (deploy, CI, secretos, observabilidad, backoffice) | Sin esto no se puede operar dinero de terceros con responsabilidad. |
| F12 | Cumplimiento legal/regulatorio base | Prerrequisito para firmar con adquirente, STP y PAC. |

**Fuera del MVP (fase 2+):** RBF/financiamiento, switch ISO 8583 propio, DUKPT/HSM/traducción de PIN, lotes STEC, calculadora MIF, motor de disputas propio (el adquirente gestiona contracargos; el MVP solo necesita reflejarlos en el ledger), SAF offline, iOS, worker automático de 69-B (en MVP basta consulta en el alta), páginas de economics/roadmap.

---

## 5. Gaps por funcionalidad

Severidad: **P0** = bloquea MVP · **P1** = necesario antes de clientes reales · **P2** = deseable en MVP.
Esfuerzo: **S** ≤ 1 semana · **M** 1–3 semanas · **L** > 3 semanas (1 dev).

### F1 — Alta y KYB/KYC del comercio
**Existe:** validador RFC/CURP, parser CSF y QR de constancia, lista 69-B con repo SQL, validador de CSD `.cer/.key` (Android), `POST /api/v1/merchants` (valida el formato del RFC y deja el alta en `DRAFT`). Desde el Lote G (ver `docs/KYB.md`): flujo `DRAFT → SUBMITTED → IN_REVIEW → APPROVED/REJECTED` con motivo (`merchant_kyb`, migración 012), captura de datos (razón social, RFC, CURP, régimen, MCC, domicilio, contacto) y documentos INE/selfie/comprobante de domicilio/estado de cuenta/CSF cifrados con la bóveda (`kyb_documents`, blobs detrás de `store.DocumentBlobStore`), validaciones al enviar (dígito verificador del RFC, persona física por longitud del RFC, solo RESICO PF, CURP ↔ RFC, documentos completos, 69-B → rechazo automático), puerto `kyb.KYCProvider` (prueba de vida/INE) con mock, revisión manual en el backoffice; al aprobar se asigna la CLABE virtual y se da de alta el sub-afiliado vía `acquirer.Provider.OnboardMerchant` (mock). Sin aprobación (o con el comercio bloqueado) no hay intents de tarjeta, QR CoDi/SPEI, dispersiones ni originación RBF. Auditoría del Lote G: la originación RBF quedó tras la aprobación y solo para `operator` (el dueño podía acreditarse un préstamo sin comisión), la CLABE virtual se asigna una sola vez aunque dos aprobaciones compitan, nunca se aprueba un RFC 69-B `LISTED`, un operador no aprueba ni desbloquea un comercio del que es miembro y el 69-B queda `UNKNOWN` (no `CLEAR`) sin lista cargada.

| Gap | Sev. | Esf. | Detalle / evidencia |
|---|---|---|---|
| Proveedor real de verificación de identidad | P0 | M | Solo existe `kyb.MockKYCProvider` (`KYC_PROVIDER=mock`, rechazado en producción; sin proveedor la identidad queda `UNKNOWN` y la revisa el operador). Falta contratar el proveedor (Truora, Metamap, …), su adaptador de `kyb.KYCProvider` (prueba de vida, INE contra lista nominal, coincidencia facial) y los requisitos documentales que pida el adquirente. |
| Lista 69-B sin ingesta | P0 | S | `identity.SATIngestionWorker` existe pero `cmd/server` no lo invoca y `sat_blacklist` arranca vacía: desde la auditoría del Lote G el envío del KYB deja `list_69b = UNKNOWN` y el operador revisa el RFC a mano. Falta descargar/cargar el CSV del SAT (job programado o `cmd`), registrar fecha de la última carga (hoy no se detecta una lista vieja) y re-revisar comercios aprobados (`RetroscreenMerchants`). |
| Documento subido durante el envío | P1 | S | La subida verifica que el KYB sea editable y luego guarda el documento sin atarlo a la versión del registro: una subida concurrente con `submit` puede reemplazar (supersede) un INE/selfie después de que el proveedor KYC verificó el anterior, y el operador vería otro archivo que el verificado. Guardar los ids de documento verificados en el registro (o ligar la subida al `version` con compare-and-set). |
| Alta real en el adquirente | P0 | M | El puerto `acquirer.Provider.OnboardMerchant` (llave `kerno_onboard_<id>`) se llama al aprobar el KYB, pero solo con el mock; falta la API de onboarding de sub-afiliados del procesador elegido (ver F3). |
| Expediente KYC: retención y representante legal | P1 | S | Falta política de conservación/borrado de expedientes (PLD, LFPDPPP), almacenamiento de objetos (S3) para los blobs y, cuando se soporten personas morales, representante legal y beneficiario controlador. |
| Registro de la CLABE virtual en STP | P1 | S | Desde el Lote G la CLABE virtual se asigna al aprobar el alta (aleatoria `646180…`, el cliente nunca la elige). Falta usar el rango/prefijo que asigne STP al participante y registrarla en STP. |

### F2 — Autenticación, sesiones y multi-tenant
**Existe (Lote B, ver `docs/AUTH.md`):** usuarios y membresías con rol, login OTP (`/api/v1/auth/otp/*`), access JWT HS256 de 15 min + refresh opaco rotativo con detección de reuso y logout, JWT montado en todas las rutas salvo lista pública, tenant derivado del token (body/path ajeno → 403, recursos ajenos → 404, idempotencia por tenant), dispersiones solo `owner`, alta de comercio con id generado por el servidor, nonce de atestación de un solo uso + verificación Play Integrity, rate limit por IP en `/auth/*`, webhook CoDi firmado (HMAC + timestamp + anti-replay), config guard que exige `JWT_SECRET`/`CODI_WEBHOOK_SECRET` y rechaza `DEV_MODE` en producción.

| Gap | Sev. | Esf. | Detalle / evidencia |
|---|---|---|---|
| Proveedor OTP real | P0 | S | Solo existe `auth.LogSender` (no entrega el código fuera de `DEV_MODE`). Implementar `auth.OTPSender` con SMS/WhatsApp y límites por número/día. |
| Bóveda CSD sin KMS real | P1 | S | Desde el Lote C la llave sale de `VAULT_MASTER_KEY_HEX` vía `identity.KeyProvider` (sin llave fuera de `DEV_MODE` no arranca) y cada sobre guarda su id de llave. Falta cliente KMS real detrás de `identity.KMSClient` y un job que re-cifre (`RewrapCSD`) los CSD al rotar. |
| Atestación no exigida | P1 | S | El endpoint de nonce y la verificación existen, pero el veredicto no se exige ni se liga a la sesión (login o cobro). |
| CORS / TLS / rate limit distribuido | P1 | S | El rate limit actual es en memoria por proceso y usa la IP TCP; CORS y TLS ausentes. Normalmente en API gateway. |
| Gestión de miembros y permisos | P2 | S | Roles `owner`/`cashier`/`operator` en el modelo y operadores de plataforma (backoffice, Lote G), pero no hay endpoints para invitar cajeros ni permisos finos por operador ni 2FA de operadores. Los access tokens no se pueden revocar antes de expirar. |

### F3 — Cobro con tarjeta contactless (Tap to Phone)
**Existe:** parser TLV, motor de estados de pago, reglas CVM, orquestador de tap con auto-reversa, adaptador multi-riel (`WhiteLabelTapProvider`), simulador en Web POS. Desde el Lote E (ver `docs/ACQUIRER_INTEGRATION.md`): puerto `acquirer.Provider` + mock (aprobado / declinado `…51` / timeout `…52`), tabla `card_payments` con máquina de estados, `provider_payment_id`, llave de idempotencia del intent, comisión y fecha de liquidación del procesador; `POST /api/v1/card-payments/intents` (devuelve `client_secret` para el SDK) y `GET /api/v1/card-payments/{id}`; el estado solo cambia con webhooks firmados `POST /api/v1/webhooks/acquirer/{provider}` (HMAC + ventana, exactly-once por id de evento, tolerancia a desorden), la captura postea el split existente (tarifa, apartado fiscal, RESICO) + comisión del procesador en una transacción con outbox; conciliación de payouts del procesador (`ASSET:ACQUIRER_RECEIVABLE` → `ASSET:BANK_OPERATING`, faltantes / diferencias de monto o comisión / duplicados / no liquidados) con reporte para `operator`. Los endpoints EMV legacy (`authorize`/`settle`/`reverse`) responden `410` salvo con `DEV_MODE`.

| Gap | Sev. | Esf. | Detalle / evidencia |
|---|---|---|---|
| Sin proveedor de adquirencia integrado | P0 | L | Elegir y contratar procesador con Tap to Pay on Android en MX; implementar su adaptador real de `acquirer.Provider` (API, formato y firma de sus webhooks, reporte de liquidaciones → `payout.settled`; checklist en `docs/ACQUIRER_INTEGRATION.md` §6) e integrar su SDK en la app. El resto del flujo backend ya existe con el mock (Lote E). |
| `IsoDepEmvReader` no es un kernel EMV | — (fase 2) | — | Solo envía SELECT PPSE y espera PAN y 9F26 en esa respuesta; no hace SELECT AID / GPO / READ RECORD / GENERATE AC, y marca `Approved` localmente. Útil solo como referencia. |
| Web NFC no sirve para tarjetas | P1 | S | El SDK web usa `NDEFReader`, que lee etiquetas NDEF, no tarjetas EMV ISO-DEP. El Web POS debe quedar como demo o como canal solo-SPEI; documentarlo. |

### F4 — Cobro por transferencia SPEI (CLABE / QR)
**Existe:** CLABE virtual, webhook `stp/abono` con verificación de firma, generador "CoDi QR", cliente HTTP STP.

| Gap | Sev. | Esf. | Detalle / evidencia |
|---|---|---|---|
| Contrato y ambiente STP reales | P0 | M | Cuenta concentradora, llaves, certificados, IP allowlist, ambiente de pruebas STP. |
| Formato "CoDi" no es el de Banxico | P1 | M | `codiWirePayload` es un JSON propio (`v, ic, c, m…`). Un cobro CoDi real se origina vía un participante del esquema. Para MVP lo más simple es **QR/enlace con CLABE + referencia** (SPEI estándar) y dejar CoDi/DiMo para después; validar con STP qué ofrece. |
| Webhook de abono → pago del comercio | P1 | S | Verificar que el abono identifique comercio + referencia de cobro, idempotencia por `claveRastreo`, y postee en ledger persistente. |
| Expiración y montos no coincidentes | P2 | S | Qué pasa si pagan de más/menos o después de expirar. |

### F5 — Ledger persistente, saldos y conciliación
**Existe:** motor de partida doble, repo SQL con bloqueo pesimista, driver `pgx` y migraciones versionadas 001–009 embebidas (`schema_migrations`), estado de comercios/pagos EMV/CoDi/CSD/acumulado mensual en PostgreSQL (repos en memoria solo sin `DATABASE_URL`), tests de integración contra Postgres real, outbox con dispatcher activo, idempotencia SQL/Redis; desde el Lote C, unidad de trabajo `store.MoneyStore` (pago + `payment_settlements` + asiento + acumulado + evento en una transacción, claves exactly-once en `money_operations`, errores de posteo → rollback y 5xx) y `settle` con monto del pago autorizado y comisión por tarifa del comercio (`platform_fee_bps`); desde el Lote D, mes del acumulado RESICO calculado en `America/Mexico_City` (tzdata embebido). (El estado RBF sigue en memoria, pero RBF queda fuera del MVP.)

| Gap | Sev. | Esf. | Detalle / evidencia |
|---|---|---|---|
| Publicador real del outbox | P1 | S | Desde el Lote C los flujos de dinero encolan `payment.*`, `codi.confirmed`, `spei.abono_received` y `disbursement.*` en la misma transacción, pero solo hay publicadores `log`/`noop`. Falta un publicador real (webhook/broker/PAC). |
| Conciliación diaria | P1 | M | Desde el Lote E existe la conciliación procesador ↔ ledger (payouts del procesador contra capturas, `NOT_SETTLED`, reporte `GET /api/v1/acquirer/reconciliation` para `operator`; `cmd/reconciler` ya no es un stub). Falta: ledger vs. estado de cuenta STP, depósito del procesador vs. estado de cuenta bancario real y asientos de corrección de las diferencias desde backoffice. |

### F6 — Liquidación (payout) al comercio
**Existe:** `POST /api/v1/disbursements` (solo `owner`, solo a la CLABE de liquidación registrada y `VERIFIED`; `Idempotency-Key` obligatoria por comercio —misma clave y cuerpo reproduce el resultado, otro cuerpo `409`—; fila en `payouts` + saldo y débito en una transacción con bloqueo; asiento compensatorio solo si STP rechaza la orden; si el resultado es desconocido —timeout, 5xx— los fondos quedan reservados, `202 PENDING_RECONCILIATION`), reconciliador de recuperación (Lote D: payouts `RESERVED`/`PENDING_RECONCILIATION` viejos se resuelven contra el estado de la orden en STP: liquidada → `SETTLED`, rechazada/inexistente → compensación única, desconocida → se conserva y alerta), alta de la CLABE de liquidación (`PUT /api/v1/merchants/{id}/settlement-account`) con revisión de operador, reconciliación CEP.

| Gap | Sev. | Esf. | Detalle / evidencia |
|---|---|---|---|
| Verificación real de titularidad de la CLABE | P0 | S | El verificador es un stub (`PendingAccountVerifier`): la cuenta queda `PENDING` hasta que un `operator` la marca. Falta consulta de titularidad (RFC del titular = RFC del comercio) vía STP/banco o proveedor KYB, 2FA al cambiarla y llevar esa revisión al backoffice de operadores de plataforma (hoy usa el rol `operator` por comercio; el estado de cuenta ya se captura en el KYB). |
| STP real no cableado | P0 | S | `STP_BASE_URL` se lee pero nunca se usa; el servidor siempre instancia `MockSTPClient` y `MockCEPProvider`. Falta también el cliente real de consulta de estado de orden (`stp.OrderStatusClient`) que usa el reconciliador de payouts. |
| Payout automático programado | P1 | M | Barrido diario/T+0 del saldo disponible (neto de apartado fiscal) en vez de dispersión manual por API. |
| Devoluciones SPEI después de liquidar | P1 | S | Las dispersiones con resultado desconocido o interrumpidas ya se resuelven (Lote D). Falta: webhook de estado de STP para órdenes devueltas **después** de liquidarse (revertir asiento y reintentar) y llevar `GET /disbursements/{clave}` y el seguimiento CEP a la tabla `payouts` (hoy siguen en memoria). |
| Robustez del reconciliador de payouts | P1 | S | Auditoría del Lote D: (1) lista los candidatos por `updated_at` más antiguo sin tocar los que siguen desconocidos, así que ≥ `BatchSize` (50) payouts atascados en estado desconocido bloquean la recuperación de los demás; (2) sin `SKIP LOCKED` en el listado, cada réplica consulta a STP los mismos payouts (la doble compensación sí está protegida: fila `FOR UPDATE` + estado + id de asiento determinista); (3) `fecha_operacion` se calcula con la fecha UTC (después de las 18:00 en CDMX sale con la fecha de mañana) y debe ser la fecha de operación SPEI en `America/Mexico_City` según el contrato STP; (4) con `MockSTPClient` el estado de órdenes vive en memoria del proceso: tras reiniciar, un payout `RESERVED` ya aceptado por el mock se compensa (solo dev). |
| Fondos de terceros | P1 | — | Definir con asesoría legal dónde y bajo qué figura se custodia el saldo del comercio entre cobro y payout. |

### F7 — Apartado fiscal RESICO / IVA
**Existe:** `CalculateResicoTaxSplit` con tramos y retención PM; split en ledger; presentador y exportador CSV en Android. Desde el Lote G solo se aprueban comercios RESICO persona física: otros regímenes y personas morales (derivadas de la longitud del RFC) se rechazan explícitamente al enviar el KYB, y la retención PM del split la decide el servidor (nunca el body).

| Gap | Sev. | Esf. | Detalle / evidencia |
|---|---|---|---|
| Validación fiscal por contador | P0 | S | Revisar con un contador: base de ISR RESICO (ingresos efectivamente cobrados), tratamiento de IVA trasladado vs. acreditable, retenciones de PM, cambio de tramo intra-mes, tope anual. Documentar supuestos. |
| ¿Qué pasa con el dinero apartado? | P0 | M | Hoy es una subcuenta contable. Definir si se retiene en cuenta segregada, cuándo se libera y cómo el comercio paga al SAT (¿Kerno solo aparta o también paga?). Implica F6 y temas legales. |
| Reporte mensual para declaración | P1 | S | Resumen de ingresos, IVA, ISR estimado por mes. El CSV de Android es local; debe salir del backend. |

### F8 — Facturación CFDI 4.0
**Existe:** builder XML, cadena original, sello RSA-SHA256, factura global diaria, orquestador multi-PAC con failover, bóveda CSD.

| Gap | Sev. | Esf. | Detalle / evidencia |
|---|---|---|---|
| Sin PAC real | P0 | M | Solo `MockPACClient`. Contratar PAC y construir cliente real (sandbox → prod), manejo de códigos de error SAT. |
| Cadena original manual | P0 | M | `BuildCadenaOriginal` arma el string a mano; el SAT la define vía XSLT oficial. Riesgo alto de sellos inválidos en casos borde. Validar contra el XSLT del SAT o usar el servicio del PAC; probar con el validador del SAT. |
| Carga real de CSD por el comercio | P0 | S | Backend para subir `.cer/.key` + contraseña, validar vigencia/RFC y guardarlo cifrado con llave de KMS (ver F2). Hoy solo se siembra un CSD autofirmado de demo. |
| Datos del receptor | P1 | S | Captura de RFC, nombre, régimen fiscal, CP y uso CFDI del cliente final; autofactura desde ticket (portal o QR). De ahí debe salir la retención ISR de persona moral (Art. 113-J) del split: desde el Lote G el servidor la decide y, sin receptor validado, trata toda venta como público en general. |
| Cancelación CFDI | P1 | M | No existe (motivos 01–04, folio de sustitución, aceptación del receptor). |
| Representación impresa (PDF) y envío | P1 | S | PDF con QR SAT y envío por email/WhatsApp. |
| Factura global automática | P1 | S | Generar periódicamente la global de ventas a público en general que no se facturaron individualmente. |
| Catálogos SAT | P2 | S | Clave de producto/unidad por comercio, actualizables. |

### F9 — App Android del comercio
**Existe:** lógica de presentación pura (ViewModel de terminal, presentadores de bóveda fiscal, onboarding y RBF; descriptores de pantallas y navegación), cliente API con idempotencia, generador QR. Desde el Lote F (AND-30..32, `docs/ANDROID_APP.md`): módulo `com.android.application` con Compose Material 3, DI manual y `gradlew` (Gradle 8.14.3); login teléfono → OTP contra `docs/AUTH.md` con refresco single-flight y logout; sesión solo como cifrado AES-256-GCM bajo llave de Android Keystore; pantallas Inicio (disponible vs. apartado fiscal), Cobrar (teclado en centavos enteros → tap con proveedor mock en debug / QR SPEI), Ventas (lista y detalle) y Perfil (CLABE, salir). En release el cobro con tarjeta queda deshabilitado hasta tener SDK del procesador.

| Gap | Sev. | Esf. | Detalle / evidencia |
|---|---|---|---|
| Sin UI real (pantallas restantes) | P0 | M | Desde el Lote F existen login/OTP, saldo/apartado, cobrar, ventas y perfil. Faltan: onboarding KYB (contrato backend listo en el Lote G, `docs/KYB.md`), devoluciones (sobre `POST /api/v1/card-payments/{id}/refunds`), facturar y selector de comercio cuando el usuario tiene varias membresías. |
| Contrato de cobro de la app desalineado con el backend | P0 | S | La app aún llama al endpoint legacy `/api/v1/payments/emv/authorize` (`SoftPosApiClient`, responde `410` sin `DEV_MODE`) y su `CardPaymentsApi` usa un contrato provisional (`id`, estados en minúsculas). Debe usar `/api/v1/card-payments/*` (campo `card_payment_id`, estados del backend) y consultar `GET /api/v1/card-payments/{id}` hasta `final: true`. (`GET /api/v1/merchants/{id}/sales` existe desde el Lote G.) |
| Integrar SDK del procesador | P0 | M | Tap to Pay del proveedor elegido (camino A), incluidos sus requisitos de dispositivo, atestación y permisos. |
| Play Integrity real | P1 | S | El cliente es lógica pura con interfaz; falta el SDK real de Google (el endpoint de nonce ya existe desde el Lote B). |
| Firma, distribución y updates | P1 | S | Keystore de firma, Play Console (track interno/cerrado), política de versión mínima forzada. Antes de subir a Play: `targetSdk` 36 (hoy 35) y regenerar `gradle-wrapper.jar` para Gradle 8.14.3 con `distributionSha256Sum` en `gradle-wrapper.properties`. |
| Telemetría y crash reporting | P1 | S | Crashlytics/Sentry sin datos sensibles. |

### F10 — Devoluciones, cancelaciones y comprobante
**Existe:** desde el Lote E, `POST /api/v1/card-payments/{id}/refunds` (solo `owner` salvo `ALLOW_CASHIER_REFUNDS`, `Idempotency-Key` obligatoria, monto retenido contra el saldo reembolsable): la solicitud va al procesador y el ledger solo se mueve con su webhook `payment.refunded` (reversión proporcional); contracargos `chargeback.opened`/`reversed` reflejados en el ledger (débito del disponible del comercio, que puede quedar negativo) con evento de outbox para notificarlo. Legacy (solo `DEV_MODE`): `POST /api/v1/payments/emv/reverse`: void y devolución total o parcial (acotada al saldo reembolsable, solo `owner` salvo `ALLOW_CASHIER_REFUNDS`) que revierte proporcionalmente disponible, apartado IVA/ISR, comisión y acumulado RESICO en una transacción (el barrido RBF no se revierte).

| Gap | Sev. | Esf. | Detalle / evidencia |
|---|---|---|---|
| Comprobante de venta | P1 | S | No existe ticket/recibo (solo en la demo web). Enviar por SMS/WhatsApp/email con PAN enmascarado, autorización y enlace de autofactura. |
| Solicitudes de devolución sin resultado | P2 | S | Desde la auditoría del Lote E un webhook de devolución se asocia por `kerno_refund_id` aunque la llamada al procesador haya vencido, pero si el procesador nunca recibió la solicitud ésta queda `REQUESTED` reteniendo su monto hasta que el comercio reintente con la misma `Idempotency-Key`; falta un barrido que la reconsulte/expire. Un contracargo no descuenta las devoluciones en curso (si ambas proceden, la devolución llega como `EVENT_CONFLICT` para revisión). |
| Notificación y defensa de contracargos | P1 | S | Desde el Lote E el contracargo se refleja en el ledger y encola `card_payment.chargeback_opened`; falta entregar el aviso al comercio (depende del publicador real del outbox, F5) y el flujo para enviar evidencia al procesador. |

### F11 — Plataforma y operación
| Gap | Sev. | Esf. | Detalle / evidencia |
|---|---|---|---|
| CD y protección de `main` | P1 | S | El CI existe desde el Lote D (`go vet`, `go test -race`, Postgres 16, Web POS, Kotlin, imagen Docker). Falta: despliegue continuo a staging/prod, publicar la imagen, lint/`gofmt` (hay archivos sin formato en paquetes de fase 2), APK firmado de release (el CI ya compila el APK debug desde el Lote F) y exigir CI verde en `main` (protección de rama). |
| Secretos en despliegue | P1 | S | Desde el Lote D no hay secretos en el repo (`.env` ignorado, `.env.example` con placeholders) y el guard de producción cubre cada secreto leído (test que lo amarra al código). Falta inyectarlos desde un Secrets Manager/KMS en el despliegue y definir su rotación. |
| Secretos del historial y semántica del outbox | P1 | S | Los valores que estuvieron en `docker-compose.yml` antes del Lote D (contraseña de Postgres, `STP_HMAC_SECRET`, `VAULT_MASTER_KEY_HEX`) siguen en el historial de git: no reutilizarlos en ningún entorno. Outbox: desde la auditoría del Lote D un lote se corta antes de que una publicación pueda exceder el lease; los eventos sobrantes se reclaman al vencer el lease y ese reclamo cuenta como intento aunque no se hayan publicado (falta liberarlos sin contar). |
| Infraestructura de despliegue | P0 | M | Postgres administrado con backups/PITR, Redis administrado, TLS, dominio, entornos dev/staging/prod. |
| Observabilidad | P1 | S | Logs estructurados existen (los payouts sin resolver ya emiten `ERROR` `ALERT payout unresolved`); faltan métricas, trazas y alertas enrutadas (fallos de webhook, descuadres de conciliación, payouts atascados, eventos `FAILED` del outbox). |
| Bitácora de operadores mutable | P1 | S | `operator_audit_log` es "append-only" solo por convención: la app no expone borrado, pero el rol de base de datos de la app puede `UPDATE`/`DELETE`. Revocar esos permisos al rol de la app (o trigger que lo impida) y exportar a almacenamiento inmutable. Además `merchant_kyb`/`kyb_documents` tienen `ON DELETE CASCADE` desde `merchants` (borrar un comercio borra su expediente). |
| Backoffice interno | P2 | S | Desde el Lote G existe el backoffice mínimo (`docs/BACKOFFICE.md`): revisión KYB con descarga auditada de documentos, aprobar/rechazar, bloquear, búsqueda de pagos, ledger por comercio, reintento de payouts, conciliación del procesador y bitácora `operator_audit_log`; UI estática en `/backoffice/`. Falta: permisos por operador, 2FA/IP allowlist, asientos de corrección y reintento de timbrados (sin PAC real). |
| Tests de integración E2E | P1 | M | Cobro → webhook → ledger → payout → CFDI contra sandboxes reales. Hoy todo es unitario con mocks. |

### F12 — Cumplimiento legal / regulatorio base (no-código)
| Gap | Sev. | Detalle |
|---|---|---|
| Figura y contratos | P0 | Contrato con adquirente/procesador como agregador (o el modelo que ofrezca), con STP y con PAC. Revisar con abogado si Kerno queda como agregador ante el adquirente y qué certificaciones exige. |
| PLD / KYC | P0 | Políticas de identificación del comercio, monitoreo de operaciones inusuales y conservación de expedientes según lo que exijan el adquirente y la regulación aplicable. |
| Datos personales | P0 | Aviso de privacidad (LFPDPPP), T&C del comercio, consentimiento para consulta fiscal. |
| Alcance PCI | P1 | Con camino A, Kerno no toca PAN en claro y el alcance se reduce, pero se debe documentar (SAQ correspondiente según el procesador). |
| Fiscal | P0 | Validación del modelo de apartado y de la facturación por un contador/fiscalista (F7, F8). |

---

## 6. Hallazgos críticos (corregir antes de cualquier demo con dinero real)

1. **Cadena de fraude cerrada en lo esencial (Lote C):** `settle` toma el monto del pago autorizado y la comisión de la tarifa, el posteo es atómico y exactly-once, las dispersiones van solo a la CLABE de liquidación verificada y las devoluciones revierten apartado y comisión. Queda: verificación real de titularidad de esa CLABE (hoy la marca un operador) y 2FA al cambiarla (ver F6).
2. **La bóveda CSD ya no usa llave hardcodeada** (Lote C); falta KMS real y re-cifrado al rotar (ver F2).
3. **La app Android ya es una app (Lote F), pero su cobro con tarjeta no está conectado al flujo real**: sigue llamando al endpoint EMV legacy (`410` sin `DEV_MODE`) y a un contrato provisional en vez de `/api/v1/card-payments/*`, (el endpoint de ventas y el paquete de Play Integrity del backend ya quedaron alineados en el Lote G; ver F9).
4. **No hay ninguna integración real con terceros** (procesador, PAC, STP, CEP): todo pasa por mocks. Para el procesador, el flujo backend completo (intent, webhooks firmados, devoluciones, contracargos, conciliación) ya existe sobre el puerto `acquirer.Provider` (Lote E); falta el adaptador real.

---

## 7. Plan sugerido hacia el MVP

**Fase 0 — Decisiones y contratos (en paralelo, 2–4 semanas)**
- Elegir procesador con Tap to Pay on Android en MX y modelo comercial (agregador/sub-afiliado).
- Contratos/sandboxes: procesador, STP, PAC, proveedor KYC.
- Validación fiscal del modelo RESICO y del apartado; asesoría legal sobre custodia de fondos y PLD.

**Fase 1 — Cimientos seguros ✅ cerrada (Lotes A–D, MVP-01..13, 2026-11-20)**
- ~~Driver Postgres + tests de integración reales; todo el estado a DB; outbox worker activo.~~ ✅ Lote A; transacción única pago+asiento+acumulado y eventos en el outbox ✅ Lote C; outbox con leases por dueño y timeout ✅ Lote D.
- ~~Usuarios, login OTP, JWT montado, tenant desde token, webhooks firmados~~ ✅ Lote B; llave de la bóveda desde el entorno ✅ Lote C.
- ~~Corregir `settle`/`disbursements`/refund (montos desde el servidor, transacción única pago+asiento).~~ ✅ Lote C; payouts idempotentes y recuperación de resultados desconocidos/caídas ✅ Lote D.
- ~~CI con `go test -race`, tests Kotlin y build de imagen; secretos fuera del repo.~~ ✅ Lote D.
- Queda de la Fase 1, pasa a la Fase 2: publicador real del outbox, proveedor OTP real, KMS real para la bóveda, verificación real de titularidad de la CLABE de liquidación y CD/protección de `main`.

**Fase 2 — Rieles de cobro y payout (≈4–6 semanas)**
- ~~Alta y KYB con revisión de operador, bloqueo y backoffice mínimo~~ ✅ Lote G (MVP-17..19: KYB con documentos cifrados y estados, alta en el adquirente por el puerto, backoffice con auditoría, endpoint de ventas, ventana de gracia del refresh); falta proveedor KYC y onboarding real del procesador.
- ~~Integración procesador (backend + webhooks)~~ ✅ Lote E sobre el puerto `acquirer.Provider` con mock (MVP-14..16); falta el adaptador del procesador elegido. STP real (abonos, órdenes, consulta de estado para el reconciliador de payouts, CEP, devoluciones).
- Pendientes heredados de la Fase 1 (ver arriba).
- Payout automático diario; conciliador real: procesador vs. ledger ✅ Lote E; falta STP/banco vs. ledger.

**Fase 3 — App y fiscal (≈5–7 semanas, en paralelo con fase 2)**
- ~~Módulo Android real con Compose~~ ✅ Lote F (AND-30..32: shell, OTP, sesión en Keystore, pantallas de saldo/cobro/ventas/perfil); falta alinear su contrato con `/api/v1/card-payments/*`, SDK Tap to Pay del procesador y pantallas restantes (KYB, devoluciones, facturar).
- PAC real, carga de CSD, cadena original validada, cancelación, PDF, factura global automática, comprobante de venta.

**Fase 4 — Piloto controlado (≈2–3 semanas)**
- ~~Backoffice mínimo~~ ✅ Lote G; alertas, runbooks; 5–20 comercios piloto con límites de monto y payout supervisado.

> Con 2–3 desarrolladores, el rango realista hasta un piloto con dinero real es de **3 a 5 meses**, condicionado sobre todo por los tiempos de contratación y certificación de procesador, STP y PAC.

---

## 8. Qué reutilizar tal cual y qué congelar

| Reutilizar en MVP | Congelar para fase 2 (adquirencia propia / producto financiero) |
|---|---|
| `ledger`, `store` (`pgx` + migraciones versionadas), idempotencia, outbox (dispatcher) | `iso8583`, `crypto` (DUKPT, PIN translation, HSM) |
| `stp` (CLABE, firma, cliente, CEP) | `clearing` (STEC, MIF) |
| `fiscal` (con validación contable y XSLT), `pac` (interfaz + failover) | `rbf` y endpoints RBF |
| `identity` (RFC/CURP, CSF, 69-B, bóveda CSD con KMS) | `IsoDepEmvReader`, `ScrambledKeypadModel`, `PinBlockFormatter`, `CvmRulesEvaluator` (las CVM las resuelve el SDK del procesador) |
| `middleware_auth`, `play_integrity`, `config_guard` | `StoreAndForwardQueue` (offline) |
| Android: `MoneySplitCalculator`, `PosTerminalViewModel`, `SoftPosApiClient`, `TaxVaultPresenter`, `KybOnboardingPresenter`, `CodiQrGenerator`, `WindowSecurityGuard` | `disputes` (solo reutilizar la parte de asientos contables de contracargo) |

---

## 9. Observaciones sobre el proceso de desarrollo

- Atendido en la Fase 1: el backlog `MVP-NN` se definió por flujos y cada lote trae tests de integración contra PostgreSQL real (p. ej. abono SPEI → saldo → payout → recuperación). Falta llevar esos criterios E2E a **sandboxes reales** (procesador, STP, PAC) en la Fase 2.
- Atendido en el Lote D: `README`, `HANDOFF.md`, `AGENTS.md` y el tablero `tech_roadmap.html` describen el estado real (prototipo, Fase 1 cerrada, sin rieles reales). `docs/PRODUCTION_IMPLEMENTATION_SPEC.md` queda marcado como diseño objetivo.
- Atendido parcialmente: el proceso ahora es Claude Code con subagente implementador + auditor independiente por lote, validación completa antes de push y CI en GitHub que debe quedar verde (`AGENTS.md` §2). El push directo a `main` se mantiene por decisión del dueño; cuando haya más colaboradores o dinero real, conviene PR + CI obligatoria (protección de rama, ver F11).
