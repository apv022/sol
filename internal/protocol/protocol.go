// Package protocol implements Sol's connection handshake.
//
// Every connection one node opens to another (over Tor, I2P, or clearnet)
// starts with one challenge-response round trip. After it succeeds, the
// connection is a raw byte pipe to the requested service. The full
// specification, including the byte layout, is in docs/PROTOCOL.md.
//
//	caller -> callee : ClientHello = "SOLH" | min u16 | max u16
//	callee -> caller : Hello       = "SOLH" | min u16 | max u16 | callee ed25519 pubkey (32) | nonce (32)
//	caller -> callee : Request     = uint16 length | JSON {v, op, service, pubkey, sig, ...}
//	callee -> caller : 0x01 (OK), optionally followed by a JSON frame
//	                   or the callee closes the connection without sending anything.
//
// The caller speaks first, so anything that is not a Sol node (port
// scanners, HTTP probes) gets no bytes back at all.
//
// Both hellos open with the same 8 bytes, fixed for every protocol version:
// the magic and the sender's supported version range. Each side picks the
// highest version both speak (Negotiate). When the ranges don't overlap the
// callee sends only those 8 bytes, which every Sol caller gets anyway, and
// closes; the caller reports an *IncompatibleError that says which side must
// update. The negotiated version and both ranges are covered by the
// signature, so a man in the middle who rewrites a hello to force an older
// version breaks the signature.
//
// The signature covers a domain-separation tag, the callee's public key, the
// fresh nonce, and every request field (including the pairing card), so a
// signature can never be replayed on another connection, another node, or
// for another service, and nothing in the request can be swapped in transit.
//
// This package only checks that the request is well formed and correctly
// signed. Deciding whether the key is a peer is the caller's job (see
// internal/node).
//
// The protocol is versioned from v0.1.0. Version 1 is locked: never change
// a version in place, add a new one (docs/PROTOCOL.md, "Versioning").
package protocol

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"time"

	"github.com/apv022/sol/internal/identity"
)

// The protocol versions this build speaks. Raise MaxVersion when adding a
// version; raise MinVersion only to drop an old one on purpose.
const (
	MinVersion = 1
	MaxVersion = 1
)

// Version is the newest protocol version this build speaks.
const Version = MaxVersion

// Versions is an inclusive range of protocol versions. The zero value means
// this build's range (MinVersion..MaxVersion).
//
// A version above MaxVersion runs the version 1 layout: such ranges exist
// only in tests that exercise negotiation. A real version 2 branches on the
// negotiated version.
type Versions struct {
	Min, Max uint16
}

// Supported returns this build's range.
func Supported() Versions { return Versions{MinVersion, MaxVersion} }

func (v Versions) orDefault() Versions {
	if v == (Versions{}) {
		return Supported()
	}
	return v
}

// Valid reports whether v is a usable range: 1 <= Min <= Max.
func (v Versions) Valid() bool { return v.Min >= 1 && v.Min <= v.Max }

// String renders v as "1" or "1-3".
func (v Versions) String() string {
	if v.Min == v.Max {
		return fmt.Sprint(v.Min)
	}
	return fmt.Sprintf("%d-%d", v.Min, v.Max)
}

// Negotiate returns the highest version both ranges contain, or false when
// they don't overlap.
func Negotiate(a, b Versions) (uint16, bool) {
	lo, hi := max(a.Min, b.Min), min(a.Max, b.Max)
	if !a.Valid() || !b.Valid() || hi < lo {
		return 0, false
	}
	return hi, true
}

// Operations a caller can request.
const (
	OpConnect = "connect" // pipe to Request.Service
	OpPair    = "pair"    // redeem Request.Secret with Request.Card; reply carries PeerInfo
	OpList    = "list"    // reply carries PeerInfo, then close
	OpUnpair  = "unpair"  // the caller removed us; remove it too. Reply is OK, then close
)

const (
	magic        = "SOLH"
	prefixSize   = len(magic) + 4 // magic | min u16 | max u16, in both hellos
	nonceSize    = 32
	helloSize    = prefixSize + ed25519.PublicKeySize + nonceSize
	maxFrameSize = 8 << 10
	replyOK      = 0x01
	sigDomain    = "sol handshake"
)

// HandshakeTimeout bounds the whole handshake. Tor round trips can take
// several seconds, so this is generous.
var HandshakeTimeout = 45 * time.Second

