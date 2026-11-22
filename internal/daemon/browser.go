package daemon

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"html/template"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/apv022/sol/internal/fsutil"
	"github.com/apv022/sol/sdk/go/sol"
	home "github.com/apv022/sol/ui/home"
)

// Browser login (design/ADMIN.md "Who may use it").
//
// Other users of this computer can reach loopback, so the Sol app at the UI root needs a
// session. `sol open` asks for a one-time login code with the admin token and opens
// /login?code=<code>; redeeming it sets the sol_session cookie and redirects to /.
//
// Invariants:
//   - A code is 32 random bytes, valid once, for LoginCodeTTL. Only its SHA-256 is kept.
//   - A session token is 32 random bytes; only its SHA-256 is stored (browser-sessions.json,
//     0600, atomic), so the file cannot be replayed as a cookie. A session lasts SessionTTL
//     from its last use (renewed on use) and can be revoked.
//   - The cookie is HttpOnly, SameSite=Strict, Path=/ and host-only (no Domain), so app origins
//     (<app>.localhost) never receive it.
//   - A browser admin request is served only when its Host is localhost:<port> or
//     127.0.0.1:<port> (the UI router checks that), its Origin is that same origin (a request
//     without Origin is accepted only for GET/HEAD, and then not when Sec-Fetch-Site says
//     another site), and the cookie names a live session. A request from an app origin or
//     any other origin is refused before the cookie is looked at.
//   - A browser with no live session may be let in without a code when the TCP connection
//     belongs to the daemon's own OS user (autoLogin, peercred.go); it passes the same Host and
//     Origin checks first, and app origins never reach it.
//   - Minting login codes needs the admin token: a browser session cannot mint more.

// Tunables (package variables so tests can shorten them).
var (
	LoginCodeTTL = 60 * time.Second
	SessionTTL   = 30 * 24 * time.Hour
	// sessionTouchEvery bounds how often a session's renewal is written to disk.
	sessionTouchEvery = time.Minute
)

// SessionCookie is the browser session cookie's name.
const SessionCookie = "sol_session"

// BrowserSession is one browser login (GET /admin/v1/sessions).
type BrowserSession struct {
	ID       string    `json:"id"`
	Client   string    `json:"client"` // the browser's User-Agent at login, shortened
	Created  time.Time `json:"created"`
	LastUsed time.Time `json:"last_used"`
	Expires  time.Time `json:"expires"`
	Current  bool      `json:"current,omitempty"`
}

type storedSession struct {
	BrowserSession
	Hash string `json:"hash"` // hex SHA-256 of the cookie value
	// SameUser marks a session started by same-user auto-login: it is good only on connections
	// of the daemon's own OS user. Cookies are not scoped by port, so any page served on
	// another localhost port (another user's server, say) receives it; bound like this, a
	// stolen cookie is worthless to anyone else (security review F10).
	SameUser bool `json:"same_user,omitempty"`
}

type browserAuth struct {
	path string

	mu       sync.Mutex
	codes    map[[32]byte]time.Time // code hash -> expiry
	appCodes map[[32]byte]appCode   // code hash -> the app origin it signs in on
	sess     map[[32]byte]*storedSession
}

// appCode is a one-time code that signs a browser in on one app or peer origin for the root
// session that minted it (see appGate).
type appCode struct {
	origin  string
	session [32]byte // the minting session's token hash
	expires time.Time
}

func openBrowserAuth(p string) (*browserAuth, error) {
	a := &browserAuth{path: p, codes: map[[32]byte]time.Time{}, appCodes: map[[32]byte]appCode{},
		sess: map[[32]byte]*storedSession{}}
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return a, nil
	}
	if err != nil {
		return nil, err
	}
	var list []storedSession
	if err := json.Unmarshal(b, &list); err != nil {
		return nil, errors.New("daemon: browser-sessions.json is corrupt")
	}
	now := time.Now()
	for i := range list {
		raw, err := hex.DecodeString(list[i].Hash)
		if err != nil || len(raw) != 32 || now.After(list[i].Expires) {
			continue
		}
		a.sess[[32]byte(raw)] = &list[i]
	}
	return a, nil
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func hash32(s string) [32]byte { return sha256.Sum256([]byte(s)) }

