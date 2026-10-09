package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/store"
)

func TestRecoverScansCommand(t *testing.T) {
	repoRoot := t.TempDir()
	home := t.TempDir()
	t.Setenv("CODEGRAPH_HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, "config", "config.json"), []byte("{\n  \"db_dir\": \"repo\"\n}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "main.go"), []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prev := startupVersionCheck
	startupVersionCheck = func(context.Context, io.Writer) {}
	t.Cleanup(func() { startupVersionCheck = prev })
	ctx := context.Background()
	var out, errOut bytes.Buffer

	if err := Run(ctx, []string{"recover-scans", repoRoot}, &out, &errOut); err == nil || !strings.Contains(err.Error(), "not indexed") {
		t.Fatalf("unindexed repo: err=%v", err)
	}
	dbPath := filepath.Join(repoRoot, ".codegraph", store.RepoDatabaseFileName)
	if _, err := os.Stat(dbPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("recover-scans created a database: %v", err)
	}

	if err := Run(ctx, []string{"index", repoRoot}, &out, &errOut); err != nil {
		t.Fatal(err)
	}
	recover := func() ([]int64, error) {
		out.Reset()
		if err := Run(ctx, []string{"recover-scans", repoRoot}, &out, &errOut); err != nil {
			return nil, err
		}
		var got struct {
			Recovered []int64 `json:"recovered_scans"`
		}
		if err := json.Unmarshal(out.Bytes(), &got); err != nil {
			t.Fatalf("output %q: %v", out.String(), err)
		}
		return got.Recovered, nil
	}
	if ids, err := recover(); err != nil || ids == nil || len(ids) != 0 {
		t.Fatalf("clean index: ids=%v err=%v output=%s", ids, err, out.String())
	}

	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	repo, err := s.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	live, _, err := s.BeginScan(ctx, repo.ID, "update")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recover(); !errors.Is(err, store.ErrScanActive) {
		t.Fatalf("live scan: %v", err)
	}
	if err := s.Close(); err != nil { // the owner goes away without closing its row
		t.Fatal(err)
	}
	if ids, err := recover(); err != nil || !slices.Equal(ids, []int64{live}) {
		t.Fatalf("abandoned scan: ids=%v err=%v", ids, err)
	}
}
