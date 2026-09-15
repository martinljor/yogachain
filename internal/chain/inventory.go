package chain

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"

	"yogachain/internal/vbr"
)

// Lo marcado con  // LAB  todavia no se confirmo contra un VBR real. Lo que ya se
// valido con el diagnostico de vbr-03 (v13, API 1.3-rev2) esta marcado  // FIELD.

const invKey = "inventory"
const progressKey = "inventory.progress"

// Progress: estado de la carga del inventario, para la barra de la UI. Se guarda
// en la sesion y el frontend lo consulta mientras espera /inventory.
type Progress struct {
	Stage   string `json:"stage"`   // repos | jobs | backups | objects | done | error
	Done    int    `json:"done"`    // items procesados en la etapa
	Total   int    `json:"total"`   // items totales de la etapa (0 = desconocido)
	Message string `json:"message"` // ultimo detalle (nombre del job, error tolerado...)
	Percent int    `json:"percent"` // 0-100 estimado sobre toda la carga
}

// InventoryProgress devuelve el progreso actual (o "done" si ya esta cargado).
func InventoryProgress(s *vbr.Session) Progress {
	if v, ok := s.Value(progressKey); ok {
		if p, ok := v.(Progress); ok {
			return p
		}
	}
	if v, ok := s.Value(invKey); ok && v != nil {
		return Progress{Stage: "done", Percent: 100}
	}
	return Progress{Stage: "idle"}
}

// pesos por etapa para el porcentaje estimado (los detalles de jobs y los
// objetos por backup son las partes lentas).
var stageBase = map[string]int{"repos": 0, "jobs": 10, "backups": 55, "objects": 65, "done": 100}
var stageSpan = map[string]int{"repos": 10, "jobs": 45, "backups": 10, "objects": 35}

func progress(s *vbr.Session, stage string, done, total int, msg string) {
	p := Progress{Stage: stage, Done: done, Total: total, Message: msg, Percent: stageBase[stage]}
	if total > 0 {
		p.Percent += stageSpan[stage] * done / total
	}
	if p.Percent > 100 {
		p.Percent = 100
	}
	s.Set(progressKey, p)
}

// LoadInventory carga (o devuelve de la sesion) jobs, workloads y repositorios.
func LoadInventory(ctx context.Context, s *vbr.Session) (*Inventory, error) {
	if v, ok := s.Value(invKey); ok {
		if inv, ok := v.(*Inventory); ok {
			return inv, nil
		}
	}
	inv, err := buildInventory(ctx, s)
	if err != nil {
		progress(s, "error", 0, 0, err.Error())
		return nil, err
	}
	s.Set(invKey, inv)
	progress(s, "done", 1, 1, "")
	return inv, nil
}

// Refresh descarta el inventario cacheado (el usuario toco "actualizar").
func Refresh(s *vbr.Session) { s.Set(invKey, nil); s.Set(progressKey, nil) }

