// Package replicate keeps feeds in step between devices over sessions
// (design/PROTOCOL.md §Session endpoints, POST /p/sync).
//
// Every session runs replication in both directions: each side pulls from
// the other with one long-lived POST /p/sync, and serves the other's pull.
//
// Wire format of /p/sync:
//
// Request body (JSON):
//
//	{"feeds": {"<feed address>": {"<device ID>": <have seq>, ...}, ...},
//	 "all": true}
//
// feeds carries what the caller already holds of each feed, per device
// chain; all asks for every feed the caller may read, not only those listed.
//
// Response: 200, Content-Type application/x-ndjson, a stream of JSON
// messages, one per line, that ends only when the caller goes away:
//
//	{"t":"feed","feed":"<addr>","readable":true,"mode":"read|write|coauthor"}
//	{"t":"feed","feed":"<addr>"}            no longer readable: stop
//	{"t":"entry","entry":{feed,seq,device,time(unix ms),prev,body,sig}}
//	{"t":"live"}                            the backlog is sent
//
// The callee first announces the feeds it lets a contact read (feed
// messages; none for my own devices, which may read everything), then sends,
// in its own arrival order, every entry with seq above the caller's have for
// that chain (never the caller's own chain), then "live" once, then new
// entries and readability changes as they happen. Within one stream every
// chain's entries arrive in seq order without gaps, so the caller can
// Accept them as they come; on reconnect it sends fresh haves from its
// store and the stream resumes without duplicates or gaps.
//
// Invariants:
//
//   - Permissions are checked by the callee, on every pass: my own devices
//     get every feed I store; a contact gets only my feeds I share with
//     them (a co-author feed is one my app shared back; a contact's
//     announced write share alone grants nothing). Unsharing ends the feed
//     in live streams at once (an unreadable message, no more entries).
//   - The caller accepts from a contact only entries of that contact's
//     feeds announced readable in the same stream, and only after the store's
//     full verification.
//   - Fan-out never blocks: each stream reads the store at its own cursor
//     and is only woken by the bus (which drops instead of blocking), so a
//     slow peer only slows its own stream.
//   - What a contact announces as readable is recorded as "incoming"
//     shares (feeds.Shares.SetIncoming): what they share with me, never
//     what I share with them.
package replicate

import (
	"bufio"
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/apv022/sol/internal/feeds"
	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/session"
)

// Path is the session endpoint served by Handler.
const Path = "/p/sync"

// Message types on the /p/sync response stream.
const (
	MsgEntry = "entry"
	MsgFeed  = "feed"
	MsgLive  = "live"
)

// Request is the body of POST /p/sync.
type Request struct {
	Feeds map[string]map[string]uint64 `json:"feeds"`
	All   bool                         `json:"all"`
}

// Message is one line of the /p/sync response.
type Message struct {
	T        string      `json:"t"`
	Entry    *feeds.Wire `json:"entry,omitempty"`
	Feed     string      `json:"feed,omitempty"`
	Readable bool        `json:"readable,omitempty"`
	Mode     string      `json:"mode,omitempty"`
}

// Tunables, package variables so tests can shorten them.
var (
	// RetryMin and RetryMax bound the backoff between pulls over a session
	// that is still up.
	RetryMin = 200 * time.Millisecond
	RetryMax = 30 * time.Second
)

const (
	maxRequest = 64 << 20 // a request lists every feed with its heads
	maxLine    = 1 << 20  // one message; an entry body is at most 64 KiB
	readChunk  = 256
	flushEvery = 256
)

// Config configures a Replicator.
type Config struct {
	Store  *feeds.Store
	Shares *feeds.Shares
	// Refresh, if set, asks person's devices for their newest device list
	// (session.Manager.Refresh). It is called once per stream when an
	// entry comes from a device not in the list known here.
	Refresh func(ctx context.Context, person ed25519.PublicKey) error
	Log     *slog.Logger
}

// Replicator serves and runs feed replication. Safe for concurrent use.
type Replicator struct {
	cfg Config
	log *slog.Logger

	ctx    context.Context
	cancel context.CancelFunc
	mu     sync.Mutex
	closed bool
	wg     sync.WaitGroup
}

