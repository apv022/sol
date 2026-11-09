package transport

// WireGuard direct mode: peers reach each other over a WireGuard tunnel
// (the official wireguard-go, in userspace with its own network stack, so no
// root, no TUN device, no VPN slot), punched through NATs, and fall back to
// Tor when no direct path can be made (docs/PROTOCOL.md, "Direct mode").
//
// How a path is made (the Tailscale recipe, with Tor in place of its
// coordination servers and relays):
//
//  1. Each node has a WireGuard key (wg.key) and learns its public UDP
//     address from STUN, on the WireGuard socket itself.
//  2. Peers exchange "sol-wg:<key>@<endpoint>,..." addresses over the
//     authenticated Tor channel (pairing, list requests, PeerInfo).
//  3. Both sides send small punch packets ("SOLP" + their WireGuard key) to
//     each other's candidates. Outgoing packets open each NAT for the other
//     side's packets, and the peer learns where the sender really appears
//     from. Only one side, the one with the lower key, starts WireGuard
//     handshakes (to that observed address, else rotating through the
//     candidates): two simultaneous initiations cancel each other out.
//     WireGuard then follows whichever endpoint answers.
//  4. Inside the tunnel the node is at a private IPv6 address derived from
//     its key, and the usual Sol handshake runs on top.
//
// Invariants:
//   - WireGuard answers only keys it was given (paired peers), so a stranger
//     gets no reply at all, and AllowedIPs pins each peer to its own tunnel
//     address. The Sol handshake inside still checks the node key.
//   - Only the node publication is carried (no public services).
//   - Symmetric NATs (some mobile carriers, many corporate networks) can't
//     be punched; those peers use Tor. Nothing is relayed through third
//     parties.

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.zx2c4.com/wireguard/conn"
	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"

	"github.com/apv022/sol/internal/netutil"
)

const (
	wgScheme = "sol-wg:"
	// tunnelNodePort is where the node listens inside the tunnel.
	tunnelNodePort = 7700
	// wgMTU leaves room for WireGuard's 80 bytes of overhead on 1360+ paths.
	wgMTU = 1280
	// WGDialTimeout bounds waiting for a handshake before giving up on the
	// direct path for this connection (the punch keeps going).
	WGDialTimeout = 3 * time.Second
	// wgFresh: a handshake younger than this means the tunnel is up
	// (WireGuard rekeys every 2 minutes; sessions die at 3).
	wgFresh = 150 * time.Second
	// wgTryEvery matches WireGuard's own handshake retry (REKEY_TIMEOUT).
	wgTryEvery  = 5 * time.Second
	wgKeepalive = 25 // seconds; keeps NAT mappings open once a tunnel is up
	// observedFor: a punch packet's source address is preferred for this long.
	observedFor = 30 * time.Second
)

// DefaultSTUN are the public STUN servers asked for this node's public
// address. They learn that address (not whom you talk to).
var DefaultSTUN = []string{"stun.cloudflare.com:3478", "stun.l.google.com:19302"}

// IsWG reports whether addr is a WireGuard direct address.
func IsWG(addr string) bool { return strings.HasPrefix(addr, wgScheme) }

// WG is the WireGuard direct transport.
type WG struct {
	ListenAddr string   // UDP, e.g. ":7701"
	Advertise  []string // replaces the advertised candidates (a forwarded port)
	STUN       []string // STUN servers; empty means none (LAN/IPv6 only)
	keyPath    string

	allowLoopback bool

	mu      sync.Mutex
	state   State
	detail  string
	priv    [32]byte
	pub     [32]byte
	ip      netip.Addr
	dev     *device.Device
	tnet    *netstack.Net
	bind    *stunBind
	port    uint16
	target  string
	ln      net.Listener
	public  netip.AddrPort // from STUN
	note    string         // shown in Status (e.g. a port fallback)
	peers   map[[32]byte]*wgPeer
	stopped chan struct{}
}

type wgPeer struct {
	pub      [32]byte
	ip       netip.Addr
	cands    []string
	idx      int
	nextTry  time.Time
	misses   int
	lastHS   time.Time
	up       bool      // keepalive configured (tunnel live)
	observed string    // where its last punch packet came from
	obsAt    time.Time // when
	lastAct  time.Time // last punch reply / handshake start caused by a punch
	lastRst  time.Time // last session reset caused by a punch
}

// resetEvery limits session resets caused by punch packets (which anyone
// could forge) to a short blip at worst.
const resetEvery = 10 * time.Second

