package daemon

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/apv022/sol/internal/apphost"
)

func manifest(id, exec, desc string) []byte {
	return []byte(`{"id":"` + id + `","name":"` + id + `","exec":"` + exec + `","description":"` + desc + `","version":"1","scopes":[]}`)
}

// TestSyncBuiltins: an upgrade adds builtins new in this version, refreshes changed builtin
// manifests, keeps a builtin the person removed removed, and never touches a non-builtin app.
func TestSyncBuiltins(t *testing.T) {
	dir := t.TempDir()
	read := func(id string) string {
		b, err := os.ReadFile(filepath.Join(dir, "apps", id, "app.json"))
		if err != nil {
			return ""
		}
		return string(b)
	}
	v1 := map[string]apphost.Builtin{"files": {Manifest: manifest("files", "builtin:files", "v1")}}
	if err := syncBuiltins(dir, v1); err != nil {
		t.Fatal(err)
	}
	// Someone's own app under an id a later version makes a builtin.
	own := manifest("calendar", "bin/cal", "mine")
	if err := os.MkdirAll(filepath.Join(dir, "apps", "calendar"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "apps", "calendar", "app.json"), own, 0o600); err != nil {
		t.Fatal(err)
	}
	v2 := map[string]apphost.Builtin{
		"files":    {Manifest: manifest("files", "builtin:files", "v2")},
		"tasks":    {Manifest: manifest("tasks", "builtin:tasks", "v2")},
		"calendar": {Manifest: manifest("calendar", "builtin:calendar", "v2")},
	}
	if err := syncBuiltins(dir, v2); err != nil {
		t.Fatal(err)
	}
	if read("files") != string(v2["files"].Manifest) {
		t.Errorf("files not refreshed: %s", read("files"))
	}
	if read("tasks") != string(v2["tasks"].Manifest) {
		t.Errorf("tasks not added: %q", read("tasks"))
	}
	if read("calendar") != string(own) {
		t.Errorf("a non-builtin app was overwritten: %s", read("calendar"))
	}
	// The person removes Tasks (apphost.Host.Remove leaves the marker); the next start keeps it out.
	if err := os.RemoveAll(filepath.Join(dir, "apps", "tasks")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(apphost.RemovedMarker(filepath.Join(dir, "apps"), "tasks"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := syncBuiltins(dir, v2); err != nil {
		t.Fatal(err)
	}
	if read("tasks") != "" {
		t.Error("a removed builtin came back")
	}
}
