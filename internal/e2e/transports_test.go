package e2e

// C10: the default transports working together (WireGuard through NATs, Tor as the fallback)
// and anonymous (Tor-only) contacts.

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/apv022/sol/internal/apphost"
	"github.com/apv022/sol/internal/builtins"
	sold "github.com/apv022/sol/internal/daemon"
	"github.com/apv022/sol/internal/faultnet"
	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/pairing"
	solruntime "github.com/apv022/sol/internal/runtime"
	"github.com/apv022/sol/internal/session"
	"github.com/apv022/sol/internal/transport"
	"github.com/apv022/sol/internal/wire"
)

// netNode configures one device for startNet.
type netNode struct {
	o          sold.InitOptions
	transports []sold.TransportConfig
	tor        solruntime.Provider
	builtins   map[string]apphost.Builtin
	wrap       func(transport.Transport) transport.Transport
}

// startNet starts a real daemon with the given transports.
func startNet(t *testing.T, n netNode) *sold.Daemon {
	t.Helper()
	dir := t.TempDir()
	n.o.Config = sold.Config{UI: "127.0.0.1:0", Files: filepath.Join(dir, "SolDrive"), Transports: n.transports}
	n.o.Builtins = n.builtins
	if err := sold.Init(dir, n.o); err != nil {
		t.Fatal(err)
	}
	d, err := sold.Start(sold.Options{Dir: dir, Builtins: n.builtins, InProcessApps: true, TorProvider: n.tor,
		WrapTransport: n.wrap, Log: testLog(n.o.Device),
		SameUser: func(_, _ netip.AddrPort) (bool, error) { return true, nil }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

// behindNAT puts the WireGuard transport behind nat (the others are left alone).
func behindNAT(nat *faultnet.NAT) func(transport.Transport) transport.Transport {
	return func(tr transport.Transport) transport.Transport {
		if w, ok := tr.(*transport.WG); ok {
			w.PacketConn = nat.Listen
		}
		return tr
	}
}

// shortPaths makes failed paths come back within a second, for the tests that move a session
// between paths. Restored after the test's daemons are closed (it is registered first).
func shortPaths(t *testing.T) {
	ob, om, of, op := session.PathBackoff, session.PathBackoffMax, session.StableFor, session.StableProbes
	t.Cleanup(func() {
		session.PathBackoff, session.PathBackoffMax, session.StableFor, session.StableProbes = ob, om, of, op
	})
	session.PathBackoff, session.PathBackoffMax = 300*time.Millisecond, time.Second
	session.StableFor, session.StableProbes = 600*time.Millisecond, 2
}

// pathBetween is the transport of the live session between a and b as the side that dialed
// it knows it ("" while there is none, or only the callee's view is known).
func pathBetween(a, b *sold.Daemon) string {
	for _, p := range [][2]*sold.Daemon{{a, b}, {b, a}} {
		dev, _ := identity.ParseID(p[1].DeviceID())
		if s, ok := p[0].Sessions.Session(dev); ok && s.Alive() && s.Path().Transport != "" {
			return s.Path().Transport
		}
	}
	return ""
}

// syncs writes a file into from's folder and waits for it in to's.
func syncs(t *testing.T, from, to *sold.Daemon, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(from.Dir(), "SolDrive", name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	eventually(t, name+" synced", func() bool {
		b, err := os.ReadFile(filepath.Join(to.Dir(), "SolDrive", name))
		return err == nil && string(b) == content
	})
}

// TestWireGuardThroughNATAndTorFallback: two devices, each behind its own home router that
// drops unsolicited UDP, reach each other by punching (the session moves to WireGuard and a
// file syncs over it); with UDP blocked they fall back to Tor and keep syncing; when UDP
// comes back, the session returns to WireGuard after the probation.
func TestWireGuardThroughNATAndTorFallback(t *testing.T) {
	shortPaths(t)
	transport.SetWGLiveness(t, 1, 1500*time.Millisecond)
	fake := faultnet.NewFakeTor(t.TempDir())
	bi := builtins.All()
	node := func(o sold.InitOptions) (*sold.Daemon, *faultnet.NAT) {
		nat, err := faultnet.NewNAT()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(nat.Close)
		d := startNet(t, netNode{o: o, tor: fake.Provider(), builtins: bi, wrap: behindNAT(nat),
			transports: []sold.TransportConfig{{Name: "wg", Listen: "127.0.0.1:0", Advertise: []string{nat.Addr()}}, {Name: "tor"}}})
		waitOnion(t, d, time.Minute)
		return d, nat
	}
	laptop, natL := node(sold.InitOptions{Name: "Alice", Device: "laptop"})
	phone, natP := node(sold.InitOptions{Join: true, Device: "phone"})
	link, err := laptop.Invite(true)
	if err != nil {
		t.Fatal(err)
	}
	redeem(t, phone, link) // the wg path can't answer yet (the laptop doesn't know the key): Tor
	waitSession(t, laptop, phone)

	eventually(t, "the session on WireGuard", func() bool { return pathBetween(laptop, phone) == "wg" })
	if natL.Dropped()+natP.Dropped() == 0 {
		t.Fatal("the routers dropped nothing: the NAT is not filtering")
	}
	syncs(t, laptop, phone, "over-wg.txt", "punched through two NATs")

	// UDP blocked: WireGuard goes silent, the session falls back to Tor, and files still sync.
	natL.Block(true)
	eventually(t, "the fallback to Tor", func() bool { return pathBetween(laptop, phone) == "tor" })
	syncs(t, phone, laptop, "over-tor.txt", "the long way round")

	// UDP back: the wg path is probed, proves itself (probation) and the session moves back.
	natL.Block(false)
	eventually(t, "the session back on WireGuard", func() bool { return pathBetween(laptop, phone) == "wg" })
	syncs(t, laptop, phone, "back-on-wg.txt", "home again")
}

// tap records every byte a device receives over Tor (both the connections it dials and the
// ones its onion forwards to it). The fake tor carries the Sol handshake and sessions in the
// clear, so an address that reached the device shows up here.
type tap struct {
	mu   sync.Mutex
	seen []*bytes.Buffer
}

func (tp *tap) record() *bytes.Buffer {
	b := &bytes.Buffer{}
	tp.mu.Lock()
	tp.seen = append(tp.seen, b)
	tp.mu.Unlock()
	return b
}

// contains reports whether any connection carried s.
func (tp *tap) contains(s string) bool {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	for _, b := range tp.seen {
		if bytes.Contains(b.Bytes(), []byte(s)) {
			return true
		}
	}
	return false
}

// bytes counts what was recorded.
func (tp *tap) total() int {
	tp.mu.Lock()
	defer tp.mu.Unlock()
	n := 0
	for _, b := range tp.seen {
		n += b.Len()
	}
	return n
}

type tapConn struct {
	net.Conn
	mu  *sync.Mutex
	buf *bytes.Buffer
}

func (c *tapConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.mu.Lock()
	c.buf.Write(p[:n])
	c.mu.Unlock()
	return n, err
}

// tappedTor is the Tor transport with its traffic recorded (every other method, client auth
// included, is the real one).
type tappedTor struct {
	*transport.Tor
	tp *tap
}

func (t *tappedTor) Dial(ctx context.Context, addr string) (net.Conn, error) {
	c, err := t.Tor.Dial(ctx, addr)
	if err != nil {
		return nil, err
	}
	return &tapConn{Conn: c, mu: &t.tp.mu, buf: t.tp.record()}, nil
}

// Publish puts a recording forwarder in front of the target.
func (t *tappedTor) Publish(ctx context.Context, p transport.Publication) (string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	go func() {
		<-ctx.Done()
		ln.Close()
	}()
	target := p.Target
	go func() {
		for {
			in, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer in.Close()
				out, err := net.Dial("tcp", target)
				if err != nil {
					return
				}
				defer out.Close()
				go func() { _, _ = io.Copy(out, &tapConn{Conn: in, mu: &t.tp.mu, buf: t.tp.record()}); out.Close() }()
				_, _ = io.Copy(in, out)
			}()
		}
	}()
	p.Target = ln.Addr().String()
	return t.Tor.Publish(ctx, p)
}

func (tp *tap) wrap(tr transport.Transport) transport.Transport {
	if tor, ok := tr.(*transport.Tor); ok {
		return &tappedTor{Tor: tor, tp: tp}
	}
	return tr
}

// revealing lists every address of ds that is not an onion: what an anonymous contact must
// never learn.
func revealing(ds ...*sold.Daemon) []string {
	var out []string
	for _, d := range ds {
		for _, e := range d.Keyring.Endpoints().Endpoints {
			if !identity.AnonymousTransport(e.Transport) {
				out = append(out, e.Address)
			}
		}
	}
	return out
}

// onlyOnions fails unless every endpoint record of ds that seer holds lists onions only.
func onlyOnions(t *testing.T, seer *sold.Daemon, ds ...*sold.Daemon) {
	t.Helper()
	for _, d := range ds {
		person, _ := identity.ParseID(d.PersonID())
		dev, _ := identity.ParseID(d.DeviceID())
		rec, ok := seer.Cache.Endpoints(person, dev)
		if !ok {
			t.Fatalf("%s holds no endpoints of %s", seer.DeviceID(), d.DeviceID())
		}
		for _, e := range rec.Endpoints {
			if !identity.AnonymousTransport(e.Transport) {
				t.Fatalf("an anonymous contact holds a %s address: %+v", e.Transport, e)
			}
		}
	}
}

// heldSeq is the seq of d's endpoint record that seer holds.
func heldSeq(seer, d *sold.Daemon) uint64 {
	person, _ := identity.ParseID(d.PersonID())
	dev, _ := identity.ParseID(d.DeviceID())
	rec, _ := seer.Cache.Endpoints(person, dev)
	return rec.Seq
}

// dialDirect performs Bob's side of a session handshake with d over plain TCP to addr.
func dialDirect(bob, d *sold.Daemon, addr string) error {
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	b, err := bob.Keyring.Bundle()
	if err != nil {
		return err
	}
	want, _ := identity.ParseID(d.DeviceID())
	_, err = wire.Dial(conn, bob.Keyring, wire.Call{Op: wire.OpSession, Want: want, Bundle: b.Marshal()})
	return err
}

// TestAnonymousContact: an anonymous invite carries only the onion; the contact made with it
// is reached over Tor only, never receives a non-onion address of mine (not from this device,
// not from my other devices, not when my addresses change), and its direct dials are
// refused. Making it a buddy gives it my addresses; making it anonymous again closes direct
// sessions and sends onion-only records from then on.
func TestAnonymousContact(t *testing.T) {
	fake := faultnet.NewFakeTor(t.TempDir())
	both := []sold.TransportConfig{{Name: "clearnet", Listen: "127.0.0.1:0"}, {Name: "tor"}}
	laptop := startNet(t, netNode{o: sold.InitOptions{Name: "Alice", Device: "laptop"}, tor: fake.Provider(), transports: both})
	phone := startNet(t, netNode{o: sold.InitOptions{Join: true, Device: "phone"}, tor: fake.Provider(), transports: both})
	tp := &tap{}
	bob := startNet(t, netNode{o: sold.InitOptions{Name: "Bob", Device: "desktop"}, tor: fake.Provider(), wrap: tp.wrap,
		transports: []sold.TransportConfig{{Name: "tor"}}})
	for _, d := range []*sold.Daemon{laptop, phone, bob} {
		waitOnion(t, d, time.Minute)
	}
	join, err := laptop.Invite(true)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := phone.Pair(context.Background(), join); err != nil {
		t.Fatal(err)
	}
	waitSession(t, laptop, phone)

	// The anonymous link: the onion and nothing else.
	link, err := laptop.InviteAnonymous()
	if err != nil {
		t.Fatal(err)
	}
	l, err := pairing.ParseLink(link)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Endpoints.Endpoints) == 0 {
		t.Fatal("the anonymous link has no onion")
	}
	for _, e := range l.Endpoints.Endpoints {
		if e.Transport != "tor" {
			t.Fatalf("the anonymous link carries %+v", e)
		}
	}
	redeem(t, bob, link)
	bobID, _ := identity.ParseID(bob.PersonID())
	eventually(t, "Bob anonymous on both of Alice's devices", func() bool {
		return laptop.Contacts.Anonymous(bobID) && phone.Contacts.Anonymous(bobID)
	})
	waitSession(t, bob, laptop)
	waitSession(t, bob, phone) // the phone learned Bob from the laptop and dials him over Tor
	waitSession(t, phone, bob)

	// Alice's addresses change (a transport turned on): the new record reaches Bob onion-only.
	before := heldSeq(bob, laptop)
	if _, err := laptop.SetTransport("direct", true); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the new record at Bob", func() bool { return heldSeq(bob, laptop) > before })
	onlyOnions(t, bob, laptop, phone)
	if tp.total() == 0 {
		t.Fatal("the tap recorded nothing")
	}
	for _, a := range revealing(laptop, phone) {
		if tp.contains(a) {
			t.Fatalf("an address reached the anonymous contact: %s", a)
		}
	}
	for _, d := range []*sold.Daemon{laptop, phone} {
		dev, _ := identity.ParseID(d.DeviceID())
		if s, ok := bob.Sessions.Session(dev); ok && s.Path().Transport != "" && s.Path().Transport != "tor" {
			t.Fatalf("Bob reaches %s over %s", d.DeviceID(), s.Path().Transport)
		}
	}

	// Bob dialing Alice's clearnet address directly (as if he had learned it elsewhere) is
	// refused; Alice's own phone is welcome there.
	var clear string
	for _, e := range laptop.Keyring.Endpoints().Endpoints {
		if e.Transport == "clearnet" {
			clear = e.Address
		}
	}
	if err := dialDirect(bob, laptop, clear); err == nil {
		t.Fatal("the anonymous contact's direct dial was admitted")
	}
	if err := dialDirect(phone, laptop, clear); err != nil {
		t.Fatalf("my own device's direct dial: %v", err)
	}

	// A buddy now: Bob learns the addresses (which also shows the tap would have seen them).
	if _, err := laptop.SetAnonymous(bob.PersonID(), false); err != nil {
		t.Fatal(err)
	}
	eventually(t, "Alice's address at Bob", func() bool { return tp.contains(clear) })
	if err := dialDirect(bob, laptop, clear); err != nil {
		t.Fatalf("a buddy's direct dial: %v", err)
	}

	// Anonymous again: newer, onion-only records, and no direct path is used or admitted.
	seq := heldSeq(bob, laptop)
	if _, err := laptop.SetAnonymous(bob.PersonID(), true); err != nil {
		t.Fatal(err)
	}
	eventually(t, "an onion-only record newer than the full one", func() bool {
		return heldSeq(bob, laptop) > seq && heldSeq(bob, phone) > 0
	})
	eventually(t, "both of Alice's devices re-signed", func() bool {
		for _, d := range []*sold.Daemon{laptop, phone} {
			person, _ := identity.ParseID(d.PersonID())
			dev, _ := identity.ParseID(d.DeviceID())
			rec, _ := bob.Cache.Endpoints(person, dev)
			for _, e := range rec.Endpoints {
				if !identity.AnonymousTransport(e.Transport) {
					return false
				}
			}
		}
		return true
	})
	if err := dialDirect(bob, laptop, clear); err == nil {
		t.Fatal("a direct dial after switching back to anonymous was admitted")
	}
	waitSession(t, bob, laptop)
}

// testLog discards, unless SOL_TEST_LOG=1 (then: debug lines on stderr, tagged with name).
func testLog(name string) *slog.Logger {
	if os.Getenv("SOL_TEST_LOG") != "1" {
		return slog.New(slog.DiscardHandler)
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug})).With("node", name)
}

