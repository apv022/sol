package daemon

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/apv022/sol/sdk/go/sol"
)

func TestWallpaperSetting(t *testing.T) {
	for _, ok := range []string{"", "dusk", "aurora", "ember", "slate", "custom:0123456789abcdef"} {
		if err := checkThemeSetting("wallpaper", ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"sunset", "custom:", "custom:../../etc", "custom:XYZ", "Dusk"} {
		if err := checkThemeSetting("wallpaper", bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	d := testDaemon(t)
	if _, err := d.UpdateThemeSettings(map[string]string{"wallpaper": "ember"}); err != nil || d.themeSettings()["wallpaper"] != "ember" {
		t.Fatalf("%v", err)
	}
	if !strings.Contains(string(d.themePayload()), `"wallpaper":"ember"`) {
		t.Fatalf("event: %s", d.themePayload())
	}
}

var tinyPNG = append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...)

func TestWallpapers(t *testing.T) {
	d := testDaemon(t)
	root := "localhost:" + strconv.Itoa(d.UIPort())
	origin := "http://" + root
	cookie := login(t, d)
	post := func(ctype string, body []byte) *http.Response {
		return uiReq(t, d, "POST", root, "/admin/v1/wallpapers", origin, cookie, string(body), map[string]string{"Content-Type": ctype})
	}
	// Wrong type, a lie about the type, and too big: refused.
	for name, r := range map[string]*http.Response{
		"gif":     post("image/gif", tinyPNG),
		"lying":   post("image/jpeg", tinyPNG),
		"svg":     post("image/svg+xml", []byte("<svg/>")),
		"toolong": post("image/png", append(tinyPNG, make([]byte, MaxWallpaperBytes)...)),
	} {
		if r.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: %d", name, r.StatusCode)
		}
	}
	r := post("image/png", tinyPNG)
	var w WallpaperInfo
	if err := json.NewDecoder(r.Body).Decode(&w); err != nil || r.StatusCode != 200 || !wallpaperIDRe.MatchString(w.ID) || w.Setting != "custom:"+w.ID {
		t.Fatalf("%d %+v %v", r.StatusCode, w, err)
	}
	p := filepath.Join(d.cfg.Files, ".sol", "wallpapers", w.ID+".png")
	if fi, err := os.Stat(p); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("file: %v %v", fi, err)
	}
	// Listed, and chosen through the setting.
	var list []WallpaperInfo
	if err := adminClient(t, d).Do(context.Background(), "GET", "/admin/v1/wallpapers", nil, &list); err != nil || len(list) != 1 || list[0].ID != w.ID {
		t.Fatalf("%v %+v", err, list)
	}
	if _, err := d.UpdateThemeSettings(map[string]string{"wallpaper": w.Setting}); err != nil {
		t.Fatal(err)
	}
	// Served with its type and nosniff, to a session only, and never by a path trick.
	g := uiReq(t, d, "GET", root, w.URL, "", cookie, "", nil)
	b, _ := io.ReadAll(g.Body)
	if g.StatusCode != 200 || g.Header.Get("Content-Type") != "image/png" || g.Header.Get("X-Content-Type-Options") != "nosniff" || len(b) != len(tinyPNG) {
		t.Fatalf("serve: %d %v", g.StatusCode, g.Header)
	}
	if g := uiReq(t, d, "GET", root, w.URL, "", "", "", nil); g.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no session: %d", g.StatusCode)
	}
	for _, bad := range []string{"/wallpapers/..%2f..%2fconfig.json", "/wallpapers/" + strings.Repeat("a", 16) + ".png", "/wallpapers/zz"} {
		if g := uiReq(t, d, "GET", root, bad, "", cookie, "", nil); g.StatusCode == 200 {
			t.Fatalf("%s served", bad)
		}
	}
	// Deleting the chosen one resets the setting.
	if r := uiReq(t, d, "DELETE", root, "/admin/v1/wallpapers/"+w.ID, origin, cookie, "", nil); r.StatusCode != 200 {
		t.Fatalf("delete: %d", r.StatusCode)
	}
	if _, err := os.Stat(p); err == nil || d.themeSettings()["wallpaper"] != "" {
		t.Fatalf("still there: %v %q", err, d.themeSettings()["wallpaper"])
	}
	if r := uiReq(t, d, "DELETE", root, "/admin/v1/wallpapers/"+w.ID, origin, cookie, "", nil); r.StatusCode != http.StatusNotFound {
		t.Fatalf("delete twice: %d", r.StatusCode)
	}
}

