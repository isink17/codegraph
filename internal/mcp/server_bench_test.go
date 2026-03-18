package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/query"
	"github.com/isink17/codegraph/internal/store"
)

func BenchmarkServerToolGraphStats(b *testing.B) {
	ctx := context.Background()
	server := setupMCPBenchServer(b, ctx)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := server.callTool(ctx, "graph_stats", json.RawMessage(`{}`))
		if err != nil {
			b.Fatalf("callTool(graph_stats) error = %v", err)
		}
	}
}

func BenchmarkServerToolSearchSymbols(b *testing.B) {
	ctx := context.Background()
	server := setupMCPBenchServer(b, ctx)
	raw := json.RawMessage(`{"query":"BenchFn","limit":20,"offset":0}`)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, err := server.callTool(ctx, "search_symbols", raw)
		if err != nil {
			b.Fatalf("callTool(search_symbols) error = %v", err)
		}
	}
}

func setupMCPBenchServer(b *testing.B, ctx context.Context) *Server {
	b.Helper()
	repoRoot := b.TempDir()
	for i := 0; i < 100; i++ {
		name := fmt.Sprintf("file_%03d.go", i)
		content := fmt.Sprintf("package bench\n\nfunc BenchFn%d() int { return %d }\n", i, i)
		path := filepath.Join(repoRoot, name)
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			b.Fatalf("WriteFile(%q) error = %v", path, err)
		}
	}

	s, err := store.Open(filepath.Join(b.TempDir(), "mcp-bench.sqlite"))
	if err != nil {
		b.Fatalf("store.Open() error = %v", err)
	}
	b.Cleanup(func() { _ = s.Close() })

	idx := indexer.New(s, parser.NewRegistry(goparser.New()))
	repo, err := s.UpsertRepo(ctx, repoRoot)
	if err != nil {
		b.Fatalf("UpsertRepo() error = %v", err)
	}
	if _, err := idx.Index(ctx, indexer.Options{RepoRoot: repoRoot}); err != nil {
		b.Fatalf("Index() error = %v", err)
	}
	return NewServer(repoRoot, repo.ID, s, idx, query.New(s))
}