func buildInventory(ctx context.Context, s *vbr.Session) (*Inventory, error) {
	inv := &Inventory{
		Server: fmt.Sprintf("%s:%d", s.Host, s.Port), APIVersion: s.APIVersion,
		backupJob: map[string]string{}, backupRepo: map[string]string{}, backupPlatform: map[string]string{},
		objectVM: map[string]string{}, objectBackup: map[string]string{},
	}
	if s.Demo {
		inv.Server, inv.APIVersion = "demo-vbr", "demo"
	}

	// --- repositorios: config (inmutabilidad) + estado (capacidad) + SOBR -------
	// FIELD: /repositories/states no lista los scale-out; los jobs que apuntan a un
	// SOBR quedaban sin nombre de repositorio. Los SOBR salen de
	// /backupInfrastructure/scaleOutRepositories (como en yogabench).
	progress(s, "repos", 0, 3, "")
	cfg := map[string]obj{}
	repos, err := getAll(ctx, s, "v1/backupInfrastructure/repositories", 0)
	if err != nil {
		return nil, err
	}
	for _, r := range repos {
		cfg[str(r, "id")] = r
	}
	progress(s, "repos", 1, 3, "")
	states, err := getAll(ctx, s, "v1/backupInfrastructure/repositories/states", 0)
	if err != nil {
		log.Printf("inventory: repositories/states failed (%v), continuing without capacity", err)
		for _, r := range repos { // sin estado: al menos nombre y tipo
			states = append(states, r)
		}
	}
	seen := map[string]bool{}
	for _, st := range states {
		id := str(st, "id")
		if seen[id] {
			continue
		}
		seen[id] = true
		c := cfg[id]
		inv.Repos = append(inv.Repos, Repo{
			ID: id, Name: str(st, "name"), Type: str(st, "type"), ImmDays: immutabilityDays(c),
			CapacityGB: num(st, "capacityGB"), UsedGB: num(st, "usedSpaceGB"), FreeGB: num(st, "freeGB"),
			Online: st["isOnline"] == nil || boolean(st, "isOnline"),
		})
	}
	progress(s, "repos", 2, 3, "")
	if sobrs, err := getAll(ctx, s, "v1/backupInfrastructure/scaleOutRepositories", 0); err != nil {
		log.Printf("inventory: scaleOutRepositories failed (%v), SOBR-backed jobs will show no repository name", err)
	} else {
		for _, so := range sobrs {
			id := str(so, "id")
			if seen[id] {
				continue
			}
			seen[id] = true
			r := Repo{ID: id, Name: str(so, "name"), Type: "ScaleOut", Online: true}
			// inmutabilidad: la del extent mas restrictivo del performance tier  // LAB
			for _, e := range arr(sub(so, "performanceTier"), "performanceExtents") {
				eo, _ := e.(map[string]any)
				if x := inv.Repo(str(eo, "id")); x != nil {
					r.CapacityGB += x.CapacityGB
					r.UsedGB += x.UsedGB
					r.FreeGB += x.FreeGB
					if x.ImmDays > r.ImmDays {
						r.ImmDays = x.ImmDays
					}
				}
			}
			inv.Repos = append(inv.Repos, r)
		}
	}

	// --- jobs: lista + detalle (storage, GFS, guest processing) ---------------
	// FIELD (vbrdb-01): GET /jobs devuelve 500 "Source item ... is not part of
	// 'Clusters' hierarchy" cuando hay jobs de aplicacion (plug-ins RMAN/HANA/SQL/
	// MongoDB): VBR intenta expandir los objetos de origen de TODOS los jobs y uno
	// que no pertenece a la jerarquia virtual tira toda la lista. Ademas /jobs es
	// el endpoint que mas tarda (75 s en un VBR cargado). Por eso la lista sale de
	// /jobs/states (liviano, y el unico que tambien lista jobs de plug-in), con
	// /jobs y /backups como fallback, y el detalle se pide job por job tolerando
	// los que fallen.
	apps := map[string]string{} // nombre de VM -> app (segun appSettings del job)
	jobs, listSrc := listJobs(ctx, s)
	progress(s, "jobs", 0, len(jobs), listSrc)
	for i, j := range jobs {
		id := str(j, "id")
		progress(s, "jobs", i, len(jobs), str(j, "name"))
		full, err := getObj(ctx, s, "v1/jobs/"+id)
		detailErr := ""
		if err != nil {
			log.Printf("inventory: job %s (%s) detail failed: %v", str(j, "name"), id, err)
			full = j
			detailErr = err.Error()
		}
		storage := sub(full, "storage")
		adv := sub(sub(storage, "advancedSettings"), "storageData")
		repoID := str(storage, "backupRepositoryId")
		if repoID == "" {
			repoID = str(j, "repositoryId")
		}
		kind, noChain := jobKind(str(j, "type"))
		job := Job{
			ID: id, Name: str(j, "name"), Type: str(j, "type"), Kind: kind, NoChain: noChain, RepoID: repoID, RPO: 24,
			Sched: schedText(sub(full, "schedule")), GFS: gfsText(sub(storage, "gfsPolicy")), GFSPolicy: gfsPolicy(sub(storage, "gfsPolicy")),
			Comp: str(adv, "compressionLevel"), Block: str(adv, "storageOptimization"),
			Enc: boolean(sub(adv, "encryption"), "isEnabled"), Aaip: aaipConfig(full),
			Workload: str(j, "workload"), LastResult: str(j, "lastResult"), DetailError: dbgClip(detailErr, 200),
		}
		if job.Kind == "Backup to tape" {
			job.RPO = 168
		}
		if r := inv.Repo(repoID); r != nil {
			job.RepoName = r.Name
		} else {
			job.RepoName = str(j, "repositoryName")
		}
		for _, a := range arr(sub(sub(full, "guestProcessing"), "appAwareProcessing"), "appSettings") {
			as, _ := a.(map[string]any)
			if name, app := str(sub(as, "vmObject"), "name"), appFromSettings(as); name != "" && app != "" {
				apps[name] = app
			}
		}
		inv.Jobs = append(inv.Jobs, job)
	}
	progress(s, "jobs", len(jobs), len(jobs), "")

	// --- backups (cadenas): backup -> job / repo ---------------------------------
	progress(s, "backups", 0, 1, "")
	backups, err := getAll(ctx, s, "v1/backups", 0)
	if err != nil {
		return nil, err
	}
	for _, b := range backups {
		id, jobID := str(b, "id"), str(b, "jobId")
		if inv.Job(jobID) == nil && jobID != "" {
			// job que no aparece en ninguna lista (politica de plug-in, AHV, importado):
			// lo creamos desde el backup para que sus restore points tengan donde colgarse.
			kind, noChain := jobKind(str(b, "jobType"))
			inv.Jobs = append(inv.Jobs, Job{ID: jobID, Name: str(b, "name"), Type: str(b, "jobType"), Kind: kind, NoChain: noChain,
				RepoID: str(b, "repositoryId"), RepoName: str(b, "repositoryName"), RPO: 24, Sched: "—", GFS: "—", FromBackup: true})
		}
		inv.backupJob[id] = jobID
		inv.backupRepo[id] = str(b, "repositoryId")
		inv.backupPlatform[id] = str(b, "platformName")
		if j := inv.Job(jobID); j != nil {
			j.BackupIDs = append(j.BackupIDs, id)
			if j.RepoID == "" { // FIELD: backup copy no trae storage.backupRepositoryId
				j.RepoID = str(b, "repositoryId")
				if r := inv.Repo(j.RepoID); r != nil {
					j.RepoName = r.Name
				} else {
					j.RepoName = str(b, "repositoryName")
				}
			}
		}
	}

	// --- workloads: backupObjects agrupados por identidad de VM -------------------
	// Un mismo VM aparece una vez por backup (primario, copia, tape) con distinto
	// id de backupObject; lo unificamos por objectId (moref/uuid). Sin objectId
	// (agentes, plug-in ABR) la identidad es el propio backupObject.
	// FIELD: BackupObjectModel.backupId NO coincide con el backupId de sus restore
	// points (apunta a otro backup, a veces uno que /backups no lista). La relacion
	// job <-> objeto se toma de /backups/{id}/objects, que si es confiable.
	progress(s, "objects", 0, len(backups)+1, "")
	objects, err := getAll(ctx, s, "v1/backupObjects", 0)
	if err != nil {
		return nil, err
	}
	byKey := map[string]*Workload{}
	add := func(o obj) *Workload {
		boID := str(o, "id")
		key := str(o, "objectId")
		if key == "" {
			key = boID
		}
		w := byKey[key]
		if w == nil {
			w = &Workload{ID: key, Name: str(o, "name"), Platform: str(o, "platformName"), Kind: str(o, "type"),
				SizeGB: toGB(o, "size"), App: apps[str(o, "name")]}
			byKey[key] = w
		}
		if !contains(w.ObjectIDs, boID) {
			w.ObjectIDs = append(w.ObjectIDs, boID)
		}
		inv.objectVM[boID] = key
		if b := str(o, "backupId"); b != "" && inv.objectBackup[boID] == "" {
			inv.objectBackup[boID] = b
		}
		return w
	}
	for _, o := range objects {
		add(o)
	}
	link := func(w *Workload, jobID string) {
		if jobID == "" {
			return
		}
		if !contains(w.JobIDs, jobID) {
			w.JobIDs = append(w.JobIDs, jobID)
		}
		if j := inv.Job(jobID); j != nil && !contains(j.VMIDs, w.ID) {
			j.VMIDs = append(j.VMIDs, w.ID)
		}
	}
	for i, b := range backups {
		bid := str(b, "id")
		progress(s, "objects", i+1, len(backups)+1, str(b, "name"))
		objs, err := getAll(ctx, s, "v1/backups/"+bid+"/objects", 0)
		if err != nil {
			log.Printf("inventory: objects of backup %s failed: %v", bid, err)
			continue
		}
		for _, o := range objs {
			w := add(o)
			inv.objectBackup[str(o, "id")] = bid
			link(w, inv.backupJob[bid])
		}
		if len(objs) == 0 {
			// FIELD (vbrdb-01, agent Oracle RMAN): /backups/{id}/objects vacio aunque el
			// backup tiene restore points. Los archivos si traen objectIds: linkeamos por ahi.
			if bfs, err := getAll(ctx, s, "v1/backups/"+bid+"/backupFiles", 0); err == nil {
				for _, bf := range bfs {
					for _, oid := range strs(bf, "objectIds") {
						if key := inv.objectVM[oid]; key != "" {
							inv.objectBackup[oid] = bid
							link(byKey[key], inv.backupJob[bid])
						}
					}
				}
			}
		}
	}
	// fallback: objetos cuyo backupId si esta en /backups
	for _, w := range byKey {
		if len(w.JobIDs) == 0 {
			for _, oid := range w.ObjectIDs {
				link(w, inv.backupJob[inv.objectBackup[oid]])
			}
		}
	}
	for _, w := range byKey {
		inv.VMs = append(inv.VMs, *w)
	}
	sort.Slice(inv.VMs, func(a, b int) bool { return strings.ToLower(inv.VMs[a].Name) < strings.ToLower(inv.VMs[b].Name) })
	sort.Slice(inv.Jobs, func(a, b int) bool { return strings.ToLower(inv.Jobs[a].Name) < strings.ToLower(inv.Jobs[b].Name) })
	sort.Slice(inv.Repos, func(a, b int) bool { return strings.ToLower(inv.Repos[a].Name) < strings.ToLower(inv.Repos[b].Name) })
	linked := 0
	for _, w := range inv.VMs {
		if len(w.JobIDs) > 0 {
			linked++
		}
	}
	log.Printf("inventory: %d jobs, %d workloads (%d linked to a job), %d repositories, %d backups", len(inv.Jobs), len(inv.VMs), linked, len(inv.Repos), len(backups))
	return inv, nil
}

