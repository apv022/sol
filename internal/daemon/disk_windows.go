//go:build windows

package daemon

import (
	"syscall"
	"unsafe"
)

var getDiskFreeSpaceEx = syscall.NewLazyDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW")

// diskSpace returns the bytes available to this user and the total bytes of the volume holding path.
func diskSpace(path string) (free, total uint64, ok bool) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, false
	}
	var avail, tot, spare uint64
	r, _, _ := getDiskFreeSpaceEx.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&avail)),
		uintptr(unsafe.Pointer(&tot)), uintptr(unsafe.Pointer(&spare)))
	if r == 0 {
		return 0, 0, false
	}
	return avail, tot, true
}
