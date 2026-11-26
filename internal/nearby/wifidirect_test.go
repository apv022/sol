package nearby

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"
)

// An ad is recognised only by the devices it has entries for; the credentials of the group it
// offers reach them and nobody else, and nothing in it is a key or the credentials in clear.
func TestAdMatch(t *testing.T) {
	a, b, c := device(t), device(t), device(t)
	now := time.Unix(1_800_000_000, 0)
	g := NewGroup()
	if !strings.HasPrefix(g.SSID, "DIRECT-") || len(g.Pass) < 8 {
		t.Fatalf("group %+v", g)
	}
	txt, exp, err := EncodeAd([]PeerKey{peerKey(t, a, b.DevicePublic())}, a.DevicePublic(), &g, now)
	if err != nil {
		t.Fatal(err)
	}
	if !exp.After(now) || exp.Sub(now) > WindowLength {
		t.Fatalf("expires %v", exp)
	}
	if txt["v"] != AdVersion || txt["t0"] == "" {
		t.Fatalf("txt %v", txt)
	}
	for k, v := range txt {
		if len(v) > 255 || strings.Contains(v, g.Pass) || strings.Contains(v, strings.TrimPrefix(g.SSID, "DIRECT-")) {
			t.Fatalf("txt %s=%q is too long or carries the group in clear", k, v)
		}
	}
	raw, _ := b64.DecodeString(txt["t0"])
	if len(raw) != adPadTo*adEntrySize || bytes.Contains(raw, a.DevicePublic()[:8]) || bytes.Contains(raw, b.DevicePublic()[:8]) {
		t.Fatalf("raw ad: %d bytes, or it carries a key", len(raw))
	}
	got := NewAdTable([]PeerKey{peerKey(t, b, a.DevicePublic())}, now).Match(txt)
	if len(got) != 1 || !bytes.Equal(got[0].Device, a.DevicePublic()) || got[0].Group == nil || *got[0].Group != g {
		t.Fatalf("b found %+v", got)
	}
	if got := NewAdTable([]PeerKey{peerKey(t, c, a.DevicePublic())}, now).Match(txt); len(got) != 0 {
		t.Fatalf("c found %+v", got)
	}
	// Without a group: recognised, nothing offered.
	txt, _, _ = EncodeAd([]PeerKey{peerKey(t, a, b.DevicePublic())}, a.DevicePublic(), nil, now)
	if got := NewAdTable([]PeerKey{peerKey(t, b, a.DevicePublic())}, now).Match(txt); len(got) != 1 || got[0].Group != nil {
		t.Fatalf("b found %+v", got)
	}
	// Two windows away: not recognised; the ad's tags differ from the beacon's.
	if got := NewAdTable([]PeerKey{peerKey(t, b, a.DevicePublic())}, now.Add(2*WindowLength)).Match(txt); len(got) != 0 {
		t.Fatalf("a stale ad matched: %+v", got)
	}
	ab := peerKey(t, a, b.DevicePublic())
	bt, _ := derive(ab.Key, a.DevicePublic(), window(now))
	at, _ := deriveAd(ab.Key, a.DevicePublic(), window(now))
	if bt == at {
		t.Fatal("ad tag equals beacon tag")
	}
	// Malformed records and credentials.
	for _, bad := range []map[string]string{nil, {"v": "2", "t0": txt["t0"]}, {"v": "1", "t0": "!!"}, {"v": "1", "t0": txt["t0"][:10]}} {
		if got := NewAdTable([]PeerKey{peerKey(t, b, a.DevicePublic())}, now).Match(bad); len(got) != 0 {
			t.Fatalf("%v matched", bad)
		}
	}
	if _, _, err := EncodeAd(nil, a.DevicePublic(), &P2PGroup{SSID: "home", Pass: "x"}, now); !errors.Is(err, ErrBadGroup) {
		t.Fatalf("bad group: %v", err)
	}
}

