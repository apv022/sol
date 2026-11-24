package e2e

import "os"

// Test daemons (in process, and sol binaries started by tests, which inherit the environment)
// must never post real desktop notifications (internal/daemon/notify.go).
func init() { os.Setenv("SOL_NOTIFY", "off") }
