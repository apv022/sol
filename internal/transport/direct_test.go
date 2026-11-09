package transport

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/apv022/sol/internal/identity"
)

// echoTarget is a plain TCP echo server standing in for the node listener.
func echoTarget(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() { defer c.Close(); io.Copy(c, c) }()
		}
	}()
	return ln.Addr().String()
}

func startDirect(t *testing.T) (*Direct, string) {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	d := NewDirect("127.0.0.1:0", priv)
	if err := d.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Stop() })
	addr, err := d.Publish(context.Background(), Publication{ID: "node", Target: echoTarget(t)})
	if err != nil {
		t.Fatal(err)
	}
	return d, addr
}

func TestDirectDialPinsTheNodeKey(t *testing.T) {
	d, addr := startDirect(t)
	if !d.Handles(addr) || !strings.HasPrefix(addr, "sol-direct:"+d.nodeID+"@127.0.0.1:") {
		t.Fatalf("address %q", addr)
	}
	c, err := d.Dial(context.Background(), addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("ping"))
	buf := make([]byte, 4)
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
		t.Fatalf("echo through TLS: %q %v", buf, err)
	}

	// The same host:port under another node's id must be refused: that is
	// what stops a man in the middle (or a stale address pointing at
	// someone else's node).
	other, _, _ := ed25519.GenerateKey(rand.Reader)
	_, cands, _ := ParseDirectAddr(addr)
	forged := "sol-direct:" + identity.NodeID(other) + "@" + cands[0]
	// (It never even gets a certificate: the preamble tag doesn't match.)
	if c, err := d.Dial(context.Background(), forged); err == nil {
		c.Close()
		t.Fatal("dial with the wrong key succeeded")
	}
}

func TestDirectIsSilentToProbes(t *testing.T) {
	_, addr := startDirect(t)
	_, cands, _ := ParseDirectAddr(addr)
	// Includes the bare magic: without this node's tag it gets nothing either.
	for _, probe := range []string{"GET / HTTP/1.0\r\n\r\n", "\x16\x03\x01\x00\x05hello", "SOLD1" + strings.Repeat("x", 16)} {
		c, err := net.Dial("tcp", cands[0])
		if err != nil {
			t.Fatal(err)
		}
		c.Write([]byte(probe))
		c.SetReadDeadline(time.Now().Add(1500 * time.Millisecond))
		n, _ := c.Read(make([]byte, 64))
		c.Close()
		if n != 0 {
			t.Fatalf("probe %q got %d bytes back", probe, n)
		}
	}
}

func TestDirectCarriesOnlyTheNode(t *testing.T) {
	d, _ := startDirect(t)
	if _, err := d.Publish(context.Background(), Publication{ID: "svc-blog", Target: "127.0.0.1:1"}); err == nil {
		t.Fatal("a public service was published on direct")
	}
}

func TestDirectFallsThroughDeadCandidates(t *testing.T) {
	d, addr := startDirect(t)
	_, cands, _ := ParseDirectAddr(addr)
	dead, _ := net.Listen("tcp", "127.0.0.1:0")
	deadAddr := dead.Addr().String()
	dead.Close()
	mixed := "sol-direct:" + d.nodeID + "@" + deadAddr + "," + cands[0]
	c, err := d.Dial(context.Background(), mixed)
	if err != nil {
		t.Fatalf("one good candidate should be enough: %v", err)
	}
	c.Close()
	start := time.Now()
	if _, err := d.Dial(context.Background(), "sol-direct:"+d.nodeID+"@"+deadAddr); err == nil {
		t.Fatal("dead address dialed")
	}
	if time.Since(start) > DirectDialTimeout+time.Second {
		t.Fatal("a dead direct path must fail fast")
	}
}

func TestParseDirectAddr(t *testing.T) {
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	id := identity.NodeID(priv.Public().(ed25519.PublicKey))
	for _, bad := range []string{"", "sol-direct:", "sol-direct:" + id, "sol-direct:" + id + "@", "sol-direct:nope@1.2.3.4:1", "x.onion:80"} {
		if _, _, err := ParseDirectAddr(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	if _, c, err := ParseDirectAddr("sol-direct:" + id + "@[2001:db8::1]:7701,10.0.0.2:7701"); err != nil || len(c) != 2 {
		t.Fatalf("v6+v4: %v %v", c, err)
	}
}

func TestDirectRetriesABusyPort(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	old := retryListenEvery
	retryListenEvery = 20 * time.Millisecond
	t.Cleanup(func() { retryListenEvery = old })
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	d := NewDirect(busy.Addr().String(), priv)
	d.Start(context.Background())
	t.Cleanup(func() { d.Stop() })
	if _, err := d.Publish(context.Background(), Publication{ID: "node", Target: echoTarget(t)}); err == nil {
		t.Fatal("publish on a busy port should report it")
	}
	if st := d.Status(); st.State == StateReady {
		t.Fatalf("busy port shown as ready: %+v", st)
	}
	busy.Close()
	for i := 0; i < 200 && d.Status().Addresses["node"] == ""; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if st := d.Status(); st.State != StateReady || st.Addresses["node"] == "" {
		t.Fatalf("port never retried: %+v", st)
	}
}

// A man in the middle who knows the node id (so it can compute the
// preamble) still can't pass: its certificate isn't the node's key.
func TestDirectRejectsAnImpostorCertificate(t *testing.T) {
	_, victim, _ := ed25519.GenerateKey(rand.Reader)
	_, impostor, _ := ed25519.GenerateKey(rand.Reader)
	cert, err := selfSigned(impostor)
	if err != nil {
		t.Fatal(err)
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				io.ReadFull(c, make([]byte, len(directMagic)+directTagSize)) // accept any tag
				tls.Server(c, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}).Handshake()
				c.Close()
			}()
		}
	}()
	d := NewDirect("127.0.0.1:0", impostor)
	addr := "sol-direct:" + identity.NodeID(victim.Public().(ed25519.PublicKey)) + "@" + ln.Addr().String()
	if _, err := d.Dial(context.Background(), addr); err == nil || !strings.Contains(err.Error(), "different node") {
		t.Fatalf("impostor accepted: %v", err)
	}
}
