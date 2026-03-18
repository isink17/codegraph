package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunInstallCreatesConfigAndPrintsSnippets(t *testing.T) {
	home := filepath.Join(t.TempDir(), "codegraph-home")
	t.Setenv("CODEGRAPH_HOME", home)
	prev := startupVersionCheck
	startupVersionCheck = func(context.Context, io.Writer) {}
	t.Cleanup(func() {
		startupVersionCheck = prev
	})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := Run(context.Background(), []string{"install"}, &stdout, &stderr); err != nil {
		t.Fatalf("Run(install) error = %v", err)
	}

	configPath := filepath.Join(home, "config", "config.json")
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("expected config file at %q: %v", configPath, err)
	}

	out := stdout.String()
	for _, needle := range []string{
		"codegraph install complete",
		"Codex MCP snippet:",
		"Gemini CLI MCP snippet:",
		"Claude/Desktop MCP snippet:",
		"If `codegraph` is not found after `go install`",
	} {
		if !strings.Contains(out, needle) {
			t.Fatalf("install output missing %q\noutput:\n%s", needle, out)
		}
	}
}

func TestRunFindSymbolQueryCommand(t *testing.T) {
	home := filepath.Join(t.TempDir(), "codegraph-home")
	t.Setenv("CODEGRAPH_HOME", home)
	repoRoot := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repoRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll(repoRoot) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "main.go"), []byte(`package main
func HelloWorld() {}
`), 0o644); err != nil {
		t.Fatalf("WriteFile(main.go) error = %v", err)
	}
	prev := startupVersionCheck
	startupVersionCheck = func(context.Context, io.Writer) {}
	t.Cleanup(func() {
		startupVersionCheck = prev
	})

	var out bytes.Buffer
	var errOut bytes.Buffer
	if err := Run(context.Background(), []string{"index", repoRoot}, &out, &errOut); err != nil {
		t.Fatalf("Run(index) error = %v", err)
	}

	out.Reset()
	if err := Run(context.Background(), []string{"find-symbol", repoRoot, "HelloWorld"}, &out, &errOut); err != nil {
		t.Fatalf("Run(find-symbol) error = %v", err)
	}
	if !strings.Contains(out.String(), "HelloWorld") {
		t.Fatalf("find-symbol output missing symbol, output:\n%s", out.String())
	}
}

func TestRunIndexJSONL(t *testing.T) {
	home := filepath.Join(t.TempDir(), "codegraph-home")
	t.Setenv("CODEGRAPH_HOME", home)
	repoRoot := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repoRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll(repoRoot) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "main.go"), []byte(`package main
func main() {}
`), 0o644); err != nil {
		t.Fatalf("WriteFile(main.go) error = %v", err)
	}
	prev := startupVersionCheck
	startupVersionCheck = func(context.Context, io.Writer) {}
	t.Cleanup(func() {
		startupVersionCheck = prev
	})

	var out bytes.Buffer
	var errOut bytes.Buffer
	if err := Run(context.Background(), []string{"index", repoRoot, "--jsonl"}, &out, &errOut); err != nil {
		t.Fatalf("Run(index --jsonl) error = %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected at least 2 jsonl lines, got %d: %q", len(lines), out.String())
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("first line unmarshal error = %v", err)
	}
	if first["type"] != "scan_summary" {
		t.Fatalf("first event type = %v, want scan_summary", first["type"])
	}
}
