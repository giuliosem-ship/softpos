---
trigger: always_on
description: Regla de validación completa y push a main al finalizar cada ciclo de loop.
---

# Regla de Validación y Push a Main en Ciclos de Loop

Al finalizar la ejecución de las tareas de un loop o batch:
1. Asegurar que cada tarea completada tenga su commit atómico en Git.
2. Ejecutar la validación completa del repositorio:
   - `go test -v ./...`
   - `node --test web/pos/softpos_sdk.test.js`
3. Si no hay ningún issue, ejecutar `git push origin main`.
4. Si se detecta cualquier fallo o regresión, detener el proceso y resolver el error antes de autorizar el push.
5. Actualizar `tools/loop/HANDOFF.md` con el estado del push.
