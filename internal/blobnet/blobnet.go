// Package blobnet moves blobs between devices over sessions
// (design/PROTOCOL.md §Blobs on the wire, design/API.md §4): it serves
// GET|HEAD /p/blobs/{hash} to verified peers, fetches missing blobs from
// whichever devices are online, keeps pinned blobs on my anchors, and
// collects garbage on request.
//
// Wire format (settled in C5):
//
//	HEAD /p/blobs/{hash}            {hash}: "sha256:<64 hex>" (bare hex also accepted)
//	  200  Sol-Blob-Size: <bytes>
//	       Sol-Blob-Ranges: <first>-<last>[,<first>-<last>...]   bytes held, inclusive, merged; "" if none
//	       Sol-Blob-Complete: true|false                         whole and verified by SHA-256
//	       Accept-Ranges: bytes, ETag: "sha256:<hex>"
//	  403  not allowed (asked before looking: says nothing about possession)
//	  404  not held at all
//	GET  /p/blobs/{hash}            same headers, then
//	  200  the whole blob (complete blobs only)
//	  206  Range: bytes=a-b | a- | -n (one range only), served from complete
//	       or partial data, Content-Range: bytes a-b/size
//	  416  Content-Range: bytes */size: the range is out of bounds, is not
//	       held (yet), has several parts, or no Range was sent for a blob
//	       that is not complete
//
// Invariants:
//
//   - Permission (API.md §4) is checked first, on every request: my own
//     devices may fetch anything; a contact only a blob referenced by an
//     entry of a feed I share with them (feeds.BlobRefs.MayFetch). A refused
//     request gets a bare 403 and is logged; no blob bytes or metadata.
//   - Bytes reach the local store only through blob.Partial: each 4 MiB range
//     is durable before it counts, and a blob is complete only after its
//     whole SHA-256 matches. A bad hash discards it and logs every source.
//   - Fetching never polls: sources are asked once (HEAD), ranges are
//     fetched at most MaxInFlight at a time, readers wait on the partial
//     blob's change channel, and the anchor copier wakes on pin entries and
//     new sessions.
//   - One fetch per blob at a time: readers, Fetch calls and the anchor
//     copier share it.
//   - Released blobs are collected without asking: when an entry of a feed of
//     mine releases blobs (feeds.ReleaseField, API.md §4), here or replicated
//     from another device of mine, each is removed once ReleaseGrace has
//     passed, if it is then still unpinned, unreferenced by any feed of mine,
//     unlabelled, not being fetched, and its bytes were not stored again in
//     the last ReleaseGrace (an app re-uploading the same bytes puts them
//     before it appends the entry that references them again). One timer,
//     set to the next due blob; nothing polls. Nothing else is collected
//     automatically: GC is still an explicit decision.
package blobnet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/apv022/sol/internal/blob"
	"github.com/apv022/sol/internal/events"
	"github.com/apv022/sol/internal/feeds"
	"github.com/apv022/sol/internal/session"
)

// Path is the session endpoint prefix served by Service.ServeHTTP.
const Path = "/p/blobs/"

// Response headers of GET and HEAD /p/blobs/{hash}.
const (
	HeaderSize     = "Sol-Blob-Size"
	HeaderRanges   = "Sol-Blob-Ranges"
	HeaderComplete = "Sol-Blob-Complete"
)

// Tunables, package variables so tests can change them.
var (
	// MaxInFlight bounds the ranges one fetch requests at once.
	MaxInFlight = 4
	// HeadTimeout bounds asking one source what it holds.
	HeadTimeout = 15 * time.Second
	// RangeTimeout bounds fetching one 4 MiB range from one source.
	RangeTimeout = 2 * time.Minute
	// IdleTimeout ends a fetch opened by Get that nobody reads from.
	IdleTimeout = time.Minute
	// ReleaseGrace is how long a released blob waits before it is collected,
	// and how recently stored bytes must not be: room for an app that
	// re-uploads the same content, or a device that sees a release crossing
	// a concurrent new reference, to reference it again.
	ReleaseGrace = time.Minute
)

// Errors.
var (
	// ErrNoSource means no online device that may serve the blob has the
	// bytes still missing.
	ErrNoSource = errors.New("blobnet: no online device has the blob")
	// ErrBadRef is returned for a malformed blob hash.
	ErrBadRef = errors.New("blobnet: malformed blob hash")
	// ErrClosed is returned once the Service is closed.
	ErrClosed = errors.New("blobnet: closed")
)

