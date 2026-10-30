# Especificación Técnica y Toolkit macOS: App Nativa Android (Kotlin + Jetpack Compose)

**Ruta del documento:** `docs/ANDROID_NATIVE_SPEC.md`  
**Objetivo:** Definir la arquitectura 100% nativa (Opción B), los features de negocio y el toolkit de línea de comandos en macOS para que el orquestador autónomo (`tools/loop/loop_runner.py`) y subagentes puedan instalar dependencias, compilar, ejecutar pruebas unitarias/instrumentadas y construir el `.apk`/`.aab` sin depender de interfaz gráfica manual.

---

## 1. Toolkit Requerido en macOS (Headless CLI para Loops de IA)

Para que los loops de código (`Qwen 2.5 Coder` + `Gemma 4` + `Gemini`) puedan compilar y correr tests en milisegundos desde la terminal de macOS (Apple Silicon / Intel), se requiere instalar el toolchain de Android en modo **Command-Line Only**:

### 1.1 Instalación Automatizable vía Homebrew
```zsh
# 1. Instalar JDK 17 (Requerido por Android Gradle Plugin 8.x y Kotlin 2.0)
brew install --cask temurin@17

# 2. Instalar Android Command-Line Tools, Gradle y herramientas de análisis estático Kotlin
brew install --cask android-commandlinetools
brew install gradle kotlin ktlint detekt

# 3. (Opcional) Visor de pantalla de dispositivo físico por USB para debugging
brew install scrcpy
```

### 1.2 Variables de Entorno (`~/.zshrc`)
```zsh
export JAVA_HOME=$(/usr/libexec/java_home -v 17)
export ANDROID_HOME="/opt/homebrew/share/android-commandlinetools"
export ANDROID_SDK_ROOT="$ANDROID_HOME"
export PATH="$JAVA_HOME/bin:$ANDROID_HOME/cmdline-tools/latest/bin:$ANDROID_HOME/platform-tools:$ANDROID_HOME/emulator:$PATH"
```

### 1.3 Paquetes del SDK y Aceptación de Licencias (Sin UI)
```zsh
# Aceptar todas las licencias del SDK automáticamente
yes | sdkmanager --licenses

# Instalar plataforma Android 15 (API 35), Build-Tools y Emulador ARM64
sdkmanager \
  "platform-tools" \
  "platforms;android-35" \
  "build-tools;35.0.0" \
  "emulator" \
  "system-images;android-35;google_apis;arm64-v8a"
```

### 1.4 Extensión de `tools/loop/loop_runner.py` para Kotlin
Para que `loop_runner.py` ejecute el ciclo **RED -> GREEN -> AUDIT** sobre la app de Android igual que lo hace con Go:
* Detectar extensión `.kt`: `is_kotlin = test_path.endswith("Test.kt")`
* Comando de prueba rápida en JVM local (JUnit 5 + Coroutines Test, `< 2s`):
  ```zsh
  ./gradlew -p mobile/android :app:testDebugUnitTest --info
  ```
* Comando de auditoría estática para el paso de `Gemma 4`:
  ```zsh
  ktlint "mobile/android/app/src/**/*.kt" && detekt --input mobile/android/app/src
  ```

---

## 2. Arquitectura del Proyecto Android (`mobile/android/`)

Se utilizará **Clean Architecture + MVVM (Unidirectional Data Flow)** en un proyecto multimódulo o de paquetes desacoplados bajo `mobile/android/`:

