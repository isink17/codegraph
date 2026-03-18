package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunInstallCreatesConfigAndPrintsSnippets(t *testing.T) {
	home := filepath.Join(t.TempDir(), "codegraph-home")
	t.Setenv("CODEGRAPH_HOME", home)

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
	} {
		if !strings.Contains(out, needle) {
			t.Fatalf("install output missing %q\noutput:\n%s", needle, out)
		}
	}
}
