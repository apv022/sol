package channels

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/apv022/sol/internal/identity"
)

// The wire surface of a channel, served on its public onion:
//
//	GET /channel          {"channel": "<C_pub ID>", "records": ["<base64 wire form>", ...]}
//	                      the latest record of every app, by app id
//	GET /channel/stream   text/event-stream: an "release" event per record, every current one
//	                      first, then each new one as it is published; a comment line every
//	                      KeepAlive so idle Tor circuits stay up
//	GET /blob/{sha256}    the artifact bytes (application/octet-stream)
//
// Everything on it is public: records are signed and artifacts are named by hash, so the server
// needs no login and a follower trusts none of it before checking.

// KeepAlive is how often an idle stream carries a comment line. A variable for tests.
var KeepAlive = 30 * time.Second

// Listing is the reply of GET /channel.
type Listing struct {
	Channel string   `json:"channel"`
	Records []string `json:"records"`
}

// Server serves one hosted channel. Safe for concurrent use.
type Server struct {
	channel string
	blobs   *Blobs

	mu      sync.Mutex
	records map[string]identity.Release // by app id
	subs    map[chan []byte]struct{}
}

// NewServer serves channel's records (initial, already verified) and blobs.
func NewServer(channel string, blobs *Blobs, initial []identity.Release) *Server {
	s := &Server{channel: channel, blobs: blobs, records: map[string]identity.Release{}, subs: map[chan []byte]struct{}{}}
	for _, r := range initial {
		s.records[r.App] = r
	}
	return s
}

// Publish makes r the app's current record and pushes it to every open stream. The caller has
// verified it and checked it is newer.
func (s *Server) Publish(r identity.Release) {
	raw := r.Marshal()
	s.mu.Lock()
	s.records[r.App] = r
	for c := range s.subs {
		select {
		case c <- raw:
		default: // a reader that stopped reading: it reconnects and gets everything anew
			close(c)
			delete(s.subs, c)
		}
	}
	s.mu.Unlock()
}

// Close ends every open stream.
func (s *Server) Close() {
	s.mu.Lock()
	for c := range s.subs {
		close(c)
		delete(s.subs, c)
	}
	s.mu.Unlock()
}

func (s *Server) current() [][]byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	apps := make([]string, 0, len(s.records))
	for a := range s.records {
		apps = append(apps, a)
	}
	slices.Sort(apps)
	out := make([][]byte, 0, len(apps))
	for _, a := range apps {
		out = append(out, s.records[a].Marshal())
	}
	return out
}

// ServeHTTP implements http.Handler.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	switch {
	case r.Method != http.MethodGet && r.Method != http.MethodHead:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	case r.URL.Path == "/channel":
		l := Listing{Channel: s.channel, Records: []string{}}
		for _, b := range s.current() {
			l.Records = append(l.Records, base64.StdEncoding.EncodeToString(b))
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(l)
	case r.URL.Path == "/channel/stream":
		s.stream(w, r)
	case strings.HasPrefix(r.URL.Path, "/blob/"):
		h := strings.TrimPrefix(r.URL.Path, "/blob/")
		f, err := s.blobs.Open(h)
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer f.Close()
		w.Header().Set("Content-Type", "application/octet-stream")
		http.ServeContent(w, r, "", time.Time{}, f)
	default:
		http.NotFound(w, r)
	}
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	c := make(chan []byte, 16)
	s.mu.Lock()
	s.subs[c] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		if _, ok := s.subs[c]; ok {
			delete(s.subs, c)
			close(c)
		}
		s.mu.Unlock()
	}()
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	send := func(b []byte) error {
		_, err := fmt.Fprintf(w, "event: release\ndata: %s\n\n", base64.StdEncoding.EncodeToString(b))
		fl.Flush()
		return err
	}
	// Records published between subscribing and here arrive twice; followers ignore repeats.
	for _, b := range s.current() {
		if send(b) != nil {
			return
		}
	}
	if _, err := io.WriteString(w, ": ready\n\n"); err != nil {
		return
	}
	fl.Flush()
	tick := time.NewTicker(KeepAlive)
	defer tick.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case b, ok := <-c:
			if !ok || send(b) != nil {
				return
			}
		case <-tick.C:
			if _, err := io.WriteString(w, ": keepalive\n\n"); err != nil {
				return
			}
			fl.Flush()
		}
	}
}

// MaxEventSize bounds one line of a channel stream: a record's base64 and its prefix.
const MaxEventSize = identity.MaxReleaseSize*4/3 + 1024

// Stream reads a channel's event stream at base (http://<onion>) with client and calls onRecord
// with each record's wire form, until ctx ends or the stream breaks (the error says why). ready
// is called once the server has sent every current record, so a caller can reset its backoff.
// onRecord gets bytes from the network: it must verify them.
func Stream(ctx context.Context, client *http.Client, base string, ready func(), onRecord func([]byte)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(base, "/")+"/channel/stream", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "text/event-stream")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("channels: the channel answered %s", resp.Status)
	}
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 4096), MaxEventSize)
	event := ""
	for sc.Scan() {
		line := sc.Text()
		switch {
		case line == ": ready":
			if ready != nil {
				ready()
			}
		case strings.HasPrefix(line, "event: "):
			event = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: ") && event == "release":
			if b, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(line, "data: ")); err == nil {
				onRecord(b)
			}
		case line == "":
			event = ""
		}
	}
	if err := sc.Err(); err != nil {
		return err
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return errors.New("channels: the stream ended")
}
