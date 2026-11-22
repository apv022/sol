package daemon

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/apv022/sol/internal/fsutil"
	"github.com/apv022/sol/sdk/go/sol"
	kit "github.com/apv022/sol/ui/kit"
)

// Themes (design/UI.md "Themes", ui/kit/TOKENS.md). The active theme's name is the one line of
// <files>/.sol/theme (the app host serves it as /_sol/theme.css), the appearance settings are
// <files>/.sol/theme-settings.json, and theme files are <files>/.sol/themes/<name>.css next to
// the built-ins. Changing any of them (through the admin API, or by editing the files, which
// the daemon watches where the OS lets it) pushes a "theme" event to every app's event
// stream, so open UIs swap live.

var (
	themeNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)
	accentRe    = regexp.MustCompile(`^#([0-9a-fA-F]{3}|[0-9a-fA-F]{4}|[0-9a-fA-F]{6}|[0-9a-fA-F]{8})$`)
)

// ThemeSettingValues are the allowed values of each appearance setting ("accent" is any
// #rgb, #rgba, #rrggbb or #rrggbbaa colour, or "" for the theme's own; "wallpaper" is one of
// BuiltinWallpapers or "custom:<id>" for an uploaded image, wallpapers.go).
var ThemeSettingValues = map[string][]string{
	"scheme":  {"auto", "light", "dark"},
	"density": {"comfortable", "compact"},
	"radius":  {"rounded", "square"},
	"glass":   {"on", "off"},
	"font":    {"system", "inter", "mono"},
	"accent":  nil,
	// wallpaper is checked by checkThemeSetting (built-ins or custom:<id>), so it has no fixed list.
	"wallpaper": nil,
}

// BuiltinWallpapers are the wallpapers drawn in CSS (ui/home/wallpaper.css); "dusk" is the default.
var BuiltinWallpapers = []string{"dusk", "aurora", "ember", "slate"}

// ThemeInfo is one theme `sol theme list` shows.
type ThemeInfo struct {
	Name     string   `json:"name"`
	Title    string   `json:"title"`
	Author   string   `json:"author,omitempty"`
	Supports []string `json:"supports,omitempty"`
	Builtin  bool     `json:"builtin"`
}

// ThemeState is the reply of GET /admin/v1/themes.
type ThemeState struct {
	Active   string            `json:"active"`
	Settings map[string]string `json:"settings"`
	Themes   []ThemeInfo       `json:"themes"`
}

func (d *Daemon) solDir() string { return filepath.Join(d.cfg.Files, ".sol") }

// parseThemeHeader reads the @theme, @author and @supports lines of a theme file's header.
func parseThemeHeader(name string, b []byte, builtin bool) ThemeInfo {
	t := ThemeInfo{Name: name, Title: name, Builtin: builtin}
	end := strings.Index(string(b), "*/")
	if !strings.HasPrefix(strings.TrimSpace(string(b)), "/*") || end < 0 {
		return t
	}
	sc := bufio.NewScanner(strings.NewReader(string(b[:end])))
	for sc.Scan() {
		line := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(sc.Text()), "/*"))
		key, val, _ := strings.Cut(line, " ")
		val = strings.TrimSpace(val)
		switch key {
		case "@theme":
			if val != "" {
				t.Title = val
			}
		case "@author":
			t.Author = val
		case "@supports":
			t.Supports = strings.Fields(val)
		}
	}
	return t
}

// Themes lists the built-in themes and the person's own, the active one and the settings.
func (d *Daemon) Themes() ThemeState {
	st := ThemeState{Active: d.activeTheme(), Settings: d.themeSettings()}
	seen := map[string]bool{}
	if d.cfg.Files != "" {
		ents, _ := os.ReadDir(filepath.Join(d.solDir(), "themes"))
		for _, e := range ents {
			n, ok := strings.CutSuffix(e.Name(), ".css")
			if !ok || e.IsDir() || !themeNameRe.MatchString(n) {
				continue
			}
			b, err := os.ReadFile(filepath.Join(d.solDir(), "themes", e.Name()))
			if err != nil {
				continue
			}
			seen[n] = true
			st.Themes = append(st.Themes, parseThemeHeader(n, b, false))
		}
	}
	ents, _ := fs.ReadDir(kit.FS, "themes")
	for _, e := range ents {
		n, ok := strings.CutSuffix(e.Name(), ".css")
		if !ok || seen[n] {
			continue
		}
		b, _ := fs.ReadFile(kit.FS, "themes/"+e.Name())
		st.Themes = append(st.Themes, parseThemeHeader(n, b, true))
	}
	sort.Slice(st.Themes, func(i, j int) bool { return st.Themes[i].Name < st.Themes[j].Name })
	return st
}

// activeTheme is the name in <files>/.sol/theme, "sol" by default.
func (d *Daemon) activeTheme() string {
	if d.cfg.Files == "" {
		return "sol"
	}
	b, err := os.ReadFile(filepath.Join(d.solDir(), "theme"))
	if n := strings.TrimSpace(string(b)); err == nil && themeNameRe.MatchString(n) {
		return n
	}
	return "sol"
}

