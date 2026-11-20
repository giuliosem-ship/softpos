# Kerno (SoftPOS) — Estado actual y análisis de gaps para MVP

**Fecha de revisión:** 2026-11-19
**Commit revisado:** `4dacefd` (`main`, sincronizado con `origin/main`, 67 commits desde 2026-09-15)
**Alcance:** backend Go (`cmd/`, `pkg/`, `migrations/`), app Android (`mobile/android`), Web POS (`web/pos`), docs y orquestador (`tools/loop`).

---

## 1. Resumen ejecutivo

Kerno tiene una **base de dominio amplia y bien testeada** (61/61 tareas del backlog cerradas, ~10k LOC Go de producción + ~5.6k LOC de tests, cobertura 60–89 % por paquete, todas las suites en verde). Lo que **no** tiene todavía es un producto que pueda cobrar dinero real de punta a punta: los módulos existen como librerías aisladas, pero la integración, la persistencia, la seguridad perimetral, la app móvil y los conectores con terceros reales (adquirente, PAC, STP) están incompletos o simulados.

| Dimensión | Estado | Comentario |
|---|---|---|
| Lógica de dominio (ledger, RESICO, CLABE, CFDI XML, ISO 8583, DUKPT, STEC, MIF, disputas) | 🟢 Avanzado | Librerías puras con tests unitarios sólidos. |
| Integración en el servidor HTTP | 🟡 Parcial | Solo ledger, pagos EMV/CoDi, CFDI, STP, RBF están ruteados. ISO 8583, HSM/DUKPT, clearing y disputas **no están conectados** a ninguna ruta. |
| Persistencia | 🔴 Bloqueante | `go.mod` no declara driver PostgreSQL → con `DATABASE_URL` el servidor **aborta al arrancar**. Sin DB todo vive en memoria. |
| Seguridad perimetral | 🔴 Bloqueante | El middleware JWT existe y tiene tests, pero **no está montado**: todas las rutas (incluidas liquidación y dispersión SPEI) son públicas. |
| Cobro con tarjeta real | 🔴 Bloqueante | No hay kernel EMV ni autorización contra emisor; el backend "aprueba" al parsear TLV. |
| App Android | 🔴 Bloqueante | Es un **módulo Kotlin/JVM** (sin Android Gradle Plugin, sin Activity/Compose, sin `android.nfc`). No genera APK. |
| Integraciones externas | 🔴 Bloqueante | PAC: solo `MockPACClient`. STP/CEP: existe cliente HTTP, pero el servidor siempre usa el mock. |
| Operación (CI/CD, observabilidad, conciliación) | 🔴 | Sin CI, `cmd/reconciler` es un stub, sin métricas ni backoffice. |

**Conclusión:** el proyecto está en etapa de *"motor de dominio prototipado"*, no de MVP. Estimo que lo construido cubre ~25–30 % del trabajo de un MVP que cobre dinero real, y gran parte del código más sofisticado (switch ISO 8583, DUKPT, STEC, MIF) **no es necesario para el MVP** si se cobra con tarjeta a través de un proveedor certificado.

---

## 2. Validación ejecutada en esta revisión

| Verificación | Resultado |
|---|---|
| `go test -race -count=1 ./...` (Go 1.24 local, directiva bajada temporalmente desde 1.27.1 en una copia) | ✅ 14/14 paquetes OK, sin data races |
| Cobertura (`go test -cover`) | api 62 %, stp 61 %, payments 67 %, rbf 71 %, fiscal 75 %, pac 79 %, store 79 %, ledger 80 %, identity 83 %, disputes 82 %, iso8583 84 %, clearing 88 %, crypto 89 %, cmd/server 60 % |
| `node --test web/pos/softpos_sdk.test.js` | ✅ 7/7 |
| Tests Android | Último reporte en `build/test-results` (1-nov): 78 tests, 0 fallos. No se re-ejecutó (no hay `gradlew` ni Gradle en el entorno); el HANDOFF reporta 79 tras AND-22. |
| Arranque con `DATABASE_URL` | ❌ `sql: unknown driver "postgres" (forgotten import?)` → exit 1. `docker compose up` falla por lo mismo. |
| `POST /api/v1/disbursements` y `GET /api/v1/merchants/{id}/balances` sin token | ❌ Responden sin autenticación (400 por validación de monto / 200 con saldo). |

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
- `store`: repos SQL (ledger, comercios, RBF, pagos EMV, outbox, idempotencia), runner de migraciones, cliente Redis RESP propio.
- `api`: 16 rutas REST + servidor estático del Web POS; idempotencia; middleware JWT (no montado).

