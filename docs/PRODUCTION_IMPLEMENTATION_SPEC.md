# Especificación Técnica de Producción: SoftPOS Hybrid Fiscal & Payment Platform (México)

**Documento:** `docs/PRODUCTION_IMPLEMENTATION_SPEC.md`  
**Rol:** Technical Lead — Infraestructura de Pagos FinTech & Adquirencia  
**Versión:** 2.0.0 (Producción Bancaria / PCI MPoC / Banxico / SAT)

---

## 1. Definición de Negocio y Reglas CVM en México

### 1.1 Umbrales Contactless y Cardholder Verification Method (CVM)
* **Límite No-CVM Actual:** En México, el límite sin PIN ni firma para tarjetas físicas sin contacto (*Contactless*) está fijado en **$1,000.00 MXN** (100,000 centavos) como estándar general por la Asociación de Bancos de México (ABM), Visa, Mastercard, Carnet, PROSA y E-Global (con ciertos emisores y giros hasta $1,200–$1,500 MXN).
* **Transacciones $\le \$1,000.00$ MXN:** Pasan de forma inmediata con un solo "tap" sin solicitar verificación al tarjetahabiente (`CvmResult.NoCvmRequired`).
* **Transacciones $> \$1,000.00$ MXN con Tarjeta Física:** Requieren verificación CVM. Dado que un teléfono celular no cuenta con ranura para chip de contacto, la transacción exige **PIN-on-Glass (PCI MPoC SCRP)** en la pantalla del dispositivo.
* **Excepción CDCVM (Apple Pay / Google Pay):** Cuando el cobro se realiza con un teléfono o reloj inteligente, opera el método **Consumer Device CVM (CDCVM)** verificado en el dispositivo del comprador vía Face ID, Touch ID o biometría. **Estas transacciones NO tienen límite de $1,000 MXN** y pueden procesarse por cualquier monto sin solicitar PIN en la terminal del comercio.

### 1.2 Alcance de Plataformas
* **Android (Kotlin 2.0 + Jetpack Compose):** **Foco 100% de la fase actual.** Cubre más del 82% de las terminales en el segmento PyME mexicano.
* **iOS (Apple Tap to Pay on iPhone):** **Congelado.** No se invertirá esfuerzo en iOS por el momento hasta consolidar la certificación y despliegue masivo en Android.

---

## 2. Arquitectura de Batches para el Loop Autónomo (`tools/loop/`)

El desarrollo se organiza en lotes de 3 tareas con contratos TDD estrictos (**Fase RED ➔ GREEN ➔ AUDIT**):

```
┌────────────────────────────────────────────────────────┐
│ LOOP 1: Persistencia SQL & Outbox (COMPLETADO)         │
│ • TASK-GO-016: SQLRBFRepository con SELECT FOR UPDATE  │
│ • TASK-GO-017: SQLEMVPaymentRepository Transiciones    │
│ • TASK-GO-018: Transactional Outbox Pattern y Dispatch │
└───────────────────────────┬────────────────────────────┘
                            │
┌───────────────────────────▼────────────────────────────┐
│ LOOP 2: Seguridad Zero-Trust & Atestación (PENDIENTE)  │
│ • TASK-GO-019: Middleware JWT & Tenant Scoping         │
│ • TASK-GO-020: Validador Google Play Integrity         │
│ • TASK-GO-021: Fail-Fast Production Config Guard       │
└───────────────────────────┬────────────────────────────┘
                            │
┌───────────────────────────▼────────────────────────────┐
│ LOOP 3: Android SoftPOS & CVM Rules (PENDIENTE)        │
│ • AND-17: CvmRulesEvaluator ($1,000 MXN vs CDCVM)      │
│ • AND-18: ScrambledKeypadModel (PCI SCRP Desordenado)  │
│ • AND-19: PinBlockFormatter (ISO 9564 Formato 0)       │
└────────────────────────────────────────────────────────┘
```

---

## 3. Especificación de Componentes por Archivo

