package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/apv022/sol/internal/appapi"
	"github.com/apv022/sol/sdk/go/sol"
)

// The admin event stream (GET /admin/v1/events, design/ADMIN.md): what the Sol app needs to
// stay live without polling. Event types and their data:
//
//	status   Status
//	person   PersonInfo, or {"id": ..., "removed": true}
//	device   DeviceInfo, or {"id": ..., "removed": true}
//	session  SessionInfo plus "up": whether it came up or ended
//	app      AppInfo, or {"id": ..., "removed": true}
//	inbox    sol.InboxItem
//	service  Service, or {"name": ..., "removed": true}
//	tunnel   TunnelInfo, or {"id": ..., "removed": true}
//	theme    {"name": ..., "settings": {...}} as on app event streams
//	nearby   nearby.Status (LAN discovery and Wi-Fi Direct; the Android shell follows it)
//
// Ids are "<epoch>.<n>" like the app API's; Last-Event-ID resumes within the window
// (MaxAdminEvents), and an id from an earlier daemon run replays this run's whole window.

// MaxAdminEvents is how many recent admin events are kept for Last-Event-ID resume.
var MaxAdminEvents = 4096

// Admin event types.
const (
	EvStatus  = "status"
	EvPerson  = "person"
	EvDevice  = "device"
	EvSession = "session"
	EvApp     = "app"
	EvInbox   = "inbox"
	EvService = "service"
	EvTunnel  = "tunnel"
	// EvOnion: the public onion services changed, or one got its address (onions.go).
	EvOnion = "onion"
	EvTheme = "theme"
	// EvNearby: nearby or its Wi-Fi Direct switch or state changed (nearby.go).
	EvNearby = "nearby"
)

type adminEvent struct {
	n    uint64
	kind string
	data []byte
}

// adminEvents is the in-memory window of recent admin events.
type adminEvents struct {
	epoch string

	mu     sync.Mutex
	recs   []adminEvent
	next   uint64
	wake   chan struct{}
	closed bool
}

func newAdminEvents() *adminEvents {
	return &adminEvents{epoch: strconv.FormatInt(time.Now().UnixNano(), 36), next: 1, wake: make(chan struct{})}
}

// add logs one event; v is marshalled to JSON now, so later changes to it don't leak in.
func (l *adminEvents) add(kind string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	l.recs = append(l.recs, adminEvent{n: l.next, kind: kind, data: b})
	l.next++
	if len(l.recs) > MaxAdminEvents {
		l.recs = append([]adminEvent(nil), l.recs[len(l.recs)-MaxAdminEvents/2:]...)
	}
	close(l.wake)
	l.wake = make(chan struct{})
}

func (l *adminEvents) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.closed {
		l.closed = true
		close(l.wake)
	}
}

// cursor turns a Last-Event-ID into the n after which to deliver ("" means from now).
func (l *adminEvents) cursor(last string) (uint64, error) {
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

func (l *adminEvents) since(cursor uint64) ([]adminEvent, <-chan struct{}, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []adminEvent
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

// serve streams events as text/event-stream until the client goes or the daemon stops.
func (l *adminEvents) serve(w http.ResponseWriter, r *http.Request) {
	last := r.Header.Get("Last-Event-ID")
	if last == "" {
		last = r.URL.Query().Get("last")
	}
	cur, err := l.cursor(last)
	if err != nil {
		sol.WriteError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)
	rc := http.NewResponseController(w)
	fmt.Fprint(w, ": ok\n\n")
	_ = rc.Flush()
	beat := time.NewTicker(appapi.Heartbeat)
	defer beat.Stop()
	for {
		batch, wake, closed := l.since(cur)
		for _, ev := range batch {
			cur = ev.n
			if _, err := fmt.Fprintf(w, "id: %s.%d\nevent: %s\ndata: %s\n\n", l.epoch, ev.n, ev.kind, ev.data); err != nil {
				return
			}
		}
		if len(batch) > 0 {
			_ = rc.Flush()
		}
		if closed {
			return
		}
		select {
		case <-wake:
		case <-beat.C:
			if _, err := fmt.Fprint(w, ": beat\n\n"); err != nil {
				return
			}
			_ = rc.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

// emit logs an admin event.
func (d *Daemon) emit(kind string, v any) {
	if d.events != nil {
		d.events.add(kind, v)
	}
}

// emitStatus pushes the current Status (sessions, endpoints) to the admin stream.
func (d *Daemon) emitStatus() { d.emit(EvStatus, d.Status()) }

// removed is the data of an event about something that no longer exists.
func removed(key, id string) map[string]any { return map[string]any{key: id, "removed": true} }
