# Matriz de compatibilidad

Cada celda se completa **solo** con evidencia de una prueba real (operación ejecutada, respuesta redactada, firmware y fecha). Valores posibles: `confirmed`, `unsupported`, `unknown`, `error`. No se infiere nada por fabricante ni modelo.

## Cámaras

| Modelo | Firmware | ONVIF Media/Media2 | RTSP main/sub | Events (PullPoint) | Profile G Search/Replay | Export | microSD | Persona/Vehículo | Evidencia |
|---|---|---|---|---|---|---|---|---|---|
| Provision-ISR `I4-320IPSN-VF-V4` | — | unknown | unknown | unknown | unknown | unknown | unknown | unknown | Sin validar (Regla #4) |
| Provision-ISR `DI-340IPSN-MVF-V2` (`cochera`) | `5.1` / TVT `5.1.1.0(65585)` | `confirmed` (`Media2.GetProfiles`, 3 perfiles) | main/sub `confirmed` | `confirmed` (`GetEventProperties` 39 tópicos + `CreatePullPointSubscription`) | `confirmed` (`GetRecordingSummary`, `FindRecordings`, `GetRecordings`, `GetReplayUri` + RTSP `Range: clock=`) | `confirmed` (MP4/ZIP + SHA-256) | `confirmed` (`GetDiskInfo` `read/write` 128 GB; 5 seg. `.ts` H.265 `2560×1440` recuperados = 31,4 MB) | `confirmed` (HTTP Push + NVR local `LiveHub`; TVT Perímetro `confirmed`, Tripwire `unsupported`) | ver nota 2026-11-17 |
| Provision-ISR `DI-320IPSN-28-V4` (`patio de luz`; ONVIF reporta `DI-340IPSN-28-V4`) | `5.1` / TVT `5.1.1.0(54774)` | `confirmed` (`Media2.GetProfiles`, 3 perfiles) | main/sub `confirmed` | `confirmed` (`GetEventProperties` 39 tópicos + `CreatePullPointSubscription`) | `confirmed` (`GetRecordingSummary`, `FindRecordings`, `GetReplayUri`) | `confirmed` | `unsupported` en equipo actual (sin microSD física instalada; `/GetDiskInfo` `totalSpace=0`, `status=unformat`) | `confirmed` (TVT `/SendAlarmData` `person` c/ bbox + TVT Perímetro `confirmed`, Tripwire `unsupported`) | ver nota 2026-11-17 |

**Nota 2026-11-17 (Provision-ISR `DI-340IPSN-MVF-V2` y `DI-320IPSN-28-V4` en Windows 11):**
- Grabación continua RTSP (`/profile1` main y `/profile2` sub sobre TCP): audio G.711 transcodificado a AAC en contenedor MPEG-TS y video copiado (`-c copy`).
- Vista en vivo WebRTC (`SD` en grilla, `HD` al maximizar / zoom > 1.8×), reproducción sincronizada en modo **Grabado**, marcas Inicio/Fin y exportación MP4/ZIP validados en navegador.
- Límite de sesiones RTSP simultáneas en cámara: detectado `453 Not Enough Bandwidth` al abrir una 3.ª sesión; resuelto haciendo que `MotionDetector` consuma de `LiveHub` en memoria (`pipe:0`), dejando libre una sesión RTSP para replay ONVIF Profile G.
- **Profile G microSD en `cochera` (`DI-340IPSN-MVF-V2`):** una vez formateada la tarjeta de 128 GB en la web de la cámara (`status=read/write`, `DataFrom` real), el servicio ejecutó backfill automático periódico y reactivo (`OnGapClosed`) reemplazando `Range: npt=` por `Range: clock=` en `PLAY` y aplicando fallback video-only para la pista inactiva `pcma/0` del SDP `/Recording`, recuperando 5 segmentos H.265 `2560×1440` (`31.427.208 bytes`) verificados con SHA-256 y `ffprobe`. En `patio de luz` (`DI-320IPSN-28-V4`) no hay tarjeta microSD instalada físicamente.
- Analíticas TVT/Provision: ambas cámaras entregaron eventos HTTP push reales en `/SendAlarmData` (`person`/`vehicle` con bounding box normalizado); `GetPerimeterConfig` devolvió `switch=true` y `GetTripwireConfig` `switch=false`.

Las IPs y credenciales de laboratorio se pasan por configuración local (`camera-credentials.yaml`, cifrado AES-256-GCM en reposo / permisos `0600`) y nunca se guardan en el repositorio.

## Plataformas

| Plataforma | Compila | Tests unitarios | Servicio | Volúmenes probados | Evidencia |
|---|---|---|---|---|---|
| Linux amd64 | sí | sí (`go test -race ./...` en contenedor `golang:1.26`) | código listo (`systemd`); pendiente en host real | ext4 (contenedor) | CI / contenedor |
| Linux arm64 | sí (cross) | pendiente | pendiente | — | — |
| Windows amd64 | sí (cross) | sí (`go test -race ./...` verde + binario nativo en Win 11) | código SCM listo; prueba admin SCM/Tray pendiente | NTFS (GUID de volumen + probe I/O real verificados) | prueba real 2026-11-17 |
