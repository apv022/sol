//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// detach puts the child in its own session, so closing the terminal or Ctrl-C in it does not
// reach the daemon.
func detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }

// terminate asks the process to stop (the SIGTERM fallback of sol stop).
func terminate(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }
