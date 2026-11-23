package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// syncBuf is a bytes.Buffer safe to read while streamEvents writes to it.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) String() string              { s.mu.Lock(); defer s.mu.Unlock(); return s.b.String() }

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(5 * time.Second); time.Now().Before(end); time.Sleep(10 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestStreamEvents: SSE parsing, the bearer token, resume with Last-Event-ID after the
// daemon drops the stream, and retrying while the daemon is not running yet.
func TestStreamEvents(t *testing.T) {
	old := eventsBackoff
	eventsBackoff.min, eventsBackoff.max = 5*time.Millisecond, 20*time.Millisecond
	t.Cleanup(func() { eventsBackoff = old })

	var mu sync.Mutex
	var lasts []string
	conns := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/admin/v1/events" || r.Header.Get("Authorization") != "Bearer tok" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		mu.Lock()
		conns++
		n := conns
		lasts = append(lasts, r.Header.Get("Last-Event-ID"))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, ": ok\n\n")
		if n == 1 {
			fmt.Fprint(w, "id: e.1\nevent: inbox\ndata: {\"id\":\"i1\",\"kind\":\"approval\"}\n\n")
			fmt.Fprint(w, ": beat\n\n")
			fmt.Fprint(w, "id: e.2\nevent: app\ndata: {\"id\":\"files\"}\n\n")
			return // the daemon goes away
		}
		fmt.Fprint(w, "id: e.3\nevent: inbox\ndata: {\"id\":\"i1\",\"removed\":true}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer srv.Close()

	dir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	var out syncBuf
	done := make(chan error, 1)
	go func() { done <- streamEvents(ctx, dir, true, &out) }()
	time.Sleep(30 * time.Millisecond) // not running yet: it keeps retrying
	if err := os.WriteFile(filepath.Join(dir, "admin.token"), []byte("tok\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "api.url"), []byte(srv.URL+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "three events", func() bool { return strings.Count(out.String(), "\n") >= 3 })
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	var evs []streamEvent
	for _, l := range lines {
		var ev streamEvent
		if err := json.Unmarshal([]byte(l), &ev); err != nil {
			t.Fatalf("line %q: %v", l, err)
		}
		evs = append(evs, ev)
	}
	if len(evs) != 3 || evs[0].ID != "e.1" || evs[0].Type != "inbox" || evs[1].Type != "app" || evs[2].ID != "e.3" ||
		!strings.Contains(string(evs[2].Data), `"removed":true`) {
		t.Fatalf("events: %s", out.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(lasts) < 2 || lasts[0] != "" || lasts[1] != "e.2" {
		t.Fatalf("Last-Event-ID per connection: %q", lasts)
	}
}

// TestEventsCommand runs `sol events --json` against a real daemon and sees an event it emits.
func TestEventsCommand(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the binary")
	}
	bin := solBin(t)
	dir := filepath.Join(t.TempDir(), "d")
	solRun(t, bin, dir, "init", "--name", "Test", "--device", "t", "--ui", freeAddr(t), "--listen", "127.0.0.1:0")
	t.Cleanup(func() { _ = exec.Command(bin, "stop", "--dir", dir).Run() })
	solRun(t, bin, dir, "start")

	var out syncBuf
	ev := exec.Command(bin, "events", "--json", "--dir", dir)
	ev.Stdout = &out
	if err := ev.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ev.Process.Kill(); _ = ev.Wait() })
	time.Sleep(300 * time.Millisecond) // connected: the stream starts from now
	// A theme change is an admin event every daemon emits.
	solRun(t, bin, dir, "theme", "settings", "--accent", "#123456")
	waitFor(t, "a theme event", func() bool { return strings.Contains(out.String(), `"type":"theme"`) })
	line := strings.SplitN(strings.TrimSpace(out.String()), "\n", 2)[0]
	var e streamEvent
	if err := json.Unmarshal([]byte(line), &e); err != nil || e.ID == "" || len(e.Data) == 0 {
		t.Fatalf("event line %q: %v", line, err)
	}
}
