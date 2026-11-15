package transport

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/netutil"
	"github.com/apv022/sol/internal/runtime"
)

// onionVirtualPort is the port every Sol onion listens on. Port 80 means a
// public HTTP service opens in Tor Browser as http://<onion>.onion/.
const onionVirtualPort = 80

// Tor runs a private tor process and manages onion services through its
// control port. A node uses it both ways: Publish for its own onion, and
// Dial + AuthorizeClient to reach its peers' onions.
//
// Onion keys are stored per publication ID in <DataDir>/onions/<id>.key, so
// addresses are stable across restarts.
//
// Self-healing. One supervisor goroutine owns the tor process and never
// gives up on it: nobody should have to press "Reconnect". It replaces tor,
// and re-registers every onion and client key, when
//
//   - the process exits or cannot start (retried with backoff);
//   - the control port stops answering (a frozen or half-dead tor);
//   - bootstrap makes no progress for bootstrapStall (after
//     purgeAfterStalls such attempts in a row the cached directory files are
//     thrown away, never the guard state or any key);
//   - the node's address stays unreachable (the self-check, torhealth.go);
//   - the network changed or the machine woke up (NetworkChanged).
//
// Status says honestly which of these is happening: the phase, the bootstrap
// percentage, what it is waiting for, since when, and which attempt this is.
//
// Invariants:
//   - Publications and client keys given while tor is down are remembered
//     and registered as soon as a tor is up, so a slow start loses nothing.
//   - SIGNAL NEWNYM is never sent (it drops client-auth keys).
//   - While the machine has no route to the internet the watchdog waits
//     instead of restarting tor in a loop.
type Tor struct {
	DataDir  string
	Provider runtime.Provider
	// ExtraConfig is appended to the generated torrc, one line each (tests
	// point tor at a proxy this way). Set it before Start.
	ExtraConfig []string
	// Notify, if set before Start, is called with a short sentence whenever
	// the supervisor recovers from something (tor exited, bootstrap stalled,
	// ...), for the node's activity log. It must not block.
	Notify func(msg string)

	mu         sync.Mutex
	state      State
	detail     string
	socksAddr  string
	ctrl       *torControl
	proc       *runtime.Proc
	onions     map[string]*onion // publication ID -> onion
	clientKeys map[string]string // service ID -> private key (connect side)
	stopping   bool
	cancel     context.CancelFunc

	// Supervisor state, reported by Status (see Status for the meanings).
	phase      string
	phaseSince time.Time
	progress   int
	summary    string // tor's own words for the bootstrap step
	warning    string // tor's bootstrap warning, if it reports trouble
	clockSkew  string // set when tor says this machine's clock is wrong
	attempt    int    // start attempts since tor was last healthy (1: first)
	retryAt    time.Time
	problem    string // why the previous attempt was given up
	restarts   int    // automatic recoveries since Start
	launchedAt time.Time
	healthy    bool          // the current tor got as far as "ready"
	stalls     int           // bootstrap stalls in a row (purgeAfterStalls)
	netc       chan netEvent // NetworkChanged -> netLoop
	kick       chan struct{} // a requested restart: cut the backoff short
	// wg counts every goroutine Start (and the supervisor) runs, so Stop can
	// wait for all of them. Add is only called while stopping is false, or
	// from a goroutine wg already counts, so it never races Stop's Wait.
	wg sync.WaitGroup

	// Health (see torhealth.go).
	self            identity.X25519Key // lets the self-check through client auth
	reachable       bool               // published and not known to be unreachable (torhealth.go)
	reachDetail     string             // why we are not (yet) reachable
	restartReason   string
	restartNow      bool      // the pending restart was asked for: skip the backoff
	lastAutoRestart time.Time // the self-check's last restart (minAutoRestartGap)
}

// netEvent is one NetworkChanged call, stamped so netLoop can tell whether
// the running tor started after it (and so already sees the new network).
type netEvent struct {
	reason string
	at     time.Time
}

type onion struct {
	serviceID string // 56 chars, no ".onion"; "" until tor has registered it once
	pub       Publication
	live      bool // registered with the tor that is running now
}

// address is the onion's public address, or "" before tor first registered it.
func (o *onion) address() string {
	if o.serviceID == "" {
		return ""
	}
	return o.serviceID + ".onion"
}

