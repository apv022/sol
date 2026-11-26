package nearby

import (
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	mrand "math/rand/v2"
	"slices"
	"strings"
	"time"
)

// The beacon format and token scheme (design/PROTOCOL.md §Nearby).
const (
	// Group and Port are where beacons go: an administratively scoped IPv4 group, sent with
	// TTL 1 so they never leave the link.
	Group = "239.255.83.79"
	Port  = 7748
	// WindowLength is how long one set of tags lasts. Receivers accept the windows before and
	// after the current one, so clocks may differ by a few minutes.
	WindowLength = 5 * time.Minute

	magic      = "SOLN"
	version    = 1
	headerSize = len(magic) + 2
	entrySize  = 16
	tagSize    = 8
	maxEntries = 64 // per datagram, so one fits any link's MTU (1030 bytes)
	padTo      = 8  // entry counts are rounded up to this, so they say little
	// MaxDatagram is the largest valid beacon datagram.
	MaxDatagram = headerSize + maxEntries*entrySize
)

// ErrMalformed is returned for a datagram that is not a beacon of this version.
var ErrMalformed = errors.New("nearby: not a beacon")

// Ports are the listening ports a beacon advertises (0: that transport isn't listening on the
// LAN).
type Ports struct {
	WG, Direct, Clearnet uint16
}

// IsZero reports whether no port is advertised: nothing on the LAN could reach this device.
func (p Ports) IsZero() bool { return p == Ports{} }

func (p Ports) bytes() [8]byte {
	var b [8]byte
	binary.BigEndian.PutUint16(b[0:], p.WG)
	binary.BigEndian.PutUint16(b[2:], p.Direct)
	binary.BigEndian.PutUint16(b[4:], p.Clearnet)
	return b // the last two bytes stay zero: a receiver checks them
}

// window is the number of the time window t is in.
func window(t time.Time) uint64 {
	s := t.Unix()
	if s < 0 {
		s = 0
	}
	return uint64(s) / uint64(WindowLength/time.Second)
}

// derive computes the tag and port mask that sender's beacon carries for its peer in window w,
// under the pair's key: HMAC-SHA256(K, "beacon" | D_sender | u64 w). The sender's key is in
// the input, so the two directions of a pair have different tags (an observer can't match
// equal tags in two devices' beacons and learn that they know each other).
func derive(key [32]byte, sender ed25519.PublicKey, w uint64) (tag, mask [8]byte) {
	m := hmac.New(sha256.New, key[:])
	m.Write([]byte("beacon"))
	m.Write(sender)
	var wb [8]byte
	binary.BigEndian.PutUint64(wb[:], w)
	m.Write(wb[:])
	out := m.Sum(nil)
	copy(tag[:], out[0:8])
	copy(mask[:], out[8:16])
	return tag, mask
}

// PeerKey is a device this device keeps, with the key the two share.
type PeerKey struct {
	Device ed25519.PublicKey
	Key    [32]byte
}

// Encode builds the beacon datagrams self sends at now: one entry per peer (its tag and the
// masked ports), padded with random entries to a multiple of padTo, shuffled, at most
// maxEntries per datagram. With no peers it returns nothing: a beacon nobody can recognise
// would only tell the network that Sol is here.
func Encode(peers []PeerKey, self ed25519.PublicKey, ports Ports, now time.Time) [][]byte {
	if len(peers) == 0 {
		return nil
	}
	w := window(now)
	pb := ports.bytes()
	entries := make([][entrySize]byte, 0, len(peers))
	for _, p := range peers {
		tag, mask := derive(p.Key, self, w)
		var e [entrySize]byte
		copy(e[:tagSize], tag[:])
		for i := range pb {
			e[tagSize+i] = pb[i] ^ mask[i]
		}
		entries = append(entries, e)
	}
	mrand.Shuffle(len(entries), func(i, j int) { entries[i], entries[j] = entries[j], entries[i] })
	var out [][]byte
	for chunk := range slices.Chunk(entries, maxEntries) {
		n := (len(chunk) + padTo - 1) / padTo * padTo
		b := make([]byte, headerSize, headerSize+n*entrySize)
		copy(b, magic)
		b[len(magic)] = version
		b[len(magic)+1] = byte(n)
		for _, e := range chunk {
			b = append(b, e[:]...)
		}
		pad := make([]byte, (n-len(chunk))*entrySize)
		rand.Read(pad)
		b = append(b, pad...)
		// The padding sits at the end; shuffle once more so it doesn't stand out by position.
		es := b[headerSize:]
		mrand.Shuffle(n, func(i, j int) {
			var t [entrySize]byte
			copy(t[:], es[i*entrySize:])
			copy(es[i*entrySize:(i+1)*entrySize], es[j*entrySize:(j+1)*entrySize])
			copy(es[j*entrySize:], t[:])
		})
		out = append(out, b)
	}
	return out
}

