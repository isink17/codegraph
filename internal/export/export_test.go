package export

import (
	"context"
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

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
}
