package chain

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"

	"yogachain/internal/vbr"
)

// Todo lo marcado con  // LAB  hay que confirmarlo contra un VBR real: el schema
// sale de la referencia 1.3-rev2 pero algunos valores (tipos de job, nombres de
// plataforma, escala de los ratios, textos de log) conviene verlos con datos reales.

const invKey = "inventory"

// Inventory carga (o devuelve de la sesion) jobs, workloads y repositorios.
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

	// --- repositorios: config (inmutabilidad) + estado (capacidad) -------------
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
		job := Job{
			ID: id, Name: str(j, "name"), Kind: jobKind(str(j, "type")), RepoID: repoID, RPO: 24,
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
			if j.RepoID == "" { // backup copy / tape a veces no traen backupRepositoryId en storage  // LAB
				j.RepoID = str(b, "repositoryId")
				if r := inv.Repo(j.RepoID); r != nil {
					j.RepoName = r.Name
				}
			}
		}
	}

	// --- workloads: backupObjects agrupados por identidad de VM -------------------
	// Un mismo VM aparece una vez por backup (primario, copia, tape) con distinto
	// id de backupObject; lo unificamos por objectId (moref/uuid) y, si no viene,
	// por nombre.  // LAB: confirmar campo objectId en BackupObjectModel.
	objects, err := getAll(ctx, s, "v1/backupObjects", 0)
	if err != nil {
		return nil, err
	}
	byKey := map[string]*Workload{}
	for _, o := range objects {
		boID := str(o, "id")
		key := str(o, "objectId")
		if key == "" {
			key = strings.ToLower(str(o, "name"))
		}
		w := byKey[key]
		if w == nil {
			w = &Workload{ID: key, Name: str(o, "name"), Platform: str(o, "platformName"), SizeGB: toGB(o, "size"), App: apps[str(o, "name")]}
			byKey[key] = w
		}
		w.ObjectIDs = append(w.ObjectIDs, boID)
		inv.objectVM[boID] = key
		inv.objectBackup[boID] = str(o, "backupId")
		if jobID := inv.backupJob[str(o, "backupId")]; jobID != "" && !contains(w.JobIDs, jobID) {
			w.JobIDs = append(w.JobIDs, jobID)
			if j := inv.Job(jobID); j != nil && !contains(j.VMIDs, key) {
				j.VMIDs = append(j.VMIDs, key)
			}
		}
	}
	for _, w := range byKey {
		inv.VMs = append(inv.VMs, *w)
	}
	sort.Slice(inv.VMs, func(a, b int) bool { return inv.VMs[a].Name < inv.VMs[b].Name })
	sort.Slice(inv.Jobs, func(a, b int) bool { return inv.Jobs[a].Name < inv.Jobs[b].Name })
	sort.Slice(inv.Repos, func(a, b int) bool { return inv.Repos[a].Name < inv.Repos[b].Name })
	log.Printf("inventory: %d jobs, %d workloads, %d repositories, %d backups", len(inv.Jobs), len(inv.VMs), len(inv.Repos), len(backups))
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

// jobKind: EJobType -> etiqueta corta.  // LAB: completar con los valores reales
func jobKind(t string) string {
	switch t {
	case "Backup", "":
		return "Backup"
	case "BackupCopy", "SimpleBackupCopy", "ImmediateBackupCopy":
		return "Backup copy"
	case "BackupToTape":
		return "Backup to tape"
	case "FileToTape":
		return "File to tape"
	case "AgentBackup", "EpAgentBackup":
		return "Agent backup"
	case "Replica":
		return "Replica"
	}
	return t
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

func schedText(sch obj) string {
	if sch == nil || !boolean(sch, "runAutomatically") {
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
	return "Scheduled"
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
// repository.makeRecentBackupsImmutableDays.  // LAB
func immutabilityDays(c obj) int {
	if b := sub(sub(c, "bucket"), "immutability"); boolean(b, "isEnabled") {
		return int(num(b, "daysCount"))
	}
	return int(num(sub(c, "repository"), "makeRecentBackupsImmutableDays"))
}
