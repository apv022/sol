package daemon

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/apv022/sol/internal/channels"
	"github.com/apv022/sol/internal/feeds"
	"github.com/apv022/sol/internal/fsutil"
	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/transport"
	"github.com/apv022/sol/sdk/go/sol"
)

// App channels (design/CHANNELS.md), the daemon's side: hosting channels on public onions
// (the developer's node), following channels (every node of mine, through one internal feed),
// and installing, updating and rolling back apps from them.
//
// On disk:
//
//	<dir>/channel-keys/<name>.key  channel keys `sol channel new` makes (0600; the daemon never
//	                               reads them: records are signed by the CLI or CI)
//	<dir>/channels/hosted.json     the channels this node hosts and their latest records (0600)
//	<dir>/channels/blobs/<sha256>  artifacts, hosted or downloaded (content-addressed, 0600)
//	<dir>/channels/apps.json       per installed app: its pinned channel, seq, version, policy
//	<dir>/channels/prev/<id>/      the previous version of an app, for one-step rollback
//	<dir>/channels/staging/        unpacking (emptied at start)
//
// Invariants:
//   - The followed channels and the releases learned from them live in my internal feed
//     "channels" (feeds.InternalApp), so every device of mine follows the same channels and
//     learns every release, whichever device received it. A release entry is written only after
//     its record verified against the followed channel's key and is newer than any held.
//   - Exactly one device of mine is meant to hold each channel's Tor stream (electHolder): the
//     lowest-ID anchor among my devices that are up, else the lowest-ID device that is up, as
//     this device sees them. Two devices that cannot see each other may both hold it; the feed
//     makes that harmless (a record already held is not written again).
//   - Nothing polls: records arrive pushed over the stream or replicated over sessions; a broken
//     stream is retried with exponential backoff.
//   - Trust on first use per app: the first install from a channel pins the channel key in the
//     app's binding; an update must come from that channel with a higher seq, and its artifact
//     must match the signed hash. Switching channels means removing the app and installing it
//     again.

// ChannelsFeed is the internal feed of followed channels and their releases.
const ChannelsFeed = "channels"

// channelIDPrefix namespaces hosted channels among public onion publications.
const channelIDPrefix = publicIDPrefix + "channel-"

// Update policies of an app installed from a channel.
const (
	PolicyAuto = "auto"
	PolicyAsk  = "ask"
	PolicyOff  = "off"
)

// Stream backoff bounds. Variables for tests.
var (
	ChannelRetryMin = time.Second
	ChannelRetryMax = 5 * time.Minute
)

// ThisPlatform is the platform name of sol-app artifacts this build installs.
var ThisPlatform = runtime.GOOS + "-" + runtime.GOARCH

// chanEntry is the body of an entry in my channels feed.
type chanEntry struct {
	Op      string `json:"op"` // follow, unfollow or release
	Channel string `json:"channel"`
	Onion   string `json:"onion,omitempty"`
	Record  string `json:"record,omitempty"` // a release's wire form, base64
}

// appBinding is what Sol keeps about an app it installed from a channel (or, for an Android
// app connected to Sol, the channel that offers its updates).
type appBinding struct {
	App     string `json:"app"`
	Channel string `json:"channel"` // pinned on first install
	Kind    string `json:"kind"`    // identity.KindSolApp or KindAPK
	Seq     uint64 `json:"seq"`     // the highest release seq installed
	Version string `json:"version"` // the version installed now
	Policy  string `json:"policy"`
	// PrevVersion is the version kept in prev/<id> for rollback ("" when none).
	PrevVersion string `json:"prev_version,omitempty"`
	// AskedSeq is the release an Inbox item was filed for, so each release asks once.
	AskedSeq uint64 `json:"asked_seq,omitempty"`
}

// hostedChannel is one channel this node serves.
type hostedChannel struct {
	Name    string   `json:"name"`
	Channel string   `json:"channel"`
	Records []string `json:"records"` // wire forms, base64, the latest per app

	srv *channels.Server
	hs  *http.Server
	ln  net.Listener
}

// FollowedChannel is one row of sol.apps.channels.
type FollowedChannel struct {
	Channel string   `json:"channel"`
	Onion   string   `json:"onion"`
	Link    string   `json:"link"`
	Apps    []string `json:"apps"`
	// Stream is how this device gets the channel's releases: "live" (the stream is open),
	// "connecting", "retrying: <why>", "via my other devices" (another device holds it) or
	// "tor is off".
	Stream string `json:"stream"`
}

// AvailableApp is one row of sol.apps.available: the latest release of an app in a followed
// channel.
type AvailableApp struct {
	ID        string    `json:"id"`
	Version   string    `json:"version"`
	Seq       uint64    `json:"seq"`
	Channel   string    `json:"channel"`
	Time      time.Time `json:"time"`
	Notes     string    `json:"notes,omitempty"`
	Platforms []string  `json:"platforms"`
	// Installable is true when it has a build this device can install.
	Installable bool `json:"installable"`
	// Installed is the version installed here ("" when it is not).
	Installed string `json:"installed,omitempty"`
}

// UpdateInfo is one row of sol.apps.updates.
type UpdateInfo struct {
	ID        string `json:"id"`
	Installed string `json:"installed"`
	Version   string `json:"version"`
	Seq       uint64 `json:"seq"`
	Channel   string `json:"channel"`
	Kind      string `json:"kind"`
	Policy    string `json:"policy"`
	Notes     string `json:"notes,omitempty"`
}

// HostedInfo is one channel this node hosts (sol channel list).
type HostedInfo struct {
	Name    string `json:"name"`
	Channel string `json:"channel"`
	// Address is the channel's .onion once Tor published it.
	Address string `json:"address,omitempty"`
	// Link is the follow link (once Address is known).
	Link string         `json:"link,omitempty"`
	Apps []HostedRecord `json:"apps"`
}

// HostedRecord summarises one record a hosted channel serves.
type HostedRecord struct {
	App     string `json:"app"`
	Version string `json:"version"`
	Seq     uint64 `json:"seq"`
	// Blobs reports whether every artifact is stored here (else followers use the mirrors).
	Blobs bool `json:"blobs"`
}

