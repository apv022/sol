// Package pairing issues the one-time secrets behind `pair` (add a person)
// and `join` (add a device to my identity), and encodes the links that carry
// them (design/PROTOCOL.md §Pairing links):
//
//	sol://pair?v=3&k=<base64url(person | device | secret | addresses)>
//	sol://join?v=3&k=<…>
//
// Invariants:
//
//   - A secret is 32 random bytes, valid for TTL, and works exactly once:
//     Consume burns it whether or not it was used for the right kind.
//   - Secrets are stored and looked up by SHA-256, so a lookup's timing
//     says nothing about the secret.
//   - A link carries only what first contact needs: the inviter's person and
//     device keys, the secret, and at most two addresses, so it stays under
//     MaxLinkLen characters (an easily scanned QR code). The addresses are
//     hints, not signed: who answers is checked by the handshake against the
//     link's device key, and the full endpoint record comes in that handshake.
//   - A pending secret also opens the inviter's onion: AccessKey derives a Tor
//     client-auth key from it, which the inviter lists on its onion until the
//     secret is used or expires (Tokens.AccessKeys).
//
// The handshakes that redeem tokens live in internal/session.
package pairing

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/sha3"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/transport"
)

// TTL is how long a secret stays valid.
const TTL = 10 * time.Minute

// SecretSize is the size of a secret in bytes.
const SecretSize = 32

// LinkVersion is the link format this build writes and reads. Version 3 is the compact
// binary form; version 2 links (full endpoint records, ~600 characters) are refused.
const LinkVersion = 3

// MaxLinkLen bounds the length of a link String writes.
const MaxLinkLen = 200

// Kind is what a token or link is for.
type Kind string

// Kinds.
const (
	KindPair Kind = "pair" // add a person: both become contacts
	KindJoin Kind = "join" // add a device to my identity
)

// Errors.
var (
	// ErrInvalidToken is returned for unknown, expired, used or
	// wrong-kind secrets. It never says which.
	ErrInvalidToken = errors.New("pairing: invalid or expired secret")
	// ErrLinkVersion means the link was made by a newer Sol.
	ErrLinkVersion = errors.New("pairing: this link was made by a newer Sol; update Sol here and try again")
	// ErrLinkOld means the link was made by an older Sol, in a format this one no longer reads.
	ErrLinkOld = errors.New("pairing: this link was made by an older Sol; update Sol on the device that made it and ask for a new link")
	// ErrNoAddress means this device has no address that fits in a link yet.
	ErrNoAddress = errors.New("pairing: this device has no address to put in a link yet (is a transport running?)")
	// ErrBadLink means the text is not a valid pair or join link.
	ErrBadLink = errors.New("pairing: not a valid sol://pair or sol://join link")
)

type token struct {
	kind    Kind
	expires time.Time
	access  string // AccessKey(secret).TorPublic()
}

// Tokens holds pending secrets. Safe for concurrent use.
type Tokens struct {
	// OnChange, if set before the first New, is called (without locks held) after a secret
	// was issued or burned, so the owner can update the onion's client-auth set.
	OnChange func()

	ttl time.Duration
	now func() time.Time

	mu sync.Mutex
	m  map[[32]byte]token
}

// NewTokens returns an empty store whose secrets live for TTL.
func NewTokens() *Tokens {
	return &Tokens{ttl: TTL, now: time.Now, m: map[[32]byte]token{}}
}

// New issues a secret of the given kind.
func (t *Tokens) New(kind Kind) ([]byte, time.Time, error) {
	if kind != KindPair && kind != KindJoin {
		return nil, time.Time{}, fmt.Errorf("pairing: unknown kind %q", kind)
	}
	secret := make([]byte, SecretSize)
	if _, err := rand.Read(secret); err != nil {
		return nil, time.Time{}, err
	}
	ak, err := AccessKey(secret)
	if err != nil {
		return nil, time.Time{}, err
	}
	exp := t.now().Add(t.ttl)
	t.mu.Lock()
	t.purgeLocked()
	t.m[sha256.Sum256(secret)] = token{kind, exp, ak.TorPublic()}
	t.mu.Unlock()
	t.changed()
	return secret, exp, nil
}

// Consume checks a secret and burns it. It succeeds only for an unexpired
// secret of the given kind; any presented secret that exists is burned.
func (t *Tokens) Consume(kind Kind, secret []byte) error {
	key := sha256.Sum256(secret)
	t.mu.Lock()
	tok, ok := t.m[key]
	delete(t.m, key)
	t.mu.Unlock()
	if ok {
		t.changed()
	}
	if !ok || tok.kind != kind || t.now().After(tok.expires) || len(secret) != SecretSize {
		return ErrInvalidToken
	}
	return nil
}

// Pending returns how many unexpired secrets are outstanding.
func (t *Tokens) Pending() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.purgeLocked()
	return len(t.m)
}

