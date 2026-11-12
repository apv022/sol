package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
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

func TestDirectAdvertiseValidation(t *testing.T) {
	c := Default()
	c.Transports.DirectAdvertise = []string{"home.example:7701", "0.0.0.0:7701", "[fe80::1]:7701", "224.0.0.1:7701",
		"127.0.0.1:7701", "1.2.3.4:0", "1.2.3.4:abc", "203.0.113.7:7701", "[2001:db8::1]:7701"}
	usable, problems := c.UsableDirectAdvertise()
	if len(usable) != 2 || usable[0] != "203.0.113.7:7701" || usable[1] != "[2001:db8::1]:7701" || len(problems) != 7 {
		t.Fatalf("usable %v, problems %v", usable, problems)
	}
}

// Unusable entries never stop the config loading, and loading never
// rewrites them (the user's file stays as written until they fix it).
func TestLoadKeepsDirectAdvertiseAsWritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	c := Default()
	c.Transports.Direct = true
	c.Transports.DirectAdvertise = []string{"myhost.example:7701"}
	b, _ := json.Marshal(c)
	os.WriteFile(path, b, 0o600)
	got, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := got.SetName("renamed"); err != nil { // any save
		t.Fatal(err)
	}
	again, _ := Load(path)
	tc := again.Snapshot().Transports
	if !tc.Direct || len(tc.DirectAdvertise) != 1 {
		t.Fatalf("stored direct settings changed: %+v", tc)
	}
}

func TestDirectListenValidation(t *testing.T) {
	c := Default()
	for _, bad := range []string{"7701", "host", ":99999"} {
		c.Transports.DirectListen = bad
		if c.Validate() == nil {
			t.Errorf("direct_listen %q accepted", bad)
		}
	}
	c.Transports.DirectListen = "127.0.0.1:7701"
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
}

// No STUN server is used unless the user lists one; entries are checked.
func TestSTUNIsOptInAndValidated(t *testing.T) {
	c := Default()
	if len(c.Transports.STUN) != 0 {
		t.Fatalf("a STUN server is chosen by default: %v", c.Transports.STUN)
	}
	c.Transports.STUN = []string{"cloudflare", "mozilla", "stun.example.org:3478"}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	c.Transports.STUN = []string{"cloudflar"}
	if c.Validate() == nil {
		t.Fatal("unknown preset accepted")
	}
}

// A site is a folder instead of a port; a record needs HTTP to be served.
func TestSiteAndRecordValidation(t *testing.T) {
	abs, err := filepath.Abs("site")
	if err != nil {
		t.Fatal(err)
	}
	c := Default()
	if err := c.AddService(Service{Name: "blog", Dir: abs, Visibility: VisibilityPublic, Record: true}); err != nil {
		t.Fatal(err)
	}
	if s, _ := c.Service("blog"); s.Protocol != ProtocolHTTP || s.LocalPort != 0 {
		t.Fatalf("a site must default to http with no port: %+v", s)
	}
	bad := map[string]Service{
		"relative folder":    {Name: "a", Dir: "site", Visibility: VisibilityPublic},
		"folder and port":    {Name: "b", Dir: abs, LocalPort: 80, Visibility: VisibilityPublic},
		"tcp site":           {Name: "c", Dir: abs, Protocol: ProtocolTCP, Visibility: VisibilityPublic},
		"record over tcp":    {Name: "d", LocalPort: 22, Record: true, Visibility: VisibilityPublic},
		"no port, no folder": {Name: "e", Visibility: VisibilityPrivate},
	}
	for name, s := range bad {
		if err := c.AddService(s); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := c.AddService(Service{Name: "app", LocalPort: 8000, Protocol: ProtocolHTTP, Record: true, Visibility: VisibilityPublic}); err != nil {
		t.Errorf("an http app with a record: %v", err)
	}
}

// `sol config` and the panel change settings through SetOption: a bad value
// changes nothing, and the answer says whether a restart is needed.
func TestOptions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	get := func(key string) string {
		for _, o := range c.Options() {
			if o.Key == key {
				return o.Value
			}
		}
		t.Fatalf("no option %q", key)
		return ""
	}
	for _, o := range c.Options() {
		if o.Label == "" || o.Help == "" || o.Kind == "" {
			t.Errorf("option %q is missing a label, help or kind", o.Key)
		}
	}
	if get("tor") != "on" || get("direct") != "off" || get("stun") != "" || get("help_tor") != "off" {
		t.Fatalf("defaults: %+v", c.Options())
	}

	if restart, err := c.SetOption("direct", "on"); err != nil || !restart {
		t.Fatalf("direct on: restart=%v err=%v", restart, err)
	}
	if restart, err := c.SetOption("name", "attic"); err != nil || restart {
		t.Fatalf("name: restart=%v err=%v", restart, err)
	}
	if _, err := c.SetOption("stun", "cloudflare, stun.example.org:3478"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.SetOption("direct_advertise", "203.0.113.7:7701"); err != nil {
		t.Fatal(err)
	}

	bad := map[string]string{
		"direct":           "maybe",
		"panel_port":       "0",
		"node_port":        "8080", // the panel's port
		"stun":             "not a server",
		"direct_advertise": "example.org:7701", // names are never dialled
		"direct_listen":    "7701",
		"clearnet_listen":  "nope",
		"name":             "",
		"nonsense":         "1",
	}
	before := c.Options()
	for key, v := range bad {
		if _, err := c.SetOption(key, v); err == nil {
			t.Errorf("%s=%q: accepted", key, v)
		}
	}
	if after := c.Options(); !slices.Equal(before, after) {
		t.Fatalf("a rejected value changed the config:\n%+v\n%+v", before, after)
	}

	// Saved: a fresh load sees the same values.
	c2, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(c.Options(), c2.Options()) {
		t.Fatalf("not saved:\n%+v\n%+v", c.Options(), c2.Options())
	}
	if get("stun") != "cloudflare, stun.example.org:3478" || get("direct_advertise") != "203.0.113.7:7701" {
		t.Fatalf("lists: stun=%q advertise=%q", get("stun"), get("direct_advertise"))
	}
}
