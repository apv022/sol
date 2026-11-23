// Package calendar is the Calendar app: calendars of events with time zones, recurrence,
// alerts, sharing, invitations and .ics import and export, kept in Sol feeds
// (design/apps/CALENDAR.md).
//
// Each calendar is one feed, owned by the person who created it, whose entries are batches of
// crdt.Map ops; its state is the fold of every op in the feeds the app may read. A write share
// makes the other person's same-named feed a co-author feed, which the owner folds in (and relays
// into its own feed) only for people it granted write. Invitations travel in two more feeds per
// pair of people: the organiser's inv.<invitee> (event snapshots) and the invitee's
// rsvp.<organiser> (answers), each shared read-only with the other person.
//
// Invariants:
//   - feed content is untrusted: every op is validated, owner-only keys are honoured only from
//     the owner's feed, co-author ops only with a write grant, an inv. or rsvp. feed only when it
//     names me and comes from someone else;
//   - state is rebuilt from the feeds alone, so folding is idempotent and order-independent;
//     nothing is polled: feeds are read at start and then follow the events stream;
//   - alerts use one timer for the earliest unfired one, re-armed after each change;
//   - recurrence is expanded on demand in the event's own zone (times.go, rrule.go);
//   - every action requires a Sol-Self caller; the UI uses only actions.
package calendar

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/apv022/sol/sdk/go/crdt"
	"github.com/apv022/sol/sdk/go/sol"
)

// ManifestJSON is the app's app.json.
//
//go:embed app.json
var ManifestJSON []byte

const appID = "calendar"

// Clock is the app's time source, injectable so tests can drive alerts.
type Clock interface {
	Now() time.Time
	// AfterFunc calls f after d and returns a function that stops it.
	AfterFunc(d time.Duration, f func()) (stop func() bool)
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }
func (realClock) AfterFunc(d time.Duration, f func()) func() bool {
	return time.AfterFunc(d, f).Stop
}

// Config is how a Calendar instance is set up.
type Config struct {
	// DataDir holds fired alerts, asked invitations and hidden calendars. Empty means
	// $SOL_APP_DATA; none at all keeps them in memory.
	DataDir string
	// Listener is where the UI and actions are served; nil means sol.Serve (SOL_APP_LISTEN).
	Listener net.Listener
	// Clock overrides the time source (tests).
	Clock Clock
	// Zone is the device's IANA zone name; empty means found from the system (or the Go local zone).
	Zone string
	// Logf receives diagnostics; nil discards them. Never given event text.
	Logf func(format string, args ...any)
}

type feedKind int

const (
	kindCal feedKind = iota
	kindInv
	kindRsvp
)

type feedBuf struct {
	addr, person, feed string
	kind               feedKind
	about              string // inv./rsvp.: the person the feed is for
	ops                []crdt.MapOp
	seen               map[string]struct{} // key|stamp of every op in an owner feed of mine (for relaying)
	owner              bool                // cal.: contains c.owner naming its own person
	revoked            bool
	cursor             uint64
}

// invite is an event someone invited me to.
type invite struct {
	ref              string // <organiser>/<calendar>/<event>
	from, cal, event string
	snap             snapshot
	ev               *Event
	status           string
}

// App is one running Calendar instance.
type App struct {
	api      sol.API
	cfg      Config
	manifest *sol.Manifest
	clock    Clock
	loc      *time.Location
	zone     string
	data     string

	me, dev string
	crdtClk *crdt.Clock

	pullMu sync.Mutex // serialises feed reads so each entry is folded once
	mu     sync.RWMutex
	feeds  map[string]*feedBuf
	cals   map[string]*calState // by owner feed address
	// invites to me, by ref; myInv: what my inv.<person> feeds hold (person → key → raw);
	// answers: rsvps to my invitations (cal.event → person → status); mine: my own rsvps (ref → status).
	invites map[string]*invite
	myInv   map[string]map[string]json.RawMessage
	answers map[string]map[string]string
	mine    map[string]string
	dirty   bool
	rev     uint64
	hidden  map[string]bool // calendars hidden on this device, and calendars I left
	left    map[string]bool
	back    map[string]bool // feeds already shared with their reader
	names   map[string]string

	subMu    sync.Mutex // subscription files (subscribe.go)
	subWrite sync.Mutex
	subTimer *time.Timer
	subStop  bool
	subFiles []subFile

	noteMu sync.Mutex
	fired  map[string]int64  // alert key → when
	asked  map[string]string // invite ref → inbox item id
	told   map[string]string // answer key → status I was told about
	primed bool
	timer  func() bool

	noFollow bool // tests: do not follow the events stream, so the test decides when feeds are read
	started  bool
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	handler  http.Handler
}

