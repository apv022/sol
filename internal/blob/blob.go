// Package blob is a content-addressed file store under ~/.sol/blobs.
//
// Each file is stored at <dir>/<sha256 hex>. The same file has the same
// hash on every transport, so a downloader can verify what they received
// (spec.md 6.4). Files are served with http.ServeContent, which supports
// range requests, so audio and video can stream and seek.
//
// Each blob may carry metadata (a display name, and which peer sent it) in
// <dir>/.names.json. Content is always addressed by hash; metadata is only a
// label. Files a peer sent are kept apart from the owner's own: they are not
// listed on the shared-files page until the owner shares them.
package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"

	"github.com/apv022/sol/web"
	"time"

	"github.com/apv022/sol/internal/config"
)

var hashRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Errors.
var (
	// ErrNotFound is returned for unknown hashes.
	ErrNotFound = errors.New("blob not found")
	// ErrTooLarge is returned by PutLimited when the content exceeds the limit.
	ErrTooLarge = errors.New("file is larger than allowed")
)

// Store is a directory of blobs. Safe for concurrent use: writes go to a
// temp file and are renamed into place, so readers never see partial data.
type Store struct {
	Dir string

	mu sync.Mutex // guards the metadata file
}

// namesFile maps hash -> meta. Older builds stored hash -> name (a plain
// string); readMeta accepts both.
const namesFile = ".names.json"

// meta is the metadata stored for one blob.
type meta struct {
	Name string `json:"name,omitempty"`
	From string `json:"from,omitempty"` // peer id that sent it; "" means the owner added it
}

// Entry describes one stored blob.
type Entry struct {
	Hash     string    `json:"hash"`
	Name     string    `json:"name,omitempty"`
	From     string    `json:"from,omitempty"` // peer id that sent it; "" for the owner's own files
	Size     int64     `json:"size"`
	Modified time.Time `json:"modified"`
}

// New opens (creating if needed, 0700) a store at dir.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Store{Dir: dir}, nil
}

// ValidHash reports whether h looks like a lowercase SHA-256 hex digest.
func ValidHash(h string) bool { return hashRE.MatchString(h) }

// Put streams r into the store and returns its hash.
func (s *Store) Put(r io.Reader) (string, error) { return s.put(r, -1) }

