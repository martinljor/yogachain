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
	if len(inv.Jobs) != 9 || len(inv.Repos) != 5 || len(inv.VMs) != 15 {
		t.Fatalf("inventario: jobs=%d repos=%d vms=%d (esperado 9/5/15)", len(inv.Jobs), len(inv.Repos), len(inv.VMs))
	}
	j := inv.Job("j7")
	if j == nil || j.Kind != "Backup" || j.Type != "VSphereBackup" || j.Comp != "DedupFriendly" || j.GFS != "W4 / M12 / Y1" {
		t.Fatalf("job j7 mal mapeado: %+v", j)
	}
	if j.Aaip.On == nil || !*j.Aaip.On || j.Aaip.Oracle == nil || j.Aaip.Oracle["archiveLogs"] != "DeleteExpiredHours" {
		t.Fatalf("AAIP Oracle no mapeado: %+v", j.Aaip)
	}
	if r := inv.Repo("r3"); r == nil || r.ImmDays != 30 {
		t.Fatalf("inmutabilidad object storage no mapeada: %+v", r)
	}
	if sure := inv.Job("j-sure"); sure == nil || !sure.NoChain || sure.Kind != "SureBackup" {
		t.Fatalf("SureBackup deberia marcarse sin cadena: %+v", sure)
	}
	if sobr := inv.Repo("sobr-1"); sobr == nil || sobr.Type != "ScaleOut" || sobr.CapacityGB != 120000 {
		t.Fatalf("SOBR no mapeado: %+v", sobr)
	}
	if len(inv.Job("j1").VMIDs) != 3 {
		t.Fatalf("j1 deberia tener 3 VMs via /backups/{id}/objects: %v", inv.Job("j1").VMIDs)
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

func TestPluginBackupFromFiles(t *testing.T) {
	ctx := context.Background()
	s := demoSession()
	inv, err := LoadInventory(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	j := inv.Job("j-plug")
	if j == nil || !j.FromBackup || j.Kind != "Application backup" || len(j.VMIDs) != 1 {
		t.Fatalf("job de plug-in sintetizado mal: %+v", j)
	}
	rps, err := RestorePoints(ctx, s, inv, "job", "j-plug", time.Now().AddDate(0, 0, -7))
	if err != nil {
		t.Fatal(err)
	}
	if len(rps) < 14 || len(rps) > 16 {
		t.Fatalf("esperaba ~16 piezas en 7 dias, got %d", len(rps))
	}
	for _, r := range rps {
		if !r.FromFile || r.Type != "file" || r.VMID != "bo-plug" || r.CompressRatio < 2.6 || r.CompressRatio > 2.8 || r.SizeGB <= 0 {
			t.Fatalf("pieza mal mapeada: %+v", r)
		}
	}
	d, err := LoadDetail(ctx, s, inv, rps[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.Chain) != 42 || d.RP.ID != rps[0].ID {
		t.Fatalf("detalle de pieza: chain=%d rp=%s", len(d.Chain), d.RP.ID)
	}
}

func TestRatioSemantics(t *testing.T) {
	// FIELD: 65 % restante -> 1.54x; 25 % -> 4x; 0 -> sin dato -> 1x
	for v, want := range map[float64]float64{65: 100.0 / 65, 25: 4, 100: 1, 0: 1} {
		if got := ratio(v); got < want-0.001 || got > want+0.001 {
			t.Fatalf("ratio(%v)=%v, esperado %v", v, got, want)
		}
	}
	if k, nc := jobKind("WindowsAgentBackup"); k != "Agent backup" || nc {
		t.Fatalf("WindowsAgentBackup -> %q %v", k, nc)
	}
	if k, nc := jobKind("VSphereReplica"); k != "Replica" || !nc {
		t.Fatalf("VSphereReplica -> %q %v", k, nc)
	}
	// FIELD (vbrdb-01 / cdp): tipos que no deben romper nunca el inventario
	for typ, want := range map[string]string{"VSphereCdpReplica": "CDP policy", "CdpPolicy": "CDP policy", "Unknown": "Application backup",
		"OracleRmanBackup": "Application backup", "SapHanaBackup": "Application backup", "MongoDbBackup": "Application backup",
		"HyperVReplica": "Replica", "SureBackup": "SureBackup", "KastenBackup": "Kubernetes backup", "AzureVmBackup": "Cloud backup", "WeirdNewType": "Other (WeirdNewType)"} {
		if k, _ := jobKind(typ); k != want {
			t.Fatalf("jobKind(%s) = %q, esperado %q", typ, k, want)
		}
	}
	for _, typ := range []string{"VSphereCdpReplica", "HyperVReplica", "SureBackupContentScan"} {
		if _, nc := jobKind(typ); !nc {
			t.Fatalf("jobKind(%s) deberia ser sin cadena", typ)
		}
	}
	if got := schedText(obj{"scheduleMode": "Continuous", "type": "Immediate"}); got != "Immediate" {
		t.Fatalf("schedText copy inmediato = %q", got)
	}
}

func TestProgress(t *testing.T) {
	s := demoSession()
	if p := InventoryProgress(s); p.Stage != "idle" {
		t.Fatalf("progreso inicial = %+v", p)
	}
	if _, err := LoadInventory(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if p := InventoryProgress(s); p.Stage != "done" || p.Percent != 100 {
		t.Fatalf("progreso final = %+v", p)
	}
	Refresh(s)
	if p := InventoryProgress(s); p.Stage != "idle" {
		t.Fatalf("progreso tras refresh = %+v", p)
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
