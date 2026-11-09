package node

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"sort"
	"time"

	"github.com/apv022/sol/internal/api"
	"github.com/apv022/sol/internal/audit"
	"github.com/apv022/sol/internal/autostart"
	"github.com/apv022/sol/internal/blob"
	"github.com/apv022/sol/internal/config"
	"github.com/apv022/sol/internal/events"
	"github.com/apv022/sol/internal/pairing"
	"github.com/apv022/sol/internal/peers"
	"github.com/apv022/sol/internal/transport"
)

// This file implements api.Backend.

var _ api.Backend = (*Node)(nil)

// Status implements api.Backend.
func (n *Node) Status() api.StatusInfo {
	return api.StatusInfo{
		NodeID:     n.id.NodeID(),
		Name:       n.cfg.NodeName(),
		Version:    n.opts.Version,
		StartedAt:  n.started,
		Uptime:     time.Since(n.started).Round(time.Second).String(),
		Transports: n.reg.Statuses(),
		Autostart:  n.autostartState(),
	}
}

// autostartState reports whether this node starts at login (files only;
// the Manager is built once in New).
func (n *Node) autostartState() autostart.State {
	return n.autostart.Status() // a zero Manager (no GOOS) reports unsupported
}

// SetName implements api.Backend. Peers learn the new name the next time
// they refresh.
func (n *Node) SetName(name string) error {
	if err := config.ValidateName(name); err != nil {
		return err
	}
	if err := n.cfg.SetName(name); err != nil {
		return err
	}
	n.log.Add(events.KindService, "this node is now called "+name)
	return nil
}

// Services implements api.Backend.
func (n *Node) Services() []api.ServiceView {
	var out []api.ServiceView
	n.mu.Lock()
	for _, s := range n.cfg.ServiceList() {
		v := api.ServiceView{Service: s}
		if p := n.public[s.Name]; p != nil {
			v.Addresses = p.addrs
		}
		for _, ci := range n.conns {
			if ci.service == s.Name && !ci.outbound {
				v.ActiveStreams++
			}
		}
		out = append(out, v)
	}
	n.mu.Unlock()
	// Probe outside the lock: is something actually listening on the port?
	for i := range out {
		c, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", out[i].LocalPort), 300*time.Millisecond)
		if err == nil {
			c.Close()
			out[i].Running = true
		}
	}
	if out == nil {
		out = []api.ServiceView{}
	}
	return out
}

// AddService implements api.Backend. A new service is shared with nobody.
func (n *Node) AddService(s config.Service) error {
	if s.Protocol == "" {
		s.Protocol = config.ProtocolTCP
	}
	if err := n.cfg.AddService(s); err != nil {
		return err
	}
	if s.Visibility != config.VisibilityPrivate && n.ctx != nil {
		n.publishService(s)
	}
	n.log.Add(events.KindService, fmt.Sprintf("service added: %s -> 127.0.0.1:%d (%s)", s.Name, s.LocalPort, s.Visibility))
	return nil
}

// RemoveService implements api.Backend. It is unshared from every peer and
// live connections to it are closed.
func (n *Node) RemoveService(name string) error {
	if err := n.cfg.RemoveService(name); err != nil {
		return err
	}
	n.unpublishService(name)
	n.closeConns(func(ci connInfo) bool { return ci.service == name && !ci.outbound })
	n.log.Add(events.KindService, "service removed: "+name)
	return nil
}

// SetServiceVisibility implements api.Backend.
func (n *Node) SetServiceVisibility(name, visibility string) error {
	if err := n.cfg.SetServiceVisibility(name, visibility); err != nil {
		return err
	}
	n.unpublishService(name)
	if svc, ok := n.cfg.Service(name); ok && svc.Visibility != config.VisibilityPrivate && n.ctx != nil {
		n.publishService(svc)
	}
	n.log.Add(events.KindService, fmt.Sprintf("service %s is now %s", name, visibility))
	return nil
}