// put stores r, refusing (before anything reaches the store) content longer
// than max bytes when max >= 0.
func (s *Store) put(r io.Reader, max int64) (string, error) {
	tmp, err := os.CreateTemp(s.Dir, ".upload-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	if max >= 0 {
		r = io.LimitReader(r, max+1)
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(tmp, h), r)
	if err != nil {
		tmp.Close()
		return "", fmt.Errorf("storing blob: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", err
	}
	if max >= 0 && n > max {
		return "", ErrTooLarge
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if err := os.Rename(tmp.Name(), filepath.Join(s.Dir, sum)); err != nil {
		return "", err
	}
	return sum, nil
}

// PutNamed is Put plus a display name (e.g. the uploaded filename).
func (s *Store) PutNamed(r io.Reader, name string) (string, error) {
	hash, err := s.Put(r)
	if err != nil {
		return "", err
	}
	return hash, s.setMeta(hash, meta{Name: CleanName(name)})
}

// PutLimited stores at most max bytes from r, labelled with name and the
// peer it came from. If r holds more than max bytes nothing is kept and
// ErrTooLarge is returned.
func (s *Store) PutLimited(r io.Reader, name, from string, max int64) (string, error) {
	hash, err := s.put(r, max)
	if err != nil {
		return "", err
	}
	return hash, s.setMeta(hash, meta{Name: CleanName(name), From: from})
}

// CleanName reduces a user-supplied filename to a safe display label: no
// directories, no control characters, at most 200 bytes.
func CleanName(name string) string {
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	if name == "." || name == "/" {
		return ""
	}
	if len(name) > 200 {
		name = name[:200]
	}
	return name
}

func (s *Store) setMeta(hash string, m meta) error {
	if m.Name == "" && m.From == "" {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	all := s.readMeta()
	if old, ok := all[hash]; ok && old.From == "" && m.From != "" {
		// The owner already has this exact file. A peer sending the same
		// bytes doesn't make it "received", hide it from _files, or rename it.
		return nil
	}
	all[hash] = m
	data, _ := json.Marshal(all)
	return config.WriteFileAtomic(filepath.Join(s.Dir, namesFile), data, 0o600)
}

// readMeta must be called with s.mu held.
func (s *Store) readMeta() map[string]meta {
	out := map[string]meta{}
	data, err := os.ReadFile(filepath.Join(s.Dir, namesFile))
	if err != nil {
		return out
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(data, &raw) != nil {
		return out
	}
	for h, v := range raw {
		var m meta
		if json.Unmarshal(v, &m) != nil {
			var name string // older builds: hash -> name
			if json.Unmarshal(v, &name) != nil {
				continue
			}
			m.Name = name
		}
		out[h] = m
	}
	return out
}

// Open returns the blob with the given hash.
func (s *Store) Open(hash string) (*os.File, error) {
	if !ValidHash(hash) {
		return nil, ErrNotFound
	}
	f, err := os.Open(filepath.Join(s.Dir, hash))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNotFound
	}
	return f, err
}

// Has reports whether a blob exists.
func (s *Store) Has(hash string) bool {
	if !ValidHash(hash) {
		return false
	}
	_, err := os.Stat(filepath.Join(s.Dir, hash))
	return err == nil
}

// List returns every blob, newest first.
func (s *Store) List() ([]Entry, error) {
	des, err := os.ReadDir(s.Dir)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	all := s.readMeta()
	s.mu.Unlock()
	var out []Entry
	for _, de := range des {
		if !ValidHash(de.Name()) {
			continue
		}
		info, err := de.Info()
		if err != nil {
			continue
		}
		m := all[de.Name()]
		out = append(out, Entry{Hash: de.Name(), Name: m.Name, From: m.From, Size: info.Size(), Modified: info.ModTime().UTC()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	return out, nil
}

// Received returns the total size of the files peers have sent.
func (s *Store) Received() (int64, error) {
	list, err := s.List()
	var n int64
	for _, e := range list {
		if e.From != "" {
			n += e.Size
		}
	}
	return n, err
}

// Handler serves GET <prefix><hash>[/<filename>] with range support. The
// ETag is the hash itself, so clients can verify content. The optional
// filename only sets the Content-Type and the saved name.
//
// Responses carry "Content-Security-Policy: sandbox": an uploaded HTML file
// opened in a browser cannot run scripts in the origin it is served from.
func (s *Store) Handler(prefix string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		hash, name, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, prefix), "/")
		f, err := s.Open(hash)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		info, err := f.Stat()
		if err != nil {
			http.Error(w, "read error", http.StatusInternalServerError)
			return
		}
		h := w.Header()
		h.Set("ETag", `"`+hash+`"`)
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Content-Security-Policy", "sandbox")
		if r.URL.Query().Has("download") {
			if name == "" {
				name = hash
			}
			h.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name}))
		}
		http.ServeContent(w, r, name, info.ModTime(), f)
	})
}

// Browser serves a file-list page at "/" and the files under "/blob/".
// It is what peers see when they open the _files service. Only the owner's
// own files are listed or served: files other peers sent stay private.
func (s *Store) Browser() http.Handler {
	mux := http.NewServeMux()
	own := s.Handler("/blob/")
	mux.Handle("GET /blob/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hash, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/blob/"), "/")
		s.mu.Lock()
		received := s.readMeta()[hash].From != ""
		s.mu.Unlock()
		if received {
			http.NotFound(w, r)
			return
		}
		own.ServeHTTP(w, r)
	}))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		entries, err := s.List()
		if err != nil {
			http.Error(w, "could not list files", http.StatusInternalServerError)
			return
		}
		rows := make([]listRow, 0, len(entries))
		for _, e := range entries {
			if e.From != "" {
				continue
			}
			name := e.Name
			if name == "" {
				name = e.Hash[:12] + "…"
			}
			link := "/blob/" + e.Hash
			if e.Name != "" {
				link += "/" + url.PathEscape(e.Name)
			}
			rows = append(rows, listRow{Name: name, Link: link, Hash: e.Hash, Size: humanSize(e.Size), When: e.Modified.Format("2 Jan 2006 15:04")})
		}
		h := w.Header()
		h.Set("Content-Type", "text/html; charset=utf-8")
		h.Set("Cache-Control", "no-store")
		h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
		h.Set("X-Content-Type-Options", "nosniff")
		listPage.Execute(w, listData{Tokens: template.CSS(web.Tokens()), CSS: filesCSS, Rows: rows})
	})
	return mux
}

type listRow struct{ Name, Link, Hash, Size, When string }

func humanSize(n int64) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	case n < 1<<30:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%.2f GB", float64(n)/(1<<30))
	}
}