// saveLocked writes the sessions file. a.mu held.
func (a *browserAuth) saveLocked() error {
	list := make([]storedSession, 0, len(a.sess))
	for _, s := range a.sess {
		list = append(list, *s)
	}
	slices.SortFunc(list, func(x, y storedSession) int { return x.Created.Compare(y.Created) })
	b, _ := json.MarshalIndent(list, "", "  ")
	return fsutil.WriteFileAtomic(a.path, b, 0o600)
}

// code mints a one-time login code.
func (a *browserAuth) code() string {
	c := randomHex(32)
	now := time.Now()
	a.mu.Lock()
	for h, exp := range a.codes {
		if now.After(exp) {
			delete(a.codes, h)
		}
	}
	a.codes[hash32(c)] = now.Add(LoginCodeTTL)
	a.mu.Unlock()
	return c
}

// redeem burns a code and, if it was valid, starts a session and returns its cookie value.
func (a *browserAuth) redeem(code, client string) (string, *BrowserSession, bool) {
	h := hash32(code)
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.codes[h]
	delete(a.codes, h)
	if !ok || now.After(exp) {
		return "", nil, false
	}
	return a.startLocked(client, now, false)
}

// start begins a session without a code (same-OS-user auto-login), bound to that user.
func (a *browserAuth) start(client string) (string, *BrowserSession, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.startLocked(client, time.Now(), true)
}

func (a *browserAuth) startLocked(client string, now time.Time, sameUser bool) (string, *BrowserSession, bool) {
	tok := randomHex(32)
	th := hash32(tok)
	client = friendlyClient(client)
	s := &storedSession{BrowserSession: BrowserSession{ID: randomHex(8), Client: client, Created: now.UTC(),
		LastUsed: now.UTC(), Expires: now.Add(SessionTTL).UTC()}, Hash: hex.EncodeToString(th[:]), SameUser: sameUser}
	a.sess[th] = s
	if err := a.saveLocked(); err != nil {
		delete(a.sess, th)
		return "", nil, false
	}
	bs := s.BrowserSession
	return tok, &bs, true
}

// check returns the live session a cookie value names, renewing it (renewed reports that the
// expiry moved, so the cookie should be sent again). sameUser(), asked only for a session
// bound to the daemon's OS user, must confirm the request's connection is that user's.
func (a *browserAuth) check(tok string, sameUser func() bool) (BrowserSession, bool, bool) {
	h := hash32(tok)
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sess[h]
	if !ok || s.SameUser && !sameUser() {
		return BrowserSession{}, false, false
	}
	if now.After(s.Expires) {
		delete(a.sess, h)
		_ = a.saveLocked()
		return BrowserSession{}, false, false
	}
	renewed := false
	if now.Sub(s.LastUsed) >= sessionTouchEvery {
		s.LastUsed, s.Expires = now.UTC(), now.Add(SessionTTL).UTC()
		_ = a.saveLocked()
		renewed = true
	}
	return s.BrowserSession, true, renewed
}

// live reports whether tok names an unexpired session, without renewing it.
func (a *browserAuth) live(tok string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	s, ok := a.sess[hash32(tok)]
	return ok && time.Now().Before(s.Expires)
}

// list returns the live sessions, oldest first; current is marked.
func (a *browserAuth) list(current string) []BrowserSession {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := []BrowserSession{}
	now := time.Now()
	for _, s := range a.sess {
		if now.After(s.Expires) {
			continue
		}
		bs := s.BrowserSession
		bs.Client = friendlyClient(bs.Client) // sessions stored before this kept the raw User-Agent
		bs.Current = bs.ID == current
		out = append(out, bs)
	}
	slices.SortFunc(out, func(x, y BrowserSession) int { return x.Created.Compare(y.Created) })
	return out
}

// revoke ends a session by ID.
func (a *browserAuth) revoke(id string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for h, s := range a.sess {
		if s.ID == id {
			delete(a.sess, h)
			_ = a.saveLocked()
			return true
		}
	}
	return false
}

// appToken is the sol_app cookie value of a root session on an app origin: an HMAC of the
// origin keyed by the session's token hash. It is bound to one origin (another app's or a
// contact's page can't reuse it), it dies with the session (logout and revocation sign out
// every app origin too), and it needs no state of its own, so it survives restarts.
func appToken(session [32]byte, origin string) string {
	m := hmac.New(sha256.New, session[:])
	m.Write([]byte("sol app origin v1|" + origin))
	return hex.EncodeToString(m.Sum(nil))
}

