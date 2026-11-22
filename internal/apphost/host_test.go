package apphost

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apv022/sol/sdk/go/sol"
)

type fakeTokens struct {
	mu sync.Mutex
	n  int
	m  map[string]string
}

func (f *fakeTokens) Issue(app string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	t := app + "-" + strings.Repeat("x", f.n)
	f.m[t] = app
	return t
}
func (f *fakeTokens) RevokeToken(t string) { f.mu.Lock(); delete(f.m, t); f.mu.Unlock() }
func (f *fakeTokens) Revoke(app string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for t, a := range f.m {
		if a == app {
			delete(f.m, t)
		}
	}
}
func (f *fakeTokens) count(app string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, a := range f.m {
		if a == app {
			n++
		}
	}
	return n
}

const echoManifest = `{"id":"echo","name":"Echo","version":"1","exec":"builtin:echo","ui":"/","scopes":["actions"],"folders":[{"path":"Music","mode":"read"}]}`

// newHost builds a host with one in-process builtin, "echo", that serves its env and the
// caller headers it got.
func newHost(t *testing.T, bridge http.Handler) (*Host, *fakeTokens, chan Env) {
	t.Helper()
	dir := t.TempDir()
	runDir, err := os.MkdirTemp("", "ah-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(runDir) })
	envs := make(chan Env, 10)
	tok := &fakeTokens{m: map[string]string{}}
	h, err := New(Config{AppsDir: filepath.Join(dir, "apps"), DataDir: filepath.Join(dir, "data"), RunDir: runDir,
		Files: filepath.Join(dir, "files"), API: "unix:///nowhere", Tokens: tok, InProcess: true, Bridge: bridge,
		Me: func() (string, string) { return "me", "dev" }, Log: slog.New(slog.DiscardHandler),
		Builtins: map[string]Builtin{"echo": {Manifest: []byte(echoManifest), Run: func(ctx context.Context, env Env) error {
			envs <- env
			ln, err := Listen(env.Listen)
			if err != nil {
				return err
			}
			return sol.ServeListener(ctx, ln, nil, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				io.WriteString(w, r.Header.Get("Sol-Person")+"|"+r.Header.Get("Sol-Self")+"|"+r.Header.Get("Sol-Agent")+"|"+r.Header.Get("Authorization"))
			}))
		}}}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h, tok, envs
}

func TestInstallRunAndUI(t *testing.T) {
	bridged := make(chan *http.Request, 1)
	h, tok, envs := newHost(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bridged <- r
		w.WriteHeader(204)
	}))
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "app.json"), []byte(echoManifest), 0o600)
	if _, err := h.Install(src); err != nil {
		t.Fatal(err)
	}
	h.Start(context.Background())
	var env Env
	select {
	case env = <-envs:
	case <-time.After(5 * time.Second):
		t.Fatal("app did not start")
	}
	if env.Token == "" || env.Files == "" || (runtime.GOOS != "windows") != strings.HasPrefix(env.Listen, "unix://") || env.API != "unix:///nowhere" {
		t.Fatalf("%+v", env)
	}
	if st, err := os.Stat(env.Data); err != nil || runtime.GOOS != "windows" && st.Mode().Perm() != 0o700 {
		t.Fatalf("data dir: %v %v", st, err)
	}
	srv := h.UI(7777)
	get := func(host, path string, hdr map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", path, nil)
		r.Host = host
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		srv.ServeHTTP(w, r)
		return w
	}
	var w *httptest.ResponseRecorder
	deadline := time.Now().Add(5 * time.Second)
	for {
		w = get("echo.localhost:7777", "/", map[string]string{"Sol-Person": "forged", "Sol-Agent": "forged", "Authorization": "Bearer x"})
		if w.Code == 200 || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if w.Body.String() != "me|1||" || w.Header().Get("Content-Security-Policy") != CSP {
		t.Fatalf("%d %q %v", w.Code, w.Body.String(), w.Header())
	}
	if w := get("echo.localhost:7778", "/", nil); w.Code != http.StatusMisdirectedRequest {
		t.Fatal("wrong port accepted")
	}
	if w := get("nope.localhost:7777", "/", nil); w.Code != 404 {
		t.Fatal(w.Code)
	}
	if w := get("echo.bob.localhost:7777", "/", nil); w.Code != 501 {
		t.Fatal(w.Code)
	}
	if w := get("localhost:7777", "/", nil); w.Code != 404 {
		t.Fatal("no Root configured, yet the UI root answered", w.Code)
	}
	if w := get("echo.localhost:7777", "/_sol/theme.css", nil); w.Code != 200 || !strings.Contains(w.Body.String(), "@theme") {
		t.Fatal(w.Code)
	}
	os.MkdirAll(filepath.Join(h.cfg.Files, ".sol", "themes"), 0o700)
	os.WriteFile(filepath.Join(h.cfg.Files, ".sol", "theme"), []byte("mine\n"), 0o600)
	os.WriteFile(filepath.Join(h.cfg.Files, ".sol", "themes", "mine.css"), []byte(":root{--tint:red}"), 0o600)
	if w := get("echo.localhost:7777", "/_sol/theme.css", nil); w.Body.String() != ":root{--tint:red}" {
		t.Fatal(w.Body.String())
	}
	// /_sol/nav names Home and the apps that have a UI, marking the one asked.
	if w := get("echo.localhost:7777", "/_sol/nav", nil); w.Code != 200 || !strings.Contains(w.Body.String(), `"home":"http://localhost:7777/"`) ||
		!strings.Contains(w.Body.String(), `"id":"echo"`) || !strings.Contains(w.Body.String(), `"current":true`) ||
		!strings.Contains(w.Body.String(), `"url":"http://echo.localhost:7777/"`) {
		t.Fatal(w.Code, w.Body.String())
	}
	// The bridge adds the app's bridge token.
	if w := get("echo.localhost:7777", "/_sol/v1/whoami", nil); w.Code != 204 {
		t.Fatal(w.Code)
	}
	if r := <-bridged; r.URL.Path != "/v1/whoami" || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer echo-") {
		t.Fatalf("%s %s", r.URL.Path, r.Header.Get("Authorization"))
	}
	if n := tok.count("echo"); n != 2 { // the run's token and the bridge's
		t.Fatalf("%d tokens", n)
	}
	if err := h.Remove("echo"); err != nil {
		t.Fatal(err)
	}
	if n := tok.count("echo"); n != 0 {
		t.Fatalf("%d tokens after remove", n)
	}
	if _, err := os.Stat(RemovedMarker(h.cfg.AppsDir, "echo")); err != nil {
		t.Fatalf("removing a builtin left no marker: %v", err) // the daemon's start would add it back
	}
}

