package appapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/apv022/sol/internal/feeds"
	"github.com/apv022/sol/sdk/go/sol"
)

// toSol converts a stored entry to the API shape (the JSON is the same; Sig is never exposed).
func toSol(e feeds.Entry) sol.Entry {
	return sol.Entry{Feed: e.Feed, Seq: e.Seq, Pos: e.Pos, Device: e.Device, Time: e.Time,
		Body: e.Body, Hash: e.Hash, Prev: e.Prev}
}

// feedErr maps store errors onto API errors.
func feedErr(err error) error {
	switch {
	case errors.Is(err, feeds.ErrInvalid), errors.Is(err, feeds.ErrNotMine):
		return sol.Errorf("bad_request", err.Error())
	case errors.Is(err, feeds.ErrNoPerson), errors.Is(err, feeds.ErrClosed):
		return sol.Errorf("unavailable", err.Error())
	}
	return err
}

// mayRead reports whether app q may read the feed at addr: the feed's app is q's own or one of
// its manifest's reads, and the feed is mine, shared with me by its owner, or the co-author feed
// of one I write-shared with its owner (API.md §5).
func (s *Server) mayRead(q req, addr string) bool {
	owner, app, name, err := feeds.ParseAddress(addr)
	if err != nil || app == feeds.InternalApp {
		return false
	}
	if app != q.app && !slices.Contains(q.m.Reads, app) {
		return false
	}
	return s.readable(owner, app, name)
}

// readable is the sharing half of mayRead.
func (s *Server) readable(owner, app, name string) bool {
	if me, _ := s.me(); owner == me {
		return true
	}
	return s.n.Shares.Incoming(owner, app, name) != "" || s.n.Shares.Mode(app, name, owner) == feeds.ModeWrite
}

// append serves POST /v1/feeds/{name}/append.
func (s *Server) append(q req, w http.ResponseWriter, r *http.Request) (any, error) {
	name, ok := strings.CutSuffix(r.PathValue("rest"), "/append")
	if !ok {
		return nil, sol.Errorf("not_found", "no such path")
	}
	if !feeds.ValidName(name) {
		return nil, sol.Errorf("bad_request", "bad feed name")
	}
	var b struct {
		Body json.RawMessage `json:"body"`
	}
	if err := decode(r, &b); err != nil {
		return nil, err
	}
	if len(b.Body) == 0 {
		return nil, sol.Errorf("bad_request", "missing body")
	}
	e, err := s.n.Store.Append(q.app, name, b.Body)
	if err != nil {
		return nil, feedErr(err)
	}
	return toSol(e), nil
}

// read serves GET /v1/feeds/{address}?from=&limit=.
func (s *Server) read(q req, w http.ResponseWriter, r *http.Request) (any, error) {
	addr := r.PathValue("rest")
	if _, _, _, err := feeds.ParseAddress(addr); err != nil {
		return nil, sol.Errorf("bad_request", "bad feed address")
	}
	if !s.mayRead(q, addr) {
		return nil, sol.Errorf("forbidden", "feed is not readable by this app")
	}
	from, _ := strconv.ParseUint(r.URL.Query().Get("from"), 10, 64)
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 {
		limit = 500
	}
	limit = min(limit, 5000)
	from = max(from, 1)
	es, err := s.n.Store.Read(addr, from, limit)
	if err != nil {
		return nil, feedErr(err)
	}
	out := make([]sol.Entry, 0, len(es))
	for _, e := range es {
		out = append(out, toSol(e))
	}
	return out, nil
}

// feedList serves GET /v1/feeds?owner=.
func (s *Server) feedList(q req, w http.ResponseWriter, r *http.Request) (any, error) {
	owner := r.URL.Query().Get("owner")
	out := []sol.FeedInfo{}
	for _, f := range s.n.Store.Feeds() {
		o, app, _, err := feeds.ParseAddress(f.Feed)
		if err != nil || app != q.app || (owner != "" && o != owner) || !s.mayRead(q, f.Feed) {
			continue
		}
		out = append(out, sol.FeedInfo{Feed: f.Feed, Pos: f.Pos})
	}
	return out, nil
}

// checkShare validates a share request: a feed name, a mode, and people who are my contacts.
func (s *Server) checkShare(r *http.Request) (sol.ShareRequest, error) {
	var b sol.ShareRequest
	if err := decode(r, &b); err != nil {
		return b, err
	}
	if !feeds.ValidName(b.Feed) {
		return b, sol.Errorf("bad_request", "bad feed name")
	}
	if b.Mode == "" {
		b.Mode = sol.ModeRead
	}
	if b.Mode != sol.ModeRead && b.Mode != sol.ModeWrite {
		return b, sol.Errorf("bad_request", "mode must be read or write")
	}
	for _, id := range b.People {
		if !s.isContact(id) {
			return b, sol.Errorf("not_found", "unknown person "+id)
		}
	}
	return b, nil
}

// share serves POST /v1/shares.
func (s *Server) share(q req, w http.ResponseWriter, r *http.Request) (any, error) {
	b, err := s.checkShare(r)
	if err != nil {
		return nil, err
	}
	if err := s.n.Shares.Share(q.app, b.Feed, b.People, b.Mode); err != nil {
		return nil, feedErr(err)
	}
	for _, id := range b.People {
		s.PersonChanged(id)
	}
	return nil, nil
}

// unshare serves DELETE /v1/shares.
func (s *Server) unshare(q req, w http.ResponseWriter, r *http.Request) (any, error) {
	b, err := s.checkShare(r)
	if err != nil {
		return nil, err
	}
	if err := s.n.Shares.Unshare(q.app, b.Feed, b.People); err != nil {
		return nil, feedErr(err)
	}
	for _, id := range b.People {
		s.PersonChanged(id)
	}
	return nil, nil
}

// shareList serves GET /v1/shares?feed=.
func (s *Server) shareList(q req, w http.ResponseWriter, r *http.Request) (any, error) {
	name := r.URL.Query().Get("feed")
	if !feeds.ValidName(name) {
		return nil, sol.Errorf("bad_request", "bad feed name")
	}
	out := []sol.Share{}
	for _, sh := range s.n.Shares.List(q.app, name) {
		out = append(out, sol.Share{Feed: name, Person: sh.Person, Mode: sh.Mode})
	}
	return out, nil
}
