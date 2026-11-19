package memsol

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/apv022/sol/sdk/go/sol"
)

// Server serves a Network over HTTP with the paths of API.md. Each request is authenticated by
// the bearer token of a Client (see Client.Token). Use it with httptest.NewServer.
type Server struct {
	n   *Network
	mux *http.ServeMux
}

// NewServer returns the HTTP front end of n.
func NewServer(n *Network) *Server {
	s := &Server{n: n, mux: http.NewServeMux()}
	r := s.route
	r("GET /v1/whoami", func(c *Client, w http.ResponseWriter, q *http.Request) (any, error) { return c.WhoAmI(q.Context()) })
	r("GET /v1/people", func(c *Client, w http.ResponseWriter, q *http.Request) (any, error) { return c.People(q.Context()) })
	r("GET /v1/devices", func(c *Client, w http.ResponseWriter, q *http.Request) (any, error) { return c.Devices(q.Context()) })
	r("POST /v1/feeds/{rest...}", func(c *Client, w http.ResponseWriter, q *http.Request) (any, error) {
		name, ok := strings.CutSuffix(q.PathValue("rest"), "/append")
		if !ok {
			return nil, sol.Errorf("not_found", "no such path")
		}
		var b struct {
			Body json.RawMessage `json:"body"`
		}
		if err := decode(q, &b); err != nil {
			return nil, err
		}
		if len(b.Body) == 0 {
			return nil, sol.Errorf("bad_request", "missing body")
		}
		return c.Append(q.Context(), name, b.Body)
	})
	r("GET /v1/feeds/{rest...}", func(c *Client, w http.ResponseWriter, q *http.Request) (any, error) {
		from, _ := strconv.ParseUint(q.URL.Query().Get("from"), 10, 64)
		limit, _ := strconv.Atoi(q.URL.Query().Get("limit"))
		return c.Read(q.Context(), q.PathValue("rest"), from, limit)
	})
	r("GET /v1/feeds", func(c *Client, w http.ResponseWriter, q *http.Request) (any, error) {
		return c.Feeds(q.Context(), q.URL.Query().Get("owner"))
	})
	r("POST /v1/blobs/pin", s.hashes(func(c *Client, q *http.Request, h []string) error { return c.Pin(q.Context(), h) }))
	r("POST /v1/blobs/unpin", s.hashes(func(c *Client, q *http.Request, h []string) error { return c.Unpin(q.Context(), h) }))
	r("PUT /v1/blobs", func(c *Client, w http.ResponseWriter, q *http.Request) (any, error) {
		return c.PutBlob(q.Context(), q.Body, q.Header.Get("Sol-Name"))
	})
	r("GET /v1/blobs/{hash}", s.blob)
	r("HEAD /v1/blobs/{hash}", s.blob)
	r("POST /v1/shares", func(c *Client, w http.ResponseWriter, q *http.Request) (any, error) {
		var b sol.ShareRequest
		if err := decode(q, &b); err != nil {
			return nil, err
		}
		return nil, c.Share(q.Context(), b)
	})
	r("DELETE /v1/shares", func(c *Client, w http.ResponseWriter, q *http.Request) (any, error) {
		var b sol.ShareRequest
		if err := decode(q, &b); err != nil {
			return nil, err
		}
		return nil, c.Unshare(q.Context(), b)
	})
	r("GET /v1/shares", func(c *Client, w http.ResponseWriter, q *http.Request) (any, error) {
		return c.Shares(q.Context(), q.URL.Query().Get("feed"))
	})
	r("POST /v1/actions/call", func(c *Client, w http.ResponseWriter, q *http.Request) (any, error) {
		var b sol.ActionCall
		if err := decode(q, &b); err != nil {
			return nil, err
		}
		res, err := c.CallAction(q.Context(), b)
		if err != nil {
			return nil, err
		}
		if res.Approval != "" {
			return map[string]string{"approval": res.Approval}, nil
		}
		if len(res.Output) == 0 {
			return json.RawMessage("{}"), nil
		}
		return res.Output, nil
	})
	r("GET /v1/actions", func(c *Client, w http.ResponseWriter, q *http.Request) (any, error) {
		return c.Actions(q.Context(), q.URL.Query().Get("app"))
	})
	r("POST /v1/inbox", func(c *Client, w http.ResponseWriter, q *http.Request) (any, error) {
		var b sol.InboxPost
		if err := decode(q, &b); err != nil {
			return nil, err
		}
		return c.PostInbox(q.Context(), b)
	})
	r("GET /v1/activity", func(c *Client, w http.ResponseWriter, q *http.Request) (any, error) {
		limit, _ := strconv.Atoi(q.URL.Query().Get("limit"))
		return c.Activity(q.Context(), limit)
	})
	s.mux.HandleFunc("GET /v1/events", s.auth(s.events))
	s.mux.HandleFunc("POST /v1/dial", s.auth(s.dial))
	return s
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

type handler func(c *Client, w http.ResponseWriter, r *http.Request) (any, error)

func (s *Server) auth(f func(*Client, http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		s.n.mu.Lock()
		c := s.n.tokens[tok]
		s.n.mu.Unlock()
		if tok == "" || c == nil {
			sol.WriteError(w, sol.Errorf("unauthorized", "bad token"))
			return
		}
		f(c, w, r)
	}
}

func (s *Server) route(pattern string, h handler) {
	s.mux.HandleFunc(pattern, s.auth(func(c *Client, w http.ResponseWriter, r *http.Request) {
		out, err := h(c, w, r)
		if err != nil {
			sol.WriteError(w, err)
			return
		}
		if w.Header().Get("Content-Type") != "" || r.Method == "HEAD" {
			return // handler wrote the response itself
		}
		if out == nil {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		sol.WriteJSON(w, 200, out)
	}))
}

func decode(r *http.Request, v any) error {
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(v); err != nil {
		return sol.Errorf("bad_request", "bad JSON: "+err.Error())
	}
	return nil
}

func (s *Server) hashes(f func(*Client, *http.Request, []string) error) handler {
	return func(c *Client, w http.ResponseWriter, r *http.Request) (any, error) {
		var b struct {
			Hashes []string `json:"hashes"`
		}
		if err := decode(r, &b); err != nil {
			return nil, err
		}
		return nil, f(c, r, b.Hashes)
	}
}

// blob serves GET and HEAD, with Range support via http.ServeContent.
func (s *Server) blob(c *Client, w http.ResponseWriter, r *http.Request) (any, error) {
	hash := r.PathValue("hash")
	from := r.URL.Query().Get("from")
	st, err := c.StatBlob(r.Context(), hash, sol.BlobOptions{From: from})
	if err != nil {
		return nil, err
	}
	if st.Local {
		w.Header().Set("Sol-Local", "1")
	} else {
		w.Header().Set("Sol-Local", "0")
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	rc, err := c.GetBlob(r.Context(), hash, sol.BlobOptions{From: from})
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	data, _ := io.ReadAll(rc)
	http.ServeContent(w, r, "", time.Time{}, strings.NewReader(string(data)))
	return nil, nil
}

func (s *Server) events(c *Client, w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	o := sol.EventOptions{People: q.Get("people") == "1", Inbox: q.Get("inbox") == "1", LastEventID: r.Header.Get("Last-Event-ID")}
	if f := q.Get("feeds"); f != "" {
		o.Feeds = strings.Split(f, ",")
	}
	ch, err := c.Events(r.Context(), o)
	if err != nil {
		sol.WriteError(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	rc := http.NewResponseController(w)
	fmt.Fprint(w, ": ok\n\n")
	_ = rc.Flush()
	for ev := range ch {
		if sol.EncodeEvent(w, ev) != nil {
			return
		}
		_ = rc.Flush()
	}
}

func (s *Server) dial(c *Client, w http.ResponseWriter, r *http.Request) {
	var req sol.DialRequest
	if err := decode(r, &req); err != nil {
		sol.WriteError(w, err)
		return
	}
	pipe, err := c.Dial(r.Context(), req)
	if err != nil {
		sol.WriteError(w, err)
		return
	}
	conn, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		pipe.Close()
		return
	}
	_, _ = conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: sol-stream\r\n\r\n"))
	go func() {
		_, _ = io.Copy(pipe, rw.Reader)
		pipe.Close()
	}()
	_, _ = io.Copy(conn, pipe)
	conn.Close()
}
