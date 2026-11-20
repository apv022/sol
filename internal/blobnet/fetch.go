package blobnet

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/apv022/sol/internal/blob"
	"github.com/apv022/sol/internal/feeds"
	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/session"
)

// source is one device asked for a blob in one fetch.
type source struct {
	sess     *session.Session
	id       string // device ID, recorded per range for bad-hash reports
	size     int64
	complete bool
	ranges   [][2]int64 // held, [start, end)
	load     int        // ranges in flight from it
	dead     bool       // gone or refusing: not asked again in this fetch
}

func (src *source) has(off, n int64) bool {
	if src.complete {
		return true
	}
	for _, r := range src.ranges {
		if r[0] <= off && off+n <= r[1] {
			return true
		}
	}
	return false
}

// job is the one fetch of a blob in progress. Readers and Fetch calls
// share it; it ends when the blob is complete or nothing can supply the
// missing ranges.
type job struct {
	s    *Service
	hash string
	hint string // person ID to ask after my own devices, or ""

	ready chan struct{} // closed once p is set (the size is known)
	done  chan struct{} // closed when the job ends; err is then set
	kick  chan struct{} // wakes the scheduler (a new want or demand)

	mu     sync.Mutex
	p      *blob.Partial
	demand bool  // fetch everything (set by Fetch, the copier, any read)
	wants  []int // ranges a reader is waiting for, first served first
	err    error
}

// job returns the fetch of hash in progress, or starts one. demand starts
// fetching at once; otherwise the job only asks the sources and waits for a
// reader to say where it reads, so a first read in the middle of the blob
// (a seek) is the first range fetched.
func (s *Service) job(hash, hint string, demand bool, wants ...int) (*job, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ctx.Err() != nil {
		return nil, ErrClosed
	}
	j := s.jobs[hash]
	if j == nil {
		j = &job{s: s, hash: hash, hint: hint, ready: make(chan struct{}), done: make(chan struct{}), kick: make(chan struct{}, 1)}
		s.jobs[hash] = j
		s.wg.Add(1)
		go j.run()
	}
	j.want(demand, wants...)
	return j, nil
}

// want records demand and the ranges a reader waits for, and wakes the
// scheduler.
func (j *job) want(demand bool, wants ...int) {
	if !demand && len(wants) == 0 {
		return
	}
	j.mu.Lock()
	j.demand = true
	j.wants = append(j.wants, wants...)
	j.mu.Unlock()
	select {
	case j.kick <- struct{}{}:
	default:
	}
}

func (j *job) partial() *blob.Partial {
	j.mu.Lock()
	defer j.mu.Unlock()
	return j.p
}

func (j *job) result() error {
	<-j.done
	return j.err
}

func (j *job) run() {
	s := j.s
	defer s.wg.Done()
	err := j.fetch(s.ctx)
	if err != nil && s.ctx.Err() == nil && !errors.Is(err, errIdle) {
		s.log.Info("blob fetch failed", "blob", short(j.hash), "err", err)
	}
	s.mu.Lock()
	delete(s.jobs, j.hash)
	s.mu.Unlock()
	j.mu.Lock()
	j.err = err
	j.mu.Unlock()
	close(j.done)
}

// candidates returns the live sessions that may have the blob, in the order
// to prefer them: my own devices, then the hinted person's, then those of
// any contact whose feed (replicated here) references the blob.
func (j *job) candidates() []*session.Session {
	owners := map[string]bool{}
	for _, addr := range j.s.cfg.Refs.Feeds(j.hash) {
		if owner, _, _, err := feeds.ParseAddress(addr); err == nil {
			owners[owner] = true
		}
	}
	var mine, hinted, others []*session.Session
	for _, sess := range j.s.cfg.Sessions() {
		if !sess.Alive() {
			continue
		}
		p := sess.Peer()
		switch pid := identity.ID(p.Person); {
		case p.Relation == session.Self:
			mine = append(mine, sess)
		case j.hint != "" && pid == j.hint:
			hinted = append(hinted, sess)
		case owners[pid]:
			others = append(others, sess)
		}
	}
	return append(append(mine, hinted...), others...)
}

