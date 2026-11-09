// Package autostart makes the Sol node start with the computer: a systemd
// user unit on Linux, a LaunchAgent on macOS (`sol autostart on|off|status`).
//
// Invariants:
//   - Only per-user mechanisms. Nothing needs root, and the node runs as the
//     user who owns the data directory (the control socket relies on that).
//   - The unit runs the exact binary and data directory that enabled it,
//     so `sol autostart on` from a dev build or with --data does what it says.
//   - Status reads files only (it is called on every panel refresh) and never
//     runs a command.
//   - There is one entry per user. Enabling or disabling refuses to touch an
//     entry that runs a different data directory (ErrOther).
//   - The entry records PATH as it was when enabled, so the node finds tor
//     where the user's shell did (launchd's default PATH lacks Homebrew).
//   - A node that can't start is not restarted forever: the entry sets
//     SOL_SUPERVISED=1, and a supervised node that fails before it is up
//     (bad config, another node holding the data directory) exits 0, which
//     neither systemd (Restart=on-failure) nor launchd (KeepAlive on
//     unsuccessful exit) restarts. Crashes after start-up are restarted.
//   - Windows is not supported yet and says so (ErrNotImplemented).
package autostart

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"

	"github.com/apv022/sol/internal/config"
)

// ErrNotImplemented means this OS has no autostart support yet.
var ErrNotImplemented = errors.New("starting Sol at login isn't supported on this system yet; start `sol` yourself, or use the system's own startup settings")

// ErrOther means the start-at-login entry runs a different data directory.
var ErrOther = errors.New("start at login is set up for another data directory; turn it off there first")

// ErrGoRun means the binary is a temporary `go run` build.
var ErrGoRun = errors.New("this sol was started with `go run`; build it (`make build`) and run `bin/sol autostart on`")

// SupervisedEnv is set in the entry's environment; see the package doc.
const SupervisedEnv = "SOL_SUPERVISED"

// Label is the LaunchAgent label (the systemd unit is sol.service).
const (
	Label    = "dev.sol.node"
	unitName = "sol.service"
)

// Manager writes and removes the start-at-login entry for one binary and
// data directory.
type Manager struct {
	GOOS    string // runtime.GOOS
	Home    string // the user's home directory
	Exe     string // absolute path of the sol binary
	DataDir string
	UID     int    // os.Getuid(), for launchctl's gui/<uid> domain
	Path    string // PATH to record in the entry (the caller's PATH)
	// ConfigHome is $XDG_CONFIG_HOME ("" means ~/.config), where the
	// systemd user manager looks for units.
	ConfigHome string

	// Run executes a command (exec.Command(...).CombinedOutput by default);
	// tests replace it.
	Run func(name string, args ...string) ([]byte, error)
}

// State is what Status reports.
type State struct {
	Supported bool   `json:"supported"`
	Enabled   bool   `json:"enabled"`
	Path      string `json:"path,omitempty"` // the unit or plist file
	// Other is set when the entry exists but runs a different data
	// directory (Enabled is then false for this one).
	Other bool `json:"other,omitempty"`
}

func (m Manager) run(name string, args ...string) error {
	_, err := m.output(name, args...)
	return err
}

func (m Manager) output(name string, args ...string) (string, error) {
	run := m.Run
	if run == nil {
		run = func(name string, args ...string) ([]byte, error) { return exec.Command(name, args...).CombinedOutput() }
	}
	out, err := run(name, args...)
	if err != nil {
		return string(out), fmt.Errorf("%s %s: %v: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func (m Manager) userUnitDir() string {
	base := m.ConfigHome
	if base == "" {
		base = filepath.Join(m.Home, ".config")
	}
	return filepath.Join(base, "systemd", "user")
}

func (m Manager) unitPath() string { return filepath.Join(m.userUnitDir(), unitName) }

func (m Manager) wantsPath() string {
	return filepath.Join(m.userUnitDir(), "default.target.wants", unitName)
}

func (m Manager) plistPath() string {
	return filepath.Join(m.Home, "Library", "LaunchAgents", Label+".plist")
}

// Status reports whether the node starts at login. It only reads files.
func (m Manager) Status() State {
	switch m.GOOS {
	case "linux":
		_, err := os.Lstat(m.wantsPath())
		return m.state(m.unitPath(), err == nil, " --data "+systemdQuote(m.DataDir)+"\n")
	case "darwin":
		_, err := os.Stat(m.plistPath())
		return m.state(m.plistPath(), err == nil, "<string>"+xmlEscape(m.DataDir)+"</string>")
	}
	return State{}
}

// Enable writes the entry and registers it. With startNow, the node is also
// started right away (pass false when a node is already running for this
// data directory: a second one would fail to start).
func (m Manager) Enable(startNow bool) error {
	if m.Status().Other {
		return ErrOther
	}
	switch m.GOOS {
	case "linux":
		if err := writeFile(m.unitPath(), SystemdUnit(m.Exe, m.DataDir, m.Path)); err != nil {
			return err
		}
		if err := m.run("systemctl", "--user", "daemon-reload"); err != nil {
			return err
		}
		// Clears a start-limit hit from earlier failures, or --now can't start it.
		m.run("systemctl", "--user", "reset-failed", unitName)
		args := []string{"--user", "enable", unitName}
		if startNow {
			args = []string{"--user", "enable", "--now", unitName}
		}
		return m.run("systemctl", args...)
	case "darwin":
		if err := writeFile(m.plistPath(), LaunchAgentPlist(m.Exe, m.DataDir, m.Path)); err != nil {
			return err
		}
		if !startNow {
			return nil // launchd loads it at the next login
		}
		// bootout first so re-enabling picks up a changed plist; it fails
		// harmlessly when nothing is loaded.
		m.run("launchctl", "bootout", fmt.Sprintf("gui/%d/%s", m.UID, Label))
		return m.run("launchctl", "bootstrap", fmt.Sprintf("gui/%d", m.UID), m.plistPath())
	}
	return ErrNotImplemented
}

// Active reports whether the managed node's process is running right now
// (it runs a command, so it's for the CLI, not the panel).
func (m Manager) Active() bool {
	switch m.GOOS {
	case "linux":
		return m.run("systemctl", "--user", "is-active", "--quiet", unitName) == nil
	case "darwin":
		// Loaded isn't enough: the job must have a running process.
		out, err := m.output("launchctl", "print", fmt.Sprintf("gui/%d/%s", m.UID, Label))
		return err == nil && strings.Contains(out, "state = running")
	}
	return false
}

// Replace rewrites the entry and restarts the managed node with it (to pick
// up a new binary or data directory path). Each OS restarts it exactly once.
func (m Manager) Replace() error {
	switch m.GOOS {
	case "linux":
		if err := m.Enable(false); err != nil {
			return err
		}
		return m.run("systemctl", "--user", "restart", unitName)
	case "darwin":
		return m.Enable(true) // bootout + bootstrap is the restart
	}
	return ErrNotImplemented
}

// Disable stops the managed node (if it runs) and removes the entry.
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
		m.run("launchctl", "bootout", fmt.Sprintf("gui/%d/%s", m.UID, Label))
		if err := os.Remove(m.plistPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	return ErrNotImplemented
}

// state: an entry file that names another data directory is Other even
// when it isn't registered, so Enable/Disable never clobber it.
func (m Manager) state(path string, registered bool, mine string) State {
	st := State{Supported: true, Path: path}
	b, err := os.ReadFile(path)
	if err != nil {
		return st
	}
	if strings.Contains(string(b), mine) {
		st.Enabled = registered
	} else {
		st.Other = true
	}
	return st
}

func writeFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return config.WriteFileAtomic(path, []byte(content), 0o644)
}

// systemdQuote quotes one ExecStart argument (systemd.syntax: double quotes
// with backslash escapes; "%" is a specifier and must be doubled).
func systemdQuote(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "%", "%%", "$", "$$")
	return `"` + r.Replace(s) + `"`
}

// SystemdUnit is the user unit that runs the node. After 5 failed starts
// in 5 minutes systemd gives up (instead of retrying forever).
func SystemdUnit(exe, dataDir, path string) string {
	env := ""
	if path != "" {
		env = "Environment=" + systemdQuote("PATH="+path) + "\n"
	}
	// No network-online.target: it belongs to the system manager, not the
	// user one. Tor retries on its own and restarts on network changes.
	return `# Written by "sol autostart on". Remove with "sol autostart off".
[Unit]
Description=Sol node
StartLimitIntervalSec=300
StartLimitBurst=5

[Service]
Environment=` + SupervisedEnv + `=1
` + env + `ExecStart=` + systemdQuote(exe) + ` run --data ` + systemdQuote(dataDir) + `
Restart=on-failure
RestartSec=10

[Install]
WantedBy=default.target
`
}

func xmlEscape(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;").Replace(s)
}

// LaunchAgentPlist is the macOS LaunchAgent that runs the node.
func LaunchAgentPlist(exe, dataDir, path string) string {
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
		<string>--data</string>
		<string>` + xmlEscape(dataDir) + `</string>
	</array>
` + env + `	<key>RunAtLoad</key><true/>
	<key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
	<key>ThrottleInterval</key><integer>30</integer>
	<key>StandardErrorPath</key><string>` + xmlEscape(filepath.Join(dataDir, "sol.log")) + `</string>
</dict>
</plist>
`
}

// ForThisProcess returns a Manager for the running binary and dataDir.
// It refuses a `go run` binary, which lives in a temporary directory that is
// gone by the next login.
//
// The binary path is the one the user ran (a PATH lookup of os.Args[0]),
// not the symlink target: package managers point a stable symlink at a
// versioned directory that disappears on upgrade.
func ForThisProcess(goos, dataDir string) (Manager, error) {
	exe, err := invokedPath()
	if err != nil {
		return Manager{}, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return Manager{}, err
	}
	abs, err := filepath.Abs(dataDir)
	if err != nil {
		return Manager{}, err
	}
	m := Manager{GOOS: goos, Home: home, Exe: exe, DataDir: abs, UID: os.Getuid(),
		Path: os.Getenv("PATH"), ConfigHome: os.Getenv("XDG_CONFIG_HOME")}
	if strings.Contains(exe, string(filepath.Separator)+"go-build") {
		return m, ErrGoRun
	}
	return m, nil
}

// Lingering reports whether systemd keeps the current user's services
// running without a login session (so Sol starts at boot on a headless
// machine).
func Lingering() bool {
	u, err := user.Current()
	if err != nil || u.Username == "" {
		return false
	}
	_, err = os.Stat(filepath.Join("/var/lib/systemd/linger", u.Username))
	return err == nil
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
