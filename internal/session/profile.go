package session

// Profiles on the wire (design/PROTOCOL.md §Session endpoints, IDENTITY.md §Profile): a person's
// display name and picture, signed by P, travelling on their own endpoints because the bundle
// must stay three parts for older devices.
//
//   - GET /p/profile answers my profile in the form the caller may see: a buddy gets the full
//     form, an anonymous contact the form without a picture, my own devices both. POST
//     /p/profile pushes profiles; the body of both is one or more `u32 len | profile` records
//     (404 from GET when I have none).
//   - When a session comes up each side fetches the other's profile; between my own devices
//     each side also pushes every contact profile it holds (after the contact bundles, so the
//     receiver can verify them). When my profile changes here (set on this device, or learned
//     from a sibling) it is pushed to every open session; a contact's new profile learned from
//     them is relayed to my other devices. Only news is passed on, so nothing loops.
//   - A profile counts only under the P of the newest person certificate known for its person;
//     one signed by an older P is ignored (and mine is re-signed by an admin device that holds
//     the new P).
//   - GET /p/avatar/{hash} serves a picture: to my own devices any picture this device holds,
//     to a buddy only my current or previous picture, to an anonymous contact never. A picture
//     named by a profile that is news is fetched from the device that sent it, checked against
//     its hash, its magic bytes and MaxAvatarSize, and kept per hash.
//   - Only an admin device holds P. A device without it forwards a change to an admin device
//     of mine (POST /p/profile/edit, my own devices only), which signs, stores and pushes it
//     and answers the new records; with no admin device reachable the change fails with
//     ErrNoAdmin, naming them.

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/apv022/sol/internal/fsutil"
	"github.com/apv022/sol/internal/identity"
)

// Profile endpoints.
const (
	ProfilePath     = "/p/profile"
	ProfileEditPath = "/p/profile/edit"
	AvatarPath      = "/p/avatar/"
)

// maxProfileBody bounds a POST /p/profile body (many contacts relayed at once).
const (
	maxProfileRecord = 4 << 10
	maxProfileBody   = 4 << 20
)

// ProfileTimeout bounds one profile exchange (push, fetch, forward) with one device.
var ProfileTimeout = 30 * time.Second

// ErrNoAdmin is returned by SetProfile on a device without P when no admin device answered.
var ErrNoAdmin = errors.New("session: only an admin device can sign your profile")

// Profiles stores the newest profile of me and of every contact, and the pictures they name:
// profiles.json (0600, atomic) and avatars/<hex> (0600, directory 0700) under its directory, or
// only memory when the directory is "". Safe for concurrent use. It stores what it is given;
// the Manager verifies everything first.
type Profiles struct {
	dir     string
	mu      sync.Mutex
	people  map[string]*profileEntry // by person ID
	avatars map[string][]byte        // memory mode only
}

type profileEntry struct {
	Profile []byte `json:"profile"`
	// Anon is my own profile's form for anonymous contacts (mine only; my devices without P
	// cannot sign it, so it travels with the full form).
	Anon []byte `json:"anon,omitempty"`
	// Prev is my previous picture, still served to contacts who have not caught up.
	Prev string `json:"prev_avatar,omitempty"`
}

// NewProfiles opens the store in dir ("" for memory only).
func NewProfiles(dir string) (*Profiles, error) {
	p := &Profiles{dir: dir, people: map[string]*profileEntry{}, avatars: map[string][]byte{}}
	if dir == "" {
		return p, nil
	}
	b, err := os.ReadFile(filepath.Join(dir, "profiles.json"))
	if errors.Is(err, os.ErrNotExist) {
		return p, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &p.people); err != nil {
		return nil, errors.New("session: profiles.json is corrupt")
	}
	for k, e := range p.people {
		if e == nil {
			delete(p.people, k)
		}
	}
	return p, nil
}

// entry returns a copy of what is stored for person.
func (p *Profiles) entry(person ed25519.PublicKey) (profileEntry, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	e, ok := p.people[identity.ID(person)]
	if !ok {
		return profileEntry{}, false
	}
	return *e, true
}