// New creates a Calendar instance. Call Start (or Run) to begin.
func New(api sol.API, cfg Config) *App {
	m, err := sol.ParseManifest(ManifestJSON)
	if err != nil {
		panic("calendar: bad embedded app.json: " + err.Error())
	}
	if cfg.DataDir == "" {
		cfg.DataDir = os.Getenv("SOL_APP_DATA")
	}
	if cfg.Clock == nil {
		cfg.Clock = realClock{}
	}
	zone := cfg.Zone
	if zone == "" {
		zone = deviceZone()
	}
	loc := time.Local
	if zone != "" {
		if l, err := loadZone(zone, time.Local); err == nil {
			loc = l
		} else {
			zone = ""
		}
	}
	a := &App{api: api, cfg: cfg, manifest: m, clock: cfg.Clock, loc: loc, zone: zone, data: cfg.DataDir,
		feeds: map[string]*feedBuf{}, cals: map[string]*calState{}, invites: map[string]*invite{},
		myInv: map[string]map[string]json.RawMessage{}, answers: map[string]map[string]string{}, mine: map[string]string{},
		hidden: map[string]bool{}, left: map[string]bool{}, back: map[string]bool{}, names: map[string]string{},
		fired: map[string]int64{}, asked: map[string]string{}, told: map[string]string{},
		rev: uint64(time.Now().UnixMilli()) * 1000} // per process, so a UI's cached snapshot from an earlier run never looks current
	a.handler = a.buildHandler()
	return a
}

// Handler serves the UI and the action endpoints.
func (a *App) Handler() http.Handler { return a.handler }

func (a *App) logf(format string, args ...any) {
	if a.cfg.Logf != nil {
		a.cfg.Logf(format, args...)
	}
}

// now is the clock's time in the device zone.
func (a *App) now() time.Time { return a.clock.Now().In(a.loc) }

// Run starts the app and serves until ctx ends.
func (a *App) Run(ctx context.Context) error {
	if err := a.Start(ctx); err != nil {
		return err
	}
	defer a.Stop()
	var err error
	if a.cfg.Listener != nil {
		err = sol.ServeListener(ctx, a.cfg.Listener, a.api, a.handler)
	} else {
		err = sol.Serve(ctx, a.api, a.handler)
	}
	if ctx.Err() != nil {
		return nil
	}
	return err
}

// Start reads every feed, folds them, arms alerts and follows the events stream.
func (a *App) Start(ctx context.Context) error {
	if a.started {
		return errors.New("calendar: already started")
	}
	if a.api == nil {
		return errors.New("calendar: no API")
	}
	who, err := a.api.WhoAmI(ctx)
	if err != nil {
		return err
	}
	a.me, a.dev = who.Person.ID, who.Device.ID
	a.crdtClk = crdt.NewClock(a.dev)
	a.crdtClk.Wall = func() int64 { return a.clock.Now().UnixMilli() }
	if a.data != "" {
		if err := os.MkdirAll(a.data, 0o700); err != nil {
			return err
		}
		loadJSON(filepath.Join(a.data, "fired.json"), &a.fired)
		loadJSON(filepath.Join(a.data, "asked.json"), &a.asked)
		loadJSON(filepath.Join(a.data, "hidden.json"), &a.hidden)
		loadJSON(filepath.Join(a.data, "left.json"), &a.left)
		a.primed = loadJSON(filepath.Join(a.data, "told.json"), &a.told)
	}
	ctx, a.cancel = context.WithCancel(ctx)
	// Subscribe before the first read so nothing slips between them.
	var ev <-chan sol.Event
	if !a.noFollow {
		if ev, err = a.api.Events(ctx, sol.EventOptions{People: true, Inbox: true}); err != nil {
			a.cancel()
			return err
		}
	}
	a.loadNames(ctx)
	if err := a.sync(ctx); err != nil {
		a.cancel()
		return err
	}
	a.noteMu.Lock()
	a.primed = true
	a.noteMu.Unlock()
	a.started = true
	a.subMu.Lock()
	a.subStop = false
	a.subMu.Unlock()
	if ev != nil {
		a.wg.Add(1)
		go a.follow(ctx, ev)
	}
	return nil
}

