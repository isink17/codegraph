package doctor

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestRunReportsCodegraphOnPath(t *testing.T) {
	tmp := t.TempDir()
	name := "codegraph"
	if runtime.GOOS == "windows" {
		name = "codegraph.exe"
	}
	bin := filepath.Join(tmp, name)
	if err := os.WriteFile(bin, []byte(""), 0o755); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", bin, err)
	}
	t.Setenv("PATH", tmp)
	if runtime.GOOS == "windows" {
		t.Setenv("PATHEXT", ".EXE")
	}

	report, err := Run()
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !report.CodegraphOnPath {
		t.Fatalf("CodegraphOnPath = false, want true")
	}
	if filepath.Clean(report.CodegraphPath) != filepath.Clean(bin) {
		t.Fatalf("CodegraphPath = %q, want %q", report.CodegraphPath, bin)
	}
	if len(report.Recommendations) != 0 {
		t.Fatalf("Recommendations = %v, want empty", report.Recommendations)
	}
}

func TestRunReportsCodegraphMissingFromPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	if runtime.GOOS == "windows" {
		t.Setenv("PATHEXT", ".EXE")
	}

	report, err := Run()
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if report.CodegraphOnPath {
		t.Fatalf("CodegraphOnPath = true, want false")
	}
	if len(report.Recommendations) == 0 {
		t.Fatalf("Recommendations = empty, want guidance")
	}
}