// chanStamp orders follow and unfollow entries across my devices (feeds' lww order).
type chanStamp struct {
	ms  int64
	dev string
	seq uint64
}

func (a chanStamp) after(b chanStamp) bool {
	if a.ms != b.ms {
		return a.ms > b.ms
	}
	if a.dev != b.dev {
		return a.dev > b.dev
	}
	return a.seq > b.seq
}

// chanFollow is a followed (or once followed) channel in the fold.
type chanFollow struct {
	onion    string
	followed bool
	at       chanStamp
}

// chanState is the daemon's channels state.
type chanState struct {
	d      *Daemon
	dir    string
	blobs  *channels.Blobs
	client *http.Client // mirrors, over HTTPS
	kick   chan struct{}

	mu       sync.Mutex
	pos      uint64 // how far the fold has read my channels feed
	owner    string // whose feed the fold read
	follows  map[string]*chanFollow
	releases map[string]map[string]identity.Release // channel -> app -> latest
	streams  map[string]context.CancelFunc          // the streams this device holds
	state    map[string]string                      // channel -> stream state
	hosted   []*hostedChannel
	apps     map[string]*appBinding
	tried    map[string]uint64 // app -> the release seq an automatic update was tried for

	installMu sync.Mutex // one install, update or rollback at a time
}

func newChanState(d *Daemon, client *http.Client) (*chanState, error) {
	dir := filepath.Join(d.dir, "channels")
	for _, sub := range []string{"", "prev"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return nil, err
		}
	}
	_ = os.RemoveAll(filepath.Join(dir, "staging"))
	blobs, err := channels.OpenBlobs(filepath.Join(dir, "blobs"))
	if err != nil {
		return nil, err
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Minute}
	}
	// Mirrors are HTTPS; a redirect elsewhere is refused (the bytes would be checked anyway, but
	// a plain-HTTP fetch shows the world what this device installs).
	c := *client
	c.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if r.URL.Scheme != "https" || len(via) > 5 {
			return errors.New("mirrors may only redirect to https")
		}
		return nil
	}
	cs := &chanState{d: d, dir: dir, blobs: blobs, client: &c, kick: make(chan struct{}, 1),
		follows: map[string]*chanFollow{}, releases: map[string]map[string]identity.Release{},
		streams: map[string]context.CancelFunc{}, state: map[string]string{}, apps: map[string]*appBinding{},
		tried: map[string]uint64{}}
	if err := readJSON(filepath.Join(dir, "hosted.json"), &cs.hosted); err != nil {
		return nil, err
	}
	var apps []*appBinding
	if err := readJSON(filepath.Join(dir, "apps.json"), &apps); err != nil {
		return nil, err
	}
	for _, a := range apps {
		cs.apps[a.App] = a
	}
	return cs, nil
}

func (cs *chanState) saveAppsLocked() error {
	list := make([]*appBinding, 0, len(cs.apps))
	for _, a := range cs.apps {
		list = append(list, a)
	}
	sort.Slice(list, func(i, j int) bool { return list[i].App < list[j].App })
	b, _ := json.MarshalIndent(list, "", "  ")
	return fsutil.WriteFileAtomic(filepath.Join(cs.dir, "apps.json"), b, 0o600)
}

func (cs *chanState) saveHostedLocked() error {
	b, _ := json.MarshalIndent(cs.hosted, "", "  ")
	return fsutil.WriteFileAtomic(filepath.Join(cs.dir, "hosted.json"), b, 0o600)
}

// Kick wakes the channel loop: a session, a device or a transport changed, so the stream
// holder may have to change.
func (cs *chanState) Kick() {
	select {
	case cs.kick <- struct{}{}:
	default:
	}
}

// start serves the hosted channels and runs the loop that follows the feed and holds streams.
func (cs *chanState) start() {
	cs.mu.Lock()
	for _, h := range cs.hosted {
		if err := cs.serveLocked(h); err != nil {
			cs.d.log.Warn("serving a hosted channel", "channel", h.Name, "err", err)
		}
	}
	cs.mu.Unlock()
	sub := cs.d.Store.Bus().Subscribe(256)
	cs.d.wg.Add(1)
	go func() {
		defer cs.d.wg.Done()
		defer sub.Close()
		defer cs.stopAll()
		cs.step()
		dropped := uint64(0)
		for {
			select {
			case <-cs.d.ctx.Done():
				return
			case ev := <-sub.C():
				// An event is a wake-up: the fold re-reads the feed. A dropped one may have
				// been ours, so a drop wakes it too.
				mine := ev.Entry != nil && strings.HasSuffix(ev.Entry.Feed, "/"+feeds.InternalApp+"/"+ChannelsFeed)
				n := sub.Dropped()
				if !mine && n == dropped {
					continue
				}
				dropped = n
			case <-cs.kick:
			}
			cs.step()
		}
	}()
}

// step folds new feed entries, then re-elects the stream holder and applies updates.
func (cs *chanState) step() {
	cs.fold()
	cs.manageStreams()
	cs.react()
}

// stopAll ends the hosted channels' servers and this device's streams.
func (cs *chanState) stopAll() {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	for _, cancel := range cs.streams {
		cancel()
	}
	cs.streams = map[string]context.CancelFunc{}
	for _, h := range cs.hosted {
		if h.srv != nil {
			h.srv.Close()
		}
		if h.hs != nil {
			_ = h.hs.Close()
		}
	}
}

