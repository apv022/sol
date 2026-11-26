package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httputil"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/apv022/sol/internal/appapi"
	"github.com/apv022/sol/internal/fsutil"
	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/session"
	"github.com/apv022/sol/sdk/go/sol"
)

// People and devices as the admin API shows them, local petnames, unpairing, and the router
// for http://<name>.<petname>.localhost:<port>/ (a contact's app UI or HTTP service).

// UnpairPath is the session endpoint a device calls to tell a contact it unpaired them: they
// drop me as a contact too (their copies of what I shared stay with them).
const UnpairPath = "/p/unpair"

// petnameRe: a petname is a DNS label, since it appears in hostnames.
var petnameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)

// ShareRef is one thing shared between me and a contact.
type ShareRef struct {
	// Kind is "app" (feeds of an app; Feeds names them) or "service" (port sharing).
	Kind  string   `json:"kind"`
	Name  string   `json:"name"`
	Feeds []string `json:"feeds,omitempty"`
	HTTP  bool     `json:"http,omitempty"`
	// URL opens it in the browser on this device (their app UI, or an HTTP service).
	URL string `json:"url,omitempty"`
}

// PersonInfo is one contact (GET /admin/v1/people).
type PersonInfo struct {
	ID string `json:"id"`
	// Name is the name in their person certificate (as paired).
	Name string `json:"name"`
	// DisplayName is the name to show: my petname for them if I renamed them, else the name
	// their profile chose, else Name; NameSource says which ("petname", "profile",
	// "certificate"). ProfileName is their profile's name, if any.
	DisplayName string `json:"display_name"`
	NameSource  string `json:"name_source"`
	ProfileName string `json:"profile_name,omitempty"`
	// Avatar is the URL of their picture on the UI root, once this device holds it.
	Avatar  string `json:"avatar,omitempty"`
	Petname string `json:"petname"`
	// Renamed is true when I gave this person a petname myself; otherwise Petname is the
	// default address label derived from their name, and UIs show Name.
	Renamed bool `json:"renamed,omitempty"`
	Online  bool `json:"online"`
	// Anonymous: reached over Tor only, both ways; they only ever learn my onion addresses.
	Anonymous    bool         `json:"anonymous"`
	Devices      []sol.Device `json:"devices"`
	SharesWithMe []ShareRef   `json:"shares_with_me"`
	IShare       []ShareRef   `json:"i_share"`
}

// DeviceInfo is one of my devices (GET /admin/v1/devices).
type DeviceInfo struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Online   bool      `json:"online"`
	Admin    bool      `json:"admin"`
	Anchor   bool      `json:"anchor"`
	Added    time.Time `json:"added,omitzero"`
	LastSeen time.Time `json:"last_seen,omitzero"`
	This     bool      `json:"this"`
}

// petnames are the local names I gave contacts (petnames.json, 0600).
type petnames struct {
	path string
	mu   sync.Mutex
	m    map[string]string
}

func openPetnames(p string) (*petnames, error) {
	pn := &petnames{path: p, m: map[string]string{}}
	return pn, readJSON(p, &pn.m)
}

func (p *petnames) get(id string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.m[id]
}

func (p *petnames) set(id, name string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if name == "" {
		delete(p.m, id)
	} else {
		p.m[id] = name
	}
	b, _ := json.MarshalIndent(p.m, "", "  ")
	return fsutil.WriteFileAtomic(p.path, b, 0o600)
}

