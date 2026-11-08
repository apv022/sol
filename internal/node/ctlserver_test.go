package node

import (
	"path/filepath"
	"strings"
	"testing"
)

// Regression: on macOS a long data directory made the control socket fail
// with a cryptic "bind: invalid argument". It must fail clearly instead, on
// every OS, before reaching the OS limit.
func TestControlSocketPathTooLong(t *testing.T) {
	long := filepath.Join("/tmp", strings.Repeat("d", 100), "control.sock")
	if _, err := listenControl(long); err == nil || !strings.Contains(err.Error(), "shorter data directory") {
		t.Fatalf("long socket path: %v", err)
	}
}
