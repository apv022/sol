//go:build linux

package main

import (
	"context"
	"os"
	"syscall"
)

// waitGrow blocks until path is longer than off bytes (or was written to; inotify) to (inotify) or ctx ends, so `sol logs -f` sleeps in
// the kernel between lines. It returns false when ctx ended.
func waitGrow(ctx context.Context, path string, off int64) bool {
	fd, err := syscall.InotifyInit1(syscall.IN_CLOEXEC)
	if err != nil {
		return sleepGrow(ctx)
	}
	f := os.NewFile(uintptr(fd), "inotify")
	defer f.Close()
	if _, err := syscall.InotifyAddWatch(fd, path, syscall.IN_MODIFY); err != nil {
		return sleepGrow(ctx)
	}
	if fi, err := os.Stat(path); err == nil && fi.Size() != off {
		return true // written between the last read and the watch
	}
	ready := make(chan struct{})
	go func() {
		buf := make([]byte, 4096)
		_, _ = f.Read(buf)
		close(ready)
	}()
	select {
	case <-ready:
		return true
	case <-ctx.Done():
		return false // the deferred Close unblocks the reader
	}
}