// put stores person's profile (and, for me, the anonymous form and previous picture), and drops
// the picture it replaced unless something still names it.
func (p *Profiles) put(person ed25519.PublicKey, e profileEntry) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	id := identity.ID(person)
	var old []string
	if cur := p.people[id]; cur != nil {
		if cp, err := identity.ParseProfile(cur.Profile); err == nil {
			old = append(old, cp.Avatar)
		}
		old = append(old, cur.Prev)
	}
	p.people[id] = &e
	if err := p.saveLocked(); err != nil {
		return err
	}
	for _, h := range old {
		if h != "" && !p.namedLocked(h) {
			p.dropAvatarLocked(h)
		}
	}
	return nil
}

// forget drops what is stored for person (an unpaired contact).
func (p *Profiles) forget(person ed25519.PublicKey) {
	p.mu.Lock()
	defer p.mu.Unlock()
	id := identity.ID(person)
	cur := p.people[id]
	if cur == nil {
		return
	}
	delete(p.people, id)
	_ = p.saveLocked()
	if cp, err := identity.ParseProfile(cur.Profile); err == nil && cp.Avatar != "" && !p.namedLocked(cp.Avatar) {
		p.dropAvatarLocked(cp.Avatar)
	}
}

func (p *Profiles) namedLocked(h string) bool {
	for _, e := range p.people {
		if e.Prev == h {
			return true
		}
		if cp, err := identity.ParseProfile(e.Profile); err == nil && cp.Avatar == h {
			return true
		}
	}
	return false
}

func (p *Profiles) saveLocked() error {
	if p.dir == "" {
		return nil
	}
	b, _ := json.Marshal(p.people)
	return fsutil.WriteFileAtomic(filepath.Join(p.dir, "profiles.json"), b, 0o600)
}

func (p *Profiles) avatarFile(h string) string {
	return filepath.Join(p.dir, "avatars", strings.TrimPrefix(h, "sha256:"))
}

// Avatar returns a stored picture and its media type.
func (p *Profiles) Avatar(h string) ([]byte, string, bool) {
	if !identity.ValidAvatarHash(h) {
		return nil, "", false
	}
	p.mu.Lock()
	var b []byte
	if p.dir == "" {
		b = p.avatars[h]
	}
	p.mu.Unlock()
	if p.dir != "" {
		var err error
		if b, err = os.ReadFile(p.avatarFile(h)); err != nil {
			return nil, "", false
		}
	}
	if b == nil || identity.AvatarHash(b) != h {
		return nil, "", false
	}
	t, err := identity.AvatarType(b)
	if err != nil {
		return nil, "", false
	}
	return b, t, true
}

func (p *Profiles) has(h string) bool {
	if !identity.ValidAvatarHash(h) {
		return false
	}
	if p.dir == "" {
		p.mu.Lock()
		defer p.mu.Unlock()
		_, ok := p.avatars[h]
		return ok
	}
	st, err := os.Stat(p.avatarFile(h))
	return err == nil && st.Mode().IsRegular()
}

// putAvatar checks and stores a picture and returns its hash.
func (p *Profiles) putAvatar(b []byte) (string, error) {
	if _, err := identity.AvatarType(b); err != nil {
		return "", err
	}
	h := identity.AvatarHash(b)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.dir == "" {
		p.avatars[h] = bytes.Clone(b)
		return h, nil
	}
	dir := filepath.Join(p.dir, "avatars")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return h, fsutil.WriteFileAtomic(p.avatarFile(h), b, 0o600)
}

func (p *Profiles) dropAvatarLocked(h string) {
	if !identity.ValidAvatarHash(h) {
		return
	}
	if p.dir == "" {
		delete(p.avatars, h)
		return
	}
	_ = os.Remove(p.avatarFile(h))
}

// ---------- the Manager's side ----------

// certFor returns the newest verified person certificate known for person.
func (m *Manager) certFor(person ed25519.PublicKey) (identity.PersonCert, bool) {
	if me := m.kr.Person(); me != nil && bytes.Equal(person, me) {
		b, err := m.kr.Bundle()
		return b.Person, err == nil
	}
	pc, _, ok := m.cache.Latest(person)
	return pc, ok
}