// fold reads the entries of my channels feed it has not seen yet.
func (cs *chanState) fold() {
	me := cs.d.PersonID()
	if me == "" {
		return
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if cs.owner != me {
		cs.owner, cs.pos = me, 0
		cs.follows, cs.releases = map[string]*chanFollow{}, map[string]map[string]identity.Release{}
	}
	addr := feeds.Address(me, feeds.InternalApp, ChannelsFeed)
	for {
		es, err := cs.d.Store.Read(addr, cs.pos+1, 1000)
		if err != nil || len(es) == 0 {
			return
		}
		for _, e := range es {
			cs.pos = e.Pos
			var ce chanEntry
			if json.Unmarshal(e.Body, &ce) != nil {
				continue
			}
			st := chanStamp{e.Time.UnixMilli(), e.Device, e.Seq}
			switch ce.Op {
			case "follow", "unfollow":
				f := cs.follows[ce.Channel]
				if f == nil {
					f = &chanFollow{}
					cs.follows[ce.Channel] = f
				} else if !st.after(f.at) {
					continue
				}
				f.at, f.followed = st, ce.Op == "follow"
				if ce.Onion != "" {
					f.onion = ce.Onion
				}
			case "release":
				// Checked again here: an entry replicated from another device of mine is
				// signed by that device, but its record must still verify on its own.
				raw, err := base64.StdEncoding.DecodeString(ce.Record)
				if err != nil {
					continue
				}
				r, err := identity.ParseRelease(raw)
				if err != nil || r.Verify() != nil || identity.ID(r.Channel) != ce.Channel {
					continue
				}
				cs.keepReleaseLocked(r)
			}
		}
	}
}

// keepReleaseLocked records r when it is newer than the release held for its channel and app,
// and reports whether it was.
func (cs *chanState) keepReleaseLocked(r identity.Release) bool {
	ch := identity.ID(r.Channel)
	m := cs.releases[ch]
	if m == nil {
		m = map[string]identity.Release{}
		cs.releases[ch] = m
	}
	if cur, ok := m[r.App]; ok {
		if n, err := identity.CompareRelease(cur, r); err != nil || n != 1 {
			return false
		}
	}
	m[r.App] = r
	return true
}

// followedLocked returns the followed channels' IDs, sorted.
func (cs *chanState) followedLocked() []string {
	var out []string
	for ch, f := range cs.follows {
		if f.followed {
			out = append(out, ch)
		}
	}
	slices.Sort(out)
	return out
}

// Follow follows the channel a link names, on all my devices.
func (cs *chanState) Follow(link string, dry bool) (FollowedChannel, error) {
	pub, onion, err := channels.ParseLink(link)
	if err != nil {
		return FollowedChannel{}, sol.Errorf("bad_request", strings.TrimPrefix(err.Error(), "channels: "))
	}
	id := identity.ID(pub)
	out := FollowedChannel{Channel: id, Onion: onion, Link: channels.FormatLink(pub, onion), Apps: []string{}}
	if dry {
		return out, nil
	}
	cs.fold()
	cs.mu.Lock()
	f := cs.follows[id]
	same := f != nil && f.followed && f.onion == onion
	cs.mu.Unlock()
	if !same {
		if err := cs.append(chanEntry{Op: "follow", Channel: id, Onion: onion}); err != nil {
			return FollowedChannel{}, err
		}
	}
	cs.d.log.Info("following an app channel", "channel", id[:8])
	cs.Kick()
	return out, nil
}

// Unfollow stops following a channel on all my devices. Apps installed from it stay, and keep
// their pinned key: following it again resumes their updates.
func (cs *chanState) Unfollow(channel string, dry bool) (map[string]any, error) {
	cs.fold()
	cs.mu.Lock()
	f := cs.follows[channel]
	cs.mu.Unlock()
	if f == nil || !f.followed {
		return nil, sol.Errorf("not_found", "not following channel "+channel)
	}
	if dry {
		return map[string]any{"dry_run": true, "would_unfollow": channel}, nil
	}
	if err := cs.append(chanEntry{Op: "unfollow", Channel: channel}); err != nil {
		return nil, err
	}
	cs.Kick()
	return map[string]any{"unfollowed": channel}, nil
}

func (cs *chanState) append(e chanEntry) error {
	b, _ := json.Marshal(e)
	if _, err := cs.d.Store.Append(feeds.InternalApp, ChannelsFeed, b); err != nil {
		if errors.Is(err, feeds.ErrNoPerson) {
			return sol.Errorf("conflict", "this device does not belong to a person yet")
		}
		return err
	}
	cs.fold()
	return nil
}

// errNotNewer refuses a record that is not newer than the one held.
func errNotNewer(r identity.Release) error {
	return sol.Errorf("conflict", fmt.Sprintf("a release of %s with seq %d or higher is already known", r.App, r.Seq))
}

// Offer takes a record that arrived for a followed channel (from its stream, or imported from
// a file): it must verify against the channel's key (expect, when the caller knows which channel
// it asked) and be newer than the release held; then it goes into my channels feed, which
// tells my other devices.
func (cs *chanState) Offer(raw []byte, expect ed25519.PublicKey) (identity.Release, error) {
	r, err := identity.ParseRelease(raw)
	if err != nil {
		return r, sol.Errorf("bad_request", "not a release record: "+err.Error())
	}
	if expect != nil {
		err = r.VerifyFrom(expect)
	} else {
		err = r.Verify()
	}
	if err != nil {
		return r, sol.Errorf("forbidden", "the release record is not signed by the channel's key")
	}
	ch := identity.ID(r.Channel)
	cs.fold()
	cs.mu.Lock()
	f := cs.follows[ch]
	if f == nil || !f.followed {
		cs.mu.Unlock()
		return r, sol.Errorf("forbidden", "not following channel "+ch)
	}
	cur, ok := cs.releases[ch][r.App]
	cs.mu.Unlock()
	if ok {
		n, err := identity.CompareRelease(cur, r)
		if err == nil && n == 0 {
			return r, nil // already held: nothing to tell
		}
		if err != nil || n != 1 {
			return r, errNotNewer(r)
		}
	}
	if err := cs.append(chanEntry{Op: "release", Channel: ch, Record: base64.StdEncoding.EncodeToString(raw)}); err != nil {
		return r, err
	}
	cs.d.log.Info("a new app release", "app", r.App, "version", r.Version, "channel", ch[:8])
	return r, nil
}

// electHolder picks the device of mine that holds the channel streams, among those up as this
// device sees them (itself included): the lowest-ID anchor, else the lowest ID.
func electHolder(devs []DeviceInfo) (DeviceInfo, bool) {
	var best *DeviceInfo
	for i := range devs {
		dv := &devs[i]
		if !dv.Online {
			continue
		}
		if best == nil || dv.Anchor && !best.Anchor || dv.Anchor == best.Anchor && dv.ID < best.ID {
			best = dv
		}
	}
	if best == nil {
		return DeviceInfo{}, false
	}
	return *best, true
}

// manageStreams opens a stream to every followed channel when this device is the holder, and
// closes them when it is not (or the channel was unfollowed).
func (cs *chanState) manageStreams() {
	if cs.d.ctx.Err() != nil {
		return
	}
	holder, ok := electHolder(cs.d.Devices())
	tor := cs.d.tor()
	cs.mu.Lock()
	defer cs.mu.Unlock()
	want := map[string]string{}
	for _, ch := range cs.followedLocked() {
		switch {
		case !ok || !holder.This:
			cs.state[ch] = "via my other devices"
		case tor == nil:
			cs.state[ch] = "tor is off"
		default:
			want[ch] = cs.follows[ch].onion
		}
	}
	for ch, cancel := range cs.streams {
		if _, keep := want[ch]; !keep {
			cancel()
			delete(cs.streams, ch)
		}
	}
	for ch, onion := range want {
		if _, running := cs.streams[ch]; running {
			continue
		}
		pub, err := identity.ParseID(ch)
		if err != nil {
			continue
		}
		ctx, cancel := context.WithCancel(cs.d.ctx)
		cs.streams[ch] = cancel
		cs.state[ch] = "connecting"
		cs.d.wg.Add(1)
		go func() {
			defer cs.d.wg.Done()
			cs.hold(ctx, ch, pub, onion)
		}()
	}
}

// torHTTP is an HTTP client that reaches onions through this device's Tor.
func torHTTP(tor *transport.Tor) *http.Client {
	return &http.Client{Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) { return tor.Dial(ctx, addr) },
		// A fresh connection per stream; nothing to share across onions.
		DisableKeepAlives: true, ResponseHeaderTimeout: 2 * time.Minute,
	}}
}