### Batch 1: Backend Zero-Trust Security & Attestation (Go)
1. **`TASK-GO-019` — `pkg/api/middleware_auth.go`:**
   - Extrae el token `Bearer <jwt>` del encabezado `Authorization`.
   - Valida firma criptográfica y expiración.
   - Extrae el `merchant_id` del token e inyecta en el `context.Context`.
   - Si la ruta contiene un parámetro de comercio (`/api/v1/merchants/{id}/...`), verifica que coincida con el token (*Anti-IDOR*), devolviendo `403 Forbidden` en caso de discrepancia.
2. **`TASK-GO-020` — `pkg/identity/play_integrity.go`:**
   - Valida el token criptográfico emitido por Google Play Integrity API.
   - Verifica los veredictos de integridad del dispositivo (`MEETS_DEVICE_INTEGRITY`).
   - Rechaza transacciones originadas en dispositivos con root, bootloader desbloqueado o ganchos de memoria (*hooks* Frida/Xposed).
3. **`TASK-GO-021` — `cmd/server/config_guard.go`:**
   - Valida variables de entorno durante el inicio del servidor (`BuildServer`).
   - Si `ENV=production` y se detectan claves por defecto (`defaultVaultMasterKeyHex` o `softpos_dev_secret_key_12345678`), aborta inmediatamente la ejecución con `log/slog.Error` y código de salida 1.

### Batch 2: Android SoftPOS & Lógica CVM (Kotlin)
1. **`AND-17` — `mobile/android/app/src/main/java/mx/softpos/app/nfc/CvmRulesEvaluator.kt`:**
   - `fun evaluate(amountCents: Long, emvTags: Map<String, String>): CvmResult`
   - Si `amountCents <= 100_000L` ➔ `CvmResult.NoCvmRequired`.
   - Si `amountCents > 100_000L` y tag `9F34` indica CDCVM ➔ `CvmResult.CdcvmVerified`.
   - Si `amountCents > 100_000L` con tarjeta física ➔ `CvmResult.PinRequired`.
2. **`AND-18` — `mobile/android/app/src/main/java/mx/softpos/app/ui/tap/ScrambledKeypadModel.kt`:**
   - Genera permutación pseudoaleatoria criptográficamente segura de los dígitos `0..9` para la cuadrícula 3x4 del teclado de PIN en pantalla.
   - Cada toque emite solo el índice de la coordenada, nunca el dígito en claro en logs ni vistas.
3. **`AND-19` — `mobile/android/app/src/main/java/mx/softpos/app/core/security/PinBlockFormatter.kt`:**
   - Construye el bloque de PIN de acuerdo con el estándar bancario **ISO 9564 Formato 0**:
     - `Plaintext PIN Block = (0 || PIN_Length || PIN || Pad_F)`
     - `PAN Block = (0000 || 12_Rightmost_PAN_Digits_Excluding_Check_Digit)`
     - `Formatted Block = Plaintext_PIN_Block XOR PAN_Block`
   - Garantiza la limpieza obligatoria de arreglos de memoria con `sensitiveBuffer.fill(0)` en bloques `finally`.

---

## 4. Instrucciones Operativas y Regla Obligatoria de Validación y Push

Para ejecutar el siguiente lote de tareas mediante el orquestador:
```zsh
# 1. Consultar estado del backlog
python3 tools/loop/loop_runner.py --status

# 2. Ejecutar las 3 tareas del siguiente batch
python3 tools/loop/loop_runner.py --batch 3
```

### Regla de Cierre de Loop (Mandatoria para Agentes y Desarrolladores):
1. **Commits Atómicos:** Cada tarea completada debe tener su commit individual en Git.
2. **Validación Completa (Zero Issues):** Al finalizar todas las tareas del loop o batch, se ejecuta automáticamente la validación integral:
   - Backend Go: `go test -v ./...`
   - Web POS SDK: `node --test web/pos/softpos_sdk.test.js`
3. **Push Automático a `origin main`:**
   - Si y solo si todas las pruebas pasan limpiamente (cero fallos), se realiza `git push origin main`.
   - Si alguna prueba falla, el push queda estrictamente bloqueado.
4. **Handoff:** Actualizar `tools/loop/HANDOFF.md` con el estado final y el commit pushado.
