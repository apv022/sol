package calendar

// Subscription files: for each calendar, and for all of them, a read-only .ics under
// <app data dir>/subscribe/ that a desktop calendar app (Thunderbird, GNOME Calendar,
// Evolution) can subscribe to as a local file. The files live in the app's own data dir, not
// in the synced folder, so no two devices write the same file. They are rewritten atomically
// after the calendars change; a burst of changes (a sync folding many entries) costs one
// rewrite, scheduled by a timer, never by polling.

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/apv022/sol/sdk/go/sol"
)

// subDelay is how long a burst of changes settles before the files are rewritten.
const subDelay = 400 * time.Millisecond

// subFile is one subscription file.
type subFile struct {
	Calendar string `json:"calendar"` // the calendar's id, "all" for every calendar
	Name     string `json:"name"`
	Path     string `json:"path"`
	URL      string `json:"url"`
}

func (a *App) subDir() string {
	if a.data == "" {
		return ""
	}
	return filepath.Join(a.data, "subscribe")
}

// subTouch schedules a rewrite of the subscription files.
func (a *App) subTouch() {
	if a.data == "" {
		return
	}
	a.subMu.Lock()
	defer a.subMu.Unlock()
	if a.subStop {
		return
	}
	if a.subTimer != nil {
		a.subTimer.Reset(subDelay)
		return
	}
	a.subTimer = time.AfterFunc(subDelay, func() {
		if err := a.writeSubscriptions(); err != nil {
			a.logf("calendar: subscription files: %v", err)
		}
	})
}

// subClose stops the pending rewrite.
func (a *App) subClose() {
	a.subMu.Lock()
	a.subStop = true
	if a.subTimer != nil {
		a.subTimer.Stop()
	}
	a.subMu.Unlock()
}

func fileURL(p string) string {
	p = filepath.ToSlash(p)
	if runtime.GOOS == "windows" {
		p = "/" + p
	}
	return (&url.URL{Scheme: "file", Path: p}).String()
}

// writeSubscriptions rewrites every file and removes those of calendars that are gone.
func (a *App) writeSubscriptions() error {
	dir := a.subDir()
	if dir == "" {
		return nil
	}
	a.subWrite.Lock()
	defer a.subWrite.Unlock()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	cals := a.view()
	want := map[string]bool{"all.ics": true}
	var files []subFile
	put := func(file, id, name, content string) error {
		p := filepath.Join(dir, file)
		want[file] = true
		files = append(files, subFile{Calendar: id, Name: name, Path: p, URL: fileURL(p)})
		return writeReadOnly(p, content)
	}
	if err := put("all.ics", "all", "All calendars", a.exportICS(cals)); err != nil {
		return err
	}
	used := map[string]bool{"all": true}
	for _, cs := range cals {
		base := safeFilename(cs.info.Name)
		if used[strings.ToLower(base)] {
			base += "-" + cs.id
		}
		used[strings.ToLower(base)] = true
		if err := put(base+".ics", cs.id, cs.info.Name, a.exportICS([]*calState{cs})); err != nil {
			return err
		}
	}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if !want[e.Name()] && strings.HasSuffix(e.Name(), ".ics") {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
	a.subMu.Lock()
	a.subFiles = files
	a.subMu.Unlock()
	return nil
}

// writeReadOnly replaces p atomically with a file nobody but the app can change (0444: a
// subscribing app that tries to write back is refused).
func writeReadOnly(p, content string) error {
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(0o444); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), p); err != nil {
		// Windows refuses to rename over a read-only file (POSIX allows it, the directory is what
		// counts). Remove the old copy and try again; a reader that has it open keeps its handle.
		if rerr := os.Remove(p); rerr != nil && !os.IsNotExist(rerr) {
			return err
		}
		return os.Rename(tmp.Name(), p)
	}
	return nil
}

// actSubscription serves calendar.subscription: the files a desktop calendar app can subscribe to.
func (a *App) actSubscription(_ context.Context, _ sol.Call) (any, error) {
	if a.data == "" {
		return nil, sol.Errorf("not_implemented", "no app data directory on this device")
	}
	if err := a.writeSubscriptions(); err != nil {
		return nil, err
	}
	a.subMu.Lock()
	files := append([]subFile(nil), a.subFiles...)
	a.subMu.Unlock()
	return map[string]any{"dir": a.subDir(), "files": files,
		"note": "Read-only copies kept current on this device; subscribe from a desktop calendar app with the URL (File or On the Network, then iCalendar or .ics)."}, nil
}
