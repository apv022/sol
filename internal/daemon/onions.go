package daemon

import (
	"encoding/json"
	"errors"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"

	"github.com/apv022/sol/internal/fsutil"
	"github.com/apv022/sol/internal/transport"
	"github.com/apv022/sol/sdk/go/sol"
)

// Public onion services (ADMIN.md §Public onion services): a local port published as an open
// Tor onion service, reachable by anyone who has its .onion address, contact or not. This is
// unlike port sharing (services.go), which reaches chosen contacts over authenticated sessions.
//
// Invariants:
//   - Only Tor carries them, and never with client auth: they are meant to be public.
//   - Each service has its own onion key (onions/pub-<name>.key, kept by the Tor transport), so
//     its address is stable across restarts and unrelated to this node's own onion.
//   - Sol's own loopback ports (the UI, the app API, an app's port, the node listeners) can never
//     be published: they trust loopback callers, and Tor delivers strangers from loopback.
//   - Removing a service deletes its key: the address is gone for good.

// PublicService is one local port published as an open onion service.
type PublicService struct {
	// Name is lowercase letters, digits and dashes, at most 32.
	Name string `json:"name"`
	// Port is the local port on 127.0.0.1 that visitors reach.
	Port int `json:"port"`
	// OnionPort is the port on the .onion address (80 when 0, so a browser needs no port).
	OnionPort int `json:"onion_port,omitempty"`
	// Address is the .onion address once Tor has published it ("" before; it then stays the
	// same for as long as the service exists).
	Address string `json:"address,omitempty"`
}

// PublicOnionWarning is shown before a port is made public.
const PublicOnionWarning = "anyone who has the .onion address can connect to this port, without a Sol login: the service must protect itself"

// publicServicesFile holds the configured public services (0600; the addresses are derived
// from keys the Tor transport keeps).
const publicServicesFile = "publicservices.json"

// publicIDPrefix namespaces public services among Tor publications ("node" is this device).
const publicIDPrefix = "pub-"

// onionPortDefault is the .onion port when none is given.
const onionPortDefault = 80

var publicNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

type publicOnions struct {
	d    *Daemon
	path string
	mu   sync.Mutex
	list []PublicService // by name; Address is never stored
}

func newPublicOnions(d *Daemon) (*publicOnions, error) {
	po := &publicOnions{d: d, path: filepath.Join(d.dir, publicServicesFile)}
	b, err := os.ReadFile(po.path)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return po, nil
	case err != nil:
		return nil, err
	}
	if err := json.Unmarshal(b, &po.list); err != nil {
		return nil, errors.New("daemon: " + publicServicesFile + " is corrupt")
	}
	return po, nil
}

func (po *publicOnions) saveLocked() error {
	b, err := json.MarshalIndent(po.list, "", "  ")
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(po.path, b, 0o600)
}

// tor returns the running Tor transport, or nil when Tor is off.
func (d *Daemon) tor() *transport.Tor {
	for _, t := range d.transportsNow() {
		if tor, ok := t.(*transport.Tor); ok {
			return tor
		}
	}
	return nil
}

func publication(s PublicService) transport.Publication {
	port := s.OnionPort
	if port == 0 {
		port = onionPortDefault
	}
	// No ClientAuth: open to anyone with the address, which is the point.
	return transport.Publication{ID: publicIDPrefix + s.Name, Target: net.JoinHostPort("127.0.0.1", strconv.Itoa(s.Port)), Port: port}
}

// List returns the public services with their addresses as Tor reports them.
func (po *publicOnions) List() []PublicService {
	po.mu.Lock()
	out := slices.Clone(po.list)
	po.mu.Unlock()
	var addrs map[string]string
	if tor := po.d.tor(); tor != nil {
		addrs = tor.Status().Addresses
	}
	for i := range out {
		out[i].Address = addrs[publicIDPrefix+out[i].Name]
	}
	return out
}

