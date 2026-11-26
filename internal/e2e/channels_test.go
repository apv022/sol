package e2e

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apv022/sol/internal/channels"
	sold "github.com/apv022/sol/internal/daemon"
	"github.com/apv022/sol/internal/faultnet"
	"github.com/apv022/sol/internal/identity"
	solruntime "github.com/apv022/sol/internal/runtime"
	"github.com/apv022/sol/sdk/go/sol"
)

// startChanNode starts a device on clearnet and Tor (run by p) whose channel mirrors are
// fetched with mirror; with join it joins admin's person.
func startChanNode(t *testing.T, p solruntime.Provider, mirror *http.Client, o sold.InitOptions, admin *sold.Daemon) *sold.Daemon {
	t.Helper()
	dir := t.TempDir()
	o.Config = sold.Config{UI: "127.0.0.1:0", Files: filepath.Join(dir, "SolDrive"),
		Transports: []sold.TransportConfig{{Name: "clearnet", Listen: "127.0.0.1:0"}, {Name: "tor"}}}
	if err := sold.Init(dir, o); err != nil {
		t.Fatal(err)
	}
	d, err := sold.Start(sold.Options{Dir: dir, InProcessApps: true, TorProvider: p, MirrorClient: mirror,
		Log: testLog(o.Device), SameUser: func(_, _ netip.AddrPort) (bool, error) { return true, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	if admin != nil {
		link, err := admin.Invite(true)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := d.Pair(context.Background(), link); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

type tarFile struct{ name, body string }

// appTar is a sol-app artifact of app demo at version, plus extra entries.
func appTar(t *testing.T, version string, extra ...tarFile) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	w := tar.NewWriter(zw)
	files := append([]tarFile{
		{"app.json", `{"id":"demo","name":"Demo","version":"` + version + `","exec":"run"}`},
		{"run", "#!/bin/sh\nexit 0\n"},
		{"VERSION", version},
	}, extra...)
	for _, f := range files {
		if err := w.WriteHeader(&tar.Header{Name: f.name, Mode: 0o755, Size: int64(len(f.body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(f.body))
	}
	w.Close()
	zw.Close()
	return buf.Bytes()
}

// act runs a built-in action through d's admin API.
func act(d *sold.Daemon, action string, args any) (json.RawMessage, error) {
	c, err := sold.Connect(d.Dir())
	if err != nil {
		return nil, err
	}
	var b []byte
	if args != nil {
		b, _ = json.Marshal(args)
	}
	var out json.RawMessage
	err = c.Do(context.Background(), "POST", "/admin/v1/actions/call", sol.ActionCall{Action: action, Args: b}, &out)
	return out, err
}

func errCode(err error) string {
	var se *sol.Error
	if errors.As(err, &se) {
		return se.Code
	}
	return ""
}

func available(d *sold.Daemon, app string) (sold.AvailableApp, bool) {
	out, err := act(d, "sol.apps.available", nil)
	if err != nil {
		return sold.AvailableApp{}, false
	}
	var as []sold.AvailableApp
	json.Unmarshal(out, &as)
	for _, a := range as {
		if a.ID == app {
			return a, true
		}
	}
	return sold.AvailableApp{}, false
}

func version(d *sold.Daemon, app string) string {
	if m, ok := d.Host.Manifest(app); ok {
		return m.Version
	}
	return ""
}

// TestChannels: a developer publishes app releases on their node's public onion; Alice follows
// the channel from her laptop, which tells her phone through the internal feed while one of
// them holds the Tor stream; she installs the app, an update is pushed and installed
// automatically, rolled back, then asked for in the Inbox. Forged, older, tampered and
// path-traversing releases are refused, an Android app gets a notice, and a mirror serves the
// bytes once the onion is gone.
func TestChannels(t *testing.T) {
	sold.ChannelRetryMin, sold.ChannelRetryMax = 50*time.Millisecond, 500*time.Millisecond
	fake := faultnet.NewFakeTor(t.TempDir())
	p := fake.Provider()

	var mu sync.Mutex
	mirrored := map[string][]byte{}
	mirror := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		b, ok := mirrored[r.URL.Path]
		mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write(b)
	}))
	defer mirror.Close()

	dev := startTorNode(t, p, sold.InitOptions{Name: "Dev", Device: "ci"})
	laptop := startChanNode(t, p, mirror.Client(), sold.InitOptions{Name: "Alice", Device: "laptop"}, nil)
	phone := startChanNode(t, p, mirror.Client(), sold.InitOptions{Join: true, Device: "phone"}, laptop)
	waitSession(t, laptop, phone)
	waitSession(t, phone, laptop)

	key, err := channels.NewKey("demo")
	if err != nil {
		t.Fatal(err)
	}
	devc, err := sold.Connect(dev.Dir())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	seq := uint64(0)
	// publish signs a release of app with one artifact (uploaded to the dev node unless
	// mirrorOnly) and hosts it.
	publish := func(app, ver, platform, kind string, art []byte, mirrorOnly bool) identity.Release {
		t.Helper()
		seq++
		sum := sha256.Sum256(art)
		a := identity.Artifact{Platform: platform, Kind: kind, SHA256: sum[:], Size: uint64(len(art))}
		if mirrorOnly {
			path := "/" + app + "-" + ver
			mu.Lock()
			mirrored[path] = art
			mu.Unlock()
			a.Mirrors = []string{mirror.URL + path}
		} else if err := devc.Upload(ctx, "/admin/v1/channels/blobs", bytes.NewReader(art), nil); err != nil {
			t.Fatal(err)
		}
		r := identity.Release{Seq: seq, App: app, Version: ver, Time: uint64(time.Now().Unix()), Notes: "Notes of " + ver, Artifacts: []identity.Artifact{a}}
		if err := r.Sign(key.Private()); err != nil {
			t.Fatal(err)
		}
		if err := devc.Do(ctx, "POST", "/admin/v1/channels/hosted", map[string]any{"name": "demo", "record": r.Marshal()}, nil); err != nil {
			t.Fatal(err)
		}
		return r
	}
	plat := sold.ThisPlatform
	publish("demo", "0.1.0", plat, identity.KindSolApp, appTar(t, "0.1.0"), false)
	var link string
	eventually(t, "the channel's onion", func() bool {
		hs := dev.Hosted()
		if len(hs) == 1 && hs[0].Link != "" {
			link = hs[0].Link
		}
		return link != ""
	})

	// Following is one device's action; the phone follows too, through the feed.
	if _, err := act(laptop, "sol.apps.follow", map[string]string{"link": link}); err != nil {
		t.Fatal(err)
	}
	for _, d := range []*sold.Daemon{laptop, phone} {
		eventually(t, "the release reaches "+d.DeviceID()[:6], func() bool { a, ok := available(d, "demo"); return ok && a.Version == "0.1.0" && a.Installable })
	}
	streams := func() (live, via int) {
		for _, d := range []*sold.Daemon{laptop, phone} {
			for _, c := range d.Channels() {
				switch c.Stream {
				case "live":
					live++
				case "via my other devices":
					via++
				}
			}
		}
		return
	}
	eventually(t, "exactly one device holding the stream", func() bool { l, v := streams(); return l == 1 && v == 1 })

	// Install: pinned, policy auto.
	if _, err := act(laptop, "sol.apps.install", map[string]string{"id": "demo"}); err != nil {
		t.Fatal(err)
	}
	if v := version(laptop, "demo"); v != "0.1.0" {
		t.Fatalf("installed %q", v)
	}
	if _, err := act(laptop, "sol.apps.install", map[string]string{"id": "demo"}); errCode(err) != "conflict" {
		t.Fatalf("installing twice: %v", err)
	}

	// An update is pushed and installed by itself.
	publish("demo", "0.2.0", plat, identity.KindSolApp, appTar(t, "0.2.0"), false)
	eventually(t, "the automatic update", func() bool { return version(laptop, "demo") == "0.2.0" })
	if version(phone, "demo") != "" {
		t.Fatal("the phone installed an app it never installed")
	}
	dirOf := func() string {
		b, _ := os.ReadFile(filepath.Join(laptop.Dir(), "apps", "demo", "VERSION"))
		return string(b)
	}
	if dirOf() != "0.2.0" {
		t.Fatalf("app directory holds %q", dirOf())
	}

	// Rollback puts 0.1.0 back, and the update rolled back from is not reapplied.
	if _, err := act(laptop, "sol.apps.rollback", map[string]string{"id": "demo"}); err != nil {
		t.Fatal(err)
	}
	if version(laptop, "demo") != "0.1.0" || dirOf() != "0.1.0" {
		t.Fatalf("after rollback: %s / %s", version(laptop, "demo"), dirOf())
	}
	if out, err := act(laptop, "sol.apps.update", map[string]string{"id": "demo"}); err != nil || !strings.Contains(string(out), "up_to_date") {
		t.Fatalf("update after rollback: %s %v", out, err)
	}

	// Ask: an Inbox approval; approving installs.
	if _, err := act(laptop, "sol.apps.auto", map[string]string{"id": "demo", "policy": "ask"}); err != nil {
		t.Fatal(err)
	}
	publish("demo", "0.3.0", plat, identity.KindSolApp, appTar(t, "0.3.0"), false)
	var item sol.InboxItem
	eventually(t, "the update approval", func() bool {
		for _, it := range laptop.API.InboxItems() {
			if it.Kind == "approval" && it.Title == "Demo 0.3.0 is ready" && it.Answer == "" {
				item = it
				return true
			}
		}
		return false
	})
	if item.Body != "Notes of 0.3.0" || version(laptop, "demo") != "0.1.0" {
		t.Fatalf("item %+v, version %s", item, version(laptop, "demo"))
	}
	if _, err := laptop.API.Answer(ctx, item.ID, true); err != nil {
		t.Fatal(err)
	}
	if version(laptop, "demo") != "0.3.0" {
		t.Fatalf("approved, still %s", version(laptop, "demo"))
	}
	if _, err := act(laptop, "sol.apps.auto", map[string]string{"id": "demo", "policy": "off"}); err != nil {
		t.Fatal(err)
	}

	// Refused records: signed by another key while naming the channel, an older seq, and a
	// channel not followed.
	importRec := func(r identity.Release) error {
		c, _ := sold.Connect(laptop.Dir())
		return c.Do(ctx, "POST", "/admin/v1/channels/records", map[string]any{"record": r.Marshal()}, nil)
	}
	_, mallory, _ := ed25519.GenerateKey(nil)
	sum := sha256.Sum256([]byte("evil"))
	forged := identity.Release{Seq: 99, App: "demo", Version: "6.6.6", Artifacts: []identity.Artifact{{Platform: plat, Kind: identity.KindSolApp, SHA256: sum[:], Size: 4}}}
	forged.Sign(mallory)
	forged.Channel = key.Public() // claims to be the channel; the signature says otherwise
	if err := importRec(forged); errCode(err) != "forbidden" {
		t.Fatalf("forged record: %v", err)
	}
	other := forged
	other.Sign(mallory) // a well-signed record of a channel I don't follow
	if err := importRec(other); errCode(err) != "forbidden" {
		t.Fatalf("unfollowed channel: %v", err)
	}
	old := identity.Release{Seq: 1, App: "demo", Version: "0.0.9", Artifacts: forged.Artifacts}
	old.Sign(key.Private())
	if err := importRec(old); errCode(err) != "conflict" {
		t.Fatalf("older seq: %v", err)
	}
	if a, _ := available(laptop, "demo"); a.Version != "0.3.0" {
		t.Fatalf("available after refusals: %+v", a)
	}

	// A tampered blob is refused, and nothing changes.
	r4 := publish("demo", "0.4.0", plat, identity.KindSolApp, appTar(t, "0.4.0"), false)
	os.WriteFile(filepath.Join(dev.Dir(), "channels", "blobs", r4.Artifacts[0].Hash()), appTar(t, "6.6.6"), 0o600)
	eventually(t, "0.4.0 offered", func() bool { a, _ := available(laptop, "demo"); return a.Version == "0.4.0" })
	if _, err := act(laptop, "sol.apps.update", map[string]string{"id": "demo"}); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("tampered blob: %v", err)
	}
	if version(laptop, "demo") != "0.3.0" {
		t.Fatalf("after a tampered update: %s", version(laptop, "demo"))
	}

	// A tar that climbs out of its directory is refused.
	publish("demo", "0.5.0", plat, identity.KindSolApp, appTar(t, "0.5.0", tarFile{"../../evil", "x"}), false)
	eventually(t, "0.5.0 offered", func() bool { a, _ := available(laptop, "demo"); return a.Version == "0.5.0" })
	if _, err := act(laptop, "sol.apps.update", map[string]string{"id": "demo"}); err == nil || !strings.Contains(err.Error(), "leaves the archive") {
		t.Fatalf("traversal: %v", err)
	}
	if _, err := os.Stat(filepath.Join(laptop.Dir(), "channels", "evil")); err == nil {
		t.Fatal("the traversal wrote a file")
	}

	// An Android app connected to Sol gets a notice; an APK is not installed by the daemon.
	if _, err := laptop.RegisterExternal(&sol.Manifest{ID: "raypass", Name: "Raypass", Version: "0.0.1"}, "org.example.raypass",
		"aa00000000000000000000000000000000000000000000000000000000000001", nil); err != nil {
		t.Fatal(err)
	}
	publish("raypass", "0.1.0", "android", identity.KindAPK, []byte("PK fake apk"), false)
	eventually(t, "the update notice", func() bool {
		for _, it := range laptop.API.InboxItems() {
			if it.Kind == "notice" && it.Title == "Raypass 0.1.0 is available" {
				return true
			}
		}
		return false
	})
	if _, err := act(laptop, "sol.apps.update", map[string]string{"id": "raypass"}); errCode(err) != "not_implemented" {
		t.Fatalf("apk update: %v", err)
	}

	// The onion goes away; the mirror still serves the bytes, checked against the signed hash.
	publish("demo", "0.6.0", plat, identity.KindSolApp, appTar(t, "0.6.0"), true)
	eventually(t, "0.6.0 offered on both devices", func() bool {
		a, _ := available(laptop, "demo")
		b, _ := available(phone, "demo")
		return a.Version == "0.6.0" && b.Version == "0.6.0"
	})
	if err := devc.Do(ctx, "DELETE", "/admin/v1/channels/hosted/demo", nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := act(laptop, "sol.apps.update", map[string]string{"id": "demo"}); err != nil {
		t.Fatalf("update from the mirror: %v", err)
	}
	if version(laptop, "demo") != "0.6.0" {
		t.Fatalf("from the mirror: %s", version(laptop, "demo"))
	}
}
