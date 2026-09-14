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

// LoadInventory carga (o devuelve de la sesion) jobs, workloads y repositorios.
func LoadInventory(ctx context.Context, s *vbr.Session) (*Inventory, error) {
	if v, ok := s.Value(invKey); ok {
		if inv, ok := v.(*Inventory); ok {
			return inv, nil
		}
	}
	inv, err := buildInventory(ctx, s)
	if err != nil {
		return nil, err
	}
	s.Set(invKey, inv)
	return inv, nil
}

// Refresh descarta el inventario cacheado (el usuario toco "actualizar").
func Refresh(s *vbr.Session) { s.Set(invKey, nil) }

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
	cfg := map[string]obj{}
	repos, err := getAll(ctx, s, "v1/backupInfrastructure/repositories", 0)
	if err != nil {
		return nil, err
	}
	for _, r := range repos {
		cfg[str(r, "id")] = r
	}
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
	apps := map[string]string{} // nombre de VM -> app (segun appSettings del job)
	jobs, err := getAll(ctx, s, "v1/jobs", 0)
	if err != nil {
		return nil, err
	}
	for _, j := range jobs {
		id := str(j, "id")
		full, err := getObj(ctx, s, "v1/jobs/"+id)
		if err != nil {
			log.Printf("inventory: job %s detail failed: %v", id, err)
			full = j
		}
		storage := sub(full, "storage")
		adv := sub(sub(storage, "advancedSettings"), "storageData")
		repoID := str(storage, "backupRepositoryId")
		kind, noChain := jobKind(str(j, "type"))
		job := Job{
			ID: id, Name: str(j, "name"), Type: str(j, "type"), Kind: kind, NoChain: noChain, RepoID: repoID, RPO: 24,
			Sched: schedText(sub(full, "schedule")), GFS: gfsText(sub(storage, "gfsPolicy")),
			Comp: str(adv, "compressionLevel"), Block: str(adv, "storageOptimization"),
			Enc: boolean(sub(adv, "encryption"), "isEnabled"), Aaip: aaipConfig(full),
		}
		if job.Kind == "Backup to tape" {
			job.RPO = 168
		}
		if r := inv.Repo(repoID); r != nil {
			job.RepoName = r.Name
		}
		for _, a := range arr(sub(sub(full, "guestProcessing"), "appAwareProcessing"), "appSettings") {
			as, _ := a.(map[string]any)
			name := str(sub(as, "vmObject"), "name")
			for key, app := range map[string]string{"sql": "SQL", "oracle": "Oracle", "postgreSQL": "PostgreSQL"} {
				if as[key] != nil && name != "" {
					apps[name] = app
				}
			}
		}
		inv.Jobs = append(inv.Jobs, job)
	}

	// --- backups (cadenas): backup -> job / repo ---------------------------------
	backups, err := getAll(ctx, s, "v1/backups", 0)
	if err != nil {
		return nil, err
	}
	for _, b := range backups {
		id, jobID := str(b, "id"), str(b, "jobId")
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
	for _, b := range backups {
		bid := str(b, "id")
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
// (SureBackup, replicas) y por lo tanto no tiene restore points que mostrar.
// FIELD: valores vistos en vbr-03: VSphereBackup, HyperVBackup, CloudDirectorBackup,
// WindowsAgentBackup, LinuxAgentBackup, FileBackup, ObjectStorageBackup,
// VSphereReplica, SureBackupContentScan, mas los copy.
func jobKind(t string) (kind string, noChain bool) {
	switch t {
	case "Backup", "VSphereBackup", "HyperVBackup", "CloudDirectorBackup", "NutanixBackup", "ProxmoxBackup", "":
		return "Backup", false
	case "BackupCopy", "SimpleBackupCopy", "ImmediateBackupCopy", "PeriodicBackupCopy":
		return "Backup copy", false
	case "BackupToTape":
		return "Backup to tape", false
	case "FileToTape":
		return "File to tape", false
	case "WindowsAgentBackup", "LinuxAgentBackup", "MacAgentBackup", "AgentBackup", "EpAgentBackup", "EpAgentPolicy":
		return "Agent backup", false
	case "FileBackup", "NasBackup":
		return "File share backup", false
	case "ObjectStorageBackup":
		return "Object storage backup", false
	case "EntraIDTenantBackup", "EntraIDAuditLogBackup":
		return "Entra ID backup", false
	}
	switch {
	case strings.Contains(t, "Replica"):
		return "Replica", true
	case strings.HasPrefix(t, "SureBackup"):
		return "SureBackup", true
	case strings.Contains(t, "Copy"):
		return "Backup copy", false
	}
	return t, false
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
