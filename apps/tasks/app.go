// Package tasks is the Tasks app: lists and tasks with due dates, subtasks, recurrence and
// reminders, kept in Sol feeds (design/apps/TASKS.md).
//
// Each list is one feed, owned by the person who created it, whose entries are batches of
// crdt.Map ops; the list's state is the fold of every op in the feeds the app may read. Lists
// are shared with the daemon's feed sharing: a write share makes the other person's same-named
// feed a co-author feed, which the owner folds in (and relays into its own feed) only for people
// it granted write.
//
// Invariants:
//   - feed content is untrusted: every op is validated, owner-only keys (grants, ownership,
//     deletion of the list) are honoured only from the owner's feed, and a co-author's ops only
//     when the owner holds a write grant for that person;
//   - state is rebuilt from the feeds alone, so folding is idempotent and order-independent;
//     nothing is polled: feeds are read at start and then follow the events stream;
//   - reminders are timers for the earliest unfired one, re-armed after each change;
//   - dates are wall-clock strings in the device's zone (dates.go);
//   - every action requires a Sol-Self caller; the UI uses only actions.
package tasks

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

// Clock is the app's time source, injectable so tests can drive reminders.
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

// Config is how a Tasks instance is set up.
type Config struct {
	// DataDir holds fired reminders and hidden lists. Empty means $SOL_APP_DATA; none at all keeps them in memory.
	DataDir string
	// Listener is where the UI and actions are served; nil means sol.Serve (SOL_APP_LISTEN).
	Listener net.Listener
	// Clock overrides the time source (tests).
	Clock Clock
	// Loc is the zone due dates are read in; nil means time.Local.
	Loc *time.Location
	// Logf receives diagnostics; nil discards them. Never given task text.
	Logf func(format string, args ...any)
}

type feedBuf struct {
	addr, person, feed string
	ops                []crdt.MapOp
	seen               map[string]struct{} // key|stamp of every op in an owner feed of mine (for relaying)
	owner              bool                // contains l.owner naming its own person
	revoked            bool
	cursor             uint64
}

// App is one running Tasks instance.
type App struct {
	api      sol.API
	cfg      Config
	manifest *sol.Manifest
	clock    Clock
	loc      *time.Location
	data     string

	me, dev string
	crdtClk *crdt.Clock
	scratch *crdt.Map

	pullMu sync.Mutex // serialises feed reads so each entry is folded once
	mu     sync.RWMutex
	feeds  map[string]*feedBuf
	lists  map[string]*listState // by owner feed address
	dirty  bool
	rev    uint64
	hidden map[string]bool // list keys I left
	back   map[string]bool // co-author feeds already shared back
	names  map[string]string

	remMu  sync.Mutex
	fired  map[string]int64
	timer  func() bool
	rearmN int

	noFollow bool // tests: do not follow the events stream, so the test decides when feeds are read
	started  bool
	cancel   context.CancelFunc
	wg       sync.WaitGroup
	handler  http.Handler
}

// New creates a Tasks instance. Call Start (or Run) to begin.
func New(api sol.API, cfg Config) *App {
	m, err := sol.ParseManifest(ManifestJSON)
	if err != nil {
		panic("tasks: bad embedded app.json: " + err.Error())
	}
	if cfg.DataDir == "" {
		cfg.DataDir = os.Getenv("SOL_APP_DATA")
	}
	if cfg.Clock == nil {
		cfg.Clock = realClock{}
	}
	if cfg.Loc == nil {
		cfg.Loc = time.Local
	}
	a := &App{api: api, cfg: cfg, manifest: m, clock: cfg.Clock, loc: cfg.Loc, data: cfg.DataDir,
		feeds: map[string]*feedBuf{}, lists: map[string]*listState{}, hidden: map[string]bool{}, back: map[string]bool{},
		fired: map[string]int64{}, scratch: crdt.NewMap(), names: map[string]string{}}
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

// now is the clock's time in the app's zone.
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

// Start reads every feed, folds them, arms reminders and follows the events stream.
func (a *App) Start(ctx context.Context) error {
	if a.started {
		return errors.New("tasks: already started")
	}
	if a.api == nil {
		return errors.New("tasks: no API")
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
		loadJSON(filepath.Join(a.data, "hidden.json"), &a.hidden)
	}
	ctx, a.cancel = context.WithCancel(ctx)
	// Subscribe before the first read so nothing slips between them.
	var ev <-chan sol.Event
	if !a.noFollow {
		if ev, err = a.api.Events(ctx, sol.EventOptions{People: true}); err != nil {
			a.cancel()
			return err
		}
	}
	a.loadNames(ctx)
	if err := a.sync(ctx); err != nil {
		a.cancel()
		return err
	}
	a.started = true
	if ev != nil {
		a.wg.Add(1)
		go a.follow(ctx, ev)
	}
	return nil
}

// Stop ends the event loop and the reminder timer.
func (a *App) Stop() {
	if !a.started {
		return
	}
	a.cancel()
	a.remMu.Lock()
	if a.timer != nil {
		a.timer()
		a.timer = nil
	}
	a.remMu.Unlock()
	a.wg.Wait()
	a.started = false
}

func loadJSON(path string, v any) {
	if b, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(b, v)
	}
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
	}
}

