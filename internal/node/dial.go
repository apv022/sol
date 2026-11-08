package node

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/apv022/sol/internal/config"
	"github.com/apv022/sol/internal/events"
	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/pairing"
	"github.com/apv022/sol/internal/peers"
	"github.com/apv022/sol/internal/protocol"
	"github.com/apv022/sol/internal/transport"
)

// perAddrDialTimeout bounds one transport-level dial (Tor can be slow).
const perAddrDialTimeout = 2 * time.Minute

// transportPreference orders a peer's addresses: anonymous transports first,
// clearnet (LAN) last. Unknown future transports go in between.
func transportPreference(name string) int {
	switch name {
	case "tor":
		return 0
	case "i2p":
		return 1
	case "clearnet":
		return 9
	}
	return 5
}

// addrsInOrder lists p's addresses, the one that last worked first, then
// the ones whose transport is ready right now (e.g. LAN while tor is still
// bootstrapping), then by transport preference.
func (n *Node) addrsInOrder(p config.Peer) []string {
	type cand struct {
		addr  string
		pref  int
		ready bool
	}
	var cs []cand
	for name, a := range p.Addrs {
		if a == "" {
			continue
		}
		t, ok := n.reg.Get(name)
		ready := ok && t.Status().State == transport.StateReady
		cs = append(cs, cand{a, transportPreference(name), ready})
	}
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[i].ready != cs[j].ready {
			return cs[i].ready
		}
		return cs[i].pref < cs[j].pref
	})
	n.mu.Lock()
	last := n.lastGood[p.ID]
	n.mu.Unlock()
	out := make([]string, 0, len(cs)+1)
	if last != "" {
		out = append(out, last)
	}
	for _, c := range cs {
		if c.addr != last {
			out = append(out, c.addr)
		}
	}
	return out
}

// dialOnce opens a stream to p and completes the handshake for req, trying
// each address once.
func (n *Node) dialOnce(ctx context.Context, p config.Peer, req protocol.Request) (net.Conn, error) {
	addrs := n.addrsInOrder(p)
	if len(addrs) == 0 {
		return nil, fmt.Errorf("%s has no address to reach it at (it may not publish anything); it can still connect to you", nameOr(p))
	}
	var errs []error
	for _, addr := range addrs {
		t, err := n.reg.For(addr)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		dctx, cancel := context.WithTimeout(ctx, perAddrDialTimeout)
		conn, err := t.Dial(dctx, addr)
		cancel()
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", t.Name(), err))
			continue
		}
		if err := protocol.ClientHandshake(conn, n.id, p.NodeID, req); err != nil {
			conn.Close()
			if errors.Is(err, protocol.ErrRejected) || errors.Is(err, protocol.ErrWrongNode) {
				return nil, err // the peer answered; another path will not help
			}
			errs = append(errs, fmt.Errorf("%s: %w", t.Name(), err))
			continue
		}
		if p.ID != "" {
			n.mu.Lock()
			n.lastGood[p.ID] = addr
			n.mu.Unlock()
		}
		return conn, nil
	}
	return nil, errors.Join(errs...)
}

// dial is dialOnce with retries for errors that usually clear up on their
// own, like an onion that was published a moment ago. retry=false makes one
// attempt (used for best-effort messages such as unpair).
func (n *Node) dial(ctx context.Context, p config.Peer, req protocol.Request, retry bool) (net.Conn, error) {
	attempts := 1
	if retry {
		attempts = n.opts.DialAttempts
	}
	var err error
	for attempt := 1; attempt <= attempts; attempt++ {
		var conn net.Conn
		if conn, err = n.dialOnce(ctx, p, req); err == nil {
			return conn, nil
		}
		if errors.Is(err, protocol.ErrRejected) || errors.Is(err, protocol.ErrWrongNode) || ctx.Err() != nil {
			return nil, err
		}
		if attempt < attempts {
			n.log.Add(events.KindTransport, fmt.Sprintf("can't reach %s yet (%s); retrying in %s", nameOr(p), explain(err), n.opts.DialRetryDelay))
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(n.opts.DialRetryDelay):
			}
		}
	}
	return nil, fmt.Errorf("could not reach %s: %s. Check that it is running and online, and that both machines' clocks are correct", nameOr(p), explain(err))
}

func nameOr(p config.Peer) string {
	if p.Name != "" {
		return p.Name
	}
	return "the other node"
}

// explain turns a dial error into a short plain-English reason.
func explain(err error) string {
	msg := err.Error()
	var ne net.Error
	switch {
	case errors.As(err, &ne) && ne.Timeout(), strings.Contains(msg, "i/o timeout"):
		return "Tor timed out finding it"
	case strings.Contains(msg, "descriptor not found"), strings.Contains(msg, "host unreachable"):
		return "its .onion isn't visible yet; this is normal for a minute or two after it starts"
	case strings.Contains(msg, "client authorization"):
		return "Tor rejected this node's access key"
	case strings.Contains(msg, "connection refused"):
		return "connection refused"
	}
	return msg
}

// trackedConn unregisters itself from the node's connection table on Close.
type trackedConn struct {
	net.Conn
	n    *Node
	once sync.Once
}

func (c *trackedConn) Close() error {
	c.once.Do(func() { c.n.untrack(c) })
	return c.Conn.Close()
}

// CloseWrite half-closes when the underlying connection can, so tunnels
// keep netutil.Pipe's half-close behaviour.
func (c *trackedConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Close()
}