// initiator: the side with the lower key starts handshakes.
func (w *WG) initiator(p *wgPeer) bool { return bytes.Compare(w.pub[:], p.pub[:]) < 0 }

// NewWG constructs the transport. keyPath holds the WireGuard key (created
// on first start, 0600).
func NewWG(listenAddr, keyPath string, advertise, stun []string) *WG {
	return &WG{ListenAddr: listenAddr, keyPath: keyPath, Advertise: advertise, STUN: stun,
		allowLoopback: netutil.IsLoopbackListen(listenAddr), state: StateStopped, peers: map[[32]byte]*wgPeer{}}
}

// Name implements Transport.
func (w *WG) Name() string { return "wg" }

// tunnelIP is a peer's address inside the tunnel: fd53:4f4c::/32 ("SOL")
// plus 96 bits of the hash of its WireGuard key.
func tunnelIP(pub [32]byte) netip.Addr {
	h := sha256.Sum256(pub[:])
	var a [16]byte
	a[0], a[1], a[2], a[3] = 0xfd, 0x53, 0x4f, 0x4c
	copy(a[4:], h[:12])
	return netip.AddrFrom16(a)
}

func loadOrCreateWGKey(path string) (priv, pub [32]byte, err error) {
	if b, err := os.ReadFile(path); err == nil {
		raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
		if err != nil || len(raw) != 32 {
			return priv, pub, fmt.Errorf("%s: not a WireGuard key", path)
		}
		copy(priv[:], raw)
	} else if errors.Is(err, os.ErrNotExist) {
		if _, err := rand.Read(priv[:]); err != nil {
			return priv, pub, err
		}
		priv[0] &= 248 // X25519 clamping, as WireGuard keys are stored
		priv[31] = priv[31]&127 | 64
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return priv, pub, err
		}
		tmp := path + ".tmp"
		if err := os.WriteFile(tmp, []byte(base64.StdEncoding.EncodeToString(priv[:])+"\n"), 0o600); err != nil {
			return priv, pub, err
		}
		if err := os.Rename(tmp, path); err != nil {
			return priv, pub, err
		}
	} else {
		return priv, pub, err
	}
	k, err := ecdh.X25519().NewPrivateKey(priv[:])
	if err != nil {
		return priv, pub, err
	}
	copy(pub[:], k.PublicKey().Bytes())
	return priv, pub, nil
}

// Start brings the WireGuard device up (no peers yet).
func (w *WG) Start(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped != nil {
		return nil
	}
	fail := func(err error) error {
		w.state, w.detail = StateFailed, err.Error()
		return err
	}
	priv, pub, err := loadOrCreateWGKey(w.keyPath)
	if err != nil {
		return fail(err)
	}
	_, portStr, err := net.SplitHostPort(w.ListenAddr)
	if err != nil {
		return fail(fmt.Errorf("wireguard listen %q: %w", w.ListenAddr, err))
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return fail(fmt.Errorf("wireguard listen %q: bad port", w.ListenAddr))
	}
	ip := tunnelIP(pub)
	tun, tnet, err := netstack.CreateNetTUN([]netip.Addr{ip}, nil, wgMTU)
	if err != nil {
		return fail(err)
	}
	bind := newSTUNBind(conn.NewDefaultBind())
	bind.onPunch = w.onPunch
	dev := device.NewDevice(tun, bind, device.NewLogger(wgLogLevel(), "wg: "))
	note := ""
	if err := dev.IpcSet(fmt.Sprintf("private_key=%s\nlisten_port=%d\n", hex.EncodeToString(priv[:]), port)); err != nil {
		// The port is taken (another program, or an old node still exiting).
		// Any port works: peers learn the current one with the address.
		if err2 := dev.IpcSet("listen_port=0\n"); err2 != nil {
			dev.Close()
			return fail(fmt.Errorf("wireguard: %w", err))
		}
		note = fmt.Sprintf("UDP %d was taken, so a random port is used (a forwarded port won't work until it's free and Sol restarts)", port)
	}
	if err := dev.Up(); err != nil {
		// Depending on the platform the port clash shows up here, when the
		// socket is opened, rather than when the port is set.
		if note != "" || dev.IpcSet("listen_port=0\n") != nil || dev.Up() != nil {
			dev.Close()
			return fail(fmt.Errorf("wireguard: %w", err))
		}
		note = fmt.Sprintf("UDP %d was taken, so a random port is used (a forwarded port won't work until it's free and Sol restarts)", port)
	}
	w.priv, w.pub, w.ip, w.dev, w.tnet, w.bind = priv, pub, ip, dev, tnet, bind
	w.port = uint16(port)
	if got := ipcValue(dev, "listen_port"); got != "" {
		if p, err := strconv.Atoi(got); err == nil {
			w.port = uint16(p)
		}
	}
	w.state, w.detail, w.note = StateReady, "", note
	w.stopped = make(chan struct{})
	for _, p := range w.peers { // peers set before Start
		w.configureLocked(p)
	}
	go w.loop(w.stopped)
	return nil
}

