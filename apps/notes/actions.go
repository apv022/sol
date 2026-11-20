package notes

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/apv022/sol/sdk/go/sol"
)

func bad(format string, a ...any) error { return sol.Errorf("bad_request", fmt.Sprintf(format, a...)) }
func missing(what string) error         { return sol.Errorf("not_found", "no such "+what) }
func clash(format string, a ...any) error {
	return sol.Errorf("conflict", fmt.Sprintf(format, a...))
}

// maxAttach is the largest attachment accepted inline (base64 in the action body; actions carry at
// most 4 MiB of JSON, and base64 adds a third). Blobs have no cap.
const maxAttach = 3 << 20

func (a *App) registerActions(act *sol.Actions) {
	h := func(name string, f func(ctx context.Context, c sol.Call) (any, error)) {
		act.Handle(name, func(ctx context.Context, c sol.Call) (any, error) {
			if !c.Caller.Self {
				return nil, sol.Errorf("forbidden", "Notes is for my own devices and agents only")
			}
			return f(ctx, c)
		})
	}
	h("notes.list", a.actList)
	h("notes.read", a.actRead)
	h("notes.write", a.actWrite)
	h("notes.create", a.actCreate)
	h("notes.move", a.actMove)
	h("notes.delete", a.actDelete)
	h("notes.search", a.actSearch)
	h("notes.tags", func(context.Context, sol.Call) (any, error) {
		return map[string]any{"tags": a.idx.tags()}, nil
	})
	h("notes.backlinks", a.actBacklinks)
	h("notes.toggle_task", a.actToggle)
	h("notes.attach", a.actAttach)
	h("notes.pin", a.actPin)
}

type noteSummary struct {
	Path    string   `json:"path"`
	Title   string   `json:"title"`
	Folder  string   `json:"folder"`
	Tags    []string `json:"tags"`
	Mtime   int64    `json:"mtime"` // ms
	Size    int64    `json:"size"`
	Pinned  bool     `json:"pinned"`
	Snippet string   `json:"snippet,omitempty"`
}

func (a *App) summary(d *doc, snippet bool) noteSummary {
	s := noteSummary{Path: d.path, Title: d.title, Folder: dirOf(d.path), Tags: d.tags, Mtime: d.mtime / 1e6, Size: d.size, Pinned: a.isPinned(d.path)}
	if s.Tags == nil {
		s.Tags = []string{}
	}
	if snippet {
		s.Snippet = d.snip
	}
	return s
}

func (a *App) actList(_ context.Context, c sol.Call) (any, error) {
	var in struct {
		Folder   *string `json:"folder"`
		Tag      string  `json:"tag"`
		Pinned   bool    `json:"pinned"`
		Limit    int     `json:"limit"`
		Snippets bool    `json:"snippets"`
		Since    *uint64 `json:"since"`
	}
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	rev := a.idx.revision()
	if in.Since != nil && *in.Since == rev {
		return map[string]any{"rev": rev, "unchanged": true}, nil
	}
	folder := ""
	if in.Folder != nil {
		f, err := cleanRel(*in.Folder, true)
		if err != nil {
			return nil, bad("%v", err)
		}
		folder = f
	}
	tag := strings.ToLower(strings.TrimPrefix(in.Tag, "#"))
	var out []noteSummary
	docs := a.idx.snapshot()
	for _, d := range docs {
		if in.Folder != nil && dirOf(d.path) != folder {
			continue
		}
		if tag != "" && !hasTag(d, tag) {
			continue
		}
		s := a.summary(d, in.Snippets)
		if in.Pinned && !s.Pinned {
			continue
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Pinned != out[j].Pinned {
			return out[i].Pinned
		}
		if out[i].Mtime != out[j].Mtime {
			return out[i].Mtime > out[j].Mtime
		}
		return out[i].Path < out[j].Path
	})
	total := len(out)
	limit := in.Limit
	if limit <= 0 {
		limit = 2000
	}
	if len(out) > limit {
		out = out[:limit]
	}
	if out == nil {
		out = []noteSummary{}
	}
	return map[string]any{"rev": a.idx.revision(), "total": total, "notes": out, "folders": a.idx.folders()}, nil
}

