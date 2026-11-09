package node

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/apv022/sol/internal/config"
	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/protocol"
	"github.com/apv022/sol/internal/transport"
)

func nodeID(t *testing.T) string {
	pub, _, _ := ed25519.GenerateKey(rand.Reader)
	return identity.NodeID(pub)
}

func TestSetDirectOnlyAcceptsThePeersOwnAddress(t *testing.T) {
	own, other := nodeID(t), nodeID(t)
	p := config.Peer{NodeID: own, Addrs: map[string]string{"tor": "x.onion:80"}}
	setDirect(&p, "sol-direct:"+own+"@10.0.0.2:7701")
	if p.Addrs["direct"] == "" {
		t.Fatal("own address dropped")
	}
	// Another node's key: dropped (and the old one with it, it's stale).
	setDirect(&p, "sol-direct:"+other+"@10.0.0.9:22")
	if p.Addrs["direct"] != "" {
		t.Fatalf("accepted another node's address: %q", p.Addrs["direct"])
	}
	// A direct address under another key would bypass the check (the dialer
	// routes by address, not key): dropped.
	p.Addrs["clearnet"] = "sol-direct:" + other + "@10.0.0.9:7701"
	setDirect(&p, "")
	if _, ok := p.Addrs["clearnet"]; ok {
		t.Fatal("kept a direct address under another key")
	}
	setDirect(&p, "sol-direct:"+own+"@10.0.0.2:7701")
	setDirect(&p, "garbage")
	if p.Addrs["direct"] != "" || p.Addrs["tor"] == "" {
		t.Fatalf("malformed address handling: %v", p.Addrs)
	}
}

func TestDirectBackoffDoubles(t *testing.T) {
	n := &Node{directFail: map[string]directFailure{}}
	n.noteDirectFailure("", "a") // pairing dials aren't tracked
	if len(n.directFail) != 0 {
		t.Fatal("tracked a dial without a peer id")
	}
	waits := []time.Duration{}
	for range 8 {
		n.noteDirectFailure("p", "a")
		waits = append(waits, n.directFail["p"].wait)
	}
	if waits[0] != directBackoff || waits[1] != 2*directBackoff || waits[7] != directBackoffMax {
		t.Fatalf("backoff: %v", waits)
	}
	n.noteDirectFailure("p", "b") // a new address starts over
	if n.directFail["p"].wait != directBackoff {
		t.Fatal("new address kept the old backoff")
	}
}

// fakeTransport handles "<name>:" addresses; Dial blocks until ctx ends.
type fakeTransport struct{ name string }

