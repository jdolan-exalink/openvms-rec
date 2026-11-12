# ADR-002 — Resiliencia de disco y distribución

- **Estado:** aceptado (PRD v1.1, §17); implementado en PR 1 (`internal/storage`)
- **Fecha:** 2026-11-12

## Contexto

Las instalaciones típicas tienen varios discos (D:, E:, F: o `/srv/cctv-*`). Se pide repartir grabaciones, respetar una cuota del 90 % por volumen y seguir grabando si un disco falla. Distribuir no es lo mismo que proteger.

## Decisión

### Disponibilidad vs. durabilidad

- **Disponibilidad:** si falla un volumen, Recorder lo marca `degraded`/`missing`, deja de escribir ahí, sigue grabando en los volúmenes sanos con espacio, alerta y conserva el índice de todo lo demás. Si no queda destino, registra un gap.
- **Durabilidad:** si no se puede perder ninguna grabación ante una falla de disco, se exige redundancia **por debajo** de Recorder: RAID 1/5/6/10, Storage Spaces (Windows), ZFS/mdadm (Linux) o copias protegidas independientes. Recorder **no** implementa RAID propio ni crea/destruye pools. RAID no reemplaza backups ni protege contra borrado lógico o ransomware.
- Tres letras de unidad no equivalen a tres discos físicos; un volumen RAID puede contener varios. Nunca se garantiza tolerancia a fallas por la cantidad de volúmenes. La API siempre devuelve la advertencia `no_redundancy_assumed`.

### Unidad de distribución

Se reparten **segmentos completos y decodificables**. Cada segmento registra `volume_id` y `relative_path`. Nunca se reparten cuadros entre discos.

### Políticas de colocación (`internal/storage/placement`)

| Política | Comportamiento |
|---|---|
| `balanced` | Round-robin ponderado suave (estilo nginx) entre volúmenes aptos: respeta pesos exactamente y evita ráfagas sobre un disco. |
| `camera_affinity` | La cámara usa sus volúmenes asignados y se mantiene en el último elegido (persistible). Si todos sus asignados fallan, cae a `balanced` y marca `Fallback` para alertar. Sin asignación: afinidad pegajosa elegida por `balanced`. |
| `manual` | Solo los volúmenes asignados. Sin destino apto → error `ErrNoEligibleVolume` y gap; nunca escribe en otro lado. |

`priority` define niveles: los volúmenes de menor valor se llenan primero; un nivel inferior solo recibe datos cuando ninguno del nivel superior es apto. Apto = estado `healthy`, habilitado, no drenando, con margen bajo el límite efectivo y, si es disco de sistema o de red, permitido explícitamente por configuración.

### Recuperación

- Al reaparecer un volumen se reindexa y valida checksums (job posterior). No se sobrescriben segmentos recuperados en otro disco sin conciliación.
- Rebalanceo, migración y reparación corren como jobs en background con prioridad inferior al ingest, con checksum y conmutación atómica de referencias.

### Plataformas

- Windows: Storage Spaces/RAID de hardware se tratan como un volumen montado más. Identidad por GUID de volumen (`\\?\Volume{...}\`).
- Linux: ZFS/mdadm/RAID de hardware montados; ZFS no es dependencia. Identidad por UUID de sistema de archivos (o número de dispositivo si no hay UUID).

## Consecuencias

- En instalaciones con discos independientes la pérdida de un disco destruye sus segmentos; la UI debe mostrarlo.
- Las pruebas de aceptación ST-02/03/04/05/08 dependen de este diseño; ST-08 (RAID degradado) requiere laboratorio real.