// New returns a Replicator.
func New(cfg Config) (*Replicator, error) {
	if cfg.Store == nil || cfg.Shares == nil {
		return nil, errors.New("replicate: Store and Shares are required")
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Replicator{cfg: cfg, log: cfg.Log, ctx: ctx, cancel: cancel}, nil
}

// Close stops every pull and every served stream and waits for the pulls.
func (r *Replicator) Close() {
	r.mu.Lock()
	r.closed = true
	r.mu.Unlock()
	r.cancel()
	r.wg.Wait()
}

func short(id string) string { return id[:min(8, len(id))] }

// ServeHTTP serves POST /p/sync to a verified session peer.
func (r *Replicator) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	peer, ok := session.PeerFrom(req.Context())
	if !ok || (peer.Relation != session.Self && peer.Relation != session.Contact) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	if req.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var sr Request
	if err := json.NewDecoder(io.LimitReader(req.Body, maxRequest)).Decode(&sr); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithCancel(req.Context())
	defer cancel()
	stop := context.AfterFunc(r.ctx, cancel)
	defer stop()

	// Subscribe before the first pass, so nothing stored after it is missed.
	sub := r.cfg.Store.Bus().Subscribe(1)
	defer sub.Close()
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	st := &stream{r: r, peer: peer, req: sr, enc: json.NewEncoder(w),
		caller: identity.ID(peer.Device), person: identity.ID(peer.Person),
		cursor: map[string]uint64{}, modes: map[string]string{}, denied: map[string]bool{}}
	if st.req.Feeds == nil {
		st.req.Feeds = map[string]map[string]uint64{}
	}
	rc := http.NewResponseController(w)
	live := false
	for {
		if err := st.pass(); err != nil {
			return
		}
		if !live {
			live = true
			if st.enc.Encode(Message{T: MsgLive}) != nil {
				return
			}
		}
		if rc.Flush() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-sub.C():
		}
	}
}

// stream is the callee's state for one served pull.
type stream struct {
	r              *Replicator
	peer           session.Peer
	req            Request
	enc            *json.Encoder
	caller, person string            // IDs of the caller's device and person
	cursor         map[string]uint64 // feed -> last pos looked at
	modes          map[string]string // feed -> mode announced (contacts)
	denied         map[string]bool   // requested feeds refused and said so
}

// allowed returns the feeds this caller may read now, with the reason.
func (st *stream) allowed() map[string]string {
	out := map[string]string{}
	want := func(addr string) bool { _, ok := st.req.Feeds[addr]; return st.req.All || ok }
	if st.peer.Relation == session.Self {
		for _, fi := range st.r.cfg.Store.Feeds() {
			if want(fi.Feed) {
				out[fi.Feed] = "self"
			}
		}
		return out
	}
	for addr, mode := range st.r.cfg.Shares.ReadableBy(st.person) {
		if want(addr) {
			out[addr] = mode
		}
	}
	return out
}

// pass sends every change since the last pass: readability, then entries.
func (st *stream) pass() error {
	allowed := st.allowed()
	contact := st.peer.Relation == session.Contact
	for addr := range st.req.Feeds {
		if _, ok := allowed[addr]; !ok && !st.denied[addr] && st.modes[addr] == "" {
			st.denied[addr] = true
			if err := st.enc.Encode(Message{T: MsgFeed, Feed: addr}); err != nil {
				return err
			}
		}
	}
	for addr := range st.modes {
		if _, ok := allowed[addr]; !ok {
			delete(st.modes, addr)
			delete(st.cursor, addr)
			st.r.log.Info("feed no longer shared with a peer", "feed", addr, "peer", short(st.caller))
			if err := st.enc.Encode(Message{T: MsgFeed, Feed: addr}); err != nil {
				return err
			}
		}
	}
	addrs := make([]string, 0, len(allowed))
	for addr, mode := range allowed {
		addrs = append(addrs, addr)
		if contact && st.modes[addr] != mode {
			st.modes[addr] = mode
			delete(st.denied, addr)
			if err := st.enc.Encode(Message{T: MsgFeed, Feed: addr, Readable: true, Mode: mode}); err != nil {
				return err
			}
		}
	}
	// Internal feeds (sharing records, contacts) first, so a permission
	// change reaches my other devices before data written after it.
	internal := func(a string) bool { _, app, _, _ := feeds.ParseAddress(a); return app == feeds.InternalApp }
	sort.Slice(addrs, func(i, j int) bool {
		if ii, ij := internal(addrs[i]), internal(addrs[j]); ii != ij {
			return ii
		}
		return addrs[i] < addrs[j]
	})
	for _, addr := range addrs {
		if err := st.sendFeed(addr); err != nil {
			return err
		}
	}
	return nil
}

// sendFeed sends the entries of addr past the cursor that the caller lacks.
func (st *stream) sendFeed(addr string) error {
	have := st.req.Feeds[addr]
	if have == nil {
		have = map[string]uint64{}
		st.req.Feeds[addr] = have
	}
	for {
		es, err := st.r.cfg.Store.Read(addr, st.cursor[addr]+1, readChunk)
		if err != nil {
			return err
		}
		for _, e := range es {
			st.cursor[addr] = e.Pos
			if e.Device == st.caller || e.Seq <= have[e.Device] {
				continue
			}
			w := e.Wire()
			if err := st.enc.Encode(Message{T: MsgEntry, Entry: &w}); err != nil {
				return err
			}
			have[e.Device] = e.Seq
		}
		if len(es) < readChunk {
			return nil
		}
	}
}

