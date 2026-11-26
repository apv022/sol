package daemon

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/apv022/sol/internal/fsutil"
	"github.com/apv022/sol/sdk/go/sol"
)

// External apps (design/APPS_EVERYWHERE.md M5): programs Sol does not run, such as a separate
// Android APK, that the person approved as Sol apps. apphost keeps them (RegisterExternal);
// the daemon gives them a way in. The app API normally listens on a unix socket in Sol's own
// run directory, which another Android app cannot open, so while any external app is
// registered the daemon also serves the app API, /v1/ only (never /admin/ or /agent/), on a
// loopback TCP port. The port is chosen once and kept in external-api.addr, because the
// external app stores the URL it was given. Loopback is open to every program on the device:
// the token is the only gate, as it is for the API everywhere (API.md "How an app talks to Sol").

// externalAPIFile keeps the external API's address across restarts.
const externalAPIFile = "external-api.addr"

// ExternalApp is the answer of POST /admin/v1/apps/external: the token is shown only here.
type ExternalApp struct {
	ID    string `json:"id"`
	Token string `json:"token"`
	API   string `json:"api"`
}

// extAPI is the loopback listener external apps reach the app API on.
type extAPI struct {
	mu  sync.Mutex
	url string // "http://127.0.0.1:PORT" once serving
	srv *http.Server
}

// externalAPIURL starts the external API listener if it is not running and returns its URL.
func (d *Daemon) externalAPIURL() (string, error) {
	d.ext.mu.Lock()
	defer d.ext.mu.Unlock()
	if d.ext.url != "" {
		return d.ext.url, nil
	}
	path := filepath.Join(d.dir, externalAPIFile)
	addr := "127.0.0.1:0"
	if b, err := os.ReadFile(path); err == nil {
		addr = strings.TrimSpace(string(b))
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		// Moving silently would break every external app holding the old URL: say so.
		return "", sol.Errorf("unavailable", fmt.Sprintf("the external app API address %s is in use: %v", addr, err))
	}
	if addr != ln.Addr().String() {
		if err := fsutil.WriteFileAtomic(path, []byte(ln.Addr().String()+"\n"), 0o600); err != nil {
			ln.Close()
			return "", err
		}
	}
	api := d.API.Handler()
	d.ext.srv = &http.Server{ReadHeaderTimeout: 10 * time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/v1/") {
			sol.WriteError(w, sol.Errorf("not_found", "only the app API (/v1/) is served here"))
			return
		}
		api.ServeHTTP(w, r)
	})}
	go func() { _ = d.ext.srv.Serve(ln) }()
	d.ext.url = "http://" + ln.Addr().String()
	return d.ext.url, nil
}

// startExternalAPI serves the external API at start when external apps are registered.
func (d *Daemon) startExternalAPI() {
	if !d.Host.HasExternal() {
		return
	}
	if _, err := d.externalAPIURL(); err != nil {
		d.log.Warn("external apps cannot reach Sol", "err", err)
	}
}

// closeExternalAPI stops the external API listener.
func (d *Daemon) closeExternalAPI() {
	d.ext.mu.Lock()
	defer d.ext.mu.Unlock()
	if d.ext.srv != nil {
		_ = d.ext.srv.Close()
	}
}

// RegisterExternal registers an external app (POST /admin/v1/apps/external) and returns its
// token, shown this once, and the API URL it uses. icon is the app's launcher icon (PNG) or nil.
func (d *Daemon) RegisterExternal(m *sol.Manifest, pkg, cert string, icon []byte) (ExternalApp, error) {
	if m == nil {
		return ExternalApp{}, sol.Errorf("bad_request", "manifest is required")
	}
	api, err := d.externalAPIURL()
	if err != nil {
		return ExternalApp{}, err
	}
	got, token, err := d.Host.RegisterExternal(m, pkg, cert, icon)
	if err != nil {
		return ExternalApp{}, err
	}
	return ExternalApp{ID: got.ID, Token: token, API: api}, nil
}

// glyphName is a kit glyph name, the only kind of icon an external app's manifest may name.
var glyphName = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// externalIconURL is where the UI root serves external app id's icon (serveExternalIcon). The
// version (the icon's modification time) changes the URL when a re-connect brings a new icon.
func externalIconURL(id string, mod time.Time) string {
	return "/app-icons/" + url.PathEscape(id) + "?v=" + strconv.FormatInt(mod.UnixNano(), 36)
}

// serveExternalIcon serves GET /app-icons/<id> on the UI root: an external app's icon.png. It
// needs the session cookie like the wallpapers (the home page's own request carries it); the
// bytes were validated as a PNG when the app connected and go out as image/png with nosniff.
func (d *Daemon) serveExternalIcon(w http.ResponseWriter, r *http.Request) {
	if !d.hasSession(r) {
		sol.WriteError(w, sol.Errorf("unauthorized", "This browser isn't signed in to Sol."))
		return
	}
	p, mod, ok := d.Host.ExternalIcon(r.PathValue("id"))
	if !ok {
		sol.WriteError(w, sol.Errorf("not_found", "no such icon"))
		return
	}
	f, err := os.Open(p)
	if err != nil {
		sol.WriteError(w, sol.Errorf("not_found", "no such icon"))
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Cache-Control", "private, no-cache")
	http.ServeContent(w, r, "", mod, f)
}
