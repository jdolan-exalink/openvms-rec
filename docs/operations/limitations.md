# Informe de limitaciones

Actualizado con cada entrega. Última: **PR 1 — base + almacenamiento multivolumen** (2026-11-12).

## Implementado y probado (tests automáticos)

- Cálculo de límite efectivo, histéresis y fórmulas de capacidad del PRD §7.
- Identidad de volumen con marcador, cambio de ruta/letra, disco equivocado montado, duplicados, estados `healthy/full/degraded/missing/disabled/draining/uninitialized`.
- Colocación `balanced`/`camera_affinity`/`manual` con pesos, prioridades, fallback y persistencia de afinidad.
- Planificador y ejecutor de retención sobre índice SQLite real y archivos reales, incluida la carrera "export toma el segmento entre plan y borrado".
- Migraciones SQLite idempotentes en ruta con espacios y Unicode.
- API de salud y almacenamiento; CLI `storage check/init`, `serve`, `migrate`, `version`.

## No implementado todavía

- Ingest RTSP, segmentación, reconexión (PR 2).
- Monitor periódico de almacenamiento y ejecución automática de retención dentro de `serve`.
- Alta/edición de volúmenes y cámaras por API/UI (hoy: archivo YAML).
- ONVIF, drivers, microSD/Profile G, eventos, timeline, playback, export, UI.
- Autenticación, TLS, RBAC, CSRF, allowlist de redes. **Mitigación actual:** `serve` escucha en `127.0.0.1` por defecto y solo hay un endpoint mutante (`validate`, que escribe un marcador en una carpeta vacía). No exponer el puerto a la red hasta que llegue la autenticación.
- `/metrics`, SMART, latencias de IO, estimación de días de retención.
- Servicio Windows, systemd, Docker, instaladores, backup/restore de SQLite.

## Validado solo parcialmente

- **Windows:** el código de plataforma (GUID de volumen, espacio libre, tipo de unidad, disco de sistema) compila y pasa `go vet`, pero **no se ejecutó en Windows**. Falta runner Windows en CI y pruebas NTFS/ReFS, cambio de letra real, ACL y servicio sin sesión (PRD §11).
- **Linux:** probado en ext4 dentro de contenedor. Faltan XFS, ZFS, mdadm, discos extraíbles reales y desconexión física.
- **Cuota de carpeta** (`FolderQuotaBytes`): soportada en el cálculo, pero no se detecta automáticamente en ningún SO todavía.

## Hardware

Ninguna capacidad de cámara está confirmada. La Provision I4-320IPSN-VF-V4 está pendiente de laboratorio. No se declara soporte de Profile G, microSD replay ni analíticas para ningún modelo.
