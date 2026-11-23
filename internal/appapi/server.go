// Package appapi is the daemon's app API server (design/API.md §1–§8): the HTTP/1.1 + JSON
// contract every app talks to over a unix socket (TCP loopback on Windows), and the session
// endpoints that carry action calls, dialed streams and inbox notices between devices.
//
// It translates the API onto the daemon's parts: feeds and sharing onto internal/feeds, blobs
// onto internal/blob and internal/blobnet, people onto the contacts view, the identity cache and
// the session manager, actions onto the installed apps' sockets (Apps), and the event stream onto
// an in-memory event log fed by the store's bus.
//
// Invariants:
//   - Every request carries a bearer token that names one app (Issue) or the admin (the CLI).
//     Tokens are compared by SHA-256 lookup, never logged, and an app may only use the API areas
//     its manifest's scopes name.
//   - An app writes only its own app's feeds and reads its own plus those of the apps in its
//     manifest's reads, within what is shared with me (mayRead). Sol's internal feeds ("sol") are
//     never visible to apps.
//   - Callers are decided by the daemon: requests to apps carry Sol-Person, Sol-Device and
//     Sol-Self from the verified session peer (or me), never from anything an app or peer sent.
//   - Events are pushed, never polled for: the event log is filled from the store's bus, each
//     event has an id "<epoch>.<n>", and Last-Event-ID resumes without gaps within the log's
//     window (MaxEvents). An id from an earlier daemon run replays this run's whole log.
//   - Nothing here logs tokens, request bodies or entry bodies.
package appapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"

	"github.com/apv022/sol/internal/blob"
	"github.com/apv022/sol/internal/blobnet"
	"github.com/apv022/sol/internal/feeds"
	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/session"
	"github.com/apv022/sol/sdk/go/sol"
)

// Node is what the API server needs of the daemon: this device's identity and stores.
type Node struct {
	Keyring  *identity.Keyring
	Cache    *identity.Cache
	Store    *feeds.Store
	Contacts *feeds.Contacts
	Shares   *feeds.Shares
	Pins     *feeds.Pins
	Blobs    *blob.Store
	Blobnet  *blobnet.Service
	Sessions *session.Manager
}

// App is an installed app as the API server sees it.
type App struct {
	Manifest *sol.Manifest
	// Addr is where the app serves HTTP, in SOL_APP_LISTEN form ("unix:///path" or
	// "127.0.0.1:PORT"); "" while it is not running.
	Addr string
}

// Apps is the registry of installed apps (internal/apphost implements it).
type Apps interface {
	App(id string) (App, bool)
	List() []App
}

// Config configures a Server.
type Config struct {
	Node Node
	Apps Apps
	// DataDir holds the inbox (inbox.json, 0600, written atomically).
	DataDir string
	// Admin serves /admin/ to callers holding the admin token (the CLI); nil refuses.
	Admin http.Handler
	// Agent serves /agent/ (agent keys, internal/daemon), which authenticates its own callers by
	// their certificate and signature; nil refuses.
	Agent http.Handler
	// Tap, if set, is told of every person and inbox event as it is logged (kind
	// sol.EventPerson with the contact's ID, sol.EventInbox with the item's ID), so the
	// daemon's admin event stream can follow them. It must not block.
	Tap func(kind, id string)
	// Petname, if set, returns the local petname I gave a contact ("" for the default).
	Petname func(id string) string
	Log     *slog.Logger
}

// principal is who a token belongs to.
type principal struct {
	app   string
	admin bool
}

// Server is the app API. Safe for concurrent use.
type Server struct {
	cfg Config
	n   Node
	log *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup

	mu     sync.Mutex
	tokens map[[32]byte]principal

	events *eventLog
	inbox  *inbox

	clients sync.Map // app addr -> *http.Client
}

