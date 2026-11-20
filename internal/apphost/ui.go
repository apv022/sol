package apphost

import (
	"context"
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

// CSP is the Content-Security-Policy of every app origin and the UI root (design/UI.md
// "Themes", Safety). Scripts never run inline or from eval, and styles come only from the
// origin's own stylesheets: no style attributes or <style> elements in markup, so injected
// markup can't restyle a page either. UIs set computed values through the CSSOM
// (element.style.setProperty, which the policy allows) or sol.js's data-var-* attributes.
const CSP = "default-src 'self'; img-src 'self' data: blob:; media-src 'self' data: blob:; font-src 'self' data:; " +
	"style-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'"

var themeName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// UI returns the UI router for the given port: http://localhost:<port>/ (and
// http://127.0.0.1:<port>/) is the Sol app (Config.Root), http://<app>.localhost:<port>/ an
// app's origin, and http://<name>.<petname>.localhost:<port>/ a contact's app or service
// (Config.Peer). The port is part of the Host check, so a page served elsewhere cannot rebind a
// name to it.
func (h *Host) UI(port int) http.Handler {
	ps := strconv.Itoa(port)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, p, err := net.SplitHostPort(r.Host)
		if err != nil || p != ps {
			http.Error(w, "wrong host", http.StatusMisdirectedRequest)
			return
		}
		host = strings.ToLower(host)
		if host == "localhost" || host == "127.0.0.1" {
			if h.cfg.Root == nil {
				http.NotFound(w, r)
				return
			}
			SecurityHeaders(w.Header())
			h.cfg.Root.ServeHTTP(w, r)
			return
		}
		sub, ok := strings.CutSuffix(host, ".localhost")
		if !ok {
			http.Error(w, "wrong host", http.StatusMisdirectedRequest)
			return
		}
		id, peer, _ := strings.Cut(sub, ".")
		origin := "http://" + host + ":" + ps
		SecurityHeaders(w.Header())
		if !FetchAllowed(r, origin) {
			h.log.Info("refused a cross-site request to an app origin", "host", host, "method", r.Method)
			sol.WriteError(w, sol.Errorf("forbidden", "cross-site request"))
			return
		}
		if h.cfg.Gate != nil && !h.cfg.Gate(w, r, origin) {
			return
		}
		if peer != "" {
			if h.cfg.Peer == nil || strings.Contains(peer, ".") {
				sol.WriteError(w, sol.Errorf("not_implemented", "opening a peer's app is not available here"))
				return
			}
			h.cfg.Peer(w, r, id, peer, origin)
			return
		}
		h.mu.Lock()
		a := h.apps[id]
		h.mu.Unlock()
		if a == nil {
			http.Error(w, "no such app", http.StatusNotFound)
			return
		}
		h.serveApp(w, r, a, origin)
	})
}

// SecurityHeaders sets the headers every UI origin's responses carry: the CSP and its
// companions. The Sol app at the UI root gets exactly the same policy as app origins.
func SecurityHeaders(hd http.Header) {
	hd.Set("Content-Security-Policy", CSP)
	hd.Set("X-Content-Type-Options", "nosniff")
	hd.Set("Referrer-Policy", "no-referrer")
	hd.Set("Cross-Origin-Opener-Policy", "same-origin")
}

// ServeShared serves the files every UI origin has in common (/_sol/theme.css, /sol.js and
// /kit/...) and reports whether r was one of them.
func (h *Host) ServeShared(w http.ResponseWriter, r *http.Request) bool {
	switch p := r.URL.Path; {
	case p == "/_sol/theme.css":
		h.theme(w, r)
	case p == "/sol.js":
		http.ServeFileFS(w, r, soljs.FS, "sol.js")
	case strings.HasPrefix(p, "/kit/"):
		http.StripPrefix("/kit/", http.FileServerFS(kit.FS)).ServeHTTP(w, r)
	default:
		return false
	}
	return true
}

// serveApp serves one request on an app's origin.
func (h *Host) serveApp(w http.ResponseWriter, r *http.Request, a *app, origin string) {
	if h.ServeShared(w, r) {
		return
	}
	switch p := r.URL.Path; {
	case strings.HasPrefix(p, "/_sol/v1/"):
		h.bridge(w, r, a, origin)
	case strings.HasPrefix(p, "/_sol/"):
		sol.WriteError(w, sol.Errorf("not_found", "no such path"))
	default:
		h.proxy(w, r, a)
	}
}

// FetchAllowed is the CSRF check of every app and peer origin, in front of whatever the app
// serves: a request that changes something (any method but GET and HEAD) must come from the
// origin itself (sameOrigin), and a GET or HEAD from another site (Sec-Fetch-Site same-site or
// cross-site, which includes every other *.localhost origin) must be a top-level navigation,
// so opening an app from a link works but no other page can drive it or embed its responses.
// The apps behind it see Sol-Self: 1 and trust it; this is what makes that safe against a
// page open in the same browser (another app, a contact's page, any website).
func FetchAllowed(r *http.Request, origin string) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return sameOrigin(r, origin)
	}
	if o := r.Header.Get("Origin"); o != "" && o != origin {
		return false
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
		return true
	}
	return r.Header.Get("Sec-Fetch-Mode") == "navigate"
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

// AppURL returns the UI origin of an app for a UI port.
func AppURL(id string, port int) string {
	return "http://" + id + ".localhost:" + strconv.Itoa(port) + "/"
}
