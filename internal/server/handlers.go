package server

import (
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"yogachain/internal/chain"
	"yogachain/internal/vbr"
)

// --- helpers ----------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeRaw(w http.ResponseWriter, raw json.RawMessage) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(raw)
}

// writeErr traduce un APIError a su status; el resto es 500.
func writeErr(w http.ResponseWriter, err error) {
	var ae *vbr.APIError
	if errors.As(err, &ae) {
		writeJSON(w, ae.Status, map[string]string{"detail": ae.Message})
		return
	}
	writeJSON(w, http.StatusInternalServerError, map[string]string{"detail": err.Error()})
}

// session resuelve la sesion del path o responde 404.
func (s *Server) session(w http.ResponseWriter, r *http.Request) (*vbr.Session, bool) {
	sess, ok := s.store.Get(r.PathValue("session"))
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"detail": "Session not found. Reconnect to VBR."})
		return nil, false
	}
	return sess, true
}

func queryInt(r *http.Request, key string, def int) int {
	if v, err := strconv.Atoi(r.URL.Query().Get(key)); err == nil && v >= 0 {
		return v
	}
	return def
}

// --- handlers ---------------------------------------------------------------

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "active_sessions": s.store.Count(), "version": s.version})
}

type connectRequest struct {
	Host       string `json:"host"`
	Port       int    `json:"port"`
	Username   string `json:"username"`
	Password   string `json:"password"`
	APIVersion string `json:"api_version"`
	VerifySSL  bool   `json:"verify_ssl"`
}

func (s *Server) connect(w http.ResponseWriter, r *http.Request) {
	var req connectRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "invalid body"})
		return
	}
	if req.Port == 0 {
		req.Port = 9419
	}
	// "auto": negotiate the highest supported revision (1.3-rev2 = 13.1, rev1 =
	// 13.0.1, rev0 = 13.0.0; 1.2-rev1 = v12.3). A server that does not support a
	// revision rejects it with an explicit version error, so we walk down.
	tryVersions := []string{req.APIVersion}
	if req.APIVersion == "" || req.APIVersion == "auto" {
		tryVersions = []string{"1.3-rev2", "1.3-rev1", "1.3-rev0", "1.2-rev1"}
	}
	var access, refresh string
	var expiresIn int
	var err error
	for i, v := range tryVersions {
		access, refresh, expiresIn, err = vbr.Authenticate(
			r.Context(), req.Host, req.Port, req.Username, req.Password, v, req.VerifySSL)
		if err == nil {
			req.APIVersion = v
			break
		}
		if i < len(tryVersions)-1 && isVersionError(err) {
			log.Printf("VBR: apiVersion %s not supported by %s, trying %s", v, req.Host, tryVersions[i+1]) // no password
			continue
		}
		log.Printf("VBR connection failed: host=%s port=%d apiVersion=%s: %v", req.Host, req.Port, v, err) // no password
		writeErr(w, err)
		return
	}
	sess := &vbr.Session{
		Host: req.Host, Port: req.Port, APIVersion: req.APIVersion, VerifySSL: req.VerifySSL,
		CreatedAt: time.Now(),
	}
	sess.SetTokens(access, refresh, expiresIn) // se renuevan solos (ver vbr.Get)
	id := s.store.New(sess)
	log.Printf("VBR connected: host=%s port=%d apiVersion=%s", req.Host, req.Port, req.APIVersion) // no password/token
	writeJSON(w, http.StatusOK, map[string]any{"session_id": id, "expires_in": expiresIn, "api_version": req.APIVersion})
}

// isVersionError: Veeam rejects an unsupported x-api-version with an explicit
// message; anything else (bad credentials, network) must not trigger a retry.
func isVersionError(err error) bool {
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "api-version") || strings.Contains(msg, "api version") ||
		strings.Contains(msg, "not supported") && strings.Contains(msg, "version")
}

func (s *Server) connectDemo(w http.ResponseWriter, r *http.Request) {
	sess := &vbr.Session{Demo: true, Host: "demo-vbr", Port: 9419, APIVersion: "demo", CreatedAt: time.Now()}
	id := s.store.New(sess)
	writeJSON(w, http.StatusOK, map[string]any{"session_id": id, "expires_in": 3600, "api_version": "demo"})
}

func (s *Server) disconnect(w http.ResponseWriter, r *http.Request) {
	s.store.Delete(r.PathValue("session"))
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) inventory(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	inv, err := chain.LoadInventory(r.Context(), sess)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, inv)
}

func (s *Server) inventoryRefresh(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	chain.Refresh(sess)
	s.inventory(w, r)
}

// restorePoints: ?pivot=job|vm|repo&id=...&days=30&skip=0&limit=100&aaip=1
// Pagina de a 100 para que la UI vaya pintando mientras carga. aaip=1 completa
// el resultado de guest processing por RP (2-3 llamadas REST por RP: usar solo
// en modo Aplicacion).
func (s *Server) restorePoints(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	q := r.URL.Query()
	pivot, id := q.Get("pivot"), q.Get("id")
	if pivot == "" || id == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"detail": "pivot and id are required"})
		return
	}
	days := queryInt(r, "days", 30)
	if days < 1 || days > 3650 {
		days = 30
	}
	skip, limit := queryInt(r, "skip", 0), queryInt(r, "limit", 100)
	if limit < 1 || limit > 1000 {
		limit = 100
	}

	inv, err := chain.LoadInventory(r.Context(), sess)
	if err != nil {
		writeErr(w, err)
		return
	}
	// La lista completa del pivot se calcula una vez y se guarda en la sesion
	// (key por pivot/id/days) para servir las paginas siguientes sin volver a VBR.
	key := "rps:" + pivot + ":" + id + ":" + strconv.Itoa(days)
	var rps []chain.RestorePoint
	if skip > 0 {
		if v, ok := sess.Value(key); ok {
			rps, _ = v.([]chain.RestorePoint)
		}
	}
	if rps == nil {
		rps, err = chain.RestorePoints(r.Context(), sess, inv, pivot, id, time.Now().AddDate(0, 0, -days))
		if err != nil {
			writeErr(w, err)
			return
		}
		sess.Set(key, rps)
	}
	end := skip + limit
	if skip > len(rps) {
		skip = len(rps)
	}
	if end > len(rps) {
		end = len(rps)
	}
	page := make([]chain.RestorePoint, end-skip)
	copy(page, rps[skip:end])
	if q.Get("aaip") == "1" {
		for i := range page {
			chain.Aaip(r.Context(), sess, inv, &page[i])
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"total": len(rps), "skip": skip, "limit": limit, "data": page})
}

func (s *Server) restorePoint(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	inv, err := chain.LoadInventory(r.Context(), sess)
	if err != nil {
		writeErr(w, err)
		return
	}
	d, err := chain.LoadDetail(r.Context(), sess, inv, r.PathValue("rp"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, d)
}

// rawGet reenvia el JSON crudo de una ruta de la REST API (ej: /api/{s}/raw/v1/backups).
func (s *Server) rawGet(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.session(w, r)
	if !ok {
		return
	}
	path := r.PathValue("path")
	if r.URL.RawQuery != "" {
		path += "?" + r.URL.RawQuery
	}
	raw, err := vbr.Get(r.Context(), sess, path)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeRaw(w, raw)
}
