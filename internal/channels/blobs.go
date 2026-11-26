package channels

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
)

// MaxArtifactSize bounds one artifact (a sol-app tar or an APK) on upload and download.
const MaxArtifactSize = 512 << 20

var hashRe = regexp.MustCompile(`^[0-9a-f]{64}$`)

// ValidHash reports whether h is a lowercase hex SHA-256.
func ValidHash(h string) bool { return hashRe.MatchString(h) }

// Blobs is a content-addressed store of artifacts: <dir>/<sha256 hex>, 0600. A file is there
// only once its bytes hashed to its name.
type Blobs struct{ dir string }

// OpenBlobs opens (creating, 0700) the store at dir.
func OpenBlobs(dir string) (*Blobs, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Blobs{dir: dir}, nil
}

// Path is where the blob of hash lives (whether or not it is there).
func (b *Blobs) Path(hash string) string { return filepath.Join(b.dir, hash) }

// Has reports whether the blob of hash is stored.
func (b *Blobs) Has(hash string) bool {
	if !ValidHash(hash) {
		return false
	}
	st, err := os.Stat(b.Path(hash))
	return err == nil && st.Mode().IsRegular()
}

// Put stores what r yields (at most max bytes) and returns its hash and size. When want is not
// empty the bytes must hash to it and be wantSize long, else nothing is kept.
func (b *Blobs) Put(r io.Reader, max int64, want string, wantSize int64) (string, int64, error) {
	f, err := os.CreateTemp(b.dir, ".in-*")
	if err != nil {
		return "", 0, err
	}
	tmp := f.Name()
	defer os.Remove(tmp) // gone after the rename, or on any failure
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(r, max+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", 0, err
	}
	if n > max {
		return "", 0, fmt.Errorf("channels: larger than %d MiB", max>>20)
	}
	sum := hex.EncodeToString(h.Sum(nil))
	if want != "" && (sum != want || n != wantSize) {
		return "", 0, ErrTampered
	}
	if err := os.Chmod(tmp, 0o600); err != nil {
		return "", 0, err
	}
	if err := os.Rename(tmp, b.Path(sum)); err != nil {
		return "", 0, err
	}
	return sum, n, nil
}

// Remove deletes a blob (a no-op when absent).
func (b *Blobs) Remove(hash string) {
	if ValidHash(hash) {
		_ = os.Remove(b.Path(hash))
	}
}

// Verify re-hashes a stored blob, deleting it when it does not match its name (a disk error, or
// someone editing the store), so a bad file is fetched again instead of served or installed.
func (b *Blobs) Verify(hash string, size int64) bool {
	f, err := os.Open(b.Path(hash))
	if err != nil {
		return false
	}
	h := sha256.New()
	n, err := io.Copy(h, f)
	f.Close()
	if err != nil || n != size || hex.EncodeToString(h.Sum(nil)) != hash {
		b.Remove(hash)
		return false
	}
	return true
}

// ErrTampered is returned when downloaded bytes do not match the hash and size a signed record
// names.
var ErrTampered = errors.New("channels: the artifact does not match the hash its release names")

// Fetch downloads url with client into the store, checking the bytes against hash and size.
// It returns nil once the blob is stored (also when it already was).
func (b *Blobs) Fetch(ctx context.Context, client *http.Client, url, hash string, size int64) error {
	if b.Has(hash) && b.Verify(hash, size) {
		return nil
	}
	if size > MaxArtifactSize {
		return fmt.Errorf("channels: an artifact of %d MiB is larger than allowed", size>>20)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("channels: %s answered %s", req.URL.Host, resp.Status)
	}
	_, _, err = b.Put(resp.Body, size, hash, size)
	return err
}

// Open opens a stored blob.
func (b *Blobs) Open(hash string) (*os.File, error) {
	if !ValidHash(hash) {
		return nil, fs.ErrNotExist
	}
	return os.Open(b.Path(hash))
}