// discover asks every candidate what it holds (HEAD, in parallel) and
// returns the usable sources, in candidate order.
func (j *job) discover(ctx context.Context) []*source {
	cands := j.candidates()
	srcs := make([]*source, len(cands))
	var wg sync.WaitGroup
	for i, sess := range cands {
		wg.Go(func() {
			srcs[i] = head(ctx, sess, j.hash)
		})
	}
	wg.Wait()
	var out []*source
	for _, src := range srcs {
		if src != nil {
			out = append(out, src)
		}
	}
	return out
}

// head asks one device what it holds of the blob; nil if it has nothing or
// refuses.
func head(ctx context.Context, sess *session.Session, hash string) *source {
	ctx, cancel := context.WithTimeout(ctx, HeadTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodHead, "http://peer"+Path+blob.Ref(hash), nil)
	if err != nil {
		return nil
	}
	resp, err := sess.HTTPClient().Do(req)
	if err != nil {
		return nil
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	size, err := strconv.ParseInt(resp.Header.Get(HeaderSize), 10, 64)
	if err != nil || size < 0 {
		return nil
	}
	src := &source{sess: sess, id: identity.ID(sess.Peer().Device), size: size,
		complete: resp.Header.Get(HeaderComplete) == "true"}
	if !src.complete {
		if src.ranges, err = blob.ParseRanges(resp.Header.Get(HeaderRanges), size); err != nil || len(src.ranges) == 0 {
			return nil
		}
	}
	return src
}

// fetch finds sources, settles the size, then fetches the missing ranges
// and verifies the whole blob.
func (j *job) fetch(ctx context.Context) error {
	store := j.s.cfg.Blobs
	local, err := store.Partial(j.hash, -1)
	if err != nil && !errors.Is(err, blob.ErrNotFound) {
		return err
	}
	var srcs []*source
	if local == nil || len(local.Missing()) > 0 {
		srcs = j.discover(ctx)
	}
	// The size: what is stored if a source agrees (or none is needed),
	// else what the most preferred source says. Sources that disagree are
	// dropped: one of us is wrong, and the hash will tell.
	var p *blob.Partial
	switch {
	case local != nil && len(local.Missing()) == 0:
		p = local
	case local != nil && agree(srcs, local.Size()):
		p = local
	case len(srcs) == 0:
		return ErrNoSource
	default:
		if local != nil {
			local.Discard()
		}
		if p, err = store.Partial(j.hash, srcs[0].size); err != nil {
			return err
		}
	}
	kept := srcs[:0]
	for _, src := range srcs {
		if src.size == p.Size() {
			kept = append(kept, src)
		}
	}
	srcs = kept
	j.mu.Lock()
	j.p = p
	j.mu.Unlock()
	close(j.ready)
	if p.Complete() {
		return nil
	}
	if err := j.schedule(ctx, p, srcs); err != nil {
		return err
	}
	if err := p.Finish(); err != nil {
		var bad *blob.BadHashError
		if errors.As(err, &bad) {
			j.s.log.Warn("fetched blob failed its hash; discarded", "blob", short(j.hash), "sources", sourceList(bad.Sources))
		}
		return err
	}
	return nil
}

func agree(srcs []*source, size int64) bool {
	for _, src := range srcs {
		if src.size == size {
			return true
		}
	}
	return false
}

type rangeResult struct {
	i    int
	src  *source
	data []byte
	err  error
}

// schedule fetches every missing range, at most MaxInFlight at once,
// spreading them over the least loaded sources that hold them, retrying a
// failed range on another source. It returns once every range is present.
func (j *job) schedule(ctx context.Context, p *blob.Partial, srcs []*source) error {
	inflight := map[int]*source{}
	failed := map[int]map[*source]bool{}
	results := make(chan rangeResult, MaxInFlight) // senders never block
	cursor := 0
	defer func() {
		for range inflight { // let in-flight ranges finish before returning
			<-results
		}
	}()
	for {
		if len(inflight) == 0 && len(p.Missing()) == 0 {
			return nil
		}
		j.mu.Lock()
		demand := j.demand
		j.mu.Unlock()
		for demand && len(inflight) < MaxInFlight {
			i, src := j.pick(p, srcs, inflight, failed, &cursor)
			if src == nil {
				break
			}
			inflight[i] = src
			src.load++
			go func() {
				data, err := fetchRange(ctx, src.sess, j.hash, i, p.Size())
				results <- rangeResult{i, src, data, err}
			}()
		}
		if demand && len(inflight) == 0 {
			return fmt.Errorf("%w: %d ranges missing", ErrNoSource, len(p.Missing()))
		}
		var idle <-chan time.Time
		if !demand {
			idle = time.After(IdleTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-idle:
			return errIdle
		case <-j.kick:
		case r := <-results:
			delete(inflight, r.i)
			r.src.load--
			err := r.err
			if err == nil {
				if err = p.WriteChunk(r.i, r.data, r.src.id); errors.Is(err, blob.ErrDiscarded) {
					return err
				}
			}
			if err != nil {
				if failed[r.i] == nil {
					failed[r.i] = map[*source]bool{}
				}
				failed[r.i][r.src] = true
				var se statusError
				if !r.src.sess.Alive() || errors.As(err, &se) && se != http.StatusRequestedRangeNotSatisfiable {
					r.src.dead = true
				}
				j.s.log.Debug("blob range failed", "blob", short(j.hash), "range", r.i, "source", short(r.src.id), "err", err)
			}
		}
	}
}

// pick returns the next range to fetch and the source to fetch it from:
// ranges readers wait for first, then the rest in order from just after
// the last range a reader asked for (read-ahead from a seek), wrapping
// around. src is nil when nothing can be started now.
func (j *job) pick(p *blob.Partial, srcs []*source, inflight map[int]*source, failed map[int]map[*source]bool, cursor *int) (int, *source) {
	size := p.Size()
	best := func(i int) *source {
		off, n := blob.ChunkRange(i, size)
		var b *source
		for _, src := range srcs {
			if !src.dead && !failed[i][src] && src.has(off, n) && (b == nil || src.load < b.load) {
				b = src
			}
		}
		return b
	}
	j.mu.Lock()
	for len(j.wants) > 0 {
		i := j.wants[0]
		j.wants = j.wants[1:]
		if i < 0 || i >= blob.Chunks(size) || p.Has(i) || inflight[i] != nil {
			continue
		}
		if src := best(i); src != nil {
			*cursor = i + 1
			j.mu.Unlock()
			return i, src
		}
	}
	j.mu.Unlock()
	n := blob.Chunks(size)
	for k := range n {
		i := (*cursor + k) % n
		if p.Has(i) || inflight[i] != nil {
			continue
		}
		if src := best(i); src != nil {
			return i, src
		}
	}
	return -1, nil
}

// errIdle ends a job that nobody read from for IdleTimeout.
var errIdle = errors.New("blobnet: nobody read the blob")

// statusError is an unexpected HTTP status from a source.
type statusError int

func (e statusError) Error() string { return "blobnet: source answered " + http.StatusText(int(e)) }

// fetchRange gets range i of the blob from one device.
func fetchRange(ctx context.Context, sess *session.Session, hash string, i int, size int64) ([]byte, error) {
	off, n := blob.ChunkRange(i, size)
	ctx, cancel := context.WithTimeout(ctx, RangeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://peer"+Path+blob.Ref(hash), nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Range", "bytes="+strconv.FormatInt(off, 10)+"-"+strconv.FormatInt(off+n-1, 10))
	resp, err := sess.HTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusPartialContent {
		return nil, statusError(resp.StatusCode)
	}
	want := "bytes " + strconv.FormatInt(off, 10) + "-" + strconv.FormatInt(off+n-1, 10) + "/" + strconv.FormatInt(size, 10)
	if resp.Header.Get("Content-Range") != want {
		return nil, fmt.Errorf("blobnet: source sent range %q, want %q", resp.Header.Get("Content-Range"), want)
	}
	data := make([]byte, n)
	if _, err := io.ReadFull(resp.Body, data); err != nil {
		return nil, err
	}
	return data, nil
}

// Fetch makes the blob (ref: "sha256:<hex>" or hex) complete locally,
// fetching what is missing, and waits for it. hint, a person ID or "", is
// asked right after my own devices. It returns ErrNoSource when no online
// device that may serve it has the bytes, and a *blob.BadHashError
// (errors.Is blob.ErrBadHash) when they did not match the hash.
func (s *Service) Fetch(ctx context.Context, ref, hint string) error {
	hash, err := ParseHash(ref)
	if err != nil {
		return err
	}
	if s.cfg.Blobs.Has(hash) {
		return nil
	}
	j, err := s.job(hash, hint, true)
	if err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-j.done:
		return j.err
	}
}

// Get returns a reader of the blob (ref: "sha256:<hex>" or hex). If the blob
// is not complete locally, the reader fetches it as it is read: sources are
// my own online devices first, then hint's (a person ID, or ""), then any
// contact whose feed references it. A read waits only for the ranges it
// covers, which are fetched first, so a read in the middle (a seek in audio
// or video) does not wait for the start; once reading has begun the whole
// blob is fetched and cached. Get waits only until the size is known.
//
// ctx bounds Get and every read of the returned reader.
func (s *Service) Get(ctx context.Context, ref, hint string) (*Reader, error) {
	hash, err := ParseHash(ref)
	if err != nil {
		return nil, err
	}
	r := &Reader{s: s, ctx: ctx, hash: hash, hint: hint}
	if p, err := s.cfg.Blobs.Partial(hash, -1); err == nil && p.Complete() {
		r.p = p
		return r, nil
	}
	j, err := s.job(hash, hint, false)
	if err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-j.ready:
		r.p = j.partial()
		return r, nil
	case <-j.done:
		if p := j.partial(); p != nil && j.err == nil {
			r.p = p
			return r, nil
		}
		return nil, j.err
	}
}

// Reader reads one blob as far as it has arrived, waiting for (and asking
// for) the ranges it needs. It implements io.ReadSeeker and io.ReaderAt.
// Safe for concurrent ReadAt; Read and Seek share an offset.
type Reader struct {
	s    *Service
	ctx  context.Context
	hash string
	hint string
	p    *blob.Partial

	mu  sync.Mutex
	off int64
}

// Size returns the blob's size in bytes.
func (r *Reader) Size() int64 { return r.p.Size() }

// Complete reports whether the blob is now whole and verified locally.
func (r *Reader) Complete() bool { return r.p.Complete() }

// Read reads from the current offset, returning as soon as some bytes are
// there.
func (r *Reader) Read(b []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	n, err := r.read(b, r.off, false)
	r.off += int64(n)
	return n, err
}

// ReadAt fills b from off, waiting until every byte has arrived (or the
// blob ends, io.EOF).
func (r *Reader) ReadAt(b []byte, off int64) (int, error) { return r.read(b, off, true) }

// Seek sets the offset for Read.
func (r *Reader) Seek(offset int64, whence int) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += r.off
	case io.SeekEnd:
		offset += r.p.Size()
	default:
		return 0, errors.New("blobnet: bad whence")
	}
	if offset < 0 {
		return 0, errors.New("blobnet: negative offset")
	}
	r.off = offset
	return offset, nil
}

