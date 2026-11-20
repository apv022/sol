package e2e

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/apv022/sol/internal/builtins"
	sold "github.com/apv022/sol/internal/daemon"
)

// ui sends a request to an app origin of d's UI server ("" app: the launcher).
func ui(t *testing.T, d *sold.Daemon, app, method, path, origin, body string, hdr map[string]string) (int, string) {
	t.Helper()
	host := "localhost:" + strconv.Itoa(d.UIPort())
	if app != "" {
		host = app + "." + host
	}
	req, err := http.NewRequest(method, "http://127.0.0.1:"+strconv.Itoa(d.UIPort())+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Host = host
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func origin(d *sold.Daemon, app string) string {
	return "http://" + app + ".localhost:" + strconv.Itoa(d.UIPort())
}

// call runs an action through an app's /_sol/ bridge, as that app's own UI would.
func call(t *testing.T, d *sold.Daemon, action, args string) (int, string) {
	t.Helper()
	app, _, _ := strings.Cut(action, ".")
	return ui(t, d, app, "POST", "/_sol/v1/actions/call", origin(d, app), `{"action":"`+action+`","args":`+args+`}`, nil)
}

// TestFilesAcrossDevicesAndPeople runs the builtin Files app on three real daemons (Alice's
// laptop and phone, and Bob) against the real app API, through the real UI router.
func TestFilesAcrossDevicesAndPeople(t *testing.T) {
	bi := builtins.All()
	laptop := startNode(t, sold.InitOptions{Name: "Alice", Device: "laptop"}, bi)
	phone := joinNode(t, laptop, bi)
	bob := startNode(t, sold.InitOptions{Name: "Bob", Device: "desktop"}, bi)
	pairNodes(t, laptop, bob)
	waitSession(t, laptop, phone)
	waitSession(t, laptop, bob)
	eventually(t, "Files running everywhere", func() bool {
		for _, d := range []*sold.Daemon{laptop, phone, bob} {
			if code, _ := call(t, d, "files.list", `{}`); code != 200 {
				return false
			}
		}
		return true
	})

	// The launcher lists the apps with their origins.
	if code, page := ui(t, laptop, "", "GET", "/", "", "", nil); code != 200 || !strings.Contains(page, origin(laptop, "files")+"/") {
		t.Fatalf("launcher: %d %s", code, page)
	}
	// An app origin serves the UI with the CSP, the kit, sol.js and the theme.
	for _, p := range []string{"/", "/kit/kit.css", "/sol.js", "/_sol/theme.css"} {
		if code, _ := ui(t, laptop, "files", "GET", p, "", "", nil); code != 200 {
			t.Fatalf("GET %s: %d", p, code)
		}
	}

	// 1. A file written into the laptop's folder appears in the phone's.
	lroot := filepath.Join(laptop.Dir(), "SolDrive")
	proot := filepath.Join(phone.Dir(), "SolDrive")
	if err := os.MkdirAll(filepath.Join(lroot, "Photos"), 0o755); err != nil {
		t.Fatal(err)
	}
	content := "the quick brown fox jumps over the lazy dog"
	if err := os.WriteFile(filepath.Join(lroot, "Photos", "fox.txt"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the file on the phone", func() bool {
		b, err := os.ReadFile(filepath.Join(proot, "Photos", "fox.txt"))
		return err == nil && string(b) == content
	})

	// 2. Alice shares the folder with Bob; it appears for him.
	if code, out := call(t, laptop, "files.share", `{"path":"Photos","people":["bob"],"mode":"read"}`); code != 200 {
		t.Fatalf("share: %d %s", code, out)
	}
	var entry struct {
		Entries []struct {
			Name, Blob string
		}
	}
	eventually(t, "the shared folder at Bob", func() bool {
		code, out := call(t, bob, "files.list", `{"path":"Shared/alice/Photos"}`)
		return code == 200 && json.Unmarshal([]byte(out), &entry) == nil && len(entry.Entries) == 1 && entry.Entries[0].Name == "fox.txt"
	})

	// 3. Bob streams the shared file by hash, with Range, through the real API.
	code, part := ui(t, bob, "files", "GET", "/_sol/v1/blobs/"+entry.Entries[0].Blob, "", "", map[string]string{"Range": "bytes=4-8"})
	if code != http.StatusPartialContent || part != "quick" {
		t.Fatalf("range: %d %q", code, part)
	}

	// 4. A call through the bridge from another app's origin is refused, and so is a
	// state-changing call that proves no origin at all.
	if code, _ := ui(t, laptop, "files", "POST", "/_sol/v1/actions/call", origin(laptop, "notes"),
		`{"action":"files.delete","args":{"paths":["Photos"]}}`, nil); code != http.StatusForbidden {
		t.Fatalf("cross-origin call: %d", code)
	}
	if code, _ := ui(t, laptop, "files", "POST", "/_sol/v1/actions/call", "",
		`{"action":"files.delete","args":{"paths":["Photos"]}}`, map[string]string{"Sec-Fetch-Site": "cross-site"}); code != http.StatusForbidden {
		t.Fatalf("cross-site call: %d", code)
	}
	if code, _ := ui(t, laptop, "files", "POST", "/_sol/v1/actions/call", "", `{"action":"files.list"}`, nil); code != http.StatusForbidden {
		t.Fatalf("origin-less POST: %d", code)
	}
	// The bridge never reaches the admin API, and /_sol/actions is not passed to the app.
	if code, _ := ui(t, laptop, "files", "GET", "/_sol/admin/v1/status", origin(laptop, "files"), "", nil); code != http.StatusNotFound {
		t.Fatalf("admin through bridge: %d", code)
	}
	if code, _ := ui(t, laptop, "files", "POST", "/_sol/actions/files.delete", origin(laptop, "files"), `{"paths":["Photos"]}`, nil); code != http.StatusNotFound {
		t.Fatalf("actions through UI: %d", code)
	}
	// Unknown hosts are refused (DNS rebinding).
	req, _ := http.NewRequest("GET", "http://127.0.0.1:"+strconv.Itoa(laptop.UIPort())+"/", nil)
	req.Host = "evil.example:" + strconv.Itoa(laptop.UIPort())
	if resp, err := http.DefaultClient.Do(req); err != nil || resp.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("foreign host: %v %v", resp, err)
	}
	if _, err := os.Stat(filepath.Join(lroot, "Photos", "fox.txt")); err != nil {
		t.Fatal("the refused delete ran:", err)
	}
}

// TestAppInstallRemove installs an app from a directory, lists it, and removes it.
func TestAppInstallRemove(t *testing.T) {
	d := startNode(t, sold.InitOptions{Name: "Alice", Device: "laptop"}, builtins.All())
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "app.json"), []byte(`{"id":"hello","name":"Hello","version":"1","exec":"hello.sh","scopes":["actions"]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Host.Install(src); err == nil {
		t.Fatal("installed an app whose exec is missing")
	}
	if err := os.WriteFile(filepath.Join(src, "hello.sh"), []byte("#!/bin/sh\nsleep 3600\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Host.Install(src); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, a := range d.Apps() {
		ids[a.ID] = true
	}
	if !ids["hello"] || !ids["files"] || !ids["notes"] {
		t.Fatalf("%+v", d.Apps())
	}
	if err := d.Host.Remove("hello"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(d.Dir(), "apps", "hello")); !os.IsNotExist(err) {
		t.Fatal("app directory left behind")
	}
	if _, ok := d.Host.App("hello"); ok {
		t.Fatal("removed app still registered")
	}
	// Bad manifests are refused.
	for _, bad := range []string{
		`{"id":"sol","version":"1","exec":"hello.sh"}`,
		`{"id":"x1","version":"1","exec":"../hello.sh"}`,
		`{"id":"x2","version":"1","exec":"builtin:nope"}`,
		`{"id":"x3","version":"1","exec":"hello.sh","folders":[{"path":"../etc","mode":"read"}]}`,
	} {
		_ = os.WriteFile(filepath.Join(src, "app.json"), []byte(bad), 0o600)
		if _, err := d.Host.Install(src); err == nil {
			t.Errorf("installed %s", bad)
		}
	}
}
