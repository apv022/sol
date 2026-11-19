// Package blob is a content-addressed file store under ~/.sol/blobs.
//
// Each file is stored at <dir>/<sha256 hex>. The same file has the same
// hash on every transport, so a downloader can verify what they received
// (spec.md 6.4). Files are served with http.ServeContent, which supports
// range requests, so audio and video can stream and seek.
//
// Each blob may carry metadata in <dir>/.names.json: the owner's display
// names for it (each one a file on the shared-files page; names are unique),
// and, for a file a peer sent, the name it gave and which peer it was.
// Content is always addressed by hash; metadata is only a label. Files a peer
// sent are kept apart from the owner's own: they are not listed on the
// shared-files page until the owner shares them.
//
// The owner can add, rename, replace and delete files. Because the same
// bytes can be under several names (and in the inbox) at once, a blob is
// deleted from disk only when the last name or inbox entry referring to it
// goes. An unlabelled blob is an unnamed file of the owner's.
package blob

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"

	"time"

	"github.com/apv022/sol/internal/fsutil"
)

var hashRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// Errors.
var (
	// ErrNotFound is returned for unknown hashes.
	ErrNotFound = errors.New("blob not found")
	// ErrTooLarge is returned by PutLimited when the content exceeds the limit.
	ErrTooLarge = errors.New("file is larger than allowed")
	// ErrDuplicate is returned when a shared file already has the name.
	// Callers map it to 409 Conflict.
	ErrDuplicate = errors.New("already exists")
)

// Store is a directory of blobs. Safe for concurrent use: writes go to a
// temp file and are renamed into place, so readers never see partial data.
type Store struct {
	Dir string

	// NodeName, if set, supplies the owner's node name
	mu sync.Mutex // guards the metadata file
}

// namesFile maps hash -> meta. Older builds stored hash -> name (a plain
// string), or hash -> {name} for the owner's files; readMeta accepts both.
const namesFile = ".names.json"

// meta is the metadata stored for one blob. The same bytes can be the
// owner's (under one or more names) and a file a peer sent at once: the blob
// stays on disk while either still refers to it.
type meta struct {
	// Names are the owner's display names for these bytes: each is one file
	// on the shared-files page. Unique across the store.
	Names []string `json:"names,omitempty"`
	// Name and From label a file a peer sent (the inbox): the name it gave
	// and its peer id. From is "" when no peer sent it.
	Name string `json:"name,omitempty"`
	From string `json:"from,omitempty"`
}

// shared reports whether the owner shares these bytes: under a name, or as
// an unnamed file of their own (no peer sent it).
func (m meta) shared() bool { return len(m.Names) > 0 || m.From == "" }

// Entry describes one file: one of the owner's names, an unnamed owner file,
// or a file a peer sent. The same hash can appear in several entries.
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

// Put streams r into the store and returns its hash (an unnamed file of the
// owner's, unless the bytes already have labels).
func (s *Store) Put(r io.Reader) (string, error) {
	return s.store(r, -1, func(map[string]meta, string) ([]string, error) { return nil, nil })
}

// store streams r to a temp file, refusing (before anything reaches the
// store) content longer than max bytes when max >= 0. Then, under the lock,
// label edits the metadata and names blobs that lost their last reference;
// the new file is moved into place, those blobs removed and the metadata
// saved. Doing all of that under one lock means a concurrent Delete can't
// remove bytes that were just labelled.
func (s *Store) store(r io.Reader, max int64, label func(all map[string]meta, hash string) (drop []string, err error)) (string, error) {
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
	s.mu.Lock()
	defer s.mu.Unlock()
	all := s.readMeta()
	drop, err := label(all, sum)
	if err != nil {
		return "", err
	}
	if err := os.Rename(tmp.Name(), filepath.Join(s.Dir, sum)); err != nil {
		return "", err
	}
	return sum, s.commitLocked(all, drop)
}

