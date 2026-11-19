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