// AccessKeys returns the Tor client-auth public keys (AccessKey) of the unexpired secrets,
// sorted, and when the next of them expires (zero if none is pending).
func (t *Tokens) AccessKeys() ([]string, time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.purgeLocked()
	var keys []string
	var next time.Time
	for _, tok := range t.m {
		keys = append(keys, tok.access)
		if next.IsZero() || tok.expires.Before(next) {
			next = tok.expires
		}
	}
	slices.Sort(keys)
	return keys, next
}

func (t *Tokens) changed() {
	if t.OnChange != nil {
		t.OnChange()
	}
}

// AccessKey derives the Tor client-auth keypair a pair or join secret stands for: the
// inviter lists its public half on its onion while the secret is pending, and whoever holds
// the link installs the private half, so a link opens the onion without carrying a key.
func AccessKey(secret []byte) (identity.X25519Key, error) {
	h := sha256.New()
	h.Write([]byte("sol tor pair v1"))
	h.Write(secret)
	return identity.X25519FromSeed(h.Sum(nil))
}

func (t *Tokens) purgeLocked() {
	now := t.now()
	for k, tok := range t.m {
		if now.After(tok.expires) {
			delete(t.m, k)
		}
	}
}

// Link is a pair or join invitation.
type Link struct {
	Kind   Kind
	Person ed25519.PublicKey // the inviter's person ID (R_pub)
	Device ed25519.PublicKey // the inviter's device: the handshake must reach exactly it
	Secret []byte
	// Endpoints are where to reach Device. String keeps at most two addresses of them (the
	// signature is not carried); ParseLink returns an unsigned record of those, for dialing.
	Endpoints identity.EndpointRecord
}

var b64 = base64.RawURLEncoding

// Address tags in a link's payload. Ports are big-endian.
const (
	tagTor      = 1 // 32-byte onion v3 public key
	tagDirect4  = 2 // IPv4 (4) | port (2), a candidate of the direct transport
	tagDirect6  = 3 // IPv6 (16) | port (2)
	tagClear4   = 4 // clearnet IPv4 | port
	tagClear6   = 5 // clearnet IPv6 | port
	maxAddrs    = 2
	fixedLen    = 3 * 32 // person | device | secret
	linkPrefix  = len("sol://join?v=3&k=")
	maxPayload  = (MaxLinkLen - linkPrefix) * 3 / 4 // raw base64url: 4 characters per 3 bytes
	onionKeyLen = 32
)

// String encodes the link. A link with no address that fits reaches nothing; Encode reports
// that instead.
func (l Link) String() string {
	s, _ := l.Encode()
	return s
}

// Encode encodes the link: version, then person, device, secret and the best one or two
// addresses packed in binary (the onion, if any, and the first IPv4 LAN candidate), so it
// stays within MaxLinkLen. It fails with ErrNoAddress when no address fits.
func (l Link) Encode() (string, error) {
	b := make([]byte, 0, maxPayload)
	b = append(b, l.Person...)
	b = append(b, l.Device...)
	b = append(b, l.Secret...)
	addrs := linkAddrs(l.Endpoints, maxPayload-len(b))
	for _, a := range addrs {
		b = append(b, a...)
	}
	s := "sol://" + string(l.Kind) + "?v=" + strconv.Itoa(LinkVersion) + "&k=" + b64.EncodeToString(b)
	if len(addrs) == 0 {
		return s, ErrNoAddress
	}
	return s, nil
}

// linkAddrs picks the addresses to carry: the onion (it works from anywhere) plus the best
// IP address that still fits, or without an onion the two best IP addresses; IPv4 before
// IPv6, the direct transport before clearnet, record order otherwise.
func linkAddrs(rec identity.EndpointRecord, budget int) [][]byte {
	var onion []byte
	var ips [][]byte
	for _, e := range rec.Endpoints {
		switch e.Transport {
		case "tor":
			if k := onionKey(e.Address); k != nil && onion == nil {
				onion = append([]byte{tagTor}, k...)
			}
		case "direct":
			if _, cands, err := transport.ParseDirectAddr(e.Address); err == nil {
				for _, c := range cands {
					if a := ipAddr(c, tagDirect4, tagDirect6); a != nil {
						ips = append(ips, a)
					}
				}
			}
		case "clearnet":
			if a := ipAddr(e.Address, tagClear4, tagClear6); a != nil {
				ips = append(ips, a)
			}
		}
	}
	rank := func(a []byte) int { return int(a[0]%2)*10 + int(a[0]) } // IPv4 (even tags) first, then direct
	slices.SortStableFunc(ips, func(a, b []byte) int { return rank(a) - rank(b) })
	var out [][]byte
	if onion != nil && len(onion) <= budget {
		budget -= len(onion)
		out = append(out, onion)
	}
	for _, a := range ips {
		if len(out) < maxAddrs && len(a) <= budget {
			budget -= len(a)
			out = append(out, a)
		}
	}
	return out
}

