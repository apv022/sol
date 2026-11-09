package transport

// Direct mode, first rung of the ladder in docs/spec.md 10.1: peers connect
// straight to this machine's IP addresses (same LAN, IPv6, a tailnet, a
// public IP) when they can, and over Tor when they can't.
//
// Wire: the dialer sends the preamble "SOLD1" + tag (16 bytes,
// SHA-256("sol direct" | node public key)), then a TLS 1.3 handshake, then
// the usual Sol handshake (internal/protocol) inside TLS. The tag proves the
// dialer already knows which node it wants, so the listener never shows its
// certificate (the node's public key) to anyone who doesn't.
//
// Invariants:
//   - The server certificate is self-signed with the node's own ed25519 key,
//     and the dialer accepts only the exact key named in the address
//     ("sol-direct:<node id>@host:port,..."). There is no CA and nothing to
//     trust on first use: a man in the middle can't complete the handshake.
//   - Anything that doesn't send this node's preamble gets no bytes back
//     (port scanners and HTTP probes see a silent port, like the node
//     listener), and can't learn the node's key.
//   - A port that can't be opened is retried until it can.
//   - Only the node publication ("node") is carried. Public and anonymous
//     services are never published here.
//   - Direct mode shows peers this machine's IP addresses, so it is opt-in
//     (config transports.direct).

import (
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/netutil"
)

const (
	directScheme  = "sol-direct:"
	directMagic   = "SOLD1"
	directTagSize = 16
	// DirectDialTimeout bounds one direct attempt (all candidates in
	// parallel). It is short on purpose: Tor is the fallback.
	DirectDialTimeout = 3 * time.Second
	directMaxCands    = 8
)

// Direct is the direct-mode transport.
type Direct struct {
	ListenAddr string // e.g. ":7701"
	// Advertise overrides the advertised host:port candidates (tests, or a
	// port forwarded on the router). Empty means this machine's addresses.
	Advertise []string

	key      crypto.Signer // the node key (ed25519)
	nodeID   string
	preamble []byte      // what dialers must send first (see the file comment)
	srvTLS   *tls.Config // built once in Start

	// allowLoopback lets candidates be loopback addresses; only for a node
	// whose own direct listener is on loopback (tests, single machine).
	allowLoopback bool
	retryEvery    time.Duration // retryListenEvery at construction

	mu       sync.Mutex
	state    State
	detail   string
	ln       net.Listener
	target   string
	retrying bool
	cands    []string // cached candidates, refreshed every candidateTTL
	candsAt  time.Time
	stopped  chan struct{}
}

// candidateTTL is how long the interface scan is cached. Status is called
// on every dial and panel refresh; addresses change rarely.
const candidateTTL = 30 * time.Second

// retryListenEvery is how often a busy direct port is tried again.
var retryListenEvery = 10 * time.Second

func preambleFor(pub ed25519.PublicKey) []byte {
	h := sha256.Sum256(append([]byte("sol direct"), pub...))
	return append([]byte(directMagic), h[:directTagSize]...)
}

// NewDirect constructs the transport for the node key (identity.Signer).
func NewDirect(listenAddr string, key crypto.Signer, advertise ...string) *Direct {
	pub := key.Public().(ed25519.PublicKey)
	return &Direct{ListenAddr: listenAddr, Advertise: advertise, key: key,
		nodeID: identity.NodeID(pub), preamble: preambleFor(pub), state: StateStopped,
		allowLoopback: netutil.IsLoopbackListen(listenAddr), retryEvery: retryListenEvery}
}

// SetAdvertise replaces the advertised candidates (nil: this machine's
// addresses). Peers pick up the change at their next refresh.
func (d *Direct) SetAdvertise(cands []string) {
	d.mu.Lock()
	d.Advertise = cands
	d.candsAt = time.Time{}
	d.mu.Unlock()
}

// Name implements Transport.
func (d *Direct) Name() string { return "direct" }

