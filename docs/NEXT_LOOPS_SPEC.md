# Especificación Técnica para Próximos Loops: SoftPOS Tier-1

**Documento:** `docs/NEXT_LOOPS_SPEC.md`  
**Objetivo:** Guía exhaustiva y ejecutable para que nuevos agentes/sesiones implementen los Loops 5, 6 y 7 en ciclos independientes **RED ➔ GREEN ➔ AUDIT**.

---

## 1. Estado Actual del Repositorio (`origin/main`)
* **Total de Tareas Completadas:** 52 / 52 en `tools/loop/tasks.json`.
* **Último Commit en Main:** `6bbba76` (`feat(iso8583): implement acquirer message engine, DE 55 BER-TLV, and ARQC/ARPC crypto [TASK-GO-022..024]`).
* **Suites Validadas:**
  * Go Backend: `go test -v ./...` (0 fallos).
  * Web POS SDK: `node --test web/pos/softpos_sdk.test.js` (7/7 tests pasan).

---

## 2. Reglas Mandatorias para Cualquier Agente / Loop
1. **Aritmética Financiera:** Estrictamente enteros de 64 bits (`int64` en Go, `Long` en Kotlin). Prohibido el uso de flotantes (`float32`/`float64`/`Double`/`Float`).
2. **CVM México Contactless:**
   * $\le \$1,000.00$ MXN: `NoCvmRequired`.
   * $> \$1,000.00$ MXN con tarjeta física: `PinRequired` (PIN-on-Glass con teclado aleatorizado).
   * $> \$1,000.00$ MXN con digital wallet (Apple/Google Pay): `CdcvmVerified` (biometría de comprador, sin tope).
3. **PCI Sanitización de Memoria:** Limpieza obligatoria de búferes de PIN/tarjeta (`sensitive.fill(0)`) en bloques `finally`.
4. **Ciclo TDD Obligatorio:**
   * **Fase RED:** Escribir primero el test unitario en el archivo correspondiente.
   * **Fase GREEN:** Desarrollar el código de producción que satisfaga el test.
   * **Fase AUDIT:** Revisar concurrencia, memory leaks y apego regulatorio.
5. **Cierre de Loop y Push:**
   * Commit atómico con formato convencional (ej. `feat(<comp>): <desc> [TASK-ID]`).
   * Ejecutar suite completa (`go test ./...` y `node --test ...`).
   * Si cero errores: `git push origin main`.
   * Actualizar `tools/loop/HANDOFF.md`.

---

## 3. Desglose Detallado de los Siguientes Loops

```
┌────────────────────────────────────────────────────────────────────────┐
│ LOOP 5: Criptografía Bancaria HSM & DUKPT (ANSI X9.24)                │
│ • TASK-GO-025: Motor Derivador DUKPT (BDK ➔ IPEK ➔ PEK con KSN)       │
│ • TASK-GO-026: Servicio de Traducción de PIN (PEK ➔ ZPK de Switch)     │
│ • TASK-GO-027: Abstracción de Cliente Payment HSM (Thales/Futurex/KMS) │
└───────────────────────────────────┬────────────────────────────────────┘
                                    │
┌───────────────────────────────────▼────────────────────────────────────┐
│ LOOP 6: Compensación, Lotes STEC & Controversias (Clearing & Disputes) │
│ • TASK-GO-028: Generador de Lotes STEC/TEF para PROSA / E-Global       │
│ • TASK-GO-029: Calculadora de Tasa de Intercambio (MIF Banxico / ABM)  │
│ • TASK-GO-030: Motor de Contracargos y Reserva Dinámica (Escrow Risk)  │
└───────────────────────────────────┬────────────────────────────────────┘
                                    │
┌───────────────────────────────────▼────────────────────────────────────┐
│ LOOP 7: Blindaje Mobile PCI MPoC & Handshake (Android Kotlin)          │
│ • AND-20: Android Window Security Guard (FLAG_SECURE & Anti-Keylogger) │
│ • AND-21: Handshake Cliente Play Integrity (Nonce + Token en EMV)      │
│ • AND-22: Cola Offline Store-and-Forward (SAF) Cifrada                 │
└────────────────────────────────────────────────────────────────────────┘
```

---

### LOOP 5: Criptografía Bancaria HSM & DUKPT (ANSI X9.24)

#### Tarea 1: `TASK-GO-025` — Motor Derivador DUKPT en Go
* **Archivos:**
  * Test: `pkg/crypto/dukpt_test.go`
  * Impl: `pkg/crypto/dukpt.go`
