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
| GET | `/api/{session}/restore-points?pivot=job\|vm\|repo&id=…&days=30&skip=0&limit=100[&aaip=1]` | normalized restore points, paged by 100 |
| GET | `/api/{session}/restore-points/{rp}` | detail: restore point + AAIP result + chain + other locations |
| GET | `/api/{session}/raw/{path...}` | read-only passthrough to the REST API (e.g. `raw/v1/backups`) to inspect the real schema |
| GET | `/api/{session}/diagnostics` | alpha diagnostics bundle (see below) |

## Pending / to validate in a lab (`// LAB` in the code)

- Workload identity across backups (primary, copy, tape): currently `BackupObjectModel.objectId`, falling back to the name.
- **Synthetic vs. active full**: the API returns `Increment|Full`; `taskSession.algorithm = Synthetic` is queried only in the detail view.
- Scale of `dedupRatio` / `compressRatio` (factor vs. percentage) in `BackupFileModel`.
- Immutability per restore point: not in the RP; derived from `creationTime + repo.immDays` (hardened / object lock).
- Text patterns in `/taskSessions/{id}/logs` to classify guest processing (VSS, Oracle, SQL, PostgreSQL).
- Real `EJobType` values for backup copy / tape / agents; `backupRepositoryId` on copy jobs.
- **RMAN / SAP HANA / SQL plug-in** backups: they do not show up as VM restore points (`platformName = ApplicationBackupRepository`). Affects phase 2.
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
