# AGENTS.md — reglas para quien desarrolle en este repositorio (humanos y agentes)

## Alcance

OpenVMS Recorder es un NVR autónomo. Debe instalarse y funcionar sin OpenVMS Core, Frigate, PostgreSQL, NATS ni SeaweedFS.

## Reglas duras

1. **No modificar el repositorio `jdolan-exalink/openvms`** durante esta fase. La integración será un adaptador posterior (PRD §9).
2. **No importar paquetes de OpenVMS como dependencia** (el Recorder compila solo). **Copiar código de `jdolan-exalink/openvms` sí está permitido** (autorizado por el dueño de ambos repos, 2026-11-13) para no rehacer lo que ya está probado: se copia al árbol del Recorder, con una nota de origen en el paquete. Ya copiado: `internal/onvif` (de `openvms/internal/onvif`, más el probe de `internal/agent/onvifdiscover`).
3. **Sin dependencias obligatorias de Frigate.**
4. **No afirmar capacidades de hardware sin prueba real.** Toda capacidad de cámara se reporta como `confirmed`, `unsupported`, `unknown` o `error`, con evidencia de la operación ejecutada. Nunca inferir Profile G, microSD replay ni analíticas por fabricante o modelo. La Provision I4-320IPSN-VF-V4 está sin validar.
5. **Nunca simular reproducción** de microSD ni inventar continuidad: los huecos se registran como gaps.
6. **Nunca formatear, inicializar ni reparar automáticamente** un disco. Un volumen con fallas se marca `degraded` y se deja de escribir en él.
7. **Nunca borrar evidencia protegida** (`protection_until` futuro) ni segmentos con hold de exportación. La retención falla en forma visible (alertas), no silenciosa.
8. **Varios volúmenes no son redundancia.** La UI y la documentación deben decirlo; no implementar RAID propio en Go.
9. **Secretos**: nunca en logs, configuración en texto plano, PRD, tests ni fixtures. Credenciales de cámara en archivo 0600 o secret manager; URLs RTSP se guardan sin credenciales. Redactar IPs/tokens en diagnósticos.
10. **Video nunca dentro de SQLite.** El índice referencia archivos por `volume_id` + `relative_path`.
11. No transcodificar en la ruta normal de grabación (`-c copy`).

## Convenciones

- Go 1.26, `gofmt`, `go vet`, `go test -race ./...` verdes antes de cada PR.
- SQLite vía `modernc.org/sqlite` (Go puro) para compilar Windows/ARM sin CGO.
- Tiempo: UTC en milisegundos en la base; RFC 3339 UTC en la API.
- API contract-first: cambiar `api/openapi.yaml` en el mismo PR que el handler. Operaciones no implementadas llevan `x-status: planned`.
- Migraciones: archivos nuevos `NNNN_descripcion.sql`, nunca editar una migración publicada.
- Rutas: probar con espacios y Unicode; `relative_path` usa `/` y se convierte con `filepath.FromSlash`.
- Código y comentarios en inglés; documentación de producto en español.
- Cada fase se entrega con tests, evidencia y actualización de `docs/operations/limitations.md`.

## Orden de trabajo (PRD §14)

1. Diseño de almacenamiento multiplataforma/multivolumen ✅ (PR 1)
2. Ingest RTSP estable con cuotas y retención ✅ (MVP: más playback, export, auth, UI y despliegue)
3. ONVIF, microSD/Profile G y eventos (con hardware real)
4. Integración con OpenVMS Core (otro PRD)

## Documentos

- `docs/architecture/adr-001-recording-streams-aging.md`
- `docs/architecture/adr-002-disk-resilience.md`
- `docs/architecture/adr-003-sd-sync.md`
- `docs/architecture/storage.md` — diseño M04 con fórmulas
- `docs/compatibility/README.md` — matriz de compatibilidad con evidencia
- `docs/operations/limitations.md` — informe de limitaciones vigente
