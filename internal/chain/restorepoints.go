package chain

import (
	"context"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"yogachain/internal/vbr"
)

// RestorePoints devuelve los RP normalizados para un pivot (job | vm | repo)
// creados desde `since`. Se consulta por backupObject (una llamada por VM y
// backup, paginada), asi cada RP queda atado a su workload sin ambiguedad, y se
// unen los BackupFileModel (compresion, dedup, GFS, nombre de archivo) por
// backupFileId.
func RestorePoints(ctx context.Context, s *vbr.Session, inv *Inventory, pivot, id string, since time.Time) ([]RestorePoint, error) {
	// 1) que backupObjects hay que consultar
	var objectIDs []string
	switch pivot {
	case "job":
		j := inv.Job(id)
		if j == nil {
			return nil, &vbr.APIError{Status: 404, Message: "Job not found"}
		}
		for _, w := range inv.VMs {
			if contains(w.JobIDs, j.ID) {
				for _, oid := range w.ObjectIDs {
					if contains(j.BackupIDs, objectBackup(ctx, s, inv, oid)) {
						objectIDs = append(objectIDs, oid)
					}
				}
			}
		}
	case "vm":
		w := inv.VM(id)
		if w == nil {
			return nil, &vbr.APIError{Status: 404, Message: "Workload not found"}
		}
		objectIDs = w.ObjectIDs
	case "repo":
		for _, w := range inv.VMs {
			for _, oid := range w.ObjectIDs {
				if inv.backupRepo[objectBackup(ctx, s, inv, oid)] == id {
					objectIDs = append(objectIDs, oid)
				}
			}
		}
	default:
		return nil, &vbr.APIError{Status: 400, Message: "pivot must be job, vm or repo"}
	}

	// 2) restore points por objeto, en paralelo acotado (vbr.Get es concurrente-seguro)
	after := since.UTC().Format("2006-01-02T15:04:05Z")
	type result struct {
		oid string
		rps []obj
		err error
	}
	results := make([]result, len(objectIDs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, oid := range objectIDs {
		wg.Add(1)
		go func(i int, oid string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			rps, err := getAll(ctx, s, "v1/backupObjects/"+oid+"/restorePoints?createdAfterFilter="+after, 0)
			results[i] = result{oid, rps, err}
		}(i, oid)
	}
	wg.Wait()

	// 3) archivos de backup por backup (cacheados en la sesion por vbr.Get)
	files := map[string]obj{}
	seenBackup := map[string]bool{}
	var out []RestorePoint
	for _, r := range results {
		if r.err != nil {
			log.Printf("restorePoints: object %s failed: %v", r.oid, r.err)
			continue
		}
		for _, raw := range r.rps {
			bid := str(raw, "backupId")
			if !seenBackup[bid] {
				seenBackup[bid] = true
				bfs, err := getAll(ctx, s, "v1/backups/"+bid+"/backupFiles", 0)
				if err != nil {
					log.Printf("restorePoints: backupFiles of %s failed: %v (sizes/ratios unavailable)", bid, err)
				}
				for _, bf := range bfs {
					files[str(bf, "id")] = bf
				}
			}
			out = append(out, mapRP(inv, raw, inv.objectVM[r.oid], files[str(raw, "backupFileId")]))
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Date < out[b].Date })
	return out, nil
}

// objectBackup: backupId de un backupObject (lo guarda el inventario al listar
// objetos; si falta, se pregunta a la API).
func objectBackup(ctx context.Context, s *vbr.Session, inv *Inventory, oid string) string {
	if b, ok := inv.objectBackup[oid]; ok {
		return b
	}
	o, err := getObj(ctx, s, "v1/backupObjects/"+oid)
	if err != nil {
		return ""
	}
	return str(o, "backupId")
}

// RestorePoint trae un RP puntual (para el detalle), con su BackupFile.
func GetRestorePoint(ctx context.Context, s *vbr.Session, inv *Inventory, rpID string) (RestorePoint, error) {
	raw, err := getObj(ctx, s, "v1/restorePoints/"+rpID)
	if err != nil {
		return RestorePoint{}, err
	}
	bid := str(raw, "backupId")
	var bf obj
	if bfs, err := getAll(ctx, s, "v1/backups/"+bid+"/backupFiles", 0); err == nil {
		for _, f := range bfs {
			if str(f, "id") == str(raw, "backupFileId") {
				bf = f
			}
		}
	}
	// vmId: por el objectIds[] del archivo (cadenas per-VM)  // LAB
	vmID := ""
	for _, oid := range strs(bf, "objectIds") {
		if v := inv.objectVM[oid]; v != "" {
			vmID = v
		}
	}
	return mapRP(inv, raw, vmID, bf), nil
}

var gfsLabel = map[string]string{"Weekly": "Weekly", "Monthly": "Monthly", "Quarterly": "Quarterly", "Yearly": "Yearly"}

func mapRP(inv *Inventory, raw obj, vmID string, bf obj) RestorePoint {
	bid := str(raw, "backupId")
	jobID := inv.backupJob[bid]
	job := inv.Job(jobID)
	kind := "Backup"
	if job != nil {
		kind = job.Kind
	}
	platform := inv.backupPlatform[bid]

	// tipo: la API da Increment/Full; sintetico vs activo lo refina Aaip() con
	// taskSession.algorithm cuando se pide el detalle.
	rtype := "inc"
	switch {
	case platform == "Tape" || kind == "Backup to tape":
		rtype = "tape"
	case kind == "Backup copy":
		rtype = "copy"
	case str(raw, "type") == "Full":
		rtype = "full"
	}

	gfs := ""
	periods := strs(bf, "gfsPeriods")
	if len(periods) == 0 {
		periods = strs(raw, "gfsPeriods")
	}
	for _, p := range periods {
		if p != "" && p != "None" {
			gfs = gfsLabel[p]
			if gfs == "" {
				gfs = p
			}
			break
		}
	}

	created := str(raw, "creationTime")
	imm := ""
	if r := inv.Repo(inv.backupRepo[bid]); r != nil && r.ImmDays > 0 {
		if t, err := time.Parse(time.RFC3339, created); err == nil {
			imm = t.AddDate(0, 0, r.ImmDays).Format(time.RFC3339)
		}
	}
	mal := str(raw, "malwareStatus")
	if mal == "" {
		mal = str(bf, "severity")
	}
	if mal == "" || mal == "Informative" {
		mal = "Clean"
	}
	data := toGB(bf, "dataSize")
	if data == 0 {
		data = toGB(raw, "originalSize")
	}
	return RestorePoint{
		ID: str(raw, "id"), JobID: jobID, VMID: vmID, RepoID: inv.backupRepo[bid], BackupID: bid,
		BackupFileID: str(raw, "backupFileId"), Date: created, Type: rtype, Full: str(raw, "type") == "Full" || rtype == "tape", GFS: gfs,
		SizeGB: toGB(bf, "backupSize"), DataSize: data,
		CompressRatio: ratio(num(bf, "compressRatio")), DedupRatio: ratio(num(bf, "dedupRatio")),
		ImmUntil: imm, Mal: mal, File: str(bf, "name"), AllowedOperations: strs(raw, "allowedOperations"),
		SessionID: str(raw, "sessionId"),
	}
}

// ratio: BackupFileModel expone dedupRatio/compressRatio como enteros. En la UI
// de VBR se ven como "1.3x"; asumimos porcentaje (130) si el valor es grande y
// factor (1.3) si es chico.  // LAB
func ratio(v float64) float64 {
	switch {
	case v <= 0:
		return 1
	case v > 20:
		return v / 100
	}
	return v
}

// isFullFile: por si el tipo del RP no viene, la extension del archivo lo dice.
func isFullFile(name string) bool {
	return strings.HasSuffix(strings.ToLower(name), ".vbk")
}
