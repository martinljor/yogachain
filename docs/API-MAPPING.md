# Mapeo REST API de VBR → modelo de YogaChain

Referencia validada: VBR 13, REST API **1.3-rev2**
(https://helpcenter.veeam.com/references/vbr/13/rest/1.3-rev2/). Nombres de campos exactos según la spec OpenAPI.
Todos los endpoints listan el rol **Backup Viewer** como suficiente.

Paginado: `skip`, `limit`, `orderColumn`, `orderAsc`; respuesta `{data:[…], pagination:{total,count,skip,limit}}`.
Header obligatorio: `x-api-version: 1.3-rev2`. Login: `POST /api/oauth2/token` (grant_type=password).

## Inventario

| Modelo | Endpoint | Campos usados |
|---|---|---|
| `Job` | `GET /api/v1/jobs/states` (lista; fallback `/jobs`, luego `/backups`) + `GET /api/v1/jobs/{id}` (detalle, tolerante a 500) | `name`, `type`, `storage.backupRepositoryId`, `storage.gfsPolicy{weekly,monthly,yearly}`, `storage.advancedSettings.storageData{compressionLevel, storageOptimization, inlineDataDedupEnabled, encryption.isEnabled}`, `schedule` |
| `Job.aaip` | `GET /api/v1/jobs/{id}` | `guestProcessing.appAwareProcessing{isEnabled, appSettings[]{vmObject, vss, usePersistentGuestAgent, transactionLogs, sql{logsProcessing, backupMinsCount, retainLogBackups, keepDaysCount}, oracle{useGuestCredentials, credentialsId, archiveLogs, deleteHoursCount, deleteGBsCount, backupLogs, backupMinsCount, retainLogBackups, keepDaysCount}, postgreSQL{backupLogs, backupMinsCount, …}}}`, `guestProcessing.guestFSIndexing.isEnabled` |
| `Workload` | `GET /api/v1/backupObjects` + **`GET /api/v1/backups/{id}/objects`** | `id`, `objectId`, `name`, `type`, `platformName`, `restorePointsCount`, `size`. **FIELD:** `backupObject.backupId` no coincide con el `backupId` de sus RPs; la relación job↔objeto sale de `/backups/{id}/objects` |
| `Repo` | `GET /api/v1/backupInfrastructure/repositories` + `/repositories/states` + `/scaleOutRepositories` (SOBR no están en states) | `type`, `bucket.immutability{isEnabled, daysCount, immutabilityMode}`, `repository.makeRecentBackupsImmutableDays`; states: `capacityGB`, `freeGB`, `usedSpaceGB`, `isOnline` |
| backup → job/repo | `GET /api/v1/backups` | `id`, `jobId`, `repositoryId`, `platformName`, `creationTime` |
| agregado por backup | `POST /api/v1/backups/{id}/details` (lectura) | `objectOriginalSize`, `backupSize`, `actualSize`, `restorePointsCount` |

## Restore points

| Campo YogaChain | Origen |
|---|---|
| `id`, `date`, `backupId`, `backupFileId`, `allowedOperations`, `mal` | `GET /api/v1/restorePoints?backupIdFilter=…&backupObjectIdFilter=…&createdAfterFilter=…` → `id`, `creationTime`, `backupId`, `backupFileId`, `allowedOperations[]`, `malwareStatus` (Clean/Suspicious/Infected/Informative) |
| `type` | `restorePoint.type` (Increment/Full/Rollback/Snapshot/Cdp/Different) + `job.type` (BackupCopy → copy, BackupToTape → tape). Sintético vs. activo: `taskSession.algorithm` (Full/Increment/Synthetic) |
| `gfs` | `BackupFileModel.gfsPeriods[]` (None/Weekly/Monthly/Quarterly/Yearly) — en el RP `gfsPeriods` está documentado solo para NAS |
| `dataSize`, `sizeGB`, `compressRatio`, `dedupRatio`, `file` | **`GET /api/v1/backups/{id}/backupFiles`** → `dataSize`, `backupSize`, `dedupRatio`, `compressRatio`, `name`, `restorePointIds[]`, `objectIds[]`, `severity`. Join por `restorePoint.backupFileId`. **FIELD:** los ratios son el % del tamaño que queda tras cada etapa (65 · 93 → 61 % de `dataSize`); factor = 100/valor; 0 = sin dato |
| `repoId` | `backup.repositoryId` (el RP **no** trae repositorio) |
| `immUntil` | **No existe en la API.** Derivar: `creationTime + repo.immDays` |
| `vmId` | Se consulta `GET /backupObjects/{id}/restorePoints` por cada backupObject del workload (identidad unificada por `BackupObjectModel.objectId`), asi el RP queda atado a su VM sin ambiguedad. Para un RP puntual: `BackupFileModel.objectIds[]` |
| `aaip`, `aaipDetail` | **No existe flag por RP.** `restorePoint.sessionId` → `GET /sessions/{id}/taskSessions` (buscar `restorePointId`) → `GET /taskSessions/{id}/logs` → `status` Succeeded/Warning/Failed + `title` |
| log backups | `GET /api/v1/sessions?typeFilter=SqlLogBackup|OracleLogBackup|PostgreSqlLogBackup&jobIdFilter=…` |
| malware | `GET /api/v1/malwareDetection/events`, `/malwareDetection/backupObjects` (nuevo en 1.3) |

## Limitaciones encontradas

- `allowedOperations` solo incluye operaciones de VM/disco/FLR/réplica (`StartViVMInstantRecovery`, `StartEntireVmRestore`, `StartFlrRestore`, `StartDiskPublish`, failover/failback…). **Restores de aplicación (Veeam Explorers SQL/Oracle/AD/PostgreSQL) no están en REST**; solo `GET /restore/applicationItems/{browseSessionId}/childSessions` lista sesiones ya ejecutadas.
- Sesiones exponen `processedSize/readSize/transferredSize`, no ratios. Repositorios exponen capacidad, no ratios.
- Backups de plug-in (RMAN, SAP HANA, SQL plug-in): `EPlatformType.ApplicationBackupRepository`; no aparecen como RP de VM. El estado del plug-in está en `/agents` (`DiscoveredComputerPluginModel.type`: MSSQL, OracleRMAN, SAPHANA, SAPOnOracle).
- `BackupObjectModel.type` es string libre (sin enum).
- No hay endpoint de "RPO": el RPO por job se configura en la tool.
- **FIELD (vbrdb-01):** `GET /jobs` → 500 `Source item ... is not part of 'Clusters' hierarchy` cuando hay jobs de aplicación (plug-ins RMAN/HANA/MongoDB/SQL). `/jobs/states` no expande objetos y sí los lista (tipo `Unknown`).
- **FIELD (cdp):** un VBR solo con políticas CDP devuelve 0 jobs y 0 backups: CDP y réplicas van por `/replicas` y `/cdp` (fase 3).

## Retención GFS

| Qué | Origen |
|---|---|
| Punto GFS y nivel | `BackupFileModel.gfsPeriods[]` (Weekly / Monthly / Quarterly / Yearly) del archivo del restore point |
| Política | `job.storage.gfsPolicy.{weekly.keepForNumberOfWeeks, monthly.keepForNumberOfMonths, yearly.keepForNumberOfYears}` |
| Vencimiento | **No lo expone la API.** Estimado = `creationTime` + retención del nivel. |
| Períodos esperados / faltantes | Calculados por la tool: últimos N períodos ISO (semana / mes / año) contra los puntos existentes; solo en pivot Job (una política) |

## Backups de plug-in (FIELD vbrdb-01)

| Qué | Cómo lo expone la REST API |
|---|---|
| Backup | `/backups`: `platformName: CustomPlatform`, `jobType: Unknown`, `name: "<host> <App> backup (<repo>)"`, `creationTime: 0001-01-01`. No está en `/jobs` ni `/jobs/states`. |
| Objeto | `/backupObjects` y `/backups/{id}/objects`: `type: Directory`, `restorePointsCount: 0`, `size: 0`, sin `objectId`. |
| Restore points | `/backupObjects/{id}/restorePoints` → **vacío**. |
| Archivos | `/backups/{id}/backupFiles`: un `.vab` por backup piece/canal, con `creationTime`, `dataSize`, `backupSize`, `compressRatio`, `dedupRatio`, `objectIds: []`. |
| Sesiones | pendiente de validar (`/sessions?jobIdFilter=`); el diagnóstico las muestrea. |
| Modelo YogaChain | `RestorePoint{FromFile: true, Type: "file", ID: "bf:<fileId>"}` por archivo; detalle con la cadena = todos los archivos del backup. |

Consecuencia para la fase 2 (Oracle): la REST API no distingue full / incremental / archive log en un backup de plug-in. Para eso hace falta el catálogo RMAN (`LIST BACKUP` en el servidor) o los logs del plug-in; la tool puede mostrar cadencia, tamaño y eficiencia por pieza, pero no point-in-time.

## Implementación

El mapeo vive en `internal/chain/` (Go): `inventory.go` (jobs, workloads, repos), `restorepoints.go` (RP + backupFiles), `aaip.go` (guest processing desde logs), `detail.go` (cadena + ubicaciones). `internal/vbr/demo.go` responde estos mismos endpoints con JSON simulado para desarrollo y tests.

## Endpoints de fase 2 (escritura, requieren rol mayor)

- `POST /api/v1/restore/instantRecovery/vSphere/vm` (Instant Recovery)
- `POST /api/v1/restore/flr` (mount FLR) + `DELETE` para desmontar
- `POST /api/v1/restore/entireVm/vSphere`
- SureBackup: `GET /api/v1/jobs` tipo SureBackup + `POST /jobs/{id}/start`
