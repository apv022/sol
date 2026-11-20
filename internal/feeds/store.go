package feeds

import (
	"bufio"
	"crypto"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/apv022/sol/internal/events"
	"github.com/apv022/sol/internal/identity"
)

// Identity is what the store needs of this device (an *identity.Keyring
// satisfies it): who I am, and a signer for my device key.
type Identity interface {
	Person() ed25519.PublicKey
	DevicePublic() ed25519.PublicKey
	Signer() crypto.Signer
}

// Lists reports whether device is in the newest device list known for
// person. It is the receiver's device-list check for Accept.
type Lists func(person, device ed25519.PublicKey) bool

// IdentityLists answers Lists from my keyring (for my own person) and the
// identity cache the session layer keeps for contacts.
func IdentityLists(kr *identity.Keyring, cache *identity.Cache) Lists {
	return func(person, device ed25519.PublicKey) bool {
		if me := kr.Person(); me != nil && string(me) == string(person) {
			b, err := kr.Bundle()
			if err != nil {
				return false
			}
			_, ok := b.Devices.Find(device)
			return ok
		}
		_, dl, ok := cache.Latest(person)
		if !ok {
			return false
		}
		_, ok = dl.Find(device)
		return ok
	}
}

// Event types on the store's bus.
const (
	EventEntry = "entry" // Entry is set: a new entry was stored (appended or accepted)
	EventFeed  = "feed"  // Feed and Readable are set: a feed became readable or unreadable to me
)

// Event is what the store (and the views over it) publish on Bus.
type Event struct {
	Type     string
	Entry    *Entry
	Feed     string
	Readable bool
}

// FeedInfo is one row of Feeds: a feed address and its latest pos.
type FeedInfo struct {
	Feed string `json:"feed"`
	Pos  uint64 `json:"pos"`
}

// Config configures a Store.
type Config struct {
	Dir   string // root directory, created 0700
	Self  Identity
	Lists Lists
	Log   *slog.Logger
}

// Store holds every feed this device has, as per-device signed chains on
// disk. Safe for concurrent use.
type Store struct {
	dir   string
	self  Identity
	lists Lists
	log   *slog.Logger
	bus   events.Bus[Event]

	mu      sync.Mutex
	closed  bool
	feeds   map[string]*feed
	devices map[string]bool     // owner|device pairs with a chain anywhere
	dirty   map[string]struct{} // files written since the last Flush
	hooks   []func(Entry)       // views' synchronous watchers (onStored)
}

type feed struct {
	addr  string
	owner ed25519.PublicKey
	dir   string

	mu      sync.RWMutex
	chains  map[string]*chain // by device ID
	refs    []ref             // refs[pos-1]
	idxSize int64
}

type ref struct {
	c   *chain
	seq uint64
}

type chain struct {
	dev    ed25519.PublicKey
	id     string
	path   string
	offs   []int64  // offs[seq-1]: offset of that record
	hashes []string // hashes[seq-1]
	size   int64
}

func (c *chain) last() uint64 { return uint64(len(c.offs)) }

func (c *chain) lastHash() string {
	if len(c.hashes) == 0 {
		return ""
	}
	return c.hashes[len(c.hashes)-1]
}

