package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

// benchBridgedStore builds the canonical bridge fixture -- one TypeScript file
// importing one Python file, n same-named symbol pairs -- without going through
// the *testing.T-shaped test helpers, so a benchmark gets real cleanup instead
// of a fabricated T whose t.Cleanup never runs.
func benchBridgedStore(b *testing.B, n int) (context.Context, *Store, int64) {
	b.Helper()
	ctx := context.Background()
	s, err := Open(filepath.Join(b.TempDir(), "graph.sqlite"))
	if err != nil {
		b.Fatalf("Open() error = %v", err)
	}
	b.Cleanup(func() { _ = s.Close() })
	repo, err := s.UpsertRepo(ctx, b.TempDir())
	if err != nil {
		b.Fatalf("UpsertRepo() error = %v", err)
	}

	tsFile, err := insertTestFileLang(ctx, s, repo.ID, "src/ts/client.ts", "typescript")
	if err != nil {
		b.Fatalf("insert ts file error = %v", err)
	}
	pyFile, err := insertTestFileLang(ctx, s, repo.ID, "src/shared/model.py", "python")
	if err != nil {
		b.Fatalf("insert py file error = %v", err)
	}
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("Payload%04d", i)
		if _, err := insertTestSymbolKind(ctx, s, repo.ID, tsFile, name, "client."+name, "function", "", "typescript"); err != nil {
			b.Fatalf("insert ts symbol error = %v", err)
		}
		if _, err := insertTestSymbolKind(ctx, s, repo.ID, pyFile, name, "model."+name, "function", "", "python"); err != nil {
			b.Fatalf("insert py symbol error = %v", err)
		}
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO file_imports(repo_id, file_id, import_path) VALUES(?, ?, 'src/shared/model.ts')`,
		repo.ID, tsFile); err != nil {
		b.Fatalf("insert file_import error = %v", err)
	}
	return ctx, s, repo.ID
}

// BenchmarkResolveCrossLanguageLinks measures the steady state: the link set
// already matches, so this is candidate computation plus a no-op
// reconciliation. Work scales with the symbols of bridged files, never with all
// symbol pairs.
func BenchmarkResolveCrossLanguageLinks(b *testing.B) {
	for _, n := range []int{0, 10, 50, 51, 100, 500, 1000} {
		b.Run(fmt.Sprintf("eligible=%d", n), func(b *testing.B) {
			ctx, s, repoID := benchBridgedStore(b, n)
			created, err := s.ResolveCrossLanguageLinks(ctx, repoID)
			if err != nil || created != n {
				b.Fatalf("warm-up created %d links (err %v), want %d", created, err, n)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := s.ResolveCrossLanguageLinks(ctx, repoID); err != nil {
					b.Fatalf("ResolveCrossLanguageLinks() error = %v", err)
				}
			}
		})
	}
}

// BenchmarkResolveCrossLanguageLinksCold measures the first pass, which also
// writes every link.
func BenchmarkResolveCrossLanguageLinksCold(b *testing.B) {
	for _, n := range []int{50, 500} {
		b.Run(fmt.Sprintf("eligible=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				ctx, s, repoID := benchBridgedStore(b, n)
				b.StartTimer()
				created, err := s.ResolveCrossLanguageLinks(ctx, repoID)
				if err != nil || created != n {
					b.Fatalf("created %d links (err %v), want %d", created, err, n)
				}
			}
		})
	}
}
