package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/apv022/sol/internal/apphost"
	"github.com/apv022/sol/internal/autostart"
	"github.com/apv022/sol/internal/builtins"
	"github.com/apv022/sol/internal/daemon"
	"github.com/apv022/sol/sdk/go/sol"
)

// This file is the background-daemon part of the CLI: `sol` and `sol start` run `sol run`
// detached and return, `sol stop` and `sol restart` use the admin API, `sol logs` shows the
// log, `sol autostart` manages the login entry. The admin socket is the single-instance lock
// (daemon.Start refuses a second daemon on one directory).

const (
	logName     = "sol.log"
	logMaxBytes = 10 << 20 // sol.log moves to sol.log.1 when a start finds it this large
	startWait   = 30 * time.Second
	stopWait    = 10 * time.Second
)

func logPath(dir string) string { return filepath.Join(dir, logName) }

// startCmd is `sol` and `sol start`. It reports a running daemon, or starts one and waits
// until it answers; it does not need the daemon's ready line, the admin socket appearing is
// the signal (daemon.WaitUp).
func startCmd(dir string) error {
	if !daemon.Initialised(dir) {
		return firstRun(dir)
	}
	st, already, err := startDaemon(dir)
	if err != nil {
		return err
	}
	if already {
		fmt.Printf("Sol is already running.\n  UI:   %s\n  Data: %s\n  Open it with `sol open`.\n", st.UI, dir)
		return nil
	}
	fmt.Printf("Sol is running in the background.\n  UI:   %s\n  Data: %s\n  Open it with `sol open`. Logs: `sol logs -f`. Stop: `sol stop`.\n", st.UI, dir)
	return nil
}

// startDaemon makes sure a daemon answers on an initialised dir; already reports that one
// was running (or won a race to start).
func startDaemon(dir string) (st daemon.Status, already bool, err error) {
	if st, err := daemon.Running(dir); err == nil {
		return st, true, nil
	}
	exited, err := spawn(dir)
	if err != nil {
		return st, false, err
	}
	st, err = daemon.WaitUp(dir, exited, startWait)
	switch {
	case errors.Is(err, daemon.ErrExited):
		// A racing start may have won; then somebody answers now.
		if st, err2 := daemon.Running(dir); err2 == nil {
			return st, true, nil
		}
		return st, false, fmt.Errorf("the daemon stopped right after starting; its last lines (%s):\n%s", logPath(dir), tail(logPath(dir), 8))
	case err != nil:
		return st, false, fmt.Errorf("the daemon did not answer within %s; see %s", startWait, logPath(dir))
	}
	return st, false, nil
}

// spawn starts `sol run --dir dir` detached, output to <dir>/sol.log. The returned channel
// closes when the child exits.
func spawn(dir string) (<-chan struct{}, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	rotate(logPath(dir))
	// Stale files of a dead daemon would make WaitUp's first answer a lie only if a socket
	// answered, which it cannot; but removing the token makes the file watcher fire on the new one.
	_ = os.Remove(filepath.Join(dir, "admin.token"))
	logf, err := os.OpenFile(logPath(dir), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	defer logf.Close() // the child holds its own copy
	cmd := exec.Command(exe, "run", "--dir", dir)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, logf, logf
	detach(cmd)
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting the daemon: %w", err)
	}
	exited := make(chan struct{})
	go func() { _ = cmd.Wait(); close(exited) }()
	return exited, nil
}

// rotate keeps one old log: a failure only means a longer file.
func rotate(path string) {
	if fi, err := os.Stat(path); err == nil && fi.Size() > logMaxBytes {
		_ = os.Rename(path, path+".1")
	}
}