const (
	indexFile  = "pos.idx"
	chainExt   = ".log"
	idxRecSize = ed25519.PublicKeySize + 8
	// maxRecord bounds one stored record: a 64 KiB body JSON-escaped can
	// grow, so leave plenty of room, but refuse absurd lengths from a torn
	// header.
	maxRecord = 1 << 20
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

// Open opens (or creates) the store in cfg.Dir, repairing any torn tail a
// crash left behind.
func Open(cfg Config) (*Store, error) {
	if cfg.Self == nil || cfg.Lists == nil {
		return nil, errors.New("feeds: Self and Lists are required")
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if err := os.MkdirAll(cfg.Dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{dir: cfg.Dir, self: cfg.Self, lists: cfg.Lists, log: cfg.Log,
		feeds: map[string]*feed{}, devices: map[string]bool{}, dirty: map[string]struct{}{}}
	people, err := os.ReadDir(cfg.Dir)
	if err != nil {
		return nil, err
	}
	for _, p := range people {
		if !p.IsDir() {
			continue
		}
		apps, _ := os.ReadDir(filepath.Join(cfg.Dir, p.Name()))
		for _, a := range apps {
			names, _ := os.ReadDir(filepath.Join(cfg.Dir, p.Name(), a.Name()))
			for _, n := range names {
				addr := Address(p.Name(), a.Name(), n.Name())
				if _, _, _, err := ParseAddress(addr); err != nil || !n.IsDir() {
					continue
				}
				f, err := s.load(addr)
				if err != nil {
					return nil, fmt.Errorf("feeds: loading %s: %w", addr, err)
				}
				s.feeds[addr] = f
			}
		}
	}
	return s, nil
}

// Dir returns the store's root directory.
func (s *Store) Dir() string { return s.dir }

// Bus returns the bus the store publishes entry events on. Views over the
// store publish their feed events on it too.
func (s *Store) Bus() *events.Bus[Event] { return &s.bus }

// load reads one feed from disk, truncating torn or invalid tails.
func (s *Store) load(addr string) (*feed, error) {
	person, _, _, _ := ParseAddress(addr)
	owner, _ := identity.ParseID(person)
	f := &feed{addr: addr, owner: owner, dir: filepath.Join(s.dir, filepath.FromSlash(addr)), chains: map[string]*chain{}}
	files, err := os.ReadDir(f.dir)
	if err != nil {
		return nil, err
	}
	for _, fi := range files {
		id, ok := strings.CutSuffix(fi.Name(), chainExt)
		if !ok {
			continue
		}
		dev, err := identity.ParseID(id)
		if err != nil {
			continue
		}
		c := &chain{dev: dev, id: id, path: filepath.Join(f.dir, fi.Name())}
		if err := s.scanChain(addr, c); err != nil {
			return nil, err
		}
		f.chains[id] = c
		s.devices[person+"|"+id] = true
	}
	return f, s.loadIndex(f)
}

// scanChain reads a chain file, keeping the longest valid prefix.
func (s *Store) scanChain(addr string, c *chain) error {
	fh, err := os.Open(c.path)
	if err != nil {
		return err
	}
	defer fh.Close()
	r := bufio.NewReader(fh)
	var off int64
	for {
		w, n, err := readRecord(r)
		if err == io.EOF {
			break
		}
		if err == nil {
			err = s.checkStored(addr, c, w)
		}
		if err != nil {
			s.log.Warn("truncating a torn or invalid feed record", "feed", addr, "device", c.id, "seq", c.last()+1, "reason", err.Error())
			fh.Close()
			if terr := os.Truncate(c.path, off); terr != nil {
				return terr
			}
			break
		}
		e, _ := w.signed()
		c.offs = append(c.offs, off)
		c.hashes = append(c.hashes, hashOf(e, w.Sig))
		off += n
	}
	c.size = off
	return nil
}

// checkStored validates a record read back from disk: it must continue the
// chain. The signature was checked when it was stored; the CRC guards the
// bytes since.
func (s *Store) checkStored(addr string, c *chain, w Wire) error {
	if w.Feed != addr || w.Device != c.id || w.Seq != c.last()+1 || w.Prev != c.lastHash() {
		return errors.New("record does not continue the chain")
	}
	_, err := w.signed()
	return err
}

// loadIndex reads pos.idx, keeps its longest prefix that matches the
// chains, and appends the entries a crash left out of it.
func (s *Store) loadIndex(f *feed) error {
	path := filepath.Join(f.dir, indexFile)
	raw, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	seen := map[string]uint64{}
	var good int
	for good+idxRecSize <= len(raw) {
		rec := raw[good : good+idxRecSize]
		id := identity.ID(ed25519.PublicKey(rec[:ed25519.PublicKeySize]))
		seq := binary.BigEndian.Uint64(rec[ed25519.PublicKeySize:])
		c := f.chains[id]
		if c == nil || seq != seen[id]+1 || seq > c.last() {
			break
		}
		seen[id] = seq
		f.refs = append(f.refs, ref{c, seq})
		good += idxRecSize
	}
	repair := good != len(raw)
	var missing []byte
	ids := make([]string, 0, len(f.chains))
	for id := range f.chains {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		c := f.chains[id]
		for seq := seen[id] + 1; seq <= c.last(); seq++ {
			missing = append(missing, idxRecord(c.dev, seq)...)
			f.refs = append(f.refs, ref{c, seq})
		}
	}
	f.idxSize = int64(good)
	if !repair && len(missing) == 0 {
		return nil
	}
	s.log.Warn("repairing a feed index", "feed", f.addr, "kept", good/idxRecSize, "added", len(missing)/idxRecSize)
	if err := writeAt(path, int64(good), missing, true); err != nil {
		return err
	}
	f.idxSize += int64(len(missing))
	return nil
}

func idxRecord(dev ed25519.PublicKey, seq uint64) []byte {
	return binary.BigEndian.AppendUint64(append([]byte{}, dev...), seq)
}

// encodeRecord frames a Wire for a chain file: u32 len | JSON | u32 crc32c.
func encodeRecord(w Wire) ([]byte, error) {
	js, err := json.Marshal(w)
	if err != nil {
		return nil, err
	}
	b := binary.BigEndian.AppendUint32(nil, uint32(len(js)))
	b = append(b, js...)
	return binary.BigEndian.AppendUint32(b, crc32.Checksum(js, crcTable)), nil
}

// readRecord reads one framed record and returns its size on disk.
// io.EOF means a clean end; any other error a torn or corrupt record.
func readRecord(r io.Reader) (Wire, int64, error) {
	var hdr [4]byte
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		if err == io.EOF {
			return Wire{}, 0, io.EOF
		}
		return Wire{}, 0, errors.New("torn header")
	}
	n := binary.BigEndian.Uint32(hdr[:])
	if n == 0 || n > maxRecord {
		return Wire{}, 0, errors.New("bad record length")
	}
	buf := make([]byte, n+4)
	if _, err := io.ReadFull(r, buf); err != nil {
		return Wire{}, 0, errors.New("torn record")
	}
	js := buf[:n]
	if crc32.Checksum(js, crcTable) != binary.BigEndian.Uint32(buf[n:]) {
		return Wire{}, 0, errors.New("bad checksum")
	}
	var w Wire
	if err := json.Unmarshal(js, &w); err != nil {
		return Wire{}, 0, err
	}
	return w, int64(n) + 8, nil
}

// writeAt writes b at off in path (creating it), truncating any torn write
// back to off so the file never keeps a partial record. sync fsyncs.
func writeAt(path string, off int64, b []byte, sync bool) error {
	fh, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	if _, err = fh.WriteAt(b, off); err == nil {
		err = fh.Truncate(off + int64(len(b))) // drop any stale tail beyond
	}
	if err == nil && sync {
		err = fh.Sync()
	}
	if err != nil {
		fh.Truncate(off)
		fh.Close()
		return err
	}
	return fh.Close()
}

// feed returns the in-memory feed for addr, creating it if create is set.
func (s *Store) feed(addr string, create bool) (*feed, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil, ErrClosed
	}
	f := s.feeds[addr]
	if f == nil && create {
		person, _, _, _ := ParseAddress(addr)
		owner, _ := identity.ParseID(person)
		f = &feed{addr: addr, owner: owner, dir: filepath.Join(s.dir, filepath.FromSlash(addr)), chains: map[string]*chain{}}
		s.feeds[addr] = f
	}
	return f, nil
}

// Append signs body as the next entry of this device's chain in my feed
// app/name, stores it durably (fsync before returning: a local write is
// irreplaceable) and returns it.
func (s *Store) Append(app, name string, body []byte) (Entry, error) {
	me := s.self.Person()
	if me == nil {
		return Entry{}, ErrNoPerson
	}
	if !ValidApp(app) || !ValidName(name) {
		return Entry{}, fmt.Errorf("%w: bad app or feed name", ErrInvalid)
	}
	body, err := Canonical(body)
	if err != nil {
		return Entry{}, err
	}
	if len(body) > MaxBodySize {
		return Entry{}, fmt.Errorf("%w: body over 64 KiB: put it in a blob", ErrInvalid)
	}
	addr := Address(identity.ID(me), app, name)
	f, err := s.feed(addr, true)
	if err != nil {
		return Entry{}, err
	}
	dev := s.self.DevicePublic()
	f.mu.Lock()
	c := f.chains[identity.ID(dev)]
	w := Wire{Feed: addr, Device: identity.ID(dev), Time: time.Now().UnixMilli(), Body: body, Seq: 1}
	if c != nil {
		w.Seq, w.Prev = c.last()+1, c.lastHash()
	}
	e, err := w.signed()
	if err == nil {
		w.Sig, err = s.self.Signer().Sign(rand.Reader, e, crypto.Hash(0))
	}
	var out Entry
	if err == nil {
		out, err = s.store(f, w, hashOf(e, w.Sig), true)
	}
	f.mu.Unlock()
	if err != nil {
		return Entry{}, err
	}
	s.stored(out)
	return out, nil
}

// Accept verifies an entry from another device and stores it. Receiver
// checks (PROTOCOL.md §Feeds on the wire): a valid signature by the
// entry's device, seq exactly last+1 with a matching prev, and the device
// in the owner's newest device list.
//
// Errors: ErrDuplicate (already stored, harmless), *GapError (fetch the
// missing range first), ErrFork, ErrBadPrev, ErrBadSignature, ErrInvalid,
// ErrUnknownDevice, and ErrRemovedDevice: the device is no longer listed
// though this store holds a chain of it. Entries already stored stay valid
// (the removal point is what this store held when it learned of the
// removal), but no new ones are accepted.
//
// Accepted entries are not fsynced one by one: call Flush after a batch.
// A crash may lose them, and replication fetches them again.
func (s *Store) Accept(w Wire) (Entry, error) {
	hash, err := w.verify()
	if err != nil {
		return Entry{}, err
	}
	f, err := s.feed(w.Feed, true)
	if err != nil {
		return Entry{}, err
	}
	dev, _ := identity.ParseID(w.Device)
	f.mu.Lock()
	out, err := s.accept(f, w, dev, hash)
	f.mu.Unlock()
	if err != nil {
		return Entry{}, err
	}
	s.stored(out)
	return out, nil
}

func (s *Store) accept(f *feed, w Wire, dev ed25519.PublicKey, hash string) (Entry, error) {
	c := f.chains[w.Device]
	var last uint64
	if c != nil {
		last = c.last()
	}
	switch {
	case w.Seq <= last:
		if c.hashes[w.Seq-1] == hash {
			return Entry{}, ErrDuplicate
		}
		return Entry{}, fmt.Errorf("%w: %s@%s seq %d", ErrFork, w.Feed, w.Device, w.Seq)
	case w.Seq > last+1:
		return Entry{}, &GapError{Feed: w.Feed, Device: w.Device, From: last + 1, To: w.Seq - 1}
	}
	prev := ""
	if c != nil {
		prev = c.lastHash()
	}
	if w.Prev != prev {
		return Entry{}, fmt.Errorf("%w: %s@%s seq %d", ErrBadPrev, w.Feed, w.Device, w.Seq)
	}
	if !s.lists(f.owner, dev) {
		person, _, _, _ := ParseAddress(w.Feed)
		s.mu.Lock()
		known := s.devices[person+"|"+w.Device]
		s.mu.Unlock()
		if known {
			return Entry{}, ErrRemovedDevice
		}
		return Entry{}, ErrUnknownDevice
	}
	return s.store(f, w, hash, false)
}

// store writes w (already verified to continue its chain) to disk and to
// the in-memory index. f.mu is held.
func (s *Store) store(f *feed, w Wire, hash string, sync bool) (Entry, error) {
	rec, err := encodeRecord(w)
	if err != nil {
		return Entry{}, err
	}
	c := f.chains[w.Device]
	if c == nil {
		if err := os.MkdirAll(f.dir, 0o700); err != nil {
			return Entry{}, err
		}
		dev, _ := identity.ParseID(w.Device)
		c = &chain{dev: dev, id: w.Device, path: filepath.Join(f.dir, w.Device+chainExt)}
	}
	if err := writeAt(c.path, c.size, rec, sync); err != nil {
		return Entry{}, err
	}
	idxPath := filepath.Join(f.dir, indexFile)
	if err := writeAt(idxPath, f.idxSize, idxRecord(c.dev, w.Seq), sync); err != nil {
		os.Truncate(c.path, c.size) // keep the chain and index in step
		return Entry{}, err
	}
	if f.chains[w.Device] == nil {
		f.chains[w.Device] = c
		syncDir(f.dir)
	}
	c.offs = append(c.offs, c.size)
	c.hashes = append(c.hashes, hash)
	c.size += int64(len(rec))
	f.idxSize += idxRecSize
	f.refs = append(f.refs, ref{c, w.Seq})
	person, _, _, _ := ParseAddress(f.addr)
	s.mu.Lock()
	s.devices[person+"|"+w.Device] = true
	if !sync {
		s.dirty[c.path] = struct{}{}
		s.dirty[idxPath] = struct{}{}
	}
	s.mu.Unlock()
	return w.entry(uint64(len(f.refs)), hash), nil
}

// stored announces a stored entry: on the bus, then to the views' hooks
// (synchronously, with no store lock held, so they may read the store).
func (s *Store) stored(e Entry) {
	s.bus.Publish(Event{Type: EventEntry, Entry: &e})
	s.mu.Lock()
	hooks := s.hooks
	s.mu.Unlock()
	for _, h := range hooks {
		h(e)
	}
}

// onStored registers a hook run after every stored entry. Views use it to
// react to entries that replication brings in, without a goroutine.
func (s *Store) onStored(h func(Entry)) {
	s.mu.Lock()
	s.hooks = append(s.hooks[:len(s.hooks):len(s.hooks)], h)
	s.mu.Unlock()
}

// syncDir makes a new file's directory entry durable (best effort: not
// every OS can fsync a directory).
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		d.Sync()
		d.Close()
	}
}

