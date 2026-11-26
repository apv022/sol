package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"

	"github.com/apv022/sol/internal/apphost"
	"github.com/apv022/sol/internal/daemon"
)

// The Android shell's `sol nearby` commands: the ad (with group credentials only on stdin),
// a match request on stdin, interface addresses and a state report, all JSON; usage errors
// for a missing "-"; not_implemented without the shell.
func TestNearbyCmd(t *testing.T) {
	dir := t.TempDir()
	if err := daemon.Init(dir, daemon.InitOptions{Name: "A", Device: "phone", Config: daemon.Config{UI: "127.0.0.1:0",
		Files: filepath.Join(dir, "SolDrive"), Transports: []daemon.TransportConfig{{Name: "clearnet", Listen: "127.0.0.1:0"}}}}); err != nil {
		t.Fatal(err)
	}
	d, err := daemon.Start(daemon.Options{Dir: dir, Builtins: map[string]apphost.Builtin{}, InProcessApps: true, APIOverTCP: true,
		Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	c, err := daemon.Connect(dir)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	run := func(cmd, stdin string, args ...string) (string, error) {
		var out bytes.Buffer
		err := nearbyCmd(ctx, c, cmd, args, strings.NewReader(stdin), &out)
		return out.String(), err
	}
	if _, err := run("nearby ad", ""); err == nil || !strings.Contains(err.Error(), "Android") {
		t.Fatalf("without the shell: %v", err)
	}
	t.Setenv("SOL_ANDROID_SHELL", "1")
	out, err := run("nearby ad", "")
	var ad daemon.WifiDirectAd
	if err != nil || json.Unmarshal([]byte(out), &ad) != nil || ad.TXT["v"] != "1" {
		t.Fatalf("ad: %q %v", out, err)
	}
	if out, err = run("nearby ad", `{"group":{"ssid":"DIRECT-AAAAAAAA","pass":"AAAAAAAAAAAAAAAAAAAA"}}`, "-"); err != nil {
		t.Fatalf("ad with a group: %q %v", out, err)
	}
	if _, err := run("nearby match", `{}`); err == nil {
		t.Fatal("match without -")
	}
	if out, err = run("nearby match", `{"state":"none","seen":[{"id":"x","txt":{"v":"1"}}]}`, "-"); err != nil || !strings.Contains(out, `"role":"none"`) {
		t.Fatalf("match: %q %v", out, err)
	}
	if _, err = run("nearby interfaces", "", "192.168.49.1"); err != nil {
		t.Fatal(err)
	}
	if _, err = run("nearby interfaces", "", "not-an-ip"); err == nil {
		t.Fatal("bad address accepted")
	}
	if _, err := d.SetWifiDirect(true); err != nil {
		t.Fatal(err)
	}
	if out, err = run("nearby report", "", "unsupported", "needs", "Android 10"); err != nil || !strings.Contains(out, "needs Android 10") {
		t.Fatalf("report: %q %v", out, err)
	}
}
