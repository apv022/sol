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
// control port. The same type is used by the node (Publish) and by
// `sol connect` (Dial + AuthorizeClient).
//
// Onion keys are stored per publication ID in <DataDir>/onions/<id>.key, so
// addresses are stable across restarts. If tor crashes, or the network
// changes, or our onions stop being reachable, tor is restarted and every
// onion and client key is re-registered (torhealth.go).
type Tor struct {
	DataDir  string
	Provider runtime.Provider

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

	// Health (see torhealth.go).
	self          identity.X25519Key // lets the self-check through client auth
	reachable     bool               // a self-check succeeded since the last launch
	reachDetail   string             // why we are not (yet) reachable
	restartReason string
	lastRestart   time.Time
}

type onion struct {
	serviceID string // 56 chars, no ".onion"
	pub       Publication
}

// NewTor constructs the transport. It does not start tor.
func NewTor(dataDir string, provider runtime.Provider) *Tor {
	self, _ := identity.NewX25519() // a zero key only disables the self-check
	return &Tor{
		DataDir: dataDir, Provider: provider, state: StateStopped,
		onions: map[string]*onion{}, clientKeys: map[string]string{}, self: self,
	}
}

func (t *Tor) Name() string { return "tor" }

// Start launches tor and returns once the control port is authenticated.
// Bootstrapping continues in the background; Status reports progress.
func (t *Tor) Start(ctx context.Context) error {
	t.mu.Lock()
	t.state, t.detail, t.stopping = StateStarting, "launching tor", false
	runCtx, cancel := context.WithCancel(context.Background())
	t.cancel = cancel
	t.mu.Unlock()

	if err := t.launch(ctx, runCtx); err != nil {
		t.setState(StateFailed, err.Error())
		cancel()
		return err
	}
	go t.supervise(runCtx)
	go t.watchNetwork(runCtx)
	go t.selfCheck(runCtx)
	return nil
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
	torrc := strings.Join([]string{
		"SocksPort " + socks,
		"ControlPort " + control,
		"CookieAuthentication 1",
		"CookieAuthFile " + cookie,
		"DataDirectory " + filepath.Join(dataDir, "data"),
		"Log notice stdout",
		"__OwningControllerProcess " + strconv.Itoa(os.Getpid()),
		"",
	}, "\n")
	if err := os.MkdirAll(filepath.Join(dataDir, "data"), 0o700); err != nil {
		return err
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
	deadline := time.Now().Add(60 * time.Second)
	var ctrl *torControl
	for {
		select {
		case <-proc.Done():
			return fmt.Errorf("tor exited during startup (see %s): %v", filepath.Join(dataDir, "tor.log"), proc.Err())
		case <-ctx.Done():
			proc.Stop()
			return ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
		dctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		ctrl, err = dialTorControl(dctx, control, cookie)
		cancel()
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			proc.Stop()
			return fmt.Errorf("tor control port never came up: %w", err)
		}
	}

	t.mu.Lock()
	t.proc, t.ctrl, t.socksAddr = proc, ctrl, socks
	t.reachable, t.reachDetail = false, "publishing your address on Tor (usually under a minute)"
	onions := make([]*onion, 0, len(t.onions))
	for _, o := range t.onions {
		onions = append(onions, o)
	}
	clientKeys := make(map[string]string, len(t.clientKeys))
	for k, v := range t.clientKeys {
		clientKeys[k] = v
	}
	t.mu.Unlock()

	// After a restart, re-register everything.
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

// supervise tracks bootstrap progress and restarts tor if it dies.
func (t *Tor) supervise(runCtx context.Context) {
	backoff := time.Second
	for {
		t.mu.Lock()
		proc := t.proc
		t.mu.Unlock()
		t.watchBootstrap(runCtx, proc)

		select {
		case <-runCtx.Done():
			return
		case <-proc.Done():
		}
		t.mu.Lock()
		stopping := t.stopping
		t.mu.Unlock()
		if stopping {
			return
		}
		t.mu.Lock()
		reason := t.restartReason
		t.restartReason = ""
		t.mu.Unlock()
		if reason == "" {
			reason = fmt.Sprintf("tor exited (%v)", proc.Err())
		}
		t.setState(StateStarting, reason+"; reconnecting to Tor")
		select {
		case <-runCtx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, time.Minute)
		if err := t.launch(runCtx, runCtx); err != nil {
			t.setState(StateFailed, "restart failed: "+err.Error())
			continue
		}
		backoff = time.Second
	}
}

// watchBootstrap polls bootstrap progress until 100% or the process exits.
func (t *Tor) watchBootstrap(runCtx context.Context, proc *runtime.Proc) {
	for {
		t.mu.Lock()
		ctrl := t.ctrl
		t.mu.Unlock()
		if ctrl == nil {
			return // stopped
		}
		phase, err := ctrl.getInfo("status/bootstrap-phase")
		if err == nil {
			progress := fieldValue(phase, "PROGRESS")
			summary := strings.Trim(fieldValue(phase, "SUMMARY"), `"`)
			if progress == "100" {
				t.setState(StateReady, "connected to the Tor network")
				return
			}
			t.setState(StateStarting, fmt.Sprintf("bootstrapping %s%%: %s", progress, summary))
		}
		select {
		case <-runCtx.Done():
			return
		case <-proc.Done():
			return
		case <-time.After(time.Second):
		}
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

func (t *Tor) setState(s State, detail string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.state, t.detail = s, detail
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
	t.mu.Unlock()
	if ctrl == nil {
		return "", errors.New("tor is not running")
	}

	keyArg := "NEW:ED25519-V3"
	if b, err := os.ReadFile(t.keyPath(p.ID)); err == nil {
		keyArg = strings.TrimSpace(string(b))
	}
	if old != nil {
		// Tor cannot change client auth on a live onion, so remove and re-add
		// it with the same key (same address). Existing streams survive on
		// already-built circuits in practice; new ones use the new rules.
		if _, err := ctrl.do("DEL_ONION " + old.serviceID); err != nil {
			slog.Debug("tor: DEL_ONION failed", "err", err)
		}
	}

	lines, err := ctrl.do(addOnionCommand(keyArg, t.withSelf(p)))
	if err != nil {
		if old != nil {
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
	t.onions[p.ID] = &onion{serviceID: sid, pub: p}
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

// AuthorizeClient gives this tor the client-auth private key for an onion
// (connect side). addr may include ".onion" and a port.
func (t *Tor) AuthorizeClient(addr, privateKey string) error {
	sid := onionServiceID(addr)
	t.mu.Lock()
	t.clientKeys[sid] = privateKey
	t.mu.Unlock()
	return t.addClientAuth(sid, privateKey)
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
	s := Status{Name: t.Name(), State: t.state, Detail: t.detail, Addresses: map[string]string{}}
	for id, o := range t.onions {
		s.Addresses[id] = o.serviceID + ".onion"
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

// WaitReady blocks until bootstrap reaches 100% or ctx ends.
func (t *Tor) WaitReady(ctx context.Context) error {
	for {
		st := t.Status()
		switch st.State {
		case StateReady:
			return nil
		case StateFailed:
			return errors.New(st.Detail)
		}
		select {
		case <-ctx.Done():
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
	return nil
}