// Stop ends the event loop and the alert timer.
func (a *App) Stop() {
	if !a.started {
		return
	}
	a.cancel()
	a.subClose()
	a.noteMu.Lock()
	if a.timer != nil {
		a.timer()
		a.timer = nil
	}
	a.noteMu.Unlock()
	a.wg.Wait()
	a.started = false
}

func loadJSON(path string, v any) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return json.Unmarshal(b, v) == nil
}

// saveJSON writes atomically with private permissions.
func saveJSON(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (a *App) save(name string, v any) {
	if a.data == "" {
		return
	}
	if err := saveJSON(filepath.Join(a.data, name), v); err != nil {
		a.logf("calendar: saving %s: %v", name, err)
	}
}

// follow folds feed events as they arrive.
func (a *App) follow(ctx context.Context, ev <-chan sol.Event) {
	defer a.wg.Done()
	for e := range ev {
		switch {
		case e.Type == sol.EventEntry && e.Entry != nil:
			_ = a.pull(ctx, e.Entry.Feed)
		case e.Type == sol.EventFeed && e.Feed != nil:
			a.setRevoked(e.Feed.Feed, !e.Feed.Readable)
			if e.Feed.Readable {
				_ = a.pull(ctx, e.Feed.Feed)
			}
		case e.Type == sol.EventPerson:
			a.loadNames(ctx)
			continue
		case e.Type == sol.EventInbox && e.Inbox != nil:
			a.onInbox(ctx, *e.Inbox)
			continue
		default:
			continue
		}
		a.afterChange(ctx)
	}
}

func (a *App) setRevoked(addr string, revoked bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if b := a.feeds[addr]; b != nil && b.revoked != revoked {
		b.revoked = revoked
		a.dirty = true
		a.rev++
	}
}

// afterChange runs what follows any folded change: relaying co-author ops, keeping invitations
// current, telling me about invitations and answers, and re-arming alerts.
func (a *App) afterChange(ctx context.Context) {
	if err := a.relay(ctx); err != nil {
		a.logf("calendar: relay: %v", err)
	}
	if err := a.syncInvites(ctx); err != nil {
		a.logf("calendar: invitations: %v", err)
	}
	a.notifyInvites(ctx)
	a.rearm()
	a.subTouch()
}

// classify tells what a feed name is for, and whom it is about.
func classify(feed string) (feedKind, string, bool) {
	switch {
	case strings.HasPrefix(feed, "cal."):
		return kindCal, "", validCalID(feed[4:])
	case strings.HasPrefix(feed, "inv."):
		return kindInv, feed[4:], validPerson(feed[4:])
	case strings.HasPrefix(feed, "rsvp."):
		return kindRsvp, feed[5:], validPerson(feed[5:])
	}
	return 0, "", false
}

// sync reads every readable feed that has entries we have not folded.
func (a *App) sync(ctx context.Context) error {
	infos, err := a.api.Feeds(ctx, "")
	if err != nil {
		return err
	}
	var first error
	for _, fi := range infos {
		_, app, feed, err := sol.ParseAddress(fi.Feed)
		if err != nil || app != appID {
			continue
		}
		if _, _, ok := classify(feed); !ok {
			continue
		}
		a.mu.RLock()
		b := a.feeds[fi.Feed]
		known := b != nil && b.cursor >= fi.Pos && !b.revoked
		a.mu.RUnlock()
		if known {
			continue
		}
		if b != nil && b.revoked {
			a.setRevoked(fi.Feed, false)
		}
		if err := a.pull(ctx, fi.Feed); err != nil && first == nil {
			first = err
		}
	}
	a.afterChange(ctx)
	return first
}

// pull reads and folds the new entries of one feed.
func (a *App) pull(ctx context.Context, addr string) error {
	person, app, feed, err := sol.ParseAddress(addr)
	if err != nil || app != appID {
		return nil
	}
	kind, about, ok := classify(feed)
	if !ok {
		return nil
	}
	// An inv. or rsvp. feed of someone else matters only when it is about me.
	if kind != kindCal && person != a.me && about != a.me {
		return nil
	}
	a.pullMu.Lock()
	defer a.pullMu.Unlock()
	a.mu.Lock()
	b := a.feeds[addr]
	if b == nil {
		b = &feedBuf{addr: addr, person: person, feed: feed, kind: kind, about: about, seen: map[string]struct{}{}}
		a.feeds[addr] = b
	}
	from := b.cursor + 1
	a.mu.Unlock()
	for {
		es, err := a.api.Read(ctx, addr, from, 500)
		if err != nil {
			return err
		}
		if len(es) == 0 {
			return nil
		}
		a.mu.Lock()
		for _, e := range es {
			a.ingest(b, e)
			if e.Pos > b.cursor {
				b.cursor = e.Pos
			}
		}
		a.dirty = true
		a.rev++
		a.mu.Unlock()
		from = es[len(es)-1].Pos + 1
		if len(es) < 500 {
			return nil
		}
	}
}

func stampKey(op crdt.MapOp) string {
	return op.Key + "|" + op.Stamp.Device + "|" + itoa(op.Stamp.Wall) + "|" + itoa(int64(op.Stamp.Counter))
}

// ingest validates one entry's ops and stores them. mu held.
func (a *App) ingest(b *feedBuf, e sol.Entry) {
	var body struct {
		Ops []crdt.MapOp `json:"ops"`
	}
	if json.Unmarshal(e.Body, &body) != nil {
		return
	}
	maxWall := a.clock.Now().Add(24 * time.Hour).UnixMilli()
	for _, op := range body.Ops {
		if !validOp(op, maxWall) {
			continue
		}
		prefix := op.Key[:2]
		if (b.kind == kindCal && (prefix == "i." || prefix == "r.")) || (b.kind == kindInv && prefix != "i.") || (b.kind == kindRsvp && prefix != "r.") {
			continue
		}
		if op.Key == "c.owner" && !op.Delete {
			var who string
			_ = json.Unmarshal(op.Value, &who)
			b.owner = b.owner || who == b.person
		}
		a.crdtClk.ObserveOp(op)
		b.ops = append(b.ops, op)
		if b.person == a.me {
			b.seen[stampKey(op)] = struct{}{}
		}
	}
}

// view returns the visible calendars, rebuilding the fold first if feeds changed.
func (a *App) view() []*calState {
	a.mu.RLock()
	if !a.dirty {
		defer a.mu.RUnlock()
		return a.sortedCals()
	}
	a.mu.RUnlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.dirty {
		a.rebuild()
	}
	return a.sortedCals()
}

func (a *App) sortedCals() []*calState {
	out := make([]*calState, 0, len(a.cals))
	for _, cs := range a.cals {
		out = append(out, cs)
	}
	sort.Slice(out, func(i, j int) bool {
		x, y := out[i], out[j]
		if (x.owner == a.me) != (y.owner == a.me) {
			return x.owner == a.me
		}
		if x.info.Order != y.info.Order {
			return x.info.Order < y.info.Order
		}
		return x.key < y.key
	})
	return out
}

// invitesView returns the invitations to me, rebuilding first if needed.
func (a *App) invitesView() []*invite {
	a.view()
	a.mu.RLock()
	defer a.mu.RUnlock()
	out := make([]*invite, 0, len(a.invites))
	for _, iv := range a.invites {
		out = append(out, iv)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].ev.start.Equal(out[j].ev.start) {
			return out[i].ev.start.Before(out[j].ev.start)
		}
		return out[i].ref < out[j].ref
	})
	return out
}