// DialPeer opens an authenticated stream to one of peer id's services. The
// stream is tracked, so removing the peer closes it.
func (n *Node) DialPeer(ctx context.Context, id, service string) (net.Conn, error) {
	p, ok := n.peers.Get(id)
	if !ok {
		return nil, fmt.Errorf("peer %q: %w", id, config.ErrNotFound)
	}
	conn, err := n.dial(ctx, p, protocol.Request{Op: protocol.OpConnect, Service: service}, true)
	if errors.Is(err, protocol.ErrRejected) {
		return nil, fmt.Errorf("%s didn't accept the connection to %q: it isn't shared with you, or %s removed you", p.Name, service, p.Name)
	}
	if err != nil {
		return nil, err
	}
	tc := &trackedConn{Conn: conn, n: n}
	n.track(tc, connInfo{peer: p.ID, service: service, since: time.Now().UTC(), outbound: true})
	return tc, nil
}

// Pair redeems another node's pairing link (the joiner's side, see
// docs/PEERS.md) and returns the new peer.
func (n *Node) Pair(ctx context.Context, link string) (config.Peer, error) {
	l, err := pairing.ParseLink(link)
	if err != nil {
		return config.Peer{}, err
	}
	if l.NodeID == n.id.NodeID() {
		return config.Peer{}, errors.New("that link was made by this node; open it on the other machine")
	}
	pub, err := identity.ParseNodeID(l.NodeID)
	if err != nil {
		return config.Peer{}, err
	}
	inviter := config.Peer{NodeID: l.NodeID, Name: l.Name, Addrs: l.Addrs, AccessOut: l.Access}
	if inviter.Name == "" || config.ValidateName(inviter.Name) != nil {
		inviter.Name = "peer"
	}

	// So we can reach the inviter, and so it can reach us afterwards.
	n.installAccess(inviter)
	issued, err := n.issueAccess()
	if err != nil {
		return config.Peer{}, err
	}
	card := &protocol.Card{Name: n.cfg.NodeName(), Addrs: n.ownAddrs(), Access: map[string]string{}}
	accessIn := map[string]string{}
	for t, a := range issued {
		card.Access[t] = a.Secret
		accessIn[t] = a.Authorize
	}
	n.log.Add(events.KindPair, fmt.Sprintf("pairing with %s…", inviter.Name))

	conn, err := n.dial(ctx, inviter, protocol.Request{Op: protocol.OpPair, Secret: l.Secret, Card: card}, true)
	if err != nil {
		if _, known := n.peers.Get(identity.DeviceID(pub)); !known {
			n.removeAccess(inviter)
		}
		if errors.Is(err, protocol.ErrRejected) {
			return config.Peer{}, errors.New("the other node refused the link: it has expired (10 minutes), was already used, or belongs to another node. Make a new one")
		}
		return config.Peer{}, err
	}
	defer conn.Close()
	info, err := protocol.ReadPeerInfo(conn)
	if err != nil {
		return config.Peer{}, fmt.Errorf("reading the other node's answer: %w", err)
	}
	if config.ValidateName(info.Name) == nil {
		inviter.Name = info.Name
	}
	inviter.AccessIn = accessIn
	inviter.Services = remoteServices(info.Services)
	p, err := n.peers.Add(inviter, pub)
	if err != nil {
		return config.Peer{}, err
	}
	n.installAccess(p) // Add replaced any old record, whose removal forgot the key
	n.log.Add(events.KindPair, "paired with "+peers.Describe(p))
	return p, nil
}

func remoteServices(in []protocol.ServiceInfo) []config.RemoteService {
	out := make([]config.RemoteService, 0, len(in))
	for _, s := range in {
		out = append(out, config.RemoteService{Name: s.Name, Protocol: s.Protocol})
	}
	return out
}

// RefreshPeer asks a peer what it currently shares with us (and its current
// name) and saves the answer.
func (n *Node) RefreshPeer(ctx context.Context, id string) (config.Peer, error) {
	p, ok := n.peers.Get(id)
	if !ok {
		return config.Peer{}, fmt.Errorf("peer %q: %w", id, config.ErrNotFound)
	}
	conn, err := n.dial(ctx, p, protocol.Request{Op: protocol.OpList}, false)
	if errors.Is(err, protocol.ErrRejected) {
		return p, fmt.Errorf("%s no longer accepts this node; it may have unpaired", p.Name)
	}
	if err != nil {
		return p, err
	}
	defer conn.Close()
	info, err := protocol.ReadPeerInfo(conn)
	if err != nil {
		return p, err
	}
	return n.peers.Update(id, func(rec *config.Peer) error {
		rec.Services = remoteServices(info.Services)
		if config.ValidateName(info.Name) == nil {
			rec.Name = info.Name
		}
		return nil
	})
}

// Unpair removes a peer here and tries to tell it, so it removes us too.
// notified reports whether the peer got the message.
func (n *Node) Unpair(ctx context.Context, id string) (p config.Peer, notified bool, err error) {
	p, ok := n.peers.Get(id)
	if !ok {
		return config.Peer{}, false, fmt.Errorf("peer %q: %w", id, config.ErrNotFound)
	}
	// Tell the peer first, while our access key for it is still installed.
	nctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	if conn, derr := n.dial(nctx, p, protocol.Request{Op: protocol.OpUnpair}, false); derr == nil {
		conn.Close()
		notified = true
	}
	cancel()
	if err := n.peers.Remove(id); err != nil {
		return p, notified, err
	}
	return p, notified, nil
}
