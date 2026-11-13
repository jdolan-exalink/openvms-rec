# ADR-003 — Sincronización tras cortes y microSD

- **Estado:** aceptado (PRD v1.1, §18); implementación en fase 2, **condicionada a hardware real**
- **Fecha:** 2026-11-12

## Contexto

Las cámaras con microSD pueden seguir grabando cuando se corta la red o falla Recorder. Recuperar esos tramos solo es posible si la cámara expone una interfaz de búsqueda y replay/export utilizable (ONVIF Profile G o API propietaria). Eso no se puede asumir por marca ni modelo.

## Decisión

1. Ante un corte de conectividad o falla de grabación local, se registra un **gap** por cámara (`recording_gaps`). No se inventa continuidad.
2. Al restablecerse la conexión, y **solo si la cámara tiene confirmadas** (con evidencia) las capacidades de búsqueda y replay/export, se consulta el inventario de la microSD, se comparan rangos UTC y se encola la recuperación.
3. Políticas: `recover_all`, `recover_events_only`, `index_remote_only`, `disabled` (y `when_supported` como valor por defecto, que equivale a `recover_all` solo sobre capacidades confirmadas). Límites de Mbps por cámara/sitio, concurrencia, backoff, checksum y estado por job.
4. Prioridad: primero el material que está más cerca de ser sobrescrito en la microSD.
5. Se distinguen orígenes de segmento: `live_ingest`, `recovered_sd`, `remote_only`. En el timeline, `camera_sd` tiene disponibilidad y latencia propias, y se distingue "índice consultado" de "grabación efectivamente accesible".
6. Desfase de relojes: se mide el offset cámara/host y se documenta la precisión de GOP; no se recortan tramos por relojes desfasados.
7. Sin API de reproducción/exportación histórica la sincronización física **no es posible**; la UI lo muestra explícitamente.

## Gates de laboratorio (PRD §8)

- Si Profile G no responde: abrir investigación del driver propietario.
- Si tampoco hay replay accesible: marcar Edge SD no compatible para ese firmware y mantener grabación central.

## Consecuencias

- El esquema ya incluye `source`, `recording_gaps`, `jobs` y `camera_capabilities` con evidencia.
- Ninguna capacidad de la Provision I4-320IPSN-VF-V4 está confirmada a la fecha.