// SetShare lets peer id open service (built-ins included), or stops it.
// Stopping closes the peer's live connections to that service at once.
func (n *Node) SetShare(id, service string, shared bool) (config.Peer, error) {
	if !slices.Contains(peers.Builtins, service) {
		if _, ok := n.cfg.Service(service); !ok {
			return config.Peer{}, fmt.Errorf("service %q: %w", service, config.ErrNotFound)
		}
	}
	p, err := n.peers.SetShared(id, service, shared)
	if err != nil {
		return p, err
	}
	if shared {
		n.log.Add(events.KindService, fmt.Sprintf("%s can now open %s", p.Name, service))
	} else {
		n.closeConns(func(ci connInfo) bool { return ci.peer == id && ci.service == service && !ci.outbound })
		n.log.Add(events.KindService, fmt.Sprintf("%s can no longer open %s", p.Name, service))
	}
	return p, nil
}

// Peers implements api.Backend. Each peer carries its live,
// handshake-verified connections in both directions, grouped by service.
func (n *Node) Peers() []api.PeerView {
	type key struct {
		peer, svc string
		out       bool
	}
	live := map[key]*api.ConnectionView{}
	n.mu.Lock()
	for _, ci := range n.conns {
		k := key{ci.peer, ci.service, ci.outbound}
		cv := live[k]
		if cv == nil {
			cv = &api.ConnectionView{Service: ci.service, Outbound: ci.outbound, Since: ci.since, Via: ci.via}
			live[k] = cv
		}
		cv.Streams++
		if ci.since.Before(cv.Since) {
			cv.Since = ci.since
		}
	}
	n.mu.Unlock()

	out := []api.PeerView{}
	for _, p := range n.peers.List() {
		v := api.PeerView{
			ID: p.ID, NodeID: p.NodeID, Name: p.Name, Addrs: p.Addrs, Locked: []string{},
			Shares: p.Shares, Services: p.Services, Added: p.Added, LastSeen: p.LastSeen,
			Connections: []api.ConnectionView{},
		}
		if v.Addrs == nil {
			v.Addrs = map[string]string{}
		}
		for t, k := range p.AccessIn {
			if k != "" {
				v.Locked = append(v.Locked, t)
			}
		}
		sort.Strings(v.Locked)
		for k, cv := range live {
			if k.peer == p.ID {
				v.Connections = append(v.Connections, *cv)
			}
		}
		sort.Slice(v.Connections, func(i, j int) bool {
			if v.Connections[i].Service != v.Connections[j].Service {
				return v.Connections[i].Service < v.Connections[j].Service
			}
			return !v.Connections[i].Outbound
		})
		v.Online = len(v.Connections) > 0
		out = append(out, v)
	}
	return out
}

// Invite implements api.Backend: it creates a one-time token and returns
// the pairing link with every address this node is reachable at.
func (n *Node) Invite() (api.InviteInfo, error) {
	access, err := n.issueAccess()
	if err != nil {
		return api.InviteInfo{}, err
	}
	addrs := n.ownAddrs()
	if len(addrs) == 0 {
		return api.InviteInfo{}, errors.New("no transport has published this node yet; check the transport status on the dashboard")
	}
	tok, err := n.pairing.NewToken(access)
	if err != nil {
		return api.InviteInfo{}, err
	}
	n.refreshNodePublication(false) // authorize the token's access keys
	link := pairing.Link{NodeID: n.id.NodeID(), Name: n.cfg.NodeName(), Secret: tok.Secret, Addrs: addrs, Access: map[string]string{}}
	for t, a := range access {
		link.Access[t] = a.Secret
	}
	return api.InviteInfo{Link: link.String(), Expires: tok.Expires}, nil
}

// RestartTransport implements api.Backend.
func (n *Node) RestartTransport(name string) error {
	t, ok := n.reg.Get(name)
	if !ok {
		return fmt.Errorf("transport %q: %w", name, config.ErrNotFound)
	}
	r, ok := t.(interface{ Restart(reason string) })
	if !ok {
		return fmt.Errorf("transport %q cannot be restarted", name)
	}
	r.Restart("reconnect requested from the panel")
	n.log.Add(events.KindTransport, name+": reconnecting (requested from the panel)")
	return nil
}

// Blobs implements api.Backend.
func (n *Node) Blobs() *blob.Store { return n.blobs }

// Events implements api.Backend.
func (n *Node) Events() *events.Log { return n.log }

// Audit implements api.Backend.
func (n *Node) Audit(ctx context.Context) []audit.Result {
	return audit.Run(ctx, audit.Env{Config: n.cfg.Snapshot()}, "")
}

// Transports lists the node's transports (for tests and the CLI).
func (n *Node) Transports() []transport.Transport { return n.reg.List() }
