package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestResolveRepoRoot_ToolParamWinsOverFlag(t *testing.T) {
	t.Cleanup(resetRepoRootTestHooks(t))

	execCommand = func(name string, args ...string) *exec.Cmd {
		t.Fatalf("execCommand should not be called when toolParam is set")
		return nil
	}
	getwd = func() (string, error) {
		t.Fatalf("getwd should not be called when toolParam is set")
		return "", nil
	}

	got, err := ResolveRepoRoot("  /explicit  ", "  /tool  ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "/tool" {
		t.Fatalf("got %q, want %q", got, "/tool")
	}
}

func TestResolveRepoRoot_GitRootDetected(t *testing.T) {
	t.Cleanup(resetRepoRootTestHooks(t))

	dir := t.TempDir()
	gitRoot := filepath.Join(dir, "repo")
	if err := os.MkdirAll(gitRoot, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Use real git for this test to validate integration with `git rev-parse`.
	execCommand = realExecCommand

	old, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	if err := os.Chdir(gitRoot); err != nil {
		t.Fatalf("chdir: %v", err)
	}
	t.Cleanup(func() { _ = os.Chdir(old) })

	if err := runGit(gitRoot, "init"); err != nil {
		t.Fatalf("git init: %v", err)
	}

	got, err := ResolveRepoRoot("", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	gotEval, err := filepath.EvalSymlinks(got)
	if err != nil {
		t.Fatalf("evalsymlinks(got): %v", err)
	}
	wantEval, err := filepath.EvalSymlinks(gitRoot)
	if err != nil {
		t.Fatalf("evalsymlinks(want): %v", err)
	}
	if filepath.Clean(gotEval) != filepath.Clean(wantEval) {
		t.Fatalf("got %q, want %q", gotEval, wantEval)
	}
}

func TestResolveRepoRoot_CwdFallbackWhenNotGitRepo(t *testing.T) {
	t.Cleanup(resetRepoRootTestHooks(t))

	dir := t.TempDir()

	// Ensure git detection fails.
	execCommand = func(name string, args ...string) *exec.Cmd {
		cmd := realExecCommand(name, args...)
		// Point at an empty temp dir with no .git to ensure rev-parse fails.
		cmd.Dir = dir
		return cmd
	}
	getwd = func() (string, error) { return dir, nil }

	got, err := ResolveRepoRoot("", "")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	gotEval, err := filepath.EvalSymlinks(got)
	if err != nil {
		t.Fatalf("evalsymlinks(got): %v", err)
	}
	wantEval, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("evalsymlinks(want): %v", err)
	}
	if filepath.Clean(gotEval) != filepath.Clean(wantEval) {
		t.Fatalf("got %q, want %q", gotEval, wantEval)
	}
}

func TestResolveRepoRoot_ErrorWhenCwdUnavailable(t *testing.T) {
	t.Cleanup(resetRepoRootTestHooks(t))

	execCommand = func(name string, args ...string) *exec.Cmd {
		// Simulate git failing.
		cmd := realExecCommand(name, args...)
		cmd.Dir = t.TempDir()
		return cmd
	}
	getwd = func() (string, error) { return "", os.ErrNotExist }

	_, err := ResolveRepoRoot("", "")
	if err == nil {
		t.Fatalf("expected error, got nil")
	}
}

// --- test helpers ---

func resetRepoRootTestHooks(t *testing.T) func() {
	origExec := execCommand
	origGetwd := getwd
	return func() {
		execCommand = origExec
		getwd = origGetwd
	}
}

func realExecCommand(name string, args ...string) *exec.Cmd {
	return exec.Command(name, args...)
}

func runGit(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	return cmd.Run()
}
