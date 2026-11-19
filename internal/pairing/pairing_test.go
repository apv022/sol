package pairing

import (
	"bytes"
	"errors"
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

func testLink(t *testing.T, kind Kind) (Link, *identity.Keyring) {
	k, err := identity.Create(t.TempDir(), "Alice", "laptop", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	rec, err := k.SetEndpoints([]identity.Endpoint{{Transport: "clearnet", Address: "127.0.0.1:7700"}}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	s, _, _ := NewTokens().New(kind)
	return Link{Kind: kind, Person: k.Person(), Device: k.DevicePublic(), Secret: s, Endpoints: rec, Name: "Alice & co"}, k
}

func TestLinkRoundTrip(t *testing.T) {
	for _, kind := range []Kind{KindPair, KindJoin} {
		l, _ := testLink(t, kind)
		s := l.String()
		if !strings.HasPrefix(s, "sol://"+string(kind)+"?") || !strings.Contains(s, "v=2") {
			t.Fatalf("link %s", s)
		}
		got, err := ParseLink(s)
		if err != nil {
			t.Fatal(err)
		}
		if got.Kind != kind || !bytes.Equal(got.Person, l.Person) || !bytes.Equal(got.Device, l.Device) ||
			!bytes.Equal(got.Secret, l.Secret) || got.Name != l.Name ||
			!bytes.Equal(got.Endpoints.Marshal(), l.Endpoints.Marshal()) {
			t.Fatalf("round trip changed the link: %+v", got)
		}
	}
}

func TestParseLinkRejects(t *testing.T) {
	l, _ := testLink(t, KindPair)
	_, other := testLink(t, KindPair)
	bare, err := identity.NewDevice(t.TempDir(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	good := l.String()
	mut := func(f func(*Link)) string { c := l; f(&c); return c.String() }
	cases := map[string]string{
		"scheme":       strings.Replace(good, "sol://", "http://", 1),
		"kind":         strings.Replace(good, "sol://pair", "sol://other", 1),
		"no version":   strings.Replace(good, "&v=2", "", 1),
		"old version":  strings.Replace(good, "v=2", "v=1", 1),
		"short secret": mut(func(c *Link) { c.Secret = c.Secret[:16] }),
		"foreign endpoints": mut(func(c *Link) {
			c.Endpoints = other.Endpoints()
		}),
		"no endpoints": mut(func(c *Link) { c.Endpoints = bare.Endpoints(); c.Device = bare.DevicePublic() }),
	}
	for name, s := range cases {
		if _, err := ParseLink(s); err == nil {
			t.Errorf("%s: accepted %s", name, s)
		}
	}
	if _, err := ParseLink(strings.Replace(good, "v=2", "v=9", 1)); !errors.Is(err, ErrLinkVersion) {
		t.Errorf("newer link: %v", err)
	}
}