// readNote reads a note's bytes from disk (not the index), so the etag is always what is there now.
func (a *App) readNote(rel string) ([]byte, fs.FileInfo, error) {
	full, err := a.abs(rel)
	if err != nil {
		return nil, nil, bad("%v", err)
	}
	fi, err := os.Lstat(full)
	if err != nil || !fi.Mode().IsRegular() {
		return nil, nil, missing("note")
	}
	if fi.Size() > maxNote {
		return nil, nil, bad("note is larger than 4 MiB")
	}
	b, err := os.ReadFile(full)
	if err != nil {
		return nil, nil, missing("note")
	}
	return b, fi, nil
}

func (a *App) actRead(_ context.Context, c sol.Call) (any, error) {
	var in struct {
		Path string `json:"path"`
	}
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	rel, err := cleanNote(in.Path)
	if err != nil {
		return nil, bad("%v", err)
	}
	b, fi, err := a.readNote(rel)
	if err != nil {
		return nil, err
	}
	info := parseNote(rel, string(b))
	return map[string]any{"path": rel, "title": info.Title, "text": string(b), "etag": etagOf(b), "mtime": fi.ModTime().UnixMilli(),
		"pinned": a.isPinned(rel), "rev": a.idx.revision()}, nil
}

// putLocked writes a note atomically and updates the index at once (the watcher's later event
// then finds nothing new). wmu is held.
func (a *App) putLocked(rel string, data []byte) error {
	full, err := a.abs(rel)
	if err != nil {
		return bad("%v", err)
	}
	if err := writeAtomic(full, data); err != nil {
		return err
	}
	if fi, err := os.Stat(full); err == nil {
		a.idx.put(buildDoc(rel, string(data), fi.ModTime().UnixNano(), int64(len(data)), etagOf(data)))
	}
	return nil
}

func dry(what string, extra map[string]any) map[string]any {
	m := map[string]any{"dry_run": true, "would": what}
	for k, v := range extra {
		m[k] = v
	}
	return m
}

func (a *App) actWrite(_ context.Context, c sol.Call) (any, error) {
	var in struct {
		Path string `json:"path"`
		Text string `json:"text"`
		Base string `json:"base"`
	}
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	rel, err := cleanNote(in.Path)
	if err != nil {
		return nil, bad("%v", err)
	}
	if len(in.Text) > maxNote {
		return nil, bad("note is larger than 4 MiB")
	}
	a.wmu.Lock()
	defer a.wmu.Unlock()
	if full, err := a.abs(rel); err != nil {
		return nil, bad("%v", err)
	} else if cur, err := os.ReadFile(full); err == nil {
		if in.Base != "" && etagOf(cur) != in.Base {
			return nil, sol.Errorf("conflict", "the note changed since you read it (etag "+etagOf(cur)+")")
		}
	} else if in.Base != "" && errors.Is(err, fs.ErrNotExist) {
		return nil, sol.Errorf("conflict", "the note was deleted since you read it")
	}
	if c.DryRun {
		return dry("write "+rel, map[string]any{"path": rel, "bytes": len(in.Text)}), nil
	}
	data := []byte(in.Text)
	if err := a.putLocked(rel, data); err != nil {
		return nil, err
	}
	return map[string]any{"path": rel, "etag": etagOf(data), "rev": a.idx.revision()}, nil
}

// uniqueName finds a free file name stem+ext in dir (relative to Notes), adding " 2", " 3"...
func (a *App) uniqueName(dir, stem, ext string) (string, error) {
	for i := 1; i < 10000; i++ {
		n := stem
		if i > 1 {
			n = fmt.Sprintf("%s %d", stem, i)
		}
		rel := join(dir, n+ext)
		full, err := a.abs(rel)
		if err != nil {
			return "", bad("%v", err)
		}
		if _, err := os.Lstat(full); errors.Is(err, fs.ErrNotExist) {
			return rel, nil
		}
	}
	return "", clash("could not find a free name")
}

