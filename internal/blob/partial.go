package blob

// Partial storage: a blob that is still arriving (design/PROTOCOL.md
// §Blobs on the wire).
//
// A blob being fetched lives in <dir>/.partial/<hex> (the bytes, written in
// place at their offsets, so the file is sparse until it fills up) next to
// <hex>.json (its size, which ChunkSize ranges are present, and which source
// served each). A range is marked present only after its bytes are fsynced,
// so after a crash or an interrupted fetch the map never claims bytes that
// are not on disk, and the fetch resumes with the ranges still missing.
//
// When every range is present, Finish hashes the whole file. Only if the
// SHA-256 matches is the file moved to <dir>/<hex> and becomes a complete
// blob; otherwise it is discarded and the error names the sources that
// served it. Nothing is ever served as complete before that check.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/apv022/sol/internal/fsutil"
)

// ChunkSize is the unit of partial storage and of fetching: blobs travel in
// ranges of 4 MiB (the last one shorter).
const ChunkSize = 4 << 20

// RefPrefix starts every blob name in the app API and on the wire.
const RefPrefix = "sha256:"

// Errors of partial storage.
var (
	// ErrAbsent means the bytes asked for have not arrived yet.
	ErrAbsent = errors.New("blob: range not present yet")
	// ErrSizeMismatch means a size given for a blob disagrees with the size
	// already recorded for it.
	ErrSizeMismatch = errors.New("blob: size does not match the stored blob")
	// ErrDiscarded is returned by a Partial that was thrown away.
	ErrDiscarded = errors.New("blob: partial blob was discarded")
	// ErrBadHash is matched (errors.Is) by *BadHashError.
	ErrBadHash = errors.New("blob: content does not match its hash")
)

// BadHashError reports a fetched blob whose bytes did not hash to its name.
// The bytes were discarded. Sources lists who served any of them (device
// IDs, as given to WriteChunk), so the caller can stop trusting them.
type BadHashError struct {
	Hash    string
	Sources []string
}

func (e *BadHashError) Error() string {
	return fmt.Sprintf("blob %s: bytes do not match the hash (served by %s)", e.Hash, strings.Join(e.Sources, ", "))
}

// Is makes errors.Is(err, ErrBadHash) true.
func (e *BadHashError) Is(target error) bool { return target == ErrBadHash }

// ParseRef returns the hex digest of a blob name "sha256:<64 hex>".
func ParseRef(ref string) (string, bool) {
	h, ok := strings.CutPrefix(ref, RefPrefix)
	return h, ok && ValidHash(h)
}

// Ref returns the blob name of a hex digest: "sha256:" + hex.
func Ref(hash string) string { return RefPrefix + hash }

// Chunks returns how many ChunkSize ranges a blob of size bytes has.
func Chunks(size int64) int { return int((size + ChunkSize - 1) / ChunkSize) }

// ChunkRange returns the offset and length of range i of a blob of size
// bytes.
func ChunkRange(i int, size int64) (off, n int64) {
	off = int64(i) * ChunkSize
	return off, max(0, min(ChunkSize, size-off))
}

// Partial is one blob as far as it has arrived, or a complete blob seen
// through the same interface. Every holder of the same incomplete blob from
// one Store shares one *Partial. Safe for concurrent use.
type Partial struct {
	s    *Store
	hash string
	size int64

	wmu sync.Mutex // serializes writers (WriteChunk, Finish, Discard)

	mu        sync.RWMutex // guards everything below; held while reading the file
	complete  bool
	discarded bool
	have      []byte  // bitmap of present ranges
	src       []uint8 // per range: index+1 into sources, 0 unknown
	sources   []string
	changed   chan struct{}
}

// partialMap is the JSON form of <hex>.json.
type partialMap struct {
	Size    int64    `json:"size"`
	Have    []byte   `json:"have"` // bitmap, bit i of byte i/8
	Src     []byte   `json:"src"`
	Sources []string `json:"sources"`
}

func (s *Store) partialDir() string { return filepath.Join(s.Dir, ".partial") }

func (p *Partial) dataPath() string { return filepath.Join(p.s.partialDir(), p.hash) }
func (p *Partial) mapPath() string  { return filepath.Join(p.s.partialDir(), p.hash+".json") }