* **Especificación:**
  * Implementar derivación de llaves por transacción según **ANSI X9.24-1**:
    1. Derivar **IPEK** (Initial PIN Encryption Key) a partir de una BDK (Base Derivation Key) de 16 bytes y los 8 bytes iniciales del KSN (Key Serial Number, 10 bytes / 20 caracteres hex).
    2. Calcular la llave de sesión de transacción (**PEK**) a partir del IPEK y el contador de transacciones de 21 bits extraído de los bytes 7 a 9 del KSN.
    3. Descifrar el bloque de PIN ISO 9564 Formato 0 utilizando Triple-DES en modo ECB/CBC con la PEK obtenida.
* **Casos de Test Obligatorios:**
  * Vector oficial ANSI X9.24 con BDK conocida, KSN y resultado exacto de IPEK/PEK.
  * Incremento de contador de transacciones en KSN derivando llaves PEK distintas.
  * Rechazo de KSNs inválidos (< 10 bytes o caracteres no hex).

#### Tarea 2: `TASK-GO-026` — Servicio de Traducción de PIN (PIN Translation)
* **Archivos:**
  * Test: `pkg/crypto/pin_translation_test.go`
  * Impl: `pkg/crypto/pin_translation.go`
* **Especificación:**
  * Implementar el servicio `TranslatePinBlock(incomingBlock []byte, ksn []byte, bdk []byte, zpk []byte, pan string) ([]byte, error)`.
  * Flujo seguro:
    1. Deriva la PEK de la terminal con `TASK-GO-025`.
    2. Descifra el bloque de PIN entrante bajo la PEK.
    3. Re-cifra el bloque de PIN resultante bajo la **ZPK** (Zone PIN Key) del Switch Bancario (PROSA/E-Global).
    4. Limpia los búferes temporales en memoria (`for i := range temp { temp[i] = 0 }`) en bloques `defer`.
* **Casos de Test Obligatorios:**
  * Traducción correcta: bloque cifrado con ZPK puede ser descifrado por el mock del switch bancario produciendo el mismo PIN original.
  * Manejo seguro en caso de error: cualquier falla limpia la memoria inmediatamente.

#### Tarea 3: `TASK-GO-027` — Abstracción e Interfaz de Cliente Payment HSM
* **Archivos:**
  * Test: `pkg/crypto/hsm_client_test.go`
  * Impl: `pkg/crypto/hsm_client.go`
* **Especificación:**
  * Diseñar la interfaz desacoplada:
    ```go
    type PaymentHSM interface {
        GenerateZPK() ([]byte, error)
        TranslatePIN(ctx context.Context, req PINTranslationRequest) (*PINTranslationResponse, error)
        VerifyARQC(ctx context.Context, req ARQCVerifyRequest) (bool, error)
        GenerateARPC(ctx context.Context, req ARPCGenerateRequest) ([]byte, error)
    }
    ```
  * Implementar un cliente mock/simulador para entornos de testing y desarrollo local (`SoftwareHSMSimulator`), con soporte para extensiones hacia Thales payShield 10K y AWS CloudHSM.
* **Casos de Test Obligatorios:**
  * Llamadas concurrentes al simulador HSM bajo múltiples goroutines sin condiciones de carrera.
  * Verificación de comandos y códigos de respuesta HSM.

---

### LOOP 6: Compensación, Lotes STEC & Controversias (Clearing & Disputes)

#### Tarea 1: `TASK-GO-028` — Generador de Lotes de Compensación STEC / TEF (PROSA/E-Global)
* **Archivos:**
  * Test: `pkg/clearing/stec_test.go`
  * Impl: `pkg/clearing/stec.go`
* **Especificación:**
  * Formato plano de intercambio adquirente para cámaras de compensación en México:
    * **Header:** Identificador de adquirente, fecha de corte, número de lote secuencial.
    * **Detail Records:** Transacciones aprobadas del día (Visa, Mastercard, Carnet), con PAN enmascarado/cifrado, importe en centavos, código de autorización, número de afiliación (MID).
    * **Trailer:** Conteo total de transacciones, suma acumulada en centavos de débitos y créditos, hashes de integridad.
* **Casos de Test Obligatorios:**
  * Generación de archivo con 100 transacciones: sumas de cuadre en el trailer coinciden exactamente con la suma de los detalles.
  * Formato de longitud fija por línea (sin desfasamiento de columnas).

#### Tarea 2: `TASK-GO-029` — Calculadora de Tasa de Intercambio (MIF Banxico / ABM)
* **Archivos:**
  * Test: `pkg/clearing/interchange_test.go`
  * Impl: `pkg/clearing/interchange.go`
* **Especificación:**
  * Reglas de comisiones adquirente vs emisor según la regulación de Banco de México:
    * Débito: Tasa tope regulada (bps) según giro/MCC (ej. gasolineras, autoservicio, educación vs general).
    * Crédito: Tasa por segmento de tarjeta (Clásica, Oro, Platino, Corporativa).
    * Cálculo de IVA (16%) sobre la comisión en centavos con redondeo half-up.
