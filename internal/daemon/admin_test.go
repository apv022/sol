package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/apv022/sol/sdk/go/sol"
)

// uiReq sends a request to d's UI server with the given Host, Origin and cookie.
func uiReq(t *testing.T, d *Daemon, method, host, path, origin, cookie, body string, hdr map[string]string) *http.Response {
	t.Helper()
	req, err := http.NewRequest(method, "http://127.0.0.1:"+strconv.Itoa(d.UIPort())+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	if cookie != "" {
		req.AddCookie(&http.Cookie{Name: SessionCookie, Value: cookie})
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func adminClient(t *testing.T, d *Daemon) *Client {
	t.Helper()
	c, err := Connect(d.Dir())
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// login gets a login code through the CLI's API and redeems it; it returns the cookie value.
func login(t *testing.T, d *Daemon) string {
	t.Helper()
	var lc struct{ Code, URL string }
	if err := adminClient(t, d).Do(context.Background(), "POST", "/admin/v1/login-code", nil, &lc); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(lc.URL)
	if u.Path != "/login" || u.Query().Get("code") != lc.Code || len(lc.Code) != 64 {
		t.Fatalf("login url %q", lc.URL)
	}
	resp := uiReq(t, d, "GET", u.Host, "/login?code="+lc.Code, "", "", "", nil)
	if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/" {
		t.Fatalf("login: %d", resp.StatusCode)
	}
	for _, c := range resp.Cookies() {
		if c.Name == SessionCookie {
			if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.Domain != "" || c.Path != "/" || c.MaxAge < 29*24*3600 {
				t.Fatalf("cookie attributes: %+v", c)
			}
			// Again: a code works once.
			if r := uiReq(t, d, "GET", u.Host, "/login?code="+lc.Code, "", "", "", nil); r.StatusCode != http.StatusForbidden {
				t.Fatalf("code reused: %d", r.StatusCode)
			}
			return c.Value
		}
	}
	t.Fatal("no session cookie")
	return ""
}

func TestBrowserLogin(t *testing.T) {
	d := testDaemon(t)
	root := "localhost:" + strconv.Itoa(d.UIPort())
	origin := "http://" + root
	cookie := login(t, d)

	// The cookie, the right Origin and Host: served.
	if r := uiReq(t, d, "GET", root, "/admin/v1/status", origin, cookie, "", nil); r.StatusCode != 200 {
		t.Fatalf("status: %d", r.StatusCode)
	}
	// 127.0.0.1 is the UI root too, with its own origin.
	ip := "127.0.0.1:" + strconv.Itoa(d.UIPort())
	if r := uiReq(t, d, "GET", ip, "/admin/v1/status", "http://"+ip, cookie, "", nil); r.StatusCode != 200 {
		t.Fatalf("127.0.0.1: %d", r.StatusCode)
	}
	// A same-origin GET without Origin (EventSource, fetch) passes; a cross-site one doesn't.
	if r := uiReq(t, d, "GET", root, "/admin/v1/status", "", cookie, "", map[string]string{"Sec-Fetch-Site": "same-origin"}); r.StatusCode != 200 {
		t.Fatalf("same-origin GET: %d", r.StatusCode)
	}
	if r := uiReq(t, d, "GET", root, "/admin/v1/status", "", cookie, "", map[string]string{"Sec-Fetch-Site": "same-site"}); r.StatusCode != http.StatusForbidden {
		t.Fatalf("same-site GET: %d", r.StatusCode)
	}
	// A state change without Origin, from a foreign origin, or from an app origin: refused.
	for _, o := range []string{"", "http://evil.example", "http://files.localhost:" + strconv.Itoa(d.UIPort()), "http://localhost:1"} {
		if r := uiReq(t, d, "POST", root, "/admin/v1/network-changed", o, cookie, "{}", nil); r.StatusCode != http.StatusForbidden {
			t.Fatalf("origin %q: %d", o, r.StatusCode)
		}
	}
	// An app origin's Host never reaches the admin API, even with the cookie.
	appHost := "files.localhost:" + strconv.Itoa(d.UIPort())
	if r := uiReq(t, d, "GET", appHost, "/admin/v1/status", "http://"+appHost, cookie, "", nil); r.StatusCode == 200 {
		t.Fatal("app origin reached the admin API")
	}
	// A wrong Host (DNS rebinding) is refused by the router.
	if r := uiReq(t, d, "GET", "evil.example:"+strconv.Itoa(d.UIPort()), "/admin/v1/status", "", cookie, "", nil); r.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("rebinding: %d", r.StatusCode)
	}
	// No or a bad cookie: 401.
	for _, c := range []string{"", "nope"} {
		if r := uiReq(t, d, "GET", root, "/admin/v1/status", origin, c, "", nil); r.StatusCode != http.StatusUnauthorized {
			t.Fatalf("cookie %q: %d", c, r.StatusCode)
		}
	}
	// A browser session cannot mint login codes.
	if r := uiReq(t, d, "POST", root, "/admin/v1/login-code", origin, cookie, "", nil); r.StatusCode != http.StatusForbidden {
		t.Fatalf("login-code from a browser: %d", r.StatusCode)
	}
	// The session file holds hashes, never the cookie.
	if b, _ := readFile(d.Dir(), "browser-sessions.json"); strings.Contains(b, cookie) || !strings.Contains(b, `"hash"`) {
		t.Fatal("cookie stored in clear")
	}
	// Sessions: listed (this one marked), another revoked, then logout.
	other := login(t, d)
	var list []BrowserSession
	if err := adminClient(t, d).Do(context.Background(), "GET", "/admin/v1/sessions", nil, &list); err != nil || len(list) != 2 {
		t.Fatalf("%v %v", list, err)
	}
	r := uiReq(t, d, "GET", root, "/admin/v1/sessions", origin, other, "", nil)
	var mine []BrowserSession
	decode(t, r, &mine)
	cur := ""
	for _, s := range mine {
		if s.Current {
			cur = s.ID
		}
	}
	if r := uiReq(t, d, "DELETE", root, "/admin/v1/sessions/"+cur, origin, cookie, "", nil); r.StatusCode != 200 {
		t.Fatalf("revoke: %d", r.StatusCode)
	}
	if r := uiReq(t, d, "GET", root, "/admin/v1/status", origin, other, "", nil); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("revoked session: %d", r.StatusCode)
	}
	if r := uiReq(t, d, "DELETE", root, "/admin/v1/sessions/"+cur, origin, cookie, "", nil); r.StatusCode != http.StatusNotFound {
		t.Fatalf("revoke twice: %d", r.StatusCode)
	}
	if r := uiReq(t, d, "POST", root, "/admin/v1/logout", origin, cookie, "", nil); r.StatusCode != 200 {
		t.Fatalf("logout: %d", r.StatusCode)
	}
	if r := uiReq(t, d, "GET", root, "/admin/v1/status", origin, cookie, "", nil); r.StatusCode != http.StatusUnauthorized {
		t.Fatalf("after logout: %d", r.StatusCode)
	}
	if err := adminClient(t, d).Do(context.Background(), "POST", "/admin/v1/logout", nil, nil); !errors.Is(err, sol.ErrBadRequest) {
		t.Fatalf("CLI logout: %v", err)
	}
}

func TestLoginCodeExpires(t *testing.T) {
	old := LoginCodeTTL
	LoginCodeTTL = time.Millisecond
	defer func() { LoginCodeTTL = old }()
	d := testDaemon(t)
	var lc struct{ Code string }
	if err := adminClient(t, d).Do(context.Background(), "POST", "/admin/v1/login-code", nil, &lc); err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond)
	r := uiReq(t, d, "GET", "localhost:"+strconv.Itoa(d.UIPort()), "/login?code="+lc.Code, "", "", "", nil)
	if r.StatusCode != http.StatusForbidden || len(r.Cookies()) != 0 {
		t.Fatalf("expired code: %d", r.StatusCode)
	}
}