// Read returns up to limit entries of the feed at addr with pos >= from,
// in pos order. An unknown feed has no entries.
func (s *Store) Read(addr string, from uint64, limit int) ([]Entry, error) {
	f, err := s.feed(addr, false)
	if err != nil || f == nil {
		return nil, err
	}
	from = max(from, 1)
	f.mu.RLock()
	defer f.mu.RUnlock()
	if from > uint64(len(f.refs)) || limit <= 0 {
		return nil, nil
	}
	refs := f.refs[from-1 : min(uint64(len(f.refs)), from-1+uint64(limit))]
	files := map[*chain]*os.File{}
	defer func() {
		for _, fh := range files {
			fh.Close()
		}
	}()
	out := make([]Entry, 0, len(refs))
	for i, r := range refs {
		fh := files[r.c]
		if fh == nil {
			if fh, err = os.Open(r.c.path); err != nil {
				return nil, err
			}
			files[r.c] = fh
		}
		off := r.c.offs[r.seq-1]
		end := r.c.size
		if r.seq < r.c.last() {
			end = r.c.offs[r.seq]
		}
		w, _, err := readRecord(io.NewSectionReader(fh, off, end-off))
		if err != nil {
			return nil, fmt.Errorf("feeds: reading %s: %w", addr, err)
		}
		out = append(out, w.entry(from+uint64(i), r.c.hashes[r.seq-1]))
	}
	return out, nil
}

