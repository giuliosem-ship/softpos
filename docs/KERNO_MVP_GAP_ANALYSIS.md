# Kerno (SoftPOS) — Estado actual y análisis de gaps para MVP

**Fecha de revisión:** 2026-11-19
**Commit revisado:** `4dacefd` (`main`, sincronizado con `origin/main`, 67 commits desde 2026-09-15)
**Última actualización:** 2026-11-21 — Lotes K (MVP-23..26: notas de crédito, factura global por partes y `Fecha` por CP, ventas con factura y nombres en membresías, comprobante con autofactura) y L (MVP-27..31: ciclo del bloqueo 69-B, bitácora inmutable, barrido de devoluciones, métricas, endurecimiento de auth) aplicados y auditados
**Alcance:** backend Go (`cmd/`, `pkg/`, `migrations/`), app Android (`mobile/android`), Web POS (`web/pos`), docs y orquestador (`tools/loop`).

---

## 1. Resumen ejecutivo

Kerno tiene una **base de dominio amplia y bien testeada** (61/61 tareas del backlog cerradas, ~10k LOC Go de producción + ~5.6k LOC de tests, cobertura 60–89 % por paquete, todas las suites en verde). Lo que **no** tiene todavía es un producto que pueda cobrar dinero real de punta a punta: los módulos existen como librerías aisladas, pero la integración, la persistencia, la seguridad perimetral, la app móvil y los conectores con terceros reales (adquirente, PAC, STP) están incompletos o simulados.

| Dimensión | Estado | Comentario |
|---|---|---|
| Lógica de dominio (ledger, RESICO, CLABE, CFDI XML, ISO 8583, DUKPT, STEC, MIF, disputas) | 🟢 Avanzado | Librerías puras con tests unitarios sólidos. |
| Integración en el servidor HTTP | 🟡 Parcial | Solo ledger, pagos EMV/CoDi, CFDI, STP, RBF están ruteados. ISO 8583, HSM/DUKPT, clearing y disputas **no están conectados** a ninguna ruta. |
| Persistencia | 🟡 Parcial | Lote A: driver `pgx`, migraciones versionadas embebidas y comercios, pagos EMV, CoDi, CSD, acumulado mensual, ledger e idempotencia en PostgreSQL con `DATABASE_URL` (probado contra Postgres 16 real). Lote C: pago + asiento + acumulado RESICO + evento de outbox en una sola transacción (`store.MoneyStore`, ver `docs/MONEY_FLOWS.md`). Lote D: payouts persistidos (`payouts`, migración 009) y outbox con leases por dueño. Falta: publicador real del outbox y RBF en DB. Sin DB todo sigue en memoria (modo dev). |
| Seguridad perimetral | 🟡 Parcial | Lote B: login OTP, JWT montado en todas las rutas `/api/v1/*` salvo una lista pública explícita, tenant tomado del token (anti-IDOR), webhook CoDi firmado y secretos de dev solo con `DEV_MODE` (ver `docs/AUTH.md`). Lote C: montos de `settle` y CLABE de dispersión ya no vienen del cliente; llave de la bóveda desde el entorno. Lote G: alta con KYB (`docs/KYB.md`) — ningún comercio mueve dinero sin aprobación de un operador —, bloqueo de comercios, operadores de plataforma con bitácora de auditoría (`docs/BACKOFFICE.md`), `is_persona_moral` ya no viene del cliente y ventana de gracia del refresh token. Lote L: allowlist CORS, `iss`/`aud` del JWT y tope diario de OTP por número. Falta: proveedor OTP real, TLS/gateway, KMS real y verificación real de titularidad de la CLABE de liquidación. |
| Cobro con tarjeta real | 🔴 Bloqueante | Lote E (camino A): puerto `acquirer.Provider` con adaptador mock, modelo persistente `card_payments`, intents para el SDK Tap to Pay, webhooks firmados del procesador (captura → split en ledger, devoluciones, contracargos) y conciliación de sus liquidaciones; los endpoints EMV que "aprueban" al parsear TLV quedan solo con `DEV_MODE`. Falta elegir y contratar el procesador, su adaptador real y su SDK en la app. |
| App Android | 🟡 Parcial | Lote F (AND-30..32): módulo Android real (AGP, Compose Material 3, `MainActivity`, genera APK debug), login OTP con sesión cifrada bajo Android Keystore y pantallas de saldo/apartado, cobrar (teclado → tap mock / QR SPEI), ventas y perfil/CLABE (ver `docs/ANDROID_APP.md`). Falta: pasar del contrato de tarjeta provisional y del endpoint EMV legacy a `/api/v1/card-payments/*`, el SDK del procesador, Play Integrity real, pantallas de onboarding KYB (el backend y su contrato existen desde el Lote G)/devoluciones/facturar y la distribución en Play. |
| Integraciones externas | 🔴 Bloqueante | Procesador de tarjetas: solo `acquirer.MockProvider`. PAC: solo `MockPACClient`, pero desde el Lote I el flujo CFDI completo (facturas desde ventas, timbrado, cancelación con aceptación, consulta de estado, factura global, PDF) corre sobre el puerto `pac.PACClient` (`docs/CFDI.md`), y desde el Lote K también las notas de crédito y la autofactura desde el comprobante. Email/SMS: solo puertos con implementación de log. STP/CEP: existe cliente HTTP, pero el servidor siempre usa el mock. Lista 69-B del SAT: ingesta real desde el Lote I. |
| Operación (CI/CD, observabilidad, conciliación) | 🟡 Parcial | Lote D: CI en GitHub Actions (Go vet + race, integración Postgres 16, Web POS, Kotlin, imagen Docker), toolchain Go 1.27.1 fijado, secretos fuera del repo y reconciliador de payouts activo en `cmd/server`. Lote E: conciliación de liquidaciones del procesador (reporte de diferencias para operador, alertas `ERROR`, worker en `cmd/server` y `cmd/reconciler` de una pasada). Lote G: backoffice interno mínimo (revisión KYB, bloqueo, búsqueda de pagos, ledger por comercio, reintento de payouts, conciliación del procesador, bitácora; UI en `/backoffice/`). Lote L: métricas Prometheus y reglas de alerta (`docs/OBSERVABILITY.md`). Falta: despliegue (CD) e infraestructura, conciliación contra el estado de cuenta STP/bancario y enrutar las alertas a un pager real. |

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