// publishAll (re)publishes every public service on tor; Tor remembers ones it cannot publish
// yet and publishes them once it runs.
func (po *publicOnions) publishAll(tor *transport.Tor) {
	po.mu.Lock()
	list := slices.Clone(po.list)
	po.mu.Unlock()
	for _, s := range list {
		if _, err := tor.Publish(po.d.ctx, publication(s)); err != nil {
			po.d.log.Info("public onion not published yet", "service", s.Name, "err", err)
		}
	}
}

// sealedPort reports whether port is one Sol or one of its apps listens on. Those trust callers
// on loopback, and a public onion delivers strangers from loopback.
func (d *Daemon) sealedPort(port int) bool {
	if d.ownPort(port) {
		return true
	}
	portOf := func(addr string) int {
		_, p, err := net.SplitHostPort(addr)
		if err != nil {
			return 0
		}
		n, _ := strconv.Atoi(p)
		return n
	}
	for _, ln := range []net.Listener{d.nodeLn, d.anonLn} {
		if ln != nil && portOf(ln.Addr().String()) == port {
			return true
		}
	}
	d.ext.mu.Lock()
	ext := d.ext.url
	d.ext.mu.Unlock()
	if u, err := url.Parse(ext); err == nil && ext != "" && u.Port() == strconv.Itoa(port) {
		return true
	}
	if d.Host != nil {
		for _, a := range d.Host.List() {
			if a.Addr != "" && portOf(a.Addr) == port {
				return true
			}
		}
	}
	return false
}

// Add publishes a local port as a public onion service. Tor must be on.
func (po *publicOnions) Add(s PublicService) (PublicService, error) {
	s.Address = ""
	if !publicNameRe.MatchString(s.Name) {
		return PublicService{}, sol.Errorf("bad_request", "a service name is lowercase letters, digits and dashes (at most 32)")
	}
	if s.Port < 1 || s.Port > 65535 {
		return PublicService{}, sol.Errorf("bad_request", "port must be 1-65535")
	}
	if s.OnionPort < 0 || s.OnionPort > 65535 {
		return PublicService{}, sol.Errorf("bad_request", "onion_port must be 1-65535")
	}
	if po.d.sealedPort(s.Port) {
		return PublicService{}, sol.Errorf("bad_request", "that port is Sol's own or one of its apps'; it cannot be made public")
	}
	tor := po.d.tor()
	if tor == nil {
		return PublicService{}, sol.Errorf("unavailable", "Tor is off; turn it on first: sol do sol.transports.enable --name tor")
	}
	po.mu.Lock()
	if slices.ContainsFunc(po.list, func(x PublicService) bool { return x.Name == s.Name }) {
		po.mu.Unlock()
		return PublicService{}, sol.Errorf("conflict", "a public service named "+s.Name+" exists")
	}
	po.list = append(po.list, s)
	slices.SortFunc(po.list, func(a, b PublicService) int { return strings.Compare(a.Name, b.Name) })
	err := po.saveLocked()
	po.mu.Unlock()
	if err != nil {
		return PublicService{}, err
	}
	addr, perr := tor.Publish(po.d.ctx, publication(s))
	if perr != nil {
		// Saved: Tor publishes it once it runs; the address shows in the list then.
		po.d.log.Info("public onion not published yet", "service", s.Name, "err", perr)
	}
	s.Address = addr
	po.d.log.Info("public onion service added", "service", s.Name, "port", s.Port)
	return s, nil
}

// Remove unpublishes a public service and deletes its key: its address is gone for good.
func (po *publicOnions) Remove(name string) error {
	po.mu.Lock()
	i := slices.IndexFunc(po.list, func(x PublicService) bool { return x.Name == name })
	if i < 0 {
		po.mu.Unlock()
		return sol.Errorf("not_found", "no public service "+name)
	}
	po.list = slices.Delete(po.list, i, i+1)
	err := po.saveLocked()
	po.mu.Unlock()
	if err != nil {
		return err
	}
	if tor := po.d.tor(); tor != nil {
		return tor.Forget(publicIDPrefix + name)
	}
	// Tor is off: delete the key file it would have used, so the address cannot come back.
	key := filepath.Join(po.d.dir, "tor", "onions", publicIDPrefix+name+".key")
	if err := os.Remove(key); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}
