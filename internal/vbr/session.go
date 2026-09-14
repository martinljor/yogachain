// Package vbr habla con la REST API de Veeam Backup & Replication y guarda las
// sesiones. El resto de la app nunca ve una password ni un token: solo maneja
// un session_id opaco. (Mismo diseno que Yoga Benchmark.)
package vbr

import (
	"encoding/json"
	"sync"
	"time"
)

// Session es una conexion viva a un VBR (o una sesion demo). Los tokens se
// renuevan solos (ver renewToken en client.go): el usuario no tiene que
// reconectarse a los 25 minutos.
type Session struct {
	Demo       bool
	Host       string
	Port       int
	APIVersion string
	VerifySSL  bool
	CreatedAt  time.Time

	tokMu        sync.Mutex // protege los tokens (los GET corren en paralelo)
	renewMu      sync.Mutex // serializa la renovacion (una sola, no una por GET)
	accessToken  string
	refreshToken string
	expiresAt    time.Time

	// data: resultados ya calculados en esta sesion (ej: el inventario
	// normalizado), para no volver a pagar las llamadas REST en cada click.
	mu   sync.Mutex
	data map[string]any

	cacheMu sync.Mutex
	cache   map[string]cacheEntry

	// inflight: one request per path even if several parallel GETs hit the same
	// cache miss (p.ej. varios carriles pidiendo los mismos backupFiles).
	inflightMu sync.Mutex
	inflight   map[string]*inflightCall

	// trace: ultimas llamadas REST (path, status, ms, bytes, error) para el
	// bundle de diagnostico. En alpha es lo que permite ver que fallo en un
	// ambiente real sin pedirle al usuario que copie logs a mano.
	traceMu sync.Mutex
	trace   []TraceEntry
}

// TraceEntry: una llamada REST (o un hit de cache) registrada para diagnostico.
type TraceEntry struct {
	At     time.Time `json:"at"`
	Path   string    `json:"path"`
	Status int       `json:"status"`
	Ms     int64     `json:"ms"`
	Bytes  int       `json:"bytes"`
	Cached bool      `json:"cached,omitempty"`
	Err    string    `json:"err,omitempty"`
}

const traceMax = 300

func (s *Session) addTrace(e TraceEntry) {
	s.traceMu.Lock()
	defer s.traceMu.Unlock()
	s.trace = append(s.trace, e)
	if len(s.trace) > traceMax {
		s.trace = s.trace[len(s.trace)-traceMax:]
	}
}

// Trace devuelve una copia de las ultimas llamadas REST.
func (s *Session) Trace() []TraceEntry {
	s.traceMu.Lock()
	defer s.traceMu.Unlock()
	out := make([]TraceEntry, len(s.trace))
	copy(out, s.trace)
	return out
}

// Info: datos de la sesion sin credenciales ni tokens (para el diagnostico).
func (s *Session) Info() map[string]any {
	return map[string]any{
		"demo": s.Demo, "host": s.Host, "port": s.Port, "api_version": s.APIVersion,
		"verify_ssl": s.VerifySSL, "created_at": s.CreatedAt, "rest_calls": len(s.Trace()),
	}
}

// inflightCall: a fetch in progress that other callers can wait on.
type inflightCall struct {
	done chan struct{}
	body json.RawMessage
	err  error
}

// joinOrLead returns the call for this path and whether the caller must perform
// the fetch (leader) or just wait for it.
func (s *Session) joinOrLead(path string) (*inflightCall, bool) {
	s.inflightMu.Lock()
	defer s.inflightMu.Unlock()
	if c := s.inflight[path]; c != nil {
		return c, false
	}
	c := &inflightCall{done: make(chan struct{})}
	if s.inflight == nil {
		s.inflight = map[string]*inflightCall{}
	}
	s.inflight[path] = c
	return c, true
}

// leadDone publishes the result to everyone waiting on this path.
func (s *Session) leadDone(path string, c *inflightCall, body json.RawMessage, err error) {
	c.body, c.err = body, err
	s.inflightMu.Lock()
	delete(s.inflight, path)
	s.inflightMu.Unlock()
	close(c.done)
}

// cacheEntry: a GET response kept for a short while (see cacheable in client.go).
type cacheEntry struct {
	at   time.Time
	body json.RawMessage
}

func (s *Session) cacheGet(path string) (json.RawMessage, bool) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	e, ok := s.cache[path]
	if !ok || time.Since(e.at) > cacheTTL {
		return nil, false
	}
	return e.body, true
}

func (s *Session) cacheGetStale(path string) (json.RawMessage, bool) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	e, ok := s.cache[path]
	if !ok || time.Since(e.at) > staleTTL {
		return nil, false
	}
	return e.body, true
}

func (s *Session) cachePut(path string, body json.RawMessage) {
	s.cacheMu.Lock()
	defer s.cacheMu.Unlock()
	if s.cache == nil {
		s.cache = map[string]cacheEntry{}
	}
	s.cache[path] = cacheEntry{at: time.Now(), body: body}
}

// Set guarda un valor calculado en la sesion (se sobreescribe por key).
func (s *Session) Set(key string, v any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data == nil {
		s.data = map[string]any{}
	}
	s.data[key] = v
}

// Value devuelve un valor guardado con Set.
func (s *Session) Value(key string) (any, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.data[key]
	return v, ok
}

// SetTokens guarda el par de tokens y cuando expira el access token.
// expiresIn<=0 = sin dato: asumimos una vida corta y refrescamos por 401.
func (s *Session) SetTokens(access, refresh string, expiresIn int) {
	s.tokMu.Lock()
	defer s.tokMu.Unlock()
	s.accessToken, s.refreshToken = access, refresh
	if expiresIn > 0 {
		s.expiresAt = time.Now().Add(time.Duration(expiresIn) * time.Second)
	} else {
		s.expiresAt = time.Time{}
	}
}

// token devuelve el access token vigente y si conviene renovarlo ya (queda
// menos de un minuto de vida).
func (s *Session) token() (tok string, stale bool) {
	s.tokMu.Lock()
	defer s.tokMu.Unlock()
	return s.accessToken, !s.expiresAt.IsZero() && time.Now().After(s.expiresAt.Add(-time.Minute))
}

// Store guarda las sesiones en memoria. En una version productiva esto iria a
// un vault/persistencia.
type Store struct {
	mu       sync.RWMutex
	sessions map[string]*Session
}

func NewStore() *Store {
	return &Store{sessions: make(map[string]*Session)}
}

// New guarda la sesion con un id aleatorio y lo devuelve.
func (st *Store) New(s *Session) string {
	id := randID()
	st.mu.Lock()
	st.sessions[id] = s
	st.mu.Unlock()
	return id
}

func (st *Store) Get(id string) (*Session, bool) {
	st.mu.RLock()
	s, ok := st.sessions[id]
	st.mu.RUnlock()
	return s, ok
}

func (st *Store) Delete(id string) {
	st.mu.Lock()
	delete(st.sessions, id)
	st.mu.Unlock()
}

func (st *Store) Count() int {
	st.mu.RLock()
	n := len(st.sessions)
	st.mu.RUnlock()
	return n
}