// commitLocked removes the blobs in drop (and their metadata), then saves
// all. Files go first: if saving fails afterwards, a leftover label points
// at nothing, which List ignores; the other order could leave a peer's file
// unlabelled, and an unlabelled blob counts as the owner's shared file.
func (s *Store) commitLocked(all map[string]meta, drop []string) error {
	for _, h := range drop {
		if err := os.Remove(filepath.Join(s.Dir, h)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		delete(all, h)
	}
	return s.writeMeta(all)
}

// PutNamed is Add without replacing: it stores r as one of the owner's
// files called name (e.g. the uploaded filename). See Add.
func (s *Store) PutNamed(r io.Reader, name string) (string, error) {
	return s.Add(r, name, false)
}

// Add stores r as one of the owner's files, shown on the shared-files page
// as name (cleaned with CleanName; "" keeps it unnamed). If another file
// already has that name, Add fails with ErrDuplicate unless replace is set:
// then the name moves to the new bytes, and the old bytes are deleted when
// nothing else refers to them. Adding the same bytes under a second name
// keeps both names.
func (s *Store) Add(r io.Reader, name string, replace bool) (string, error) {
	name = CleanName(name)
	return s.store(r, -1, func(all map[string]meta, hash string) ([]string, error) {
		m, ok := all[hash]
		if name == "" {
			if ok && !m.shared() {
				// Bytes only a peer had sent: adding them makes them the
				// owner's too, under the name the peer gave.
				n := cmpName(m.Name, hash)
				if _, taken := findName(all, n); taken {
					n = hash[:12]
				}
				m.Names = []string{n}
				all[hash] = m
			}
			return nil, nil
		}
		var drop []string
		if old, found := findName(all, name); found && old != hash {
			if !replace {
				return nil, fmt.Errorf("a shared file called %q: %w", name, ErrDuplicate)
			}
			om := all[old]
			om.Names = without(om.Names, name)
			all[old] = om
			if !om.referenced() {
				drop = append(drop, old)
			}
		}
		if !slices.Contains(m.Names, name) {
			m.Names = append(slices.Clone(m.Names), name)
		}
		all[hash] = m
		return drop, nil
	})
}

// referenced reports whether anything still refers to labelled bytes: an
// owner's name or the inbox.
func (m meta) referenced() bool { return len(m.Names) > 0 || m.From != "" }

// without returns names minus name, as a new slice (nil when empty).
func without(names []string, name string) []string {
	out := slices.DeleteFunc(slices.Clone(names), func(x string) bool { return x == name })
	if len(out) == 0 {
		return nil
	}
	return out
}

// cmpName returns name, or a short form of hash when name is empty.
func cmpName(name, hash string) string {
	if name != "" {
		return name
	}
	return hash[:12]
}

// findName returns the hash the owner's name refers to.
func findName(all map[string]meta, name string) (string, bool) {
	for h, m := range all {
		if slices.Contains(m.Names, name) {
			return h, true
		}
	}
	return "", false
}

// PutLimited stores at most max bytes from r, labelled with name and the
// peer it came from. If r holds more than max bytes nothing is kept and
// ErrTooLarge is returned.
func (s *Store) PutLimited(r io.Reader, name, from string, max int64) (string, error) {
	name = CleanName(name)
	return s.store(r, max, func(all map[string]meta, hash string) ([]string, error) {
		m, ok := all[hash]
		if from == "" || ok && m.shared() || !ok && s.exists(hash) {
			// The owner already has this exact file. A peer sending the same
			// bytes doesn't make it "received", hide it from _files, or rename it.
			return nil, nil
		}
		m.Name, m.From = name, from
		all[hash] = m
		return nil, nil
	})
}

// exists reports whether the blob file is on disk.
func (s *Store) exists(hash string) bool {
	_, err := os.Stat(filepath.Join(s.Dir, hash))
	return err == nil
}

// CleanName reduces a user-supplied filename to a safe display label: no
// directories, no control characters, at most 200 bytes.
func CleanName(name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	name = filepath.Base(strings.ReplaceAll(name, "\\", "/"))
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, name)
	if name == "." || name == "/" || name == ".." {
		return ""
	}
	if len(name) > 200 {
		name = strings.ToValidUTF8(name[:200], "")
	}
	return name
}

// resolveLocked finds one of the owner's files: ref is a name, or the hash
// of an unnamed owner file. name is "" for an unnamed file.
func (s *Store) resolveLocked(all map[string]meta, ref string) (hash, name string, err error) {
	if h, ok := findName(all, ref); ok && s.exists(h) {
		return h, ref, nil
	}
	if m, ok := all[ref]; ValidHash(ref) && s.exists(ref) && (!ok || len(m.Names) == 0 && m.From == "") {
		return ref, "", nil
	}
	return "", "", fmt.Errorf("shared file %q: %w", ref, ErrNotFound)
}