// More peers than fit: the ad has AdMaxEntries, split into TXT values of at most 200 chars.
func TestAdLimits(t *testing.T) {
	a := device(t)
	var peers []PeerKey
	for range AdMaxEntries + 3 {
		peers = append(peers, peerKey(t, a, device(t).DevicePublic()))
	}
	txt, _, err := EncodeAd(peers, a.DevicePublic(), nil, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if n := len(decodeAd(txt)); n != AdMaxEntries {
		t.Fatalf("%d entries", n)
	}
	if len(txt) < 3 {
		t.Fatalf("txt not split: %v", txt)
	}
	for _, v := range txt {
		if len(v) > adChunk {
			t.Fatalf("value of %d chars", len(v))
		}
	}
}

// The lowest key leads, the other waits and then joins the leader's group; a connected device
// or a stranger is no reason to form one, and a device already in a group stays.
func TestDecide(t *testing.T) {
	x, y := device(t), device(t)
	lo, hi := x, y
	if bytes.Compare(x.DevicePublic(), y.DevicePublic()) > 0 {
		lo, hi = y, x
	}
	now := time.Now()
	never := func(ed25519.PublicKey) bool { return false }
	loT := NewAdTable([]PeerKey{peerKey(t, lo, hi.DevicePublic())}, now)
	hiT := NewAdTable([]PeerKey{peerKey(t, hi, lo.DevicePublic())}, now)
	ad := func(from, to interface {
		DevicePublic() ed25519.PublicKey
	}, k PeerKey, g *P2PGroup) AdSeen {
		txt, _, err := EncodeAd([]PeerKey{k}, from.DevicePublic(), g, now)
		if err != nil {
			t.Fatal(err)
		}
		return AdSeen{ID: "aa:bb", TXT: txt}
	}
	hiAd := ad(hi, lo, peerKey(t, hi, lo.DevicePublic()), nil)
	p := Decide(lo.DevicePublic(), "none", loT, []AdSeen{hiAd}, never)
	if p.Role != "lead" || p.Group == nil || len(p.Matched) != 1 {
		t.Fatalf("lo: %+v", p)
	}
	loAd := ad(lo, hi, peerKey(t, lo, hi.DevicePublic()), nil)
	if p := Decide(hi.DevicePublic(), "none", hiT, []AdSeen{loAd}, never); p.Role != "wait" {
		t.Fatalf("hi before the group: %+v", p)
	}
	loAd = ad(lo, hi, peerKey(t, lo, hi.DevicePublic()), p.Group)
	if q := Decide(hi.DevicePublic(), "none", hiT, []AdSeen{loAd}, never); q.Role != "join" || q.ID != "aa:bb" || *q.Group != *p.Group {
		t.Fatalf("hi: %+v", q)
	}
	if q := Decide(lo.DevicePublic(), "owner", loT, []AdSeen{hiAd}, never); q.Role != "stay" {
		t.Fatalf("owner: %+v", q)
	}
	always := func(ed25519.PublicKey) bool { return true }
	if q := Decide(lo.DevicePublic(), "none", loT, []AdSeen{hiAd}, always); q.Role != "none" || len(q.Matched) != 1 {
		t.Fatalf("connected: %+v", q)
	}
	stranger := device(t)
	sAd := ad(stranger, lo, peerKey(t, stranger, lo.DevicePublic()), nil)
	if q := Decide(lo.DevicePublic(), "none", loT, []AdSeen{sAd}, never); q.Role != "none" || len(q.Matched) != 0 {
		t.Fatalf("stranger: %+v", q)
	}
}

// A beacon socket opened by address (no interface listing) sends to and receives from the
// group on that interface. Needs a multicast-capable interface with an IPv4 address.
func TestUDPExtraAddress(t *testing.T) {
	ifs, err := net.Interfaces()
	if err != nil {
		t.Skip("no interface list:", err)
	}
	var addr netip.Addr
	for _, ifc := range ifs {
		if ok, as := usable(ifc); ok {
			addr = as[0]
			break
		}
	}
	if !addr.IsValid() {
		t.Skip("no multicast-capable interface")
	}
	c1, err := openAddr(addr)
	if errors.Is(err, errors.ErrUnsupported) {
		t.Skip("not on this OS")
	}
	if err != nil {
		t.Skip("cannot join the group here:", err)
	}
	defer c1.Close()
	c2, err := openAddr(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c2.Close()
	msg := []byte("SOLN-test-extra")
	if _, err := c1.WriteToUDPAddrPort(msg, groupAddr); err != nil {
		t.Skip("cannot send to the group here:", err)
	}
	c2.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 64)
	for {
		n, src, err := c2.ReadFromUDPAddrPort(buf)
		if err != nil {
			t.Skip("no multicast delivery here:", err)
		}
		if string(buf[:n]) == string(msg) {
			if src.Addr().Unmap() != addr {
				t.Fatalf("from %v, want %v", src, addr)
			}
			return
		}
	}
}

// UDP.Open in the Android fallback (interfaces can't be listed) still opens a socket per
// extra address and counts them.
func TestUDPOpenExtraInFallback(t *testing.T) {
	ifs, err := net.Interfaces()
	if err != nil {
		t.Skip(err)
	}
	var addr netip.Addr
	for _, ifc := range ifs {
		if ok, as := usable(ifc); ok {
			addr = as[0]
			break
		}
	}
	if !addr.IsValid() || !supported {
		t.Skip("no multicast-capable interface")
	}
	if _, err := openAddr(addr); errors.Is(err, errors.ErrUnsupported) {
		t.Skip("not on this OS")
	}
	ifacesForbidden.Store(true)
	defer ifacesForbidden.Store(false)
	c, err := UDP{Extra: func() []netip.Addr { return []netip.Addr{addr, addr, netip.MustParseAddr("127.0.0.1")} }}.Open()
	if err != nil {
		t.Skip("cannot open here:", err)
	}
	defer c.Close()
	if n := c.Interfaces(); n < 1 || n > 2 {
		t.Fatalf("interfaces %d", n)
	}
}
