package apphost

import (
	"context"
	"html/template"
	"io/fs"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	soljs "github.com/apv022/sol/sdk/js"
	kit "github.com/apv022/sol/ui/kit"

	"github.com/apv022/sol/sdk/go/sol"
)

// CSP is the Content-Security-Policy of every app origin (design/UI.md "Themes", Safety).
// Inline styles stay allowed (UIs set them from JS); scripts never run inline or from eval.
const CSP = "default-src 'self'; img-src 'self' data: blob:; media-src 'self' data: blob:; font-src 'self' data:; " +
	"style-src 'self' 'unsafe-inline'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'"

var themeName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// UI returns the UI router for the given port: http://localhost:<port>/ is the launcher,
// http://<app>.localhost:<port>/ an app's origin. The port is part of the Host check, so a
// page served elsewhere cannot rebind a name to it.
func (h *Host) UI(port int) http.Handler {
	ps := strconv.Itoa(port)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, p, err := net.SplitHostPort(r.Host)
		if err != nil || p != ps {
			http.Error(w, "wrong host", http.StatusMisdirectedRequest)
			return
		}
		host = strings.ToLower(host)
		if host == "localhost" {
			h.launcher(w, r, ps)
			return
		}
		sub, ok := strings.CutSuffix(host, ".localhost")
		if !ok {
			http.Error(w, "wrong host", http.StatusMisdirectedRequest)
			return
		}
		id, peer, _ := strings.Cut(sub, ".")
		h.mu.Lock()
		a := h.apps[id]
		h.mu.Unlock()
		if a == nil {
			http.Error(w, "no such app", http.StatusNotFound)
			return
		}
		hd := w.Header()
		hd.Set("Content-Security-Policy", CSP)
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("Cross-Origin-Opener-Policy", "same-origin")
		if peer != "" {
			// A peer's app proxied over Sol (API.md §10) is a later ticket.
			sol.WriteError(w, sol.Errorf("not_implemented", "opening a peer's app is not built yet"))
			return
		}
		h.serveApp(w, r, a, "http://"+id+".localhost:"+ps)
	})
}

// serveApp serves one request on an app's origin.
func (h *Host) serveApp(w http.ResponseWriter, r *http.Request, a *app, origin string) {
	switch p := r.URL.Path; {
	case p == "/_sol/theme.css":
		h.theme(w, r)
	case strings.HasPrefix(p, "/_sol/v1/"):
		h.bridge(w, r, a, origin)
	case strings.HasPrefix(p, "/_sol/"):
		sol.WriteError(w, sol.Errorf("not_found", "no such path"))
	case p == "/sol.js":
		http.ServeFileFS(w, r, soljs.FS, "sol.js")
	case strings.HasPrefix(p, "/kit/"):
		http.StripPrefix("/kit/", http.FileServerFS(kit.FS)).ServeHTTP(w, r)
	default:
		h.proxy(w, r, a)
	}
}