func TestSameOrigin(t *testing.T) {
	const o = "http://files.localhost:1"
	for _, c := range []struct {
		method, origin, site string
		ok                   bool
	}{
		{"GET", "", "", true},
		{"GET", o, "same-origin", true},
		{"GET", "", "cross-site", false},
		{"GET", "http://notes.localhost:1", "", false},
		{"POST", o, "", true},
		{"POST", "", "same-origin", true},
		{"POST", "", "", false},
		{"POST", "null", "", false},
	} {
		r := httptest.NewRequest(c.method, "/_sol/v1/x", nil)
		if c.origin != "" {
			r.Header.Set("Origin", c.origin)
		}
		if c.site != "" {
			r.Header.Set("Sec-Fetch-Site", c.site)
		}
		if got := sameOrigin(r, o); got != c.ok {
			t.Errorf("%+v: got %v", c, got)
		}
	}
}

func TestSupervisorRestarts(t *testing.T) {
	RestartMin = 10 * time.Millisecond
	defer func() { RestartMin = 500 * time.Millisecond }()
	runs := make(chan struct{}, 10)
	dir := t.TempDir()
	h, err := New(Config{AppsDir: filepath.Join(dir, "a"), DataDir: filepath.Join(dir, "d"), RunDir: filepath.Join(dir, "r"),
		Tokens: &fakeTokens{m: map[string]string{}}, InProcess: true, Log: slog.New(slog.DiscardHandler),
		Builtins: map[string]Builtin{"echo": {Manifest: []byte(echoManifest), Run: func(ctx context.Context, env Env) error {
			runs <- struct{}{}
			panic("boom")
		}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer h.Close()
	src := t.TempDir()
	os.WriteFile(filepath.Join(src, "app.json"), []byte(echoManifest), 0o600)
	h.Start(context.Background())
	if _, err := h.Install(src); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		select {
		case <-runs:
		case <-time.After(5 * time.Second):
			t.Fatal("not restarted")
		}
	}
}