// New returns a Server and starts feeding its event log from the store's bus.
func New(cfg Config) (*Server, error) {
	if cfg.Node.Store == nil || cfg.Node.Keyring == nil || cfg.Apps == nil {
		return nil, errors.New("appapi: Node.Store, Node.Keyring and Apps are required")
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	in, err := openInbox(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{cfg: cfg, n: cfg.Node, log: cfg.Log, ctx: ctx, cancel: cancel,
		tokens: map[[32]byte]principal{}, events: newEventLog(), inbox: in}
	if cfg.Tap != nil {
		s.events.tap = func(r rec) {
			switch {
			case r.kind == sol.EventPerson && r.person != nil:
				cfg.Tap(r.kind, r.person.ID)
			case r.kind == sol.EventInbox && r.inbox != nil:
				cfg.Tap(r.kind, r.inbox.ID)
			}
		}
	}
	s.wg.Add(1)
	go s.pump()
	return s, nil
}

// Close stops the event pump and ends every open event stream.
func (s *Server) Close() {
	s.cancel()
	s.events.close()
	s.wg.Wait()
}

func newToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Issue returns a new token for app. An app may hold several (its process, its UI bridge).
func (s *Server) Issue(app string) string {
	t := newToken()
	s.mu.Lock()
	s.tokens[sha256.Sum256([]byte(t))] = principal{app: app}
	s.mu.Unlock()
	return t
}

// Grant accepts the token whose SHA-256 is sum as one of app's, without ever knowing the
// token: an external app's long-lived token is stored only hashed, and granted again by its
// hash at every start (apphost.RegisterExternal). Revoke drops it like any other.
func (s *Server) Grant(app string, sum [32]byte) {
	s.mu.Lock()
	s.tokens[sum] = principal{app: app}
	s.mu.Unlock()
}

// IssueAdmin returns a new admin token (for the CLI).
func (s *Server) IssueAdmin() string {
	t := newToken()
	s.mu.Lock()
	s.tokens[sha256.Sum256([]byte(t))] = principal{admin: true}
	s.mu.Unlock()
	return t
}

// RevokeToken drops one token.
func (s *Server) RevokeToken(token string) {
	s.mu.Lock()
	delete(s.tokens, sha256.Sum256([]byte(token)))
	s.mu.Unlock()
}

// Revoke drops every token of app.
func (s *Server) Revoke(app string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, p := range s.tokens {
		if p.app == app {
			delete(s.tokens, k)
		}
	}
}

func (s *Server) principal(r *http.Request) (principal, bool) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || tok == "" {
		return principal{}, false
	}
	s.mu.Lock()
	p, ok := s.tokens[sha256.Sum256([]byte(tok))]
	s.mu.Unlock()
	return p, ok
}

// me returns my person and device IDs ("" person before the device joined one).
func (s *Server) me() (person, device string) {
	if p := s.n.Keyring.Person(); p != nil {
		person = identity.ID(p)
	}
	return person, identity.ID(s.n.Keyring.DevicePublic())
}

// req is one authenticated API request: the calling app and its manifest.
type req struct {
	app string
	m   *sol.Manifest
}

func (q req) has(scope string) bool { return slices.Contains(q.m.Scopes, scope) }

type handler func(q req, w http.ResponseWriter, r *http.Request) (any, error)

// Handler returns the app API: /v1/... for app tokens and /admin/ for the admin token.
func (s *Server) Handler() http.Handler {
	mx := http.NewServeMux()
	r := func(pattern, scope string, h handler) { mx.HandleFunc(pattern, s.wrap(scope, h)) }
	r("GET /v1/whoami", "", s.whoami)
	r("GET /v1/people", "people", s.people)
	r("GET /v1/devices", "people", s.devices)
	r("POST /v1/feeds/{rest...}", "feeds", s.append)
	r("GET /v1/feeds/{rest...}", "feeds", s.read)
	r("GET /v1/feeds", "feeds", s.feedList)
	r("PUT /v1/blobs", "blobs", s.putBlob)
	r("GET /v1/blobs/{hash}", "blobs", s.getBlob)
	r("HEAD /v1/blobs/{hash}", "blobs", s.getBlob)
	r("POST /v1/blobs/pin", "blobs", s.pin)
	r("POST /v1/blobs/unpin", "blobs", s.unpin)
	r("POST /v1/shares", "feeds", s.share)
	r("DELETE /v1/shares", "feeds", s.unshare)
	r("GET /v1/shares", "feeds", s.shareList)
	r("POST /v1/actions/call", "actions", s.callAction)
	r("GET /v1/actions", "actions", s.actionList)
	r("GET /v1/activity", "actions", s.activity)
	r("POST /v1/inbox", "inbox", s.postInbox)
	r("GET /v1/inbox", "inbox", s.listInbox)
	r("GET /v1/events", "", s.eventStream)
	r("POST /v1/dial", "dial", s.dial)
	mx.HandleFunc("/admin/", func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.principal(r)
		if !ok || !p.admin || s.cfg.Admin == nil {
			sol.WriteError(w, sol.Errorf("unauthorized", "admin token required"))
			return
		}
		s.cfg.Admin.ServeHTTP(w, r)
	})
	mx.HandleFunc("/agent/", func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.Agent == nil {
			sol.WriteError(w, sol.Errorf("unauthorized", "agents are not accepted here"))
			return
		}
		s.cfg.Agent.ServeHTTP(w, r)
	})
	mx.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		sol.WriteError(w, sol.Errorf("not_found", "no such path"))
	})
	return mx
}

