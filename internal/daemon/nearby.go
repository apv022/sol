package daemon

// Nearby: LAN discovery (design/PROTOCOL.md §Nearby). The nearby service sends beacons only my
// devices and my buddies' devices can recognise; a recognised beacon becomes an address hint
// for that one device in the session layer, which dials it and authenticates whoever answers.
// On unless config.json says "nearby": false; `sol do sol.nearby`, `sol transports
// enable|disable nearby` and the Sol app's Advanced view switch it.

import (
	"crypto/ed25519"
	"net/netip"
	"slices"
	"strconv"

	"github.com/apv022/sol/internal/nearby"
	"github.com/apv022/sol/internal/netutil"
	"github.com/apv022/sol/internal/transport"
)

// startNearby starts the nearby service, on or off as configured.
func (d *Daemon) startNearby() {
	d.near = nearby.New(nearby.Config{
		Self:          d.Keyring.DevicePublic(),
		Key:           d.Keyring.NearbyKey,
		Peers:         d.nearbyPeers,
		Ports:         d.nearbyPorts,
		Found:         d.nearbyFound,
		Net:           d.opt.NearbyNet,
		AllowLoopback: d.opt.NearbyNet != nil,
		Log:           d.log,
	})
	d.near.SetEnabled(d.nearbyOn())
}

// nearbyOn reports whether nearby is switched on in the config.
func (d *Daemon) nearbyOn() bool {
	d.trMu.Lock()
	defer d.trMu.Unlock()
	return d.cfg.Nearby == nil || *d.cfg.Nearby
}

// nearbyPeers are the devices beacons are for: kept ones, without anonymous contacts'.
func (d *Daemon) nearbyPeers() []nearby.Peer {
	ps := d.Sessions.NearbyPeers()
	out := make([]nearby.Peer, len(ps))
	for i, p := range ps {
		out[i] = nearby.Peer{Device: p.Device, Connected: p.Connected}
	}
	return out
}

// nearbyPorts are the ports a beacon advertises: those of the running transports that listen
// on a non-loopback address (a loopback listener is reachable from nothing on the LAN; with a
// test bus, loopback counts as the LAN).
func (d *Daemon) nearbyPorts() nearby.Ports {
	d.trMu.Lock()
	listen := map[string]string{}
	for _, tc := range d.cfg.Transports {
		listen[tc.Name] = tc.Listen
	}
	ts := slices.Clone(d.transports)
	d.trMu.Unlock()
	var p nearby.Ports
	for _, t := range ts {
		if !transport.Usable(t) || (netutil.IsLoopbackListen(listen[t.Name()]) && d.opt.NearbyNet == nil) {
			continue
		}
		port := transport.LocalPort(t)
		if port <= 0 || port > 65535 {
			continue
		}
		switch t.Name() {
		case "wg":
			p.WG = uint16(port)
		case "direct":
			p.Direct = uint16(port)
		case "clearnet":
			p.Clearnet = uint16(port)
		}
	}
	return p
}

// nearbyFound hands a recognised device's LAN address to the session layer.
func (d *Daemon) nearbyFound(dev ed25519.PublicKey, src netip.Addr, p nearby.Ports) {
	addrs := map[string]string{}
	for name, port := range map[string]uint16{"wg": p.WG, "direct": p.Direct, "clearnet": p.Clearnet} {
		if port != 0 {
			addrs[name] = netip.AddrPortFrom(src, port).String()
		}
	}
	d.Sessions.NoteNearby(dev, addrs)
}

// Nearby reports whether LAN discovery is on and how it is doing.
func (d *Daemon) Nearby() nearby.Status {
	if d.near == nil {
		return nearby.Status{State: "off"}
	}
	return d.near.Status()
}

// SetNearby switches LAN discovery on or off, now and in config.json.
func (d *Daemon) SetNearby(on bool) (nearby.Status, error) {
	d.trMu.Lock()
	cfg := d.cfg
	cfg.Nearby = &on
	err := writeConfig(d.dir, cfg)
	if err == nil {
		d.cfg.Nearby = cfg.Nearby
	}
	d.trMu.Unlock()
	if err != nil {
		return nearby.Status{}, err
	}
	if d.near != nil {
		d.near.SetEnabled(on)
	}
	d.activity("sol.nearby", map[string]string{"state": map[bool]string{true: "on", false: "off"}[on]}, nil)
	d.emitStatus()
	return d.Nearby(), nil
}

// nearbyRow is nearby as a row of the transports list (`sol transports list`, `sol status`).
func (d *Daemon) nearbyRow() TransportState {
	st := d.Nearby()
	row := TransportState{Name: "nearby", Enabled: st.Enabled, Detail: st.Detail}
	if st.Enabled {
		row.State = st.State
	}
	if st.State == "up" {
		row.Listen = "udp " + nearby.Group + ":" + strconv.Itoa(nearby.Port)
	}
	return row
}
