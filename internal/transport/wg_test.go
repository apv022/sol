package transport

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"
)

func startWG(t *testing.T, stun ...string) (*WG, string) {
	t.Helper()
	w := NewWG("127.0.0.1:0", filepath.Join(t.TempDir(), "wg.key"), nil, stun)
	if err := w.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Stop() })
	addr, err := w.Publish(context.Background(), Publication{ID: "node", Target: echoTarget(t)})
	if err != nil {
		t.Fatal(err)
	}
	return w, addr
}

func TestWireGuardTunnelBothWays(t *testing.T) {
	a, aAddr := startWG(t)
	b, bAddr := startWG(t)
	if !IsWG(aAddr) || !a.Handles(aAddr) {
		t.Fatalf("address %q", aAddr)
	}
	// Each side knows the other (as after pairing), and either can dial.
	if err := a.SetPeer(bAddr); err != nil {
		t.Fatal(err)
	}
	if err := b.SetPeer(aAddr); err != nil {
		t.Fatal(err)
	}
	for _, d := range []struct {
		from *WG
		to   string
	}{{a, bAddr}, {b, aAddr}} {
		c, err := d.from.Dial(context.Background(), d.to)
		if err != nil {
			t.Fatal(err)
		}
		c.Write([]byte("ping"))
		buf := make([]byte, 4)
		c.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
			t.Fatalf("echo through WireGuard: %q %v", buf, err)
		}
		c.Close()
	}
	if st := a.Status(); !strings.Contains(st.Detail, "1 of 1 peers connected") {
		// the loop records handshakes every 500 ms
		time.Sleep(time.Second)
		if st = a.Status(); !strings.Contains(st.Detail, "1 of 1 peers connected") {
			t.Fatalf("status: %s", st.Detail)
		}
	}
}

// A node that doesn't know us (not paired) never completes a handshake:
// WireGuard answers only keys it was given.
func TestWireGuardIgnoresStrangers(t *testing.T) {
	a, _ := startWG(t)
	_, bAddr := startWG(t) // b was never told about a
	start := time.Now()
	if _, err := a.Dial(context.Background(), bAddr); err == nil {
		t.Fatal("reached a node that doesn't know us")
	}
	if time.Since(start) > WGDialTimeout+time.Second {
		t.Fatal("a dead direct path must fail fast")
	}
}

// Removing a peer (unpairing) cuts the tunnel.
func TestWireGuardRemovePeer(t *testing.T) {
	a, aAddr := startWG(t)
	b, bAddr := startWG(t)
	a.SetPeer(bAddr)
	b.SetPeer(aAddr)
	c, err := a.Dial(context.Background(), bAddr)
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	b.RemovePeer(aAddr)
	time.Sleep(200 * time.Millisecond)
	c, err = a.Dial(context.Background(), bAddr)
	if err == nil {
		c.Write([]byte("x"))
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := c.Read(make([]byte, 1)); err == nil {
			t.Fatal("removed peer still reaches the node")
		}
		c.Close()
	}
}

func TestParseWGAddr(t *testing.T) {
	for _, bad := range []string{"", "sol-wg:", "sol-wg:abc@1.2.3.4:1", "sol-wg:" + strings.Repeat("A", 43) + "@", "x.onion:80"} {
		if _, _, err := ParseWGAddr(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	_, addr := startWG(t)
	if _, c, err := ParseWGAddr(addr); err != nil || len(c) != 1 {
		t.Fatalf("%q: %v %v", addr, c, err)
	}
}

// fakeSTUN answers Binding requests with the sender's address
// (XOR-MAPPED-ADDRESS), like a real server.
func fakeSTUN(t *testing.T) string {
	t.Helper()
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pc.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, from, err := pc.ReadFrom(buf)
			if err != nil {
				return
			}
			if n < stunHeaderSize || !isSTUN(buf[:n]) {
				continue
			}
			ua := from.(*net.UDPAddr)
			resp := make([]byte, stunHeaderSize+12)
			binary.BigEndian.PutUint16(resp[0:], stunBindingSuccess)
			binary.BigEndian.PutUint16(resp[2:], 12)
			copy(resp[4:20], buf[4:20]) // cookie + transaction id
			attr := resp[20:]
			binary.BigEndian.PutUint16(attr[0:], stunAttrXORMapped)
			binary.BigEndian.PutUint16(attr[2:], 8)
			attr[5] = 0x01
			binary.BigEndian.PutUint16(attr[6:], uint16(ua.Port)^uint16(stunMagic>>16))
			ip4 := ua.IP.To4()
			for i := range 4 {
				attr[8+i] = ip4[i] ^ resp[4+i]
			}
			pc.WriteTo(resp, from)
		}
	}()
	return pc.LocalAddr().String()
}