// Supervisor timings. Variables so tests can shorten them (set before Start,
// restore after Stop).
var (
	// torPollEvery is how often bootstrap progress and the control port are
	// checked.
	torPollEvery = time.Second
	// torControlWait is how long a new tor gets to open its control port.
	torControlWait = 60 * time.Second
	// bootstrapStall: a tor whose bootstrap percentage has not moved for
	// this long (with a network to use) is replaced.
	bootstrapStall = 2 * time.Minute
	// purgeAfterStalls: after this many stalled attempts in a row, tor's
	// cached directory documents are deleted before the next one.
	purgeAfterStalls = 3
	// torRetryBase and torRetryMax bound the backoff between start attempts.
	torRetryBase = time.Second
	torRetryMax  = time.Minute
	// failedAfter: this many start failures in a row (tor cannot even open
	// its control port) are reported as "failed". It is still retried.
	failedAfter = 3
	// torOnline reports whether the machine has a route to the internet.
	torOnline = netutil.Online
)

// Phases of the Tor transport, in Status.Phase.
const (
	PhaseStopped       = "stopped"
	PhaseLaunching     = "launching"     // starting the tor program
	PhaseBootstrapping = "bootstrapping" // tor is connecting to the network (Progress)
	PhasePublishing    = "publishing"    // connected; waiting for this node's address to go live
	PhaseReady         = "ready"         // connected and reachable
	PhaseOffline       = "offline"       // this machine has no network to use
	PhaseRetrying      = "retrying"      // waiting to start tor again (RetryAt)
)

// NewTor constructs the transport. It does not start tor.
func NewTor(dataDir string, provider runtime.Provider) *Tor {
	self, _ := identity.NewX25519() // a zero key only disables the self-check
	return &Tor{
		DataDir: dataDir, Provider: provider, state: StateStopped, phase: PhaseStopped,
		onions: map[string]*onion{}, clientKeys: map[string]string{}, self: self,
		netc: make(chan netEvent, 1), kick: make(chan struct{}, 1),
	}
}

func (t *Tor) Name() string { return "tor" }

