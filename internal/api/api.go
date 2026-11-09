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
//	GET    /api/status                      node id, name, uptime, transports, csrf token
//	PUT    /api/name                        {name}: this node's name, as peers see it
//	POST   /api/transports/{name}/restart   reconnect a transport (e.g. tor after a network change)
//	GET    /api/services                    list (with public addresses)
//	POST   /api/services                    add {name, local_port, visibility, protocol}
//	PUT    /api/services/{name}/visibility  {visibility}
//	DELETE /api/services/{name}
//	GET    /api/peers                       peers with live connections (never credentials)
//	POST   /api/peers/invite                -> {link, expires}
//	POST   /api/peers/pair                  {link}: join another node
//	POST   /api/peers/{id}/refresh          ask the peer what it shares now
//	PUT    /api/peers/{id}/shares/{service} {shared}: let the peer open a service, or stop
//	POST   /api/peers/{id}/send             multipart field "file": send to the peer's inbox
//	DELETE /api/peers/{id}                  unpair (and tell the peer)
//	POST   /api/peers/{id}/services/{service}/open  {port?}: keep the peer's service on 127.0.0.1 -> tunnel
//	GET    /api/tunnels                     open tunnels with their status
//	DELETE /api/tunnels/{port}              close a tunnel
//	GET    /api/files                       list blobs (with who sent each)
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
	"io"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/apv022/sol/internal/audit"
	"github.com/apv022/sol/internal/autostart"
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
	SetName(name string) error
	Services() []ServiceView
	AddService(config.Service) error
	RemoveService(name string) error
	SetServiceVisibility(name, visibility string) error
	Peers() []PeerView
	Invite() (InviteInfo, error)
	Pair(ctx context.Context, link string) (config.Peer, error)
	RefreshPeer(ctx context.Context, id string) (config.Peer, error)
	SetShare(id, service string, shared bool) (config.Peer, error)
	SendFile(ctx context.Context, id, name string, r io.Reader) (hash string, size int64, err error)
	Unpair(ctx context.Context, id string) (p config.Peer, notified bool, err error)
	RestartTransport(name string) error
	Tunnels() []TunnelView
	OpenTunnel(peer, service string, port int) (TunnelView, error)
	CloseTunnel(port int) error
	Blobs() *blob.Store
	Events() *events.Log
	Audit(ctx context.Context) []audit.Result
}

// StatusInfo is the body of GET /api/status (plus the CSRF token).
type StatusInfo struct {
	NodeID     string             `json:"node_id"`
	Name       string             `json:"name"`
	Version    string             `json:"version"`
	StartedAt  time.Time          `json:"started_at"`
	Uptime     string             `json:"uptime"`
	Transports []transport.Status `json:"transports"`
	Autostart  autostart.State    `json:"autostart"` // starts at login? (sol autostart)
	CSRF       string             `json:"csrf,omitempty"`
}

// ServiceView is a service plus live state.
type ServiceView struct {
	config.Service
	Addresses     map[string]string `json:"addresses,omitempty"` // public services only
	Running       bool              `json:"running"`             // something listens on local_port
	ActiveStreams int               `json:"active_streams"`      // open peer connections to it
}

// PeerView is a peer as the panel shows it. It deliberately has no access
// credentials: those never leave the node.
type PeerView struct {
	ID       string                 `json:"id"`
	NodeID   string                 `json:"node_id"`
	Name     string                 `json:"name"`
	Addrs    map[string]string      `json:"addrs"`    // how we reach it, per transport
	Locked   []string               `json:"locked"`   // transports where only this peer's key opens our address
	Shares   []string               `json:"shares"`   // what we let it open
	Services []config.RemoteService `json:"services"` // what it lets us open
	Added    time.Time              `json:"added"`
	LastSeen time.Time              `json:"last_seen"`
	// Online is true while there is at least one open connection either way.
	// Every connection passed the signed handshake with this peer's key.
	Online      bool             `json:"online"`
	Connections []ConnectionView `json:"connections"`
}

// ConnectionView groups one peer's open connections to one service.
type ConnectionView struct {
	Service  string    `json:"service"`
	Outbound bool      `json:"outbound"`      // we opened it to the peer's service
	Streams  int       `json:"streams"`       // open TCP streams (a browser opens several)
	Since    time.Time `json:"since"`         // oldest open stream
	Via      string    `json:"via,omitempty"` // outbound: transport that carries it ("tor", "direct", ...)
}

