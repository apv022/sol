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
	"io"
	"net/http"
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

// Handler serves GET <prefix><hash> with range support. The ETag is the
// hash itself, so clients can verify content.
func (s *Store) Handler(prefix string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		hash := strings.TrimPrefix(r.URL.Path, prefix)
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
		w.Header().Set("ETag", `"`+hash+`"`)
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		http.ServeContent(w, r, "", info.ModTime(), f)
	})
}
