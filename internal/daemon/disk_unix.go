//go:build linux || darwin || freebsd

package daemon

import "syscall"

// diskSpace returns the bytes available to this user (Bavail, not Bfree: the root-reserved
// blocks are not ours to use) and the total bytes of the filesystem holding path.
func diskSpace(path string) (free, total uint64, ok bool) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0, false
	}
	bs := uint64(st.Bsize)
	return uint64(st.Bavail) * bs, uint64(st.Blocks) * bs, true
}