// appCodeFor mints a one-time code for origin on behalf of the live root session tok. A
// session bound to the daemon's OS user mints none (that user's browser is let in on app
// origins without a code).
func (a *browserAuth) appCodeFor(tok, origin string) (string, bool) {
	th := hash32(tok)
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	if s, ok := a.sess[th]; !ok || now.After(s.Expires) || s.SameUser {
		return "", false
	}
	for h, c := range a.appCodes {
		if now.After(c.expires) {
			delete(a.appCodes, h)
		}
	}
	c := randomHex(32)
	a.appCodes[hash32(c)] = appCode{origin: origin, session: th, expires: now.Add(LoginCodeTTL)}
	return c, true
}

// redeemApp burns an app code and returns the cookie value for origin, if the code was minted
// for exactly this origin, is unexpired, and its session is still live.
func (a *browserAuth) redeemApp(code, origin string) (string, bool) {
	h := hash32(code)
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	c, ok := a.appCodes[h]
	delete(a.appCodes, h)
	if !ok || now.After(c.expires) || c.origin != origin {
		return "", false
	}
	if s, ok := a.sess[c.session]; !ok || now.After(s.Expires) || s.SameUser {
		return "", false
	}
	return appToken(c.session, origin), true
}

// appLive reports whether cookie is the app token of a live session for origin.
func (a *browserAuth) appLive(cookie, origin string) bool {
	if len(cookie) != 64 {
		return false
	}
	now := time.Now()
	a.mu.Lock()
	defer a.mu.Unlock()
	for h, s := range a.sess {
		if now.Before(s.Expires) && !s.SameUser && hmac.Equal([]byte(appToken(h, origin)), []byte(cookie)) {
			return true
		}
	}
	return false
}

// browserKey marks an admin request that came from a browser session; its value is the
// session ID.
type browserKey struct{}

// browserSession returns the browser session ID of an admin request ("" for the CLI).
func browserSession(ctx context.Context) string {
	s, _ := ctx.Value(browserKey{}).(string)
	return s
}

// SignedOutCookie marks a browser that signed out: it turns same-user auto-login off for that
// browser until the person asks to sign in again. It holds no secret.
const SignedOutCookie = "sol_signed_out"

// setSignedOutCookie sets (on) or clears the signed-out marker, HttpOnly and SameSite=Strict
// like the session cookie, and long-lived so it outlasts the browser session.
func setSignedOutCookie(w http.ResponseWriter, on bool) {
	c := &http.Cookie{Name: SignedOutCookie, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode}
	if on {
		c.Value, c.MaxAge = "1", 10*365*24*60*60
	} else {
		c.MaxAge = -1
	}
	http.SetCookie(w, c)
}

// signIn is POST /_sol/signin: the "Sign in" button of the locked screen. It starts a session
// for a browser on the daemon's own OS user (same rules as autoLogin: Host pinned by the router,
// Origin must be the UI root, the connection's owner must be this user) and clears the
// signed-out marker. Any other browser still needs a login code.
func (d *Daemon) signIn(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if _, ok := d.startSameUser(w, r); !ok {
		sol.WriteError(w, sol.Errorf("forbidden", "Only the person who runs Sol on this computer can sign in this way. Run `sol open` for a login link."))
		return
	}
	setSignedOutCookie(w, false)
	d.emitStatus()
	sol.WriteJSON(w, http.StatusOK, struct{}{})
}

func setSessionCookie(w http.ResponseWriter, tok string, maxAge int) {
	http.SetCookie(w, &http.Cookie{Name: SessionCookie, Value: tok, Path: "/", MaxAge: maxAge,
		HttpOnly: true, SameSite: http.SameSiteStrictMode})
}