// Close releases the reader. The fetch it started goes on, so the blob is
// cached.
func (r *Reader) Close() error { return nil }

// read is Read (full false) and ReadAt (full true).
func (r *Reader) read(b []byte, off int64, full bool) (int, error) {
	total := 0
	var j *job
	for {
		changed := r.p.Changed() // before the read, so no arrival is missed
		n, err := r.p.ReadAt(b[total:], off+int64(total))
		total += n
		switch {
		case err == nil || err == io.EOF:
			return total, err
		case errors.Is(err, blob.ErrDiscarded):
			if j != nil && j.result() != nil {
				return total, j.err
			}
			return total, err
		case !errors.Is(err, blob.ErrAbsent):
			return total, err
		case !full && total > 0:
			return total, nil
		}
		if j != nil {
			select {
			case <-j.done: // the fetch ended and the bytes are still not here
				if j.err != nil && !errors.Is(j.err, errIdle) {
					return total, j.err
				}
			default:
			}
		}
		// Ask for the ranges still needed, first one first.
		from, to := off+int64(total), min(off+int64(len(b)), r.p.Size())
		var wants []int
		for i := int(from / blob.ChunkSize); int64(i)*blob.ChunkSize < to; i++ {
			if !r.p.Has(i) {
				wants = append(wants, i)
			}
		}
		var jerr error
		if j, jerr = r.s.job(r.hash, r.hint, true, wants...); jerr != nil {
			return total, jerr
		}
		if j.partial() != nil && j.partial() != r.p {
			return total, blob.ErrDiscarded // the fetch started over
		}
		select {
		case <-r.ctx.Done():
			return total, r.ctx.Err()
		case <-changed:
		case <-j.done:
		}
	}
}