// Config configures a Service.
type Config struct {
	Blobs *blob.Store
	Feeds *feeds.Store // its bus wakes the anchor copier
	Refs  *feeds.BlobRefs
	Pins  *feeds.Pins
	// Sessions returns the live sessions (session.Manager.Sessions).
	Sessions func() []*session.Session
	// Anchor reports whether this device is one of my anchors (the
	// FlagAnchor bit of its entry in my device list). Anchors copy every
	// pinned blob. nil means never.
	Anchor func() bool
	Log    *slog.Logger
}

// Service serves, fetches, pins and collects blobs. Safe for concurrent
// use.
type Service struct {
	cfg    Config
	log    *slog.Logger
	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
	wake   chan struct{} // the anchor copier's doorbell

	mu   sync.Mutex
	jobs map[string]*job // fetches in progress, by hash

	relMu    sync.Mutex
	released map[string]time.Time // released blobs waiting to be collected, by hash: when
	relTimer *time.Timer          // set to the earliest of them
	closed   bool
}

// New returns a Service and starts its anchor copier.
func New(cfg Config) (*Service, error) {
	if cfg.Blobs == nil || cfg.Feeds == nil || cfg.Refs == nil || cfg.Pins == nil || cfg.Sessions == nil {
		return nil, errors.New("blobnet: Blobs, Feeds, Refs, Pins and Sessions are required")
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	s := &Service{cfg: cfg, log: cfg.Log, wake: make(chan struct{}, 1), jobs: map[string]*job{},
		released: map[string]time.Time{}}
	s.ctx, s.cancel = context.WithCancel(context.Background())
	sub := cfg.Feeds.Bus().Subscribe(64)
	s.wg.Add(1)
	go s.copier(sub)
	cfg.Refs.OnRelease(s.release)
	// Releases stored while the daemon was down (or before this device
	// collected them) are collected too.
	s.release(cfg.Refs.Released())
	return s, nil
}

// Close stops every fetch and the anchor copier, and waits for them.
// Partial blobs stay on disk, so a later fetch resumes them.
func (s *Service) Close() {
	s.mu.Lock() // job checks ctx under mu, so no fetch starts after this
	s.cancel()
	s.mu.Unlock()
	s.relMu.Lock()
	s.closed = true
	if s.relTimer != nil {
		s.relTimer.Stop()
	}
	s.relMu.Unlock()
	s.wg.Wait()
}

// SessionUp tells the Service a session came up, so pinned blobs still
// missing on an anchor are tried again. It returns at once.
func (s *Service) SessionUp(*session.Session) { s.ring() }

func (s *Service) ring() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// ParseHash accepts "sha256:<64 hex>" or bare lowercase hex and returns the
// hex digest.
func ParseHash(ref string) (string, error) {
	if h, ok := blob.ParseRef(ref); ok {
		return h, nil
	}
	if blob.ValidHash(ref) {
		return ref, nil
	}
	return "", ErrBadRef
}

func parseAll(refs []string) ([]string, error) {
	out := make([]string, 0, len(refs))
	for _, r := range refs {
		h, err := ParseHash(r)
		if err != nil {
			return nil, err
		}
		out = append(out, h)
	}
	return out, nil
}

// Pin pins blobs ("sha256:<hex>" or hex): they are kept from garbage
// collection on every device of mine and copied to my anchors in the
// background (the pin replicates as an entry of my internal pins feed).
func (s *Service) Pin(refs []string) error {
	hs, err := parseAll(refs)
	if err != nil {
		return err
	}
	return s.cfg.Pins.Pin(hs)
}

// Unpin unpins blobs; they become collectable once no feed of mine
// references them. One a feed of mine released while it was pinned is
// collected like any released blob.
func (s *Service) Unpin(refs []string) error {
	hs, err := parseAll(refs)
	if err != nil {
		return err
	}
	if err := s.cfg.Pins.Unpin(hs); err != nil {
		return err
	}
	rel := s.cfg.Refs.Released()
	s.release(slices.DeleteFunc(hs, func(h string) bool { _, ok := slices.BinarySearch(rel, h); return !ok }))
	return nil
}

// copier keeps pinned blobs on this device while it is an anchor. It wakes
// on entries of my pins feed (or when the bus dropped events, since one of
// them may have been a pin), on new sessions, and once at start.
func (s *Service) copier(sub *events.Sub[feeds.Event]) {
	defer s.wg.Done()
	defer sub.Close()
	var dropped uint64
	s.copyPinned()
	for {
		select {
		case <-s.ctx.Done():
			return
		case <-s.wake:
		case ev, ok := <-sub.C():
			if !ok {
				return
			}
			d := sub.Dropped()
			pins := ev.Type == feeds.EventEntry && ev.Entry != nil && ev.Entry.Feed == s.cfg.Pins.Address()
			if !pins && d == dropped {
				continue
			}
			dropped = d
		}
		s.copyPinned()
	}
}

// copyPinned starts a background fetch of every pinned blob this anchor
// lacks. Failures (no source online yet) are retried at the next wake.
func (s *Service) copyPinned() {
	if s.cfg.Anchor == nil || !s.cfg.Anchor() {
		return
	}
	for _, h := range s.cfg.Pins.List() {
		if s.cfg.Blobs.Has(h) {
			continue
		}
		if _, err := s.job(h, "", true); err != nil {
			return
		}
	}
}

// GC removes every blob, complete or partial, that is not pinned, not
// referenced by a feed of mine, not labelled in the local store (the
// owner's named files and inbox) and not being fetched. It returns the
// hashes removed. Nothing calls it on a timer: collecting is an explicit
// decision (API.md §4 says unpinned unreferenced blobs *may* be collected).
func (s *Service) GC() ([]string, error) {
	s.mu.Lock() // no fetch starts while we decide
	defer s.mu.Unlock()
	seen := map[string]bool{}
	var removed []string
	var errs []error
	for _, h := range append(s.cfg.Blobs.Hashes(), s.cfg.Blobs.PartialHashes()...) {
		if seen[h] {
			continue
		}
		seen[h] = true
		if s.jobs[h] != nil || s.cfg.Pins.Pinned(h) || s.cfg.Refs.Mine(h) || s.cfg.Blobs.Labelled(h) {
			continue
		}
		if err := s.cfg.Blobs.Remove(h); err != nil {
			errs = append(errs, fmt.Errorf("blobnet: collecting %s: %w", short(h), err))
			continue
		}
		removed = append(removed, h)
	}
	return removed, errors.Join(errs...)
}

// release queues released blobs (hex) for collection after ReleaseGrace. It
// is the feeds.BlobRefs hook, so it runs inside the store's hook and only
// records and sets the timer.
func (s *Service) release(hashes []string) {
	if len(hashes) == 0 {
		return
	}
	due := time.Now().Add(ReleaseGrace)
	s.relMu.Lock()
	defer s.relMu.Unlock()
	for _, h := range hashes {
		s.released[h] = due
	}
	s.armLocked()
}

// armLocked sets the one timer to the earliest released blob. relMu held.
func (s *Service) armLocked() {
	if s.closed || len(s.released) == 0 {
		return
	}
	var next time.Time
	for _, t := range s.released {
		if next.IsZero() || t.Before(next) {
			next = t
		}
	}
	if s.relTimer != nil {
		s.relTimer.Stop()
	}
	s.relTimer = time.AfterFunc(max(time.Until(next), 0), s.collectReleased)
}

// collectReleased removes the released blobs that are due and still
// collectable (see the package invariants), and sets the timer again for
// the rest.
func (s *Service) collectReleased() {
	now := time.Now()
	s.relMu.Lock()
	var due []string
	for h, t := range s.released {
		if !t.After(now) {
			due = append(due, h)
			delete(s.released, h)
		}
	}
	s.relMu.Unlock()
	var later []string
	s.mu.Lock()
	for _, h := range due {
		if s.ctx.Err() != nil {
			break
		}
		if s.jobs[h] != nil {
			later = append(later, h) // being fetched: look again once the grace has passed
			continue
		}
		if s.cfg.Pins.Pinned(h) || s.cfg.Refs.Mine(h) || s.cfg.Blobs.Labelled(h) {
			continue // kept; a later release queues it again
		}
		if f, err := s.cfg.Blobs.Open(h); err == nil {
			st, err := f.Stat()
			f.Close()
			if err == nil && now.Sub(st.ModTime()) < ReleaseGrace {
				later = append(later, h) // stored again just now: its new reference may be on its way
				continue
			}
		}
		if !s.cfg.Blobs.Has(h) && !slices.Contains(s.cfg.Blobs.PartialHashes(), h) {
			continue // never held here, or already gone
		}
		if err := s.cfg.Blobs.Remove(h); err != nil {
			s.log.Warn("collecting a released blob", "blob", short(h), "err", err)
			continue
		}
		s.log.Info("collected a released blob", "blob", short(h))
	}
	s.mu.Unlock()
	s.release(later)
	s.relMu.Lock()
	s.armLocked()
	s.relMu.Unlock()
}

func short(id string) string { return id[:min(12, len(id))] }

// sourceList names sources for a log line.
func sourceList(ids []string) string {
	out := make([]string, len(ids))
	for i, id := range ids {
		out[i] = short(id)
	}
	return strings.Join(out, ",")
}