// Attach pulls over s until it dies (or the Replicator closes), retrying
// with backoff. It is meant as session.Config.OnSession.
func (r *Replicator) Attach(s *session.Session) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.wg.Add(1)
	r.mu.Unlock()
	defer r.wg.Done()
	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()
	go func() {
		select {
		case <-s.Done():
			cancel()
		case <-ctx.Done():
		}
	}()
	backoff := RetryMin
	for {
		live, err := r.pullOnce(ctx, s)
		if ctx.Err() != nil {
			return
		}
		r.log.Debug("feed pull ended", "peer", short(identity.ID(s.Peer().Device)), "err", err)
		if live {
			backoff = RetryMin
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, RetryMax)
	}
}

// pullOnce opens one /p/sync stream over s and stores what arrives until
// the stream ends. It reports whether the stream got live.
func (r *Replicator) pullOnce(ctx context.Context, s *session.Session) (live bool, err error) {
	store := r.cfg.Store
	peer := s.Peer()
	self := peer.Relation == session.Self
	pid := identity.ID(peer.Person)
	sr := Request{Feeds: map[string]map[string]uint64{}, All: true}
	for _, fi := range store.Feeds() {
		if owner, _, _, _ := feeds.ParseAddress(fi.Feed); self || owner == pid {
			sr.Feeds[fi.Feed] = store.Heads(fi.Feed)
		}
	}
	body, err := json.Marshal(sr)
	if err != nil {
		return false, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://peer"+Path, bytes.NewReader(body))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.HTTPClient().Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("replicate: /p/sync: %s", resp.Status)
	}
	defer store.Flush()

	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), maxLine)
	readable := map[string]string{} // contact feeds announced, addr -> mode
	blocked := map[string]bool{}    // feed|device chains given up on in this stream
	refreshed := map[string]bool{}
	n := 0
	for sc.Scan() {
		var m Message
		if err := json.Unmarshal(sc.Bytes(), &m); err != nil {
			return live, fmt.Errorf("replicate: bad message: %w", err)
		}
		switch m.T {
		case MsgLive:
			live = true
			if !self {
				r.setIncoming(pid, readable)
			}
			store.Flush()
		case MsgFeed:
			if self {
				continue
			}
			if owner, _, _, err := feeds.ParseAddress(m.Feed); err != nil || owner != pid {
				continue
			}
			if m.Readable {
				readable[m.Feed] = m.Mode
			} else {
				delete(readable, m.Feed)
			}
			if live {
				r.setIncoming(pid, readable)
			}
		case MsgEntry:
			if m.Entry == nil {
				continue
			}
			e := *m.Entry
			if !self && readable[e.Feed] == "" {
				r.log.Info("dropping an entry of a feed not shared with me", "feed", e.Feed, "peer", short(identity.ID(peer.Device)))
				continue
			}
			chain := e.Feed + "|" + e.Device
			if blocked[chain] {
				continue
			}
			_, err := store.Accept(e)
			if errors.Is(err, feeds.ErrUnknownDevice) && r.cfg.Refresh != nil {
				owner, _, _, _ := feeds.ParseAddress(e.Feed)
				if !refreshed[owner] {
					refreshed[owner] = true
					if pub, perr := identity.ParseID(owner); perr == nil {
						r.cfg.Refresh(ctx, pub)
					}
				}
				_, err = store.Accept(e)
			}
			var gap *feeds.GapError
			switch {
			case err == nil || errors.Is(err, feeds.ErrDuplicate):
			case errors.As(err, &gap):
				// A stream never skips; a gap means the peer misbehaved or
				// lost data. Start over from fresh haves.
				return live, err
			default:
				blocked[chain] = true
				r.log.Info("rejected a feed entry", "feed", e.Feed, "device", short(e.Device), "seq", e.Seq,
					"peer", short(identity.ID(peer.Device)), "reason", err.Error())
			}
			if n++; n%flushEvery == 0 {
				store.Flush()
			}
		}
	}
	if err := sc.Err(); err != nil {
		return live, err
	}
	return live, io.EOF
}

// setIncoming records what the contact pid announced as readable to me,
// keyed "<app>/<name>".
func (r *Replicator) setIncoming(pid string, readable map[string]string) {
	m := make(map[string]string, len(readable))
	for addr, mode := range readable {
		m[strings.TrimPrefix(addr, pid+"/")] = mode
	}
	if err := r.cfg.Shares.SetIncoming(pid, m); err != nil {
		r.log.Warn("recording incoming shares", "err", err)
	}
}
