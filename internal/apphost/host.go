// Package apphost installs, removes, runs and serves Sol apps (design/API.md §9, §10): it keeps
// the registry of installed manifests the app API routes actions by, supervises each app's
// process with restart backoff, and serves every app's UI at its own origin
// http://<app>.localhost:<port>/ with the /_sol/ bridge to the app API. The same router hands
// the UI root (the Sol app, Config.Root) and http://<name>.<petname>.localhost:<port>/ (a
// contact's app or service, Config.Peer) to the daemon, under the same Host check and CSP.
//
// An installed app is a directory <apps>/<id>/ holding its app.json and files. First-party apps
// are built into the sol binary ("exec": "builtin:<name>"): the host runs them as
// `sol app run <id>` child processes of the same binary, or, with Config.InProcess, as
// goroutines (tests). Each run gets:
//
//	SOL_API        where the app API listens
//	SOL_APP_TOKEN  a fresh token for this run, revoked when it ends
//	SOL_APP_LISTEN where the app must serve: unix://<run>/<id>.sock (127.0.0.1:PORT on Windows)
//	SOL_APP_DATA   <appdata>/<id>, 0700
//	SOL_FILES      the synced folder, only for apps whose manifest declares folders
//
// Invariants:
//   - A manifest is validated before anything of it is used, and an app's exec never points
//     outside its own directory. The app ID "sol" is reserved for Sol's own feeds.
//   - UI requests are served only for a Host header of this port and a known app; requests
//     reach the app with caller headers the host sets (Sol-Self: 1, my person and device), any
//     Sol-* or Authorization header the browser sent is dropped, and /_sol/ is never passed to
//     the app. Every app origin's responses carry a Content-Security-Policy that keeps scripts,
//     fonts and images on the same origin (data: and blob: for media), without unsafe-eval.
//   - The /_sol/ bridge calls the app API with the app's own bridge token, only after the
//     Origin (when sent) is the app's own origin and Sec-Fetch-Site (when sent) is same-origin;
//     a state-changing request needs one of the two, so a page on another origin can never
//     call an app's API (CSRF).
//   - Removing an app stops it and revokes its tokens; its data directory is kept, so removing
//     and reinstalling an app loses nothing.
package apphost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/apv022/sol/internal/appapi"
	"github.com/apv022/sol/internal/fsutil"
	"github.com/apv022/sol/sdk/go/sol"
)

// Restart backoff of a supervised app: doubled after every quick exit, reset after a run that
// lasted StableAfter.
var (
	RestartMin  = 500 * time.Millisecond
	RestartMax  = time.Minute
	StableAfter = 30 * time.Second
	// StopGrace is how long an app gets to exit after an interrupt before it is killed.
	StopGrace = 5 * time.Second
)

// Env is the environment of one app run.
type Env struct {
	API    string // SOL_API
	Token  string // SOL_APP_TOKEN
	Listen string // SOL_APP_LISTEN
	Data   string // SOL_APP_DATA
	Files  string // SOL_FILES ("" when the app declares no folders)
}

// Environ returns e as KEY=value pairs.
func (e Env) Environ() []string {
	out := []string{"SOL_API=" + e.API, "SOL_APP_TOKEN=" + e.Token, "SOL_APP_LISTEN=" + e.Listen, "SOL_APP_DATA=" + e.Data}
	if e.Files != "" {
		out = append(out, "SOL_FILES="+e.Files)
	}
	return out
}

// EnvFromOS reads an Env from the process environment (`sol app run`).
func EnvFromOS() Env {
	return Env{API: os.Getenv("SOL_API"), Token: os.Getenv("SOL_APP_TOKEN"), Listen: os.Getenv("SOL_APP_LISTEN"),
		Data: os.Getenv("SOL_APP_DATA"), Files: os.Getenv("SOL_FILES")}
}

