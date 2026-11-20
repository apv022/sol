package e2e

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/apv022/sol/internal/blob"
	"github.com/apv022/sol/internal/blobnet"
	"github.com/apv022/sol/internal/feeds"
	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/session"
)

// randomBlob returns n pseudo-random bytes and their hex SHA-256.
func randomBlob(n int, seed uint64) ([]byte, string) {
	b := make([]byte, n)
	r := rand.New(rand.NewPCG(seed, 7))
	for i := 0; i+8 <= n; i += 8 {
		v := r.Uint64()
		for k := range 8 {
			b[i+k] = byte(v >> (8 * k))
		}
	}
	d := sha256.Sum256(b)
	return b, hex.EncodeToString(d[:])
}

func (d *daemon) putBlob(t *testing.T, data []byte) string {
	t.Helper()
	h, err := d.blobs.Put(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// blobBytes reads a complete local blob.
func (d *daemon) blobBytes(t *testing.T, hash string) []byte {
	t.Helper()
	f, err := d.blobs.Open(hash)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	b, _ := io.ReadAll(f)
	return b
}

// blobReq sends one raw request for a blob over s.
func blobReq(t *testing.T, s *session.Session, method, hash, rng string) (*http.Response, []byte) {
	t.Helper()
	req, _ := http.NewRequest(method, "http://peer"+blobnet.Path+blob.Ref(hash), nil)
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	resp, err := s.HTTPClient().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

// shareBlob has Alice's laptop share notes "files/music" with Bob, holding
// one entry that references hash, and waits until Bob has it.
func shareBlob(t *testing.T, a1, b *daemon, hash string) string {
	t.Helper()
	if err := a1.shares.Share("files", "music", []string{b.person()}, feeds.ModeRead); err != nil {
		t.Fatal(err)
	}
	a1.append(t, "files", "music", `{"song":"`+blob.Ref(hash)+`","title":"x"}`)
	addr := feeds.Address(a1.person(), "files", "music")
	b.count(t, addr, 1)
	return addr
}

func TestBlobPermission(t *testing.T) {
	a1, a2, b := aliceAndBob(t)
	ctx := context.Background()
	shared, sh := randomBlob(blob.ChunkSize+1000, 1)
	private, ph := randomBlob(1000, 2)
	a1.putBlob(t, shared)
	a1.putBlob(t, private)
	a1.append(t, "files", "secret", `{"f":"`+blob.Ref(ph)+`"}`) // not shared
	shareBlob(t, a1, b, sh)

	// Allowed: referenced by a feed Alice shares with Bob.
	if err := b.bn.Fetch(ctx, blob.Ref(sh), a1.person()); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(b.blobBytes(t, sh), shared) {
		t.Fatal("Bob got the wrong bytes")
	}
	s := b.session(t, a1)
	resp, body := blobReq(t, s, http.MethodGet, sh, "bytes=10-19")
	if resp.StatusCode != http.StatusPartialContent || !bytes.Equal(body, shared[10:20]) ||
		resp.Header.Get("Content-Range") != "bytes 10-19/4195304" {
		t.Fatalf("Range GET: %s %q", resp.Status, resp.Header.Get("Content-Range"))
	}
	resp, _ = blobReq(t, s, http.MethodHead, sh, "")
	if resp.StatusCode != http.StatusOK || resp.Header.Get(blobnet.HeaderSize) != "4195304" ||
		resp.Header.Get(blobnet.HeaderComplete) != "true" || resp.Header.Get(blobnet.HeaderRanges) != "0-4195303" {
		t.Fatalf("HEAD: %s %v", resp.Status, resp.Header)
	}
	if resp, _ := blobReq(t, s, http.MethodGet, sh, "bytes=0-1,5-6"); resp.StatusCode != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("multi-range: %s", resp.Status)
	}

	// Rejected at the handler: held, but referenced by no feed shared with Bob.
	for _, m := range []string{http.MethodGet, http.MethodHead} {
		if resp, body := blobReq(t, s, m, ph, ""); resp.StatusCode != http.StatusForbidden || len(body) != 0 {
			t.Fatalf("%s of an unshared blob: %s", m, resp.Status)
		}
	}
	if err := b.bn.Fetch(ctx, blob.Ref(ph), a1.person()); !errors.Is(err, blobnet.ErrNoSource) {
		t.Fatalf("Fetch of an unshared blob: %v", err)
	}
	if b.blobs.Has(ph) {
		t.Fatal("Bob holds the unshared blob")
	}

	// My own devices may fetch anything.
	if err := a2.bn.Fetch(ctx, ph, ""); err != nil || !bytes.Equal(a2.blobBytes(t, ph), private) {
		t.Fatalf("own device fetch: %v", err)
	}

	// Unsharing revokes.
	if err := a1.shares.Unshare("files", "music", []string{b.person()}); err != nil {
		t.Fatal(err)
	}
	if resp, _ := blobReq(t, s, http.MethodGet, sh, ""); resp.StatusCode != http.StatusForbidden {
		t.Fatalf("GET after unshare: %s", resp.Status)
	}
}

func TestBlobFromTwoDevicesOneDies(t *testing.T) {
	a1, a2, b := aliceAndBob(t)
	data, h := randomBlob(50<<20, 3)
	a1.putBlob(t, data)
	a2.putBlob(t, data)
	shareBlob(t, a1, b, h)
	eventually(t, "the phone to allow Bob", func() bool { return a2.refs.MayFetch(b.person(), h) })

	var n1, n2 atomic.Int32
	killed := make(chan struct{})
	g1 := func(r *http.Request) bool {
		if r.Method == http.MethodGet {
			n1.Add(1)
		}
		return true
	}
	g2 := func(r *http.Request) bool {
		if r.Method != http.MethodGet {
			return true
		}
		// The phone's third request means one of its first two was answered
		// (a source is only given more once its load drops): mid-transfer,
		// the phone goes away.
		if n2.Add(1) == 3 {
			go func() { a2.stopNet(); close(killed) }()
			select {
			case <-r.Context().Done():
			case <-time.After(5 * time.Second):
			}
			return false
		}
		return true
	}
	a1.blobGate.Store(&g1)
	a2.blobGate.Store(&g2)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := b.bn.Fetch(ctx, h, a1.person()); err != nil {
		t.Fatal(err)
	}
	<-killed
	if got := b.blobBytes(t, h); !bytes.Equal(got, data) {
		t.Fatal("the blob does not match")
	}
	if n1.Load() < 1 || n2.Load() < 3 {
		t.Fatalf("ranges served: laptop %d, phone %d (want both used)", n1.Load(), n2.Load())
	}
	t.Logf("ranges served: laptop %d, phone %d", n1.Load(), n2.Load())
	a2.blobGate.Store(nil)
	a2.startNet(a2.addr)
}

func TestBlobRangeReadBeforeComplete(t *testing.T) {
	a1, _, b := aliceAndBob(t)
	data, h := randomBlob(20<<20, 4)
	a1.putBlob(t, data)
	shareBlob(t, a1, b, h)

	// The laptop serves range 3 at once and holds every other one back.
	release := make(chan struct{})
	want := "bytes=" + strconv.Itoa(3*blob.ChunkSize) + "-"
	g := func(r *http.Request) bool {
		if r.Method == http.MethodGet && !strings.HasPrefix(r.Header.Get("Range"), want) {
			select {
			case <-release:
			case <-r.Context().Done():
				return false
			}
		}
		return true
	}
	a1.blobGate.Store(&g)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	r, err := b.bn.Get(ctx, blob.Ref(h), a1.person())
	if err != nil {
		t.Fatal(err)
	}
	if r.Size() != int64(len(data)) {
		t.Fatalf("Size = %d", r.Size())
	}
	off := int64(3*blob.ChunkSize + 12345)
	buf := make([]byte, 5000)
	if n, err := r.ReadAt(buf, off); err != nil || n != len(buf) || !bytes.Equal(buf, data[off:off+5000]) {
		t.Fatalf("ReadAt mid-blob: %d, %v", n, err)
	}
	if r.Complete() || b.blobs.Has(h) {
		t.Fatal("complete before the other ranges were released")
	}
	close(release)
	all, err := io.ReadAll(r)
	if err != nil || !bytes.Equal(all, data) {
		t.Fatalf("streamed read: %d bytes, %v", len(all), err)
	}
	eventually(t, "the blob to complete", func() bool { return b.blobs.Has(h) })
}

func TestPinCopiesToAnchor(t *testing.T) {
	a1, a2, _ := aliceAndBob(t)
	// The phone becomes an anchor.
	dl, err := a1.kr.AddDevice(a2.dev(), "anchor", identity.FlagAnchor, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	bnd, _ := a1.kr.Bundle()
	if err := a2.kr.Adopt(bnd.Person, dl); err != nil || !a2.anchor() {
		t.Fatalf("adopt: %v", err)
	}
	data, h := randomBlob(blob.ChunkSize*2+5, 5)
	kept, kh := randomBlob(100, 6)
	loose, lh := randomBlob(100, 7) // neither pinned nor referenced
	a1.putBlob(t, data)
	a1.putBlob(t, kept)
	a1.putBlob(t, loose)
	a1.append(t, "files", "docs", `{"f":"`+blob.Ref(kh)+`"}`)
	if err := a1.bn.Pin([]string{blob.Ref(h)}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the pinned blob on the anchor", func() bool { return a2.blobs.Has(h) })
	if !bytes.Equal(a2.blobBytes(t, h), data) {
		t.Fatal("the anchor's copy differs")
	}
	if a2.blobs.Has(kh) || a2.blobs.Has(lh) {
		t.Fatal("the anchor copied an unpinned blob")
	}
	// GC keeps pinned and referenced blobs and drops the rest.
	removed, err := a1.bn.GC()
	if err != nil || len(removed) != 1 || removed[0] != lh || !a1.blobs.Has(h) || !a1.blobs.Has(kh) {
		t.Fatalf("GC removed %v, %v", removed, err)
	}
	if err := a1.bn.Unpin([]string{h}); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the unpin on the anchor", func() bool { return !a2.pins.Pinned(h) })
	if removed, err := a2.bn.GC(); err != nil || len(removed) != 1 || removed[0] != h || a2.blobs.Has(h) {
		t.Fatalf("anchor GC removed %v, %v", removed, err)
	}
}

func TestBlobFetchResumes(t *testing.T) {
	a1, _, b := aliceAndBob(t)
	data, h := randomBlob(3*blob.ChunkSize, 8)
	a1.putBlob(t, data)
	shareBlob(t, a1, b, h)
	ctx := context.Background()

	// Only range 0 gets through: the fetch fails, keeping what arrived.
	first := "bytes=0-"
	g := func(r *http.Request) bool {
		return r.Method != http.MethodGet || strings.HasPrefix(r.Header.Get("Range"), first)
	}
	a1.blobGate.Store(&g)
	blobnet.MaxInFlight = 1 // range 0 first, so it is served before the source is dropped
	defer func() { blobnet.MaxInFlight = 4 }()
	if err := b.bn.Fetch(ctx, h, a1.person()); !errors.Is(err, blobnet.ErrNoSource) {
		t.Fatalf("interrupted fetch: %v", err)
	}
	if ps := b.blobs.PartialHashes(); len(ps) != 1 || ps[0] != h {
		t.Fatal("no partial blob kept")
	}
	// The retry fetches only the two missing ranges.
	blobnet.MaxInFlight = 4
	var gets atomic.Int32
	g2 := func(r *http.Request) bool {
		if r.Method == http.MethodGet {
			gets.Add(1)
		}
		return true
	}
	a1.blobGate.Store(&g2)
	if err := b.bn.Fetch(ctx, h, a1.person()); err != nil {
		t.Fatal(err)
	}
	if gets.Load() != 2 || !bytes.Equal(b.blobBytes(t, h), data) {
		t.Fatalf("resumed with %d range requests", gets.Load())
	}
}
