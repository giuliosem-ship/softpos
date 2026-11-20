# Reglas de colaboración para agentes (Kerno / SoftPOS)

## 1. Reglas de dominio y ciclo TDD
- Cada tarea en el backlog (`tools/loop/tasks.json`) sigue el ciclo estricto: **RED ➔ GREEN ➔ AUDIT** (ver §2).
- **Aritmética Financiera:** No se introducen números en punto flotante (`float32`/`float64`/`Double`/`Float`) para montos monetarios o centavos; se usan exclusivamente enteros de 64 bits (`int64` en Go, `Long` en Kotlin) y puntos base (`bps`) con aritmética half-up.
- **Sanitización Estricta de Memoria (PCI):** Búferes sensibles de tarjetas o PIN deben limpiarse explícitamente (`ByteArray.fill(0)`) en bloques `finally`.
- **Reglas CVM Contactless (México):**
  - $\le \$1,000.00$ MXN: Aprobación sin CVM (`NoCvmRequired`).
  - $> \$1,000.00$ MXN con tarjeta física: Requiere PIN-on-Glass con teclado aleatorizado (`PinRequired`).
  - $> \$1,000.00$ MXN con billetera digital (Apple Pay / Google Pay): Aprobación directa vía biometría del comprador (`CdcvmVerified`), sin límite de $1,000 MXN.
- **Alcance Mobile:** Enfoque 100% en Android (Kotlin 2.0). iOS permanece congelado.

---

## 2. Proceso de ingeniería (desde la Fase 1 del MVP, 2026-11)

### 2.1 Quién ejecuta el loop
- El loop lo ejecuta **Claude Code** por lotes (*batches*) de 3–4 tareas `MVP-NN` definidas a partir de `docs/KERNO_MVP_GAP_ANALYSIS.md`:
  1. Un **subagente implementador** por lote: spec/docs → tests **RED** → código **GREEN** → autoverificación, con un commit atómico por tarea.
  2. Un **subagente auditor independiente** por lote, que revisa el diff sin el razonamiento del implementador (**AUDIT**); cada hallazgo se corrige en un commit `fix(...)` con sufijo `[MVP-NN-audit]`.
- El orquestador multi-modelo de LM Studio (`tools/loop/loop_runner.py`, Qwen/Gemma) queda como **herramienta legacy y opcional**: no es parte del proceso obligatorio ni de la validación.

### 2.2 Commits
- Un commit por tarea, mensaje semántico en inglés: `feat(<componente>): <título> [MVP-NN]` (o `fix`/`ci`/`docs`), incluyendo tests, docs y la entrada de `tools/loop/tasks.json`.
- Nunca se commitean `.env`, llaves, certificados, binarios ni salidas de build (`.env.example` sí, con placeholders `CHANGE_ME`).

### 2.3 Validación completa antes de push (cero fallos)
```zsh
go vet ./...
go test -race -count=1 ./...
# Integración con PostgreSQL 16 real (embedded-postgres local o el servicio de CI)
TEST_DATABASE_URL=postgres://… go test -race -count=1 -run PGIntegration ./...
node --test web/pos/softpos_sdk.test.js
(cd mobile/android && JAVA_HOME=<jdk17> ./gradlew :app:test)
```

### 2.4 Push y CI
- Con la validación completa en verde, el lote se publica con `git push origin main` (decisión del dueño del repo).
- El workflow `.github/workflows/ci.yml` (Go vet + race, integración Postgres, Web POS, Kotlin, build de imagen Docker) debe quedar **verde** en GitHub; un CI rojo se corrige antes de empezar el siguiente lote.

### 2.5 Backlog vivo
- `docs/KERNO_MVP_GAP_ANALYSIS.md` es el backlog vivo: en el **mismo lote** que resuelve un gap se elimina su fila (o se reduce al alcance pendiente) y se actualizan el encabezado `Última actualización`, §1, §7 y §9.
- `tools/loop/HANDOFF.md` resume el estado al cerrar cada lote o fase.