**Fuera del MVP (fase 2+):** RBF/financiamiento, switch ISO 8583 propio, DUKPT/HSM/traducción de PIN, lotes STEC, calculadora MIF, motor de disputas propio (el adquirente gestiona contracargos; el MVP solo necesita reflejarlos en el ledger), SAF offline, iOS, páginas de economics/roadmap.

---

## 5. Gaps por funcionalidad

Severidad: **P0** = bloquea MVP · **P1** = necesario antes de clientes reales · **P2** = deseable en MVP.
Esfuerzo: **S** ≤ 1 semana · **M** 1–3 semanas · **L** > 3 semanas (1 dev).

### F1 — Alta y KYB/KYC del comercio
**Existe:** validador RFC/CURP, parser CSF y QR de constancia, lista 69-B con repo SQL, validador de CSD `.cer/.key` (Android), `POST /api/v1/merchants` (valida el formato del RFC y deja el alta en `DRAFT`). Desde el Lote G (ver `docs/KYB.md`): flujo `DRAFT → SUBMITTED → IN_REVIEW → APPROVED/REJECTED` con motivo (`merchant_kyb`, migración 012), captura de datos (razón social, RFC, CURP, régimen, MCC, domicilio, contacto) y documentos INE/selfie/comprobante de domicilio/estado de cuenta/CSF cifrados con la bóveda (`kyb_documents`, blobs detrás de `store.DocumentBlobStore`), validaciones al enviar (dígito verificador del RFC, persona física por longitud del RFC, solo RESICO PF, CURP ↔ RFC, documentos completos, 69-B → rechazo automático), puerto `kyb.KYCProvider` (prueba de vida/INE) con mock, revisión manual en el backoffice; al aprobar se asigna la CLABE virtual y se da de alta el sub-afiliado vía `acquirer.Provider.OnboardMerchant` (mock). Sin aprobación (o con el comercio bloqueado) no hay intents de tarjeta, QR CoDi/SPEI, dispersiones ni originación RBF. Auditoría del Lote G: la originación RBF quedó tras la aprobación y solo para `operator` (el dueño podía acreditarse un préstamo sin comisión), la CLABE virtual se asigna una sola vez aunque dos aprobaciones compitan, nunca se aprueba un RFC 69-B `LISTED`, un operador no aprueba ni desbloquea un comercio del que es miembro y el 69-B queda `UNKNOWN` (no `CLEAR`) sin lista cargada. Desde el Lote I (MVP-20) la lista 69-B completa del SAT se ingiere de verdad (worker con `SAT_69B_URL`, disparo auditado desde el backoffice u `operatorctl load-69b`): parser robusto Latin-1, reemplazo atómico vía tabla staging, protección contra listas que se encogen, versión/fecha de carga, lista vieja → `UNKNOWN` y re-revisión de comercios aprobados (bloqueo auditado si aparecen listados). Desde el Lote L (MVP-27, migración 020): el bloqueo 69-B emite el evento `merchant.blocked_69b` en la misma transacción (notificación vía outbox), se levanta solo al pasar a `DESVIRTUADO`/`SENTENCIA FAVORABLE` (o salir de la lista) si lo puso el proceso 69-B (nunca un bloqueo manual; `blocked_source`), un desbloqueo de operador de un RFC aún listado exige motivo y registra una excepción documentada por situación (un cambio de situación vuelve a bloquear) y la carga rechaza archivos con situaciones cambiadas masivamente o RFCs reemplazados (`REJECTED_CHANGES`, override auditado; auditoría del Lote L: los RFCs reemplazados cuentan porque su ausencia desbloquea, y un operador puede tomar un bloqueo `SAT_69B` como propio); el proceso queda por validar con cumplimiento.

