//go:build linux

package files

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

// inotifyWatcher watches a folder tree with inotify through the standard library's syscall
// package (no dependency). Linux and Android only.
type inotifyWatcher struct {
	root   string
	f      *os.File
	fd     int
	mu     sync.Mutex
	wds    map[int32]string // watch descriptor -> folder path relative to root ("" is the root)
	paths  map[string]int32
	out    chan string
	done   chan struct{}
	ignore func(string) bool
}

const watchMask = syscall.IN_CREATE | syscall.IN_DELETE | syscall.IN_MODIFY | syscall.IN_CLOSE_WRITE |
	syscall.IN_MOVED_FROM | syscall.IN_MOVED_TO | syscall.IN_DELETE_SELF | syscall.IN_ATTRIB

func newWatcher(root string, ignore func(string) bool) (watcher, error) {
	fd, err := syscall.InotifyInit1(syscall.IN_CLOEXEC | syscall.IN_NONBLOCK)
	if err != nil {
		return nil, err
	}
	w := &inotifyWatcher{root: root, fd: fd, f: os.NewFile(uintptr(fd), "inotify"), wds: map[int32]string{}, paths: map[string]int32{},
		out: make(chan string, 4096), done: make(chan struct{}), ignore: ignore}
	if err := w.addTree(""); err != nil {
		_ = w.f.Close()
		return nil, err
	}
	go w.run()
	return w, nil
}

// Events delivers paths (relative, "/"-separated) that may have changed. The empty path means
// "unknown, scan everything" (the kernel queue overflowed).
func (w *inotifyWatcher) Events() <-chan string { return w.out }

func (w *inotifyWatcher) Close() error {
	select {
	case <-w.done:
		return nil
	default:
		close(w.done)
	}
	return w.f.Close()
}

// addTree watches dir and every folder below it.
func (w *inotifyWatcher) addTree(dir string) error {
	full := filepath.Join(w.root, filepath.FromSlash(dir))
	if dir != "" && w.ignore(dir) {
		return nil
	}
	if err := w.add(dir, full); err != nil {
		if dir == "" {
			return err
		}
		return nil // vanished or unreadable: the scan will notice
	}
	ents, err := os.ReadDir(full)
	if err != nil {
		return nil
	}
	for _, e := range ents {
		if e.IsDir() {
			_ = w.addTree(join(dir, e.Name()))
		}
	}
	return nil
}

func (w *inotifyWatcher) add(dir, full string) error {
	p, err := syscall.BytePtrFromString(full)
	if err != nil {
		return err
	}
	wd, _, errno := syscall.Syscall(syscall.SYS_INOTIFY_ADD_WATCH, uintptr(w.fd), uintptr(unsafe.Pointer(p)), uintptr(watchMask|syscall.IN_ONLYDIR))
	if errno != 0 {
		return errno
	}
	w.mu.Lock()
	w.wds[int32(wd)] = dir
	w.paths[dir] = int32(wd)
	w.mu.Unlock()
	return nil
}

func (w *inotifyWatcher) emit(p string) {
	select {
	case w.out <- p:
	case <-w.done:
	default:
		// Queue full: fall back to "scan everything" instead of blocking the kernel reader.
		select {
		case w.out <- "":
		default:
		}
	}
}

func (w *inotifyWatcher) run() {
	buf := make([]byte, 64*1024)
	for {
		n, err := w.f.Read(buf)
		if err != nil {
			if errors.Is(err, os.ErrClosed) || errors.Is(err, syscall.EBADF) {
				return
			}
			select {
			case <-w.done:
				return
			default:
			}
			continue
		}
		for off := 0; off+syscall.SizeofInotifyEvent <= n; {
			ev := (*syscall.InotifyEvent)(unsafe.Pointer(&buf[off]))
			nameLen := int(ev.Len)
			name := ""
			if nameLen > 0 {
				raw := buf[off+syscall.SizeofInotifyEvent : off+syscall.SizeofInotifyEvent+nameLen]
				name = strings.TrimRight(string(raw), "\x00")
			}
			off += syscall.SizeofInotifyEvent + nameLen
			if ev.Mask&syscall.IN_Q_OVERFLOW != 0 {
				w.emit("")
				continue
			}
			w.mu.Lock()
			dir, ok := w.wds[ev.Wd]
			w.mu.Unlock()
			if !ok {
				continue
			}
			if ev.Mask&syscall.IN_IGNORED != 0 {
				w.mu.Lock()
				delete(w.wds, ev.Wd)
				if w.paths[dir] == ev.Wd {
					delete(w.paths, dir)
				}
				w.mu.Unlock()
				continue
			}
			p := join(dir, name)
			if name == "" {
				p = dir
			}
			if p != "" && w.ignore(p) {
				continue
			}
			if ev.Mask&syscall.IN_ISDIR != 0 && ev.Mask&(syscall.IN_CREATE|syscall.IN_MOVED_TO) != 0 {
				// A new folder: watch it, then report what is already inside (files created
				// before the watch existed would otherwise be missed).
				_ = w.addTree(p)
				w.emitTree(p)
			}
			w.emit(p)
		}
	}
}

func (w *inotifyWatcher) emitTree(dir string) {
	_ = filepath.WalkDir(filepath.Join(w.root, filepath.FromSlash(dir)), func(full string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		r, e := filepath.Rel(w.root, full)
		if e != nil {
			return nil
		}
		r = filepath.ToSlash(r)
		if r != dir && !w.ignore(r) {
			w.emit(r)
		}
		return nil
	})
}