// decode returns a datagram's entries.
func decode(b []byte) ([][entrySize]byte, error) {
	if len(b) < headerSize || string(b[:len(magic)]) != magic || b[len(magic)] != version {
		return nil, ErrMalformed
	}
	n := int(b[len(magic)+1])
	if n == 0 || n > maxEntries || len(b) != headerSize+n*entrySize {
		return nil, ErrMalformed
	}
	out := make([][entrySize]byte, n)
	for i := range out {
		copy(out[i][:], b[headerSize+i*entrySize:])
	}
	return out, nil
}

// Found is a peer device recognised in a beacon, with the ports it advertised.
type Found struct {
	Device ed25519.PublicKey
	Ports  Ports
}

type expect struct {
	device ed25519.PublicKey
	mask   [8]byte
}

// Table holds the tags this device expects from each of its peers in the windows around one
// moment, so a beacon costs one map lookup per entry.
type Table struct {
	w    uint64
	sig  string
	tags map[[tagSize]byte]expect
}

// tableSig identifies a set of peers, so a table is rebuilt only when it changed.
func tableSig(peers []PeerKey) string {
	ids := make([]string, len(peers))
	for i, p := range peers {
		ids[i] = string(p.Device)
	}
	slices.Sort(ids)
	return strings.Join(ids, "")
}

// NewTable precomputes the tags peers send in the window of now and the two next to it.
func NewTable(peers []PeerKey, now time.Time) *Table {
	w := window(now)
	t := &Table{w: w, sig: tableSig(peers), tags: make(map[[tagSize]byte]expect, 3*len(peers))}
	for _, p := range peers {
		for _, ww := range []uint64{w - 1, w, w + 1} {
			tag, mask := derive(p.Key, p.Device, ww)
			t.tags[tag] = expect{device: p.Device, mask: mask}
		}
	}
	return t
}

// fresh reports whether t still fits peers at now.
func (t *Table) fresh(peers []PeerKey, now time.Time) bool {
	return t != nil && t.w == window(now) && t.sig == tableSig(peers)
}

// Match returns the peers recognised in a beacon datagram (none for anything else).
func (t *Table) Match(b []byte) []Found {
	es, err := decode(b)
	if err != nil {
		return nil
	}
	var out []Found
	for _, e := range es {
		var tag [tagSize]byte
		copy(tag[:], e[:tagSize])
		x, ok := t.tags[tag]
		if !ok {
			continue
		}
		var pb [8]byte
		for i := range pb {
			pb[i] = e[tagSize+i] ^ x.mask[i]
		}
		if pb[6] != 0 || pb[7] != 0 {
			continue // a tag collision with a random entry, or a newer format
		}
		out = append(out, Found{Device: x.device, Ports: Ports{
			WG:       binary.BigEndian.Uint16(pb[0:]),
			Direct:   binary.BigEndian.Uint16(pb[2:]),
			Clearnet: binary.BigEndian.Uint16(pb[4:]),
		}})
	}
	return out
}
