package daemon

import "strings"

// menuChanged tells Options.OnApps the apps that have a UI when that set (ids and names)
// differs from what it was last told, so an install or removal updates the desktop's app menu
// without every restart or run-state change rewriting it. The hook runs in its own goroutine:
// it writes files and must never hold up the app host.
func (d *Daemon) menuChanged() {
	if d.onApps == nil || d.Host == nil {
		return
	}
	apps := d.Host.MenuApps()
	var key strings.Builder
	for _, a := range apps {
		key.WriteString(a.ID + "\x00" + a.Name + "\x00")
	}
	d.menuMu.Lock()
	same := d.menuKey == key.String()
	d.menuKey = key.String()
	d.menuMu.Unlock()
	if !same {
		go d.onApps(apps)
	}
}