| Gap | Sev. | Esf. | Detalle / evidencia |
|---|---|---|---|
| Proveedor real de verificación de identidad | P0 | M | Solo existe `kyb.MockKYCProvider` (`KYC_PROVIDER=mock`, rechazado en producción; sin proveedor la identidad queda `UNKNOWN` y la revisa el operador). Falta contratar el proveedor (Truora, Metamap, …), su adaptador de `kyb.KYCProvider` (prueba de vida, INE contra lista nominal, coincidencia facial) y los requisitos documentales que pida el adquirente. |
| Documento subido durante el envío | P1 | S | La subida verifica que el KYB sea editable y luego guarda el documento sin atarlo a la versión del registro: una subida concurrente con `submit` puede reemplazar (supersede) un INE/selfie después de que el proveedor KYC verificó el anterior, y el operador vería otro archivo que el verificado. Guardar los ids de documento verificados en el registro (o ligar la subida al `version` con compare-and-set). |
| Alta real en el adquirente | P0 | M | El puerto `acquirer.Provider.OnboardMerchant` (llave `kerno_onboard_<id>`) se llama al aprobar el KYB, pero solo con el mock; falta la API de onboarding de sub-afiliados del procesador elegido (ver F3). |
| Expediente KYC: retención y representante legal | P1 | S | Falta política de conservación/borrado de expedientes (PLD, LFPDPPP), almacenamiento de objetos (S3) para los blobs y, cuando se soporten personas morales, representante legal y beneficiario controlador. |
| Registro de la CLABE virtual en STP | P1 | S | Desde el Lote G la CLABE virtual se asigna al aprobar el alta (aleatoria `646180…`, el cliente nunca la elige). Falta usar el rango/prefijo que asigne STP al participante y registrarla en STP. |

