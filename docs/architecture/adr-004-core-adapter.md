# ADR-004 — Adaptador opcional de federación con OpenVMS Core (Fase 4)

- **Estado:** aceptado e implementado (PRD §9, Fase 4)
- **Fecha:** 2026-11-17

## Contexto

OpenVMS Recorder debe instalarse, grabar, reproducir y exportar de forma 100 % autónoma sin depender de OpenVMS Core, PostgreSQL, NATS ni SeaweedFS (`AGENTS.md`). Al mismo tiempo, la Fase 4 requiere integrar nodos grabadores autónomos con OpenVMS Core sin crear acoplamiento binario ni modificar el repositorio `jdolan-exalink/openvms` durante esta etapa.

## Decisión

Se implementa un puente bidireccional desacoplado por HTTP/JSON en `internal/core` y `/api/v1/core/*`, deshabilitado por defecto y activable por YAML (`core:`) o API/UI (**Configuración → Sistema**):

1. **Autonomía total:** si `core.enabled=false` o OpenVMS Core devuelve error/timeout, el grabador mantiene intactos su ingesta RTSP, retención, Profile G y base SQLite. El adaptador reporta estado `disabled`, `connected`, `degraded` o `offline` sin bloquear hilos de grabación.
2. **Modo Push (saliente):**
   - `POST {core_url}/api/v1/recorders/heartbeat`: envía `core.Manifest` (`schema_version: openvms-rec.v1`) con identidad (`node_id`, `site_id`, versión, OS/arch), catálogo de cámaras y capacidades reales verificadas, estado de streams y resumen/volúmenes de almacenamiento.
   - `POST {core_url}/api/v1/recorders/events`: envía lotes ordenados de hasta 100 eventos usando cursor persistido en SQLite (`core_adapter_state`: `last_event_ms`, `last_event_id`, `events_synced`), avanzando el cursor únicamente ante confirmación HTTP `2xx`.
   - Autenticación saliente con `Authorization: Bearer <token>` y firma `X-OpenVMS-Signature: sha256=<HMAC-SHA256(body)>`. El token se guarda cifrado en reposo en `core_adapter_config.token_enc` usando AES-256-GCM (`enc:v1:…`).
3. **Modo Pull (entrante):**
   - `GET /api/v1/core/manifest` expone el manifiesto normalizado (incluyendo rutas relativas de WebRTC, snapshot y grabaciones, más `event_cursor`) para workers pull autenticados.
   - `GET /api/v1/core/status`, `PUT /api/v1/core/config` y `POST /api/v1/core/sync` permiten emparejamiento, diagnóstico y sincronización manual desde API/UI.

## Consecuencias

- Cero dependencias binarias respecto de paquetes internos de `jdolan-exalink/openvms`.
- Los credenciales y tokens de enlace quedan cifrados en reposo con clave local (`0600` / ACL) y nunca se devuelven en respuestas `GET`.
- Cuando se implemente el receptor homólogo en OpenVMS Core (PR separado sobre aquel repo), podrá consumir tanto el flujo push firmado como `/api/v1/core/manifest`.
