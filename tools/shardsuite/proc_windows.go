//go:build windows

package main

import (
	"os/exec"
	"strconv"
)

func setProcGroup(*exec.Cmd) {}

// taskkill /T also ends the test binary that go test started.
func killTree(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := exec.Command("taskkill", "/T", "/F", "/PID", strconv.Itoa(cmd.Process.Pid)).Run(); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}