// ipcValue reads one top-level key from the device's UAPI dump.
func ipcValue(dev *device.Device, key string) string {
	dump, err := dev.IpcGet()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(dump, "\n") {
		if v, ok := strings.CutPrefix(line, key+"="); ok {
			return v
		}
		if strings.HasPrefix(line, "public_key=") {
			break // past the interface section
		}
	}
	return ""
}

// Publish serves the node inside the tunnel.
func (w *WG) Publish(ctx context.Context, p Publication) (string, error) {
	if p.ID != "node" {
		return "", fmt.Errorf("direct mode carries only the node itself, not %q: %w", p.ID, ErrNotImplemented)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped == nil {
		return "", errors.New("wireguard: not started")
	}
	w.target = p.Target
	if w.ln == nil {
		ln, err := w.tnet.ListenTCPAddrPort(netip.AddrPortFrom(w.ip, tunnelNodePort))
		if err != nil {
			return "", fmt.Errorf("wireguard: listen in tunnel: %w", err)
		}
		w.ln = ln
		go w.serve(ln)
	}
	return w.addressLocked(), nil
}

func (w *WG) serve(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) || strings.Contains(err.Error(), "closed") {
				return
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go func() {
			w.mu.Lock()
			target := w.target
			w.mu.Unlock()
			if target == "" {
				c.Close()
				return
			}
			out, err := net.DialTimeout("tcp", target, 5*time.Second)
			if err != nil {
				slog.Debug("wireguard: node listener unreachable", "err", err)
				c.Close()
				return
			}
			netutil.Pipe(c, out)
		}()
	}
}

