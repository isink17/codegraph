//go:build windows

package main

import "testing"

// Not checked on Windows: there is no portable signal-0 probe.
func assertGone(*testing.T, int) {}
