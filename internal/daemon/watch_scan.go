package daemon

import (
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// scanWatch is the fallback watcher where the OS has no native one (macOS, Windows): it
// fingerprints the directories' entries (name, size, mtime) on a timer and reports a change.
// The timer backs off from min to max while nothing changes and snaps back to min on a change,
// so an idle daemon wakes at most every max (local files, a few entries: the scan is cheap).
func scanWatch(min, max time.Duration, dirs ...string) (<-chan struct{}, func(), error) {
	ch := make(chan struct{}, 1)
	done := make(chan struct{})
	var once sync.Once
	last := fingerprint(dirs)
	go func() {
		defer close(ch)
		wait := min
		t := time.NewTimer(wait)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
			}
			if fp := fingerprint(dirs); fp != last {
				last, wait = fp, min
				select {
				case ch <- struct{}{}:
				default:
				}
			} else if wait *= 2; wait > max {
				wait = max
			}
			t.Reset(wait)
		}
	}()
	return ch, func() { once.Do(func() { close(done) }) }, nil
}

func fingerprint(dirs []string) string {
	var parts []string
	for _, d := range dirs {
		ents, _ := os.ReadDir(d)
		for _, e := range ents {
			if info, err := e.Info(); err == nil && !strings.HasSuffix(e.Name(), ".tmp") {
				parts = append(parts, filepath.Join(d, e.Name())+"|"+strconv.FormatInt(info.Size(), 10)+"|"+strconv.FormatInt(info.ModTime().UnixNano(), 10))
			}
		}
	}
	sort.Strings(parts)
	return strings.Join(parts, "\n")
}
