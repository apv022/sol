package nearby

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"testing"
	"time"
)

// A BLE entry is 10 bytes (what a legacy advert leaves next to a 128-bit UUID), recognised only
// by the device it is for, and carries the PSM and the Wi-Fi Direct flag masked; entries pad to
// a multiple of 4, at most 8, and carry no key.
func TestBLEEntryFormat(t *testing.T) {
	a, b, c := device(t), device(t), device(t)
	now := time.Unix(1_800_000_000, 0)
	adv := BLEAdvert{PSM: 0x85, WifiDirect: true}
	es, exp := EncodeBLE([]PeerKey{peerKey(t, a, b.DevicePublic())}, a.DevicePublic(), adv, now)
	if len(es) != blePadTo || !exp.After(now) || exp.Sub(now) > WindowLength {
		t.Fatalf("%d entries, expires %v", len(es), exp)
	}
	found := 0
	tb := NewBLETable([]PeerKey{peerKey(t, b, a.DevicePublic())}, now)
	tc := NewBLETable([]PeerKey{peerKey(t, c, a.DevicePublic())}, now)
	for _, e := range es {
		if e[0] != BLEVersion || e[7] == adv.PSM && e[8] == 1 && e[9] == 0 {
			t.Fatalf("entry %x: wrong version, or the PSM in clear", e)
		}
		if bytes.Contains(e[:], a.DevicePublic()[:4]) || bytes.Contains(e[:], b.DevicePublic()[:4]) {
			t.Fatalf("entry %x carries a key", e)
		}
		if f, ok := tb.Match(e[:]); ok {
			found++
			if !bytes.Equal(f.Device, a.DevicePublic()) || f.BLEAdvert != adv {
				t.Fatalf("b found %+v", f)
			}
		}
		if f, ok := tc.Match(e[:]); ok {
			t.Fatalf("a stranger matched %+v", f)
		}
	}
	if found != 1 {
		t.Fatalf("b found %d entries", found)
	}
	// Bad sizes and versions match nothing.
	e := es[0]
	if _, ok := tb.Match(e[:9]); ok {
		t.Fatal("short entry matched")
	}
	e[0] = 2
	if _, ok := tb.Match(e[:]); ok {
		t.Fatal("another version matched")
	}
	// Many peers: capped at BLEMaxEntries.
	var many []PeerKey
	for range 11 {
		many = append(many, peerKey(t, a, device(t).DevicePublic()))
	}
	if es, _ := EncodeBLE(many, a.DevicePublic(), adv, now); len(es) != BLEMaxEntries {
		t.Fatalf("%d entries for 11 peers", len(es))
	}
	if es, _ := EncodeBLE(nil, a.DevicePublic(), adv, now); len(es) != 0 {
		t.Fatalf("entries with no peers: %d", len(es))
	}
}

// Entries rotate with the window (the neighbours are accepted, further ones not), the two
// directions of a pair differ, and the "ble" label keeps them apart from beacons and ads.
func TestBLEPrivacy(t *testing.T) {
	a, b := device(t), device(t)
	now := time.Unix(1_800_000_000, 0)
	ab, ba := peerKey(t, a, b.DevicePublic()), peerKey(t, b, a.DevicePublic())
	w := window(now)
	t1, _ := deriveBLE(ab.Key, a.DevicePublic(), w)
	t2, _ := deriveBLE(ab.Key, a.DevicePublic(), w+1)
	t3, _ := deriveBLE(ba.Key, b.DevicePublic(), w)
	bt, _ := derive(ab.Key, a.DevicePublic(), w)
	at, _ := deriveAd(ab.Key, a.DevicePublic(), w)
	if t1 == t2 || t1 == t3 || bytes.Equal(t1[:], bt[:bleTagSize]) || bytes.Equal(t1[:], at[:bleTagSize]) {
		t.Fatal("tags repeat across windows, directions or labels")
	}
	one := func(at time.Time) [BLEEntrySize]byte {
		es, _ := EncodeBLE([]PeerKey{ab}, a.DevicePublic(), BLEAdvert{PSM: 0x90}, at)
		for _, e := range es {
			if _, ok := NewBLETable([]PeerKey{ba}, at).Match(e[:]); ok {
				return e
			}
		}
		t.Fatal("no entry")
		return [BLEEntrySize]byte{}
	}
	e := one(now)
	if e2 := one(now.Add(WindowLength)); e2 == e {
		t.Fatal("the entry did not rotate")
	}
	if _, ok := NewBLETable([]PeerKey{ba}, now.Add(WindowLength)).Match(e[:]); !ok {
		t.Fatal("the next window refused the entry")
	}
	if _, ok := NewBLETable([]PeerKey{ba}, now.Add(2*WindowLength)).Match(e[:]); ok {
		t.Fatal("a stale entry matched")
	}
}

