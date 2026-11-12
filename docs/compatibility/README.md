# Matriz de compatibilidad

Cada celda se completa **solo** con evidencia de una prueba real (operación ejecutada, respuesta redactada, firmware y fecha). Valores posibles: `confirmed`, `unsupported`, `unknown`, `error`. No se infiere nada por fabricante ni modelo.

## Cámaras

| Modelo | Firmware | ONVIF Media/Media2 | RTSP main/sub | Events (PullPoint) | Profile G Recording/Search | Replay | Export | microSD | Persona/Vehículo | Evidencia |
|---|---|---|---|---|---|---|---|---|---|---|
| Provision-ISR I4-320IPSN-VF-V4 | — | unknown | unknown | unknown | unknown | unknown | unknown | unknown | unknown | pendiente laboratorio (PRD §8) |

Las IPs y credenciales de laboratorio se pasan por variables de entorno locales y nunca se guardan en el repositorio.

## Plataformas

| Plataforma | Compila | Tests unitarios | Servicio | Volúmenes probados | Evidencia |
|---|---|---|---|---|---|
| Linux amd64 | sí | sí (CI) | pendiente (systemd) | ext4 (contenedor de CI) | CI |
| Linux arm64 | sí (cross) | pendiente | pendiente | — | — |
| Windows amd64 | sí (cross) | pendiente (runner Windows) | pendiente (SCM) | — | — |