// Start launches tor and returns once the control port is authenticated,
// or once the first attempt has failed. It reports that failure, but the
// transport is started either way: the supervisor keeps trying, and Publish
// and AuthorizeClient are remembered until a tor is up. Bootstrapping
// continues in the background; Status reports progress.
func (t *Tor) Start(ctx context.Context) error {
	t.mu.Lock()
	if t.cancel != nil && !t.stopping {
		t.mu.Unlock()
		return nil // already started
	}
	t.state, t.detail, t.stopping = StateStarting, "launching tor", false
	t.attempt, t.problem, t.restarts, t.stalls, t.healthy = 1, "", 0, 0, false
	t.restartReason, t.restartNow, t.lastAutoRestart = "", false, time.Time{}
	t.retryAt, t.progress, t.summary, t.warning, t.clockSkew = time.Time{}, 0, "", "", ""
	t.setPhaseLocked(PhaseLaunching)
	runCtx, cancel := context.WithCancel(context.Background())
	t.cancel = cancel
	t.mu.Unlock()

	err := t.launch(ctx, runCtx)
	if ctx.Err() != nil && err != nil {
		t.Stop()
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopping {
		return errors.New("tor was stopped")
	}
	t.goLocked(func() { t.supervise(runCtx, err) })
	t.goLocked(func() { t.watchNetwork(runCtx) })
	t.goLocked(func() { t.netLoop(runCtx) })
	t.goLocked(func() { t.selfCheck(runCtx) })
	return err
}

// goLocked runs f in a goroutine Stop waits for. t.mu must be held and
// t.stopping false (or the caller must itself be counted in t.wg).
func (t *Tor) goLocked(f func()) {
	t.wg.Add(1)
	go func() {
		defer t.wg.Done()
		f()
	}()
}

// setPhaseLocked records a phase change (and when it began).
func (t *Tor) setPhaseLocked(phase string) {
	if t.phase != phase {
		t.phase, t.phaseSince = phase, time.Now()
	}
}

// launch starts one tor process and connects to its control port.
func (t *Tor) launch(ctx, runCtx context.Context) error {
	dataDir, err := filepath.Abs(t.DataDir)
	if err != nil {
		return err
	}
	ports, err := netutil.FreePorts(2)
	if err != nil {
		return err
	}
	socks := fmt.Sprintf("127.0.0.1:%d", ports[0])
	control := fmt.Sprintf("127.0.0.1:%d", ports[1])
	cookie := filepath.Join(dataDir, "control_auth_cookie")
	os.Remove(cookie) // never authenticate with a stale cookie
	lines := []string{
		"SocksPort " + socks,
		"ControlPort " + control,
		"CookieAuthentication 1",
		"CookieAuthFile " + cookie,
		"DataDirectory " + filepath.Join(dataDir, "data"),
		"Log notice stdout",
		"__OwningControllerProcess " + strconv.Itoa(os.Getpid()),
		// The node is the reason this tor runs: never let it go dormant.
		"DormantCanceledByStartup 1",
	}
	lines = append(lines, t.ExtraConfig...)
	torrc := strings.Join(append(lines, ""), "\n")
	if err := os.MkdirAll(filepath.Join(dataDir, "data"), 0o700); err != nil {
		return err
	}
	if t.Provider == nil {
		return errors.New("no way to run tor")
	}
	proc, err := t.Provider.Start(runCtx, runtime.Program{
		Name:        "tor",
		DataDir:     dataDir,
		ConfigFiles: map[string]string{"torrc": torrc},
		Args:        []string{"-f", filepath.Join(dataDir, "torrc")},
	})
	if err != nil {
		return err
	}

	// Wait for the control port (tor writes the cookie when it is ready).
	deadline := time.Now().Add(torControlWait)
	var ctrl *torControl
	for wait := 20 * time.Millisecond; ; wait = min(wait*2, 250*time.Millisecond) {
		select {
		case <-proc.Done():
			return fmt.Errorf("tor exited during startup (see %s): %v", filepath.Join(dataDir, "tor.log"), proc.Err())
		case <-ctx.Done():
			proc.Stop()
			return ctx.Err()
		case <-time.After(wait):
		}
		dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		ctrl, err = dialTorControl(dctx, control, cookie)
		cancel()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			proc.Kill()
			return fmt.Errorf("tor control port never came up: %w", err)
		}
	}

	t.mu.Lock()
	if t.stopping {
		t.mu.Unlock()
		ctrl.close()
		proc.Stop()
		return errors.New("tor was stopped")
	}
	// Tor tells us when a directory has accepted our descriptor: that is
	// when peers can find us (torhealth.go), and about trouble such as a
	// wrong clock (noteEvent). The watcher ends with this tor.
	t.goLocked(func() {
		wctx, cancel := context.WithCancel(runCtx)
		defer cancel()
		go func() {
			select {
			case <-proc.Done():
				cancel()
			case <-wctx.Done():
			}
		}()
		if err := watchTorEvents(wctx, control, cookie, "HS_DESC STATUS_GENERAL", t.noteEvent); err != nil && wctx.Err() == nil {
			slog.Debug("tor: event watcher stopped", "err", err)
		}
	})
	t.proc, t.ctrl, t.socksAddr = proc, ctrl, socks
	t.restartReason, t.restartNow = "", false
	t.launchedAt, t.healthy = time.Now(), false
	t.progress, t.summary, t.warning, t.clockSkew = 0, "", "", ""
	t.state, t.detail = StateStarting, "tor is running; connecting to the Tor network"
	t.setPhaseLocked(PhaseBootstrapping)
	t.reachable, t.reachDetail = false, "publishing your address on Tor (usually under a minute)"
	onions := make([]*onion, 0, len(t.onions))
	for _, o := range t.onions {
		o.live = false // this tor has never heard of it
		onions = append(onions, &onion{pub: o.pub})
	}
	clientKeys := make(map[string]string, len(t.clientKeys))
	for k, v := range t.clientKeys {
		clientKeys[k] = v
	}
	t.mu.Unlock()

	// Register everything we were asked to publish, before or during a restart.
	for _, o := range onions {
		if _, err := t.addOnion(o.pub); err != nil {
			slog.Warn("tor: re-publishing onion failed", "id", o.pub.ID, "err", err)
		}
	}
	for sid, key := range clientKeys {
		if err := t.addClientAuth(sid, key); err != nil {
			slog.Warn("tor: re-adding client auth failed", "err", err)
		}
	}
	return nil
}

