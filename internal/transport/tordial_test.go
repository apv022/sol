package transport

import (
	"context"
	"testing"
	"time"

	"github.com/apv022/sol/internal/faultnet"
)

// A descriptor fetch that hangs (the circuit to the one directory tor
// asked stalled) used to hold every dial for the full timeout. Dial asks
// tor for another fetch (HSFETCH) when no descriptor has arrived in time.
func TestTorDialHedgesAStuckDescriptorFetch(t *testing.T) {
	// One fake network: the first tor publishes, the second one's lookups hang.
	a := newSupervised(t, faultnet.TorPlan{}, faultnet.TorPlan{StuckFetch: true})
	setVar(t, &hsFetchAfter, 50*time.Millisecond)
	setVar(t, &hsFetchEvery, 50*time.Millisecond)
	if err := a.start(t); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, a.Tor, "publisher ready", isReady)
	addr, err := a.Publish(context.Background(), Publication{ID: "node", Target: echoTarget(t)})
	if err != nil {
		t.Fatal(err)
	}
	b := NewTor(t.TempDir(), a.fake.Provider())
	t.Cleanup(func() { b.Stop() })
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	waitStatus(t, b, "dialer ready", isReady)

	// A plain SOCKS connect (no hedging) waits out its deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	b.mu.Lock()
	socks := b.socksAddr
	b.mu.Unlock()
	if conn, err := dialSOCKS5(ctx, socks, addr+":7"); err == nil {
		conn.Close()
		t.Fatal("a stuck lookup connected without HSFETCH; the fake tor is not stuck")
	}
	cancel()

	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	start := time.Now()
	conn, err := b.Dial(ctx, addr)
	if err != nil {
		t.Fatalf("hedged dial: %v", err)
	}
	conn.Close()
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("hedged dial took %s", d)
	}
	// The event comes on tor's second control connection, read by its own
	// goroutine: it may be handled just after the SOCKS reply that it
	// unblocked, so give that reader a moment instead of racing it.
	for end := time.Now().Add(5 * time.Second); !b.descSince(onionServiceID(addr), start); time.Sleep(5 * time.Millisecond) {
		if time.Now().After(end) {
			t.Fatal("the HS_DESC RECEIVED event was not recorded")
		}
	}
}

// A descriptor that arrived during the dial means the lookup worked: no
// HSFETCH then. One from before the dial does not count.
func TestTorDescReceivedEvent(t *testing.T) {
	tr := NewTor(t.TempDir(), nil)
	sid := "abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz2345"
	before := time.Now()
	tr.noteEvent("650 HS_DESC RECEIVED " + sid + " NO_AUTH $DIR descid")
	if !tr.descSince(sid, before) {
		t.Fatal("RECEIVED not recorded")
	}
	if tr.descSince(sid, time.Now().Add(time.Second)) {
		t.Fatal("an older descriptor counted for a later dial")
	}
	tr.noteEvent("650 HS_DESC REQUESTED other NO_AUTH $DIR descid")
	if tr.descSince("other", before) {
		t.Fatal("REQUESTED counted as RECEIVED")
	}
}
