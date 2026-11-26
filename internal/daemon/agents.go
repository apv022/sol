package daemon

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/apv022/sol/internal/feeds"
	"github.com/apv022/sol/internal/fsutil"
	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/sdk/go/sol"
)

// Agent keys (design/IDENTITY.md "Agent certificate", API.md §6 "Agents").
//
// An agent key G lives where the agent runs, in a key file (AgentKey, 0600) the CLI writes; the
// daemon never sees its private half. The device that issued the certificate keeps a registry
// of what it issued (agents.json, 0600) for `sol agent list`, revocation and renewal.
// Revocations are entries in the internal "revocations" feed, which every device of mine
// replicates and checks, because a certificate is good on any of my devices.
//
// An agent calls /agent/v1/... on the app API with three headers instead of a bearer token:
//
//	Sol-Agent-Cert  base64 of the certificate's wire form
//	Sol-Agent-Time  unix seconds
//	Sol-Agent-Nonce base64 of 16 random bytes (two equal requests in one second differ)
//	Sol-Agent-Sig   base64 of G's signature of
//	                lp("sol agent-request v1") | lp(method) | lp(path?query) | u64(time)
//	                | lp(nonce) | lp(sha256(body))
//
// The daemon checks the certificate against my current device list (VerifyAgent: right
// person, a device of mine signed it, not expired), the time (within AgentClockSkew), the
// signature, that the signature was not seen before (replay), and the revocations. Calls
// then run with Sol-Agent set to the agent's ID, limited to its scopes. When the certificate
// has less than a third of its lifetime left and this device issued it, the reply carries a
// renewed one in Sol-Agent-Renewed, and the CLI replaces its key file.

// RevocationsFeed is the internal feed (under feeds.InternalApp) of revoked agent keys.
const RevocationsFeed = "revocations"

// AgentsFeed is the internal feed where each device announces the agents it issues (name, id,
// scopes, times; never the certificate), so `sol agent list` shows agents of all my devices.
const AgentsFeed = "agents"

// AgentClockSkew bounds how far an agent request's time may be from the daemon's.
const AgentClockSkew = 2 * time.Minute

// agentRequestContext starts every signed agent request.
const agentRequestContext = "sol agent-request v1"

var agentNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

// AgentInfo is one agent this device issued (GET /admin/v1/agents, sol agent list).
type AgentInfo struct {
	Name    string    `json:"name"`
	ID      string    `json:"id"`
	Scopes  []string  `json:"scopes"`
	Issued  time.Time `json:"issued"`
	Expires time.Time `json:"expires"`
	Revoked bool      `json:"revoked,omitempty"`
	// Device and DeviceName are the device that issued the agent (this one, or another of
	// mine learned through the replicated agents feed).
	Device     string `json:"device,omitempty"`
	DeviceName string `json:"device_name,omitempty"`
	// Cert is the current certificate's wire form, for renewal.
	Cert []byte `json:"cert"`
}

// AgentKey is the content of an agent key file: everything an agent needs to call, and the
// one secret it holds. Written 0600, atomically; never logged.
type AgentKey struct {
	Name string `json:"name"`
	// Key is G's ed25519 seed (32 bytes).
	Key []byte `json:"key"`
	// Cert is the certificate's wire form.
	Cert []byte `json:"cert"`
}

// NewAgentKey generates an agent key pair for name (the certificate comes from the daemon).
func NewAgentKey(name string) (AgentKey, ed25519.PublicKey, error) {
	pub, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		return AgentKey{}, nil, err
	}
	return AgentKey{Name: name, Key: priv.Seed()}, pub, nil
}

// LoadAgentKey reads an agent key file.
func LoadAgentKey(path string) (AgentKey, error) {
	var k AgentKey
	b, err := os.ReadFile(path)
	if err != nil {
		return k, err
	}
	if err := json.Unmarshal(b, &k); err != nil || len(k.Key) != ed25519.SeedSize {
		return k, errors.New("daemon: not an agent key file")
	}
	return k, nil
}

// Save writes the key file (0600, atomically).
func (k AgentKey) Save(path string) error {
	b, _ := json.MarshalIndent(k, "", "  ")
	return fsutil.WriteFileAtomic(path, b, 0o600)
}

// private returns G.
func (k AgentKey) private() ed25519.PrivateKey { return ed25519.NewKeyFromSeed(k.Key) }