func TestFriendlyClient(t *testing.T) {
	for ua, want := range map[string]string{
		"Mozilla/5.0 (X11; Linux x86_64; rv:130.0) Gecko/20100101 Firefox/130.0":                                                         "Firefox on Linux",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36":                "Chrome on Windows",
		"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Safari/537.36 Edg/126.0":          "Edge on Windows",
		"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Safari/605.1.15":          "Safari on macOS",
		"Mozilla/5.0 (iPhone; CPU iPhone OS 17_5 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.5 Mobile Safari/604": "Safari on iOS",
		"Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0 Mobile Safari/537.36":              "Chrome on Android",
		"Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 Chrome/126.0 Mobile Safari/537.36 SolAndroid/1.2":                   "Sol app on Android",
		"curl/8.5.0":       "Browser",
		"":                 "Browser",
		"Firefox on Linux": "Firefox on Linux",
	} {
		if got := friendlyClient(ua); got != want {
			t.Errorf("%q: %q, want %q", ua, got, want)
		}
	}
}

func TestSessionClientIsFriendly(t *testing.T) {
	d := testDaemonAs(t, func(_, _ netip.AddrPort) (bool, error) { return true, nil })
	root := "localhost:" + strconv.Itoa(d.UIPort())
	r := uiReq(t, d, "GET", root, "/admin/v1/status", "http://"+root, "", "", map[string]string{"User-Agent": "Mozilla/5.0 (X11; Linux x86_64; rv:130.0) Gecko/20100101 Firefox/130.0"})
	if r.StatusCode != 200 {
		t.Fatal(r.StatusCode)
	}
	ss := d.browser.list("")
	if len(ss) != 1 || ss[0].Client != "Firefox on Linux" {
		t.Fatalf("%+v", ss)
	}
}

// TestSignOutStaysSignedOut: logout must not be undone by same-user auto-login; the locked
// screen's Sign in button starts a session again, only for the daemon's own OS user.
func TestSignOutStaysSignedOut(t *testing.T) {
	same := true
	d := testDaemonAs(t, func(_, _ netip.AddrPort) (bool, error) { return same, nil })
	root := "localhost:" + strconv.Itoa(d.UIPort())
	origin := "http://" + root
	cookieOf := func(r *http.Response, name string) *http.Cookie {
		for _, c := range r.Cookies() {
			if c.Name == name {
				return c
			}
		}
		return nil
	}
	r := uiReq(t, d, "GET", root, "/admin/v1/status", origin, "", "", nil)
	tok := cookieOf(r, SessionCookie)
	if r.StatusCode != 200 || tok == nil || tok.Value == "" {
		t.Fatalf("auto-login: %d", r.StatusCode)
	}
	hdr := map[string]string{"Cookie": SessionCookie + "=" + tok.Value}
	out := uiReq(t, d, "POST", root, "/admin/v1/logout", origin, "", "", hdr)
	so := cookieOf(out, SignedOutCookie)
	if out.StatusCode != 200 || so == nil || so.Value != "1" || !so.HttpOnly || so.SameSite != http.SameSiteStrictMode || so.MaxAge < 365*24*3600 {
		t.Fatalf("logout: %d %+v", out.StatusCode, so)
	}
	marker := map[string]string{"Cookie": SignedOutCookie + "=1"}
	// With the marker: no auto-login on the API or on a page load.
	if r := uiReq(t, d, "GET", root, "/admin/v1/status", origin, "", "", marker); r.StatusCode != http.StatusUnauthorized || cookieOf(r, SessionCookie) != nil && cookieOf(r, SessionCookie).Value != "" {
		t.Fatalf("auto-login after logout: %d", r.StatusCode)
	}
	if r := uiReq(t, d, "GET", root, "/", "", "", "", marker); cookieOf(r, SessionCookie) != nil && cookieOf(r, SessionCookie).Value != "" {
		t.Fatal("page load signed in after logout")
	}
	// Without the marker (another browser) the same user is still let in.
	if r := uiReq(t, d, "GET", root, "/admin/v1/status", origin, "", "", nil); r.StatusCode != 200 {
		t.Fatalf("other browser: %d", r.StatusCode)
	}
	// Sign in: needs the Origin, and the daemon's own OS user.
	if r := uiReq(t, d, "POST", root, "/_sol/signin", "", "", "", marker); r.StatusCode != http.StatusForbidden {
		t.Fatalf("signin without Origin: %d", r.StatusCode)
	}
	if r := uiReq(t, d, "POST", root, "/_sol/signin", "http://evil.example", "", "", marker); r.StatusCode != http.StatusForbidden {
		t.Fatalf("signin from another origin: %d", r.StatusCode)
	}
	same = false
	if r := uiReq(t, d, "POST", root, "/_sol/signin", origin, "", "", marker); r.StatusCode != http.StatusForbidden || cookieOf(r, SessionCookie) != nil {
		t.Fatalf("signin by another user: %d", r.StatusCode)
	}
	same = true
	r = uiReq(t, d, "POST", root, "/_sol/signin", origin, "", "", marker)
	if c := cookieOf(r, SessionCookie); r.StatusCode != 200 || c == nil || c.Value == "" {
		t.Fatalf("signin: %d", r.StatusCode)
	}
	if c := cookieOf(r, SignedOutCookie); c == nil || c.MaxAge >= 0 {
		t.Fatalf("signed-out marker not cleared: %+v", c)
	}
}