// supervise owns the tor process for as long as the transport is started:
// it watches the running tor (monitor) and starts a new one whenever that
// ends, with backoff while attempts keep failing. launchErr is the result
// of the first launch, made by Start.
func (t *Tor) supervise(runCtx context.Context, launchErr error) {
	fails := 0       // attempts in a row that never got healthy
	launchFails := 0 // of those, the ones where tor could not even start
	for {
		var why string
		wait := time.Duration(0)
		if launchErr == nil {
			var asked bool
			why, asked = t.monitor(runCtx)
			if runCtx.Err() != nil || t.isStopping() {
				return
			}
			t.mu.Lock()
			healthy := t.healthy
			t.mu.Unlock()
			launchFails = 0
			switch {
			case healthy:
				fails = 0 // it worked until now: start again without a penalty
				if !asked {
					wait = torRetryBase
				}
			case asked:
				// A requested restart (the network changed, the user asked)
				// is not a failure, but an unhealthy streak isn't over either.
			default:
				fails++
				wait = min(torRetryBase<<min(fails-1, 16), torRetryMax)
			}
		} else {
			if runCtx.Err() != nil || t.isStopping() {
				return
			}
			why = "tor could not start: " + launchErr.Error()
			fails++
			launchFails++
			wait = min(torRetryBase<<min(fails-1, 16), torRetryMax)
		}
		select {
		case <-t.kick: // belonged to the restart just handled
		default:
		}

		purged := false
		t.mu.Lock()
		if t.stalls >= purgeAfterStalls {
			t.stalls, purged = 0, true
		}
		t.restarts++
		t.attempt = fails + 1
		t.problem = why
		t.reachable = false
		t.ctrl, t.socksAddr = nil, ""
		t.state = StateStarting
		if launchFails >= failedAfter {
			t.state = StateFailed // honest: tor does not run here. Still retried.
		}
		t.retryAt = time.Now().Add(wait)
		t.detail = why + "; starting Tor again"
		if wait >= time.Second {
			t.detail += " in " + wait.Round(time.Second).String()
		}
		if t.attempt > 1 {
			t.detail += fmt.Sprintf(" (attempt %d)", t.attempt)
		}
		t.setPhaseLocked(PhaseRetrying)
		detail := t.detail
		t.mu.Unlock()
		slog.Info("tor: " + detail)
		t.notify("Tor: " + detail)
		if purged {
			t.purgeDirectoryCache()
		}

		select {
		case <-runCtx.Done():
			return
		case <-t.kick: // Restart or NetworkChanged: don't sit out the backoff
		case <-time.After(wait):
		}
		t.mu.Lock()
		t.retryAt = time.Time{}
		t.restartReason, t.restartNow = "", false
		if t.state != StateFailed {
			t.detail = "launching tor"
			if t.attempt > 1 {
				t.detail += fmt.Sprintf(" (attempt %d; before: %s)", t.attempt, why)
			}
		}
		t.setPhaseLocked(PhaseLaunching)
		t.mu.Unlock()
		launchErr = t.launch(runCtx, runCtx)
	}
}

func (t *Tor) isStopping() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.stopping
}

func (t *Tor) notify(msg string) {
	if t.Notify != nil {
		t.Notify(msg)
	}
}

// purgeDirectoryCache deletes tor's cached directory documents (consensus,
// certificates, relay descriptors) so the next start fetches them afresh: a
// cache that keeps tor stuck is not worth keeping. The "state" file (guard
// choices) and every key stay: new guards on every hiccup would cost
// anonymity.
func (t *Tor) purgeDirectoryCache() {
	dir := filepath.Join(t.DataDir, "data")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if n := e.Name(); strings.HasPrefix(n, "cached-") || strings.HasPrefix(n, "unverified-") {
			os.Remove(filepath.Join(dir, n))
		}
	}
	slog.Info("tor: bootstrap kept stalling; cleared the cached directory files")
	t.notify("Tor kept stalling, so its cached network information was cleared")
}