// The decision: nothing for a device on an IP path; Wi-Fi Direct first when both have it (for
// BLEWDGrace, at most every BLEWDEvery); the lower key connects at once, the higher one after
// BLEWait; links at most every BLEConnectEvery; an upgrade kick for a device only on BLE; scan
// while any kept device lacks an IP path.
func TestBLEDecide(t *testing.T) {
	a, b := device(t), device(t)
	lo, hi := a, b
	if bytes.Compare(a.DevicePublic(), b.DevicePublic()) > 0 {
		lo, hi = b, a
	}
	now := time.Unix(1_800_000_000, 0)
	seenFrom := func(from, to interface {
		DevicePublic() ed25519.PublicKey
	}, k PeerKey, adv BLEAdvert, at time.Time) []BLESeen {
		es, _ := EncodeBLE([]PeerKey{k}, from.DevicePublic(), adv, at)
		s := BLESeen{ID: "dev"}
		for _, e := range es {
			s.Data = append(s.Data, base64.StdEncoding.EncodeToString(e[:]))
		}
		return []BLESeen{s, {ID: "junk", Data: []string{"!!", base64.StdEncoding.EncodeToString(make([]byte, 10))}}}
	}
	loKeyHi, hiKeyLo := peerKey(t, lo, hi.DevicePublic()), peerKey(t, hi, lo.DevicePublic())
	tLo := NewBLETable([]PeerKey{loKeyHi}, now) // lo's table: what hi sends
	tHi := NewBLETable([]PeerKey{hiKeyLo}, now)
	none := []BLEPeer{{Device: hi.DevicePublic()}}
	adv := BLEAdvert{PSM: 0x81}

	var dl BLEDecider
	p := dl.Decide(lo.DevicePublic(), tLo, none, seenFrom(hi, lo, hiKeyLo, adv, now), false, now)
	if !p.Scan || len(p.Matched) != 1 || p.Matched[0] != "dev" || len(p.Actions) != 1 || p.Actions[0].Do != "connect" || p.Actions[0].PSM != 0x81 {
		t.Fatalf("lower key: %+v", p)
	}
	if p := dl.Decide(lo.DevicePublic(), tLo, none, seenFrom(hi, lo, hiKeyLo, adv, now), false, now.Add(time.Second)); len(p.Actions) != 0 {
		t.Fatalf("connect repeated at once: %+v", p)
	}
	if p := dl.Decide(lo.DevicePublic(), tLo, none, seenFrom(hi, lo, hiKeyLo, adv, now), false, now.Add(BLEConnectEvery)); len(p.Actions) != 1 {
		t.Fatalf("no connect after BLEConnectEvery: %+v", p)
	}
	ip := []BLEPeer{{Device: hi.DevicePublic(), IP: true, Any: true}}
	if p := dl.Decide(lo.DevicePublic(), tLo, ip, seenFrom(hi, lo, hiKeyLo, adv, now), true, now.Add(time.Hour)); p.Scan || len(p.Actions) != 0 || len(p.Matched) != 1 {
		t.Fatalf("on an IP path: %+v", p)
	}

	var dh BLEDecider
	noneLo := []BLEPeer{{Device: lo.DevicePublic()}}
	if p := dh.Decide(hi.DevicePublic(), tHi, noneLo, seenFrom(lo, hi, loKeyHi, adv, now), false, now); len(p.Actions) != 0 {
		t.Fatalf("higher key connected at once: %+v", p)
	}
	if p := dh.Decide(hi.DevicePublic(), tHi, noneLo, seenFrom(lo, hi, loKeyHi, adv, now), false, now.Add(BLEWait)); len(p.Actions) != 1 || p.Actions[0].Do != "connect" {
		t.Fatalf("higher key after BLEWait: %+v", p)
	}
	// No PSM: nothing to connect to.
	var dz BLEDecider
	if p := dz.Decide(lo.DevicePublic(), tLo, none, seenFrom(hi, lo, hiKeyLo, BLEAdvert{}, now), false, now); len(p.Actions) != 0 {
		t.Fatalf("no PSM: %+v", p)
	}

	// Both have Wi-Fi Direct: kicked first, at most every BLEWDEvery, then a link after the grace.
	var dw BLEDecider
	wd := BLEAdvert{PSM: 0x81, WifiDirect: true}
	at := now
	if p := dw.Decide(lo.DevicePublic(), tLo, none, seenFrom(hi, lo, hiKeyLo, wd, at), true, at); len(p.Actions) != 1 || p.Actions[0].Do != "wifi-direct" {
		t.Fatalf("wifi-direct: %+v", p)
	}
	if p := dw.Decide(lo.DevicePublic(), tLo, none, seenFrom(hi, lo, hiKeyLo, wd, at), true, at.Add(time.Second)); len(p.Actions) != 0 {
		t.Fatalf("kicked again at once: %+v", p)
	}
	if p := dw.Decide(lo.DevicePublic(), tLo, none, seenFrom(hi, lo, hiKeyLo, wd, at), true, at.Add(BLEWDEvery)); len(p.Actions) != 1 || p.Actions[0].Do != "wifi-direct" {
		t.Fatalf("no second kick: %+v", p)
	}
	if p := dw.Decide(lo.DevicePublic(), tLo, none, seenFrom(hi, lo, hiKeyLo, wd, at), true, at.Add(BLEWDGrace)); len(p.Actions) != 1 || p.Actions[0].Do != "connect" {
		t.Fatalf("no link after the grace: %+v", p)
	}
	// Only the peer has Wi-Fi Direct: straight to a link.
	var dx BLEDecider
	if p := dx.Decide(lo.DevicePublic(), tLo, none, seenFrom(hi, lo, hiKeyLo, wd, at), false, at); len(p.Actions) != 1 || p.Actions[0].Do != "connect" {
		t.Fatalf("one-sided Wi-Fi Direct: %+v", p)
	}
	// A session only over BLE: no new link, a Wi-Fi Direct kick every BLEWDUpgrade.
	bleOnly := []BLEPeer{{Device: hi.DevicePublic(), Any: true}}
	var du BLEDecider
	if p := du.Decide(lo.DevicePublic(), tLo, bleOnly, seenFrom(hi, lo, hiKeyLo, wd, at), true, at); !p.Scan || len(p.Actions) != 1 || p.Actions[0].Do != "wifi-direct" {
		t.Fatalf("upgrade kick: %+v", p)
	}
	if p := du.Decide(lo.DevicePublic(), tLo, bleOnly, seenFrom(hi, lo, hiKeyLo, wd, at), true, at.Add(BLEWDEvery)); len(p.Actions) != 0 {
		t.Fatalf("upgrade kick too soon: %+v", p)
	}
	if p := du.Decide(lo.DevicePublic(), tLo, bleOnly, seenFrom(hi, lo, hiKeyLo, adv, at), false, at.Add(time.Hour)); len(p.Actions) != 0 {
		t.Fatalf("a second link over BLE: %+v", p)
	}
	// A device that is not kept (left out of peers) is matched but never acted on.
	var dn BLEDecider
	if p := dn.Decide(lo.DevicePublic(), tLo, nil, seenFrom(hi, lo, hiKeyLo, adv, at), false, at); p.Scan || len(p.Actions) != 0 {
		t.Fatalf("not kept: %+v", p)
	}
}
