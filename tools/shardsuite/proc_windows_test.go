//go:build windows

package main

import "testing"

// Unused: the fixture tests skip on Windows, which the runner refuses.
func assertGone(*testing.T, int) {}