func TestSTUNOnTheWireGuardSocket(t *testing.T) {
	server := fakeSTUN(t)
	w := NewWG("127.0.0.1:0", filepath.Join(t.TempDir(), "wg.key"), nil, nil)
	if err := w.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { w.Stop() })
	ap, err := w.bind.Query(netip.MustParseAddrPort(server), 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	// The mapped port is WireGuard's own port: the mapping peers will use.
	if ap.Port() != w.port {
		t.Fatalf("STUN mapped port %d, WireGuard listens on %d", ap.Port(), w.port)
	}
}

func TestSTUNParsing(t *testing.T) {
	req, id := stunRequest()
	if !isSTUN(req) {
		t.Fatal("request not recognised as STUN")
	}
	if _, _, err := parseSTUNResponse(req); err == nil {
		t.Fatal("a request parsed as a response")
	}
	// WireGuard packets (type 1..4 then three zero bytes) are never STUN.
	for typ := byte(1); typ <= 4; typ++ {
		p := make([]byte, 148)
		p[0] = typ
		if isSTUN(p) {
			t.Fatalf("WireGuard message type %d taken for STUN", typ)
		}
	}
	_ = id
}

// Both sides behind NAT-like filtering: inbound packets are dropped unless
// that side already sent to the source. A tunnel forms only if both punch,
// whichever side dials (initiator or not).
func TestWireGuardPunchesThroughNATFiltering(t *testing.T) {
	for _, dialer := range []string{"a", "b"} {
		t.Run("dialer "+dialer, func(t *testing.T) {
			a, aAddr := startWG(t)
			b, bAddr := startWG(t)
			for _, w := range []*WG{a, b} {
				w.mu.Lock()
				b := w.bind
				w.mu.Unlock()
				b.mu.Lock()
				b.natFilter, b.sentTo = true, map[string]bool{}
				b.mu.Unlock()
			}
			a.SetPeer(bAddr)
			b.SetPeer(aAddr)
			from, to := a, bAddr
			if dialer == "b" {
				from, to = b, aAddr
			}
			var c net.Conn
			var err error
			for range 3 { // the first attempt may race the other side's punch
				if c, err = from.Dial(context.Background(), to); err == nil {
					break
				}
			}
			if err != nil {
				t.Fatalf("no tunnel through NAT filtering: %v", err)
			}
			defer c.Close()
			c.Write([]byte("ping"))
			buf := make([]byte, 4)
			c.SetReadDeadline(time.Now().Add(3 * time.Second))
			if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "ping" {
				t.Fatalf("echo: %q %v", buf, err)
			}
		})
	}
}

func TestWireGuardBusyPortFallsBack(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows lets two UDP sockets share a port, so there is no clash to fall back from")
	}
	busy, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer busy.Close()
	_, port, _ := net.SplitHostPort(busy.LocalAddr().String())
	// WireGuard binds all addresses on the port; take it on 0.0.0.0 too.
	busyAll, err := net.ListenPacket("udp", ":"+port)
	if err == nil {
		defer busyAll.Close()
	}
	w := NewWG("127.0.0.1:"+port, filepath.Join(t.TempDir(), "wg.key"), nil, nil)
	if err := w.Start(context.Background()); err != nil {
		t.Fatalf("busy port should fall back, got %v", err)
	}
	t.Cleanup(func() { w.Stop() })
	if st := w.Status(); st.State != StateReady || !strings.Contains(st.Detail, "random port") {
		t.Fatalf("status: %+v", st)
	}
}

func TestUDPResetIsNotFatal(t *testing.T) {
	for _, err := range []error{syscall.ECONNREFUSED, syscall.ECONNRESET,
		errors.New("wsarecvfrom: An existing connection was forcibly closed by the remote host.")} {
		if !isUDPReset(err) {
			t.Errorf("%v treated as fatal", err)
		}
	}
	if isUDPReset(net.ErrClosed) {
		t.Fatal("a closed socket must stay fatal")
	}
}

