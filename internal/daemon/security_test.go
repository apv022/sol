package daemon

import (
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

// TestAppOriginGate (security review F2): app and peer origins serve nothing to a browser that
// isn't signed in there. Before, anything that could reach loopback (another OS user, another
// Android app) got any app's UI and its /_sol/ bridge as me just by sending the right Host. A
// signed-in root session opens an app origin through a one-time code bound to that origin.
func TestAppOriginGate(t *testing.T) {
	d := testDaemon(t) // SameUser always says no: a browser of another OS user
	port := strconv.Itoa(d.UIPort())
	root := "localhost:" + port
	app := "files.localhost:" + port
	appOrigin := "http://" + app

	// Unsigned: the bridge and the app's pages are refused (a page load is sent to sign in).
	if r := uiReq(t, d, "GET", app, "/_sol/v1/whoami", appOrigin, "", "", map[string]string{"Sec-Fetch-Mode": "cors"}); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unsigned bridge call: %d", r.StatusCode)
	}
	if r := uiReq(t, d, "POST", app, "/_sol/v1/actions/call", appOrigin, "", "{}", nil); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unsigned bridge POST: %d", r.StatusCode)
	}
	if r := uiReq(t, d, "GET", "notes.bob."+root, "/", "", "", "", nil); r.StatusCode != http.StatusSeeOther {
		t.Fatalf("unsigned peer origin: %d", r.StatusCode)
	}
	r := uiReq(t, d, "GET", app, "/docs?x=1", "", "", "", map[string]string{"Sec-Fetch-Mode": "navigate"})
	loc, _ := url.Parse(r.Header.Get("Location"))
	if r.StatusCode != http.StatusSeeOther || loc.Host != root || loc.Path != "/_sol/open" ||
		loc.Query().Get("to") != "files" || loc.Query().Get("next") != "/docs?x=1" {
		t.Fatalf("unsigned page load: %d %q", r.StatusCode, r.Header.Get("Location"))
	}

	// /_sol/open without a root session goes home; it never mints a code.
	if r := uiReq(t, d, "GET", root, loc.RequestURI(), "", "", "", nil); r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/" {
		t.Fatalf("open without a session: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	cookie := login(t, d)
	// A fetch can't collect a code, and the target must be an app-origin name.
	if r := uiReq(t, d, "GET", root, loc.RequestURI(), "", cookie, "", map[string]string{"Sec-Fetch-Mode": "cors"}); r.StatusCode != http.StatusForbidden {
		t.Fatalf("open by fetch: %d", r.StatusCode)
	}
	if r := uiReq(t, d, "GET", root, "/_sol/open?to=evil.example%2Fx", "", cookie, "", nil); r.StatusCode != http.StatusBadRequest {
		t.Fatalf("open of a bad name: %d", r.StatusCode)
	}
	r = uiReq(t, d, "GET", root, loc.RequestURI(), "", cookie, "", map[string]string{"Sec-Fetch-Mode": "navigate"})
	to, _ := url.Parse(r.Header.Get("Location"))
	if r.StatusCode != http.StatusSeeOther || to.Host != app || to.Path != "/_sol/login" || len(to.Query().Get("code")) != 64 {
		t.Fatalf("open: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	code := to.Query().Get("code")

	// The code is bound to its origin: another app's origin can't redeem it.
	if r := uiReq(t, d, "GET", "notes."+root, to.RequestURI(), "", "", "", nil); r.StatusCode != http.StatusForbidden {
		t.Fatalf("code redeemed on another origin: %d", r.StatusCode)
	}
	// It was burned by that attempt; mint another and redeem it where it belongs.
	r = uiReq(t, d, "GET", root, loc.RequestURI(), "", cookie, "", nil)
	to, _ = url.Parse(r.Header.Get("Location"))
	if to.Query().Get("code") == code {
		t.Fatal("same code twice")
	}
	r = uiReq(t, d, "GET", app, to.RequestURI(), "", "", "", nil)
	if r.StatusCode != http.StatusSeeOther || r.Header.Get("Location") != "/docs?x=1" {
		t.Fatalf("redeem: %d %q", r.StatusCode, r.Header.Get("Location"))
	}
	var appCookie *http.Cookie
	for _, c := range r.Cookies() {
		if c.Name == AppCookie {
			appCookie = c
		}
	}
	if appCookie == nil || !appCookie.HttpOnly || appCookie.SameSite != http.SameSiteStrictMode || appCookie.Domain != "" {
		t.Fatalf("app cookie: %+v", appCookie)
	}
	if r := uiReq(t, d, "GET", app, to.RequestURI(), "", "", "", nil); r.StatusCode != http.StatusForbidden {
		t.Fatalf("code reused: %d", r.StatusCode)
	}
	withApp := func(_, cookie string) map[string]string {
		return map[string]string{"Cookie": AppCookie + "=" + cookie, "Sec-Fetch-Mode": "cors"}
	}
	// Signed in: the bridge answers (whoami). The cookie only works on its own origin.
	if r := uiReq(t, d, "GET", app, "/_sol/v1/whoami", appOrigin, "", "", withApp(app, appCookie.Value)); r.StatusCode == http.StatusUnauthorized {
		t.Fatalf("signed-in bridge call: %d", r.StatusCode)
	}
	if r := uiReq(t, d, "GET", "notes."+root, "/_sol/v1/whoami", "http://notes."+root, "", "", withApp(app, appCookie.Value)); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("cookie of files worked on notes: %d", r.StatusCode)
	}
	// Signing out of the root signs out every app origin.
	if r := uiReq(t, d, "POST", root, "/admin/v1/logout", "http://"+root, cookie, "", nil); r.StatusCode/100 != 2 {
		t.Fatalf("logout: %d", r.StatusCode)
	}
	if r := uiReq(t, d, "GET", app, "/_sol/v1/whoami", appOrigin, "", "", withApp(app, appCookie.Value)); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("app cookie outlived its session: %d", r.StatusCode)
	}
}

