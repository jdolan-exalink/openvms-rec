# ADR-001 — Grabación, streams y envejecimiento de datos

- **Estado:** aceptado (PRD v1.1, §16)
- **Fecha:** 2026-11-12

## Contexto

Recorder debe grabar continuamente sin depender de analíticas, minimizar CPU (sin transcodificar) y permitir más adelante retener más tiempo en baja calidad.

## Decisión

- Modo predeterminado `continuous_with_event_markers`: se graba siempre; los eventos solo marcan el timeline y pueden proteger segmentos. El modo "solo por eventos" queda fuera del MVP.
- Por defecto se graba **solo el stream principal**. El substream se usa para mosaicos en vivo y acceso de bajo ancho de banda, sin guardarse.
- `dual_recording` (opcional, por cámara): main y sub se graban como **representaciones independientes** (`representation_id` = `main` / `sub`), con segmentos y manifiestos separados, sincronizados por tiempo UTC.
- Envejecimiento (arquitectura desde el día uno, implementación posterior al MVP): p. ej. main 7 días, sub 30 días. Borrar una representación no rompe la otra porque no comparten archivos. Los eventos protegidos retienen el main durante su política de evidencia.
- **Nunca** "aligerar" un archivo H.264/H.265 borrando cuadros arbitrarios: GOP, P/B frames y referencias lo vuelven indecodificable. Tampoco repartir cuadros entre discos (p. ej. 3/3/4): sin codificación independiente ni paridad, perder un disco arruina el resto.

## Modelo de segmento

Implementado en `migrations/0001_init.sql` (tabla `segments`) e `internal/segments`:

| Campo | Notas |
|---|---|
| `camera_id`, `representation_id` | `main` / `sub` |
| `start_utc`, `end_utc` | ms UTC; `end_utc` NULL mientras se escribe |
| `codec`, `keyframe_info` | keyframes en JSON para cortes de export |
| `volume_id`, `relative_path` | ubicación exacta; `relative_path` con `/` |
| `size_bytes`, `checksum` | SHA-256 |
| `status` | `writing`, `final`, `corrupt`, `deleting`, `deleted` |
| `protection_until` | evidencia / fijado por usuario; solo se extiende, nunca se acorta |
| `source` | `live_ingest`, `recovered_sd`, `remote_only` (ADR-003) |
| `generation` | para migraciones y re-indexado |

Los holds de exportación/migración están en `segment_holds` y bloquean el borrado mientras duren.

## Alternativas para investigar (no MVP)

Códecs escalables (SVC), GOP independientes, representaciones por capas y transcodificación diferida. Requieren benchmark real de CPU/GPU, bitrate y compatibilidad. No se promete ahorro fijo.

## Consecuencias

- Grabación dual duplica conexiones RTSP y escritura; se habilita por cámara y solo si el substream es adecuado.
- `low_days` no tiene efecto con `dual_recording: false`.
