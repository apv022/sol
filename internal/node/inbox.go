package node

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sync"
	"time"

	"github.com/apv022/sol/internal/blob"
	"github.com/apv022/sol/internal/config"
	"github.com/apv022/sol/internal/events"
	"github.com/apv022/sol/internal/peers"
)

// The _inbox service is HTTP spoken over one handshake-verified connection,
// so the handler always knows which peer is sending (docs/PEERS.md):
//
//	PUT /files/{name}   body = file bytes  ->  201 {"hash": "<sha256>", "size": n}
//
// Browsers can't be tricked into sending: an HTML form can't PUT, a
// cross-origin PUT needs a CORS preflight we never answer, and any request
// carrying an Origin header is refused outright.

// inboxHandler serves the inbox for one peer's connection.
func (n *Node) inboxHandler(p config.Peer) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "%s's inbox. Send it files with: sol send %q <file>\n", n.cfg.NodeName(), n.cfg.NodeName())
	})
	mux.HandleFunc("PUT /files/{name}", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Origin") != "" || r.Header.Get("Sec-Fetch-Site") != "" {
			http.Error(w, "browsers can't send to an inbox", http.StatusForbidden)
			return
		}
		name := blob.CleanName(r.PathValue("name"))
		if name == "" {
			http.Error(w, "missing file name", http.StatusBadRequest)
			return
		}
		received, _ := n.blobs.Received()
		room := n.opts.InboxMaxTotal - received
		limit := min(n.opts.InboxMaxFile, room)
		if limit <= 0 || (r.ContentLength > 0 && r.ContentLength > limit) {
			code, why := http.StatusRequestEntityTooLarge, fmt.Sprintf("larger than the %s per-file limit", humanBytes(n.opts.InboxMaxFile))
			if r.ContentLength > room || room <= 0 {
				code, why = http.StatusInsufficientStorage, fmt.Sprintf("the inbox is full (%s of files from peers)", humanBytes(n.opts.InboxMaxTotal))
			}
			http.Error(w, why, code)
			return
		}
		hash, err := n.blobs.PutLimited(r.Body, name, p.ID, limit)
		if errors.Is(err, blob.ErrTooLarge) {
			http.Error(w, "file is larger than the inbox accepts", http.StatusRequestEntityTooLarge)
			return
		}
		if err != nil {
			http.Error(w, "could not store the file", http.StatusInternalServerError)
			return
		}
		size := int64(0)
		if f, err := n.blobs.Open(hash); err == nil {
			if st, err := f.Stat(); err == nil {
				size = st.Size()
			}
			f.Close()
		}
		n.log.Add(events.KindFile, fmt.Sprintf("received %s (%s) from %s", name, humanBytes(size), peers.Describe(p)))
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{"hash": hash, "size": size})
	})
	return mux
}

// SendFile sends r to peer id's inbox under name and returns the SHA-256 of
// what the peer stored, after checking it matches what we sent.
func (n *Node) SendFile(ctx context.Context, id, name string, r io.Reader) (hash string, size int64, err error) {
	p, ok := n.peers.Get(id)
	if !ok {
		return "", 0, fmt.Errorf("peer %q: %w", id, config.ErrNotFound)
	}
	name = blob.CleanName(name)
	if name == "" {
		return "", 0, errors.New("the file needs a name")
	}
	conn, err := n.DialPeer(ctx, p.ID, peers.Inbox)
	if err != nil {
		return "", 0, err
	}
	defer conn.Close()

	h := sha256.New()
	counted := &countingReader{r: io.TeeReader(r, h)}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, "http://inbox/files/"+url.PathEscape(name), counted)
	if err != nil {
		return "", 0, err
	}
	resp, err := oneConnClient(conn).Do(req)
	if err != nil {
		return "", 0, fmt.Errorf("sending to %s: %w", p.Name, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", 0, fmt.Errorf("%s refused the file: %s", p.Name, trimSpace(string(msg)))
	}
	var got struct {
		Hash string `json:"hash"`
		Size int64  `json:"size"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&got); err != nil {
		return "", 0, fmt.Errorf("reading %s's answer: %w", p.Name, err)
	}
	want := hex.EncodeToString(h.Sum(nil))
	if got.Hash != want {
		return "", 0, fmt.Errorf("%s stored something different from what was sent (hash %s, sent %s)", p.Name, short(got.Hash), short(want))
	}
	n.log.Add(events.KindFile, fmt.Sprintf("sent %s (%s) to %s", name, humanBytes(counted.n), peers.Describe(p)))
	return want, counted.n, nil
}

type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	k, err := c.r.Read(p)
	c.n += int64(k)
	return k, err
}

// oneConnClient is an HTTP client whose only connection is conn.
func oneConnClient(conn net.Conn) *http.Client {
	var used sync.Once
	return &http.Client{Transport: &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			var c net.Conn
			used.Do(func() { c = conn })
			if c == nil {
				return nil, errors.New("connection already used")
			}
			return c, nil
		},
		DisableKeepAlives:     true,
		ResponseHeaderTimeout: 10 * time.Minute, // the peer hashes and stores before answering
	}}
}

// serveHTTPConn serves HTTP on one already-accepted connection and returns
// when that connection is closed.
func serveHTTPConn(c net.Conn, h http.Handler) {
	oc := &onceConn{Conn: c, closed: make(chan struct{})}
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 30 * time.Second, IdleTimeout: 2 * time.Minute}
	srv.Serve(&oneListener{c: oc})
	<-oc.closed
}

type onceConn struct {
	net.Conn
	once   sync.Once
	closed chan struct{}
}

func (c *onceConn) Close() error {
	c.once.Do(func() { close(c.closed) })
	return c.Conn.Close()
}

// oneListener hands out one connection, then blocks until it is closed.
type oneListener struct {
	c    *onceConn
	used bool
	mu   sync.Mutex
}

func (l *oneListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	first := !l.used
	l.used = true
	l.mu.Unlock()
	if first {
		return l.c, nil
	}
	<-l.c.closed
	return nil, net.ErrClosed
}

func (l *oneListener) Close() error   { return nil }
func (l *oneListener) Addr() net.Addr { return l.c.LocalAddr() }

func humanBytes(n int64) string {
	switch {
	case n < 1<<10:
		return fmt.Sprintf("%d B", n)
	case n < 1<<20:
		return fmt.Sprintf("%.1f KB", float64(n)/(1<<10))
	case n < 1<<30:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	default:
		return fmt.Sprintf("%.2f GB", float64(n)/(1<<30))
	}
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12] + "…"
	}
	return h
}

func trimSpace(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	return s
}
