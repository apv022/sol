package wire

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"

	"github.com/apv022/sol/internal/identity"
)

// Call is the caller's input to Dial.
type Call struct {
	Op string // OpSession, OpPair, OpJoin, OpRecover or OpProbe
	// Want is the device the caller means to reach (from the pairing link
	// or the cached bundle). Required: the callee is always authenticated.
	Want ed25519.PublicKey
	// Bundle is the caller's bundle_a in wire form. For OpJoin, which a
	// device makes before it has a person, it is the device's signed
	// endpoint record instead.
	Bundle []byte
	// Secret is the one-time secret for OpPair and OpJoin.
	Secret []byte
	// Name is the joining device's display name (OpJoin only).
	Name string
	// Recovery is the recovery key R (OpRecover only). It signs T next to the device key and
	// never leaves this process.
	Recovery ed25519.PrivateKey
	// Gate, if set, runs after the callee's Hello and before the Request is
	// sent: the last moment the caller can still back out without the
	// callee completing its side. A hedged dialer uses it so that only one
	// of several parallel attempts commits at a time. An error aborts the
	// handshake; release, if not nil, runs when Dial returns.
	Gate func() (release func(), err error)

	// tamper, when set (tests only), may rewrite the request and bundle
	// after signing.
	tamper func(r *request, bundle *[]byte)
}

// Result is a completed handshake as the caller sees it.
type Result struct {
	Version uint16
	Device  ed25519.PublicKey // the callee's device key, = Call.Want
	// Bundle is the callee's identity bundle, already checked with
	// identity.VerifyBundle against Device.
	Bundle identity.Bundle
}

// Dial runs the caller side of the handshake on conn. On any error conn is
// closed. On success conn is ready to carry the session.
func Dial(conn net.Conn, local Local, c Call) (*Result, error) {
	res, err := dial(conn, local, c, Supported())
	if err != nil {
		conn.Close()
		return nil, err
	}
	return res, nil
}

func dial(conn net.Conn, local Local, c Call, v Versions) (*Result, error) {
	if len(c.Want) != keySize {
		return nil, errors.New("wire: no device to reach")
	}
	if !validOp(c.Op) || hasSecret(c.Op) != (len(c.Secret) > 0) || (c.Op == OpRecover) != (c.Recovery != nil) ||
		(c.Name != "" && c.Op != OpJoin) || len(c.Name) > identity.MaxNameLen {
		return nil, fmt.Errorf("wire: bad call op %q", c.Op)
	}
	conn.SetDeadline(time.Now().Add(HandshakeTimeout))
	defer conn.SetDeadline(time.Time{})

	if _, err := conn.Write(prefix(v)); err != nil {
		return nil, err
	}
	hello := make([]byte, helloSize)
	if _, err := io.ReadFull(conn, hello[:prefixSize]); err != nil {
		return nil, rejectedOr(err)
	}
	calleeV, err := parsePrefix(hello)
	if err != nil {
		return nil, err
	}
	version, ok := Negotiate(v, calleeV)
	if !ok {
		return nil, &IncompatibleError{Local: v, Remote: calleeV}
	}
	if _, err := io.ReadFull(conn, hello[prefixSize:]); err != nil {
		return nil, rejectedOr(err)
	}
	db := ed25519.PublicKey(bytes.Clone(hello[prefixSize : prefixSize+keySize]))
	nb := hello[prefixSize+keySize:]
	if !bytes.Equal(db, c.Want) {
		return nil, ErrWrongDevice
	}

	da := local.DevicePublic()
	na, err := randomNonce()
	if err != nil {
		return nil, err
	}
	nv := negotiated{version, v, calleeV}
	var extra []byte
	switch c.Op {
	case OpJoin:
		extra = []byte(c.Name)
	case OpRecover:
		extra = c.Recovery.Public().(ed25519.PublicKey)
	}
	t := transcript(nv, da, db, na, nb, c.Op, c.Secret, c.Bundle, extra)
	sig, err := sign(local.Signer(), t)
	if err != nil {
		return nil, err
	}
	req := request{V: int(version), Op: c.Op, Device: b64.EncodeToString(da),
		Nonce: b64.EncodeToString(na), Name: c.Name, Sig: b64.EncodeToString(sig)}
	if len(c.Secret) > 0 {
		req.Secret = b64.EncodeToString(c.Secret)
	}
	if c.Op == OpRecover {
		req.Person = b64.EncodeToString(extra)
		req.RSig = b64.EncodeToString(ed25519.Sign(c.Recovery, t))
	}
	bundle := c.Bundle
	if c.tamper != nil {
		c.tamper(&req, &bundle)
	}
	if c.Gate != nil {
		release, err := c.Gate()
		if err != nil {
			return nil, err
		}
		if release != nil {
			defer release()
		}
	}
	js, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	out, err := appendFrame(nil, js)
	if err != nil {
		return nil, err
	}
	if out, err = appendBundleFrame(out, bundle); err != nil {
		return nil, err
	}
	if _, err := conn.Write(out); err != nil {
		return nil, rejectedOr(err)
	}

	var reply [1]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		return nil, rejectedOr(err)
	}
	if reply[0] != replyOK {
		return nil, fmt.Errorf("%w: reply byte %#x", ErrMalformed, reply[0])
	}
	rawB, err := readBundleFrame(conn)
	if err != nil {
		return nil, err
	}
	sigB := make([]byte, ed25519.SignatureSize)
	if _, err := io.ReadFull(conn, sigB); err != nil {
		return nil, readErr(err)
	}
	if !ed25519.Verify(db, calleeMessage(t, rawB), sigB) {
		return nil, ErrBadSignature
	}
	b, err := identity.ParseBundle(rawB)
	if err != nil {
		return nil, fmt.Errorf("%w: callee bundle: %v", ErrMalformed, err)
	}
	if _, err := identity.VerifyBundle(b, db); err != nil {
		return nil, fmt.Errorf("callee bundle: %w", err)
	}
	return &Result{Version: version, Device: db, Bundle: b}, nil
}

// rejectedOr maps a closed or reset connection to ErrRejected; a timeout
// stays a timeout.
func rejectedOr(err error) error {
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return readErr(err)
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, net.ErrClosed) || errors.Is(err, io.ErrClosedPipe) {
		return ErrRejected
	}
	var oe *net.OpError
	if errors.As(err, &oe) {
		return ErrRejected
	}
	return err
}
