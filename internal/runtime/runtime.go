// Package runtime gets external programs (tor today, i2pd later) running as
// child processes and manages their lifecycle.
//
// Tor and i2pd are C/C++ programs, so Sol runs them rather than linking them.
// A Provider knows how to obtain and start a program. The daemon tries them for tor in
// this order (Select; the first that can run it wins):
//
//	platform a file the platform ships and has already verified: SOL_TOR_BIN, else the
//	         config's tor_bin (`sol init --tor-bin <path>`). Android sets SOL_TOR_BIN to
//	         the APK's lib/<abi>/libtor.so (the APK is signed, so no checksum here).
//	         Named but missing is an error, not a fallback.
//	bundled  binaries embedded at build time with -tags bundled (desktop release builds,
//	         `make build-bundled`: tor 0.4.9.16 from the Tor Browser 16.0.1 expert bundles
//	         and its shared libraries, fetched and signature-checked by
//	         scripts/fetch-tor.sh). Extracted into <data dir>/bin/<os>-<arch>/, every file
//	         checked against its SHA-256 pinned in bin/checksums.json (an unpinned or
//	         changed file refuses the whole bundle), and run with that folder on
//	         LD_LIBRARY_PATH (DYLD_LIBRARY_PATH on macOS). If it can't run, the next
//	         provider that can (native) runs instead, and the reason is logged.
//	native   the program found on PATH (development: apt install tor), or the file named
//	         by SOL_BIN_<NAME>
//	docker   containers (stub; see docker.go)
//
// Whichever provider runs a program logs, at its first start, the file and its version
// (`<file> --version`, first line), so a log always says which tor ran.
//
// Every provider returns the same thing: a *Proc with a Done channel and a
// Stop method. Callers (transports) never care which provider was used.
package runtime

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
	// Platform names program files the platform provides (PlatformPaths), for the
	// "platform" provider.
	Platform map[string]string
	// Log is where providers log (nil: slog's default).
	Log *slog.Logger
}

// TorProviders is the order the daemon tries for tor (see the package comment).
var TorProviders = []string{"platform", "bundled", "native"}

// Select returns the first provider in order that can run program. A bundled provider falls
// back, at Start, to the next one in order that can run it (a tor on PATH).
func Select(order []string, program string, opts Options) (Provider, error) {
	var tried []string
	for i, name := range order {
		p, err := provider(name, opts)
		if err != nil {
			return nil, err
		}
		if !p.Available(program) {
			if path := opts.Platform[program]; name == "platform" && path != "" {
				return nil, fmt.Errorf("the platform's %s is set to %s (%s or tor_bin), which is not a program file", program, path, TorBinEnv)
			}
			tried = append(tried, name)
			continue
		}
		if b, ok := p.(*Bundled); ok {
			for _, next := range order[i+1:] {
				if f, err := provider(next, opts); err == nil && f.Available(program) {
					b.Fallback = f
					break
				}
			}
		}
		return p, nil
	}
	return nil, fmt.Errorf("no provider can run %s (tried %v); install it, e.g. `sudo apt install %s`", program, tried, program)
}

func provider(name string, opts Options) (Provider, error) {
	switch name {
	case "platform":
		return &Platform{Paths: opts.Platform, Log: opts.Log}, nil
	case "bundled":
		return &Bundled{BinDir: opts.BinDir, Log: opts.Log}, nil
	case "native":
		return &Native{Log: opts.Log}, nil
	case "docker":
		return &Docker{Binary: opts.ContainerRuntime}, nil
	}
	return nil, fmt.Errorf("unknown provider %q", name)
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

// Kill ends the process at once, without asking. For a process that has
// stopped answering (a frozen tor): Stop would wait 5 seconds for it first.
func (p *Proc) Kill() {
	select {
	case <-p.done:
		return
	default:
	}
	_ = p.cmd.Process.Kill()
	<-p.done
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
	return startBinaryEnv(ctx, path, p, nil)
}

// startBinaryEnv is startBinary with extra environment variables for the
// child ("KEY=value", added to this process's environment).
func startBinaryEnv(ctx context.Context, path string, p Program, env []string) (*Proc, error) {
	if err := WriteConfigFiles(p); err != nil {
		return nil, err
	}
	logf, err := os.OpenFile(filepath.Join(p.DataDir, p.Name+".log"), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, path, p.Args...)
	cmd.Dir = p.DataDir
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
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