// person builds a contact's PersonInfo.
func (d *Daemon) person(id string) (PersonInfo, bool) {
	c, ok := d.API.Contact(id)
	if !ok {
		return PersonInfo{}, false
	}
	pi := PersonInfo{ID: c.ID, Name: c.Name, Petname: c.Petname, Online: c.Online, Devices: c.Devices,
		SharesWithMe: []ShareRef{}, IShare: []ShareRef{}}
	pi.Renamed = d.pets.get(id) != ""
	pi.DisplayName, pi.NameSource, pi.ProfileName, pi.Avatar = d.shownName(id, c.Name, c.Petname, pi.Renamed)
	if pub, err := identity.ParseID(c.ID); err == nil {
		pi.Anonymous = d.Contacts.Anonymous(pub)
	}
	group := func(keys []string, url bool) []ShareRef {
		apps := map[string][]string{}
		for _, k := range keys {
			app, feed, _ := strings.Cut(k, "/")
			apps[app] = append(apps[app], feed)
		}
		out := []ShareRef{}
		for app, fs := range apps {
			r := ShareRef{Kind: "app", Name: app, Feeds: fs}
			if url {
				r.URL = d.peerURL(app, c.Petname)
			}
			out = append(out, r)
		}
		sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		return out
	}
	pi.SharesWithMe = group(c.SharesWithMe, true)
	pi.IShare = group(c.IShare, false)
	for _, s := range d.ports.Offered(id) {
		r := ShareRef{Kind: "service", Name: s.Name, HTTP: s.HTTP}
		if s.HTTP {
			r.URL = d.peerURL(s.Name, c.Petname)
		}
		pi.SharesWithMe = append(pi.SharesWithMe, r)
	}
	for _, s := range d.ports.Services() {
		for _, p := range s.People {
			if p == id {
				pi.IShare = append(pi.IShare, ShareRef{Kind: "service", Name: s.Name, HTTP: s.HTTP})
			}
		}
	}
	return pi, true
}

// peerURL is where a contact's app or HTTP service opens on this device.
func (d *Daemon) peerURL(name, petname string) string {
	if !petnameRe.MatchString(petname) {
		return ""
	}
	return "http://" + name + "." + petname + ".localhost:" + itoa(d.UIPort()) + "/"
}