// Partial returns the blob hash as far as it is stored. If it is complete,
// the result says so (and reads the complete file). If part of it is stored,
// that partial blob is returned. Otherwise, with size >= 0, a new empty
// partial blob of that size is created; with size < 0, ErrNotFound.
//
// A size >= 0 that disagrees with what is stored fails with ErrSizeMismatch
// (the stored partial blob is left alone; Discard it to start over).
func (s *Store) Partial(hash string, size int64) (*Partial, error) {
	if !ValidHash(hash) {
		return nil, ErrNotFound
	}
	s.pmu.Lock()
	defer s.pmu.Unlock()
	if p := s.partials[hash]; p != nil {
		if size >= 0 && size != p.size {
			return nil, ErrSizeMismatch
		}
		return p, nil
	}
	if info, err := os.Stat(filepath.Join(s.Dir, hash)); err == nil {
		if size >= 0 && size != info.Size() {
			return nil, ErrSizeMismatch
		}
		return &Partial{s: s, hash: hash, size: info.Size(), complete: true, changed: make(chan struct{})}, nil
	}
	p := &Partial{s: s, hash: hash, changed: make(chan struct{})}
	raw, err := os.ReadFile(p.mapPath())
	switch {
	case err == nil:
		var m partialMap
		if err := json.Unmarshal(raw, &m); err != nil || m.Size < 0 {
			return nil, fmt.Errorf("blob: partial map of %s is corrupt", hash)
		}
		if size >= 0 && size != m.Size {
			return nil, ErrSizeMismatch
		}
		p.size, p.sources = m.Size, m.Sources
		n := Chunks(m.Size)
		p.have, p.src = make([]byte, (n+7)/8), make([]uint8, n)
		copy(p.have, m.Have)
		copy(p.src, m.Src)
		info, err := os.Stat(p.dataPath())
		if err != nil || info.Size() != m.Size {
			// The bytes are gone or torn: nothing in the map can be trusted.
			clear(p.have)
			if err := os.WriteFile(p.dataPath(), nil, 0o600); err != nil {
				return nil, err
			}
			if err := os.Truncate(p.dataPath(), m.Size); err != nil {
				return nil, err
			}
			if err := p.saveLocked(); err != nil {
				return nil, err
			}
		}
	case errors.Is(err, os.ErrNotExist) && size >= 0:
		if err := os.MkdirAll(s.partialDir(), 0o700); err != nil {
			return nil, err
		}
		p.size = size
		n := Chunks(size)
		p.have, p.src = make([]byte, (n+7)/8), make([]uint8, n)
		if err := os.WriteFile(p.dataPath(), nil, 0o600); err != nil {
			return nil, err
		}
		if err := os.Truncate(p.dataPath(), size); err != nil { // sparse
			return nil, err
		}
		if err := p.saveLocked(); err != nil {
			return nil, err
		}
	case errors.Is(err, os.ErrNotExist):
		return nil, ErrNotFound
	default:
		return nil, err
	}
	if s.partials == nil {
		s.partials = map[string]*Partial{}
	}
	s.partials[hash] = p
	return p, nil
}

// PartialHashes lists the blobs stored only in part.
func (s *Store) PartialHashes() []string {
	des, _ := os.ReadDir(s.partialDir())
	var out []string
	for _, de := range des {
		if h, ok := strings.CutSuffix(de.Name(), ".json"); ok && ValidHash(h) {
			out = append(out, h)
		}
	}
	sort.Strings(out)
	return out
}

// Hashes lists the complete blobs in the store.
func (s *Store) Hashes() []string {
	des, _ := os.ReadDir(s.Dir)
	var out []string
	for _, de := range des {
		if ValidHash(de.Name()) && de.Type().IsRegular() {
			out = append(out, de.Name())
		}
	}
	return out
}

// Labelled reports whether the blob carries a name or inbox label (the
// owner's shared-files view), which keeps it out of garbage collection.
func (s *Store) Labelled(hash string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, ok := s.readMeta()[hash]
	return ok && m.referenced()
}

// Remove deletes a blob, complete or partial, and its labels. Readers of a
// partial blob see ErrDiscarded.
func (s *Store) Remove(hash string) error {
	if !ValidHash(hash) {
		return ErrNotFound
	}
	if p, err := s.Partial(hash, -1); err == nil && !p.Complete() {
		p.Discard()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	all := s.readMeta()
	if _, ok := all[hash]; !ok {
		err := os.Remove(filepath.Join(s.Dir, hash))
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return s.commitLocked(all, []string{hash})
}

// Hash returns the blob's hex digest.
func (p *Partial) Hash() string { return p.hash }

// Size returns the blob's total size in bytes.
func (p *Partial) Size() int64 { return p.size }

// Complete reports whether the blob is whole and verified.
func (p *Partial) Complete() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.complete
}

// Discarded reports whether the partial blob was thrown away (bad hash,
// Discard or Remove).
func (p *Partial) Discarded() bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.discarded
}

