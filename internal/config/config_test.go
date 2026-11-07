package config

import (
	"errors"
	"os"
	"path/filepath"
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

func TestDeviceMutators(t *testing.T) {
	c := Default() // in-memory: Save is a no-op
	d := Device{ID: "abc", Name: "phone", PubKey: "k", Kind: DeviceKindDevice}
	if err := c.AddDevice(d); err != nil {
		t.Fatal(err)
	}
	if err := c.AddDevice(d); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("want ErrDuplicate, got %v", err)
	}
	got, err := c.DeleteDevice("abc")
	if err != nil || got.Name != "phone" {
		t.Fatalf("DeleteDevice = %+v, %v", got, err)
	}
	if _, ok := c.Device("abc"); ok {
		t.Fatal("device still present")
	}
}
