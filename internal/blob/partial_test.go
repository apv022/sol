package blob

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math/rand/v2"
	"os"
	"path/filepath"
	"testing"
)

// testBlob returns n pseudo-random bytes and their hash.
func testBlob(n int, seed uint64) ([]byte, string) {
	b := make([]byte, n)
	r := rand.New(rand.NewPCG(seed, 1))
	for i := range b {
		b[i] = byte(r.Uint32())
	}
	d := sha256.Sum256(b)
	return b, hex.EncodeToString(d[:])
}

func chunk(data []byte, i int) []byte {
	off, n := ChunkRange(i, int64(len(data)))
	return data[off : off+n]
}

func TestRangeMath(t *testing.T) {
	for _, tc := range []struct {
		size   int64
		chunks int
		last   int64
	}{{0, 0, 0}, {1, 1, 1}, {ChunkSize, 1, ChunkSize}, {ChunkSize + 1, 2, 1}, {3*ChunkSize - 5, 3, ChunkSize - 5}} {
		if got := Chunks(tc.size); got != tc.chunks {
			t.Errorf("Chunks(%d) = %d", tc.size, got)
		}
		if tc.chunks > 0 {
			if off, n := ChunkRange(tc.chunks-1, tc.size); n != tc.last || off != int64(tc.chunks-1)*ChunkSize {
				t.Errorf("ChunkRange(last, %d) = %d, %d", tc.size, off, n)
			}
		}
	}
	rs := [][2]int64{{0, ChunkSize}, {2 * ChunkSize, 2*ChunkSize + 10}}
	v := FormatRanges(rs)
	if v != "0-4194303,8388608-8388617" {
		t.Fatalf("FormatRanges = %q", v)
	}
	back, err := ParseRanges(v, 2*ChunkSize+10)
	if err != nil || len(back) != 2 || back[0] != rs[0] || back[1] != rs[1] {
		t.Fatalf("ParseRanges = %v, %v", back, err)
	}
	for _, bad := range []string{"5-4", "0-100", "x-1", "-3", "1"} {
		if _, err := ParseRanges(bad, 100); err == nil {
			t.Errorf("ParseRanges(%q) accepted", bad)
		}
	}
	if r, err := ParseRanges("", 0); err != nil || len(r) != 0 {
		t.Fatal("empty ranges")
	}
	if h, ok := ParseRef(Ref(sha256Hex(""))); !ok || h != sha256Hex("") {
		t.Fatal("ParseRef")
	}
	if _, ok := ParseRef("sha256:XYZ"); ok {
		t.Fatal("ParseRef accepted junk")
	}
}

func sha256Hex(s string) string {
	d := sha256.Sum256([]byte(s))
	return hex.EncodeToString(d[:])
}

