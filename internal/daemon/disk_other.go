//go:build !linux && !darwin && !freebsd && !windows

package daemon

// diskUsage is unknown on this OS: the status leaves storage out.
func diskUsage(string) (used, total uint64, ok bool) { return 0, 0, false }
