package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"image"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/apv022/sol/sdk/go/sol"
)

const extCert = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// extGet calls the external API with an app token and returns the status and body.
func extGet(t *testing.T, method, url, token, body string) (int, string) {
	t.Helper()
	req, _ := http.NewRequest(method, url, strings.NewReader(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// TestExternalApp: an external app registered through the CLI's endpoint gets a token that
// works on the loopback API within its scopes only, keeps working (same URL, same token) after
// a restart, and stops at removal. A browser session cannot register one.
func TestExternalApp(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{UI: "127.0.0.1:0", Files: filepath.Join(dir, "files"), Transports: []TransportConfig{{Name: "clearnet", Listen: "127.0.0.1:0"}}}
	if err := Init(dir, InitOptions{Name: "A", Device: "d", Config: cfg}); err != nil {
		t.Fatal(err)
	}
	start := func() *Daemon {
		d, err := Start(Options{Dir: dir, InProcessApps: true, Log: slog.New(slog.DiscardHandler),
			SameUser: func(_, _ netip.AddrPort) (bool, error) { return false, nil }})
		if err != nil {
			t.Fatal(err)
		}
		return d
	}
	d := start()
	defer func() { d.Close() }()
	ctx := context.Background()
	c := adminClient(t, d)
	m := sol.Manifest{ID: "music", Name: "Music", Scopes: []string{"people", "actions"},
		Actions: []sol.ActionSpec{{Name: "music.play", Effect: sol.EffectRead, Input: &sol.Schema{Type: "object"}}}}
	var got ExternalApp
	if err := c.Do(ctx, "POST", "/admin/v1/apps/external", map[string]any{"manifest": m, "package": "org.example.music", "cert": extCert}, &got); err != nil {
		t.Fatal(err)
	}
	if got.ID != "music" || len(got.Token) != 64 || !strings.HasPrefix(got.API, "http://127.0.0.1:") {
		t.Fatalf("%+v", got)
	}
	// The token is nowhere on disk.
	_ = filepath.WalkDir(dir, func(p string, e os.DirEntry, err error) error {
		if err == nil && !e.IsDir() {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), got.Token) {
				t.Errorf("token stored in %s", p)
			}
		}
		return nil
	})
	if code, body := extGet(t, "GET", got.API+"/v1/whoami", got.Token, ""); code != 200 || !strings.Contains(body, `"music"`) {
		t.Fatalf("whoami %d %s", code, body)
	}
	if code, _ := extGet(t, "GET", got.API+"/v1/people", got.Token, ""); code != 200 {
		t.Fatalf("people %d", code)
	}
	if code, _ := extGet(t, "GET", got.API+"/v1/feeds", got.Token, ""); code != http.StatusForbidden {
		t.Fatalf("feeds without the scope: %d", code)
	}
	if code, _ := extGet(t, "GET", got.API+"/v1/whoami", "nope", ""); code != http.StatusUnauthorized {
		t.Fatalf("bad token: %d", code)
	}
	// The admin API is not served on the external port, not even with the admin token.
	adminTok, _ := os.ReadFile(filepath.Join(dir, "admin.token"))
	if code, _ := extGet(t, "GET", got.API+"/admin/v1/status", strings.TrimSpace(string(adminTok)), ""); code != http.StatusNotFound {
		t.Fatalf("admin on the external port: %d", code)
	}
	// Its declared actions are honest stubs: Sol cannot call into it.
	call, _ := json.Marshal(sol.ActionCall{Action: "music.play", Args: json.RawMessage(`{}`)})
	if code, body := extGet(t, "POST", got.API+"/v1/actions/call", got.Token, string(call)); !strings.Contains(body, "not_implemented") {
		t.Fatalf("action call %d %s", code, body)
	}
	// Listed as an external app.
	var apps []AppInfo
	if err := c.Do(ctx, "GET", "/admin/v1/apps", nil, &apps); err != nil {
		t.Fatal(err)
	}
	var listed bool
	for _, a := range apps {
		listed = listed || (a.ID == "music" && a.External && a.Package == "org.example.music" && a.URL == "")
	}
	if !listed {
		t.Fatalf("apps %+v", apps)
	}
	// Refusals: another package's id, another cert for the same package, a bad scope.
	for _, b := range []map[string]any{
		{"manifest": sol.Manifest{ID: "music"}, "package": "org.x.y", "cert": extCert},
		{"manifest": sol.Manifest{ID: "music"}, "package": "org.example.music", "cert": strings.Repeat("f", 64)},
		{"manifest": sol.Manifest{ID: "x", Scopes: []string{"everything"}}, "package": "org.x.y", "cert": extCert},
		{"package": "org.x.y", "cert": extCert},
	} {
		if err := c.Do(ctx, "POST", "/admin/v1/apps/external", b, nil); err == nil {
			t.Errorf("%v accepted", b)
		}
	}
	// A browser session must not mint app tokens.
	cookie := login(t, d)
	root := "localhost:" + strconv.Itoa(d.UIPort())
	body, _ := json.Marshal(map[string]any{"manifest": sol.Manifest{ID: "evil"}})
	if r := uiReq(t, d, "POST", root, "/admin/v1/apps/external", "http://"+root, cookie, string(body), nil); r.StatusCode != http.StatusForbidden {
		t.Fatalf("browser registered an app: %d", r.StatusCode)
	}

	// After a restart: same URL, same token.
	d.Close()
	d = start()
	if code, body := extGet(t, "GET", got.API+"/v1/whoami", got.Token, ""); code != 200 {
		t.Fatalf("whoami after restart %d %s", code, body)
	}
	// Removal revokes it.
	if err := adminClient(t, d).Do(ctx, "POST", "/admin/v1/apps/music/remove", nil, nil); err != nil {
		t.Fatal(err)
	}
	if code, _ := extGet(t, "GET", got.API+"/v1/whoami", got.Token, ""); code != http.StatusUnauthorized {
		t.Fatalf("whoami after remove: %d", code)
	}
}

// TestExternalAppIcon: the icon an external app connects with is listed as a URL on the UI
// root and served there to a signed-in browser only, as image/png; a bad icon refuses the
// connect, a path in the manifest's own icon never reaches the list, and removal ends it.
func TestExternalAppIcon(t *testing.T) {
	d := testDaemon(t)
	ctx := context.Background()
	c := adminClient(t, d)
	var buf bytes.Buffer
	_ = png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 192, 192)))
	icon := buf.Bytes()
	if err := c.Do(ctx, "POST", "/admin/v1/apps/external", map[string]any{"manifest": sol.Manifest{ID: "raypass", Name: "Raypass"},
		"package": "app.raypass", "cert": extCert, "icon_png": []byte("<svg/>")}, nil); err == nil {
		t.Fatal("a non-PNG icon accepted")
	}
	m := sol.Manifest{ID: "raypass", Name: "Raypass", Icon: "/admin/v1/status"}
	if err := c.Do(ctx, "POST", "/admin/v1/apps/external", map[string]any{"manifest": m, "package": "app.raypass", "cert": extCert, "icon_png": icon}, nil); err != nil {
		t.Fatal(err)
	}
	find := func() (AppInfo, bool) {
		var apps []AppInfo
		if err := c.Do(ctx, "GET", "/admin/v1/apps", nil, &apps); err != nil {
			t.Fatal(err)
		}
		for _, a := range apps {
			if a.ID == "raypass" {
				return a, true
			}
		}
		return AppInfo{}, false
	}
	a, ok := find()
	if !ok || !a.External || !strings.HasPrefix(a.Icon, "/app-icons/raypass?v=") {
		t.Fatalf("listed %+v", a)
	}
	root := "localhost:" + strconv.Itoa(d.UIPort())
	if r := uiReq(t, d, "GET", root, a.Icon, "", "", "", nil); r.StatusCode == http.StatusOK {
		t.Fatal("icon served without a session")
	}
	cookie := login(t, d)
	r := uiReq(t, d, "GET", root, a.Icon, "", cookie, "", nil)
	got, _ := io.ReadAll(r.Body)
	if r.StatusCode != 200 || r.Header.Get("Content-Type") != "image/png" || r.Header.Get("X-Content-Type-Options") != "nosniff" || !bytes.Equal(got, icon) {
		t.Fatalf("icon %d %q %d bytes", r.StatusCode, r.Header.Get("Content-Type"), len(got))
	}
	if r := uiReq(t, d, "GET", root, "/app-icons/nope", "", cookie, "", nil); r.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown icon %d", r.StatusCode)
	}
	// Without an icon, a manifest path is dropped (a glyph name would stay).
	if err := c.Do(ctx, "POST", "/admin/v1/apps/external", map[string]any{"manifest": m, "package": "app.raypass", "cert": extCert}, nil); err != nil {
		t.Fatal(err)
	}
	if a, _ := find(); a.Icon != "" {
		t.Fatalf("icon %q", a.Icon)
	}
	if err := c.Do(ctx, "POST", "/admin/v1/apps/raypass/remove", nil, nil); err != nil {
		t.Fatal(err)
	}
	if r := uiReq(t, d, "GET", root, "/app-icons/raypass", "", cookie, "", nil); r.StatusCode != http.StatusNotFound {
		t.Fatalf("icon after remove %d", r.StatusCode)
	}
}

