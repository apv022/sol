package session

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/pairing"
	"github.com/apv022/sol/internal/transport"
)

// anonMem is MemContacts that can mark contacts anonymous.
type anonMem struct {
	*MemContacts
	mu   sync.Mutex
	anon map[string]bool
}

func (c *anonMem) Anonymous(p ed25519.PublicKey) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.anon[string(p)]
}

func (c *anonMem) AddAnonymousContact(p ed25519.PublicKey, name string) error {
	c.mu.Lock()
	c.anon[string(p)] = true
	c.mu.Unlock()
	return c.AddContact(p, name)
}

func (c *anonMem) set(p ed25519.PublicKey, on bool) {
	c.mu.Lock()
	c.anon[string(p)] = on
	c.mu.Unlock()
}

// nearbyNode is newNode with contacts that know anonymity, and its clearnet transport.
func nearbyNode(t *testing.T, name string) (*node, *anonMem, *transport.Clearnet) {
	t.Helper()
	kr, err := identity.Create(t.TempDir(), name, name+"-1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cl := transport.NewClearnet("127.0.0.1:0", "")
	cl.Start(ctx)
	addr, err := cl.Publish(ctx, transport.Publication{ID: "node", Target: ln.Addr().String()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := kr.SetEndpoints([]identity.Endpoint{{Transport: "clearnet", Address: addr}}, time.Now()); err != nil {
		t.Fatal(err)
	}
	cs := &anonMem{MemContacts: NewMemContacts(), anon: map[string]bool{}}
	n := &node{kr: kr, contacts: cs.MemContacts, tokens: pairing.NewTokens()}
	n.m, err = New(Config{Keyring: kr, Contacts: cs, Tokens: n.tokens, Transports: []transport.Transport{cl},
		Handler: http.HandlerFunc(whoami), Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	go n.m.Serve(ln)
	t.Cleanup(func() { n.m.Close(); cl.Stop() })
	return n, cs, cl
}

// A nearby hint puts the beacon's address in front of a kept device's signed endpoints (an
// extra clearnet endpoint; the first candidate of wg and direct addresses, keys kept), only
// while it is fresh; anonymous contacts are neither offered to the beacon nor given hints.
func TestNearbyHints(t *testing.T) {
	a, acs, _ := nearbyNode(t, "Alice")
	b, _, _ := nearbyNode(t, "Bob")
	pairNodes(t, a, b)
	bp, bd := b.kr.Person(), b.kr.DevicePublic()
	eventually(t, "b kept", func() bool {
		for _, p := range a.m.NearbyPeers() {
			if bytes.Equal(p.Device, bd) && bytes.Equal(p.Person, bp) {
				return true
			}
		}
		return false
	})
	before, _ := a.m.endpointsFor(bp, bd)
	if !a.m.NoteNearby(bd, map[string]string{"clearnet": "127.0.0.1:1", "wg": "192.168.1.5:7001"}) {
		t.Fatal("hint for a kept buddy refused")
	}
	after, _ := a.m.endpointsFor(bp, bd)
	if len(after.Endpoints) != len(before.Endpoints)+1 || after.Endpoints[0] != (identity.Endpoint{Transport: "clearnet", Address: "127.0.0.1:1"}) ||
		after.Endpoints[1] != before.Endpoints[0] {
		t.Fatalf("endpoints with the hint: %+v (before %+v)", after.Endpoints, before.Endpoints)
	}
	// wg isn't in the record, so its hint added nothing; the signed record itself is unchanged.
	if rec, _ := a.m.cache.Endpoints(bp, bd); rec.Verify() != nil || len(rec.Endpoints) != len(before.Endpoints) {
		t.Fatalf("the cached record changed: %+v", rec)
	}
	if a.m.NoteNearby(make([]byte, 32), map[string]string{"clearnet": "127.0.0.1:1"}) {
		t.Fatal("hint for a device that isn't kept")
	}

	a.m.mu.Lock()
	h := a.m.nearby[string(bd)]
	h.at = h.at.Add(-NearbyTTL)
	a.m.nearby[string(bd)] = h
	a.m.mu.Unlock()
	if got, _ := a.m.endpointsFor(bp, bd); len(got.Endpoints) != len(before.Endpoints) {
		t.Fatalf("an expired hint still used: %+v", got.Endpoints)
	}

	acs.set(bp, true)
	for _, p := range a.m.NearbyPeers() {
		if bytes.Equal(p.Person, bp) {
			t.Fatal("an anonymous contact is offered to the beacon")
		}
	}
	if a.m.NoteNearby(bd, map[string]string{"clearnet": "127.0.0.1:1"}) {
		t.Fatal("hint for an anonymous contact accepted")
	}
}

// AddCandidate puts the hinted address first and keeps the key.
func TestAddCandidate(t *testing.T) {
	got, ok := transport.AddCandidate("sol-direct:abc@10.0.0.1:7,192.168.1.5:7", "192.168.1.5:7")
	if !ok || got != "sol-direct:abc@192.168.1.5:7,10.0.0.1:7" {
		t.Fatalf("%q %v", got, ok)
	}
	if _, ok := transport.AddCandidate("127.0.0.1:7", "127.0.0.1:8"); ok {
		t.Fatal("clearnet address merged")
	}
}
