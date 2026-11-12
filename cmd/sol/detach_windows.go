//go:build windows

package main

import (
	"os/exec"
	"syscall"
)

// Process creation flags (winbase.h).
const (
	detachedProcess       = 0x00000008
	createNewProcessGroup = 0x00000200
)

// detach makes cmd outlive this process and its console window.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: detachedProcess | createNewProcessGroup, HideWindow: true}
}