func TestStorageHonest(t *testing.T) {
	d := testDaemon(t)
	// Wait for the measurement the start kicked off, then ask again after it is stale.
	var st *Storage
	for i := 0; i < 100; i++ {
		if st = d.Status().Storage; st != nil && st.Measured {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if st == nil || !st.Measured || st.Free == 0 || st.Total < st.Free {
		t.Fatalf("%+v", st)
	}
	// Not re-measured within the minute: a new file is not counted yet.
	before := st.Used
	if err := os.WriteFile(filepath.Join(d.cfg.Files, "a.bin"), make([]byte, 5000), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := d.Status().Storage.Used; got != before {
		t.Fatalf("walked on a status call: %d vs %d", got, before)
	}
	old := folderRefresh
	folderRefresh = 0
	t.Cleanup(func() { folderRefresh = old })
	d.Status() // starts a refresh in the background
	for i := 0; i < 100 && d.Status().Storage.Used < before+5000; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if got := d.Status().Storage.Used; got < before+5000 {
		t.Fatalf("not refreshed: %d vs %d", got, before)
	}
}

func TestAgentNewFromBrowser(t *testing.T) {
	d := testDaemon(t)
	root := "localhost:" + strconv.Itoa(d.UIPort())
	origin := "http://" + root
	cookie := login(t, d)
	r := uiReq(t, d, "POST", root, "/admin/v1/agents/new", origin, cookie, `{"name":"bot","scopes":["sol.theme.list"],"days":7}`, nil)
	var out struct {
		Agent   AgentInfo `json:"agent"`
		KeyFile string    `json:"key_file"`
		Path    string    `json:"path"`
	}
	if err := json.NewDecoder(r.Body).Decode(&out); err != nil || r.StatusCode != 200 || out.Agent.Name != "bot" || out.KeyFile == "" {
		t.Fatalf("%d %v %+v", r.StatusCode, err, out)
	}
	// The key file is where `sol agent new` keeps it, 0600, and loads as an agent key that works.
	if fi, err := os.Stat(out.Path); err != nil || fi.Mode().Perm() != 0o600 || out.Path != filepath.Join(d.Dir(), "agents", "bot.key") {
		t.Fatalf("%v %v", fi, err)
	}
	k, err := LoadAgentKey(out.Path)
	if err != nil {
		t.Fatal(err)
	}
	var shown AgentKey
	if json.Unmarshal([]byte(out.KeyFile), &shown) != nil || string(shown.Key) != string(k.Key) {
		t.Fatal("returned key differs from the file")
	}
	ac, err := ConnectAgent(d.Dir(), out.Path)
	if err != nil {
		t.Fatal(err)
	}
	var apps []ActionApp
	if err := ac.Do(context.Background(), "GET", "/agent/v1/actions", nil, &apps); err != nil {
		t.Fatal(err)
	}
	// Listed afterwards without the key; a second one under the same name conflicts; bad input is refused.
	var list []AgentInfo
	if err := adminClient(t, d).Do(context.Background(), "GET", "/admin/v1/agents", nil, &list); err != nil || len(list) != 1 || strings.Contains(string(mustJSON(list)), "\"key\"") {
		t.Fatalf("%v %+v", err, list)
	}
	for body, want := range map[string]int{
		`{"name":"bot","scopes":["*"]}`:      http.StatusConflict,
		`{"name":"Bad Name","scopes":["*"]}`: http.StatusBadRequest,
		`{"name":"x","scopes":[]}`:           http.StatusBadRequest,
	} {
		if r := uiReq(t, d, "POST", root, "/admin/v1/agents/new", origin, cookie, body, nil); r.StatusCode != want {
			t.Errorf("%s: %d, want %d", body, r.StatusCode, want)
		}
	}
	_ = sol.ErrBadRequest
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }
