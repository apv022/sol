package pairing

import (
	"bytes"
	"crypto/rand"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/apv022/sol/internal/identity"
)

func TestTokensOneTime(t *testing.T) {
	tk := NewTokens()
	s, exp, err := tk.New(KindPair)
	if err != nil || len(s) != SecretSize || time.Until(exp) > TTL {
		t.Fatal(err, len(s), exp)
	}
	if err := tk.Consume(KindPair, s); err != nil {
		t.Fatal(err)
	}
	if err := tk.Consume(KindPair, s); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("second use: %v", err)
	}
	// A secret presented for the wrong kind is refused and burned.
	j, _, _ := tk.New(KindJoin)
	if err := tk.Consume(KindPair, j); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("wrong kind: %v", err)
	}
	if err := tk.Consume(KindJoin, j); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("burned secret: %v", err)
	}
	if err := tk.Consume(KindJoin, make([]byte, SecretSize)); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("unknown secret accepted")
	}
}

func TestTokensExpire(t *testing.T) {
	now := time.Now()
	tk := NewTokens()
	tk.now = func() time.Time { return now }
	s, _, _ := tk.New(KindJoin)
	tk.New(KindPair)
	if tk.Pending() != 2 {
		t.Fatal("pending")
	}
	now = now.Add(TTL + time.Second)
	if err := tk.Consume(KindJoin, s); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expired secret: %v", err)
	}
	if tk.Pending() != 0 {
		t.Fatal("expired secrets kept")
	}
}

// onionOf is a valid v3 onion address for a random key.
func onionOf(t *testing.T) string {
	k := make([]byte, 32)
	rand.Read(k)
	return onionAddress(k)
}

func testLink(t *testing.T, kind Kind, eps ...identity.Endpoint) (Link, *identity.Keyring) {
	k, err := identity.Create(t.TempDir(), "Alice", "laptop", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(eps) == 0 {
		eps = []identity.Endpoint{{Transport: "clearnet", Address: "127.0.0.1:7700"}}
	}
	rec, err := k.SetEndpoints(eps, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	s, _, _ := NewTokens().New(kind)
	return Link{Kind: kind, Person: k.Person(), Device: k.DevicePublic(), Secret: s, Endpoints: rec}, k
}

func TestLinkRoundTrip(t *testing.T) {
	for _, kind := range []Kind{KindPair, KindJoin} {
		l, _ := testLink(t, kind)
		s, err := l.Encode()
		if err != nil || !strings.HasPrefix(s, "sol://"+string(kind)+"?v=3&k=") {
			t.Fatalf("link %s %v", s, err)
		}
		got, err := ParseLink(s)
		if err != nil {
			t.Fatal(err)
		}
		if got.Kind != kind || !bytes.Equal(got.Person, l.Person) || !bytes.Equal(got.Device, l.Device) ||
			!bytes.Equal(got.Secret, l.Secret) || !bytes.Equal(got.Endpoints.Device, l.Device) ||
			len(got.Endpoints.Endpoints) != 1 || got.Endpoints.Endpoints[0] != l.Endpoints.Endpoints[0] {
			t.Fatalf("round trip changed the link: %+v", got)
		}
	}
}

// A device with an onion, a WireGuard address and many LAN candidates still fits a short
// link: the onion and the first IPv4 candidate, nothing else.
func TestLinkShortAndBest(t *testing.T) {
	onion := onionOf(t)
	l, k := testLink(t, KindPair,
		identity.Endpoint{Transport: "wg", Address: "sol-wg:" + strings.Repeat("a", 52) + "@10.0.0.2:7702"},
		identity.Endpoint{Transport: "direct", Address: "sol-direct:x@[2001:db8::1]:7701,192.168.1.20:7701,10.0.0.2:7701"},
		identity.Endpoint{Transport: "tor", Address: onion, Access: identity.TorAccessV1})
	l.Endpoints.Endpoints[1].Address = "sol-direct:" + identity.NodeID(k.DevicePublic()) + "@[2001:db8::1]:7701,192.168.1.20:7701,10.0.0.2:7701"
	s, err := l.Encode()
	if err != nil || len(s) > MaxLinkLen {
		t.Fatalf("%d chars: %s %v", len(s), s, err)
	}
	got, err := ParseLink(s)
	if err != nil {
		t.Fatal(err)
	}
	want := []identity.Endpoint{{Transport: "tor", Address: onion},
		{Transport: "direct", Address: "sol-direct:" + identity.NodeID(k.DevicePublic()) + "@192.168.1.20:7701"}}
	if !slices.Equal(got.Endpoints.Endpoints, want) {
		t.Fatalf("%+v", got.Endpoints.Endpoints)
	}
	// Without an onion: the two best IP addresses, IPv4 first.
	l.Endpoints.Endpoints = l.Endpoints.Endpoints[:2]
	got, _ = ParseLink(l.String())
	if len(got.Endpoints.Endpoints) != 1 || !strings.HasSuffix(got.Endpoints.Endpoints[0].Address, "@192.168.1.20:7701,10.0.0.2:7701") {
		t.Fatalf("%+v", got.Endpoints.Endpoints)
	}
	// IPv6 only, clearnet: still fits.
	l.Endpoints.Endpoints = []identity.Endpoint{{Transport: "clearnet", Address: "[2001:db8::1]:7700"}, {Transport: "clearnet", Address: "[2001:db8::2]:7700"}}
	if s, err := l.Encode(); err != nil || len(s) > MaxLinkLen {
		t.Fatalf("%d %v", len(s), err)
	}
	// Nothing usable: a host name only.
	l.Endpoints.Endpoints = []identity.Endpoint{{Transport: "clearnet", Address: "example.org:7700"}}
	if _, err := l.Encode(); !errors.Is(err, ErrNoAddress) {
		t.Fatal(err)
	}
}

func TestOnionAddress(t *testing.T) {
	// A well-known v3 address (the Tor Project's) round-trips through its key.
	const tp = "2gzyxa5ihm7nsggfxnu52rck2vv4rvmdlkiu3zzui5du4xyclen53wid.onion"
	k := onionKey(tp)
	if k == nil || onionAddress(k) != tp {
		t.Fatalf("%x", k)
	}
	if onionKey("2gzyxa5ihm7nsggfxnu52rck2vv4rvmdlkiu3zzui5du4xyclen53wia.onion") != nil {
		t.Fatal("bad checksum accepted")
	}
}

func TestParseLinkRejects(t *testing.T) {
	l, _ := testLink(t, KindPair)
	good := l.String()
	cases := map[string]string{
		"scheme":     strings.Replace(good, "sol://", "http://", 1),
		"kind":       strings.Replace(good, "sol://pair", "sol://other", 1),
		"no version": strings.Replace(good, "v=3&", "", 1),
		"short":      good[:len(good)-12],
		"bad tag":    good[:len(good)-10] + "AAAAAAAAAA",
		"no address": "sol://pair?v=3&k=" + b64.EncodeToString(make([]byte, 96)),
	}
	for name, s := range cases {
		if _, err := ParseLink(s); err == nil {
			t.Errorf("%s: accepted %s", name, s)
		}
	}
	if _, err := ParseLink(strings.Replace(good, "v=3", "v=9", 1)); !errors.Is(err, ErrLinkVersion) {
		t.Errorf("newer link: %v", err)
	}
	if _, err := ParseLink("sol://pair?v=2&person=x&device=y&s=z&e=w"); !errors.Is(err, ErrLinkOld) {
		t.Errorf("older link: %v", err)
	}
}

// An anonymous pair link says so, carries only the onion (whatever the record held), stays
// short, and fields from a newer Sol are refused as such rather than as garbage.
func TestLinkAnonymous(t *testing.T) {
	onion := onionOf(t)
	l, _ := testLink(t, KindPair,
		identity.Endpoint{Transport: "clearnet", Address: "192.0.2.1:7700"},
		identity.Endpoint{Transport: "tor", Address: onion})
	l.Anonymous = true
	s, err := l.Encode()
	if err != nil || len(s) > MaxLinkLen {
		t.Fatalf("%d chars: %v", len(s), err)
	}
	got, err := ParseLink(s)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Anonymous || !slices.Equal(got.Endpoints.Endpoints, []identity.Endpoint{{Transport: "tor", Address: onion}}) {
		t.Fatalf("%+v", got)
	}
	// A plain link is not anonymous and has no flags field.
	l.Anonymous = false
	if got, _ := ParseLink(l.String()); got.Anonymous || len(got.Endpoints.Endpoints) != 2 {
		t.Fatalf("%+v", got)
	}
	// Without an onion an anonymous link reaches nothing.
	l.Anonymous = true
	l.Endpoints.Endpoints = l.Endpoints.Endpoints[:1]
	if _, err := l.Encode(); !errors.Is(err, ErrNoAddress) {
		t.Fatal(err)
	}
	// A join link can't be anonymous, either way.
	j, _ := testLink(t, KindJoin, identity.Endpoint{Transport: "tor", Address: onion})
	j.Anonymous = true
	if _, err := j.Encode(); err == nil {
		t.Fatal("an anonymous join link was encoded")
	}
	raw, _ := b64.DecodeString(strings.SplitN(s, "k=", 2)[1])
	join := "sol://join?v=3&k=" + b64.EncodeToString(raw)
	if _, err := ParseLink(join); !errors.Is(err, ErrBadLink) {
		t.Fatalf("anonymous join link: %v", err)
	}
	// Unknown flags or fields: made by a newer Sol.
	for _, extra := range [][]byte{{tagFlags, 0x80}, {9, 1, 2}} {
		b := slices.Concat(raw[:fixedLen], extra, raw[fixedLen:])
		if _, err := ParseLink("sol://pair?v=3&k=" + b64.EncodeToString(b)); !errors.Is(err, ErrLinkVersion) {
			t.Errorf("% x: %v", extra, err)
		}
	}
}

// The secret's access key is listed while the secret is pending, and OnChange fires when it
// is issued and when it is burned.
func TestTokensAccessKeys(t *testing.T) {
	tk := NewTokens()
	changes := 0
	tk.OnChange = func() { changes++ }
	s, exp, _ := tk.New(KindPair)
	ak, err := AccessKey(s)
	if err != nil {
		t.Fatal(err)
	}
	keys, next := tk.AccessKeys()
	if len(keys) != 1 || keys[0] != ak.TorPublic() || !next.Equal(exp) || changes != 1 {
		t.Fatalf("%v %v %d", keys, next, changes)
	}
	_ = tk.Consume(KindPair, s)
	if keys, next := tk.AccessKeys(); len(keys) != 0 || !next.IsZero() || changes != 2 {
		t.Fatalf("%v %v %d", keys, next, changes)
	}
}
