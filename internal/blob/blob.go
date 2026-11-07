// Package blob is a content-addressed file store under ~/.sol/blobs.
//
// Each file is stored at <dir>/<sha256 hex>. The same file has the same
// hash on every transport, so a downloader can verify what they received
// (spec.md 6.4). Files are served with http.ServeContent, which supports
// range requests, so audio and video can stream and seek.
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
	"time"
)

var hashRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ErrNotFound is returned for unknown hashes.
var ErrNotFound = errors.New("blob not found")

// Store is a directory of blobs. Safe for concurrent use: writes go to a
// temp file and are renamed into place, so readers never see partial data.
type Store struct {
	Dir string

	mu sync.Mutex // guards the names file
}

// namesFile maps hash -> original filename. Names are display labels only;
// content is always addressed by hash.
const namesFile = ".names.json"

// Entry describes one stored blob.
type Entry struct {
	Hash     string    `json:"hash"`
	Name     string    `json:"name,omitempty"`
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
func (s *Store) Put(r io.Reader) (string, error) {
	tmp, err := os.CreateTemp(s.Dir, ".upload-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(tmp.Name())
	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), r); err != nil {
		tmp.Close()
		return "", fmt.Errorf("storing blob: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", err
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
	if err != nil || name == "" {
		return hash, err
	}
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	if len(name) > 200 {
		name = name[:200]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	names := s.readNames()
	names[hash] = name
	data, _ := json.Marshal(names)
	return hash, os.WriteFile(filepath.Join(s.Dir, namesFile), data, 0o600)
}

func (s *Store) readNames() map[string]string {
	names := map[string]string{}
	if data, err := os.ReadFile(filepath.Join(s.Dir, namesFile)); err == nil {
		json.Unmarshal(data, &names)
	}
	return names
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
	names := s.readNames()
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
		out = append(out, Entry{Hash: de.Name(), Name: names[de.Name()], Size: info.Size(), Modified: info.ModTime().UTC()})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Modified.After(out[j].Modified) })
	return out, nil
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
// It is what paired devices see when they open the _files service.
func (s *Store) Browser() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /blob/", s.Handler("/blob/"))
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		entries, err := s.List()
		if err != nil {
			http.Error(w, "could not list files", http.StatusInternalServerError)
			return
		}
		rows := make([]listRow, 0, len(entries))
		for _, e := range entries {
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
		listPage.Execute(w, rows)
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

// listPage is rendered with html/template, which escapes every value.
var listPage = template.Must(template.New("files").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Shared files · Sol</title>
<style>
:root{--bg:#f6f5f1;--panel:#fff;--ink:#1c1d21;--muted:#636672;--line:#e3e0d8;--accent:#e08a00;color-scheme:light}
@media (prefers-color-scheme:dark){:root{--bg:#0e0f13;--panel:#17191f;--ink:#ecedf0;--muted:#969aa6;--line:#2a2d36;--accent:#f5b13d;color-scheme:dark}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--ink);font:15px/1.5 system-ui,-apple-system,"Segoe UI",Roboto,sans-serif}
main{max-width:820px;margin:0 auto;padding:24px 16px 48px}h1{font-size:22px;margin:16px 0 4px}
.brand{display:flex;align-items:center;gap:10px;font-weight:700;font-size:20px}
.sun{width:22px;height:22px;border-radius:50%;background:radial-gradient(circle,var(--accent) 55%,transparent 58%),repeating-conic-gradient(var(--accent) 0 10deg,transparent 10deg 30deg)}
.lock{display:inline-block;font-size:12px;font-weight:700;padding:1px 8px;border-radius:99px;background:color-mix(in srgb,#1f8a57 15%,transparent);color:#1f8a57}
.muted{color:var(--muted);font-size:13px}
.file{display:flex;gap:12px;align-items:center;flex-wrap:wrap;background:var(--panel);border:1px solid var(--line);border-radius:12px;padding:12px 14px;margin:10px 0}
.file .name{flex:1;min-width:180px;font-weight:600;word-break:break-word}
.file code{font-size:12px;color:var(--muted);word-break:break-all}
a.btn{display:inline-block;padding:6px 12px;border-radius:8px;border:1px solid var(--line);color:var(--ink);text-decoration:none;font-weight:600;font-size:13px}
a.btn.primary{background:var(--accent);border-color:transparent;color:#1c1d21}
</style></head><body><main>
<div class="brand"><span class="sun"></span>Sol</div>
<h1>Shared files</h1>
<p class="muted"><span class="lock">🔒 Verified connection</span> Shared from your Sol node over Tor. Only
paired devices can open this page. The fingerprint (SHA-256) under each file lets you check
that what you downloaded is exactly what was shared.</p>
{{range .}}<div class="file">
  <div class="name">{{.Name}}<div class="muted">{{.Size}} · {{.When}}</div><code title="SHA-256">{{.Hash}}</code></div>
  <a class="btn" href="{{.Link}}">Open</a>
  <a class="btn primary" href="{{.Link}}?download">Download</a>
</div>{{else}}<p>No files shared yet. Add some in the Sol panel on your node (Files tab).</p>{{end}}
</main></body></html>`))
