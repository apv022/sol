package autostart

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type recorder struct{ calls []string }

func (r *recorder) run(name string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, name+" "+strings.Join(args, " "))
	return nil, nil
}

func TestSystemdUnitQuotes(t *testing.T) {
	u := SystemdUnit(`/opt/my sol/sol`, `/home/a "b"/100%/$x`, "/opt/homebrew/bin:/usr/bin")
	want := `ExecStart="/opt/my sol/sol" run --data "/home/a \"b\"/100%%/$$x"`
	if !strings.Contains(u, want+"\n") {
		t.Fatalf("unit:\n%s\nwant line %s", u, want)
	}
	for _, line := range []string{"Restart=on-failure", "WantedBy=default.target", "StartLimitBurst=5",
		`Environment="PATH=/opt/homebrew/bin:/usr/bin"`, "Environment=SOL_SUPERVISED=1"} {
		if !strings.Contains(u, line) {
			t.Fatalf("unit lacks %q", line)
		}
	}
}

func TestPlistEscapes(t *testing.T) {
	p := LaunchAgentPlist("/Apps/S&L/sol", "/Users/a/<sol>", "/opt/homebrew/bin:/usr/bin")
	for _, want := range []string{"<string>/Apps/S&amp;L/sol</string>", "<string>/Users/a/&lt;sol&gt;</string>", "<key>RunAtLoad</key><true/>",
		"<key>PATH</key><string>/opt/homebrew/bin:/usr/bin</string>", "<key>SOL_SUPERVISED</key><string>1</string>"} {
		if !strings.Contains(p, want) {
			t.Fatalf("plist lacks %q:\n%s", want, p)
		}
	}
}

func TestLinuxEnableDisable(t *testing.T) {
	home := t.TempDir()
	rec := &recorder{}
	m := Manager{GOOS: "linux", SystemdRunning: true, Home: home, Exe: "/usr/bin/sol", DataDir: filepath.Join(home, "data"), Run: rec.run}
	if st := m.Status(); !st.Supported || st.Enabled {
		t.Fatalf("before: %+v", st)
	}
	if err := m.Enable(false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".config/systemd/user/sol.service")); err != nil {
		t.Fatal(err)
	}
	want := []string{"systemctl --user show-environment", "systemctl --user daemon-reload", "systemctl --user reset-failed sol.service", "systemctl --user enable sol.service"}
	if strings.Join(rec.calls, "|") != strings.Join(want, "|") {
		t.Fatalf("calls %q, want %q", rec.calls, want)
	}
	// systemctl enable makes this symlink; Status looks for it.
	wants := filepath.Join(home, ".config/systemd/user/default.target.wants")
	os.MkdirAll(wants, 0o755)
	os.Symlink("../sol.service", filepath.Join(wants, "sol.service"))
	if st := m.Status(); !st.Enabled {
		t.Fatalf("after enable: %+v", st)
	}
	other := m
	other.DataDir = filepath.Join(home, "other")
	if st := other.Status(); st.Enabled || !st.Other {
		t.Fatalf("another data dir: %+v", st)
	}
	n := len(rec.calls)
	if err := other.Disable(); err != ErrOther {
		t.Fatalf("disable from another data dir: %v", err)
	}
	if err := other.Enable(true); err != ErrOther {
		t.Fatalf("enable from another data dir: %v", err)
	}
	if len(rec.calls) != n {
		t.Fatalf("refused calls still ran commands: %q", rec.calls[n:])
	}

	rec.calls = nil
	if err := m.Enable(true); err != nil {
		t.Fatal(err)
	}
	if rec.calls[3] != "systemctl --user enable --now sol.service" {
		t.Fatalf("start now: %q", rec.calls)
	}

	rec.calls = nil
	if err := m.Disable(); err != nil {
		t.Fatal(err)
	}
	if rec.calls[0] != "systemctl --user disable --now sol.service" {
		t.Fatalf("disable: %q", rec.calls)
	}
	if _, err := os.Stat(m.unitPath()); !os.IsNotExist(err) {
		t.Fatal("unit file left behind")
	}
}

func TestDarwinEnable(t *testing.T) {
	old := bootoutWait
	bootoutWait = 10 * time.Millisecond
	t.Cleanup(func() { bootoutWait = old })
	home := t.TempDir()
	rec := &recorder{}
	m := Manager{GOOS: "darwin", Home: home, Exe: "/usr/local/bin/sol", DataDir: filepath.Join(home, ".sol"), UID: 501, Run: rec.run}
	if err := m.Enable(true); err != nil {
		t.Fatal(err)
	}
	if !m.Status().Enabled {
		t.Fatal("plist not written")
	}
	if last := rec.calls[len(rec.calls)-1]; !strings.HasPrefix(last, "launchctl bootstrap gui/501 ") {
		t.Fatalf("calls %q", rec.calls)
	}
	if err := m.Disable(); err != nil || m.Status().Enabled {
		t.Fatalf("disable: %v %+v", err, m.Status())
	}
}

func TestWindowsIsHonest(t *testing.T) {
	m := Manager{GOOS: "windows", Home: t.TempDir()}
	if m.Status().Supported {
		t.Fatal("windows claims support")
	}
	if err := m.Enable(true); err != ErrNotImplemented {
		t.Fatalf("enable: %v", err)
	}
}

