package sol

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// Caller is who sent a request the daemon forwarded to the app, from the Sol-* headers.
// Apps decide by this, never by anything in a request body.
type Caller struct {
	Person string
	Device string
	// Agent is the agent key id; empty for humans.
	Agent string
	// Self is true for my own devices and agents.
	Self bool
}

// IsAgent reports whether the caller is an agent key.
func (c Caller) IsAgent() bool { return c.Agent != "" }

// callerOf parses the caller headers of r.
func callerOf(r *http.Request) Caller {
	return Caller{
		Person: r.Header.Get("Sol-Person"),
		Device: r.Header.Get("Sol-Device"),
		Agent:  r.Header.Get("Sol-Agent"),
		Self:   r.Header.Get("Sol-Self") == "1",
	}
}

type ctxKey struct{}

// FromContext returns the API that Serve attached to a request context (nil if none).
func FromContext(ctx context.Context) API {
	a, _ := ctx.Value(ctxKey{}).(API)
	return a
}

// Serve listens on SOL_APP_LISTEN and serves h until ctx ends. Each request's context carries
// api (see FromContext). A unix:// address has its stale socket removed first.
func Serve(ctx context.Context, api API, h http.Handler) error {
	addr := os.Getenv("SOL_APP_LISTEN")
	if addr == "" {
		return errors.New("sol: SOL_APP_LISTEN is not set")
	}
	network, a := "tcp", addr
	if p, ok := strings.CutPrefix(addr, "unix://"); ok {
		network, a = "unix", p
		_ = os.Remove(a)
	}
	ln, err := net.Listen(network, a)
	if err != nil {
		return err
	}
	return ServeListener(ctx, ln, api, h)
}

// ServeListener is Serve on an existing listener.
func ServeListener(ctx context.Context, ln net.Listener, api API, h http.Handler) error {
	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			h.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, api)))
		}),
		ReadHeaderTimeout: 10 * time.Second,
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			sc, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			_ = srv.Shutdown(sc)
			_ = srv.Close()
		case <-done:
		}
	}()
	err := srv.Serve(ln)
	close(done)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// CallerOf parses the caller headers of r: Sol-Person, Sol-Device, Sol-Agent, Sol-Self.
func CallerOf(r *http.Request) Caller { return callerOf(r) }

// AcceptStream answers a dialed stream (API.md §7). The daemon hands a peer's Dial to the app as
// an upgrade request ("Upgrade: sol-stream") on the app's socket at the dialed path; the app
// calls AcceptStream from its handler, which replies 101 Switching Protocols and returns the raw
// connection and the verified caller. Anything else is refused with bad_request.
func AcceptStream(w http.ResponseWriter, r *http.Request) (net.Conn, Caller, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "sol-stream") {
		err := Errorf("bad_request", "not a sol-stream upgrade")
		WriteError(w, err)
		return nil, Caller{}, err
	}
	conn, rw, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return nil, Caller{}, err
	}
	if _, err := conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: sol-stream\r\n\r\n")); err != nil {
		conn.Close()
		return nil, Caller{}, err
	}
	_ = conn.SetDeadline(time.Time{})
	if rw.Reader.Buffered() > 0 {
		return &bufConn{Conn: conn, r: rw.Reader}, callerOf(r), nil
	}
	return conn, callerOf(r), nil
}

// IsStream reports whether r is a dialed stream (an "Upgrade: sol-stream" request).
func IsStream(r *http.Request) bool { return strings.EqualFold(r.Header.Get("Upgrade"), "sol-stream") }