// sameUIOrigin is the browser admin API's CSRF check: Origin must be the UI root origin the
// request was sent to (the router already pinned Host to localhost:<port> or
// 127.0.0.1:<port>). Browsers omit Origin on some same-origin GETs (EventSource, plain
// fetch), so a GET or HEAD without it passes unless Sec-Fetch-Site names another site; a
// GET changes nothing, and the SameSite=Strict cookie is not sent cross-site anyway.
func sameUIOrigin(r *http.Request) bool {
	o := r.Header.Get("Origin")
	if o != "" {
		return o == "http://"+strings.ToLower(r.Host)
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	sfs := r.Header.Get("Sec-Fetch-Site")
	return sfs == "" || sfs == "same-origin" || sfs == "none"
}

var loginFailed = template.Must(template.New("login").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Sol</title><link rel="stylesheet" href="/kit/kit.css"></head>
<body><main class="empty">
<h1>This login link has expired</h1>
<p>Login links work once, for a minute. Open Sol from the app, or run <code>sol open</code> again.</p>
</main></body></html>`))

// autoLogin starts a browser session when the request came from the daemon's own OS user: the
// connection's client end must be a loopback socket owned by the same uid (SID on Windows).
// The Host was pinned by the router; the Origin / Sec-Fetch-Site check is repeated here so a
// cross-site navigation cannot trigger it. It looks the owner up on every call and caches
// nothing. On success the session cookie is set on w and the session ID returned.
//
// A browser that signed out holds the sol_signed_out cookie (logout sets it) and is not let in
// again this way: signing out would otherwise undo itself on the next request. It signs back in
// with the "Sign in" button on the locked screen (signIn), which clears the cookie.
func (d *Daemon) autoLogin(w http.ResponseWriter, r *http.Request) (string, bool) {
	if c, err := r.Cookie(SignedOutCookie); err == nil && c.Value != "" {
		return "", false
	}
	return d.startSameUser(w, r)
}

// startSameUser is autoLogin without the signed-out check.
func (d *Daemon) startSameUser(w http.ResponseWriter, r *http.Request) (string, bool) {
	if !sameUIOrigin(r) || !d.sameUser(r) {
		return "", false
	}
	tok, s, ok := d.browser.start(r.UserAgent())
	if !ok {
		return "", false
	}
	setSessionCookie(w, tok, int(SessionTTL/time.Second))
	d.activity("sol.login", map[string]string{"session": s.ID, "how": "same-user"}, nil)
	return s.ID, true
}

// sameUser reports whether the TCP connection r arrived on belongs to the daemon's own OS user
// (peercred.go). It looks the owner up on every call and caches nothing.
func (d *Daemon) sameUser(r *http.Request) bool {
	remote, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	la, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr)
	if la == nil {
		return false
	}
	local, err := netip.ParseAddrPort(la.String())
	if err != nil {
		return false
	}
	same := d.opt.SameUser
	if same == nil {
		same = defaultSameUser
	}
	ok, err := same(remote, local)
	if err != nil && !errors.Is(err, ErrSameUserUnsupported) {
		d.log.Debug("same-user check failed", "err", err)
	}
	return err == nil && ok
}

// AppCookie signs a browser in on one app or peer origin (appGate).
const AppCookie = "sol_app"

// appHostRE matches the part of an app or peer origin's host before ".localhost".
var appHostRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}(\.[a-z0-9][a-z0-9-]{0,62})?$`)

// safeNext returns p if it is a path on the same origin ("/..." but not "//..." or "/\..."),
// else "/".
func safeNext(p string) string {
	if !strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.ContainsAny(p, "\\\r\n") {
		return "/"
	}
	return p
}

// appGate guards every app and peer origin (apphost.Config.Gate; design/ADMIN.md "Who may use
// it"). Apps trust Sol-Self: 1 on whatever reaches them, and loopback is reachable by other OS
// users and other Android apps, so nothing on an app origin is served to a browser that isn't
// signed in there. A browser is signed in when the connection belongs to the daemon's own OS
// user (looked up per request), or when it holds the sol_app cookie of that origin, which it
// gets by a one-time code from the UI root's live session: an unsigned page load is sent to
// /_sol/open on the root, which mints the code and sends the browser back to
// /_sol/login?code= here. The CSRF rule (apphost.FetchAllowed) has already run.
func (d *Daemon) appGate(w http.ResponseWriter, r *http.Request, origin string) bool {
	if r.URL.Path == "/_sol/login" {
		w.Header().Set("Cache-Control", "no-store")
		v, ok := d.browser.redeemApp(r.URL.Query().Get("code"), origin)
		if !ok {
			d.log.Info("refused an app login code (unknown, used, expired or for another origin)")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusForbidden)
			_ = loginFailed.Execute(w, nil)
			return false
		}
		http.SetCookie(w, &http.Cookie{Name: AppCookie, Value: v, Path: "/", MaxAge: int(SessionTTL / time.Second),
			HttpOnly: true, SameSite: http.SameSiteStrictMode})
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return false
	}
	if c, err := r.Cookie(AppCookie); err == nil && d.browser.appLive(c.Value, origin) {
		return true
	}
	if d.sameUser(r) {
		return true
	}
	if m := r.Header.Get("Sec-Fetch-Mode"); (r.Method == http.MethodGet || r.Method == http.MethodHead) && (m == "" || m == "navigate") {
		host := strings.TrimSuffix(strings.TrimPrefix(origin, "http://"), ":"+strconv.Itoa(d.UIPort()))
		sub := strings.TrimSuffix(host, ".localhost")
		q := url.Values{"to": {sub}, "next": {r.URL.RequestURI()}}
		http.Redirect(w, r, "http://localhost:"+strconv.Itoa(d.UIPort())+"/_sol/open?"+q.Encode(), http.StatusSeeOther)
		return false
	}
	sol.WriteError(w, sol.Errorf("unauthorized", "This browser isn't signed in to Sol here. Open the app from Sol."))
	return false
}

