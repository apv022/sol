package node

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/apv022/sol/internal/api"
	"github.com/apv022/sol/internal/config"
	"github.com/apv022/sol/internal/control"
	"github.com/apv022/sol/internal/netutil"
)

// maxSocketPath is the longest Unix socket path every supported OS accepts
// (sun_path is 104 bytes on macOS and the BSDs, 108 on Linux, including the
// terminating NUL).
const maxSocketPath = 103

// ErrAlreadyRunning means another node holds this data directory.
var ErrAlreadyRunning = errors.New("another Sol node is already running with this data directory")

// listenControl opens the control socket. A stale socket file left by a
// crashed node is replaced; a live one means another node already runs on
// this data directory.
func listenControl(path string) (net.Listener, error) {
	if len(path) > maxSocketPath {
		return nil, fmt.Errorf("the control socket path %s is %d characters, but Unix sockets allow at most %d; use a shorter data directory (--data or SOL_DATA)", path, len(path), maxSocketPath)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
		c.Close()
		return nil, ErrAlreadyRunning
	}
	os.Remove(path)
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, fmt.Errorf("control socket: %w", err)
	}
	// Only our user may connect (the data directory is 0700 as well).
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

func (n *Node) serveControl(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		if !n.goBG(func() { n.handleControl(c) }) {
			c.Close()
		}
	}
}

// handleControl runs one control request (see package control).
func (n *Node) handleControl(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	c.SetReadDeadline(time.Now().Add(30 * time.Second))
	line, err := br.ReadBytes('\n')
	if err != nil {
		return
	}
	c.SetReadDeadline(time.Time{})
	var req control.Request
	if err := json.Unmarshal(line, &req); err != nil {
		control.WriteResponse(c, nil, errors.New("bad request"))
		return
	}
	ctx, cancel := context.WithCancel(n.ctx)
	defer cancel()

	reply := func(data any, err error) { control.WriteResponse(c, data, err) }
	switch req.Op {
	case control.OpStatus:
		reply(n.controlStatus(), nil)
	case control.OpName:
		var err error
		if req.Name != "" {
			err = n.SetName(req.Name)
		}
		reply(n.controlStatus(), err)
	case control.OpInvite:
		inv, err := n.Invite()
		reply(control.Invite{Link: inv.Link, Expires: inv.Expires.Format(time.RFC3339)}, err)
	case control.OpPair:
		p, err := n.Pair(ctx, req.Link)
		reply(n.controlView(p, nil), err)
	case control.OpPeers:
		list := n.peers.List()
		if req.Peer != "" {
			p, err := n.peers.Find(req.Peer)
			if err != nil {
				reply(nil, err)
				return
			}
			list = []config.Peer{p}
		}
		var out []control.PeerView
		for _, p := range list {
			var rerr error
			if req.Refresh {
				rctx, rcancel := context.WithTimeout(ctx, 90*time.Second)
				p, rerr = n.RefreshPeer(rctx, p.ID)
				rcancel()
			}
			out = append(out, n.controlView(p, rerr))
		}
		if out == nil {
			out = []control.PeerView{}
		}
		reply(out, nil)
	case control.OpShare:
		p, err := n.peers.Find(req.Peer)
		if err == nil {
			p, err = n.SetShare(p.ID, req.Service, req.Shared)
		}
		reply(n.controlView(p, nil), err)
	case control.OpUnpair:
		p, err := n.peers.Find(req.Peer)
		if err != nil {
			reply(nil, err)
			return
		}
		_, notified, err := n.Unpair(ctx, p.ID)
		reply(control.Unpaired{Name: p.Name, Notified: notified}, err)
	case control.OpDial:
		p, err := n.peers.Find(req.Peer)
		if err != nil {
			reply(nil, err)
			return
		}
		remote, err := n.DialPeer(ctx, p.ID, req.Service)
		if err != nil {
			reply(nil, err)
			return
		}
		if err := control.WriteResponse(c, nil, nil); err != nil {
			remote.Close()
			return
		}
		netutil.Pipe(&bufferedConn{Conn: c, r: br}, remote)
	case control.OpSend:
		p, err := n.peers.Find(req.Peer)
		if err != nil {
			reply(nil, err)
			return
		}
		if err := control.WriteResponse(c, nil, nil); err != nil {
			return
		}
		hash, size, err := n.SendFile(ctx, p.ID, req.Name, br)
		reply(control.Sent{Hash: hash, Size: size}, err)
	case control.OpServices:
		out := []control.LocalService{}
		for _, s := range n.Services() {
			out = append(out, n.controlService(s))
		}
		reply(out, nil)
	case control.OpServiceAdd:
		vis := req.Visibility
		if vis == "" {
			vis = config.VisibilityPrivate
		}
		err := n.AddService(config.Service{Name: req.Service, LocalPort: req.Port, Visibility: vis, Protocol: req.Protocol,
			Dir: req.Dir, Record: req.Record})
		reply(n.controlServiceNamed(req.Service), err)
	case control.OpServiceRemove:
		reply(nil, n.RemoveService(req.Service))
	case control.OpServiceVis:
		err := n.SetServiceVisibility(req.Service, req.Visibility)
		reply(n.controlServiceNamed(req.Service), err)
	case control.OpConfig, control.OpConfigSet:
		var err error
		if req.Op == control.OpConfigSet {
			_, err = n.SetOption(req.Key, req.Value)
		}
		out := control.Config{RestartNeeded: n.restartNeeded.Load()}
		for _, o := range n.Options() {
			out.Options = append(out.Options, control.Option{Key: o.Key, Value: o.Value, Help: o.Help, Restart: o.Restart})
		}
		reply(out, err)
	case control.OpStop:
		reply(nil, nil)
		n.Stop()
	case control.OpRestart:
		reply(nil, nil)
		n.Restart()
	case control.OpHelpTor:
		var err error
		switch req.Set {
		case "on", "off":
			err = n.SetHelpTor(req.Set == "on")
		case "":
		default:
			err = fmt.Errorf("help-tor: want on or off, got %q", req.Set)
		}
		st := n.helper.Status()
		reply(control.HelpTor{Enabled: st.Enabled, State: string(st.State), Detail: st.Detail}, err)
	case control.OpTunnels:
		out := []control.Tunnel{}
		for _, t := range n.Tunnels() {
			out = append(out, controlTunnel(t))
		}
		reply(out, nil)
	case control.OpTunnelOpen:
		p, err := n.peers.Find(req.Peer)
		if err != nil {
			reply(nil, err)
			return
		}
		t, err := n.OpenTunnel(p.ID, req.Service, req.Port)
		reply(controlTunnel(t), err)
	case control.OpTunnelClose:
		p, err := n.peers.Find(req.Peer)
		if err != nil {
			reply(nil, err)
			return
		}
		closed := 0
		for _, t := range n.Tunnels() {
			if t.Peer == p.ID && (req.Service == "" || t.Service == req.Service) {
				if err := n.CloseTunnel(t.Port); err != nil {
					reply(nil, err)
					return
				}
				closed++
			}
		}
		if closed == 0 {
			what := p.Name
			if req.Service != "" {
				what += "'s " + req.Service
			}
			reply(nil, fmt.Errorf("no open tunnel to %s", what))
			return
		}
		reply(nil, nil)
	default:
		reply(nil, fmt.Errorf("unknown op %q", req.Op))
	}
}

