package config

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLoadCreatesDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "config.json")
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.PanelPort != 8080 || !c.Transports.Tor {
		t.Fatalf("unexpected defaults: %+v", c)
	}
	// Windows has no Unix permission bits.
	if runtime.GOOS == "windows" {
		return
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("config mode = %v, want 0600", st.Mode().Perm())
	}
}

func TestRoundTripAndDefaultsFill(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	// An old/minimal file: missing ports and protocol.
	if err := os.WriteFile(path, []byte(`{"services":[{"name":"music","local_port":4533,"visibility":"private"}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	s, ok := c.Service("music")
	if !ok || s.Protocol != ProtocolTCP || c.NodePort != 8081 || c.Version != CurrentVersion {
		t.Fatalf("defaults not applied: %+v %+v", c, s)
	}
}

func TestRejectsNewerVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	os.WriteFile(path, []byte(`{"version":999}`), 0o600)
	if _, err := Load(path); err == nil {
		t.Fatal("expected error for future config version")
	}
}

func TestServiceMutators(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.AddService(Service{Name: "music", LocalPort: 4533, Visibility: VisibilityPrivate}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddService(Service{Name: "music", LocalPort: 1, Visibility: VisibilityPrivate}); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("want ErrDuplicate, got %v", err)
	}
	for _, bad := range []Service{
		{Name: "Bad Name", LocalPort: 1, Visibility: VisibilityPrivate},
		{Name: "x", LocalPort: 0, Visibility: VisibilityPrivate},
		{Name: "y", LocalPort: 1, Visibility: "secret"},
	} {
		if err := c.AddService(bad); err == nil {
			t.Fatalf("accepted invalid service %+v", bad)
		}
	}
	if len(c.ServiceList()) != 1 {
		t.Fatal("invalid service was not rolled back")
	}
	if err := c.SetServiceVisibility("music", VisibilityPublic); err != nil {
		t.Fatal(err)
	}

	// Reload from disk and confirm persistence.
	c2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := c2.Service("music"); s.Visibility != VisibilityPublic {
		t.Fatalf("visibility not persisted: %+v", s)
	}
	if err := c2.RemoveService("music"); err != nil {
		t.Fatal(err)
	}
	if err := c2.RemoveService("music"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("want ErrNotFound, got %v", err)
	}
}

func TestPeerMutators(t *testing.T) {
	c := Default() // in-memory: Save is a no-op
	p := Peer{ID: "abc", NodeID: "n", Name: "laptop", PubKey: "k",
		Addrs: map[string]string{"tor": "x.onion"}, Shares: []string{"_inbox"}}
	if err := c.AddPeer(p); err != nil {
		t.Fatal(err)
	}
	if err := c.AddPeer(p); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("want ErrDuplicate, got %v", err)
	}

	// Readers get copies: changing one must not change the stored peer.
	got, _ := c.Peer("abc")
	got.Addrs["tor"] = "evil.onion"
	got.Shares[0] = "music"
	if again, _ := c.Peer("abc"); again.Addrs["tor"] != "x.onion" || again.Shares[0] != "_inbox" {
		t.Fatalf("Peer returned shared state: %+v", again)
	}

	// UpdatePeer keeps the change only if fn succeeds.
	if _, err := c.UpdatePeer("abc", func(p *Peer) error { p.Shares = append(p.Shares, "web"); return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := c.UpdatePeer("abc", func(p *Peer) error { p.Name = ""; return nil }); err == nil {
		t.Fatal("UpdatePeer accepted an empty name")
	}
	if p, _ := c.Peer("abc"); p.Name != "laptop" || !p.Shared("web") {
		t.Fatalf("after updates: %+v", p)
	}

	removed, err := c.DeletePeer("abc")
	if err != nil || removed.Name != "laptop" {
		t.Fatalf("DeletePeer = %+v, %v", removed, err)
	}
	if _, ok := c.Peer("abc"); ok {
		t.Fatal("peer still present")
	}
}

func TestRemoveServiceUnshares(t *testing.T) {
	c := Default()
	if err := c.AddService(Service{Name: "web", LocalPort: 80, Visibility: VisibilityPrivate}); err != nil {
		t.Fatal(err)
	}
	if err := c.AddPeer(Peer{ID: "abc", NodeID: "n", Name: "laptop", PubKey: "k", Shares: []string{"_inbox", "web"}}); err != nil {
		t.Fatal(err)
	}
	if err := c.RemoveService("web"); err != nil {
		t.Fatal(err)
	}
	if p, _ := c.Peer("abc"); p.Shared("web") || !p.Shared("_inbox") {
		t.Fatalf("shares after removing web: %v", p.Shares)
	}
}

func TestMigrateDropsDevices(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	old := `{"version":1,"panel_port":8080,"node_port":8081,"transports":{"tor":true},
		"services":[],"devices":[{"id":"abc","name":"phone","pubkey":"k","kind":"device"}]}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Version != CurrentVersion || len(c.PeerList()) != 0 || c.NodeName() == "" {
		t.Fatalf("migrated: version %d, peers %v, name %q", c.Version, c.PeerList(), c.NodeName())
	}
	data, _ := os.ReadFile(path)
	if strings.Contains(string(data), "devices") {
		t.Fatal("old devices were written back")
	}
}
