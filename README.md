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

1. **`internal/ledger/models.py`** ([`TASK-001`](file:///Users/giuliosemino/fin_pltfm/SoftPOS/internal/ledger/models.py)): Modelos de dominio inmutables de partida doble (`Money`, `Account`, `Entry`, `JournalEntry`, `Direction`, `AccountType`) con validación de balance débito = crédito en centavos enteros.
2. **`internal/ledger/engine.py`** ([`TASK-002`](file:///Users/giuliosemino/fin_pltfm/SoftPOS/internal/ledger/engine.py)): Motor contable in-memory con soporte de asientos atómicos, saldos normales y splits automáticos de cobros SoftPOS (`record_split_payment`).
3. **`internal/stp/clabe.py`** ([`TASK-003`](file:///Users/giuliosemino/fin_pltfm/SoftPOS/internal/stp/clabe.py)): Generador y validador de CLABEs interbancarias de 18 dígitos bajo banco STP (646) con algoritmo de dígito verificador módulo 10.
4. **`internal/fiscal/resico.py`** ([`TASK-004`](file:///Users/giuliosemino/fin_pltfm/SoftPOS/internal/fiscal/resico.py)): Calculadora oficial RESICO Persona Física (Art. 113-E y 113-J LISR) con segregación de IVA, retención PM del 1.25% y tasas escalonadas (1.0% a 2.5%).