// listJobs devuelve la lista basica de jobs y de donde salio. Orden:
// /jobs/states -> /jobs -> /backups (sintetizados). Nunca aborta el inventario.
func listJobs(ctx context.Context, s *vbr.Session) ([]obj, string) {
	if states, err := getAll(ctx, s, "v1/jobs/states", 0); err == nil && len(states) > 0 {
		return states, "jobs/states"
	} else if err != nil {
		log.Printf("inventory: /jobs/states failed (%v), trying /jobs", err)
	}
	if jobs, err := getAll(ctx, s, "v1/jobs", 0); err == nil {
		return jobs, "jobs"
	} else {
		log.Printf("inventory: /jobs failed (%v), synthesizing the job list from /backups", err)
	}
	var out []obj
	seen := map[string]bool{}
	if backups, err := getAll(ctx, s, "v1/backups", 0); err == nil {
		for _, b := range backups {
			jid := str(b, "jobId")
			if jid == "" || seen[jid] {
				continue
			}
			seen[jid] = true
			out = append(out, obj{"id": jid, "name": str(b, "name"), "type": str(b, "jobType"),
				"repositoryId": str(b, "repositoryId"), "repositoryName": str(b, "repositoryName")})
		}
	}
	return out, "backups"
}

// appFromSettings: que aplicacion protege realmente un appSettings. FIELD: VBR
// devuelve los bloques sql/oracle/postgreSQL en TODOS los jobs con AAIP, con sus
// valores por defecto (sql.logsProcessing=Truncate, oracle.archiveLogs=Preserve,
// useGuestCredentials=true, backupLogs=false). Solo cuenta lo que se aparta del
// default; si nada se aparta, no hay app detectada (una VM PostgreSQL salia como
// "Oracle detected").
func appFromSettings(as obj) string {
	if o := sub(as, "oracle"); o != nil && (boolean(o, "backupLogs") || (o["useGuestCredentials"] != nil && !boolean(o, "useGuestCredentials")) ||
		(str(o, "archiveLogs") != "" && str(o, "archiveLogs") != "Preserve")) {
		return "Oracle"
	}
	if q := sub(as, "sql"); q != nil && (str(q, "logsProcessing") == "Backup" || str(q, "logsProcessing") == "NeverTruncate") {
		return "SQL"
	}
	if pg := sub(as, "postgreSQL"); pg != nil && (boolean(pg, "backupLogs") || (pg["useGuestCredentials"] != nil && !boolean(pg, "useGuestCredentials"))) {
		return "PostgreSQL"
	}
	return ""
}

