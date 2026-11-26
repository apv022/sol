// Package memsol is an in-memory fake of the whole Sol app API (design/API.md). One Network
// simulates several people, each with several devices, all in one process; a Client is the
// sol.API as seen by one (person, device, app). Apps and their tests run against it with no
// daemon, and Server exposes the same fake over real HTTP so the real client and JS UIs can be
// tried against it.
//
// Simplifications, all deliberate: replication is instant (every reader sees the same pos
// numbering), entries are hashed but not signed, and scopes are reported but not enforced.
// What is enforced: an app only touches its own app's feeds, feeds are readable only by owners
// and people they are shared with, and blobs follow the API.md rule (fetchable from a person
// only if referenced by an entry of a feed that person shares with you).
package memsol

import (
	"crypto/sha256"
	"encoding/base32"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/apv022/sol/sdk/go/sol"
)

var b32 = base32.StdEncoding.WithPadding(base32.NoPadding)

func newID(kind, name string) string {
	sum := sha256.Sum256([]byte(kind + ":" + name))
	return strings.ToLower(b32.EncodeToString(sum[:]))
}

// DialHandler serves a dialed stream for an app. path is the requested path.
type DialHandler func(conn net.Conn, caller sol.Caller, path string)

type appReg struct {
	manifest *sol.Manifest
	handler  http.Handler
	dial     DialHandler
}

// Person is one simulated person.
type Person struct {
	n        *Network
	ID, Name string
	Devices  []*Device
	online   bool
	contacts map[string]bool
	apps     map[string]*appReg
	inbox    []*InboxItem
	activity []sol.ActivityEntry
	pinned   map[string]bool
}

// Device is one device of a Person.
type Device struct {
	p      *Person
	ID     string
	Name   string
	Anchor bool
}

// Network is the whole simulated world. Safe for concurrent use.
type Network struct {
	// Now supplies entry timestamps; tests may replace it before use.
	Now func() time.Time

	mu      sync.Mutex
	people  map[string]*Person
	order   []*Person
	tokens  map[string]*Client
	feeds   map[string]*feedState        // by address
	shares  map[string]map[string]string // address -> person -> mode
	blobs   map[string][]byte
	holders map[string]map[string]bool // hash -> persons holding it locally
	refs    sol.BlobIndex              // blob references and releases of every feed (API.md §4)
	log     []logged
	notify  chan struct{}
	nextIn  int
}

// New creates an empty network.
func New() *Network {
	return &Network{
		Now:     func() time.Time { return time.Now().UTC() },
		people:  map[string]*Person{},
		tokens:  map[string]*Client{},
		feeds:   map[string]*feedState{},
		shares:  map[string]map[string]string{},
		blobs:   map[string][]byte{},
		holders: map[string]map[string]bool{},
		notify:  make(chan struct{}),
	}
}

// AddPerson adds a person with one device named "main". IDs are derived from the name, so they
// are stable between runs.
func (n *Network) AddPerson(name string) *Person {
	n.mu.Lock()
	defer n.mu.Unlock()
	p := &Person{n: n, ID: newID("person", name), Name: name, online: true,
		contacts: map[string]bool{}, apps: map[string]*appReg{}, pinned: map[string]bool{}}
	n.people[p.ID] = p
	n.order = append(n.order, p)
	p.addDeviceLocked("main", false)
	return p
}

// Person looks a person up by id.
func (n *Network) Person(id string) *Person {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.people[id]
}

// Pair makes a and b contacts of each other.
func (n *Network) Pair(a, b *Person) {
	n.mu.Lock()
	defer n.mu.Unlock()
	a.contacts[b.ID] = true
	b.contacts[a.ID] = true
	n.emitPerson(a, b)
	n.emitPerson(b, a)
}

// AddDevice adds another device to the person.
func (p *Person) AddDevice(name string, anchor bool) *Device {
	p.n.mu.Lock()
	defer p.n.mu.Unlock()
	return p.addDeviceLocked(name, anchor)
}

func (p *Person) addDeviceLocked(name string, anchor bool) *Device {
	d := &Device{p: p, ID: newID("device", p.Name+"/"+name), Name: name, Anchor: anchor}
	p.Devices = append(p.Devices, d)
	return d
}

// SetOnline marks the person offline or online; dials and cross-person action calls to an
// offline person fail with sol.ErrUnavailable.
func (p *Person) SetOnline(on bool) {
	p.n.mu.Lock()
	p.online = on
	for _, o := range p.n.order {
		if o.contacts[p.ID] {
			p.n.emitPerson(o, p)
		}
	}
	p.n.mu.Unlock()
}

// Install registers an app for this person: its manifest (for Actions and approval policy) and
// the handler that receives "/_sol/actions/{name}" requests.
func (p *Person) Install(m *sol.Manifest, h http.Handler) {
	p.n.mu.Lock()
	defer p.n.mu.Unlock()
	r := p.reg(m.ID)
	r.manifest, r.handler = m, h
}

// Handle registers just an action handler for an app id (no manifest).
func (p *Person) Handle(app string, h http.Handler) {
	p.n.mu.Lock()
	defer p.n.mu.Unlock()
	p.reg(app).handler = h
}

// OnDial registers the handler that receives dialed streams for an app.
func (p *Person) OnDial(app string, h DialHandler) {
	p.n.mu.Lock()
	defer p.n.mu.Unlock()
	p.reg(app).dial = h
}

func (p *Person) reg(app string) *appReg {
	r := p.apps[app]
	if r == nil {
		r = &appReg{}
		p.apps[app] = r
	}
	return r
}

// Pinned reports whether the person pinned the blob.
func (p *Person) Pinned(hash string) bool {
	p.n.mu.Lock()
	defer p.n.mu.Unlock()
	return p.pinned[hash]
}

func (n *Network) paired(a, b string) bool {
	return a == b || n.people[a].contacts[b]
}

func (p *Person) device(id string) *Device {
	for _, d := range p.Devices {
		if d.ID == id {
			return d
		}
	}
	return nil
}

func (p *Person) contactView(of *Person, viewer *Person) sol.Contact {
	c := sol.Contact{ID: of.ID, Name: of.Name, Petname: strings.ToLower(of.Name), Online: of.online,
		Devices: []sol.Device{}, SharesWithMe: []string{}, IShare: []string{}}
	for _, d := range of.Devices {
		c.Devices = append(c.Devices, sol.Device{ID: d.ID, Name: d.Name, Online: of.online, Anchor: d.Anchor})
	}
	n := viewer.n
	for addr, m := range n.shares {
		o, app, feed, _ := sol.ParseAddress(addr)
		if _, ok := m[viewer.ID]; ok && o == of.ID {
			c.SharesWithMe = append(c.SharesWithMe, app+"/"+feed)
		}
		if _, ok := m[of.ID]; ok && o == viewer.ID {
			c.IShare = append(c.IShare, app+"/"+feed)
		}
	}
	return c
}
