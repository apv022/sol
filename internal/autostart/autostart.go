// Package autostart makes the Sol daemon start with the computer: a systemd user unit on
// Linux, a LaunchAgent on macOS (`sol autostart on|off|status`).
//
// Invariants:
//   - Only per-user mechanisms. Nothing needs root; the daemon runs as the user who owns
//     the data directory.
//   - The entry runs the exact binary and data directory that enabled it (`sol run --dir`).
//   - Status reads files only and never runs a command.
//   - There is one entry per user. Enabling or disabling refuses to touch an entry that
//     runs a different data directory (ErrOther).
//   - The entry records PATH as it was when enabled (launchd's default lacks Homebrew).
//   - A daemon that finds another one on its directory exits 0 when SOL_SUPERVISED=1, which
//     neither systemd (Restart=on-failure) nor launchd (KeepAlive on failure) restarts, so
//     a node that can never start is not restarted forever.
//   - Linux means a systemd user manager; other init systems report ErrNotImplemented.
//   - Windows is not implemented and says so (ErrNotImplemented).
package autostart

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	pathpkg "path"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/apv022/sol/internal/fsutil"
)

// ErrNotImplemented means this system has no autostart support yet.
var ErrNotImplemented = errors.New("not_implemented: starting Sol at login is not supported on this system yet; use the system's own startup settings to run `sol run`")

// ErrOther means the entry runs a different data directory.
var ErrOther = errors.New("start at login is set up for another data directory; turn it off there first")

// ErrGoRun means the binary is a temporary `go run` or test build.
var ErrGoRun = errors.New("this sol is a temporary build (go run); build it (`make build`) and run `bin/sol autostart on`")

// SupervisedEnv is set in the entry's environment; see the package comment.
const SupervisedEnv = "SOL_SUPERVISED"

// Label is the LaunchAgent label; the systemd unit is sol.service.
const (
	Label    = "dev.sol.node"
	unitName = "sol.service"
)

// Manager writes and removes the entry for one binary and data directory.
type Manager struct {
	GOOS           string // runtime.GOOS
	Home           string // the user's home directory
	Exe            string // absolute path of the sol binary
	Dir            string // the data directory
	UID            int    // for launchctl's gui/<uid> domain
	Path           string // PATH to record in the entry
	ConfigHome     string // $XDG_CONFIG_HOME ("" means ~/.config)
	SystemdRunning bool   // systemd is the init system

	// Run executes a command; tests replace it.
	Run func(name string, args ...string) ([]byte, error)
}

// State is what Status reports.
type State struct {
	Supported bool   `json:"supported"`
	Enabled   bool   `json:"enabled"`
	Path      string `json:"path,omitempty"` // the unit or plist file
	// Other is set when the entry exists but runs a different data directory.
	Other bool `json:"other,omitempty"`
}

func (m Manager) run(name string, args ...string) error {
	run := m.Run
	if run == nil {
		run = func(name string, args ...string) ([]byte, error) { return exec.Command(name, args...).CombinedOutput() }
	}
	if out, err := run(name, args...); err != nil {
		return fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (m Manager) unitPath() string {
	base := m.ConfigHome
	if base == "" {
		base = filepath.Join(m.Home, ".config")
	}
	return filepath.Join(base, "systemd", "user", unitName)
}

func (m Manager) plistPath() string {
	return filepath.Join(m.Home, "Library", "LaunchAgents", Label+".plist")
}

// execLine is the line of the unit that names the data directory; Status compares it.
func (m Manager) execLine() string { return " run --dir " + systemdQuote(m.Dir) + "\n" }

// Status reports whether the daemon starts at login. It only reads files.
func (m Manager) Status() State {
	switch m.GOOS {
	case "linux":
		if !m.SystemdRunning {
			return State{}
		}
		return m.state(m.unitPath(), m.execLine())
	case "darwin":
		return m.state(m.plistPath(), "<string>"+xmlEscape(m.Dir)+"</string>")
	}
	return State{}
}

// state: an entry naming another data directory is Other even when not registered, so
// Enable and Disable never clobber it.
func (m Manager) state(path, mine string) State {
	st := State{Supported: true, Path: path}
	b, err := os.ReadFile(path)
	if err != nil {
		return st
	}
	if strings.Contains(string(b), mine) {
		st.Enabled = true
	} else {
		st.Other = true
	}
	return st
}

// Enable writes the entry and registers it; with startNow the daemon also starts at once
// (pass false when one already runs on the directory).
func (m Manager) Enable(startNow bool) error {
	if m.Status().Other {
		return ErrOther
	}
	switch m.GOOS {
	case "linux":
		if !m.SystemdRunning {
			return ErrNotImplemented
		}
		if err := writeFile(m.unitPath(), SystemdUnit(m.Exe, m.Dir, m.Path)); err != nil {
			return err
		}
		if err := m.run("systemctl", "--user", "daemon-reload"); err != nil {
			return err
		}
		_ = m.run("systemctl", "--user", "reset-failed", unitName) // a start-limit hit blocks --now
		args := []string{"--user", "enable", unitName}
		if startNow {
			args = []string{"--user", "enable", "--now", unitName}
		}
		return m.run("systemctl", args...)
	case "darwin":
		if err := os.MkdirAll(m.Dir, 0o700); err != nil { // launchd needs the log's directory
			return err
		}
		if err := writeFile(m.plistPath(), LaunchAgentPlist(m.Exe, m.Dir, m.Path)); err != nil {
			return err
		}
		if !startNow {
			return nil // launchd loads it at the next login
		}
		_ = m.run("launchctl", "bootout", fmt.Sprintf("gui/%d/%s", m.UID, Label)) // picks up a changed plist
		return m.run("launchctl", "bootstrap", fmt.Sprintf("gui/%d", m.UID), m.plistPath())
	}
	return ErrNotImplemented
}

// Disable stops the managed daemon (if it runs) and removes the entry.
func (m Manager) Disable() error {
	if m.Status().Other {
		return ErrOther
	}
	switch m.GOOS {
	case "linux":
		if _, err := os.Stat(m.unitPath()); errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err := m.run("systemctl", "--user", "disable", "--now", unitName); err != nil {
			return err
		}
		if err := os.Remove(m.unitPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return m.run("systemctl", "--user", "daemon-reload")
	case "darwin":
		_ = m.run("launchctl", "bootout", fmt.Sprintf("gui/%d/%s", m.UID, Label))
		if err := os.Remove(m.plistPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	return ErrNotImplemented
}

func writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(path, []byte(content), 0o644)
}

// systemdQuote quotes one ExecStart argument (double quotes with backslash escapes; "%" and
// "$" are specifiers and are doubled).
func systemdQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "$", "$$").Replace(s) + `"`
}

// envQuote quotes an Environment= assignment; "$" stays as is there.
func envQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%").Replace(s) + `"`
}

// SystemdUnit is the user unit that runs the daemon. After 5 failed starts in 5 minutes
// systemd gives up instead of retrying forever.
func SystemdUnit(exe, dir, path string) string {
	env := ""
	if path != "" {
		env = "Environment=" + envQuote("PATH="+path) + "\n"
	}
	return `# Written by "sol autostart on". Remove with "sol autostart off".
[Unit]
Description=Sol
StartLimitIntervalSec=300
StartLimitBurst=5

[Service]
Environment=` + SupervisedEnv + `=1
` + env + `ExecStart=` + systemdQuote(exe) + ` run --dir ` + systemdQuote(dir) + `
Restart=on-failure
RestartSec=10

[Install]
WantedBy=default.target
`
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;").Replace(s)
}

// LaunchAgentPlist is the macOS LaunchAgent that runs the daemon.
func LaunchAgentPlist(exe, dir, path string) string {
	env := ""
	if path != "" {
		env = "<key>PATH</key><string>" + xmlEscape(path) + "</string>"
	}
	env = "\t<key>EnvironmentVariables</key><dict><key>" + SupervisedEnv + "</key><string>1</string>" + env + "</dict>\n"
	return `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<!-- Written by "sol autostart on". Remove with "sol autostart off". -->
<plist version="1.0">
<dict>
	<key>Label</key><string>` + Label + `</string>
	<key>ProgramArguments</key>
	<array>
		<string>` + xmlEscape(exe) + `</string>
		<string>run</string>
		<string>--dir</string>
		<string>` + xmlEscape(dir) + `</string>
	</array>
` + env + `	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
	<key>ThrottleInterval</key><integer>30</integer>
	<key>StandardErrorPath</key><string>` + xmlEscape(pathpkg.Join(dir, "sol.log")) + `</string>
</dict>
</plist>
`
}

// ForThisProcess returns a Manager for the running binary and dir. It refuses a `go run`
// or test binary: those live in temporary directories gone by the next login. The binary
// path is the one the user ran, not the symlink target, so package-manager symlinks survive
// upgrades.
func ForThisProcess(goos, dir string) (Manager, error) {
	exe, err := invokedPath()
	if err != nil {
		return Manager{}, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return Manager{}, err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Manager{}, err
	}
	m := Manager{GOOS: goos, Home: home, Exe: exe, Dir: abs, UID: os.Getuid(),
		Path: os.Getenv("PATH"), ConfigHome: os.Getenv("XDG_CONFIG_HOME")}
	if fi, err := os.Stat("/run/systemd/system"); err == nil && fi.IsDir() { // sd_booted(3)
		m.SystemdRunning = true
	}
	if isGoRunBinary(exe) {
		return m, ErrGoRun
	}
	return m, nil
}

var goRunPath = regexp.MustCompile(`(^|/)go-build[0-9]+/b[0-9]+/exe/[^/]+$`)

func isGoRunBinary(exe string) bool {
	base := filepath.Base(exe)
	if goRunPath.MatchString(filepath.ToSlash(exe)) || strings.HasSuffix(base, ".test") || strings.HasSuffix(base, ".test.exe") {
		return true
	}
	cache := os.Getenv("GOCACHE")
	if cache == "" {
		if d, err := os.UserCacheDir(); err == nil {
			cache = filepath.Join(d, "go-build")
		}
	}
	return cache != "" && cache != "off" && strings.HasPrefix(exe, filepath.Clean(cache)+string(filepath.Separator))
}

func invokedPath() (string, error) {
	if len(os.Args) > 0 && os.Args[0] != "" {
		arg := os.Args[0]
		if !strings.ContainsRune(arg, filepath.Separator) {
			if p, err := exec.LookPath(arg); err == nil {
				arg = p
			}
		}
		if abs, err := filepath.Abs(arg); err == nil {
			if fi, err := os.Stat(abs); err == nil && !fi.IsDir() {
				return abs, nil
			}
		}
	}
	return os.Executable()
}
