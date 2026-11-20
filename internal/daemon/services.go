package daemon

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/apv022/sol/internal/fsutil"
	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/session"
	"github.com/apv022/sol/sdk/go/sol"
)

// Port sharing (design/ADMIN.md "Port sharing"), ported from the PoC's services and tunnels.
//
// A service is a local TCP port (127.0.0.1:<port>) I share with chosen people (services.json,
// 0600). A contact reaches it over a session: GET /p/service/{name} with "Upgrade:
// sol-stream" becomes a raw stream into the port once the share check passes. Which services
// a device offers a peer is pushed to that peer (POST /p/services) when the session comes up
// and whenever the list changes, so nothing polls.
//
// A tunnel opens a contact's service on a local port: the daemon listens on 127.0.0.1:<port>
// and forwards each accepted connection as a fresh session stream. Invariants (the PoC's):
//   - Tunnels listen on 127.0.0.1 only, and are saved (tunnels.json, 0600): a tunnel reopens on
//     the same port when the daemon starts, so bookmarks keep working.
//   - The listener stays up while the peer is unreachable; every new connection dials the peer
//     fresh (Sessions.Connect redials), so restarts and network changes heal on their own.
//   - A saved tunnel whose port is busy keeps retrying it (retryPortEvery).
//   - HTTP tunnels check the Host header (127.0.0.1:<port> or localhost:<port>), so a web page
//     cannot use DNS rebinding to read a contact's service through this loopback port.
//   - Closing a tunnel cuts every stream it opened; unsharing (removing a service or a person
//     from it) cuts the sharer's streams of the people who lost access; unpairing closes and
//     forgets the person's tunnels and drops them from my services.
//   - My own devices may always open my services (relation self).

// ServicePath prefixes the session endpoint of a shared service: GET /p/service/{name}.
const ServicePath = "/p/service/"

// ServicesPushPath is the session endpoint where a device tells a peer which of its services
// the peer may open.
const ServicesPushPath = "/p/services"

// FirstTunnelPort is where automatic tunnel ports start: friendly numbers to bookmark.
const FirstTunnelPort = 9000

// retryPortEvery is how often a saved tunnel whose port was busy tries again.
var retryPortEvery = 10 * time.Second

// serviceNameRe: a service name is a DNS label, since it appears in
// http://<service>.<petname>.localhost:<port>/.
var serviceNameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

// Service is one local port I share (GET /admin/v1/services).
type Service struct {
	Name string `json:"name"`
	Port int    `json:"port"`
	// HTTP marks a web service: contacts can open it in the browser.
	HTTP bool `json:"http"`
	// People are the person IDs who may open it.
	People []string `json:"people"`
}

// RemoteService is a service a peer device offers me.
type RemoteService struct {
	Name string `json:"name"`
	HTTP bool   `json:"http"`
}

// TunnelInfo is one tunnel (GET /admin/v1/tunnels).
type TunnelInfo struct {
	ID      string `json:"id"`
	Person  string `json:"person"`
	Name    string `json:"person_name,omitempty"`
	Service string `json:"service"`
	Port    int    `json:"port"`
	Addr    string `json:"addr"`
	HTTP    bool   `json:"http"`
	URL     string `json:"url,omitempty"`
	// State is "open" (listening; the last stream worked or none was tried yet), "error" (the
	// last stream failed: LastError says why), "waiting" (the port is busy; retrying) or
	// "not_shared" (the person no longer offers the service).
	State     string    `json:"state"`
	Active    int       `json:"active"`
	LastOK    time.Time `json:"last_ok,omitzero"`
	LastError string    `json:"last_error,omitempty"`
	Device    string    `json:"device,omitempty"` // the device the last stream went to
}

type tunnelConfig struct {
	ID      string `json:"id"`
	Person  string `json:"person"`
	Service string `json:"service"`
	Port    int    `json:"port"`
	HTTP    bool   `json:"http"`
	Device  string `json:"device,omitempty"` // last device that served it, for dialing before a push arrives
}

type tunnel struct {
	cfg    tunnelConfig
	active atomic.Int64

	mu       sync.Mutex
	ln       net.Listener // nil while the port is busy
	srv      *http.Server
	lastOK   time.Time
	lastErr  string
	device   string
	unshared bool
	streams  map[net.Conn]struct{}
	stopped  bool
}

