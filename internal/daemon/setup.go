package daemon

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/apv022/sol/internal/apphost"
	"github.com/apv022/sol/internal/fsutil"
	"github.com/apv022/sol/sdk/go/sol"
	soljs "github.com/apv022/sol/sdk/js"
	home "github.com/apv022/sol/ui/home"
	kit "github.com/apv022/sol/ui/kit"
)

// Setup in the browser (design/ADMIN.md "Setup"). A data directory with a config but no
// identity runs in setup mode: instead of the daemon, a small server on the UI address serves
// ui/home/setup.html, where the person picks Start new, Add to my Sol (a join link) or Restore
// from backup words. It only answers requests carrying the setup token, a random secret that
// reaches the browser in the URL `sol` opens (and that <dir>/setup.url, 0600, holds for
// `sol open`), with the UI origin's own Origin. Once a choice succeeded the server stops and
// the caller starts the daemon on the same address.
//
// "Remove Sol from this device" (POST /admin/v1/reset) is the way back: the daemon stops,
// ArchiveIdentity moves the identity and app state aside into <dir>/removed-<time>/ (nothing
// is deleted, and the synced folder is never touched), and the same process enters setup mode
// with a token it handed the browser in the reset reply.

// SetupHeader carries the setup token on setup API requests.
const SetupHeader = "Sol-Setup"

// SetupOptions configure RunSetup.
type SetupOptions struct {
	Dir      string
	Builtins map[string]apphost.Builtin
	// Token is the setup secret; empty makes a new one.
	Token string
	// Name and Device prefill the form (the OS account's name, the host name).
	Name, Device string
	Log          *slog.Logger
}

// SetupResult is what the person chose.
type SetupResult struct {
	// JoinLink is set for "Add to my Sol": the caller pairs with it once the daemon runs.
	JoinLink string
}

// NewSetupToken returns a fresh setup secret.
func NewSetupToken() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// SetupURLFile is where a running setup server writes its URL (with the token).
func SetupURLFile(dir string) string { return filepath.Join(dir, "setup.url") }

// PrepareSetup writes dir's config.json (defaults filled in) and the builtin apps, without an
// identity, so `sol run` there starts in setup mode.
func PrepareSetup(dir string, cfg Config, builtins map[string]apphost.Builtin) error {
	if Initialised(dir) {
		return ErrExists
	}
	if c, err := LoadConfig(dir); err == nil {
		cfg = c // a removed identity's config: keep its ports, folder and transports
	}
	return prepareDir(dir, &cfg, builtins)
}