### F2 — Autenticación, sesiones y multi-tenant
**Existe (Lote B, ver `docs/AUTH.md`):** usuarios y membresías con rol, login OTP (`/api/v1/auth/otp/*`), access JWT HS256 de 15 min + refresh opaco rotativo con detección de reuso y logout, JWT montado en todas las rutas salvo lista pública, tenant derivado del token (body/path ajeno → 403, recursos ajenos → 404, idempotencia por tenant), dispersiones solo `owner`, alta de comercio con id generado por el servidor, nonce de atestación de un solo uso + verificación Play Integrity, rate limit por IP en `/auth/*`, webhook CoDi firmado (HMAC + timestamp + anti-replay), config guard que exige `JWT_SECRET`/`CODI_WEBHOOK_SECRET` y rechaza `DEV_MODE` en producción.

| Gap | Sev. | Esf. | Detalle / evidencia |
|---|---|---|---|
| Proveedor OTP real | P0 | S | Solo existe `auth.LogSender` (no entrega el código fuera de `DEV_MODE`). Implementar `auth.OTPSender` con un proveedor SMS/WhatsApp real (el tope diario por número ya existe desde el Lote L, MVP-31: `OTP_DAILY_LIMIT`, compartido entre instancias). |
| Bóveda CSD sin KMS real | P1 | S | Desde el Lote C la llave sale de `VAULT_MASTER_KEY_HEX` vía `identity.KeyProvider` (sin llave fuera de `DEV_MODE` no arranca) y cada sobre guarda su id de llave. Falta cliente KMS real detrás de `identity.KMSClient` y un job que re-cifre (`RewrapCSD`) los CSD al rotar. |
| Sobre del CSD sin ligar al comercio | P2 | S | Auditoría del Lote I: el sobre de la bóveda (`EncryptedCSD`) se cifra con AES-GCM sin datos asociados: quien pueda escribir en `merchants.encrypted_csd_json` podría copiar el CSD de un comercio a otro y firmar con él (el PAC lo rechazaría por RFC, pero no hay defensa propia). Usar el `merchant_id` como AAD. La detección FIEL vs. CSD usa key usage / EKU; añadir la regla de la OU (sucursal) como segunda señal. |
| Atestación no exigida | P1 | S | El endpoint de nonce y la verificación existen, pero el veredicto no se exige ni se liga a la sesión (login o cobro). |
| TLS / rate limit distribuido | P1 | S | El rate limit por IP es en memoria por proceso y usa la IP TCP; TLS ausente en el servidor (normalmente en el API gateway / balanceador). Desde el Lote L (MVP-31) CORS es allowlist explícita (`CORS_ALLOWED_ORIGINS`, deny por defecto), el JWT valida `iss`/`aud` y el tope diario de OTP por número es persistente entre instancias. |
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
| Validación fiscal por contador | P0 | S | Revisar con un contador: base de ISR RESICO (ingresos efectivamente cobrados), tratamiento de IVA trasladado vs. acreditable, retenciones de PM, cambio de tramo intra-mes, tope anual. Documentar supuestos. Decisiones del Lote I (`docs/CFDI.md`) que debe confirmar: (1) toda venta lleva IVA 16 % incluido en el precio (base = bruto/1.16 redondeo half-up), sin tasa 0 %, exento ni 8 % de región fronteriza; (2) una venta con devolución parcial se factura por el neto cobrado (y si el neto del split cae fuera de los límites del SAT se re-divide el bruto neto: base a ±1 centavo del ledger); (3) retención ISR 1.25 % (113-J) cuando un PM recibe factura de un RESICO PF, sin retención de IVA y sin re-postear el split de la venta; (4) factura global mensual por defecto (diaria opcional), Periodicidad 04/01, conceptos `01010101`/`ACT`/"Venta" uno por venta, FormaPago = la de mayor monto, se emite aunque pasen > 72 h del cierre y, desde el Lote K, en varias partes por periodo (máx. 5,000 conceptos) incluyendo ventas tardías o liberadas hasta `CFDI_GLOBAL_MAX_AGE` (90 días); (5) una venta en una global activa no se factura individualmente: hay que cancelar la global con motivo 04 y re-emitirla; (6) clave de producto/unidad única por comercio; (7) `Fecha` en la hora local del CP de `LugarExpedicion` (tabla de rangos de CP, Lote K; los periodos de la global siguen en hora de la Ciudad de México); (8) las ventas en contracargo (`CHARGEBACK`, incluso `CHARGEBACK_REVERSED`) no se facturan; (9) desde el Lote K una devolución o contracargo posterior a la factura genera automáticamente un CFDI de egreso (tipo `E`, relación 01 con el UUID de la factura, concepto `84111506`/`ACT`, IVA y retención ISR proporcionales, `UsoCFDI` G02 o, para la global, público en general con su `InformacionGlobal`), en lugar de cancelar con sustitución; un contracargo revertido después de la nota no se re-factura (se marca para el operador); (10) auditoría del Lote K: una factura con notas de crédito vigentes no se cancela (`409 INVOICE_HAS_CREDIT_NOTES`, hay que cancelar antes las notas, como exige el SAT), la última nota de un concepto toma el ISR retenido restante, el CFDI de ingreso manual tras un contracargo revertido debe ser por el neto cobrado actual menos el neto fiscal (factura − notas vigentes), y tras cancelar una global con motivo 04 la venta a facturar nominativamente puede volver a una parte nueva de la global en la siguiente corrida (1 h) si no se factura antes. |
| ¿Qué pasa con el dinero apartado? | P0 | M | Hoy es una subcuenta contable. Definir si se retiene en cuenta segregada, cuándo se libera y cómo el comercio paga al SAT (¿Kerno solo aparta o también paga?). Implica F6 y temas legales. |
| Reporte mensual para declaración | P1 | S | Resumen de ingresos, IVA, ISR estimado por mes. El CSV de Android es local; debe salir del backend. |