// stored returns person's stored profiles, valid under their newest certificate. anon is the
// form for anonymous contacts (zero when this device holds none, e.g. a contact's profile).
func (m *Manager) stored(person ed25519.PublicKey) (full, anon identity.Profile, prev string, ok bool) {
	e, found := m.profiles.entry(person)
	if !found {
		return
	}
	pc, known := m.certFor(person)
	if !known {
		return
	}
	full, err := identity.ParseProfile(e.Profile)
	if err != nil || full.Verify(pc) != nil {
		return identity.Profile{}, identity.Profile{}, "", false
	}
	if full.Anonymous() {
		anon = full
	} else if len(e.Anon) > 0 {
		if a, err := identity.ParseProfile(e.Anon); err == nil && a.Verify(pc) == nil && a.Seq == full.Seq && a.Anonymous() && a.Name == full.Name {
			anon = a
		}
	}
	return full, anon, e.Prev, true
}

// Profile returns the newest valid profile of person (me or a contact).
func (m *Manager) Profile(person ed25519.PublicKey) (identity.Profile, bool) {
	full, _, _, ok := m.stored(person)
	return full, ok
}

// resignProfile re-signs my profile on an admin device when the P that signed it was rotated
// away (it no longer verifies under my newest person certificate), so it counts again.
func (m *Manager) resignProfile() {
	me := m.kr.Person()
	if me == nil || !m.kr.IsAdmin() {
		return
	}
	m.profMu.Lock()
	defer m.profMu.Unlock()
	if _, ok := m.Profile(me); ok {
		return
	}
	e, found := m.profiles.entry(me)
	if !found {
		return
	}
	if old, err := identity.ParseProfile(e.Profile); err == nil && bytes.Equal(old.Person, me) {
		if _, err := m.signMine(old.Seq, old.Name, old.Avatar, e.Prev); err != nil {
			m.log.Debug("re-signing my profile failed", "err", err)
		}
	}
}

// HasAvatar reports whether this device holds the picture (cheap: no read).
func (m *Manager) HasAvatar(hash string) bool { return m.profiles.has(hash) }

// Avatar returns a picture this device holds and its media type.
func (m *Manager) Avatar(hash string) ([]byte, string, bool) { return m.profiles.Avatar(hash) }

// ForgetProfile drops what is stored about person (call it after unpairing).
func (m *Manager) ForgetProfile(person ed25519.PublicKey) { m.profiles.forget(person) }

// ProfileChange is a change to my profile; nil or false fields stay as they are.
type ProfileChange struct {
	Name        *string `json:"name,omitempty"`
	Avatar      []byte  `json:"avatar,omitempty"` // a new picture (JPEG, PNG or WebP, at most MaxAvatarSize)
	ClearAvatar bool    `json:"clear_avatar,omitempty"`
}

func (c ProfileChange) check() error {
	if c.Name != nil {
		if len(*c.Name) > identity.MaxNameLen {
			return fmt.Errorf("%w: a name is at most %d bytes", identity.ErrInvalid, identity.MaxNameLen)
		}
	}
	if c.Avatar != nil {
		if c.ClearAvatar {
			return fmt.Errorf("%w: set a picture or clear it, not both", identity.ErrInvalid)
		}
		if _, err := identity.AvatarType(c.Avatar); err != nil {
			return err
		}
	}
	return nil
}

// SetProfile changes my profile and returns the new one. An admin device signs it with P,
// stores it and pushes it to every open session; any other device forwards the change to an
// admin device of mine (ErrNoAdmin, naming them, when none answers).
func (m *Manager) SetProfile(ctx context.Context, c ProfileChange) (identity.Profile, error) {
	if err := c.check(); err != nil {
		return identity.Profile{}, err
	}
	if m.kr.IsAdmin() {
		return m.applyChange(c)
	}
	if c.Avatar != nil {
		// Kept here as well, so this device can show and serve it at once.
		if _, err := m.profiles.putAvatar(c.Avatar); err != nil {
			return identity.Profile{}, err
		}
	}
	return m.forwardChange(ctx, c)
}