func (f fakeTransport) Name() string                { return f.name }
func (f fakeTransport) Start(context.Context) error { return nil }
func (f fakeTransport) Unpublish(string) error      { return nil }
func (f fakeTransport) Stop() error                 { return nil }
func (f fakeTransport) Status() transport.Status {
	return transport.Status{Name: f.name, State: transport.StateReady}
}
func (f fakeTransport) Handles(a string) bool {
	if f.name == "direct" {
		return transport.IsDirect(a)
	}
	return strings.HasPrefix(a, f.name+":")
}
func (f fakeTransport) Publish(context.Context, transport.Publication) (string, error) {
	return "", transport.ErrNotImplemented
}
func (f fakeTransport) Dial(ctx context.Context, _ string) (net.Conn, error) {
	if f.name == "direct" {
		return nil, errors.New("connection refused") // a dead direct path, at once
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func testNode() *Node {
	n := &Node{reg: transport.NewRegistry(), lastGood: map[string]string{}, directFail: map[string]directFailure{},
		lastDirectRefresh: map[string]time.Time{}}
	n.reg.Add(fakeTransport{"direct"})
	n.reg.Add(fakeTransport{"tor"})
	return n
}

func TestStaleDirectLastGoodIsSkipped(t *testing.T) {
	n := testNode()
	newA, oldA := "sol-direct:new@10.0.0.2:7701", "sol-direct:old@10.0.0.1:7701"
	p := config.Peer{ID: "p", Addrs: map[string]string{"direct": newA, "tor": "tor:x"}}
	n.lastGood["p"] = oldA // worked before the peer moved
	if got := n.addrsInOrder(p); strings.Join(got, ",") != newA+",tor:x" {
		t.Fatalf("order %v", got)
	}
	// With direct mode off here, a direct last-good address isn't tried at all.
	off := &Node{reg: transport.NewRegistry(), lastGood: map[string]string{"p": oldA}, directFail: map[string]directFailure{}}
	off.reg.Add(fakeTransport{"tor"})
	if got := off.addrsInOrder(config.Peer{ID: "p", Addrs: map[string]string{"tor": "tor:x"}}); strings.Join(got, ",") != "tor:x" {
		t.Fatalf("direct off: %v", got)
	}
}

func TestDirectBlameRules(t *testing.T) {
	p := config.Peer{ID: "p", Addrs: map[string]string{"direct": "sol-direct:a@10.0.0.2:7701"}}

	n := testNode()
	n.dialOnce(context.Background(), p, protocol.Request{Op: protocol.OpConnect})
	if n.directFail["p"].addr != p.Addrs["direct"] {
		t.Fatal("a failed direct attempt with a live context wasn't recorded")
	}

	n = testNode()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	n.dialOnce(ctx, p, protocol.Request{Op: protocol.OpConnect})
	if len(n.directFail) != 0 {
		t.Fatal("our own cancel was blamed on the direct path")
	}

	// Through dialOnce: a caller deadline that is already spent blames nothing.
	n = testNode()
	n.dialOnce(expired(t), p, protocol.Request{Op: protocol.OpConnect})
	if len(n.directFail) != 0 {
		t.Fatal("the caller's spent deadline was blamed on the direct path")
	}
	if blameDirect(expired(t), time.Now().Add(-time.Second)) {
		t.Fatal("an attempt cut short by the caller was blamed")
	}
	if !blameDirect(expired(t), time.Now().Add(-transport.DirectDialTimeout)) {
		t.Fatal("an attempt that had its full time and failed wasn't blamed")
	}
}

func expired(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	t.Cleanup(cancel)
	<-ctx.Done()
	return ctx
}

// Only unusable direct_advertise entries: direct mode is unavailable rather
// than advertising every local address (fail closed), and config is untouched.
func TestBadDirectAdvertiseFailsClosed(t *testing.T) {
	cfg := config.Default()
	cfg.Transports.Tor = false
	cfg.Transports.Direct = true
	cfg.Transports.DirectAdvertise = []string{"myhost.example:7701"}
	id, _ := identity.Generate()
	ts, warnings := buildTransports(cfg, t.TempDir(), id)
	if len(ts) != 1 || transport.Usable(ts[0]) || ts[0].Name() != "direct" || len(warnings) != 2 {
		t.Fatalf("transports: %+v, warnings %v", ts, warnings)
	}
	// A failed-closed direct transport doesn't make the node chase direct
	// addresses it can never dial.
	n := &Node{reg: transport.NewRegistry(), lastGood: map[string]string{}, directFail: map[string]directFailure{}}
	n.reg.Add(ts[0])
	n.reg.Add(fakeTransport{"tor"})
	if n.directUsable() {
		t.Fatal("unavailable direct transport counted as usable")
	}
	da := "sol-direct:" + id.NodeID() + "@10.0.0.2:7701"
	p := config.Peer{ID: "p", Addrs: map[string]string{"direct": da, "tor": "tor:x"}}
	n.directFail["p"] = directFailure{addr: da, at: time.Now(), wait: time.Hour} // also as last resort
	if got := n.addrsInOrder(p); strings.Join(got, ",") != "tor:x" {
		t.Fatalf("failed-closed direct still dialled: %v", got)
	}
	cfg.Transports.DirectAdvertise = []string{"myhost.example:7701", "203.0.113.7:7701"}
	ts, _ = buildTransports(cfg, t.TempDir(), id)
	d, ok := ts[0].(*transport.Direct)
	if !ok || len(d.Advertise) != 1 || d.Advertise[0] != "203.0.113.7:7701" {
		t.Fatalf("mixed entries: %+v", ts[0])
	}
}