### F8 — Facturación CFDI 4.0
**Existe:** desde el Lote K (MVP-23..26): notas de crédito automáticas (CFDI de egreso, relación 01) para devoluciones/contracargos posteriores a la factura, factura global en varias partes por periodo con reconciliación de ventas sin factura y alerta, `Fecha` en la zona horaria del CP de expedición, ventas con su factura activa en la API y comprobante de venta público con autofactura. Desde el Lote I (MVP-21/22, `docs/CFDI.md`): cadena original validada byte a byte contra el XSLT oficial del SAT (`xsltproc` en tests y CI, 12+ variantes) y sello sobre la cadena; carga de CSD por el dueño (`.cer` + `.key` PKCS#8 cifrado + contraseña; RFC, vigencia, CSD vs FIEL, llave↔certificado) guardado cifrado en la bóveda; catálogos SAT versionados embebidos + endpoint; datos del receptor validados contra las reglas de catálogo (régimen ↔ uso CFDI, PF/PM, CP); facturas desde ventas con montos del split posteado, idempotencia y una factura activa por venta; cancelación con motivos 01–04 (01 con sustituto, aceptación del receptor por *polling*); PDF con QR de verificación del SAT (Go puro) y envío por el puerto `Mailer`; factura global automática (mensual/diaria); reintento de timbrados fallidos en el backoffice.

| Gap | Sev. | Esf. | Detalle / evidencia |
|---|---|---|---|
| Sin PAC real | P0 | M | Solo `MockPACClient` (simula reglas SAT). Contratar PAC y construir el adaptador real del puerto `pac.PACClient` (`Stamp` idempotente por sello, `Cancel`, `Status`; contrato en `docs/CFDI.md`), mapear sus códigos de error, validar CFDIs del sandbox con el validador del SAT y verificar la cadena de certificación del CSD contra las AC del SAT / LCO. |
| Retención en el split | P1 | S | Desde el Lote I el receptor se captura y valida al facturar y el CFDI a persona moral lleva la retención ISR 1.25 % (Art. 113-J); desde el Lote K el cliente final se autofactura desde el comprobante (`/r/{token}`). Falta re-postear el split de la venta (apartado ISR) cuando la factura resulta a persona moral. |
| Envío real del CFDI y del comprobante | P1 | S | Desde el Lote I el PDF con QR del SAT se envía por el puerto `api.Mailer` y desde el Lote K el comprobante de venta por `Mailer` y el puerto `api.SMSSender`, pero solo existen `LogMailer` / `LogSMSSender`. Falta proveedor de email, SMS y WhatsApp real. |
| Formato `tt` del QR | P2 | S | Auditoría del Lote I: el QR usa `tt` con 10 enteros y 6 decimales rellenados con ceros (formato de CFDI 3.2, igual al ejemplo de algunos proveedores); varias implementaciones de 3.3/4.0 omiten los ceros no significativos. Confirmar contra el Anexo 20 vigente (apartado D, representación impresa) y el verificador del SAT con un CFDI real del sandbox. |
| Catálogos SAT completos | P2 | S | Desde el Lote I hay un subconjunto versionado embebido (régimen, uso CFDI, claves de producto/unidad elegibles, motivos, periodicidad) y claves por comercio. Falta el catálogo completo de `c_ClaveProdServ` / `c_CodigoPostal` y un proceso de actualización cuando el SAT publique cambios. |

### F9 — App Android del comercio
**Existe:** lógica de presentación pura (ViewModel de terminal, presentadores de bóveda fiscal, onboarding y RBF; descriptores de pantallas y navegación), cliente API con idempotencia, generador QR. Desde el Lote F (AND-30..32, `docs/ANDROID_APP.md`): módulo `com.android.application` con Compose Material 3, DI manual y `gradlew` (Gradle 8.14.3); login teléfono → OTP contra `docs/AUTH.md` con refresco single-flight y logout; sesión solo como cifrado AES-256-GCM bajo llave de Android Keystore; pantallas Inicio (disponible vs. apartado fiscal), Cobrar (teclado en centavos enteros → tap con proveedor mock en debug / QR SPEI), Ventas (lista y detalle) y Perfil (CLABE, salir). En release el cobro con tarjeta queda deshabilitado hasta tener SDK del procesador.

| Gap | Sev. | Esf. | Detalle / evidencia |
|---|---|---|---|
| Ventas incluidas en factura global sin marca en la app | P2 | S | Desde el Lote K el backend expone `invoice_id` / `invoice_kind` / `invoice_status` y notas de crédito por venta (`GET …/sales`, `GET …/sales/{sale_id}`), y el nombre del comercio en las membresías (el comprobante ya se comparte y envía desde el detalle de venta, AND-39). Falta que la app Android los use: deshabilitar "Facturar" en ventas ya facturadas o en una global y mostrar el nombre en el selector de comercio. |
| Integrar SDK del procesador | P0 | M | Tap to Pay del proveedor elegido (camino A), incluidos sus requisitos de dispositivo, atestación y permisos. |
| Play Integrity real | P1 | S | El cliente es lógica pura con interfaz; falta el SDK real de Google (el endpoint de nonce ya existe desde el Lote B). |
| Firma, distribución y updates | P1 | S | Keystore de firma, Play Console (track interno/cerrado), política de versión mínima forzada. (`targetSdk` 36 y wrapper 8.14.3 con `distributionSha256Sum` resueltos en el Lote H.) |
| Telemetría y crash reporting | P1 | S | Crashlytics/Sentry sin datos sensibles. |

### F10 — Devoluciones, cancelaciones y comprobante
**Existe:** desde el Lote K, comprobante de venta público por venta confirmada (`/r/{token}` firmado y con expiración: PAN enmascarado, marca, autorización, monto, fecha, nombre/RFC del comercio y autofactura) enviable por email/SMS (`docs/CFDI.md`), y notas de crédito automáticas para devoluciones/contracargos posteriores a la factura. Desde el Lote E, `POST /api/v1/card-payments/{id}/refunds` (solo `owner` salvo `ALLOW_CASHIER_REFUNDS`, `Idempotency-Key` obligatoria, monto retenido contra el saldo reembolsable): la solicitud va al procesador y el ledger solo se mueve con su webhook `payment.refunded` (reversión proporcional); contracargos `chargeback.opened`/`reversed` reflejados en el ledger (débito del disponible del comercio, que puede quedar negativo) con evento de outbox para notificarlo. Desde el Lote L (MVP-29) un barrido (`REFUND_SWEEP_*`, `FOR UPDATE SKIP LOCKED`) reconsulta en el procesador (`acquirer.Provider.GetRefund`) las solicitudes que quedan `REQUESTED` y las resuelve por el mismo camino del webhook (exactly-once aunque el webhook llegue a la vez), a `PENDING`, `FAILED` o expiradas (retención liberada), con alerta tras N intentos; un contracargo con devoluciones en curso que exceden el remanente alerta al operador (documentado en `docs/ACQUIRER_INTEGRATION.md` §4). Legacy (solo `DEV_MODE`): `POST /api/v1/payments/emv/reverse`: void y devolución total o parcial (acotada al saldo reembolsable, solo `owner` salvo `ALLOW_CASHIER_REFUNDS`) que revierte proporcionalmente disponible, apartado IVA/ISR, comisión y acumulado RESICO en una transacción (el barrido RBF no se revierte).

| Gap | Sev. | Esf. | Detalle / evidencia |
|---|---|---|---|
| Devoluciones `PENDING` sin webhook | P2 | S | Auditoría del Lote L: el barrido (MVP-29) solo reconsulta solicitudes `REQUESTED`; una devolución que el procesador aceptó (`PENDING`, con `provider_refund_id`) y cuyo webhook final se pierde retiene su monto indefinidamente. Extender el barrido a `PENDING` antiguas (`GetRefund` → `succeeded`/`failed` por el camino del webhook) con su propia alerta. |
| Notificación y defensa de contracargos | P1 | S | Desde el Lote E el contracargo se refleja en el ledger y encola `card_payment.chargeback_opened`; falta entregar el aviso al comercio (depende del publicador real del outbox, F5) y el flujo para enviar evidencia al procesador. |

### F11 — Plataforma y operación
| Gap | Sev. | Esf. | Detalle / evidencia |
|---|---|---|---|
| CD y protección de `main` | P1 | S | El CI existe desde el Lote D (`go vet`, `go test -race`, Postgres 16, Web POS, Kotlin, imagen Docker). Falta: despliegue continuo a staging/prod, publicar la imagen, lint/`gofmt` (hay archivos sin formato en paquetes de fase 2), APK firmado de release (el CI ya compila el APK debug desde el Lote F) y exigir CI verde en `main` (protección de rama). |
| Secretos en despliegue | P1 | S | Desde el Lote D no hay secretos en el repo (`.env` ignorado, `.env.example` con placeholders) y el guard de producción cubre cada secreto leído (test que lo amarra al código). Falta inyectarlos desde un Secrets Manager/KMS en el despliegue y definir su rotación. |
| Secretos del historial y semántica del outbox | P1 | S | Los valores que estuvieron en `docker-compose.yml` antes del Lote D (contraseña de Postgres, `STP_HMAC_SECRET`, `VAULT_MASTER_KEY_HEX`) siguen en el historial de git: no reutilizarlos en ningún entorno. Outbox: desde la auditoría del Lote D un lote se corta antes de que una publicación pueda exceder el lease; los eventos sobrantes se reclaman al vencer el lease y ese reclamo cuenta como intento aunque no se hayan publicado (falta liberarlos sin contar). |
| Infraestructura de despliegue | P0 | M | Postgres administrado con backups/PITR, Redis administrado, TLS, dominio, entornos dev/staging/prod. |
| Observabilidad | P1 | S | Desde el Lote L (MVP-30, `docs/OBSERVABILITY.md`) hay métricas Prometheus en un listener aparte (`METRICS_ADDR`, `METRICS_TOKEN`): HTTP por ruta/estado/latencia, fallos de webhook por origen, descuadres de conciliación, payouts y devoluciones atascados, outbox `FAILED` y antigüedad de pendientes, cargas 69-B, barrido de devoluciones; reglas sugeridas en `deploy/prometheus/kerno-alerts.yml`. Falta: enrutar las alertas a un pager real (Alertmanager → PagerDuty/Opsgenie) con guardia, trazas distribuidas (OpenTelemetry), dashboards y envío/retención de logs. |
| Bitácora de operadores: exportación inmutable | P1 | S | Desde el Lote L (MVP-28, migración 021) la base rechaza `UPDATE`/`DELETE`/`TRUNCATE` en `operator_audit_log` (trigger) y `merchant_kyb`/`kyb_documents` son `ON DELETE RESTRICT`; el SQL de permisos del rol de la app está en `docs/BACKOFFICE.md`. Falta: exportar la bitácora a almacenamiento inmutable (WORM, encadenada por hash) y separar el rol de migraciones del rol de la app en el despliegue. |
| Backoffice interno | P2 | S | Desde el Lote G existe el backoffice mínimo (`docs/BACKOFFICE.md`): revisión KYB con descarga auditada de documentos, aprobar/rechazar, bloquear, búsqueda de pagos, ledger por comercio, reintento de payouts, conciliación del procesador y bitácora `operator_audit_log`; UI estática en `/backoffice/`. Falta: permisos por operador, 2FA/IP allowlist, asientos de corrección. (Desde el Lote I: estado/disparo de la lista 69-B y reintento de timbrados CFDI.) |
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
3. **La app Android usa ya el contrato real de cobro (Lote H)**, pero sin SDK de Tap to Pay de un procesador real no puede cobrar con tarjeta fuera de `DEV_MODE` (ver F3/F9).
4. **No hay ninguna integración real con terceros** (procesador, PAC, STP, CEP): todo pasa por mocks. Para el procesador, el flujo backend completo (intent, webhooks firmados, devoluciones, contracargos, conciliación) ya existe sobre el puerto `acquirer.Provider` (Lote E); para el PAC, el ciclo CFDI completo existe sobre `pac.PACClient` (Lote I); faltan los adaptadores reales.

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
- Ingesta real de la lista 69-B del SAT ✅ Lote I (MVP-20).
- ~~Alta y KYB con revisión de operador, bloqueo y backoffice mínimo~~ ✅ Lote G (MVP-17..19: KYB con documentos cifrados y estados, alta en el adquirente por el puerto, backoffice con auditoría, endpoint de ventas, ventana de gracia del refresh); falta proveedor KYC y onboarding real del procesador.
- ~~Integración procesador (backend + webhooks)~~ ✅ Lote E sobre el puerto `acquirer.Provider` con mock (MVP-14..16); falta el adaptador del procesador elegido. STP real (abonos, órdenes, consulta de estado para el reconciliador de payouts, CEP, devoluciones).
- Pendientes heredados de la Fase 1 (ver arriba).
- Payout automático diario; conciliador real: procesador vs. ledger ✅ Lote E; falta STP/banco vs. ledger.

**Fase 3 — App y fiscal (≈5–7 semanas, en paralelo con fase 2)**
- ~~Módulo Android real con Compose~~ ✅ Lote F (AND-30..32: shell, OTP, sesión en Keystore, pantallas de saldo/cobro/ventas/perfil); falta alinear su contrato con `/api/v1/card-payments/*`, SDK Tap to Pay del procesador y pantallas restantes (KYB, devoluciones, facturar).
- ~~Carga de CSD, cadena original validada, cancelación, PDF, factura global automática~~ ✅ Lote I (MVP-21/22, `docs/CFDI.md`); ~~notas de crédito, factura global por partes/reconciliación, `Fecha` por CP, autofactura y comprobante de venta~~ ✅ Lote K (MVP-23..26); falta PAC real (adaptador del puerto), proveedores de email/SMS, validación del contador y que la app use los nuevos campos.

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
- Lote K (MVP-23..26): cada cambio fiscal nuevo (egreso, partes de la global, zona horaria, autofactura) quedó documentado en `docs/CFDI.md` con sus decisiones pendientes de contador (F7), golden tests del XSLT para el egreso y pruebas contra PostgreSQL real (jobs de nota de crédito exactly-once, partes concurrentes, autofactura concurrente, migraciones 018/019 down/up).
- Atendido parcialmente: el proceso ahora es Claude Code con subagente implementador + auditor independiente por lote, validación completa antes de push y CI en GitHub que debe quedar verde (`AGENTS.md` §2). El push directo a `main` se mantiene por decisión del dueño; cuando haya más colaboradores o dinero real, conviene PR + CI obligatoria (protección de rama, ver F11).