// stopCmd is `sol stop`: ask the daemon to shut down, wait for it to go, and fall back to
// SIGTERM (a hard kill on Windows) if it does not.
func stopCmd(dir string, quiet bool) error {
	st, err := daemon.Running(dir)
	if err != nil && stopSetup(dir) {
		if !quiet {
			fmt.Println("Sol stopped (it was waiting to be set up).")
		}
		return nil
	}
	if err != nil {
		if !quiet {
			fmt.Println("Sol is not running.")
		}
		return nil
	}
	if c, err := daemon.Connect(dir); err == nil {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		// The daemon may drop the connection while closing; waiting for it to go is the check.
		_ = c.Do(ctx, "POST", "/admin/v1/shutdown", nil, nil)
		cancel()
	}
	if err := daemon.WaitDown(dir, stopWait); err != nil {
		if st.PID == 0 {
			return fmt.Errorf("the daemon did not stop within %s", stopWait)
		}
		fmt.Fprintln(os.Stderr, "The daemon is slow to stop; sending it a termination signal.")
		if err := terminate(st.PID); err != nil {
			return err
		}
		if err := daemon.WaitDown(dir, stopWait); err != nil {
			return fmt.Errorf("the daemon (pid %d) is still running after %s", st.PID, 2*stopWait)
		}
	}
	if !quiet {
		fmt.Println("Sol stopped.")
	}
	return nil
}

// restartCmd is `sol restart`.
func restartCmd(dir string) error {
	if err := stopCmd(dir, true); err != nil {
		return err
	}
	return startCmd(dir)
}

// ensureRunning starts the daemon if it is not running (sol open).
func ensureRunning(dir string) error {
	if _, err := daemon.Running(dir); err == nil {
		return nil
	}
	if !daemon.Initialised(dir) {
		return fmt.Errorf("Sol is not set up in %s yet; run `sol init --name YOURNAME`", dir)
	}
	return startCmd(dir)
}

// logsCmd is `sol logs [-f] [-n N]`.
func logsCmd(dir string, args []string) error {
	fs := flag.NewFlagSet("sol logs", flag.ContinueOnError)
	follow := fs.Bool("f", false, "keep printing new lines (Ctrl-C to stop)")
	n := fs.Int("n", 40, "how many of the last lines to print")
	d := fs.String("dir", dir, "data directory")
	if err := fs.Parse(args); err != nil {
		return err
	}
	path := logPath(*d)
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("no log yet at %s (`sol start` writes one; `sol run` prints to the terminal)", path)
	}
	fmt.Print(tail(path, *n))
	if !*follow {
		return nil
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	return followLog(ctx, path, os.Stdout)
}

// followLog copies what is appended to path to w until ctx ends.
func followLog(ctx context.Context, path string, w io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	off, err := f.Seek(0, io.SeekEnd)
	if err != nil {
		return err
	}
	buf := make([]byte, 32<<10)
	for {
		n, err := f.Read(buf)
		if n > 0 {
			off += int64(n)
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
			continue
		}
		if err != nil && err != io.EOF {
			return err
		}
		if !waitGrow(ctx, path, off) {
			return nil
		}
	}
}

// sleepGrow is the fallback wait: a short sleep at EOF. It returns false when ctx ended.
func sleepGrow(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(300 * time.Millisecond):
		return true
	}
}

// tail returns the last n lines of the file at path ("" if unreadable).
func tail(path string, n int) string {
	f, err := os.Open(path)
	if err != nil || n <= 0 {
		return ""
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() > 256<<10 { // enough for any sane n
		_, _ = f.Seek(-256<<10, io.SeekEnd)
	}
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		lines = append(lines, sc.Text())
		if len(lines) > n {
			lines = lines[1:]
		}
	}
	out := ""
	for _, l := range lines {
		out += l + "\n"
	}
	return out
}

// autostartCmd is `sol autostart on|off|status`.
func autostartCmd(dir string, args []string, asJSON bool) error {
	if len(args) != 1 || (args[0] != "on" && args[0] != "off" && args[0] != "status") {
		return errors.New("usage: sol autostart on|off|status")
	}
	m, err := autostart.ForThisProcess(runtime.GOOS, dir)
	if err != nil && !(args[0] == "status" && errors.Is(err, autostart.ErrGoRun)) {
		return err
	}
	switch args[0] {
	case "on":
		_, running := daemon.Running(dir)
		if err := m.Enable(running != nil); err != nil {
			return err
		}
		fmt.Println("Sol will start when you log in.")
		if err := launcherOn(dir); err != nil && !errors.Is(err, autostart.ErrLauncherNotImplemented) {
			fmt.Fprintln(os.Stderr, "note: no app-menu entry:", err)
		}
	case "off":
		if err := m.Disable(); err != nil {
			return err
		}
		fmt.Println("Sol will no longer start when you log in.")
		if err := launcherOff(dir); err != nil && !errors.Is(err, autostart.ErrLauncherNotImplemented) {
			fmt.Fprintln(os.Stderr, "note: could not remove the app-menu entry:", err)
		}
	default:
		st := m.Status()
		if asJSON {
			return printJSON(st)
		}
		switch {
		case !st.Supported:
			fmt.Println("not_implemented: starting at login is not supported on this system yet")
		case st.Other:
			fmt.Printf("set up for another data directory (%s)\n", st.Path)
		case st.Enabled:
			fmt.Printf("on (%s)\n", st.Path)
		default:
			fmt.Println("off")
		}
	}
	return nil
}

