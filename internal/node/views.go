package node

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
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
		HelpTor:    n.helper.Status(),

		RestartNeeded: n.restartNeeded.Load(),
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
	// (A site runs when its folder could be opened.)
	for i := range out {
		if out[i].Dir != "" {
			out[i].Running = n.siteHandler(out[i].Name) != nil
			continue
		}
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
		if s.Dir != "" {
			s.Protocol = config.ProtocolHTTP
		}
	}
	if err := n.cfg.AddService(s); err != nil {
		return err
	}
	where := fmt.Sprintf("127.0.0.1:%d", s.LocalPort)
	if s.Dir != "" {
		where = "the folder " + s.Dir
		if err := n.openSite(s); err != nil {
			n.cfg.RemoveService(s.Name) // a site nobody can open is not worth keeping
			return err
		}
	}
	if s.Visibility != config.VisibilityPrivate && n.ctx != nil {
		n.publishService(s)
	}
	n.log.Add(events.KindService, fmt.Sprintf("service added: %s -> %s (%s)", s.Name, where, s.Visibility))
	return nil
}

// RemoveService implements api.Backend. It is unshared from every peer and
// live connections to it are closed.
func (n *Node) RemoveService(name string) error {
	if err := n.cfg.RemoveService(name); err != nil {
		return err
	}
	n.unpublishService(name)
	n.closeSite(name)
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
		v.Path = n.peerPath(p)
		out = append(out, v)
	}
	return out
}

