package daemon

// Transports: which are on, starting them without blocking, and turning them on and off at
// run time (sol transports, POST /admin/v1/transports/{name}, the Sol app's Advanced view).
//
// A new setup turns on WireGuard direct (with hole punching), TLS direct and Tor together;
// path ranking picks the best per peer (design/PROTOCOL.md §Liveness and healing). Tor never
// holds up startup: the daemon is usable on WireGuard and TLS while tor launches and
// bootstraps, and the endpoint record gains the onion when tor has registered it
// (transport.Tor.OnAddress). The anonymous transports (Tor, I2P) publish a listener of their
// own, so the session layer knows which connections came over them (anonymous contacts are
// admitted only there).

import (
	"slices"
	"strconv"

	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/transport"
	"github.com/apv022/sol/sdk/go/sol"
)

// KnownTransports are the transports `sol transports enable` can turn on, in path order.
var KnownTransports = []string{"wg", "direct", "tor"}

// DefaultTransports is the transport set of a new setup: WireGuard direct and TLS direct on
// one free port number (UDP and TCP), and Tor.
func DefaultTransports() []TransportConfig {
	port := strconv.Itoa(freePort())
	return []TransportConfig{
		{Name: "wg", Listen: "0.0.0.0:" + port},
		{Name: "direct", Listen: "0.0.0.0:" + port},
		{Name: "tor"},
	}
}

// defaultTransport is the configuration `sol transports enable <name>` adds.
func defaultTransport(name string) (TransportConfig, error) {
	switch name {
	case "wg", "direct":
		return TransportConfig{Name: name, Listen: "0.0.0.0:" + strconv.Itoa(freePort())}, nil
	case "tor":
		return TransportConfig{Name: "tor"}, nil
	}
	return TransportConfig{}, sol.Errorf("bad_request", "unknown transport "+name+" (wg, direct or tor)")
}

// TransportState is one row of GET /admin/v1/transports: a transport this build knows or one
// the config enables, whether it is on, and how it is doing.
type TransportState struct {
	Name    string `json:"name"`
	Enabled bool   `json:"enabled"`
	// State is "up", "starting", "stopped" or "failed" (empty while disabled); the "nearby"
	// row (LAN discovery, nearby.go) may also be "idle", with the reason in Detail.
	State  string `json:"state,omitempty"`
	Detail string `json:"detail,omitempty"`
	Listen string `json:"listen,omitempty"`
}

// transportsNow returns the running transports.
func (d *Daemon) transportsNow() []transport.Transport {
	d.trMu.Lock()
	defer d.trMu.Unlock()
	return slices.Clone(d.transports)
}

// targetFor is the local listener transport t forwards to: the anonymous one for Tor and I2P.
func (d *Daemon) targetFor(t transport.Transport) string {
	if identity.AnonymousTransport(t.Name()) && d.anonLn != nil {
		return d.anonLn.Addr().String()
	}
	return d.nodeLn.Addr().String()
}

// startTransport starts t and publishes this device on it. It returns the endpoint to sign,
// or false when there is none yet (tor still launching: OnAddress tells refreshEndpoints).
func (d *Daemon) startTransport(t transport.Transport) (identity.Endpoint, bool) {
	if err := t.Start(d.ctx); err != nil {
		d.log.Warn("transport did not start", "transport", t.Name(), "err", err)
		if !transport.Usable(t) {
			return identity.Endpoint{}, false
		}
	}
	p := transport.Publication{ID: "node", Target: d.targetFor(t)}
	if issuesAccess(t) {
		d.accessMu.Lock()
		auth, _ := d.accessSet()
		d.accessMu.Unlock()
		p.ClientAuth = auth // never an open onion, not even for a moment
	}
	addr, err := t.Publish(d.ctx, p)
	if tor, ok := t.(*transport.Tor); ok && d.onions != nil {
		d.onions.publishAll(tor) // public onion services ride along (onions.go)
	}
	if err != nil {
		d.log.Info("transport did not publish yet", "transport", t.Name(), "err", err)
		return identity.Endpoint{}, false
	}
	return endpointOf(t, addr), true
}

// startSlow starts the transports that may take long (Tor) in the background; each signs its
// address into the endpoint record when it has one.
func (d *Daemon) startSlow(ts []transport.Transport) {
	for _, t := range ts {
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			if _, ok := d.startTransport(t); ok {
				d.refreshEndpoints()
			}
		}()
	}
}