func (a *App) actCreate(_ context.Context, c sol.Call) (any, error) {
	var in struct {
		Kind   string  `json:"kind"`
		Folder string  `json:"folder"`
		Title  string  `json:"title"`
		Text   *string `json:"text"`
	}
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	folder, err := cleanRel(in.Folder, true)
	if err != nil {
		return nil, bad("%v", err)
	}
	title := strings.TrimSpace(in.Title)
	stem := sanitizeName(title)
	a.wmu.Lock()
	defer a.wmu.Unlock()
	if in.Kind == "folder" {
		rel, err := a.uniqueName(folder, stem, "")
		if err != nil {
			return nil, err
		}
		if c.DryRun {
			return dry("create folder "+rel, map[string]any{"path": rel}), nil
		}
		full, _ := a.abs(rel)
		if err := os.MkdirAll(full, 0o755); err != nil {
			return nil, err
		}
		a.idx.addDir(rel)
		return map[string]any{"path": rel, "kind": "folder", "rev": a.idx.revision()}, nil
	}
	if in.Kind != "" && in.Kind != "note" {
		return nil, bad("kind is note or folder")
	}
	rel, err := a.uniqueName(folder, stem, ".md")
	if err != nil {
		return nil, err
	}
	text := "# " + stemOf(rel) + "\n\n"
	if title == "" {
		text = "# \n\n"
	}
	if in.Text != nil {
		text = *in.Text
	}
	if c.DryRun {
		return dry("create "+rel, map[string]any{"path": rel}), nil
	}
	if err := a.putLocked(rel, []byte(text)); err != nil {
		return nil, err
	}
	return map[string]any{"path": rel, "kind": "note", "etag": etagOf([]byte(text)), "rev": a.idx.revision()}, nil
}

// planRewrites lists the notes whose wiki links resolve to path (before it moves) by file name
// or by path, and so need rewriting when that name changes.
func (a *App) planRewrites(path string) []string {
	var out []string
	oldStem := strings.ToLower(stemOf(path))
	oldPath := strings.ToLower(strings.TrimSuffix(path, ".md"))
	for _, d := range a.idx.snapshot() {
		for _, l := range d.links {
			t := strings.ToLower(strings.TrimSuffix(l, ".md"))
			if (t == oldStem || t == oldPath) && a.idx.resolve(d.path, l) == path {
				out = append(out, d.path)
				break
			}
		}
	}
	sort.Strings(out)
	return out
}

