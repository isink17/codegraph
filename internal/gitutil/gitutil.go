package gitutil

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"strings"
)

func DiffPaths(ctx context.Context, repoRoot, base string) ([]string, error) {
	if base == "" {
		return nil, nil
	}
	cmd := exec.CommandContext(ctx, "git", "-C", repoRoot, "diff", "--name-only", base, "--")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &bytes.Buffer{}
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	var paths []string
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		paths = append(paths, filepath.Clean(line))
	}
	return paths, nil
}