// hold keeps one channel's stream open until ctx ends, retrying with exponential backoff.
func (cs *chanState) hold(ctx context.Context, ch string, pub ed25519.PublicKey, onion string) {
	backoff := ChannelRetryMin
	setState := func(s string) {
		cs.mu.Lock()
		if _, ok := cs.streams[ch]; ok {
			cs.state[ch] = s
		}
		cs.mu.Unlock()
	}
	for ctx.Err() == nil {
		tor := cs.d.tor()
		if tor == nil {
			setState("tor is off")
			return // turning Tor on kicks the loop, which starts this again
		}
		err := channels.Stream(ctx, torHTTP(tor), "http://"+onion, func() {
			backoff = ChannelRetryMin
			setState("live")
		}, func(raw []byte) {
			if _, err := cs.Offer(raw, pub); err != nil {
				var se *sol.Error
				if !errors.As(err, &se) || se.Code != "conflict" {
					cs.d.log.Info("refused a release from a channel", "channel", ch[:8], "err", err)
				}
			}
		})
		if ctx.Err() != nil {
			return
		}
		setState("retrying: " + err.Error())
		// Jitter, so my devices and other followers don't all knock at once after an outage.
		wait := backoff/2 + rand.N(backoff/2+1)
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		backoff = min(backoff*2, ChannelRetryMax)
	}
}

// Channels lists the followed channels.
func (cs *chanState) Channels() []FollowedChannel {
	cs.fold()
	cs.mu.Lock()
	defer cs.mu.Unlock()
	out := []FollowedChannel{}
	for _, ch := range cs.followedLocked() {
		pub, _ := identity.ParseID(ch)
		f := cs.follows[ch]
		fc := FollowedChannel{Channel: ch, Onion: f.onion, Link: channels.FormatLink(pub, f.onion), Apps: []string{}, Stream: cs.state[ch]}
		for app := range cs.releases[ch] {
			fc.Apps = append(fc.Apps, app)
		}
		slices.Sort(fc.Apps)
		out = append(out, fc)
	}
	return out
}

// pickArtifact chooses what this device installs from r: the sol-app for ThisPlatform.
func pickArtifact(r identity.Release) (identity.Artifact, bool) {
	for _, a := range r.Artifacts {
		if a.Kind == identity.KindSolApp && a.Platform == ThisPlatform {
			return a, true
		}
	}
	return identity.Artifact{}, false
}

func platforms(r identity.Release) []string {
	var out []string
	for _, a := range r.Artifacts {
		out = append(out, a.Platform)
	}
	return out
}

func hasAPK(r identity.Release) bool {
	return slices.ContainsFunc(r.Artifacts, func(a identity.Artifact) bool { return a.Kind == identity.KindAPK })
}

// Available lists the latest release of every app in the followed channels.
func (cs *chanState) Available() []AvailableApp {
	cs.fold()
	cs.mu.Lock()
	var rs []identity.Release
	for _, ch := range cs.followedLocked() {
		for _, r := range cs.releases[ch] {
			rs = append(rs, r)
		}
	}
	cs.mu.Unlock()
	out := []AvailableApp{}
	for _, r := range rs {
		_, ok := pickArtifact(r)
		a := AvailableApp{ID: r.App, Version: r.Version, Seq: r.Seq, Channel: identity.ID(r.Channel),
			Time: time.Unix(int64(r.Time), 0).UTC(), Notes: r.Notes, Platforms: platforms(r), Installable: ok}
		if m, ok := cs.d.Host.Manifest(r.App); ok {
			a.Installed = m.Version
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].ID != out[j].ID {
			return out[i].ID < out[j].ID
		}
		return out[i].Channel < out[j].Channel
	})
	return out
}

// latest returns the latest release of app in a followed channel ch.
func (cs *chanState) latest(ch, app string) (identity.Release, bool) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	if f := cs.follows[ch]; f == nil || !f.followed {
		return identity.Release{}, false
	}
	r, ok := cs.releases[ch][app]
	return r, ok
}

// binding returns a copy of app's binding, dropping one whose app is gone (removed with
// `sol app remove`: installing it again pins afresh).
func (cs *chanState) binding(app string) (appBinding, bool) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	b := cs.apps[app]
	if b == nil {
		return appBinding{}, false
	}
	if _, ok := cs.d.Host.Manifest(app); !ok {
		delete(cs.apps, app)
		_ = cs.saveAppsLocked()
		_ = os.RemoveAll(filepath.Join(cs.dir, "prev", app))
		return appBinding{}, false
	}
	return *b, true
}