// applyChange signs and stores a change on this admin device, then pushes it.
func (m *Manager) applyChange(c ProfileChange) (identity.Profile, error) {
	me := m.kr.Person()
	if me == nil {
		return identity.Profile{}, identity.ErrNoPerson
	}
	m.profMu.Lock()
	defer m.profMu.Unlock()
	// What this device stored, valid or not (signed by a rotated P): the change builds on it and
	// its seq, so the new profile is above everything this device has seen.
	var cur identity.Profile
	prev := ""
	if e, ok := m.profiles.entry(me); ok {
		if p, err := identity.ParseProfile(e.Profile); err == nil && bytes.Equal(p.Person, me) {
			cur, prev = p, e.Prev
		}
	}
	_, valid := m.Profile(me)
	name, avatar := cur.Name, cur.Avatar
	if c.Name != nil {
		name = strings.TrimSpace(*c.Name)
	}
	switch {
	case c.ClearAvatar:
		avatar = ""
	case c.Avatar != nil:
		h, err := m.profiles.putAvatar(c.Avatar)
		if err != nil {
			return identity.Profile{}, err
		}
		avatar = h
	}
	if valid && name == cur.Name && avatar == cur.Avatar {
		return cur, nil
	}
	if cur.Avatar != "" && cur.Avatar != avatar {
		prev = cur.Avatar
	}
	return m.signMine(cur.Seq, name, avatar, prev)
}

// signMine signs my profile at the seq after prevSeq, stores both forms and pushes them.
func (m *Manager) signMine(prevSeq uint64, name, avatar, prev string) (identity.Profile, error) {
	full, anon, err := m.kr.SignProfile(prevSeq, name, avatar, time.Now())
	if err != nil {
		return identity.Profile{}, err
	}
	if prev == avatar {
		prev = ""
	}
	if err := m.profiles.put(full.Person, profileEntry{Profile: full.Marshal(), Anon: anon.Marshal(), Prev: prev}); err != nil {
		return identity.Profile{}, err
	}
	m.profileNews(full.Person, nil)
	return full, nil
}

// forwardChange asks an admin device of mine to make the change (this device has no P).
func (m *Manager) forwardChange(ctx context.Context, c ProfileChange) (identity.Profile, error) {
	b, err := m.kr.Bundle()
	if err != nil {
		return identity.Profile{}, err
	}
	body, _ := json.Marshal(c)
	var names []string
	var errs []error
	for _, d := range b.Devices.Devices {
		if !d.Admin() || bytes.Equal(d.Key, m.kr.DevicePublic()) {
			continue
		}
		names = append(names, d.Name)
		cctx, cancel := context.WithTimeout(ctx, ProfileTimeout)
		p, err := m.forwardTo(cctx, b.Person.Person, d.Key, body)
		cancel()
		if err == nil {
			return p, nil
		}
		errs = append(errs, err)
	}
	if len(names) == 0 {
		return identity.Profile{}, fmt.Errorf("%w, and you have none left: restore one from your recovery words", ErrNoAdmin)
	}
	m.log.Debug("forwarding a profile change failed", "err", errors.Join(errs...))
	return identity.Profile{}, fmt.Errorf("%w (%s), and none is reachable now: try again when it is on", ErrNoAdmin, strings.Join(names, ", "))
}