// filesCSS is the page's own rules. Every colour, the font and the base size
// come from the panel's tokens (web.Tokens, inlined next to this), so the
// page follows the panel's look and themes. No JS and nothing external: the
// page is served to peers and visitors under a CSP that allows inline style
// only.
const filesCSS = `
*{box-sizing:border-box}
body{margin:0;background:var(--bg);color:var(--ink);font:var(--size)/1.55 var(--mono);-webkit-text-size-adjust:100%}
a{color:var(--accent)}
code{font-family:var(--mono);font-size:.93em}
header{border-bottom:1px solid var(--line);padding:14px 0}
.brand{display:flex;align-items:center;gap:8px;font-size:1.1em;color:var(--accent);max-width:52em;margin:0 auto;padding:0 16px}
.sun{width:18px;height:18px;flex:none}
.brand .muted{font-size:.9em}
.muted{color:var(--muted)}
main{max-width:52em;margin:0 auto;padding:20px 16px 48px}
h1{font-size:1.25em;line-height:1.3;margin:0 0 6px}
.lede{margin:0 0 18px;color:var(--muted)}
.lede b{color:var(--ok);font-weight:700}
ul{list-style:none;margin:0;padding:0}
.file{border-top:1px solid var(--line);padding:14px 0;display:grid;grid-template-columns:minmax(0,1fr) auto;gap:8px 16px;align-items:center}
.file:last-child{border-bottom:1px solid var(--line)}
.name{font-weight:700;overflow-wrap:anywhere;word-break:break-word}
.meta{color:var(--muted);font-size:.9em}
.hash{grid-column:1/-1;color:var(--muted);font-size:.8em;overflow-wrap:anywhere;word-break:break-all}
.actions{display:flex;gap:8px}
.button{display:inline-flex;align-items:center;justify-content:center;min-height:44px;padding:6px 16px;border:1px solid var(--accent);border-radius:2px;color:var(--accent);font-weight:700;text-decoration:none;white-space:nowrap}
.button:hover,.button.primary{background:var(--accent);color:var(--accent-ink)}
.button.primary:hover{filter:brightness(1.1)}
.button.ghost{color:var(--ink);border-color:var(--line);font-weight:400}
.button.ghost:hover{background:var(--panel-2);color:var(--ink);border-color:var(--muted)}
.button:focus-visible{outline:1px solid var(--accent);outline-offset:2px}
.empty{border:1px dashed var(--line);padding:28px 16px;text-align:center;color:var(--muted)}
.empty b{display:block;color:var(--ink);margin-bottom:4px}
@media (max-width:560px){
 .file{grid-template-columns:minmax(0,1fr)}
 .actions .button{flex:1}
}
`

// listPage is rendered with html/template, which escapes every value. The
// page opts into the panel's "system" theme, so it follows the visitor's
// light or dark setting with the same tokens and no script.
var listPage = template.Must(template.New("files").Parse(`<!doctype html>
<html lang="en" data-theme="system"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<meta name="color-scheme" content="dark light">
<title>Shared files · Sol</title>
<style>{{.Tokens}}{{.CSS}}</style></head><body>
<header><div class="brand"><svg class="sun" viewBox="0 0 64 64" aria-hidden="true" fill="currentColor"><circle cx="32" cy="32" r="8"/><g fill="none" stroke="currentColor" stroke-linecap="round"><path stroke-width="5" d="M15.26 34.95A17 17 0 1 1 29.05 48.74"/><path stroke-width="5" d="M57.61 27.49A26 26 0 1 1 36.51 6.39"/></g></svg><b>sol</b><span class="muted">shared files</span></div></header>
<main>
<h1>Shared files</h1>
<p class="lede"><b>Verified connection.</b> Shared from a Sol node; only its peers can open this page. The fingerprint (SHA-256) under each file lets you check that what you downloaded is exactly what was shared.</p>
{{if .Rows}}<ul>{{range .Rows}}
<li class="file">
  <div><div class="name">{{.Name}}</div><div class="meta">{{.Size}} · {{.When}}</div></div>
  <div class="actions"><a class="button ghost" href="{{.Link}}">Open</a><a class="button primary" href="{{.Link}}?download">Download</a></div>
  <code class="hash" title="SHA-256">{{.Hash}}</code>
</li>{{end}}</ul>{{else}}<div class="empty"><b>No files shared yet.</b>Add some in the Sol panel on your node (Files tab).</div>{{end}}
</main></body></html>`))

// listData is what listPage renders. Tokens is the panel's own token block.
type listData struct {
	Tokens, CSS template.CSS
	Rows        []listRow
}