// slowStart reports whether t may take seconds to start (tor launches a process and waits for
// its control port).
func slowStart(t transport.Transport) bool { return identity.AnonymousTransport(t.Name()) }

// hookTransport wires t's callbacks to this daemon before it starts.
func (d *Daemon) hookTransport(t transport.Transport) {
	if tor, ok := t.(*transport.Tor); ok {
		tor.OnAddress = func() {
			if d.ctx.Err() == nil {
				d.refreshEndpoints()
				if d.onions != nil {
					d.emit(EvOnion, d.onions.List()) // a public service may have its address now
				}
			}
		}
	}
}

// Transports lists the known transports and every configured one, with their state.
func (d *Daemon) Transports() []TransportState {
	d.trMu.Lock()
	cfg := slices.Clone(d.cfg.Transports)
	running := slices.Clone(d.transports)
	d.trMu.Unlock()
	var out []TransportState
	seen := map[string]bool{}
	for _, tc := range cfg {
		ts := TransportState{Name: tc.Name, Enabled: true, Listen: tc.Listen, State: "stopped"}
		for _, t := range running {
			if t.Name() == tc.Name {
				ts.State, ts.Detail = transportState(t)
			}
		}
		seen[tc.Name] = true
		out = append(out, ts)
	}
	for _, n := range KnownTransports {
		if !seen[n] {
			out = append(out, TransportState{Name: n})
		}
	}
	// LAN discovery is not a transport, but it is switched and watched alongside them.
	return append(out, d.nearbyRow())
}

// transportState is t's state as the admin API words it.
func transportState(t transport.Transport) (string, string) {
	ts := t.Status()
	if ts.State == transport.StateReady {
		return "up", ts.Detail
	}
	return string(ts.State), ts.Detail
}

// SetTransport turns a transport on or off, now and in config.json. Turning one off stops it
// and drops its address from the endpoint record (pushed to every peer); turning one on
// starts it (Tor in the background) and adds its address. The last transport stays on.
func (d *Daemon) SetTransport(name string, on bool) ([]TransportState, error) {
	if name == "nearby" {
		if _, err := d.SetNearby(on); err != nil {
			return nil, err
		}
		return d.Transports(), nil
	}
	d.trMu.Lock()
	idx := slices.IndexFunc(d.cfg.Transports, func(tc TransportConfig) bool { return tc.Name == name })
	switch {
	case on && idx >= 0, !on && idx < 0:
		d.trMu.Unlock()
		return d.Transports(), nil
	case !on && len(d.cfg.Transports) == 1:
		d.trMu.Unlock()
		return nil, sol.Errorf("conflict", "this is the only transport on; turn another on first")
	}
	cfg := d.cfg
	cfg.Transports = slices.Clone(cfg.Transports)
	var started transport.Transport
	var stopped []transport.Transport
	if on {
		tc, err := defaultTransport(name)
		if err != nil {
			d.trMu.Unlock()
			return nil, err
		}
		ts := d.newTransports([]TransportConfig{tc})
		if len(ts) == 0 {
			d.trMu.Unlock()
			return nil, sol.Errorf("bad_request", "unknown transport "+name)
		}
		started = ts[0]
		cfg.Transports = append(cfg.Transports, tc)
		d.transports = append(d.transports, started)
	} else {
		cfg.Transports = slices.Delete(cfg.Transports, idx, idx+1)
		d.transports = slices.DeleteFunc(d.transports, func(t transport.Transport) bool {
			if t.Name() == name {
				stopped = append(stopped, t)
				return true
			}
			return false
		})
	}
	err := writeConfig(d.dir, cfg)
	if err == nil {
		d.cfg.Transports = cfg.Transports
	}
	all := slices.Clone(d.transports)
	d.trMu.Unlock()
	if err != nil {
		return nil, err
	}
	d.Sessions.SetTransports(all)
	for _, t := range stopped {
		_ = t.Unpublish("node")
		_ = t.Stop()
	}
	if started != nil {
		if slowStart(started) {
			d.startSlow([]transport.Transport{started})
		} else if _, ok := d.startTransport(started); !ok {
			d.log.Warn("the new transport has no address yet", "transport", name)
		}
	}
	d.refreshEndpoints()
	if d.near != nil {
		d.near.Update() // the ports a beacon advertises changed
	}
	d.activity("sol.transports."+map[bool]string{true: "enable", false: "disable"}[on], map[string]string{"name": name}, nil)
	d.emitStatus()
	return d.Transports(), nil
}
