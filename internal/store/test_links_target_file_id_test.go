package store_test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/store"
)

// TestIndexer_TestLinksPopulateTargetFileID drives the real indexer over a
// tiny pkg/_test.go pair and asserts that test_links rows store
// target_file_id pointing at the target file. Without this column being
// populated, the purge-time DELETE in nullifyDeletedSymbolReferences cannot
// match (target_file_id IS NULL) and ghost test associations leak — and
// RelatedTests(file=...) will not even return live associations because it
// joins on target_file_id.
func TestIndexer_TestLinksPopulateTargetFileID(t *testing.T) {
	ctx := context.Background()
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, "helper.go"), []byte(`package pkg

func Helper() {}
`), 0o644); err != nil {
		t.Fatalf("write helper.go: %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "helper_test.go"), []byte(`package pkg

import "testing"

func TestHelper(t *testing.T) { Helper() }
`), 0o644); err != nil {
		t.Fatalf("write helper_test.go: %v", err)
	}

	dbPath := filepath.Join(t.TempDir(), "graph.sqlite")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	defer s.Close()

	idx := indexer.New(s, parser.NewRegistry(goparser.New()), nil)
	if _, err := idx.Index(ctx, indexer.Options{RepoRoot: repoRoot}); err != nil {
		t.Fatalf("Index() error = %v", err)
	}
	repo, err := s.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}

	// RelatedTests(file=helper.go) joins on test_links.target_file_id, so a
	// non-empty result proves target_file_id is populated end-to-end.
	got, err := s.RelatedTests(ctx, repo.ID, "", "helper.go", 10, 0)
	if err != nil {
		t.Fatalf("RelatedTests(file=helper.go) error = %v", err)
	}
	if len(got) == 0 {
		t.Fatalf("RelatedTests(file=helper.go) returned 0 rows; expected at least one (target_file_id likely NULL)")
	}
	foundTestFile := false
	for _, r := range got {
		if r.File == "helper_test.go" {
			foundTestFile = true
			break
		}
	}
	if !foundTestFile {
		t.Fatalf("RelatedTests did not surface helper_test.go: %+v", got)
	}

	// End-to-end purge: deleting helper.go must drop the linked row, which
	// only works if target_file_id was populated so the purge DELETE matches.
	if err := os.Remove(filepath.Join(repoRoot, "helper.go")); err != nil {
		t.Fatalf("Remove(helper.go) error = %v", err)
	}
	if _, err := idx.Update(ctx, indexer.Options{RepoRoot: repoRoot}); err != nil {
		t.Fatalf("Update(after delete) error = %v", err)
	}
	gotAfter, err := s.RelatedTests(ctx, repo.ID, "", "helper.go", 10, 0)
	if err != nil {
		t.Fatalf("RelatedTests(file=helper.go) post-purge error = %v", err)
	}
	if len(gotAfter) != 0 {
		t.Fatalf("RelatedTests(file=helper.go) post-purge = %d rows, want 0: %+v", len(gotAfter), gotAfter)
	}
}
