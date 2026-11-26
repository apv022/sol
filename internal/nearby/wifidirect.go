package nearby

// Wi-Fi Direct ads (design/PROTOCOL.md §Nearby "Wi-Fi Direct (Android)"): the TXT record of the
// DNS-SD service the Android shell advertises over Wi-Fi P2P, and the decision whether to join,
// lead or wait once ads of kept devices are seen. The shell never sees a key: it passes TXT
// records in and gets a decision (and group credentials) out.

import (
	"bytes"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	mrand "math/rand/v2"
	"slices"
	"strconv"
	"strings"
	"time"
)

// The ad format.
const (
	// AdService is the DNS-SD service type the shell advertises and looks for.
	AdService = "_sol._udp"
	// AdVersion is the TXT record's "v".
	AdVersion = "1"
	// AdMaxEntries bounds the entries of one ad (service discovery frames are small).
	AdMaxEntries = 8

	adEntrySize = 32
	adPadTo     = 4
	adChunk     = 200 // characters per TXT value (a TXT string holds at most 255 bytes)
	ssidSize    = 6
	passSize    = 15
	ssidPrefix  = "DIRECT-"
)

var b64 = base64.RawURLEncoding

// P2PGroup is the credentials of a Wi-Fi Direct group this device owns: a random network name
// (Android requires the DIRECT- prefix) and passphrase, handed only to kept devices, masked in
// their entries.
type P2PGroup struct {
	SSID string `json:"ssid"`
	Pass string `json:"pass"`
}

// NewGroup returns fresh random credentials.
func NewGroup() P2PGroup {
	var s [ssidSize]byte
	var p [passSize]byte
	rand.Read(s[:])
	rand.Read(p[:])
	return P2PGroup{SSID: ssidPrefix + b64.EncodeToString(s[:]), Pass: b64.EncodeToString(p[:])}
}

// ErrBadGroup is returned for credentials that NewGroup could not have made.
var ErrBadGroup = errors.New("nearby: malformed Wi-Fi Direct group credentials")

// payload is the 24 bytes an entry masks: kind 1, the name's and passphrase's random bytes,
// two zero bytes a receiver checks.
func (g P2PGroup) payload() ([adEntrySize - tagSize]byte, error) {
	var out [adEntrySize - tagSize]byte
	s, err1 := b64.DecodeString(strings.TrimPrefix(g.SSID, ssidPrefix))
	p, err2 := b64.DecodeString(g.Pass)
	if !strings.HasPrefix(g.SSID, ssidPrefix) || err1 != nil || err2 != nil || len(s) != ssidSize || len(p) != passSize {
		return out, ErrBadGroup
	}
	out[0] = 1
	copy(out[1:], s)
	copy(out[1+ssidSize:], p)
	return out, nil
}

// deriveAd computes the tag and payload mask of sender's ad entry for its peer in window w:
// HMAC-SHA256(K, "wifi-direct" | D_sender | u64 w). Another label than beacons' keeps a phone's
// ads and its beacons unlinkable.
func deriveAd(key [32]byte, sender ed25519.PublicKey, w uint64) (tag [tagSize]byte, mask [adEntrySize - tagSize]byte) {
	m := hmac.New(sha256.New, key[:])
	m.Write([]byte("wifi-direct"))
	m.Write(sender)
	var wb [8]byte
	binary.BigEndian.PutUint64(wb[:], w)
	m.Write(wb[:])
	out := m.Sum(nil)
	copy(tag[:], out[:tagSize])
	copy(mask[:], out[tagSize:])
	return tag, mask
}