// inStream is a stream into one of my services, held so unsharing can cut it.
type inStream struct {
	conn   net.Conn
	person string
	self   bool
}

// portShare holds services, what peers offer me, and tunnels.
type portShare struct {
	d       *Daemon
	svcPath string
	tunPath string

	mu       sync.Mutex
	services map[string]Service
	in       map[string]map[*inStream]struct{}     // service -> open inbound streams
	remote   map[string]map[string][]RemoteService // person -> device -> offered services
	tunnels  map[string]*tunnel                    // by ID
	proxies  map[string]*http.Transport            // "<person>/<service>" for the petname router
	cancel   context.CancelFunc
	ctx      context.Context
	wg       sync.WaitGroup
}

func openPortShare(d *Daemon, svcPath, tunPath string) (*portShare, error) {
	ps := &portShare{d: d, svcPath: svcPath, tunPath: tunPath, services: map[string]Service{},
		in: map[string]map[*inStream]struct{}{}, remote: map[string]map[string][]RemoteService{},
		tunnels: map[string]*tunnel{}, proxies: map[string]*http.Transport{}}
	ps.ctx, ps.cancel = context.WithCancel(d.ctx)
	var svcs []Service
	if err := readJSON(svcPath, &svcs); err != nil {
		return nil, err
	}
	for _, s := range svcs {
		ps.services[s.Name] = s
	}
	var tcs []tunnelConfig
	if err := readJSON(tunPath, &tcs); err != nil {
		return nil, err
	}
	for _, c := range tcs {
		ps.tunnels[c.ID] = &tunnel{cfg: c}
	}
	return ps, nil
}

// readJSON reads a JSON file into v; a missing file leaves v alone.
func readJSON(p string, v any) error {
	b, err := os.ReadFile(p)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return errors.New("daemon: " + p + " is corrupt")
	}
	return nil
}

// start opens the saved tunnels.
func (ps *portShare) start() {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	for _, t := range ps.tunnels {
		if !ps.tryListenLocked(t) {
			ps.d.log.Info("tunnel port is busy; retrying", "service", t.cfg.Service, "port", t.cfg.Port)
			ps.wg.Add(1)
			go ps.retry(t)
		}
	}
}

// close stops every tunnel and cuts every stream.
func (ps *portShare) close() {
	ps.cancel()
	ps.mu.Lock()
	for _, t := range ps.tunnels {
		t.stop()
	}
	for _, m := range ps.in {
		for s := range m {
			s.conn.Close()
		}
	}
	for _, tr := range ps.proxies {
		tr.CloseIdleConnections()
	}
	ps.mu.Unlock()
	ps.wg.Wait()
}

func (ps *portShare) saveServicesLocked() error {
	list := make([]Service, 0, len(ps.services))
	for _, s := range ps.services {
		list = append(list, s)
	}
	slices.SortFunc(list, func(a, b Service) int { return strings.Compare(a.Name, b.Name) })
	b, _ := json.MarshalIndent(list, "", "  ")
	return fsutil.WriteFileAtomic(ps.svcPath, b, 0o600)
}

func (ps *portShare) saveTunnelsLocked() error {
	list := make([]tunnelConfig, 0, len(ps.tunnels))
	for _, t := range ps.tunnels {
		list = append(list, t.config())
	}
	slices.SortFunc(list, func(a, b tunnelConfig) int { return a.Port - b.Port })
	b, _ := json.MarshalIndent(list, "", "  ")
	return fsutil.WriteFileAtomic(ps.tunPath, b, 0o600)
}

