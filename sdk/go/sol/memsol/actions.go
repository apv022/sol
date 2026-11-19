package memsol

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"

	"github.com/apv022/sol/sdk/go/sol"
)

// InboxItem is an inbox entry of a Person.
type InboxItem struct {
	ID       string
	App      string
	Kind     string
	Title    string
	Body     string
	Answer   string // "", "approved" or "denied"
	action   *sol.ActionCall
	requests *Client
}

// Inbox lists the person's inbox items.
func (p *Person) Inbox() []InboxItem {
	p.n.mu.Lock()
	defer p.n.mu.Unlock()
	out := make([]InboxItem, len(p.inbox))
	for i, it := range p.inbox {
		out[i] = *it
	}
	return out
}

// Answer approves or denies an approval item. On approval the stored action runs as the
// requesting app, and an "inbox" event reports the answer.
func (p *Person) Answer(id string, approve bool) error {
	n := p.n
	n.mu.Lock()
	var it *InboxItem
	for _, x := range p.inbox {
		if x.ID == id {
			it = x
		}
	}
	if it == nil || it.Kind != "approval" || it.Answer != "" {
		n.mu.Unlock()
		return sol.Errorf("not_found", "no open approval "+id)
	}
	it.Answer = "denied"
	if approve {
		it.Answer = "approved"
	}
	n.append(logged{kind: sol.EventInbox, app: it.App, target: p.ID,
		inbox: &sol.InboxEvent{ID: it.ID, Kind: it.Kind, Title: it.Title, Answer: it.Answer}})
	n.mu.Unlock()
	if approve && it.action != nil {
		_, err := it.requests.invoke(*it.action, true)
		return err
	}
	return nil
}

// PostInbox implements sol.API.
func (c *Client) PostInbox(_ context.Context, post sol.InboxPost) (sol.InboxResult, error) {
	if post.Kind != "notice" && post.Kind != "approval" {
		return sol.InboxResult{}, sol.Errorf("bad_request", "kind must be notice or approval")
	}
	if post.Kind == "approval" && post.Action == nil {
		return sol.InboxResult{}, sol.Errorf("bad_request", "approval needs an action")
	}
	n := c.n
	n.mu.Lock()
	defer n.mu.Unlock()
	to := c.person
	if post.To != "" && post.To != "self" {
		to = n.people[post.To]
		if to == nil || !n.paired(c.person.ID, to.ID) {
			return sol.InboxResult{}, sol.Errorf("not_found", "unknown person")
		}
	}
	if post.Kind == "approval" && to != c.person {
		return sol.InboxResult{}, sol.Errorf("bad_request", "approvals go to yourself")
	}
	return c.postLocked(to, post.Kind, post.Title, post.Body, post.Action), nil
}

func (c *Client) postLocked(to *Person, kind, title, body string, act *sol.ActionCall) sol.InboxResult {
	n := c.n
	n.nextIn++
	it := &InboxItem{ID: "in" + strconv.Itoa(n.nextIn), App: c.app, Kind: kind, Title: title, Body: body, action: act, requests: c}
	to.inbox = append(to.inbox, it)
	n.append(logged{kind: sol.EventInbox, app: c.app, target: to.ID,
		inbox: &sol.InboxEvent{ID: it.ID, Kind: kind, Title: title}})
	return sol.InboxResult{ID: it.ID}
}

// Activity implements sol.API.
func (c *Client) Activity(_ context.Context, limit int) ([]sol.ActivityEntry, error) {
	c.n.mu.Lock()
	defer c.n.mu.Unlock()
	if limit <= 0 {
		limit = 100
	}
	a := c.person.activity
	if len(a) > limit {
		a = a[len(a)-limit:]
	}
	return append([]sol.ActivityEntry{}, a...), nil
}

// Actions implements sol.API.
func (c *Client) Actions(_ context.Context, app string) ([]sol.ActionSpec, error) {
	c.n.mu.Lock()
	defer c.n.mu.Unlock()
	out := []sol.ActionSpec{}
	for id, r := range c.person.apps {
		if r.manifest != nil && (app == "" || app == id) {
			out = append(out, r.manifest.Actions...)
		}
	}
	return out, nil
}

// CallAction implements sol.API.
func (c *Client) CallAction(_ context.Context, req sol.ActionCall) (sol.ActionResult, error) {
	return c.invoke(req, false)
}

