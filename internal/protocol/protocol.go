// Package protocol implements the sol/1 connection handshake.
//
// Every connection a client opens to a node (over Tor, I2P, or clearnet)
// starts with one challenge-response round trip. After it succeeds, the
// connection is a raw byte pipe to the requested service. The full
// specification, including the byte layout, is in docs/PROTOCOL.md.
//
//	client -> node   : ClientHello = "SOL1"
//	node   -> client : Hello       = "SOL1" | node ed25519 pubkey (32) | nonce (32)
//	client -> node   : Request     = uint16 length | JSON {v, op, service, pubkey, sig, ...}
//	node   -> client : 0x01 (OK), optionally followed by a JSON frame
//	                   or the node closes the connection without sending anything.
//
// The client speaks first, so anything that is not a Sol client (port
// scanners, HTTP probes) gets no bytes back at all.
//
// The signature covers a domain-separation tag, the node's public key, the
// fresh nonce, and every request field, so a signature can never be replayed
// on another connection, another node, or for another service.
//
// This package only checks that the request is well formed and correctly
// signed. Deciding whether the key is allowed is the caller's job (see
// internal/node).
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

// Operations a client can request.
const (
	OpConnect = "connect" // pipe to Request.Service
	OpPair    = "pair"    // register Request.PubKey using Request.Secret; reply carries ServiceList
	OpList    = "list"    // reply carries ServiceList, then close
)

const (
	magic        = "SOL1"
	nonceSize    = 32
	helloSize    = len(magic) + ed25519.PublicKeySize + nonceSize
	maxFrameSize = 8 << 10
	replyOK      = 0x01
	sigDomain    = "sol/1 handshake"
)

// HandshakeTimeout bounds the whole handshake. Tor round trips can take
// several seconds, so this is generous.
var HandshakeTimeout = 45 * time.Second

// Errors.
var (
	// ErrRejected means the node closed the connection instead of accepting.
	// By design the client cannot tell why (unknown key, revoked, bad service).
	ErrRejected     = errors.New("node rejected the connection")
	ErrWrongNode    = errors.New("node identity does not match the expected node id")
	ErrBadSignature = errors.New("bad handshake signature")
	ErrMalformed    = errors.New("malformed handshake")
	// ErrNoData means the peer connected and closed without sending
	// anything (reachability probes, including the node's own self-check).
	ErrNoData = errors.New("connection closed before any data")
)

// Request is the client's handshake message.
type Request struct {
	V       int    `json:"v"`
	Op      string `json:"op"`
	Service string `json:"service,omitempty"`
	Name    string `json:"name,omitempty"`   // pair: device label
	Secret  string `json:"secret,omitempty"` // pair: one-time pairing secret
	PubKey  string `json:"pubkey"`           // base64 ed25519 device key
	Sig     string `json:"sig"`              // base64 signature, see signedMessage

	// Key is the decoded PubKey, filled in by ReadRequest.
	Key ed25519.PublicKey `json:"-"`
}

// ServiceInfo describes one service a device may connect to.
type ServiceInfo struct {
	Name     string `json:"name"`
	Protocol string `json:"protocol"`
}

// ServiceList is the JSON body sent after OK for OpPair and OpList.
type ServiceList struct {
	NodeID   string        `json:"node_id"`
	DeviceID string        `json:"device_id"`
	Services []ServiceInfo `json:"services"`
}

// signedMessage builds the exact bytes that are signed. Each variable-length
// field is length-prefixed so that field boundaries cannot be shifted.
func signedMessage(nodePub ed25519.PublicKey, nonce []byte, r *Request) []byte {
	var b bytes.Buffer
	lp := func(s []byte) {
		binary.Write(&b, binary.BigEndian, uint16(len(s)))
		b.Write(s)
	}
	lp([]byte(sigDomain))
	lp(nodePub)
	lp(nonce)
	lp([]byte(r.Op))
	lp([]byte(r.Service))
	lp([]byte(r.Name))
	lp([]byte(r.Secret))
	return b.Bytes()
}

// ReadRequest runs the node side of the handshake: it sends Hello, reads the
// request, and verifies its signature. On error the caller must close conn
// without writing anything.
func ReadRequest(conn net.Conn, node *identity.Identity) (*Request, error) {
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
		return nil, fmt.Errorf("%w: not a sol client", ErrMalformed)
	}

	nonce := make([]byte, nonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	hello := make([]byte, 0, helloSize)
	hello = append(hello, magic...)
	hello = append(hello, node.Public()...)
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
	case OpConnect, OpPair, OpList:
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
	if !identity.Verify(req.Key, signedMessage(node.Public(), nonce, &req), sig) {
		return nil, ErrBadSignature
	}
	return &req, nil
}

// Accept tells the client the handshake succeeded. If body is non-nil it is
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

// ClientHandshake runs the client side. It checks that the node's key
// matches wantNodeID (pass "" to skip, only safe when the transport itself
// authenticates the node), signs req with dev, and waits for OK.
//
// For OpPair and OpList, follow up with ReadServiceList.
func ClientHandshake(conn net.Conn, dev *identity.Identity, wantNodeID string, req Request) error {
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
	nodePub := ed25519.PublicKey(hello[len(magic) : len(magic)+ed25519.PublicKeySize])
	nonce := hello[len(magic)+ed25519.PublicKeySize:]
	if wantNodeID != "" && identity.NodeID(nodePub) != wantNodeID {
		return ErrWrongNode
	}

	req.V = Version
	req.PubKey = identity.EncodePublicKey(dev.Public())
	req.Sig = base64.StdEncoding.EncodeToString(dev.Sign(signedMessage(nodePub, nonce, &req)))
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

// ReadServiceList reads the JSON frame that follows OK for OpPair/OpList.
func ReadServiceList(conn net.Conn) (*ServiceList, error) {
	conn.SetReadDeadline(time.Now().Add(HandshakeTimeout))
	defer conn.SetReadDeadline(time.Time{})
	frame, err := readFrame(conn)
	if err != nil {
		return nil, err
	}
	var sl ServiceList
	if err := json.Unmarshal(frame, &sl); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	return &sl, nil
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
