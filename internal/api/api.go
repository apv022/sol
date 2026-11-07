// Package api is the control panel's JSON API and static file server.
//
// The panel listens on 127.0.0.1 only and is never exposed through a
// transport. On top of that, every request must carry a loopback Host header
// (blocks DNS-rebinding attacks from web pages), and every state-changing
// request must carry the X-CSRF-Token returned by GET /api/status.
//
// The API talks to the node only through the Backend interface, so it can be
// tested with a fake and the node can change without touching HTTP code.
//
// Routes:
//
//	GET    /api/status                      node id, uptime, transports, csrf token
//	GET    /api/services                    list (with public addresses)
//	POST   /api/services                    add {name, local_port, visibility, protocol}
//	PUT    /api/services/{name}/visibility  {visibility}
//	DELETE /api/services/{name}
//	GET    /api/devices
//	POST   /api/devices/pair                -> {link, expires}
//	DELETE /api/devices/{id}
//	GET    /api/files                       list blobs
//	POST   /api/files                       multipart upload, field "file"
//	GET    /blob/{hash}                     stream a blob (range requests)
//	GET    /api/events                      recent log
//	GET    /api/audit                       leak-check results
package api

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"time"

	"github.com/apv022/sol/internal/audit"
	"github.com/apv022/sol/internal/blob"
	"github.com/apv022/sol/internal/config"
	"github.com/apv022/sol/internal/events"
	"github.com/apv022/sol/internal/transport"
)

// maxUpload bounds a single file upload.
const maxUpload = 4 << 30 // 4 GiB

// Backend is what the API needs from the node.
type Backend interface {
	Status() StatusInfo
	Services() []ServiceView
	AddService(config.Service) error
	RemoveService(name string) error
	SetServiceVisibility(name, visibility string) error
	Devices() []DeviceView
	RemoveDevice(id string) error
	NewPairing() (PairingInfo, error)
	Blobs() *blob.Store
	Events() *events.Log
	Audit(ctx context.Context) []audit.Result
}

// StatusInfo is the body of GET /api/status (plus the CSRF token).
type StatusInfo struct {
	NodeID     string             `json:"node_id"`
	Version    string             `json:"version"`
	StartedAt  time.Time          `json:"started_at"`
	Uptime     string             `json:"uptime"`
	Transports []transport.Status `json:"transports"`
	CSRF       string             `json:"csrf,omitempty"`
}

// ServiceView is a service plus live state.
type ServiceView struct {
	config.Service
	Addresses     map[string]string `json:"addresses,omitempty"` // public services only
	Running       bool              `json:"running"`             // something listens on local_port
	ActiveStreams int               `json:"active_streams"`      // open device connections
}

// DeviceView is a paired device plus its live connections.
type DeviceView struct {
	config.Device
	// Online is true while the device has at least one open connection.
	// Every connection passed the sol/1 handshake: the device proved it
	// holds the private key paired as PubKey by signing a fresh nonce.
	Online         bool             `json:"online"`
	Connections    []ConnectionView `json:"connections"`
	KeyFingerprint string           `json:"key_fingerprint"`
	TorAuth        bool             `json:"tor_auth"` // also locked at the Tor layer
}

// ConnectionView groups one device's open connections to one service.
type ConnectionView struct {
	Service string    `json:"service"`
	Streams int       `json:"streams"` // open TCP streams (a browser opens several)
	Since   time.Time `json:"since"`   // oldest open stream
}

// PairingInfo is the body of POST /api/devices/pair.
type PairingInfo struct {
	Link    string    `json:"link"`
	Expires time.Time `json:"expires"`
}

type server struct {
	b    Backend
	csrf string
}