func TestSolAppServed(t *testing.T) {
	d := testDaemon(t)
	root := "localhost:" + strconv.Itoa(d.UIPort())
	for _, p := range []string{"/", "/app.js", "/kit/kit.css", "/sol.js", "/_sol/theme.css", "/people"} {
		r := uiReq(t, d, "GET", root, p, "", "", "", nil)
		if r.StatusCode != 200 || r.Header.Get("Content-Security-Policy") == "" {
			t.Fatalf("GET %s: %d %v", p, r.StatusCode, r.Header)
		}
	}
	if r := uiReq(t, d, "GET", root, "/nope.js", "", "", "", nil); r.StatusCode != 404 {
		t.Fatal(r.StatusCode)
	}
}

// TestAdminEndpoints runs each endpoint's happy path and one failure through the CLI client.
func TestAdminEndpoints(t *testing.T) {
	d := testDaemon(t)
	c := adminClient(t, d)
	ctx := context.Background()
	do := func(method, path string, in, out any) error { return c.Do(ctx, method, path, in, out) }
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	fail := func(want error, err error) {
		t.Helper()
		if !errors.Is(err, want) {
			t.Fatalf("want %v, got %v", want, err)
		}
	}
	var st Status
	must(do("GET", "/admin/v1/status", nil, &st))
	if st.Me.Person.Name != "A" || len(st.Transports) != 1 || st.Transports[0].State == "" || st.Storage == nil || st.Storage.Total == 0 {
		t.Fatalf("%+v", st)
	}
	fail(sol.ErrNotFound, do("GET", "/admin/v1/nope", nil, nil))

	var people []PersonInfo
	must(do("GET", "/admin/v1/people", nil, &people))
	fail(sol.ErrNotFound, do("PATCH", "/admin/v1/people/"+st.Me.Device.ID, map[string]string{"petname": "x"}, nil))
	fail(sol.ErrNotFound, do("DELETE", "/admin/v1/people/"+st.Me.Device.ID, nil, nil))

	var devs []DeviceInfo
	must(do("GET", "/admin/v1/devices", nil, &devs))
	if len(devs) != 1 || !devs[0].This || !devs[0].Admin || !devs[0].Online {
		t.Fatalf("%+v", devs)
	}
	var dv DeviceInfo
	must(do("PATCH", "/admin/v1/devices/"+devs[0].ID, map[string]any{"name": "desk", "anchor": true}, &dv))
	if dv.Name != "desk" || !dv.Anchor {
		t.Fatalf("%+v", dv)
	}
	fail(sol.ErrNotImplemented, do("PATCH", "/admin/v1/devices/"+devs[0].ID, map[string]any{"admin": false}, nil))
	fail(sol.ErrBadRequest, do("DELETE", "/admin/v1/devices/"+devs[0].ID, nil, nil)) // cannot remove itself

	var inv struct {
		Link    string
		Expires time.Time
	}
	must(do("POST", "/admin/v1/invite", map[string]bool{"join": false}, &inv))
	if !strings.HasPrefix(inv.Link, "sol://pair") || time.Until(inv.Expires) < 9*time.Minute {
		t.Fatalf("%+v", inv)
	}
	fail(sol.ErrBadRequest, do("POST", "/admin/v1/pair", map[string]string{"link": "nonsense"}, nil))

	var apps []AppInfo
	must(do("GET", "/admin/v1/apps", nil, &apps))
	fail(sol.ErrNotFound, do("POST", "/admin/v1/apps/nope/remove", nil, nil))
	fail(sol.ErrNotFound, do("POST", "/admin/v1/apps/nope/restart", nil, nil))
	fail(sol.ErrBadRequest, do("POST", "/admin/v1/apps/install", map[string]string{"dir": t.TempDir()}, nil))

	var svc Service
	must(do("POST", "/admin/v1/services", Service{Name: "web", Port: 8080, HTTP: true}, &svc))
	fail(sol.ErrConflict, do("POST", "/admin/v1/services", Service{Name: "web", Port: 8081}, nil))
	fail(sol.ErrBadRequest, do("POST", "/admin/v1/services", Service{Name: "Bad Name", Port: 1}, nil))
	must(do("PATCH", "/admin/v1/services/web", map[string]int{"port": 8082}, &svc))
	if svc.Port != 8082 || !svc.HTTP {
		t.Fatalf("%+v", svc)
	}
	fail(sol.ErrBadRequest, do("PATCH", "/admin/v1/services/web", map[string][]string{"people": {"nobody"}}, nil))
	var svcs []Service
	must(do("GET", "/admin/v1/services", nil, &svcs))
	if len(svcs) != 1 {
		t.Fatal(svcs)
	}
	must(do("DELETE", "/admin/v1/services/web", nil, nil))
	fail(sol.ErrNotFound, do("DELETE", "/admin/v1/services/web", nil, nil))

	var tuns []TunnelInfo
	must(do("GET", "/admin/v1/tunnels", nil, &tuns))
	fail(sol.ErrNotFound, do("POST", "/admin/v1/tunnels", map[string]any{"person": "nobody", "service": "web"}, nil))
	fail(sol.ErrNotFound, do("DELETE", "/admin/v1/tunnels/nope", nil, nil))

	var items []sol.InboxItem
	must(do("GET", "/admin/v1/inbox", nil, &items))
	for _, v := range []string{"approve", "deny", "dismiss"} {
		fail(sol.ErrNotFound, do("POST", "/admin/v1/inbox/nope/"+v, nil, nil))
	}

	var th struct {
		Name     string
		Settings map[string]string
	}
	must(do("POST", "/admin/v1/theme", map[string]string{"name": "graphite"}, nil))
	must(do("GET", "/admin/v1/theme", nil, &th))
	if th.Name != "graphite" {
		t.Fatalf("%+v", th)
	}
	fail(sol.ErrBadRequest, do("POST", "/admin/v1/theme", map[string]string{"name": "../x"}, nil))
	must(do("POST", "/admin/v1/theme/settings", map[string]string{"scheme": "dark"}, nil))
	fail(sol.ErrBadRequest, do("POST", "/admin/v1/theme/settings", map[string]string{"scheme": "purple"}, nil))
	var ts ThemeState
	must(do("GET", "/admin/v1/themes", nil, &ts))

	var bk BackupState
	must(do("GET", "/admin/v1/backup", nil, &bk))
	if len(bk.Words) != 24 || bk.Confirmed {
		t.Fatalf("%+v", bk)
	}
	must(do("POST", "/admin/v1/backup/confirm", nil, nil))
	must(do("GET", "/admin/v1/backup", nil, &bk))
	if len(bk.Words) != 0 || !bk.Confirmed {
		t.Fatalf("%+v", bk)
	}
	fail(sol.ErrNotFound, do("POST", "/admin/v1/backup/confirm", nil, nil))

	var ags []AgentInfo
	must(do("GET", "/admin/v1/agents", nil, &ags))
	fail(sol.ErrNotFound, do("POST", "/admin/v1/agents/nope/revoke", nil, nil))
	must(do("POST", "/admin/v1/network-changed", map[string]string{"reason": "test"}, nil))

	// Every change went to the activity log as sol.*.
	me, _ := d.me()
	es, err := d.Store.Read(me+"/sol/activity", 1, 1000)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, e := range es {
		var row sol.ActivityEntry
		if jsonUnmarshal(e.Body, &row) == nil {
			seen[row.Action] = true
		}
	}
	for _, a := range []string{"sol.services.add", "sol.services.remove", "sol.theme.set", "sol.devices.update", "sol.backup.confirm"} {
		if !seen[a] {
			t.Fatalf("no %s in the activity log: %v", a, seen)
		}
	}
}

