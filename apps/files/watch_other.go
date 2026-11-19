//go:build !linux

package files

import "errors"

// newWatcher: only Linux and Android have a watcher (inotify); elsewhere Files scans the folder
// every ScanEvery seconds.
func newWatcher(root string, ignore func(string) bool) (watcher, error) {
	return nil, errors.New("no file watcher on this system")
}
