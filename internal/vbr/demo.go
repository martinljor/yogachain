package vbr

// demo.go — synthetic VBR for exercising the UI without a live server. It answers
// the same REST paths the real client uses, with the same JSON shapes (1.3-rev2),
// so the mapping code in internal/chain runs unchanged in demo mode. The data is
// built relative to time.Now() and deliberately includes every case the UI must
// show:
//   - SQL job with log backups every 15 min, one VM with 2-day gap (mirrored in
//     the S3 copy) and crash-consistent points (VSS timeout),
//   - PostgreSQL VM with malware suspicion and WAL backups with warnings,
//   - Oracle job: one DB in ARCHIVELOG (point-in-time OK, one day with FRA full)
//     and one in NOARCHIVELOG (no log backups, no point-in-time),
//   - backup copy to object storage with immutability, weekly tape,
//   - low compression on databases, high on web tier (DedupFriendly on Oracle).
// Names are generic; nothing here comes from a real environment.

import (
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type demoVM struct {
	id, name, os, app, db string
	sizeGB                float64
}

type demoJob struct {
	id, name, typ, repo, comp, block string
	enc                              bool
	gfs                              map[string]any
	vms                              []string
	aaip                             map[string]any // guestProcessing.appAwareProcessing (nil = no aplica)
	hour                             int
}

type demoRP struct {
	id, job, vm, backupFile, session, task string
	date                                   time.Time
	rpType                                 string // Full | Increment
	synthetic                              bool
	gfs                                    []string
	dataGB, backupGB                       float64
	dedup, compress                        int // % del tamano que queda tras cada etapa (FIELD, como BackupFileModel)
	malware                                string
	aaip, aaipTitle                        string // "" = no aplica
}

var (
	demoOnce  sync.Once
	demoRepos []map[string]any
	demoState []map[string]any
	demoJobs  []demoJob
	demoVMs   []demoVM
	demoRPs   []demoRP
)

func demoBuild() {
	demoRepos = []map[string]any{
		{"id": "r1", "name": "REPO-HARDENED-01", "type": "LinuxHardened", "repository": map[string]any{"makeRecentBackupsImmutableDays": 14}},
		{"id": "r2", "name": "SOBR-PROD-WIN", "type": "ScaleOut"},
		{"id": "r3", "name": "S3-COPY-USEAST", "type": "AmazonS3", "bucket": map[string]any{"immutability": map[string]any{"isEnabled": true, "daysCount": 30, "immutabilityMode": "Compliance"}}},
		{"id": "r4", "name": "TAPE-LTO9-POOL", "type": "Tape"},
	}
	demoState = []map[string]any{
		{"id": "r1", "name": "REPO-HARDENED-01", "type": "LinuxHardened", "capacityGB": 48000, "usedSpaceGB": 31200, "freeGB": 16800, "isOnline": true},
		{"id": "r2", "name": "SOBR-PROD-WIN", "type": "ScaleOut", "capacityGB": 120000, "usedSpaceGB": 87500, "freeGB": 32500, "isOnline": true},
		{"id": "r3", "name": "S3-COPY-USEAST", "type": "AmazonS3", "capacityGB": 0, "usedSpaceGB": 54900, "freeGB": 0, "isOnline": true},
		{"id": "r4", "name": "TAPE-LTO9-POOL", "type": "Tape", "isOnline": true},
	}
	demoVMs = []demoVM{
		{"v1", "sql-prod-01", "Windows Server 2022", "SQL", "", 1240}, {"v2", "sql-prod-02", "Windows Server 2022", "SQL", "", 1180},
		{"v3", "sql-rpt-01", "Windows Server 2019", "SQL", "", 640},
		{"v4", "app-web-01", "Ubuntu 22.04", "", "", 120}, {"v5", "app-web-02", "Ubuntu 22.04", "", "", 118},
		{"v6", "app-api-01", "RHEL 9", "PostgreSQL", "", 210}, {"v7", "app-queue-01", "RHEL 9", "", "", 95},
		{"v8", "dc-01", "Windows Server 2022", "AD", "", 80}, {"v9", "dc-02", "Windows Server 2022", "AD", "", 78},
		{"v10", "fs-corp-01", "Windows Server 2019", "", "", 2860},
		{"v11", "vdi-gold-w11", "Windows 11", "", "", 64}, {"v12", "vdi-gold-w10", "Windows 10", "", "", 58},
		{"v13", "ora-erp-01", "Oracle Linux 8.9", "Oracle", "ERPPRD · 19c · ARCHIVELOG", 3400},
		{"v14", "ora-dwh-01", "Oracle Linux 8.9", "Oracle", "DWHPRD · 19c · NOARCHIVELOG", 5200},
	}
	gfs := func(w, m, y int) map[string]any {
		return map[string]any{"isEnabled": w+m+y > 0,
			"weekly":  map[string]any{"isEnabled": w > 0, "keepForNumberOfWeeks": w},
			"monthly": map[string]any{"isEnabled": m > 0, "keepForNumberOfMonths": m},
			"yearly":  map[string]any{"isEnabled": y > 0, "keepForNumberOfYears": y}}
	}
	sqlAA := map[string]any{"isEnabled": true, "appSettings": []any{map[string]any{
		"vmObject": map[string]any{"name": "sql-prod-01"}, "vss": "RequireSuccess", "transactionLogs": "Process",
		"sql": map[string]any{"logsProcessing": "Backup", "backupMinsCount": 15, "retainLogBackups": "UntilBackupDeleted"}}}}
	pgAA := map[string]any{"isEnabled": true, "appSettings": []any{map[string]any{
		"vmObject": map[string]any{"name": "app-api-01"}, "vss": "IgnoreFailures",
		"postgreSQL": map[string]any{"backupLogs": true, "backupMinsCount": 30, "retainLogBackups": "UntilBackupDeleted"}}}}
	adAA := map[string]any{"isEnabled": true, "appSettings": []any{map[string]any{"vmObject": map[string]any{"name": "dc-01"}, "vss": "RequireSuccess"}}}
	oraAA := map[string]any{"isEnabled": true, "appSettings": []any{map[string]any{
		"vmObject": map[string]any{"name": "ora-erp-01"}, "vss": "RequireSuccess",
		"oracle": map[string]any{"useGuestCredentials": false, "credentialsId": "cred-oracle-sysdba", "archiveLogs": "DeleteExpiredHours",
			"deleteHoursCount": 24, "backupLogs": true, "backupMinsCount": 15, "retainLogBackups": "KeepOnlyDays", "keepDaysCount": 14}}}}
	demoJobs = []demoJob{
		{"j1", "BKP-SQL-PROD", "VSphereBackup", "r1", "Optimal", "1MB", true, gfs(4, 12, 0), []string{"v1", "v2", "v3"}, sqlAA, 22},
		{"j2", "BKP-APP-TIER", "VSphereBackup", "r2", "Optimal", "1MB", false, gfs(4, 0, 0), []string{"v4", "v5", "v6", "v7"}, pgAA, 23},
		{"j3", "BKP-INFRA-CORE", "VSphereBackup", "r2", "High", "1MB", false, gfs(0, 0, 0), []string{"v8", "v9", "v10"}, adAA, 1},
		{"j4", "BKP-VDI-POOL", "HyperVBackup", "r2", "Optimal", "512KB", false, gfs(0, 0, 0), []string{"v11", "v12"}, map[string]any{"isEnabled": false}, 22},
		{"j5", "COPY-SQL-PROD-S3", "BackupCopy", "r3", "Optimal", "1MB", true, gfs(0, 12, 3), []string{"v1", "v2", "v3"}, nil, 22},
		{"j6", "TAPE-WEEKLY-ALL", "BackupToTape", "r4", "None", "", true, gfs(0, 0, 7), []string{"v1", "v2", "v3", "v4", "v5", "v6", "v7", "v8", "v9", "v10", "v13", "v14"}, nil, 6},
		{"j7", "BKP-ORA-ERP", "VSphereBackup", "r1", "DedupFriendly", "1MB", true, gfs(4, 12, 1), []string{"v13", "v14"}, oraAA, 21},
	}

	seed := uint32(7)
	rnd := func() float64 { seed = (seed*9301 + 49297) % 233280; return float64(seed) / 233280 }
	today := time.Now().Truncate(time.Hour).Add(-time.Duration(time.Now().Hour()) * time.Hour)
	vmByID := map[string]demoVM{}
	for _, v := range demoVMs {
		vmByID[v.id] = v
	}
	n := 0
	push := func(j demoJob, vm demoVM, date time.Time, rpType string, synthetic bool, gfsP []string, dataGB float64, comp, dedup float64, malware, aaip, aaipTitle string) {
		n++
		demoRPs = append(demoRPs, demoRP{
			id: fmt.Sprintf("rp-%05d", n), job: j.id, vm: vm.id, backupFile: fmt.Sprintf("bf-%05d", n),
			session: fmt.Sprintf("ses-%05d", n), task: fmt.Sprintf("ts-%05d", n), date: date, rpType: rpType, synthetic: synthetic,
			gfs: gfsP, dataGB: dataGB, backupGB: dataGB / (comp * dedup), dedup: int(100/dedup + .5), compress: int(100/comp + .5),
			malware: malware, aaip: aaip, aaipTitle: aaipTitle,
		})
	}
	for _, j := range demoJobs {
		for _, vid := range j.vms {
			vm := vmByID[vid]
			for d := 70; d >= 0; d-- {
				date := today.AddDate(0, 0, -d).Add(time.Duration(j.hour)*time.Hour + time.Duration(rnd()*50)*time.Minute)
				dow := date.Weekday()
				if j.typ == "BackupToTape" && dow != time.Sunday {
					continue
				}
				if j.typ == "BackupCopy" {
					date = date.Add(3 * time.Hour)
				}
				// huecos deliberados
				if (j.id == "j1" || j.id == "j5") && vid == "v3" && (d == 9 || d == 10) {
					continue
				}
				if j.id == "j2" && vid == "v7" && d >= 4 && d <= 6 {
					continue
				}
				if j.id == "j3" && vid == "v10" && d == 17 {
					continue
				}
				rpType, synthetic := "Increment", false
				switch {
				case j.typ == "BackupToTape":
					rpType = "Full"
				case j.typ == "BackupCopy":
					if dow == time.Sunday { // la copia replica los fulls del origen
						rpType = "Full"
					}
				case j.id == "j1" && dow == time.Sunday, (j.id == "j2" || j.id == "j7") && dow == time.Saturday:
					rpType, synthetic = "Full", true
				case j.id == "j3" && dow == time.Sunday:
					rpType = "Full"
				}
				var gfsP []string
				if rpType == "Full" && j.typ != "BackupToTape" && j.gfs["isEnabled"] == true {
					if date.Day() <= 7 {
						gfsP = []string{"Monthly"}
					} else {
						gfsP = []string{"Weekly"}
					}
				}
				if j.typ == "BackupCopy" && date.Day() == 1 {
					gfsP = []string{"Monthly"}
				}
				if j.typ == "BackupToTape" {
					gfsP = []string{"Yearly"}
				}
				full := rpType == "Full"
				var dataGB float64
				if full {
					dataGB = vm.sizeGB * (0.78 + rnd()*.08)
				} else {
					dataGB = vm.sizeGB * (0.03 + rnd()*.06)
				}
				var comp, dedup float64
				switch {
				case j.typ == "BackupToTape":
					comp, dedup = 1, 1
				case vm.app == "Oracle":
					comp = 1.25 + rnd()*.2
				case vm.app == "SQL":
					comp = 1.4 + rnd()*.3
				case strings.Contains(vm.name, "dc") || strings.Contains(vm.name, "fs"):
					comp = 1.9 + rnd()*.4
				default:
					comp = 2.3 + rnd()*.6
				}
				if dedup == 0 {
					if full {
						dedup = 1.15 + rnd()*.35
					} else {
						dedup = 1.0 + rnd()*.15
					}
				}
				malware := "Clean"
				if j.id == "j2" && vid == "v6" && (d == 2 || d == 3) {
					malware = "Suspicious"
				}
				aaip, title := demoAaip(j, vm, date)
				push(j, vm, date, rpType, synthetic, gfsP, dataGB, comp, dedup, malware, aaip, title)
				if j.id == "j4" && !(vid == "v12" && d == 12) { // segundo RP del dia (cada 12 h)
					push(j, vm, date.Add(-12*time.Hour), "Increment", false, nil, vm.sizeGB*.04, 2.4, 1.05, "Clean", "off", "")
				}
			}
		}
	}
	sort.Slice(demoRPs, func(a, b int) bool { return demoRPs[a].date.Before(demoRPs[b].date) })
}

// demoAaip devuelve el resultado de guest processing simulado y el titulo del
// log que lo justifica (lo que luego parsea chain.Aaip).
func demoAaip(j demoJob, vm demoVM, date time.Time) (string, string) {
	if j.aaip == nil {
		return "", ""
	}
	if j.aaip["isEnabled"] != true {
		return "off", "Application-aware processing is disabled"
	}
	day := date.Day()
	switch {
	case vm.id == "v14":
		return "warn", "Database is in NOARCHIVELOG mode, archived log backup skipped"
	case vm.id == "v13" && day == 8:
		return "warn", "Failed to delete archived logs: ORA-19809 limit exceeded for recovery files"
	case vm.id == "v6" && day%7 == 3:
		return "warn", "Failed to connect to guest agent: credentials expired"
	case vm.id == "v3" && day%11 == 5:
		return "fail", "Failed to prepare guest for freeze: VSS timeout"
	case vm.app == "" && day%9 == 0:
		return "warn", "Guest processing skipped: no application detected"
	}
	return "ok", "Application-aware processing completed successfully"
}

// ---------------------------------------------------------------- respuestas

func demoResponse(path string) json.RawMessage {
	demoOnce.Do(demoBuild)
	p, q, _ := strings.Cut(path, "?")
	qs, _ := url.ParseQuery(q)
	parts := strings.Split(strings.TrimPrefix(p, "v1/"), "/")

	switch {
	case p == "v1/jobs":
		out := []map[string]any{}
		for _, j := range demoJobs {
			out = append(out, map[string]any{"id": j.id, "name": j.name, "type": j.typ, "isDisabled": false})
		}
		out = append(out, map[string]any{"id": "j-sure", "name": "SureBackup-Malware-Scan", "type": "SureBackupContentScan", "isDisabled": false})
		return demoPage(out, qs)
	case len(parts) == 2 && parts[0] == "jobs":
		for _, j := range demoJobs {
			if j.id == parts[1] {
				return demoJSON(demoJobDetail(j))
			}
		}
	case p == "v1/backupInfrastructure/repositories":
		return demoPage(demoRepos, qs)
	case p == "v1/backupInfrastructure/repositories/states":
		return demoPage(demoState, qs)
	case p == "v1/backupInfrastructure/scaleOutRepositories":
		return demoPage([]map[string]any{{"id": "sobr-1", "name": "SOBR-PROD", "description": "demo",
			"performanceTier": map[string]any{"performanceExtents": []any{map[string]any{"id": "r2", "name": "SOBR-PROD-WIN"}}}}}, qs)
	case p == "v1/backups":
		out := []map[string]any{}
		for _, j := range demoJobs {
			out = append(out, demoBackup(j))
		}
		return demoPage(out, qs)
	case len(parts) == 2 && parts[0] == "backups":
		if j, ok := demoJobByBackup(parts[1]); ok {
			return demoJSON(demoBackup(j))
		}
	case len(parts) == 3 && parts[0] == "backups" && parts[2] == "objects":
		if j, ok := demoJobByBackup(parts[1]); ok {
			out := []map[string]any{}
			for _, vid := range j.vms {
				out = append(out, demoObject(j, vid))
			}
			return demoPage(out, qs)
		}
	case len(parts) == 3 && parts[0] == "backups" && parts[2] == "backupFiles":
		out := []map[string]any{}
		for _, r := range demoRPs {
			if "bk-"+r.job == parts[1] {
				out = append(out, demoBackupFile(r))
			}
		}
		return demoPage(demoFilterCreated(out, qs), qs)
	case p == "v1/backupObjects":
		out := []map[string]any{}
		for _, j := range demoJobs {
			for _, vid := range j.vms {
				out = append(out, demoObject(j, vid))
			}
		}
		return demoPage(out, qs)
	case len(parts) == 3 && parts[0] == "backupObjects" && parts[2] == "restorePoints":
		out := []map[string]any{}
		for _, r := range demoRPs {
			if "bo-"+r.job+"-"+r.vm == parts[1] {
				out = append(out, demoRestorePoint(r))
			}
		}
		return demoPage(demoFilterCreated(out, qs), qs)
	case p == "v1/restorePoints":
		out := []map[string]any{}
		for _, r := range demoRPs {
			if b := qs.Get("backupIdFilter"); b != "" && "bk-"+r.job != b {
				continue
			}
			if o := qs.Get("backupObjectIdFilter"); o != "" && "bo-"+r.job+"-"+r.vm != o {
				continue
			}
			out = append(out, demoRestorePoint(r))
		}
		return demoPage(demoFilterCreated(out, qs), qs)
	case len(parts) == 2 && parts[0] == "restorePoints":
		for _, r := range demoRPs {
			if r.id == parts[1] {
				return demoJSON(demoRestorePoint(r))
			}
		}
	case len(parts) == 3 && parts[0] == "sessions" && parts[2] == "taskSessions":
		for _, r := range demoRPs {
			if r.session == parts[1] {
				alg := r.rpType
				if r.synthetic {
					alg = "Synthetic"
				}
				return demoPage([]map[string]any{{"id": r.task, "sessionId": r.session, "restorePointId": r.id, "algorithm": alg,
					"repositoryId": demoJobRepo(r.job), "state": "Stopped", "result": map[string]any{"result": "Success"}}}, qs)
			}
		}
	case len(parts) == 3 && parts[0] == "taskSessions" && parts[2] == "logs":
		for _, r := range demoRPs {
			if r.task == parts[1] {
				return demoPage(demoLogs(r), qs)
			}
		}
	}
	return json.RawMessage(`{"data":[],"pagination":{"total":0,"count":0,"skip":0,"limit":100}}`)
}

func demoJobByBackup(id string) (demoJob, bool) {
	for _, j := range demoJobs {
		if "bk-"+j.id == id {
			return j, true
		}
	}
	return demoJob{}, false
}

func demoJobRepo(jobID string) string {
	for _, j := range demoJobs {
		if j.id == jobID {
			return j.repo
		}
	}
	return ""
}

func demoJobDetail(j demoJob) map[string]any {
	storage := map[string]any{
		"backupRepositoryId": j.repo,
		"retentionPolicy":    map[string]any{"type": "Days", "quantity": 14},
		"gfsPolicy":          j.gfs,
		"advancedSettings": map[string]any{"backupModeType": "Incremental", "storageData": map[string]any{
			"compressionLevel": j.comp, "storageOptimization": j.block, "inlineDataDedupEnabled": true,
			"encryption": map[string]any{"isEnabled": j.enc}}},
	}
	sched := map[string]any{"runAutomatically": true, "daily": map[string]any{"isEnabled": true, "localTime": fmt.Sprintf("%02d:00", j.hour)}}
	if j.id == "j4" {
		sched = map[string]any{"runAutomatically": true, "periodically": map[string]any{"isEnabled": true, "frequency": 12, "periodicallyKind": "Hours"}}
	}
	out := map[string]any{"id": j.id, "name": j.name, "type": j.typ, "storage": storage, "schedule": sched}
	if j.aaip != nil {
		out["guestProcessing"] = map[string]any{"appAwareProcessing": j.aaip,
			"guestFSIndexing": map[string]any{"isEnabled": j.id == "j1" || j.id == "j3"}}
	}
	return out
}

func demoBackup(j demoJob) map[string]any {
	platform := "VMware"
	switch j.typ {
	case "BackupToTape":
		platform = "Tape"
	case "HyperVBackup":
		platform = "HyperV"
	}
	return map[string]any{"id": "bk-" + j.id, "name": j.name, "jobId": j.id, "repositoryId": j.repo, "platformName": platform,
		"jobType": j.typ, "creationTime": time.Now().AddDate(0, 0, -70).Format(time.RFC3339)}
}

func demoObject(j demoJob, vid string) map[string]any {
	var vm demoVM
	for _, v := range demoVMs {
		if v.id == vid {
			vm = v
		}
	}
	return map[string]any{"id": "bo-" + j.id + "-" + vid, "objectId": "vm-" + vid, "name": vm.name, "type": "VM", "platformName": "VMware",
		"platformId": "vc-01", "backupId": "bk-" + j.id, "size": int64(vm.sizeGB * 1024 * 1024 * 1024), "restorePointsCount": 30}
}

func demoRestorePoint(r demoRP) map[string]any {
	ops := []string{"StartViVMInstantRecovery", "StartEntireVmRestore", "StartFlrRestore", "StartDiskPublish"}
	gos := "Windows"
	for _, v := range demoVMs {
		if v.id == r.vm && !strings.HasPrefix(v.os, "Windows") {
			gos = "Linux"
		}
	}
	return map[string]any{"id": r.id, "name": "", "platformName": "VMware", "platformId": "vc-01", "creationTime": r.date.Format(time.RFC3339),
		"backupId": "bk-" + r.job, "type": r.rpType, "sessionId": r.session, "allowedOperations": ops, "malwareStatus": r.malware,
		"backupFileId": r.backupFile, "guestOsFamily": gos, "originalSize": int64(r.dataGB * 1024 * 1024 * 1024)}
}

func demoBackupFile(r demoRP) map[string]any {
	var name string
	for _, v := range demoVMs {
		if v.id == r.vm {
			name = v.name
		}
	}
	ext := ".vib"
	if r.rpType == "Full" {
		ext = ".vbk"
	}
	gb := 1024.0 * 1024 * 1024
	gfs := []string{"None"}
	if len(r.gfs) > 0 {
		gfs = r.gfs
	}
	return map[string]any{"id": r.backupFile, "name": fmt.Sprintf("%s/%s.%s%s", demoJobName(r.job), name, r.date.Format("2006-01-02T1504"), ext),
		"backupId": "bk-" + r.job, "objectIds": []string{"bo-" + r.job + "-" + r.vm}, "restorePointIds": []string{r.id},
		"dataSize": int64(r.dataGB * gb), "backupSize": int64(r.backupGB * gb), "dedupRatio": r.dedup, "compressRatio": r.compress,
		"creationTime": r.date.Format(time.RFC3339), "gfsPeriods": gfs, "severity": r.malware}
}

func demoJobName(id string) string {
	for _, j := range demoJobs {
		if j.id == id {
			return j.name
		}
	}
	return id
}

func demoLogs(r demoRP) []map[string]any {
	logs := []map[string]any{
		{"title": "Queued for processing at " + r.date.Format("1/2/2006 3:04:05 PM"), "status": "Succeeded"},
		{"title": "Processing " + r.vm, "status": "Succeeded"},
	}
	switch r.aaip {
	case "ok":
		logs = append(logs, map[string]any{"title": "Guest processing: " + r.aaipTitle, "status": "Succeeded"})
	case "warn":
		logs = append(logs, map[string]any{"title": "Guest processing: " + r.aaipTitle, "status": "Warning"})
	case "fail":
		logs = append(logs, map[string]any{"title": "Guest processing: " + r.aaipTitle, "status": "Failed"})
	}
	logs = append(logs, map[string]any{"title": "Busy: Source 34% > Proxy 62% > Network 12% > Target 41%", "status": "Succeeded"})
	return logs
}

// demoFilterCreated aplica createdAfterFilter / createdBeforeFilter sobre creationTime.
func demoFilterCreated(items []map[string]any, qs url.Values) []map[string]any {
	after, _ := time.Parse(time.RFC3339, qs.Get("createdAfterFilter"))
	before, _ := time.Parse(time.RFC3339, qs.Get("createdBeforeFilter"))
	if after.IsZero() && before.IsZero() {
		return items
	}
	out := items[:0:0]
	for _, it := range items {
		t, _ := time.Parse(time.RFC3339, fmt.Sprint(it["creationTime"]))
		if !after.IsZero() && t.Before(after) {
			continue
		}
		if !before.IsZero() && t.After(before) {
			continue
		}
		out = append(out, it)
	}
	return out
}

// demoPage envuelve una lista con la paginacion real de VBR ({data, pagination}),
// respetando skip/limit para que el cliente paginado se comporte igual que en prod.
func demoPage(items []map[string]any, qs url.Values) json.RawMessage {
	skip, _ := strconv.Atoi(qs.Get("skip"))
	limit, _ := strconv.Atoi(qs.Get("limit"))
	if limit <= 0 {
		limit = 100
	}
	total := len(items)
	if skip > total {
		skip = total
	}
	end := skip + limit
	if end > total {
		end = total
	}
	page := items[skip:end]
	return demoJSON(map[string]any{"data": page, "pagination": map[string]any{"total": total, "count": len(page), "skip": skip, "limit": limit}})
}

func demoJSON(v any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}
