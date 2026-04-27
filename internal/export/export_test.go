package export

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/query"
	"github.com/isink17/codegraph/internal/store"
)

func TestJSONAndDOTIncludeGraphData(t *testing.T) {
	ctx := context.Background()
	repoRoot := t.TempDir()
	writeFile(t, filepath.Join(repoRoot, "main.go"), `package main
func helper() {}
func main() { helper() }
`)

	s, err := store.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	defer s.Close()
	idx := indexer.New(s, parser.NewRegistry(goparser.New()), nil)
	repo, err := s.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	if _, err := idx.Index(ctx, indexer.Options{RepoRoot: repoRoot}); err != nil {
		t.Fatalf("Index() error = %v", err)
	}

	svc := New(query.New(s, nil))
	jsonOut, err := svc.JSON(ctx, repo.ID)
	if err != nil {
		t.Fatalf("JSON() error = %v", err)
	}
	if !strings.Contains(string(jsonOut), `"symbols"`) {
		t.Fatalf("JSON output missing symbols section: %s", string(jsonOut))
	}
	if !strings.Contains(string(jsonOut), `"edges"`) {
		t.Fatalf("JSON output missing edges section: %s", string(jsonOut))
	}

	dotOut, err := svc.DOT(ctx, repo.ID, "", 0)
	if err != nil {
		t.Fatalf("DOT() error = %v", err)
	}
	if !strings.Contains(string(dotOut), "->") {
		t.Fatalf("DOT output missing edge rendering: %s", string(dotOut))
	}
}

// TestJSONPagedNoFocusUsesPagingHelpers verifies that the bounded-page
// no-focus path (`symbol == "" && limit > 0`) returns at most `limit`
// symbols/edges and that subsequent pages cover disjoint ranges. This is the
// path that previously materialized the entire repo via GraphSnapshot before
// slicing client-side.
func TestJSONPagedNoFocusUsesPagingHelpers(t *testing.T) {
	ctx := context.Background()
	repoRoot := t.TempDir()
	for i := 0; i < 8; i++ {
		writeFile(t, filepath.Join(repoRoot, fmt.Sprintf("file_%d.go", i)),
			fmt.Sprintf("package main\nfunc helper%d() {}\nfunc main%d() { helper%d() }\n", i, i, i))
	}

	s, err := store.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	defer s.Close()
	idx := indexer.New(s, parser.NewRegistry(goparser.New()), nil)
	repo, err := s.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	if _, err := idx.Index(ctx, indexer.Options{RepoRoot: repoRoot}); err != nil {
		t.Fatalf("Index() error = %v", err)
	}

	svc := New(query.New(s, nil))

	page0, err := svc.JSONPaged(ctx, repo.ID, "", 0, 4, 0)
	if err != nil {
		t.Fatalf("JSONPaged(page0) error = %v", err)
	}
	page1, err := svc.JSONPaged(ctx, repo.ID, "", 0, 4, 4)
	if err != nil {
		t.Fatalf("JSONPaged(page1) error = %v", err)
	}

	parsed0 := struct {
		Symbols []map[string]any `json:"symbols"`
	}{}
	parsed1 := struct {
		Symbols []map[string]any `json:"symbols"`
	}{}
	if err := json.Unmarshal(page0, &parsed0); err != nil {
		t.Fatalf("Unmarshal page0: %v", err)
	}
	if err := json.Unmarshal(page1, &parsed1); err != nil {
		t.Fatalf("Unmarshal page1: %v", err)
	}
	if len(parsed0.Symbols) == 0 || len(parsed0.Symbols) > 4 {
		t.Fatalf("page0 symbol count = %d, want 1..4", len(parsed0.Symbols))
	}
	if len(parsed1.Symbols) > 4 {
		t.Fatalf("page1 symbol count = %d, want <=4", len(parsed1.Symbols))
	}
	// Pages must not overlap on stable_key.
	keys := map[string]bool{}
	for _, sym := range parsed0.Symbols {
		if k, ok := sym["stable_key"].(string); ok && k != "" {
			keys[k] = true
		}
	}
	for _, sym := range parsed1.Symbols {
		if k, ok := sym["stable_key"].(string); ok && k != "" && keys[k] {
			t.Fatalf("page1 overlaps page0 on stable_key %q", k)
		}
	}
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
