//go:build linux || darwin || freebsd

package daemon

import "syscall"

// diskUsage returns the used and total bytes of the filesystem holding path.
func diskUsage(path string) (used, total uint64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, false
	}
	bs := uint64(st.Bsize)
	total = uint64(st.Blocks) * bs
	return total - uint64(st.Bfree)*bs, total, true
}
