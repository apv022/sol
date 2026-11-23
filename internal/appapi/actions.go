package appapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/apv022/sol/internal/feeds"
	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/session"
	"github.com/apv022/sol/sdk/go/sol"
)

// ActivityFeed is the internal feed (under feeds.InternalApp) holding the activity log: every
// action that changed something, signed by the device that ran it like any entry, and
// replicated to my other devices.
const ActivityFeed = "activity"

// maxOutput bounds an action's output relayed to the caller.
const maxOutput = 16 << 20

// caller is who runs an action, as the app will see it in the Sol-* headers.
type caller struct {
	person, device, agent string
	self                  bool
}

func (c caller) set(h http.Header) {
	h.Set("Sol-Person", c.person)
	h.Set("Sol-Device", c.device)
	if c.agent != "" {
		h.Set("Sol-Agent", c.agent)
	}
	if c.self {
		h.Set("Sol-Self", "1")
	}
}

// localCaller is me, on this device.
func (s *Server) localCaller() caller {
	me, dev := s.me()
	return caller{person: me, device: dev, self: true}
}

// peerCaller is the verified session peer of a request.
func peerCaller(p session.Peer) caller {
	return caller{person: identity.ID(p.Person), device: identity.ID(p.Device), self: p.Relation == session.Self}
}

// dialer returns how to reach an app address in SOL_APP_LISTEN form.
func dialer(addr string) (network, a string) {
	if p, ok := strings.CutPrefix(addr, "unix://"); ok {
		return "unix", p
	}
	return "tcp", addr
}

// client returns the HTTP client for an app's socket.
func (s *Server) client(addr string) *http.Client {
	if c, ok := s.clients.Load(addr); ok {
		return c.(*http.Client)
	}
	network, a := dialer(addr)
	c := &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, a)
		},
		MaxIdleConnsPerHost: 4,
		IdleConnTimeout:     time.Minute,
	}}
	got, _ := s.clients.LoadOrStore(addr, c)
	return got.(*http.Client)
}

// actionList serves GET /v1/actions?app=.
func (s *Server) actionList(q req, w http.ResponseWriter, r *http.Request) (any, error) {
	app := r.URL.Query().Get("app")
	out := []sol.ActionSpec{}
	for _, a := range s.cfg.Apps.List() {
		if app == "" || a.Manifest.ID == app {
			out = append(out, a.Manifest.Actions...)
		}
	}
	return out, nil
}