// TestAppOriginCSRF (security review F3): another page in the same browser (a website, a
// contact's app page, another app) can't drive an app origin: a state change needs the
// origin's own Origin, and a cross-site GET must be a top-level navigation. This holds even for
// the daemon's own OS user, whose browser is otherwise let in without a cookie.
func TestAppOriginCSRF(t *testing.T) {
	d := testDaemonAs(t, func(_, _ netip.AddrPort) (bool, error) { return true, nil })
	port := strconv.Itoa(d.UIPort())
	app := "files.localhost:" + port
	for _, c := range []struct {
		method, origin, site, mode string
		ok                         bool
	}{
		{"POST", "http://evil.example", "cross-site", "no-cors", false},
		{"POST", "http://files.bob.localhost:" + port, "same-site", "no-cors", false},
		{"POST", "", "", "", false}, // a form post that hides its origin
		{"POST", "http://" + app, "same-origin", "cors", true},
		{"GET", "", "cross-site", "no-cors", false}, // <img>/<script> from another site
		{"GET", "http://notes.localhost:" + port, "same-site", "cors", false},
		{"GET", "", "cross-site", "navigate", true}, // a link from elsewhere opens the app
		{"GET", "", "same-origin", "cors", true},
	} {
		hdr := map[string]string{}
		if c.site != "" {
			hdr["Sec-Fetch-Site"], hdr["Sec-Fetch-Mode"] = c.site, c.mode
		}
		r := uiReq(t, d, c.method, app, "/_sol/v1/whoami", c.origin, "", "", hdr)
		if refused := r.StatusCode == http.StatusForbidden && strings.Contains(r.Header.Get("Content-Type"), "json"); refused == c.ok {
			t.Errorf("%s from %q (%s, %s): %d", c.method, c.origin, c.site, c.mode, r.StatusCode)
		}
	}
}

// TestCannotShareOwnPort (security review F7): the daemon dials a shared port itself, so the
// contact's stream would reach Sol's UI as this OS user and be signed in without a code.
func TestCannotShareOwnPort(t *testing.T) {
	d := testDaemon(t)
	if _, err := d.ports.AddService(Service{Name: "sol", Port: d.UIPort()}); err == nil {
		t.Fatal("shared Sol's own UI port")
	}
	if _, err := d.ports.AddService(Service{Name: "web", Port: 1}); err != nil {
		t.Fatal(err)
	}
}

// TestSameUserSessionBound (security review F10): a session that same-user auto-login started
// works only on that user's connections. Cookies ignore ports, so a server another user runs on
// some other localhost port gets the cookie from my browser; replaying it must not sign them in.
func TestSameUserSessionBound(t *testing.T) {
	var same atomic.Bool
	same.Store(true)
	d := testDaemonAs(t, func(_, _ netip.AddrPort) (bool, error) { return same.Load(), nil })
	root := "localhost:" + strconv.Itoa(d.UIPort())
	origin := "http://" + root
	r := uiReq(t, d, "GET", root, "/admin/v1/status", origin, "", "", nil)
	var tok string
	for _, c := range r.Cookies() {
		if c.Name == SessionCookie {
			tok = c.Value
		}
	}
	if r.StatusCode != 200 || tok == "" {
		t.Fatalf("auto-login: %d", r.StatusCode)
	}
	if r := uiReq(t, d, "GET", root, "/admin/v1/status", origin, tok, "", nil); r.StatusCode != 200 {
		t.Fatalf("same user with the cookie: %d", r.StatusCode)
	}
	same.Store(false) // another OS user replays the stolen cookie
	if r := uiReq(t, d, "GET", root, "/admin/v1/status", origin, tok, "", nil); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("stolen same-user cookie: %d", r.StatusCode)
	}
	if r := uiReq(t, d, "GET", root, "/_sol/open?to=files", "", tok, "", nil); strings.Contains(r.Header.Get("Location"), "code=") {
		t.Fatal("a stolen same-user cookie minted an app code")
	}
	// A session from a login code is the bearer kind (another uid's browser, Android).
	code := login(t, d)
	if r := uiReq(t, d, "GET", root, "/admin/v1/status", origin, code, "", nil); r.StatusCode != 200 {
		t.Fatalf("login-code session: %d", r.StatusCode)
	}
}
