package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/user"
	"runtime"
	"strings"
	"time"

	"github.com/apv022/sol/internal/autostart"
	"github.com/apv022/sol/internal/builtins"
	"github.com/apv022/sol/internal/daemon"
)

// The first run: plain `sol` on a machine with no data directory sets Sol up with defaults
// (the person is named after the OS account, the device after the host), starts it in the
// background and opens the Sol app already signed in. `sol init` stays for explicit setups
// (a chosen name, --join, ports, transports).

// accountName is the OS account's full name (GECOS on Unix), else the login name.
func accountName() string {
	if u, err := user.Current(); err == nil {
		if n, _, _ := strings.Cut(u.Name, ","); strings.TrimSpace(n) != "" {
			return strings.TrimSpace(n)
		}
		if n := lastPart(u.Username); n != "" {
			return n
		}
	}
	if n := os.Getenv("USER"); n != "" {
		return n
	}
	return "Me"
}

// lastPart drops a Windows DOMAIN\ prefix.
func lastPart(s string) string {
	if i := strings.LastIndexByte(s, '\\'); i >= 0 {
		s = s[i+1:]
	}
	return s
}

// canOpenBrowser: false over SSH and, on Linux, without a display.
func canOpenBrowser() bool {
	if os.Getenv("SSH_CONNECTION") != "" || os.Getenv("SSH_TTY") != "" {
		return false
	}
	if runtime.GOOS == "linux" && os.Getenv("DISPLAY") == "" && os.Getenv("WAYLAND_DISPLAY") == "" {
		return false
	}
	return true
}

// defaultUI is the app UI address of a fresh setup: the well-known port, or any free one
// when something else holds it.
func defaultUI() string {
	const want = "127.0.0.1:7777"
	l, err := net.Listen("tcp", want)
	if err != nil {
		return "127.0.0.1:0"
	}
	_ = l.Close()
	return want
}

// firstRun is what plain `sol` does when dir holds no identity. With a browser at hand it
// starts the daemon in setup mode and opens the setup page (Start new, Add to my Sol, Restore
// from backup words); without one (SSH, a server) it sets up a new Sol with defaults.
func firstRun(dir string) error {
	if canOpenBrowser() && os.Getenv("SOL_AUTO_INIT") != "1" {
		return browserSetup(dir)
	}
	name := accountName()
	if err := daemon.Init(dir, daemon.InitOptions{Name: name, Device: hostname(),
		Config: daemon.Config{UI: defaultUI()}, Builtins: builtins.All()}); err != nil {
		return err
	}
	if _, _, err := startDaemon(dir); err != nil {
		return err
	}
	// The app-menu entry is a convenience: a failure (or a temporary go-run binary) is not one.
	menu := false
	if err := launcherOn(dir); err == nil {
		menu = true
	} else if !errors.Is(err, autostart.ErrLauncherNotImplemented) && !errors.Is(err, autostart.ErrGoRun) {
		fmt.Fprintln(os.Stderr, "note: no app-menu entry:", err)
	}
	fmt.Printf("Welcome to Sol, %s.\n  Your data lives in %s\n", name, dir)
	c, err := daemon.Connect(dir)
	if err != nil {
		return err
	}
	if canOpenBrowser() {
		fmt.Println("  Opening Sol in your browser...")
	} else {
		fmt.Println("  No display here; open this link in a browser within a minute:")
	}
	if err := openUI(context.Background(), c, !canOpenBrowser()); err != nil {
		return err
	}
	fmt.Println("  Open it again: `sol open`")
	if menu {
		fmt.Println("                 or pick Sol from your app menu")
	}
	fmt.Println("  Stop it:       `sol stop`      Start at login: `sol autostart on`")
	return nil
}

// browserSetup starts `sol run` on a directory without identity (it serves the setup page) and
// opens that page. A setup already waiting is just opened again.
func browserSetup(dir string) error {
	if url, ok := runningSetup(dir); ok {
		fmt.Println("Sol is waiting to be set up in your browser.")
		if !openBrowser(url) {
			fmt.Println("  Open:", url)
		}
		return nil
	}
	if err := daemon.PrepareSetup(dir, daemon.Config{UI: defaultUI()}, builtins.All()); err != nil {
		return err
	}
	_ = os.Remove(daemon.SetupURLFile(dir))
	exited, err := spawn(dir)
	if err != nil {
		return err
	}
	// Wait for the setup server to write its URL (a few hundred ms), or the child to fail.
	deadline := time.Now().Add(startWait)
	for {
		if url, ok := runningSetup(dir); ok {
			if menuErr := launcherOn(dir); menuErr != nil && !errors.Is(menuErr, autostart.ErrLauncherNotImplemented) && !errors.Is(menuErr, autostart.ErrGoRun) {
				fmt.Fprintln(os.Stderr, "note: no app-menu entry:", menuErr)
			}
			fmt.Println("Welcome to Sol. Finish setting up in your browser:")
			fmt.Println("  start new, add this computer to your Sol, or restore from your backup words.")
			if !openBrowser(url) {
				fmt.Println("  Open:", url)
			}
			return nil
		}
		select {
		case <-exited:
			return fmt.Errorf("Sol stopped right after starting; its last lines (%s):\n%s", logPath(dir), tail(logPath(dir), 8))
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("Sol's setup did not start within %s; see %s", startWait, logPath(dir))
		}
	}
}

// runningSetup returns the setup page's URL when a setup server answers on dir.
func runningSetup(dir string) (string, bool) {
	b, err := os.ReadFile(daemon.SetupURLFile(dir))
	if err != nil {
		return "", false
	}
	url := strings.TrimSpace(string(b))
	base, _, _ := strings.Cut(url, "?")
	c := http.Client{Timeout: 2 * time.Second}
	resp, err := c.Get(base)
	if err != nil {
		return "", false
	}
	resp.Body.Close()
	return url, resp.StatusCode == http.StatusOK
}
