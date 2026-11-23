package files

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/apv022/sol/sdk/go/sol"
)

// Send to a person (design/apps/FILES.md "Send"). The sender appends one entry per send to its
// own feed send.<recipient>, shared read-only with the recipient: the entry names the files by
// blob hash, so the blob permission rule (API.md §4) lets exactly those blobs through. The
// recipient files one Inbox approval per send and, on approval, copies the files into
// Received/<sender>/ through the normal write path and says so in its own feed recv.<sender>,
// shared read-only with the sender. Nothing here is ever pushed: a send the recipient never
// answers simply stays "sent".

const (
	sendFeedPrefix = "send."
	recvFeedPrefix = "recv."
	receivedDir    = "Received"

	maxSendItems = 100
	maxSendNote  = 1000

	statusSent     = "sent"
	statusSaved    = "saved"
	statusDeclined = "declined"
	statusPending  = "pending"
)

// sendItem is one file of a send.
type sendItem struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	Blob string `json:"blob"`
	Type string `json:"type,omitempty"`
}

// sendEntry is the body of an entry of send.<recipient>.
type sendEntry struct {
	ID    string     `json:"id"`
	Items []sendItem `json:"items"`
	Note  string     `json:"note,omitempty"`
	Time  int64      `json:"time"`
}

// sendAnswer is the body of an entry of recv.<sender>.
type sendAnswer struct {
	Send   string `json:"send"`
	Status string `json:"status"`
	Time   int64  `json:"time"`
	Dir    string `json:"dir,omitempty"` // where the files were saved
}

// sendRec is a send as folded from a feed: Peer is the other person (the sender for incoming,
// the recipient for outgoing).
type sendRec struct {
	sendEntry
	Peer string
}

func (s *sendRec) total() int64 {
	var n int64
	for _, it := range s.Items {
		n += it.Size
	}
	return n
}

// sendState is the model of sends: what I sent, what was sent to me and every answer. It is
// guarded by App.mu.
type sendState struct {
	out    map[string]*sendRec   // "<recipient>/<id>" → my sends
	in     map[string]*sendRec   // "<sender>/<id>" → sends to me
	mine   map[string]sendAnswer // "<sender>/<id>" → my answers
	theirs map[string]sendAnswer // "<recipient>/<id>" → their answers to my sends
	asked  map[string]string     // "<sender>/<id>" → inbox item id (this device filed it)
}

func newSendState() *sendState {
	return &sendState{out: map[string]*sendRec{}, in: map[string]*sendRec{}, mine: map[string]sendAnswer{},
		theirs: map[string]sendAnswer{}, asked: map[string]string{}}
}

// isSendFeed reports whether feed is one of the send/answer feeds, and which person it names.
func isSendFeed(feed string) (kind, person string, ok bool) {
	if p, ok := strings.CutPrefix(feed, sendFeedPrefix); ok {
		return "send", p, true
	}
	if p, ok := strings.CutPrefix(feed, recvFeedPrefix); ok {
		return "recv", p, true
	}
	return "", "", false
}

// applySendEntry folds one entry of a send.* or recv.* feed. a.mu held for writing. Entries are
// untrusted: anything malformed is dropped, and the first entry for an id wins so a later one
// cannot change what an approval was asked about.
func (a *App) applySendEntry(owner, feed string, e sol.Entry) {
	kind, person, _ := isSendFeed(feed)
	st := a.sends
	switch kind {
	case "send":
		if owner != a.me && person != a.me {
			return // someone else's send to a third person
		}
		var s sendEntry
		if json.Unmarshal(e.Body, &s) != nil || !validShareID(s.ID) || len(s.Items) == 0 || len(s.Items) > maxSendItems {
			return
		}
		for i := range s.Items {
			it := &s.Items[i]
			if !sol.ValidHash(it.Blob) || it.Size < 0 || it.Name == "" {
				return
			}
			it.Name = safeName(it.Name)
		}
		s.Note = clip(s.Note, maxSendNote)
		if owner == a.me {
			k := person + "/" + s.ID
			if st.out[k] == nil {
				st.out[k] = &sendRec{sendEntry: s, Peer: person}
			}
		} else {
			k := owner + "/" + s.ID
			if st.in[k] == nil {
				st.in[k] = &sendRec{sendEntry: s, Peer: owner}
			}
		}
	case "recv":
		var an sendAnswer
		if json.Unmarshal(e.Body, &an) != nil || !validShareID(an.Send) || (an.Status != statusSaved && an.Status != statusDeclined) {
			return
		}
		an.Dir = clip(an.Dir, 300)
		switch {
		case owner == a.me:
			if _, dup := st.mine[person+"/"+an.Send]; !dup {
				st.mine[person+"/"+an.Send] = an
			}
		case person == a.me:
			if _, dup := st.theirs[owner+"/"+an.Send]; !dup {
				st.theirs[owner+"/"+an.Send] = an
			}
		}
	}
}