* **Casos de Test Obligatorios:**
  * Transacciones de débito vs crédito en centavos comprobando que el desglose de adquirencia, intercambio e IVA cuadre al centavo exacto con el libro mayor.

#### Tarea 3: `TASK-GO-030` — Motor de Contracargos y Reserva Dinámica (*Rolling Reserve*)
* **Archivos:**
  * Test: `pkg/disputes/disputes_test.go`
  * Impl: `pkg/disputes/disputes.go`
* **Especificación:**
  * Ciclo de vida de controversias: `CHARGEBACK_OPENED`, `DOCS_REQUIRED`, `REPRESENTMENT_SUBMITTED`, `WON`, `LOST`.
  * Integración con Ledger: Al abrirse un contracargo, congela automáticamente los fondos del comercio debitando `LIABILITY:MERCHANT:AVAILABLE` y acreditando `LIABILITY:MERCHANT:DISPUTE_ESCROW`.
  * Reserva Dinámica: Retención porcentual (ej. 500 bps = 5%) por $N$ días para comercios de alto riesgo.
* **Casos de Test Obligatorios:**
  * Apertura de contracargo: el saldo disponible se bloquea inmediatamente sin alterar el balance patrimonial global.
  * Resolución favorable (WON): los fondos regresan al saldo disponible.
  * Resolución adversa (LOST): los fondos se liquidan contra la cuenta del banco emisor.

---

### LOOP 7: Blindaje Mobile PCI MPoC & Handshake (Android Kotlin)

#### Tarea 1: `AND-20` — Window Security Guard & Detección de Accesibilidad
* **Archivos:**
  * Test: `mobile/android/app/src/test/java/mx/softpos/app/ui/security/WindowSecurityGuardTest.kt`
  * Impl: `mobile/android/app/src/main/java/mx/softpos/app/ui/security/WindowSecurityGuard.kt`
* **Especificación:**
  * Envoltorio para pantallas de PIN-on-Glass:
    * Activa `WindowManager.LayoutParams.FLAG_SECURE` para evitar screenshots, screen-recording y screencast.
    * Escanea `AccessibilityManager.getEnabledAccessibilityServiceList` y detecta herramientas de lectura de pantalla no autorizadas (mitigación de keyloggers por software).
* **Casos de Test:**
  * Detección de servicios de accesibilidad sospechosos bloqueando el despliegue del teclado PIN.
  * Limpieza de flags al cerrar la vista.

#### Tarea 2: `AND-21` — Handshake Cliente Play Integrity
* **Archivos:**
  * Test: `mobile/android/app/src/test/java/mx/softpos/app/core/network/PlayIntegrityClientTest.kt`
  * Impl: `mobile/android/app/src/main/java/mx/softpos/app/core/network/PlayIntegrityClient.kt`
* **Especificación:**
  * Protocolo cliente de atestación:
    1. Solicita un `nonce` criptográfico al backend Go `/api/v1/auth/attestation-nonce`.
    2. Construye el payload de atestación para la API de Google Play Integrity.
    3. Adjunta el token resultante en el encabezado `X-Play-Integrity-Token` al solicitar autorización de cobro EMV.
* **Casos de Test:**
  * Reintento automático con backoff exponencial si la llamada a Play Integrity falla por timeout.
  * Validación de frescura del nonce.

#### Tarea 3: `AND-22` — Cola Offline Store-and-Forward (SAF) Cifrada
* **Archivos:**
  * Test: `mobile/android/app/src/test/java/mx/softpos/app/domain/saf/StoreAndForwardQueueTest.kt`
  * Impl: `mobile/android/app/src/main/java/mx/softpos/app/domain/saf/StoreAndForwardQueue.kt`
* **Especificación:**
  * Transacciones offline para pagos $\le \$400.00$ MXN:
    * Almacena transacciones firmadas offline en base de datos local cifrada con SQLCipher / Android Keystore (AES-256-GCM).
    * Al detectar conectividad de red, despacha las transacciones en orden FIFO hacia el backend.
* **Casos de Test:**
  * Rechazo de transacciones $> \$400$ MXN en modo offline.
  * Despacho y vaciado ordenado de la cola al restablecer la conexión.

---

## 4. Instrucciones para Arrancar el Siguiente Loop en un Nuevo Contexto

1. **Agregar las 3 tareas del lote elegido a `tools/loop/tasks.json`:**
   * Si arrancas con el **Loop 5**: agregar `TASK-GO-025`, `TASK-GO-026`, `TASK-GO-027` con `status: "pending"`.
2. **Escribir las suites de prueba en la Fase RED.**
3. **Ejecutar el loop runner o la implementación guiada:**
   ```zsh
   python3 tools/loop/loop_runner.py --batch 3
   ```
4. **Validación completa pre-push obligatoria:**
   ```zsh
   go test -v ./...
   node --test web/pos/softpos_sdk.test.js
   git push origin main
   ```