// Rename gives one of the owner's files a new display name. ref is the
// current name, or the hash of an unnamed file. The new name must not be in
// use by another file. Peers see the new name the next time they load the
// shared-files page; links by hash keep working.
func (s *Store) Rename(ref, newName string) error {
	newName = CleanName(newName)
	if newName == "" {
		return errors.New("the new name is empty")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	all := s.readMeta()
	hash, old, err := s.resolveLocked(all, ref)
	if err != nil {
		return err
	}
	if old == newName {
		return nil
	}
	if _, taken := findName(all, newName); taken {
		return fmt.Errorf("a shared file called %q: %w", newName, ErrDuplicate)
	}
	m := all[hash]
	names := slices.Clone(m.Names)
	if i := slices.Index(names, old); old != "" && i >= 0 {
		names[i] = newName
	} else {
		names = append(names, newName)
	}
	m.Names = names
	all[hash] = m
	return s.writeMeta(all)
}

// Delete removes one of the owner's files: ref is its name, or the hash of
// an unnamed file. The bytes are deleted only when nothing else refers to
// them (another name, or the same bytes in the inbox).
func (s *Store) Delete(ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	all := s.readMeta()
	hash, name, err := s.resolveLocked(all, ref)
	if err != nil {
		return err
	}
	m := all[hash]
	m.Names = without(m.Names, name)
	if name == "" || !m.referenced() {
		return s.commitLocked(all, []string{hash})
	}
	all[hash] = m
	return s.writeMeta(all)
}

// DeleteReceived removes a file a peer sent from the inbox. The bytes stay
// when the owner also shares them.
func (s *Store) DeleteReceived(hash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	all := s.readMeta()
	m, ok := all[hash]
	if !ValidHash(hash) || !ok || m.From == "" || !s.exists(hash) {
		return fmt.Errorf("received file %q: %w", hash, ErrNotFound)
	}
	m.Name, m.From = "", ""
	if !m.referenced() {
		return s.commitLocked(all, []string{hash})
	}
	all[hash] = m
	return s.writeMeta(all)
}

// writeMeta saves all. It must be called with s.mu held.
func (s *Store) writeMeta(all map[string]meta) error {
	data, err := json.Marshal(all)
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(filepath.Join(s.Dir, namesFile), data, 0o600)
}

// readMeta must be called with s.mu held. Labels from older builds are
// read as today's: a plain string or a {name} without "from" was the
// owner's file under that name.
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
		if m.From == "" && m.Name != "" {
			if !slices.Contains(m.Names, m.Name) {
				m.Names = append(m.Names, m.Name)
			}
			m.Name = ""
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
	return s.exists(hash)
}

// Shared reports whether the owner shares the bytes of hash, so a peer on
// the shared-files page may download them.
func (s *Store) Shared(hash string) bool {
	s.mu.Lock()
	m, ok := s.readMeta()[hash]
	s.mu.Unlock()
	return s.Has(hash) && (!ok || m.shared())
}

// List returns every file, newest first: one entry per owner's name, one
// for each unnamed owner file, and one for each file a peer sent.
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
		e := Entry{Hash: de.Name(), Size: info.Size(), Modified: info.ModTime().UTC()}
		m := all[de.Name()]
		for _, name := range m.Names {
			e.Name = name
			out = append(out, e)
		}
		if m.From != "" {
			e.Name, e.From = m.Name, m.From
			out = append(out, e)
		} else if len(m.Names) == 0 {
			out = append(out, e) // an unnamed file of the owner's
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Modified.Equal(out[j].Modified) {
			return out[i].Modified.After(out[j].Modified)
		}
		return out[i].Name < out[j].Name
	})
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

// Browser serves the owner's shared files under "/blob/". Only the owner's
// own files are served: files other peers sent stay private.
//
// TODO(C3): the PoC also rendered an HTML file-list page at "/" (with
// NodeName/UserCSS hooks); it depended on the panel's web assets and is cut.
func (s *Store) Browser() http.Handler {
	mux := http.NewServeMux()
	own := s.Handler("/blob/")
	mux.Handle("GET /blob/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hash, _, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/blob/"), "/")
		if !s.Shared(hash) {
			http.NotFound(w, r)
			return
		}
		own.ServeHTTP(w, r)
	}))
	return mux
}