func foldMap(ops []crdt.MapOp) *crdt.Map {
	m := crdt.NewMap()
	for _, op := range ops {
		m.Apply(op)
	}
	return m
}

// rebuild folds every feed. mu held.
func (a *App) rebuild() {
	byName := map[string][]*feedBuf{}
	for _, b := range a.feeds {
		if b.kind == kindCal && !b.owner && !b.revoked {
			byName[b.feed] = append(byName[b.feed], b)
		}
	}
	cals := map[string]*calState{}
	for addr, ob := range a.feeds {
		if ob.kind != kindCal || !ob.owner || ob.revoked {
			continue
		}
		cs := &calState{key: addr, feed: ob.feed, id: strings.TrimPrefix(ob.feed, "cal."), owner: ob.person, m: foldMap(ob.ops), grants: map[string]string{}}
		for _, k := range cs.m.Keys() {
			if strings.HasPrefix(k, "g.") {
				cs.grants[k[2:]] = str(cs.m, k)
			}
		}
		for _, cb := range byName[ob.feed] {
			if cs.grants[cb.person] != modeWrite {
				continue
			}
			for _, op := range cb.ops {
				if !ownerOnly(op.Key) {
					cs.m.Apply(op)
				}
			}
		}
		switch {
		case cs.owner == a.me:
			cs.mode = modeOwner
		case cs.grants[a.me] == modeWrite:
			cs.mode = modeWrite
		case cs.grants[a.me] == modeRead:
			cs.mode = modeRead
		default:
			continue // nobody shared this with me
		}
		if flagOf(cs.m, "c.deleted") || a.left[addr] {
			continue
		}
		cs.build(a.loc)
		cs.info.OwnerName = a.names[cs.owner]
		cs.info.Shown = !a.hidden[cs.id]
		for p, m := range cs.grants {
			cs.info.Members = append(cs.info.Members, Member{Person: p, Name: a.names[p], Mode: m})
		}
		sort.Slice(cs.info.Members, func(i, j int) bool { return cs.info.Members[i].Person < cs.info.Members[j].Person })
		cals[addr] = cs
	}
	a.cals = cals

	// Invitation feeds.
	a.invites = map[string]*invite{}
	a.myInv = map[string]map[string]json.RawMessage{}
	a.answers = map[string]map[string]string{}
	a.mine = map[string]string{}
	for _, b := range a.feeds {
		if b.revoked || b.kind == kindCal {
			continue
		}
		m := foldMap(b.ops)
		switch {
		case b.kind == kindRsvp && b.person == a.me:
			for _, k := range m.Keys() {
				a.mine[b.about+"/"+k[2:10]+"/"+k[11:]] = str(m, k)
			}
		case b.kind == kindRsvp && b.about == a.me:
			for _, k := range m.Keys() {
				ce := k[2:]
				if a.answers[ce] == nil {
					a.answers[ce] = map[string]string{}
				}
				a.answers[ce][b.person] = str(m, k)
			}
		case b.kind == kindInv && b.person == a.me:
			got := map[string]json.RawMessage{}
			for _, k := range m.Keys() {
				got[k], _ = m.Get(k)
			}
			a.myInv[b.about] = got
		case b.kind == kindInv && b.about == a.me:
			for _, k := range m.Keys() {
				raw, _ := m.Get(k)
				var s snapshot
				if json.Unmarshal(raw, &s) != nil || s.Cancelled {
					continue
				}
				ref := b.person + "/" + k[2:10] + "/" + k[11:]
				ev := s.event(k[11:], a.loc)
				if ev == nil {
					continue
				}
				a.invites[ref] = &invite{ref: ref, from: b.person, cal: k[2:10], event: k[11:], snap: s, ev: ev}
			}
		}
	}
	for ref, iv := range a.invites {
		iv.status = a.mine[ref]
	}
	// Answers count only for people the event invites.
	for _, cs := range a.cals {
		if cs.owner != a.me {
			continue
		}
		for _, e := range cs.events {
			ans := a.answers[cs.id+"."+e.ID]
			e.Attendees = nil
			for _, p := range e.invitees {
				st := ans[p]
				if st == "" {
					st = "invited"
				}
				e.Attendees = append(e.Attendees, Attendee{Person: p, Name: a.names[p], Status: st})
			}
		}
	}
	for _, cs := range a.cals {
		if cs.owner == a.me {
			continue
		}
		for _, e := range cs.events {
			e.Attendees = nil
			for _, p := range e.invitees {
				e.Attendees = append(e.Attendees, Attendee{Person: p, Name: a.names[p], Status: "invited"})
			}
		}
	}
	a.dirty = false
}