func TestPartialReadAsItArrives(t *testing.T) {
	s, _ := New(t.TempDir())
	data, hash := testBlob(3*ChunkSize+100, 1)
	p, err := s.Partial(hash, int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := s.Partial(hash, -1); again != p {
		t.Fatal("two Partials for one blob")
	}
	if _, err := s.Partial(hash, 5); !errors.Is(err, ErrSizeMismatch) {
		t.Fatal("size mismatch not caught")
	}
	buf := make([]byte, 10)
	if _, err := p.ReadAt(buf, 0); !errors.Is(err, ErrAbsent) {
		t.Fatalf("read of nothing: %v", err)
	}
	changed := p.Changed()
	if err := p.WriteChunk(0, chunk(data, 0), "devA"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
	default:
		t.Fatal("Changed not closed by a write")
	}
	if err := p.WriteChunk(2, chunk(data, 2), "devB"); err != nil {
		t.Fatal(err)
	}
	if err := p.WriteChunk(1, []byte("short"), "devA"); err == nil {
		t.Fatal("short chunk accepted")
	}
	// A read across the gap returns what is there, then ErrAbsent.
	big := make([]byte, ChunkSize+10)
	n, err := p.ReadAt(big, ChunkSize-10)
	if n != 10 || !errors.Is(err, ErrAbsent) || !bytes.Equal(big[:10], data[ChunkSize-10:ChunkSize]) {
		t.Fatalf("ReadAt across the gap: %d, %v", n, err)
	}
	if n, err := p.ReadAt(buf, 2*ChunkSize+5); n != 10 || err != nil || !bytes.Equal(buf, data[2*ChunkSize+5:2*ChunkSize+15]) {
		t.Fatalf("ReadAt mid-blob: %d, %v", n, err)
	}
	if got := FormatRanges(p.Ranges()); got != "0-4194303,8388608-12582911" {
		t.Fatalf("Ranges = %s", got)
	}
	if !p.HasRange(2*ChunkSize, 10) || p.HasRange(ChunkSize-1, 2) || p.HasRange(0, int64(len(data))+1) {
		t.Fatal("HasRange")
	}
	if m := p.Missing(); len(m) != 2 || m[0] != 1 || m[1] != 3 {
		t.Fatalf("Missing = %v", m)
	}
	if err := p.Finish(); !errors.Is(err, ErrAbsent) {
		t.Fatalf("Finish with ranges missing: %v", err)
	}
	if hs := s.PartialHashes(); len(hs) != 1 || hs[0] != hash || len(s.Hashes()) != 0 {
		t.Fatal("PartialHashes / Hashes")
	}

	// Resume: a fresh Store over the same directory knows what arrived.
	s2, _ := New(s.Dir)
	p2, err := s2.Partial(hash, -1)
	if err != nil || !p2.Has(0) || p2.Has(1) || !p2.Has(2) || p2.Size() != int64(len(data)) {
		t.Fatalf("resume: %v", err)
	}
	for _, i := range p2.Missing() {
		if err := p2.WriteChunk(i, chunk(data, i), "devC"); err != nil {
			t.Fatal(err)
		}
	}
	if p2.Complete() {
		t.Fatal("complete before Finish")
	}
	if err := p2.Finish(); err != nil {
		t.Fatal(err)
	}
	if !p2.Complete() || !s2.Has(hash) || len(s2.PartialHashes()) != 0 {
		t.Fatal("not complete after Finish")
	}
	all, err := io.ReadAll(io.NewSectionReader(p2, 0, p2.Size()))
	if err != nil || !bytes.Equal(all, data) {
		t.Fatal("complete blob reads wrong")
	}
	// A complete blob is seen through the same interface.
	p3, err := s2.Partial(hash, -1)
	if err != nil || !p3.Complete() || len(p3.Missing()) != 0 || FormatRanges(p3.Ranges()) != "0-12583011" {
		t.Fatal("complete blob as Partial")
	}
	if _, err := s2.Partial(sha256Hex("nope"), -1); !errors.Is(err, ErrNotFound) {
		t.Fatal("unknown blob")
	}
}

func TestPartialBadHash(t *testing.T) {
	s, _ := New(t.TempDir())
	data, hash := testBlob(ChunkSize+7, 2)
	p, _ := s.Partial(hash, int64(len(data)))
	evil := bytes.Clone(chunk(data, 1))
	evil[3] ^= 1
	p.WriteChunk(0, chunk(data, 0), "good")
	p.WriteChunk(1, evil, "evil")
	err := p.Finish()
	var bh *BadHashError
	if !errors.As(err, &bh) || !errors.Is(err, ErrBadHash) || len(bh.Sources) != 2 || bh.Sources[1] != "evil" {
		t.Fatalf("Finish: %v", err)
	}
	if !p.Discarded() || s.Has(hash) || len(s.PartialHashes()) != 0 {
		t.Fatal("bad bytes kept")
	}
	if _, err := p.ReadAt(make([]byte, 1), 0); !errors.Is(err, ErrDiscarded) {
		t.Fatal("discarded partial still reads")
	}
	// It can be fetched afresh.
	p2, err := s.Partial(hash, int64(len(data)))
	if err != nil || p2 == p || p2.Has(0) {
		t.Fatal("no fresh start after a bad hash")
	}
}

func TestPartialTornDataResets(t *testing.T) {
	s, _ := New(t.TempDir())
	data, hash := testBlob(2*ChunkSize, 3)
	p, _ := s.Partial(hash, int64(len(data)))
	p.WriteChunk(0, chunk(data, 0), "a")
	// The data file is lost (a crash, a cleaner): the map can't be trusted.
	os.Remove(filepath.Join(s.Dir, ".partial", hash))
	s2, _ := New(s.Dir)
	p2, err := s2.Partial(hash, -1)
	if err != nil || p2.Has(0) {
		t.Fatalf("torn partial kept its map: %v", err)
	}
}

func TestEmptyAndRemove(t *testing.T) {
	s, _ := New(t.TempDir())
	hash := sha256Hex("")
	p, err := s.Partial(hash, 0)
	if err != nil || p.Finish() != nil || !s.Has(hash) {
		t.Fatalf("empty blob: %v", err)
	}
	data, h2 := testBlob(100, 4)
	p2, _ := s.Partial(h2, 100)
	p2.WriteChunk(0, data, "x")
	if err := s.Remove(h2); err != nil || !p2.Discarded() || len(s.PartialHashes()) != 0 {
		t.Fatal("Remove of a partial blob")
	}
	if err := s.Remove(hash); err != nil || s.Has(hash) {
		t.Fatal("Remove of a complete blob")
	}
	h3, _ := s.Put(bytes.NewReader([]byte("labelled")))
	if s.Labelled(h3) {
		t.Fatal("an unnamed Put is not labelled")
	}
	h4, _ := s.PutNamed(bytes.NewReader([]byte("named")), "a.txt")
	if !s.Labelled(h4) {
		t.Fatal("a named file is labelled")
	}
}