// Start makes the certificate; the listener opens on Publish("node").
// Starting a started transport is a no-op (a running retry loop keeps going).
func (d *Direct) Start(ctx context.Context) error {
	d.mu.Lock()
	running := d.stopped != nil
	d.mu.Unlock()
	if running {
		return nil
	}
	// The certificate is made outside the lock (crypto/rand could block at
	// early boot, and Status must stay responsive), then checked again.
	cert, err := selfSigned(d.key)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped != nil {
		return nil // a concurrent Start finished first
	}
	if err != nil {
		d.state, d.detail = StateFailed, err.Error()
		return err
	}
	d.srvTLS = &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}
	d.stopped = make(chan struct{}) // non-nil exactly while started
	d.state, d.retrying = StateReady, false
	return nil
}

func selfSigned(priv crypto.Signer) (tls.Certificate, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 64))
	if err != nil {
		return tls.Certificate{}, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().AddDate(10, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"sol"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, priv.Public(), priv)
	if err != nil {
		return tls.Certificate{}, err
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: priv}, nil
}

// Publish opens the direct listener for the node publication. Other
// publications are refused: they must never be reachable directly.
func (d *Direct) Publish(ctx context.Context, p Publication) (string, error) {
	if p.ID != "node" {
		return "", fmt.Errorf("direct mode carries only the node itself, not %q: %w", p.ID, ErrNotImplemented)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stopped == nil {
		return "", errors.New("direct: not started")
	}
	d.target = p.Target // also while the port is still being retried
	if d.ln == nil && !d.listenLocked() {
		if !d.retrying {
			d.retrying = true
			go d.retryListen(d.stopped)
		}
		return "", errors.New("direct: " + d.detail)
	}
	return d.addressLocked(), nil
}

func (d *Direct) listenLocked() bool {
	ln, err := net.Listen("tcp", d.ListenAddr)
	if err != nil {
		d.state, d.detail = StateStarting, "can't listen on "+d.ListenAddr+" ("+err.Error()+"); retrying"
		return false
	}
	d.ln, d.state, d.detail, d.candsAt = ln, StateReady, "", time.Time{}
	go d.serve(ln)
	return true
}

// retryListen keeps trying the port until it opens or the transport stops.
// stopped is the channel of the Start it belongs to: after a Stop/Start
// cycle an old loop exits and can't touch the new one's state.
// Peers learn the new address at their next refresh.
func (d *Direct) retryListen(stopped chan struct{}) {
	tick := time.NewTicker(d.retryEvery)
	defer tick.Stop()
	for {
		select {
		case <-stopped:
			return
		case <-tick.C:
		}
		d.mu.Lock()
		if d.stopped != stopped { // stopped and started again meanwhile
			d.mu.Unlock()
			return
		}
		done := d.ln != nil || d.target == "" || d.listenLocked() // target "": unpublished
		if done {
			d.retrying = false
		}
		d.mu.Unlock()
		if done {
			return
		}
	}
}

// addressLocked builds "sol-direct:<node id>@cand,cand,...".
func (d *Direct) addressLocked() string {
	if d.ln == nil {
		return ""
	}
	la := d.ln.Addr().(*net.TCPAddr)
	cands := d.Advertise
	switch {
	case len(cands) > 0:
	case la.IP.IsLoopback(): // tests, or a node only meant for this machine
		cands = []string{la.String()}
	default:
		if time.Since(d.candsAt) > candidateTTL {
			d.cands, d.candsAt = candidates(la, d.ListenAddr), time.Now()
		}
		cands = d.cands
	}
	if len(cands) == 0 {
		return ""
	}
	return directScheme + d.nodeID + "@" + strings.Join(cands, ",")
}

// skipIface names virtual interfaces whose addresses a peer can't use
// (container and VM bridges on this machine).
func skipIface(name string) bool {
	for _, p := range []string{"docker", "br-", "veth", "virbr", "podman", "cni", "flannel", "lxc", "vmnet", "vboxnet"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// candidates lists host:port for every address a peer might reach (no
// loopback, no link-local), most useful first: private IPv4 (same LAN),
// then global IPv6, then other IPv4 (tailnet, public), so the cap doesn't
// push out a working IPv6 address.
//
// Only addresses the listener actually accepts are listed: a specific bind
// IP is advertised alone, and "0.0.0.0" means IPv4 only.
func candidates(la *net.TCPAddr, listen string) []string {
	port := la.Port
	if !la.IP.IsUnspecified() {
		return []string{la.String()}
	}
	host, _, _ := net.SplitHostPort(listen)
	v4only := host == "0.0.0.0"
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var lan, v6, v4 []string
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 || skipIface(ifc.Name) {
			continue
		}
		addrs, _ := ifc.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipn.IP
			if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsMulticast() || ip.IsUnspecified() {
				continue
			}
			hp := net.JoinHostPort(ip.String(), strconv.Itoa(port))
			switch {
			case ip.To4() != nil && ip.IsPrivate():
				lan = append(lan, hp)
			case ip.To4() != nil:
				v4 = append(v4, hp)
			case ip.IsGlobalUnicast() && !v4only:
				v6 = append(v6, hp)
			}
		}
	}
	out := append(append(lan, v6...), v4...)
	if len(out) > directMaxCands {
		out = out[:directMaxCands]
	}
	return out
}

// maxPreAuth caps connections still in the preamble or TLS handshake. The
// port can be public, so without a cap anyone could hold thousands of idle
// sockets for 10 s each and exhaust the node's file descriptors.
const maxPreAuth = 64

func (d *Direct) serve(ln net.Listener) {
	slots := make(chan struct{}, maxPreAuth)
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		select {
		case slots <- struct{}{}:
			go d.handle(c, func() { <-slots })
		default:
			c.Close() // too many unauthenticated connections: drop, send nothing
		}
	}
}

