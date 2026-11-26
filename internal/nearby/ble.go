package nearby

// Bluetooth LE adverts (design/PROTOCOL.md §Nearby "Bluetooth LE (Android, nearby step 3)"): the
// 10-byte service data entries the Android shell advertises under Sol's service UUID, and the
// decision what to do once entries of kept devices are seen (trigger Wi-Fi Direct, or open an
// L2CAP link). The shell never sees a key: it passes entries in and gets actions out.

import (
	"bytes"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	mrand "math/rand/v2"
	"sync"
	"time"
)

// The advert format.
const (
	// BLEServiceUUID is Sol's 128-bit service UUID: the service data the entries travel under,
	// and what the shell's scan filter looks for.
	BLEServiceUUID = "36360d3f-85e7-4adb-8ab0-1a8849b5fa1b"
	// BLEVersion is an entry's first byte.
	BLEVersion = 1
	// BLEEntrySize is one entry: what a legacy advert has room for next to Sol's UUID.
	BLEEntrySize = 10
	// BLEMaxEntries bounds the entries of one advert rotation.
	BLEMaxEntries = 8

	bleTagSize = 6
	blePadTo   = 4
	bleFlagWD  = 1 // flags bit 0: Wi-Fi Direct is on and working here
)

// BLE decision timings (package variables so tests can shorten them).
var (
	// BLEWDGrace is how long after a device without a session is first seen Wi-Fi Direct is
	// given before an L2CAP link is opened instead.
	BLEWDGrace = time.Minute
	// BLEWDEvery bounds how often Wi-Fi Direct is kicked for one device.
	BLEWDEvery = 30 * time.Second
	// BLEWDUpgrade is how often Wi-Fi Direct is kicked for a device whose only session runs over
	// BLE.
	BLEWDUpgrade = 2 * time.Minute
	// BLEWait is how long the device with the higher key waits for the other to connect before
	// it connects itself.
	BLEWait = 20 * time.Second
	// BLEConnectEvery bounds how often a link is opened to one device.
	BLEConnectEvery = 30 * time.Second
	// BLEForget drops what is remembered of a device not seen for this long.
	BLEForget = 5 * time.Minute
)

// deriveBLE computes the tag and mask of sender's entry for its peer in window w:
// HMAC-SHA256(K, "ble" | D_sender | u64 w). Its own label keeps adverts, beacons and Wi-Fi
// Direct ads unlinkable.
func deriveBLE(key [32]byte, sender ed25519.PublicKey, w uint64) (tag [bleTagSize]byte, mask [3]byte) {
	m := hmac.New(sha256.New, key[:])
	m.Write([]byte("ble"))
	m.Write(sender)
	var wb [8]byte
	binary.BigEndian.PutUint64(wb[:], w)
	m.Write(wb[:])
	out := m.Sum(nil)
	copy(tag[:], out[:bleTagSize])
	copy(mask[:], out[bleTagSize:bleTagSize+3])
	return tag, mask
}

// BLEAdvert is what an entry tells the peer it is for, masked: the LE PSM this phone's L2CAP
// server listens on (0x80-0xFF; 0: none) and whether its Wi-Fi Direct works.
type BLEAdvert struct {
	PSM        uint8
	WifiDirect bool
}

// EncodeBLE builds the entries self advertises at now, one per peer (the first BLEMaxEntries:
// the caller orders them, those without a session over an IP path first), padded with random
// entries to a multiple of 4 and shuffled. It also returns when the window ends. No peers: no
// entries (an advert nobody can recognise only says Sol is here).
func EncodeBLE(peers []PeerKey, self ed25519.PublicKey, a BLEAdvert, now time.Time) ([][BLEEntrySize]byte, time.Time) {
	w := window(now)
	expires := time.Unix(int64((w+1)*uint64(WindowLength/time.Second)), 0)
	if len(peers) == 0 {
		return nil, expires
	}
	if len(peers) > BLEMaxEntries {
		peers = peers[:BLEMaxEntries]
	}
	var flags byte
	if a.WifiDirect {
		flags |= bleFlagWD
	}
	n := (len(peers) + blePadTo - 1) / blePadTo * blePadTo
	es := make([][BLEEntrySize]byte, n)
	for i := range es {
		es[i][0] = BLEVersion
		if i >= len(peers) {
			rand.Read(es[i][1:])
			continue
		}
		tag, mask := deriveBLE(peers[i].Key, self, w)
		copy(es[i][1:1+bleTagSize], tag[:])
		es[i][7] = a.PSM ^ mask[0]
		es[i][8] = flags ^ mask[1]
		es[i][9] = mask[2]
	}
	mrand.Shuffle(n, func(i, j int) { es[i], es[j] = es[j], es[i] })
	return es, expires
}

