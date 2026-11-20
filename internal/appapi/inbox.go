package appapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/apv022/sol/internal/feeds"
	"github.com/apv022/sol/internal/fsutil"
	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/session"
	"github.com/apv022/sol/sdk/go/sol"
)

// maxInbox bounds the stored items; the oldest answered ones go first.
const maxInbox = 1000

// item is one inbox item (API.md §8), as stored and as GET /v1/inbox shows it.
type item struct {
	ID     string          `json:"id"`
	App    string          `json:"app"`
	Kind   string          `json:"kind"`
	Title  string          `json:"title"`
	Body   string          `json:"body,omitempty"`
	From   string          `json:"from,omitempty"` // a contact's person ID; "" for my own apps
	Time   time.Time       `json:"time"`
	Answer string          `json:"answer,omitempty"`
	Action *sol.ActionCall `json:"action,omitempty"`
	Agent  string          `json:"agent,omitempty"` // the agent whose call needed approval
}

// inbox is the persisted list of items, oldest first.
type inbox struct {
	path  string
	mu    sync.Mutex
	items []item
}

func openInbox(dir string) (*inbox, error) {
	in := &inbox{}
	if dir == "" {
		return in, nil
	}
	in.path = filepath.Join(dir, "inbox.json")
	b, err := os.ReadFile(in.path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return in, nil
	case err != nil:
		return nil, err
	}
	if err := json.Unmarshal(b, &in.items); err != nil {
		return nil, errors.New("appapi: inbox.json is corrupt")
	}
	return in, nil
}

// saveLocked writes the inbox atomically (0600). in.mu held.
func (in *inbox) saveLocked() error {
	if in.path == "" {
		return nil
	}
	b, err := json.Marshal(in.items)
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(in.path, b, 0o600)
}

func (in *inbox) post(it item) (string, error) {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	it.ID, it.Time = "in-"+hex.EncodeToString(b), time.Now().UTC()
	in.mu.Lock()
	defer in.mu.Unlock()
	in.items = append(in.items, it)
	if len(in.items) > maxInbox {
		if i := slices.IndexFunc(in.items, func(x item) bool { return x.Kind == "notice" || x.Answer != "" }); i >= 0 {
			in.items = slices.Delete(in.items, i, i+1)
		}
	}
	return it.ID, in.saveLocked()
}

func (in *inbox) get(id string) (item, bool) {
	in.mu.Lock()
	defer in.mu.Unlock()
	for _, it := range in.items {
		if it.ID == id {
			return it, true
		}
	}
	return item{}, false
}

// list returns the items of app ("" for all), newest first.
func (in *inbox) list(app string) []item {
	in.mu.Lock()
	defer in.mu.Unlock()
	out := []item{}
	for i := len(in.items) - 1; i >= 0; i-- {
		if app == "" || in.items[i].App == app {
			out = append(out, in.items[i])
		}
	}
	return out
}

// answer records the answer of an open approval and returns it.
func (in *inbox) answer(id string, approve bool) (item, error) {
	in.mu.Lock()
	defer in.mu.Unlock()
	for i := range in.items {
		it := &in.items[i]
		if it.ID != id {
			continue
		}
		if it.Kind != "approval" || it.Answer != "" {
			break
		}
		it.Answer = "denied"
		if approve {
			it.Answer = "approved"
		}
		return *it, in.saveLocked()
	}
	return item{}, sol.Errorf("not_found", "no open approval "+id)
}

func (s *Server) inboxEvent(app, id string) {
	if it, ok := s.inbox.get(id); ok {
		s.events.add(rec{kind: sol.EventInbox, app: app, inbox: &sol.InboxEvent{ID: it.ID, Kind: it.Kind, Title: it.Title, Answer: it.Answer}})
	}
}

