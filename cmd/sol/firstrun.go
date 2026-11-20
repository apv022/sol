package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/user"
	"runtime"
	"strings"

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

// firstRun is what plain `sol` does when dir holds no identity.
func firstRun(dir string) error {
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