// BLEFound is a kept device recognised in an entry.
type BLEFound struct {
	Device ed25519.PublicKey
	BLEAdvert
}

type bleExpect struct {
	device ed25519.PublicKey
	mask   [3]byte
}

// BLETable holds the entry tags this device expects from its peers around one moment.
type BLETable struct {
	tags map[[bleTagSize]byte]bleExpect
}

// NewBLETable precomputes the tags peers' entries carry in the window of now and the two next to
// it.
func NewBLETable(peers []PeerKey, now time.Time) *BLETable {
	w := window(now)
	t := &BLETable{tags: make(map[[bleTagSize]byte]bleExpect, 3*len(peers))}
	for _, p := range peers {
		for _, ww := range []uint64{w - 1, w, w + 1} {
			tag, mask := deriveBLE(p.Key, p.Device, ww)
			t.tags[tag] = bleExpect{device: p.Device, mask: mask}
		}
	}
	return t
}

// Match returns the device an entry is from, if it is a kept device's.
func (t *BLETable) Match(e []byte) (BLEFound, bool) {
	if len(e) != BLEEntrySize || e[0] != BLEVersion {
		return BLEFound{}, false
	}
	var tag [bleTagSize]byte
	copy(tag[:], e[1:1+bleTagSize])
	x, ok := t.tags[tag]
	if !ok {
		return BLEFound{}, false
	}
	psm, flags, zero := e[7]^x.mask[0], e[8]^x.mask[1], e[9]^x.mask[2]
	if zero != 0 || flags&^bleFlagWD != 0 {
		return BLEFound{}, false // a collision with a random entry, or a newer format
	}
	return BLEFound{Device: x.device, BLEAdvert: BLEAdvert{PSM: psm, WifiDirect: flags&bleFlagWD != 0}}, true
}

// BLESeen is what the shell saw from one advertiser: ID is its opaque handle, echoed back; Data
// the entries (standard base64) it sent lately.
type BLESeen struct {
	ID   string   `json:"id"`
	Data []string `json:"data"`
}

// BLEAction is what the shell should do for one advertiser: Do is "wifi-direct" (make Wi-Fi
// Direct advertise and discover now) or "connect" (open an L2CAP channel to PSM and bridge it
// with Link).
type BLEAction struct {
	ID     string            `json:"id"`
	Do     string            `json:"do"`
	PSM    int               `json:"psm,omitempty"`
	Link   string            `json:"link,omitempty"`
	Device ed25519.PublicKey `json:"-"`
}

// BLEPlan is the reply to the shell's matches: the actions, the IDs whose entries carried a kept
// device, and whether to keep scanning.
type BLEPlan struct {
	Actions []BLEAction `json:"actions"`
	Matched []string    `json:"matched"`
	Scan    bool        `json:"scan"`
}

// BLEPeer is what the decision needs to know of a kept device's sessions.
type BLEPeer struct {
	Device ed25519.PublicKey
	// IP: a live session over an IP path. Any: a live session over any path, BLE included.
	IP, Any bool
}

// BLEDecider remembers, per device, when it was first seen and when Wi-Fi Direct was kicked or a
// link opened for it, so a decision repeated every few seconds acts at a bounded rate. Safe for
// concurrent use.
type BLEDecider struct {
	mu   sync.Mutex
	devs map[string]*bleDev
}

