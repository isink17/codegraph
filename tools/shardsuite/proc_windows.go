//go:build windows

package main

import "os/exec"

// runSuite refuses Windows before starting any process; these only keep the
// package building there.
func setProcGroup(*exec.Cmd) {}

func killTree(cmd *exec.Cmd) error { return cmd.Process.Kill() }