// TunnelView is a peer's service this node keeps reachable on 127.0.0.1.
type TunnelView struct {
	Port      int       `json:"port"`
	Peer      string    `json:"peer"` // peer id
	PeerName  string    `json:"peer_name"`
	Service   string    `json:"service"`
	Protocol  string    `json:"protocol"`
	Addr      string    `json:"addr"`          // 127.0.0.1:<port>
	URL       string    `json:"url,omitempty"` // http services: what to open in a browser
	Listening bool      `json:"listening"`     // false: the port couldn't be opened
	Active    int       `json:"active"`        // connections in use right now
	LastOK    time.Time `json:"last_ok"`       // last time the peer answered
	LastError string    `json:"last_error,omitempty"`
	Via       string    `json:"via,omitempty"` // transport of the last good connection
}

// InviteInfo is the body of POST /api/peers/invite.
type InviteInfo struct {
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
	mux.HandleFunc("PUT /api/name", s.setName)
	mux.HandleFunc("POST /api/transports/{name}/restart", s.restartTransport)
	mux.HandleFunc("GET /api/services", s.listServices)
	mux.HandleFunc("POST /api/services", s.addService)
	mux.HandleFunc("PUT /api/services/{name}/visibility", s.setVisibility)
	mux.HandleFunc("DELETE /api/services/{name}", s.removeService)
	mux.HandleFunc("GET /api/peers", s.listPeers)
	mux.HandleFunc("POST /api/peers/invite", s.invite)
	mux.HandleFunc("POST /api/peers/pair", s.pair)
	mux.HandleFunc("POST /api/peers/{id}/refresh", s.refreshPeer)
	mux.HandleFunc("PUT /api/peers/{id}/shares/{service}", s.setShare)
	mux.HandleFunc("POST /api/peers/{id}/send", s.sendFile)
	mux.HandleFunc("DELETE /api/peers/{id}", s.unpair)
	mux.HandleFunc("POST /api/peers/{id}/services/{service}/open", s.openTunnel)
	mux.HandleFunc("GET /api/tunnels", s.listTunnels)
	mux.HandleFunc("DELETE /api/tunnels/{port}", s.closeTunnel)
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

func (s *server) restartTransport(w http.ResponseWriter, r *http.Request) {
	if err := s.b.RestartTransport(r.PathValue("name")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
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

func (s *server) setName(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&body); err != nil {
		writeErr(w, err)
		return
	}
	if err := s.b.SetName(body.Name); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) listPeers(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.b.Peers())
}

func (s *server) invite(w http.ResponseWriter, r *http.Request) {
	p, err := s.b.Invite()
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// peerTimeout bounds panel requests that reach out to a peer over Tor.
const peerTimeout = 5 * time.Minute

func (s *server) pair(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Link string `json:"link"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<14)).Decode(&body); err != nil {
		writeErr(w, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), peerTimeout)
	defer cancel()
	p, err := s.b.Pair(ctx, body.Link)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"id": p.ID, "name": p.Name})
}

func (s *server) refreshPeer(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Minute)
	defer cancel()
	if _, err := s.b.RefreshPeer(ctx, r.PathValue("id")); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) setShare(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Shared bool `json:"shared"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&body); err != nil {
		writeErr(w, err)
		return
	}
	if _, err := s.b.SetShare(r.PathValue("id"), r.PathValue("service"), body.Shared); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *server) sendFile(w http.ResponseWriter, r *http.Request) {
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
		ctx, cancel := context.WithTimeout(r.Context(), 6*time.Hour)
		hash, size, err := s.b.SendFile(ctx, r.PathValue("id"), part.FileName(), part)
		cancel()
		if err != nil {
			writeErr(w, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"hash": hash, "size": size})
		return
	}
}

func (s *server) unpair(w http.ResponseWriter, r *http.Request) {
	_, notified, err := s.b.Unpair(r.Context(), r.PathValue("id"))
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true, "notified": notified})
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

func (s *server) listTunnels(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.b.Tunnels())
}

func (s *server) openTunnel(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Port int `json:"port"`
	}
	if r.ContentLength != 0 {
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<12)).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			writeErr(w, err)
			return
		}
	}
	t, err := s.b.OpenTunnel(r.PathValue("id"), r.PathValue("service"), body.Port)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, t)
}

func (s *server) closeTunnel(w http.ResponseWriter, r *http.Request) {
	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil {
		writeErr(w, errors.New("bad port"))
		return
	}
	if err := s.b.CloseTunnel(port); err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
