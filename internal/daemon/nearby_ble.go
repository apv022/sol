package daemon

// Nearby over Bluetooth LE (design/PROTOCOL.md §Nearby "Bluetooth LE (Android, nearby step 3)").
// The Android shell drives the radio; the daemon answers its short commands (the entries to
// advertise, what to do with the entries seen, its state) and owns the BLE transport, whose
// bridge port the shell hands every L2CAP link to. A link carries a session like any other path
// (the handshake authenticates; nothing about the link is trusted) and ranks last. Only under the
// shell (SOL_ANDROID_SHELL=1), off unless config.json says "ble": true; elsewhere the switch is
// not_implemented and the status "unavailable".

import (
	"crypto/ed25519"
	"encoding/hex"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/apv022/sol/internal/nearby"
	"github.com/apv022/sol/internal/session"
	"github.com/apv022/sol/internal/transport"
	"github.com/apv022/sol/sdk/go/sol"
)

// bleShell is what the daemon keeps of the shell's Bluetooth LE: the transport, the listener
// inbound links are served on, and the state the shell last reported.
type bleShell struct {
	t  *transport.BLE
	ln net.Listener

	mu     sync.Mutex
	state  string
	detail string
}

// errNoBLE is the honest answer off Android.
func errNoBLE() error {
	return sol.Errorf("not_implemented", "Bluetooth LE is only in the Android app for now")
}

// addBLE adds the BLE transport under the Android shell (before the transports start).
func (d *Daemon) addBLE() {
	if !AndroidShell() {
		return
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		d.log.Warn("ble: no listener", "err", err)
		return
	}
	b := transport.NewBLE()
	b.Log = d.log
	b.Allow = func() bool { return d.nearbyOn() && d.bleOn() }
	b.OnLink = d.bleLink
	d.bt.t, d.bt.ln = b, ln
	d.transports = append(d.transports, b)
}

// serveBLE serves sessions on the BLE listener (after the session manager exists).
func (d *Daemon) serveBLE() {
	if d.bt.ln != nil {
		go func() { _ = d.Sessions.ServeVia(d.bt.ln, "ble") }()
	}
}

// bleLink: a link the shell opened waits at addr; its device's keeper dials it if nothing
// better answers.
func (d *Daemon) bleLink(addr string) {
	raw, err := hex.DecodeString(strings.TrimPrefix(addr, "ble:"))
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return
	}
	d.Sessions.NoteBLE(ed25519.PublicKey(raw), addr)
}

// overBLE reports whether s runs over Bluetooth LE.
func overBLE(s *session.Session) bool {
	p := s.Path()
	return p.Transport == "ble" || p.Via == "ble"
}

// blobSessions are the sessions blobs are fetched over: not those on Bluetooth LE, which carries
// small syncs only (a file would take minutes and the battery with it).
func (d *Daemon) blobSessions() []*session.Session {
	return slices.DeleteFunc(d.Sessions.Sessions(), overBLE)
}

// bleOn reports whether Bluetooth LE is switched on in the config.
func (d *Daemon) bleOn() bool {
	d.trMu.Lock()
	defer d.trMu.Unlock()
	return d.cfg.BLE != nil && *d.cfg.BLE
}

// bleStatus is Bluetooth LE's part of the nearby status.
func (d *Daemon) bleStatus() nearby.BLEStatus {
	if !AndroidShell() {
		return nearby.BLEStatus{State: "unavailable", Detail: "only in the Android app for now"}
	}
	on := d.bleOn()
	d.bt.mu.Lock()
	defer d.bt.mu.Unlock()
	st := nearby.BLEStatus{Enabled: on, State: d.bt.state, Detail: d.bt.detail}
	switch {
	case !on:
		st.State, st.Detail = "off", ""
	case st.State == "" || st.State == "off":
		st.State, st.Detail = "starting", ""
	}
	return st
}

// SetBLE switches Bluetooth LE on or off in config.json; the Android shell follows (the nearby
// event) and asks for the permissions it needs. Off cuts every link at once.
func (d *Daemon) SetBLE(on bool) (nearby.Status, error) {
	if !AndroidShell() {
		return nearby.Status{}, errNoBLE()
	}
	d.trMu.Lock()
	cfg := d.cfg
	cfg.BLE = &on
	err := writeConfig(d.dir, cfg)
	if err == nil {
		d.cfg.BLE = cfg.BLE
	}
	d.trMu.Unlock()
	if err != nil {
		return nearby.Status{}, err
	}
	if !on && d.bt.t != nil {
		d.bt.t.CloseLinks()
	}
	d.activity("sol.nearby", map[string]string{"ble": map[bool]string{true: "on", false: "off"}[on]}, nil)
	d.nearbyChanged()
	return d.Nearby(), nil
}

