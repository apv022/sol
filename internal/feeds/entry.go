package feeds

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/apv022/sol/internal/identity"
)

// MaxBodySize is the largest entry body in bytes (API.md §3): larger data
// goes in a blob and the entry points at it.
const MaxBodySize = 64 << 10

// InternalApp is the app ID of Sol's own feeds (contacts, sharing records).
// They replicate between my own devices and are never shared with anyone.
const InternalApp = "sol"

// entryContext starts every signed entry (PROTOCOL.md §Feeds on the wire).
const entryContext = "sol entry v1"

var (
	appIDRe    = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)
	feedNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
)

// ValidApp reports whether s is a legal app ID.
func ValidApp(s string) bool { return appIDRe.MatchString(s) }

// ValidName reports whether s is a legal feed name.
func ValidName(s string) bool { return feedNameRe.MatchString(s) }

// Address builds a feed address <person>/<app>/<feed>.
func Address(person, app, name string) string { return person + "/" + app + "/" + name }

// ParseAddress splits and validates a feed address. The person part must
// be a canonical ID, so an address is also a safe relative path.
func ParseAddress(addr string) (person, app, name string, err error) {
	p := strings.Split(addr, "/")
	if len(p) != 3 || !ValidApp(p[1]) || !ValidName(p[2]) {
		return "", "", "", fmt.Errorf("%w: bad feed address", ErrInvalid)
	}
	if _, err := identity.ParseID(p[0]); err != nil {
		return "", "", "", fmt.Errorf("%w: bad person in feed address", ErrInvalid)
	}
	return p[0], p[1], p[2], nil
}

// Entry is one stored, verified feed entry, in the JSON shape of the app API
// (API.md §3, sdk/go/sol.Entry). Pos is this device's arrival cursor for
// the feed and is not covered by Hash.
type Entry struct {
	Feed   string          `json:"feed"`
	Seq    uint64          `json:"seq"`
	Pos    uint64          `json:"pos"`
	Device string          `json:"device"`
	Time   time.Time       `json:"time"`
	Body   json.RawMessage `json:"body"`
	Hash   string          `json:"hash"`
	Prev   string          `json:"prev"`
	// Sig is the device's signature over the encoded entry; not part of
	// the app API.
	Sig []byte `json:"-"`
}

// Wire returns the replication form of e.
func (e Entry) Wire() Wire {
	return Wire{Feed: e.Feed, Seq: e.Seq, Device: e.Device, Time: e.Time.UnixMilli(),
		Prev: e.Prev, Body: e.Body, Sig: e.Sig}
}

// Wire is an entry as it travels between devices and as it is stored on
// disk: everything signed plus the signature. Hash and pos are derived by
// the receiver.
type Wire struct {
	Feed   string          `json:"feed"`
	Seq    uint64          `json:"seq"`
	Device string          `json:"device"`
	Time   int64           `json:"time"` // unix milliseconds
	Prev   string          `json:"prev,omitempty"`
	Body   json.RawMessage `json:"body"`
	Sig    []byte          `json:"sig"`
}

// entry turns a verified Wire into an Entry.
func (w Wire) entry(pos uint64, hash string) Entry {
	return Entry{Feed: w.Feed, Seq: w.Seq, Pos: pos, Device: w.Device,
		Time: time.UnixMilli(w.Time).UTC(), Body: w.Body, Hash: hash, Prev: w.Prev, Sig: w.Sig}
}

