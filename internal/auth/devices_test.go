package auth

import (
	"testing"

	"github.com/apv022/sol/internal/config"
	"github.com/apv022/sol/internal/identity"
)

func TestAddLookupRemove(t *testing.T) {
	d := NewDevices(config.Default())
	dev, _ := identity.Generate()
	stranger, _ := identity.Generate()

	var removed []string
	changes := 0
	d.OnRemove(func(c config.Device) { removed = append(removed, c.ID) })
	d.OnChange(func() { changes++ })

	rec, err := d.Add("laptop", dev.Public(), "TORKEY", "")
	if err != nil {
		t.Fatal(err)
	}
	if !d.IsAllowed(dev.Public()) || d.IsAllowed(stranger.Public()) {
		t.Fatal("IsAllowed wrong")
	}
	if _, err := d.Add("again", dev.Public(), "", ""); err == nil {
		t.Fatal("duplicate key accepted")
	}
	if keys := d.TorAuthKeys(); len(keys) != 1 || keys[0] != "TORKEY" {
		t.Fatalf("TorAuthKeys = %v", keys)
	}
	if err := d.Remove(rec.ID); err != nil {
		t.Fatal(err)
	}
	if d.IsAllowed(dev.Public()) {
		t.Fatal("revoked device still allowed")
	}
	if len(removed) != 1 || removed[0] != rec.ID || changes != 2 {
		t.Fatalf("hooks: removed=%v changes=%d", removed, changes)
	}
}

func TestValidateName(t *testing.T) {
	for _, bad := range []string{"", string(make([]byte, 65)), "tab\there"} {
		if ValidateName(bad) == nil {
			t.Errorf("accepted %q", bad)
		}
	}
	if ValidateName("Alice's phone 📱") != nil {
		t.Error("rejected a normal name")
	}
}