// openApp serves GET /_sol/open?to=<sub>&next=<path> on the UI root: with a live session it
// mints a one-time code for http://<sub>.localhost:<port> and sends the browser there (appGate
// redeems it); without one, to the Sol app, which shows how to sign in. Only a top-level
// navigation may use it, so no page can collect codes with fetch.
func (d *Daemon) openApp(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if m := r.Header.Get("Sec-Fetch-Mode"); m != "" && m != "navigate" {
		sol.WriteError(w, sol.Errorf("forbidden", "only a page load may open an app"))
		return
	}
	sub := r.URL.Query().Get("to")
	if !appHostRE.MatchString(sub) {
		sol.WriteError(w, sol.Errorf("bad_request", "bad app"))
		return
	}
	port := strconv.Itoa(d.UIPort())
	origin := "http://" + sub + ".localhost:" + port
	c, err := r.Cookie(SessionCookie)
	if err != nil || c.Value == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	code, ok := d.browser.appCodeFor(c.Value, origin)
	if !ok {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	q := url.Values{"code": {code}, "next": {safeNext(r.URL.Query().Get("next"))}}
	http.Redirect(w, r, origin+"/_sol/login?"+q.Encode(), http.StatusSeeOther)
}

// hasSession reports whether the request carries a live session cookie (without renewing).
func (d *Daemon) hasSession(r *http.Request) bool {
	c, err := r.Cookie(SessionCookie)
	if err != nil || c.Value == "" {
		return false
	}
	return d.browser.live(c.Value)
}

// rootHandler serves the UI root: the Sol app (ui/home), /login, and the admin API for the
// browser at /admin/v1/.
func (d *Daemon) rootHandler() http.Handler {
	selftest := os.Getenv("SOL_UI_SELFTEST") == "1"
	mx := http.NewServeMux()
	mx.HandleFunc("GET /login", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		tok, s, ok := d.browser.redeem(r.URL.Query().Get("code"), r.UserAgent())
		if !ok {
			d.log.Info("refused a login code (unknown, used or expired)")
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusForbidden)
			_ = loginFailed.Execute(w, nil)
			return
		}
		setSessionCookie(w, tok, int(SessionTTL/time.Second))
		d.activity("sol.login", map[string]string{"session": s.ID}, nil)
		to := "/"
		if selftest && r.URL.Query().Has("selftest") {
			to = "/?selftest=1"
		}
		http.Redirect(w, r, to, http.StatusSeeOther)
	})
	if selftest {
		// Test hook for `make ui-e2e`: the page reports what it rendered. Off unless the
		// daemon runs with SOL_UI_SELFTEST=1; the body lands in <dir>/selftest.json.
		// ?shot=1 pages hold their load event on this, so a screenshot waits for the data.
		mx.HandleFunc("GET /__hold", func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(1500 * time.Millisecond)
			w.Header().Set("Content-Type", "text/css")
		})
		mx.HandleFunc("POST /__selftest", func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
			_ = fsutil.WriteFileAtomic(filepath.Join(d.dir, "selftest.json"), b, 0o600)
		})
	}
	mx.HandleFunc("GET /_sol/open", d.openApp)
	mx.HandleFunc("POST /_sol/signin", d.signIn)
	mx.HandleFunc("GET /wallpapers/{id}", d.serveWallpaper)
	mx.HandleFunc("/admin/", d.browserAdmin)
	files := http.FileServerFS(home.FS)
	mx.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if d.Host.ServeShared(w, r) {
			return
		}
		if strings.HasPrefix(r.URL.Path, "/_sol/") {
			sol.WriteError(w, sol.Errorf("not_found", "no such path"))
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			sol.WriteError(w, sol.Errorf("bad_request", "the Sol app's files are read-only"))
			return
		}
		// Unknown paths without an extension are the app's own routes: serve the shell.
		if p := path.Clean(r.URL.Path); p != "/" && path.Ext(p) == "" {
			if _, err := fs.Stat(home.FS, strings.TrimPrefix(p, "/")); err != nil {
				r2 := r.Clone(r.Context())
				r2.URL.Path = "/"
				if !d.hasSession(r) {
					d.autoLogin(w, r)
				}
				files.ServeHTTP(w, r2)
				return
			}
		}
		// A page load without a session: let the daemon's own user straight in.
		if p := path.Clean(r.URL.Path); (p == "/" || path.Ext(p) == "") && !d.hasSession(r) {
			d.autoLogin(w, r)
		}
		w.Header().Set("Cache-Control", "no-cache")
		files.ServeHTTP(w, r)
	})
	return mx
}

