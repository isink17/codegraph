package indexer

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/store"
)

func TestIndexAndIncrementalUpdate(t *testing.T) {
	ctx := context.Background()
	repoRoot := t.TempDir()
	writeFile(t, filepath.Join(repoRoot, "main.go"), `package main

func helper() {}

func main() {
	helper()
}
`)

	dbPath := filepath.Join(t.TempDir(), "graph.sqlite")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	defer s.Close()

	idx := New(s, parser.NewRegistry(goparser.New()), nil)

	summary, err := idx.Index(ctx, Options{RepoRoot: repoRoot})
	if err != nil {
		t.Fatalf("Index() error = %v", err)
	}
	if summary.FilesIndexed != 1 {
		t.Fatalf("FilesIndexed = %d, want 1", summary.FilesIndexed)
	}
	if summary.FilesChanged != 1 {
		t.Fatalf("FilesChanged = %d, want 1", summary.FilesChanged)
	}

	repo, err := s.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	stats, err := s.Stats(ctx, repo.ID)
	if err != nil {
		t.Fatalf("Stats() error = %v", err)
	}
	if stats.Files != 1 {
		t.Fatalf("stats.Files = %d, want 1", stats.Files)
	}
	if stats.Symbols < 2 {
		t.Fatalf("stats.Symbols = %d, want at least 2", stats.Symbols)
	}

	updateSummary, err := idx.Update(ctx, Options{RepoRoot: repoRoot})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if updateSummary.FilesSkipped != 1 {
		t.Fatalf("FilesSkipped = %d, want 1", updateSummary.FilesSkipped)
	}
	if updateSummary.FilesIndexed != 0 {
		t.Fatalf("FilesIndexed = %d, want 0", updateSummary.FilesIndexed)
	}

	time.Sleep(2 * time.Millisecond)
	writeFile(t, filepath.Join(repoRoot, "main.go"), `package main

func helper() {}

func main() {
	helper()
	helper()
}
`)

	modifiedSummary, err := idx.Update(ctx, Options{RepoRoot: repoRoot})
	if err != nil {
		t.Fatalf("Update(modified) error = %v", err)
	}
	if modifiedSummary.FilesIndexed != 1 {
		t.Fatalf("FilesIndexed after modification = %d, want 1", modifiedSummary.FilesIndexed)
	}
	if modifiedSummary.FilesChanged != 1 {
		t.Fatalf("FilesChanged after modification = %d, want 1", modifiedSummary.FilesChanged)
	}

	if err := os.Remove(filepath.Join(repoRoot, "main.go")); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	deletedSummary, err := idx.Update(ctx, Options{RepoRoot: repoRoot})
	if err != nil {
		t.Fatalf("Update(delete) error = %v", err)
	}
	if deletedSummary.FilesDeleted != 1 {
		t.Fatalf("FilesDeleted = %d, want 1", deletedSummary.FilesDeleted)
	}

	stats, err = s.Stats(ctx, repo.ID)
	if err != nil {
		t.Fatalf("Stats(after delete) error = %v", err)
	}
	if stats.Files != 0 {
		t.Fatalf("stats.Files after delete = %d, want 0", stats.Files)
	}
	if stats.Symbols != 0 {
		t.Fatalf("stats.Symbols after delete = %d, want 0", stats.Symbols)
	}
	if stats.References != 0 {
		t.Fatalf("stats.References after delete = %d, want 0", stats.References)
	}
	if stats.Edges != 0 {
		t.Fatalf("stats.Edges after delete = %d, want 0", stats.Edges)
	}
}

func TestIndexSkipsDotAndGeneratedDirectories(t *testing.T) {
	ctx := context.Background()
	repoRoot := t.TempDir()
	writeFile(t, filepath.Join(repoRoot, "main.go"), `package main

func main() {}
`)
	writeFile(t, filepath.Join(repoRoot, ".hidden", "ignored.go"), `package hidden
func Ignored() {}
`)
	writeFile(t, filepath.Join(repoRoot, "app", "build", "generated.go"), `package generated
func Generated() {}
`)

	dbPath := filepath.Join(t.TempDir(), "graph.sqlite")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	defer s.Close()

	idx := New(s, parser.NewRegistry(goparser.New()), nil)
	summary, err := idx.Index(ctx, Options{RepoRoot: repoRoot})
	if err != nil {
		t.Fatalf("Index() error = %v", err)
	}
	if summary.FilesIndexed != 2 {
		t.Fatalf("FilesIndexed = %d, want 2", summary.FilesIndexed)
	}

	repo, err := s.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	stats, err := s.Stats(ctx, repo.ID)
	if err != nil {
		t.Fatalf("Stats() error = %v", err)
	}
	if stats.Files != 2 {
		t.Fatalf("stats.Files = %d, want 2", stats.Files)
	}
}