// Handler returns the panel handler serving the API and the static files in web.
func Handler(b Backend, web fs.FS) http.Handler {
	tok := make([]byte, 32)
	rand.Read(tok)
	s := &server{b: b, csrf: base64.RawURLEncoding.EncodeToString(tok)}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/status", s.status)
	mux.HandleFunc("GET /api/services", s.listServices)
	mux.HandleFunc("POST /api/services", s.addService)
	mux.HandleFunc("PUT /api/services/{name}/visibility", s.setVisibility)
	mux.HandleFunc("DELETE /api/services/{name}", s.removeService)
	mux.HandleFunc("GET /api/devices", s.listDevices)
	mux.HandleFunc("POST /api/devices/pair", s.pair)
	mux.HandleFunc("DELETE /api/devices/{id}", s.removeDevice)
	mux.HandleFunc("GET /api/files", s.listFiles)
	mux.HandleFunc("POST /api/files", s.upload)
	mux.Handle("GET /blob/", b.Blobs().Handler("/blob/"))
	mux.HandleFunc("GET /api/events", s.events)
	mux.HandleFunc("GET /api/audit", s.audit)
	mux.Handle("GET /", http.FileServerFS(web))
	return s.guard(mux)
}

// guard enforces loopback Host and CSRF, and sets security headers.
func (s *server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !loopbackHost(r.Host) {
			http.Error(w, "forbidden host", http.StatusForbidden)
			return
		}
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self' data:; media-src 'self'; style-src 'self'; script-src 'self'; frame-ancestors 'none'")
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			got := r.Header.Get("X-CSRF-Token")
			if subtle.ConstantTimeCompare([]byte(got), []byte(s.csrf)) != 1 {
				http.Error(w, "missing or bad CSRF token", http.StatusForbidden)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func loopbackHost(hostport string) bool {
	host, _, err := net.SplitHostPort(hostport)
	if err != nil {
		host = hostport
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	code := http.StatusBadRequest
	switch {
	case errors.Is(err, config.ErrNotFound):
		code = http.StatusNotFound
	case errors.Is(err, config.ErrDuplicate):
		code = http.StatusConflict
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func (s *server) status(w http.ResponseWriter, r *http.Request) {
	st := s.b.Status()
	st.CSRF = s.csrf
	writeJSON(w, http.StatusOK, st)
}

func (s *server) listServices(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.b.Services())
}

func (s *server) addService(w http.ResponseWriter, r *http.Request) {
	var svc config.Service
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<16)).Decode(&svc); err != nil {
		writeErr(w, err)
		return
	}
	if err := s.b.AddService(svc); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]bool{"ok": true})
}

func (s *server) setVisibility(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Visibility string `json:"visibility"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&body); err != nil {
		writeErr(w, err)
		return
	}
	if err := s.b.SetServiceVisibility(r.PathValue("name"), body.Visibility); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) removeService(w http.ResponseWriter, r *http.Request) {
	if err := s.b.RemoveService(r.PathValue("name")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) listDevices(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.b.Devices())
}

func (s *server) pair(w http.ResponseWriter, r *http.Request) {
	p, err := s.b.NewPairing()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, p)
}

func (s *server) removeDevice(w http.ResponseWriter, r *http.Request) {
	if err := s.b.RemoveDevice(r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) listFiles(w http.ResponseWriter, r *http.Request) {
	list, err := s.b.Blobs().List()
	if err != nil {
		writeErr(w, err)
		return
	}
	if list == nil {
		list = []blob.Entry{}
	}
	writeJSON(w, http.StatusOK, list)
}

func (s *server) upload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxUpload)
	mr, err := r.MultipartReader()
	if err != nil {
		writeErr(w, err)
		return
	}
	for {
		part, err := mr.NextPart()
		if err != nil {
			writeErr(w, errors.New(`no "file" field in upload`))
			return
		}
		if part.FormName() != "file" {
			continue
		}
		// Stream straight to disk; the file is never held in memory.
		hash, err := s.b.Blobs().PutNamed(part, part.FileName())
		if err != nil {
			writeErr(w, err)
			return
		}
		s.b.Events().Add(events.KindService, fmt.Sprintf("file stored: %s (%s…)", part.FileName(), hash[:12]))
		writeJSON(w, http.StatusCreated, map[string]string{"hash": hash})
		return
	}
}

func (s *server) events(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.b.Events().Recent(200))
}

func (s *server) audit(w http.ResponseWriter, r *http.Request) {
	res := s.b.Audit(r.Context())
	if res == nil {
		res = []audit.Result{}
	}
	writeJSON(w, http.StatusOK, res)
}