// RunSetup serves the setup page on the configured UI address until the person set Sol up
// (Init, Restore, or a joining device) or ctx ends.
func RunSetup(ctx context.Context, o SetupOptions) (SetupResult, error) {
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Token == "" {
		o.Token = NewSetupToken()
	}
	cfg, err := LoadConfig(o.Dir)
	if err != nil {
		cfg = Config{}
		if err := prepareDir(o.Dir, &cfg, o.Builtins); err != nil {
			return SetupResult{}, err
		}
	}
	addr := cfg.UI
	if addr == "" {
		addr = "127.0.0.1:7777"
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return SetupResult{}, fmt.Errorf("daemon: UI address %s: %w", addr, err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	if strings.HasSuffix(addr, ":0") { // keep the port a free-port config was given
		cfg.UI = fmt.Sprintf("127.0.0.1:%d", port)
		_ = writeConfig(o.Dir, cfg)
	}
	s := &setupServer{o: o, cfg: cfg, port: port, done: make(chan SetupResult, 1), stop: make(chan struct{})}
	srv := &http.Server{Handler: s.handler(), ReadHeaderTimeout: 10 * time.Second}
	go func() { _ = srv.Serve(ln) }()
	url := fmt.Sprintf("http://localhost:%d/?setup=%s", port, o.Token)
	if err := fsutil.WriteFileAtomic(SetupURLFile(o.Dir), []byte(url+"\n"), 0o600); err != nil {
		_ = srv.Close()
		return SetupResult{}, err
	}
	o.Log.Info("waiting for setup in the browser", "ui", fmt.Sprintf("http://localhost:%d/", port))
	defer os.Remove(SetupURLFile(o.Dir))
	select {
	case <-ctx.Done():
		_ = srv.Close()
		return SetupResult{}, ctx.Err()
	case <-s.stop: // sol stop
		_ = srv.Close()
		return SetupResult{}, ErrSetupStopped
	case res := <-s.done:
		// Let the reply that finished setup reach the browser, then free the port.
		sctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
		return res, nil
	}
}

type setupServer struct {
	o    SetupOptions
	cfg  Config
	port int
	mu   sync.Mutex // one setup at a time
	done chan SetupResult
	stop chan struct{}
	once sync.Once
	used bool
}

// ErrSetupStopped is RunSetup's error when `sol stop` ended a setup that was waiting.
var ErrSetupStopped = errors.New("daemon: setup stopped")

func (s *setupServer) handler() http.Handler {
	mx := http.NewServeMux()
	mx.HandleFunc("GET /setup/v1/state", s.api(func(r *http.Request) (any, *SetupResult, error) {
		return map[string]string{"name": s.o.Name, "device": s.o.Device, "files": s.cfg.Files}, nil, nil
	}))
	mx.HandleFunc("POST /setup/v1/stop", s.api(func(r *http.Request) (any, *SetupResult, error) {
		s.once.Do(func() { close(s.stop) })
		return map[string]bool{"stopping": true}, nil, nil
	}))
	mx.HandleFunc("POST /setup/v1/new", s.api(func(r *http.Request) (any, *SetupResult, error) {
		var in struct{ Name, Device string }
		if err := readSetup(r, &in); err != nil {
			return nil, nil, err
		}
		if strings.TrimSpace(in.Name) == "" {
			return nil, nil, sol.Errorf("bad_request", "enter your name")
		}
		err := Init(s.o.Dir, InitOptions{Name: strings.TrimSpace(in.Name), Device: deviceOr(in.Device, s.o.Device),
			Config: s.cfg, Builtins: s.o.Builtins})
		return nil, &SetupResult{}, err
	}))
	mx.HandleFunc("POST /setup/v1/restore", s.api(func(r *http.Request) (any, *SetupResult, error) {
		var in struct{ Words, Name, Device, From string }
		if err := readSetup(r, &in); err != nil {
			return nil, nil, err
		}
		ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
		defer cancel()
		err := Restore(ctx, s.o.Dir, RestoreOptions{Words: strings.ToLower(in.Words), Name: strings.TrimSpace(in.Name),
			Device: deviceOr(in.Device, s.o.Device), From: strings.TrimSpace(in.From), Config: s.cfg, Builtins: s.o.Builtins, Log: s.o.Log})
		if err != nil {
			// A half-made directory would make the next try fail with "exists".
			_ = os.RemoveAll(keysDir(s.o.Dir))
			return nil, nil, sol.Errorf("bad_request", "restore failed: "+err.Error())
		}
		return nil, &SetupResult{}, nil
	}))
	mx.HandleFunc("POST /setup/v1/join", s.api(func(r *http.Request) (any, *SetupResult, error) {
		var in struct{ Link, Device string }
		if err := readSetup(r, &in); err != nil {
			return nil, nil, err
		}
		link := strings.TrimSpace(in.Link)
		if !strings.HasPrefix(link, "sol://join") {
			return nil, nil, sol.Errorf("bad_request", "paste a sol://join link (Devices, Add my device, on your other device)")
		}
		dev := deviceOr(in.Device, s.o.Device)
		cfg := s.cfg
		cfg.DeviceName = dev
		err := Init(s.o.Dir, InitOptions{Join: true, Device: dev, Config: cfg, Builtins: s.o.Builtins})
		return nil, &SetupResult{JoinLink: link}, err
	}))
	mx.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		apphost.SecurityHeaders(w.Header())
		switch p := r.URL.Path; {
		case p == "/sol.js":
			http.ServeFileFS(w, r, soljs.FS, "sol.js")
		case strings.HasPrefix(p, "/kit/"):
			http.StripPrefix("/kit/", http.FileServerFS(kit.FS)).ServeHTTP(w, r)
		case p == "/_sol/theme.css":
			w.Header().Set("Content-Type", "text/css; charset=utf-8")
			http.ServeFileFS(w, r, kit.FS, "themes/sol.css")
		case p == "/" || p == "/index.html":
			w.Header().Set("Cache-Control", "no-store")
			http.ServeFileFS(w, r, home.FS, "setup.html")
		case strings.HasPrefix(p, "/admin/"), strings.HasPrefix(p, "/_sol/"):
			// The page tells setup mode from the running daemon by this answer.
			sol.WriteError(w, sol.Errorf("not_found", "Sol is not set up yet"))
		default:
			http.FileServerFS(home.FS).ServeHTTP(w, r)
		}
	})
	return mx
}