// peerPath works out how this node reaches p: the transport in use (or the
// one that last worked), and, when that isn't WireGuard, why direct mode is
// not carrying the connection and what would change that. It only reads
// what the dialer and the liveness supervisor know (PeerLink), the last good
// address, and which transports are running.
func (n *Node) peerPath(p config.Peer) api.PathView {
	var v api.PathView
	nameOf := func(addr string) string {
		if t, err := n.reg.For(addr); err == nil && addr != "" {
			return t.Name()
		}
		return ""
	}
	usable := func(name string) bool {
		t, ok := n.reg.Get(name)
		return ok && transport.Usable(t)
	}

	wgAddr, tlsAddr := p.Addrs["wg"], p.Addrs["direct"]
	link, _ := n.PeerLink(p.ID)
	n.mu.Lock()
	last := n.lastGood[p.ID]
	n.mu.Unlock()

	// When direct is tried first again: a path in probation is preferred at
	// PreferredAt, one in backoff is next probed at NextProbe. A direct
	// address that isn't listed is preferred already (direct_next).
	var retry time.Duration
	var probation, preferred bool
	now := time.Now()
	for kind, a := range map[string]string{"wg": wgAddr, "direct": tlsAddr} {
		if a == "" {
			continue
		}
		dp, failed := link.Direct[kind]
		if !failed {
			preferred = true
			continue
		}
		at, prob := dp.NextProbe, dp.State == "probation"
		if prob {
			at = dp.PreferredAt
		}
		d := at.Sub(now)
		switch {
		case prob && !probation, prob == probation && (retry <= 0 || d < retry):
			retry, probation = d, prob
		}
	}
	if link.Down {
		v.Down = true
		v.RedialIn = max(0, int(link.NextRedial.Sub(now).Round(time.Second)/time.Second))
	}

	v.Via, v.Live = link.Via, link.Live
	if v.Via == "" {
		v.Via = nameOf(last)
	}
	if order := n.addrsInOrder(p); len(order) > 0 {
		v.Next = nameOf(order[0])
	}
	if v.Via == "wg" {
		return v // the fastest path there is
	}

	who := nameOr(p)
	port := "the direct mode port"
	if _, pt, err := net.SplitHostPort(n.cfg.Snapshot().Transports.DirectListen); err == nil && pt != "" {
		port = "port " + pt
	}
	failed := func(name string) string { // "" unless the transport is registered but broken
		if t, ok := n.reg.Get(name); ok && !transport.Usable(t) {
			return t.Status().Detail
		}
		return ""
	}
	switch {
	case v.Via == "direct" && wgAddr != "" && usable("wg"):
		v.Reason = "wg_blocked"
		v.Detail = fmt.Sprintf("WireGuard (UDP) didn't get through to %s, so this is a direct TLS connection over TCP instead. It is still direct, only a little slower.", who)
		v.Fix = fmt.Sprintf("Allow UDP %s in the firewall on both machines.", port)
	case v.Via == "direct":
		// TLS direct is the best this pair can do; nothing to explain.
	case failed("wg") != "" || failed("direct") != "":
		v.Reason = "direct_failed"
		v.Detail = "Direct mode is on but couldn't start: " + cmp(failed("wg"), failed("direct")) + "."
		v.Fix = "Check the direct mode port in Settings (another program may be using it), then restart the node."
	case !usable("wg") && !usable("direct"):
		if n.cfg.Snapshot().Transports.Direct {
			v.Reason = "direct_restart"
			v.Detail = "Direct mode is turned on but hasn't started: the node needs a restart first."
			v.Fix = "Restart the node (Settings)."
		} else {
			v.Reason = "direct_off"
			v.Detail = "Direct mode is off on this machine."
			v.Fix = fmt.Sprintf("For a much faster connection, turn on direct mode in Settings here and on %s. Each machine then sees the other's IP address.", who)
		}
	case wgAddr == "" && tlsAddr == "":
		v.Reason = "peer_no_direct"
		v.Detail = fmt.Sprintf("Direct mode is on here, but %s hasn't given this machine a direct address: direct mode is off there, or it hasn't told this machine yet.", who)
		v.Fix = fmt.Sprintf("Turn on direct mode on %s and restart it, then choose \"Check again\" here.", who)
	case preferred:
		v.Reason = "direct_next"
		v.Detail = fmt.Sprintf("%s has a direct address. Sol tries it first on the next connection.", who)
	case probation:
		v.Reason = "direct_probation"
		v.RetryIn = max(0, int(retry.Round(time.Second)/time.Second))
		v.Detail = fmt.Sprintf("Direct to %s works again, but Sol waits until it has stayed up for a while before going back to it.", who)
		if v.RetryIn > 0 {
			v.Detail = fmt.Sprintf("Direct to %s works again, but Sol waits until it has stayed up for a while: it goes back to direct in %s unless it fails again.", who, roughly(retry))
		}
	default:
		v.Reason = "direct_backoff"
		v.RetryIn = max(0, int(retry.Round(time.Second)/time.Second))
		v.Detail = fmt.Sprintf("The last direct connection to %s failed, so Sol fell back to a path that works. It checks direct again in the background", who)
		if v.RetryIn > 0 {
			v.Detail += " in " + roughly(retry)
		}
		v.Detail += ", and goes back to it once that stays reliable."
		v.Fix = fmt.Sprintf("Usually a firewall or router is blocking %s (UDP and TCP) on one side. Allow it on both machines, or forward it on the router and set \"Forwarded addresses\" in Settings.", port)
	}
	return v
}

