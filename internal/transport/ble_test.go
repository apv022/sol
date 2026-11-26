package transport

import (
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"net"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

// bridge connects to b's bridge port as the Android shell does and writes the first line.
func bridge(t *testing.T, b *BLE, line string) net.Conn {
	t.Helper()
	c, err := net.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(b.BridgePort())))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	if _, err := io.WriteString(c, line); err != nil {
		t.Fatal(err)
	}
	return c
}

// closed reports whether the daemon end closed c (a read sees EOF or a reset soon).
func closed(c net.Conn) bool {
	_ = c.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, err := c.Read(make([]byte, 1))
	var ne net.Error
	return err != nil && !(errors.As(err, &ne) && ne.Timeout())
}

// The BLE transport: an "out" link is dialable once, only as the address its handle was issued
// for; "in" links reach the publication's target with the bytes after the first line intact;
// unknown handles, bad first lines and links while switched off are closed; CloseLinks cuts
// everything; nothing is ever published.
func TestBLEBridge(t *testing.T) {
	b := NewBLE()
	var allow atomic.Bool
	allow.Store(true)
	b.Allow = allow.Load
	links := make(chan string, 4)
	b.OnLink = func(addr string) { links <- addr }
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer b.Stop()
	target, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	if addr, err := b.Publish(context.Background(), Publication{ID: "node", Target: target.Addr().String()}); err != nil || addr != "" {
		t.Fatalf("publish: %q %v", addr, err)
	}
	if st := b.Status(); st.State != StateReady || len(st.Addresses) != 0 {
		t.Fatalf("status %+v", st)
	}
	dev := ed25519.PublicKey(make([]byte, ed25519.PublicKeySize))
	dev[0] = 7
	addr := BLEAddr(dev)
	if !b.Handles(addr) || b.Handles("127.0.0.1:1") || (&Clearnet{}).Handles(addr) {
		t.Fatal("Handles")
	}
	if _, err := b.Dial(context.Background(), addr); !errors.Is(err, ErrNoBLELink) {
		t.Fatalf("dial with no link: %v", err)
	}

	// out: the shell's end writes after the line; the dialer reads exactly that.
	h := b.Expect(addr)
	shell := bridge(t, b, "SOLBLE1 out "+h+"\nhello")
	if got := <-links; got != addr {
		t.Fatalf("link for %q", got)
	}
	c, err := b.Dial(context.Background(), addr)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 5)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "hello" {
		t.Fatalf("read %q %v", buf, err)
	}
	if _, err := b.Dial(context.Background(), addr); !errors.Is(err, ErrNoBLELink) {
		t.Fatalf("second dial: %v", err)
	}
	if b.Links() != 1 {
		t.Fatalf("links %d", b.Links())
	}
	// The handle was used: the same handle again is refused.
	if again := bridge(t, b, "SOLBLE1 out "+h+"\n"); !closed(again) {
		t.Fatal("a used handle was accepted")
	}
	for _, line := range []string{"SOLBLE1 out 00\n", "HELLO\n", "SOLBLE1 sideways\n", "SOLBLE1 " + string(make([]byte, 80)) + "\n"} {
		if bad := bridge(t, b, line); !closed(bad) {
			t.Fatalf("%q was accepted", line)
		}
	}

	// in: reaches the target, the first line stripped.
	in := bridge(t, b, "SOLBLE1 in\nping")
	tc, err := target.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer tc.Close()
	buf = make([]byte, 4)
	if _, err := io.ReadFull(tc, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("target read %q %v", buf, err)
	}
	io.WriteString(tc, "pong")
	if _, err := io.ReadFull(in, buf); err != nil || string(buf) != "pong" {
		t.Fatalf("shell read %q %v", buf, err)
	}

	// Switched off: new links are closed; CloseLinks cuts the open ones.
	allow.Store(false)
	if off := bridge(t, b, "SOLBLE1 in\n"); !closed(off) {
		t.Fatal("a link was accepted while off")
	}
	b.CloseLinks()
	if !closed(shell) || !closed(in) {
		t.Fatal("CloseLinks left a link open")
	}

	// An expired link is closed and no longer dialable.
	allow.Store(true)
	old := BLELinkTTL
	BLELinkTTL = 100 * time.Millisecond
	defer func() { BLELinkTTL = old }()
	late := bridge(t, b, "SOLBLE1 out "+b.Expect(addr)+"\n")
	<-links
	if !closed(late) || b.Waiting(addr) {
		t.Fatal("an unused link outlived BLELinkTTL")
	}
}
