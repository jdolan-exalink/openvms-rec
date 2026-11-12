# Informe de limitaciones

Actualizado con cada entrega. Última: **MVP en curso — ingest RTSP** (2026-11-12).

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
- Prueba de punta a punta manual con MediaMTX (RTSP con autenticación), 2 cámaras simuladas (H.264+AAC y H.265+G.711), 2 volúmenes, corte de cámara y `kill -9` del recorder.

## No implementado todavía

- Alta/edición de volúmenes y cámaras por API/UI (hoy: archivo YAML).
- ONVIF, drivers, microSD/Profile G, eventos, timeline, playback, export, UI.
- Autenticación, TLS, RBAC, CSRF, allowlist de redes. **Mitigación actual:** `serve` escucha en `127.0.0.1` por defecto y solo hay un endpoint mutante (`validate`, que escribe un marcador en una carpeta vacía). No exponer el puerto a la red hasta que llegue la autenticación.
- `/metrics`, SMART, latencias de IO, estimación de días de retención.
- Servicio Windows, systemd, Docker, instaladores, backup/restore de SQLite.

## Precisión y comportamiento a conocer

- Los tiempos de segmento son reloj del host (UTC) al recibir los paquetes, no el reloj de la cámara: precisión ≈ un GOP + jitter de red. Duración de segmento = objetivo … objetivo + 1 GOP.
- Si falla un disco a mitad de segmento, la sesión RTSP se reinicia para que el próximo segmento empiece en keyframe en otro volumen: pérdida típica 1–3 s, registrada como gap.
- En Windows FFmpeg termina por pipe roto si el servicio muere; el job object del servicio nativo llega con el instalador.

## Validado solo parcialmente

- **Windows:** el código de plataforma (GUID de volumen, espacio libre, tipo de unidad, disco de sistema) compila y pasa `go vet`, pero **no se ejecutó en Windows**. Falta runner Windows en CI y pruebas NTFS/ReFS, cambio de letra real, ACL y servicio sin sesión (PRD §11).
- **Linux:** probado en ext4 dentro de contenedor. Faltan XFS, ZFS, mdadm, discos extraíbles reales y desconexión física.
- **Cuota de carpeta** (`FolderQuotaBytes`): soportada en el cálculo, pero no se detecta automáticamente en ningún SO todavía.

## Hardware

Ninguna capacidad de cámara está confirmada. La Provision I4-320IPSN-VF-V4 está pendiente de laboratorio. No se declara soporte de Profile G, microSD replay ni analíticas para ningún modelo.
