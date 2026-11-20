package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestScanWatchSeesEdits(t *testing.T) {
	dir := t.TempDir()
	ch, stop, err := scanWatch(10*time.Millisecond, 40*time.Millisecond, dir)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()
	expect := func(what string, want bool) {
		t.Helper()
		select {
		case <-ch:
			if !want {
				t.Fatalf("%s: unexpected change", what)
			}
		case <-time.After(time.Second):
			if want {
				t.Fatalf("%s: no change reported", what)
			}
		}
	}
	f := filepath.Join(dir, "a.css")
	os.WriteFile(f, []byte("x"), 0o644)
	expect("create", true)
	os.WriteFile(f, []byte("xy"), 0o644)
	expect("edit", true)
	os.Remove(f)
	expect("delete", true)
	select { // idle: backed off, no spurious events
	case <-ch:
		t.Fatal("idle event")
	case <-time.After(150 * time.Millisecond):
	}
}

// TestSyncedThemeArrivesLive: Files on this device writes .sol/theme when another device
// changes it; the daemon turns that into a live theme event (no Sol call involved).
func TestSyncedThemeArrivesLive(t *testing.T) {
	d := testDaemon(t)
	sol := filepath.Join(d.cfg.Files, ".sol")
	_ = os.MkdirAll(filepath.Join(sol, "themes"), 0o755)
	cur, _, _ := d.events.since(0)
	start := uint64(0)
	if n := len(cur); n > 0 {
		start = cur[n-1].n
	}
	if err := os.WriteFile(filepath.Join(sol, "themes", "paper.css"), []byte("/* @theme Paper */"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sol, "theme"), []byte("paper\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		evs, wake, _ := d.events.since(start)
		for _, e := range evs {
			if e.kind == EvTheme && strings.Contains(string(e.data), `"paper"`) {
				return
			}
		}
		select {
		case <-wake:
		case <-time.After(100 * time.Millisecond):
		}
	}
	t.Fatal("no theme event after the synced files changed")
}
