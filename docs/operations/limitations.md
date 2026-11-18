# Informe de limitaciones

Actualizado con cada entrega. Última: **Cierre Fases 1–3 + Adaptador Fase 4 OpenVMS Core + validación real Profile G microSD en `cochera`** (2026-11-17).

## Implementado y probado (tests automáticos + UI en vivo)

- **Almacenamiento multivolumen y políticas (Fase 1):**
  - Cálculo de límite efectivo (incluyendo `folder_quota_bytes`), histéresis, colocación `balanced` / `camera_affinity` / `manual`, asignación de cámaras por disco desde API/UI y estimador de retención en 3 etapas (`GET /api/v1/storage/estimate`, `PUT /api/v1/storage/retention`).
  - Identidad de volumen por marcador y GUID/UUID, probe I/O activo con medición de latencia (`probe.latency_ms`), drenado de volúmenes (`POST /api/v1/storage/volumes/{id}/drain` con migración opcional verificada por SHA-256 y aumento atómico de `generation`), y políticas globales modificables en caliente (`GET /api/v1/storage/policies`, `PATCH /api/v1/storage/policies/{id}`).
  - Planificador y ejecutor de retención con holds de exportación y preservación de evidencia protegida; migraciones SQLite `0001`–`0009`.
- **Ingest RTSP, reproducción, exportaciones y seguridad (Fase 2):**
  - Ingest `-c copy` en video (AAC en audio G.711), corte propio en keyframes, escritura atómica `.tmp/`, SHA-256, reconexión exponencial, gaps explícitos y pausa/reanudación de grabación por cámara (`POST /api/v1/cameras/{id}/recording/start|stop`, persistido también para cámaras YAML).
  - Vista **En vivo (WebRTC)** con grillas configurables, drag & drop en árbol lateral, zoom digital 1×–8× SD/HD, audio muteado por defecto con control de volumen; modo **Grabado** con timeline sincronizado, HLS VOD (`Compat H.264` opcional) y selección Inicio/Fin para **Exportaciones** MP4/ZIP con modal reproductor, HTTP Range y cancelación.
  - RBAC (`admin`, `operator`, `viewer`), tokens `ovs_…`, revocación individual de sesiones (`POST /api/v1/auth/logout` con tabla `revoked_tokens`), API keys `ovr_…` SHA-256, **cifrado AES-256-GCM en reposo** para `<data.dir>/camera-credentials.yaml` (`enc:v1:…` con fallback de lectura legacy) y token del adaptador Core, rate-limiting 429, TLS, SSRF guard (`server.camera_networks`), `backup`/`restore` y `/metrics`.
- **ONVIF + TVT/Provision + Profile G + Analíticas (Fase 3):**
  - WS-Discovery, PullPoint, push HTTP TVT (`/SendAlarmData`, `/GetDiskInfo`, perímetro), `MotionDetector` conectado a `LiveHub` (`pipe:0`, sin sesión RTSP extra) y snapshots anotados.
  - Backfill **Profile G** automático (periódico + reactivo en `OnGapClosed` con debounce 3 s), proxy RTSP `Range: clock=` con reemplazo de `Range: npt=`, fallback video-only para pistas `pcma/0`, y distinción explícita en UI entre tarjeta sin formatear y ausencia de tarjeta / sin formato (`sin tarjeta / sin formatear`).
- **Integración OpenVMS Core (Fase 4 — [ADR-004](../architecture/adr-004-core-adapter.md)):**
  - Adaptador opcional e independiente en `internal/core` con endpoints `GET /api/v1/core/status`, `PUT /api/v1/core/config`, `POST /api/v1/core/sync` y `GET /api/v1/core/manifest`, push firmado HMAC-SHA256 (`heartbeat` + eventos por cursor SQLite) y panel de administración en **Configuración → Sistema**.

## No implementado todavía

- Monitoreo SMART de hardware y autodetección automática desde el SO de cuotas NTFS/ext4 por carpeta (`folder_quota_gb` hoy es configurable explícitamente).
- Adaptadores propietarios de descarga SD para cámaras que no implementen ONVIF Profile G.
- mTLS máquina-a-máquina.
- Instaladores firmados `.msi` / `.deb`.
- Adaptador homólogo del lado de **OpenVMS Core** (`jdolan-exalink/openvms`), fuera de alcance por Regla #1 de `AGENTS.md`.

## Precisión y comportamiento a conocer

- Los tiempos de segmento usan reloj del host (UTC) al recibir paquetes: precisión ≈ 1 GOP + jitter de red.
- Si falla un disco a mitad de segmento o durante drenado, el ingest cambia de volumen en keyframe (pérdida típica 1–3 s registrada como gap).
- La reproducción directa H.265/HEVC depende del navegador; `Compat H.264` transcodifica solo la vista previa sin alterar los archivos grabados ni los ZIP/MP4 exportados.

## Validado solo parcialmente

- **Windows:** API, ingest, WebRTC live, timeline Grabado, Exportaciones y backfill real Profile G microSD validados en Windows 11 real (`cochera` con microSD 128 GB; `patio de luz` no tiene microSD física); siguen pendientes pruebas administrativas de **Windows Service SCM** (`install.ps1` / `uninstall.ps1`), `openvms-tray.exe` y discos ReFS / cambio real de letra en producción.
- **Linux:** probado en ext4 dentro de contenedor (`go test -race ./...`); faltan pruebas en `systemd` real, XFS, ZFS y mdadm.
- **Docker / CI:** Dockerfile y compose definidos; falta runner Windows en CI.

## Hardware

- Ver matriz y evidencia redactada en [`docs/compatibility/README.md`](../compatibility/README.md).
- `Provision-ISR I4-320IPSN-VF-V4` sigue sin validar (`unknown`).

## Autenticación de usuarios

En el primer despliegue (cuando la tabla `users` en SQLite está vacía) se crea automáticamente el usuario por defecto `admin` con contraseña `admin123` (hash `pbkdf2_sha256` con sal). **Cambiar la contraseña de `admin` inmediatamente en producción.**
