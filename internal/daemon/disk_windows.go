//go:build windows

package daemon

import (
	"syscall"
	"unsafe"
)

var getDiskFreeSpaceEx = syscall.NewLazyDLL("kernel32.dll").NewProc("GetDiskFreeSpaceExW")

// diskUsage returns the used and total bytes of the volume holding path.
func diskUsage(path string) (used, total uint64, ok bool) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, 0, false
	}
	var avail, tot, free uint64
	r, _, _ := getDiskFreeSpaceEx.Call(uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&avail)),
		uintptr(unsafe.Pointer(&tot)), uintptr(unsafe.Pointer(&free)))
	if r == 0 {
		return 0, 0, false
	}
	return tot - free, tot, true
}
