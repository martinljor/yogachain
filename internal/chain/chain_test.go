package chain

import (
	"context"
	"testing"
	"time"

	"yogachain/internal/vbr"
)

// Los tests corren contra la sesion demo (JSON con forma de VBR), asi ejercitan
// el mismo mapeo que en produccion.

func demoSession() *vbr.Session { return &vbr.Session{Demo: true, Host: "demo-vbr", Port: 9419} }

func TestInventoryDemo(t *testing.T) {
	inv, err := LoadInventory(context.Background(), demoSession())
	if err != nil {
		t.Fatalf("inventario demo: %v", err)
	}
	if len(inv.Jobs) != 7 || len(inv.Repos) != 4 || len(inv.VMs) != 14 {
		t.Fatalf("inventario: jobs=%d repos=%d vms=%d (esperado 7/4/14)", len(inv.Jobs), len(inv.Repos), len(inv.VMs))
	}
	j := inv.Job("j7")
	if j == nil || j.Kind != "Backup" || j.Comp != "DedupFriendly" || j.GFS != "W4 / M12 / Y1" {
		t.Fatalf("job j7 mal mapeado: %+v", j)
	}
	if j.Aaip.On == nil || !*j.Aaip.On || j.Aaip.Oracle == nil || j.Aaip.Oracle["archiveLogs"] != "DeleteExpiredHours" {
		t.Fatalf("AAIP Oracle no mapeado: %+v", j.Aaip)
	}
	if r := inv.Repo("r3"); r == nil || r.ImmDays != 30 {
		t.Fatalf("inmutabilidad object storage no mapeada: %+v", r)
	}
	ora := inv.VM("vm-v13")
	if ora == nil || ora.App != "Oracle" || len(ora.ObjectIDs) != 2 { // primario + tape
		t.Fatalf("workload ora-erp-01 mal unificado: %+v", ora)
	}
}

func TestRestorePointsAndDetail(t *testing.T) {
	ctx := context.Background()
	s := demoSession()
	inv, err := LoadInventory(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	rps, err := RestorePoints(ctx, s, inv, "job", "j7", time.Now().AddDate(0, 0, -30))
	if err != nil {
		t.Fatal(err)
	}
	if len(rps) < 50 {
		t.Fatalf("pocos RP para j7 en 30 dias: %d", len(rps))
	}
	var fulls int
	for _, r := range rps {
		if r.VMID == "" || r.RepoID != "r1" || r.SizeGB <= 0 || r.DataSize <= r.SizeGB {
			t.Fatalf("RP mal mapeado: %+v", r)
		}
		if r.CompressRatio < 1.2 || r.CompressRatio > 1.5 {
			t.Fatalf("compressRatio fuera de rango (escala %%): %v", r.CompressRatio)
		}
		if r.Type == "full" {
			fulls++
		}
	}
	if fulls == 0 {
		t.Fatal("ningun full en la cadena Oracle")
	}

	// detalle de un RP de la BD en NOARCHIVELOG: advertencia y sin tape/copia
	var dwh RestorePoint
	for _, r := range rps {
		if r.VMID == "vm-v14" && r.Type == "inc" {
			dwh = r
		}
	}
	d, err := LoadDetail(ctx, s, inv, dwh.ID)
	if err != nil {
		t.Fatal(err)
	}
	if d.RP.Aaip != "warn" || d.RP.AaipDetail == "" {
		t.Fatalf("AAIP esperado warn (NOARCHIVELOG), got %q %q", d.RP.Aaip, d.RP.AaipDetail)
	}
	if len(d.Chain) < 2 || !d.Chain[0].Full || d.Needed.Files < 2 {
		t.Fatalf("cadena mal armada: len=%d needed=%+v", len(d.Chain), d.Needed)
	}

	// pivot repo: el hardened tiene SQL + Oracle
	byRepo, err := RestorePoints(ctx, s, inv, "repo", "r1", time.Now().AddDate(0, 0, -7))
	if err != nil {
		t.Fatal(err)
	}
	if len(byRepo) == 0 {
		t.Fatal("sin RP por repositorio")
	}

	// synth se refina con taskSession.algorithm en el detalle
	for _, r := range rps {
		if r.VMID == "vm-v13" && r.Type == "full" {
			dd, err := LoadDetail(ctx, s, inv, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if dd.RP.Type != "synth" {
				t.Fatalf("full sabatino de j7 deberia ser sintetico, got %s", dd.RP.Type)
			}
			break
		}
	}
}

func TestClassifyGuestLogs(t *testing.T) {
	logs := []obj{
		{"title": "Queued for processing", "status": "Succeeded"},
		{"title": "Guest processing: VSS freeze ok", "status": "Succeeded"},
		{"title": "Failed to delete archived logs: ORA-19809", "status": "Warning"},
	}
	if res, det := classifyGuestLogs(logs); res != "warn" || det == "" {
		t.Fatalf("esperado warn, got %s (%s)", res, det)
	}
	logs = append(logs, obj{"title": "Failed to prepare guest for freeze: VSS timeout", "status": "Failed"})
	if res, _ := classifyGuestLogs(logs); res != "fail" {
		t.Fatalf("esperado fail, got %s", res)
	}
}