func (n *Node) controlServiceNamed(name string) control.LocalService {
	for _, s := range n.Services() {
		if s.Name == name {
			return n.controlService(s)
		}
	}
	return control.LocalService{Name: name}
}

func (n *Node) controlService(s api.ServiceView) control.LocalService {
	v := control.LocalService{Name: s.Name, Port: s.LocalPort, Dir: s.Dir, Record: s.Record, Visibility: s.Visibility,
		Protocol: s.Protocol, Running: s.Running, Addrs: s.Addresses, SharedWith: []string{}}
	for _, p := range n.peers.List() {
		if p.Shared(s.Name) {
			v.SharedWith = append(v.SharedWith, p.Name)
		}
	}
	return v
}

func (n *Node) controlStatus() control.Status {
	return control.Status{PID: os.Getpid(), NodeID: n.id.NodeID(), Name: n.cfg.NodeName(), Panel: n.panelAddr, Addrs: n.ownAddrs()}
}

func (n *Node) controlView(p config.Peer, refreshErr error) control.PeerView {
	v := control.PeerView{ID: p.ID, Name: p.Name, NodeID: p.NodeID, Shares: p.Shares, Services: []control.Service{}}
	for _, s := range p.Services {
		v.Services = append(v.Services, control.Service{Name: s.Name, Protocol: s.Protocol})
	}
	n.mu.Lock()
	for _, ci := range n.conns {
		if ci.peer == p.ID {
			v.Online = true
			break
		}
	}
	n.mu.Unlock()
	if refreshErr != nil {
		v.Error = refreshErr.Error()
	} else if v.ID != "" && n.recentlyReached(p.ID) {
		v.Online = true
	}
	return v
}

// recentlyReached reports whether a dial to peer id has worked in this run.
func (n *Node) recentlyReached(id string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.lastGood[id] != ""
}

// bufferedConn reads through r first, so bytes the control reader already
// buffered (right after the request line) aren't lost when piping.
type bufferedConn struct {
	net.Conn
	r *bufio.Reader
}

func (b *bufferedConn) Read(p []byte) (int, error) { return b.r.Read(p) }

func (b *bufferedConn) CloseWrite() error {
	if cw, ok := b.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return b.Conn.Close()
}

func controlTunnel(t api.TunnelView) control.Tunnel {
	name := t.PeerName
	if name == "" {
		name = t.Peer
	}
	return control.Tunnel{
		Port: t.Port, Peer: name, Service: t.Service, Protocol: t.Protocol, Addr: t.Addr,
		URL: t.URL, Listening: t.Listening, LastError: t.LastError, Via: t.Via,
	}
}