**Android (`mobile/android`)** — 25 clases Kotlin con lógica pura y descriptores de UI (calculadora de splits, CVM, teclado aleatorio, ISO 9564, TLV, orquestador de tap, cliente API, SAF cifrada, guard de ventana). Ninguna depende del SDK de Android; los puntos de contacto con el SO son interfaces (`IsoDepTransceiver`, `SecureWindow`, `AccessibilityScanner`).

**Web POS (`web/pos`)** — demo funcional con simulación de tap, CoDi y desglose fiscal, más páginas de narrativa (overview, economics, roadmap).

**Tooling** — orquestador multi-modelo (LM Studio, Qwen/Gemma/DeepSeek) con ciclo RED→GREEN→AUDIT.

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
**Existe:** validador RFC/CURP, parser CSF y QR de constancia, lista 69-B con repo SQL, validador de CSD `.cer/.key` (Android), `POST /api/v1/merchants`.

| Gap | Sev. | Esf. | Detalle / evidencia |
|---|---|---|---|
| El alta no valida nada | P0 | S | `HandleRegisterMerchant` solo exige `id`, `rfc`, `name`; no llama al validador RFC ni consulta 69-B (`pkg/api/server.go:396`). El `id` lo manda el cliente. |
| Sin modelo de usuario/propietario | P0 | M | No existe entidad usuario, ni relación usuario↔comercio, ni datos de contacto/representante. |
| Sin captura documental KYC | P0 | M | INE/selfie/prueba de vida, comprobante de domicilio, estado de cuenta (CLABE de liquidación), giro/MCC. Normalmente vía proveedor de identidad (Truora, Metamap, etc.) + requisitos del adquirente. |
| Sin alta en el adquirente | P0 | M | Con camino A, el comercio debe crearse como sub-afiliado/cuenta en el proveedor (API de onboarding del procesador). |
| Sin flujo de estado del alta | P1 | S | `PENDING → IN_REVIEW → APPROVED/REJECTED`, con motivo; revisión manual en backoffice. |
| Asignación de CLABE virtual | P1 | S | Generador existe; falta asignarla automáticamente al aprobar y registrar la cuenta en STP. |
| Persistencia | P0 | — | Depende de F5 (DB no arranca). |

### F2 — Autenticación, sesiones y multi-tenant
**Existe:** `middleware_auth.go` (JWT HS256, `merchant_id` en contexto, anti-IDOR) con tests; verificador Play Integrity en Go; config guard de producción.

| Gap | Sev. | Esf. | Detalle / evidencia |
|---|---|---|---|
| Middleware JWT no montado | P0 | S | `Router()` solo envuelve `Recovery` + `Logging` (`pkg/api/server.go:474`). Todas las rutas son públicas. |
| No hay emisión de tokens | P0 | M | No existe login, registro, OTP por SMS/WhatsApp, refresh tokens, revocación ni logout. |
| Rutas no derivan el tenant del token | P0 | S | Handlers como `settle` y `disbursements` toman `merchant_id` del body; deben tomarlo del contexto autenticado. |
| Webhooks sin autenticación | P0 | S | `POST /webhooks/codi/confirm` no verifica firma. El de STP sí verifica (RSA/HMAC), pero acepta HMAC con secreto por defecto en dev. |
| Endpoint de nonce de atestación inexistente | P1 | S | Android (`PlayIntegrityClient`) pide `/api/v1/auth/attestation-nonce`; el backend no lo expone. |
| Bóveda CSD con llave fija | P0 | S | `NewServer` crea la bóveda con `defaultVaultMasterKeyHex` hardcodeada e **ignora** `VAULT_MASTER_KEY_HEX` (`pkg/api/server.go:63`); el config guard valida una variable que no se usa. Mover a KMS. |
| Rate limiting / CORS / TLS | P1 | S | Ausentes en el servidor; normalmente en API gateway. |
| Roles | P2 | S | Dueño vs. cajero vs. operador interno (backoffice). |

### F3 — Cobro con tarjeta contactless (Tap to Phone)
**Existe:** parser TLV, motor de estados de pago, reglas CVM, orquestador de tap con auto-reversa, adaptador multi-riel (`WhiteLabelTapProvider`), simulador en Web POS.

