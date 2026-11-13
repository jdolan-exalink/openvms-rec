# Matriz de compatibilidad

Cada celda se completa **solo** con evidencia de una prueba real (operación ejecutada, respuesta redactada, firmware y fecha). Valores posibles: `confirmed`, `unsupported`, `unknown`, `error`. No se infiere nada por fabricante ni modelo.

## Cámaras

| Modelo | Firmware | ONVIF Media/Media2 | RTSP main/sub | Events (PullPoint) | Profile G Recording/Search | Replay | Export | microSD | Persona/Vehículo | Evidencia |
|---|---|---|---|---|---|---|---|---|---|---|
| Provision-ISR I4-320IPSN-VF-V4 (modelo informado por el usuario, no leído por ONVIF) | — | unknown | main `confirmed` (`/profile1`); sub unknown | unknown | unknown | unknown | unknown | unknown | unknown | ver nota 2026-11-13 |

**Nota 2026-11-13 (Provision-ISR, grabación continua en Windows 11):**
- RTSP `/profile1` sobre TCP con autenticación: HEVC Main 2560×1440, ~15 fps, ~2.5 Mb/s.
- Audio en un códec que MPEG-TS no admite. El modo `auto` lo transcodifica a AAC 8 kHz mono; el video se copia.
- Segmentos de ~64 s que empiezan en keyframe (I).
- Playback HLS (m3u8) y export MP4 con SHA-256 del manifest verificados.
- Durante unos 14 minutos la cámara rechazó SETUP con `453 Not Enough Bandwidth`, y el recorder lo registró como gap `stream_lost` con reintentos. No se determinó la causa: es posible que otro cliente estuviera ocupando sesiones.
- No se probó ONVIF, eventos, Profile G ni microSD.
- La reproducción de HEVC en el navegador depende del navegador.

Las IPs y credenciales de laboratorio se pasan por variables de entorno locales y nunca se guardan en el repositorio.

## Plataformas

| Plataforma | Compila | Tests unitarios | Servicio | Volúmenes probados | Evidencia |
|---|---|---|---|---|---|
| Linux amd64 | sí | sí (CI) | pendiente (systemd) | ext4 (contenedor de CI) | CI |
| Linux arm64 | sí (cross) | pendiente | pendiente | — | — |
| Windows amd64 | sí (cross) | sí: 73 ok en Windows 11 real (2026-11-13) | pendiente (SCM, requiere admin) | NTFS (GUID de volumen verificado) | prueba manual 2026-11-13 |