func (cs *chanState) putBinding(b appBinding) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.apps[b.App] = &b
	return cs.saveAppsLocked()
}

// Updates lists the apps whose pinned channel has a newer release than the one installed.
func (cs *chanState) Updates() []UpdateInfo {
	cs.fold()
	out := []UpdateInfo{}
	cs.mu.Lock()
	ids := make([]string, 0, len(cs.apps))
	for id := range cs.apps {
		ids = append(ids, id)
	}
	cs.mu.Unlock()
	slices.Sort(ids)
	for _, id := range ids {
		b, ok := cs.binding(id)
		if !ok {
			continue
		}
		r, ok := cs.latest(b.Channel, id)
		if !ok || r.Seq <= b.Seq {
			continue
		}
		out = append(out, UpdateInfo{ID: id, Installed: b.Version, Version: r.Version, Seq: r.Seq, Channel: b.Channel,
			Kind: b.Kind, Policy: b.Policy, Notes: r.Notes})
	}
	return out
}

// Install installs app from a followed channel (channel "" means the only one offering it).
func (cs *chanState) Install(ctx context.Context, app, channel string, dry bool) (any, error) {
	cs.fold()
	if _, ok := cs.d.Host.Manifest(app); ok {
		if _, bound := cs.binding(app); bound {
			return nil, sol.Errorf("conflict", app+" is installed from a channel; update it with sol.apps.update")
		}
		return nil, sol.Errorf("conflict", app+" is already installed (not from a channel); remove it first to install it from one")
	}
	cs.mu.Lock()
	var cands []identity.Release
	for _, ch := range cs.followedLocked() {
		if r, ok := cs.releases[ch][app]; ok && (channel == "" || channel == ch) {
			cands = append(cands, r)
		}
	}
	cs.mu.Unlock()
	switch {
	case len(cands) == 0:
		return nil, sol.Errorf("not_found", "no followed channel offers "+app)
	case len(cands) > 1:
		return nil, sol.Errorf("conflict", "several followed channels offer "+app+"; choose one with --channel")
	}
	r := cands[0]
	art, ok := pickArtifact(r)
	if !ok {
		return nil, noBuild(r)
	}
	if dry {
		return map[string]any{"dry_run": true, "would_install": app, "version": r.Version, "channel": identity.ID(r.Channel), "size": art.Size}, nil
	}
	return cs.apply(ctx, r, art, nil)
}

// noBuild explains why r has nothing this device can install.
func noBuild(r identity.Release) error {
	if hasAPK(r) {
		return sol.Errorf("not_implemented", r.App+" is an Android app: installing an APK is done by Sol's Android app, which does not do it yet")
	}
	return sol.Errorf("not_found", r.App+" "+r.Version+" has no build for "+ThisPlatform)
}

// Update installs the newest release of app from its pinned channel.
func (cs *chanState) Update(ctx context.Context, app string, dry bool) (any, error) {
	cs.fold()
	b, ok := cs.binding(app)
	if !ok {
		return nil, sol.Errorf("not_found", app+" was not installed from a channel")
	}
	r, ok := cs.latest(b.Channel, app)
	if !ok {
		return nil, sol.Errorf("not_found", "the channel "+app+" was installed from is not followed any more")
	}
	if r.Seq <= b.Seq {
		return map[string]any{"id": app, "version": b.Version, "up_to_date": true}, nil
	}
	if b.Kind == identity.KindAPK {
		return nil, sol.Errorf("not_implemented", app+" is an Android app: Sol's Android app will install "+r.Version+" with PackageInstaller (not built yet)")
	}
	art, ok := pickArtifact(r)
	if !ok {
		return nil, noBuild(r)
	}
	if dry {
		return map[string]any{"dry_run": true, "would_update": app, "from": b.Version, "to": r.Version}, nil
	}
	return cs.apply(ctx, r, art, &b)
}

// fetch puts art into the blob store: from the channel's onion, else from its mirrors. The
// bytes are checked against the signed hash either way.
func (cs *chanState) fetch(ctx context.Context, r identity.Release, art identity.Artifact) error {
	h, size := art.Hash(), int64(art.Size)
	if cs.blobs.Has(h) && cs.blobs.Verify(h, size) {
		return nil
	}
	var errs []error
	cs.mu.Lock()
	onion := ""
	if f := cs.follows[identity.ID(r.Channel)]; f != nil {
		onion = f.onion
	}
	cs.mu.Unlock()
	if tor := cs.d.tor(); tor != nil && onion != "" {
		octx, cancel := context.WithTimeout(ctx, 30*time.Minute)
		err := cs.blobs.Fetch(octx, torHTTP(tor), "http://"+onion+"/blob/"+h, h, size)
		cancel()
		if err == nil {
			return nil
		}
		errs = append(errs, fmt.Errorf("onion: %w", err))
	} else {
		errs = append(errs, errors.New("onion: tor is off"))
	}
	for _, m := range art.Mirrors {
		err := cs.blobs.Fetch(ctx, cs.client, m, h, size)
		if err == nil {
			cs.d.log.Info("downloaded an app from a mirror", "app", r.App)
			return nil
		}
		errs = append(errs, fmt.Errorf("mirror: %w", err))
	}
	err := errors.Join(errs...)
	if errors.Is(err, channels.ErrTampered) {
		return sol.Errorf("conflict", "refused "+r.App+" "+r.Version+": "+channels.ErrTampered.Error()[len("channels: "):])
	}
	return sol.Errorf("unavailable", "could not download "+r.App+" "+r.Version+": "+err.Error())
}