// signed returns E, the bytes the device signs:
//
//	lp("sol entry v1") | lp(feed address) | u64(seq) | lp(prev hash)
//	| u64(unix_ms) | lp(D_pub) | lp(app id) | lp(body)
//
// prev is the raw 32-byte hash (empty for seq 1).
func (w Wire) signed() ([]byte, error) {
	_, app, _, err := ParseAddress(w.Feed)
	if err != nil {
		return nil, err
	}
	dev, err := identity.ParseID(w.Device)
	if err != nil {
		return nil, fmt.Errorf("%w: bad device", ErrInvalid)
	}
	prev, err := parseHash(w.Prev)
	if err != nil {
		return nil, err
	}
	if w.Seq == 0 || w.Time < 0 || (w.Seq == 1) != (prev == nil) {
		return nil, fmt.Errorf("%w: bad seq, time or prev", ErrInvalid)
	}
	var b []byte
	lp := func(x []byte) {
		b = binary.BigEndian.AppendUint32(b, uint32(len(x)))
		b = append(b, x...)
	}
	lp([]byte(entryContext))
	lp([]byte(w.Feed))
	b = binary.BigEndian.AppendUint64(b, w.Seq)
	lp(prev)
	b = binary.BigEndian.AppendUint64(b, uint64(w.Time))
	lp(dev)
	lp([]byte(app))
	lp(w.Body)
	return b, nil
}

// verify checks the shape and signature of w and returns its hash.
func (w Wire) verify() (string, error) {
	if len(w.Body) > MaxBodySize {
		return "", fmt.Errorf("%w: body over 64 KiB", ErrInvalid)
	}
	if c, err := Canonical(w.Body); err != nil || !bytes.Equal(c, w.Body) {
		return "", fmt.Errorf("%w: body is not canonical JSON", ErrInvalid)
	}
	e, err := w.signed()
	if err != nil {
		return "", err
	}
	dev, _ := identity.ParseID(w.Device)
	if len(w.Sig) != ed25519.SignatureSize || !ed25519.Verify(dev, e, w.Sig) {
		return "", ErrBadSignature
	}
	return hashOf(e, w.Sig), nil
}

// hashOf is sha256(E | sig) in the API's "sha256:<hex>" form.
func hashOf(e, sig []byte) string {
	h := sha256.New()
	h.Write(e)
	h.Write(sig)
	return "sha256:" + hex.EncodeToString(h.Sum(nil))
}

// parseHash decodes "sha256:<64 hex>" ("" is no hash).
func parseHash(s string) ([]byte, error) {
	if s == "" {
		return nil, nil
	}
	h, ok := strings.CutPrefix(s, "sha256:")
	if !ok || len(h) != 64 || strings.ToLower(h) != h {
		return nil, fmt.Errorf("%w: bad hash", ErrInvalid)
	}
	raw, err := hex.DecodeString(h)
	if err != nil {
		return nil, fmt.Errorf("%w: bad hash", ErrInvalid)
	}
	return raw, nil
}

// Canonical re-encodes JSON with sorted object keys and no insignificant
// whitespace, exactly as the SDK does (numbers keep their literal text).
// Canonical bytes are a fixed point, so they survive being embedded in
// other JSON.
func Canonical(raw []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("%w: body: %v", ErrInvalid, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("%w: body has trailing data", ErrInvalid)
	}
	return json.Marshal(v)
}

// Errors from Append and Accept. Accept wraps them with detail.
var (
	ErrInvalid       = errors.New("feeds: invalid entry")
	ErrBadSignature  = errors.New("feeds: bad entry signature")
	ErrUnknownDevice = errors.New("feeds: device is not in the owner's device list")
	ErrRemovedDevice = errors.New("feeds: device was removed from the owner's device list")
	ErrDuplicate     = errors.New("feeds: entry already stored")
	ErrFork          = errors.New("feeds: a different entry with this seq is stored")
	ErrBadPrev       = errors.New("feeds: prev does not match the chain")
	ErrNotMine       = errors.New("feeds: only my own feeds can be appended to")
	ErrNoPerson      = errors.New("feeds: this device has not joined a person yet")
	ErrClosed        = errors.New("feeds: store closed")
)

// GapError means an entry arrived ahead of its chain: entries From..To of
// Device's chain in Feed must be fetched first.
type GapError struct {
	Feed, Device string
	From, To     uint64
}

func (e *GapError) Error() string {
	return fmt.Sprintf("feeds: gap in %s@%s: missing seq %d..%d", e.Feed, e.Device, e.From, e.To)
}
