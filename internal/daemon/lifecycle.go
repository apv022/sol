package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/apv022/sol/internal/identity"
)

// ErrRunning means a daemon already answers on this data directory. It doubles as the
// single-instance lock: nothing else is needed because the admin socket is what a second
// daemon would have to take over.
var ErrRunning = errors.New("daemon: sol is already running on this directory")

// DefaultDir is where a device keeps its data: $SOL_DIR, else the platform's per-user data
// directory (~/.local/share/sol or $XDG_DATA_HOME/sol, ~/Library/Application Support/Sol,
// %LOCALAPPDATA%\Sol). It is never ~/.sol, which belongs to the old proof of concept.
func DefaultDir() string {
	home, _ := os.UserHomeDir()
	return dirFor(runtime.GOOS, os.Getenv, home)
}

// dirFor is DefaultDir with its inputs passed in, for tests.
func dirFor(goos string, env func(string) string, home string) string {
	if d := env("SOL_DIR"); d != "" {
		return d
	}
	switch goos {
	case "darwin":
		return filepath.Join(home, "Library", "Application Support", "Sol")
	case "windows":
		if d := env("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "Sol")
		}
		return filepath.Join(home, "AppData", "Local", "Sol")
	}
	if d := env("XDG_DATA_HOME"); d != "" && filepath.IsAbs(d) {
		return filepath.Join(d, "sol")
	}
	return filepath.Join(home, ".local", "share", "sol")
}

// Initialised reports whether dir holds a Sol identity (Init or Restore ran there).
func Initialised(dir string) bool {
	_, err := identity.Open(keysDir(dir))
	return err == nil
}

// Running asks the daemon on dir for its status. ErrNotRunning (or a dial error) means none
// answers, including when files of a dead daemon are still there.
func Running(dir string) (Status, error) {
	var st Status
	c, err := Connect(dir)
	if err != nil {
		return st, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err = c.Do(ctx, "GET", "/admin/v1/status", nil, &st)
	return st, err
}

// WaitUp waits until a daemon on dir answers, until exited closes (the process we started
// died) or until timeout. It wakes on changes to dir (the daemon writes api.url and
// admin.token when it is ready) and otherwise backs off from 20 ms to 500 ms, so there is
// no fixed sleep and no busy loop on systems without a file watcher.
func WaitUp(dir string, exited <-chan struct{}, timeout time.Duration) (Status, error) {
	return wait(dir, exited, timeout, func() (Status, bool) {
		st, err := Running(dir)
		return st, err == nil
	})
}

// WaitDown waits until no daemon answers on dir, or timeout.
func WaitDown(dir string, timeout time.Duration) error {
	_, err := wait(dir, nil, timeout, func() (Status, bool) {
		_, err := Running(dir)
		return Status{}, err != nil
	})
	return err
}

// ErrWaitTimeout is returned by WaitUp and WaitDown when time ran out.
var ErrWaitTimeout = errors.New("daemon: timed out")

// ErrExited is returned by WaitUp when the process being waited for exited first.
var ErrExited = errors.New("daemon: the process exited")

func wait(dir string, exited <-chan struct{}, timeout time.Duration, done func() (Status, bool)) (Status, error) {
	events, stop, err := watchDirs(dir)
	if err == nil {
		defer stop()
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	backoff := 20 * time.Millisecond
	for {
		if st, ok := done(); ok {
			return st, nil
		}
		t := time.NewTimer(backoff)
		select {
		case <-events:
		case <-t.C:
			if backoff < 500*time.Millisecond {
				backoff *= 2
			}
		case <-exited:
			t.Stop()
			return Status{}, ErrExited
		case <-deadline.C:
			t.Stop()
			return Status{}, ErrWaitTimeout
		}
		t.Stop()
	}
}

// RequestShutdown asks the process that owns the daemon to stop it; `sol run` waits on
// ShutdownRequested. It is safe to call more than once.
func (d *Daemon) RequestShutdown() { d.shutdownOnce.Do(func() { close(d.shutdown) }) }

// ShutdownRequested is closed once an admin client asked the daemon to stop (sol stop).
func (d *Daemon) ShutdownRequested() <-chan struct{} { return d.shutdown }

// requestReset records "Remove Sol from this device" and asks for a shutdown. It returns the
// setup token the next setup server must use (the browser gets it in the reply).
func (d *Daemon) requestReset() string {
	d.mu.Lock()
	if d.resetToken == "" {
		d.resetToken = NewSetupToken()
	}
	t := d.resetToken
	d.mu.Unlock()
	d.RequestShutdown()
	return t
}

// ResetRequested reports whether the shutdown was a "Remove Sol from this device": then the
// owner of the process archives the identity (ArchiveIdentity) and runs setup with token.
func (d *Daemon) ResetRequested() (token string, ok bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.resetToken, d.resetToken != ""
}