// TestAnonymousBothWays: the anonymous link says it is anonymous, so the side that redeems it
// treats the inviter as anonymous too. Each holds only onion-only records of the other, no
// non-onion address of either crosses Tor in either direction (not when an address changes
// either), and each refuses the other's direct dial.
func TestAnonymousBothWays(t *testing.T) {
	fake := faultnet.NewFakeTor(t.TempDir())
	both := []sold.TransportConfig{{Name: "clearnet", Listen: "127.0.0.1:0"}, {Name: "tor"}}
	tpA, tpB := &tap{}, &tap{}
	alice := startNet(t, netNode{o: sold.InitOptions{Name: "Alice", Device: "laptop"}, tor: fake.Provider(), wrap: tpA.wrap, transports: both})
	bob := startNet(t, netNode{o: sold.InitOptions{Name: "Bob", Device: "desktop"}, tor: fake.Provider(), wrap: tpB.wrap, transports: both})
	for _, d := range []*sold.Daemon{alice, bob} {
		waitOnion(t, d, time.Minute)
	}
	link, err := alice.InviteAnonymous()
	if err != nil {
		t.Fatal(err)
	}
	if l, err := pairing.ParseLink(link); err != nil || !l.Anonymous || len(link) > pairing.MaxLinkLen {
		t.Fatalf("%d chars, %+v, %v", len(link), l, err)
	}
	redeem(t, bob, link)
	aliceID, _ := identity.ParseID(alice.PersonID())
	bobID, _ := identity.ParseID(bob.PersonID())
	if !bob.Contacts.Anonymous(aliceID) {
		t.Fatal("Bob does not treat Alice as anonymous")
	}
	eventually(t, "Alice treats Bob as anonymous", func() bool { return alice.Contacts.Anonymous(bobID) })
	waitSession(t, bob, alice)
	waitSession(t, alice, bob)

	// Both change their addresses: the new records cross onion-only.
	seqA, seqB := heldSeq(bob, alice), heldSeq(alice, bob)
	for _, d := range []*sold.Daemon{alice, bob} {
		if _, err := d.SetTransport("direct", true); err != nil {
			t.Fatal(err)
		}
	}
	eventually(t, "the new records", func() bool { return heldSeq(bob, alice) > seqA && heldSeq(alice, bob) > seqB })
	onlyOnions(t, bob, alice)
	onlyOnions(t, alice, bob)
	if tpA.total() == 0 || tpB.total() == 0 {
		t.Fatal("a tap recorded nothing")
	}
	for _, a := range revealing(alice) {
		if tpB.contains(a) {
			t.Fatalf("Alice's address reached Bob: %s", a)
		}
	}
	for _, a := range revealing(bob) {
		if tpA.contains(a) {
			t.Fatalf("Bob's address reached Alice: %s", a)
		}
	}
	for _, p := range [][2]*sold.Daemon{{alice, bob}, {bob, alice}} {
		dev, _ := identity.ParseID(p[1].DeviceID())
		if s, ok := p[0].Sessions.Session(dev); ok && s.Path().Transport != "" && s.Path().Transport != "tor" {
			t.Fatalf("%s reaches %s over %s", p[0].DeviceID(), p[1].DeviceID(), s.Path().Transport)
		}
	}

	// Direct dials are refused both ways (as if the address had leaked elsewhere).
	clearOf := func(d *sold.Daemon) string {
		for _, e := range d.Keyring.Endpoints().Endpoints {
			if e.Transport == "clearnet" {
				return e.Address
			}
		}
		t.Fatalf("%s has no clearnet address", d.DeviceID())
		return ""
	}
	if err := dialDirect(alice, bob, clearOf(bob)); err == nil {
		t.Fatal("the inviter's direct dial was admitted by the anonymous redeemer")
	}
	if err := dialDirect(bob, alice, clearOf(alice)); err == nil {
		t.Fatal("the redeemer's direct dial was admitted by the anonymous inviter")
	}
}