// sameOrigin is the bridge's CSRF check.
func sameOrigin(r *http.Request, origin string) bool {
	o := r.Header.Get("Origin")
	sfs := r.Header.Get("Sec-Fetch-Site")
	if o != "" && o != origin {
		return false
	}
	if sfs != "" && sfs != "same-origin" && sfs != "none" {
		return false
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead && o == "" && sfs == "" {
		return false
	}
	return true
}

// bridge serves /_sol/v1/... on an app origin: the app API, as the app.
func (h *Host) bridge(w http.ResponseWriter, r *http.Request, a *app, origin string) {
	if !sameOrigin(r, origin) {
		h.log.Info("refused cross-origin bridge request", "app", a.m.ID, "origin", r.Header.Get("Origin"))
		sol.WriteError(w, sol.Errorf("forbidden", "cross-origin request"))
		return
	}
	if h.cfg.Bridge == nil {
		sol.WriteError(w, sol.Errorf("unavailable", "no API"))
		return
	}
	r2 := r.Clone(r.Context())
	r2.URL.Path = strings.TrimPrefix(r.URL.Path, "/_sol")
	r2.URL.RawPath = ""
	r2.RequestURI = ""
	r2.Header.Set("Authorization", "Bearer "+a.bridge)
	// The bridge must not reach the admin API whatever the path says.
	if !strings.HasPrefix(r2.URL.Path, "/v1/") {
		sol.WriteError(w, sol.Errorf("not_found", "no such path"))
		return
	}
	h.cfg.Bridge.ServeHTTP(w, r2)
}

// theme serves /_sol/theme.css: <files>/.sol/theme names the active theme; it is looked up in
// <files>/.sol/themes/<name>.css, then among the built-in themes; the default is the built-in
// "sol" theme.
func (h *Host) theme(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	name := "sol"
	if h.cfg.Files != "" {
		if b, err := os.ReadFile(filepath.Join(h.cfg.Files, ".sol", "theme")); err == nil {
			if n := strings.TrimSpace(string(b)); themeName.MatchString(n) {
				name = n
			}
		}
		if b, err := os.ReadFile(filepath.Join(h.cfg.Files, ".sol", "themes", name+".css")); err == nil {
			_, _ = w.Write(b)
			return
		}
	}
	b, err := fs.ReadFile(kit.FS, "themes/"+name+".css")
	if err != nil {
		b, _ = fs.ReadFile(kit.FS, "themes/sol.css")
	}
	_, _ = w.Write(b)
}

// proxy passes a request to the app's socket as my own (Sol-Self: 1).
func (h *Host) proxy(w http.ResponseWriter, r *http.Request, a *app) {
	h.mu.Lock()
	up := a.running || a.attached
	addr := a.addr
	h.mu.Unlock()
	if !up || a.m.UI == "" {
		http.Error(w, "this app has no UI running", http.StatusServiceUnavailable)
		return
	}
	network, target := "tcp", addr
	if p, ok := strings.CutPrefix(addr, "unix://"); ok {
		network, target = "unix", p
	}
	person, device := "", ""
	if h.cfg.Me != nil {
		person, device = h.cfg.Me()
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = "http", "app"
			pr.Out.Host = pr.In.Host
			for k := range pr.Out.Header {
				if strings.HasPrefix(http.CanonicalHeaderKey(k), "Sol-") {
					pr.Out.Header.Del(k)
				}
			}
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Set("Sol-Person", person)
			pr.Out.Header.Set("Sol-Device", device)
			pr.Out.Header.Set("Sol-Self", "1")
		},
		Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, target)
		}, ResponseHeaderTimeout: time.Minute},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "the app is not answering", http.StatusBadGateway)
		},
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Del("Content-Security-Policy") // the host's policy is the only one
			return nil
		},
	}
	rp.ServeHTTP(w, r)
}

var launcherPage = template.Must(template.New("launcher").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Sol</title>
<style>
:root{color-scheme:light dark;--bg:#f6f6f8;--fg:#1c1c1e;--muted:#6e6e73;--card:#fff;--line:#e5e5ea;--tint:#c8890a}
@media (prefers-color-scheme:dark){:root{--bg:#111114;--fg:#f2f2f7;--muted:#98989f;--card:#1c1c1f;--line:#2c2c30}}
body{margin:0;background:var(--bg);color:var(--fg);font:15px/1.4 system-ui,-apple-system,"Segoe UI",sans-serif}
main{max-width:640px;margin:0 auto;padding:40px 16px}
h1{font-size:22px;margin:0 0 4px}p{color:var(--muted);margin:0 0 24px}
ul{list-style:none;padding:0;margin:0;display:grid;gap:10px}
a,div.app{display:block;background:var(--card);border:1px solid var(--line);border-radius:12px;padding:14px 16px;color:inherit;text-decoration:none}
a:hover{border-color:var(--tint)}b{display:block}span{color:var(--muted);font-size:13px}
</style></head><body><main>
<h1>Sol</h1><p>Your apps on this device.</p>
<ul>{{range .}}<li>{{if .URL}}<a href="{{.URL}}">{{else}}<div class="app">{{end}}<b>{{.Name}}</b><span>{{.Description}}{{if not .Running}} (not running){{end}}</span>{{if .URL}}</a>{{else}}</div>{{end}}</li>{{else}}<li><div class="app"><span>No apps installed.</span></div></li>{{end}}</ul>
</main></body></html>`))

// launcher serves the page at http://localhost:<port>/ that lists the installed apps.
func (h *Host) launcher(w http.ResponseWriter, r *http.Request, port string) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	type row struct {
		Name, Description, URL string
		Running                bool
	}
	var rows []row
	h.mu.Lock()
	for _, s := range h.statusLocked() {
		a := h.apps[s.ID]
		rw := row{Name: a.m.Name, Description: a.m.Description, Running: s.Running}
		if rw.Name == "" {
			rw.Name = a.m.ID
		}
		if a.m.UI != "" {
			rw.URL = "http://" + a.m.ID + ".localhost:" + port + a.m.UI
		}
		rows = append(rows, rw)
	}
	h.mu.Unlock()
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = launcherPage.Execute(w, rows)
}

// AppURL returns the UI origin of an app for a UI port.
func AppURL(id string, port int) string {
	return "http://" + id + ".localhost:" + strconv.Itoa(port) + "/"
}
