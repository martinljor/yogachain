# Yoga Chain for Veeam

> [!CAUTION]
> # 🚧 ALPHA — EXPERIMENTAL PROTOTYPE 🚧
> **This project is in *alpha*. It is NOT production-ready.**
> - It is a prototype under active development: interfaces, endpoints and behavior **may change or break** without notice.
> - **Read-only** against the VBR REST API: it never creates, modifies or deletes anything.
> - **No authentication** on the console and **credentials kept in memory** — it listens on `127.0.0.1` by default. Do not expose it on an untrusted network.
> - Not a Veeam product, no official support. **Use at your own risk.**

*for VBR to know how are your chains made.*

A tool for the backup administrator to understand **which restore points exist, of what type
and where**, reading the Veeam Backup & Replication REST API (v12.3 / v13). A single binary
(Windows, Linux, macOS) that serves the web console and the API on the same port: run it on
your laptop/workstation and the browser opens. No agents, no access to the VBR database,
nothing to install.

It complements Veeam ONE, which shows aggregates (restore point counts, RPO) and static
reports: here the chain is explored interactively from three pivots — **Job**, **Workload**
and **Repository** — on a per-lane timeline. Sibling of
[Yoga Benchmark](https://github.com/martinljor/yogabench): same architecture, same style.

## What it shows

| View | What it answers |
|---|---|
| **Type** | Active full / synthetic full / incremental / GFS / copy / tape, gaps vs. RPO, malware, immutability |
| **Efficiency** | Total reduction per restore point (amber → green), `compressRatio` and `dedupRatio` from `backupFiles`, source data vs. on-disk size |
| **Application** | Application consistency per restore point (derived from the task session logs), log backup cadence (SQL / Oracle / PostgreSQL) and which restore options each point enables |
| **Retention** | GFS points per tier (weekly / monthly / yearly) with the estimated expiry from the job policy, points expiring within 30 days, and the periods that are missing their GFS point |

Detail panel per restore point: backup file, chain and how many files a restore has to read,
where else the same data exists (S3 copy, tape), the job's `appAwareProcessing` settings
(`vss`, `sql.logsProcessing`, `oracle.archiveLogs`, …) and `allowedOperations`.

## Phases

1. **VMs, read-only, one VBR** ← this one
2. Databases (Oracle, SQL) in depth; call the restore menu (Instant Recovery / FLR via REST; Explorers from the console)
3. Remaining workloads (agents, NAS, cloud). Unify the UI with Yoga Benchmark.

## Usage (end user)

1. Download the binary for your OS from *Releases* (`yogachain-windows-amd64-vX.Y.Z.zip`, `yogachain-linux-amd64-vX.Y.Z`, `yogachain-darwin-arm64-vX.Y.Z`). Verify with `SHA256SUMS.txt`.
2. Run it. `http://localhost:8001` opens in the browser (Yoga Benchmark uses 8000; both can run side by side).
3. **Connect** to a VBR with a user in the **Backup Viewer** role (enough for everything in phase 1), or **Demo** to see the console without a VBR.

Flags: `-port 8001`, `-bind 127.0.0.1` (change only in an isolated lab), `-no-browser`, `-log yogachain.log` (empty = console only), `-debug` (default true; never logs passwords or tokens).

Windows: if the browser blocks the bare `.exe`, use the `.zip`. Linux/macOS: `chmod +x`.

## Development

Requires Go 1.26+ **only to build**.

```bash
./run.sh                 # go run . (opens the browser)
go test ./...            # tests against the demo session (same mapping code as production)
./build.sh               # binaries in dist/ for linux/windows/macos + zip + SHA256SUMS
```

**Open Sans** font: copy `frontend/opensans.woff2` from yogabench before building (it is embedded
in the binary). If missing, the console falls back to the system font.

## Layout

```
yogachain/
├── main.go                 # flags, banner, opens the browser, ListenAndServe
├── web.go                  # //go:embed frontend
├── build.sh                # cross-compile linux/windows/macos -> dist/
├── frontend/
│   └── index.html          # web console: single file, no frameworks or CDN, i18n en/es/pt
├── internal/
│   ├── dbg/                # debug logging gated by --debug
│   ├── vbr/                # sessions, REST client (OAuth2 + refresh, cache, single-flight), demo, REST trace
│   ├── chain/              # VBR REST -> normalized model: inventory, restore points,
│   │                       #   chain/locations, application-aware (+ tests)
│   └── server/             # mux + handlers; serves the embedded frontend; diagnostics bundle
└── docs/API-MAPPING.md     # endpoint -> field, with the API limitations found
```

## Backend API

| Method | Route | Description |
|---|---|---|
| GET | `/health` | `{ok, active_sessions, version}` |
| POST | `/api/connect` | `{host, port, username, password, api_version, verify_ssl}` → `{session_id, expires_in, api_version}`. `api_version: "auto"` negotiates 1.3-rev2 → 1.3-rev1 → 1.3-rev0 → 1.2-rev1 |
| POST | `/api/connect-demo` | session with simulated data (JSON in the real VBR shape) |
| POST | `/api/{session}/disconnect` | closes the session |
| GET | `/api/{session}/inventory` | jobs (storage, GFS, guest processing), workloads, repositories |
| POST | `/api/{session}/inventory/refresh` | drops the cached inventory and reloads it |
| GET | `/api/{session}/inventory/progress` | `{stage, done, total, message, percent}` while the inventory loads (drives the progress bar) |
| GET | `/api/{session}/restore-points?pivot=job\|vm\|repo&id=…&days=30&skip=0&limit=100[&aaip=1]` | normalized restore points, paged by 100 |
| GET | `/api/{session}/restore-points/{rp}` | detail: restore point + AAIP result + chain + other locations |
| GET | `/api/{session}/retention?pivot=job\|vm\|repo&id=…` | GFS retention view: tiers, estimated expiry (creation + `keepForNumberOf…`), expected vs. missing periods (Job pivot) |
| GET | `/api/{session}/raw/{path...}` | read-only passthrough to the REST API (e.g. `raw/v1/backups`) to inspect the real schema |
| GET | `/api/{session}/diagnostics` | alpha diagnostics bundle (see below) |

## Validated in the field (vbr-03, v13 / API 1.3-rev2) — `// FIELD` in the code

- `dedupRatio` / `compressRatio` in `BackupFileModel` are the **percentage of size remaining** after each stage (65 + 93 → 61 % of `dataSize`). Reduction factor = 100 / value. 0 = no data (Nutanix / plug-in placeholder files).
- `BackupObjectModel.backupId` does **not** match the `backupId` of the object's restore points; the job ↔ object relation is taken from `GET /backups/{id}/objects`.
- `/backupObjects/{id}/restorePoints` can return points from other backups of the same object; the job pivot keeps only the job's backups, the repository pivot only that repository.
- Scale-out repositories are not in `/repositories/states`; they come from `/backupInfrastructure/scaleOutRepositories`.
- Real `EJobType` values: `VSphereBackup`, `HyperVBackup`, `CloudDirectorBackup`, `WindowsAgentBackup`, `LinuxAgentBackup`, `FileBackup`, `ObjectStorageBackup`, `BackupCopy`, `VSphereReplica`, `SureBackupContentScan`. Replicas and SureBackup have no backup chain and are shown dimmed.
- Immediate backup copy: `schedule = {scheduleMode: Continuous, type: Immediate}`.
- Nutanix AHV and the Application Backup Repository (RMAN / SQL plug-in) return `sessionId = 00000000-…`: no task session to derive guest processing from.
- `v1/jobs` can take > 75 s on a loaded VBR (timeout budget inherited from Yoga Benchmark is right).
- **`GET /jobs` returns HTTP 500** ("Source item … is not part of 'Clusters' hierarchy") on a VBR with application plug-in jobs (Oracle RMAN, SAP HANA, MongoDB, SQL plug-in): VBR expands the source objects of every job and one outside the virtual hierarchy fails the whole list. The job list therefore comes from `/jobs/states` (light, also lists plug-in jobs) with `/jobs` and `/backups` as fallbacks; `GET /jobs/{id}` is called per job and a failure only marks that job (`!` badge).
- A CDP-only VBR exposes no jobs and no backups over REST: CDP policies and replicas are not backup chains (phase 3). The console says so instead of showing an empty screen.
- **Plug-in backups (Oracle RMAN, SAP HANA/backint, SQL plug-in, MongoDB — configured on the server or VBR-managed)** appear in `/backups` as `platformName: CustomPlatform`, `jobType: Unknown`, not in `/jobs`; their object is `type: Directory` with `restorePointsCount: 0` and `/backupObjects/{id}/restorePoints` is **empty**. What the API exposes are the **backup files** (`/backups/{id}/backupFiles`: one `.vab` per backup piece/channel with `creationTime`, `dataSize`, `backupSize`, ratios). The timeline shows one amber diamond per piece; full vs. incremental vs. archive log is only known to the application catalog (RMAN, backint).
- Agent job with Oracle processing (`VBR Managed Agent - Oracle RMAN`): `/backups/{id}/objects` can come back empty while restore points exist; objects are then linked through `backupFiles[].objectIds`.
- Unknown `EJobType` values never break the inventory: they fall into a generic category (CDP policy, Replica, Application backup, Kubernetes backup, Cloud backup, Other).

## Pending / to validate in a lab (`// LAB` in the code)

- **Synthetic vs. active full**: the API returns `Increment|Full`; `taskSession.algorithm = Synthetic` is queried only in the detail view.
- Immutability per restore point: not in the RP; derived from `creationTime + repo.immDays` (hardened / object lock). SOBR immutability taken from the most restrictive performance extent.
- Text patterns in `/taskSessions/{id}/logs` to classify guest processing (VSS, Oracle, SQL, PostgreSQL).
- Application Backup Repository objects (RMAN / SAP HANA / SQL plug-in) show up as a workload with `platformName = ApplicationBackupRepository`, `StartFlrRestore` only. To be handled as its own workload type in phase 2.
- Self-signed certificate: `verify_ssl=false` by default. CORS open (the frontend is served from the same origin).
- `aaip=1` against a real VBR: 2–3 calls per restore point → cache by `sessionId`.

## Release

GitHub Actions builds the release when a `vX.Y.Z[-alpha]` tag is pushed (`.github/workflows/release.yml`):
it runs the tests, checks that `const version` in `main.go` matches the tag, runs `build.sh` and
publishes the binaries + `SHA256SUMS.txt` as a pre-release when the tag contains `alpha`/`beta`.

```bash
# bump: edit const version in main.go, commit, then
git tag -a v0.1.1-alpha -m "v0.1.1-alpha" && git push origin main --tags
```

## Diagnostics in alpha

When something looks wrong against a real VBR, send two things:

1. The **Diagnostics** button in the top bar → downloads `yogachain-diagnostics-<date>.json`: version, OS,
   session (no credentials or tokens), inventory summary, 1–2 raw items of each REST API collection
   (to compare the real schema with what `// LAB` assumes) and the trace of the last 300 REST calls
   with status, ms, bytes and error.
2. `yogachain.log` (next to the binary). With `-debug` (default) it includes every GET, the paging and the
   first raw item of each collection. It never contains passwords or tokens.
