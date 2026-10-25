package main

import (
	"path"
	"path/filepath"
	"strings"
)

// isAbsolutePath accepts both the host spelling and the other platform's
// absolute spellings because manifest paths are portable input, not filesystem
// paths owned by the current host.
func isAbsolutePath(p string) bool {
	if path.IsAbs(p) || filepath.IsAbs(p) || filepath.VolumeName(p) != "" || strings.HasPrefix(p, `\`) {
		return true
	}
	return len(p) >= 3 && ((p[0] >= 'A' && p[0] <= 'Z') || (p[0] >= 'a' && p[0] <= 'z')) && p[1] == ':' && (p[2] == '/' || p[2] == '\\')
}
