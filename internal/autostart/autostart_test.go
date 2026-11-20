package autostart

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSystemdUnitGolden(t *testing.T) {
	got := SystemdUnit("/usr/local/bin/sol", "/home/a b/.local/share/sol", "/usr/bin:/bin")
	want := `# Written by "sol autostart on". Remove with "sol autostart off".
[Unit]
Description=Sol
StartLimitIntervalSec=300
StartLimitBurst=5

[Service]
Environment=SOL_SUPERVISED=1
Environment="PATH=/usr/bin:/bin"
ExecStart="/usr/local/bin/sol" run --dir "/home/a b/.local/share/sol"
Restart=on-failure
RestartSec=10

[Install]
WantedBy=default.target
`
	if got != want {
		t.Fatalf("unit:\n%s\nwant:\n%s", got, want)
	}
}

func TestPlistGolden(t *testing.T) {
	got := LaunchAgentPlist("/opt/sol", "/Users/a/Library/Application Support/Sol", "/opt/homebrew/bin")
	for _, w := range []string{
		"<key>Label</key><string>dev.sol.node</string>",
		"<string>/opt/sol</string>\n\t\t<string>run</string>\n\t\t<string>--dir</string>\n\t\t<string>/Users/a/Library/Application Support/Sol</string>",
		"<key>SOL_SUPERVISED</key><string>1</string><key>PATH</key><string>/opt/homebrew/bin</string>",
		"<key>StandardErrorPath</key><string>/Users/a/Library/Application Support/Sol/sol.log</string>",
	} {
		if !strings.Contains(got, w) {
			t.Errorf("plist lacks %q:\n%s", w, got)
		}
	}
}

// fake records commands instead of running them.
type fake struct{ calls []string }

func (f *fake) run(name string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, name+" "+strings.Join(args, " "))
	return nil, nil
}

func TestEnableDisableLinux(t *testing.T) {
	home := t.TempDir()
	f := &fake{}
	m := Manager{GOOS: "linux", Home: home, Exe: "/bin/sol", Dir: "/data/sol", SystemdRunning: true, Run: f.run}
	if st := m.Status(); !st.Supported || st.Enabled {
		t.Fatalf("before: %+v", st)
	}
	if err := m.Enable(true); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(home, ".config", "systemd", "user", "sol.service"))
	if err != nil || !strings.Contains(string(b), `ExecStart="/bin/sol" run --dir "/data/sol"`) {
		t.Fatalf("unit %v %s", err, b)
	}
	if st := m.Status(); !st.Enabled || st.Other {
		t.Fatalf("after enable: %+v", st)
	}
	if last := f.calls[len(f.calls)-1]; last != "systemctl --user enable --now sol.service" {
		t.Fatalf("calls %q", f.calls)
	}
	other := m
	other.Dir = "/elsewhere"
	if err := other.Enable(false); !errors.Is(err, ErrOther) {
		t.Fatalf("other dir: %v", err)
	}
	if err := m.Disable(); err != nil {
		t.Fatal(err)
	}
	if st := m.Status(); st.Enabled {
		t.Fatalf("after disable: %+v", st)
	}
}

func TestEnableMac(t *testing.T) {
	home, dir := t.TempDir(), t.TempDir()
	f := &fake{}
	m := Manager{GOOS: "darwin", Home: home, Exe: "/bin/sol", Dir: dir, UID: 501, Run: f.run}
	if err := m.Enable(false); err != nil || len(f.calls) != 0 {
		t.Fatalf("enable: %v %q", err, f.calls)
	}
	if st := m.Status(); !st.Enabled {
		t.Fatalf("status %+v", st)
	}
	if err := m.Disable(); err != nil {
		t.Fatal(err)
	}
}

func TestWindowsNotImplemented(t *testing.T) {
	m := Manager{GOOS: "windows"}
	if err := m.Enable(true); !errors.Is(err, ErrNotImplemented) {
		t.Fatalf("got %v", err)
	}
	if m.Status().Supported {
		t.Fatal("supported")
	}
}

func TestGoRunDetected(t *testing.T) {
	if !isGoRunBinary("/tmp/go-build123/b001/exe/sol") || !isGoRunBinary("/x/autostart.test") {
		t.Fatal("not detected")
	}
}
