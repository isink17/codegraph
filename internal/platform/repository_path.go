package platform

import (
	"errors"
	"path"
	"path/filepath"
	"runtime"
	"strings"
)

var ErrInvalidRepositoryPath = errors.New("invalid repository-relative path")

// LogicalRepositoryPath validates a persisted logical repository path. It
// deliberately does not clean or translate separators: storage is identity.
func LogicalRepositoryPath(p string) (string, error) {
	if p == "" || p == "." || strings.ContainsRune(p, 0) || strings.HasPrefix(p, "/") || strings.HasPrefix(p, "//") || strings.HasSuffix(p, "/") || strings.Contains(p, "//") || filepath.VolumeName(p) != "" || isWindowsAbsoluteSpelling(p) {
		return "", ErrInvalidRepositoryPath
	}
	for _, part := range strings.Split(p, "/") {
		if part == "" || part == "." || part == ".." {
			return "", ErrInvalidRepositoryPath
		}
	}
	return p, nil
}

// PublicRepositoryPath accepts the host's public relative spelling once, then
// returns the logical storage identity. On POSIX a backslash remains data.
func PublicRepositoryPath(p string) (string, error) {
	if runtime.GOOS == "windows" {
		p = filepath.ToSlash(filepath.Clean(p))
	} else {
		p = path.Clean(p)
	}
	if p == "." {
		return "", ErrInvalidRepositoryPath
	}
	return LogicalRepositoryPath(p)
}

// NativeRelativeToLogical converts a host-native relative filesystem path at
// the traversal/watcher boundary. It is deliberately separate from public
// input compatibility.
func NativeRelativeToLogical(p string) (string, error) {
	p = filepath.Clean(p)
	if p == "." || filepath.IsAbs(p) || filepath.VolumeName(p) != "" {
		return "", ErrInvalidRepositoryPath
	}
	return LogicalRepositoryPath(filepath.ToSlash(p))
}

func isWindowsAbsoluteSpelling(p string) bool {
	return len(p) >= 2 && ((p[0] >= 'A' && p[0] <= 'Z') || (p[0] >= 'a' && p[0] <= 'z')) && p[1] == ':' || strings.HasPrefix(p, `\\`)
}

// NativePath joins a logical path to root for filesystem use only.
func NativePath(root, logical string) (string, error) {
	logical, err := LogicalRepositoryPath(logical)
	if err != nil {
		return "", err
	}
	return filepath.Join(root, filepath.FromSlash(logical)), nil
}
