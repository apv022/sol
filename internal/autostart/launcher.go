package autostart

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/apv022/sol/internal/fsutil"
)

// The desktop launcher: a "Sol" entry in the system's app menu that runs `sol open`, so a
// person who never touches a terminal can start Sol and get the app already signed in.
//
// Linux writes ~/.local/share/applications/sol.desktop and ~/.local/share/icons/sol.svg
// (honouring $XDG_DATA_HOME). macOS and Windows say not_implemented for now. Like the
// autostart entry it is per-user and removed by `sol autostart off` / `sol launcher off`;
// removing it never touches anything the user put there other than these two files.

// LauncherName is the launcher's file stem (sol.desktop, sol.svg).
const LauncherName = "sol"

// ErrLauncherNotImplemented means this system has no launcher support yet.
var ErrLauncherNotImplemented = errors.New("not_implemented: the app-menu entry is only available on Linux for now")

// Launcher writes and removes the app-menu entry for one binary and data directory.
type Launcher struct {
	GOOS     string
	Home     string
	DataHome string // $XDG_DATA_HOME ("" means ~/.local/share)
	Exe      string // absolute path of the sol binary
	Dir      string // the data directory `sol open` should use
}

func (l Launcher) dataHome() string {
	if l.DataHome != "" {
		return l.DataHome
	}
	return filepath.Join(l.Home, ".local", "share")
}

// DesktopPath is the .desktop file; IconPath the icon it names.
func (l Launcher) DesktopPath() string {
	return filepath.Join(l.dataHome(), "applications", LauncherName+".desktop")
}

// IconPath is where the icon is written.
func (l Launcher) IconPath() string {
	return filepath.Join(l.dataHome(), "icons", LauncherName+".svg")
}

// desktopQuote quotes one Exec argument per the Desktop Entry spec (the reserved characters
// get a backslash, a literal % is doubled).
func desktopQuote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "`", "\\`", "$", `\$`, "%", "%%").Replace(s) + `"`
}

// DesktopEntry is the launcher file's text.
func DesktopEntry(exe, dir, icon string) string {
	return `# Written by "sol autostart on" / "sol launcher on". Remove with "sol launcher off".
[Desktop Entry]
Type=Application
Name=Sol
Comment=Open Sol in your browser
Exec=` + desktopQuote(exe) + ` open --dir ` + desktopQuote(dir) + `
Icon=` + icon + `
Terminal=false
Categories=Network;
StartupNotify=false
`
}

// IconSVG is the sun mark of ui/kit/icons.js on a calm rounded square.
const IconSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 64 64" width="64" height="64">
<rect width="64" height="64" rx="14" fill="#1f2a44"/>
<g transform="translate(8 8) scale(2)" fill="none" stroke="#ffc65c" stroke-width="1.6" stroke-linecap="round" stroke-linejoin="round">
<path d="M12 7.8a4.2 4.2 0 100 8.4 4.2 4.2 0 000-8.4z"/>
<path d="M12 2.8v2M12 19.2v2M2.8 12h2M19.2 12h2M5.5 5.5l1.4 1.4M17.1 17.1l1.4 1.4M5.5 18.5l1.4-1.4M17.1 6.9l1.4-1.4"/>
</g>
</svg>
`

// Enable writes the launcher and its icon.
func (l Launcher) Enable() error {
	if l.GOOS != "linux" {
		return ErrLauncherNotImplemented
	}
	if err := os.MkdirAll(filepath.Dir(l.IconPath()), 0o755); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(l.DesktopPath()), 0o755); err != nil {
		return err
	}
	if err := fsutil.WriteFileAtomic(l.IconPath(), []byte(IconSVG), 0o644); err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(l.DesktopPath(), []byte(DesktopEntry(l.Exe, l.Dir, l.IconPath())), 0o644)
}

// Disable removes both files; missing files are fine.
func (l Launcher) Disable() error {
	if l.GOOS != "linux" {
		return ErrLauncherNotImplemented
	}
	for _, p := range []string{l.DesktopPath(), l.IconPath()} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("removing %s: %w", p, err)
		}
	}
	return nil
}

// Enabled reports whether the .desktop file exists.
func (l Launcher) Enabled() bool {
	_, err := os.Stat(l.DesktopPath())
	return err == nil
}

// LauncherForThisProcess is a Launcher for the running binary (ErrGoRun for a temporary build).
func LauncherForThisProcess(goos, dir string) (Launcher, error) {
	exe, err := invokedPath()
	if err != nil {
		return Launcher{}, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return Launcher{}, err
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return Launcher{}, err
	}
	l := Launcher{GOOS: goos, Home: home, DataHome: os.Getenv("XDG_DATA_HOME"), Exe: exe, Dir: abs}
	if isGoRunBinary(exe) {
		return l, ErrGoRun
	}
	return l, nil
}
