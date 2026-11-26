package appapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
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

// resultTTL is how long an approved action's output waits in memory for its requester
// (API.md section 8). A variable so tests can shorten it.
var resultTTL = 10 * time.Minute

// item is one inbox item (API.md §8), as stored and as GET /v1/inbox shows it.
type item = sol.InboxItem

// inbox is the list of items, oldest first, persisted to inbox.json. What it persists never
// holds a secret or a result: an answered approval keeps only the action's name, the args
// digest, the answer and the outcome; an open one keeps its args with secret values redacted.
// The secret values and the outputs waiting for their requester live in memory only, so a
// restart drops them (and expires the open approvals whose args they were).
type inbox struct {
	path  string
	mu    sync.Mutex
	items []item
	// secrets holds the unredacted args of open approvals whose calls have secret values.
	secrets map[string]json.RawMessage
	// results holds approved actions' outputs until their requester takes them, or resultTTL.
	results map[string]heldResult
}

// heldResult is an output waiting for its requester.
type heldResult struct {
	out   json.RawMessage
	timer *time.Timer
}

// expiredErr answers an approval whose secret args a restart lost.
func expiredErr() *sol.Error {
	return sol.Errorf("unavailable", "Sol restarted before this approval was answered; its secret arguments were kept only in memory, so make the call again")
}

