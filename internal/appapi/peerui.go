package appapi

import (
	"net/http"
	"net/http/httputil"
	"strings"

	"github.com/apv022/sol/internal/feeds"
	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/session"
	"github.com/apv022/sol/sdk/go/sol"
)

// PeerUIPath prefixes the session endpoint that serves a contact my app's UI (API.md §10:
// http://<app>.<petname>.localhost:<port>/ on their device is proxied to /p/ui/<app>/<path>
// on mine).
const PeerUIPath = "/p/ui/"

// peerUI serves /p/ui/{app}/{path...} to a verified session peer: the request goes to my app's
// UI with the peer as the caller, if I share any of that app's feeds with them (my own devices
// always may). /_sol/ is never passed through: the bridge belongs to the origin's own device.
func (s *Server) peerUI(w http.ResponseWriter, r *http.Request) {
	p, ok := session.PeerFrom(r.Context())
	if !ok {
		sol.WriteError(w, sol.Errorf("forbidden", "not a session peer"))
		return
	}
	appID := r.PathValue("app")
	if !feeds.ValidApp(appID) || p.Relation != session.Self && !s.sharesApp(identity.ID(p.Person), appID) {
		sol.WriteError(w, sol.Errorf("forbidden", "that app is not shared with you"))
		return
	}
	path := "/" + r.PathValue("path")
	if strings.HasPrefix(path, "/_sol/") {
		sol.WriteError(w, sol.Errorf("not_found", "no such path"))
		return
	}
	a, ok := s.cfg.Apps.App(appID)
	if !ok || a.Manifest.UI == "" {
		sol.WriteError(w, sol.Errorf("not_found", "that app has no UI here"))
		return
	}
	if a.Addr == "" {
		sol.WriteError(w, sol.Errorf("unavailable", "app "+appID+" is not running"))
		return
	}
	who := peerCaller(p)
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = "http", "app"
			pr.Out.URL.Path, pr.Out.URL.RawPath = path, ""
			for k := range pr.Out.Header {
				if strings.HasPrefix(http.CanonicalHeaderKey(k), "Sol-") {
					pr.Out.Header.Del(k)
				}
			}
			pr.Out.Header.Del("Authorization")
			pr.Out.Header.Del("Cookie")
			who.set(pr.Out.Header)
		},
		Transport: s.client(a.Addr).Transport,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			sol.WriteError(w, sol.Errorf("unavailable", "the app is not answering"))
		},
	}
	rp.ServeHTTP(w, r)
}
