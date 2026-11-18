# OpenVMS Recorder

Motor NVR autónomo escrito en Go. Graba cámaras RTSP/ONVIF en varios discos locales con cuotas y retención, ofrece consola web VMS completa (WebRTC en vivo, timeline grabado HLS, eventos con detecciones, exportaciones MP4/ZIP verificables) y expone una API REST versionada (`/api/v1`) más un puente opcional para federarse con OpenVMS Core.

Funciona de forma totalmente autónoma sin PostgreSQL, NATS, SeaweedFS, Frigate ni OpenVMS Core (`AGENTS.md`).

> **Estado:** Fases 1–3 y adaptador Fase 4 implementados con tests automáticos (`go test -race ./...`) y validados contra hardware real (`Provision-ISR DI-340IPSN-MVF-V2` y `DI-320IPSN-28-V4`) en Windows 11. Ver [limitaciones vigentes](docs/operations/limitations.md) y [matriz de compatibilidad](docs/compatibility/README.md).

## Qué hay implementado

| Área | Estado |
|---|---|
| Volúmenes: identidad estable (UUID/GUID + marcador), duplicados, probe I/O con latencia, histéresis 87 % → 84 %, `folder_quota_bytes`, asignación por cámara | ✅ con tests |
| Colocación `balanced` / `camera_affinity` / `manual`, políticas editables en runtime (`/storage/policies`), drenado + migración SHA-256 (`/volumes/{id}/drain`) | ✅ con tests |
| Retención por edad, cuota, mínimo garantizado y 3 etapas estimativas (`full`/`half`/`events`), evidencia protegida y holds de exportación | ✅ con tests |
| Ingest RTSP: FFmpeg `-c copy` (audio G.711 → AAC), segmentos MPEG-TS en keyframe, SHA-256, reconexión, gaps, control `recording/start` y `stop` | ✅ con tests + cámaras reales |
| Consola VMS: En vivo WebRTC (SD/HD, grilla, drag & drop, zoom 1×–8×, audio), Grabado HLS (+ `Compat H.264`) con marcas Inicio/Fin, Eventos y Exportaciones MP4/ZIP | ✅ con tests + navegador |
| ONVIF WS-Discovery, PullPoint, TVT HTTP Push + poller, `MotionDetector` vía `LiveHub` (sin sesión RTSP extra), backfill automático Profile G (`Range: clock=`) | ✅ con tests + `cochera` SD 128 GB |
| Seguridad: RBAC (`admin`/`operator`/`viewer`), tokens `ovs_…` con revocación en logout, API keys `ovr_…`, credenciales cifradas AES-256-GCM en reposo, TLS, `/metrics` | ✅ con tests |
| Adaptador OpenVMS Core (Fase 4): puente push firmado HMAC-SHA256 (`heartbeat` + eventos por cursor) y manifiesto pull `/api/v1/core/manifest` | ✅ con tests (`internal/core`) |
| `systemd`, Docker multiarch, servicio Windows (`deploy/`) y `openvms-tray.exe` | ✅ código listo; pruebas admin/campo pendientes |

## Uso rápido

```bash
go build -o openvms-recorder ./cmd/recorder
cp configs/recorder.example.yaml recorder.yaml   # volúmenes y cámaras
./openvms-recorder storage check -config recorder.yaml
./openvms-recorder storage init  -config recorder.yaml cctv-d   # solo sobre carpeta vacía
./openvms-recorder apikey create -config recorder.yaml admin    # muestra la clave una sola vez
./openvms-recorder serve -config recorder.yaml
# UI: http://127.0.0.1:8640/  (usuario inicial: admin / admin123 — cambiar enseguida)
```

Las cámaras se administran desde **Configuración → Cámaras** (o YAML). Las credenciales nunca van en URLs: las de UI se guardan cifradas con AES-256-GCM en `<data.dir>/camera-credentials.yaml` (`0600` / ACL) y las estáticas en el archivo referenciado por `credentials_ref`. Los discos se gestionan en **Configuración → Almacenamiento** y el adaptador opcional de **OpenVMS Core (Fase 4)** en **Configuración → Sistema**.

Instalación como servicio: `deploy/linux/install.sh` (`systemd`), `deploy/windows/install.ps1` (servicio Windows) o `deploy/docker/compose.yaml`.

## Varios discos ≠ redundancia

Repartir grabaciones entre D:, E: y F: distribuye carga y capacidad, pero **si un disco falla se pierden sus segmentos**. Para sobrevivir a fallas de disco use RAID 1/5/6/10, Storage Spaces (Windows) o ZFS/mdadm (Linux) por debajo de Recorder. Ver [ADR-002](docs/architecture/adr-002-disk-resilience.md).

## Estructura principal

```text
cmd/recorder/            binario openvms-recorder (CLI + servicio)
cmd/tray/                bandeja Windows (openvms-tray.exe)
internal/api/            API REST /api/v1, WebRTC, media, core y health
internal/auth/           RBAC, PBKDF2, tokens HMAC-SHA256 y revocación
internal/cameras/        registro dual YAML/SQLite y control recording/start|stop
internal/config/         carga YAML, validación y cifrado AES-256-GCM en reposo
internal/core/           adaptador de federación Fase 4 con OpenVMS Core
internal/db/             SQLite (modernc.org/sqlite) + migraciones
internal/events/         ONVIF PullPoint, TVT, MotionDetector (LiveHub) y Profile G SD sync
internal/exports/        jobs MP4/ZIP con holds y verificación SHA-256
internal/ingest/         supervisor FFmpeg, corte TS y reconciliación
internal/live/           LiveHub y WebRTC sin sesión RTSP extra
internal/onvif/          WS-Discovery y cliente SOAP Search/Replay/Recording
internal/playback/       tokens firmados y HLS VOD (+ transcode compat H.264 opcional)
internal/segments/       índice SQLite de segmentos, gaps y reubicación
internal/storage/        volúmenes, probe I/O, drenado, colocación, retención y políticas
web/                     consola web VMS embebida
migrations/              migraciones SQL 0001–0009
api/openapi.yaml         contrato API v1
api/config.schema.json   esquema JSON de configuración
docs/                    ADRs (001–004), almacenamiento, compatibilidad y limitaciones
```

## Desarrollo y tests

```bash
gofmt -w internal/ cmd/
go vet ./...
go test -race ./...
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./cmd/recorder
```

Requiere Go 1.26. Ver [AGENTS.md](AGENTS.md) para las reglas del proyecto.

## Licencia

MIT