```text
mobile/android/
├── build.gradle.kts
├── settings.gradle.kts
├── gradle.properties
└── app/
    ├── build.gradle.kts
    └── src/
        ├── main/
        │   ├── AndroidManifest.xml
        │   └── mx/softpos/app/
        │       ├── core/
        │       │   ├── money/          # Aritmética int64 (Long) en centavos y BasisPoints (cero Double/Float)
        │       │   ├── network/        # Ktor / Retrofit + Interceptor de Idempotency-Key (UUIDv4) y JWT
        │       │   └── security/       # PlayIntegrityAttestor, KeystoreManager, BiometricAuth
        │       ├── domain/
        │       │   ├── model/          # PaymentIntent, TaxSplitPreview, RBFLoanState, CfdiInvoice
        │       │   └── repository/     # Contratos de dominio puros (interfaces)
        │       ├── nfc/
        │       │   ├── TapPaymentProvider.kt        # Interfaz agnóstica del proveedor NFC
        │       │   ├── StripeTapPaymentProvider.kt  # Fase 1: Stripe Terminal Tap-to-Pay SDK
        │       │   ├── WhiteLabelTapProvider.kt     # Fase 2/3: Kushki / Phos / MineSec SDK
        │       │   └── IsoDepEmvReader.kt           # Lector nativo APDU ISO 14443-4 + BER-TLV (Sandbox/Lab)
        │       └── ui/
        │           ├── pos/            # Teclado de cobro, selector de método y calculadora Split en vivo
        │           ├── tap/            # Animación radar NFC, lectura contactless y estado ARQC
        │           ├── codi/           # Generador QR ISO-18004 (ZXing) y confirmación SPEI en tiempo real
        │           ├── cfdi/           # Escáner de QR de CSF del SAT (ML Kit) y timbrado CFDI 4.0 en 1 toque
        │           ├── vault/          # Dashboard de Bóveda Fiscal RESICO y estado de cuenta para el contador
        │           ├── rbf/            # Panel de préstamo activo, barra RepaymentCap y originación
        │           └── onboarding/     # Alta KYB: captura de CSF (PDF/QR) y subida de sellos CSD (.cer/.key)
        └── test/
            └── mx/softpos/app/         # Suite JUnit5 + MockK + Turbine para TDD en tools/loop/
```

### Stack de Librerías Android (Version Catalog `libs.versions.toml`)
* **Lenguaje:** Kotlin `2.0.21` (con `jvmToolchain(17)`)
* **UI:** Jetpack Compose (Material 3) + Navigation Compose
* **Inyección de Dependencias:** Hilt (`dagger.hilt.android`) o Koin
* **Red & Serialización:** Retrofit2 / OkHttp3 + `kotlinx-serialization-json`
* **Cámara & Escaneo QR Fiscal:** CameraX + Google ML Kit Barcode Scanning (para escanear la Constancia de Situación Fiscal del cliente al facturar)
* **Generación de QR CoDi/SPEI:** `com.google.zxing:core` (QR ISO/IEC 18004 real con corrección Reed-Solomon Nivel M)
* **Seguridad:** `androidx.security:security-crypto` (`EncryptedSharedPreferences`), `androidx.biometric` y `com.google.android.play:integrity`

---

## 3. Catálogo de Features de la App Nativa de Android

### Feature 1: Terminal de Cobro (`POS Screen`) con Pre-Split Fiscal en Vivo
* **Teclado numérico de centavos exactos (`Long`):** El comerciante digita el monto a cobrar sin errores de punto flotante.
* **Desglose T+0 instantáneo antes de cobrar:** Mientras escribe el monto (ej. `\$1,160.00 MXN`), la pantalla muestra en tiempo real cuánto irá a:
  1. **Saldo Disponible Líquido (`LIABILITY:AVAILABLE`)**
  2. **Bóveda Fiscal IVA 16% + ISR RESICO (`LIABILITY:TAX_ESCROW`)**
  3. **Abono a su Adelanto RBF (`ASSET:RBF_LOANS_RECEIVABLE`)**
  4. **Comisión según método elegido** (mostrando el ahorro si elige cobrar con QR SPEI/CoDi vs. Tarjeta).
* **Switch "Cliente es Persona Moral (Art. 113-J LISR)":** Activa el cálculo de retención del `1.25%` de ISR para cuando el comprador es una empresa que requiere factura con retención.

### Feature 2: Motor de Cobro sin Contacto Dual (`NFC Tap-to-Phone`)
* **Arquitectura de Adaptador Intercambiable (`TapPaymentProvider`):** Permite cambiar entre el SDK de **Stripe Terminal** (Fase 1), **Kushki/Blumon** (Fase 2) y **Phos/MineSec** (Fase 3) sin tocar ni una línea de las pantallas de UI.
* **Soporte de todos los métodos Contactless:**
  * Tarjetas físicas con chip sin contacto (**TDD Débito** y **TDC Crédito** Visa, Mastercard, Carnet y Amex).
  * Billeteras digitales con tokenización EMV (**Apple Pay**, **Google Pay**, **Samsung Pay**) validando `CDCVM` (biometría del comprador en su propio teléfono/reloj).
  * **Vales de Despensa NFC** sobre red Carnet (Edenred, Pluxee, Sí Vale).
* **Lector APDU Nativo (`IsoDepEmvReader.kt`) para Laboratorio/Sandbox:** Usa `NfcAdapter.enableReaderMode` con `FLAG_READER_NFC_A | FLAG_READER_NFC_B` para intercambiar comandos ISO 7816-4 (`SELECT PPSE`, `GPO`, `READ RECORD`), construir la trama BER-TLV (`9F02`, `5F2A`, `9F27`, `9F26`, `9F36`, `5A`, `5F24`) y enviarla a `POST /api/v1/payments/emv/authorize` y `POST /api/v1/payments/settle`.
* **Cumplimiento PCI:** Enmascaramiento inmediato de PAN (`415231******9012`) y limpieza explícita de arreglos de bytes (`ByteArray.fill(0)`) en bloques `finally`.

### Feature 3: Cobro QR Dinámico SPEI / CoDi + Confirmación Sonora
* Consume `POST /api/v1/payments/codi/qr` y renderiza un **código QR ISO/IEC 18004 real** de alto contraste en pantalla completa con temporizador de expiración.
* Escucha la confirmación del webhook de STP/Banxico y dispara confirmación visual, vibración háptica y voz (*"Pago recibido por mil ciento sesenta pesos"*), mostrando el enlace al **CEP oficial de Banxico**.

### Feature 4: Facturación CFDI 4.0 Instantánea con Escáner de Constancia SAT
* En México, el mayor dolor en caja al pedir factura es teclear el RFC, Razón Social, Código Postal y Régimen Fiscal del cliente.
* **Solución en la App:** Al terminar el cobro, el botón **"Timbrar Factura CFDI 4.0"** ofrece:
  1. **Factura Público en General (`XAXX010101000`)** en 1 toque.
  2. **Escanear QR de la Constancia del Cliente con la Cámara (ML Kit):** Lee la URL oficial del SAT impresa en la cédula fiscal del cliente, extrae su RFC, Régimen y CP automáticamente, e invoca `POST /api/v1/invoices/cfdi` (con failover Multi-PAC `<1.5s`).
  3. Compartir PDF/XML por **WhatsApp** o correo electrónico desde el `Intent.ACTION_SEND` nativo de Android.

### Feature 5: Bóveda Fiscal (`Tax Vault`) y Exportación para el Contador
* Muestra en tiempo real el saldo resguardado en `LIABILITY:MERCHANT:{id}:TAX_ESCROW` separado en **IVA Cobrado** e **ISR RESICO Mensual**.
* Indicador del escalón actual de tasa RESICO (`1.00%`, `1.10%`, `1.50%`, `2.00%` o `2.50%`) según las ventas acumuladas del mes (`GetMerchantMonthlySalesCents`).
* Botón **"Enviar Paquete Mensual a mi Contador"** que genera el resumen pre-declaración y el lote de CFDI emitidos.

### Feature 6: Módulo de Capital RBF (`Revenue-Based Financing`)
* Consume `GET /api/v1/merchants/{id}/rbf` y `POST /api/v1/rbf/loans`.
* Muestra una barra de progreso clara: **Monto Prestado (`Principal`)**, **Comisión Fija (`Fee`)**, **Tope Total (`RepaymentCap`)** y **Saldo Restante (`RemainingCents`)**.
* Desglose transparente de cada micro-abono retenido automáticamente en las ventas del día.

### Feature 7: Seguridad Bancaria y Onboarding KYB en 3 Minutos
* **Atestación Google Play Integrity:** Antes de habilitar el cobro NFC, la app solicita un token a Google Play Integrity API y lo envía al backend en Go para confirmar que el dispositivo no tiene *root*, *bootloader desbloqueado* ni *hooks* de memoria.
* **Protección Biométrica (`BiometricPrompt`):** Se exige huella o rostro del comerciante para solicitar retiros SPEI (`POST /api/v1/disbursements`), cargar nuevos sellos CSD o aceptar un contrato de crédito RBF.
* **Onboarding Self-Service:** Permite seleccionar el PDF de la Constancia de Situación Fiscal y los archivos `.cer` y `.key` desde el almacenamiento del teléfono para cifrarlos en el `CSDVaultService` (`AES-256-GCM`).

---

## 4. Backlog de Tareas para `tools/loop/tasks.json` (Android Loop)

Cuando quieras lanzar el loop autónomo para construir la app de Android paso a paso con TDD, estas son las 8 tareas secuenciales listas para agregarse a `tools/loop/tasks.json`:

| ID Tarea | Componente | Archivo de Implementación | Archivo de Test (TDD Red -> Green) | Descripción |
| :--- | :--- | :--- | :--- | :--- |
| **`AND-01`** | `android-core` | `mobile/android/app/src/main/java/mx/softpos/app/core/money/MoneySplitCalculator.kt` | `mobile/android/app/src/test/java/mx/softpos/app/core/money/MoneySplitCalculatorTest.kt` | Calculadora local en centavos (`Long`) y `bps` para pre-visualizar el Split (RESICO + IVA + MDR + RBF) en la UI sin floats. |
| **`AND-02`** | `android-nfc` | `mobile/android/app/src/main/java/mx/softpos/app/nfc/EmvTlvCodec.kt` | `mobile/android/app/src/test/java/mx/softpos/app/nfc/EmvTlvCodecTest.kt` | Codificador/Decodificador BER-TLV en Kotlin + enmascarador PCI de PAN y limpieza de memoria `ByteArray.fill(0)`. |
| **`AND-03`** | `android-codi` | `mobile/android/app/src/main/java/mx/softpos/app/ui/codi/CodiQrGenerator.kt` | `mobile/android/app/src/test/java/mx/softpos/app/ui/codi/CodiQrGeneratorTest.kt` | Generador de matriz QR ISO/IEC 18004 real (ZXing) a partir del payload JSON de Banxico CoDi y validador de expiración. |
| **`AND-04`** | `android-net` | `mobile/android/app/src/main/java/mx/softpos/app/core/network/SoftPosApiClient.kt` | `mobile/android/app/src/test/java/mx/softpos/app/core/network/SoftPosApiClientTest.kt` | Cliente HTTP con inyección automática de `Idempotency-Key`, token de Play Integrity y manejo de errores deterministas del SAT/PAC. |
| **`AND-05`** | `android-nfc` | `mobile/android/app/src/main/java/mx/softpos/app/nfc/TapPaymentOrchestrator.kt` | `mobile/android/app/src/test/java/mx/softpos/app/nfc/TapPaymentOrchestratorTest.kt` | Máquina de estados del cobro NFC (`IDLE -> READING_CARD -> AUTHORIZING -> SETTLING -> COMPLETED / REVERSED`) con soporte de adaptadores SDK. |
| **`AND-06`** | `android-cfdi` | `mobile/android/app/src/main/java/mx/softpos/app/ui/cfdi/SatConstanciaUrlParser.kt` | `mobile/android/app/src/test/java/mx/softpos/app/ui/cfdi/SatConstanciaUrlParserTest.kt` | Parser del QR de la Cédula de Identificación Fiscal / CSF del SAT para autocompletar datos del receptor CFDI 4.0 en 1 segundo. |
| **`AND-07`** | `android-vm` | `mobile/android/app/src/main/java/mx/softpos/app/ui/pos/PosTerminalViewModel.kt` | `mobile/android/app/src/test/java/mx/softpos/app/ui/pos/PosTerminalViewModelTest.kt` | `ViewModel` reactivo (`StateFlow`) que coordina el teclado de cobro, el cobro NFC/CoDi, el Split contable y el timbrado post-venta. |
| **`AND-08`** | `android-sec` | `mobile/android/app/src/main/java/mx/softpos/app/core/security/DeviceAttestationGuard.kt` | `mobile/android/app/src/test/java/mx/softpos/app/core/security/DeviceAttestationGuardTest.kt` | Guardián de seguridad que verifica Play Integrity, ausencia de root/debugger y estado del Keystore antes de activar la antena NFC. |
