package appapi

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/apv022/sol/internal/feeds"
	"github.com/apv022/sol/sdk/go/sol"
)

// MaxEvents is how many recent events the log keeps for Last-Event-ID resume. A subscriber
// that falls further behind resumes at the oldest event kept.
var MaxEvents = 1 << 16

// Heartbeat is how often an idle event stream gets a comment line, so dead connections are
// noticed. It carries no data and is not polling.
var Heartbeat = 25 * time.Second

// rec is one event of the log, before filtering per subscriber.
type rec struct {
	n      uint64
	kind   string
	app    string // entry, feed, inbox: the app it concerns
	entry  *sol.Entry
	feed   *sol.FeedChange
	person *sol.Contact
	inbox  *sol.InboxEvent
	theme  []byte // raw JSON data of a "theme" event
}

// eventLog is the in-memory window of recent events. ids are "<epoch>.<n>" with n from 1.
type eventLog struct {
	epoch string

	mu     sync.Mutex
	recs   []rec
	next   uint64 // n of the next event
	wake   chan struct{}
	closed bool
}

func newEventLog() *eventLog {
	return &eventLog{epoch: strconv.FormatInt(time.Now().UnixNano(), 36), next: 1, wake: make(chan struct{})}
}

func (l *eventLog) add(r rec) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	r.n = l.next
	l.next++
	l.recs = append(l.recs, r)
	if len(l.recs) > MaxEvents {
		l.recs = append([]rec(nil), l.recs[len(l.recs)-MaxEvents/2:]...)
	}
	close(l.wake)
	l.wake = make(chan struct{})
}

func (l *eventLog) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.closed {
		l.closed = true
		close(l.wake)
	}
}

// id formats an event id.
func (l *eventLog) id(n uint64) string { return l.epoch + "." + strconv.FormatUint(n, 10) }

// cursor turns a Last-Event-ID into the n after which to deliver. "" means now; an id of
// another epoch (an earlier daemon run) means this run's whole log.
func (l *eventLog) cursor(last string) (uint64, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if last == "" {
		return l.next - 1, nil
	}
	ep, ns, ok := strings.Cut(last, ".")
	n, err := strconv.ParseUint(ns, 10, 64)
	if !ok || err != nil {
		return 0, sol.Errorf("bad_request", "bad Last-Event-ID")
	}
	if ep != l.epoch {
		return 0, nil
	}
	return min(n, l.next-1), nil
}

// since returns the events after cursor and a channel closed at the next event.
func (l *eventLog) since(cursor uint64) ([]rec, <-chan struct{}, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []rec
	if len(l.recs) > 0 {
		i := 0
		if first := l.recs[0].n; cursor >= first {
			i = int(cursor - first + 1)
		}
		if i < len(l.recs) {
			out = append(out, l.recs[i:]...)
		}
	}
	return out, l.wake, l.closed
}

// pump turns the store's bus into events: new entries (re-read from the store by pos, so a
// dropped bus event costs nothing but a rescan), feeds becoming readable or not, and contacts
// changing (person events).
func (s *Server) pump() {
	defer s.wg.Done()
	sub := s.n.Store.Bus().Subscribe(4096)
	defer sub.Close()
	last := map[string]uint64{}
	for _, f := range s.n.Store.Feeds() {
		last[f.Feed] = f.Pos
	}
	var dropped uint64
	for {
		var ev feeds.Event
		select {
		case <-s.ctx.Done():
			return
		case ev = <-sub.C():
		}
		if d := sub.Dropped(); d != dropped {
			dropped = d
			for _, f := range s.n.Store.Feeds() {
				s.catchUp(f.Feed, last)
			}
		}
		switch ev.Type {
		case feeds.EventEntry:
			if ev.Entry != nil {
				s.catchUp(ev.Entry.Feed, last)
			}
		case feeds.EventFeed:
			owner, app, _, err := feeds.ParseAddress(ev.Feed)
			if err != nil || app == feeds.InternalApp {
				continue
			}
			s.events.add(rec{kind: sol.EventFeed, app: app, feed: &sol.FeedChange{Feed: ev.Feed, Readable: ev.Readable}})
			s.PersonChanged(owner)
		}
	}
}

