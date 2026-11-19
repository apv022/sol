package transport

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"time"
)

// Reaching an onion: why Dial does more than a SOCKS connect.
//
// Before tor can connect to an onion it fetches the onion's descriptor from
// one of the directories responsible for it. Measured on the live network
// (tor 0.4.9), that fetch either answers in 3 to 5 seconds or hangs: the
// circuit to that one directory stalls, and tor waits with nothing to show
// until SocksTimeout (120 s by default) expires the fetch. Any further
// SOCKS request for the same onion meanwhile only joins the hung fetch
// ("Already a pending directory request"). Two of the first four lookups
// in one test run hung like that, so a first contact often cost a full
// two minutes per attempt.
//
// So Dial hedges: when no descriptor for the address has arrived within
// hsFetchAfter, it asks tor over the control port to fetch it again
// (HSFETCH). Tor sends that request to another directory (it does not
// re-ask one it asked in the last 15 minutes), and the descriptor that
// arrives first serves the waiting connection. HSFETCH also replaces a
// stale cached descriptor, as after a peer re-published its onion with
// new client keys. And the torrc lowers SocksTimeout (torSocksTimeout), so
// a connection that still gets nowhere fails, and is retried, sooner.
var (
	// hsFetchAfter: a dial with no descriptor for its onion after this long
	// asks tor for another fetch (HSFETCH); 0 turns hedging off. A healthy
	// fetch takes 3 to 5 s.
	hsFetchAfter = 8 * time.Second
	// hsFetchEvery spaces further HSFETCHes while the dial still waits.
	hsFetchEvery = 10 * time.Second
	// hsFetchMax bounds the HSFETCHes per dial.
	hsFetchMax = 3
	// torSocksTimeout is tor's SocksTimeout: how long a connection (or a
	// descriptor fetch) may wait for a circuit before tor gives up. A
	// complete first contact took 9 to 14 s in measurements; the default
	// 120 s mostly measured hung directory fetches.
	torSocksTimeout = 45 * time.Second
)

// dialOnion connects to an onion through SOCKS, hedging the descriptor fetch
// as described above.
func (t *Tor) dialOnion(ctx context.Context, socks, addr string) (net.Conn, error) {
	sid := onionServiceID(addr)
	if hsFetchAfter <= 0 || len(sid) != 56 {
		return dialSOCKS5(ctx, socks, addr)
	}
	type result struct {
		conn net.Conn
		err  error
	}
	done := make(chan result, 1)
	started := time.Now()
	go func() {
		c, err := dialSOCKS5(ctx, socks, addr)
		done <- result{c, err}
	}()
	timer := time.NewTimer(hsFetchAfter)
	defer timer.Stop()
	for fetches := 0; ; {
		select {
		case r := <-done:
			return r.conn, r.err
		case <-timer.C:
			if fetches < hsFetchMax && !t.descSince(sid, started) {
				fetches++
				t.hsFetch(sid)
			}
			timer.Reset(hsFetchEvery)
		}
	}
}

// descSince reports whether tor received a descriptor for sid at or after
// since (an HS_DESC RECEIVED event).
func (t *Tor) descSince(sid string, since time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	at, ok := t.descGot[sid]
	return ok && !at.Before(since)
}

// hsFetch asks tor to fetch sid's descriptor again.
func (t *Tor) hsFetch(sid string) {
	t.mu.Lock()
	ctrl := t.ctrl
	t.mu.Unlock()
	if ctrl == nil {
		return
	}
	if _, err := ctrl.do("HSFETCH " + sid); err != nil {
		slog.Debug("tor: HSFETCH", "err", err)
		return
	}
	slog.Debug("tor: descriptor fetch is slow; asked another directory")
}

// noteDescReceived records an "HS_DESC RECEIVED <address> ..." event: tor
// has a fresh descriptor for that onion.
func (t *Tor) noteDescReceived(f []string) {
	if len(f) < 4 || f[2] != "RECEIVED" {
		return
	}
	sid := strings.ToLower(f[3])
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.descGot == nil {
		t.descGot = map[string]time.Time{}
	}
	if len(t.descGot) > 1024 { // only recent lookups matter
		clear(t.descGot)
	}
	t.descGot[sid] = time.Now()
}
