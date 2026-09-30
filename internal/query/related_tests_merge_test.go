package query

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/store"
)

// One test reached from two seed files with different evidence: TestHelper is
// name-bound to Helper in a.go and calls Other in b.go. The merged row must be
// the strongest evidence (test_calls) whichever order the files are given in,
// and the whole answer must be byte-identical across input orders.
func TestRelatedTestsForFilesKeepsStrongestEvidenceRegardlessOfInputOrder(t *testing.T) {
	ctx := context.Background()
	repoRoot := t.TempDir()
	writeFixtureFile(t, repoRoot, "pkg/a.go", "package pkg\n\nfunc Helper() {}\n")
	writeFixtureFile(t, repoRoot, "pkg/b.go", "package pkg\n\nfunc Other() {}\n")
	writeFixtureFile(t, repoRoot, "pkg/a_test.go", "package pkg\n\nimport \"testing\"\n\nfunc TestHelper(t *testing.T) { Other() }\n")
	s, err := store.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if _, err := indexer.New(s, parser.NewRegistry(goparser.New()), nil).Index(ctx, indexer.Options{RepoRoot: repoRoot}); err != nil {
		t.Fatal(err)
	}
	repo, err := s.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	svc := New(s, nil)

	perSeed := map[string]string{}
	for _, f := range []string{"pkg/a.go", "pkg/b.go"} {
		got, err := svc.RelatedTests(ctx, repo.ID, "", f, 10, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range got {
			if r.Symbol == "pkg.TestHelper" {
				perSeed[f] = r.Reason
			}
		}
	}
	if perSeed["pkg/a.go"] == "" || perSeed["pkg/b.go"] == "" || perSeed["pkg/a.go"] == perSeed["pkg/b.go"] {
		t.Fatalf("fixture needs TestHelper reached from both seeds with different reasons, got %v", perSeed)
	}

	var want string
	for _, files := range [][]string{{"pkg/a.go", "pkg/b.go"}, {"pkg/b.go", "pkg/a.go"}, {"pkg/b.go", "pkg/a.go", "pkg/b.go"}} {
		got, err := svc.RelatedTestsForFilesResult(ctx, repo.ID, files, 10, 0)
		if err != nil {
			t.Fatal(err)
		}
		var helper *store.RelatedTest
		for i := range got.Tests {
			if got.Tests[i].Symbol == "pkg.TestHelper" {
				helper = &got.Tests[i]
			}
		}
		if helper == nil || helper.Reason != "test_calls" {
			t.Fatalf("files=%v merged TestHelper = %+v, want strongest test_calls", files, helper)
		}
		blob, err := json.Marshal(got.Tests)
		if err != nil {
			t.Fatal(err)
		}
		if want == "" {
			want = string(blob)
		} else if string(blob) != want {
			t.Fatalf("files=%v tests differ by input order:\nfirst: %s\nnow  : %s", files, want, blob)
		}
	}
}