// Listen opens the listener an app serves on (SOL_APP_LISTEN form), removing a stale unix
// socket first.
func Listen(addr string) (net.Listener, error) {
	if p, ok := strings.CutPrefix(addr, "unix://"); ok {
		_ = os.Remove(p)
		return net.Listen("unix", p)
	}
	return net.Listen("tcp", addr)
}

// Builtin is a first-party app compiled into the sol binary.
type Builtin struct {
	// Manifest is the app's embedded app.json.
	Manifest []byte
	// Run runs the app until ctx ends, serving on env.Listen.
	Run func(ctx context.Context, env Env) error
}

// Tokens issues app tokens (appapi.Server implements it).
type Tokens interface {
	Issue(app string) string
	RevokeToken(token string)
	Revoke(app string)
}

// Config configures a Host.
type Config struct {
	AppsDir string // installed apps: <AppsDir>/<id>/app.json
	DataDir string // SOL_APP_DATA roots: <DataDir>/<id>
	RunDir  string // app sockets
	Files   string // the synced folder (SOL_FILES)
	API     string // SOL_API
	Tokens  Tokens
	// Builtins are the first-party apps, by the name after "builtin:".
	Builtins map[string]Builtin
	// InProcess runs builtins as goroutines instead of child processes.
	InProcess bool
	// Exe is the sol binary builtins are run from ("" means os.Executable).
	Exe string
	// Bridge is the app API handler the /_sol/ bridge calls.
	Bridge http.Handler
	// Me returns my person and device IDs, for the caller headers of UI requests.
	Me func() (person, device string)
	// Anchor reports whether this device is an anchor (apps with runs_on "anchor" run only
	// there). nil means never.
	Anchor func() bool
	// Root serves the UI root, http://localhost:<port>/ (the Sol app and the browser's admin
	// API); nil answers 404.
	Root http.Handler
	// Peer serves http://<name>.<peer>.localhost:<port>/ (a contact's app UI or HTTP service);
	// origin is that origin. nil answers not_implemented.
	Peer func(w http.ResponseWriter, r *http.Request, name, peer, origin string)
	// Gate, if set, runs before anything is served on an app or peer origin (origin is
	// "http://<host>:<port>"): it signs the browser in on that origin, or answers the request
	// itself and returns false. Without it every app origin is open to anything that can reach
	// loopback (another OS user, another Android app), so the daemon always sets it; tests of
	// the router alone may leave it nil.
	Gate func(w http.ResponseWriter, r *http.Request, origin string) bool
	// OnChange, if set, is called (never with the host's lock held) whenever an app is
	// installed, removed, restarted, or starts or stops running.
	OnChange func(id string)
	Log      *slog.Logger
}

// app is one installed app.
type app struct {
	m        *sol.Manifest
	dir      string
	addr     string // SOL_APP_LISTEN
	attached bool   // running outside the host (Attach)
	running  bool
	bridge   string // the UI bridge's token
	cancel   context.CancelFunc
	done     chan struct{}
}

// Status describes an installed app (sol apps).
type Status struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Version string `json:"version"`
	Exec    string `json:"exec"`
	UI      bool   `json:"ui"`
	Running bool   `json:"running"`
}

// Host is the app host. Safe for concurrent use.
type Host struct {
	cfg Config
	log *slog.Logger

	mu      sync.Mutex
	apps    map[string]*app
	ctx     context.Context // set by Start: apps run while it lives
	stopped bool
}