func dbgClip(v string, n int) string {
	if len(v) > n {
		return v[:n] + "…"
	}
	return v
}

// ------------------------------------------------------------------ helpers

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}

// jobKind: EJobType -> etiqueta corta + si el job no genera cadena de backup
// (SureBackup, replicas, CDP) y por lo tanto no tiene restore points que mostrar.
// FIELD: valores vistos: VSphereBackup, HyperVBackup, CloudDirectorBackup,
// WindowsAgentBackup, LinuxAgentBackup, FileBackup, ObjectStorageBackup,
// VSphereReplica, SureBackupContentScan, BackupCopy; /jobs/states devuelve
// "Unknown" para jobs de plug-in (yogabench). Todo lo que no se reconoce cae en
// una categoria generica: la tool NUNCA falla por un tipo de job nuevo.
func jobKind(t string) (kind string, noChain bool) {
	switch t {
	case "Backup", "VSphereBackup", "HyperVBackup", "CloudDirectorBackup", "NutanixBackup", "AhvBackup", "ProxmoxBackup", "OvirtBackup", "KvmBackup", "":
		return "Backup", false
	case "BackupCopy", "SimpleBackupCopy", "ImmediateBackupCopy", "PeriodicBackupCopy":
		return "Backup copy", false
	case "BackupToTape":
		return "Backup to tape", false
	case "FileToTape":
		return "File to tape", false
	case "WindowsAgentBackup", "LinuxAgentBackup", "MacAgentBackup", "AgentBackup", "EpAgentBackup", "EpAgentPolicy", "AgentPolicy":
		return "Agent backup", false
	case "FileBackup", "NasBackup", "FileShareBackup":
		return "File share backup", false
	case "ObjectStorageBackup":
		return "Object storage backup", false
	case "EntraIDTenantBackup", "EntraIDAuditLogBackup":
		return "Entra ID backup", false
	}
	l := strings.ToLower(t)
	switch {
	case strings.Contains(l, "cdp"):
		return "CDP policy", true
	case strings.Contains(l, "replica"):
		return "Replica", true
	case strings.HasPrefix(l, "surebackup"):
		return "SureBackup", true
	case strings.Contains(l, "copy"):
		return "Backup copy", false
	case strings.Contains(l, "tape"):
		return "Backup to tape", false
	case strings.Contains(l, "agent"):
		return "Agent backup", false
	case strings.Contains(l, "rman"), strings.Contains(l, "oracle"), strings.Contains(l, "hana"), strings.Contains(l, "sap"),
		strings.Contains(l, "sql"), strings.Contains(l, "mongo"), strings.Contains(l, "plugin"), strings.Contains(l, "plug-in"),
		strings.Contains(l, "application"), strings.Contains(l, "backint"), strings.Contains(l, "db2"), strings.Contains(l, "postgre"):
		return "Application backup", false
	case strings.Contains(l, "kasten"), strings.Contains(l, "kubernetes"), strings.Contains(l, "k8s"):
		return "Kubernetes backup", false
	case strings.Contains(l, "aws"), strings.Contains(l, "azure"), strings.Contains(l, "gcp"), strings.Contains(l, "google"), strings.Contains(l, "cloud"):
		return "Cloud backup", false
	case strings.Contains(l, "nas"), strings.Contains(l, "file"), strings.Contains(l, "unstructured"), strings.Contains(l, "object"):
		return "File share backup", false
	case strings.Contains(l, "backup"):
		return "Backup", false
	case l == "unknown":
		return "Application backup", false
	}
	return "Other (" + t + ")", false
}

