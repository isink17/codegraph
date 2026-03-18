package platform

import (
	"path/filepath"
	"testing"
)

func TestDefaultPathsUsesCodegraphHomeOverride(t *testing.T) {
	base := filepath.Join(t.TempDir(), "cg-home")
	t.Setenv("CODEGRAPH_HOME", base)

	paths, err := DefaultPaths()
	if err != nil {
		t.Fatalf("DefaultPaths() error = %v", err)
	}

	if got, want := paths.ConfigDir, filepath.Join(base, "config"); got != want {
		t.Fatalf("ConfigDir = %q, want %q", got, want)
	}
	if got, want := paths.DataDir, filepath.Join(base, "data"); got != want {
		t.Fatalf("DataDir = %q, want %q", got, want)
	}
	if got, want := paths.CacheDir, filepath.Join(base, "cache"); got != want {
		t.Fatalf("CacheDir = %q, want %q", got, want)
	}
}