// New loads every installed app from cfg.AppsDir. A directory with a bad app.json is skipped
// and logged, never fatal: one broken app must not take Sol down.
func New(cfg Config) (*Host, error) {
	if cfg.Tokens == nil {
		return nil, errors.New("apphost: Tokens is required")
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	for _, d := range []string{cfg.AppsDir, cfg.DataDir, cfg.RunDir} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	h := &Host{cfg: cfg, log: cfg.Log, apps: map[string]*app{}}
	ents, err := os.ReadDir(cfg.AppsDir)
	if err != nil {
		return nil, err
	}
	for _, e := range ents {
		if !e.IsDir() || strings.HasSuffix(e.Name(), ".tmp") {
			continue
		}
		dir := filepath.Join(cfg.AppsDir, e.Name())
		m, err := h.load(dir)
		if err != nil || m.ID != e.Name() {
			h.log.Warn("skipping installed app", "dir", e.Name(), "err", err)
			continue
		}
		h.add(m, dir)
	}
	return h, nil
}

// load reads and validates an app directory's manifest. A builtin's manifest is the one
// compiled in (so upgrading sol upgrades it).
func (h *Host) load(dir string) (*sol.Manifest, error) {
	m, err := sol.LoadManifest(filepath.Join(dir, "app.json"))
	if err != nil {
		return nil, err
	}
	if name, ok := strings.CutPrefix(m.Exec, "builtin:"); ok {
		b, ok := h.cfg.Builtins[name]
		if !ok {
			return nil, fmt.Errorf("apphost: unknown builtin %q", name)
		}
		if m, err = sol.ParseManifest(b.Manifest); err != nil {
			return nil, err
		}
	}
	return m, h.Validate(m, dir)
}

// Validate checks what sol.Manifest.Validate does not: the reserved ID, exec (a known builtin,
// or a file inside dir), the UI path, folders and reads.
func (h *Host) Validate(m *sol.Manifest, dir string) error {
	if m.ID == "sol" {
		return errors.New("app.json: the id sol is reserved")
	}
	if name, ok := strings.CutPrefix(m.Exec, "builtin:"); ok {
		if _, ok := h.cfg.Builtins[name]; !ok {
			return fmt.Errorf("app.json: unknown builtin %q", name)
		}
	} else {
		if filepath.IsAbs(m.Exec) || !filepath.IsLocal(m.Exec) {
			return fmt.Errorf("app.json: exec %q must be a file inside the app directory", m.Exec)
		}
		if dir != "" {
			st, err := os.Stat(filepath.Join(dir, m.Exec))
			if err != nil || st.IsDir() {
				return fmt.Errorf("app.json: exec %q not found in the app directory", m.Exec)
			}
		}
	}
	if m.UI != "" && !strings.HasPrefix(m.UI, "/") {
		return errors.New("app.json: ui must be a path starting with /")
	}
	for _, f := range m.Folders {
		if f.Mode != "read" && f.Mode != "write" {
			return fmt.Errorf("app.json: folder %q: mode must be read or write", f.Path)
		}
		if f.Path != "*" && (f.Path == "" || !filepath.IsLocal(f.Path) || strings.Contains(f.Path, "\\")) {
			return fmt.Errorf("app.json: bad folder %q", f.Path)
		}
	}
	for _, r := range m.Reads {
		if !sol.ValidAppID(r) || r == "sol" {
			return fmt.Errorf("app.json: bad reads entry %q", r)
		}
	}
	return nil
}

// add registers an app (h.mu not held by the caller, or held: it takes no lock itself).
func (h *Host) add(m *sol.Manifest, dir string) *app {
	a := &app{m: m, dir: dir, addr: h.listenAddr(m.ID), bridge: h.cfg.Tokens.Issue(m.ID)}
	h.apps[m.ID] = a
	return a
}

// listenAddr picks where an app serves: a unix socket in the run directory, or a free
// loopback port on Windows.
func (h *Host) listenAddr(id string) string {
	if runtime.GOOS != "windows" {
		return "unix://" + filepath.Join(h.cfg.RunDir, id+".sock")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "127.0.0.1:0"
	}
	defer ln.Close()
	return ln.Addr().String()
}

// App implements appapi.Apps.
func (h *Host) App(id string) (appapi.App, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	a := h.apps[id]
	if a == nil {
		return appapi.App{}, false
	}
	out := appapi.App{Manifest: a.m}
	if a.running || a.attached {
		out.Addr = a.addr
	}
	return out, true
}

// List implements appapi.Apps.
func (h *Host) List() []appapi.App {
	h.mu.Lock()
	ids := make([]string, 0, len(h.apps))
	for id := range h.apps {
		ids = append(ids, id)
	}
	h.mu.Unlock()
	sort.Strings(ids)
	out := []appapi.App{}
	for _, id := range ids {
		if a, ok := h.App(id); ok {
			out = append(out, a)
		}
	}
	return out
}

// Status lists the installed apps.
func (h *Host) Status() []Status {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.statusLocked()
}

// statusLocked is Status with h.mu held.
func (h *Host) statusLocked() []Status {
	out := []Status{}
	for _, a := range h.apps {
		out = append(out, Status{ID: a.m.ID, Name: a.m.Name, Version: a.m.Version, Exec: a.m.Exec, UI: a.m.UI != "",
			Running: a.running || a.attached})
	}
	slices.SortFunc(out, func(x, y Status) int { return strings.Compare(x.ID, y.ID) })
	return out
}

// Start runs every installed app (that may run on this device) until ctx ends or Close.
// Apps installed later start at once.
func (h *Host) Start(ctx context.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.ctx != nil || h.stopped {
		return
	}
	h.ctx = ctx
	for _, a := range h.apps {
		h.startLocked(a)
	}
}

// startLocked starts supervising a. h.mu held.
func (h *Host) startLocked(a *app) {
	if h.ctx == nil || a.attached || a.cancel != nil {
		return
	}
	if slices.Contains(a.m.RunsOn, "anchor") && !slices.Contains(a.m.RunsOn, "any") && (h.cfg.Anchor == nil || !h.cfg.Anchor()) {
		return
	}
	ctx, cancel := context.WithCancel(h.ctx)
	a.cancel, a.done = cancel, make(chan struct{})
	go h.supervise(ctx, a)
}

// stopLocked stops a's supervisor and returns a channel closed once it ended. h.mu held.
func (h *Host) stopLocked(a *app) <-chan struct{} {
	if a.cancel == nil {
		c := make(chan struct{})
		close(c)
		return c
	}
	a.cancel()
	a.cancel = nil
	return a.done
}

// supervise runs a until ctx ends, restarting it with backoff.
func (h *Host) supervise(ctx context.Context, a *app) {
	defer close(a.done)
	backoff := RestartMin
	for ctx.Err() == nil {
		start := time.Now()
		err := h.runOnce(ctx, a)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > StableAfter {
			backoff = RestartMin
		}
		h.log.Warn("app exited; restarting", "app", a.m.ID, "err", err, "in", backoff)
		select {
		case <-time.After(backoff):
		case <-ctx.Done():
			return
		}
		backoff = min(backoff*2, RestartMax)
	}
}

func (h *Host) setRunning(a *app, on bool) {
	h.mu.Lock()
	a.running = on
	h.mu.Unlock()
	h.changed(a.m.ID)
}

// changed reports a change of app id to Config.OnChange.
func (h *Host) changed(id string) {
	if h.cfg.OnChange != nil {
		h.cfg.OnChange(id)
	}
}

// Restart stops an app and starts it again (sol app restart). An attached app is not
// supervised here, so it cannot be restarted.
func (h *Host) Restart(id string) error {
	h.mu.Lock()
	a := h.apps[id]
	if a == nil {
		h.mu.Unlock()
		return sol.Errorf("not_found", "no app "+id)
	}
	if a.attached {
		h.mu.Unlock()
		return sol.Errorf("conflict", id+" is attached for development; restart it yourself")
	}
	wait := h.stopLocked(a)
	h.mu.Unlock()
	<-wait
	h.mu.Lock()
	if h.apps[id] == a {
		h.startLocked(a)
	}
	h.mu.Unlock()
	h.changed(id)
	return nil
}

// Manifest returns an installed app's manifest.
func (h *Host) Manifest(id string) (*sol.Manifest, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	a := h.apps[id]
	if a == nil {
		return nil, false
	}
	return a.m, true
}

// env builds the environment of one run of a, with a fresh token.
func (h *Host) env(a *app, token string) (Env, error) {
	data := filepath.Join(h.cfg.DataDir, a.m.ID)
	if err := os.MkdirAll(data, 0o700); err != nil {
		return Env{}, err
	}
	e := Env{API: h.cfg.API, Token: token, Listen: a.addr, Data: data}
	if len(a.m.Folders) > 0 {
		e.Files = h.cfg.Files
	}
	return e, nil
}

// runOnce runs a once and returns when it exits.
func (h *Host) runOnce(ctx context.Context, a *app) error {
	token := h.cfg.Tokens.Issue(a.m.ID)
	defer h.cfg.Tokens.RevokeToken(token)
	env, err := h.env(a, token)
	if err != nil {
		return err
	}
	if p, ok := strings.CutPrefix(a.addr, "unix://"); ok {
		_ = os.Remove(p)
	}
	h.setRunning(a, true)
	defer h.setRunning(a, false)
	name, builtin := strings.CutPrefix(a.m.Exec, "builtin:")
	if builtin && h.cfg.InProcess {
		return runGuarded(ctx, h.cfg.Builtins[name].Run, env)
	}
	var cmd *exec.Cmd
	if builtin {
		exe := h.cfg.Exe
		if exe == "" {
			if exe, err = os.Executable(); err != nil {
				return err
			}
		}
		cmd = exec.CommandContext(ctx, exe, "app", "run", a.m.ID)
	} else {
		cmd = exec.CommandContext(ctx, filepath.Join(a.dir, a.m.Exec))
	}
	cmd.Dir = a.dir
	cmd.Env = append(cleanEnv(os.Environ()), env.Environ()...)
	cmd.Stdout = &logWriter{log: h.log, app: a.m.ID}
	cmd.Stderr = cmd.Stdout
	cmd.Cancel = func() error {
		if runtime.GOOS == "windows" {
			return cmd.Process.Kill()
		}
		return cmd.Process.Signal(os.Interrupt)
	}
	cmd.WaitDelay = StopGrace
	return cmd.Run()
}

// runGuarded runs an in-process builtin, turning a panic into an error.
func runGuarded(ctx context.Context, run func(context.Context, Env) error, env Env) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("apphost: app panicked: %v", p)
		}
	}()
	return run(ctx, env)
}

