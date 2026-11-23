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

func TestDesktopEntryGolden(t *testing.T) {
	got := DesktopEntry("/opt/my apps/sol", "/home/a/.local/share/sol", "/home/a/.local/share/icons/sol.svg")
	want := `# Written by "sol autostart on" / "sol launcher on". Remove with "sol launcher off".
[Desktop Entry]
Type=Application
Name=Sol
Comment=Open Sol in your browser
Exec="/opt/my apps/sol" open --dir "/home/a/.local/share/sol"
Icon=/home/a/.local/share/icons/sol.svg
Terminal=false
Categories=Network;
StartupNotify=false
`
	if got != want {
		t.Fatalf("entry:\n%s\nwant:\n%s", got, want)
	}
}

func TestLauncherEnableDisable(t *testing.T) {
	home := t.TempDir()
	l := Launcher{GOOS: "linux", Home: home, Exe: "/bin/sol", Dir: "/d"}
	if err := l.Enable(); err != nil || !l.Enabled() {
		t.Fatalf("enable: %v", err)
	}
	if b, _ := os.ReadFile(l.IconPath()); !strings.Contains(string(b), "<svg") {
		t.Fatal("icon missing")
	}
	if err := l.Disable(); err != nil || l.Enabled() {
		t.Fatalf("disable: %v", err)
	}
	if _, err := os.Stat(l.IconPath()); err == nil {
		t.Fatal("icon left behind")
	}
	if err := l.Disable(); err != nil {
		t.Fatalf("disable twice: %v", err)
	}
	if err := (Launcher{GOOS: "darwin"}).Enable(); !errors.Is(err, ErrLauncherNotImplemented) {
		t.Fatalf("darwin: %v", err)
	}
}

func TestLauncherAppEntries(t *testing.T) {
	home := t.TempDir()
	l := Launcher{GOOS: "linux", Home: home, Exe: "/bin/sol", Dir: "/d", Apps: []App{
		{ID: "files", Name: "Files", Icon: []byte("PNG1")}, {ID: "notes", Name: "No\ntes", Icon: []byte("PNG2")}, {ID: "../evil", Name: "x"}}}
	if err := l.Enable(); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(home, ".local/share/applications/sol-app-files.desktop"))
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"Name=Sol Files\n", `Exec="/bin/sol" open --dir "/d" "files"`, "Icon=" + filepath.Join(home, ".local/share/icons/sol-app-files.png")} {
		if !strings.Contains(string(b), w) {
			t.Errorf("entry lacks %q:\n%s", w, b)
		}
	}
	if n, _ := os.ReadFile(filepath.Join(home, ".local/share/applications/sol-app-notes.desktop")); !strings.Contains(string(n), "Name=Sol Notes\n") {
		t.Errorf("a line break in a name started a new key:\n%s", n)
	}
	if g, _ := filepath.Glob(filepath.Join(home, ".local/share/applications/*")); len(g) != 3 { // sol, files, notes
		t.Fatalf("entries: %v", g)
	}
	// An app removed drops its entry and icon; the Sol entry stays.
	if err := l.SyncApps(l.Apps[:1]); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(home, ".local/share/icons/sol-app-notes.png")); err == nil {
		t.Fatal("removed app's icon left behind")
	}
	if g, _ := filepath.Glob(filepath.Join(home, ".local/share/applications/*")); len(g) != 2 || !l.Enabled() {
		t.Fatalf("entries after removal: %v", g)
	}
	if err := l.Disable(); err != nil {
		t.Fatal(err)
	}
	if g, _ := filepath.Glob(filepath.Join(home, ".local/share/*/sol*")); len(g) != 0 {
		t.Fatalf("left behind: %v", g)
	}
}
