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

	if blameDirect(expired(t), time.Now()) {
		t.Fatal("a deadline that was already gone was blamed on the path")
	}
	if !blameDirect(expired(t), time.Now().Add(-2*time.Second)) {
		t.Fatal("a black hole that ate the caller's deadline wasn't blamed")
	}
}

func expired(t *testing.T) context.Context {
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	t.Cleanup(cancel)
	<-ctx.Done()
	return ctx
}