// apply downloads, checks, unpacks and installs r, keeping the version it replaces for
// rollback. prev is the app's binding when this is an update.
func (cs *chanState) apply(ctx context.Context, r identity.Release, art identity.Artifact, prev *appBinding) (any, error) {
	cs.installMu.Lock()
	defer cs.installMu.Unlock()
	if prev != nil { // re-check under the lock: another update may have won
		b, ok := cs.binding(r.App)
		if !ok || b.Channel != identity.ID(r.Channel) {
			return nil, sol.Errorf("conflict", r.App+" changed meanwhile; try again")
		}
		if b.Seq >= r.Seq {
			return map[string]any{"id": r.App, "version": b.Version, "up_to_date": true}, nil
		}
		*prev = b
	}
	if err := cs.fetch(ctx, r, art); err != nil {
		return nil, err
	}
	stage := filepath.Join(cs.dir, "staging")
	if err := os.MkdirAll(stage, 0o700); err != nil {
		return nil, err
	}
	f, err := cs.blobs.Open(art.Hash())
	if err != nil {
		return nil, err
	}
	work := filepath.Join(stage, fmt.Sprintf("%s-%d", r.App, time.Now().UnixNano()))
	root, err := channels.Unpack(f, work)
	f.Close()
	defer os.RemoveAll(work)
	if err != nil {
		cs.blobs.Remove(art.Hash()) // signed but unusable: nothing to keep it for
		return nil, sol.Errorf("bad_request", "refused "+r.App+" "+r.Version+": "+strings.TrimPrefix(err.Error(), "channels: "))
	}
	m, err := sol.LoadManifest(filepath.Join(root, "app.json"))
	switch {
	case err != nil:
		return nil, sol.Errorf("bad_request", "refused "+r.App+": "+err.Error())
	case m.ID != r.App:
		return nil, sol.Errorf("bad_request", "refused "+r.App+": its app.json says id "+m.ID)
	case m.Version != r.Version:
		return nil, sol.Errorf("bad_request", "refused "+r.App+": its app.json says version "+m.Version+", the release "+r.Version)
	case strings.HasPrefix(m.Exec, "builtin:") || strings.HasPrefix(m.Exec, "external"):
		return nil, sol.Errorf("bad_request", "refused "+r.App+": an app from a channel brings its own executable")
	}
	if err := cs.d.Host.Validate(m, root); err != nil {
		return nil, sol.Errorf("bad_request", "refused "+r.App+": "+err.Error())
	}
	// Keep what is installed now for rollback: snapshot first, swap it in once the install
	// worked.
	snap := ""
	if prev != nil {
		if cur, ok := cs.d.Host.Dir(r.App); ok {
			snap = filepath.Join(cs.dir, "prev", r.App+".new")
			_ = os.RemoveAll(snap)
			if err := copyDir(cur, snap); err != nil {
				_ = os.RemoveAll(snap)
				return nil, err
			}
		}
	}
	if _, err := cs.d.Host.Install(root); err != nil {
		if snap != "" {
			_ = os.RemoveAll(snap)
		}
		return nil, err
	}
	b := appBinding{App: r.App, Channel: identity.ID(r.Channel), Kind: identity.KindSolApp, Policy: PolicyAuto}
	if prev != nil {
		b = *prev
	}
	if snap != "" {
		dst := filepath.Join(cs.dir, "prev", r.App)
		_ = os.RemoveAll(dst)
		if err := os.Rename(snap, dst); err == nil {
			b.PrevVersion = b.Version
		}
	}
	b.Seq, b.Version = r.Seq, r.Version
	if err := cs.putBinding(b); err != nil {
		return nil, err
	}
	verb := "installed"
	if prev != nil {
		verb = "updated"
	}
	cs.d.log.Info("app "+verb+" from a channel", "app", r.App, "version", r.Version)
	return map[string]any{"id": r.App, "version": r.Version, verb: true, "channel": b.Channel, "policy": b.Policy}, nil
}

// Rollback puts back the version an update replaced (one step; rolling back again returns to
// the newer one). The pinned seq stays: the release rolled back from is not installed again
// automatically, only a newer one.
func (cs *chanState) Rollback(app string, dry bool) (any, error) {
	b, ok := cs.binding(app)
	prevDir := filepath.Join(cs.dir, "prev", app)
	if !ok {
		return nil, sol.Errorf("not_found", app+" was not installed from a channel")
	}
	if _, err := os.Stat(filepath.Join(prevDir, "app.json")); err != nil || b.PrevVersion == "" {
		return nil, sol.Errorf("not_found", "no earlier version of "+app+" is kept")
	}
	if dry {
		return map[string]any{"dry_run": true, "would_roll_back": app, "from": b.Version, "to": b.PrevVersion}, nil
	}
	cs.installMu.Lock()
	defer cs.installMu.Unlock()
	cur, ok := cs.d.Host.Dir(app)
	if !ok {
		return nil, sol.Errorf("not_found", "no app "+app)
	}
	snap := filepath.Join(cs.dir, "prev", app+".new")
	_ = os.RemoveAll(snap)
	if err := copyDir(cur, snap); err != nil {
		_ = os.RemoveAll(snap)
		return nil, err
	}
	if _, err := cs.d.Host.Install(prevDir); err != nil {
		_ = os.RemoveAll(snap)
		return nil, err
	}
	_ = os.RemoveAll(prevDir)
	if err := os.Rename(snap, prevDir); err != nil {
		return nil, err
	}
	b.Version, b.PrevVersion = b.PrevVersion, b.Version
	if err := cs.putBinding(b); err != nil {
		return nil, err
	}
	cs.d.log.Info("app rolled back", "app", app, "version", b.Version)
	return map[string]any{"id": app, "version": b.Version, "rolled_back": true}, nil
}

// SetPolicy sets how app's updates are applied: auto, ask or off.
func (cs *chanState) SetPolicy(app, policy string, dry bool) (any, error) {
	if policy != PolicyAuto && policy != PolicyAsk && policy != PolicyOff {
		return nil, sol.Errorf("bad_request", "policy must be auto, ask or off")
	}
	b, ok := cs.binding(app)
	if !ok {
		return nil, sol.Errorf("not_found", app+" was not installed from a channel")
	}
	if b.Kind == identity.KindAPK && policy == PolicyAuto {
		return nil, sol.Errorf("not_implemented", "Android apps cannot update automatically yet")
	}
	if dry {
		return map[string]any{"dry_run": true, "id": app, "would_set": policy}, nil
	}
	b.Policy = policy
	if policy != PolicyAsk {
		b.AskedSeq = 0
	}
	if err := cs.putBinding(b); err != nil {
		return nil, err
	}
	cs.Kick() // "ask" files the item for a release already waiting
	return map[string]any{"id": app, "policy": policy}, nil
}

