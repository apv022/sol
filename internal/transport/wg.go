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
//     address on the WireGuard socket itself: from its paired peers (peer
//     reflection, stun.go) and, only if the user chose one, a STUN server.
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
//   - A tunnel is live only while the peer is heard from: both sides send a
//     keepalive every wgKeepalive seconds while it is up, so wgSilent
//     without a received byte means the path died silently (a phone that
//     changed networks or slept). The tunnel then counts as down, its
//     streams are closed (the node falls back to Tor) and punching resumes.

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
	wgTryEvery = 5 * time.Second
	// observedFor: a punch packet's source address is preferred for this long.
	observedFor = 30 * time.Second
	// publicEvery: how often the public address is re-learned; publicRetry
	// while there is none yet (a peer to ask may have just been paired).
	publicEvery = 2 * time.Minute
	publicRetry = 15 * time.Second
	// replyWithin: a reflection reply counts only this soon after our query.
	replyWithin = 5 * time.Second
)

// wgKeepalive (seconds) keeps NAT mappings open once a tunnel is up and
// proves the peer is still there; wgSilent without receiving anything means
// the tunnel is dead. Variables so tests can shorten them (SetWGLiveness).
var (
	wgKeepalive = 25
	wgSilent    = 80 * time.Second // three keepalives and a margin
)

// wgNetDebounce collapses a burst of network-change signals.
const wgNetDebounce = 2 * time.Second

// IsWG reports whether addr is a WireGuard direct address.
func IsWG(addr string) bool { return strings.HasPrefix(addr, wgScheme) }

// WG is the WireGuard direct transport.
type WG struct {
	ListenAddr string   // UDP, e.g. ":7701"
	Advertise  []string // replaces the advertised candidates (a forwarded port)
	STUN       []string // STUN servers the user chose (presets or host:port); empty means none
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
	public  netip.AddrPort       // from a peer's reflection reply or STUN
	pubFrom string               // "a paired peer" or "STUN", for Status
	asked   map[string]time.Time // reflection queries sent: endpoint -> when
	note    string               // shown in Status (e.g. a port fallback)
	peers   map[[32]byte]*wgPeer
	stopped chan struct{}
	conns   map[[32]byte]map[net.Conn]time.Time // open tunnel streams, per peer: when tracked
	netCh   chan struct{}                       // NetworkChanged signals for loop
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
	lastRfl  time.Time // last reflection reply sent to it
	rx       uint64    // bytes received from it (the device's counter)
	rxAt     time.Time // when rx last grew, or a handshake completed
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
		allowLoopback: netutil.IsLoopbackListen(listenAddr), state: StateStopped, peers: map[[32]byte]*wgPeer{},
		conns: map[[32]byte]map[net.Conn]time.Time{}, netCh: make(chan struct{}, 1)}
}

// NetworkChanged implements NetworkWatcher: after a network change the
// tunnels may be dead and this node's public address has likely changed, so
// it re-learns the address, punches towards every peer right away and pokes
// the live tunnels (an authenticated packet from the new address makes the
// peer's WireGuard roam to it). Bursts are debounced.
//
// It deliberately does not call device.BindUpdate: the bind is the default
// wildcard socket (0.0.0.0 and ::, never a specific address), which the
// kernel keeps valid across interface and route changes, and wireguard-go
// drops a cached source address by itself when a send fails with EINVAL. A
// rebind would only close the socket the peers already know the port of.
func (w *WG) NetworkChanged(string) {
	select {
	case w.netCh <- struct{}{}:
	default:
	}
}

// track registers a tunnel stream with peer pub, so a dead tunnel can cut
// it; the returned conn unregisters itself on Close.
func (w *WG) track(pub [32]byte, c net.Conn) net.Conn {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.conns[pub] == nil {
		w.conns[pub] = map[net.Conn]time.Time{}
	}
	tc := &wgConn{Conn: c, w: w, pub: pub}
	w.conns[pub][tc] = time.Now()
	return tc
}

// wgConn is a tunnel stream tracked per peer.
type wgConn struct {
	net.Conn
	w    *WG
	pub  [32]byte
	once sync.Once
}