// wrap authenticates, checks the scope and writes the handler's result as JSON.
func (s *Server) wrap(scope string, h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p, ok := s.principal(r)
		if !ok || p.admin {
			sol.WriteError(w, sol.Errorf("unauthorized", "bad token"))
			return
		}
		a, ok := s.cfg.Apps.App(p.app)
		if !ok {
			sol.WriteError(w, sol.Errorf("unauthorized", "app is not installed"))
			return
		}
		q := req{app: p.app, m: a.Manifest}
		if scope != "" && !q.has(scope) {
			sol.WriteError(w, sol.Errorf("forbidden", "the app's manifest lacks the "+scope+" scope"))
			return
		}
		if me, _ := s.me(); me == "" && r.URL.Path != "/v1/whoami" {
			sol.WriteError(w, sol.Errorf("unavailable", "this device has not joined a person yet"))
			return
		}
		out, err := h(q, w, r)
		switch {
		case err == done:
			return
		case err != nil:
			sol.WriteError(w, err)
			return
		case out == nil:
			w.WriteHeader(http.StatusNoContent)
			return
		}
		sol.WriteJSON(w, http.StatusOK, out)
	}
}

// done is returned by handlers that wrote the response themselves.
var done error = errDone{}

type errDone struct{}

// Error implements error.
func (errDone) Error() string { return "response written" }

func decode(r *http.Request, v any) error {
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(v); err != nil {
		return sol.Errorf("bad_request", "bad JSON: "+err.Error())
	}
	return nil
}

// whoami serves GET /v1/whoami.
func (s *Server) whoami(q req, w http.ResponseWriter, r *http.Request) (any, error) {
	me, dev := s.me()
	out := sol.WhoAmI{Device: sol.Device{ID: dev, Online: true}, App: sol.AppInfo{ID: q.app, Scopes: q.m.Scopes}}
	out.Person.ID = me
	if b, err := s.n.Keyring.Bundle(); err == nil {
		out.Person.Name = b.Person.Name
		if d, ok := b.Devices.Find(s.n.Keyring.DevicePublic()); ok {
			out.Device.Name, out.Device.Anchor = d.Name, d.Anchor()
		}
	}
	if out.App.Scopes == nil {
		out.App.Scopes = []string{}
	}
	return out, nil
}