// Changed returns a channel closed at the next change: a range arrived,
// or the blob completed or was discarded.
func (p *Partial) Changed() <-chan struct{} {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.changed
}

// notifyLocked wakes everyone waiting on Changed. p.mu is held for writing.
func (p *Partial) notifyLocked() {
	close(p.changed)
	p.changed = make(chan struct{})
}

func (p *Partial) hasLocked(i int) bool {
	return p.complete || (i >= 0 && i < len(p.src) && p.have[i/8]&(1<<(i%8)) != 0)
}

// Has reports whether range i is present.
func (p *Partial) Has(i int) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return !p.discarded && p.hasLocked(i)
}

// Missing returns the indexes of the ranges not present yet, in order.
func (p *Partial) Missing() []int {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var out []int
	if p.complete || p.discarded {
		return out
	}
	for i := range p.src {
		if !p.hasLocked(i) {
			out = append(out, i)
		}
	}
	return out
}

// Ranges returns the byte ranges present, merged, as [start, end) pairs.
func (p *Partial) Ranges() [][2]int64 {
	p.mu.RLock()
	defer p.mu.RUnlock()
	var out [][2]int64
	if p.discarded {
		return out
	}
	for i := range Chunks(p.size) {
		if !p.hasLocked(i) {
			continue
		}
		off, n := ChunkRange(i, p.size)
		if k := len(out) - 1; k >= 0 && out[k][1] == off {
			out[k][1] = off + n
		} else {
			out = append(out, [2]int64{off, off + n})
		}
	}
	return out
}

// HasRange reports whether every byte of [off, off+n) is present.
func (p *Partial) HasRange(off, n int64) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.discarded || off < 0 || n < 0 || off+n > p.size {
		return false
	}
	for i := int(off / ChunkSize); int64(i)*ChunkSize < off+n; i++ {
		if !p.hasLocked(i) {
			return false
		}
	}
	return true
}

// Sources returns the sources that served ranges, in first-use order.
func (p *Partial) Sources() []string {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return append([]string(nil), p.sources...)
}

// ReadAt reads the present bytes at off. It returns io.EOF at the end of
// the blob, and ErrAbsent (with the bytes before it) when it reaches a range
// that has not arrived.
func (p *Partial) ReadAt(b []byte, off int64) (int, error) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.discarded {
		return 0, ErrDiscarded
	}
	if off < 0 {
		return 0, errors.New("blob: negative offset")
	}
	if off >= p.size {
		return 0, io.EOF
	}
	want := min(int64(len(b)), p.size-off)
	avail := int64(0)
	for avail < want {
		i := int((off + avail) / ChunkSize)
		if !p.hasLocked(i) {
			break
		}
		cOff, cn := ChunkRange(i, p.size)
		avail = cOff + cn - off
	}
	avail = min(avail, want)
	path := p.dataPath()
	if p.complete {
		path = filepath.Join(p.s.Dir, p.hash)
	}
	n := 0
	if avail > 0 {
		f, err := os.Open(path)
		if err != nil {
			return 0, err
		}
		n, err = f.ReadAt(b[:avail], off)
		f.Close()
		if err != nil && !errors.Is(err, io.EOF) {
			return n, err
		}
	}
	switch {
	case int64(n) < want:
		return n, ErrAbsent
	case int64(n) < int64(len(b)):
		return n, io.EOF
	}
	return n, nil
}

