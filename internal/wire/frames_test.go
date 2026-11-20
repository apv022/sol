package wire

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"strings"
	"testing"
	"time"
)

// TestLargeBundle: a device list far over the 16 KiB frame limit still fits a bundle frame,
// in both directions.
func TestLargeBundle(t *testing.T) {
	alice, bob := newKeyring(t, "alice"), newKeyring(t, "bob")
	for i := range 120 {
		pub, _, _ := ed25519.GenerateKey(nil)
		if _, err := alice.AddDevice(pub, strings.Repeat("d", 200)+string(rune('a'+i%26)), 0, time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	big := bundleOf(t, alice)
	if len(big) <= MaxFrame {
		t.Fatalf("bundle only %d bytes", len(big))
	}
	o := run(alice, Call{Op: OpSession, Want: bob.DevicePublic(), Bundle: big}, bob, acceptAll(bob), nil, nil)
	if o.callErr != nil || o.ansErr != nil {
		t.Fatalf("caller's large bundle: call %v, answer %v", o.callErr, o.ansErr)
	}
	o = run(bob, Call{Op: OpSession, Want: alice.DevicePublic(), Bundle: bundleOf(t, bob)}, alice, acceptAll(alice), nil, nil)
	if o.callErr != nil || o.ansErr != nil || len(o.res.Bundle.Devices.Devices) != 121 {
		t.Fatalf("callee's large bundle: call %v, answer %v", o.callErr, o.ansErr)
	}
	if _, err := appendBundleFrame(nil, make([]byte, MaxBundleFrame+1)); err == nil {
		t.Fatal("oversized bundle frame accepted")
	}
	hdr := []byte{0, 0x10, 0, 1} // 1 MiB + 1
	if _, err := readBundleFrame(bytes.NewReader(hdr)); !errors.Is(err, ErrMalformed) {
		t.Fatalf("oversized bundle frame read: %v", err)
	}
}

// TestProbeAndGate: a probe is an ordinary handshake under its own op, and a Gate that says
// no aborts before the callee could complete its side.
func TestProbeAndGate(t *testing.T) {
	alice, bob := newKeyring(t, "alice"), newKeyring(t, "bob")
	gated := false
	o := run(alice, Call{Op: OpProbe, Want: bob.DevicePublic(), Bundle: bundleOf(t, alice),
		Gate: func() (func(), error) { gated = true; return nil, nil }}, bob, acceptAll(bob), nil, nil)
	if o.callErr != nil || o.ansErr != nil || o.req.Op != OpProbe || !gated {
		t.Fatalf("probe: call %v, answer %v", o.callErr, o.ansErr)
	}
	no := errors.New("another path won")
	decided := false
	o = run(alice, Call{Op: OpSession, Want: bob.DevicePublic(), Bundle: bundleOf(t, alice),
		Gate: func() (func(), error) { return nil, no }}, bob,
		func(r *Request) ([]byte, error) { decided = true; return acceptAll(bob)(r) }, nil, nil)
	if !errors.Is(o.callErr, no) || o.ansErr == nil || decided {
		t.Fatalf("gated: call %v, answer %v, decided %v", o.callErr, o.ansErr, decided)
	}
}
