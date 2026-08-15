package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveEdgesOwnModuleImportUsesPackageEvidence(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/project\n\ngo 1.26\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	repo, err := s.UpsertRepo(ctx, root)
	if err != nil {
		t.Fatal(err)
	}

	targetFile, err := insertTestFileLang(ctx, s, repo.ID, "internal/parser/registry.go", "go")
	if err != nil {
		t.Fatal(err)
	}
	target, err := insertTestSymbolKind(ctx, s, repo.ID, targetFile, "NewRegistry", "parser.NewRegistry", "function", "", "go")
	if err != nil {
		t.Fatal(err)
	}
	callerFile, err := insertTestFileLang(ctx, s, repo.ID, "cmd/main.go", "go")
	if err != nil {
		t.Fatal(err)
	}
	caller, err := insertTestSymbolKind(ctx, s, repo.ID, callerFile, "main", "main", "function", "", "go")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO file_imports(repo_id, file_id, import_path) VALUES (?, ?, ?)`, repo.ID, callerFile, "example.com/project/internal/parser"); err != nil {
		t.Fatal(err)
	}
	edge, err := insertTestEdge(ctx, s, repo.ID, callerFile, caller, "example.com/project/internal/parser.NewRegistry")
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.ResolveEdges(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	var got sql.NullInt64
	var strategy string
	if err := s.db.QueryRowContext(ctx, `SELECT dst_symbol_id, resolution_strategy FROM edges WHERE id = ?`, edge).Scan(&got, &strategy); err != nil {
		t.Fatal(err)
	}
	if !got.Valid || got.Int64 != target || strategy != ResolutionStrategyModuleImport {
		t.Fatalf("resolution = (%v, %d, %q), want (%v, %d, %q)", got.Valid, got.Int64, strategy, true, target, ResolutionStrategyModuleImport)
	}
}

func TestResolveEdgesOwnModuleImportVetoesGlobalFallback(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/project\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	repo, err := s.UpsertRepo(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	other, err := insertTestFileLang(ctx, s, repo.ID, "other/registry.go", "go")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := insertTestSymbolKind(ctx, s, repo.ID, other, "NewRegistry", "other.NewRegistry", "function", "", "go"); err != nil {
		t.Fatal(err)
	}
	callerFile, err := insertTestFileLang(ctx, s, repo.ID, "cmd/main.go", "go")
	if err != nil {
		t.Fatal(err)
	}
	caller, err := insertTestSymbolKind(ctx, s, repo.ID, callerFile, "main", "main", "function", "", "go")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO file_imports(repo_id, file_id, import_path) VALUES (?, ?, ?)`, repo.ID, callerFile, "example.com/project/internal/parser"); err != nil {
		t.Fatal(err)
	}
	edge, err := insertTestEdge(ctx, s, repo.ID, callerFile, caller, "example.com/project/internal/parser.NewRegistry")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveEdges(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	var got sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT dst_symbol_id FROM edges WHERE id = ?`, edge).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got.Valid {
		t.Fatalf("edge resolved to %d despite mapped package having no candidate", got.Int64)
	}
}