// clip cuts s to at most n bytes without splitting a character.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	s = s[:n]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// safeName turns a file name from another person into one that is valid in the tree on every
// platform (the sender's system may allow names ours does not).
func safeName(n string) string {
	n = strings.ToValidUTF8(n, "_")
	if i := strings.LastIndexAny(n, `/\`); i >= 0 {
		n = n[i+1:]
	}
	var b strings.Builder
	for _, r := range n {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(`<>:"\|?*`, r) {
			r = '_'
		}
		b.WriteRune(r)
	}
	n = strings.TrimRight(b.String(), ". ")
	for validElement(n) != nil {
		if n == "" || n == "." || n == ".." {
			return "file"
		}
		if len(n) > maxElement {
			n = clip(n, maxElement-1)
			continue
		}
		n = "_" + n // reserved Windows names
		if validElement(n) != nil {
			return "file"
		}
	}
	return n
}

// ---------- sender ----------

type sendArg struct {
	Paths  []string `json:"paths"`
	Person string   `json:"person"`
	Note   string   `json:"note"`
}

func (a *App) actSend(ctx context.Context, c sol.Call) (any, error) {
	var in sendArg
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	if len(in.Paths) == 0 {
		return nil, bad("nothing to send: give at least one file")
	}
	if len(in.Paths) > maxSendItems {
		return nil, bad("at most %d files can be sent at once", maxSendItems)
	}
	if in.Person == "" {
		return nil, bad("who should get it? give person")
	}
	if len(in.Note) > maxSendNote {
		return nil, bad("the note is longer than %d bytes", maxSendNote)
	}
	paths := make([]string, 0, len(in.Paths))
	seen := map[string]bool{}
	for _, raw := range in.Paths {
		p, err := cleanArg(raw, false)
		if err != nil {
			return nil, err
		}
		if !seen[p] {
			seen[p] = true
			paths = append(paths, p)
		}
	}
	to, err := a.resolvePeople(ctx, []string{in.Person})
	if err != nil {
		return nil, err
	}
	who := to[0]
	if who.ID == a.me {
		return nil, bad("that is you")
	}
	if !c.DryRun {
		// Publish what was just dropped into the folder, so the file is in the tree.
		a.syncMu.Lock()
		for _, p := range paths {
			if !under(p, sharedDir) {
				if err := a.localChange(ctx, p); err != nil {
					a.syncMu.Unlock()
					return nil, err
				}
			}
		}
		a.syncMu.Unlock()
	}
	v := a.freshView(ctx)
	items := make([]sendItem, 0, len(paths))
	for _, p := range paths {
		f := v.files[p]
		switch {
		case f == nil && v.dirs[p]:
			return nil, bad("%q is a folder: share the folder instead (files.share)", p)
		case f == nil:
			return nil, sol.Errorf("not_found", fmt.Sprintf("no such file %q", p))
		case under(p, sharedDir):
			return nil, bad("%q belongs to someone else: copy it into your own files first", p)
		}
		name := baseOf(p)
		items = append(items, sendItem{Name: name, Size: f.Size, Blob: f.Blob, Type: mime.TypeByExtension(strings.ToLower(path.Ext(name)))})
	}
	name := who.Petname
	if name == "" {
		name = who.Name
	}
	if c.DryRun {
		return plan(planItem{Op: "send", Path: strings.Join(paths, ", "), People: []string{name}, Note: in.Note}), nil
	}
	// The blobs must stay on this device until the recipient has fetched them.
	hs := make([]string, len(items))
	for i, it := range items {
		hs[i] = it.Blob
	}
	if err := a.api.Pin(ctx, hs); err != nil {
		return nil, err
	}
	id := newShareID()
	feed := sendFeedPrefix + who.ID
	// Share before the entry exists: the first thing the recipient can read is the send.
	if err := a.api.Share(ctx, sol.ShareRequest{Feed: feed, People: []string{who.ID}, Mode: sol.ModeRead}); err != nil {
		return nil, err
	}
	if _, err := a.api.Append(ctx, feed, sendEntry{ID: id, Items: items, Note: in.Note, Time: a.nowMs()}); err != nil {
		return nil, err
	}
	a.syncMu.Lock()
	_, err = a.pull(ctx, a.addr(feed))
	a.syncMu.Unlock()
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": id, "to": who.ID, "to_name": name, "files": len(items), "bytes": sumSize(items)}, nil
}