// cleanEnv drops any SOL_* variable inherited by the daemon, so an app sees only its own.
func cleanEnv(env []string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, "SOL_") {
			out = append(out, kv)
		}
	}
	return out
}

// logWriter passes an app's output to the daemon log, one record per line.
type logWriter struct {
	log *slog.Logger
	app string
	mu  sync.Mutex
	buf []byte
}

// Write implements io.Writer.
func (w *logWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, p...)
	for {
		i := slices.Index(w.buf, '\n')
		if i < 0 {
			break
		}
		w.log.Info(string(w.buf[:i]), "app", w.app)
		w.buf = w.buf[i+1:]
	}
	if len(w.buf) > 64<<10 {
		w.buf = w.buf[:0]
	}
	return len(p), nil
}

// Install installs (or replaces) the app in dir: it validates app.json, copies the directory
// to <apps>/<id>/ and starts the app if the host is running. A builtin's directory needs only
// its app.json.
func (h *Host) Install(dir string) (*sol.Manifest, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "app.json"))
	if err != nil {
		return nil, err
	}
	m, err := sol.ParseManifest(raw)
	if err != nil {
		return nil, err
	}
	if err := h.Validate(m, dir); err != nil {
		return nil, err
	}
	dst := filepath.Join(h.cfg.AppsDir, m.ID)
	tmp := dst + ".tmp"
	_ = os.RemoveAll(tmp)
	if strings.HasPrefix(m.Exec, "builtin:") {
		if err := os.MkdirAll(tmp, 0o700); err == nil {
			err = fsutil.WriteFileAtomic(filepath.Join(tmp, "app.json"), raw, 0o600)
		}
	} else {
		err = copyTree(dir, tmp)
	}
	if err != nil {
		_ = os.RemoveAll(tmp)
		return nil, err
	}
	h.mu.Lock()
	if old := h.apps[m.ID]; old != nil {
		if old.attached {
			h.mu.Unlock()
			_ = os.RemoveAll(tmp)
			return nil, errors.New("apphost: " + m.ID + " is attached for development; stop it first")
		}
		wait := h.stopLocked(old)
		h.mu.Unlock()
		<-wait
		h.mu.Lock()
		delete(h.apps, m.ID)
		h.cfg.Tokens.Revoke(m.ID)
	}
	if err := os.RemoveAll(dst); err == nil {
		err = os.Rename(tmp, dst)
	}
	if err != nil {
		h.mu.Unlock()
		return nil, err
	}
	if m, err = h.load(dst); err != nil {
		h.mu.Unlock()
		return nil, err
	}
	h.startLocked(h.add(m, dst))
	h.mu.Unlock()
	h.changed(m.ID)
	return m, nil
}