func (c *wgConn) Close() error {
	c.once.Do(func() {
		c.w.mu.Lock()
		delete(c.w.conns[c.pub], c)
		c.w.mu.Unlock()
	})
	return c.Conn.Close()
}

// CloseWrite keeps half-close working through the wrapper.
func (c *wgConn) CloseWrite() error {
	if cw, ok := c.Conn.(interface{ CloseWrite() error }); ok {
		return cw.CloseWrite()
	}
	return c.Close()
}

// peerByIPLocked finds the peer at tunnel address ip. Called with w.mu held.
func (w *WG) peerByIPLocked(ip netip.Addr) (*wgPeer, bool) {
	for _, p := range w.peers {
		if p.ip == ip {
			return p, true
		}
	}
	return nil, false
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
	bind.onPunch, bind.onQuery, bind.onReply = w.onPunch, w.onQuery, w.onReply
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
			// Find the peer first. gVisor's RemoteAddr is nil once the stream
			// was reset, and an untracked stream would never be cut when its
			// tunnel dies, so a stream that can't be tied to a peer is dropped.
			ra := c.RemoteAddr()
			if ra == nil {
				c.Close()
				return
			}
			ap, err := netip.ParseAddrPort(ra.String())
			if err != nil {
				slog.Debug("wireguard: stream from an unparsable address", "addr", ra.String())
				c.Close()
				return
			}
			w.mu.Lock()
			p, ok := w.peerByIPLocked(ap.Addr().Unmap())
			w.mu.Unlock()
			if !ok {
				slog.Debug("wireguard: stream from an address that is no peer's", "addr", ap)
				c.Close()
				return
			}
			out, err := net.DialTimeout("tcp", target, 5*time.Second)
			if err != nil {
				slog.Debug("wireguard: node listener unreachable", "err", err)
				c.Close()
				return
			}
			netutil.Pipe(w.track(p.pub, c), out)
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
	// A tunnel that just came up is fresh before loop marks it up. Punching
	// then would make an initiating peer drop the session it just made
	// (onPunch), and reconfiguring would reset the endpoint it roamed to.
	fresh := w.fresh(pub)
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
	if w.dev != nil && (changed || (!p.up && !fresh)) {
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
	delete(w.peers, pub)
	if w.dev != nil {
		w.dev.IpcSet("public_key=" + hex.EncodeToString(pub[:]) + "\nremove=true\n")
	}
	var cut []net.Conn
	for c := range w.conns[pub] {
		cut = append(cut, c)
	}
	w.mu.Unlock()
	for _, c := range cut {
		c.Close() // without a peer the stream could only hang
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
// to WGDialTimeout for a handshake if there is no tunnel yet, and up to
// WGDialTimeout more for the stream itself.
func (w *WG) Dial(ctx context.Context, addr string) (net.Conn, error) {
	if err := w.SetPeer(addr); err != nil {
		return nil, err
	}
	pub, _, _ := ParseWGAddr(addr)
	parent := ctx
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
	// Without a tunnel, punch first. The responder answers with a punch,
	// which makes us (or, if we are the responder, the peer) start the
	// handshake through the freshly opened NATs. If no answer comes (no NAT
	// in the way, or a lost packet), the initiator starts it anyway after a
	// moment. Never punch through a live tunnel: an initiating peer takes a
	// punch to mean we lost the session and resets its own (onPunch), which
	// would stall this very dial.
	fresh := w.fresh(pub)
	if !fresh {
		w.punch(p)
	}
	w.mu.Lock()
	start := w.initiator(p)
	w.mu.Unlock()
	if start && !fresh {
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
	// The stream gets a budget of its own, not what the handshake left: a
	// handshake that just completed can leave a fraction of a second, and
	// the first SYN, sent while the session settles, may need tcp's 1 s
	// retransmit (seen on slower machines, which then fell back for no
	// reason).
	tctx, tcancel := context.WithTimeout(parent, WGDialTimeout)
	defer tcancel()
	c, err := tnet.DialContextTCPAddrPort(tctx, netip.AddrPortFrom(p.ip, tunnelNodePort))
	if err != nil {
		return nil, fmt.Errorf("no direct path: %w", err)
	}
	return w.track(pub, c), nil
}

// Live reports whether a tunnel to the peer at addr is up right now (a
// recent handshake). The node uses it to stop backing off from a path that
// has healed in the background.
func (w *WG) Live(addr string) bool {
	pub, _, err := ParseWGAddr(addr)
	return err == nil && w.fresh(pub)
}

// fresh reports whether the peer's last handshake is recent and the
// tunnel hasn't gone silent since.
func (w *WG) fresh(pub [32]byte) bool {
	st := w.stats()[pub]
	if time.Since(st.hs) >= wgFresh {
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	p := w.peers[pub]
	// Silent: nothing received for wgSilent, and no handshake since.
	return p == nil || p.rxAt.IsZero() || st.hs.After(p.rxAt) || time.Since(p.rxAt) < wgSilent
}

// wgStats is what the device reports about one peer.
type wgStats struct {
	hs time.Time // last handshake
	rx uint64    // bytes received
}

// stats reads every peer's last handshake and received bytes.
func (w *WG) stats() map[[32]byte]wgStats {
	w.mu.Lock()
	dev := w.dev
	w.mu.Unlock()
	out := map[[32]byte]wgStats{}
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
				st := out[cur]
				st.hs = time.Unix(s, 0)
				out[cur] = st
			}
		case "last_handshake_time_nsec":
			if ns, err := strconv.ParseInt(v, 10, 64); err == nil && ns > 0 {
				if st := out[cur]; !st.hs.IsZero() {
					st.hs = st.hs.Add(time.Duration(ns))
					out[cur] = st
				}
			}
		case "rx_bytes":
			if b, err := strconv.ParseUint(v, 10, 64); err == nil {
				st := out[cur]
				st.rx = b
				out[cur] = st
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
	var netAt time.Time // a network change is pending since (debounce)
	for {
		select {
		case <-stopped:
			return
		case <-w.netCh:
			if netAt.IsZero() {
				netAt = time.Now()
			}
			continue
		case <-tick.C:
		}
		if !netAt.IsZero() && time.Since(netAt) >= wgNetDebounce {
			netAt = time.Time{}
			nextSTUN = time.Time{} // our public address has likely changed
			var live []*wgPeer
			w.mu.Lock()
			for _, p := range w.peers {
				p.nextTry, p.misses = time.Time{}, 0 // punch now
				if p.up {
					live = append(live, p)
				}
			}
			w.mu.Unlock()
			for _, p := range live {
				w.poke(p)
			}
		}
		if time.Now().After(nextSTUN) {
			go w.refreshPublic()
			w.mu.Lock()
			known := w.public.IsValid()
			w.mu.Unlock()
			nextSTUN = time.Now().Add(publicRetry)
			if known {
				nextSTUN = time.Now().Add(publicEvery)
			}
		}
		statsAt := time.Now()
		stats := w.stats()
		now := time.Now()
		var punch, poke []*wgPeer
		var cut []net.Conn
		w.mu.Lock()
		if w.stopped != stopped { // Stop ran while we read the handshakes
			w.mu.Unlock()
			return
		}
		for _, p := range w.peers {
			st := stats[p.pub]
			if st.rx > p.rx || st.hs.After(p.rxAt) {
				p.rxAt = now // heard from it
			}
			p.rx = st.rx // drops to 0 when the device forgets the peer: not "heard"
			live := now.Sub(st.hs) < wgFresh && now.Sub(p.rxAt) < wgSilent
			if live {
				p.lastHS, p.misses = st.hs, 0
			}
			silent := !p.rxAt.IsZero() && now.Sub(p.rxAt) >= wgSilent
			if live != p.up { // turn keepalives on with the tunnel, off without
				if p.up && silent {
					slog.Debug("wireguard: tunnel went silent", "since", p.rxAt)
					p.lastHS = time.Time{}
				}
				p.up = live
				w.configureLocked(p)
			}
			if !live && silent {
				// Nothing heard for wgSilent: the path died. Its streams
				// would hang; cut them so the node falls back. This holds
				// however the tunnel stopped counting as up (silence seen
				// by this loop, or a session reset in onPunch), so no
				// stream outlives a dead tunnel. A stream tracked after
				// the stats were read belongs to a handshake they missed.
				for c, at := range w.conns[p.pub] {
					if at.Before(statsAt) {
						cut = append(cut, c)
					}
				}
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
		for _, c := range cut {
			c.Close()
		}
		for _, p := range punch {
			w.punch(p)
		}
		for _, p := range poke {
			w.poke(p)
		}
	}
}

// refreshPublic re-learns the public address: it asks every paired peer
// (their answers arrive in onReply) and, if the user chose any, the STUN
// servers.
func (w *WG) refreshPublic() {
	w.askPeers()
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
		w.public, w.pubFrom = ap, "STUN"
		w.mu.Unlock()
		return
	}
}

// askPeers sends a reflection query to each paired peer's candidates.
func (w *WG) askPeers() {
	w.mu.Lock()
	bind := w.bind
	if bind == nil {
		w.mu.Unlock()
		return
	}
	pkt := queryPacket(w.pub)
	now := time.Now()
	if w.asked == nil {
		w.asked = map[string]time.Time{}
	}
	for k, t := range w.asked {
		if now.Sub(t) > replyWithin {
			delete(w.asked, k)
		}
	}
	var to []string
	for _, p := range w.peers {
		for _, c := range p.cands {
			w.asked[endpointKey(c)] = now
			to = append(to, c)
		}
	}
	w.mu.Unlock()
	for _, c := range to {
		bind.sendRaw(pkt, c)
	}
}

// onQuery answers a paired peer's reflection query with the address its
// packet came from. Unknown keys get nothing (the port stays silent), and
// each peer gets at most one answer a second.
func (w *WG) onQuery(from conn.Endpoint, pub [32]byte) {
	src, err := netip.ParseAddrPort(from.DstToString())
	if err != nil {
		return
	}
	w.mu.Lock()
	p := w.peers[pub]
	if p == nil || w.bind == nil || time.Since(p.lastRfl) < time.Second {
		w.mu.Unlock()
		return
	}
	p.lastRfl = time.Now()
	bind := w.bind
	w.mu.Unlock()
	bind.sendRaw(replyPacket(src), src.String())
}

// onReply takes a peer's report of our public address, if it answers a
// query we just sent to that address and is a public address (on a
// loopback test setup, any address).
func (w *WG) onReply(from conn.Endpoint, ap netip.AddrPort) {
	w.mu.Lock()
	defer w.mu.Unlock()
	key := endpointKey(from.DstToString())
	at, ok := w.asked[key]
	if !ok || time.Since(at) > replyWithin {
		return
	}
	delete(w.asked, key)
	if !w.allowLoopback && !isPublicAddr(ap.Addr()) {
		return // a peer on our own network sees our LAN address: no use to others
	}
	w.public, w.pubFrom = ap, "a paired peer"
}

// cgnat is the carrier-grade NAT range (RFC 6598): not reachable from
// outside, so never a public address.
var cgnat = netip.MustParsePrefix("100.64.0.0/10")

func isPublicAddr(a netip.Addr) bool {
	return a.IsGlobalUnicast() && !a.IsPrivate() && !cgnat.Contains(a)
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
		pub := "no public address yet (no paired peer has reported one"
		if len(w.STUN) == 0 {
			pub += ", and no STUN server is chosen"
		}
		pub += "): same network, IPv6 and tailnet peers only"
		if w.public.IsValid() {
			pub = "public " + w.public.String() + " (from " + w.pubFrom + ")"
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

// SetWGLiveness shortens the tunnel keepalive (seconds) and the silence
// after which a tunnel counts as dead, for a test.
func SetWGLiveness(t interface{ Cleanup(func()) }, keepaliveSec int, silent time.Duration) {
	oldK, oldS := wgKeepalive, wgSilent
	wgKeepalive, wgSilent = keepaliveSec, silent
	t.Cleanup(func() { wgKeepalive, wgSilent = oldK, oldS })
}