// Errors.
var (
	// ErrRejected means the callee closed the connection instead of
	// accepting. By design the caller cannot tell why (not a peer, removed,
	// service not shared).
	ErrRejected     = errors.New("the other node rejected the connection")
	ErrWrongNode    = errors.New("node identity does not match the expected node id")
	ErrBadSignature = errors.New("bad handshake signature")
	ErrMalformed    = errors.New("malformed handshake")
	// ErrIncompatible means the two nodes share no protocol version. The
	// error is an *IncompatibleError, which says which side is older.
	ErrIncompatible = errors.New("incompatible Sol versions")
	// ErrNoData means the other side connected and closed without sending
	// anything (reachability probes, including the node's own self-check).
	ErrNoData = errors.New("connection closed before any data")
	// ErrTruncated means the stream ended or timed out part-way through the
	// handshake (the path was cut, or held bytes never arrived). Nothing
	// that did arrive was invalid, so this is not ErrMalformed; callers log
	// it quietly. It relaxes no check: the handshake still fails.
	ErrTruncated = errors.New("connection closed during handshake")
)

// readErr classifies a failed handshake read: EOF, unexpected EOF and
// timeouts are ErrTruncated, anything else ErrMalformed.
func readErr(err error) error {
	var ne net.Error
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, os.ErrDeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()) {
		return fmt.Errorf("%w: %v", ErrTruncated, err)
	}
	return fmt.Errorf("%w: %v", ErrMalformed, err)
}

// IncompatibleError is returned when the two sides' version ranges don't
// overlap. Local is this node's range, Remote the other node's (as it
// announced it in its hello).
type IncompatibleError struct {
	Local, Remote Versions
}

// PeerOlder reports whether the other node is the one that must update.
func (e *IncompatibleError) PeerOlder() bool { return e.Remote.Max < e.Local.Min }

func (e *IncompatibleError) Error() string {
	if e.PeerOlder() {
		return fmt.Sprintf("the other node runs an older Sol (protocol %s, this node needs %d or newer): update Sol there", e.Remote, e.Local.Min)
	}
	return fmt.Sprintf("the other node runs a newer Sol (protocol %s, this node speaks up to %d): update Sol here", e.Remote, e.Local.Max)
}

// Is makes errors.Is(err, ErrIncompatible) match.
func (e *IncompatibleError) Is(target error) bool { return target == ErrIncompatible }

// prefix encodes the fixed start of both hellos.
func prefix(v Versions) []byte {
	b := make([]byte, prefixSize)
	copy(b, magic)
	binary.BigEndian.PutUint16(b[4:], v.Min)
	binary.BigEndian.PutUint16(b[6:], v.Max)
	return b
}

// parsePrefix decodes the fixed start of a hello. A wrong magic or an
// invalid range is ErrMalformed.
func parsePrefix(b []byte) (Versions, error) {
	if string(b[:len(magic)]) != magic {
		return Versions{}, fmt.Errorf("%w: not a sol node", ErrMalformed)
	}
	v := Versions{binary.BigEndian.Uint16(b[4:]), binary.BigEndian.Uint16(b[6:])}
	if !v.Valid() {
		return Versions{}, fmt.Errorf("%w: bad version range %d-%d", ErrMalformed, v.Min, v.Max)
	}
	return v, nil
}

// negotiated is what both sides sign about versions: the version in use
// and both ranges as each side announced them.
type negotiated struct {
	version        uint16
	caller, callee Versions
}

func (n negotiated) bytes() []byte {
	b := make([]byte, 10)
	for i, x := range []uint16{n.version, n.caller.Min, n.caller.Max, n.callee.Min, n.callee.Max} {
		binary.BigEndian.PutUint16(b[2*i:], x)
	}
	return b
}

// Card is how a joining node introduces itself when pairing: its name, how
// to reach it, and the credential it issued for the inviter, each keyed by
// transport name (docs/PEERS.md).
type Card struct {
	Name   string            `json:"name"`
	Addrs  map[string]string `json:"addrs,omitempty"`
	Access map[string]string `json:"access,omitempty"`
}

// Request is the caller's handshake message.
type Request struct {
	V       int    `json:"v"` // the negotiated version; set by ClientHandshake
	Op      string `json:"op"`
	Service string `json:"service,omitempty"`
	Secret  string `json:"secret,omitempty"` // pair: one-time pairing secret
	Card    *Card  `json:"card,omitempty"`   // pair: the joiner's card
	// Direct (list only): the caller's current direct-mode addresses
	// ("direct", "wg"), so the callee can reach it directly too.
	Direct map[string]string `json:"direct,omitempty"`
	PubKey string            `json:"pubkey"` // base64 ed25519 key of the caller
	Sig    string            `json:"sig"`    // base64 signature, see signedMessage

	// Key is the decoded PubKey, filled in by ReadRequest.
	Key ed25519.PublicKey `json:"-"`
}

