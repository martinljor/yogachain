// Package chain traduce la REST API de VBR al modelo que consume el frontend:
// jobs, workloads, repositorios y restore points normalizados, mas la logica de
// cadena (que archivos hacen falta para restaurar), ubicaciones (donde mas existe
// el mismo dato) y el resultado de application-aware processing.
//
// Los nombres JSON (camelCase, tipos inc/full/synth/copy/tape) son los que usa
// frontend/index.html. Ver docs/API-MAPPING.md para el origen de cada campo.
package chain

// AaipConfig: guestProcessing.appAwareProcessing del job, resumido.
type AaipConfig struct {
	On       *bool          `json:"on"` // nil = hereda (backup copy / tape)
	VSS      string         `json:"vss,omitempty"`
	SQL      map[string]any `json:"sql,omitempty"`
	Oracle   map[string]any `json:"oracle,omitempty"`
	Postgres map[string]any `json:"postgres,omitempty"`
	Indexing bool           `json:"indexing"`
}

type Job struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	Type      string     `json:"type"`    // EJobType crudo (VSphereBackup, BackupCopy, ...)
	Kind      string     `json:"kind"`    // etiqueta: Backup | Backup copy | Agent backup | ...
	NoChain   bool       `json:"noChain"` // SureBackup / replica: sin restore points de backup
	RepoID    string     `json:"repoId"`
	RepoName  string     `json:"repoName"`
	Sched     string     `json:"sched"`
	GFS       string     `json:"gfs"`
	RPO       int        `json:"rpo"` // horas; VBR no expone RPO, es un parametro de la tool
	Comp      string     `json:"comp"`
	Block     string     `json:"block"`
	Enc       bool       `json:"enc"`
	Aaip      AaipConfig `json:"aaip"`
	VMIDs     []string   `json:"vmIds"`
	BackupIDs []string   `json:"backupIds"`
}

type Workload struct {
	ID        string   `json:"id"` // identidad del workload (BackupObjectModel.objectId o nombre)
	Name      string   `json:"name"`
	OS        string   `json:"os"`
	SizeGB    float64  `json:"sizeGB"`
	App       string   `json:"app,omitempty"` // SQL | Oracle | PostgreSQL | AD
	DB        string   `json:"db,omitempty"`
	Platform  string   `json:"platform"`
	Kind      string   `json:"kind,omitempty"` // BackupObjectModel.type (VM, ...)
	JobIDs    []string `json:"jobIds"`
	ObjectIDs []string `json:"objectIds"` // backupObject ids (uno por backup que lo contiene)
}

type Repo struct {
	ID         string  `json:"id"`
	Name       string  `json:"name"`
	Type       string  `json:"type"`
	ImmDays    int     `json:"immDays"`
	CapacityGB float64 `json:"capacityGB"`
	UsedGB     float64 `json:"usedGB"`
	FreeGB     float64 `json:"freeGB"`
	Online     bool    `json:"online"`
}

type RestorePoint struct {
	ID                string   `json:"id"`
	JobID             string   `json:"jobId"`
	VMID              string   `json:"vmId"`
	RepoID            string   `json:"repoId"`
	BackupID          string   `json:"backupId"`
	BackupFileID      string   `json:"backupFileId"`
	Date              string   `json:"date"` // RFC3339
	Type              string   `json:"type"` // inc | full | synth | copy | tape
	Full              bool     `json:"full"` // abre cadena (VBR type == Full), tambien para copy/tape
	GFS               string   `json:"gfs,omitempty"`
	SizeGB            float64  `json:"sizeGB"`   // backupSize
	DataSize          float64  `json:"dataSize"` // dataSize (antes de comprimir/deduplicar)
	CompressRatio     float64  `json:"compressRatio"`
	DedupRatio        float64  `json:"dedupRatio"`
	ImmUntil          string   `json:"immUntil,omitempty"`
	Mal               string   `json:"mal"` // Clean | Suspicious | Infected
	File              string   `json:"file"`
	AllowedOperations []string `json:"allowedOperations"`
	Platform          string   `json:"platform,omitempty"` // platformName del RP (VMware, Nutanix, LinuxPhysical...)
	GuestOS           string   `json:"guestOs,omitempty"`  // guestOsFamily (Windows, Linux, Other)
	SessionID         string   `json:"-"`
	Aaip              string   `json:"aaip,omitempty"` // ok | warn | fail | off
	AaipDetail        string   `json:"aaipDetail,omitempty"`
}

type Inventory struct {
	Jobs       []Job      `json:"jobs"`
	VMs        []Workload `json:"vms"`
	Repos      []Repo     `json:"repos"`
	Server     string     `json:"server"`
	APIVersion string     `json:"apiVersion"`

	// indices internos (no se serializan)
	backupJob      map[string]string // backupId -> jobId
	backupRepo     map[string]string // backupId -> repositoryId
	backupPlatform map[string]string
	objectVM       map[string]string // backupObject id -> workload id
	objectBackup   map[string]string // backupObject id -> backupId
}

func (inv *Inventory) Job(id string) *Job {
	for i := range inv.Jobs {
		if inv.Jobs[i].ID == id {
			return &inv.Jobs[i]
		}
	}
	return nil
}

func (inv *Inventory) VM(id string) *Workload {
	for i := range inv.VMs {
		if inv.VMs[i].ID == id {
			return &inv.VMs[i]
		}
	}
	return nil
}

func (inv *Inventory) Repo(id string) *Repo {
	for i := range inv.Repos {
		if inv.Repos[i].ID == id {
			return &inv.Repos[i]
		}
	}
	return nil
}

func boolPtr(b bool) *bool { return &b }