// Remove stops an app, revokes its tokens and deletes its directory. Its data stays.
func (h *Host) Remove(id string) error {
	h.mu.Lock()
	a := h.apps[id]
	if a == nil {
		h.mu.Unlock()
		return sol.Errorf("not_found", "no app "+id)
	}
	delete(h.apps, id)
	wait := h.stopLocked(a)
	h.mu.Unlock()
	<-wait
	h.cfg.Tokens.Revoke(id)
	defer h.changed(id)
	if a.attached {
		return nil
	}
	return os.RemoveAll(a.dir)
}

// Attach registers an app that is already running at addr (SOL_APP_LISTEN form), for
// development and tests: the host routes its actions and UI but does not supervise it. It
// returns the token the app uses for the API.
func (h *Host) Attach(m *sol.Manifest, addr string) (string, error) {
	if err := m.Validate(); err != nil {
		return "", err
	}
	if m.ID == "sol" {
		return "", errors.New("apphost: the id sol is reserved")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if old := h.apps[m.ID]; old != nil {
		h.stopLocked(old)
	}
	a := &app{m: m, addr: addr, attached: true, bridge: h.cfg.Tokens.Issue(m.ID)}
	h.apps[m.ID] = a
	if h.cfg.OnChange != nil {
		go h.cfg.OnChange(m.ID) // h.mu is held here
	}
	return h.cfg.Tokens.Issue(m.ID), nil
}

// Close stops every app and waits for them.
func (h *Host) Close() {
	h.mu.Lock()
	h.stopped = true
	var waits []<-chan struct{}
	for _, a := range h.apps {
		waits = append(waits, h.stopLocked(a))
	}
	h.mu.Unlock()
	for _, w := range waits {
		<-w
	}
}

// copyTree copies the regular files and directories of src into dst (created 0700), keeping
// the executable bits. Symlinks are refused: an app directory must be self-contained.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		out := filepath.Join(dst, rel)
		switch {
		case d.IsDir():
			return os.MkdirAll(out, 0o700)
		case d.Type()&fs.ModeSymlink != 0:
			return fmt.Errorf("apphost: %s is a symlink; app directories must be self-contained", rel)
		case !d.Type().IsRegular():
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		in, err := os.Open(p)
		if err != nil {
			return err
		}
		defer in.Close()
		f, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm()&0o700|0o600)
		if err != nil {
			return err
		}
		if _, err := io.Copy(f, in); err != nil {
			f.Close()
			return err
		}
		return f.Close()
	})
}