// ServiceInfo describes one service a peer may open.
type ServiceInfo struct {
	Name     string `json:"name"`
	Protocol string `json:"protocol"`
}

// PeerInfo is the JSON body sent after OK for OpPair and OpList: who the
// callee is, and what it shares with the caller.
type PeerInfo struct {
	NodeID   string        `json:"node_id"`
	Name     string        `json:"name"`
	PeerID   string        `json:"peer_id"` // the caller's id as the callee knows it
	Services []ServiceInfo `json:"services"`
	// WG is the callee's current WireGuard address ("" when off).
	WG string `json:"wg"`
	// Direct is the callee's current direct-mode address ("" when off).
	// Direct addresses change with the network, so peers refresh them here
	// rather than only at pairing. The caller never trusts it for identity:
	// the direct transport pins the node key named in the address itself.
	Direct string `json:"direct"`
}

// signedMessage builds the exact bytes that are signed. Each variable-length
// field is length-prefixed so that field boundaries cannot be shifted.
func signedMessage(calleePub ed25519.PublicKey, nonce []byte, nv negotiated, r *Request) ([]byte, error) {
	var card []byte
	if r.Card != nil {
		// encoding/json is deterministic (struct order, sorted map keys), and
		// both sides marshal the same decoded value, so this is canonical.
		var err error
		if card, err = json.Marshal(r.Card); err != nil {
			return nil, err
		}
	}
	var b bytes.Buffer
	lp := func(s []byte) {
		binary.Write(&b, binary.BigEndian, uint16(len(s)))
		b.Write(s)
	}
	lp([]byte(sigDomain))
	lp(nv.bytes())
	lp(calleePub)
	lp(nonce)
	lp([]byte(r.Op))
	lp([]byte(r.Service))
	lp([]byte(r.Secret))
	lp(card)
	var direct []byte
	if len(r.Direct) > 0 { // empty and absent sign the same (JSON omits an empty map)
		var err error
		if direct, err = json.Marshal(r.Direct); err != nil { // sorted keys: canonical
			return nil, err
		}
	}
	lp(direct)
	return b.Bytes(), nil
}

// ReadRequest is Versions.ReadRequest with this build's range.
func ReadRequest(conn net.Conn, self *identity.Identity) (*Request, error) {
	return Versions{}.ReadRequest(conn, self)
}

// ReadRequest runs the callee side of the handshake speaking versions v: it
// sends Hello, reads the request, and verifies its signature. On error the
// caller must close conn without writing anything more.
//
// A caller whose range doesn't overlap v gets only the fixed hello prefix
// (magic and v, the first bytes every Sol caller gets) and an
// *IncompatibleError is returned. Anything that isn't a Sol ClientHello,
// including one with an invalid range, gets nothing.
func (v Versions) ReadRequest(conn net.Conn, self *identity.Identity) (*Request, error) {
	v = v.orDefault()
	conn.SetDeadline(time.Now().Add(HandshakeTimeout))
	defer conn.SetDeadline(time.Time{})

	clientHello := make([]byte, prefixSize)
	if n, err := io.ReadFull(conn, clientHello); err != nil {
		if n == 0 && errors.Is(err, io.EOF) {
			return nil, ErrNoData
		}
		if n >= len(magic) && string(clientHello[:len(magic)]) != magic {
			return nil, fmt.Errorf("%w: not a sol node", ErrMalformed)
		}
		return nil, readErr(err)
	}
	callerV, err := parsePrefix(clientHello)
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

	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	hello := make([]byte, 0, helloSize)
	hello = append(hello, prefix(v)...)
	hello = append(hello, self.Public()...)
	hello = append(hello, nonce...)
	if _, err := conn.Write(hello); err != nil {
		return nil, err
	}

	frame, err := readFrame(conn)
	if err != nil {
		return nil, err
	}
	var req Request
	if err := json.Unmarshal(frame, &req); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if req.V != int(version) {
		return nil, fmt.Errorf("%w: request version %d, negotiated %d", ErrMalformed, req.V, version)
	}
	switch req.Op {
	case OpConnect, OpList, OpUnpair:
	case OpPair:
		if req.Card == nil {
			return nil, fmt.Errorf("%w: pair without a card", ErrMalformed)
		}
	default:
		return nil, fmt.Errorf("%w: unknown op %q", ErrMalformed, req.Op)
	}
	req.Key, err = identity.DecodePublicKey(req.PubKey)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	sig, err := base64.StdEncoding.DecodeString(req.Sig)
	if err != nil {
		return nil, fmt.Errorf("%w: bad sig encoding", ErrMalformed)
	}
	msg, err := signedMessage(self.Public(), nonce, negotiated{version, callerV, v}, &req)
	if err != nil || !identity.Verify(req.Key, msg, sig) {
		return nil, ErrBadSignature
	}
	return &req, nil
}