// Services lists the services I share, by name.
func (ps *portShare) Services() []Service {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	out := []Service{}
	for _, s := range ps.services {
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b Service) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// sharedWith returns the services offered to person (all of them for my own devices).
func (ps *portShare) sharedWith(person string, self bool) []RemoteService {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	out := []RemoteService{}
	for _, s := range ps.services {
		if self || slices.Contains(s.People, person) {
			out = append(out, RemoteService{Name: s.Name, HTTP: s.HTTP})
		}
	}
	slices.SortFunc(out, func(a, b RemoteService) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// checkService validates a service and its people.
func (ps *portShare) checkService(s *Service) error {
	if !serviceNameRe.MatchString(s.Name) {
		return sol.Errorf("bad_request", "a service name is lowercase letters, digits and dashes (at most 32)")
	}
	if s.Port < 1 || s.Port > 65535 {
		return sol.Errorf("bad_request", "port must be 1-65535")
	}
	if s.People == nil {
		s.People = []string{}
	}
	slices.Sort(s.People)
	s.People = slices.Compact(s.People)
	for _, p := range s.People {
		pub, err := identity.ParseID(p)
		if err != nil || !ps.d.Contacts.IsContact(pub) {
			return sol.Errorf("bad_request", "not a contact: "+p)
		}
	}
	return nil
}

// AddService shares a local port.
func (ps *portShare) AddService(s Service) (Service, error) {
	if err := ps.checkService(&s); err != nil {
		return Service{}, err
	}
	ps.mu.Lock()
	if _, ok := ps.services[s.Name]; ok {
		ps.mu.Unlock()
		return Service{}, sol.Errorf("conflict", "a service named "+s.Name+" exists")
	}
	ps.services[s.Name] = s
	if err := ps.saveServicesLocked(); err != nil {
		delete(ps.services, s.Name)
		ps.mu.Unlock()
		return Service{}, err
	}
	ps.mu.Unlock()
	ps.changed(s.Name, &s)
	return s, nil
}

// ServicePatch changes a service; nil fields stay.
type ServicePatch struct {
	Port   *int      `json:"port"`
	HTTP   *bool     `json:"http"`
	People *[]string `json:"people"`
}

// UpdateService changes a service; people who lose access lose their open streams at once.
func (ps *portShare) UpdateService(name string, p ServicePatch) (Service, error) {
	ps.mu.Lock()
	old, ok := ps.services[name]
	ps.mu.Unlock()
	if !ok {
		return Service{}, sol.Errorf("not_found", "no service "+name)
	}
	s := old
	s.People = slices.Clone(old.People)
	if p.Port != nil {
		s.Port = *p.Port
	}
	if p.HTTP != nil {
		s.HTTP = *p.HTTP
	}
	if p.People != nil {
		s.People = slices.Clone(*p.People)
	}
	if err := ps.checkService(&s); err != nil {
		return Service{}, err
	}
	ps.mu.Lock()
	ps.services[name] = s
	if err := ps.saveServicesLocked(); err != nil {
		ps.services[name] = old
		ps.mu.Unlock()
		return Service{}, err
	}
	ps.cutLocked(name, func(st *inStream) bool {
		return s.Port != old.Port || !st.self && !slices.Contains(s.People, st.person)
	})
	ps.mu.Unlock()
	ps.changed(name, &s)
	return s, nil
}

// RemoveService stops sharing a port and cuts every stream into it.
func (ps *portShare) RemoveService(name string) error {
	ps.mu.Lock()
	old, ok := ps.services[name]
	if !ok {
		ps.mu.Unlock()
		return sol.Errorf("not_found", "no service "+name)
	}
	delete(ps.services, name)
	if err := ps.saveServicesLocked(); err != nil {
		ps.services[name] = old
		ps.mu.Unlock()
		return err
	}
	ps.cutLocked(name, func(*inStream) bool { return true })
	ps.mu.Unlock()
	ps.changed(name, nil)
	return nil
}

// cutLocked closes the inbound streams of a service that match. ps.mu held.
func (ps *portShare) cutLocked(name string, match func(*inStream) bool) {
	for st := range ps.in[name] {
		if match(st) {
			st.conn.Close()
			delete(ps.in[name], st)
		}
	}
}

// changed reports a service change and pushes the new lists to every live session.
func (ps *portShare) changed(name string, s *Service) {
	if s != nil {
		ps.d.emit(EvService, s)
	} else {
		ps.d.emit(EvService, removed("name", name))
	}
	for _, ss := range ps.d.Sessions.Sessions() {
		go ps.push(ss)
	}
}

// push tells a session's peer which of my services it may open.
func (ps *portShare) push(ss *session.Session) {
	p := ss.Peer()
	list := ps.sharedWith(identity.ID(p.Person), p.Relation == session.Self)
	b, _ := json.Marshal(map[string]any{"services": list})
	ctx, cancel := context.WithTimeout(ps.ctx, 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://peer"+ServicesPushPath, strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := ss.HTTPClient().Do(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	resp.Body.Close()
}

// peerPush serves POST /p/services: the calling device's offer to me replaces the one known.
func (ps *portShare) peerPush(w http.ResponseWriter, r *http.Request) {
	p, ok := session.PeerFrom(r.Context())
	if !ok {
		sol.WriteError(w, sol.Errorf("forbidden", "not a session peer"))
		return
	}
	var b struct {
		Services []RemoteService `json:"services"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&b); err != nil {
		sol.WriteError(w, sol.Errorf("bad_request", "bad JSON"))
		return
	}
	list := []RemoteService{}
	for _, s := range b.Services {
		if serviceNameRe.MatchString(s.Name) && len(list) < 256 {
			list = append(list, s)
		}
	}
	pid, did := identity.ID(p.Person), identity.ID(p.Device)
	ps.mu.Lock()
	if ps.remote[pid] == nil {
		ps.remote[pid] = map[string][]RemoteService{}
	}
	ps.remote[pid][did] = list
	var touched []*tunnel
	for _, t := range ps.tunnels {
		if t.cfg.Person != pid {
			continue
		}
		offered := ps.offersLocked(pid, t.cfg.Service)
		t.mu.Lock()
		if t.unshared != !offered {
			t.unshared = !offered
			touched = append(touched, t)
		}
		if offered {
			for _, s := range list {
				if s.Name == t.cfg.Service {
					t.cfg.HTTP = s.HTTP
				}
			}
		}
		t.mu.Unlock()
	}
	ps.mu.Unlock()
	w.WriteHeader(http.StatusNoContent)
	if p.Relation == session.Contact {
		ps.d.API.PersonChanged(pid)
	}
	for _, t := range touched {
		ps.d.emit(EvTunnel, ps.info(t))
	}
}

// offersLocked reports whether any known device of person offers service. ps.mu held.
func (ps *portShare) offersLocked(person, service string) bool {
	for _, list := range ps.remote[person] {
		for _, s := range list {
			if s.Name == service {
				return true
			}
		}
	}
	return false
}

// Offered returns the services person's devices offer me, by name.
func (ps *portShare) Offered(person string) []RemoteService {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	seen := map[string]RemoteService{}
	for _, list := range ps.remote[person] {
		for _, s := range list {
			seen[s.Name] = s
		}
	}
	out := []RemoteService{}
	for _, s := range seen {
		out = append(out, s)
	}
	slices.SortFunc(out, func(a, b RemoteService) int { return strings.Compare(a.Name, b.Name) })
	return out
}

// peerService serves GET /p/service/{name}: after the share check, the stream becomes a raw
// stream into 127.0.0.1:<port>.
func (ps *portShare) peerService(w http.ResponseWriter, r *http.Request) {
	p, ok := session.PeerFrom(r.Context())
	if !ok {
		sol.WriteError(w, sol.Errorf("forbidden", "not a session peer"))
		return
	}
	name, pid, self := r.PathValue("name"), identity.ID(p.Person), p.Relation == session.Self
	allowed := func(s Service) bool { return self || slices.Contains(s.People, pid) }
	ps.mu.Lock()
	s, ok := ps.services[name]
	ps.mu.Unlock()
	if !ok || !allowed(s) {
		// The same answer for "no such service" and "not yours": it reveals nothing.
		ps.d.log.Info("refused a service stream", "service", name, "person", pid)
		sol.WriteError(w, sol.Errorf("forbidden", "that service is not shared with you"))
		return
	}
	if !strings.EqualFold(r.Header.Get("Upgrade"), "sol-stream") {
		sol.WriteError(w, sol.Errorf("bad_request", "a service needs Upgrade: sol-stream"))
		return
	}
	var dl net.Dialer
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	local, err := dl.DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(s.Port)))
	cancel()
	if err != nil {
		sol.WriteError(w, sol.Errorf("unavailable", "the service is not answering"))
		return
	}
	conn, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		local.Close()
		return
	}
	var c net.Conn = conn
	if rw.Reader.Buffered() > 0 {
		c = &bufConn{Conn: conn, r: rw.Reader}
	}
	st := &inStream{conn: c, person: pid, self: self}
	ps.mu.Lock()
	cur, still := ps.services[name]
	if still && allowed(cur) && cur.Port == s.Port {
		if ps.in[name] == nil {
			ps.in[name] = map[*inStream]struct{}{}
		}
		ps.in[name][st] = struct{}{}
	}
	ps.mu.Unlock()
	if !still || !allowed(cur) || cur.Port != s.Port {
		conn.Close() // unshared meanwhile: never send bytes to a refused stream
		local.Close()
		return
	}
	if _, err := conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: sol-stream\r\n\r\n")); err == nil {
		_ = conn.SetDeadline(time.Time{})
		splice(c, local)
	}
	c.Close()
	local.Close()
	ps.mu.Lock()
	delete(ps.in[name], st)
	ps.mu.Unlock()
}

// bufConn returns bytes buffered during a handshake before reading the connection.
type bufConn struct {
	net.Conn
	r *bufio.Reader
}

// Read drains the handshake buffer first.
func (b *bufConn) Read(p []byte) (int, error) { return b.r.Read(p) }

// splice copies both ways until either side ends, then closes both.
func splice(a, b net.Conn) {
	var once sync.Once
	end := func() { once.Do(func() { a.Close(); b.Close() }) }
	go func() {
		_, _ = io.Copy(a, b)
		end()
	}()
	_, _ = io.Copy(b, a)
	end()
}

// dialService opens a raw stream to person's service: over a live session with a device that
// offers it, else by dialing one (prefer is tried first). It returns the device used.
func (ps *portShare) dialService(ctx context.Context, person, service, prefer string) (net.Conn, string, error) {
	ppub, err := identity.ParseID(person)
	if err != nil {
		return nil, "", sol.Errorf("not_found", "unknown person")
	}
	ps.mu.Lock()
	var devs []string
	for dev, list := range ps.remote[person] {
		if slices.ContainsFunc(list, func(s RemoteService) bool { return s.Name == service }) {
			devs = append(devs, dev)
		}
	}
	known := len(ps.remote[person]) > 0
	ps.mu.Unlock()
	if !known && prefer != "" {
		devs = append(devs, prefer) // before the first push of this run
	}
	if len(devs) == 0 {
		return nil, "", sol.Errorf("not_found", "they do not share "+service+" with you")
	}
	slices.SortFunc(devs, func(a, b string) int {
		ao, bo := ps.liveSession(a) != nil, ps.liveSession(b) != nil
		switch {
		case a == prefer && ao == bo || ao && !bo:
			return -1
		case b == prefer && ao == bo || bo && !ao:
			return 1
		}
		return strings.Compare(a, b)
	})
	var last error = sol.Errorf("unavailable", "they are unreachable")
	for _, dev := range devs {
		dpub, err := identity.ParseID(dev)
		if err != nil {
			continue
		}
		ss := ps.liveSession(dev)
		if ss == nil {
			if ss, err = ps.d.Sessions.Connect(ctx, ppub, dpub); err != nil {
				last = sol.Errorf("unavailable", "they are unreachable")
				continue
			}
		}
		c, err := openService(ctx, ss, service)
		if err != nil {
			last = err
			continue
		}
		return c, dev, nil
	}
	return nil, "", last
}

func (ps *portShare) liveSession(dev string) *session.Session {
	pub, err := identity.ParseID(dev)
	if err != nil {
		return nil
	}
	if s, ok := ps.d.Sessions.Session(pub); ok && s.Alive() {
		return s
	}
	return nil
}

// openService upgrades a new stream of ss to the peer's service.
func openService(ctx context.Context, ss *session.Session, service string) (net.Conn, error) {
	conn, err := ss.Open(ctx)
	if err != nil {
		return nil, sol.Errorf("unavailable", "they are unreachable")
	}
	req, _ := http.NewRequest(http.MethodGet, "http://peer"+ServicePath+service, nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "sol-stream")
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	}
	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, sol.Errorf("unavailable", "stream setup failed")
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		return nil, sol.Errorf("unavailable", "stream setup failed")
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer conn.Close()
		defer resp.Body.Close()
		var w struct {
			Error *sol.Error `json:"error"`
		}
		if json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&w) == nil && w.Error != nil {
			return nil, w.Error
		}
		return nil, sol.Errorf("unavailable", "the service refused the stream")
	}
	_ = conn.SetDeadline(time.Time{})
	return &bufConn{Conn: conn, r: br}, nil
}

// --- tunnels ---

func (t *tunnel) record(dev string, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if err != nil {
		t.lastErr = err.Error()
		var se *sol.Error
		if errors.As(err, &se) {
			t.lastErr = se.Message
		}
		return
	}
	t.lastOK, t.lastErr, t.device = time.Now().UTC(), "", dev
}

// stop closes the listener and every stream the tunnel opened.
func (t *tunnel) stop() {
	t.mu.Lock()
	t.stopped = true
	streams := t.streams
	t.streams = nil
	ln, srv := t.ln, t.srv
	t.mu.Unlock()
	if srv != nil {
		srv.Close()
	}
	if ln != nil {
		ln.Close()
	}
	for c := range streams {
		c.Close()
	}
}

func (t *tunnel) addStream(c net.Conn) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.stopped {
		return false
	}
	if t.streams == nil {
		t.streams = map[net.Conn]struct{}{}
	}
	t.streams[c] = struct{}{}
	return true
}

func (t *tunnel) dropStream(c net.Conn) {
	t.mu.Lock()
	delete(t.streams, c)
	t.mu.Unlock()
}

// tunnelStream is a stream owned by a tunnel; closing it unregisters it.
type tunnelStream struct {
	net.Conn
	t    *tunnel
	once sync.Once
}

// Close implements net.Conn.
func (c *tunnelStream) Close() error {
	c.once.Do(func() { c.t.dropStream(c) })
	return c.Conn.Close()
}

// config returns a copy of t's configuration (HTTP and Device change as pushes and dials
// come in; ID, Person, Service and Port never do).
func (t *tunnel) config() tunnelConfig {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.cfg
}

// info describes a tunnel.
func (ps *portShare) info(t *tunnel) TunnelInfo {
	cfg := t.config()
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.Port))
	v := TunnelInfo{ID: cfg.ID, Person: cfg.Person, Service: cfg.Service, Port: cfg.Port, Addr: addr,
		HTTP: cfg.HTTP, Active: int(t.active.Load())}
	if pub, err := identity.ParseID(cfg.Person); err == nil {
		v.Name, _ = ps.d.Contacts.Name(pub)
	}
	if v.HTTP {
		v.URL = "http://" + addr + "/"
	}
	t.mu.Lock()
	v.LastOK, v.LastError, v.Device = t.lastOK, t.lastErr, t.device
	switch {
	case t.unshared:
		v.State = "not_shared"
	case t.ln == nil:
		v.State = "waiting"
	case t.lastErr != "":
		v.State = "error"
	default:
		v.State = "open"
	}
	t.mu.Unlock()
	return v
}

// Tunnels lists the tunnels, by port.
func (ps *portShare) Tunnels() []TunnelInfo {
	ps.mu.Lock()
	list := make([]*tunnel, 0, len(ps.tunnels))
	for _, t := range ps.tunnels {
		list = append(list, t)
	}
	ps.mu.Unlock()
	slices.SortFunc(list, func(a, b *tunnel) int { return a.cfg.Port - b.cfg.Port })
	out := []TunnelInfo{}
	for _, t := range list {
		out = append(out, ps.info(t))
	}
	return out
}

// OpenTunnel makes person's service reachable on 127.0.0.1:port (0 picks a free one from
// FirstTunnelPort up) and saves the tunnel. Opening one that exists returns it.
func (ps *portShare) OpenTunnel(person, service string, port int) (TunnelInfo, error) {
	pub, err := identity.ParseID(person)
	me, _ := ps.d.me()
	if err != nil || person != me && !ps.d.Contacts.IsContact(pub) {
		return TunnelInfo{}, sol.Errorf("not_found", "not a contact: "+person)
	}
	if port < 0 || port > 65535 {
		return TunnelInfo{}, sol.Errorf("bad_request", "port must be 0-65535")
	}
	ps.mu.Lock()
	defer ps.mu.Unlock()
	if !ps.offersLocked(person, service) {
		return TunnelInfo{}, sol.Errorf("not_found", "they do not share "+service+" with you (as far as their devices told this one)")
	}
	for _, t := range ps.tunnels {
		if t.cfg.Person == person && t.cfg.Service == service {
			if port != 0 && port != t.cfg.Port {
				return TunnelInfo{}, sol.Errorf("conflict", service+" is already open on port "+strconv.Itoa(t.cfg.Port)+"; close it first to move it")
			}
			t.mu.Lock()
			busy := t.ln == nil
			t.mu.Unlock()
			if busy {
				ps.tryListenLocked(t)
			}
			return ps.info(t), nil
		}
	}
	ln, err := ps.listenLocked(port)
	if err != nil {
		return TunnelInfo{}, err
	}
	httpSvc := false
	for _, list := range ps.remote[person] {
		for _, s := range list {
			if s.Name == service {
				httpSvc = s.HTTP
			}
		}
	}
	t := &tunnel{cfg: tunnelConfig{ID: randomHex(6), Person: person, Service: service,
		Port: ln.Addr().(*net.TCPAddr).Port, HTTP: httpSvc}}
	ps.tunnels[t.cfg.ID] = t
	if err := ps.saveTunnelsLocked(); err != nil {
		delete(ps.tunnels, t.cfg.ID)
		ln.Close()
		return TunnelInfo{}, err
	}
	ps.serveLocked(t, ln)
	return ps.info(t), nil
}

// listenLocked opens 127.0.0.1:port, or the first free port from FirstTunnelPort for 0.
func (ps *portShare) listenLocked(port int) (net.Listener, error) {
	if port != 0 {
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
		if err != nil {
			return nil, sol.Errorf("conflict", "port "+strconv.Itoa(port)+" is taken; pick another or leave it empty")
		}
		return ln, nil
	}
	used := map[int]bool{}
	for _, t := range ps.tunnels {
		used[t.cfg.Port] = true
	}
	for p := FirstTunnelPort; p < FirstTunnelPort+1000; p++ {
		if used[p] {
			continue
		}
		if ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p))); err == nil {
			return ln, nil
		}
	}
	return net.Listen("tcp", "127.0.0.1:0")
}

// CloseTunnel stops a tunnel, cuts its streams and forgets it.
func (ps *portShare) CloseTunnel(id string) error {
	ps.mu.Lock()
	t := ps.tunnels[id]
	if t == nil {
		ps.mu.Unlock()
		return sol.Errorf("not_found", "no tunnel "+id)
	}
	delete(ps.tunnels, id)
	err := ps.saveTunnelsLocked()
	ps.mu.Unlock()
	t.stop()
	return err
}

// forget drops everything of a person who is no longer a contact: their offers, their
// tunnels, and their place in my services.
func (ps *portShare) forget(person string) {
	ps.mu.Lock()
	delete(ps.remote, person)
	var gone []*tunnel
	for id, t := range ps.tunnels {
		if t.cfg.Person == person {
			gone = append(gone, t)
			delete(ps.tunnels, id)
		}
	}
	var changed []Service
	for name, s := range ps.services {
		if i := slices.Index(s.People, person); i >= 0 {
			s.People = slices.Delete(slices.Clone(s.People), i, i+1)
			ps.services[name] = s
			changed = append(changed, s)
			ps.cutLocked(name, func(st *inStream) bool { return st.person == person && !st.self })
		}
	}
	if len(gone) > 0 {
		_ = ps.saveTunnelsLocked()
	}
	if len(changed) > 0 {
		_ = ps.saveServicesLocked()
	}
	ps.mu.Unlock()
	for _, t := range gone {
		t.stop()
		ps.d.emit(EvTunnel, removed("id", t.cfg.ID))
	}
	for _, s := range changed {
		ps.d.emit(EvService, s)
	}
}

// tryListenLocked opens t's saved port and serves it.
func (ps *portShare) tryListenLocked(t *tunnel) bool {
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(t.cfg.Port)))
	if err != nil {
		t.record("", errors.New("port "+strconv.Itoa(t.cfg.Port)+" is taken by another program"))
		return false
	}
	ps.serveLocked(t, ln)
	t.mu.Lock()
	t.lastErr = ""
	t.mu.Unlock()
	return true
}

// retry keeps trying t's port until it opens, the tunnel is closed, or Sol stops.
func (ps *portShare) retry(t *tunnel) {
	defer ps.wg.Done()
	tick := time.NewTicker(retryPortEvery)
	defer tick.Stop()
	for {
		select {
		case <-ps.ctx.Done():
			return
		case <-tick.C:
		}
		ps.mu.Lock()
		current := ps.tunnels[t.cfg.ID] == t
		t.mu.Lock()
		listening := t.ln != nil
		t.mu.Unlock()
		ok := current && (listening || ps.tryListenLocked(t))
		ps.mu.Unlock()
		if ok {
			ps.d.emit(EvTunnel, ps.info(t))
		}
		if !current || ok {
			return
		}
	}
}

// serveLocked starts serving ln for t: raw TCP, or for an HTTP service a Host-checking proxy.
func (ps *portShare) serveLocked(t *tunnel, ln net.Listener) {
	t.mu.Lock()
	t.ln = ln
	if t.cfg.HTTP {
		t.srv = &http.Server{Handler: ps.httpTunnel(t), ReadHeaderTimeout: 30 * time.Second}
	}
	srv := t.srv
	t.mu.Unlock()
	if srv != nil {
		go func() { _ = srv.Serve(ln) }()
		return
	}
	go ps.tcpTunnel(t, ln)
}

// dialTunnel opens a stream for t and records how it went. A dial cancelled by the client
// says nothing about the peer, so it is not recorded as an error.
func (ps *portShare) dialTunnel(ctx context.Context, t *tunnel) (net.Conn, error) {
	before := ps.info(t)
	cfg := t.config()
	c, dev, err := ps.dialService(ctx, cfg.Person, cfg.Service, cfg.Device)
	if err != nil {
		if ctx.Err() == nil {
			t.record("", err)
		}
	} else {
		t.record(dev, nil)
		if dev != cfg.Device {
			ps.mu.Lock()
			t.mu.Lock()
			t.cfg.Device = dev
			t.mu.Unlock()
			if ps.tunnels[t.cfg.ID] == t {
				_ = ps.saveTunnelsLocked()
			}
			ps.mu.Unlock()
		}
	}
	if after := ps.info(t); after.State != before.State || after.LastError != before.LastError {
		ps.d.emit(EvTunnel, after)
	}
	if err != nil {
		return nil, err
	}
	sc := &tunnelStream{Conn: c, t: t}
	if !t.addStream(sc) {
		c.Close()
		return nil, errors.New("tunnel closed")
	}
	return sc, nil
}

func (ps *portShare) tcpTunnel(t *tunnel, ln net.Listener) {
	for {
		local, err := ln.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			time.Sleep(100 * time.Millisecond) // temporary (out of descriptors): keep serving
			continue
		}
		go func() {
			t.active.Add(1)
			defer t.active.Add(-1)
			ctx, cancel := context.WithTimeout(ps.ctx, time.Minute)
			remote, err := ps.dialTunnel(ctx, t)
			cancel()
			if err != nil {
				local.Close()
				return
			}
			splice(local, remote)
		}()
	}
}

// httpTunnel proxies HTTP to the service, one stream per upstream connection (kept alive).
func (ps *portShare) httpTunnel(t *tunnel) http.Handler {
	port := strconv.Itoa(t.cfg.Port)
	allowed := []string{"127.0.0.1:" + port, "localhost:" + port}
	tr := &http.Transport{
		DialContext:         func(ctx context.Context, _, _ string) (net.Conn, error) { return ps.dialTunnel(ctx, t) },
		MaxIdleConnsPerHost: 8,
		IdleConnTimeout:     90 * time.Second,
	}
	rp := proxyTo(tr)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !slices.Contains(allowed, strings.ToLower(r.Host)) {
			http.Error(w, "wrong host", http.StatusMisdirectedRequest)
			return
		}
		t.active.Add(1)
		defer t.active.Add(-1)
		rp.ServeHTTP(w, r)
	})
}

// proxyTo is a reverse proxy over streams tr opens; the browser's Host is kept, Sol's own
// headers are not passed on.
func proxyTo(tr http.RoundTripper) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL.Scheme, pr.Out.URL.Host = "http", "service"
			pr.Out.Host = pr.In.Host
			for k := range pr.Out.Header {
				if strings.HasPrefix(http.CanonicalHeaderKey(k), "Sol-") {
					pr.Out.Header.Del(k)
				}
			}
		},
		Transport: tr,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			msg := "the service is unreachable"
			var se *sol.Error
			if errors.As(err, &se) {
				msg = se.Message
			}
			http.Error(w, msg, http.StatusBadGateway)
		},
	}
}

// serveHTTPService serves http://<service>.<petname>.localhost:<port>/: the contact's HTTP
// service, proxied over sessions.
func (ps *portShare) serveHTTPService(w http.ResponseWriter, r *http.Request, person string, svc RemoteService) {
	key := person + "/" + svc.Name
	ps.mu.Lock()
	tr := ps.proxies[key]
	if tr == nil {
		tr = &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				c, _, err := ps.dialService(ctx, person, svc.Name, "")
				return c, err
			},
			MaxIdleConnsPerHost: 8,
			IdleConnTimeout:     90 * time.Second,
		}
		ps.proxies[key] = tr
	}
	ps.mu.Unlock()
	proxyTo(tr).ServeHTTP(w, r)
}
