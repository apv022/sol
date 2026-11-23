package calendar

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// waitFile polls (test only) until the file holds want, or fails.
func waitFile(t *testing.T, p, want string) string {
	t.Helper()
	for i := 0; i < 100; i++ {
		if b, err := os.ReadFile(p); err == nil && strings.Contains(string(b), want) {
			return string(b)
		}
		time.Sleep(50 * time.Millisecond)
	}
	b, _ := os.ReadFile(p)
	t.Fatalf("%s never held %q; has %q", p, want, b)
	return ""
}

func TestSubscriptionFiles(t *testing.T) {
	e := newEnv(t)
	data := t.TempDir()
	a := start(t, e.alice.Devices[0], e.clk, data, false)
	call(t, a, "calendar.create_calendar", map[string]any{"name": "Home"})
	call(t, a, "calendar.add", map[string]any{"calendar": "Home", "title": "Dentist", "start": "2026-11-24T10:00"})

	home := filepath.Join(data, "subscribe", "Home.ics")
	all := filepath.Join(data, "subscribe", "all.ics")
	waitFile(t, home, "SUMMARY:Dentist")
	waitFile(t, all, "SUMMARY:Dentist")
	if fi, _ := os.Stat(home); fi.Mode().Perm()&0o222 != 0 {
		t.Errorf("subscription file is writable: %v", fi.Mode())
	}

	// An add rewrites it.
	call(t, a, "calendar.add", map[string]any{"calendar": "Home", "title": "Haircut", "start": "2026-11-25T09:00"})
	got := waitFile(t, home, "SUMMARY:Haircut")

	// It parses back with the importer.
	evs, name, err := parseICS(got)
	if err != nil || len(evs) != 2 || name != "Home" {
		t.Fatalf("parse back: %d events, %q, %v", len(evs), name, err)
	}

	// The action names the files and their file:// URLs.
	out := call(t, a, "calendar.subscription", nil)
	files, _ := out["files"].([]any)
	if len(files) != 2 {
		t.Fatalf("files: %v", out)
	}
	for _, f := range files {
		m := f.(map[string]any)
		if !strings.HasPrefix(m["url"].(string), "file://") || !strings.HasSuffix(m["path"].(string), ".ics") {
			t.Errorf("file %v", m)
		}
	}

	// A deleted calendar's file goes away.
	call(t, a, "calendar.delete_calendar", map[string]any{"calendar": "Home"})
	for i := 0; i < 100; i++ {
		if _, err := os.Stat(home); err != nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("Home.ics still there after the calendar was deleted")
}