// loadNames refreshes the person-id to display-name map.
func (a *App) loadNames(ctx context.Context) {
	ppl, err := a.api.People(ctx)
	if err != nil {
		return
	}
	a.mu.Lock()
	for _, p := range ppl {
		n := p.DisplayName()
		a.names[p.ID] = n
	}
	a.dirty = true
	a.rev++
	a.mu.Unlock()
}

// relay copies ops from co-author feeds that my own owner feeds do not have yet into the owner
// feed, so every collaborator sees every other through the owner.
func (a *App) relay(ctx context.Context) error {
	a.view()
	type job struct {
		feed string
		ops  []crdt.MapOp
	}
	var jobs []job
	a.mu.RLock()
	byName := map[string][]*feedBuf{}
	for _, b := range a.feeds {
		if b.kind == kindCal && !b.owner && !b.revoked && b.person != a.me {
			byName[b.feed] = append(byName[b.feed], b)
		}
	}
	for addr, cs := range a.cals {
		ob := a.feeds[addr]
		if cs.owner != a.me || ob == nil {
			continue
		}
		var out []crdt.MapOp
		seen := map[string]bool{}
		for _, cb := range byName[ob.feed] {
			if cs.grants[cb.person] != modeWrite {
				continue
			}
			for _, op := range cb.ops {
				k := stampKey(op)
				if _, ok := ob.seen[k]; ok || seen[k] || ownerOnly(op.Key) {
					continue
				}
				seen[k] = true
				out = append(out, op)
			}
		}
		if len(out) > 0 {
			jobs = append(jobs, job{ob.feed, out})
		}
	}
	a.mu.RUnlock()
	for _, j := range jobs {
		if err := a.appendOps(ctx, j.feed, j.ops); err != nil {
			return err
		}
	}
	return nil
}

