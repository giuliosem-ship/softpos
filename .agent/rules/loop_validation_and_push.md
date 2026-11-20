---
trigger: always_on
description: Regla de validación completa y push a main al finalizar cada lote (ver AGENTS.md §2).
---

# Regla de Validación y Push a Main por Lote

La fuente de verdad del proceso es `AGENTS.md` §2. Al finalizar un lote:
1. Cada tarea completada tiene su commit atómico en Git (incluye `tools/loop/tasks.json`).
2. Validación completa del repositorio:
   - `go vet ./...` y `go test -race -count=1 ./...`
   - `TEST_DATABASE_URL=… go test -race -count=1 -run PGIntegration ./...` (PostgreSQL 16 real)
   - `node --test web/pos/softpos_sdk.test.js`
   - `cd mobile/android && ./gradlew :app:test` (JDK 17)
3. Solo con todo en verde: `git push origin main`; después, el CI de GitHub (`.github/workflows/ci.yml`) debe quedar verde.
4. Ante cualquier fallo o regresión, detener y resolver antes del push.
5. Actualizar `tools/loop/HANDOFF.md` y podar `docs/KERNO_MVP_GAP_ANALYSIS.md`.