func (m *Manager) forwardTo(ctx context.Context, me, dev ed25519.PublicKey, body []byte) (identity.Profile, error) {
	s, err := m.Connect(ctx, me, dev)
	if err != nil {
		return identity.Profile{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://peer"+ProfileEditPath, bytes.NewReader(body))
	if err != nil {
		return identity.Profile{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return identity.Profile{}, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxProfileBody))
	if err != nil {
		return identity.Profile{}, err
	}
	if resp.StatusCode != http.StatusOK {
		return identity.Profile{}, fmt.Errorf("session: POST %s: %s: %s", ProfileEditPath, resp.Status, strings.TrimSpace(string(raw)))
	}
	ps, err := parseProfiles(raw)
	if err != nil {
		return identity.Profile{}, err
	}
	m.takeProfiles(s, ps)
	p, ok := m.Profile(me)
	if !ok {
		return identity.Profile{}, errors.New("session: the admin device answered a profile this device cannot verify")
	}
	return p, nil
}

// serveEdit handles POST /p/profile/edit: a device of mine without P asks this one to sign.
func (m *Manager) serveEdit(w http.ResponseWriter, r *http.Request) {
	from, ok := PeerFrom(r.Context())
	if !ok || from.Relation != Self {
		http.Error(w, "my own devices only", http.StatusForbidden)
		return
	}
	if !m.kr.IsAdmin() {
		http.Error(w, "this device does not hold the person key", http.StatusConflict)
		return
	}
	var c ProfileChange
	if err := json.NewDecoder(io.LimitReader(r.Body, 2*identity.MaxAvatarSize)).Decode(&c); err != nil {
		http.Error(w, "bad JSON", http.StatusBadRequest)
		return
	}
	if err := c.check(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if _, err := m.applyChange(c); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(frameProfiles(m.profilesFor(from)))
}

// profilesFor returns my profile records in the forms peer may see.
func (m *Manager) profilesFor(peer Peer) [][]byte {
	me := m.kr.Person()
	if me == nil {
		return nil
	}
	full, anon, _, ok := m.stored(me)
	switch {
	case !ok:
		return nil
	case peer.Relation == Self:
		out := [][]byte{full.Marshal()}
		if anon.Seq != 0 {
			if a := anon.Marshal(); !bytes.Equal(a, out[0]) {
				out = append(out, a)
			}
		}
		return out
	case m.Anonymous(peer.Person):
		if anon.Seq == 0 {
			return nil // never the picture
		}
		return [][]byte{anon.Marshal()}
	}
	return [][]byte{full.Marshal()}
}

func frameProfiles(recs [][]byte) []byte {
	var body []byte
	for _, r := range recs {
		body = binary.BigEndian.AppendUint32(body, uint32(len(r)))
		body = append(body, r...)
	}
	return body
}

func parseProfiles(body []byte) ([]identity.Profile, error) {
	var out []identity.Profile
	for len(body) > 0 {
		if len(body) < 4 {
			return nil, errors.New("session: malformed profile records")
		}
		n := binary.BigEndian.Uint32(body)
		if n == 0 || n > maxProfileRecord || uint64(n) > uint64(len(body)-4) {
			return nil, errors.New("session: malformed profile records")
		}
		p, err := identity.ParseProfile(body[4 : 4+n])
		if err != nil {
			return nil, err
		}
		out = append(out, p)
		body = body[4+n:]
	}
	return out, nil
}

// serveProfile handles GET /p/profile.
func (m *Manager) serveProfile(w http.ResponseWriter, r *http.Request) {
	from, _ := PeerFrom(r.Context())
	recs := m.profilesFor(from)
	if len(recs) == 0 {
		http.Error(w, "no profile", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Write(frameProfiles(recs))
}

// servePushProfile handles POST /p/profile.
func (m *Manager) servePushProfile(w http.ResponseWriter, r *http.Request) {
	from, ok := PeerFrom(r.Context())
	if !ok {
		http.Error(w, "no peer", http.StatusForbidden)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxProfileBody+1))
	if err != nil {
		return
	}
	if len(body) > maxProfileBody {
		http.Error(w, "too large", http.StatusRequestEntityTooLarge)
		return
	}
	ps, err := parseProfiles(body)
	if err != nil {
		http.Error(w, "malformed", http.StatusBadRequest)
		return
	}
	s, _ := m.Session(from.Device)
	m.takeProfiles(s, ps)
	w.WriteHeader(http.StatusNoContent)
}

// serveAvatar handles GET /p/avatar/{hash}.
func (m *Manager) serveAvatar(w http.ResponseWriter, r *http.Request) {
	from, _ := PeerFrom(r.Context())
	h := r.PathValue("hash")
	allowed := false
	switch {
	case from.Relation == Self:
		allowed = true // my own devices: anything this one holds (mine and contacts' pictures)
	case from.Relation == Contact && !m.Anonymous(from.Person):
		if me := m.kr.Person(); me != nil {
			full, _, prev, ok := m.stored(me)
			allowed = ok && h != "" && (h == full.Avatar || h == prev)
		}
	}
	if !allowed {
		http.Error(w, "not yours to see", http.StatusForbidden) // decided before looking it up
		return
	}
	b, t, ok := m.profiles.Avatar(h)
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", t)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Write(b)
}

// takeProfiles folds profiles that arrived over s (nil when the session is already gone), then
// passes news on and fetches the pictures they name, in the background.
func (m *Manager) takeProfiles(s *Session, ps []identity.Profile) {
	if s == nil {
		return
	}
	var errs []error
	for _, p := range ps {
		if err := m.takeProfile(s, p); err != nil {
			errs = append(errs, err)
		}
	}
	if len(errs) > 0 {
		m.log.Debug("ignored profiles", "peer", short(s.peer.Device), "n", len(errs), "err", errors.Join(errs...))
	}
}

// takeProfile folds one profile from the peer of s. A peer may send its own person's profile;
// only my own devices may send mine or relay a contact's.
func (m *Manager) takeProfile(s *Session, p identity.Profile) error {
	from := s.peer
	me := m.kr.Person()
	mine := me != nil && bytes.Equal(p.Person, me)
	switch {
	case mine && from.Relation != Self:
		return ErrNotRelated
	case !mine && !bytes.Equal(p.Person, from.Person) && from.Relation != Self:
		return ErrNotRelated
	case !mine && !m.cfg.Contacts.IsContact(p.Person):
		return ErrNotRelated
	}
	pc, ok := m.certFor(p.Person)
	if !ok {
		return fmt.Errorf("profile: no certificate known for its person")
	}
	if err := p.Verify(pc); err != nil {
		return fmt.Errorf("profile: %w", err)
	}
	m.profMu.Lock()
	news, err := m.foldProfile(p, mine)
	m.profMu.Unlock()
	if err != nil {
		return err
	}
	if p.Avatar != "" {
		if _, _, held := m.profiles.Avatar(p.Avatar); !held {
			m.goBG(func() { m.fetchAvatar(s, p.Person, p.Avatar) })
		}
	}
	if news {
		m.profileNews(p.Person, s)
	}
	return nil
}

// foldProfile stores p if it is news. Callers hold profMu and verified p.
func (m *Manager) foldProfile(p identity.Profile, mine bool) (bool, error) {
	full, anon, prev, ok := m.stored(p.Person)
	if !ok {
		e := profileEntry{Profile: p.Marshal()}
		if mine && p.Anonymous() {
			e.Anon = e.Profile
		}
		return true, m.profiles.put(p.Person, e)
	}
	if mine && p.Anonymous() && p.Seq == full.Seq && !full.Anonymous() {
		// The anonymous form of my current profile: kept for my anonymous contacts.
		if bytes.Equal(anon.Marshal(), p.Marshal()) || p.Name != full.Name {
			return false, nil
		}
		return false, m.profiles.put(p.Person, profileEntry{Profile: full.Marshal(), Anon: p.Marshal(), Prev: prev})
	}
	n, err := identity.CompareProfile(full, p)
	if err != nil || n == 0 {
		return false, err
	}
	e := profileEntry{Profile: p.Marshal()}
	if mine {
		if p.Anonymous() {
			e.Anon = e.Profile
		}
		e.Prev = prev
		if full.Avatar != "" && full.Avatar != p.Avatar {
			e.Prev = full.Avatar
		}
		if e.Prev == p.Avatar {
			e.Prev = ""
		}
	}
	return true, m.profiles.put(p.Person, e)
}

// profileNews passes on a profile that was news here (from is where it came from; nil when it
// was made on this device): mine goes to every open session in the form each may see, a
// contact's learned from them goes to my other devices. Then Config.OnProfile is told.
func (m *Manager) profileNews(person ed25519.PublicKey, from *Session) {
	if m.cfg.OnProfile != nil {
		m.cfg.OnProfile(person)
	}
	me := m.kr.Person()
	var targets []*Session
	for _, s := range m.Sessions() {
		if s == from {
			continue
		}
		switch {
		case me != nil && bytes.Equal(person, me):
			targets = append(targets, s)
		case s.peer.Relation == Self && (from == nil || from.peer.Relation != Self):
			targets = append(targets, s)
		}
	}
	if len(targets) == 0 {
		return
	}
	m.goBG(func() {
		var wg sync.WaitGroup
		for _, s := range targets {
			var recs [][]byte
			if me != nil && bytes.Equal(person, me) {
				recs = m.profilesFor(s.peer)
			} else if full, _, _, ok := m.stored(person); ok {
				recs = [][]byte{full.Marshal()}
			}
			if len(recs) == 0 {
				continue
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				m.pushProfiles(s, recs)
			}()
		}
		wg.Wait()
	})
}

// pushProfiles sends profile records to the peer of s (POST /p/profile).
func (m *Manager) pushProfiles(s *Session, recs [][]byte) {
	ctx, cancel := context.WithTimeout(m.ctx, ProfileTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://peer"+ProfilePath, bytes.NewReader(frameProfiles(recs)))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := s.client.Do(req)
	if err != nil {
		if m.ctx.Err() == nil {
			m.log.Debug("pushing a profile failed", "peer", short(s.peer.Device), "err", err)
		}
		return
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
}

// syncProfiles runs when a session comes up: fetch the peer's profile records, and give a
// sibling every contact profile this device holds (after the contacts' bundles, which
// relayAll sent first, so it can verify them).
func (m *Manager) syncProfiles(s *Session) {
	ctx, cancel := context.WithTimeout(m.ctx, ProfileTimeout)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://peer"+ProfilePath, nil)
	if err == nil {
		if resp, err := s.client.Do(req); err == nil {
			raw, err := io.ReadAll(io.LimitReader(resp.Body, maxProfileBody))
			resp.Body.Close()
			if err == nil && resp.StatusCode == http.StatusOK {
				if ps, err := parseProfiles(raw); err == nil {
					m.takeProfiles(s, ps)
				}
			}
		}
	}
	cancel()
	if s.peer.Relation != Self {
		return
	}
	var recs [][]byte
	m.profiles.mu.Lock()
	ids := make([]string, 0, len(m.profiles.people))
	for id := range m.profiles.people {
		ids = append(ids, id)
	}
	m.profiles.mu.Unlock()
	for _, id := range ids {
		pub, err := identity.ParseID(id)
		if err != nil || !m.cfg.Contacts.IsContact(pub) {
			continue
		}
		if full, _, _, ok := m.stored(pub); ok {
			recs = append(recs, full.Marshal())
		}
	}
	for len(recs) > 0 {
		n := min(len(recs), 512)
		m.pushProfiles(s, recs[:n])
		recs = recs[n:]
	}
}

// fetchAvatar gets a picture named by person's profile from the peer of s and keeps it if it
// matches its hash and is an allowed image.
func (m *Manager) fetchAvatar(s *Session, person ed25519.PublicKey, h string) {
	if !identity.ValidAvatarHash(h) {
		return
	}
	ctx, cancel := context.WithTimeout(m.ctx, ProfileTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://peer"+AvatarPath+h, nil)
	if err != nil {
		return
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, identity.MaxAvatarSize+1))
	if err != nil || resp.StatusCode != http.StatusOK {
		return
	}
	if identity.AvatarHash(b) != h {
		m.log.Debug("a fetched picture does not match its hash", "peer", short(s.peer.Device))
		return
	}
	if _, err := m.profiles.putAvatar(b); err != nil {
		return
	}
	if m.cfg.OnProfile != nil {
		m.cfg.OnProfile(person)
	}
}
