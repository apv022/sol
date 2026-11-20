package appapi

import (
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/apv022/sol/internal/blob"
	"github.com/apv022/sol/internal/blobnet"
	"github.com/apv022/sol/sdk/go/sol"
)

// blobErr maps blob errors onto API errors. "No source" is not_found: the permission rule
// makes a refusal and an absence look the same from here, on purpose.
func blobErr(err error) error {
	switch {
	case errors.Is(err, blobnet.ErrBadRef):
		return sol.Errorf("bad_request", "bad blob hash")
	case errors.Is(err, blobnet.ErrNoSource), errors.Is(err, blob.ErrNotFound):
		return sol.Errorf("not_found", "no such blob")
	case errors.Is(err, blob.ErrBadHash):
		return sol.Errorf("unavailable", "the bytes fetched did not match the hash; try again")
	case errors.Is(err, blobnet.ErrClosed):
		return sol.Errorf("unavailable", "shutting down")
	}
	return err
}

// putBlob serves PUT /v1/blobs. The Sol-Name hint is not stored yet (storage views are a
// later ticket); it never names the blob in the owner's file list.
func (s *Server) putBlob(q req, w http.ResponseWriter, r *http.Request) (any, error) {
	h, err := s.n.Blobs.Put(r.Body)
	if err != nil {
		return nil, sol.Errorf("bad_request", "upload failed: "+err.Error())
	}
	f, err := s.n.Blobs.Open(h)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	return sol.BlobInfo{Hash: blob.Ref(h), Size: st.Size()}, nil
}

// getBlob serves GET and HEAD /v1/blobs/{hash}?from=, with Range. A blob that is not local is
// fetched as it is read (blobnet.Service.Get); HEAD only learns its size.
func (s *Server) getBlob(q req, w http.ResponseWriter, r *http.Request) (any, error) {
	h, err := blobnet.ParseHash(r.PathValue("hash"))
	if err != nil {
		return nil, sol.Errorf("bad_request", "bad blob hash")
	}
	local := s.n.Blobs.Has(h)
	rd, err := s.n.Blobnet.Get(r.Context(), h, r.URL.Query().Get("from"))
	if err != nil {
		return nil, blobErr(err)
	}
	defer rd.Close()
	w.Header().Set("Sol-Local", map[bool]string{true: "1", false: "0"}[local])
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("ETag", `"`+blob.Ref(h)+`"`)
	if r.Method == http.MethodHead {
		w.Header().Set("Content-Length", strconv.FormatInt(rd.Size(), 10))
		w.Header().Set("Accept-Ranges", "bytes")
		w.WriteHeader(http.StatusOK)
		return nil, done
	}
	http.ServeContent(w, r, "", time.Time{}, readSeeker{rd})
	return nil, done
}

// readSeeker hides everything but io.ReadSeeker from http.ServeContent.
type readSeeker struct{ io.ReadSeeker }

func (s *Server) hashes(r *http.Request) ([]string, error) {
	var b struct {
		Hashes []string `json:"hashes"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(b.Hashes))
	for _, x := range b.Hashes {
		h, err := blobnet.ParseHash(x)
		if err != nil {
			return nil, sol.Errorf("bad_request", "bad blob hash "+x)
		}
		out = append(out, h)
	}
	return out, nil
}

// pin serves POST /v1/blobs/pin: only blobs this device holds can be pinned.
func (s *Server) pin(q req, w http.ResponseWriter, r *http.Request) (any, error) {
	hs, err := s.hashes(r)
	if err != nil {
		return nil, err
	}
	for _, h := range hs {
		if !s.n.Blobs.Has(h) {
			return nil, sol.Errorf("not_found", "blob not local: "+blob.Ref(h))
		}
	}
	return nil, blobErr(s.n.Blobnet.Pin(hs))
}

// unpin serves POST /v1/blobs/unpin.
func (s *Server) unpin(q req, w http.ResponseWriter, r *http.Request) (any, error) {
	hs, err := s.hashes(r)
	if err != nil {
		return nil, err
	}
	return nil, blobErr(s.n.Blobnet.Unpin(hs))
}