// abandon gives up on the running tor: the supervisor starts a new one.
// asked means somebody wanted this restart (no backoff, not a failure).
// The first reason given for a restart wins.
func (t *Tor) abandon(reason string, asked bool) {
	t.mu.Lock()
	proc, ctrl := t.proc, t.ctrl
	if t.restartReason == "" {
		t.restartReason = reason
	}
	t.restartNow = t.restartNow || asked
	t.reachable = false
	if proc != nil && !t.stopping {
		// SIGTERM, then kill: a frozen tor may ignore the first. Not
		// waited for here, so a caller holding up the supervisor can't hang.
		t.goLocked(func() { proc.Stop() })
	}
	t.mu.Unlock()
	if asked {
		select {
		case t.kick <- struct{}{}:
		default:
		}
	}
	if ctrl != nil {
		ctrl.close() // TAKEOWNERSHIP: tor exits when this connection closes
	}
}

// monitor watches the running tor until it is gone, and returns why. It
// tracks bootstrap progress, notices a control port that stopped answering
// and a bootstrap that stopped moving, and gives up on that tor itself in
// those cases. asked: the restart was requested (Restart, NetworkChanged).
func (t *Tor) monitor(runCtx context.Context) (why string, asked bool) {
	t.mu.Lock()
	proc, ctrl := t.proc, t.ctrl
	t.mu.Unlock()
	if proc == nil || ctrl == nil {
		return "tor is not running", false
	}
	lastProgress, lastMove := -1, time.Now()
	for {
		select {
		case <-proc.Done():
		default:
			line, err := ctrl.getInfo("status/bootstrap-phase")
			switch {
			case errors.Is(err, errControlLost):
				select {
				case <-proc.Done(): // it just exited: that is the real reason
				default:
					t.abandon("tor stopped answering on its control port", false)
				}
			case err != nil:
				slog.Debug("tor: reading bootstrap progress", "err", err)
			default:
				progress, _ := strconv.Atoi(fieldValue(line, "PROGRESS"))
				if progress != lastProgress {
					lastProgress, lastMove = progress, time.Now()
				}
				if t.noteBootstrap(line, progress, &lastMove) {
					t.mu.Lock()
					t.stalls++
					summary := t.summary
					t.mu.Unlock()
					t.abandon(fmt.Sprintf("Tor made no progress for %s at %d%% (%s)",
						bootstrapStall.Round(time.Second), progress, summary), false)
				}
			}
		}
		select {
		case <-runCtx.Done():
			return "stopped", true
		case <-proc.Done():
			t.mu.Lock()
			why, asked = t.restartReason, t.restartNow
			t.restartReason, t.restartNow = "", false
			t.mu.Unlock()
			if why == "" {
				why = fmt.Sprintf("tor exited unexpectedly (%v)", proc.Err())
			}
			return why, asked
		case <-time.After(torPollEvery):
		}
	}
}

