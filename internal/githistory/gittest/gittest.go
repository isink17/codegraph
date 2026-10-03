// Package gittest builds real throwaway Git repositories for tests. Commits
// get deterministic, strictly increasing author and committer dates so every
// SHA, and therefore every aggregate, is reproducible.
package gittest

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// Repo is a Git working tree under a test temp directory.
type Repo struct {
	T    testing.TB
	Dir  string
	tick int
}

// Require skips the test when Git is not installed and isolates Git from the
// developer's global and system configuration (signing, hooks, aliases).
func Require(t testing.TB) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed: history tests need the git CLI")
	}
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
}

// Init creates a repository with an initial branch named main.
func Init(t testing.TB) *Repo {
	t.Helper()
	Require(t)
	r := &Repo{T: t, Dir: t.TempDir()}
	r.Git("init", "-q", "-b", "main")
	return r
}

// Git runs git in the repository and returns trimmed stdout.
func (r *Repo) Git(args ...string) string {
	r.T.Helper()
	return r.GitEnv(nil, args...)
}

// GitEnv runs git with extra environment variables.
func (r *Repo) GitEnv(env []string, args ...string) string {
	r.T.Helper()
	cmd := exec.Command("git", append([]string{"-C", r.Dir}, args...)...)
	date := fmt.Sprintf("%d +0000", 1_700_000_000+r.tick*60)
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Alice", "GIT_AUTHOR_EMAIL=alice@example.com",
		"GIT_COMMITTER_NAME=Alice", "GIT_COMMITTER_EMAIL=alice@example.com",
		"GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
	cmd.Env = append(cmd.Env, env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		r.T.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// Write writes a file relative to the repository root.
func (r *Repo) Write(rel, content string) {
	r.T.Helper()
	path := filepath.Join(r.Dir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		r.T.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		r.T.Fatal(err)
	}
}

// Commit stages everything and commits as the given author email (empty for
// the default), advancing the clock by one minute. It returns the new SHA.
func (r *Repo) Commit(author, message string) string {
	r.T.Helper()
	r.tick++
	r.Git("add", "-A")
	var env []string
	if author != "" {
		env = append(env, "GIT_AUTHOR_EMAIL="+author)
	}
	r.GitEnv(env, "commit", "-q", "--allow-empty", "-m", message)
	return r.Git("rev-parse", "HEAD")
}

// Tick advances the commit clock, for commits made through Git directly.
func (r *Repo) Tick() { r.tick++ }