// themeSettings reads the settings file, keeping only valid entries.
func (d *Daemon) themeSettings() map[string]string {
	out := map[string]string{}
	if d.cfg.Files == "" {
		return out
	}
	b, err := os.ReadFile(filepath.Join(d.solDir(), "theme-settings.json"))
	if err != nil {
		return out
	}
	var m map[string]string
	if json.Unmarshal(b, &m) != nil {
		return out
	}
	for k, v := range m {
		if checkThemeSetting(k, v) == nil {
			out[k] = v
		}
	}
	return out
}

func checkThemeSetting(k, v string) error {
	allowed, ok := ThemeSettingValues[k]
	switch {
	case !ok:
		return sol.Errorf("bad_request", "unknown setting "+k+" (scheme, accent, density, radius, glass, font, wallpaper)")
	case k == "accent":
		if v != "" && !accentRe.MatchString(v) {
			return sol.Errorf("bad_request", "accent is a colour like #b58900")
		}
	case k == "wallpaper":
		if v == "" || contains(BuiltinWallpapers, v) {
			return nil
		}
		if id, ok := strings.CutPrefix(v, "custom:"); !ok || !wallpaperIDRe.MatchString(id) {
			return sol.Errorf("bad_request", "wallpaper is one of "+strings.Join(BuiltinWallpapers, ", ")+" or custom:<id> of an uploaded image")
		}
	case v != "" && !contains(allowed, v):
		return sol.Errorf("bad_request", k+" is one of "+strings.Join(allowed, ", "))
	}
	return nil
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if x == y {
			return true
		}
	}
	return false
}

// SetTheme makes name the active theme and pushes a theme event.
func (d *Daemon) SetTheme(name string) error {
	if !themeNameRe.MatchString(name) {
		return sol.Errorf("bad_request", "bad theme name")
	}
	found := false
	for _, t := range d.Themes().Themes {
		found = found || t.Name == name
	}
	if !found {
		return sol.Errorf("not_found", "no theme "+name+" (sol theme list)")
	}
	if err := d.writeSol("theme", []byte(name+"\n")); err != nil {
		return err
	}
	d.pushTheme()
	return nil
}

// UpdateThemeSettings merges settings into the stored ones ("" removes a setting) and pushes a
// theme event.
func (d *Daemon) UpdateThemeSettings(settings map[string]string) (map[string]string, error) {
	cur := d.themeSettings()
	for k, v := range settings {
		if err := checkThemeSetting(k, v); err != nil {
			return nil, err
		}
		if v == "" {
			delete(cur, k)
		} else {
			cur[k] = v
		}
	}
	b, _ := json.MarshalIndent(cur, "", "  ")
	if err := d.writeSol("theme-settings.json", b); err != nil {
		return nil, err
	}
	d.pushTheme()
	return cur, nil
}

// writeSol writes a file under <files>/.sol atomically.
func (d *Daemon) writeSol(name string, b []byte) error {
	if d.cfg.Files == "" {
		return errors.New("daemon: no synced folder configured")
	}
	if err := os.MkdirAll(filepath.Join(d.solDir(), "themes"), 0o755); err != nil {
		return err
	}
	return fsutil.WriteFileAtomic(filepath.Join(d.solDir(), name), b, 0o644)
}

// themePayload is the data of a theme event (TOKENS.md): the name and the full settings, with
// the defaults spelled out so a UI can clear what was unset.
func (d *Daemon) themePayload() []byte {
	s := d.themeSettings()
	full := map[string]string{"scheme": "auto", "accent": "", "density": "comfortable", "radius": "rounded", "glass": "on", "font": "system", "wallpaper": "dusk"}
	for k, v := range s {
		full[k] = v
	}
	b, _ := json.Marshal(sol.ThemeEvent{Name: d.activeTheme(), Settings: full})
	return b
}

// pushTheme sends the current theme event to every app's event stream.
func (d *Daemon) pushTheme() {
	p := d.themePayload()
	d.API.Theme(p)
	d.emit(EvTheme, json.RawMessage(p))
}

// watchThemes pushes a theme event whenever the theme files change on disk, coalescing bursts
// (an editor's save is several events). Where the OS has no watcher here, edits made outside
// Sol show up on the next change made through Sol or the next reload; nothing is polled.
func (d *Daemon) watchThemes() {
	if d.cfg.Files == "" {
		return
	}
	if err := os.MkdirAll(filepath.Join(d.solDir(), "themes"), 0o755); err != nil {
		d.log.Warn("theme folder", "err", err)
		return
	}
	ch, stop, err := watchDirs(d.solDir(), filepath.Join(d.solDir(), "themes"))
	if err != nil {
		d.log.Info("theme files are not watched on this system", "err", err)
		return
	}
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer stop()
		var fire <-chan time.Time
		for {
			select {
			case <-d.ctx.Done():
				return
			case _, ok := <-ch:
				if !ok {
					return
				}
				if fire == nil {
					fire = time.After(100 * time.Millisecond)
				}
			case <-fire:
				fire = nil
				d.pushTheme()
			}
		}
	}()
}
