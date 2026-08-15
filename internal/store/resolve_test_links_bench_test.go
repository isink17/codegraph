package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

// BenchmarkResolveTestLinks measures the canonical pass across link-count
// scales (P22.2 §13/§24). The mix mirrors a real repo: a third of the links
// bind by name, a third only have a sibling file, a third stay unbound.
func BenchmarkResolveTestLinks(b *testing.B) {
	for _, n := range []int{10, 100, 1000, 5000} {
		b.Run(fmt.Sprintf("links=%d", n), func(b *testing.B) {
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
			repoID := repo.ID
			for i := range n {
				prodPath := fmt.Sprintf("pkg%d/impl.go", i)
				testPath := fmt.Sprintf("pkg%d/impl_test.go", i)
				prodFile, err := insertTestFileLang(ctx, s, repoID, prodPath, "go")
				if err != nil {
					b.Fatal(err)
				}
				testFile, err := insertTestFileLang(ctx, s, repoID, testPath, "go")
				if err != nil {
					b.Fatal(err)
				}
				key := fmt.Sprintf("func:pkg%d::Helper", i)
				switch i % 3 {
				case 0: // name-bindable
					if _, err := s.db.ExecContext(ctx, `
						INSERT INTO symbols(repo_id, file_id, language, kind, name, qualified_name, container_name,
							start_line, start_col, end_line, end_col, stable_key, qualified_suffix, dot_tail2, dot_tail3)
						VALUES(?, ?, 'go', 'function', 'Helper', 'Helper', '', 1, 1, 1, 1, ?, '', '', '')
					`, repoID, prodFile, key); err != nil {
						b.Fatal(err)
					}
				case 1: // sibling-only (key names nothing)
				case 2: // fully unbound: no sibling either
					if _, err := s.db.ExecContext(ctx,
						`UPDATE files SET path = ? WHERE id = ?`,
						fmt.Sprintf("pkg%d/behaviour_test.go", i), testFile); err != nil {
						b.Fatal(err)
					}
				}
				if _, err := s.db.ExecContext(ctx, `
					INSERT INTO test_links(repo_id, test_file_id, test_symbol_id, target_file_id, target_symbol_id, reason, score, target_stable_key)
					VALUES(?, ?, NULL, NULL, NULL, 'test_name_match', 0.8, ?)
				`, repoID, testFile, key); err != nil {
					b.Fatal(err)
				}
			}

			b.ReportAllocs()
			for b.Loop() {
				if _, err := s.ResolveTestLinks(ctx, repoID); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
