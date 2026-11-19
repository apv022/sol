// Package wire implements Sol's version 2 handshake (design/PROTOCOL.md
// §Handshake): the exchange that turns any byte stream into an authenticated
// connection between two devices, before it becomes a mux session.
//
//	caller -> callee : ClientHello "SOLH" | u16 min | u16 max
//	callee -> caller : Hello       "SOLH" | u16 min | u16 max | D_b | nonce_b
//	caller -> callee : frame(Request JSON) | frame(bundle_a)
//	callee -> caller : 0x01 | frame(bundle_b) | sig_b (64 bytes)
//	                   or the callee closes without sending anything
//
// Invariants:
//
//   - The caller speaks first, and the callee answers a non-Sol client with
//     zero bytes. A Sol client whose versions don't overlap gets only the
//     fixed 8-byte prefix (magic and range), never more.
//   - Every rejection on the callee side is a silent close (AGENTS.md rule 7);
//     the reason is returned to the local caller for its log only.
//   - Both sides sign the transcript T, which covers both device keys, both
//     fresh nonces, the negotiated versions, the op, the secret and the hash
//     of the caller's bundle; the callee's signature also covers its own
//     bundle. A signature is therefore valid for exactly one connection
//     between exactly these two devices.
//   - The Request JSON must be in canonical form (what encoding/json emits
//     for the decoded value), so no field can be smuggled in a form the
//     signature doesn't pin.
//   - The whole handshake runs under one deadline, HandshakeTimeout.
//
// This package verifies signatures and identity chains. Whether the verified
// device may connect (contact, own device, a valid pairing token) is decided
// by the callee's Decide callback (internal/session).
package wire

import (
	"bytes"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/apv022/sol/internal/identity"
)

// The protocol versions this build speaks. Version 2 is a clean break from
// the proof of concept's version 1.
const (
	MinVersion = 2
	MaxVersion = 2
)

// Operations a caller can request.
const (
	OpSession = "session" // an authenticated session between known devices
	OpPair    = "pair"    // redeem a pair token: the two people become contacts
	OpJoin    = "join"    // redeem a join token: the caller joins the callee's person
)

// Sizes on the wire.
const (
	magic      = "SOLH"
	prefixSize = len(magic) + 4 // magic | min | max, in both hellos
	keySize    = ed25519.PublicKeySize
	NonceSize  = 32
	helloSize  = prefixSize + keySize + NonceSize
	// MaxFrame is the largest handshake frame payload.
	MaxFrame = 16384
	// SecretSize is the size of a pair or join secret.
	SecretSize = 32
	replyOK    = 0x01
	sigContext = "sol handshake v2"
)

// HandshakeTimeout bounds the whole handshake on either side. Tor round
// trips take seconds, so it is generous. Tests may shorten it.
var HandshakeTimeout = 45 * time.Second

// Errors. The callee's errors are for its local log only; the remote side
// learns nothing but a closed connection.
var (
	// ErrRejected means the callee closed instead of accepting. By design
	// the caller cannot tell why.
	ErrRejected = errors.New("wire: the other device rejected the connection")
	// ErrWrongDevice means the callee's device key is not the one the
	// caller meant to reach.
	ErrWrongDevice = errors.New("wire: reached a different device than expected")
	// ErrBadSignature means a transcript signature did not verify.
	ErrBadSignature = errors.New("wire: bad handshake signature")
	// ErrMalformed means the bytes are not a well-formed handshake.
	ErrMalformed = errors.New("wire: malformed handshake")
	// ErrIncompatible means the two sides share no version; the error is an
	// *IncompatibleError saying which side must update.
	ErrIncompatible = errors.New("wire: incompatible Sol versions")
	// ErrNoData means the other side connected and closed without sending
	// anything (reachability probes).
	ErrNoData = errors.New("wire: connection closed before any data")
	// ErrTruncated means the stream ended or timed out mid-handshake.
	ErrTruncated = errors.New("wire: connection closed during handshake")
	// ErrDeclined is returned by Answer when Decide refused the caller.
	ErrDeclined = errors.New("wire: declined by local policy")
)

// Versions is an inclusive range of protocol versions.
type Versions struct{ Min, Max uint16 }

// Supported returns this build's range.
func Supported() Versions { return Versions{MinVersion, MaxVersion} }

// Valid reports whether 1 <= Min <= Max.
func (v Versions) Valid() bool { return v.Min >= 1 && v.Min <= v.Max }

// String renders v as "2" or "2-3".
func (v Versions) String() string {
	if v.Min == v.Max {
		return fmt.Sprint(v.Min)
	}
	return fmt.Sprintf("%d-%d", v.Min, v.Max)
}

// Negotiate returns the highest version in both ranges.
func Negotiate(a, b Versions) (uint16, bool) {
	lo, hi := max(a.Min, b.Min), min(a.Max, b.Max)
	if !a.Valid() || !b.Valid() || hi < lo {
		return 0, false
	}
	return hi, true
}

// IncompatibleError is returned when the ranges don't overlap.
type IncompatibleError struct{ Local, Remote Versions }

// PeerOlder reports whether the other side must update.
func (e *IncompatibleError) PeerOlder() bool { return e.Remote.Max < e.Local.Min }

func (e *IncompatibleError) Error() string {
	if e.PeerOlder() {
		return fmt.Sprintf("the other device runs an older Sol (protocol %s, this one needs %d or newer): update Sol there", e.Remote, e.Local.Min)
	}
	return fmt.Sprintf("the other device runs a newer Sol (protocol %s, this one speaks up to %d): update Sol here", e.Remote, e.Local.Max)
}

// Is makes errors.Is(err, ErrIncompatible) match.
func (e *IncompatibleError) Is(target error) bool { return target == ErrIncompatible }

// Local is this device's side of a handshake. *identity.Keyring implements it.
type Local interface {
	DevicePublic() ed25519.PublicKey
	Signer() crypto.Signer
}

// request is the JSON of the Request frame. Field order is the canonical
// order; see canonical check in readRequest.
type request struct {
	V      int    `json:"v"`
	Op     string `json:"op"`
	Device string `json:"device"`
	Nonce  string `json:"nonce"`
	Secret string `json:"secret,omitempty"`
	Sig    string `json:"sig"`
}

var b64 = base64.StdEncoding.Strict()

// negotiated is what both sides sign about versions.
type negotiated struct {
	version        uint16
	caller, callee Versions
}

// bytes is the 10-byte versions field of T: version, caller min/max,
// callee min/max.
func (n negotiated) bytes() []byte {
	b := make([]byte, 0, 10)
	for _, x := range []uint16{n.version, n.caller.Min, n.caller.Max, n.callee.Min, n.callee.Max} {
		b = binary.BigEndian.AppendUint16(b, x)
	}
	return b
}

// lp appends a 4-byte big-endian length and x (IDENTITY.md §Encoding).
func lp(b, x []byte) []byte {
	b = binary.BigEndian.AppendUint32(b, uint32(len(x)))
	return append(b, x...)
}

// transcript builds T (PROTOCOL.md §Signatures).
func transcript(nv negotiated, da, db, na, nb []byte, op string, secret, bundleA []byte) []byte {
	h := sha256.Sum256(bundleA)
	t := lp(nil, []byte(sigContext))
	t = append(t, nv.bytes()...)
	t = lp(t, da)
	t = lp(t, db)
	t = lp(t, na)
	t = lp(t, nb)
	t = lp(t, []byte(op))
	t = lp(t, secret)
	return lp(t, h[:])
}

// calleeMessage is what the callee signs: T | lp(sha256(bundle_b)).
func calleeMessage(t, bundleB []byte) []byte {
	h := sha256.Sum256(bundleB)
	return lp(append([]byte(nil), t...), h[:])
}

func sign(s crypto.Signer, msg []byte) ([]byte, error) {
	return s.Sign(rand.Reader, msg, crypto.Hash(0))
}

func prefix(v Versions) []byte {
	b := append([]byte(magic), 0, 0, 0, 0)
	binary.BigEndian.PutUint16(b[4:], v.Min)
	binary.BigEndian.PutUint16(b[6:], v.Max)
	return b
}

func parsePrefix(b []byte) (Versions, error) {
	if string(b[:len(magic)]) != magic {
		return Versions{}, fmt.Errorf("%w: not a sol device", ErrMalformed)
	}
	v := Versions{binary.BigEndian.Uint16(b[4:]), binary.BigEndian.Uint16(b[6:])}
	if !v.Valid() {
		return Versions{}, fmt.Errorf("%w: bad version range %d-%d", ErrMalformed, v.Min, v.Max)
	}
	return v, nil
}

func readErr(err error) error {
	var ne net.Error
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, os.ErrDeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return fmt.Errorf("%w: %v", ErrTruncated, err)
	}
	return fmt.Errorf("%w: %v", ErrMalformed, err)
}

func appendFrame(b, data []byte) ([]byte, error) {
	if len(data) == 0 || len(data) > MaxFrame {
		return nil, fmt.Errorf("wire: frame of %d bytes (1..%d)", len(data), MaxFrame)
	}
	b = binary.BigEndian.AppendUint16(b, uint16(len(data)))
	return append(b, data...), nil
}

func readFrame(r io.Reader) ([]byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, readErr(err)
	}
	n := int(binary.BigEndian.Uint16(hdr[:]))
	if n == 0 || n > MaxFrame {
		return nil, fmt.Errorf("%w: frame size %d", ErrMalformed, n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, readErr(err)
	}
	return buf, nil
}

func randomNonce() ([]byte, error) {
	n := make([]byte, NonceSize)
	_, err := rand.Read(n)
	return n, err
}

func validOp(op string) bool { return op == OpSession || op == OpPair || op == OpJoin }

// hasSecret reports whether op carries a one-time secret.
func hasSecret(op string) bool { return op == OpPair || op == OpJoin }

// checkCallerBundle verifies what the caller presented: a full identity
// bundle naming its device, or for join (the device has no person yet) only
// its own signed endpoint record.
func checkCallerBundle(op string, raw []byte, device ed25519.PublicKey) (identity.Bundle, identity.EndpointRecord, error) {
	if op == OpJoin {
		rec, err := identity.ParseEndpointRecord(raw)
		if err != nil {
			return identity.Bundle{}, rec, err
		}
		if err := rec.Verify(); err != nil {
			return identity.Bundle{}, rec, err
		}
		if !bytes.Equal(rec.Device, device) {
			return identity.Bundle{}, rec, fmt.Errorf("%w: endpoint record names another device", ErrMalformed)
		}
		return identity.Bundle{}, rec, nil
	}
	b, err := identity.ParseBundle(raw)
	if err != nil {
		return b, identity.EndpointRecord{}, err
	}
	if _, err := identity.VerifyBundle(b, device); err != nil {
		return b, identity.EndpointRecord{}, err
	}
	return b, b.Endpoints, nil
}