| Gap | Sev. | Esf. | Detalle / evidencia |
|---|---|---|---|
| Sin proveedor de adquirencia integrado | P0 | L | Elegir y contratar procesador con Tap to Pay on Android en MX; integrar su SDK en la app y su API en backend (crear cobro, capturar, webhooks de estado). |
| La autorización no es real | P0 | — | `PaymentEngine.Authorize` aprueba si el TLV trae ARQC/TC, sin consultar emisor (`pkg/payments/emv.go:252`). Con camino A, este endpoint se reemplaza por la confirmación del procesador (webhook firmado). |
| `IsoDepEmvReader` no es un kernel EMV | — (fase 2) | — | Solo envía SELECT PPSE y espera PAN y 9F26 en esa respuesta; no hace SELECT AID / GPO / READ RECORD / GENERATE AC, y marca `Approved` localmente. Útil solo como referencia. |
| Web NFC no sirve para tarjetas | P1 | S | El SDK web usa `NDEFReader`, que lee etiquetas NDEF, no tarjetas EMV ISO-DEP. El Web POS debe quedar como demo o como canal solo-SPEI; documentarlo. |
| Modelo de pago persistente y conciliable | P0 | M | Guardar `provider_payment_id`, estado, monto, comisión del procesador, fecha de liquidación del procesador; idempotencia por intento de cobro. |
| Webhooks del procesador | P0 | M | Recepción firmada de `authorized / captured / refunded / chargeback / settled` → posteo en ledger vía outbox. |
| Liquidación del procesador a Kerno | P1 | M | Conciliar los depósitos del procesador (T+n) contra pagos capturados; hoy solo existe `ASSET:ACQUIRER_RECEIVABLE` sin proceso. |

### F4 — Cobro por transferencia SPEI (CLABE / QR)
**Existe:** CLABE virtual, webhook `stp/abono` con verificación de firma, generador "CoDi QR", cliente HTTP STP.

| Gap | Sev. | Esf. | Detalle / evidencia |
|---|---|---|---|
| Contrato y ambiente STP reales | P0 | M | Cuenta concentradora, llaves, certificados, IP allowlist, ambiente de pruebas STP. |
| Formato "CoDi" no es el de Banxico | P1 | M | `codiWirePayload` es un JSON propio (`v, ic, c, m…`). Un cobro CoDi real se origina vía un participante del esquema. Para MVP lo más simple es **QR/enlace con CLABE + referencia** (SPEI estándar) y dejar CoDi/DiMo para después; validar con STP qué ofrece. |
| Webhook de abono → pago del comercio | P1 | S | Verificar que el abono identifique comercio + referencia de cobro, idempotencia por `claveRastreo`, y postee en ledger persistente. |
| Expiración y montos no coincidentes | P2 | S | Qué pasa si pagan de más/menos o después de expirar. |

### F5 — Ledger persistente, saldos y conciliación
**Existe:** motor de partida doble, repo SQL con bloqueo pesimista, migraciones 001/002, outbox con repo, idempotencia SQL/Redis.

| Gap | Sev. | Esf. | Detalle / evidencia |
|---|---|---|---|
| Sin driver PostgreSQL | P0 | S | `go.mod` sin dependencias; `sql.Open("postgres")` falla. Agregar `pgx/v5/stdlib` (o `lib/pq`), `go.sum`, y test de integración contra Postgres real (testcontainers). Hoy los tests usan `sql_test_driver.go`, un driver falso de 824 líneas que no valida SQL real. |
| `settle` confía en el cliente | P0 | S | `HandlePaymentSettle` toma `gross_amount_cents`, `platform_fee_cents` y `rbf_sweep_cents` del body, y si el `payment_id` **no existe** igual acredita al comercio (`pkg/api/handlers_payments.go:60–100`). Combinado con F2 sin auth y `disbursements` abierto, es una vía directa de fraude. El monto debe salir del pago autorizado y la comisión de la tarifa del comercio. |
| Estado de pagos y comercios en memoria | P0 | M | `PaymentEngine`, `CoDiService`, `monthlySalesCents`, `encryptedCSDs`, `merchants` viven en mapas; los repos SQL existen pero no todos están cableados en los handlers. |
| Outbox sin dispatcher | P1 | S | El repositorio y worker existen pero `main.go` no arranca ningún worker. |
| Errores de posteo ignorados | P1 | S | Ej. refund: `_ = s.ledger.PostTransaction(jnl)` (`handlers_emv_codi.go`). Un fallo deja estado de pago y ledger inconsistentes. Pago + asiento deben ir en la misma transacción DB. |
| Conciliación diaria | P1 | M | `cmd/reconciler/main.go` es un stub (crea un ledger vacío y sale). Falta: ledger vs. reporte del procesador, ledger vs. estado de cuenta STP, reporte de diferencias. |
| Migraciones | P2 | S | `docker-compose` solo monta la 001 en `initdb`; el runner aplica ambas, pero conviene una sola fuente y tabla de versiones. |