// handle runs one inbound connection; done releases its pre-auth slot once
// the TLS handshake is over (or the connection is dropped).
func (d *Direct) handle(c net.Conn, done func()) {
	released := false
	release := func() {
		if !released {
			released = true
			done()
		}
	}
	defer release()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	pre := make([]byte, len(d.preamble))
	if _, err := io.ReadFull(c, pre); err != nil || subtle.ConstantTimeCompare(pre, d.preamble) != 1 {
		c.Close() // not someone looking for this node: send nothing
		return
	}
	d.mu.Lock()
	target, cfg := d.target, d.srvTLS
	d.mu.Unlock()
	if target == "" || cfg == nil {
		c.Close() // unpublished or stopped meanwhile
		return
	}
	tc := tls.Server(c, cfg)
	if err := tc.Handshake(); err != nil {
		tc.Close()
		return
	}
	c.SetDeadline(time.Time{})
	release()
	out, err := net.DialTimeout("tcp", target, 5*time.Second)
	if err != nil {
		slog.Debug("direct: node listener unreachable", "err", err)
		tc.Close()
		return
	}
	netutil.Pipe(tc, out)
}

// Unpublish closes the listener when the node publication is removed.
func (d *Direct) Unpublish(id string) error {
	if id != "node" {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.target = "" // also tells a running retry loop to stop
	if d.ln != nil {
		d.ln.Close()
		d.ln = nil
	}
	return nil
}

// Handles implements Transport.
// Only while started: a stopped direct transport can't dial anything.
func (d *Direct) Handles(addr string) bool {
	d.mu.Lock()
	started := d.stopped != nil
	d.mu.Unlock()
	return started && IsDirect(addr)
}

// IsDirect reports whether addr is a direct-mode address.
func IsDirect(addr string) bool { return strings.HasPrefix(addr, directScheme) }

// ParseDirectAddr splits "sol-direct:<node id>@a,b" into the node key and
// the candidates.
func ParseDirectAddr(addr string) (ed25519.PublicKey, []string, error) {
	rest, ok := strings.CutPrefix(addr, directScheme)
	if !ok {
		return nil, nil, fmt.Errorf("not a direct address: %q", addr)
	}
	id, list, ok := strings.Cut(rest, "@")
	if !ok || list == "" {
		return nil, nil, fmt.Errorf("bad direct address %q", addr)
	}
	pub, err := identity.ParseNodeID(id)
	if err != nil {
		return nil, nil, err
	}
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
		return nil, nil, fmt.Errorf("bad direct address %q", addr)
	}
	return pub, cands, nil
}

