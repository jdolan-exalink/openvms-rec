# Informe de limitaciones

Actualizado con cada entrega. Última: **MVP — grabación, playback, export, UI y despliegue** (2026-11-12).

## Implementado y probado (tests automáticos)

- Cálculo de límite efectivo, histéresis y fórmulas de capacidad del PRD §7.
- Identidad de volumen con marcador, cambio de ruta/letra, disco equivocado montado, duplicados, estados `healthy/full/degraded/missing/disabled/draining/uninitialized`.
- Colocación `balanced`/`camera_affinity`/`manual` con pesos, prioridades, fallback y persistencia de afinidad.
- Planificador y ejecutor de retención sobre índice SQLite real y archivos reales, incluida la carrera "export toma el segmento entre plan y borrado".
- Migraciones SQLite idempotentes en ruta con espacios y Unicode.
- API de salud y almacenamiento; CLI `storage check/init`, `serve`, `migrate`, `version`.

- Ingest RTSP: FFmpeg `-c copy` → MPEG-TS → corte propio en keyframes; cada segmento empieza con PAT/PMT + keyframe y decodifica solo (verificado con ffprobe para H.264 y H.265). Escritura temporal en `.tmp/` del mismo volumen, fsync, rename atómico, SHA-256.
- Reconexión exponencial con jitter, gaps explícitos (`stream_lost`, `no_volume`, `recorder_stopped`, `recorder_down`), conmutación a otro volumen ante error de disco (ST-04), reconciliación tras `kill -9`/corte de energía.
- Audio: copia si el contenedor lo admite; G.711 se detecta y se transcodifica solo el audio a AAC (video siempre copiado).
- Monitor de almacenamiento cada 30 s con probe de escritura y retención automática dentro de `serve`.
- Playback HLS de los segmentos TS sin transcodificar, con `EXT-X-DISCONTINUITY` en los gaps y tokens HMAC de 15 min. La clave se genera al arrancar: los tokens no sobreviven un reinicio.
- Export: job idempotente, verifica SHA-256 de cada segmento fuente, retiene los segmentos con hold mientras dura, corta con `-c copy` al keyframe anterior al inicio pedido (el clip puede empezar hasta un GOP antes) y entrega ZIP con `video.mp4` + `manifest.json`. Descargas y exports quedan en `audit_log`.
- API keys `ovr_…` (solo hash SHA-256 en archivo 0600), `X-API-Key` o `Bearer`, 429 tras 10 fallos/min por IP. `serve` no arranca fuera de loopback sin claves. `/metrics` Prometheus.
- Búsqueda de cámaras en la LAN (WS-Discovery multicast en todas las interfaces más barrido unicast opcional de la subred, hasta /22) y "Leer por ONVIF": modelo, firmware, perfiles y URLs RTSP principal/secundaria completados solos. Solo lectura, sin tocar la configuración de la cámara; limitado a `discovery.allowed_networks` (por defecto redes privadas), incluidas las direcciones de servicio que informa el equipo. Requiere ONVIF habilitado en la cámara y un usuario con permiso ONVIF. Probado con un servidor ONVIF simulado; **sin prueba todavía contra una cámara real**.
- Alta, edición y baja de cámaras desde la UI/API, con botón "Probar conexión" (códecs, resolución, error de clave/ruta/red). Los cambios se aplican en caliente: solo se reinicia la grabación de esa cámara. Las cámaras del YAML se muestran como solo lectura. Una cámara con grabaciones no se puede borrar, solo deshabilitar.
- UI web mínima en español (panel, cámaras, timeline, reproducción, export, almacenamiento), verificada con Chromium headless.
- `backup DEST` (copia consistente de SQLite con `VACUUM INTO`).
- Prueba de punta a punta manual con MediaMTX (RTSP con autenticación), 2 cámaras simuladas (H.264+AAC y H.265+G.711), 2 volúmenes, corte de cámara y `kill -9` del recorder.

## No implementado todavía

- Alta/edición de volúmenes por API/UI (hoy: archivo YAML).
- ONVIF: solo lectura para el alta (descubrimiento, info del equipo, perfiles y URLs RTSP). Faltan eventos (PullPoint), PTZ, cambio de códec desde la UI, drivers propietarios, microSD/Profile G.
- Importar canales de un NVR Provision-ISR (existe en openvms, `internal/provisionnvr`, todavía no copiado).
- El cliente ONVIF autentica con WS-Security (UsernameToken digest). No hace HTTP Digest: un equipo que solo acepte HTTP Digest en ONVIF aparece como `auth_failed`.
- Snapshots/miniaturas y vista en vivo en la UI; la reproducción usa HLS nativo del navegador (Safari/Chromium recientes). En Firefox la UI ofrece la URL para VLC.
- Las credenciales de cámaras agregadas por la UI se guardan en texto plano en `<data.dir>/camera-credentials.yaml`, protegido solo por permisos (0600 en Linux, ACL del directorio de datos en Windows). No hay cifrado en reposo.
- "Probar conexión" permite a quien tenga una API key hacer que el servidor abra conexiones RTSP a cualquier host (máximo 2 a la vez). La allowlist `discovery.allowed_networks` hoy aplica solo al probe ONVIF, no a la prueba RTSP.
- TLS, RBAC y allowlist de redes. Hay API keys, pero todas tienen el mismo permiso. **Mitigación:** sin TLS, exponer el puerto solo detrás de un proxy HTTPS o en red de confianza. La UI guarda la clave en `sessionStorage` y no usa cookies, por lo que no aplica CSRF.
- SMART, latencias de IO, estimación de días de retención.
- Restore de SQLite automatizado (hoy: detener servicio y reemplazar el archivo). Instaladores MSI/.deb firmados.

## Precisión y comportamiento a conocer

- Los tiempos de segmento son reloj del host (UTC) al recibir los paquetes, no el reloj de la cámara: precisión ≈ un GOP + jitter de red. Duración de segmento = objetivo … objetivo + 1 GOP.
- Si falla un disco a mitad de segmento, la sesión RTSP se reinicia para que el próximo segmento empiece en keyframe en otro volumen: pérdida típica 1–3 s, registrada como gap.
- En Windows FFmpeg se asigna a un job object con `KILL_ON_JOB_CLOSE`, así muere con el servicio.

## Validado solo parcialmente

- **Windows:** el código de plataforma (GUID de volumen, espacio libre, tipo de unidad, disco de sistema) compila y pasa `go vet`, pero **no se ejecutó en Windows**, tampoco el servicio ni `install.ps1`. Falta runner Windows en CI y pruebas NTFS/ReFS, cambio de letra real, ACL y servicio sin sesión (PRD §11).
- **Linux:** probado en ext4 dentro de contenedor. Faltan XFS, ZFS, mdadm, discos extraíbles reales y desconexión física.
- **Cuota de carpeta** (`FolderQuotaBytes`): soportada en el cálculo, pero no se detecta automáticamente en ningún SO todavía.

- **Docker:** Dockerfile y compose escritos, pero la imagen **no se construyó ni probó** en este entorno.
- **systemd:** unidad e instalador escritos, no ejecutados en un host con systemd real.

## Hardware

Ninguna capacidad de cámara está confirmada. La Provision I4-320IPSN-VF-V4 está pendiente de laboratorio. No se declara soporte de Profile G, microSD replay ni analíticas para ningún modelo.