func TestIndexSkipsLargeFilesByRepoConfig(t *testing.T) {
	ctx := context.Background()
	repoRoot := t.TempDir()
	writeFile(t, filepath.Join(repoRoot, ".codegraph", "config.json"), `{"max_file_size_bytes":64}`)
	writeFile(t, filepath.Join(repoRoot, "small.go"), `package main
func small() {}
`)
	writeFile(t, filepath.Join(repoRoot, "large.go"), "package main\n"+strings.Repeat("var X = 1\n", 64))

	dbPath := filepath.Join(t.TempDir(), "graph.sqlite")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	defer s.Close()

	idx := New(s, parser.NewRegistry(goparser.New()), nil)
	summary, err := idx.Index(ctx, Options{RepoRoot: repoRoot})
	if err != nil {
		t.Fatalf("Index() error = %v", err)
	}
	if summary.FilesSkipped == 0 {
		t.Fatalf("FilesSkipped = %d, want at least 1", summary.FilesSkipped)
	}
	if summary.FilesIndexed != 1 {
		t.Fatalf("FilesIndexed = %d, want 1", summary.FilesIndexed)
	}
}

func TestIndexBestEffortParseErrors(t *testing.T) {
	ctx := context.Background()
	repoRoot := t.TempDir()
	writeFile(t, filepath.Join(repoRoot, ".codegraph", "config.json"), `{"parse_error_policy":"best_effort"}`)
	writeFile(t, filepath.Join(repoRoot, "main.go"), `package main
func ok() {}
`)
	writeFile(t, filepath.Join(repoRoot, "broken.go"), `package main
func broken( {
`)

	dbPath := filepath.Join(t.TempDir(), "graph.sqlite")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	defer s.Close()

	idx := New(s, parser.NewRegistry(goparser.New()), nil)
	summary, err := idx.Index(ctx, Options{RepoRoot: repoRoot})
	if err != nil {
		t.Fatalf("Index() error = %v", err)
	}
	if summary.ParseErrors != 1 {
		t.Fatalf("ParseErrors = %d, want 1", summary.ParseErrors)
	}
	if len(summary.ParseSamples) == 0 {
		t.Fatalf("ParseSamples = %v, want at least one sample", summary.ParseSamples)
	}
	if summary.FilesIndexed != 1 {
		t.Fatalf("FilesIndexed = %d, want 1", summary.FilesIndexed)
	}
	cov := summary.LanguageCoverage["go"]
	if cov.Indexed != 1 || cov.ParseFailed != 1 {
		t.Fatalf("language coverage for go = %+v, want indexed=1 parse_failed=1", cov)
	}
}

func TestCodegraphIgnoreNegationUnignoresInsideSkippedDir(t *testing.T) {
	ctx := context.Background()
	repoRoot := t.TempDir()
	writeFile(t, filepath.Join(repoRoot, ".codegraphignore"), "vendor/**\n!vendor/keep.go\n")
	writeFile(t, filepath.Join(repoRoot, "vendor", "skip.go"), `package vendor
func Skip() {}
`)
	writeFile(t, filepath.Join(repoRoot, "vendor", "keep.go"), `package vendor
func Keep() {}
`)

	dbPath := filepath.Join(t.TempDir(), "graph.sqlite")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	defer s.Close()

	idx := New(s, parser.NewRegistry(goparser.New()), nil)
	if _, err := idx.Index(ctx, Options{RepoRoot: repoRoot}); err != nil {
		t.Fatalf("Index() error = %v", err)
	}

	repo, err := s.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	existing, err := s.ExistingFiles(ctx, repo.ID)
	if err != nil {
		t.Fatalf("ExistingFiles() error = %v", err)
	}
	if _, ok := existing[filepath.Clean(filepath.Join("vendor", "keep.go"))]; !ok {
		t.Fatalf("expected vendor/keep.go to be indexed, got keys: %v", mapKeys(existing))
	}
	if _, ok := existing[filepath.Clean(filepath.Join("vendor", "skip.go"))]; ok {
		t.Fatalf("expected vendor/skip.go to be ignored")
	}
}

func TestLanguageCoverageIncludesUnknownAndSkipped(t *testing.T) {
	ctx := context.Background()
	repoRoot := t.TempDir()
	writeFile(t, filepath.Join(repoRoot, ".codegraph", "config.json"), `{"max_file_size_bytes":64}`)
	writeFile(t, filepath.Join(repoRoot, "main.go"), `package main
func main() {}
`)
	writeFile(t, filepath.Join(repoRoot, "large.go"), "package main\n"+strings.Repeat("var X = 1\n", 64))
	writeFile(t, filepath.Join(repoRoot, "README.md"), strings.Repeat("docs ", 100))

	dbPath := filepath.Join(t.TempDir(), "graph.sqlite")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	defer s.Close()

	idx := New(s, parser.NewRegistry(goparser.New()), nil)
	summary, err := idx.Index(ctx, Options{RepoRoot: repoRoot})
	if err != nil {
		t.Fatalf("Index() error = %v", err)
	}
	goCov := summary.LanguageCoverage["go"]
	if goCov.Seen == 0 || goCov.Indexed == 0 || goCov.Skipped == 0 {
		t.Fatalf("go coverage = %+v, expected seen/indexed/skipped > 0", goCov)
	}
	unknownCov := summary.LanguageCoverage["unknown"]
	if unknownCov.Seen == 0 {
		t.Fatalf("unknown coverage = %+v, expected seen > 0", unknownCov)
	}
}

func mapKeys(m map[string]store.ExistingFileMeta) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
}
