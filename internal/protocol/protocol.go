// Package protocol implements Sol's connection handshake.
//
// Every connection one node opens to another (over Tor, I2P, or clearnet)
// starts with one challenge-response round trip. After it succeeds, the
// connection is a raw byte pipe to the requested service. The full
// specification, including the byte layout, is in docs/PROTOCOL.md.
//
//	caller -> callee : ClientHello = "SOL1"
//	callee -> caller : Hello       = "SOL1" | callee ed25519 pubkey (32) | nonce (32)
//	caller -> callee : Request     = uint16 length | JSON {v, op, service, pubkey, sig, ...}
//	callee -> caller : 0x01 (OK), optionally followed by a JSON frame
//	                   or the callee closes the connection without sending anything.
//
// The caller speaks first, so anything that is not a Sol node (port
// scanners, HTTP probes) gets no bytes back at all.
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
// Sol is pre-release: this format changes in place until the first public
// release, after which changes need a new magic (see docs/PROTOCOL.md).
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
	"time"

	"github.com/apv022/sol/internal/identity"
)

// Version is the protocol version carried in Request.V.
const Version = 1

// Operations a caller can request.
const (
	OpConnect = "connect" // pipe to Request.Service
	OpPair    = "pair"    // redeem Request.Secret with Request.Card; reply carries PeerInfo
	OpList    = "list"    // reply carries PeerInfo, then close
	OpUnpair  = "unpair"  // the caller removed us; remove it too. Reply is OK, then close
)

const (
	magic        = "SOL1"
	nonceSize    = 32
	helloSize    = len(magic) + ed25519.PublicKeySize + nonceSize
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
	// ErrNoData means the other side connected and closed without sending
	// anything (reachability probes, including the node's own self-check).
	ErrNoData = errors.New("connection closed before any data")
)

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
	V       int    `json:"v"`
	Op      string `json:"op"`
	Service string `json:"service,omitempty"`
	Secret  string `json:"secret,omitempty"` // pair: one-time pairing secret
	Card    *Card  `json:"card,omitempty"`   // pair: the joiner's card
	PubKey  string `json:"pubkey"`           // base64 ed25519 key of the caller
	Sig     string `json:"sig"`              // base64 signature, see signedMessage

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
	// Direct is the callee's current direct-mode address ("" when off).
	// Direct addresses change with the network, so peers refresh them here
	// rather than only at pairing. The caller never trusts it for identity:
	// the direct transport pins the node key named in the address itself.
	Direct string `json:"direct"`
}

// signedMessage builds the exact bytes that are signed. Each variable-length
// field is length-prefixed so that field boundaries cannot be shifted.
func signedMessage(calleePub ed25519.PublicKey, nonce []byte, r *Request) ([]byte, error) {
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
	lp(calleePub)
	lp(nonce)
	lp([]byte(r.Op))
	lp([]byte(r.Service))
	lp([]byte(r.Secret))
	lp(card)
	return b.Bytes(), nil
}

// ReadRequest runs the callee side of the handshake: it sends Hello, reads
// the request, and verifies its signature. On error the caller must close
// conn without writing anything.
func ReadRequest(conn net.Conn, self *identity.Identity) (*Request, error) {
	conn.SetDeadline(time.Now().Add(HandshakeTimeout))
	defer conn.SetDeadline(time.Time{})

	clientHello := make([]byte, len(magic))
	if n, err := io.ReadFull(conn, clientHello); err != nil {
		if n == 0 && errors.Is(err, io.EOF) {
			return nil, ErrNoData
		}
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	if string(clientHello) != magic {
		return nil, fmt.Errorf("%w: not a sol node", ErrMalformed)
	}

	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	hello := make([]byte, 0, helloSize)
	hello = append(hello, magic...)
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
	if req.V != Version {
		return nil, fmt.Errorf("%w: unsupported version %d", ErrMalformed, req.V)
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
	msg, err := signedMessage(self.Public(), nonce, &req)
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

// ClientHandshake runs the caller side. It checks that the callee's key
// matches wantNodeID (pass "" to skip, only safe when the transport itself
// authenticates the callee), signs req with self, and waits for OK.
//
// For OpPair and OpList, follow up with ReadPeerInfo.
func ClientHandshake(conn net.Conn, self *identity.Identity, wantNodeID string, req Request) error {
	conn.SetDeadline(time.Now().Add(HandshakeTimeout))
	defer conn.SetDeadline(time.Time{})

	if _, err := conn.Write([]byte(magic)); err != nil {
		return err
	}
	hello := make([]byte, helloSize)
	if _, err := io.ReadFull(conn, hello); err != nil {
		if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return ErrRejected
		}
		return err
	}
	if string(hello[:len(magic)]) != magic {
		return fmt.Errorf("%w: not a sol node", ErrMalformed)
	}
	calleePub := ed25519.PublicKey(hello[len(magic) : len(magic)+ed25519.PublicKeySize])
	nonce := hello[len(magic)+ed25519.PublicKeySize:]
	if wantNodeID != "" && identity.NodeID(calleePub) != wantNodeID {
		return ErrWrongNode
	}

	req.V = Version
	req.PubKey = identity.EncodePublicKey(self.Public())
	msg, err := signedMessage(calleePub, nonce, &req)
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
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	n := int(binary.BigEndian.Uint16(hdr[:]))
	if n == 0 || n > maxFrameSize {
		return nil, fmt.Errorf("%w: frame size %d", ErrMalformed, n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
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