// noteBootstrap records one bootstrap-phase reading and reports whether
// bootstrap has stalled. While the machine is offline nothing can move, so
// the stall clock (lastMove) is held back instead.
func (t *Tor) noteBootstrap(line string, progress int, lastMove *time.Time) (stalled bool) {
	summary := strings.Trim(fieldValue(line, "SUMMARY"), `"`)
	warning := ""
	if strings.HasPrefix(line, "WARN") {
		warning = strings.Trim(fieldValue(line, "WARNING"), `"`)
		if r := fieldValue(line, "REASON"); r != "" && warning != "" {
			warning += " (" + strings.ToLower(r) + ")"
		}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.progress, t.summary, t.warning = progress, summary, warning
	if progress >= 100 {
		t.state, t.detail = StateReady, "connected to the Tor network"
		t.stalls, t.clockSkew = 0, ""
		t.updateReadyLocked()
		return false
	}
	t.state = StateStarting
	if !torOnline() {
		*lastMove = time.Now()
		t.setPhaseLocked(PhaseOffline)
		t.detail = "this machine has no network connection; Tor will connect when there is one"
		return false
	}
	t.setPhaseLocked(PhaseBootstrapping)
	t.detail = fmt.Sprintf("bootstrapping %d%%: %s", progress, summary)
	stuck := time.Since(*lastMove)
	if stuck > bootstrapStall/4 && stuck > 5*time.Second {
		t.detail += fmt.Sprintf("; no progress for %s", stuck.Round(time.Second))
	}
	if t.clockSkew != "" {
		t.detail += ". " + t.clockSkew
	} else if warning != "" {
		t.detail += ". Tor says: " + warning
	}
	if t.attempt > 1 {
		t.detail += fmt.Sprintf(" (attempt %d)", t.attempt)
	}
	return stuck > bootstrapStall
}

// updateReadyLocked moves a bootstrapped tor between "publishing" and
// "ready": ready once it is reachable, or at once with nothing published.
// Ready is what counts as having worked: it ends a failing streak.
func (t *Tor) updateReadyLocked() {
	if t.progress < 100 || t.ctrl == nil {
		return
	}
	if len(t.onions) == 0 || t.reachable {
		t.healthy = true
		t.attempt, t.problem = 1, ""
		t.setPhaseLocked(PhaseReady)
	} else {
		t.setPhaseLocked(PhasePublishing)
	}
}

// fieldValue extracts KEY=value from a tor status line (values may be quoted).
func fieldValue(line, key string) string {
	i := strings.Index(line, key+"=")
	if i < 0 {
		return ""
	}
	v := line[i+len(key)+1:]
	if strings.HasPrefix(v, `"`) {
		if j := strings.Index(v[1:], `"`); j >= 0 {
			return v[:j+2]
		}
	}
	if j := strings.IndexByte(v, ' '); j >= 0 {
		return v[:j]
	}
	return v
}

func (t *Tor) keyPath(id string) string {
	return filepath.Join(t.DataDir, "onions", id+".key")
}

// Publish creates (or replaces) the onion service for p.ID.
func (t *Tor) Publish(ctx context.Context, p Publication) (string, error) {
	p.ClientAuth = slices.Clone(p.ClientAuth)
	addr, err := t.addOnion(p)
	if err != nil {
		return "", err
	}
	return addr, nil
}

// addOnion registers p with the running tor, replacing an existing onion
// with the same ID. It persists the onion key on first use.
func (t *Tor) addOnion(p Publication) (string, error) {
	if strings.ContainsAny(p.ID, `/\.`) || p.ID == "" {
		return "", fmt.Errorf("invalid publication id %q", p.ID)
	}
	t.mu.Lock()
	ctrl := t.ctrl
	old := t.onions[p.ID]
	if ctrl == nil {
		// Remember it: the next tor registers it (launch). With a known
		// address (same key file, same address) that is as good as done.
		sid := ""
		if old != nil {
			sid = old.serviceID
		}
		t.onions[p.ID] = &onion{serviceID: sid, pub: p}
		t.mu.Unlock()
		if sid != "" {
			return sid + ".onion", nil
		}
		return "", errors.New("tor is not running yet; the address is published once it is")
	}
	t.mu.Unlock()

	keyArg := "NEW:ED25519-V3"
	if b, err := os.ReadFile(t.keyPath(p.ID)); err == nil {
		keyArg = strings.TrimSpace(string(b))
	}
	if old != nil && old.live {
		// Tor cannot change client auth on a live onion, so remove and re-add
		// it with the same key (same address). Existing streams survive on
		// already-built circuits in practice; new ones use the new rules.
		if _, err := ctrl.do("DEL_ONION " + old.serviceID); err != nil {
			slog.Debug("tor: DEL_ONION failed", "err", err)
		}
	}

	lines, err := ctrl.do(addOnionCommand(keyArg, t.withSelf(p)))
	if err != nil {
		switch {
		case errors.Is(err, errControlLost):
			// This tor is going away; the next one registers p (launch).
			t.mu.Lock()
			sid := ""
			if old != nil {
				sid = old.serviceID
			}
			t.onions[p.ID] = &onion{serviceID: sid, pub: p}
			t.mu.Unlock()
		case old != nil && old.live:
			// Don't leave the service offline: restore the previous
			// publication (its key file exists, so the address is the same).
			if _, rerr := ctrl.do(addOnionCommand(keyArg, t.withSelf(old.pub))); rerr != nil {
				t.mu.Lock()
				delete(t.onions, p.ID)
				t.mu.Unlock()
			}
		}
		return "", fmt.Errorf("ADD_ONION: %w", err)
	}
	sid := replyValue(lines, "ServiceID")
	if sid == "" {
		return "", errors.New("ADD_ONION: no ServiceID in reply")
	}
	if pk := replyValue(lines, "PrivateKey"); pk != "" {
		if err := os.MkdirAll(filepath.Dir(t.keyPath(p.ID)), 0o700); err != nil {
			return "", err
		}
		if err := os.WriteFile(t.keyPath(p.ID), []byte(pk+"\n"), 0o600); err != nil {
			return "", fmt.Errorf("saving onion key: %w", err)
		}
	}
	t.mu.Lock()
	t.onions[p.ID] = &onion{serviceID: sid, pub: p, live: true}
	t.mu.Unlock()
	if len(p.ClientAuth) > 0 && len(t.self.Private) > 0 {
		if err := t.addClientAuth(sid, t.self.TorPrivate()); err != nil {
			slog.Debug("tor: self client auth", "err", err)
		}
	}
	return sid + ".onion", nil
}

// withSelf adds the self-check key to a publication that uses client auth.
func (t *Tor) withSelf(p Publication) Publication {
	if len(p.ClientAuth) == 0 || len(t.self.Public) == 0 {
		return p
	}
	p.ClientAuth = append(slices.Clone(p.ClientAuth), t.self.TorPublic())
	return p
}

// addOnionCommand builds the ADD_ONION line. Tor requires Flags=V3Auth
// whenever ClientAuthV3 keys are given ("512 No auth type specified").
func addOnionCommand(keyArg string, p Publication) string {
	cmd := fmt.Sprintf("ADD_ONION %s", keyArg)
	if len(p.ClientAuth) > 0 {
		cmd += " Flags=V3Auth"
	}
	cmd += fmt.Sprintf(" Port=%d,%s", onionVirtualPort, p.Target)
	for _, k := range p.ClientAuth {
		cmd += " ClientAuthV3=" + k
	}
	return cmd
}

// Unpublish removes the onion for id. Its key is kept, so publishing the
// same ID later gives the same address.
func (t *Tor) Unpublish(id string) error {
	t.mu.Lock()
	o, ok := t.onions[id]
	delete(t.onions, id)
	ctrl := t.ctrl
	t.mu.Unlock()
	if !ok || ctrl == nil {
		return nil
	}
	_, err := ctrl.do("DEL_ONION " + o.serviceID)
	return err
}

// NewAccess implements AccessIssuer: a fresh x25519 client-auth keypair.
// The public half goes on our onion, the private half to the peer.
func (t *Tor) NewAccess() (Access, error) {
	k, err := identity.NewX25519()
	if err != nil {
		return Access{}, err
	}
	return Access{Authorize: k.TorPublic(), Secret: k.TorPrivate()}, nil
}

// AuthorizeClient implements ClientAuthorizer: it gives this tor the
// client-auth private key for a peer's onion. addr may include ".onion" and
// a port. The key is remembered and re-added whenever tor restarts.
func (t *Tor) AuthorizeClient(addr, privateKey string) error {
	sid := onionServiceID(addr)
	t.mu.Lock()
	t.clientKeys[sid] = privateKey
	t.mu.Unlock()
	return t.addClientAuth(sid, privateKey)
}

// DeauthorizeClient forgets the client-auth key for addr (after unpairing).
func (t *Tor) DeauthorizeClient(addr string) error {
	sid := onionServiceID(addr)
	t.mu.Lock()
	delete(t.clientKeys, sid)
	ctrl := t.ctrl
	t.mu.Unlock()
	if ctrl == nil {
		return nil
	}
	_, err := ctrl.do("ONION_CLIENT_AUTH_REMOVE " + sid)
	return err
}

func (t *Tor) addClientAuth(sid, privateKey string) error {
	t.mu.Lock()
	ctrl := t.ctrl
	t.mu.Unlock()
	if ctrl == nil {
		return errors.New("tor is not running")
	}
	_, err := ctrl.do(fmt.Sprintf("ONION_CLIENT_AUTH_ADD %s x25519:%s", sid, privateKey))
	return err
}

func onionServiceID(addr string) string {
	host := addr
	if h, _, err := net.SplitHostPort(addr); err == nil {
		host = h
	}
	return strings.TrimSuffix(host, ".onion")
}

func (t *Tor) Handles(addr string) bool { return IsOnion(addr) }

// LogPath is where tor's own log is written (useful in error messages).
func (t *Tor) LogPath() string {
	if abs, err := filepath.Abs(t.DataDir); err == nil {
		return filepath.Join(abs, "tor.log")
	}
	return filepath.Join(t.DataDir, "tor.log")
}

// Dial connects to an onion through tor's SOCKS port. A missing port
// defaults to the Sol onion port.
func (t *Tor) Dial(ctx context.Context, addr string) (net.Conn, error) {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		addr = net.JoinHostPort(addr, strconv.Itoa(onionVirtualPort))
	}
	t.mu.Lock()
	socks := t.socksAddr
	t.mu.Unlock()
	if socks == "" {
		return nil, errors.New("tor is not running")
	}
	return dialSOCKS5(ctx, socks, addr)
}