### F6 — Liquidación (payout) al comercio
**Existe:** `POST /api/v1/disbursements` (valida CLABE, verifica saldo, postea débito, llama STP), reconciliación CEP.

| Gap | Sev. | Esf. | Detalle / evidencia |
|---|---|---|---|
| Endpoint abierto y con CLABE arbitraria | P0 | S | Sin auth y con `destination_clabe` libre en el body. La CLABE de liquidación debe ser la registrada y verificada en KYB (titularidad = RFC del comercio); cambios con 2FA. |
| STP real no cableado | P0 | S | `STP_BASE_URL` se lee pero nunca se usa; el servidor siempre instancia `MockSTPClient` y `MockCEPProvider`. |
| Payout automático programado | P1 | M | Barrido diario/T+0 del saldo disponible (neto de apartado fiscal) en vez de dispersión manual por API. |
| Devoluciones SPEI y estados finales | P1 | M | Manejar órdenes devueltas/rechazadas por STP (webhook de estado), revertir asiento y reintentar. |
| Fondos de terceros | P1 | — | Definir con asesoría legal dónde y bajo qué figura se custodia el saldo del comercio entre cobro y payout. |

### F7 — Apartado fiscal RESICO / IVA
**Existe:** `CalculateResicoTaxSplit` con tramos y retención PM; split en ledger; presentador y exportador CSV en Android.

| Gap | Sev. | Esf. | Detalle / evidencia |
|---|---|---|---|
| Acumulado mensual en memoria | P0 | S | `monthlySalesCents` es un mapa; existe repo SQL (`AddMonthlySalesCents`) pero el handler usa el mapa. Al reiniciar se pierde el tramo. |
| Validación fiscal por contador | P0 | S | Revisar con un contador: base de ISR RESICO (ingresos efectivamente cobrados), tratamiento de IVA trasladado vs. acreditable, retenciones de PM, cambio de tramo intra-mes, tope anual. Documentar supuestos. |
| ¿Qué pasa con el dinero apartado? | P0 | M | Hoy es una subcuenta contable. Definir si se retiene en cuenta segregada, cuándo se libera y cómo el comercio paga al SAT (¿Kerno solo aparta o también paga?). Implica F6 y temas legales. |
| Comercios no RESICO | P1 | M | Régimen general PF/PM sin soporte; al menos bloquearlos explícitamente en el alta o modo "sin apartado". |
| `ISRRate` como `float64` en la API | P2 | S | Contradice la regla de no-floats (`fiscal/resico.go:12`, `handlers_payments.go:28`); exponer `isr_rate_bps`. |
| Reporte mensual para declaración | P1 | S | Resumen de ingresos, IVA, ISR estimado por mes. El CSV de Android es local; debe salir del backend. |

### F8 — Facturación CFDI 4.0
**Existe:** builder XML, cadena original, sello RSA-SHA256, factura global diaria, orquestador multi-PAC con failover, bóveda CSD.

| Gap | Sev. | Esf. | Detalle / evidencia |
|---|---|---|---|
| Sin PAC real | P0 | M | Solo `MockPACClient`. Contratar PAC y construir cliente real (sandbox → prod), manejo de códigos de error SAT. |
| Cadena original manual | P0 | M | `BuildCadenaOriginal` arma el string a mano; el SAT la define vía XSLT oficial. Riesgo alto de sellos inválidos en casos borde. Validar contra el XSLT del SAT o usar el servicio del PAC; probar con el validador del SAT. |
| Carga real de CSD por el comercio | P0 | S | Backend para subir `.cer/.key` + contraseña, validar vigencia/RFC y guardarlo cifrado con llave de KMS (ver F2). Hoy solo se siembra un CSD autofirmado de demo. |
| Datos del receptor | P1 | S | Captura de RFC, nombre, régimen fiscal, CP y uso CFDI del cliente final; autofactura desde ticket (portal o QR). |
| Cancelación CFDI | P1 | M | No existe (motivos 01–04, folio de sustitución, aceptación del receptor). |
| Representación impresa (PDF) y envío | P1 | S | PDF con QR SAT y envío por email/WhatsApp. |
| Factura global automática | P1 | S | Generar periódicamente la global de ventas a público en general que no se facturaron individualmente. |
| Catálogos SAT | P2 | S | Clave de producto/unidad por comercio, actualizables. |