func gfsPolicy(p obj) GFSPolicy {
	if p == nil || !boolean(p, "isEnabled") {
		return GFSPolicy{}
	}
	g := GFSPolicy{Enabled: true}
	if w := sub(p, "weekly"); boolean(w, "isEnabled") {
		g.Weekly = int(num(w, "keepForNumberOfWeeks"))
	}
	if m := sub(p, "monthly"); boolean(m, "isEnabled") {
		g.Monthly = int(num(m, "keepForNumberOfMonths"))
	}
	if y := sub(p, "yearly"); boolean(y, "isEnabled") {
		g.Yearly = int(num(y, "keepForNumberOfYears"))
	}
	return g
}

func gfsText(p obj) string {
	if p == nil || !boolean(p, "isEnabled") {
		return "—"
	}
	var parts []string
	if w := sub(p, "weekly"); boolean(w, "isEnabled") {
		parts = append(parts, fmt.Sprintf("W%d", int(num(w, "keepForNumberOfWeeks"))))
	}
	if m := sub(p, "monthly"); boolean(m, "isEnabled") {
		parts = append(parts, fmt.Sprintf("M%d", int(num(m, "keepForNumberOfMonths"))))
	}
	if y := sub(p, "yearly"); boolean(y, "isEnabled") {
		parts = append(parts, fmt.Sprintf("Y%d", int(num(y, "keepForNumberOfYears"))))
	}
	if len(parts) == 0 {
		return "—"
	}
	return strings.Join(parts, " / ")
}

