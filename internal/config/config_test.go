package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMigratesLegacyGlobalDBDirToRepo(t *testing.T) {
	home := filepath.Join(t.TempDir(), "codegraph-home")
	t.Setenv("CODEGRAPH_HOME", home)

	cfgDir := filepath.Join(home, "config")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatalf("MkdirAll(config) error = %v", err)
	}
	legacyDBDir := filepath.Join(home, "data", "db")
	configJSON := "{\n  \"db_dir\": \"" + filepath.ToSlash(legacyDBDir) + "\"\n}\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "config.json"), []byte(configJSON), 0o644); err != nil {
		t.Fatalf("WriteFile(config.json) error = %v", err)
	}

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if cfg.DBDir != RepoDBDir {
		t.Fatalf("DBDir = %q, want %q", cfg.DBDir, RepoDBDir)
	}
}