// callAction serves POST /v1/actions/call.
func (s *Server) callAction(q req, w http.ResponseWriter, r *http.Request) (any, error) {
	var b sol.ActionCall
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	out, err := s.Call(r.Context(), q.app, b)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Call runs an action for the app named requester: on this device, or with c.Person on that
// person's device over a session. It returns the output, or {"approval": id}.
func (s *Server) Call(ctx context.Context, requester string, c sol.ActionCall) (json.RawMessage, error) {
	if _, _, err := splitAction(c.Action); err != nil {
		return nil, err
	}
	if me, _ := s.me(); c.Person != "" && c.Person != me {
		return s.callRemote(ctx, c, "")
	}
	return s.run(ctx, requester, c, s.localCaller(), false)
}

// CallAs runs an action for the CLI: as me, or with agent (an agent key's ID) as that agent,
// whose calls are subject to the actions' "agents" approval policy on the device that runs
// them. Approvals are filed under the action's own app. The agent's scopes are the caller's to
// check.
func (s *Server) CallAs(ctx context.Context, agent string, c sol.ActionCall) (json.RawMessage, error) {
	app, _, err := splitAction(c.Action)
	if err != nil {
		return nil, err
	}
	if me, _ := s.me(); c.Person != "" && c.Person != me {
		// The target device applies its own approval policy to the agent (Sol-Agent).
		return s.callRemote(ctx, c, agent)
	}
	who := s.localCaller()
	who.agent = agent
	return s.run(ctx, app, c, who, false)
}

// needsApproval applies an action's approval policy to a caller (agent "" is a person).
func needsApproval(spec sol.ActionSpec, agent string) bool {
	return spec.Approval == sol.ApprovalAlways || spec.Approval == sol.ApprovalAgents && agent != ""
}

// fileApproval posts an approval item for a call and returns {"approval": id}.
func (s *Server) fileApproval(requester string, c sol.ActionCall, agent string) (json.RawMessage, error) {
	id, err := s.inbox.post(item{App: requester, Kind: "approval", Title: "Approve " + c.Action + "?",
		Action: &sol.ActionCall{Action: c.Action, Args: c.Args, Person: c.Person}, Agent: agent})
	if err != nil {
		return nil, err
	}
	s.inboxEvent(requester, id)
	return json.Marshal(map[string]string{"approval": id})
}

func splitAction(name string) (app, verb string, err error) {
	app, verb, ok := strings.Cut(name, ".")
	if !ok || !feeds.ValidApp(app) || verb == "" {
		return "", "", sol.Errorf("bad_request", "action must be <app>.<verb>")
	}
	return app, verb, nil
}

// run runs an action of an app installed here. approved skips the approval policy (the person
// approved it). requester is the app on whose behalf approvals are filed.
func (s *Server) run(ctx context.Context, requester string, c sol.ActionCall, who caller, approved bool) (json.RawMessage, error) {
	appID, _, err := splitAction(c.Action)
	if err != nil {
		return nil, err
	}
	a, ok := s.cfg.Apps.App(appID)
	if !ok {
		return nil, sol.Errorf("not_found", "no app "+appID)
	}
	spec, ok := a.Manifest.Action(c.Action)
	if !ok {
		return nil, sol.Errorf("not_found", "no such action "+c.Action)
	}
	// An app calling another app's action that changes something is not the person acting:
	// it needs the person's approval, whatever the action's policy (security review F4).
	// Otherwise any app with the actions scope could delete my files as me.
	crossApp := requester != appID && spec.Effect != sol.EffectRead
	if !approved && !c.DryRun && (needsApproval(spec, who.agent) || crossApp) {
		return s.fileApproval(requester, sol.ActionCall{Action: c.Action, Args: c.Args}, who.agent)
	}
	if a.Addr == "" && strings.HasPrefix(a.Manifest.Exec, "external") {
		// An external app (a separate Android APK, say) calls the API but does not listen for
		// calls: the daemon has no way to reach it, so its declared actions are not callable.
		return nil, sol.Errorf("not_implemented", "app "+appID+" runs outside Sol and cannot receive action calls")
	}
	if a.Addr == "" {
		return nil, sol.Errorf("unavailable", "app "+appID+" is not running")
	}
	out, err := s.dispatch(ctx, a.Addr, c, who)
	if spec.Effect != sol.EffectRead && !c.DryRun {
		s.logActivity(who, appID, c, err)
	}
	return out, err
}

// dispatch sends POST /_sol/actions/{name} to the app's socket and relays its reply.
func (s *Server) dispatch(ctx context.Context, addr string, c sol.ActionCall, who caller) (json.RawMessage, error) {
	args := c.Args
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://app/_sol/actions/"+c.Action, bytes.NewReader(args))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Content-Type", "application/json")
	who.set(hr.Header)
	if c.DryRun {
		hr.Header.Set("Sol-Dry-Run", "1")
	}
	resp, err := s.client(addr).Do(hr)
	if err != nil {
		return nil, sol.Errorf("unavailable", "app is not answering")
	}
	defer resp.Body.Close()
	return relay(resp)
}

// relay turns an HTTP reply from an app or a peer into output or an API error.
func relay(resp *http.Response) (json.RawMessage, error) {
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxOutput))
	if err != nil {
		return nil, sol.Errorf("unavailable", "reply cut short")
	}
	if resp.StatusCode/100 == 2 {
		body = bytes.TrimSpace(body)
		if len(body) == 0 {
			body = []byte("{}")
		}
		if !json.Valid(body) {
			return nil, sol.Errorf("internal", "the app replied with something other than JSON")
		}
		return body, nil
	}
	var w struct {
		Error *sol.Error `json:"error"`
	}
	if json.Unmarshal(body, &w) == nil && w.Error != nil && w.Error.Code != "" {
		return nil, w.Error
	}
	return nil, sol.Errorf("internal", http.StatusText(resp.StatusCode))
}

// logActivity appends a row to the activity log. Failures are logged, never fatal: the action
// already ran.
func (s *Server) logActivity(who caller, app string, c sol.ActionCall, err error) {
	sum := sha256.Sum256(c.Args)
	result := "ok"
	if err != nil {
		result = "error: " + err.Error()
	}
	row := sol.ActivityEntry{Time: time.Now().UTC(), Person: who.person, Device: who.device, Agent: who.agent,
		App: app, Action: c.Action, ArgsDigest: "sha256:" + hex.EncodeToString(sum[:]), Result: result}
	b, _ := json.Marshal(row)
	if _, err := s.n.Store.Append(feeds.InternalApp, ActivityFeed, b); err != nil {
		s.log.Warn("activity log", "err", err)
	}
}

// activity serves GET /v1/activity?limit=: the newest rows, oldest first.
func (s *Server) activity(q req, w http.ResponseWriter, r *http.Request) (any, error) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 100
	}
	limit = min(limit, 5000)
	out := []sol.ActivityEntry{}
	me, _ := s.me()
	addr := feeds.Address(me, feeds.InternalApp, ActivityFeed)
	var pos uint64
	for _, f := range s.n.Store.Feeds() {
		if f.Feed == addr {
			pos = f.Pos
		}
	}
	from := uint64(1)
	if pos > uint64(limit) {
		from = pos - uint64(limit) + 1
	}
	es, err := s.n.Store.Read(addr, from, limit)
	if err != nil {
		return nil, feedErr(err)
	}
	for _, e := range es {
		var row sol.ActivityEntry
		if json.Unmarshal(e.Body, &row) == nil {
			out = append(out, row)
		}
	}
	return out, nil
}

