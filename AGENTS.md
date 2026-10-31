# Antigravity & Multi-Agent Collaboration Rules (SoftPOS)

## 1. Loop Execution & TDD Lifecycle
- Cada tarea en el backlog (`tools/loop/tasks.json`) sigue el ciclo estricto: **RED ➔ GREEN ➔ AUDIT**.
- **Aritmética Financiera:** No se introducen números en punto flotante (`float32`/`float64`/`Double`/`Float`) para montos monetarios o centavos; se usan exclusivamente enteros de 64 bits (`int64` en Go, `Long` en Kotlin) y puntos base (`bps`) con aritmética half-up.
- **Sanitización Estricta de Memoria (PCI):** Búferes sensibles de tarjetas o PIN deben limpiarse explícitamente (`ByteArray.fill(0)`) en bloques `finally`.
- **Reglas CVM Contactless (México):**
  - $\le \$1,000.00$ MXN: Aprobación sin CVM (`NoCvmRequired`).
  - $> \$1,000.00$ MXN con tarjeta física: Requiere PIN-on-Glass con teclado aleatorizado (`PinRequired`).
  - $> \$1,000.00$ MXN con billetera digital (Apple Pay / Google Pay): Aprobación directa vía biometría del comprador (`CdcvmVerified`), sin límite de $1,000 MXN.
- **Alcance Mobile:** Enfoque 100% en Android (Kotlin 2.0). iOS permanece congelado.

---

## 2. Regla Obligatoria de Commits, Validación Completa y Push a Main
1. **Commits Atómicos por Tarea:**
   Al completar cada tarea de un loop o batch, todos los archivos modificados, suites de prueba y `tools/loop/tasks.json` deben quedar committeados con un mensaje semántico (ej. `feat(<component>): <title> [<task_id>]`).

2. **Validación Completa Pre-Push (Cero Issues):**
   Al terminar todos los commits correspondientes a ese loop o batch, es **MANDATORIO** ejecutar la suite completa de validación del repositorio:
   ```zsh
   # 1. Validación Backend Go completa
   go test -v ./...

   # 2. Validación Web POS SDK
   node --test web/pos/softpos_sdk.test.js
   ```

3. **Push Automático a `origin main`:**
   Si y solo si **todas las pruebas pasan limpiamente** (cero fallos, cero advertencias críticas), se ejecuta de inmediato:
   ```zsh
   git push origin main
   ```
   Si alguna prueba falla, el push queda **estrictamente bloqueado** hasta solucionar el issue.

4. **Actualización de Handoff:**
   Actualizar `tools/loop/HANDOFF.md` reflejando el progreso, el resumen de tareas completadas y la confirmación del push a `main`.
