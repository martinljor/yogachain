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
	// 1) que backupObjects hay que consultar (sin duplicados: un mismo objeto puede
	//    llegar por varios caminos y cada GET repetido es una llamada real a VBR)
	var objectIDs []string
	seenObj := map[string]bool{}
	addObj := func(oid string) {
		if oid != "" && !seenObj[oid] {
			seenObj[oid] = true
			objectIDs = append(objectIDs, oid)
		}
	}
	keep := func(rp RestorePoint) bool { return true }
	switch pivot {
	case "job":
		j := inv.Job(id)
		if j == nil {
			return nil, &vbr.APIError{Status: 404, Message: "Job not found"}
		}
		if j.NoChain {
			return nil, nil
		}
		for _, w := range inv.VMs {
			if contains(w.JobIDs, j.ID) {
				for _, oid := range w.ObjectIDs {
					if contains(j.BackupIDs, inv.objectBackup[oid]) {
						addObj(oid)
					}
				}
			}
		}
		// FIELD: /backupObjects/{id}/restorePoints puede devolver RPs de OTROS backups
		// (copias, otra cadena del mismo objeto): nos quedamos con los del job.
		keep = func(rp RestorePoint) bool { return contains(j.BackupIDs, rp.BackupID) }
	case "vm":
		w := inv.VM(id)
		if w == nil {
			return nil, &vbr.APIError{Status: 404, Message: "Workload not found"}
		}
		for _, oid := range w.ObjectIDs {
			addObj(oid)
		}
	case "repo":
		for _, w := range inv.VMs {
			for _, oid := range w.ObjectIDs {
				if inv.backupRepo[inv.objectBackup[oid]] == id {
					addObj(oid)
				}
			}
		}
		keep = func(rp RestorePoint) bool { return rp.RepoID == id }
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
	seenRP := map[string]bool{}
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
			rp := mapRP(inv, raw, inv.objectVM[r.oid], files[str(raw, "backupFileId")])
			if !keep(rp) || seenRP[rp.ID] {
				continue
			}
			seenRP[rp.ID] = true
			out = append(out, rp)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Date < out[b].Date })
	return out, nil
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
	if platform == "" {
		platform = str(raw, "platformName")
	}

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
		ImmUntil: imm, Mal: mal, File: strings.TrimSpace(str(bf, "name")), AllowedOperations: strs(raw, "allowedOperations"),
		Platform: str(raw, "platformName"), GuestOS: str(raw, "guestOsFamily"),
		SessionID: str(raw, "sessionId"),
	}
}

// ratio: BackupFileModel expone dedupRatio/compressRatio como enteros que son el
// PORCENTAJE DEL TAMANO QUE QUEDA despues de cada etapa (FIELD, vbr-03:
// compress 65 + dedup 93 con dataSize 20.3 GB -> backupSize 12.5 GB = 61 %;
// compress 63 + dedup 25 -> 16 %). Factor de reduccion = 100 / valor.
// 0 = sin dato (archivos placeholder de Nutanix/ABR) -> 1x.
func ratio(v float64) float64 {
	if v <= 0 {
		return 1
	}
	return 100 / v
}

// isFullFile: por si el tipo del RP no viene, la extension del archivo lo dice.
func isFullFile(name string) bool {
	return strings.HasSuffix(strings.ToLower(name), ".vbk")
}
