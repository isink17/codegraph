//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// go test runs each test binary as its own child; a process group lets one
// kill reach the binary too, not only the go command.
func setProcGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

func killTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}
