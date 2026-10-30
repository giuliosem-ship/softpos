# SoftPOS Hybrid Fiscal & Payment Platform (Mexico)

Plataforma híbrida de pagos y automatización fiscal para comercios en México:
- **Frontend / Punto de Cobro:** Tap-to-Phone (NFC / SoftPOS) y QR Dinámico (SPEI).
- **Core Ledger:** Motor contable de partida doble inmutable con splits automáticos en T+0.
- **Bóveda Fiscal:** Cálculo y apartado automático de ISR (1.0% a 2.5% RESICO) e IVA (16%), y timbrado CFDI 4.0.
- **Financiamiento RBF:** Revenue-Based Financing con amortización automática vía deducciones porcentuales de ventas diarias.
- **STP Concentradora:** Cuentas CLABE virtuales de 18 dígitos y conciliación inmediata con CEP de Banxico.

---

## 🤖 Engineering Autonomous Loop (Multi-Model TDD)

Este repositorio cuenta con un orquestador de desarrollo autónomo multi-modelo que coordina tres IAs especializadas:

| Rol | Agente / Modelo | Responsabilidad |
| :--- | :--- | :--- |
| **Architect & Orchestrator** | **Antigravity (Gemini 3.8 Flash)** | Define especificaciones, contratos de tipos, arquitectura regulatoria y escribe suites de tests TDD (Fase RED). |
| **The Coder** | **Qwen 2.5 Coder 14B** *(vía LM Studio)* | Implementa el código para que todos los tests pasen (Fase GREEN). Cuenta con auto-healing en caso de errores en tests. |
| **The Auditor & Critic** | **Gemma 4** *(vía LM Studio)* | Audita la solución: busca condiciones de carrera, precisión numérica (evitar floats en dinero), edge cases del SAT y limpieza de código. |

El orquestador gestiona el **hot-swapping en caliente** en LM Studio (`lms load` / `lms unload`) en **~5-8 segundos**, garantizando que tu Apple M4 Pro nunca supere el límite de memoria unificada.

---

## 🚀 Comandos del Loop

El estado del backlog y las tareas se gestionan en [`tools/loop/tasks.json`](file:///Users/giuliosemino/fin_pltfm/SoftPOS/tools/loop/tasks.json).

```bash
# 1. Ver el estado del backlog y tareas completadas
python3 tools/loop/loop_runner.py --status

# 2. Ejecutar la siguiente tarea pendiente del backlog
python3 tools/loop/loop_runner.py --step

# 3. Ejecutar una tarea específica por ID
python3 tools/loop/loop_runner.py --task TASK-001

# 4. Cambiar manualmente de modelo en LM Studio
python3 tools/loop/loop_runner.py --swap qwen
python3 tools/loop/loop_runner.py --swap gemma

# 5. Ejecutar la suite completa de pruebas unitarias
PYTHONPATH=. python3 -m unittest discover -s tests -p "test_*.py"
```

---

## 📁 Estado Actual de la Base de Código

1. **`internal/` & `pkg/`**: Motores de dominio de partida doble (`Money`, `Account`, `JournalEntry`), generador/validador de CLABEs STP, cálculo de impuestos RESICO Persona Física (Art. 113-E y 113-J LISR), bóveda de certificados CSD con cifrado envolvente AES-GCM, validador SAT 69-B y timbrado multi-PAC.
2. **`pkg/store/` & `migrations/`**: Capa de persistencia relacional PostgreSQL con bloqueo pesimista a nivel de fila (`SELECT ... FOR UPDATE`), ejecutor transaccional de migraciones y cliente Redis RESP para caché de idempotencia.
3. **`cmd/server/main.go`**: Punto de entrada ejecutable que carga variables de entorno, conecta bases de datos, corre migraciones, siembra cuentas del ledger y comercio demo, y expone el servidor HTTP con apagado elegante (*graceful shutdown*).
4. **`web/pos/`**: Frontend y SDK móvil para Punto de Cobro con lectura Tap-to-Phone (NFC EMV L2 contactless), generación y confirmación de QR dinámico CoDi/SPEI, desglose fiscal RESICO en tiempo real y timbrado CFDI 4.0.

---

## 🚀 Ejecución del Servidor y Punto de Cobro

### 1. Ejecución Local (Go)
```bash
# Compilar y arrancar el servidor en modo desarrollo
go run ./cmd/server

# Abrir el Punto de Cobro en el navegador
open http://localhost:8080/pos/
```

### 2. Infraestructura con Docker Compose (PostgreSQL + Redis + Server)
```bash
docker compose up --build
```

### 3. Validación y Pruebas
```bash
# Pruebas Go completas con detector de condiciones de carrera (-race)
go test -v -race ./...

# Pruebas del SDK JavaScript del Punto de Cobro (EMV TLV, Web NFC, CoDi)
node --test web/pos/softpos_sdk.test.js

# Pruebas unitarias de Python
PYTHONPATH=. python3 -m unittest discover -s tests -p "test_*.py"
```