// invoke runs an action. approved skips the approval policy (used when a person approved it).
func (c *Client) invoke(req sol.ActionCall, approved bool) (sol.ActionResult, error) {
	app, verb, ok := strings.Cut(req.Action, ".")
	if !ok || !sol.ValidAppID(app) || verb == "" {
		return sol.ActionResult{}, sol.Errorf("bad_request", "action must be <app>.<verb>")
	}
	n := c.n
	n.mu.Lock()
	target := c.person
	if req.Person != "" {
		target = n.people[req.Person]
		if target == nil || !n.paired(c.person.ID, target.ID) {
			n.mu.Unlock()
			return sol.ActionResult{}, sol.Errorf("not_found", "unknown person")
		}
		if !target.online {
			n.mu.Unlock()
			return sol.ActionResult{}, sol.Errorf("unavailable", "person is offline")
		}
	}
	reg := target.apps[app]
	if reg == nil || reg.handler == nil {
		n.mu.Unlock()
		return sol.ActionResult{}, sol.Errorf("not_found", "no app "+app)
	}
	spec := sol.ActionSpec{Name: req.Action, Effect: sol.EffectWrite}
	if reg.manifest != nil {
		s, ok := reg.manifest.Action(req.Action)
		if !ok {
			n.mu.Unlock()
			return sol.ActionResult{}, sol.Errorf("not_found", "no such action")
		}
		spec = s
	}
	if !approved && !req.DryRun && (spec.Approval == sol.ApprovalAlways || (spec.Approval == sol.ApprovalAgents && c.agent != "")) {
		r := c.postLocked(c.person, "approval", "Approve "+req.Action+"?", "", &sol.ActionCall{Action: req.Action, Args: req.Args, Person: req.Person})
		n.mu.Unlock()
		return sol.ActionResult{Approval: r.ID}, nil
	}
	h := reg.handler
	n.mu.Unlock()

	hr := httptest.NewRequest("POST", "/_sol/actions/"+req.Action, bytes.NewReader(req.Args))
	hr.Header.Set("Sol-Person", c.person.ID)
	hr.Header.Set("Sol-Device", c.device.ID)
	if c.agent != "" {
		hr.Header.Set("Sol-Agent", c.agent)
	}
	if target == c.person {
		hr.Header.Set("Sol-Self", "1")
	}
	if req.DryRun {
		hr.Header.Set("Sol-Dry-Run", "1")
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, hr)
	var res sol.ActionResult
	var err error
	if rec.Code/100 == 2 {
		res.Output = bytes.TrimSpace(rec.Body.Bytes())
	} else {
		var w struct {
			Error *sol.Error `json:"error"`
		}
		if json.Unmarshal(rec.Body.Bytes(), &w) == nil && w.Error != nil {
			err = w.Error
		} else {
			err = sol.Errorf("internal", http.StatusText(rec.Code))
		}
	}
	if spec.Effect != sol.EffectRead && !req.DryRun {
		sum := sha256.Sum256(req.Args)
		result := "ok"
		if err != nil {
			result = "error: " + err.Error()
		}
		n.mu.Lock()
		target.activity = append(target.activity, sol.ActivityEntry{Time: n.Now().UTC(), Person: c.person.ID,
			Device: c.device.ID, Agent: c.agent, App: app, Action: req.Action,
			ArgsDigest: "sha256:" + hex.EncodeToString(sum[:]), Result: result})
		n.mu.Unlock()
	}
	return res, err
}

// Dial implements sol.API: the target app's DialHandler gets one end of a net.Pipe.
func (c *Client) Dial(_ context.Context, req sol.DialRequest) (net.Conn, error) {
	n := c.n
	n.mu.Lock()
	t := n.people[req.Person]
	if t == nil || !n.paired(c.person.ID, t.ID) {
		n.mu.Unlock()
		return nil, sol.Errorf("not_found", "unknown person")
	}
	if !t.online {
		n.mu.Unlock()
		return nil, sol.Errorf("unavailable", "person is offline")
	}
	if req.Device != "" && t.device(req.Device) == nil {
		n.mu.Unlock()
		return nil, sol.Errorf("not_found", "unknown device")
	}
	reg := t.apps[req.App]
	if reg == nil || reg.dial == nil {
		n.mu.Unlock()
		return nil, sol.Errorf("not_found", "app does not accept streams")
	}
	h := reg.dial
	n.mu.Unlock()
	a, b := net.Pipe()
	go h(b, sol.Caller{Person: c.person.ID, Device: c.device.ID, Agent: c.agent, Self: t == c.person}, req.Path)
	return a, nil
}
