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

func TestIndex_DeniedExtensionsAreSeenButNeverIndexed(t *testing.T) {
	ctx := context.Background()
	repoRoot := t.TempDir()

	writeFile(t, filepath.Join(repoRoot, "build.vcxproj"), `<Project></Project>`)
	writeFile(t, filepath.Join(repoRoot, "rules.props"), `<Project></Project>`)
	writeFile(t, filepath.Join(repoRoot, "resource.rc"), `#define IDD_ABOUTBOX 100`)
	writeFile(t, filepath.Join(repoRoot, "exports.def"), `LIBRARY Foo`)
	writeFile(t, filepath.Join(repoRoot, "inline.inl"), `inline int x() { return 1; }`)

	// Denied extensions: the content doesn't matter; they must never be indexed.
	writeFile(t, filepath.Join(repoRoot, "logo.png"), "not really a png")
	writeFile(t, filepath.Join(repoRoot, "image.tif"), "not really a tif")
	writeFile(t, filepath.Join(repoRoot, "engine.lib"), "not really a lib")
	writeFile(t, filepath.Join(repoRoot, "runtime.dll"), "not really a dll")

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
	if summary.FilesSeen != 9 {
		t.Fatalf("FilesSeen = %d, want 9", summary.FilesSeen)
	}
	if summary.FilesIndexed != 5 {
		t.Fatalf("FilesIndexed = %d, want 5", summary.FilesIndexed)
	}
	if summary.FilesSkipped != 4 {
		t.Fatalf("FilesSkipped = %d, want 4", summary.FilesSkipped)
	}

	unknown, ok := summary.LanguageCoverage["unknown"]
	if !ok {
		t.Fatalf("LanguageCoverage[unknown] missing")
	}
	if unknown.Extensions == nil {
		t.Fatalf("unknown.Extensions is nil")
	}
	for _, ext := range []string{".png", ".tif", ".lib", ".dll"} {
		c := unknown.Extensions[ext]
		if c.Seen != 1 || c.Skipped != 1 || c.Indexed != 0 {
			t.Fatalf("unknown.Extensions[%s]=%+v, want seen=1 skipped=1 indexed=0", ext, c)
		}
	}

	repo, err := s.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	stats, err := s.Stats(ctx, repo.ID)
	if err != nil {
		t.Fatalf("Stats() error = %v", err)
	}
	if stats.Files != 5 {
		t.Fatalf("stats.Files = %d, want 5", stats.Files)
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

func TestOwnModuleImportLifecycleAndFullUpdateParity(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeFile(t, filepath.Join(root, "go.mod"), "module example.com/project\n")
	writeFile(t, filepath.Join(root, "cmd", "main.go"), "package main\n\nimport \"example.com/project/pkg\"\n\nfunc main() { pkg.New() }\n")
	writeFile(t, filepath.Join(root, "pkg", "target.go"), "package pkg\nfunc New() {}\n")
	writeFile(t, filepath.Join(root, "other", "target.go"), "package other\nfunc New() {}\n")

	open := func() (*store.Store, *Indexer) {
		s, err := store.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		return s, New(s, parser.NewRegistry(goparser.New()), nil)
	}
	s, idx := open()
	defer s.Close()
	if _, err := idx.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	repo, err := s.UpsertRepo(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	callees, err := s.FindCallees(ctx, repo.ID, "main", 0, 20, 0)
	if err != nil || len(callees) != 1 || callees[0].QualifiedName != "pkg.New" {
		t.Fatalf("initial callees = (%v, %v), want pkg.New", callees, err)
	}
	if err := os.Remove(filepath.Join(root, "pkg", "target.go")); err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Update(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	if unresolved, err := s.CountUnresolvedEdgesByDstName(ctx, repo.ID, "example.com/project/pkg.New"); err != nil || unresolved != 1 {
		t.Fatalf("after deletion unresolved edge count = (%d, %v), want 1", unresolved, err)
	}
	writeFile(t, filepath.Join(root, "pkg", "target.go"), "package pkg\nfunc New() {}\n")
	if _, err := idx.Update(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	callees, err = s.FindCallees(ctx, repo.ID, "main", 0, 20, 0)
	if err != nil || len(callees) != 1 || callees[0].QualifiedName != "pkg.New" {
		t.Fatalf("after recreation callees = (%v, %v), want pkg.New", callees, err)
	}
	fresh, freshIdx := open()
	defer fresh.Close()
	if _, err := freshIdx.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	freshRepo, err := fresh.UpsertRepo(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	freshCallees, err := fresh.FindCallees(ctx, freshRepo.ID, "main", 0, 20, 0)
	if err != nil || len(freshCallees) != len(callees) || freshCallees[0].QualifiedName != callees[0].QualifiedName {
		t.Fatalf("full/update callees differ: update=%v full=%v err=%v", callees, freshCallees, err)
	}
}
