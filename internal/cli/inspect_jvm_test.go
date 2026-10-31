package cli

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestInspectJVMHelpDoesNotLoadConfigOrRequireJava(t *testing.T) {
	t.Setenv("CODEGRAPH_HOME", filepath.Join(t.TempDir(), "invalid-config"))
	if err := os.MkdirAll(filepath.Join(os.Getenv("CODEGRAPH_HOME"), "config"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(os.Getenv("CODEGRAPH_HOME"), "config", "config.json"), []byte("{invalid"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "")
	previous := startupVersionCheck
	startupVersionCheck = func(context.Context, io.Writer) { t.Fatal("help ran release check") }
	t.Cleanup(func() { startupVersionCheck = previous })

	var stdout bytes.Buffer
	if err := Run(context.Background(), []string{"inspect-jvm-class", "--help"}, &stdout, io.Discard); err != nil {
		t.Fatalf("inspect help error = %v", err)
	}
	for _, text := range []string{"Java 17 executable", "--trust-inspector", "external inspector"} {
		if !strings.Contains(stdout.String(), text) {
			t.Fatalf("inspect help missing %q: %s", text, stdout.String())
		}
	}
}

func TestOrdinaryIndexWorksWithoutJavaOrInspector(t *testing.T) {
	repo := t.TempDir()
	if err := os.WriteFile(filepath.Join(repo, "main.go"), []byte("package main\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "")
	previous := startupVersionCheck
	startupVersionCheck = func(context.Context, io.Writer) {}
	t.Cleanup(func() { startupVersionCheck = previous })

	var stdout bytes.Buffer
	if err := Run(context.Background(), []string{"index", repo, "--no-history"}, &stdout, io.Discard); err != nil {
		t.Fatalf("index without Java or helper error = %v", err)
	}
	if !strings.Contains(stdout.String(), "files_seen") {
		t.Fatalf("index output did not report the scan: %s", stdout.String())
	}
}

func TestInspectCommandRequiresTrustAcknowledgement(t *testing.T) {
	err := Run(context.Background(), []string{"inspect-jvm-class", "--input", "unused.class"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "--trust-inspector") {
		t.Fatalf("inspect without trust acknowledgement error = %v", err)
	}
}