// EncodeAd builds the TXT record self advertises at now: an entry per peer (the first
// AdMaxEntries: the caller orders them, those without a session first), carrying g's
// credentials when g is not nil, padded with random entries to a multiple of 4 and shuffled.
// It also returns when the window ends, so the shell re-registers the ad then.
func EncodeAd(peers []PeerKey, self ed25519.PublicKey, g *P2PGroup, now time.Time) (map[string]string, time.Time, error) {
	var pl [adEntrySize - tagSize]byte
	if g != nil {
		var err error
		if pl, err = g.payload(); err != nil {
			return nil, time.Time{}, err
		}
	}
	w := window(now)
	expires := time.Unix(int64((w+1)*uint64(WindowLength/time.Second)), 0)
	txt := map[string]string{"v": AdVersion}
	if len(peers) == 0 {
		return txt, expires, nil
	}
	if len(peers) > AdMaxEntries {
		peers = peers[:AdMaxEntries]
	}
	n := (len(peers) + adPadTo - 1) / adPadTo * adPadTo
	es := make([][adEntrySize]byte, n)
	for i, p := range peers {
		tag, mask := deriveAd(p.Key, self, w)
		copy(es[i][:tagSize], tag[:])
		for j := range pl {
			es[i][tagSize+j] = pl[j] ^ mask[j]
		}
	}
	for i := len(peers); i < n; i++ {
		rand.Read(es[i][:])
	}
	mrand.Shuffle(n, func(i, j int) { es[i], es[j] = es[j], es[i] })
	var raw []byte
	for _, e := range es {
		raw = append(raw, e[:]...)
	}
	s := b64.EncodeToString(raw)
	for i := 0; len(s) > 0; i++ {
		k := min(adChunk, len(s))
		txt["t"+strconv.Itoa(i)] = s[:k]
		s = s[k:]
	}
	return txt, expires, nil
}

// decodeAd returns an ad's entries (nil for anything else).
func decodeAd(txt map[string]string) [][adEntrySize]byte {
	if txt["v"] != AdVersion {
		return nil
	}
	var sb strings.Builder
	for i := 0; ; i++ {
		v, ok := txt["t"+strconv.Itoa(i)]
		if !ok {
			break
		}
		sb.WriteString(v)
	}
	raw, err := b64.DecodeString(sb.String())
	if err != nil || len(raw) == 0 || len(raw)%adEntrySize != 0 || len(raw)/adEntrySize > AdMaxEntries {
		return nil
	}
	out := make([][adEntrySize]byte, len(raw)/adEntrySize)
	for i := range out {
		copy(out[i][:], raw[i*adEntrySize:])
	}
	return out
}

// AdFound is a kept device recognised in an ad, with the group it offers (nil: none).
type AdFound struct {
	Device ed25519.PublicKey
	Group  *P2PGroup
}

type adExpect struct {
	device ed25519.PublicKey
	mask   [adEntrySize - tagSize]byte
}

// AdTable holds the ad tags this device expects from its peers around one moment.
type AdTable struct {
	tags map[[tagSize]byte]adExpect
}

// NewAdTable precomputes the tags peers' ads carry in the window of now and the two next to it.
func NewAdTable(peers []PeerKey, now time.Time) *AdTable {
	w := window(now)
	t := &AdTable{tags: make(map[[tagSize]byte]adExpect, 3*len(peers))}
	for _, p := range peers {
		for _, ww := range []uint64{w - 1, w, w + 1} {
			tag, mask := deriveAd(p.Key, p.Device, ww)
			t.tags[tag] = adExpect{device: p.Device, mask: mask}
		}
	}
	return t
}

// Match returns the peers recognised in an ad's TXT record.
func (t *AdTable) Match(txt map[string]string) []AdFound {
	var out []AdFound
	for _, e := range decodeAd(txt) {
		var tag [tagSize]byte
		copy(tag[:], e[:tagSize])
		x, ok := t.tags[tag]
		if !ok {
			continue
		}
		var pl [adEntrySize - tagSize]byte
		for i := range pl {
			pl[i] = e[tagSize+i] ^ x.mask[i]
		}
		if pl[len(pl)-2] != 0 || pl[len(pl)-1] != 0 {
			continue // a collision with a random entry, or a newer format
		}
		f := AdFound{Device: x.device}
		switch pl[0] {
		case 0:
		case 1:
			f.Group = &P2PGroup{SSID: ssidPrefix + b64.EncodeToString(pl[1:1+ssidSize]),
				Pass: b64.EncodeToString(pl[1+ssidSize : 1+ssidSize+passSize])}
		default:
			continue
		}
		out = append(out, f)
	}
	return out
}

