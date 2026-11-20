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

// Request is a verified caller as the callee sees it, handed to Decide.
type Request struct {
	Version uint16
	Op      string
	Device  ed25519.PublicKey // D_a, proven by the transcript signature
	Secret  []byte            // pair and join only
	// Bundle is the caller's identity bundle, checked with
	// identity.VerifyBundle against Device. Zero for OpJoin.
	Bundle identity.Bundle
	// Endpoints is the caller's signed endpoint record (for OpJoin and
	// OpRecover the only thing it presents).
	Endpoints identity.EndpointRecord
	// Name is the joining device's display name (OpJoin; may be empty).
	Name string
	// Person is the person the caller proved with the recovery key (OpRecover).
	Person ed25519.PublicKey
}

// Decide is the callee's policy. It returns the wire form of the bundle to
// present (bundle_b), computed after the decision so a join can include the
// newly signed device list. Any error rejects the caller silently.
type Decide func(r *Request) (bundle []byte, err error)

// Answer runs the callee side of the handshake on conn. On any error conn is
// closed, having sent nothing beyond the Hello (or, to a Sol caller with no
// common version, the 8-byte prefix). On success conn is ready to carry the
// session.
func Answer(conn net.Conn, local Local, decide Decide) (*Request, error) {
	r, err := answer(conn, local, decide, Supported())
	if err != nil {
		conn.Close()
		return nil, err
	}
	return r, nil
}

func answer(conn net.Conn, local Local, decide Decide, v Versions) (*Request, error) {
	conn.SetDeadline(time.Now().Add(HandshakeTimeout))
	defer conn.SetDeadline(time.Time{})

	ch := make([]byte, prefixSize)
	if n, err := io.ReadFull(conn, ch); err != nil {
		if n == 0 && errors.Is(err, io.EOF) {
			return nil, ErrNoData
		}
		if n >= len(magic) && string(ch[:len(magic)]) != magic {
			return nil, fmt.Errorf("%w: not a sol device", ErrMalformed)
		}
		return nil, readErr(err)
	}
	callerV, err := parsePrefix(ch)
	if err != nil {
		return nil, err
	}
	version, ok := Negotiate(callerV, v)
	if !ok {
		if _, err := conn.Write(prefix(v)); err != nil {
			return nil, err
		}
		return nil, &IncompatibleError{Local: v, Remote: callerV}
	}

	db := local.DevicePublic()
	nb, err := randomNonce()
	if err != nil {
		return nil, err
	}
	hello := append(prefix(v), db...)
	hello = append(hello, nb...)
	if _, err := conn.Write(hello); err != nil {
		return nil, err
	}

	frame, err := readFrame(conn)
	if err != nil {
		return nil, err
	}
	rawA, err := readFrame(conn)
	if err != nil {
		return nil, err
	}
	req, sig, rsig, na, err := parseRequest(frame, version)
	if err != nil {
		return nil, err
	}
	if bytes.Equal(req.Device, db) {
		return nil, fmt.Errorf("%w: a device cannot call itself", ErrMalformed)
	}
	nv := negotiated{version, callerV, v}
	var extra []byte
	switch req.Op {
	case OpJoin:
		extra = []byte(req.Name)
	case OpRecover:
		extra = req.Person
	}
	t := transcript(nv, req.Device, db, na, nb, req.Op, req.Secret, rawA, extra)
	if !ed25519.Verify(req.Device, t, sig) {
		return nil, ErrBadSignature
	}
	if req.Op == OpRecover && !ed25519.Verify(req.Person, t, rsig) {
		return nil, ErrBadSignature
	}
	if req.Bundle, req.Endpoints, err = checkCallerBundle(req.Op, rawA, req.Device); err != nil {
		return nil, fmt.Errorf("caller bundle: %w", err)
	}

	rawB, err := decide(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrDeclined, err)
	}
	sigB, err := sign(local.Signer(), calleeMessage(t, rawB))
	if err != nil {
		return nil, err
	}
	out, err := appendFrame([]byte{replyOK}, rawB)
	if err != nil {
		return nil, err
	}
	out = append(out, sigB...)
	if _, err := conn.Write(out); err != nil {
		return nil, err
	}
	return req, nil
}

// parseRequest decodes and checks the Request JSON: canonical form, the
// negotiated version, a known op, field sizes. It returns the request, the
// caller's signature, the recovery signature (recover only) and the nonce.
func parseRequest(frame []byte, version uint16) (*Request, []byte, []byte, []byte, error) {
	var rq request
	dec := json.NewDecoder(bytes.NewReader(frame))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&rq); err != nil {
		return nil, nil, nil, nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if canon, err := json.Marshal(rq); err != nil || !bytes.Equal(canon, frame) {
		return nil, nil, nil, nil, fmt.Errorf("%w: request is not canonical JSON", ErrMalformed)
	}
	if rq.V != int(version) {
		return nil, nil, nil, nil, fmt.Errorf("%w: request version %d, negotiated %d", ErrMalformed, rq.V, version)
	}
	if !validOp(rq.Op) {
		return nil, nil, nil, nil, fmt.Errorf("%w: unknown op %q", ErrMalformed, rq.Op)
	}
	device, err1 := b64.DecodeString(rq.Device)
	nonce, err2 := b64.DecodeString(rq.Nonce)
	sig, err3 := b64.DecodeString(rq.Sig)
	secret, err4 := b64.DecodeString(rq.Secret)
	if err := errors.Join(err1, err2, err3, err4); err != nil ||
		len(device) != keySize || len(nonce) != NonceSize || len(sig) != ed25519.SignatureSize {
		return nil, nil, nil, nil, fmt.Errorf("%w: bad field encoding", ErrMalformed)
	}
	if hasSecret(rq.Op) != (len(secret) == SecretSize) || (!hasSecret(rq.Op) && len(secret) != 0) {
		return nil, nil, nil, nil, fmt.Errorf("%w: secret does not fit op %q", ErrMalformed, rq.Op)
	}
	person, err5 := b64.DecodeString(rq.Person)
	rsig, err6 := b64.DecodeString(rq.RSig)
	if err := errors.Join(err5, err6); err != nil ||
		(rq.Op == OpRecover) != (len(person) == keySize && len(rsig) == ed25519.SignatureSize) ||
		(rq.Op != OpRecover && (len(person) != 0 || len(rsig) != 0)) {
		return nil, nil, nil, nil, fmt.Errorf("%w: recovery fields do not fit op %q", ErrMalformed, rq.Op)
	}
	if rq.Name != "" && (rq.Op != OpJoin || len(rq.Name) > identity.MaxNameLen) {
		return nil, nil, nil, nil, fmt.Errorf("%w: name does not fit op %q", ErrMalformed, rq.Op)
	}
	r := &Request{Version: version, Op: rq.Op, Device: ed25519.PublicKey(device), Name: rq.Name}
	if len(secret) > 0 {
		r.Secret = secret
	}
	if len(person) > 0 {
		r.Person = ed25519.PublicKey(person)
	}
	return r, sig, rsig, nonce, nil
}
