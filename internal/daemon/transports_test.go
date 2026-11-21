package daemon

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	solruntime "github.com/apv022/sol/internal/runtime"
	"github.com/apv022/sol/sdk/go/sol"
)

// A new setup turns on WireGuard, TLS direct and Tor together.
func TestDefaultTransports(t *testing.T) {
	dir := t.TempDir()
	if err := Init(dir, InitOptions{Name: "A", Device: "d", Config: Config{UI: "127.0.0.1:0"}}); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tc := range cfg.Transports {
		names = append(names, tc.Name)
		if tc.Name != "tor" && !strings.HasPrefix(tc.Listen, "0.0.0.0:") {
			t.Fatalf("%s listens on %q", tc.Name, tc.Listen)
		}
	}
	if !slices.Equal(names, []string{"wg", "direct", "tor"}) {
		t.Fatalf("default transports %v", names)
	}
}

func endpointKinds(d *Daemon) []string {
	var out []string
	for _, e := range d.Keyring.Endpoints().Endpoints {
		out = append(out, e.Transport)
	}
	return out
}

// Turning a transport on and off works at run time, is kept in config.json and changes the
// endpoint record; the last transport can't be turned off. The same is reachable as sol
// actions (with a dry run).
func TestTransportsEnableDisable(t *testing.T) {
	d := testDaemon(t)
	c := adminClient(t, d)
	ctx := context.Background()
	var ts []TransportState
	if err := c.Do(ctx, "POST", "/admin/v1/transports/direct", map[string]bool{"enabled": true}, &ts); err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(endpointKinds(d), "direct") {
		t.Fatalf("no direct endpoint after enabling it: %v", endpointKinds(d))
	}
	var out json.RawMessage
	if err := c.Do(ctx, "POST", "/admin/v1/actions/call", sol.ActionCall{Action: "sol.transports.disable",
		Args: json.RawMessage(`{"name":"clearnet"}`), DryRun: true}, &out); err != nil || !strings.Contains(string(out), "dry_run") {
		t.Fatalf("%s %v", out, err)
	}
	if !slices.Contains(endpointKinds(d), "clearnet") {
		t.Fatal("a dry run disabled the transport")
	}
	if err := c.Do(ctx, "POST", "/admin/v1/transports/clearnet", map[string]bool{"enabled": false}, &ts); err != nil {
		t.Fatal(err)
	}
	if got := endpointKinds(d); !slices.Equal(got, []string{"direct"}) {
		t.Fatalf("endpoints after disabling clearnet: %v", got)
	}
	cfg, _ := LoadConfig(d.Dir())
	if len(cfg.Transports) != 1 || cfg.Transports[0].Name != "direct" {
		t.Fatalf("config.json: %+v", cfg.Transports)
	}
	err := c.Do(ctx, "POST", "/admin/v1/actions/call", sol.ActionCall{Action: "sol.transports.disable", Args: json.RawMessage(`{"name":"direct"}`)}, &out)
	if err == nil || !strings.Contains(err.Error(), "only transport") {
		t.Fatalf("turning off the last transport: %v", err)
	}
	var st Status
	if err := c.Do(ctx, "GET", "/admin/v1/status", nil, &st); err != nil {
		t.Fatal(err)
	}
	on := map[string]bool{}
	for _, tr := range st.Transports {
		on[tr.Name] = tr.Enabled
	}
	if !on["direct"] || on["wg"] || on["tor"] || len(st.Transports) != 3 {
		t.Fatalf("status transports %+v", st.Transports)
	}
}

// A tor that never comes up (here: a program that only sleeps) does not hold up Start: the
// daemon is up on its other transports at once, and tor shows as starting.
func TestTorDoesNotBlockStart(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses a shell script as tor")
	}
	dir := t.TempDir()
	script := filepath.Join(t.TempDir(), "tor")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 120\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := Config{UI: "127.0.0.1:0", Files: filepath.Join(dir, "files"),
		Transports: []TransportConfig{{Name: "clearnet", Listen: "127.0.0.1:0"}, {Name: "tor"}}}
	if err := Init(dir, InitOptions{Name: "A", Device: "d", Config: cfg}); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	d, err := Start(Options{Dir: dir, InProcessApps: true, APIOverTCP: true, Log: slog.New(slog.DiscardHandler),
		TorProvider: &solruntime.Exec{Path: script}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if took := time.Since(start); took > 10*time.Second {
		t.Fatalf("Start took %v waiting for tor", took)
	}
	if got := endpointKinds(d); !slices.Equal(got, []string{"clearnet"}) {
		t.Fatalf("endpoints %v", got)
	}
	for _, tr := range d.Transports() {
		if tr.Name == "tor" && (!tr.Enabled || tr.State != "starting") {
			t.Fatalf("tor %+v", tr)
		}
	}
}

// The config's tor_bin (sol init --tor-bin) runs tor, and SOL_TOR_BIN (what the Android app
// sets) wins over it.
func TestTorBinFromConfigAndEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses shell scripts as tor")
	}
	bin := t.TempDir()
	script := func(name string) (string, string) {
		p, marker := filepath.Join(bin, name), filepath.Join(bin, name+".ran")
		body := "#!/bin/sh\n[ \"$1\" = --version ] && { echo 'Tor version 0.4.9.16.'; exit 0; }\n: > " + marker + "\nexec sleep 120\n"
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		return p, marker
	}
	cfgTor, cfgRan := script("cfg-tor")
	envTor, envRan := script("libtor.so")
	run := func(env string) {
		t.Helper()
		t.Setenv(solruntime.TorBinEnv, env)
		dir := t.TempDir()
		cfg := Config{UI: "127.0.0.1:0", Files: filepath.Join(dir, "files"), TorBin: cfgTor,
			Transports: []TransportConfig{{Name: "clearnet", Listen: "127.0.0.1:0"}, {Name: "tor"}}}
		if err := Init(dir, InitOptions{Name: "A", Device: "d", Config: cfg}); err != nil {
			t.Fatal(err)
		}
		d, err := Start(Options{Dir: dir, InProcessApps: true, APIOverTCP: true, Log: slog.New(slog.DiscardHandler)})
		if err != nil {
			t.Fatal(err)
		}
		defer d.Close()
		want := cfgRan
		if env != "" {
			want = envRan
		}
		deadline := time.Now().Add(10 * time.Second)
		for {
			if _, err := os.Stat(want); err == nil {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("%s never ran", want)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	run("")
	if _, err := os.Stat(envRan); err == nil {
		t.Fatal("SOL_TOR_BIN's tor ran without being set")
	}
	_ = os.Remove(cfgRan)
	run(envTor)
	if _, err := os.Stat(cfgRan); err == nil {
		t.Fatal("tor_bin ran although SOL_TOR_BIN was set")
	}
}
