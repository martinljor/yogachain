package server

// diagnostics.go — bundle de diagnostico para la fase alpha. Un JSON que el
// usuario descarga desde la consola y comparte: version, SO, sesion (sin
// credenciales ni tokens), resumen del inventario normalizado, muestras crudas
// de la REST API (1-2 items por coleccion, para validar el schema real contra
// lo asumido en internal/chain) y la traza de llamadas REST con status/ms/error.

import (
	"encoding/json"
	"net/http"
	"runtime"
	"strconv"
	"time"

	"yogachain/internal/chain"
	"yogachain/internal/vbr"
)

func (s *Server) diagnostics(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	out := map[string]any{
		"tool":         "yogachain",
		"version":      s.version,
		"generated_at": time.Now().Format(time.RFC3339),
		"runtime":      map[string]any{"go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH},
		"session":      sess.Info(),
	}

	// Inventario normalizado (resumen, no todo)
	inv, err := chain.LoadInventory(ctx, sess)
	if err != nil {
		out["inventory_error"] = err.Error()
	} else {
		kinds := map[string]int{}
		aaipOn := 0
		for _, j := range inv.Jobs {
			kinds[j.Kind]++
			if j.Aaip.On != nil && *j.Aaip.On {
				aaipOn++
			}
		}
		apps := map[string]int{}
		multi := 0
		for _, v := range inv.VMs {
			if v.App != "" {
				apps[v.App]++
			}
			if len(v.ObjectIDs) > 1 {
				multi++
			}
		}
		out["inventory"] = map[string]any{
			"jobs": len(inv.Jobs), "job_kinds": kinds, "jobs_with_aaip": aaipOn,
			"workloads": len(inv.VMs), "workloads_with_app": apps, "workloads_in_several_backups": multi,
			"repositories": len(inv.Repos), "repos": inv.Repos, "jobs_detail": inv.Jobs,
		}
	}

	// Muestras crudas: lo minimo para ver el schema real (limit=1 o 2)
	samples := map[string]any{}
	sample := func(key, path string) {
		raw, err := vbr.Get(ctx, sess, path)
		if err != nil {
			samples[key] = map[string]string{"error": err.Error(), "path": path}
			return
		}
		var v any
		if json.Unmarshal(raw, &v) != nil {
			samples[key] = map[string]string{"error": "non-JSON", "path": path}
			return
		}
		samples[key] = map[string]any{"path": path, "body": v}
	}
	sample("jobs", "v1/jobs?skip=0&limit=1")
	sample("repositories", "v1/backupInfrastructure/repositories?skip=0&limit=1")
	sample("repository_states", "v1/backupInfrastructure/repositories/states?skip=0&limit=1")
	sample("backups", "v1/backups?skip=0&limit=1")
	sample("backupObjects", "v1/backupObjects?skip=0&limit=2")
	if inv != nil {
		if len(inv.Jobs) > 0 {
			sample("job_detail", "v1/jobs/"+inv.Jobs[0].ID)
			if len(inv.Jobs[0].BackupIDs) > 0 {
				sample("backupFiles", "v1/backups/"+inv.Jobs[0].BackupIDs[0]+"/backupFiles?skip=0&limit=2")
			}
		}
		if len(inv.VMs) > 0 && len(inv.VMs[0].ObjectIDs) > 0 {
			sample("restorePoints", "v1/backupObjects/"+inv.VMs[0].ObjectIDs[0]+"/restorePoints?skip=0&limit=2")
			if rps, err := chain.RestorePoints(ctx, sess, inv, "vm", inv.VMs[0].ID, time.Now().AddDate(0, 0, -7)); err == nil && len(rps) > 0 {
				last := rps[len(rps)-1]
				out["normalized_sample"] = last
				if last.SessionID != "" {
					sample("taskSessions", "v1/sessions/"+last.SessionID+"/taskSessions?skip=0&limit=5")
				}
			} else if err != nil {
				out["restore_points_error"] = err.Error()
			}
		}
	}
	// FIELD (vbrdb-01): los backups de plug-in (CustomPlatform) no publican restore
	// points; muestreamos hasta 3 para ver que SI exponen (archivos, objetos, sesiones).
	if inv != nil {
		n := 0
		for _, j := range inv.Jobs {
			if j.Kind != "Application backup" || len(j.BackupIDs) == 0 || n >= 3 {
				continue
			}
			n++
			bid := j.BackupIDs[0]
			key := "plugin_" + strconv.Itoa(n) + "_"
			samples[key+"job"] = map[string]any{"name": j.Name, "type": j.Type, "backupId": bid, "fromBackup": j.FromBackup, "detailError": j.DetailError}
			sample(key+"objects", "v1/backups/"+bid+"/objects?skip=0&limit=5")
			sample(key+"backupFiles", "v1/backups/"+bid+"/backupFiles?skip=0&limit=5")
			sample(key+"sessions", "v1/sessions?jobIdFilter="+bid+"&skip=0&limit=3")
			for _, v := range inv.VMs {
				if contains(v.JobIDs, j.ID) && len(v.ObjectIDs) > 0 {
					sample(key+"restorePoints", "v1/backupObjects/"+v.ObjectIDs[0]+"/restorePoints?skip=0&limit=3")
					break
				}
			}
		}
	}
	out["samples"] = samples
	out["rest_trace"] = sess.Trace()

	w.Header().Set("Content-Disposition", "attachment; filename=yogachain-diagnostics-"+time.Now().Format("20060102-150405")+".json")
	writeJSON(w, http.StatusOK, out)
}

func contains(xs []string, x string) bool {
	for _, v := range xs {
		if v == x {
			return true
		}
	}
	return false
}
