package chain

import (
	"context"
	"log"
	"strings"

	"yogachain/internal/vbr"
)

// Aaip completa rp.Aaip / rp.AaipDetail (resultado de application-aware
// processing) y refina rp.Type (full sintetico vs activo). La REST API no expone
// un flag por restore point: se deriva de la task session del RP y de sus logs
// (2-3 llamadas por RP, por eso solo se hace a pedido: detalle o modo Aplicacion).
//
//	restorePoint.sessionId -> GET /sessions/{id}/taskSessions (buscar restorePointId)
//	                       -> GET /taskSessions/{id}/logs      (status Warning/Failed)
func Aaip(ctx context.Context, s *vbr.Session, inv *Inventory, rp *RestorePoint) {
	job := inv.Job(rp.JobID)
	if job == nil || job.Aaip.On == nil {
		return // backup copy / tape: hereda del origen
	}
	if !*job.Aaip.On {
		rp.Aaip = "off"
		return
	}
	if rp.SessionID == "" {
		rp.Aaip, rp.AaipDetail = "ok", "No sessionId on the restore point"
		return
	}
	tasks, err := getAll(ctx, s, "v1/sessions/"+rp.SessionID+"/taskSessions", 0)
	if err != nil {
		log.Printf("aaip: taskSessions of %s failed: %v", rp.SessionID, err)
		return
	}
	var task obj
	for _, t := range tasks {
		if str(t, "restorePointId") == rp.ID {
			task = t
			break
		}
	}
	if task == nil && len(tasks) == 1 {
		task = tasks[0]
	}
	if task == nil {
		rp.Aaip, rp.AaipDetail = "ok", "Task session not found"
		return
	}
	if rp.Type == "full" && str(task, "algorithm") == "Synthetic" {
		rp.Type = "synth"
	}
	logs, err := getAll(ctx, s, "v1/taskSessions/"+str(task, "id")+"/logs", 0)
	if err != nil {
		log.Printf("aaip: logs of task %s failed: %v", str(task, "id"), err)
		return
	}
	rp.Aaip, rp.AaipDetail = classifyGuestLogs(logs)
}

// guestKeywords: lineas del log que hablan de guest processing.  // LAB: afinar
// con logs reales (VSS, Oracle, SQL, PostgreSQL, indexacion).
var guestKeywords = []string{"guest", "vss", "freeze", "application", "oracle", "sql", "postgre", "archiv", "log backup", "transaction", "index"}

// classifyGuestLogs devuelve ok|warn|fail y el titulo de la linea que lo justifica.
func classifyGuestLogs(logs []obj) (string, string) {
	var firstOK string
	result := "ok"
	detail := ""
	for _, l := range logs {
		title := str(l, "title")
		low := strings.ToLower(title)
		guest := false
		for _, k := range guestKeywords {
			if strings.Contains(low, k) {
				guest = true
				break
			}
		}
		if !guest {
			continue
		}
		switch str(l, "status") {
		case "Failed":
			return "fail", title
		case "Warning":
			if result != "fail" {
				result, detail = "warn", title
			}
		default:
			if firstOK == "" {
				firstOK = title
			}
		}
	}
	if detail == "" {
		detail = firstOK
	}
	return result, detail
}