// schedText resume el schedule. FIELD: backup copy inmediato viene como
// {scheduleMode: Continuous, type: Immediate} sin runAutomatically.
func schedText(sch obj) string {
	if sch == nil {
		return "Manual"
	}
	if t := str(sch, "type"); t == "Immediate" || str(sch, "scheduleMode") == "Continuous" {
		return "Immediate"
	}
	if t := str(sch, "type"); t == "Periodically" || t == "Daily" || t == "Monthly" {
		// modo periodico explicito (copy jobs): sigue abajo con los detalles
	} else if !boolean(sch, "runAutomatically") && sch["runAutomatically"] != nil {
		return "Manual"
	}
	if d := sub(sch, "daily"); boolean(d, "isEnabled") {
		return strings.TrimSpace("Daily " + str(d, "localTime"))
	}
	if p := sub(sch, "periodically"); boolean(p, "isEnabled") {
		return fmt.Sprintf("Every %d %s", int(num(p, "frequency")), strings.ToLower(str(p, "periodicallyKind")))
	}
	if m := sub(sch, "monthly"); boolean(m, "isEnabled") {
		return "Monthly"
	}
	if boolean(sch, "runAutomatically") {
		return "Scheduled"
	}
	return "Manual"
}

// aaipConfig resume guestProcessing.appAwareProcessing. Toma la primera
// appSettings con datos.  // LAB: por VM (appSettings es por vmObject)
func aaipConfig(job obj) AaipConfig {
	gp := sub(job, "guestProcessing")
	aa := sub(gp, "appAwareProcessing")
	if aa == nil {
		return AaipConfig{}
	}
	if !boolean(aa, "isEnabled") {
		return AaipConfig{On: boolPtr(false)}
	}
	cfg := AaipConfig{On: boolPtr(true), Indexing: boolean(sub(gp, "guestFSIndexing"), "isEnabled")}
	for _, a := range arr(aa, "appSettings") {
		as, _ := a.(map[string]any)
		if cfg.VSS == "" {
			cfg.VSS = str(as, "vss")
		}
		if cfg.SQL == nil {
			cfg.SQL = sub(as, "sql")
		}
		if cfg.Oracle == nil {
			cfg.Oracle = sub(as, "oracle")
		}
		if cfg.Postgres == nil {
			cfg.Postgres = sub(as, "postgreSQL")
		}
	}
	return cfg
}

// immutabilityDays: object storage -> bucket.immutability; hardened ->
// repository.makeRecentBackupsImmutableDays; FIELD: algunos tipos traen
// repository.immutability{isEnabled, daysCount}.
func immutabilityDays(c obj) int {
	if b := sub(sub(c, "bucket"), "immutability"); boolean(b, "isEnabled") {
		return int(num(b, "daysCount"))
	}
	r := sub(c, "repository")
	if i := sub(r, "immutability"); boolean(i, "isEnabled") {
		if d := int(num(i, "daysCount")); d > 0 {
			return d
		}
	}
	return int(num(r, "makeRecentBackupsImmutableDays"))
}
