package wire

import (
	"bytes"
	"crypto/ed25519"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/apv022/sol/internal/identity"
)

func newKeyring(t testing.TB, name string) *identity.Keyring {
	t.Helper()
	k, err := identity.Create(t.TempDir(), name, name+"-dev", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func bundleOf(t testing.TB, k *identity.Keyring) []byte {
	t.Helper()
	b, err := k.Bundle()
	if err != nil {
		t.Fatal(err)
	}
	return b.Marshal()
}

func acceptAll(k *identity.Keyring) Decide {
	return func(*Request) ([]byte, error) {
		b, err := k.Bundle()
		return b.Marshal(), err
	}
}

type outcome struct {
	res     *Result
	callErr error
	req     *Request
	ansErr  error
}

// run performs one handshake over an in-memory pipe. wrapA wraps the
// caller's end, wrapB the callee's.
func run(caller Local, c Call, callee Local, decide Decide, wrapA, wrapB func(net.Conn) net.Conn) outcome {
	a, b := net.Pipe()
	var ca, cb net.Conn = a, b
	if wrapA != nil {
		ca = wrapA(a)
	}
	if wrapB != nil {
		cb = wrapB(b)
	}
	var o outcome
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		o.req, o.ansErr = Answer(cb, callee, decide)
	}()
	o.res, o.callErr = Dial(ca, caller, c)
	if o.callErr == nil {
		ca.Close() // end the "session" so the callee side finishes
	}
	wg.Wait()
	return o
}

func TestGoldenTranscript(t *testing.T) {
	rep := func(b byte) []byte { return bytes.Repeat([]byte{b}, 32) }
	nv := negotiated{2, Versions{2, 2}, Versions{2, 2}}
	got := transcript(nv, rep(1), rep(2), rep(3), rep(4), "pair", rep(5), []byte("bundle"))
	want := "00000010" + "736f6c2068616e647368616b65207632" + // lp("sol handshake v2")
		"00020002000200020002" + // versions
		"00000020" + strings.Repeat("01", 32) + // lp(D_a)
		"00000020" + strings.Repeat("02", 32) + // lp(D_b)
		"00000020" + strings.Repeat("03", 32) + // lp(nonce_a)
		"00000020" + strings.Repeat("04", 32) + // lp(nonce_b)
		"00000004" + "70616972" + // lp("pair")
		"00000020" + strings.Repeat("05", 32) + // lp(secret)
		"00000020" + "1e6ed65d77d6364eeaed5a745ba5c4985ae2b700dd85d7cf7f027bdf294a33fc" // lp(sha256("bundle"))
	if hex.EncodeToString(got) != want {
		t.Fatalf("transcript\n got %x\nwant %s", got, want)
	}
	// A session has an empty secret: lp of nothing is four zero bytes.
	s := transcript(nv, rep(1), rep(2), rep(3), rep(4), "session", nil, []byte("bundle"))
	if !bytes.Contains(s, append([]byte("\x00\x00\x00\x07session"), 0, 0, 0, 0)) {
		t.Fatalf("session transcript lacks an empty secret field: %x", s)
	}
	m := calleeMessage(got, []byte("b"))
	if !bytes.HasPrefix(m, got) || len(m) != len(got)+4+32 {
		t.Fatal("callee message is not T | lp(sha256(bundle_b))")
	}
}

func TestSessionHandshake(t *testing.T) {
	alice, bob := newKeyring(t, "alice"), newKeyring(t, "bob")
	o := run(alice, Call{Op: OpSession, Want: bob.DevicePublic(), Bundle: bundleOf(t, alice)},
		bob, acceptAll(bob), nil, nil)
	if o.callErr != nil || o.ansErr != nil {
		t.Fatalf("call %v, answer %v", o.callErr, o.ansErr)
	}
	if !bytes.Equal(o.res.Device, bob.DevicePublic()) || !bytes.Equal(o.res.Bundle.Person.Person, bob.Person()) {
		t.Fatal("caller got the wrong callee identity")
	}
	if !bytes.Equal(o.req.Device, alice.DevicePublic()) || !bytes.Equal(o.req.Bundle.Person.Person, alice.Person()) || o.req.Op != OpSession {
		t.Fatal("callee got the wrong caller identity")
	}
	if o.res.Version != 2 || o.req.Version != 2 {
		t.Fatal("version is not 2")
	}
}

func TestPairAndJoinHandshake(t *testing.T) {
	alice, bob := newKeyring(t, "alice"), newKeyring(t, "bob")
	secret := bytes.Repeat([]byte{7}, SecretSize)
	o := run(bob, Call{Op: OpPair, Want: alice.DevicePublic(), Bundle: bundleOf(t, bob), Secret: secret},
		alice, func(r *Request) ([]byte, error) {
			if r.Op != OpPair || !bytes.Equal(r.Secret, secret) {
				return nil, errors.New("bad")
			}
			return bundleOf(t, alice), nil
		}, nil, nil)
	if o.callErr != nil || o.ansErr != nil {
		t.Fatalf("pair: call %v, answer %v", o.callErr, o.ansErr)
	}

	joiner, err := identity.NewDevice(t.TempDir(), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	o = run(joiner, Call{Op: OpJoin, Want: alice.DevicePublic(), Bundle: joiner.Endpoints().Marshal(), Secret: secret},
		alice, func(r *Request) ([]byte, error) {
			if r.Op != OpJoin || !bytes.Equal(r.Endpoints.Device, joiner.DevicePublic()) {
				return nil, errors.New("bad")
			}
			if _, err := alice.AddDevice(r.Device, "phone", 0, time.Now()); err != nil {
				return nil, err
			}
			return bundleOf(t, alice), nil
		}, nil, nil)
	if o.callErr != nil || o.ansErr != nil {
		t.Fatalf("join: call %v, answer %v", o.callErr, o.ansErr)
	}
	if _, ok := o.res.Bundle.Devices.Find(joiner.DevicePublic()); !ok {
		t.Fatal("join reply does not carry the new device list")
	}
	if err := joiner.Adopt(o.res.Bundle.Person, o.res.Bundle.Devices); err != nil {
		t.Fatal(err)
	}
}

func TestDeclineIsSilent(t *testing.T) {
	alice, bob := newKeyring(t, "alice"), newKeyring(t, "bob")
	o := run(alice, Call{Op: OpSession, Want: bob.DevicePublic(), Bundle: bundleOf(t, alice)},
		bob, func(*Request) ([]byte, error) { return nil, errors.New("not a contact") }, nil, nil)
	if !errors.Is(o.callErr, ErrRejected) || !errors.Is(o.ansErr, ErrDeclined) {
		t.Fatalf("call %v, answer %v", o.callErr, o.ansErr)
	}
}

func TestWrongDevice(t *testing.T) {
	alice, bob, carol := newKeyring(t, "alice"), newKeyring(t, "bob"), newKeyring(t, "carol")
	called := false
	o := run(alice, Call{Op: OpSession, Want: carol.DevicePublic(), Bundle: bundleOf(t, alice)},
		bob, func(*Request) ([]byte, error) { called = true; return bundleOf(t, bob), nil }, nil, nil)
	if !errors.Is(o.callErr, ErrWrongDevice) || o.ansErr == nil || called {
		t.Fatalf("call %v, answer %v, decided %v", o.callErr, o.ansErr, called)
	}

	// A device not in its own person's list (removed) is refused before
	// Decide runs.
	phone, _ := identity.NewDevice(t.TempDir(), time.Now())
	if _, err := alice.AddDevice(phone.DevicePublic(), "phone", 0, time.Now()); err != nil {
		t.Fatal(err)
	}
	pb, _ := alice.Bundle()
	if err := phone.Adopt(pb.Person, pb.Devices); err != nil {
		t.Fatal(err)
	}
	if _, err := alice.RemoveDevice(phone.DevicePublic(), time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	stale := bundleOf(t, phone) // still lists the phone: refused only by the callee's newer state
	b2, _ := alice.Bundle()
	forged := identity.Bundle{Person: b2.Person, Devices: b2.Devices, Endpoints: phone.Endpoints()}
	o = run(phone, Call{Op: OpSession, Want: bob.DevicePublic(), Bundle: forged.Marshal()},
		bob, acceptAll(bob), nil, nil)
	if !errors.Is(o.callErr, ErrRejected) || !errors.Is(o.ansErr, identity.ErrNotInList) {
		t.Fatalf("removed device: call %v, answer %v", o.callErr, o.ansErr)
	}
	o = run(phone, Call{Op: OpSession, Want: bob.DevicePublic(), Bundle: stale},
		bob, acceptAll(bob), nil, nil)
	if o.callErr != nil || !bytes.Equal(o.req.Device, phone.DevicePublic()) {
		t.Fatalf("stale bundle should pass the wire layer (the cache refuses it): %v", o.callErr)
	}
}

// flipConn flips the low bit of the byte at read offset pos.
type flipConn struct {
	net.Conn
	pos, off int
}

func (f *flipConn) Read(p []byte) (int, error) {
	n, err := f.Conn.Read(p)
	if i := f.pos - f.off; i >= 0 && i < n {
		p[i] ^= 1
	}
	f.off += n
	return n, err
}

// countConn counts bytes written and records them.
type countConn struct {
	net.Conn
	mu sync.Mutex
	w  bytes.Buffer
}

func (c *countConn) Write(p []byte) (int, error) {
	c.mu.Lock()
	c.w.Write(p)
	c.mu.Unlock()
	return c.Conn.Write(p)
}

func (c *countConn) written() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return bytes.Clone(c.w.Bytes())
}

// TestTamperEveryByte flips every byte the caller sends, then every byte
// the callee sends, and checks that no handshake survives.
func TestTamperEveryByte(t *testing.T) {
	if testing.Short() {
		t.Skip("slow")
	}
	old := HandshakeTimeout
	HandshakeTimeout = 500 * time.Millisecond // a corrupted length waits for bytes that never come
	defer func() { HandshakeTimeout = old }()

	alice, bob := newKeyring(t, "alice"), newKeyring(t, "bob")
	secret := bytes.Repeat([]byte{9}, SecretSize)
	call := Call{Op: OpPair, Want: bob.DevicePublic(), Bundle: bundleOf(t, alice), Secret: secret}

	var ca, cb *countConn
	o := run(alice, call, bob, acceptAll(bob),
		func(c net.Conn) net.Conn { ca = &countConn{Conn: c}; return ca },
		func(c net.Conn) net.Conn { cb = &countConn{Conn: c}; return cb })
	if o.callErr != nil || o.ansErr != nil {
		t.Fatal(o.callErr, o.ansErr)
	}
	callerBytes, calleeBytes := len(ca.written()), len(cb.written())

	var wg sync.WaitGroup
	sem := make(chan struct{}, 16)
	check := func(pos int, callerSide bool) {
		defer wg.Done()
		defer func() { <-sem }()
		var wrapA, wrapB func(net.Conn) net.Conn
		if callerSide { // the callee reads tampered caller bytes
			wrapB = func(c net.Conn) net.Conn { return &flipConn{Conn: c, pos: pos} }
		} else {
			wrapA = func(c net.Conn) net.Conn { return &flipConn{Conn: c, pos: pos} }
		}
		o := run(alice, call, bob, acceptAll(bob), wrapA, wrapB)
		if o.callErr == nil {
			t.Errorf("flip at %d (caller side %v): handshake succeeded", pos, callerSide)
		}
		if callerSide && o.ansErr == nil {
			t.Errorf("flip at %d of caller bytes: callee accepted", pos)
		}
		if callerSide && !errors.Is(o.callErr, ErrRejected) && !errors.Is(o.callErr, ErrTruncated) {
			t.Errorf("flip at %d of caller bytes: caller saw %v, want a silent rejection", pos, o.callErr)
		}
	}
	for pos := range callerBytes {
		wg.Add(1)
		sem <- struct{}{}
		go check(pos, true)
	}
	for pos := range calleeBytes {
		wg.Add(1)
		sem <- struct{}{}
		go check(pos, false)
	}
	wg.Wait()
}

// TestTamperFields rewrites each request field with a well-formed but wrong
// value after signing.
func TestTamperFields(t *testing.T) {
	alice, bob, carol := newKeyring(t, "alice"), newKeyring(t, "bob"), newKeyring(t, "carol")
	secret := bytes.Repeat([]byte{9}, SecretSize)
	other := bytes.Repeat([]byte{8}, 32)
	cases := map[string]func(r *request, b *[]byte){
		"v":      func(r *request, _ *[]byte) { r.V = 3 },
		"op":     func(r *request, _ *[]byte) { r.Op = OpJoin },
		"device": func(r *request, _ *[]byte) { r.Device = b64.EncodeToString(carol.DevicePublic()) },
		"nonce":  func(r *request, _ *[]byte) { r.Nonce = b64.EncodeToString(other) },
		"secret": func(r *request, _ *[]byte) { r.Secret = b64.EncodeToString(other) },
		"no secret": func(r *request, _ *[]byte) {
			r.Secret = ""
		},
		"sig":    func(r *request, _ *[]byte) { r.Sig = b64.EncodeToString(make([]byte, 64)) },
		"bundle": func(_ *request, b *[]byte) { *b = bundleOf(t, carol) },
		"device and bundle": func(r *request, b *[]byte) {
			r.Device = b64.EncodeToString(carol.DevicePublic())
			*b = bundleOf(t, carol)
		},
	}
	for name, tamper := range cases {
		c := Call{Op: OpPair, Want: bob.DevicePublic(), Bundle: bundleOf(t, alice), Secret: secret, tamper: tamper}
		o := run(alice, c, bob, acceptAll(bob), nil, nil)
		if !errors.Is(o.callErr, ErrRejected) || o.ansErr == nil {
			t.Errorf("%s: call %v, answer %v", name, o.callErr, o.ansErr)
		}
	}
}

func TestReplayAgainstNewNonce(t *testing.T) {
	alice, bob := newKeyring(t, "alice"), newKeyring(t, "bob")
	var rec *countConn
	o := run(alice, Call{Op: OpSession, Want: bob.DevicePublic(), Bundle: bundleOf(t, alice)},
		bob, acceptAll(bob), func(c net.Conn) net.Conn { rec = &countConn{Conn: c}; return rec }, nil)
	if o.callErr != nil {
		t.Fatal(o.callErr)
	}
	recorded := rec.written()

	a, b := net.Pipe()
	errc := make(chan error, 1)
	decided := false
	go func() {
		_, err := Answer(b, bob, func(*Request) ([]byte, error) { decided = true; return bundleOf(t, bob), nil })
		errc <- err
	}()
	a.Write(recorded[:prefixSize])
	hello := make([]byte, helloSize)
	if _, err := io.ReadFull(a, hello); err != nil {
		t.Fatal(err)
	}
	go a.Write(recorded[prefixSize:])
	rest, _ := io.ReadAll(a)
	if len(rest) != 0 {
		t.Fatalf("callee sent %d bytes to a replay", len(rest))
	}
	if err := <-errc; !errors.Is(err, ErrBadSignature) || decided {
		t.Fatalf("replay: %v (decided %v)", err, decided)
	}
}

func TestIncompatibleGetsOnlyPrefix(t *testing.T) {
	bob := newKeyring(t, "bob")
	a, b := net.Pipe()
	errc := make(chan error, 1)
	go func() { _, err := Answer(b, bob, acceptAll(bob)); errc <- err }()
	go a.Write([]byte("SOLH\x00\x01\x00\x01"))
	got, _ := io.ReadAll(a)
	if !bytes.Equal(got, []byte("SOLH\x00\x02\x00\x02")) {
		t.Fatalf("incompatible caller got %q", got)
	}
	var ie *IncompatibleError
	if err := <-errc; !errors.As(err, &ie) || !ie.PeerOlder() {
		t.Fatalf("answer: %v", err)
	}

	// The caller side reports which side must update.
	alice := newKeyring(t, "alice")
	a, b = net.Pipe()
	go Answer(b, bob, acceptAll(bob))
	_, err := dial(a, alice, Call{Op: OpSession, Want: bob.DevicePublic(), Bundle: bundleOf(t, alice)}, Versions{3, 4})
	if !errors.As(err, &ie) || !ie.PeerOlder() || !errors.Is(err, ErrIncompatible) {
		t.Fatalf("dial: %v", err)
	}
	a.Close()
}

func TestNonSolClientGetsNothing(t *testing.T) {
	bob := newKeyring(t, "bob")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go Answer(c, bob, acceptAll(bob))
		}
	}()
	for _, probe := range []string{
		"GET / HTTP/1.1\r\nHost: x\r\n\r\n",
		"SSH-2.0-OpenSSH_9.6\r\n",
		"SOLH\x00\x03\x00\x02", // invalid range
		"\x16\x03\x01\x02\x00\x01\x00\x01\xfc\x03\x03",
		"",
	} {
		c, err := net.Dial("tcp", ln.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		c.Write([]byte(probe))
		c.(*net.TCPConn).CloseWrite()
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		got, err := io.ReadAll(c)
		c.Close()
		// A reset is fine (closing with unread input sends RST); data is not.
		if len(got) != 0 || (err != nil && !errors.Is(err, syscall.ECONNRESET) && !strings.Contains(err.Error(), "reset")) {
			t.Errorf("probe %q got %q, err %v", probe, got, err)
		}
	}
}

func TestDialArguments(t *testing.T) {
	alice := newKeyring(t, "alice")
	a, b := net.Pipe()
	defer b.Close()
	if _, err := Dial(a, alice, Call{Op: OpSession}); err == nil {
		t.Fatal("dial without a target device succeeded")
	}
	a, b = net.Pipe()
	defer b.Close()
	if _, err := Dial(a, alice, Call{Op: OpPair, Want: ed25519.PublicKey(make([]byte, 32))}); err == nil {
		t.Fatal("pair without a secret succeeded")
	}
}