// react applies each app's policy to releases newer than what it has: auto updates (in the
// background), ask files an Inbox approval, and an Android app connected to Sol gets an
// "update available" notice.
func (cs *chanState) react() {
	if cs.d.Host == nil || cs.d.PersonID() == "" {
		return
	}
	cs.bindExternal()
	cs.mu.Lock()
	ids := make([]string, 0, len(cs.apps))
	for id := range cs.apps {
		ids = append(ids, id)
	}
	cs.mu.Unlock()
	slices.Sort(ids)
	for _, id := range ids {
		b, ok := cs.binding(id)
		if !ok {
			continue
		}
		r, ok := cs.latest(b.Channel, id)
		if !ok || r.Seq <= b.Seq {
			continue
		}
		name := id
		if m, ok := cs.d.Host.Manifest(id); ok && m.Name != "" {
			name = m.Name
		}
		switch {
		case b.Kind == identity.KindAPK:
			if b.AskedSeq >= r.Seq {
				continue
			}
			body := strings.TrimSpace(r.Notes + "\n\nOpen Sol on the phone to install it (Sol cannot install Android apps by itself yet).")
			if _, err := cs.d.API.PostNotice(SolAppID, name+" "+r.Version+" is available", body); err != nil {
				cs.d.log.Warn("filing an update notice", "app", id, "err", err)
				continue
			}
			b.AskedSeq = r.Seq
			_ = cs.putBinding(b)
		case b.Policy == PolicyAsk:
			if b.AskedSeq >= r.Seq {
				continue
			}
			args, _ := json.Marshal(map[string]string{"id": id})
			if _, err := cs.d.API.PostApproval(SolAppID, name+" "+r.Version+" is ready", r.Notes,
				sol.ActionCall{Action: "sol.apps.update", Args: args}); err != nil {
				cs.d.log.Warn("filing an update approval", "app", id, "err", err)
				continue
			}
			b.AskedSeq = r.Seq
			_ = cs.putBinding(b)
		case b.Policy == PolicyAuto:
			cs.mu.Lock()
			tried := cs.tried[id] >= r.Seq
			if !tried {
				cs.tried[id] = r.Seq // one automatic try per release; a failure waits for the next or a manual update
			}
			cs.mu.Unlock()
			if tried {
				continue
			}
			cs.d.wg.Add(1)
			go func() {
				defer cs.d.wg.Done()
				if _, err := cs.Update(cs.d.ctx, id, false); err != nil {
					cs.d.log.Warn("automatic app update failed", "app", id, "version", r.Version, "err", err)
				}
			}()
		}
	}
}

// bindExternal pins, for an Android app connected to Sol that a followed channel offers as an
// APK, the one channel that does (trust on first use, as for an install), so its updates show.
func (cs *chanState) bindExternal() {
	for _, a := range cs.d.Host.Status() {
		bd, ok := cs.d.Host.External(a.ID)
		if !ok || bd.Package == "" {
			continue
		}
		if _, bound := cs.binding(a.ID); bound {
			continue
		}
		cs.mu.Lock()
		var offers []identity.Release
		for _, ch := range cs.followedLocked() {
			if r, ok := cs.releases[ch][a.ID]; ok && hasAPK(r) {
				offers = append(offers, r)
			}
		}
		cs.mu.Unlock()
		if len(offers) != 1 {
			continue // none, or ambiguous: the person installs from the channel they mean
		}
		r := offers[0]
		b := appBinding{App: a.ID, Channel: identity.ID(r.Channel), Kind: identity.KindAPK, Policy: PolicyAsk, Version: a.Version}
		if r.Version == a.Version {
			b.Seq = r.Seq // that release is what is installed
		}
		_ = cs.putBinding(b)
	}
}

// copyDir copies a directory of regular files (an installed app) to dst.
func copyDir(src, dst string) error {
	return filepath.WalkDir(src, func(p string, de fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, p)
		if err != nil {
			return err
		}
		out := filepath.Join(dst, rel)
		if de.IsDir() {
			return os.MkdirAll(out, 0o700)
		}
		if !de.Type().IsRegular() {
			return nil
		}
		info, err := de.Info()
		if err != nil {
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(out, b, info.Mode().Perm()&0o700|0o600)
	})
}

// --- Hosting ---

func publicationOf(h *hostedChannel) transport.Publication {
	// Open to anyone with the address, like every public onion.
	return transport.Publication{ID: channelIDPrefix + h.Name, Target: h.ln.Addr().String(), Port: onionPortDefault}
}

// serveLocked starts the local server of a hosted channel and publishes it on Tor if it runs.
func (cs *chanState) serveLocked(h *hostedChannel) error {
	var rs []identity.Release
	for _, s := range h.Records {
		raw, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			continue
		}
		if r, err := identity.ParseRelease(raw); err == nil && r.Verify() == nil && identity.ID(r.Channel) == h.Channel {
			rs = append(rs, r)
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return err
	}
	h.ln, h.srv = ln, channels.NewServer(h.Channel, cs.blobs, rs)
	h.hs = &http.Server{Handler: h.srv, ReadHeaderTimeout: time.Minute}
	go func() { _ = h.hs.Serve(ln) }()
	if tor := cs.d.tor(); tor != nil {
		if _, err := tor.Publish(cs.d.ctx, publicationOf(h)); err != nil {
			cs.d.log.Info("channel onion not published yet", "channel", h.Name, "err", err)
		}
	}
	return nil
}

// publishAll (re)publishes the hosted channels on tor (it (re)started).
func (cs *chanState) publishAll(tor *transport.Tor) {
	cs.mu.Lock()
	var pubs []transport.Publication
	for _, h := range cs.hosted {
		if h.ln != nil {
			pubs = append(pubs, publicationOf(h))
		}
	}
	cs.mu.Unlock()
	for _, p := range pubs {
		if _, err := tor.Publish(cs.d.ctx, p); err != nil {
			cs.d.log.Info("channel onion not published yet", "id", p.ID, "err", err)
		}
	}
	cs.Kick() // and follow streams can start now
}