func sumSize(items []sendItem) (n int64) {
	for _, it := range items {
		n += it.Size
	}
	return n
}

type sentItem struct {
	Name   string `json:"name"`
	Size   int64  `json:"size"`
	Status string `json:"status"`
}

type sentOut struct {
	ID     string     `json:"id"`
	To     string     `json:"to"`
	ToName string     `json:"to_name"`
	Time   int64      `json:"time"`
	Note   string     `json:"note,omitempty"`
	Status string     `json:"status"` // sent, saved or declined
	Items  []sentItem `json:"items"`
}

// actSent lists the files I sent, newest first, with what became of each.
func (a *App) actSent(ctx context.Context, c sol.Call) (any, error) {
	a.mu.RLock()
	recs := make([]*sendRec, 0, len(a.sends.out))
	for _, s := range a.sends.out {
		recs = append(recs, s)
	}
	out := make([]sentOut, 0, len(recs))
	for _, s := range recs {
		st := statusSent
		if an, ok := a.sends.theirs[s.Peer+"/"+s.ID]; ok {
			st = an.Status
		}
		so := sentOut{ID: s.ID, To: s.Peer, ToName: a.petnames[s.Peer], Time: s.Time, Note: s.Note, Status: st, Items: []sentItem{}}
		if so.ToName == "" {
			so.ToName = s.Peer
		}
		for _, it := range s.Items {
			so.Items = append(so.Items, sentItem{Name: it.Name, Size: it.Size, Status: st})
		}
		out = append(out, so)
	}
	a.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Time != out[j].Time {
			return out[i].Time > out[j].Time
		}
		return out[i].ID < out[j].ID
	})
	return map[string]any{"sent": out}, nil
}

// ---------- recipient ----------

type recvItem struct {
	Name string `json:"name"`
	Size int64  `json:"size"`
	Type string `json:"type,omitempty"`
}

type recvOut struct {
	Send     string     `json:"send"` // "<sender>/<id>", what files.receive takes
	From     string     `json:"from"`
	FromName string     `json:"from_name"`
	Time     int64      `json:"time"`
	Note     string     `json:"note,omitempty"`
	Status   string     `json:"status"` // pending, saved or declined
	Bytes    int64      `json:"bytes"`
	SavedTo  string     `json:"saved_to,omitempty"`
	Items    []recvItem `json:"items"`
}

// actReceived lists the sends addressed to me, newest first.
func (a *App) actReceived(ctx context.Context, c sol.Call) (any, error) {
	a.mu.RLock()
	out := make([]recvOut, 0, len(a.sends.in))
	for k, s := range a.sends.in {
		ro := recvOut{Send: k, From: s.Peer, FromName: a.petnames[s.Peer], Time: s.Time, Note: s.Note, Status: statusPending, Bytes: s.total(), Items: []recvItem{}}
		if ro.FromName == "" {
			ro.FromName = s.Peer
		}
		if an, ok := a.sends.mine[k]; ok {
			ro.Status, ro.SavedTo = an.Status, an.Dir
		}
		for _, it := range s.Items {
			ro.Items = append(ro.Items, recvItem{Name: it.Name, Size: it.Size, Type: it.Type})
		}
		out = append(out, ro)
	}
	a.mu.RUnlock()
	sort.Slice(out, func(i, j int) bool {
		if out[i].Time != out[j].Time {
			return out[i].Time > out[j].Time
		}
		return out[i].Send < out[j].Send
	})
	return map[string]any{"received": out}, nil
}

