package pairing

import (
	"errors"
	"testing"
	"time"

	"github.com/apv022/sol/internal/identity"
)

func TestTokenSingleUse(t *testing.T) {
	m := NewManager(0)
	tok, err := m.NewToken(true)
	if err != nil {
		t.Fatal(err)
	}
	if tok.TorAuth == nil {
		t.Fatal("expected tor auth key")
	}
	if len(m.Pending()) != 1 {
		t.Fatal("token not pending")
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
	tok, _ := m.NewToken(false)
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
	tok, _ := m.NewToken(false)
	m.Consume(tok.Secret)
	if n != 2 {
		t.Fatalf("OnChange fired %d times, want 2", n)
	}
}

func TestLinkRoundTrip(t *testing.T) {
	id, _ := identity.Generate()
	in := Link{NodeID: id.NodeID(), Onion: "abc.onion", LAN: "192.168.1.5:7700", Secret: "s3cr+t/=", TorAuthPriv: "a+b/c="}
	out, err := ParseLink(in.String())
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("round trip:\n in  %+v\n out %+v", in, out)
	}
}

func TestParseLinkRejects(t *testing.T) {
	id, _ := identity.Generate()
	for _, s := range []string{
		"https://pair?node=x",
		"sol://pair?node=bad&s=x&lan=h:1",
		"sol://pair?node=" + id.NodeID() + "&lan=h:1",         // no secret
		"sol://pair?node=" + id.NodeID() + "&s=x",             // no address
		"sol://pair?v=2&node=" + id.NodeID() + "&s=x&lan=h:1", // future version
	} {
		if _, err := ParseLink(s); err == nil {
			t.Errorf("accepted %q", s)
		}
	}
}
