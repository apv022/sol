//go:build !linux

package daemon

import "time"

// watchDirs has no native watcher here yet (kqueue, ReadDirectoryChangesW are later work), so
// it scans on a backing-off timer: 1s after a change, up to 30s when idle.
func watchDirs(dirs ...string) (<-chan struct{}, func(), error) {
	return scanWatch(time.Second, 30*time.Second, dirs...)
}
