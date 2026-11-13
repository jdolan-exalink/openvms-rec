# Diseño de almacenamiento multivolumen (M04)

Implementación: `internal/storage/{volumes,placement,retention,health}`, `internal/segments`, `internal/platform`.

## 1. Identidad de volumen

Cada volumen configurado se resuelve a un `StableID`:

| SO | Fuente | Ejemplo |
|---|---|---|
| Linux | UUID del sistema de archivos vía `/dev/disk/by-uuid`; si no existe, `major:minor` del dispositivo | `uuid:3f1c…`, `dev:254:0` |
| Windows | GUID de volumen de `GetVolumeNameForVolumeMountPoint` | `guid:\\?\volume{…}\` |

Además, `storage init` / `POST …/validate` escribe en la raíz un **marcador** `.openvms-recorder-volume.json` con `volume_id` y `stable_id`:

- El marcador viaja con el disco: un cambio de letra o de punto de montaje sigue identificando el mismo volumen (ST-05).
- Un disco desmontado deja a la vista la carpeta vacía de debajo, **sin marcador**: Recorder no escribe ahí (evita llenar el disco del sistema por un montaje caído).
- Otro volumen de Recorder montado en la ruta → `missing`.
- Se rechaza inicializar una carpeta no vacía sin marcador.
- Dos volúmenes configurados con el mismo `StableID` → el segundo queda `degraded` para no contar dos veces la capacidad (ST-02).

Se informan `is_system`, `is_network` (NFS/SMB/unidad remota) e `is_removable`. Los de sistema y red no reciben escrituras salvo `allow_system_volume` / `allow_network_volumes`.

## 2. Límite efectivo

Todos los valores en bytes de datos de Recorder sobre el volumen (`RecorderBytes`, sumados desde el índice, no recorriendo carpetas).

```
efectivo = min(
  capacidad × max_percent / 100,          # 90 % por defecto, configurable por volumen
  quota_bytes,                            # cuota absoluta, si existe
  capacidad − min_free,                   # reserva
  recorder_bytes + libre − min_free,      # espacio realmente disponible
  cuota_de_carpeta                        # si es verificable
)
```

`libre` es lo que la cuenta de servicio puede asignar (`f_bavail` / `lpFreeBytesAvailable`), por lo que ya descuenta bloques reservados a root y cuotas de usuario NTFS.

El término "espacio realmente disponible" protege volúmenes compartidos: si otro software ocupa el disco, el límite baja en lugar de invadir la reserva. Si otro software ya invadió la reserva, el límite queda por debajo de lo que Recorder tiene y la limpieza empieza de inmediato.

La API devuelve `binding_constraint` para explicar qué regla manda.

### Ejemplos (tests en `limits_test.go`)

| Caso | Capacidad | Ocupado propio | Libre | min_free | Efectivo | Regla |
|---|---|---|---|---|---|---|
| Disco dedicado | 4 TB | 0 | 4 TB | 100 GB | 3,6 TB | 90 % |
| Disco chico | 500 GB | 0 | 500 GB | 100 GB | 400 GB | reserva |
| Compartido | 2 TB | 200 GB | 600 GB | 100 GB | 700 GB | disponible |
| Reserva invadida | 1 TB | 300 GB | 50 GB | 100 GB | 250 GB | disponible |

## 3. Histéresis

Umbrales globales `cleanup_start_percent` (87) y `cleanup_target_percent` (84) expresados respecto de `default_max_percent` (90), y aplicados **proporcionalmente** al límite efectivo de cada volumen:

```
inicio  = efectivo × 87/90
objetivo = efectivo × 84/90
```

Con límite 90 % caen exactamente en 87 % y 84 % de la capacidad; un volumen al 80 % queda en 77,3 % / 74,7 %. Escalar (en vez de restar puntos fijos) garantiza `objetivo < inicio ≤ efectivo` para cualquier límite, incluido un volumen compartido muy recortado. Al cruzar `inicio` la limpieza baja hasta `objetivo` de una vez.

Estados: `full` cuando `recorder_bytes ≥ efectivo` (no recibe segmentos nuevos hasta que la limpieza libere espacio).

## 4. Retención (`retention.PlanVolume`)

1. **Nunca** se borran segmentos protegidos (`protection_until` futuro), con hold activo (export/migración) o en escritura.
2. **Edad:** se borran los que superan la edad máxima de su cámara/representación.
3. **Espacio:** si se cruzó `inicio`, se borran los más antiguos elegibles hasta `objetivo`.
   - Primero los que ya superaron la retención mínima de su cámara.
   - Si no alcanza, según `pressure_policy`:
     - `overwrite_oldest_unprotected` (por defecto): borra dentro de la retención mínima, cámaras de menor prioridad primero, y emite `min_retention_violated` por cámara.
     - `protect_minimum_retention`: no borra; emite `retention_pressure` y el volumen se llena y deja de recibir escrituras (la colocación lo saltea).
4. Si solo quedan datos protegidos por encima del objetivo: alerta `protected_data_exceeds_target`. La evidencia nunca se borra en silencio.

**Ejecución** (`retention.Execute` + `segments.Store`): por cada segmento, en una sola sentencia SQL se re-verifica que siga sin protección ni hold y se marca `deleting`; luego se borra el archivo (si ya no existe, cuenta como borrado); luego `deleted`. Una protección o export que aparezca entre el plan y la ejecución gana. Un corte entre pasos deja filas `deleting` que la reconciliación de arranque termina.

## 5. Volumen con fallas

`health.Probe` escribe, sincroniza (fsync), relee y borra un archivo temporal. Una raíz inexistente, de solo lectura o con error de IO deja el volumen `degraded`/`missing`; la colocación deja de elegirlo. Recorder no formatea, no inicializa (salvo la acción explícita sobre una carpeta vacía) y no repara.

Los metadatos de segmentos de un volumen ausente **se conservan**; las consultas de rango los devuelven y el timeline los marcará `available: false`.

## 6. Pendiente en próximas fases

- Monitor periódico (inspección + probe + plan de retención) dentro de `serve`.
- Alta/edición de volúmenes por API/UI (hoy por archivo de configuración).
- SMART sin privilegios inseguros, latencia de IO, tasa de escritura y días estimados de retención por volumen (requiere bitrate real del ingest).
- Reconciliación de arranque: archivos temporales, filas `writing`/`deleting`, volúmenes ausentes.
- Jobs de migración/rebalanceo con checksum.