// ReportBLE records the state the shell reports (`sol nearby ble-report`).
func (d *Daemon) ReportBLE(state, detail string) error {
	if !AndroidShell() {
		return errNoBLE()
	}
	switch state {
	case "off", "searching", "connected", "unsupported", "needs_permission", "bluetooth_off", "failed":
	default:
		return sol.Errorf("bad_request", "unknown Bluetooth LE state "+strconv.Quote(state))
	}
	if len(detail) > 200 {
		detail = detail[:200]
	}
	d.bt.mu.Lock()
	changed := d.bt.state != state || d.bt.detail != detail
	d.bt.state, d.bt.detail = state, detail
	d.bt.mu.Unlock()
	if changed {
		d.nearbyChanged()
	}
	return nil
}

// BLEAd is the reply of `sol nearby ble-ad`: the entries to advertise (base64, two at a time),
// when to ask again, and the loopback port links are bridged to.
type BLEAd struct {
	Adverts []string `json:"adverts"`
	Expires int64    `json:"expires"`
	Bridge  int      `json:"bridge"`
}

// wifiDirectWorking reports whether this phone's Wi-Fi Direct is on and running (searching or
// in a group), so a BLE match may trigger it.
func (d *Daemon) wifiDirectWorking() bool {
	if !d.wifiDirectOn() {
		return false
	}
	d.wd.mu.Lock()
	defer d.wd.mu.Unlock()
	return d.wd.state == "searching" || d.wd.state == "connected"
}

// BLEAdFor returns the entries the shell advertises, with psm (the LE PSM its L2CAP server
// listens on; 0: none) masked into them.
func (d *Daemon) BLEAdFor(psm int) (BLEAd, error) {
	if !AndroidShell() {
		return BLEAd{}, errNoBLE()
	}
	if psm != 0 && (psm < 0x80 || psm > 0xff) {
		return BLEAd{}, sol.Errorf("bad_request", "psm must be 0 or a dynamic LE PSM (128-255)")
	}
	if d.near == nil || d.bt.t == nil {
		return BLEAd{}, sol.Errorf("unavailable", "nearby is not running")
	}
	ad := BLEAd{Adverts: []string{}, Bridge: d.bt.t.BridgePort()}
	if !d.nearbyOn() || !d.bleOn() {
		ad.Expires = time.Now().Add(nearby.WindowLength).Unix()
		return ad, nil
	}
	es, exp := d.near.BLEAdverts(nearby.BLEAdvert{PSM: uint8(psm), WifiDirect: d.wifiDirectWorking()}, time.Now())
	ad.Adverts, ad.Expires = append(ad.Adverts, es...), exp.Unix()
	return ad, nil
}

// BLEMatch decides what the shell does with the entries it saw (`sol nearby ble-match`); each
// "connect" carries a fresh link handle for the device it is for.
func (d *Daemon) BLEMatch(seen []nearby.BLESeen) (nearby.BLEPlan, error) {
	if !AndroidShell() {
		return nearby.BLEPlan{}, errNoBLE()
	}
	if len(seen) > 64 {
		seen = seen[:64]
	}
	for i := range seen {
		if len(seen[i].Data) > 16 {
			seen[i].Data = seen[i].Data[:16]
		}
	}
	if d.near == nil || d.bt.t == nil || !d.nearbyOn() || !d.bleOn() {
		return nearby.BLEPlan{Actions: []nearby.BLEAction{}, Matched: []string{}}, nil
	}
	state := map[string]session.NearbyPeer{}
	for _, p := range d.Sessions.NearbyPeers() {
		state[string(p.Device)] = p
	}
	plan := d.near.DecideBLE(seen, d.wifiDirectWorking(), func(dev ed25519.PublicKey) (bool, bool) {
		p := state[string(dev)]
		return p.Connected && !p.OverBLE, p.Connected
	}, time.Now())
	for i, a := range plan.Actions {
		if a.Do == "connect" {
			plan.Actions[i].Link = d.bt.t.Expect(transport.BLEAddr(a.Device))
		}
	}
	return plan, nil
}