// TestSolAppsOpen: sol.apps.open opens a web app's URL in the browser (dry run says which),
// and answers not_implemented for a connected Android app, which opens from the phone's grid.
func TestSolAppsOpen(t *testing.T) {
	d := testDaemon(t)
	ctx := context.Background()
	c := adminClient(t, d)
	if _, err := d.Host.Attach(&sol.Manifest{ID: "notes", Name: "Notes", Version: "1", Exec: "x", UI: "/", Scopes: []string{}}, "127.0.0.1:1"); err != nil {
		t.Fatal(err)
	}
	if err := c.Do(ctx, "POST", "/admin/v1/apps/external", map[string]any{"manifest": sol.Manifest{ID: "raypass", Name: "Raypass"}, "package": "app.raypass", "cert": extCert}, nil); err != nil {
		t.Fatal(err)
	}
	var opened []string
	old := openURL
	openURL = func(u string) bool { opened = append(opened, u); return true }
	t.Cleanup(func() { openURL = old })
	call := func(args string, dry bool) (map[string]any, error) {
		var out map[string]any
		err := c.Do(ctx, "POST", "/admin/v1/actions/call", sol.ActionCall{Action: "sol.apps.open", Args: json.RawMessage(args), DryRun: dry}, &out)
		return out, err
	}
	out, err := call(`{"id":"notes"}`, true)
	if err != nil || out["dry_run"] != true || !strings.HasPrefix(out["url"].(string), "http://notes.localhost:") || len(opened) != 0 {
		t.Fatalf("dry run %v %v %v", out, err, opened)
	}
	if out, err = call(`{"id":"Notes"}`, false); err != nil || out["opened"] != true || len(opened) != 1 || opened[0] != out["url"] {
		t.Fatalf("open %v %v %v", out, err, opened)
	}
	if _, err = call(`{"id":"raypass"}`, false); err == nil || !strings.Contains(err.Error(), "phone") || !errors.Is(err, sol.Errorf("not_implemented", "")) {
		t.Fatalf("external: %v", err)
	}
	if _, err = call(`{"id":"nope"}`, false); err == nil {
		t.Fatal("unknown app opened")
	}
	if _, err = call(`{}`, false); err == nil {
		t.Fatal("no id accepted")
	}
	if len(opened) != 1 {
		t.Fatalf("opened %v", opened)
	}
}