// agentMessage is what an agent signs for one request.
func agentMessage(method, uri string, ts int64, nonce, body []byte) []byte {
	lp := func(b, x []byte) []byte { return append(binary.BigEndian.AppendUint32(b, uint32(len(x))), x...) }
	h := sha256.Sum256(body)
	m := lp(nil, []byte(agentRequestContext))
	m = lp(m, []byte(method))
	m = lp(m, []byte(uri))
	m = binary.BigEndian.AppendUint64(m, uint64(ts))
	m = lp(m, nonce)
	return lp(m, h[:])
}

// sign sets the agent headers of a request whose body is body.
func (k AgentKey) sign(r *http.Request, body []byte, now time.Time) {
	ts := now.Unix()
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	sig := ed25519.Sign(k.private(), agentMessage(r.Method, r.URL.RequestURI(), ts, nonce, body))
	r.Header.Set("Sol-Agent-Nonce", base64.StdEncoding.EncodeToString(nonce))
	r.Header.Set("Sol-Agent-Cert", base64.StdEncoding.EncodeToString(k.Cert))
	r.Header.Set("Sol-Agent-Time", strconv.FormatInt(ts, 10))
	r.Header.Set("Sol-Agent-Sig", base64.StdEncoding.EncodeToString(sig))
}

// agents is the registry of the agents this device issued, plus the replay window.
type agents struct {
	path string

	mu   sync.Mutex
	list []AgentInfo
	seen map[[32]byte]time.Time // signatures seen within the clock window
}

func openAgents(path string) (*agents, error) {
	a := &agents{path: path, seen: map[[32]byte]time.Time{}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return a, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &a.list); err != nil {
		return nil, errors.New("daemon: agents.json is corrupt")
	}
	return a, nil
}

// saveLocked writes agents.json (0600, atomic). a.mu held.
func (a *agents) saveLocked() error {
	b, _ := json.MarshalIndent(a.list, "", "  ")
	return fsutil.WriteFileAtomic(a.path, b, 0o600)
}

// put adds or replaces the record named info.Name.
func (a *agents) put(info AgentInfo) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for i := range a.list {
		if a.list[i].Name == info.Name {
			a.list[i] = info
			return a.saveLocked()
		}
	}
	a.list = append(a.list, info)
	return a.saveLocked()
}

// find returns the record whose name or ID is key.
func (a *agents) find(key string) (AgentInfo, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, x := range a.list {
		if x.Name == key || x.ID == key {
			return x, true
		}
	}
	return AgentInfo{}, false
}