type bleDev struct {
	first, last, wd, conn time.Time
}

// Decide turns the entries seen into a plan. wd says whether this phone's Wi-Fi Direct is on
// and working. The device with the lower key connects; the other waits BLEWait for it.
func (b *BLEDecider) Decide(self ed25519.PublicKey, table *BLETable, peers []BLEPeer, seen []BLESeen, wd bool, now time.Time) BLEPlan {
	p := BLEPlan{Actions: []BLEAction{}, Matched: []string{}}
	state := make(map[string]BLEPeer, len(peers))
	for _, pr := range peers {
		state[string(pr.Device)] = pr
		p.Scan = p.Scan || !pr.IP
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.devs == nil {
		b.devs = map[string]*bleDev{}
	}
	for k, d := range b.devs {
		if now.Sub(d.last) >= BLEForget {
			delete(b.devs, k)
		}
	}
	done := map[string]bool{}
	for _, s := range seen {
		var f BLEFound
		ok := false
		for _, d := range s.Data {
			raw, err := base64.StdEncoding.DecodeString(d)
			if err != nil {
				continue
			}
			if f, ok = table.Match(raw); ok {
				break
			}
		}
		if !ok {
			continue
		}
		p.Matched = append(p.Matched, s.ID)
		st, kept := state[string(f.Device)]
		if !kept || st.IP || done[string(f.Device)] {
			continue
		}
		done[string(f.Device)] = true
		d := b.devs[string(f.Device)]
		if d == nil {
			d = &bleDev{first: now}
			b.devs[string(f.Device)] = d
		}
		d.last = now
		both := wd && f.WifiDirect
		switch {
		case st.Any: // only over BLE: look for the faster link now and then
			if both && now.Sub(d.wd) >= BLEWDUpgrade {
				d.wd = now
				p.Actions = append(p.Actions, BLEAction{ID: s.ID, Do: "wifi-direct", Device: f.Device})
			}
		case both && now.Sub(d.first) < BLEWDGrace:
			if now.Sub(d.wd) >= BLEWDEvery {
				d.wd = now
				p.Actions = append(p.Actions, BLEAction{ID: s.ID, Do: "wifi-direct", Device: f.Device})
			}
		case f.PSM == 0:
		case bytes.Compare(self, f.Device) < 0 || now.Sub(d.first) >= BLEWait:
			if now.Sub(d.conn) >= BLEConnectEvery {
				d.conn = now
				p.Actions = append(p.Actions, BLEAction{ID: s.ID, Do: "connect", PSM: int(f.PSM), Device: f.Device})
			}
		}
	}
	return p
}

// BLEStatus is Bluetooth LE's part of Status, as for Wi-Fi Direct.
type BLEStatus = WifiDirectStatus

// BLEAdverts returns the entries this device advertises now (base64) and when to ask again.
func (s *Service) BLEAdverts(a BLEAdvert, now time.Time) ([]string, time.Time) {
	peers, connected := s.adPeers()
	// Devices with a session over an IP path need no advert: leave them out, so a phone whose
	// devices are all connected stops advertising.
	keep := peers[:0]
	for _, p := range peers {
		if !connected[string(p.Device)] {
			keep = append(keep, p)
		}
	}
	es, exp := EncodeBLE(keep, s.cfg.Self, a, now)
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = base64.StdEncoding.EncodeToString(e[:])
	}
	return out, exp
}

// DecideBLE matches the entries the shell saw against the kept devices and says what to do.
// sessions reports, per kept device, whether it has a live session over an IP path and over any.
func (s *Service) DecideBLE(seen []BLESeen, wd bool, sessions func(ed25519.PublicKey) (ip, any bool), now time.Time) BLEPlan {
	keys, _ := s.peerKeys()
	peers := make([]BLEPeer, len(keys))
	for i, k := range keys {
		ip, any := sessions(k.Device)
		peers[i] = BLEPeer{Device: k.Device, IP: ip, Any: any}
	}
	return s.ble.Decide(s.cfg.Self, NewBLETable(keys, now), peers, seen, wd, now)
}
