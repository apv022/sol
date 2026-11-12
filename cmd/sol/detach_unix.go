//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// detach makes cmd outlive this process and its terminal: a new session has
// no controlling terminal, so closing the terminal doesn't signal it.
func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
