//go:build !linux

package netwatch

import "context"

// watchOS has no push source here yet (route sockets on macOS and the BSDs,
// NotifyIpInterfaceChange on Windows are later work): Watch scans the addresses instead.
func watchOS(context.Context, func(string)) bool { return false }