func (cs *chanState) hostedInfoLocked(h *hostedChannel, addrs map[string]string) HostedInfo {
	hi := HostedInfo{Name: h.Name, Channel: h.Channel, Address: addrs[channelIDPrefix+h.Name], Apps: []HostedRecord{}}
	if hi.Address != "" {
		if pub, err := identity.ParseID(h.Channel); err == nil {
			hi.Link = channels.FormatLink(pub, hi.Address)
		}
	}
	for _, s := range h.Records {
		raw, _ := base64.StdEncoding.DecodeString(s)
		r, err := identity.ParseRelease(raw)
		if err != nil {
			continue
		}
		all := true
		for _, a := range r.Artifacts {
			all = all && cs.blobs.Has(a.Hash())
		}
		hi.Apps = append(hi.Apps, HostedRecord{App: r.App, Version: r.Version, Seq: r.Seq, Blobs: all})
	}
	sort.Slice(hi.Apps, func(i, j int) bool { return hi.Apps[i].App < hi.Apps[j].App })
	return hi
}

// Hosted lists the channels this node hosts.
func (cs *chanState) Hosted() []HostedInfo {
	var addrs map[string]string
	if tor := cs.d.tor(); tor != nil {
		addrs = tor.Status().Addresses
	}
	cs.mu.Lock()
	defer cs.mu.Unlock()
	out := []HostedInfo{}
	for _, h := range cs.hosted {
		out = append(out, cs.hostedInfoLocked(h, addrs))
	}
	return out
}

// PutBlob stores an artifact a publisher uploads (sol channel publish).
func (cs *chanState) PutBlob(r *http.Request) (map[string]any, error) {
	h, n, err := cs.blobs.Put(r.Body, channels.MaxArtifactSize, "", 0)
	if err != nil {
		return nil, sol.Errorf("bad_request", strings.TrimPrefix(err.Error(), "channels: "))
	}
	return map[string]any{"sha256": h, "size": n}, nil
}

// Publish adds a signed record to a hosted channel (starting to host it, under name, when it is
// new here) and pushes it to the followers' streams. The channel key never comes here: the
// record is signed where the key lives.
func (cs *chanState) Publish(name string, raw []byte) (HostedInfo, error) {
	r, err := identity.ParseRelease(raw)
	if err != nil {
		return HostedInfo{}, sol.Errorf("bad_request", "not a release record: "+err.Error())
	}
	if err := r.Verify(); err != nil {
		return HostedInfo{}, sol.Errorf("forbidden", "the release record's signature does not verify")
	}
	ch := identity.ID(r.Channel)
	if tor := cs.d.tor(); tor == nil {
		return HostedInfo{}, sol.Errorf("unavailable", "Tor is off; a channel is served on a public onion: sol do sol.transports.enable --name tor")
	}
	cs.mu.Lock()
	var h *hostedChannel
	for _, x := range cs.hosted {
		if x.Channel == ch {
			h = x
		} else if x.Name == name {
			cs.mu.Unlock()
			return HostedInfo{}, sol.Errorf("conflict", "this node hosts another channel named "+name)
		}
	}
	if h == nil {
		if !channels.NameRe.MatchString(name) {
			cs.mu.Unlock()
			return HostedInfo{}, sol.Errorf("bad_request", "a new channel needs a name: lowercase letters, digits and dashes (at most 24)")
		}
		h = &hostedChannel{Name: name, Channel: ch}
	}
	recs := map[string]identity.Release{}
	for _, s := range h.Records {
		b, _ := base64.StdEncoding.DecodeString(s)
		if x, err := identity.ParseRelease(b); err == nil {
			recs[x.App] = x
		}
	}
	if cur, ok := recs[r.App]; ok {
		if n, err := identity.CompareRelease(cur, r); err != nil || n != 1 {
			cs.mu.Unlock()
			return HostedInfo{}, errNotNewer(r)
		}
	}
	recs[r.App] = r
	h.Records = h.Records[:0]
	apps := make([]string, 0, len(recs))
	for a := range recs {
		apps = append(apps, a)
	}
	slices.Sort(apps)
	for _, a := range apps {
		h.Records = append(h.Records, base64.StdEncoding.EncodeToString(recs[a].Marshal()))
	}
	isNew := h.srv == nil
	if isNew && !slices.Contains(cs.hosted, h) {
		cs.hosted = append(cs.hosted, h)
	}
	if err := cs.saveHostedLocked(); err != nil {
		cs.mu.Unlock()
		return HostedInfo{}, err
	}
	if isNew {
		if err := cs.serveLocked(h); err != nil {
			cs.mu.Unlock()
			return HostedInfo{}, err
		}
	} else {
		h.srv.Publish(r)
	}
	cs.mu.Unlock()
	cs.d.log.Info("published an app release", "channel", h.Name, "app", r.App, "version", r.Version)
	cs.d.emit(EvOnion, cs.d.onions.List())
	for _, hi := range cs.Hosted() {
		if hi.Channel == ch {
			return hi, nil
		}
	}
	return HostedInfo{}, nil
}

// Unhost stops serving a channel and deletes its onion key: its address is gone for good, and
// followers keep what they installed.
func (cs *chanState) Unhost(name string) error {
	cs.mu.Lock()
	i := slices.IndexFunc(cs.hosted, func(h *hostedChannel) bool { return h.Name == name })
	if i < 0 {
		cs.mu.Unlock()
		return sol.Errorf("not_found", "no hosted channel "+name)
	}
	h := cs.hosted[i]
	cs.hosted = slices.Delete(cs.hosted, i, i+1)
	err := cs.saveHostedLocked()
	cs.mu.Unlock()
	if h.srv != nil {
		h.srv.Close()
		_ = h.hs.Close()
	}
	if err != nil {
		return err
	}
	if tor := cs.d.tor(); tor != nil {
		return tor.Forget(channelIDPrefix + name)
	}
	key := filepath.Join(cs.d.dir, "tor", "onions", channelIDPrefix+name+".key")
	if err := os.Remove(key); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// Channels lists the app channels I follow (sol.apps.channels).
func (d *Daemon) Channels() []FollowedChannel { return d.chans.Channels() }

// Hosted lists the app channels this node hosts (sol channel list).
func (d *Daemon) Hosted() []HostedInfo { return d.chans.Hosted() }