// api wraps a setup endpoint: token and Origin checks, one setup at a time, JSON out, and the
// end of setup mode when f returns a result.
func (s *setupServer) api(f func(r *http.Request) (any, *SetupResult, error)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		apphost.SecurityHeaders(w.Header())
		w.Header().Set("Cache-Control", "no-store")
		if subtle.ConstantTimeCompare([]byte(r.Header.Get(SetupHeader)), []byte(s.o.Token)) != 1 {
			sol.WriteError(w, sol.Errorf("forbidden", "open setup from `sol` or `sol open` on this computer"))
			return
		}
		if r.Method != http.MethodGet && !s.sameOrigin(r.Header.Get("Origin")) {
			sol.WriteError(w, sol.Errorf("forbidden", "wrong origin"))
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.used {
			sol.WriteError(w, sol.Errorf("conflict", "Sol is already set up; reload the page"))
			return
		}
		out, res, err := f(r)
		if err != nil {
			var se *sol.Error
			if !errors.As(err, &se) {
				err = sol.Errorf("bad_request", err.Error())
			}
			sol.WriteError(w, err)
			return
		}
		if res != nil {
			s.used = true
			setSignedOutCookie(w, false) // a fresh Sol signs this browser in
			out = map[string]bool{"ok": true}
			sol.WriteJSON(w, http.StatusOK, out)
			s.done <- *res
			return
		}
		sol.WriteJSON(w, http.StatusOK, out)
	}
}

func (s *setupServer) sameOrigin(o string) bool {
	// No Origin: not a browser (the CLI's stop). Browsers always send it on a POST; the token
	// is required either way.
	return o == "" || o == fmt.Sprintf("http://localhost:%d", s.port) || o == fmt.Sprintf("http://127.0.0.1:%d", s.port)
}

func readSetup(r *http.Request, v any) error {
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(v); err != nil {
		return sol.Errorf("bad_request", "bad JSON")
	}
	return nil
}

func deviceOr(d, def string) string {
	if d = strings.TrimSpace(d); d != "" {
		return d
	}
	if def != "" {
		return def
	}
	return "computer"
}

// keepOnReset are the entries of a data directory that survive "Remove Sol from this device":
// the config (ports, synced folder, transports), the logs, and earlier archives.
func keepOnReset(name string) bool {
	return name == "config.json" || strings.HasPrefix(name, "sol.log") || strings.HasPrefix(name, "removed-")
}

// ArchiveIdentity moves everything that makes dir a Sol (keys, feeds, contacts, app state,
// sessions) into dir/removed-<UTC time>/ and returns that path. The daemon must be stopped.
// Nothing is deleted: moving the entries back restores the old identity.
func ArchiveIdentity(dir string) (string, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	dst := filepath.Join(dir, "removed-"+time.Now().UTC().Format("20060102-150405"))
	if err := os.MkdirAll(dst, 0o700); err != nil {
		return "", err
	}
	for _, e := range ents {
		if keepOnReset(e.Name()) {
			continue
		}
		if err := os.Rename(filepath.Join(dir, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			return dst, err
		}
	}
	return dst, nil
}
