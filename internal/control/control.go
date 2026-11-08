// Package control is how `sol` commands talk to the running node: a Unix
// socket in the data directory (control.sock).
//
// Only the user who owns the data directory can connect: the socket is 0600
// inside a 0700 directory, so no further authentication is needed, and the
// socket is never reachable over the network. (Go supports Unix sockets on
// Windows 10 and later too.)
//
// Wire format: the client sends one JSON Request line. The server answers
// with one JSON Response line. For the streaming ops the connection then
// carries raw bytes:
//
//	dial  after an OK response, the connection is a pipe to the peer's service
//	send  after an OK response, the client writes the file and closes its
//	      write side; the server answers with a second Response holding the hash
package control

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"path/filepath"
)

// SocketName is the socket's file name inside the data directory.
const SocketName = "control.sock"

// Ops.
const (
	OpStatus = "status" // -> Status
	OpName   = "name"   // Name (optional, sets it) -> Status
	OpInvite = "invite" // -> Invite
	OpPair   = "pair"   // Link -> PeerView
	OpPeers  = "peers"  // Peer (optional: just that one), Refresh -> []PeerView
	OpShare  = "share"  // Peer, Service, Shared -> PeerView
	OpUnpair = "unpair" // Peer -> Unpaired
	OpDial   = "dial"   // Peer, Service; then a raw pipe
	OpSend   = "send"   // Peer, Name; then the file bytes; then Sent
)

// Request is one command.
type Request struct {
	Op      string `json:"op"`
	Peer    string `json:"peer,omitempty"` // a peer name or id prefix
	Service string `json:"service,omitempty"`
	Shared  bool   `json:"shared,omitempty"`
	Link    string `json:"link,omitempty"`
	Name    string `json:"name,omitempty"`
	Refresh bool   `json:"refresh,omitempty"` // peers: ask the peer(s) for their current list first
}

// Response is the answer. Data's type depends on the op.
type Response struct {
	OK    bool            `json:"ok"`
	Error string          `json:"error,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
}

// Status is the answer to OpStatus.
type Status struct {
	NodeID string            `json:"node_id"`
	Name   string            `json:"name"`
	Panel  string            `json:"panel"`
	Addrs  map[string]string `json:"addrs"`
}

// Invite is the answer to OpInvite.
type Invite struct {
	Link    string `json:"link"`
	Expires string `json:"expires"`
}

// Service is one service a peer shares with us.
type Service struct {
	Name     string `json:"name"`
	Protocol string `json:"protocol"`
}

// PeerView is a peer as the CLI sees it. It never includes credentials.
type PeerView struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	NodeID   string    `json:"node_id"`
	Shares   []string  `json:"shares"`   // what we let it open
	Services []Service `json:"services"` // what it lets us open
	Online   bool      `json:"online"`
	Error    string    `json:"error,omitempty"` // why a refresh failed
}

// Unpaired is the answer to OpUnpair.
type Unpaired struct {
	Name     string `json:"name"`
	Notified bool   `json:"notified"` // the other side was told and removed us too
}

// Sent is the final answer to OpSend.
type Sent struct {
	Hash string `json:"hash"`
	Size int64  `json:"size"`
}

// ErrNotRunning means nothing answers on the socket.
var ErrNotRunning = errors.New("the Sol node isn't running; start it with `sol` (in another terminal or as a service)")

// Path returns the socket path for a data directory.
func Path(dataDir string) string { return filepath.Join(dataDir, SocketName) }

// Conn is an open control connection.
type Conn struct {
	net.Conn
	r *bufio.Reader
}

// Dial connects to the node's control socket.
func Dial(ctx context.Context, path string) (*Conn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, ErrNotRunning
	}
	return &Conn{Conn: c, r: bufio.NewReader(c)}, nil
}

// Send writes a request.
func (c *Conn) Send(req Request) error { return json.NewEncoder(c.Conn).Encode(req) }

// Recv reads one response into out (which may be nil) and turns a failed
// response into an error.
func (c *Conn) Recv(out any) error {
	line, err := c.r.ReadBytes('\n')
	if err != nil {
		return fmt.Errorf("the node closed the connection: %w", err)
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return fmt.Errorf("bad response from the node: %w", err)
	}
	if !resp.OK {
		return errors.New(resp.Error)
	}
	if out != nil && len(resp.Data) > 0 {
		return json.Unmarshal(resp.Data, out)
	}
	return nil
}

// Read reads from the connection after a streaming op was accepted (bytes
// buffered while reading the response are not lost).
func (c *Conn) Read(p []byte) (int, error) { return c.r.Read(p) }

// CloseWrite half-closes the connection (end of a sent file).
func (c *Conn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return nil
}

// Call runs one non-streaming request and decodes the answer into out.
func Call(ctx context.Context, path string, req Request, out any) error {
	c, err := Dial(ctx, path)
	if err != nil {
		return err
	}
	defer c.Close()
	if dl, ok := ctx.Deadline(); ok {
		c.SetDeadline(dl)
	}
	if err := c.Send(req); err != nil {
		return err
	}
	return c.Recv(out)
}

// WriteResponse is the server side: it writes data (or err) as one line.
func WriteResponse(w io.Writer, data any, err error) error {
	resp := Response{OK: err == nil}
	if err != nil {
		resp.Error = err.Error()
	} else if data != nil {
		b, merr := json.Marshal(data)
		if merr != nil {
			return merr
		}
		resp.Data = b
	}
	return json.NewEncoder(w).Encode(resp)
}