// TestTunnelSurvivesPathMove: a port-sharing tunnel that is open while the session moves
// from Tor to WireGuard keeps carrying bytes, intact, through the move (the old session
// drains instead of closing, PROTOCOL.md §Sessions), and new tunnel connections and file sync
// go over the new path.
func TestTunnelSurvivesPathMove(t *testing.T) {
	shortPaths(t)
	transport.SetWGLiveness(t, 1, 1500*time.Millisecond)
	fake := faultnet.NewFakeTor(t.TempDir())
	bi := builtins.All()
	node := func(o sold.InitOptions) (*sold.Daemon, *faultnet.NAT) {
		nat, err := faultnet.NewNAT()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(nat.Close)
		d := startNet(t, netNode{o: o, tor: fake.Provider(), builtins: bi, wrap: behindNAT(nat),
			transports: []sold.TransportConfig{{Name: "wg", Listen: "127.0.0.1:0", Advertise: []string{nat.Addr()}}, {Name: "tor"}}})
		waitOnion(t, d, time.Minute)
		return d, nat
	}
	laptop, natL := node(sold.InitOptions{Name: "Alice", Device: "laptop"})
	phone, _ := node(sold.InitOptions{Join: true, Device: "phone"})
	natL.Block(true) // no UDP: the session stays on Tor until it is unblocked
	link, err := laptop.Invite(true)
	if err != nil {
		t.Fatal(err)
	}
	redeem(t, phone, link)
	waitSession(t, laptop, phone)
	eventually(t, "the session on Tor", func() bool { return pathBetween(laptop, phone) == "tor" })
	lapDev, _ := identity.ParseID(laptop.DeviceID())
	first, _ := phone.Sessions.Session(lapDev)

	port := echoServer(t)
	A, P := admin(t, laptop), admin(t, phone)
	if err := A("POST", "/admin/v1/services", sold.Service{Name: "echo", Port: port}, nil); err != nil {
		t.Fatal(err)
	}
	var tun sold.TunnelInfo
	eventually(t, "a tunnel to the laptop's service", func() bool {
		return P("POST", "/admin/v1/tunnels", map[string]any{"person": laptop.PersonID(), "service": "echo"}, &tun) == nil
	})
	conn, err := net.Dial("tcp", tun.Addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	br := bufio.NewReader(conn)
	big := strings.Repeat("0123456789abcdef", 64<<10/16) // 64 KiB a line
	trip := func(what string) {
		t.Helper()
		if got, err := roundTrip(conn, br, big); err != nil || got != big+"\n" {
			t.Fatalf("%s: round trip: %d bytes, %v", what, len(got), err)
		}
	}
	trip("before the move")

	// UDP back: the session moves to WireGuard while the tunnel keeps going.
	natL.Block(false)
	// Time, not a count of round trips: a fast machine finishes thousands before the path is
	// even probed again.
	deadline := time.Now().Add(60 * time.Second)
	for pathBetween(laptop, phone) != "wg" {
		trip("during the move")
		if time.Now().After(deadline) {
			t.Fatal("the session never moved to WireGuard")
		}
	}
	for range 20 {
		trip("after the move")
	}
	if cur, _ := phone.Sessions.Session(lapDev); cur == first {
		t.Fatal("the session did not move")
	}
	select {
	case <-first.Retired():
	default:
		t.Fatal("the Tor session is not retired")
	}
	if !first.Alive() {
		t.Fatal("the Tor session closed under the open tunnel")
	}

	// New connections ride the new session; the old one closes once the tunnel ends.
	c2, err := net.Dial("tcp", tun.Addr)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := roundTrip(c2, bufio.NewReader(c2), "new"); err != nil || got != "new\n" {
		t.Fatalf("new connection: %q %v", got, err)
	}
	c2.Close()
	conn.Close()
	eventually(t, "the drained Tor session to close", func() bool { return !first.Alive() })
	syncs(t, laptop, phone, "after-move.txt", "replication moved too")
}