// WriteChunk stores range i (exactly its bytes) as served by source, a
// device ID recorded in case the whole blob later fails its hash. It is
// durable when WriteChunk returns. Writing a range already present is a
// no-op.
func (p *Partial) WriteChunk(i int, data []byte, source string) error {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	p.mu.RLock()
	done, gone := p.hasLocked(i), p.discarded
	p.mu.RUnlock()
	switch {
	case gone:
		return ErrDiscarded
	case i < 0 || i >= Chunks(p.size):
		return fmt.Errorf("blob: range %d out of bounds", i)
	case done:
		return nil
	}
	off, n := ChunkRange(i, p.size)
	if int64(len(data)) != n {
		return fmt.Errorf("blob: range %d is %d bytes, got %d", i, n, len(data))
	}
	f, err := os.OpenFile(p.dataPath(), os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, err = f.WriteAt(data, off)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.have[i/8] |= 1 << (i % 8)
	p.src[i] = p.sourceLocked(source)
	if err := p.saveLocked(); err != nil {
		p.have[i/8] &^= 1 << (i % 8)
		return err
	}
	p.notifyLocked()
	return nil
}

// sourceLocked returns the 1-based index of source, adding it if new (0 if
// the table is full or source is empty).
func (p *Partial) sourceLocked(source string) uint8 {
	if source == "" {
		return 0
	}
	for k, s := range p.sources {
		if s == source {
			return uint8(k + 1)
		}
	}
	if len(p.sources) >= 255 {
		return 0
	}
	p.sources = append(p.sources, source)
	return uint8(len(p.sources))
}

// saveLocked writes the map atomically. p.mu is held (or p is private).
func (p *Partial) saveLocked() error {
	raw, err := json.Marshal(partialMap{Size: p.size, Have: p.have, Src: p.src, Sources: p.sources})
	if err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(p.mapPath(), raw, 0o600)
}

// Finish checks a fully arrived blob against its hash and makes it a
// complete blob. If a range is still missing it returns ErrAbsent. If the
// hash does not match, the bytes are discarded and a *BadHashError names
// the sources that served them.
func (p *Partial) Finish() error {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	p.mu.RLock()
	complete, gone := p.complete, p.discarded
	missing := false
	for i := range p.src {
		missing = missing || !p.hasLocked(i)
	}
	srcs := append([]string(nil), p.sources...)
	p.mu.RUnlock()
	switch {
	case complete:
		return nil
	case gone:
		return ErrDiscarded
	case missing:
		return ErrAbsent
	}
	// No writer can run (wmu), so the file is stable while it is hashed.
	f, err := os.Open(p.dataPath())
	if err != nil {
		return err
	}
	h := sha256.New()
	_, err = io.Copy(h, f)
	f.Close()
	if err != nil {
		return err
	}
	if hex.EncodeToString(h.Sum(nil)) != p.hash {
		p.discardLocked()
		return &BadHashError{Hash: p.hash, Sources: srcs}
	}
	final := filepath.Join(p.s.Dir, p.hash)
	p.mu.Lock()
	// Under p.mu no reader has the file open (Windows can't rename it then).
	err = os.Rename(p.dataPath(), final)
	if err == nil {
		p.complete = true
		os.Remove(p.mapPath())
		p.notifyLocked()
	}
	p.mu.Unlock()
	if err != nil {
		return err
	}
	syncDir(p.s.Dir)
	p.unregister()
	return nil
}

// Discard throws the partial blob away; it can then be fetched afresh.
func (p *Partial) Discard() error {
	p.wmu.Lock()
	defer p.wmu.Unlock()
	if p.Complete() {
		return nil
	}
	return p.discardLocked()
}

// discardLocked removes the files and wakes readers. p.wmu is held.
func (p *Partial) discardLocked() error {
	p.mu.Lock()
	if p.discarded {
		p.mu.Unlock()
		return nil
	}
	p.discarded = true
	err := os.Remove(p.mapPath())
	if rerr := os.Remove(p.dataPath()); err == nil {
		err = rerr
	}
	p.notifyLocked()
	p.mu.Unlock()
	p.unregister()
	if errors.Is(err, os.ErrNotExist) {
		err = nil
	}
	return err
}

func (p *Partial) unregister() {
	p.s.pmu.Lock()
	if p.s.partials[p.hash] == p {
		delete(p.s.partials, p.hash)
	}
	p.s.pmu.Unlock()
}

// syncDir makes a rename durable (best effort: not every OS can fsync a
// directory).
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
}

// FormatRanges renders [start, end) byte ranges as the Sol-Blob-Ranges
// header value: inclusive "first-last" pairs joined by commas, e.g.
// "0-4194303,8388608-9999999". No ranges is "".
func FormatRanges(rs [][2]int64) string {
	parts := make([]string, 0, len(rs))
	for _, r := range rs {
		if r[1] > r[0] {
			parts = append(parts, strconv.FormatInt(r[0], 10)+"-"+strconv.FormatInt(r[1]-1, 10))
		}
	}
	return strings.Join(parts, ",")
}

// ParseRanges parses a Sol-Blob-Ranges header value into [start, end)
// pairs, refusing ranges that fall outside a blob of size bytes.
func ParseRanges(v string, size int64) ([][2]int64, error) {
	var out [][2]int64
	v = strings.TrimSpace(v)
	if v == "" {
		return out, nil
	}
	for part := range strings.SplitSeq(v, ",") {
		a, b, ok := strings.Cut(strings.TrimSpace(part), "-")
		first, err1 := strconv.ParseInt(a, 10, 64)
		last, err2 := strconv.ParseInt(b, 10, 64)
		if !ok || err1 != nil || err2 != nil || first < 0 || last < first || last >= size {
			return nil, fmt.Errorf("blob: bad range %q", part)
		}
		out = append(out, [2]int64{first, last + 1})
	}
	return out, nil
}
