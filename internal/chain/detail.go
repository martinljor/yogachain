package chain

import (
	"context"
	"log"
	"math"
	"sort"
	"strings"
	"time"

	"yogachain/internal/vbr"
)

// Detail es lo que muestra el panel derecho al hacer click en un RP.
type Detail struct {
	RP        RestorePoint   `json:"rp"`
	Chain     []RestorePoint `json:"chain"`     // desde el ultimo full hasta el siguiente
	Needed    Needed         `json:"needed"`    // archivos a leer para restaurar este punto
	Locations []RestorePoint `json:"locations"` // el mismo dato en otros repos (copia, tape)
}

type Needed struct {
	Files  int     `json:"files"`
	SizeGB float64 `json:"sizeGB"`
}

// LoadDetail arma el detalle de un RP: resultado AAIP, cadena y ubicaciones.
func LoadDetail(ctx context.Context, s *vbr.Session, inv *Inventory, rpID string) (*Detail, error) {
	if strings.HasPrefix(rpID, "bf:") {
		return fileDetail(ctx, s, inv, strings.TrimPrefix(rpID, "bf:"))
	}
	rp, err := GetRestorePoint(ctx, s, inv, rpID)
	if err != nil {
		return nil, err
	}
	Aaip(ctx, s, inv, &rp)

	t0, _ := time.Parse(time.RFC3339, rp.Date)
	// FIELD: el full que abre la cadena puede ser MUY anterior al punto (325 dias en
	// vbrdb-01: full de octubre + incrementales hoy). Se trae toda la historia del
	// workload (una llamada paginada por backupObject; la retencion acota el volumen).
	if rp.VMID == "" {
		// sin objectIds en el archivo: si el backup tiene un solo objeto, es ese
		var only string
		for oid, b := range inv.objectBackup {
			if b == rp.BackupID {
				if only != "" && inv.objectVM[oid] != only {
					only = "?"
					break
				}
				only = inv.objectVM[oid]
			}
		}
		if only != "" && only != "?" {
			rp.VMID = only
		}
	}
	var sameVM []RestorePoint
	if rp.VMID != "" {
		sameVM, err = RestorePoints(ctx, s, inv, "vm", rp.VMID, t0.AddDate(-10, 0, 0))
		if err != nil {
			return nil, err
		}
	} else {
		log.Printf("detail: restore point %s has no workload (backupFile.objectIds did not match any backupObject); chain limited to itself", rp.ID)
	}
	d := &Detail{RP: rp, Chain: []RestorePoint{rp}, Needed: Needed{1, rp.SizeGB}}

	// cadena: RPs del mismo job/VM, desde el full anterior (inclusive) hasta el
	// siguiente full (exclusive)
	var same []RestorePoint
	for _, r := range sameVM {
		if r.JobID == rp.JobID {
			same = append(same, r)
		}
	}
	sort.Slice(same, func(a, b int) bool { return same[a].Date < same[b].Date })
	idx := -1
	for i, r := range same {
		if r.ID == rp.ID {
			idx = i
		}
	}
	if idx >= 0 {
		a := idx
		for a > 0 && !same[a].Full {
			a--
		}
		b := idx + 1
		for b < len(same) && !same[b].Full {
			b++
		}
		d.Chain = same[a:b]
		needed := same[a : idx+1]
		d.Needed = Needed{Files: len(needed)}
		for _, r := range needed {
			d.Needed.SizeGB += r.SizeGB
		}
	}

	// ubicaciones: el mismo workload, otro job/repo, dentro de +-18 h (la copia
	// y el tape se crean horas despues del primario)
	for _, r := range sameVM {
		if r.ID == rp.ID || r.JobID == rp.JobID {
			continue
		}
		t, _ := time.Parse(time.RFC3339, r.Date)
		if math.Abs(t.Sub(t0).Hours()) <= 18 {
			d.Locations = append(d.Locations, r)
		}
	}
	return d, nil
}

// fileDetail: detalle de un punto sintetizado desde un BackupFileModel (plug-in).
// La "cadena" son todos los archivos del mismo backup; sin AAIP ni ubicaciones.
func fileDetail(ctx context.Context, s *vbr.Session, inv *Inventory, fileID string) (*Detail, error) {
	for bid := range inv.backupJob {
		bfs, err := getAll(ctx, s, "v1/backups/"+bid+"/backupFiles", 0)
		if err != nil {
			continue
		}
		var chain []RestorePoint
		var me *RestorePoint
		for _, bf := range bfs {
			rp := fileRP(inv, bid, bf, nil)
			chain = append(chain, rp)
			if str(bf, "id") == fileID {
				me = &chain[len(chain)-1]
			}
		}
		if me == nil {
			continue
		}
		sort.Slice(chain, func(a, b int) bool { return chain[a].Date < chain[b].Date })
		for i := range chain {
			if chain[i].ID == "bf:"+fileID {
				me = &chain[i]
			}
		}
		me.AaipDetail = "Plug-in backup: VBR exposes backup files, not restore points, over REST. Full/incremental/log pieces are known to the application catalog (RMAN, backint), not to the API."
		return &Detail{RP: *me, Chain: chain, Needed: Needed{Files: 1, SizeGB: me.SizeGB}}, nil
	}
	return nil, &vbr.APIError{Status: 404, Message: "Backup file not found"}
}