// TestAdminEventsResume: events arrive live and Last-Event-ID resumes after the last one seen.
func TestAdminEventsResume(t *testing.T) {
	d := testDaemon(t)
	c := adminClient(t, d)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	open := func(last string) (*bufio.Reader, func()) {
		req, _ := http.NewRequestWithContext(ctx, "GET", c.base+"/admin/v1/events", nil)
		req.Header.Set("Authorization", "Bearer "+c.token)
		if last != "" {
			req.Header.Set("Last-Event-ID", last)
		}
		resp, err := c.hc.Do(req)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("%v %v", resp, err)
		}
		return bufio.NewReader(resp.Body), func() { resp.Body.Close() }
	}
	next := func(br *bufio.Reader) (id, kind, data string) {
		for {
			line, err := br.ReadString('\n')
			if err != nil {
				t.Fatal(err)
			}
			line = strings.TrimRight(line, "\n")
			switch {
			case strings.HasPrefix(line, "id: "):
				id = line[4:]
			case strings.HasPrefix(line, "event: "):
				kind = line[7:]
			case strings.HasPrefix(line, "data: "):
				data = line[6:]
			case line == "" && kind != "":
				return
			}
		}
	}
	br, stop := open("")
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(c.Do(ctx, "POST", "/admin/v1/services", Service{Name: "one", Port: 1}, nil))
	id1, kind, data := next(br)
	if kind != EvService || !strings.Contains(data, `"one"`) {
		t.Fatalf("%s %s", kind, data)
	}
	stop()
	// Missed while disconnected: a service and a theme change.
	must(c.Do(ctx, "POST", "/admin/v1/services", Service{Name: "two", Port: 2}, nil))
	must(c.Do(ctx, "POST", "/admin/v1/theme", map[string]string{"name": "paper"}, nil))
	br, stop = open(id1)
	defer stop()
	if _, kind, data := next(br); kind != EvService || !strings.Contains(data, `"two"`) {
		t.Fatalf("resume: %s %s", kind, data)
	}
	if _, kind, data := next(br); kind != EvTheme || !strings.Contains(data, `"paper"`) {
		t.Fatalf("resume: %s %s", kind, data)
	}
	// A bad id is an error, not silence.
	req, _ := http.NewRequestWithContext(ctx, "GET", c.base+"/admin/v1/events", nil)
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Last-Event-ID", "garbage")
	resp, err := c.hc.Do(req)
	if err != nil || resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("bad id: %v %v", resp, err)
	}
	resp.Body.Close()
}

func decode(t *testing.T, r *http.Response, v any) {
	t.Helper()
	b, _ := io.ReadAll(r.Body)
	if err := jsonUnmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", b, err)
	}
}

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

func readFile(dir, name string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, name))
	return string(b), err
}
