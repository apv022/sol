package peers

import (
	"errors"
	"testing"

	"github.com/apv022/sol/internal/config"
	"github.com/apv022/sol/internal/identity"
)

func TestAddLookupRemove(t *testing.T) {
	p := New(config.Default())
	key, _ := identity.Generate()
	stranger, _ := identity.Generate()

	var removed []string
	changes := 0
	p.OnRemove(func(c config.Peer) { removed = append(removed, c.ID) })
	p.OnChange(func() { changes++ })

	rec, err := p.Add(config.Peer{Name: "laptop", AccessIn: map[string]string{"tor": "TORKEY"}}, key.Public())
	if err != nil {
		t.Fatal(err)
	}
	if rec.NodeID != key.NodeID() || !rec.Shared(Inbox) || rec.Shared(Files) {
		t.Fatalf("new peer: %+v", rec)
	}
	if _, ok := p.Lookup(key.Public()); !ok {
		t.Fatal("paired key not found")
	}
	if _, ok := p.Lookup(stranger.Public()); ok {
		t.Fatal("stranger found")
	}
	if keys := p.AccessKeys("tor"); len(keys) != 1 || keys[0] != "TORKEY" {
		t.Fatalf("AccessKeys = %v", keys)
	}
	if len(p.AccessKeys("i2p")) != 0 {
		t.Fatal("keys leaked across transports")
	}

	// Re-pairing the same key replaces the record (and counts as a removal).
	if _, err := p.Add(config.Peer{Name: "laptop2"}, key.Public()); err != nil {
		t.Fatal(err)
	}
	if list := p.List(); len(list) != 1 || list[0].Name != "laptop2" {
		t.Fatalf("after re-pair: %+v", list)
	}

	if err := p.Remove(rec.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := p.Lookup(key.Public()); ok {
		t.Fatal("removed peer still found")
	}
	if len(removed) != 2 || changes != 4 {
		t.Fatalf("hooks: removed=%v changes=%d", removed, changes)
	}
}

func TestShareAndFind(t *testing.T) {
	p := New(config.Default())
	a, _ := identity.Generate()
	b, _ := identity.Generate()
	ra, _ := p.Add(config.Peer{Name: "Laptop"}, a.Public())
	p.Add(config.Peer{Name: "server"}, b.Public())

	if _, err := p.SetShared(ra.ID, "music", true); err != nil {
		t.Fatal(err)
	}
	if got, _ := p.Get(ra.ID); !got.Shared("music") {
		t.Fatal("share not saved")
	}
	if _, err := p.SetShared(ra.ID, "music", false); err != nil {
		t.Fatal(err)
	}
	if got, _ := p.Get(ra.ID); got.Shared("music") {
		t.Fatal("unshare not saved")
	}

	if got, err := p.Find("laptop"); err != nil || got.ID != ra.ID {
		t.Fatalf("Find by name = %+v, %v", got, err)
	}
	if got, err := p.Find(ra.ID[:5]); err != nil || got.ID != ra.ID {
		t.Fatalf("Find by prefix = %+v, %v", got, err)
	}
	if _, err := p.Find("nobody"); !errors.Is(err, config.ErrNotFound) {
		t.Fatalf("Find unknown: %v", err)
	}
}