func (t *Tor) Status() Status {
	t.mu.Lock()
	defer t.mu.Unlock()
	s := Status{
		Name: t.Name(), State: t.state, Detail: t.detail, Addresses: map[string]string{},
		Phase: t.phase, Progress: t.progress, PhaseSince: t.phaseSince,
		Problem: t.problem, Restarts: t.restarts,
	}
	if t.phase != PhaseStopped {
		s.Attempt = t.attempt
	}
	switch t.phase {
	case PhaseLaunching:
		s.WaitingFor = "the tor program to start"
	case PhaseBootstrapping:
		s.WaitingFor = "Tor to connect to the network"
		if t.summary != "" {
			s.WaitingFor += " (" + strings.ToLower(t.summary) + ")"
		}
	case PhasePublishing:
		s.WaitingFor = "the Tor network to accept this node's address"
	case PhaseOffline:
		s.WaitingFor = "a network connection"
	case PhaseRetrying:
		s.WaitingFor = "the next attempt to start Tor"
		s.RetryAt = t.retryAt
	}
	for id, o := range t.onions {
		if a := o.address(); a != "" {
			s.Addresses[id] = a
		}
	}
	// "Ready" means reachable, not just bootstrapped, once something is published.
	if s.State == StateReady && len(t.onions) > 0 {
		if t.reachable {
			s.Detail = "online and reachable"
		} else {
			s.State = StateStarting
			s.Detail = t.reachDetail
			if s.Detail == "" {
				s.Detail = "publishing your address on Tor (usually under a minute)"
			}
		}
	}
	return s
}

