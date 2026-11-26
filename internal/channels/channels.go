// Package channels holds the parts of app channels (design/CHANNELS.md) that do not need a
// running daemon: channel key files, channel links, the content-addressed artifact store, the
// HTTP surface a channel is served on (GET /channel, /channel/stream, /blob/{sha256}), the
// follower's side of that stream, downloading an artifact with its hash checked, and unpacking a
// sol-app tar safely. The daemon (internal/daemon/channels.go) wires them to Tor, the internal
// feed and the app host.
//
// Invariants:
//   - Nothing from the network is trusted before it is checked: a record before its signature
//     verifies against the key the caller expects, an artifact before its SHA-256 and size
//     match the signed record. Bytes that fail are deleted, never kept.
//   - A channel key file is a secret: 0600, written atomically, never logged. The daemon never
//     needs it; records are signed where the key lives (the CLI, CI).
//   - Unpacking writes only regular files and directories under the target: absolute names,
//     "..", symlinks, hard links and devices are refused, and the total size and entry count
//     are bounded.
package channels

import (
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/apv022/sol/internal/fsutil"
	"github.com/apv022/sol/internal/identity"
)

// LinkScheme starts a channel link: sol://channel?k=<C_pub as an ID>&o=<onion>.
const LinkScheme = "sol://channel"

// Key is the content of a channel key file (sol channel new): the one secret of a channel.
type Key struct {
	Name string `json:"name"`
	// Seed is C's ed25519 seed (32 bytes).
	Seed []byte `json:"key"`
}

// NameRe is what a channel's name may be: it names the key file and the onion publication
// (pub-channel-<name>), so lowercase letters, digits and dashes, at most 24.
var NameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,23}$`)

// NewKey makes a channel key named name.
func NewKey(name string) (Key, error) {
	if !NameRe.MatchString(name) {
		return Key{}, errors.New("channels: a channel name is lowercase letters, digits and dashes (at most 24)")
	}
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return Key{}, err
	}
	return Key{Name: name, Seed: priv.Seed()}, nil
}

// LoadKey reads a channel key file.
func LoadKey(path string) (Key, error) {
	var k Key
	b, err := os.ReadFile(path)
	if err != nil {
		return k, err
	}
	if err := json.Unmarshal(b, &k); err != nil || len(k.Seed) != ed25519.SeedSize || !NameRe.MatchString(k.Name) {
		return Key{}, errors.New("channels: not a channel key file")
	}
	return k, nil
}

// Save writes the key file (0600, atomically). It refuses to overwrite one: losing a channel
// key means making a new channel.
func (k Key) Save(path string) error {
	if _, err := os.Stat(path); err == nil {
		return errors.New("channels: " + path + " exists; a channel key is never overwritten")
	}
	b, _ := json.MarshalIndent(k, "", "  ")
	return fsutil.WriteFileAtomic(path, b, 0o600)
}

// Private returns C.
func (k Key) Private() ed25519.PrivateKey { return ed25519.NewKeyFromSeed(k.Seed) }

// Public returns C_pub.
func (k Key) Public() ed25519.PublicKey { return k.Private().Public().(ed25519.PublicKey) }

// ID is the channel's identity: C_pub as an ID.
func (k Key) ID() string { return identity.ID(k.Public()) }

// FormatLink is the link followers paste or scan.
func FormatLink(channel ed25519.PublicKey, onion string) string {
	q := url.Values{"k": {identity.ID(channel)}}
	if onion != "" {
		q.Set("o", onion)
	}
	return LinkScheme + "?" + q.Encode()
}

// ParseLink decodes a channel link; both the key and the onion are required.
func ParseLink(link string) (channel ed25519.PublicKey, onion string, err error) {
	rest, ok := strings.CutPrefix(strings.TrimSpace(link), LinkScheme+"?")
	if !ok {
		return nil, "", errors.New("channels: a channel link starts with " + LinkScheme + "?")
	}
	q, err := url.ParseQuery(rest)
	if err != nil {
		return nil, "", errors.New("channels: bad channel link")
	}
	channel, err = identity.ParseID(q.Get("k"))
	if err != nil {
		return nil, "", errors.New("channels: the link's k is not a channel key")
	}
	onion = q.Get("o")
	if !ValidOnion(onion) {
		return nil, "", errors.New("channels: the link's o is not a v3 .onion address")
	}
	return channel, onion, nil
}

var onionRe = regexp.MustCompile(`^[a-z2-7]{56}\.onion$`)

// ValidOnion reports whether s is a v3 onion host name (no port).
func ValidOnion(s string) bool { return onionRe.MatchString(s) }
