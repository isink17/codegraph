//go:build !windows

package watcher

func sharingViolation(error) bool { return false }