// WaitReady blocks until Tor is ready, ctx ends, or the transport is
// stopped. StateFailed does not end the wait: it only means the last few
// starts failed, and the supervisor keeps retrying in the background, so
// giving up here would abandon a tor that may still come up. When ctx ends
// first, the error says what tor was last doing, including that it was
// still retrying.
func (t *Tor) WaitReady(ctx context.Context) error {
	for {
		st := t.Status()
		if st.State == StateReady {
			return nil
		}
		t.mu.Lock()
		stopped := t.stopping
		t.mu.Unlock()
		if stopped {
			return errors.New("tor was stopped")
		}
		select {
		case <-ctx.Done():
			if st.State == StateFailed {
				return fmt.Errorf("%w; tor is still retrying: %s", ctx.Err(), st.Detail)
			}
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// Stop shuts tor down.
func (t *Tor) Stop() error {
	t.mu.Lock()
	t.stopping = true
	proc, ctrl, cancel := t.proc, t.ctrl, t.cancel
	t.ctrl, t.socksAddr = nil, ""
	t.state, t.detail = StateStopped, ""
	t.retryAt, t.reachable = time.Time{}, false
	t.setPhaseLocked(PhaseStopped)
	t.mu.Unlock()
	if ctrl != nil {
		ctrl.close() // TAKEOWNERSHIP: tor exits when this closes
	}
	if proc != nil {
		proc.Stop()
	}
	if cancel != nil {
		cancel()
	}
	t.wg.Wait() // every goroutine ends with runCtx or with the process
	return nil
}
