package daemon

// Nearby: LAN discovery (design/PROTOCOL.md §Nearby). The nearby service sends beacons only my
// devices and my buddies' devices can recognise; a recognised beacon becomes an address hint
// for that one device in the session layer, which dials it and authenticates whoever answers.
// On unless config.json says "nearby": false; `sol do sol.nearby`, `sol transports
// enable|disable nearby` and the Sol app's Advanced view switch it.
//
// Wi-Fi Direct (§Nearby "Wi-Fi Direct (Android)") is the Android shell's: it drives the radio,
// and the daemon only answers its short commands (the ad to advertise, what to do with the ads
// seen, the group's interface addresses for beacons, its state), so no key leaves the daemon.
// The switch is accepted only under the shell (SOL_ANDROID_SHELL=1); elsewhere it is
// not_implemented.

import (
	"crypto/ed25519"
	"net/netip"
	"os"
	"slices"
	"strconv"
	"sync"
	"time"

	"github.com/apv022/sol/internal/nearby"
	"github.com/apv022/sol/internal/netutil"
	"github.com/apv022/sol/internal/transport"
	"github.com/apv022/sol/sdk/go/sol"
)

// wifiDirect is what the daemon knows of the shell's Wi-Fi Direct: the group interface
// addresses beacons also go out of, and the state the shell last reported.
type wifiDirect struct {
	mu     sync.Mutex
	extra  []netip.Addr
	state  string
	detail string
}

// AndroidShell reports whether this daemon runs under the Android app, which sets
// SOL_ANDROID_SHELL=1 and is the only thing that can drive Wi-Fi Direct.
func AndroidShell() bool { return os.Getenv("SOL_ANDROID_SHELL") == "1" }

// errNoWifiDirect is the honest answer off Android.
func errNoWifiDirect() error {
	return sol.Errorf("not_implemented", "Wi-Fi Direct is only in the Android app for now")
}