func (a *App) actMove(_ context.Context, c sol.Call) (any, error) {
	var in struct {
		From         string `json:"from"`
		To           string `json:"to"`
		RewriteLinks *bool  `json:"rewrite_links"`
	}
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	from, err := cleanRel(in.From, false)
	if err != nil {
		return nil, bad("%v", err)
	}
	to, err := cleanRel(in.To, false)
	if err != nil {
		return nil, bad("%v", err)
	}
	isNote := isNoteName(from)
	if isNote && !isNoteName(to) {
		to += ".md"
	}
	if !isNote && isNoteName(to) {
		return nil, bad("a folder cannot become a note")
	}
	if to == from {
		return map[string]any{"path": to, "rewritten": []string{}, "rev": a.idx.revision()}, nil
	}
	if strings.HasPrefix(to, from+"/") {
		return nil, bad("cannot move a folder into itself")
	}
	if baseOf(from) == AttachDir || baseOf(to) == AttachDir || ignorePath(from) || ignorePath(to) {
		return nil, bad("the attachments folder is managed by Notes")
	}
	a.wmu.Lock()
	defer a.wmu.Unlock()
	ffull, err := a.abs(from)
	if err != nil {
		return nil, bad("%v", err)
	}
	tfull, err := a.abs(to)
	if err != nil {
		return nil, bad("%v", err)
	}
	fi, err := os.Lstat(ffull)
	if err != nil || (fi.Mode()&fs.ModeSymlink != 0) || fi.IsDir() == isNote {
		return nil, missing("note or folder")
	}
	if _, err := os.Lstat(tfull); err == nil {
		return nil, clash("%s already exists", to)
	}
	rewrite := isNote && stemOf(from) != stemOf(to) && (in.RewriteLinks == nil || *in.RewriteLinks)
	var plan []string
	if rewrite {
		plan = a.planRewrites(from)
	}
	if c.DryRun {
		return dry("move "+from+" to "+to, map[string]any{"path": to, "rewritten": nonNil(plan)}), nil
	}
	if err := os.MkdirAll(filepath.Dir(tfull), 0o755); err != nil {
		return nil, err
	}
	if err := os.Rename(ffull, tfull); err != nil {
		return nil, err
	}
	a.idx.remove(from)
	a.refreshLocked(to)
	a.movePins(from, to)
	var done []string
	if rewrite {
		oldStem, oldPath := strings.ToLower(stemOf(from)), strings.ToLower(strings.TrimSuffix(from, ".md"))
		for _, p := range plan {
			self := p
			if p == from {
				self = to
			}
			b, _, err := a.readNote(self)
			if err != nil {
				continue
			}
			nt, changed := rewriteLinks(string(b), func(t string) (string, bool) {
				l := strings.ToLower(strings.TrimSuffix(t, ".md"))
				switch {
				case l == oldPath && strings.Contains(t, "/"):
					return strings.TrimSuffix(to, ".md"), true
				case l == oldStem && !strings.Contains(t, "/"):
					return stemOf(to), true
				}
				return "", false
			})
			if changed && a.putLocked(self, []byte(nt)) == nil {
				done = append(done, self)
			}
		}
	}
	return map[string]any{"path": to, "rewritten": nonNil(done), "rev": a.idx.revision()}, nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func (a *App) actDelete(_ context.Context, c sol.Call) (any, error) {
	var in struct {
		Paths []string `json:"paths"`
	}
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	if len(in.Paths) == 0 {
		return nil, bad("no paths")
	}
	type item struct{ rel, full string }
	var items []item
	for _, p := range in.Paths {
		rel, err := cleanRel(p, false)
		if err != nil {
			return nil, bad("%v", err)
		}
		if ignorePath(rel) {
			return nil, bad("the attachments folder is managed by Notes")
		}
		full, err := a.abs(rel)
		if err != nil {
			return nil, bad("%v", err)
		}
		fi, err := os.Lstat(full)
		if err != nil {
			return nil, missing("note or folder: " + rel)
		}
		if !fi.IsDir() && !isNoteName(rel) {
			return nil, bad("%s is not a note", rel)
		}
		items = append(items, item{rel, full})
	}
	if c.DryRun {
		rels := make([]string, len(items))
		for i, it := range items {
			rels[i] = it.rel
		}
		return dry("delete "+strings.Join(rels, ", "), map[string]any{"paths": rels}), nil
	}
	a.wmu.Lock()
	defer a.wmu.Unlock()
	var gone []string
	for _, it := range items {
		if err := os.RemoveAll(it.full); err != nil {
			return nil, err
		}
		a.idx.remove(it.rel)
		a.dropPins(it.rel)
		gone = append(gone, it.rel)
	}
	return map[string]any{"deleted": gone, "rev": a.idx.revision()}, nil
}

// snippetFor finds the first prose line that contains one of the terms, clipped around it.
func (a *App) snippetFor(d *doc, terms []string) string {
	if len(terms) == 0 {
		return d.snip
	}
	b, _, err := a.readNote(d.path)
	if err != nil {
		return d.snip
	}
	out := ""
	eachProseLine(string(b), func(_ int, l string) {
		if out != "" || strings.HasPrefix(strings.TrimSpace(l), "# ") {
			return
		}
		ll := strings.ToLower(l)
		for _, t := range terms {
			if i := strings.Index(ll, t); i >= 0 {
				r := []rune(l)
				ri := len([]rune(l[:i]))
				from := max(0, ri-50)
				s := strings.TrimSpace(string(r[from:min(len(r), from+150)]))
				if from > 0 {
					s = "…" + s
				}
				out = s
				return
			}
		}
	})
	if out == "" {
		return d.snip
	}
	return out
}

func (a *App) actSearch(_ context.Context, c sol.Call) (any, error) {
	var in struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	if in.Limit <= 0 || in.Limit > 200 {
		in.Limit = 50
	}
	hits, terms := a.idx.search(in.Query, in.Limit)
	out := make([]map[string]any, 0, len(hits))
	for _, h := range hits {
		s := a.summary(h.d, false)
		out = append(out, map[string]any{"path": s.Path, "title": s.Title, "folder": s.Folder, "tags": s.Tags, "mtime": s.Mtime,
			"pinned": s.Pinned, "snippet": a.snippetFor(h.d, terms), "score": h.score})
	}
	return map[string]any{"results": out}, nil
}

func (a *App) actBacklinks(_ context.Context, c sol.Call) (any, error) {
	var in struct {
		Path string `json:"path"`
	}
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	rel, err := cleanNote(in.Path)
	if err != nil {
		return nil, bad("%v", err)
	}
	if a.idx.get(rel) == nil {
		return nil, missing("note")
	}
	out := []map[string]any{}
	for _, d := range a.idx.backlinks(rel) {
		line := d.snip
		if b, _, err := a.readNote(d.path); err == nil {
			found := false
			eachProseLine(string(b), func(_ int, l string) {
				if found {
					return
				}
				for _, m := range wikiRe.FindAllStringSubmatch(l, -1) {
					if a.idx.resolve(d.path, linkTarget(m[1])) == rel {
						line, found = clip(strings.TrimSpace(l), 160), true
						return
					}
				}
			})
		}
		out = append(out, map[string]any{"path": d.path, "title": d.title, "snippet": line})
	}
	return map[string]any{"backlinks": out}, nil
}

func (a *App) actToggle(_ context.Context, c sol.Call) (any, error) {
	var in struct {
		Path    string `json:"path"`
		Line    int    `json:"line"`
		Checked *bool  `json:"checked"`
		Base    string `json:"base"`
	}
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	rel, err := cleanNote(in.Path)
	if err != nil {
		return nil, bad("%v", err)
	}
	a.wmu.Lock()
	defer a.wmu.Unlock()
	b, _, err := a.readNote(rel)
	if err != nil {
		return nil, err
	}
	if in.Base != "" && etagOf(b) != in.Base {
		return nil, sol.Errorf("conflict", "the note changed since you read it")
	}
	nt, checked, ok := toggleTask(string(b), in.Line, in.Checked)
	if !ok {
		return nil, bad("line %d is not a task item", in.Line)
	}
	if c.DryRun {
		return dry(fmt.Sprintf("set line %d of %s to checked=%v", in.Line, rel, checked), map[string]any{"checked": checked}), nil
	}
	if err := a.putLocked(rel, []byte(nt)); err != nil {
		return nil, err
	}
	return map[string]any{"path": rel, "line": in.Line, "checked": checked, "text": nt, "etag": etagOf([]byte(nt)), "rev": a.idx.revision()}, nil
}

// hashFile returns the blob-style hash of a file's content, or the open error (fs.ErrNotExist when
// it is missing).
func hashFile(full string) (string, error) {
	f, err := os.Open(full)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

var blockedExt = map[string]bool{".html": true, ".htm": true, ".xhtml": true, ".js": true, ".mjs": true, ".exe": true, ".bat": true, ".cmd": true, ".ps1": true, ".sh": true, ".md": true}

var imageExt = map[string]bool{".png": true, ".jpg": true, ".jpeg": true, ".gif": true, ".webp": true, ".avif": true, ".bmp": true, ".svg": true}

func (a *App) actAttach(ctx context.Context, c sol.Call) (any, error) {
	var in struct {
		Note string `json:"note"`
		Name string `json:"name"`
		Data string `json:"data"`
		Blob string `json:"blob"`
	}
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	note, err := cleanNote(in.Note)
	if err != nil {
		return nil, bad("%v", err)
	}
	if (in.Blob == "") == (in.Data == "") {
		return nil, bad("give either blob (a stored blob hash) or data (base64, small files only)")
	}
	// want is the content hash of the attachment; it decides whether a same-named file is a copy.
	var data []byte
	var want string
	if in.Blob != "" {
		if !sol.ValidHash(in.Blob) {
			return nil, bad("blob is not a blob hash")
		}
		if a.api == nil {
			return nil, sol.Errorf("unavailable", "no daemon to read the blob from")
		}
		if _, err := a.api.StatBlob(ctx, in.Blob, sol.BlobOptions{}); err != nil {
			return nil, err
		}
		want = in.Blob
	} else {
		var err error
		data, err = base64.StdEncoding.DecodeString(in.Data)
		if err != nil {
			return nil, bad("data is not base64")
		}
		if len(data) == 0 || len(data) > maxAttach {
			return nil, bad("inline data must be between 1 byte and 3 MiB; upload bigger files as a blob")
		}
		sum := sha256.Sum256(data)
		want = "sha256:" + hex.EncodeToString(sum[:])
	}
	name := baseOf(strings.ReplaceAll(in.Name, `\`, "/"))
	ext := strings.ToLower(filepath.Ext(name))
	if blockedExt[ext] {
		return nil, bad("%s files cannot be attached", ext)
	}
	stem := sanitizeName(strings.TrimSuffix(name, filepath.Ext(name)))
	if stem == "Untitled" && name == "" {
		stem = "attachment"
	}
	if len(ext) > 12 || strings.ContainsAny(ext, " /\\") {
		ext = ""
	}
	a.wmu.Lock()
	defer a.wmu.Unlock()
	if full, err := a.abs(note); err != nil {
		return nil, bad("%v", err)
	} else if fi, err := os.Lstat(full); err != nil || !fi.Mode().IsRegular() {
		return nil, missing("note")
	}
	dir := join(dirOf(note), AttachDir)
	// Same bytes already stored under this name: reuse it (a re-paste adds no copy).
	var rel string
	exists := false
	for i := 1; ; i++ {
		n := stem
		if i > 1 {
			n = fmt.Sprintf("%s-%d", stem, i)
		}
		cand := join(dir, n+ext)
		full, err := a.abs(cand)
		if err != nil {
			return nil, bad("%v", err)
		}
		got, err := hashFile(full)
		if errors.Is(err, fs.ErrNotExist) {
			rel = cand
			break
		}
		if err == nil && got == want {
			rel = cand
			exists = true
			break
		}
		if i > 9999 {
			return nil, clash("could not find a free name")
		}
	}
	href := AttachDir + "/" + (&url.URL{Path: baseOf(rel)}).EscapedPath()
	md := "[" + stem + ext + "](" + href + ")"
	if imageExt[ext] {
		md = "!" + md
	}
	if c.DryRun {
		return dry("store "+rel, map[string]any{"path": rel, "href": href, "markdown": md}), nil
	}
	if !exists {
		full, _ := a.abs(rel)
		if in.Blob != "" {
			rc, err := a.api.GetBlob(ctx, in.Blob, sol.BlobOptions{})
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			if err := writeAtomicFrom(full, rc); err != nil {
				return nil, err
			}
		} else if err := writeAtomic(full, data); err != nil {
			return nil, err
		}
	}
	return map[string]any{"path": rel, "href": href, "markdown": md}, nil
}

func (a *App) actPin(_ context.Context, c sol.Call) (any, error) {
	var in struct {
		Path   string `json:"path"`
		Pinned bool   `json:"pinned"`
	}
	if err := c.Decode(&in); err != nil {
		return nil, bad("%v", err)
	}
	rel, err := cleanNote(in.Path)
	if err != nil {
		return nil, bad("%v", err)
	}
	if a.idx.get(rel) == nil {
		return nil, missing("note")
	}
	if c.DryRun {
		return dry(fmt.Sprintf("pin %s: %v", rel, in.Pinned), nil), nil
	}
	a.setPin(rel, in.Pinned)
	a.wmu.Lock() // bump the revision so open lists refresh
	a.idx.mu.Lock()
	a.idx.rev++
	a.idx.mu.Unlock()
	a.wmu.Unlock()
	return map[string]any{"path": rel, "pinned": in.Pinned, "rev": a.idx.revision()}, nil
}