// argsDigest is the activity log's digest of a call's args.
func argsDigest(args json.RawMessage) string {
	sum := sha256.Sum256(args)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func openInbox(dir string) (*inbox, error) {
	in := &inbox{secrets: map[string]json.RawMessage{}, results: map[string]heldResult{}}
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
	if in.scrubLocked() {
		if err := in.saveLocked(); err != nil {
			return nil, err
		}
	}
	return in, nil
}

// scrubLocked removes what must not outlive memory from the loaded items, and reports whether
// it changed anything: it expires open approvals whose secret args a restart lost, and drops
// the args and results that older versions kept on answered approvals (issue #4).
func (in *inbox) scrubLocked() bool {
	changed := false
	for i := range in.items {
		it := &in.items[i]
		if it.Kind != "approval" {
			continue
		}
		if it.Answer == "" && it.SecretArgs {
			it.Answer, it.Error = "expired", expiredErr()
		}
		if it.Answer == "" {
			continue
		}
		if it.Action != nil && it.Action.Args != nil {
			if it.ArgsDigest == "" && !it.SecretArgs {
				it.ArgsDigest = argsDigest(it.Action.Args)
			}
			it.Action = &sol.ActionCall{Action: it.Action.Action, Person: it.Action.Person}
			changed = true
		}
		if it.Result != nil {
			it.Result, changed = nil, true
		}
		if it.Answer == "approved" && it.Outcome == "" && it.Action != nil {
			it.Outcome, changed = "ok", true
			if it.Error != nil {
				it.Outcome = "error"
			}
		}
	}
	return changed
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

// post adds an item. secretArgs, if set, are the unredacted args of an approval whose stored
// Action carries them redacted: they stay in memory until it is answered.
func (in *inbox) post(it item, secretArgs json.RawMessage) (string, error) {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	it.ID, it.Time = "in-"+hex.EncodeToString(b), time.Now().UTC()
	in.mu.Lock()
	defer in.mu.Unlock()
	in.items = append(in.items, it)
	if secretArgs != nil {
		in.secrets[it.ID] = secretArgs
	}
	if len(in.items) > maxInbox {
		if i := slices.IndexFunc(in.items, func(x item) bool { return x.Kind == "notice" || x.Answer != "" }); i >= 0 {
			in.dropLocked(in.items[i].ID)
			in.items = slices.Delete(in.items, i, i+1)
		}
	}
	return it.ID, in.saveLocked()
}

// dropLocked forgets what memory holds for an item.
func (in *inbox) dropLocked(id string) {
	delete(in.secrets, id)
	if h, ok := in.results[id]; ok {
		h.timer.Stop()
		delete(in.results, id)
	}
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

// list returns the items match accepts, newest first. For those deliver also accepts, a held
// result is attached and then forgotten: each output reaches its requester once.
func (in *inbox) list(match, deliver func(item) bool) []item {
	in.mu.Lock()
	defer in.mu.Unlock()
	out := []item{}
	for i := len(in.items) - 1; i >= 0; i-- {
		it := in.items[i]
		if !match(it) {
			continue
		}
		if h, ok := in.results[it.ID]; ok && deliver != nil && deliver(it) {
			it.Result = h.out
			h.timer.Stop()
			delete(in.results, it.ID)
		}
		out = append(out, it)
	}
	return out
}

// answer records the answer of an open approval. It returns the item with the call's full
// args (secret ones from memory) to run, and stores it with only the action's name.
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
		full := *it
		if it.Action != nil {
			call := *it.Action
			if it.SecretArgs {
				call.Args = in.secrets[id]
			}
			full.Action = &call
			it.Action = &sol.ActionCall{Action: call.Action, Person: call.Person}
		}
		delete(in.secrets, id)
		return full, in.saveLocked()
	}
	return item{}, sol.Errorf("not_found", "no open approval "+id)
}

// setResult records the outcome of an approved item's action, and holds its output in memory
// for the requester for resultTTL. The output is never written to disk.
func (in *inbox) setResult(id string, out json.RawMessage, err error) error {
	in.mu.Lock()
	defer in.mu.Unlock()
	for i := range in.items {
		if in.items[i].ID != id {
			continue
		}
		in.items[i].Outcome = "ok"
		if err != nil {
			var se *sol.Error
			if !errors.As(err, &se) {
				se = sol.Errorf("internal", "action failed")
			}
			in.items[i].Outcome, in.items[i].Error = "error", se
		} else if len(out) > 0 {
			in.dropLocked(id)
			in.results[id] = heldResult{out: out, timer: time.AfterFunc(resultTTL, func() { in.expireResult(id, out) })}
		}
		return in.saveLocked()
	}
	return nil
}

// expireResult forgets a held output its requester did not take in time.
func (in *inbox) expireResult(id string, out json.RawMessage) {
	in.mu.Lock()
	defer in.mu.Unlock()
	if h, ok := in.results[id]; ok && bytes.Equal(h.out, out) {
		delete(in.results, id)
	}
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
	if b.Kind == "approval" {
		return s.postApproval(item{App: q.app, Kind: b.Kind, Title: b.Title, Body: b.Body}, *b.Action)
	}
	id, err := s.inbox.post(item{App: q.app, Kind: b.Kind, Title: b.Title, Body: b.Body, Action: b.Action}, nil)
	if err != nil {
		return nil, err
	}
	s.inboxEvent(q.app, id)
	return sol.InboxResult{ID: id}, nil
}

// postApproval files an approval of call. When the call is to an action of an app here whose
// input marks secret properties, the item carries the args redacted and the real ones stay in
// memory (API.md section 8).
func (s *Server) postApproval(it item, c sol.ActionCall) (sol.InboxResult, error) {
	call := sol.ActionCall{Action: c.Action, Args: c.Args, Person: c.Person}
	it.ArgsDigest = argsDigest(c.Args)
	var secret json.RawMessage
	if spec, ok := s.localSpec(c); ok {
		if red, changed := spec.Input.Redact(c.Args); changed {
			secret, call.Args, it.SecretArgs = c.Args, red, true
		}
	}
	it.Action = &call
	id, err := s.inbox.post(it, secret)
	if err != nil {
		return sol.InboxResult{}, err
	}
	s.inboxEvent(it.App, id)
	return sol.InboxResult{ID: id}, nil
}

// localSpec returns the spec of a call's action when it runs on this device.
func (s *Server) localSpec(c sol.ActionCall) (sol.ActionSpec, bool) {
	if me, _ := s.me(); c.Person != "" && c.Person != me {
		return sol.ActionSpec{}, false
	}
	appID, _, err := splitAction(c.Action)
	if err != nil {
		return sol.ActionSpec{}, false
	}
	if b := s.cfg.Builtin; b != nil && appID == b.ID {
		return b.Spec(c.Action)
	}
	a, ok := s.cfg.Apps.App(appID)
	if !ok {
		return sol.ActionSpec{}, false
	}
	return a.Manifest.Action(c.Action)
}

// listInbox serves GET /v1/inbox: this app's items, newest first. The outputs of the app's own
// approved requests are handed over here, once; an agent's go to the agent.
func (s *Server) listInbox(q req, w http.ResponseWriter, r *http.Request) (any, error) {
	return s.inbox.list(func(it item) bool { return it.App == q.app },
		func(it item) bool { return it.Agent == "" }), nil
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
	id, err := s.inbox.post(item{App: b.App, Kind: "notice", Title: b.Title, Body: b.Body, From: from}, nil)
	if err != nil {
		sol.WriteError(w, err)
		return
	}
	s.inboxEvent(b.App, id)
	sol.WriteJSON(w, http.StatusOK, sol.InboxResult{ID: id})
}

// InboxItems returns every inbox item, newest first (for the CLI and the Sol app). It never
// carries a result: those wait for their requester (AgentInbox, GET /v1/inbox).
func (s *Server) InboxItems() []sol.InboxItem {
	return s.inbox.list(func(item) bool { return true }, nil)
}

// AgentInbox returns the approvals an agent's calls needed, newest first, for
// GET /agent/v1/inbox. The output of an approved call is handed over here, once.
func (s *Server) AgentInbox(agent string) []sol.InboxItem {
	mine := func(it item) bool { return it.Agent == agent }
	return s.inbox.list(mine, mine)
}

// InboxItem returns one inbox item.
func (s *Server) InboxItem(id string) (sol.InboxItem, bool) { return s.inbox.get(id) }

// Answer approves or denies an approval. On approval the stored action runs as the principal
// that asked: the requesting app, on this device, with me as the caller and, when an agent's
// call needed the approval, that agent in Sol-Agent. The outcome is recorded on the item and
// the output held in memory for the requester; then an "inbox" event reports the answer to the
// app. The output is also returned, for the person who approved, unless the action declares
// output_secret (then only the requester gets it).
func (s *Server) Answer(ctx context.Context, id string, approve bool) (json.RawMessage, error) {
	it, err := s.inbox.answer(id, approve)
	if err != nil {
		return nil, err
	}
	defer s.inboxEvent(it.App, it.ID)
	if !approve || it.Action == nil {
		return nil, nil
	}
	who := s.localCaller()
	who.agent = it.Agent
	var out json.RawMessage
	if me, _ := s.me(); it.Action.Person != "" && it.Action.Person != me {
		out, err = s.callRemote(ctx, *it.Action, who.agent)
	} else {
		out, err = s.run(ctx, it.App, *it.Action, who, true)
	}
	if rerr := s.inbox.setResult(it.ID, out, err); rerr != nil {
		s.log.Warn("inbox: recording a result", "err", rerr)
	}
	if spec, ok := s.localSpec(*it.Action); ok && spec.OutputSecret {
		out = nil
	}
	return out, err
}

// Dismiss removes a notice, or an approval already answered, from the inbox. An open approval
// must be approved or denied instead.
func (s *Server) Dismiss(id string) error {
	in := s.inbox
	in.mu.Lock()
	defer in.mu.Unlock()
	for i, it := range in.items {
		if it.ID != id {
			continue
		}
		if it.Kind == "approval" && it.Answer == "" {
			return sol.Errorf("conflict", "approve or deny it instead")
		}
		in.dropLocked(id)
		in.items = slices.Delete(in.items, i, i+1)
		return in.saveLocked()
	}
	return sol.Errorf("not_found", "no inbox item "+id)
}