// Unpublish stops serving the node in the tunnel.
func (w *WG) Unpublish(id string) error {
	if id != "node" {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.target = ""
	if w.ln != nil {
		w.ln.Close()
		w.ln = nil
	}
	return nil
}

func (w *WG) addressLocked() string {
	if w.ln == nil {
		return ""
	}
	cands := w.Advertise
	if len(cands) == 0 {
		if w.allowLoopback {
			cands = []string{net.JoinHostPort("127.0.0.1", strconv.Itoa(int(w.port)))}
		} else {
			cands = candidates(&net.TCPAddr{IP: net.IPv6unspecified, Port: int(w.port)}, w.ListenAddr)
			if w.public.IsValid() {
				cands = append([]string{w.public.String()}, cands...) // what a peer elsewhere can reach
			}
			if len(cands) > directMaxCands {
				cands = cands[:directMaxCands]
			}
		}
	}
	if len(cands) == 0 {
		return ""
	}
	return wgScheme + base64.RawURLEncoding.EncodeToString(w.pub[:]) + "@" + strings.Join(cands, ",")
}

// ParseWGAddr splits "sol-wg:<key>@a,b" into the WireGuard key and the
// candidates.
func ParseWGAddr(addr string) ([32]byte, []string, error) {
	var pub [32]byte
	rest, ok := strings.CutPrefix(addr, wgScheme)
	if !ok {
		return pub, nil, fmt.Errorf("not a wireguard address: %q", addr)
	}
	key, list, ok := strings.Cut(rest, "@")
	raw, err := base64.RawURLEncoding.DecodeString(key)
	if !ok || err != nil || len(raw) != 32 || list == "" {
		return pub, nil, fmt.Errorf("bad wireguard address %q", addr)
	}
	copy(pub[:], raw)
	var cands []string
	for _, c := range strings.Split(list, ",") {
		if _, _, err := net.SplitHostPort(c); err == nil {
			cands = append(cands, c)
		}
		if len(cands) == directMaxCands {
			break
		}
	}
	if len(cands) == 0 {
		return pub, nil, fmt.Errorf("bad wireguard address %q", addr)
	}
	return pub, cands, nil
}

// Handles implements Transport (only while started).
func (w *WG) Handles(addr string) bool {
	w.mu.Lock()
	started := w.stopped != nil
	w.mu.Unlock()
	return started && IsWG(addr)
}

// SetPeer tells the device about a paired peer's WireGuard address, so the
// two can find each other from both sides. Peer-supplied candidates are
// filtered like direct ones (netutil.DirectCandidate).
func (w *WG) SetPeer(addr string) error {
	pub, all, err := ParseWGAddr(addr)
	if err != nil {
		return err
	}
	var cands []string
	for _, c := range all {
		if netutil.DirectCandidate(c, w.allowLoopback) == nil {
			cands = append(cands, c)
		}
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if pub == w.pub {
		return errors.New("wireguard: that is this node's own key")
	}
	p := w.peers[pub]
	if p == nil {
		p = &wgPeer{pub: pub, ip: tunnelIP(pub)}
		w.peers[pub] = p
	}
	changed := strings.Join(p.cands, ",") != strings.Join(cands, ",")
	if changed {
		p.cands, p.idx = cands, 0
	}
	if w.dev != nil && (changed || !p.up) {
		p.nextTry, p.misses = time.Now().Add(wgTryEvery), 0
		w.configureLocked(p)
		go w.punch(p) // right away: the other side may be punching towards us now
	}
	return nil
}

// RemovePeer forgets a peer (unpairing): its tunnel closes at once.
func (w *WG) RemovePeer(addr string) {
	pub, _, err := ParseWGAddr(addr)
	if err != nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	delete(w.peers, pub)
	if w.dev != nil {
		w.dev.IpcSet("public_key=" + hex.EncodeToString(pub[:]) + "\nremove=true\n")
	}
}

// configureLocked writes p to the device. Keepalives run only while the
// tunnel is up: without a session a keepalive starts a handshake, and only
// the initiator may do that (see the file comment).
func (w *WG) configureLocked(p *wgPeer) {
	if w.dev == nil {
		return // stopped meanwhile
	}
	keepalive := 0
	if p.up {
		keepalive = wgKeepalive
	}
	cfg := fmt.Sprintf("public_key=%s\nreplace_allowed_ips=true\nallowed_ip=%s/128\npersistent_keepalive_interval=%d\n",
		hex.EncodeToString(p.pub[:]), p.ip, keepalive)
	switch {
	case p.up:
		// Leave the endpoint alone: WireGuard roams to wherever the peer's
		// authenticated packets come from, which beats any stored address.
	case p.observed != "" && time.Since(p.obsAt) < observedFor:
		cfg += "endpoint=" + p.observed + "\n"
	case len(p.cands) > 0:
		cfg += "endpoint=" + p.cands[p.idx%len(p.cands)] + "\n"
	}
	if err := w.dev.IpcSet(cfg); err != nil {
		slog.Debug("wireguard: configuring peer", "err", err)
	}
}

// Dial opens a TCP stream to the peer's node through the tunnel, waiting up
// to WGDialTimeout for a handshake if there is no tunnel yet.
func (w *WG) Dial(ctx context.Context, addr string) (net.Conn, error) {
	if err := w.SetPeer(addr); err != nil {
		return nil, err
	}
	pub, _, _ := ParseWGAddr(addr)
	ctx, cancel := context.WithTimeout(ctx, WGDialTimeout)
	defer cancel()
	w.mu.Lock()
	tnet, p := w.tnet, w.peers[pub]
	w.mu.Unlock()
	if tnet == nil || p == nil {
		return nil, errors.New("wireguard: not started")
	}
	if len(p.cands) == 0 {
		return nil, errors.New("no direct path: no usable address")
	}
	// Punch first. The responder answers with a punch, which makes us (or,
	// if we are the responder, the peer) start the handshake through the
	// freshly opened NATs. If no answer comes (no NAT in the way, or a lost
	// packet), the initiator starts it anyway after a moment.
	w.punch(p)
	w.mu.Lock()
	start := w.initiator(p)
	w.mu.Unlock()
	if start {
		go func() {
			select {
			case <-ctx.Done():
			case <-time.After(1500 * time.Millisecond):
				if !w.fresh(pub) {
					w.poke(p)
				}
			}
		}()
	}
	for !w.fresh(pub) {
		select {
		case <-ctx.Done():
			return nil, errors.New("no direct path: the WireGuard handshake didn't complete (NAT not punched yet)")
		case <-time.After(100 * time.Millisecond):
		}
	}
	c, err := tnet.DialContextTCPAddrPort(ctx, netip.AddrPortFrom(p.ip, tunnelNodePort))
	if err != nil {
		return nil, fmt.Errorf("no direct path: %w", err)
	}
	return c, nil
}

// Live reports whether a tunnel to the peer at addr is up right now (a
// recent handshake). The node uses it to stop backing off from a path that
// has healed in the background.
func (w *WG) Live(addr string) bool {
	pub, _, err := ParseWGAddr(addr)
	return err == nil && w.fresh(pub)
}

// fresh reports whether the peer's last handshake is recent.
func (w *WG) fresh(pub [32]byte) bool {
	hs := w.handshakes()
	return time.Since(hs[pub]) < wgFresh
}

// handshakes reads every peer's last handshake time from the device.
func (w *WG) handshakes() map[[32]byte]time.Time {
	w.mu.Lock()
	dev := w.dev
	w.mu.Unlock()
	out := map[[32]byte]time.Time{}
	if dev == nil {
		return out
	}
	dump, err := dev.IpcGet()
	if err != nil {
		return out
	}
	var cur [32]byte
	sc := bufio.NewScanner(strings.NewReader(dump))
	for sc.Scan() {
		k, v, _ := strings.Cut(sc.Text(), "=")
		switch k {
		case "public_key":
			b, _ := hex.DecodeString(v)
			copy(cur[:], b)
		case "last_handshake_time_sec":
			if s, err := strconv.ParseInt(v, 10, 64); err == nil && s > 0 {
				out[cur] = time.Unix(s, 0)
			}
		}
	}
	return out
}

// punch sends a punch packet to each of p's candidates.
func (w *WG) punch(p *wgPeer) {
	w.mu.Lock()
	bind, pkt, cands := w.bind, punchPacket(w.pub), append([]string(nil), p.cands...)
	w.mu.Unlock()
	if bind == nil {
		return
	}
	for _, c := range cands {
		bind.sendRaw(pkt, c)
	}
}

// onPunch handles a punch packet. It is only a hint (anyone can send one),
// so all it does for a known peer without a tunnel is remember the source
// address, and then (at most once a second):
//   - the responding side punches back, so the initiator knows this NAT is
//     now open towards it;
//   - the initiating side starts the handshake right away, to where the
//     packet came from: a punch arriving proves the path works now.
//
// WireGuard itself authenticates whatever comes next.
func (w *WG) onPunch(from conn.Endpoint, pub [32]byte) {
	w.mu.Lock()
	p := w.peers[pub]
	if p == nil || w.dev == nil {
		w.mu.Unlock()
		return
	}
	src := from.DstToString()
	if netutil.DirectCandidate(src, w.allowLoopback) != nil || time.Since(p.lastAct) < time.Second {
		w.mu.Unlock()
		return
	}
	initiate := w.initiator(p)
	if p.up {
		// We think the tunnel is up, but peers punch only without one: its
		// side lost the session (it restarted, or moved networks). The
		// initiator drops the stale session so a new handshake starts now
		// instead of after WireGuard's 2-3 minute timers.
		if !initiate {
			// Answer, so the peer (which starts handshakes) knows the path
			// works and starts one now.
			p.lastAct = time.Now()
			bind, pkt := w.bind, punchPacket(w.pub)
			w.mu.Unlock()
			bind.sendRaw(pkt, src)
			return
		}
		if time.Since(p.lastRst) < resetEvery {
			w.mu.Unlock()
			return
		}
		p.lastRst, p.up = time.Now(), false
		w.dev.IpcSet("public_key=" + hex.EncodeToString(p.pub[:]) + "\nremove=true\n")
	}
	p.observed, p.obsAt, p.lastAct = src, time.Now(), time.Now()
	if initiate {
		w.configureLocked(p) // (re)adds it, endpoint: the observed address
	}
	bind, pkt := w.bind, punchPacket(w.pub)
	w.mu.Unlock()
	if initiate {
		go w.poke(p)
	} else {
		bind.sendRaw(pkt, src)
	}
}

// poke sends one packet into the tunnel towards p. With no session yet,
// WireGuard starts a handshake to the configured endpoint: the punch.
func (w *WG) poke(p *wgPeer) {
	w.mu.Lock()
	tnet := w.tnet
	w.mu.Unlock()
	if tnet == nil {
		return
	}
	c, err := tnet.DialUDPAddrPort(netip.AddrPort{}, netip.AddrPortFrom(p.ip, 9)) // discard port
	if err != nil {
		return
	}
	c.Write([]byte{0})
	c.Close()
}

// loop punches towards peers without a live tunnel (rotating through their
// candidates at WireGuard's retry pace, slowing down for peers that stay
// unreachable) and refreshes the STUN address.
func (w *WG) loop(stopped chan struct{}) {
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	nextSTUN := time.Time{}
	for {
		select {
		case <-stopped:
			return
		case <-tick.C:
		}
		if time.Now().After(nextSTUN) {
			go w.refreshSTUN()
			nextSTUN = time.Now().Add(2 * time.Minute)
		}
		hs := w.handshakes()
		now := time.Now()
		var punch, poke []*wgPeer
		w.mu.Lock()
		if w.stopped != stopped { // Stop ran while we read the handshakes
			w.mu.Unlock()
			return
		}
		for _, p := range w.peers {
			live := now.Sub(hs[p.pub]) < wgFresh
			if live {
				p.lastHS, p.misses = hs[p.pub], 0
			}
			if live != p.up { // turn keepalives on with the tunnel, off without
				p.up = live
				w.configureLocked(p)
			}
			if live || len(p.cands) == 0 || now.Before(p.nextTry) {
				continue
			}
			p.idx++
			if p.idx%len(p.cands) == 0 {
				p.misses++ // a full round without a handshake
			}
			wait := wgTryEvery
			if p.misses > 3 {
				wait = time.Minute // likely offline or unpunchable; Tor covers it
			}
			p.nextTry = now.Add(wait)
			punch = append(punch, p)
			// The first round only punches: the reply punch starts the
			// handshake through open NATs (onPunch). A blind handshake now
			// could hit a still-closed NAT and hold WireGuard's 5 s retry.
			// Later rounds also try blind, for peers whose punches can't
			// reach us but whose handshakes can.
			if w.initiator(p) && p.idx > 1 {
				w.configureLocked(p) // next candidate (or the observed address)
				poke = append(poke, p)
			}
		}
		w.mu.Unlock()
		for _, p := range punch {
			w.punch(p)
		}
		for _, p := range poke {
			w.poke(p)
		}
	}
}

// refreshSTUN learns the public address from the first STUN server that
// answers.
func (w *WG) refreshSTUN() {
	w.mu.Lock()
	bind, servers := w.bind, w.STUN
	w.mu.Unlock()
	if bind == nil || len(servers) == 0 || w.allowLoopback {
		return
	}
	for _, s := range resolveSTUN(servers) {
		ap, err := bind.Query(s, 2*time.Second)
		if err != nil {
			continue
		}
		w.mu.Lock()
		w.public = ap
		w.mu.Unlock()
		return
	}
}

// Status implements Transport. The address follows network changes.
func (w *WG) Status() Status {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := Status{Name: w.Name(), State: w.state, Detail: w.detail, Addresses: map[string]string{}}
	if a := w.addressLocked(); a != "" {
		s.Addresses["node"] = a
	}
	if s.State == StateReady {
		up := 0
		for _, p := range w.peers {
			if time.Since(p.lastHS) < wgFresh {
				up++
			}
		}
		pub := "no public address (STUN unreachable): same network, IPv6 and tailnet peers only"
		if w.public.IsValid() {
			pub = "public " + w.public.String()
		}
		s.Detail = fmt.Sprintf("WireGuard on UDP %d, %s; %d of %d peers connected directly", w.port, pub, up, len(w.peers))
		if w.note != "" {
			s.Detail += ". " + w.note
		}
	}
	return s
}

// Stop closes the device and every tunnel.
//
// The device is closed without holding w.mu: closing waits for
// wireguard-go's receive goroutine, which may itself be waiting for w.mu
// (a punch packet arriving now calls onPunch), so holding it would
// deadlock shutdown.
func (w *WG) Stop() error {
	w.mu.Lock()
	if w.stopped != nil {
		close(w.stopped)
		w.stopped = nil
	}
	ln, dev := w.ln, w.dev
	w.ln, w.dev, w.tnet, w.bind = nil, nil, nil, nil
	w.state = StateStopped
	w.mu.Unlock()
	if ln != nil {
		ln.Close()
	}
	if dev != nil {
		dev.Close()
	}
	return nil
}

func wgLogLevel() int {
	if os.Getenv("SOL_DEBUG_WG") != "" {
		return device.LogLevelVerbose
	}
	return device.LogLevelSilent
}