// postInbox serves POST /v1/inbox.
func (s *Server) postInbox(q req, w http.ResponseWriter, r *http.Request) (any, error) {
	var b sol.InboxPost
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if b.Kind != "notice" && b.Kind != "approval" {
		return nil, sol.Errorf("bad_request", "kind must be notice or approval")
	}
	if b.Kind == "approval" && b.Action == nil {
		return nil, sol.Errorf("bad_request", "approval needs an action")
	}
	if b.Title == "" {
		return nil, sol.Errorf("bad_request", "missing title")
	}
	if me, _ := s.me(); b.To != "" && b.To != "self" && b.To != me {
		if b.Kind == "approval" {
			return nil, sol.Errorf("bad_request", "approvals go to yourself")
		}
		return s.sendNotice(r.Context(), q.app, b)
	}
	if b.Action != nil {
		if _, _, err := splitAction(b.Action.Action); err != nil {
			return nil, err
		}
	}
	id, err := s.inbox.post(item{App: q.app, Kind: b.Kind, Title: b.Title, Body: b.Body, Action: b.Action})
	if err != nil {
		return nil, err
	}
	s.inboxEvent(q.app, id)
	return sol.InboxResult{ID: id}, nil
}

// listInbox serves GET /v1/inbox: this app's items, newest first.
func (s *Server) listInbox(q req, w http.ResponseWriter, r *http.Request) (any, error) {
	return s.inbox.list(q.app), nil
}

// peerNotice is the body of POST /p/inbox.
type peerNotice struct {
	App   string `json:"app"`
	Title string `json:"title"`
	Body  string `json:"body,omitempty"`
}

// sendNotice delivers a notice to a contact's device.
func (s *Server) sendNotice(ctx context.Context, app string, b sol.InboxPost) (any, error) {
	ss, err := s.sessionTo(b.To, "")
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(peerNotice{App: app, Title: b.Title, Body: b.Body})
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://peer/p/inbox", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Content-Type", "application/json")
	resp, err := ss.HTTPClient().Do(hr)
	if err != nil {
		return nil, sol.Errorf("unavailable", "person is unreachable")
	}
	defer resp.Body.Close()
	out, err := relay(resp)
	if err != nil {
		return nil, err
	}
	var res sol.InboxResult
	if json.Unmarshal(out, &res) != nil || res.ID == "" {
		return nil, sol.Errorf("internal", "bad reply from peer")
	}
	return res, nil
}

// peerInbox serves POST /p/inbox: a notice from one of my devices or a contact, for an app.
func (s *Server) peerInbox(w http.ResponseWriter, r *http.Request) {
	p, ok := session.PeerFrom(r.Context())
	if !ok {
		sol.WriteError(w, sol.Errorf("forbidden", "not a session peer"))
		return
	}
	var b peerNotice
	if err := decode(r, &b); err != nil {
		sol.WriteError(w, err)
		return
	}
	if !feeds.ValidApp(b.App) || b.Title == "" || len(b.Title) > 500 || len(b.Body) > 16<<10 {
		sol.WriteError(w, sol.Errorf("bad_request", "bad notice"))
		return
	}
	from := ""
	if p.Relation != session.Self {
		from = identity.ID(p.Person)
	}
	id, err := s.inbox.post(item{App: b.App, Kind: "notice", Title: b.Title, Body: b.Body, From: from})
	if err != nil {
		sol.WriteError(w, err)
		return
	}
	s.inboxEvent(b.App, id)
	sol.WriteJSON(w, http.StatusOK, sol.InboxResult{ID: id})
}

// InboxItems returns every inbox item, newest first (for the CLI and the Sol app).
func (s *Server) InboxItems() []json.RawMessage {
	var out []json.RawMessage
	for _, it := range s.inbox.list("") {
		b, _ := json.Marshal(it)
		out = append(out, b)
	}
	return out
}

// Answer approves or denies an approval. On approval the stored action runs as the requesting
// app, on this device, with me as the caller; an "inbox" event reports the answer either way.
func (s *Server) Answer(ctx context.Context, id string, approve bool) (json.RawMessage, error) {
	it, err := s.inbox.answer(id, approve)
	if err != nil {
		return nil, err
	}
	s.inboxEvent(it.App, it.ID)
	if !approve || it.Action == nil {
		return nil, nil
	}
	who := s.localCaller()
	who.agent = it.Agent
	return s.run(ctx, it.App, *it.Action, who, true)
}