// freeName is name, or "name (2).ext", "name (3).ext"... when taken is true for it.
func freeName(dir, name string, taken func(string) bool) string {
	if !taken(join(dir, name)) {
		return name
	}
	ext := path.Ext(name)
	if ext == name { // a dotfile: the whole name is the stem
		ext = ""
	}
	stem := strings.TrimSuffix(name, ext)
	for n := 2; ; n++ {
		cand := fmt.Sprintf("%s (%d)%s", stem, n, ext)
		if !taken(join(dir, cand)) {
			return cand
		}
	}
}

func (a *App) actReceive(ctx context.Context, c sol.Call) (any, error) {
	var in struct {
		Send   string `json:"send"`
		Accept *bool  `json:"accept"`
	}
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	if in.Accept == nil {
		return nil, bad("say accept: true to save the files or false to decline")
	}
	sender, id, ok := strings.Cut(in.Send, "/")
	if !ok || sender == "" || !validShareID(id) {
		return nil, bad("send must look like <sender>/<id> (see files.received)")
	}
	key := sender + "/" + id
	a.mu.RLock()
	s := a.sends.in[key]
	an, answered := a.sends.mine[key]
	a.mu.RUnlock()
	if s == nil { // it may have arrived a moment ago
		if _, err := a.pullAll(ctx); err != nil {
			a.logf("files: refresh: %v", err)
		}
		a.mu.RLock()
		s = a.sends.in[key]
		an, answered = a.sends.mine[key]
		a.mu.RUnlock()
	}
	if s == nil {
		return nil, sol.Errorf("not_found", "no such send")
	}
	if answered { // approving in the Inbox after answering in Files (or twice) is harmless
		return map[string]any{"send": key, "status": an.Status, "saved_to": an.Dir, "already": true}, nil
	}
	fromName := a.personName(sender)
	dir := join(receivedDir, safeName(fromName))
	if c.DryRun {
		if !*in.Accept {
			return plan(planItem{Op: "decline", Path: key}), nil
		}
		return plan(planItem{Op: "receive", Path: key, To: dir, Note: fmt.Sprintf("%d files", len(s.Items))}), nil
	}
	if !*in.Accept {
		if err := a.answerSend(ctx, sender, sendAnswer{Send: id, Status: statusDeclined, Time: a.nowMs()}); err != nil {
			return nil, err
		}
		return map[string]any{"send": key, "status": statusDeclined}, nil
	}
	// Fetch first, without holding the sync lock: this can take a while. The daemon verifies
	// each hash and keeps the blob, so the write below finds it local.
	sizes := make([]int64, len(s.Items))
	for i, it := range s.Items {
		rc, err := a.api.GetBlob(ctx, it.Blob, sol.BlobOptions{From: sender})
		if err != nil {
			return nil, err
		}
		n, err := io.Copy(io.Discard, rc)
		rc.Close()
		if err != nil {
			return nil, err
		}
		sizes[i] = n
	}
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	v := a.getView()
	used := map[string]bool{}
	taken := func(p string) bool { return used[p] || v.files[p] != nil || v.dirs[p] }
	saved := make([]string, 0, len(s.Items))
	hashes := make([]string, 0, len(s.Items))
	for i, it := range s.Items {
		name := freeName(dir, it.Name, taken)
		p := join(dir, name)
		used[p] = true
		if err := ValidatePath(p, false); err != nil {
			return nil, bad("%v", err)
		}
		if err := a.commit(ctx, body{Op: opPut, Path: p, Blob: it.Blob, Size: sizes[i]}); err != nil {
			return nil, err
		}
		saved = append(saved, p)
		hashes = append(hashes, it.Blob)
	}
	if a.isKept(dir) {
		_ = a.api.Pin(ctx, hashes) // the folder is kept here, so are its blobs
	}
	if err := a.answerSend(ctx, sender, sendAnswer{Send: id, Status: statusSaved, Time: a.nowMs(), Dir: dir}); err != nil {
		return nil, err
	}
	if err := a.reconcile(ctx); err != nil && transient(err) {
		a.noteErr(err)
	}
	a.afterPull(ctx)
	return map[string]any{"send": key, "status": statusSaved, "saved_to": dir, "paths": saved}, nil
}

