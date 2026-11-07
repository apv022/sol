// Package runtime gets external programs (tor today, i2pd later) running as
// child processes and manages their lifecycle.
//
// Tor and i2pd are C/C++ programs, so Sol runs them rather than linking them.
// A Provider knows how to obtain and start a program. Providers are tried in
// the order from config (runtime.provider_order):
//
//	bundled  binaries embedded at build time with -tags bundled (release builds)
//	native   the program found on PATH (default for development: apt install tor)
//	docker   containers (stub; see docker.go)
//
// Every provider returns the same thing: a *Proc with a Done channel and a
// Stop method. Callers (transports) never care which provider was used.
// See docs/spec.md 6.2.1.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// ErrNotImplemented is returned by providers that are registered but not built.
var ErrNotImplemented = errors.New("not implemented yet")

// Program describes one process to run.
type Program struct {
	Name        string            // "tor" or "i2pd"
	DataDir     string            // working directory, created 0700
	ConfigFiles map[string]string // path relative to DataDir -> contents, written (0600) before start
	Args        []string
}

// Provider obtains and starts programs.
type Provider interface {
	Name() string
	// Available reports whether this provider can run program right now.
	Available(program string) bool
	// Start writes p.ConfigFiles and launches the program.
	Start(ctx context.Context, p Program) (*Proc, error)
}

// Options configures provider selection.
type Options struct {
	BinDir           string // where bundled binaries are extracted (~/.sol/bin)
	ContainerRuntime string // "docker" or "podman"
}

// Select returns the first provider in order that can run program.
func Select(order []string, program string, opts Options) (Provider, error) {
	var tried []string
	for _, name := range order {
		var p Provider
		switch name {
		case "bundled":
			p = &Bundled{BinDir: opts.BinDir}
		case "native":
			p = &Native{}
		case "docker":
			p = &Docker{Binary: opts.ContainerRuntime}
		default:
			return nil, fmt.Errorf("unknown provider %q", name)
		}
		if p.Available(program) {
			return p, nil
		}
		tried = append(tried, name)
	}
	return nil, fmt.Errorf("no provider can run %s (tried %v); install it, e.g. `sudo apt install %s`", program, tried, program)
}

// Proc is a running program.
type Proc struct {
	Program Program
	cmd     *exec.Cmd
	done    chan struct{}

	mu  sync.Mutex
	err error
}

// Done is closed when the process exits.
func (p *Proc) Done() <-chan struct{} { return p.done }

// Err returns the exit error; valid after Done is closed.
func (p *Proc) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.err
}

// PID returns the OS process ID.
func (p *Proc) PID() int { return p.cmd.Process.Pid }

// Stop asks the process to exit (SIGTERM / interrupt), then kills it if it
// is still running after 5 seconds.
func (p *Proc) Stop() error {
	select {
	case <-p.done:
		return nil
	default:
	}
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		// Windows has no SIGTERM; fall through to Kill.
		_ = p.cmd.Process.Kill()
	}
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		_ = p.cmd.Process.Kill()
		<-p.done
	}
	return nil
}

// Signal sends sig to the process (e.g. SIGHUP to make tor reload).
func (p *Proc) Signal(sig os.Signal) error { return p.cmd.Process.Signal(sig) }

// WriteConfigFiles creates p.DataDir (0700) and writes p.ConfigFiles (0600).
func WriteConfigFiles(p Program) error {
	if err := os.MkdirAll(p.DataDir, 0o700); err != nil {
		return err
	}
	for rel, contents := range p.ConfigFiles {
		path := filepath.Join(p.DataDir, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return err
		}
		if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
			return fmt.Errorf("writing %s: %w", path, err)
		}
	}
	return nil
}

// startBinary is shared by the bundled and native providers. Output goes to
// <DataDir>/<name>.log. The process is killed if ctx is cancelled.
func startBinary(ctx context.Context, path string, p Program) (*Proc, error) {
	if err := WriteConfigFiles(p); err != nil {
		return nil, err
	}
	logf, err := os.OpenFile(filepath.Join(p.DataDir, p.Name+".log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, path, p.Args...)
	cmd.Dir = p.DataDir
	cmd.Stdout = logf
	cmd.Stderr = logf
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		logf.Close()
		return nil, fmt.Errorf("starting %s: %w", p.Name, err)
	}
	proc := &Proc{Program: p, cmd: cmd, done: make(chan struct{})}
	go func() {
		err := cmd.Wait()
		logf.Close()
		proc.mu.Lock()
		proc.err = err
		proc.mu.Unlock()
		close(proc.done)
	}()
	return proc, nil
}