// People lists my contacts, by shown name.
func (d *Daemon) People() []PersonInfo {
	out := []PersonInfo{}
	for _, id := range d.Contacts.List() {
		if p, ok := d.person(id); ok {
			out = append(out, p)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if a, b := strings.ToLower(out[i].DisplayName), strings.ToLower(out[j].DisplayName); a != b {
			return a < b
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// SetPetname renames a contact locally ("" restores the default).
func (d *Daemon) SetPetname(id, name string) (PersonInfo, error) {
	name = strings.ToLower(strings.TrimSpace(name))
	if _, ok := d.API.Contact(id); !ok {
		return PersonInfo{}, sol.Errorf("not_found", "not a contact: "+id)
	}
	if name != "" && !petnameRe.MatchString(name) {
		return PersonInfo{}, sol.Errorf("bad_request", "a petname is lowercase letters, digits and dashes (at most 32), as it appears in addresses")
	}
	if name != "" {
		for _, p := range d.People() {
			if p.ID != id && p.Petname == name {
				return PersonInfo{}, sol.Errorf("conflict", "another contact is called "+name)
			}
		}
	}
	if err := d.pets.set(id, name); err != nil {
		return PersonInfo{}, err
	}
	d.API.PersonChanged(id)
	p, _ := d.person(id)
	return p, nil
}

// Unpair drops a contact on this side and tells their online devices to drop me. Their copies
// of what I shared stay with them; my shares with them stop.
func (d *Daemon) Unpair(ctx context.Context, id string) error {
	pub, err := identity.ParseID(id)
	if err != nil || !d.Contacts.IsContact(pub) {
		return sol.Errorf("not_found", "not a contact: "+id)
	}
	for _, s := range d.Sessions.Sessions() {
		if p := s.Peer(); bytes.Equal(p.Person, pub) {
			cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			req, _ := http.NewRequestWithContext(cctx, http.MethodPost, "http://peer"+UnpairPath, nil)
			if resp, err := s.HTTPClient().Do(req); err == nil {
				resp.Body.Close()
			}
			cancel()
		}
	}
	return d.dropContact(pub)
}

// dropContact forgets a contact here: the contact record, sessions and redials, tunnels and
// service shares, the local petname.
func (d *Daemon) dropContact(pub []byte) error {
	id := identity.ID(pub)
	if err := d.Contacts.RemoveContact(pub); err != nil {
		return err
	}
	if _, dl, ok := d.Cache.Latest(pub); ok {
		for _, dv := range dl.Devices {
			d.Sessions.Forget(dv.Key)
		}
	}
	for _, s := range d.Sessions.Sessions() {
		if bytes.Equal(s.Peer().Person, pub) {
			s.Close()
		}
	}
	d.ports.forget(id)
	d.Sessions.ForgetProfile(pub)
	_ = d.pets.set(id, "")
	d.emit(EvPerson, removed("id", id))
	return nil
}

// peerUnpair serves POST /p/unpair: a contact unpaired me, so I drop them too.
func (d *Daemon) peerUnpair(w http.ResponseWriter, r *http.Request) {
	p, ok := session.PeerFrom(r.Context())
	if !ok || p.Relation != session.Contact {
		sol.WriteError(w, sol.Errorf("forbidden", "not a contact"))
		return
	}
	w.WriteHeader(http.StatusNoContent)
	go func() {
		if err := d.dropContact(p.Person); err == nil {
			d.activity("sol.people.unpaired-by", map[string]string{"person": identity.ID(p.Person)}, nil)
		}
	}()
}

// Devices lists my devices.
func (d *Daemon) Devices() []DeviceInfo {
	out := []DeviceInfo{}
	b, err := d.Keyring.Bundle()
	if err != nil {
		return out
	}
	mine := d.Keyring.DevicePublic()
	for _, dv := range b.Devices.Devices {
		this := bytes.Equal(dv.Key, mine)
		di := DeviceInfo{ID: identity.ID(dv.Key), Name: dv.Name, Admin: dv.Admin(), Anchor: dv.Anchor(), This: this}
		if dv.Added > 0 {
			di.Added = time.Unix(int64(dv.Added), 0).UTC()
		}
		if s, ok := d.Sessions.Session(dv.Key); this || ok && s.Alive() {
			di.Online = true
		}
		d.mu.Lock()
		di.LastSeen = d.lastSeen[di.ID]
		d.mu.Unlock()
		if di.Online {
			di.LastSeen = time.Now().UTC()
		}
		out = append(out, di)
	}
	return out
}

// DevicePatch changes one of my devices; nil fields stay.
type DevicePatch struct {
	Name   *string `json:"name"`
	Anchor *bool   `json:"anchor"`
	Admin  *bool   `json:"admin"`
}

// UpdateDevice renames a device or marks it an anchor (admin devices only: it re-signs my
// device list). Changing who is an admin needs handing P over, which is not built yet.
func (d *Daemon) UpdateDevice(id string, p DevicePatch) (DeviceInfo, error) {
	if !d.Keyring.IsAdmin() {
		return DeviceInfo{}, sol.Errorf("forbidden", "only an admin device can change the device list")
	}
	pub, err := identity.ParseID(id)
	if err != nil {
		return DeviceInfo{}, sol.Errorf("not_found", "no such device")
	}
	b, err := d.Keyring.Bundle()
	if err != nil {
		return DeviceInfo{}, err
	}
	dv, ok := b.Devices.Find(pub)
	if !ok {
		return DeviceInfo{}, sol.Errorf("not_found", "no such device")
	}
	if p.Admin != nil && *p.Admin != dv.Admin() {
		return DeviceInfo{}, sol.Errorf("not_implemented", "making a device an admin (or not) needs handing over the person key, which is not built yet")
	}
	flags := dv.Flags
	if p.Anchor != nil {
		if *p.Anchor {
			flags |= identity.FlagAnchor
		} else {
			flags &^= identity.FlagAnchor
		}
	}
	name := dv.Name
	if p.Name != nil {
		name = strings.TrimSpace(*p.Name)
		if name == "" || len(name) > 64 {
			return DeviceInfo{}, sol.Errorf("bad_request", "a device name is 1-64 characters")
		}
	}
	if name == dv.Name && flags == dv.Flags {
		return d.device(id), nil
	}
	if _, err := d.Keyring.AddDevice(pub, name, flags, time.Now()); err != nil {
		return DeviceInfo{}, err
	}
	d.Sessions.BundleChanged() // contacts and my other devices see the new name or flags now
	di := d.device(id)
	d.emit(EvDevice, di)
	return di, nil
}

// device returns one of my devices' info (zero if gone).
func (d *Daemon) device(id string) DeviceInfo {
	for _, di := range d.Devices() {
		if di.ID == id {
			return di
		}
	}
	return DeviceInfo{}
}

// RemoveDevice removes a device from my device list (admin devices only).
func (d *Daemon) RemoveDevice(id string) error {
	if !d.Keyring.IsAdmin() {
		return sol.Errorf("forbidden", "only an admin device can change the device list")
	}
	pub, err := identity.ParseID(id)
	if err != nil {
		return sol.Errorf("not_found", "no such device")
	}
	if _, err := d.Sessions.RemoveDevice(pub); err != nil {
		switch {
		case errors.Is(err, identity.ErrSelf):
			return sol.Errorf("bad_request", "this device cannot remove itself")
		case errors.Is(err, identity.ErrNotInList):
			return sol.Errorf("not_found", "no such device")
		}
		return err
	}
	d.emit(EvDevice, removed("id", id))
	return nil
}

// peerHost serves http://<name>.<petname>.localhost:<port>/ (apphost.Config.Peer): the
// contact's HTTP service if they offer one by that name, else their app's UI.
func (d *Daemon) peerHost(w http.ResponseWriter, r *http.Request, name, pet, origin string) {
	var who *PersonInfo
	for _, p := range d.People() {
		if p.Petname == pet {
			p := p
			who = &p
			break
		}
	}
	if who == nil {
		http.Error(w, "no contact called "+pet, http.StatusNotFound)
		return
	}
	for _, s := range d.ports.Offered(who.ID) {
		if s.Name == name {
			if !s.HTTP {
				http.Error(w, name+" is not a web service; open it with sol tunnel open", http.StatusNotFound)
				return
			}
			d.ports.serveHTTPService(w, r, who.ID, s)
			return
		}
	}
	d.peerApp(w, r, who.ID, name, origin)
}

// peerApp proxies a contact's app UI (API.md §10) over a session to /p/ui/<app>/... The kit,
// sol.js and the theme are served locally; the /_sol/ bridge is not offered on a peer origin.
func (d *Daemon) peerApp(w http.ResponseWriter, r *http.Request, person, app, origin string) {
	if d.Host.ServeShared(w, r) {
		return
	}
	if strings.HasPrefix(r.URL.Path, "/_sol/") {
		sol.WriteError(w, sol.Errorf("not_implemented", "a contact's app cannot call Sol's API from this device yet"))
		return
	}
	pub, err := identity.ParseID(person)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	_, dl, ok := d.Cache.Latest(pub)
	if !ok {
		http.Error(w, "their devices are not known yet", http.StatusServiceUnavailable)
		return
	}
	var ss *session.Session
	for _, dv := range dl.Devices {
		if s, ok := d.Sessions.Session(dv.Key); ok && s.Alive() {
			ss = s
			break
		}
	}
	if ss == nil {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		for _, dv := range dl.Devices {
			if s, err := d.Sessions.Connect(ctx, pub, dv.Key); err == nil {
				ss = s
				break
			}
		}
		cancel()
	}
	if ss == nil {
		http.Error(w, "they are offline", http.StatusBadGateway)
		return
	}
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.Out.URL = &url.URL{Scheme: "http", Host: "peer", Path: appapi.PeerUIPath + app + pr.In.URL.Path, RawQuery: pr.In.URL.RawQuery}
			pr.Out.Host = "peer"
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del("Authorization")
		},
		Transport: ss.HTTPClient().Transport,
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Del("Content-Security-Policy") // the router's policy is the only one
			resp.Header.Del("Set-Cookie")
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			http.Error(w, "their app is not answering", http.StatusBadGateway)
		},
	}
	rp.ServeHTTP(w, r)
}