// ipAddr packs an "ip:port" address, or returns nil for a host name.
func ipAddr(hostport string, tag4, tag6 byte) []byte {
	ap, err := netip.ParseAddrPort(hostport)
	if err != nil || ap.Addr().Zone() != "" {
		return nil
	}
	ip := ap.Addr().Unmap()
	out := []byte{tag6}
	if ip.Is4() {
		out[0] = tag4
	}
	out = append(out, ip.AsSlice()...)
	return append(out, byte(ap.Port()>>8), byte(ap.Port()))
}

var onionB32 = base32.StdEncoding.WithPadding(base32.NoPadding)

// onionKey returns the public key of a v3 onion address ("<56>.onion"), or nil.
func onionKey(addr string) []byte {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	id, ok := strings.CutSuffix(strings.ToLower(host), ".onion")
	if !ok || len(id) != 56 {
		return nil
	}
	raw, err := onionB32.DecodeString(strings.ToUpper(id))
	if err != nil || len(raw) != 35 || onionAddress(raw[:32]) != id+".onion" {
		return nil
	}
	return raw[:32]
}

// onionAddress is the v3 onion address of a public key (rend-spec-v3: base32 of
// pubkey | checksum | version, checksum = SHA3-256(".onion checksum" | pubkey | version)[:2]).
func onionAddress(pub []byte) string {
	sum := sha3.Sum256(slices.Concat([]byte(".onion checksum"), pub, []byte{3}))
	return strings.ToLower(onionB32.EncodeToString(slices.Concat(pub, sum[:2], []byte{3}))) + ".onion"
}

// ParseLink decodes and checks a link. The version is checked first, so a
// link from a newer Sol says "update" whatever else it holds, and one from an
// older Sol says to ask for a new link.
func ParseLink(s string) (Link, error) {
	u, err := url.Parse(strings.TrimSpace(s))
	if err != nil || u.Scheme != "sol" || (u.Host != string(KindPair) && u.Host != string(KindJoin)) {
		return Link{}, ErrBadLink
	}
	q := u.Query()
	switch v := q.Get("v"); v {
	case strconv.Itoa(LinkVersion):
	default:
		n, err := strconv.Atoi(v)
		switch {
		case err == nil && n > LinkVersion:
			return Link{}, fmt.Errorf("%w (link version %d, this Sol reads %d)", ErrLinkVersion, n, LinkVersion)
		case err == nil && n >= 1:
			return Link{}, fmt.Errorf("%w (link version %d, this Sol reads %d)", ErrLinkOld, n, LinkVersion)
		}
		return Link{}, fmt.Errorf("%w: unsupported version %q", ErrBadLink, v)
	}
	b, err := b64.DecodeString(q.Get("k"))
	if err != nil || len(b) < fixedLen {
		return Link{}, fmt.Errorf("%w: bad payload", ErrBadLink)
	}
	l := Link{Kind: Kind(u.Host),
		Person: ed25519.PublicKey(slices.Clone(b[:32])),
		Device: ed25519.PublicKey(slices.Clone(b[32:64])),
		Secret: slices.Clone(b[64:96])}
	l.Endpoints.Device = l.Device
	var direct []string
	for rest := b[fixedLen:]; len(rest) > 0; {
		tag := rest[0]
		size := map[byte]int{tagTor: onionKeyLen, tagDirect4: 6, tagDirect6: 18, tagClear4: 6, tagClear6: 18}[tag]
		if size == 0 || len(rest) < 1+size {
			return Link{}, fmt.Errorf("%w: bad address", ErrBadLink)
		}
		body := rest[1 : 1+size]
		rest = rest[1+size:]
		switch tag {
		case tagTor:
			l.Endpoints.Endpoints = append(l.Endpoints.Endpoints, identity.Endpoint{Transport: "tor", Address: onionAddress(body)})
		case tagDirect4, tagDirect6:
			direct = append(direct, unpackIP(body))
		default:
			l.Endpoints.Endpoints = append(l.Endpoints.Endpoints, identity.Endpoint{Transport: "clearnet", Address: unpackIP(body)})
		}
	}
	if len(direct) > 0 {
		l.Endpoints.Endpoints = append(l.Endpoints.Endpoints, identity.Endpoint{Transport: "direct",
			Address: "sol-direct:" + identity.NodeID(l.Device) + "@" + strings.Join(direct, ",")})
	}
	if len(l.Endpoints.Endpoints) == 0 {
		return Link{}, fmt.Errorf("%w: no way to reach the device", ErrBadLink)
	}
	return l, nil
}

// unpackIP is the "ip:port" form of a packed address body (IP bytes, then a 2-byte port).
func unpackIP(b []byte) string {
	ip, _ := netip.AddrFromSlice(b[:len(b)-2])
	return netip.AddrPortFrom(ip, uint16(b[len(b)-2])<<8|uint16(b[len(b)-1])).String()
}