// browserAdmin serves /admin/v1/... to a browser: Origin first, then the session cookie.
func (d *Daemon) browserAdmin(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if !sameUIOrigin(r) {
		d.log.Info("refused a cross-origin admin request", "origin", r.Header.Get("Origin"))
		sol.WriteError(w, sol.Errorf("forbidden", "cross-origin request"))
		return
	}
	id := ""
	if c, err := r.Cookie(SessionCookie); err == nil && c.Value != "" {
		if s, ok, renewed := d.browser.check(c.Value, func() bool { return d.sameUser(r) }); ok {
			id = s.ID
			if renewed {
				setSessionCookie(w, c.Value, int(SessionTTL/time.Second))
			}
		}
	}
	if id == "" {
		// No live session (never had one, expired, or revoked): the daemon's own user is let in.
		var ok bool
		if id, ok = d.autoLogin(w, r); !ok {
			setSessionCookie(w, "", -1)
			sol.WriteError(w, sol.Errorf("unauthorized", "This browser isn't signed in to Sol. Open Sol from your app menu, or run `sol open`"))
			return
		}
	}
	r = r.WithContext(context.WithValue(r.Context(), browserKey{}, id))
	d.admin.ServeHTTP(w, r)
}

// friendlyClient turns a User-Agent into the name Settings shows: "Sol app on Android" for the
// Android app (its agent carries a SolAndroid/ token), else "<Browser> on <OS>" ("Firefox on
// Linux"), and "Browser" when nothing is recognised. Brave and other Chromium forks say they
// are Chrome, so they show as Chrome. A string that is not a User-Agent (already a friendly
// name) comes back unchanged.
func friendlyClient(ua string) string {
	if strings.Contains(ua, "SolAndroid/") {
		return "Sol app on Android"
	}
	if ua == "" {
		return "Browser"
	}
	if !strings.Contains(ua, "/") && !strings.Contains(ua, "Mozilla") {
		return truncate(ua, 60)
	}
	browser := ""
	for _, b := range []struct{ token, name string }{
		{"Edg/", "Edge"}, {"EdgA/", "Edge"}, {"EdgiOS/", "Edge"}, {"OPR/", "Opera"},
		{"Firefox/", "Firefox"}, {"FxiOS/", "Firefox"}, {"CriOS/", "Chrome"}, {"Chrome/", "Chrome"},
		{"Chromium/", "Chromium"}, {"Safari/", "Safari"},
	} {
		if strings.Contains(ua, b.token) {
			browser = b.name
			break
		}
	}
	osName := ""
	for _, o := range []struct{ token, name string }{
		{"Android", "Android"}, {"iPhone", "iOS"}, {"iPad", "iOS"}, {"iPod", "iOS"}, {"Windows", "Windows"},
		{"Macintosh", "macOS"}, {"Mac OS X", "macOS"}, {"CrOS", "ChromeOS"}, {"Linux", "Linux"}, {"X11", "Linux"},
	} {
		if strings.Contains(ua, o.token) {
			osName = o.name
			break
		}
	}
	switch {
	case browser != "" && osName != "":
		return browser + " on " + osName
	case browser != "":
		return browser
	case osName != "":
		return "Browser on " + osName
	}
	return "Browser"
}

func truncate(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