// sessionTo returns a live session with one of person's devices (device, if given).
func (s *Server) sessionTo(person, device string) (*session.Session, error) {
	pub, err := identity.ParseID(person)
	if err != nil {
		return nil, sol.Errorf("not_found", "unknown person")
	}
	me, _ := s.me()
	if person != me && !s.n.Contacts.IsContact(pub) {
		return nil, sol.Errorf("not_found", "unknown person")
	}
	var devs [][]byte
	if person == me {
		if b, err := s.n.Keyring.Bundle(); err == nil {
			for _, d := range b.Devices.Devices {
				devs = append(devs, d.Key)
			}
		}
	} else if _, dl, ok := s.n.Cache.Latest(pub); ok {
		for _, d := range dl.Devices {
			devs = append(devs, d.Key)
		}
	}
	if device != "" {
		if !slices.ContainsFunc(devs, func(d []byte) bool { return identity.ID(d) == device }) {
			return nil, sol.Errorf("not_found", "unknown device")
		}
		devs = devs[:0]
		k, _ := identity.ParseID(device)
		devs = append(devs, k)
	}
	for _, d := range devs {
		if ss, ok := s.n.Sessions.Session(d); ok && ss.Alive() {
			return ss, nil
		}
	}
	return nil, sol.Errorf("unavailable", "person is offline")
}

// callRemote sends an action call to a contact's device: POST /p/actions/{name}. agent, if
// set, travels as Sol-Agent so the peer applies its "agents" approval policy too.
func (s *Server) callRemote(ctx context.Context, c sol.ActionCall, agent string) (json.RawMessage, error) {
	ss, err := s.sessionTo(c.Person, "")
	if err != nil {
		return nil, err
	}
	args := c.Args
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	hr, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://peer/p/actions/"+c.Action, bytes.NewReader(args))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Content-Type", "application/json")
	if c.DryRun {
		hr.Header.Set("Sol-Dry-Run", "1")
	}
	if agent != "" {
		hr.Header.Set("Sol-Agent", agent)
	}
	resp, err := ss.HTTPClient().Do(hr)
	if err != nil {
		return nil, sol.Errorf("unavailable", "person is unreachable")
	}
	defer resp.Body.Close()
	return relay(resp)
}

// peerAction serves POST /p/actions/{name} to a verified session peer. My own devices may call
// anything. A contact may call read actions, and actions that change something only of an app
// that shares at least one feed with them: sharing is what opens an app to a person. The app
// still decides by the caller headers.
func (s *Server) peerAction(w http.ResponseWriter, r *http.Request) {
	p, ok := session.PeerFrom(r.Context())
	if !ok {
		sol.WriteError(w, sol.Errorf("forbidden", "not a session peer"))
		return
	}
	name := r.PathValue("name")
	appID, _, err := splitAction(name)
	if err != nil {
		sol.WriteError(w, err)
		return
	}
	args, err := io.ReadAll(io.LimitReader(r.Body, 4<<20))
	if err != nil {
		sol.WriteError(w, sol.Errorf("bad_request", "bad body"))
		return
	}
	who := peerCaller(p)
	// A peer may say the call comes from one of its person's agents. That only ever adds
	// restrictions (the "agents" approval policy), so it is taken as given.
	if ag := r.Header.Get("Sol-Agent"); ag != "" {
		if _, err := identity.ParseID(ag); err != nil {
			sol.WriteError(w, sol.Errorf("bad_request", "bad Sol-Agent"))
			return
		}
		who.agent = ag
	}
	if !who.self {
		a, ok := s.cfg.Apps.App(appID)
		if !ok {
			sol.WriteError(w, sol.Errorf("not_found", "no app "+appID))
			return
		}
		spec, ok := a.Manifest.Action(name)
		if ok && spec.Effect != sol.EffectRead && !s.sharesApp(who.person, appID) {
			s.log.Info("refused peer action", "peer", who.person[:8], "action", name)
			sol.WriteError(w, sol.Errorf("forbidden", "nothing of this app is shared with you"))
			return
		}
	}
	out, err := s.run(r.Context(), appID, sol.ActionCall{Action: name, Args: args, DryRun: r.Header.Get("Sol-Dry-Run") == "1"}, who, false)
	if err != nil {
		var se *sol.Error
		if !errors.As(err, &se) {
			err = sol.Errorf("internal", "action failed")
		}
		sol.WriteError(w, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(out)
}

// sharesApp reports whether I share any feed of app with person.
func (s *Server) sharesApp(person, app string) bool {
	for addr := range s.n.Shares.ReadableBy(person) {
		if _, a, _, err := feeds.ParseAddress(addr); err == nil && a == app {
			return true
		}
	}
	return false
}

// PeerHandler serves the session endpoints of the app API: /p/actions/{name}, /p/dial,
// /p/inbox and /p/ui/{app}/... Mount it on the session manager's handler.
func (s *Server) PeerHandler() http.Handler {
	mx := http.NewServeMux()
	mx.HandleFunc("POST /p/actions/{name}", s.peerAction)
	mx.HandleFunc("POST /p/dial", s.peerDial)
	mx.HandleFunc("POST /p/inbox", s.peerInbox)
	mx.HandleFunc(PeerUIPath+"{app}/{path...}", s.peerUI)
	return mx
}
