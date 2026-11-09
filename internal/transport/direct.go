package transport

// Direct mode, first rung of the ladder in docs/spec.md 10.1: peers connect
// straight to this machine's IP addresses (same LAN, IPv6, a tailnet, a
// public IP) when they can, and over Tor when they can't.
//
// Wire: the dialer sends the 5-byte preamble "SOLD1", then a TLS 1.3
// handshake, then the usual Sol handshake (internal/protocol) inside TLS.
//
// Invariants:
//   - The server certificate is self-signed with the node's own ed25519 key,
//     and the dialer accepts only the exact key named in the address
//     ("sol-direct:<node id>@host:port,..."). There is no CA and nothing to
//     trust on first use: a man in the middle can't complete the handshake.
//   - Anything that doesn't send the preamble gets no bytes back (port
//     scanners and HTTP probes see a silent port, like the node listener).
//   - Only the node publication ("node") is carried. Public and anonymous
//     services are never published here.
//   - Direct mode shows peers this machine's IP addresses, so it is opt-in
//     (config transports.direct).

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
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
	directScheme   = "sol-direct:"
	directPreamble = "SOLD1"
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

	key    crypto.Signer // the node key (ed25519)
	nodeID string
	cert   tls.Certificate

	mu     sync.Mutex
	state  State
	detail string
	ln     net.Listener
	target string
}

// NewDirect constructs the transport for the node key (identity.Signer).
func NewDirect(listenAddr string, key crypto.Signer, advertise ...string) *Direct {
	return &Direct{ListenAddr: listenAddr, Advertise: advertise, key: key,
		nodeID: identity.NodeID(key.Public().(ed25519.PublicKey)), state: StateStopped}
}

// SetAdvertise replaces the advertised candidates (nil: this machine's
// addresses). Peers pick up the change at their next refresh.
func (d *Direct) SetAdvertise(cands []string) {
	d.mu.Lock()
	d.Advertise = cands
	d.mu.Unlock()
}

// Name implements Transport.
func (d *Direct) Name() string { return "direct" }

// Start makes the certificate; the listener opens on Publish("node").
func (d *Direct) Start(ctx context.Context) error {
	cert, err := selfSigned(d.key)
	d.mu.Lock()
	defer d.mu.Unlock()
	if err != nil {
		d.state, d.detail = StateFailed, err.Error()
		return err
	}
	d.cert = cert
	d.state = StateReady
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
	if d.state != StateReady {
		return "", errors.New("direct: not started")
	}
	d.target = p.Target
	if d.ln == nil {
		ln, err := net.Listen("tcp", d.ListenAddr)
		if err != nil {
			d.detail = "can't listen on " + d.ListenAddr + ": " + err.Error()
			return "", fmt.Errorf("direct listen %s: %w", d.ListenAddr, err)
		}
		d.ln = ln
		go d.serve(ln)
	}
	return d.addressLocked(), nil
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
		cands = candidates(la.Port)
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

// candidates lists host:port for every address a peer might reach: private
// and public IPv4, global IPv6 (no loopback, no link-local).
func candidates(port int) []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var v4, v6 []string
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
			if ip.To4() != nil {
				v4 = append(v4, hp)
			} else if ip.IsGlobalUnicast() {
				v6 = append(v6, hp)
			}
		}
	}
	out := append(v4, v6...)
	if len(out) > directMaxCands {
		out = out[:directMaxCands]
	}
	return out
}

func (d *Direct) serve(ln net.Listener) {
	for {
		c, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		go d.handle(c)
	}
}

func (d *Direct) handle(c net.Conn) {
	c.SetDeadline(time.Now().Add(10 * time.Second))
	pre := make([]byte, len(directPreamble))
	if _, err := io.ReadFull(c, pre); err != nil || !bytes.Equal(pre, []byte(directPreamble)) {
		c.Close() // not a Sol node: send nothing
		return
	}
	d.mu.Lock()
	cert, target := d.cert, d.target
	d.mu.Unlock()
	tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13})
	if err := tc.Handshake(); err != nil {
		tc.Close()
		return
	}
	c.SetDeadline(time.Time{})
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
	if d.ln != nil {
		d.ln.Close()
		d.ln = nil
	}
	return nil
}

// Handles implements Transport.
func (d *Direct) Handles(addr string) bool { return strings.HasPrefix(addr, directScheme) }

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
func (d *Direct) Dial(ctx context.Context, addr string) (net.Conn, error) {
	pub, cands, err := ParseDirectAddr(addr)
	if err != nil {
		return nil, err
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
	if _, err := raw.Write([]byte(directPreamble)); err != nil {
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
	d.state = StateStopped
	return nil
}
