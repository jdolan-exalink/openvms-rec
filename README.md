# OpenVMS Recorder

Motor NVR independiente escrito en Go. Graba cámaras RTSP/ONVIF en discos locales, gestiona varios volúmenes con cuotas y retención, y expone una API REST versionada (`/api/v1`) para integrarse más adelante con OpenVMS Core mediante un adaptador.

Funciona sin PostgreSQL, NATS, SeaweedFS, Frigate ni OpenVMS. Especificación: PRD OpenVMS Recorder v1.1 (12/11/2026).

> **Estado: fase 1 en construcción.** Este repositorio contiene hoy la base del proyecto y el motor de almacenamiento multivolumen (M04). La grabación RTSP, ONVIF, microSD, playback y export todavía **no** están implementados. Ver [limitaciones](docs/operations/limitations.md).

## Qué hay implementado

| Área | Estado |
|---|---|
| Volúmenes: identidad estable (UUID/GUID + marcador), detección de duplicados, disco de sistema/red/extraíble | ✅ Linux y Windows (Windows compilado, sin validar en hardware) |
| Límite efectivo: mínimo entre cuota % (90 por defecto), cuota absoluta, reserva libre, espacio realmente disponible y cuota de carpeta | ✅ con tests |
| Histéresis de limpieza 87 % → 84 % (escalada al límite efectivo de cada volumen) | ✅ con tests |
| Colocación de segmentos completos: `balanced`, `camera_affinity`, `manual`, prioridades por niveles | ✅ con tests |
| Retención: edad máxima, espacio, mínimo garantizado, prioridad de cámara, protección de evidencia y exportaciones | ✅ con tests |
| Índice SQLite (WAL, Go puro, sin CGO) con modelo de segmento ADR-001 | ✅ |
| API: health, capabilities, resumen de almacenamiento, volúmenes, validación de volumen | ✅ |
| CLI: `serve`, `storage check`, `storage init`, `migrate`, `version` | ✅ |
| Ingest RTSP, ONVIF, microSD/Profile G, eventos, playback, export, UI, auth | ⏳ planificado |

## Uso rápido

```bash
go build -o openvms-recorder ./cmd/recorder
cp configs/recorder.example.yaml recorder.yaml   # editar rutas de volúmenes
./openvms-recorder storage check -config recorder.yaml
./openvms-recorder storage init  -config recorder.yaml cctv-d   # solo sobre carpeta vacía
./openvms-recorder serve -config recorder.yaml
curl http://127.0.0.1:8640/api/v1/storage/volumes
```

`storage init` escribe un marcador `.openvms-recorder-volume.json` en la raíz del volumen. Sin marcador el volumen no recibe grabaciones: así un disco desmontado nunca se confunde con la carpeta vacía que queda debajo, y un cambio de letra de unidad sigue identificando el mismo disco. Recorder **nunca formatea, inicializa ni repara** discos.

## Varios discos ≠ redundancia

Repartir grabaciones entre D:, E: y F: distribuye carga y capacidad, pero **si un disco falla se pierden sus segmentos**. Para sobrevivir a fallas de disco use RAID 1/5/6/10, Storage Spaces (Windows) o ZFS/mdadm (Linux) por debajo de Recorder. Ver [ADR-002](docs/architecture/adr-002-disk-resilience.md).

## Plataformas

Objetivo: Windows 10/11 y Server (amd64), Debian 13 / Ubuntu 24.04 (amd64/arm64), Docker multiarch. Hoy el binario compila para los tres targets con `CGO_ENABLED=0`; instaladores, servicio Windows y systemd llegan en PRs posteriores.

## Estructura

```
cmd/recorder/            binario openvms-recorder (CLI + servicio)
internal/api/            API REST /api/v1 y health
internal/config/         carga y validación de YAML
internal/db/             SQLite + migraciones
internal/segments/       índice de segmentos (ADR-001)
internal/storage/        volúmenes, colocación, retención, salud de disco
internal/platform/       consultas de disco específicas de Linux/Windows
migrations/              migraciones SQL embebidas
api/openapi.yaml         contrato API v1 (fuente de verdad)
api/config.schema.json   esquema de configuración
configs/                 configuración de ejemplo
docs/                    ADRs, diseño, compatibilidad, operación
```

Los paquetes `ingest`, `drivers`, `timeline`, `playback`, `exports`, `events` y `web/` de la estructura del PRD se agregan con sus funcionalidades.

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