### F9 — App Android del comercio
**Existe:** lógica de presentación pura (ViewModel de terminal, presentadores de bóveda fiscal, onboarding y RBF; descriptores de pantallas y navegación), cliente API con idempotencia, generador QR.

| Gap | Sev. | Esf. | Detalle / evidencia |
|---|---|---|---|
| No es una app Android | P0 | M | `app/build.gradle.kts` aplica `kotlin.jvm`, no `com.android.application`. El manifest está vacío (sin Activity). Migrar a módulo Android (AGP 8.x ya está en el catálogo), con `MainActivity`, Compose, DI, navegación. Sin `gradlew` en el repo. |
| Sin UI real | P0 | L | Pantallas Compose: login/OTP, onboarding KYB, cobrar (teclado de monto → tap / QR SPEI), historial de ventas, detalle y devolución, saldo y apartado fiscal, facturar, perfil/CLABE. |
| Integrar SDK del procesador | P0 | M | Tap to Pay del proveedor elegido (camino A), incluidos sus requisitos de dispositivo, atestación y permisos. |
| Play Integrity real | P1 | S | El cliente es lógica pura con interfaz; falta el SDK real de Google y el endpoint de nonce (F2). |
| Almacenamiento seguro de sesión | P1 | S | Tokens en Android Keystore / EncryptedSharedPreferences. La SAF usa una llave AES en memoria, no Keystore (fase 2 de todos modos). |
| Firma, distribución y updates | P1 | S | Keystore de firma, Play Console (track interno/cerrado), política de versión mínima forzada. |
| Telemetría y crash reporting | P1 | S | Crashlytics/Sentry sin datos sensibles. |

### F10 — Devoluciones, cancelaciones y comprobante
**Existe:** `POST /api/v1/payments/emv/reverse` (void/refund) con asiento de reversa.

| Gap | Sev. | Esf. | Detalle / evidencia |
|---|---|---|---|
| Devolución vía procesador | P0 | S | Con camino A, el refund se solicita al procesador y se refleja al recibir su webhook. |
| Reversa de apartado fiscal y comisión | P1 | S | El asiento actual solo mueve disponible ↔ receivable; no revierte IVA/ISR apartado ni comisión, ni el acumulado mensual RESICO. |
| Comprobante de venta | P1 | S | No existe ticket/recibo (solo en la demo web). Enviar por SMS/WhatsApp/email con PAN enmascarado, autorización y enlace de autofactura. |
| Contracargos | P1 | S | Reflejar en ledger el aviso del procesador y notificar al comercio (el motor `disputes` puede reutilizarse en su parte contable). |

### F11 — Plataforma y operación
| Gap | Sev. | Esf. | Detalle / evidencia |
|---|---|---|---|
| CI/CD | P0 | S | No hay `.github/workflows`. Pipeline: `go vet`, `go test -race`, tests Kotlin, lint, build de imagen y APK. La regla de "push automático a main" desde un agente local debería sustituirse por PR + CI obligatoria. |
| Toolchain Go | P1 | S | `go 1.27.1` en `go.mod` obliga a ese toolchain exacto; fijarlo en CI y Dockerfile. |
| Gestión de secretos | P0 | S | `docker-compose.yml` lleva secretos y llave maestra de ejemplo en texto plano. Usar KMS/Secrets Manager; el guard debe cubrir todas las llaves usadas realmente. |
| Infraestructura de despliegue | P0 | M | Postgres administrado con backups/PITR, Redis administrado, TLS, dominio, entornos dev/staging/prod. |
| Observabilidad | P1 | S | Logs estructurados existen; faltan métricas, trazas, alertas (fallos de webhook, descuadres de conciliación, payouts atascados). |
| Backoffice interno | P1 | M | Revisar altas KYB, buscar pagos, ver ledger por comercio, reintentar payouts/timbrados, bloquear comercio. |
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