func launcherOn(dir string) error {
	l, err := autostart.LauncherForThisProcess(runtime.GOOS, dir)
	if err != nil {
		return err
	}
	// The built-in apps get their entries at once; a running Sol brings the rest (and keeps all
	// in step) through daemon.Options.OnApps.
	l.Apps = launcherApps(builtinMenuApps())
	return l.Enable()
}

// builtinMenuApps are the first-party apps that have a UI, with their icons.
func builtinMenuApps() []apphost.MenuApp {
	var out []apphost.MenuApp
	for _, b := range builtins.All() {
		m, err := sol.ParseManifest(b.Manifest)
		if err != nil || m.UI == "" {
			continue
		}
		icon, _ := fs.ReadFile(b.Icons, "icon-512.png")
		out = append(out, apphost.MenuApp{ID: m.ID, Name: m.Name, Icon: icon})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func launcherApps(in []apphost.MenuApp) []autostart.App {
	out := make([]autostart.App, 0, len(in))
	for _, a := range in {
		out = append(out, autostart.App{ID: a.ID, Name: a.Name, Icon: a.Icon})
	}
	return out
}

// syncLauncher is daemon.Options.OnApps: when the app-menu entry is on, it makes the per-app
// entries match the installed apps. Quiet on failure: the menu is a convenience.
func syncLauncher(dir string, apps []apphost.MenuApp) {
	l, err := autostart.LauncherForThisProcess(runtime.GOOS, dir)
	if err != nil || runtime.GOOS != "linux" || !l.Enabled() {
		return
	}
	_ = l.SyncApps(launcherApps(apps))
}

func launcherOff(dir string) error {
	l, err := autostart.LauncherForThisProcess(runtime.GOOS, dir)
	if err != nil && !errors.Is(err, autostart.ErrGoRun) {
		return err
	}
	return l.Disable()
}

// launcherCmd is `sol launcher on|off|status`: the "Sol" entry in the app menu (Linux).
// `sol autostart on|off` manage it too.
func launcherCmd(dir string, args []string) error {
	if len(args) != 1 || (args[0] != "on" && args[0] != "off" && args[0] != "status") {
		return errors.New("usage: sol launcher on|off|status")
	}
	switch args[0] {
	case "on":
		if err := launcherOn(dir); err != nil {
			return err
		}
		fmt.Println("Sol is in your app menu.")
	case "off":
		if err := launcherOff(dir); err != nil {
			return err
		}
		fmt.Println("Sol is no longer in your app menu.")
	default:
		l, err := autostart.LauncherForThisProcess(runtime.GOOS, dir)
		switch {
		case runtime.GOOS != "linux":
			fmt.Println(autostart.ErrLauncherNotImplemented.Error())
		case err != nil && !errors.Is(err, autostart.ErrGoRun):
			return err
		case l.Enabled():
			fmt.Printf("on (%s)\n", l.DesktopPath())
		default:
			fmt.Println("off")
		}
	}
	return nil
}

// stopSetup ends a setup server waiting on dir (no daemon runs there yet); false if none does.
func stopSetup(dir string) bool {
	url, ok := runningSetup(dir)
	if !ok {
		return false
	}
	base, q, _ := strings.Cut(url, "?setup=")
	req, err := http.NewRequest("POST", strings.TrimSuffix(base, "/")+"/setup/v1/stop", nil)
	if err != nil {
		return false
	}
	req.Header.Set(daemon.SetupHeader, q)
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return false
	}
	resp.Body.Close()
	for i := 0; i < 50; i++ { // the server is gone once setup.url is removed
		if _, err := os.Stat(daemon.SetupURLFile(dir)); err != nil {
			return true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return resp.StatusCode == http.StatusOK
}
