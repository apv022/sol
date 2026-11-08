package pairing

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/transport"
)

func TestTokenSingleUse(t *testing.T) {
	m := NewManager(0)
	tok, err := m.NewToken(map[string]transport.Access{"tor": {Authorize: "PUB", Secret: "priv"}})
	if err != nil {
		t.Fatal(err)
	}
	if p := m.Pending(); len(p) != 1 || p[0].Access["tor"].Authorize != "PUB" {
		t.Fatalf("pending = %+v", p)
	}
	if _, err := m.Consume(tok.Secret); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Consume(tok.Secret); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("second use: want ErrInvalidToken, got %v", err)
	}
	if _, err := m.Consume("garbage"); !errors.Is(err, ErrInvalidToken) {
		t.Fatal("unknown secret accepted")
	}
}

func TestTokenExpires(t *testing.T) {
	m := NewManager(time.Minute)
	now := time.Now()
	m.now = func() time.Time { return now }
	tok, _ := m.NewToken(nil)
	now = now.Add(2 * time.Minute)
	if _, err := m.Consume(tok.Secret); !errors.Is(err, ErrInvalidToken) {
		t.Fatalf("expired token: want ErrInvalidToken, got %v", err)
	}
	if len(m.Pending()) != 0 {
		t.Fatal("expired token still pending")
	}
}

func TestOnChangeFires(t *testing.T) {
	m := NewManager(0)
	n := 0
	m.OnChange(func() { n++ })
	tok, _ := m.NewToken(nil)
	m.Consume(tok.Secret)
	if n != 2 {
		t.Fatalf("OnChange fired %d times, want 2", n)
	}
}

func TestLinkRoundTrip(t *testing.T) {
	id, _ := identity.Generate()
	in := Link{NodeID: id.NodeID(), Name: "home server", Secret: "s3cr+t/=",
		Addrs:  map[string]string{"tor": "abc.onion", "clearnet": "192.168.1.5:7700", "i2p": "xyz.b32.i2p"},
		Access: map[string]string{"tor": "a+b/c="}}
	out, err := ParseLink(in.String())
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("round trip:\n in  %+v\n out %+v", in, out)
	}
}

func TestParseLinkRejects(t *testing.T) {
	id, _ := identity.Generate()
	for _, s := range []string{
		"https://pair?node=x",
		"sol://pair?node=bad&s=x&clearnet=h:1",
		"sol://pair?node=" + id.NodeID() + "&clearnet=h:1",            // no secret
		"sol://pair?node=" + id.NodeID() + "&s=x",                     // no address
		"sol://pair?node=" + id.NodeID() + "&s=x&auth.tor=only-a-key", // access but no address
	} {
		if _, err := ParseLink(s); err == nil {
			t.Errorf("accepted %q", s)
		}
	}
}
