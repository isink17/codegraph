package config

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

var (
	execCommand = exec.Command
	getwd       = os.Getwd
)

// ResolveRepoRoot resolves the repository root directory using a stable fallback chain:
//  1. Per-call MCP tool parameter (toolParam) (most specific override)
//  2. Explicit CLI flag value (flagValue) (process-level default)
//  3. `git rev-parse --show-toplevel` from the current working directory
//  4. os.Getwd()
//  5. Return error
//
// Empty inputs are treated as "not provided".
func ResolveRepoRoot(flagValue, toolParam string) (string, error) {
	if v := strings.TrimSpace(toolParam); v != "" {
		return v, nil
	}
	if v := strings.TrimSpace(flagValue); v != "" {
		return v, nil
	}

	cmd := execCommand("git", "rev-parse", "--show-toplevel")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = new(bytes.Buffer)
	if err := cmd.Run(); err == nil {
		if v := strings.TrimSpace(stdout.String()); v != "" {
			return v, nil
		}
	}

	wd, err := getwd()
	if err == nil {
		if v := strings.TrimSpace(wd); v != "" {
			return v, nil
		}
	}

	return "", fmt.Errorf("unable to resolve repo root: provide --repo-root or repo_root, or run from a git repo")
}