func TestUnregisteredOtherEntryIsProtected(t *testing.T) {
	home := t.TempDir()
	rec := &recorder{}
	a := Manager{GOOS: "linux", SystemdRunning: true, Home: home, Exe: "/usr/bin/sol", DataDir: filepath.Join(home, "a"), Run: rec.run}
	if err := a.Enable(false); err != nil {
		t.Fatal(err)
	}
	// Not registered (no wants symlink), but the file names /data/a.
	b := a
	b.DataDir = filepath.Join(home, "b")
	if err := b.Disable(); err != ErrOther {
		t.Fatalf("disable: %v", err)
	}
	if _, err := os.Stat(a.unitPath()); err != nil {
		t.Fatal("another data dir's unit was removed")
	}
}

func TestXDGConfigHome(t *testing.T) {
	m := Manager{GOOS: "linux", SystemdRunning: true, Home: "/home/a", ConfigHome: "/home/a/.xdg"}
	if m.unitPath() != "/home/a/.xdg/systemd/user/sol.service" {
		t.Fatal(m.unitPath())
	}
}

func TestDarwinActiveNeedsARunningProcess(t *testing.T) {
	m := Manager{GOOS: "darwin", UID: 501, Run: func(string, ...string) ([]byte, error) {
		return []byte("gui/501/dev.sol.node = {\n\tstate = not running\n}"), nil
	}}
	if m.Active() {
		t.Fatal("a loaded job without a process is not active")
	}
}

func TestNoSystemdIsUnsupported(t *testing.T) {
	m := Manager{GOOS: "linux", Home: t.TempDir(), DataDir: filepath.Join(os.TempDir(), "sol-autostart-test-nonexistent")}
	if m.Status().Supported {
		t.Fatal("linux without systemd claims support")
	}
	if err := m.Enable(true); err != ErrNotImplemented {
		t.Fatalf("enable: %v", err)
	}
	if _, err := os.Stat(m.unitPath()); !os.IsNotExist(err) {
		t.Fatal("unit written without systemd")
	}
}

func TestFailedReloadLeavesNoUnit(t *testing.T) {
	m := Manager{GOOS: "linux", SystemdRunning: true, Home: t.TempDir(), DataDir: filepath.Join(os.TempDir(), "sol-autostart-test-nonexistent"),
		Run: func(string, ...string) ([]byte, error) { return nil, os.ErrPermission }}
	if err := m.Enable(true); err == nil {
		t.Fatal("expected the systemctl error")
	}
	if _, err := os.Stat(m.unitPath()); !os.IsNotExist(err) {
		t.Fatal("half-set-up unit left behind")
	}
}

func TestPathWithDollarIsKept(t *testing.T) {
	if u := SystemdUnit("/s", "/d", "/opt/a$b/bin"); !strings.Contains(u, `Environment="PATH=/opt/a$b/bin"`) {
		t.Fatalf("PATH mangled:\n%s", u)
	}
}

func TestUnitGoesWhereTheManagerLooks(t *testing.T) {
	home := t.TempDir()
	m := Manager{GOOS: "linux", SystemdRunning: true, Home: home, ConfigHome: home + "/shell-only", DataDir: filepath.Join(home, "data"),
		Run: func(name string, args ...string) ([]byte, error) {
			if strings.Join(args, " ") == "--user show-environment" {
				return []byte("HOME=" + home + "\nPATH=/usr/bin\n"), nil // the manager has no XDG_CONFIG_HOME
			}
			return nil, nil
		}}
	if err := m.Enable(false); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(home + "/.config/systemd/user/sol.service"); err != nil {
		t.Fatal("unit not in the manager's config home")
	}
	// Status from the shell (with its own XDG_CONFIG_HOME) still finds it.
	os.MkdirAll(home+"/.config/systemd/user/default.target.wants", 0o755)
	os.Symlink("../sol.service", home+"/.config/systemd/user/default.target.wants/sol.service")
	if !m.Status().Enabled {
		t.Fatalf("status from the shell: %+v", m.Status())
	}
}

func TestManagerConfigHomeIsRecorded(t *testing.T) {
	home, data := t.TempDir(), t.TempDir()
	mgr := home + "/manager-xdg"
	m := Manager{GOOS: "linux", SystemdRunning: true, Home: home, DataDir: data,
		Run: func(name string, args ...string) ([]byte, error) {
			if strings.Join(args, " ") == "--user show-environment" {
				return []byte("XDG_CONFIG_HOME=" + mgr + "\n"), nil // the shell has none
			}
			return nil, nil
		}}
	if err := m.Enable(false); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(mgr+"/systemd/user/default.target.wants", 0o755)
	os.Symlink("../sol.service", mgr+"/systemd/user/default.target.wants/sol.service")
	if st := m.Status(); !st.Enabled || st.Path != mgr+"/systemd/user/sol.service" {
		t.Fatalf("status from an environment without the manager's XDG: %+v", st)
	}
	if err := m.Disable(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(mgr + "/systemd/user/sol.service"); !os.IsNotExist(err) {
		t.Fatal("disable missed the unit in the manager's config home")
	}
}

func TestGoRunDetection(t *testing.T) {
	tmp := filepath.Clean(os.TempDir())
	if !isGoRunBinary(filepath.Join(tmp, "go-build123456", "b001", "exe", "sol")) {
		t.Fatal("go run binary not detected")
	}
	for _, p := range []string{"/home/a/src/go-builder/bin/sol", filepath.Join(tmp, "go-builds", "sol"), "/usr/local/bin/sol"} {
		if isGoRunBinary(p) {
			t.Fatalf("%s taken for a go run binary", p)
		}
	}
}