// catchUp logs the entries of a feed stored since the last one logged.
func (s *Server) catchUp(addr string, last map[string]uint64) {
	_, app, name, err := feeds.ParseAddress(addr)
	if err != nil {
		return
	}
	for {
		es, err := s.n.Store.Read(addr, last[addr]+1, 1000)
		if err != nil || len(es) == 0 {
			return
		}
		for _, e := range es {
			last[addr] = e.Pos
			if app == feeds.InternalApp {
				s.internalEntry(name, e)
				continue
			}
			se := toSol(e)
			s.events.add(rec{kind: sol.EventEntry, app: app, entry: &se})
		}
	}
}

// internalEntry turns changes to my contacts (any device) and my sharing records (made on
// another device of mine; this one reports its own as it makes them) into person events.
func (s *Server) internalEntry(name string, e feeds.Entry) {
	_, mine := s.me()
	if name == feeds.ContactsFeed || (name == feeds.SharesFeed && e.Device != mine) {
		for _, c := range s.contacts() {
			s.events.add(rec{kind: sol.EventPerson, person: &c})
		}
	}
}

// Theme pushes a "theme" event to every app (design/UI.md "Themes"): data is its JSON payload.
func (s *Server) Theme(data []byte) { s.events.add(rec{kind: "theme", theme: data}) }

// sub is one subscriber's filter.
type sub struct {
	q      req
	feeds  map[string]bool
	people bool
	inbox  bool
}

// visible decides whether r goes to the subscriber.
func (s *Server) visible(r rec, f sub) bool {
	switch r.kind {
	case sol.EventEntry:
		a := r.entry.Feed
		if len(f.feeds) > 0 && !f.feeds[a] || len(f.feeds) == 0 && r.app != f.q.app {
			return false
		}
		return f.q.has("feeds") && s.mayRead(f.q, a)
	case sol.EventFeed:
		return f.q.has("feeds") && r.app == f.q.app
	case sol.EventPerson:
		return f.people
	case sol.EventInbox:
		return f.inbox && r.app == f.q.app
	case "theme":
		return true
	}
	return false
}

// eventStream serves GET /v1/events as text/event-stream.
func (s *Server) eventStream(q req, w http.ResponseWriter, r *http.Request) (any, error) {
	v := r.URL.Query()
	f := sub{q: q, feeds: map[string]bool{}, people: v.Get("people") == "1", inbox: v.Get("inbox") == "1"}
	if f.people && !q.has("people") || f.inbox && !q.has("inbox") {
		return nil, sol.Errorf("forbidden", "the app's manifest lacks the scope for these events")
	}
	if fs := v.Get("feeds"); fs != "" {
		for _, a := range strings.Split(fs, ",") {
			if _, _, _, err := feeds.ParseAddress(a); err != nil {
				return nil, sol.Errorf("bad_request", "bad feed address")
			}
			f.feeds[a] = true
		}
	}
	cur, err := s.events.cursor(r.Header.Get("Last-Event-ID"))
	if err != nil {
		return nil, err
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	fmt.Fprint(w, ": ok\n\n")
	_ = rc.Flush()
	beat := time.NewTicker(Heartbeat)
	defer beat.Stop()
	for {
		batch, wake, closed := s.events.since(cur)
		for _, ev := range batch {
			cur = ev.n
			if !s.visible(ev, f) {
				continue
			}
			if err := s.writeEvent(w, ev); err != nil {
				return nil, done
			}
		}
		if len(batch) > 0 {
			_ = rc.Flush()
		}
		if closed {
			return nil, done
		}
		select {
		case <-wake:
		case <-beat.C:
			if _, err := fmt.Fprint(w, ": beat\n\n"); err != nil {
				return nil, done
			}
			_ = rc.Flush()
		case <-r.Context().Done():
			return nil, done
		}
	}
}

func (s *Server) writeEvent(w http.ResponseWriter, r rec) error {
	id := s.events.id(r.n)
	if r.kind == "theme" {
		_, err := fmt.Fprintf(w, "id: %s\nevent: theme\ndata: %s\n\n", id, r.theme)
		return err
	}
	return sol.EncodeEvent(w, sol.Event{ID: id, Type: r.kind, Entry: r.entry, Feed: r.feed, Person: r.person, Inbox: r.inbox})
}