// afterChange runs what follows any folded change: relaying co-author ops and re-arming reminders.
func (a *App) afterChange(ctx context.Context) {
	if err := a.relay(ctx); err != nil {
		a.logf("tasks: relay: %v", err)
	}
	a.rearm()
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
		if err != nil || app != "tasks" || !strings.HasPrefix(feed, "list.") {
			continue
		}
		a.mu.RLock()
		b := a.feeds[fi.Feed]
		known := b != nil && b.cursor >= fi.Pos && !b.revoked
		a.mu.RUnlock()
		if known {
			continue
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
	if err != nil || app != "tasks" || !strings.HasPrefix(feed, "list.") || !validListID(strings.TrimPrefix(feed, "list.")) {
		return nil
	}
	a.pullMu.Lock()
	defer a.pullMu.Unlock()
	a.mu.Lock()
	b := a.feeds[addr]
	if b == nil {
		b = &feedBuf{addr: addr, person: person, feed: feed, seen: map[string]struct{}{}}
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
		if op.Key == "l.owner" && !op.Delete {
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

// view returns the visible lists, rebuilding the fold first if feeds changed.
func (a *App) view() []*listState {
	a.mu.RLock()
	if !a.dirty {
		defer a.mu.RUnlock()
		return a.sortedLists()
	}
	a.mu.RUnlock()
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.dirty {
		a.rebuild()
	}
	return a.sortedLists()
}

func (a *App) sortedLists() []*listState {
	out := make([]*listState, 0, len(a.lists))
	for _, ls := range a.lists {
		out = append(out, ls)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].info.Order != out[j].info.Order {
			return out[i].info.Order < out[j].info.Order
		}
		return out[i].key < out[j].key
	})
	return out
}

// rebuild folds every owner feed with the co-author feeds its owner granted write to. mu held.
func (a *App) rebuild() {
	byName := map[string][]*feedBuf{}
	for _, b := range a.feeds {
		if !b.owner && !b.revoked {
			byName[b.feed] = append(byName[b.feed], b)
		}
	}
	lists := map[string]*listState{}
	for addr, ob := range a.feeds {
		if !ob.owner || ob.revoked {
			continue
		}
		ls := &listState{key: addr, feed: ob.feed, id: strings.TrimPrefix(ob.feed, "list."), owner: ob.person, m: crdt.NewMap(), grants: map[string]string{}}
		for _, op := range ob.ops {
			ls.m.Apply(op)
		}
		for _, k := range ls.m.Keys() {
			if strings.HasPrefix(k, "g.") {
				ls.grants[k[2:]] = str(ls.m, k)
			}
		}
		for _, cb := range byName[ob.feed] {
			if ls.grants[cb.person] != modeWrite {
				continue
			}
			for _, op := range cb.ops {
				if !ownerOnly(op.Key) {
					ls.m.Apply(op)
				}
			}
		}
		switch {
		case ls.owner == a.me:
			ls.mode = modeOwner
		case ls.grants[a.me] == modeWrite:
			ls.mode = modeWrite
		case ls.grants[a.me] == modeRead:
			ls.mode = modeRead
		default:
			continue // nobody shared this with me
		}
		if flagOf(ls.m, "l.deleted") || a.hidden[addr] {
			continue
		}
		ls.build()
		ls.info.OwnerName = a.names[ls.owner]
		for p, m := range ls.grants {
			ls.info.Members = append(ls.info.Members, Member{Person: p, Name: a.names[p], Mode: m})
		}
		sort.Slice(ls.info.Members, func(i, j int) bool { return ls.info.Members[i].Person < ls.info.Members[j].Person })
		lists[addr] = ls
	}
	a.lists = lists
	a.dirty = false
}

// loadNames refreshes the person-id to display-name map used for members.
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
		if !b.owner && !b.revoked && b.person != a.me {
			byName[b.feed] = append(byName[b.feed], b)
		}
	}
	for addr, ls := range a.lists {
		ob := a.feeds[addr]
		if ls.owner != a.me || ob == nil {
			continue
		}
		var out []crdt.MapOp
		seen := map[string]bool{}
		for _, cb := range byName[ob.feed] {
			if ls.grants[cb.person] != modeWrite {
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
	return a.pull(ctx, sol.Address(a.me, "tasks", feed))
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

func validListID(id string) bool {
	if len(id) != 8 {
		return false
	}
	for _, c := range id {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}
