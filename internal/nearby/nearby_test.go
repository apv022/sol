package nearby

import (
	"bytes"
	"crypto/ed25519"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/apv022/sol/internal/identity"
)

func device(t *testing.T) *identity.Keyring {
	t.Helper()
	k, err := identity.NewDevice(t.TempDir(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// peerKey is what kr holds for peer.
func peerKey(t *testing.T, kr *identity.Keyring, peer ed25519.PublicKey) PeerKey {
	t.Helper()
	k, err := kr.NearbyKey(peer)
	if err != nil {
		t.Fatal(err)
	}
	return PeerKey{Device: peer, Key: k}
}

// The peer a beacon has an entry for recognises it and reads the ports; a device it has no
// entry for (an anonymous contact, a stranger) recognises nothing.
func TestBeaconMatch(t *testing.T) {
	a, b, c := device(t), device(t), device(t)
	now := time.Now()
	ports := Ports{WG: 7001, Direct: 7002}
	ds := Encode([]PeerKey{peerKey(t, a, b.DevicePublic())}, a.DevicePublic(), ports, now)
	if len(ds) != 1 || len(ds[0]) != headerSize+padTo*entrySize {
		t.Fatalf("datagrams: %d, %d bytes", len(ds), len(ds[0]))
	}
	bt := NewTable([]PeerKey{peerKey(t, b, a.DevicePublic())}, now)
	got := bt.Match(ds[0])
	if len(got) != 1 || !bytes.Equal(got[0].Device, a.DevicePublic()) || got[0].Ports != ports {
		t.Fatalf("b found %+v", got)
	}
	ct := NewTable([]PeerKey{peerKey(t, c, a.DevicePublic())}, now)
	if got := ct.Match(ds[0]); len(got) != 0 {
		t.Fatalf("c, with no entry, found %+v", got)
	}
	// The beacon has no device key in it (the ports are masked: TestBeaconUnlinkable).
	for _, needle := range [][]byte{a.DevicePublic(), b.DevicePublic()} {
		if bytes.Contains(ds[0][headerSize:], needle) {
			t.Fatalf("beacon contains %x", needle)
		}
	}
	// A device does not recognise its own beacon (its table holds what peers send it).
	at := NewTable([]PeerKey{peerKey(t, a, b.DevicePublic())}, now)
	if got := at.Match(ds[0]); len(got) != 0 {
		t.Fatalf("a recognised its own beacon: %+v", got)
	}
}

// Beacons from different windows share nothing a passive observer could link, and the two
// directions of a pair differ; adjacent windows are accepted, farther ones are not.
func TestBeaconUnlinkable(t *testing.T) {
	a, b := device(t), device(t)
	ab, ba := peerKey(t, a, b.DevicePublic()), peerKey(t, b, a.DevicePublic())
	ports := Ports{WG: 7001, Direct: 7001}
	t0 := time.Unix(1_800_000_000, 0)
	entries := func(at time.Time, pk PeerKey, self ed25519.PublicKey) map[[entrySize]byte]bool {
		out := map[[entrySize]byte]bool{}
		for _, d := range Encode([]PeerKey{pk}, self, ports, at) {
			es, err := decode(d)
			if err != nil {
				t.Fatal(err)
			}
			for _, e := range es {
				out[e] = true
			}
		}
		return out
	}
	tags := func(m map[[entrySize]byte]bool) map[[tagSize]byte]bool {
		out := map[[tagSize]byte]bool{}
		for e := range m {
			var tg [tagSize]byte
			copy(tg[:], e[:])
			out[tg] = true
		}
		return out
	}
	e1 := entries(t0, ab, a.DevicePublic())
	for _, other := range []map[[entrySize]byte]bool{
		entries(t0.Add(WindowLength), ab, a.DevicePublic()),   // the next window
		entries(t0, ba, b.DevicePublic()),                     // the other direction
		entries(t0.Add(2*WindowLength), ab, a.DevicePublic()), // later
	} {
		t1, t2 := tags(e1), tags(other)
		for tg := range t1 {
			if t2[tg] {
				t.Fatal("two beacons share a tag")
			}
		}
		for e := range e1 {
			for o := range other {
				if bytes.Equal(e[tagSize:], o[tagSize:]) {
					t.Fatal("two beacons share masked ports")
				}
			}
		}
	}
	bd := Encode([]PeerKey{ab}, a.DevicePublic(), ports, t0)[0]
	for _, c := range []struct {
		at   time.Time
		want bool
	}{{t0, true}, {t0.Add(WindowLength), true}, {t0.Add(-WindowLength), true},
		{t0.Add(2 * WindowLength), false}, {t0.Add(-2 * WindowLength), false}} {
		got := NewTable([]PeerKey{ba}, c.at).Match(bd)
		if (len(got) == 1) != c.want {
			t.Errorf("receiver at %v: found %v, want %v", c.at.Sub(t0), got, c.want)
		}
	}
}

// Many peers take several datagrams, each padded to a multiple of 8 entries and at most 64;
// every peer is still recognised; malformed datagrams are refused.
func TestBeaconChunks(t *testing.T) {
	a := device(t)
	now := time.Now()
	var send, recv []PeerKey
	peers := map[string]*identity.Keyring{}
	for range 70 {
		p := device(t)
		peers[string(p.DevicePublic())] = p
		send = append(send, peerKey(t, a, p.DevicePublic()))
	}
	ds := Encode(send, a.DevicePublic(), Ports{Direct: 1}, now)
	if len(ds) != 2 {
		t.Fatalf("%d datagrams", len(ds))
	}
	for _, d := range ds {
		n := int(d[len(magic)+1])
		if n%padTo != 0 || n > maxEntries || len(d) > MaxDatagram {
			t.Fatalf("a datagram with %d entries, %d bytes", n, len(d))
		}
	}
	found := 0
	for _, p := range peers {
		recv = append(recv[:0], peerKey(t, p, a.DevicePublic()))
		tb := NewTable(recv, now)
		for _, d := range ds {
			found += len(tb.Match(d))
		}
	}
	if found != 70 {
		t.Fatalf("%d of 70 peers recognised", found)
	}
	if Encode(nil, a.DevicePublic(), Ports{Direct: 1}, now) != nil {
		t.Fatal("a beacon with no peers")
	}
	good := ds[1]
	for _, bad := range [][]byte{nil, []byte("SOLN"), append([]byte("SOLX"), good[4:]...), good[:len(good)-1],
		append(append([]byte{}, good[:4]...), append([]byte{2}, good[5:]...)...)} {
		if _, err := decode(bad); err == nil {
			t.Fatalf("decoded %x", bad)
		}
	}
}

// fakePeers is a Config.Peers that tests can change.
type fakePeers struct {
	mu sync.Mutex
	ps []Peer
}

func (f *fakePeers) get() []Peer {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Peer(nil), f.ps...)
}

type found struct {
	dev   ed25519.PublicKey
	src   netip.Addr
	ports Ports
}

func service(t *testing.T, kr *identity.Keyring, net Network, ports Ports, peers *fakePeers) (*Service, chan found) {
	t.Helper()
	ch := make(chan found, 16)
	s := New(Config{Self: kr.DevicePublic(), Key: kr.NearbyKey, Peers: peers.get,
		Ports: func() Ports { return ports }, Net: net, AllowLoopback: true,
		Found: func(d ed25519.PublicKey, src netip.Addr, p Ports) {
			select {
			case ch <- found{d, src, p}:
			default:
			}
		}})
	t.Cleanup(s.Close)
	return s, ch
}

func waitFound(t *testing.T, ch chan found, want ed25519.PublicKey) found {
	t.Helper()
	deadline := time.After(10 * time.Second)
	for {
		select {
		case f := <-ch:
			if bytes.Equal(f.dev, want) {
				return f
			}
		case <-deadline:
			t.Fatal("not found")
		}
	}
}

// Two services on a bus find each other (with source address and ports); a third that is a
// peer of neither (say an anonymous contact, which the session layer leaves out) recognises
// nothing; a service that is off sends nothing, and one without a LAN port stays idle.
func TestServiceBus(t *testing.T) {
	bus := NewBus()
	a, b, c := device(t), device(t), device(t)
	srcA, srcB := netip.MustParseAddr("192.168.1.10"), netip.MustParseAddr("192.168.1.11")
	na, nb, nc := bus.Network(srcA), bus.Network(srcB), bus.Network(netip.MustParseAddr("192.168.1.12"))
	pa := &fakePeers{ps: []Peer{{Device: b.DevicePublic()}}}
	pb := &fakePeers{ps: []Peer{{Device: a.DevicePublic()}}}
	pc := &fakePeers{} // a, b are not its peers, nor it theirs
	sa, cha := service(t, a, na, Ports{WG: 7001, Direct: 7001}, pa)
	sb, chb := service(t, b, nb, Ports{Clearnet: 9000}, pb)
	sc, chc := service(t, c, nc, Ports{Direct: 1}, pc)
	sc.SetEnabled(true)
	sa.SetEnabled(true)
	sb.SetEnabled(true) // b starts last, so a hears b's first beacon and replies with its own
	f := waitFound(t, cha, b.DevicePublic())
	if f.src != srcB || f.ports != (Ports{Clearnet: 9000}) {
		t.Fatalf("a found %+v", f)
	}
	f = waitFound(t, chb, a.DevicePublic())
	if f.src != srcA || f.ports != (Ports{WG: 7001, Direct: 7001}) {
		t.Fatalf("b found %+v", f)
	}
	if st := sa.Status(); st.State != "up" || st.Seen != 1 {
		t.Fatalf("a's status %+v", st)
	}
	select {
	case f := <-chc:
		t.Fatalf("c recognised %+v", f)
	case <-time.After(100 * time.Millisecond):
	}

	// Off: the sockets close and nothing more is sent, even when asked.
	sa.SetEnabled(false)
	n := na.Sent()
	sa.Announce()
	sa.NetworkChanged()
	time.Sleep(100 * time.Millisecond)
	if na.Sent() != n {
		t.Fatal("a beacon went out while nearby was off")
	}
	if st := sa.Status(); st.State != "off" || st.Enabled {
		t.Fatalf("status while off: %+v", st)
	}

	// No port on the LAN: idle, nothing sent.
	nd := bus.Network(netip.MustParseAddr("192.168.1.13"))
	sd, _ := service(t, device(t), nd, Ports{}, &fakePeers{ps: []Peer{{Device: a.DevicePublic()}}})
	sd.SetEnabled(true)
	sd.Announce()
	time.Sleep(100 * time.Millisecond)
	if nd.Sent() != 0 || sd.Status().State != "idle" {
		t.Fatalf("no LAN port: sent %d, status %+v", nd.Sent(), sd.Status())
	}
}

// A service never processes beacons from sources that can't be on the link, and drops copies.
func TestServiceSources(t *testing.T) {
	bus := NewBus()
	a, b := device(t), device(t)
	ch := make(chan found, 16)
	s := New(Config{Self: a.DevicePublic(), Key: a.NearbyKey,
		Peers: func() []Peer { return []Peer{{Device: b.DevicePublic(), Connected: true}} },
		Ports: func() Ports { return Ports{Direct: 1} }, Net: bus.Network(netip.MustParseAddr("10.0.0.1")),
		Found: func(d ed25519.PublicKey, src netip.Addr, p Ports) { ch <- found{d, src, p} }})
	defer s.Close()
	s.SetEnabled(true)
	d := Encode([]PeerKey{peerKey(t, b, a.DevicePublic())}, b.DevicePublic(), Ports{Direct: 2}, time.Now())[0]
	for _, src := range []string{"127.0.0.1", "169.254.1.1", "::1", "fe80::1", "2001:db8::1", "0.0.0.0", "224.0.0.1"} {
		bus.Inject(d, netip.MustParseAddr(src))
	}
	bus.Inject(d, netip.MustParseAddr("10.0.0.2"))
	bus.Inject(d, netip.MustParseAddr("10.0.0.2")) // a copy from another interface's socket
	f := waitFound(t, ch, b.DevicePublic())
	if f.src != netip.MustParseAddr("10.0.0.2") {
		t.Fatalf("found from %v", f.src)
	}
	select {
	case f := <-ch:
		t.Fatalf("also found %+v", f)
	case <-time.After(100 * time.Millisecond):
	}
}

// Real multicast sockets: one conn hears another's datagram on this machine (loopback is on).
// Skipped where the machine has no multicast-capable interface or the network drops it (some
// CI containers).
func TestUDPMulticast(t *testing.T) {
	c1, err := UDP{}.Open()
	if err != nil {
		t.Skipf("no multicast here: %v", err)
	}
	defer c1.Close()
	c2, err := UDP{}.Open()
	if err != nil {
		t.Skipf("no multicast here: %v", err)
	}
	defer c2.Close()
	a, b := device(t), device(t)
	d := Encode([]PeerKey{peerKey(t, a, b.DevicePublic())}, a.DevicePublic(), Ports{Direct: 4242}, time.Now())[0]
	got := make(chan Found, 1)
	go func() {
		tb := NewTable([]PeerKey{peerKey(t, b, a.DevicePublic())}, time.Now())
		buf := make([]byte, MaxDatagram+1)
		for {
			n, _, err := c2.Receive(buf)
			if err != nil {
				return
			}
			if fs := tb.Match(buf[:n]); len(fs) == 1 {
				got <- fs[0]
				return
			}
		}
	}()
	deadline := time.After(3 * time.Second)
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		if err := c1.Send(d); err != nil {
			t.Skipf("can't send multicast here: %v", err)
		}
		select {
		case f := <-got:
			if f.Ports.Direct != 4242 {
				t.Fatalf("found %+v", f)
			}
			return
		case <-deadline:
			t.Skip("multicast loopback delivered nothing here")
		case <-tick.C:
		}
	}
}
