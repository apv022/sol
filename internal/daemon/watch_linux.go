//go:build linux

package daemon

import (
	"os"
	"syscall"
)

// watchDirs reports changes inside dirs (not below them) with inotify, through the standard
// library's syscall package. Linux and Android.
func watchDirs(dirs ...string) (<-chan struct{}, func(), error) {
	fd, err := syscall.InotifyInit1(syscall.IN_CLOEXEC | syscall.IN_NONBLOCK)
	if err != nil {
		return nil, nil, err
	}
	f := os.NewFile(uintptr(fd), "inotify")
	const mask = syscall.IN_CLOSE_WRITE | syscall.IN_MOVED_TO | syscall.IN_MOVED_FROM | syscall.IN_DELETE | syscall.IN_CREATE
	for _, d := range dirs {
		if _, err := syscall.InotifyAddWatch(fd, d, mask); err != nil {
			f.Close()
			return nil, nil, err
		}
	}
	ch := make(chan struct{}, 1)
	go func() {
		defer close(ch)
		buf := make([]byte, 16<<10)
		for {
			if _, err := f.Read(buf); err != nil {
				return
			}
			select {
			case ch <- struct{}{}:
			default:
			}
		}
	}()
	return ch, func() { f.Close() }, nil
}
