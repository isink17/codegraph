//go:build !windows

package watcher

func isSharingViolation(error) bool { return false }