// Accept tells the caller the handshake succeeded. If body is non-nil it is
// sent as a JSON frame right after the OK byte.
func Accept(conn net.Conn, body any) error {
	conn.SetWriteDeadline(time.Now().Add(HandshakeTimeout))
	defer conn.SetWriteDeadline(time.Time{})
	if _, err := conn.Write([]byte{replyOK}); err != nil {
		return err
	}
	if body == nil {
		return nil
	}
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	return writeFrame(conn, data)
}

// ClientHandshake is Versions.ClientHandshake with this build's range.
func ClientHandshake(conn net.Conn, self *identity.Identity, wantNodeID string, req Request) error {
	return Versions{}.ClientHandshake(conn, self, wantNodeID, req)
}

// ClientHandshake runs the caller side speaking versions v. It checks that
// the callee's key matches wantNodeID (pass "" to skip, only safe when the
// transport itself authenticates the callee), signs req with self, and
// waits for OK. When the callee's range doesn't overlap v it returns an
// *IncompatibleError without sending the request.
//
// For OpPair and OpList, follow up with ReadPeerInfo.
func (v Versions) ClientHandshake(conn net.Conn, self *identity.Identity, wantNodeID string, req Request) error {
	v = v.orDefault()
	conn.SetDeadline(time.Now().Add(HandshakeTimeout))
	defer conn.SetDeadline(time.Time{})

	if _, err := conn.Write(prefix(v)); err != nil {
		return err
	}
	pre := make([]byte, prefixSize)
	if _, err := io.ReadFull(conn, pre); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return ErrRejected
		}
		return err
	}
	calleeV, err := parsePrefix(pre)
	if err != nil {
		return err
	}
	version, ok := Negotiate(v, calleeV)
	if !ok {
		return &IncompatibleError{Local: v, Remote: calleeV}
	}
	rest := make([]byte, helloSize-prefixSize)
	if _, err := io.ReadFull(conn, rest); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return ErrRejected
		}
		return err
	}
	calleePub := ed25519.PublicKey(rest[:ed25519.PublicKeySize])
	nonce := rest[ed25519.PublicKeySize:]
	if wantNodeID != "" && identity.NodeID(calleePub) != wantNodeID {
		return ErrWrongNode
	}

	req.V = int(version)
	req.PubKey = identity.EncodePublicKey(self.Public())
	msg, err := signedMessage(calleePub, nonce, negotiated{version, v, calleeV}, &req)
	if err != nil {
		return err
	}
	req.Sig = base64.StdEncoding.EncodeToString(self.Sign(msg))
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	if err := writeFrame(conn, data); err != nil {
		return err
	}

	var reply [1]byte
	if _, err := io.ReadFull(conn, reply[:]); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || isConnReset(err) {
			return ErrRejected
		}
		return err
	}
	if reply[0] != replyOK {
		return ErrRejected
	}
	return nil
}

// ReadPeerInfo reads the JSON frame that follows OK for OpPair/OpList.
func ReadPeerInfo(conn net.Conn) (*PeerInfo, error) {
	conn.SetReadDeadline(time.Now().Add(HandshakeTimeout))
	defer conn.SetReadDeadline(time.Time{})
	frame, err := readFrame(conn)
	if err != nil {
		return nil, err
	}
	var pi PeerInfo
	if err := json.Unmarshal(frame, &pi); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	return &pi, nil
}

func writeFrame(w io.Writer, data []byte) error {
	if len(data) > maxFrameSize {
		return fmt.Errorf("frame too large (%d bytes)", len(data))
	}
	buf := make([]byte, 2+len(data))
	binary.BigEndian.PutUint16(buf, uint16(len(data)))
	copy(buf[2:], data)
	_, err := w.Write(buf)
	return err
}

func readFrame(r io.Reader) ([]byte, error) {
	var hdr [2]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return nil, readErr(err)
	}
	n := int(binary.BigEndian.Uint16(hdr[:]))
	if n == 0 || n > maxFrameSize {
		return nil, fmt.Errorf("%w: frame size %d", ErrMalformed, n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, readErr(err)
	}
	return buf, nil
}

func isConnReset(err error) bool {
	// A reset or closed socket counts as a rejection; a timeout does not.
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return false
	}
	var oe *net.OpError
	return errors.As(err, &oe) || errors.Is(err, net.ErrClosed)
}