// cmp returns the first non-empty string.
func cmp(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// roughly words a wait: "about 40 seconds", "about 12 minutes".
func roughly(d time.Duration) string {
	switch {
	case d < 90*time.Second:
		return fmt.Sprintf("about %d seconds", max(1, int(d.Seconds())))
	case d < 90*time.Minute:
		return fmt.Sprintf("about %d minutes", int(d.Round(time.Minute).Minutes()))
	}
	return "about an hour"
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

// Options implements api.Backend: every setting with its current value.
func (n *Node) Options() []config.Option { return n.cfg.Options() }

// SetOption implements api.Backend: it changes one setting. Name and
// help_tor apply at once; the rest are read at start-up, so the node
// remembers that a restart is needed (Status.RestartNeeded).
func (n *Node) SetOption(key, value string) (restart bool, err error) {
	switch key {
	case "name":
		return false, n.SetName(value)
	case "help_tor":
		on := false
		for _, o := range n.cfg.Options() {
			on = on || (o.Key == key && o.Value == "on")
		}
		if _, err := n.cfg.SetOption(key, value); err != nil { // validates the word
			return false, err
		}
		if now := n.cfg.HelpsTor(); now != on {
			return false, n.SetHelpTor(now)
		}
		return false, nil
	}
	restart, err = n.cfg.SetOption(key, value)
	if err != nil {
		return false, err
	}
	if restart {
		n.restartNeeded.Store(true)
	}
	n.log.Add(events.KindService, "setting changed: "+key)
	return restart, nil
}

// OptionImpact implements api.Backend: what SetOption(key, value) would do
// to this node and its peers, in plain sentences. It changes nothing and
// only reads the saved config, so it is right even before a restart.
func (n *Node) OptionImpact(key, value string) (api.Impact, error) {
	im := api.Impact{Key: key, Value: strings.TrimSpace(value)}
	var cur config.Option
	found := false
	for _, o := range n.cfg.Options() {
		if o.Key == key {
			cur, found = o, true
		}
	}
	if !found {
		return im, fmt.Errorf("setting %q: %w", key, config.ErrNotFound)
	}
	im.Restart = cur.Restart
	im.Changes = im.Value != cur.Value
	if !im.Changes {
		return im, nil
	}
	if err := n.cfg.CheckOption(key, im.Value); err != nil {
		im.Problem = err.Error() // refused: nothing else to preview
		return im, nil
	}
	say := func(format string, a ...any) { im.Effects = append(im.Effects, fmt.Sprintf(format, a...)) }

	tr := n.cfg.Snapshot().Transports
	on := map[string]bool{"tor": tr.Tor, "i2p": tr.I2P, "clearnet": tr.Clearnet, "wg": tr.Direct, "direct": tr.Direct}
	after := maps.Clone(on)
	turnOn := im.Value == "on"
	switch key {
	case "tor":
		after["tor"] = turnOn
	case "direct":
		after["wg"], after["direct"] = turnOn, turnOn
	case "clearnet":
		after["clearnet"] = turnOn
	}
	reach := func(p config.Peer, enabled map[string]bool) bool {
		for name, a := range p.Addrs {
			if a != "" && enabled[name] {
				return true
			}
		}
		return false
	}
	for _, p := range n.peers.List() {
		if reach(p, on) && !reach(p, after) {
			im.Unreachable = append(im.Unreachable, api.ImpactPeer{ID: p.ID, Name: nameOr(p)})
		}
	}
	lost := make([]string, 0, len(im.Unreachable))
	for _, p := range im.Unreachable {
		lost = append(lost, p.Name)
	}
	anyLeft := after["tor"] || after["clearnet"] || after["wg"]

	switch {
	case key == "tor" && !turnOn:
		im.Confirm = true
		if !anyLeft {
			say("Every way to connect would be off: no peer could reach this machine, and it could reach none.")
		}
		if len(lost) > 0 {
			say("%s will be unreachable: Tor is the only way this machine knows to reach %s. Turn on direct mode on both machines first, and connect once, to keep the connection.", list(lost), them(len(lost)))
		}
		var public []string
		for _, s := range n.cfg.ServiceList() {
			if s.Visibility != config.VisibilityPrivate {
				public = append(public, s.Name)
			}
		}
		if len(public) > 0 {
			say("The public .onion address of %s stops working.", list(public))
		}
		if anyLeft {
			say("Peers reach this machine only by its IP address, so they see where it is, and it can't be reached from behind most routers without direct mode or a forwarded port.")
		}
	case key == "tor":
		say("Tor starts with the node. It takes from 30 seconds to a couple of minutes before peers can reach this machine over it.")
		say("Connections over Tor hide this machine's IP address, and work behind any router.")
	case key == "direct" && turnOn:
		im.Confirm = true
		say("Peers you connect to directly see this machine's IP addresses, and it sees theirs. Connections are still encrypted and only your paired peers can connect.")
		say("A peer is reached directly only once direct mode is on there too. Until then, and whenever a direct connection fails, Sol keeps using %s.", cmp(pick(after["tor"], "Tor"), "the other networks"))
		say("This machine listens on %s, UDP and TCP. Allow it in the firewall.", cmp(tr.DirectListen, ":7701"))
	case key == "direct":
		im.Confirm = len(lost) > 0 || !anyLeft
		if !anyLeft {
			say("Every way to connect would be off: no peer could reach this machine, and it could reach none.")
		}
		if len(lost) > 0 {
			say("%s will be unreachable: a direct address is the only way this machine knows to reach %s.", list(lost), them(len(lost)))
		}
		if after["tor"] {
			say("Connections go through Tor again: slower, and peers no longer see this machine's IP address.")
		}
	case key == "clearnet" && turnOn:
		im.Confirm = true
		say("Traffic on the LAN transport is not encrypted: anyone on the same network can read it. Use it only on one network you trust, or for testing.")
		say("Peers still prove who they are before they get in, and prefer Tor or direct mode when those are on.")
	case key == "clearnet":
		im.Confirm = len(lost) > 0 || !anyLeft
		if !anyLeft {
			say("Every way to connect would be off: no peer could reach this machine, and it could reach none.")
		}
		if len(lost) > 0 {
			say("%s will be unreachable: the LAN address is the only way this machine knows to reach %s.", list(lost), them(len(lost)))
		}
	case key == "panel_port":
		say("This panel moves to http://127.0.0.1:%s. Bookmarks to the old address stop working.", im.Value)
	case key == "help_tor" && turnOn:
		im.Confirm = true
		say("A Snowflake proxy starts now. It contacts the Tor Project and public STUN servers from this machine's IP address, and uses some of your bandwidth.")
	case key == "stun" && im.Value != "":
		say("Each STUN server you list learns this machine's public IP address.")
	}
	if cur.Restart {
		say("Takes effect when the node restarts. Connections drop for a few seconds.")
	}
	return im, nil
}

// pick returns s if ok, else "".
func pick(ok bool, s string) string {
	if ok {
		return s
	}
	return ""
}

// list joins names for a sentence: "a", "a and b", "a, b and c".
func list(names []string) string {
	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	}
	return strings.Join(names[:len(names)-1], ", ") + " and " + names[len(names)-1]
}

func them(n int) string {
	if n == 1 {
		return "it"
	}
	return "them"
}

// SetHelpTor implements api.Backend: it saves the choice and starts or
// stops the Snowflake proxy now. It is opt-in: the proxy contacts third
// parties from this machine's IP (internal/snowflake).
func (n *Node) SetHelpTor(on bool) error {
	if err := n.cfg.SetHelpTor(on); err != nil {
		return err
	}
	if on {
		if n.ctx != nil {
			n.helper.Start(n.ctx)
		}
		n.log.Add(events.KindService, "helping the Tor network: Snowflake proxy turned on")
	} else {
		n.helper.Stop()
		n.log.Add(events.KindService, "Snowflake proxy turned off")
	}
	return nil
}

// panelCSSMax bounds the user's stylesheet; anything larger is a mistake.
const panelCSSMax = 256 << 10

// PanelCSS implements api.Backend: <DataDir>/panel.css, read on every
// request so an edit shows on reload. A missing, unreadable or oversized
// file means no user styles.
func (n *Node) PanelCSS() []byte {
	path := filepath.Join(n.opts.DataDir, "panel.css")
	fi, err := os.Stat(path)
	if err != nil || !fi.Mode().IsRegular() || fi.Size() > panelCSSMax {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	return b
}

// Transports lists the node's transports (for tests and the CLI).
func (n *Node) Transports() []transport.Transport { return n.reg.List() }
