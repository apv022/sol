//go:build !linux

package daemon

import "errors"

// watchDirs has no implementation here yet (kqueue, ReadDirectoryChangesW are later work):
// theme edits made outside Sol reach open UIs on the next change made through Sol.
func watchDirs(dirs ...string) (<-chan struct{}, func(), error) {
	return nil, nil, errors.New("no file watcher on this system")
}