// startNearby starts the nearby service, on or off as configured.
func (d *Daemon) startNearby() {
	net := d.opt.NearbyNet
	if net == nil {
		net = nearby.UDP{Extra: d.nearbyExtra}
	}
	d.near = nearby.New(nearby.Config{
		Self:          d.Keyring.DevicePublic(),
		Key:           d.Keyring.NearbyKey,
		Peers:         d.nearbyPeers,
		Ports:         d.nearbyPorts,
		Found:         d.nearbyFound,
		Net:           net,
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

// Nearby reports whether LAN discovery is on and how it is doing, Wi-Fi Direct included.
func (d *Daemon) Nearby() nearby.Status {
	st := nearby.Status{State: "off"}
	if d.near != nil {
		st = d.near.Status()
	}
	wd := d.wifiDirectStatus()
	st.WifiDirect = &wd
	return st
}

// wifiDirectOn reports whether Wi-Fi Direct is switched on in the config.
func (d *Daemon) wifiDirectOn() bool {
	d.trMu.Lock()
	defer d.trMu.Unlock()
	return d.cfg.WifiDirect != nil && *d.cfg.WifiDirect
}

// wifiDirectStatus is Wi-Fi Direct's part of the nearby status.
func (d *Daemon) wifiDirectStatus() nearby.WifiDirectStatus {
	if !AndroidShell() {
		return nearby.WifiDirectStatus{State: "unavailable", Detail: "only in the Android app for now"}
	}
	on := d.wifiDirectOn()
	d.wd.mu.Lock()
	defer d.wd.mu.Unlock()
	st := nearby.WifiDirectStatus{Enabled: on, State: d.wd.state, Detail: d.wd.detail}
	switch {
	case !on:
		st.State, st.Detail = "off", ""
	case st.State == "" || st.State == "off":
		st.State, st.Detail = "starting", ""
	}
	return st
}

// SetWifiDirect switches Wi-Fi Direct on or off in config.json; the Android shell follows (the
// nearby admin event) and asks for the permission it needs.
func (d *Daemon) SetWifiDirect(on bool) (nearby.Status, error) {
	if !AndroidShell() {
		return nearby.Status{}, errNoWifiDirect()
	}
	d.trMu.Lock()
	cfg := d.cfg
	cfg.WifiDirect = &on
	err := writeConfig(d.dir, cfg)
	if err == nil {
		d.cfg.WifiDirect = cfg.WifiDirect
	}
	d.trMu.Unlock()
	if err != nil {
		return nearby.Status{}, err
	}
	if !on {
		d.SetNearbyInterfaces(nil)
	}
	d.activity("sol.nearby", map[string]string{"wifi_direct": map[bool]string{true: "on", false: "off"}[on]}, nil)
	d.nearbyChanged()
	return d.Nearby(), nil
}

// nearbyChanged pushes the nearby status (the Android shell follows the Wi-Fi Direct switch on
// it) and the status.
func (d *Daemon) nearbyChanged() {
	d.emit(EvNearby, d.Nearby())
	d.emitStatus()
}

// ReportWifiDirect records the state the shell reports (`sol nearby report`).
func (d *Daemon) ReportWifiDirect(state, detail string) error {
	if !AndroidShell() {
		return errNoWifiDirect()
	}
	switch state {
	case "off", "searching", "connected", "unsupported", "needs_permission", "wifi_off", "failed":
	default:
		return sol.Errorf("bad_request", "unknown Wi-Fi Direct state "+strconv.Quote(state))
	}
	if len(detail) > 200 {
		detail = detail[:200]
	}
	d.wd.mu.Lock()
	changed := d.wd.state != state || d.wd.detail != detail
	d.wd.state, d.wd.detail = state, detail
	d.wd.mu.Unlock()
	if changed {
		d.nearbyChanged()
	}
	return nil
}

// WifiDirectAd is the reply of `sol nearby ad`: the TXT record to advertise and when to
// advertise anew (the tags' window ends).
type WifiDirectAd struct {
	TXT     map[string]string `json:"txt"`
	Expires int64             `json:"expires"`
}

// WifiDirectAdFor returns the ad the shell advertises, with g's credentials when it owns a
// group.
func (d *Daemon) WifiDirectAdFor(g *nearby.P2PGroup) (WifiDirectAd, error) {
	if !AndroidShell() {
		return WifiDirectAd{}, errNoWifiDirect()
	}
	if d.near == nil {
		return WifiDirectAd{}, sol.Errorf("unavailable", "nearby is not running")
	}
	txt, exp, err := d.near.Ad(g, time.Now())
	if err != nil {
		return WifiDirectAd{}, sol.Errorf("bad_request", err.Error())
	}
	return WifiDirectAd{TXT: txt, Expires: exp.Unix()}, nil
}

// WifiDirectMatch decides what the shell does with the ads it saw (`sol nearby match`).
func (d *Daemon) WifiDirectMatch(state string, seen []nearby.AdSeen) (nearby.Plan, error) {
	if !AndroidShell() {
		return nearby.Plan{}, errNoWifiDirect()
	}
	switch state {
	case "none", "owner", "client":
	default:
		return nearby.Plan{}, sol.Errorf("bad_request", "state must be none, owner or client")
	}
	if len(seen) > 64 {
		seen = seen[:64]
	}
	if d.near == nil || !d.nearbyOn() || !d.wifiDirectOn() {
		return nearby.Plan{Role: "none", Matched: []string{}}, nil
	}
	return d.near.Decide(state, seen, time.Now()), nil
}

// nearbyExtra is the addresses beacons also go out of (nearby.UDP.Extra).
func (d *Daemon) nearbyExtra() []netip.Addr {
	d.wd.mu.Lock()
	defer d.wd.mu.Unlock()
	return slices.Clone(d.wd.extra)
}

// SetNearbyInterfaces sets the IPv4 addresses of interfaces the OS won't list that beacons
// should also go out of (the Android shell's Wi-Fi Direct group; none clears them), and
// reopens the beacon sockets.
func (d *Daemon) SetNearbyInterfaces(addrs []netip.Addr) error {
	var out []netip.Addr
	for _, a := range addrs {
		a = a.Unmap()
		if !a.Is4() || a.IsLoopback() || a.IsUnspecified() || a.IsMulticast() {
			return sol.Errorf("bad_request", a.String()+" is not a unicast IPv4 address")
		}
		if !slices.Contains(out, a) {
			out = append(out, a)
		}
	}
	if len(out) > 8 {
		return sol.Errorf("bad_request", "at most 8 addresses")
	}
	d.wd.mu.Lock()
	same := slices.Equal(d.wd.extra, out)
	d.wd.extra = out
	d.wd.mu.Unlock()
	if !same && d.near != nil {
		d.near.NetworkChanged()
	}
	return nil
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
	d.nearbyChanged()
	return d.Nearby(), nil
}

// nearbyRow is nearby as a row of the transports list (`sol transports list`, `sol status`).
func (d *Daemon) nearbyRow() TransportState {
	st := d.Nearby()
	row := TransportState{Name: "nearby", Enabled: st.Enabled, Detail: st.Detail}
	if wd := st.WifiDirect; wd != nil && wd.Enabled {
		row.Detail += "; Wi-Fi Direct " + wd.State
		if wd.Detail != "" {
			row.Detail += ": " + wd.Detail
		}
	}
	if st.Enabled {
		row.State = st.State
	}
	if st.State == "up" {
		row.Listen = "udp " + nearby.Group + ":" + strconv.Itoa(nearby.Port)
	}
	return row
}
