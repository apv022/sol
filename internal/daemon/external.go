package daemon

import (
	"errors"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
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
// token, shown this once, and the API URL it uses.
func (d *Daemon) RegisterExternal(m *sol.Manifest, pkg, cert string) (ExternalApp, error) {
	if m == nil {
		return ExternalApp{}, sol.Errorf("bad_request", "manifest is required")
	}
	api, err := d.externalAPIURL()
	if err != nil {
		return ExternalApp{}, err
	}
	got, token, err := d.Host.RegisterExternal(m, pkg, cert)
	if err != nil {
		return ExternalApp{}, err
	}
	return ExternalApp{ID: got.ID, Token: token, API: api}, nil
}
