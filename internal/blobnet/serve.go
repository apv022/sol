package blobnet

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/apv022/sol/internal/blob"
	"github.com/apv022/sol/internal/identity"
	"github.com/apv022/sol/internal/session"
)

// copyBuf is the read size when serving: Partial.ReadAt opens the file per
// call, so read in large steps.
const copyBuf = 256 << 10

// ServeHTTP serves GET and HEAD /p/blobs/{hash} to a verified session peer
// (format in the package comment).
func (s *Service) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	peer, ok := session.PeerFrom(r.Context())
	if !ok {
		w.WriteHeader(http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	hash, err := ParseHash(strings.TrimPrefix(r.URL.Path, Path))
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	if peer.Relation != session.Self && !s.cfg.Refs.MayFetch(identity.ID(peer.Person), hash) {
		s.log.Info("refused a blob request", "blob", short(hash), "peer", short(identity.ID(peer.Device)))
		w.WriteHeader(http.StatusForbidden)
		return
	}
	p, err := s.cfg.Blobs.Partial(hash, -1)
	if err != nil {
		w.WriteHeader(http.StatusNotFound)
		return
	}
	size := p.Size()
	h := w.Header()
	h.Set(HeaderSize, strconv.FormatInt(size, 10))
	h.Set(HeaderRanges, blob.FormatRanges(p.Ranges()))
	h.Set(HeaderComplete, strconv.FormatBool(p.Complete()))
	h.Set("Accept-Ranges", "bytes")
	h.Set("ETag", `"`+blob.Ref(hash)+`"`)
	if r.Method == http.MethodHead {
		h.Set("Content-Length", strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusOK)
		return
	}
	off, n, ranged, err := parseRange(r.Header.Get("Range"), size)
	if err != nil || (!ranged && !p.Complete()) || !p.HasRange(off, n) {
		h.Set("Content-Range", "bytes */"+strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusRequestedRangeNotSatisfiable)
		return
	}
	h.Set("Content-Length", strconv.FormatInt(n, 10))
	h.Set("Content-Type", "application/octet-stream")
	if ranged {
		h.Set("Content-Range", "bytes "+strconv.FormatInt(off, 10)+"-"+strconv.FormatInt(off+n-1, 10)+"/"+strconv.FormatInt(size, 10))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	// Hide ReadFrom so our buffer size is used.
	if _, err := io.CopyBuffer(struct{ io.Writer }{w}, io.NewSectionReader(p, off, n), make([]byte, copyBuf)); err != nil {
		s.log.Debug("serving a blob ended early", "blob", short(hash), "err", err)
	}
}

var errRange = errors.New("blobnet: bad range")

// parseRange parses a single-range Range header ("bytes=a-b", "bytes=a-",
// "bytes=-n") against a blob of size bytes. ranged is false when the header
// is empty (the whole blob). The result is never empty.
func parseRange(v string, size int64) (off, n int64, ranged bool, err error) {
	if v == "" {
		return 0, size, false, nil
	}
	spec, ok := strings.CutPrefix(v, "bytes=")
	if !ok || strings.Contains(spec, ",") {
		return 0, 0, true, errRange
	}
	a, b, ok := strings.Cut(strings.TrimSpace(spec), "-")
	if !ok {
		return 0, 0, true, errRange
	}
	if a == "" { // suffix: the last b bytes
		k, err := strconv.ParseInt(b, 10, 64)
		if err != nil || k <= 0 || size == 0 {
			return 0, 0, true, errRange
		}
		k = min(k, size)
		return size - k, k, true, nil
	}
	first, err := strconv.ParseInt(a, 10, 64)
	if err != nil || first < 0 || first >= size {
		return 0, 0, true, errRange
	}
	last := size - 1
	if b != "" {
		if last, err = strconv.ParseInt(b, 10, 64); err != nil || last < first {
			return 0, 0, true, errRange
		}
		last = min(last, size-1)
	}
	return first, last - first + 1, true, nil
}