// all returns every record, by name.
func (a *agents) all() []AgentInfo {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := append([]AgentInfo{}, a.list...)
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// fresh records a request signature and reports whether it is new (not a replay).
func (a *agents) fresh(sig []byte, now time.Time) bool {
	h := sha256.Sum256(sig)
	a.mu.Lock()
	defer a.mu.Unlock()
	for k, t := range a.seen {
		if now.Sub(t) > 2*AgentClockSkew {
			delete(a.seen, k)
		}
	}
	if _, dup := a.seen[h]; dup {
		return false
	}
	a.seen[h] = now
	return true
}

func infoOf(c identity.AgentCert) AgentInfo {
	return AgentInfo{Name: c.Name, ID: identity.ID(c.Agent), Scopes: c.Scopes,
		Issued: time.Unix(int64(c.Issued), 0).UTC(), Expires: time.Unix(int64(c.Expires), 0).UTC(), Cert: c.Marshal()}
}

// IssueAgent signs a certificate for the agent key pub, named name, with scopes, valid for
// ttl (0: the default), and records it.
func (d *Daemon) IssueAgent(name string, pub ed25519.PublicKey, scopes []string, ttl time.Duration) (AgentInfo, error) {
	if !agentNameRe.MatchString(name) {
		return AgentInfo{}, sol.Errorf("bad_request", "an agent name is lowercase letters, digits, . _ - (up to 64)")
	}
	if prev, ok := d.agents.find(name); ok && !prev.Revoked && time.Now().Before(prev.Expires) {
		return AgentInfo{}, sol.Errorf("conflict", "an agent named "+name+" exists; revoke it first")
	}
	if len(pub) != ed25519.PublicKeySize {
		return AgentInfo{}, sol.Errorf("bad_request", "bad agent key")
	}
	if len(scopes) == 0 {
		return AgentInfo{}, sol.Errorf("bad_request", "an agent needs at least one scope (e.g. files.* or *)")
	}
	c, err := d.Keyring.IssueAgent(pub, name, scopes, ttl, time.Now())
	if err != nil {
		return AgentInfo{}, sol.Errorf("bad_request", err.Error())
	}
	info := infoOf(c)
	if err := d.agents.put(info); err != nil {
		return AgentInfo{}, err
	}
	d.announceAgent(info)
	d.log.Info("issued an agent key", "agent", name, "id", info.ID[:8])
	return info, nil
}

// RenewAgent reissues the certificate of an agent this device issued, for ttl (0: the
// lifetime it had).
func (d *Daemon) RenewAgent(name string, ttl time.Duration) (AgentInfo, error) {
	info, ok := d.agents.find(name)
	if !ok {
		return AgentInfo{}, sol.Errorf("not_found", "no agent "+name+" issued on this device")
	}
	if info.Revoked || d.revoked(info.ID) {
		return AgentInfo{}, sol.Errorf("forbidden", "agent "+name+" is revoked")
	}
	c, err := identity.ParseAgentCert(info.Cert)
	if err != nil {
		return AgentInfo{}, err
	}
	if ttl == 0 {
		ttl = time.Duration(c.Expires-c.Issued) * time.Second
	}
	nc, err := d.Keyring.RenewAgent(c, ttl, time.Now())
	if err != nil {
		return AgentInfo{}, sol.Errorf("bad_request", err.Error())
	}
	info = infoOf(nc)
	d.announceAgent(info)
	return info, d.agents.put(info)
}

// announceAgent appends the agent's public facts to the agents feed (best effort: the local
// registry is the source of truth on the issuing device).
func (d *Daemon) announceAgent(info AgentInfo) {
	b, _ := json.Marshal(AgentInfo{Name: info.Name, ID: info.ID, Scopes: info.Scopes, Issued: info.Issued, Expires: info.Expires})
	if _, err := d.Store.Append(feeds.InternalApp, AgentsFeed, b); err != nil {
		d.log.Warn("announcing an agent to my other devices", "err", err)
	}
}

// thisDevice returns this device's ID and name.
func (d *Daemon) thisDevice() (id, name string) {
	for _, dv := range d.Devices() {
		if dv.This {
			return dv.ID, dv.Name
		}
	}
	_, id = d.me()
	return id, ""
}

// AllAgents lists the agents of all my devices: those this device issued from its registry,
// the others from the agents feed (without certificates: only the issuing device can renew
// one). Revoked comes from the replicated revocations feed.
func (d *Daemon) AllAgents() []AgentInfo {
	revoked := d.revokedSet()
	me, meName := d.thisDevice()
	names := map[string]string{}
	for _, dv := range d.Devices() {
		names[dv.ID] = dv.Name
	}
	out := []AgentInfo{}
	have := map[string]bool{}
	for _, a := range d.agents.all() {
		a.Device, a.DeviceName = me, meName
		a.Revoked = a.Revoked || revoked[a.ID]
		have[a.ID] = true
		out = append(out, a)
	}
	if p := d.PersonID(); p != "" {
		latest := map[string]AgentInfo{}
		addr := feeds.Address(p, feeds.InternalApp, AgentsFeed)
		for from := uint64(1); ; {
			es, err := d.Store.Read(addr, from, 1000)
			if err != nil || len(es) == 0 {
				break
			}
			for _, e := range es {
				from = e.Pos + 1
				var a AgentInfo
				if json.Unmarshal(e.Body, &a) != nil || a.ID == "" || e.Device == me || have[a.ID] {
					continue
				}
				a.Cert, a.Device, a.DeviceName = nil, e.Device, names[e.Device]
				a.Revoked = revoked[a.ID]
				latest[a.ID] = a
			}
		}
		for _, a := range latest {
			out = append(out, a)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	return out
}

// RevokeAgent revokes an agent by name (issued here) or by ID (issued on any device of mine):
// the revocation goes into the replicated revocations feed, so every device refuses it.
func (d *Daemon) RevokeAgent(key string) (string, error) {
	id := key
	info, local := d.agents.find(key)
	if local {
		id = info.ID
	} else if _, err := identity.ParseID(key); err != nil {
		// A name: look among the agents my other devices issued.
		id = ""
		for _, a := range d.AllAgents() {
			if a.Name == key && !a.Revoked {
				id = a.ID
			}
		}
		if id == "" {
			return "", sol.Errorf("not_found", "no agent "+key)
		}
	}
	if !d.revoked(id) {
		b, _ := json.Marshal(map[string]any{"agent": id, "time": time.Now().UTC()})
		if _, err := d.Store.Append(feeds.InternalApp, RevocationsFeed, b); err != nil {
			return "", err
		}
	}
	if local {
		info.Revoked = true
		if err := d.agents.put(info); err != nil {
			return "", err
		}
	}
	d.log.Info("revoked an agent key", "id", id[:8])
	return id, nil
}

// revoked reports whether any device of mine revoked the agent ID.
func (d *Daemon) revoked(id string) bool { return d.revokedSet()[id] }

// revokedSet reads the replicated revocations feed.
func (d *Daemon) revokedSet() map[string]bool {
	out := map[string]bool{}
	me := d.PersonID()
	if me == "" {
		return out
	}
	addr := feeds.Address(me, feeds.InternalApp, RevocationsFeed)
	for from := uint64(1); ; {
		es, err := d.Store.Read(addr, from, 1000)
		if err != nil || len(es) == 0 {
			return out
		}
		for _, e := range es {
			var r struct {
				Agent string `json:"agent"`
			}
			if json.Unmarshal(e.Body, &r) == nil && r.Agent != "" {
				out[r.Agent] = true
			}
			from = e.Pos + 1
		}
	}
}

// agentAuth authenticates an agent request and returns its certificate and body.
func (d *Daemon) agentAuth(r *http.Request) (identity.AgentCert, []byte, error) {
	var c identity.AgentCert
	unauthorized := func(msg string) (identity.AgentCert, []byte, error) {
		return c, nil, sol.Errorf("unauthorized", msg)
	}
	raw, err1 := base64.StdEncoding.DecodeString(r.Header.Get("Sol-Agent-Cert"))
	sig, err2 := base64.StdEncoding.DecodeString(r.Header.Get("Sol-Agent-Sig"))
	ts, err3 := strconv.ParseInt(r.Header.Get("Sol-Agent-Time"), 10, 64)
	nonce, err4 := base64.StdEncoding.DecodeString(r.Header.Get("Sol-Agent-Nonce"))
	if errors.Join(err1, err2, err3, err4) != nil || len(raw) == 0 || len(sig) != ed25519.SignatureSize || len(nonce) != 16 {
		return unauthorized("agent credentials missing")
	}
	c, err := identity.ParseAgentCert(raw)
	if err != nil {
		return unauthorized("bad agent certificate")
	}
	now := time.Now()
	b, err := d.Keyring.Bundle()
	if err != nil {
		return unauthorized("this device has not joined a person yet")
	}
	if err := identity.VerifyAgent(c, b, now); err != nil {
		if errors.Is(err, identity.ErrExpired) {
			return unauthorized("the agent certificate expired (or is not valid yet); renew it with sol agent renew " + c.Name)
		}
		return unauthorized("the agent certificate is not valid for this person")
	}
	if t := time.Unix(ts, 0); t.Before(now.Add(-AgentClockSkew)) || t.After(now.Add(AgentClockSkew)) {
		return unauthorized("the agent's clock is off by more than " + AgentClockSkew.String())
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
	if err != nil {
		return unauthorized("bad body")
	}
	if !ed25519.Verify(c.Agent, agentMessage(r.Method, r.URL.RequestURI(), ts, nonce, body), sig) {
		return unauthorized("bad agent signature")
	}
	if !d.agents.fresh(sig, now) {
		return unauthorized("replayed agent request")
	}
	if d.revoked(identity.ID(c.Agent)) {
		return unauthorized("the agent key is revoked")
	}
	return c, body, nil
}

// renewIfDue renews a certificate this device issued once a third of its lifetime is left,
// and returns the new wire form ("" otherwise).
func (d *Daemon) renewIfDue(c identity.AgentCert) []byte {
	life := c.Expires - c.Issued
	left := int64(c.Expires) - time.Now().Unix()
	if !bytes.Equal(c.Device, d.Keyring.DevicePublic()) || left > int64(life/3) {
		return nil
	}
	info, ok := d.agents.find(identity.ID(c.Agent))
	if !ok || info.Revoked {
		return nil
	}
	n, err := d.RenewAgent(info.Name, 0)
	if err != nil {
		d.log.Warn("renewing an agent key", "agent", info.Name, "err", err)
		return nil
	}
	return n.Cert
}

// ActionApp is one app's actions, for `sol do --help` and agents.
type ActionApp struct {
	ID          string           `json:"id"`
	Name        string           `json:"name,omitempty"`
	Description string           `json:"description,omitempty"`
	Actions     []sol.ActionSpec `json:"actions"`
}

// actionApps lists the installed apps' actions (of app, "" for all), keeping those keep allows.
func (d *Daemon) actionApps(app string, keep func(string) bool) []ActionApp {
	out := []ActionApp{}
	for _, a := range d.Host.List() {
		m := a.Manifest
		if app != "" && m.ID != app {
			continue
		}
		aa := ActionApp{ID: m.ID, Name: m.Name, Description: m.Description, Actions: []sol.ActionSpec{}}
		for _, s := range m.Actions {
			if keep == nil || keep(s.Name) {
				aa.Actions = append(aa.Actions, s)
			}
		}
		if len(aa.Actions) > 0 || keep == nil {
			out = append(out, aa)
		}
	}
	if b := solActions(); app == "" || app == SolAppID {
		aa := ActionApp{ID: b.ID, Name: b.Name, Description: b.Description, Actions: []sol.ActionSpec{}}
		for _, s := range b.Actions {
			if keep == nil || keep(s.Name) {
				aa.Actions = append(aa.Actions, s)
			}
		}
		if len(aa.Actions) > 0 || keep == nil {
			out = append(out, aa)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// AgentWhoAmI is the reply of GET /agent/v1/whoami.
type AgentWhoAmI struct {
	Name    string    `json:"name"`
	ID      string    `json:"id"`
	Person  string    `json:"person"`
	Scopes  []string  `json:"scopes"`
	Expires time.Time `json:"expires"`
}

// agentHandler serves /agent/v1/ to agent keys.
func (d *Daemon) agentHandler() http.Handler {
	mx := http.NewServeMux()
	j := func(pattern string, f func(c identity.AgentCert, body []byte, r *http.Request) (any, error)) {
		mx.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			c, body, err := d.agentAuth(r)
			if err != nil {
				d.log.Info("refused an agent request", "reason", err.Error())
				sol.WriteError(w, err)
				return
			}
			if n := d.renewIfDue(c); n != nil {
				w.Header().Set("Sol-Agent-Renewed", base64.StdEncoding.EncodeToString(n))
			}
			out, err := f(c, body, r)
			if err != nil {
				var se *sol.Error
				if !errors.As(err, &se) {
					err = sol.Errorf("internal", err.Error())
				}
				sol.WriteError(w, err)
				return
			}
			if raw, ok := out.(json.RawMessage); ok {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(raw)
				return
			}
			sol.WriteJSON(w, http.StatusOK, out)
		})
	}
	j("GET /agent/v1/whoami", func(c identity.AgentCert, _ []byte, _ *http.Request) (any, error) {
		return AgentWhoAmI{Name: c.Name, ID: identity.ID(c.Agent), Person: identity.ID(c.Person), Scopes: c.Scopes,
			Expires: time.Unix(int64(c.Expires), 0).UTC()}, nil
	})
	j("GET /agent/v1/actions", func(c identity.AgentCert, _ []byte, r *http.Request) (any, error) {
		return d.actionApps(r.URL.Query().Get("app"), c.AllowsAction), nil
	})
	j("POST /agent/v1/actions/call", func(c identity.AgentCert, body []byte, r *http.Request) (any, error) {
		var call sol.ActionCall
		if err := json.Unmarshal(body, &call); err != nil {
			return nil, sol.Errorf("bad_request", "bad JSON")
		}
		target := call.Person
		if target == "" {
			target = d.PersonID()
		}
		if !c.Allows(call.Action, target) {
			d.log.Info("refused an agent action outside its scopes", "agent", c.Name, "action", call.Action)
			return nil, sol.Errorf("forbidden", "the agent's scopes do not cover "+call.Action+" for that person")
		}
		// Sol's own actions go through the app API too (appapi.Builtin), so an agent's call
		// gets the same approval policy, Inbox approval and activity log as an app's.
		return d.API.CallAs(r.Context(), identity.ID(c.Agent), call)
	})
	j("GET /agent/v1/inbox", func(c identity.AgentCert, _ []byte, _ *http.Request) (any, error) {
		// Its approved calls' outputs are handed over here once (API.md section 8).
		return d.API.AgentInbox(identity.ID(c.Agent)), nil
	})
	mx.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		sol.WriteError(w, sol.Errorf("not_found", "no such path"))
	})
	return mx
}
