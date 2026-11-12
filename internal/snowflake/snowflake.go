// Package snowflake runs a Snowflake proxy, the opt-in way for a Sol node to
// give something back to the Tor network ("help_tor" in config.json).
//
// A Snowflake proxy lets people in censored countries reach Tor through this
// machine. It needs no open port, so it works behind the home routers and
// CGNAT that Sol is built for. It is not a Tor relay: Sol never runs one,
// because a relay's public IP and uptime could be matched against this
// node's onions.
//
// Invariants:
//   - Off unless the user turns it on. While on, the proxy contacts third
//     parties from this machine's IP address: the Tor Project's broker and
//     relay, and the STUN servers built into snowflake-proxy. The UI and
//     docs/THREAT_MODEL.md must say so.
//   - It is a separate process (the official snowflake-proxy program), with
//     no access to this node's tor, keys or config.
//   - Be honest about state: a missing program is "missing", never "running".
package snowflake

import (
	"context"
	"fmt"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/apv022/sol/internal/runtime"
)

const (
	// Program is the executable a runtime.Provider must supply.
	Program = "snowflake-proxy"
	// Capacity caps how many censored users are served at once, so a home
	// connection isn't swamped (the program's own default is unlimited).
	Capacity = 10
)

// State is what the proxy is doing.
type State string

const (
	StateOff     State = "off"
	StateRunning State = "running"
	StateMissing State = "missing" // turned on, but snowflake-proxy isn't installed
	StateFailed  State = "failed"  // it exited; Sol retries
)

// Status is a snapshot for the panel and the CLI.
type Status struct {
	Enabled bool   `json:"enabled"`
	State   State  `json:"state"`
	Detail  string `json:"detail,omitempty"`
}

// restart delays after the proxy exits. Variables so tests can shorten them.
var (
	restartMin = 30 * time.Second
	restartMax = 10 * time.Minute
)

// Helper owns the proxy process.
type Helper struct {
	// DataDir is the proxy's working directory (its log goes there).
	DataDir string
	// Provider finds the program; it is asked on every start, so installing
	// snowflake-proxy later needs no restart of Sol.
	Provider func() (runtime.Provider, error)

	mu     sync.Mutex
	cancel context.CancelFunc
	done   chan struct{}
	st     Status
}

// Status returns the current state.
func (h *Helper) Status() Status {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.st.State == "" {
		return Status{State: StateOff}
	}
	return h.st
}

func (h *Helper) set(state State, detail string) {
	h.mu.Lock()
	h.st = Status{Enabled: state != StateOff, State: state, Detail: detail}
	h.mu.Unlock()
}

// Start runs the proxy until Stop or until ctx ends, restarting it if it
// exits. Calling Start while it is running does nothing.
func (h *Helper) Start(ctx context.Context) {
	h.mu.Lock()
	if h.cancel != nil {
		h.mu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	h.cancel, h.done = cancel, make(chan struct{})
	h.st = Status{Enabled: true, State: StateMissing, Detail: "starting"}
	done := h.done
	h.mu.Unlock()
	go func() {
		defer close(done)
		h.run(ctx)
	}()
}

// Stop ends the proxy and waits for it to exit.
func (h *Helper) Stop() {
	h.mu.Lock()
	cancel, done := h.cancel, h.done
	h.cancel, h.done = nil, nil
	h.mu.Unlock()
	if cancel == nil {
		return
	}
	cancel()
	<-done
	h.set(StateOff, "")
}

func (h *Helper) run(ctx context.Context) {
	delay := restartMin
	for {
		started := time.Now()
		if proc := h.launch(ctx); proc != nil {
			select {
			case <-ctx.Done():
				proc.Stop()
				return
			case <-proc.Done():
				h.set(StateFailed, fmt.Sprintf("%s stopped (%v); see %s. Retrying.", Program, proc.Err(),
					filepath.Join(h.DataDir, Program+".log")))
			}
		}
		// A run that lasted a while was healthy: start the backoff over.
		if time.Since(started) > restartMax {
			delay = restartMin
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
		if delay *= 2; delay > restartMax {
			delay = restartMax
		}
	}
}

// launch starts the program once and records the resulting state. It
// returns nil if it could not be started.
func (h *Helper) launch(ctx context.Context) *runtime.Proc {
	prov, err := h.Provider()
	if err != nil {
		h.set(StateMissing, Program+" isn't installed. Install it (for example `sudo apt install snowflake-proxy`); Sol starts it by itself within a few minutes.")
		return nil
	}
	proc, err := prov.Start(ctx, runtime.Program{
		Name:    Program,
		DataDir: h.DataDir,
		// Never -unsafe-logging: the log must not hold other people's IPs.
		Args: []string{"-capacity", strconv.Itoa(Capacity), "-summary-interval", "1h"},
	})
	if err != nil {
		h.set(StateFailed, err.Error())
		return nil
	}
	h.set(StateRunning, fmt.Sprintf("serving up to %d censored users at a time", Capacity))
	return proc
}