// Stopping while punch packets arrive must not deadlock (Stop used to hold
// the lock that the receive path needs, while waiting for that path) or
// crash the background loop. Seen on macOS CI.
func TestWireGuardStopUnderPunches(t *testing.T) {
	for range 5 {
		a, aAddr := startWG(t)
		b, bAddr := startWG(t)
		a.SetPeer(bAddr) // a knows b but has no tunnel, so b's punches reach onPunch
		b.SetPeer(aAddr)
		done := make(chan struct{})
		go func() {
			defer close(done)
			p := b.peers[func() [32]byte { k, _, _ := ParseWGAddr(aAddr); return k }()]
			for i := 0; i < 200; i++ {
				b.punch(p)
			}
		}()
		stopped := make(chan struct{})
		go func() { a.Stop(); close(stopped) }()
		select {
		case <-stopped:
		case <-time.After(5 * time.Second):
			t.Fatal("Stop deadlocked while punch packets arrived")
		}
		<-done
		time.Sleep(600 * time.Millisecond) // let a's loop tick after Stop
	}
}

// Peer reflection: a paired peer tells us the address our packets come
// from, so no STUN server is needed.
func TestPeerReflectionLearnsPublicAddress(t *testing.T) {
	a, aAddr := startWG(t)
	b, bAddr := startWG(t)
	a.SetPeer(bAddr)
	b.SetPeer(aAddr)
	a.askPeers()
	want := netip.AddrPortFrom(netip.MustParseAddr("127.0.0.1"), a.port)
	deadline := time.Now().Add(3 * time.Second)
	for {
		a.mu.Lock()
		got, from := a.public, a.pubFrom
		a.mu.Unlock()
		if got == want && from == "a paired peer" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("public address %v (from %q), want %v from a paired peer", got, from, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if st := a.Status(); !strings.Contains(st.Detail, "from a paired peer") {
		t.Fatalf("status: %s", st.Detail)
	}
}

// A node that doesn't know us gives no answer: the port stays silent to
// strangers, and an unsolicited reply changes nothing.
func TestPeerReflectionIgnoresStrangers(t *testing.T) {
	a, _ := startWG(t)
	_, bAddr := startWG(t) // b was never told about a
	a.SetPeer(bAddr)
	a.askPeers()

	// Someone else sends a reply we never asked for.
	pc, err := net.ListenPacket("udp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer pc.Close()
	pc.WriteTo(replyPacket(netip.MustParseAddrPort("203.0.113.9:4444")),
		&net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(a.port)})

	time.Sleep(time.Second)
	a.mu.Lock()
	got := a.public
	a.mu.Unlock()
	if got.IsValid() {
		t.Fatalf("learned %v from a stranger", got)
	}
}

func TestReflectionPackets(t *testing.T) {
	for _, s := range []string{"198.51.100.7:7701", "[2001:db8::1]:7701"} {
		ap := netip.MustParseAddrPort(s)
		got, ok := parseReply(replyPacket(ap))
		if !ok || got != ap {
			t.Fatalf("%s round-tripped to %v %v", s, got, ok)
		}
	}
	for _, bad := range [][]byte{nil, []byte("SOLR"), []byte("SOLRnot-an-address"), append([]byte("SOLR"), make([]byte, 80)...)} {
		if _, ok := parseReply(bad); ok {
			t.Fatalf("%q parsed", bad)
		}
	}
	var pub [32]byte
	if !isQuery(queryPacket(pub)) || isPunch(queryPacket(pub)) || isQuery(punchPacket(pub)) {
		t.Fatal("query and punch packets confused")
	}
	if isSTUN(queryPacket(pub)) || isSTUN(replyPacket(netip.MustParseAddrPort("198.51.100.7:1"))) {
		t.Fatal("a Sol packet taken for STUN")
	}
	for addr, public := range map[string]bool{"198.51.100.7": true, "2001:db8::1": true,
		"192.168.1.5": false, "10.0.0.1": false, "100.101.1.2": false, "fd7a:115c::1": false, "127.0.0.1": false} {
		if isPublicAddr(netip.MustParseAddr(addr)) != public {
			t.Errorf("isPublicAddr(%s) != %v", addr, public)
		}
	}
}