1. **Cadena de fraude abierta:** `settle` sin auth acredita montos arbitrarios aunque el pago no exista + `disbursements` sin auth con CLABE libre. Con STP real conectado, esto permite sacar dinero. → Montar JWT, derivar tenant del token, eliminar montos del body, CLABE de destino fija y verificada.
2. **El servidor no arranca con base de datos** (driver faltante). Todo lo "persistente" nunca se ha ejecutado contra Postgres real.
3. **La bóveda CSD usa una llave maestra hardcodeada** e ignora la variable de entorno que el config guard valida.
4. **La app Android no es una app**: hay que migrar el módulo a Android y construir toda la UI.
5. **No hay ninguna integración real con terceros** (procesador, PAC, STP, CEP): todo pasa por mocks.

---

## 7. Plan sugerido hacia el MVP

**Fase 0 — Decisiones y contratos (en paralelo, 2–4 semanas)**
- Elegir procesador con Tap to Pay on Android en MX y modelo comercial (agregador/sub-afiliado).
- Contratos/sandboxes: procesador, STP, PAC, proveedor KYC.
- Validación fiscal del modelo RESICO y del apartado; asesoría legal sobre custodia de fondos y PLD.

**Fase 1 — Cimientos seguros (≈3–4 semanas)**
- Driver Postgres + tests de integración reales; todo el estado a DB; outbox worker activo.
- Usuarios, login OTP, JWT montado, tenant desde token, webhooks firmados, KMS para la bóveda.
- Corregir `settle`/`disbursements`/refund (montos desde el servidor, transacción única pago+asiento).
- CI con `go test -race`, tests Kotlin y build de imagen; secretos fuera del repo.

**Fase 2 — Rieles de cobro y payout (≈4–6 semanas)**
- Integración procesador (backend + webhooks) y STP real (abonos, órdenes, CEP, devoluciones).
- Payout automático diario; conciliador real (procesador y STP vs. ledger).

**Fase 3 — App y fiscal (≈5–7 semanas, en paralelo con fase 2)**
- Módulo Android real con Compose + SDK Tap to Pay del procesador + pantallas MVP.
- PAC real, carga de CSD, cadena original validada, cancelación, PDF, factura global automática, comprobante de venta.

**Fase 4 — Piloto controlado (≈2–3 semanas)**
- Backoffice mínimo, alertas, runbooks; 5–20 comercios piloto con límites de monto y payout supervisado.

> Con 2–3 desarrolladores, el rango realista hasta un piloto con dinero real es de **3 a 5 meses**, condicionado sobre todo por los tiempos de contratación y certificación de procesador, STP y PAC.

---

## 8. Qué reutilizar tal cual y qué congelar

| Reutilizar en MVP | Congelar para fase 2 (adquirencia propia / producto financiero) |
|---|---|
| `ledger`, `store` (tras agregar driver), idempotencia, outbox | `iso8583`, `crypto` (DUKPT, PIN translation, HSM) |
| `stp` (CLABE, firma, cliente, CEP) | `clearing` (STEC, MIF) |
| `fiscal` (con validación contable y XSLT), `pac` (interfaz + failover) | `rbf` y endpoints RBF |
| `identity` (RFC/CURP, CSF, 69-B, bóveda CSD con KMS) | `IsoDepEmvReader`, `ScrambledKeypadModel`, `PinBlockFormatter`, `CvmRulesEvaluator` (las CVM las resuelve el SDK del procesador) |
| `middleware_auth`, `play_integrity`, `config_guard` | `StoreAndForwardQueue` (offline) |
| Android: `MoneySplitCalculator`, `PosTerminalViewModel`, `SoftPosApiClient`, `TaxVaultPresenter`, `KybOnboardingPresenter`, `CodiQrGenerator`, `WindowSecurityGuard` | `disputes` (solo reutilizar la parte de asientos contables de contracargo) |

---

## 9. Observaciones sobre el proceso de desarrollo

- El loop autónomo produce módulos pequeños, aislados y con tests verdes, pero **optimiza tareas, no flujos**: ninguna tarea del backlog exigía "un cobro real llega al ledger persistente y se liquida". Recomiendo que el siguiente backlog se defina por **flujos E2E con criterio de aceptación de integración** (p. ej. "abono SPEI en sandbox STP → saldo en Postgres → payout → CEP conciliado").
- Los documentos (`HANDOFF.md`, `README`, `tech_roadmap.html`) describen el sistema como "producción bancaria" y "cierre MPoC"; el estado real es de prototipo. Conviene alinear la documentación para no generar expectativas erróneas con socios o inversionistas.
- El push automático a `main` sin revisión humana es riesgoso en un sistema que moverá dinero; migrar a PR + CI + revisión.
