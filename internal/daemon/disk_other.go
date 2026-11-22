//go:build !linux && !darwin && !freebsd && !windows

package daemon

// diskSpace is unknown on this OS: the status leaves the device's numbers out.
func diskSpace(string) (free, total uint64, ok bool) { return 0, 0, false }