// Feeds lists every feed in the store with its latest pos, by address.
func (s *Store) Feeds() []FeedInfo {
	s.mu.Lock()
	fs := make([]*feed, 0, len(s.feeds))
	for _, f := range s.feeds {
		fs = append(fs, f)
	}
	s.mu.Unlock()
	out := make([]FeedInfo, 0, len(fs))
	for _, f := range fs {
		f.mu.RLock()
		n := uint64(len(f.refs))
		f.mu.RUnlock()
		if n > 0 {
			out = append(out, FeedInfo{Feed: f.addr, Pos: n})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Feed < out[j].Feed })
	return out
}

// Heads returns the last seq of every device chain in the feed at addr,
// by device ID: the "have" a replica sends.
func (s *Store) Heads(addr string) map[string]uint64 {
	out := map[string]uint64{}
	f, _ := s.feed(addr, false)
	if f == nil {
		return out
	}
	f.mu.RLock()
	defer f.mu.RUnlock()
	for id, c := range f.chains {
		out[id] = c.last()
	}
	return out
}

// Flush fsyncs every file written by Accept since the last Flush.
func (s *Store) Flush() error {
	s.mu.Lock()
	paths := s.dirty
	s.dirty = map[string]struct{}{}
	s.mu.Unlock()
	var errs []error
	for p := range paths {
		fh, err := os.OpenFile(p, os.O_WRONLY, 0)
		if err == nil {
			err = fh.Sync()
			fh.Close()
		}
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// Close flushes and closes the store. Later calls fail with ErrClosed.
func (s *Store) Close() error {
	s.mu.Lock()
	s.closed = true
	s.mu.Unlock()
	return s.Flush()
}