// appendOps writes ops to my feed in entries under the body size limit, then folds them.
func (a *App) appendOps(ctx context.Context, feed string, ops []crdt.MapOp) error {
	for len(ops) > 0 {
		n, size := 0, 64
		for n < len(ops) && size+len(ops[n].Value)+200 < sol.MaxBodySize-2048 {
			size += len(ops[n].Value) + 200
			n++
		}
		n = max(n, 1)
		if _, err := a.api.Append(ctx, feed, map[string]any{"ops": ops[:n]}); err != nil {
			return err
		}
		ops = ops[n:]
	}
	return a.pull(ctx, sol.Address(a.me, appID, feed))
}

// shareOnce shares one of my feeds read-only with a person, once per process.
func (a *App) shareOnce(ctx context.Context, feed, person string) error {
	key := feed + "|" + person
	a.mu.RLock()
	done := a.back[key]
	a.mu.RUnlock()
	if done {
		return nil
	}
	if err := a.api.Share(ctx, sol.ShareRequest{Feed: feed, People: []string{person}, Mode: sol.ModeRead}); err != nil {
		return err
	}
	a.mu.Lock()
	a.back[key] = true
	a.mu.Unlock()
	return nil
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func isHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for _, c := range s {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func validCalID(id string) bool   { return isHex(id, 8) }
func validEventID(id string) bool { return isHex(id, 12) }

// validPerson reports whether s looks like a person id (lowercase base32).
func validPerson(s string) bool {
	if len(s) < 10 || len(s) > 64 {
		return false
	}
	for _, c := range s {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}
