# Yoga Chain for Veeam

> [!CAUTION]
> # 🚧 FASE ALPHA — PROTOTIPO EXPERIMENTAL 🚧
> **Este proyecto está en fase *alpha*. NO es apto para producción.**
> - Es un prototipo en desarrollo activo: interfaces, endpoints y comportamiento **pueden cambiar o romperse** sin aviso.
> - **Solo lectura** contra la REST API de VBR: no crea, modifica ni borra nada.
> - **Sin autenticación** en la consola y **credenciales en memoria** — por defecto escucha solo en `127.0.0.1`. No lo expongas en una red no confiable.
> - No es un producto de Veeam ni tiene soporte oficial. **Uso bajo tu propia responsabilidad.**

*for VBR to know how are your chains made.*

Herramienta para que el administrador de backup entienda **qué restore points tiene, de qué
tipo y en dónde**, leyendo la REST API de Veeam Backup & Replication (v12.3 / v13). Un solo
binario (Windows, Linux, macOS) que sirve la consola web y la API en el mismo puerto: el
usuario lo ejecuta en su laptop/workstation y se abre el navegador. Sin agentes, sin acceso
a la base de datos de VBR, sin instalar nada.

Complementa a Veeam ONE, que muestra agregados (cantidad de RP, RPO) y reportes estáticos:
acá la cadena se explora de forma interactiva desde tres pivots — **Job**, **Workload** y
**Repositorio** — con una línea de tiempo por carril. Hermano de
[Yoga Benchmark](https://github.com/martinljor/yogabench): misma arquitectura, mismo estilo.

## Qué muestra

| Vista | Qué responde |
|---|---|
| **Tipo** | Full activo / sintético / incremental / GFS / copia / tape, huecos vs. RPO, malware, inmutabilidad |
| **Eficiencia** | Reducción total por RP (ámbar → verde), `compressRatio` y `dedupRatio` de `backupFiles`, datos origen vs. en disco |
| **Aplicación** | Consistencia de aplicación por RP (derivada de los logs de la task session), cadencia de log backups (SQL / Oracle / PostgreSQL) y qué opciones de restore habilita cada RP |

Panel de detalle por RP: archivo, cadena y cuántos archivos hay que leer para restaurarlo,
dónde más existe ese dato (copia S3, tape), configuración `appAwareProcessing` del job
(`vss`, `sql.logsProcessing`, `oracle.archiveLogs`, …) y `allowedOperations`.

## Fases

1. **VMs, solo lectura, un VBR** ← esta
2. Bases de datos (Oracle, SQL) en profundidad; llamar al menú de restore (Instant Recovery / FLR vía REST; Explorers desde consola)
3. Resto de workloads (agentes, NAS, cloud). Unificar UI con Yoga Benchmark.

## Uso (usuario final)

1. Descargar el binario de tu SO desde *Releases* (`yogachain-windows-amd64-vX.Y.Z.zip`, `yogachain-linux-amd64-vX.Y.Z`, `yogachain-darwin-arm64-vX.Y.Z`). Verificar con `SHA256SUMS.txt`.
2. Ejecutarlo. Se abre `http://localhost:8001` en el navegador (Yoga Benchmark usa el 8000, pueden convivir).
3. **Conectar** a un VBR con un usuario con rol **Backup Viewer** (alcanza para todo lo de la fase 1), o **Demo** para ver la consola sin VBR.

Flags: `-port 8001`, `-bind 127.0.0.1` (solo cambiar en un lab aislado), `-no-browser`, `-log yogachain.log` (vacío = solo consola), `-debug` (default true; nunca loguea passwords ni tokens).

Windows: si el navegador bloquea el `.exe` suelto, usar el `.zip`. En Linux/macOS: `chmod +x`.

## Desarrollo

Requiere Go 1.26+ **solo para compilar**.

```bash
./run.sh                 # go run . (abre el navegador)
go test ./...            # tests contra la sesion demo (mismo mapeo que en prod)
./build.sh               # binarios en dist/ para linux/windows/macos + zip + SHA256SUMS
```

Fuente **Open Sans**: copiar `frontend/opensans.woff2` desde yogabench antes de compilar (se embebe en
el binario). Si falta, la consola cae a la fuente del sistema.

## Estructura

```
yogachain/
├── main.go                 # flags, banner, abre el navegador, ListenAndServe
├── web.go                  # //go:embed frontend
├── build.sh                # cross-compile linux/windows/macos -> dist/
├── frontend/
│   └── index.html          # consola web: un solo archivo, sin frameworks ni CDN, i18n es/en/pt
├── internal/
│   ├── dbg/                # logging de depuracion gateado por --debug
│   ├── vbr/                # sesiones, cliente REST (OAuth2 + refresh, cache, single-flight), demo
│   ├── chain/              # REST de VBR -> modelo normalizado: inventario, restore points,
│   │                       #   cadena/ubicaciones, application-aware (+ tests)
│   └── server/             # mux + handlers; sirve el frontend embebido
└── docs/API-MAPPING.md     # endpoint -> campo, con las limitaciones encontradas en la API
```

## API del backend

| Método | Ruta | Descripción |
|---|---|---|
| GET | `/health` | `{ok, active_sessions, version}` |
| POST | `/api/connect` | `{host, port, username, password, api_version, verify_ssl}` → `{session_id, expires_in, api_version}`. `api_version: "auto"` negocia 1.3-rev2 → 1.3-rev1 → 1.3-rev0 → 1.2-rev1 |
| POST | `/api/connect-demo` | sesión con datos simulados (JSON con la forma real de VBR) |
| POST | `/api/{session}/disconnect` | cierra la sesión |
| GET | `/api/{session}/inventory` | jobs (storage, GFS, guest processing), workloads, repositorios |
| POST | `/api/{session}/inventory/refresh` | descarta el inventario cacheado y lo recarga |
| GET | `/api/{session}/restore-points?pivot=job\|vm\|repo&id=…&days=30&skip=0&limit=100[&aaip=1]` | RPs normalizados, paginados de a 100 |
| GET | `/api/{session}/restore-points/{rp}` | detalle: RP + resultado AAIP + cadena + otras ubicaciones |
| GET | `/api/{session}/raw/{path...}` | passthrough de lectura a la REST API (ej. `raw/v1/backups`) para inspeccionar el schema real |

## Pendientes / a validar en lab (`// LAB` en el código)

- Identidad del workload entre backups (primario, copia, tape): hoy `BackupObjectModel.objectId`, fallback nombre.
- Full **sintético vs. activo**: la API da `Increment|Full`; `taskSession.algorithm = Synthetic` se consulta solo en el detalle.
- Escala de `dedupRatio` / `compressRatio` (factor vs. porcentaje) en `BackupFileModel`.
- Inmutabilidad por RP: no viene en el RP; se deriva de `creationTime + repo.immDays` (hardened / object lock).
- Patrones de texto en `/taskSessions/{id}/logs` para clasificar guest processing (VSS, Oracle, SQL, PostgreSQL).
- `EJobType` reales para backup copy / tape / agentes; `backupRepositoryId` en jobs de copia.
- Backups del **plug-in RMAN / SAP HANA / SQL plug-in**: no aparecen como RP de VM (`platformName = ApplicationBackupRepository`). Afecta la fase 2.
- Certificado self-signed: `verify_ssl=false` por defecto. CORS abierto (el frontend se sirve del mismo origen).
- `aaip=1` contra un VBR real: 2–3 llamadas por RP → cachear por `sessionId`.