// AdSeen is one Wi-Fi Direct peer's ad as the shell saw it: ID is its P2P device address,
// opaque here and echoed back.
type AdSeen struct {
	ID  string            `json:"id"`
	TXT map[string]string `json:"txt"`
}

// Plan is what the shell should do (design/PROTOCOL.md §Nearby "Wi-Fi Direct"): Role is
// "join" (ID and Group: connect to that group), "lead" (Group: create a group with these
// credentials and advertise them), "wait" (a peer with a lower key will lead), "stay" (already
// in a group) or "none". Matched lists the IDs whose ads carried a kept device.
type Plan struct {
	Role    string    `json:"role"`
	ID      string    `json:"id,omitempty"`
	Group   *P2PGroup `json:"group,omitempty"`
	Matched []string  `json:"matched"`
}

// Decide turns the ads seen into a Plan. state is the shell's: "none", "owner" or "client".
// connected reports whether a live session to a device exists: only devices without one are
// worth a group. The lowest device key leads, so two phones never both create a group.
func Decide(self ed25519.PublicKey, state string, table *AdTable, seen []AdSeen, connected func(ed25519.PublicKey) bool) Plan {
	p := Plan{Role: "none", Matched: []string{}}
	type offer struct {
		id  string
		dev ed25519.PublicKey
		g   *P2PGroup
	}
	var offers []offer
	var wanted []ed25519.PublicKey
	for _, s := range seen {
		fs := table.Match(s.TXT)
		if len(fs) == 0 {
			continue
		}
		p.Matched = append(p.Matched, s.ID)
		for _, f := range fs {
			if connected(f.Device) {
				continue
			}
			wanted = append(wanted, f.Device)
			if f.Group != nil {
				offers = append(offers, offer{s.ID, f.Device, f.Group})
			}
		}
	}
	switch {
	case state == "owner" || state == "client":
		p.Role = "stay"
	case len(offers) > 0:
		best := slices.MinFunc(offers, func(a, b offer) int { return bytes.Compare(a.dev, b.dev) })
		p.Role, p.ID, p.Group = "join", best.id, best.g
	case len(wanted) == 0:
	case slices.IndexFunc(wanted, func(d ed25519.PublicKey) bool { return bytes.Compare(d, self) < 0 }) < 0:
		g := NewGroup()
		p.Role, p.Group = "lead", &g
	default:
		p.Role = "wait"
	}
	return p
}

// WifiDirectStatus is Wi-Fi Direct's part of Status: whether it is switched on, and what the
// Android shell last reported (design/PROTOCOL.md §Nearby "Wi-Fi Direct").
type WifiDirectStatus struct {
	Enabled bool   `json:"enabled"`
	State   string `json:"state"`
	Detail  string `json:"detail,omitempty"`
}

// adPeers returns the peers with their keys, those without a live session first (an ad has
// room for AdMaxEntries only), and which of them are connected.
func (s *Service) adPeers() ([]PeerKey, map[string]bool) {
	peers, connected := s.peerKeys()
	slices.SortStableFunc(peers, func(a, b PeerKey) int {
		ca, cb := connected[string(a.Device)], connected[string(b.Device)]
		switch {
		case ca == cb:
			return 0
		case !ca:
			return -1
		}
		return 1
	})
	return peers, connected
}

// Ad returns the TXT record this device advertises over Wi-Fi Direct now (with g's credentials
// when it owns a group) and when to advertise anew.
func (s *Service) Ad(g *P2PGroup, now time.Time) (map[string]string, time.Time, error) {
	peers, _ := s.adPeers()
	return EncodeAd(peers, s.cfg.Self, g, now)
}

// Decide matches the ads the shell saw against the kept devices and says what to do.
func (s *Service) Decide(state string, seen []AdSeen, now time.Time) Plan {
	peers, connected := s.adPeers()
	return Decide(s.cfg.Self, state, NewAdTable(peers, now), seen,
		func(d ed25519.PublicKey) bool { return connected[string(d)] })
}
