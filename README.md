# OpenVMS Recorder

Motor NVR independiente escrito en Go. Graba cámaras RTSP/ONVIF en discos locales, gestiona varios volúmenes con cuotas y retención, y expone una API REST versionada (`/api/v1`) para integrarse más adelante con OpenVMS Core mediante un adaptador.

Funciona sin PostgreSQL, NATS, SeaweedFS, Frigate ni OpenVMS. Especificación: PRD OpenVMS Recorder v1.1 (12/11/2026).

> **Estado: MVP en construcción (sin release).** Graba RTSP en varios discos, reproduce y exporta evidencia verificable desde una UI web mínima. ONVIF, microSD/Profile G y eventos todavía **no** están implementados. Ver [limitaciones](docs/operations/limitations.md).

## Qué hay implementado

| Área | Estado |
|---|---|
| Volúmenes: identidad estable (UUID/GUID + marcador), duplicados, disco de sistema/red/extraíble, límite efectivo y histéresis 87 % → 84 % | ✅ con tests |
| Colocación `balanced` / `camera_affinity` / `manual`, retención por edad, espacio y mínimo garantizado, evidencia protegida | ✅ con tests |
| Ingest RTSP: FFmpeg `-c copy`, segmentos MPEG-TS cortados en keyframe, escritura atómica, SHA-256, reconexión, gaps, recuperación tras corte | ✅ con tests + prueba con MediaMTX |
| Playback HLS sin transcodificar con tokens firmados de 15 min y saltos de gaps | ✅ |
| Export MP4 en ZIP con manifest y checksums, como job idempotente y auditado | ✅ |
| API keys (solo hash SHA-256, archivo 0600), throttling, `/metrics` Prometheus, cabeceras de seguridad | ✅ |
| UI web en español: panel, cámaras, línea de tiempo, reproducción, export, almacenamiento | ✅ |
| systemd, imagen Docker multiarch, servicio Windows con instalador PowerShell, `backup` de SQLite | ✅ código; Docker y Windows sin probar en real |
| ONVIF, microSD/Profile G, eventos, alta de cámaras/volúmenes por API, TLS | ⏳ planificado |

## Uso rápido

```bash
go build -o openvms-recorder ./cmd/recorder
cp configs/recorder.example.yaml recorder.yaml   # volúmenes y cámaras
./openvms-recorder storage check -config recorder.yaml
./openvms-recorder storage init  -config recorder.yaml cctv-d   # solo sobre carpeta vacía
./openvms-recorder apikey create -config recorder.yaml admin    # muestra la clave una sola vez
./openvms-recorder serve -config recorder.yaml
# UI: http://127.0.0.1:8640/
```

Las credenciales de cámara van en el archivo de secretos (permisos 0600) referenciado por `credentials_ref`, nunca en la URL del YAML. Requiere `ffmpeg` en el PATH (o `recording.ffmpeg_path`). `serve` se niega a escuchar fuera de loopback si no hay API keys.

`storage init` escribe un marcador `.openvms-recorder-volume.json` en la raíz del volumen. Sin marcador el volumen no recibe grabaciones: así un disco desmontado nunca se confunde con la carpeta vacía que queda debajo, y un cambio de letra de unidad sigue identificando el mismo disco. Recorder **nunca formatea, inicializa ni repara** discos.

Instalación como servicio: `deploy/linux/install.sh` (systemd), `deploy/windows/install.ps1` (servicio Windows) o `deploy/docker/compose.yaml`.

## Varios discos ≠ redundancia

Repartir grabaciones entre D:, E: y F: distribuye carga y capacidad, pero **si un disco falla se pierden sus segmentos**. Para sobrevivir a fallas de disco use RAID 1/5/6/10, Storage Spaces (Windows) o ZFS/mdadm (Linux) por debajo de Recorder. Ver [ADR-002](docs/architecture/adr-002-disk-resilience.md).

## Plataformas

Objetivo: Windows 10/11 y Server (amd64), Debian 13 / Ubuntu 24.04 (amd64/arm64), Docker multiarch. El binario compila para los tres targets con `CGO_ENABLED=0`.

## Estructura

```
cmd/recorder/            binario openvms-recorder (CLI + servicio)
internal/api/            API REST /api/v1 y health
internal/config/         carga y validación de YAML
internal/db/             SQLite + migraciones
internal/segments/       índice de segmentos (ADR-001)
internal/storage/        volúmenes, colocación, retención, salud de disco
internal/ingest/         supervisor FFmpeg, segmentador TS, escritura atómica
internal/playback/       tokens firmados y playlists HLS
internal/exports/        jobs de export con manifest verificable
internal/auth/           API keys
internal/platform/       consultas de disco y servicio Windows
web/                     UI embebida
deploy/                  systemd, Docker, PowerShell
migrations/              migraciones SQL embebidas
api/openapi.yaml         contrato API v1 (fuente de verdad)
api/config.schema.json   esquema de configuración
configs/                 configuración de ejemplo
docs/                    ADRs, diseño, compatibilidad, operación
```

Los paquetes `drivers` y `events` del PRD se agregan con sus funcionalidades.

## Desarrollo

```bash
go test ./...
go test -race ./...
go vet ./...
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build ./cmd/recorder
```

Requiere Go 1.26. Ver [AGENTS.md](AGENTS.md) para las reglas del proyecto.

## Licencia

MIT