// answerSend records my answer in recv.<sender>, shared read-only with the sender, and folds it.
func (a *App) answerSend(ctx context.Context, sender string, an sendAnswer) error {
	feed := recvFeedPrefix + sender
	if err := a.api.Share(ctx, sol.ShareRequest{Feed: feed, People: []string{sender}, Mode: sol.ModeRead}); err != nil {
		return err
	}
	if _, err := a.api.Append(ctx, feed, an); err != nil {
		return err
	}
	_, err := a.pull(ctx, a.addr(feed))
	return err
}

// notifySends files one Inbox approval per send I have not answered or been asked about. Only
// my first device does it (the same rule as conflict notices), so a person with several
// devices gets one approval, and whichever device it runs on writes the answer into the feed
// every device reads. a.syncMu held.
func (a *App) notifySends(ctx context.Context) {
	a.mu.RLock()
	var todo []string
	for k := range a.sends.in {
		if _, done := a.sends.mine[k]; done {
			continue
		}
		if _, asked := a.sends.asked[k]; asked {
			continue
		}
		todo = append(todo, k)
	}
	a.mu.RUnlock()
	if len(todo) == 0 {
		return
	}
	if ds, err := a.api.Devices(ctx); err == nil {
		for _, d := range ds {
			if d.ID < a.devID {
				return // not my job: my first device files it
			}
		}
	}
	sort.Strings(todo)
	for _, k := range todo {
		a.mu.RLock()
		s := a.sends.in[k]
		who := a.petnames[s.Peer]
		a.mu.RUnlock()
		if who == "" {
			who = "Someone"
		}
		names := make([]string, len(s.Items))
		for i, it := range s.Items {
			names[i] = "- " + it.Name + " (" + fmtBytes(it.Size) + ")"
		}
		what := clip(s.Items[0].Name, 80)
		if len(s.Items) > 1 {
			what = fmt.Sprintf("%d files", len(s.Items))
		}
		text := strings.Join(names, "\n")
		if s.Note != "" {
			text = "\"" + s.Note + "\"\n" + text
		}
		args, _ := json.Marshal(map[string]any{"send": k, "accept": true})
		res, err := a.api.PostInbox(ctx, sol.InboxPost{Kind: "approval", Title: fmt.Sprintf("%s sent you %s (%s)", clip(who, 60), what, fmtBytes(s.total())),
			Body: clip(text, 4000) + "\nApprove to save to " + receivedDir + "/" + safeName(who) + "/, deny to decline.", Action: &sol.ActionCall{Action: "files.receive", Args: args}})
		if err != nil {
			a.logf("files: send not filed: %v", err)
			continue
		}
		a.mu.Lock()
		a.sends.asked[k] = res.ID
		a.mu.Unlock()
		a.markDirty()
	}
}

// onInbox declines a send whose approval I denied.
func (a *App) onInbox(ctx context.Context, ev sol.InboxEvent) {
	if ev.Answer != "denied" {
		return
	}
	a.mu.RLock()
	ref := ""
	for k, id := range a.sends.asked {
		if id == ev.ID {
			ref = k
		}
	}
	_, answered := a.sends.mine[ref]
	a.mu.RUnlock()
	if ref == "" || answered {
		return
	}
	sender, id, _ := strings.Cut(ref, "/")
	a.syncMu.Lock()
	defer a.syncMu.Unlock()
	if err := a.answerSend(ctx, sender, sendAnswer{Send: id, Status: statusDeclined, Time: a.nowMs()}); err != nil {
		a.logf("files: declining a send: %v", err)
	}
}

// fmtBytes is a short human size for titles.
func fmtBytes(n int64) string {
	const k = 1000.0
	f := float64(n)
	switch {
	case f < k:
		return fmt.Sprintf("%d B", n)
	case f < k*k:
		return fmt.Sprintf("%.0f KB", f/k)
	case f < k*k*k:
		return fmt.Sprintf("%.1f MB", f/(k*k))
	}
	return fmt.Sprintf("%.1f GB", f/(k*k*k))
}