// Dial tries every candidate at once and returns the first TLS connection
// whose certificate key is the expected node key.
//
// Candidates come from a peer, so only plain IP addresses that could be
// another machine are dialled: no hostnames, loopback (unless this node is
// itself on loopback), unspecified, multicast or link-local addresses.
func (d *Direct) Dial(ctx context.Context, addr string) (net.Conn, error) {
	pub, all, err := ParseDirectAddr(addr)
	if err != nil {
		return nil, err
	}
	var cands []string
	for _, c := range all {
		if netutil.DirectCandidate(c, d.allowLoopback) == nil {
			cands = append(cands, c)
		}
	}
	if len(cands) == 0 {
		return nil, errors.New("no direct path: no usable address")
	}
	ctx, cancel := context.WithTimeout(ctx, DirectDialTimeout)
	defer cancel()
	type result struct {
		c   net.Conn
		err error
	}
	results := make(chan result, len(cands))
	for _, cand := range cands {
		go func() {
			c, err := dialPinned(ctx, cand, pub)
			results <- result{c, err}
		}()
	}
	var errs []error
	var won net.Conn
	for range cands {
		r := <-results
		switch {
		case r.err != nil:
			errs = append(errs, r.err)
		case won == nil:
			won = r.c
			cancel() // stop the others
		default:
			r.c.Close() // a slower candidate also made it
		}
	}
	if won != nil {
		return won, nil
	}
	return nil, fmt.Errorf("no direct path: %w", errors.Join(errs...))
}

func dialPinned(ctx context.Context, addr string, pub ed25519.PublicKey) (net.Conn, error) {
	var nd net.Dialer
	raw, err := nd.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		raw.SetDeadline(dl)
	}
	if _, err := raw.Write(preambleFor(pub)); err != nil {
		raw.Close()
		return nil, err
	}
	tc := tls.Client(raw, &tls.Config{
		MinVersion: tls.VersionTLS13,
		ServerName: "sol",
		// The pin below replaces CA verification: only the expected node key passes.
		InsecureSkipVerify: true,
		VerifyPeerCertificate: func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
			if len(rawCerts) != 1 {
				return errors.New("direct: expected exactly one certificate")
			}
			cert, err := x509.ParseCertificate(rawCerts[0])
			if err != nil {
				return err
			}
			got, ok := cert.PublicKey.(ed25519.PublicKey)
			if !ok || !got.Equal(pub) {
				return errors.New("direct: that address belongs to a different node")
			}
			// TLS 1.3's CertificateVerify, checked by crypto/tls after this,
			// proves the server holds this key.
			return nil
		},
	})
	if err := tc.HandshakeContext(ctx); err != nil {
		raw.Close()
		return nil, err
	}
	raw.SetDeadline(time.Time{})
	return tc, nil
}

// Status implements Transport. The address is recomputed each time, so it
// follows network changes (a new Wi-Fi, a new DHCP lease).
func (d *Direct) Status() Status {
	d.mu.Lock()
	defer d.mu.Unlock()
	s := Status{Name: d.Name(), State: d.state, Detail: d.detail, Addresses: map[string]string{}}
	if a := d.addressLocked(); a != "" {
		s.Addresses["node"] = a
		if s.State == StateReady {
			_, cands, _ := strings.Cut(strings.TrimPrefix(a, directScheme), "@")
			s.Detail = "peers connect straight to " + cands + " (they can see these addresses)"
		}
	}
	return s
}

// Stop closes the listener.
func (d *Direct) Stop() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.ln != nil {
		d.ln.Close()
		d.ln = nil
	}
	if d.stopped != nil {
		close(d.stopped)
		d.stopped = nil
	}
	d.state, d.retrying = StateStopped, false
	return nil
}

// SetRetryListenEvery shortens the busy-port retry interval for direct
// transports created after the call (tests in other packages).
func SetRetryListenEvery(t interface{ Cleanup(func()) }, d time.Duration) {
	old := retryListenEvery
	retryListenEvery = d
	t.Cleanup(func() { retryListenEvery = old })
}
