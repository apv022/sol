package wire

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/apv022/sol/internal/identity"
)

// TestJoinCarriesName: the joining device's name reaches the callee, and it is covered by T.
func TestJoinCarriesName(t *testing.T) {
	alice := newKeyring(t, "alice")
	joiner, err := identity.NewDevice(t.TempDir(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	secret := bytes.Repeat([]byte{7}, SecretSize)
	var got string
	o := run(joiner, Call{Op: OpJoin, Want: alice.DevicePublic(), Bundle: joiner.Endpoints().Marshal(), Secret: secret, Name: "phone"},
		alice, func(r *Request) ([]byte, error) { got = r.Name; return bundleOf(t, alice), nil }, nil, nil)
	if o.callErr != nil || o.ansErr != nil || got != "phone" {
		t.Fatalf("call %v, answer %v, name %q", o.callErr, o.ansErr, got)
	}
	c := Call{Op: OpJoin, Want: alice.DevicePublic(), Bundle: joiner.Endpoints().Marshal(), Secret: secret, Name: "phone",
		tamper: func(r *request, _ *[]byte) { r.Name = "evil" }}
	if o := run(joiner, c, alice, acceptAll(alice), nil, nil); !errors.Is(o.callErr, ErrRejected) || !errors.Is(o.ansErr, ErrBadSignature) {
		t.Fatalf("tampered name: call %v, answer %v", o.callErr, o.ansErr)
	}
	a, b := net.Pipe()
	defer b.Close()
	if _, err := Dial(a, joiner, Call{Op: OpSession, Want: alice.DevicePublic(), Name: "x"}); err == nil {
		t.Fatal("a name outside join was accepted")
	}
}

// TestRecover: the caller proves the person with R; a wrong R is refused silently.
func TestRecover(t *testing.T) {
	alice := newKeyring(t, "alice")
	dev, err := identity.NewDevice(t.TempDir(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, r, _ := ed25519.GenerateKey(nil)
	var person ed25519.PublicKey
	o := run(dev, Call{Op: OpRecover, Want: alice.DevicePublic(), Bundle: dev.Endpoints().Marshal(), Recovery: r},
		alice, func(q *Request) ([]byte, error) { person = q.Person; return bundleOf(t, alice), nil }, nil, nil)
	if o.callErr != nil || o.ansErr != nil || !bytes.Equal(person, r.Public().(ed25519.PublicKey)) {
		t.Fatalf("call %v, answer %v", o.callErr, o.ansErr)
	}
	_, other, _ := ed25519.GenerateKey(nil)
	c := Call{Op: OpRecover, Want: alice.DevicePublic(), Bundle: dev.Endpoints().Marshal(), Recovery: r,
		tamper: func(q *request, _ *[]byte) { q.Person = b64.EncodeToString(other.Public().(ed25519.PublicKey)) }}
	if o := run(dev, c, alice, acceptAll(alice), nil, nil); !errors.Is(o.callErr, ErrRejected) || !errors.Is(o.ansErr, ErrBadSignature) {
		t.Fatalf("wrong person: call %v, answer %v", o.callErr, o.ansErr)
	}
	c.tamper = func(q *request, _ *[]byte) { q.RSig = "" }
	if o := run(dev, c, alice, acceptAll(alice), nil, nil); !errors.Is(o.callErr, ErrRejected) || !errors.Is(o.ansErr, ErrMalformed) {
		t.Fatalf("no rsig: call %v, answer %v", o.callErr, o.ansErr)
	}
}
