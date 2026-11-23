package autostart

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/apv022/sol/internal/fsutil"
)

// The desktop launcher: a "Sol" entry in the system's app menu that runs `sol open`, so a
// person who never touches a terminal can start Sol and get the app already signed in.
//
// Linux writes ~/.local/share/applications/sol.desktop and ~/.local/share/icons/sol.svg
// (honouring $XDG_DATA_HOME), plus one entry per app with a UI, sol-app-<id>.desktop with the
// app's 512 px PNG as sol-app-<id>.png ("Sol Files", running `sol open files`), kept in step
// with the installed apps by SyncApps. macOS and Windows say not_implemented for now. Like the
// autostart entry it is per-user and removed by `sol autostart off` / `sol launcher off`;
// removing it never touches anything the user put there other than files named sol.* and
// sol-app-*.

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
	// Apps are the apps with a UI that Enable gives an entry of their own (see SyncApps).
	Apps []App
}

// App is an app with a UI that gets its own app-menu entry.
type App struct {
	ID, Name string
	Icon     []byte // the 512 px PNG
}

// appPrefix starts the file stem of every per-app entry (sol-app-files.desktop).
const appPrefix = LauncherName + "-app-"

var safeAppID = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)

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
	if err := fsutil.WriteFileAtomic(l.DesktopPath(), []byte(DesktopEntry(l.Exe, l.Dir, l.IconPath())), 0o644); err != nil {
		return err
	}
	return l.SyncApps(l.Apps)
}

func (l Launcher) appDesktop(id string) string {
	return filepath.Join(l.dataHome(), "applications", appPrefix+id+".desktop")
}

func (l Launcher) appIcon(id string) string {
	return filepath.Join(l.dataHome(), "icons", appPrefix+id+".png")
}

// AppEntry is the .desktop text of one app's entry: "Sol <name>", running `sol open <id>`.
func AppEntry(exe, dir string, a App, icon string) string {
	// A manifest name is data: no line break may start a new key.
	name := strings.Map(func(r rune) rune {
		if r < ' ' || r == 0x7f {
			return -1
		}
		return r
	}, a.Name)
	return `# Written by "sol launcher on". Removed with "sol launcher off" or when the app is removed.
[Desktop Entry]
Type=Application
Name=Sol ` + name + `
Comment=Open ` + name + ` in Sol
Exec=` + desktopQuote(exe) + ` open --dir ` + desktopQuote(dir) + ` ` + desktopQuote(a.ID) + `
Icon=` + icon + `
Terminal=false
Categories=Network;
StartupNotify=false
`
}

// SyncApps makes the per-app entries exactly those of apps: it writes (only when changed) an
// entry and icon for each, and removes entries of apps no longer listed. It never touches the
// "Sol" entry. An app whose id is not a plain slug is skipped, as it would not be a safe file name.
func (l Launcher) SyncApps(apps []App) error {
	if l.GOOS != "linux" {
		return ErrLauncherNotImplemented
	}
	for _, d := range []string{filepath.Dir(l.IconPath()), filepath.Dir(l.DesktopPath())} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}
	keep := map[string]bool{}
	for _, a := range apps {
		if !safeAppID.MatchString(a.ID) {
			continue
		}
		keep[l.appDesktop(a.ID)], keep[l.appIcon(a.ID)] = true, true
		icon := l.appIcon(a.ID)
		if len(a.Icon) > 0 {
			if err := writeIfChanged(icon, a.Icon); err != nil {
				return err
			}
		} else {
			icon = LauncherName // the Sol mark, from the icon theme path
			keep[l.appIcon(a.ID)] = false
		}
		if err := writeIfChanged(l.appDesktop(a.ID), []byte(AppEntry(l.Exe, l.Dir, a, icon))); err != nil {
			return err
		}
	}
	return l.removeApps(keep)
}

// removeApps deletes every sol-app-* entry and icon not in keep.
func (l Launcher) removeApps(keep map[string]bool) error {
	for _, g := range []string{
		filepath.Join(l.dataHome(), "applications", appPrefix+"*.desktop"),
		filepath.Join(l.dataHome(), "icons", appPrefix+"*.png"),
	} {
		old, _ := filepath.Glob(g)
		for _, p := range old {
			if !keep[p] {
				if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
					return fmt.Errorf("removing %s: %w", p, err)
				}
			}
		}
	}
	return nil
}

func writeIfChanged(p string, b []byte) error {
	if old, err := os.ReadFile(p); err == nil && bytes.Equal(old, b) {
		return nil
	}
	return fsutil.WriteFileAtomic(p, b, 0o644)
}

// Disable removes the entry, its icon and every per-app entry; missing files are fine.
func (l Launcher) Disable() error {
	if l.GOOS != "linux" {
		return ErrLauncherNotImplemented
	}
	for _, p := range []string{l.DesktopPath(), l.IconPath()} {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("removing %s: %w", p, err)
		}
	}
	return l.removeApps(nil)
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
