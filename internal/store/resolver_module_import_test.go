package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	goparser "github.com/isink17/codegraph/internal/parser/golang"
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
	target, err := insertTestSymbolKind(ctx, s, repo.ID, targetFile, "NewRegistry", "parser.NewRegistry", "function", "parser", "go")
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

func TestResolveEdgesOwnModuleImportPersistedPathSeparators(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		source string
		target string
	}{
		{name: "slash slash", source: "cmd/main.go", target: "pkg/target.go"},
		{name: "backslash backslash", source: `cmd\main.go`, target: `pkg\target.go`},
		{name: "backslash slash", source: `cmd\main.go`, target: "pkg/target.go"},
		{name: "slash backslash", source: "cmd/main.go", target: `pkg\target.go`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, entrypoint := range []string{"full", "paths", "names"} {
				t.Run(entrypoint, func(t *testing.T) {
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
					targetFile, err := insertTestFileLang(ctx, s, repo.ID, tc.target, "go")
					if err != nil {
						t.Fatal(err)
					}
					target, err := insertTestSymbolKind(ctx, s, repo.ID, targetFile, "New", "pkg.New", "function", "pkg", "go")
					if err != nil {
						t.Fatal(err)
					}
					otherPath := strings.Replace(tc.target, "pkg", "other", 1)
					otherFile, err := insertTestFileLang(ctx, s, repo.ID, otherPath, "go")
					if err != nil {
						t.Fatal(err)
					}
					if _, err := insertTestSymbolKind(ctx, s, repo.ID, otherFile, "New", "other.New", "function", "other", "go"); err != nil {
						t.Fatal(err)
					}
					sourceFile, err := insertTestFileLang(ctx, s, repo.ID, tc.source, "go")
					if err != nil {
						t.Fatal(err)
					}
					source, err := insertTestSymbolKind(ctx, s, repo.ID, sourceFile, "main", "main", "function", "", "go")
					if err != nil {
						t.Fatal(err)
					}
					if _, err := s.db.ExecContext(ctx, `INSERT INTO file_imports(repo_id, file_id, import_path) VALUES (?, ?, ?)`, repo.ID, sourceFile, "example.com/project/pkg"); err != nil {
						t.Fatal(err)
					}
					edge, err := insertTestEdge(ctx, s, repo.ID, sourceFile, source, "example.com/project/pkg.New")
					if err != nil {
						t.Fatal(err)
					}

					switch entrypoint {
					case "full":
						if _, err := s.ResolveEdges(ctx, repo.ID); err != nil {
							t.Fatal(err)
						}
					case "paths":
						if err := s.ResolveEdgesForPaths(ctx, repo.ID, []string{"cmd/main.go"}); err != nil {
							t.Fatal(err)
						}
					case "names":
						if _, err := s.ResolveEdgesForNames(ctx, repo.ID, []string{"New"}); err != nil {
							t.Fatal(err)
						}
					}

					var gotID sql.NullInt64
					var strategy, confidence string
					if err := s.db.QueryRowContext(ctx, `SELECT dst_symbol_id, resolution_strategy, resolution_confidence FROM edges WHERE id = ?`, edge).Scan(&gotID, &strategy, &confidence); err != nil {
						t.Fatal(err)
					}
					if !gotID.Valid || gotID.Int64 != target || strategy != ResolutionStrategyModuleImport || confidence != ResolutionConfidenceHigh {
						t.Fatalf("%s resolution = (%v, %d, %q, %q), want (%v, %d, %q, %q)", entrypoint, gotID.Valid, gotID.Int64, strategy, confidence, true, target, ResolutionStrategyModuleImport, ResolutionConfidenceHigh)
					}
					callees, err := s.FindCallees(ctx, repo.ID, "main", 0, 20, 0)
					if err != nil {
						t.Fatal(err)
					}
					if len(callees) != 1 || callees[0].QualifiedName != "pkg.New" {
						t.Fatalf("%s callees = %v, want exactly pkg.New", entrypoint, callees)
					}
				})
			}
		})
	}
}

func TestModulePackageDirUsesSegmentsAndNestedModuleBoundaries(t *testing.T) {
	tests := []struct {
		name       string
		modules    []goModule
		importPath string
		want       string
		ok         bool
	}{
		{name: "versioned module", modules: []goModule{{path: "example.com/project/v2", root: "."}}, importPath: "example.com/project/v2/pkg", want: "pkg", ok: true},
		{name: "prefix collision", modules: []goModule{{path: "example.com/project", root: "."}}, importPath: "example.com/projectx/pkg", ok: false},
		{name: "nested module wins", modules: []goModule{{path: "example.com/root", root: "."}, {path: "example.com/tools/sub", root: "tools/sub"}}, importPath: "example.com/tools/sub/pkg", want: "tools/sub/pkg", ok: true},
		{name: "malformed nested module blocks parent", modules: []goModule{{path: "example.com/root", root: "."}, {root: "tools/sub", blocked: true}}, importPath: "example.com/tools/sub/pkg", ok: false},
		{name: "valid nested module bypasses blocked ancestor", modules: []goModule{{path: "example.com/root", root: "."}, {root: "tools", blocked: true}, {path: "example.com/tools/sub", root: "tools/sub"}}, importPath: "example.com/tools/sub/pkg", want: "tools/sub/pkg", ok: true},
		{name: "duplicate declaration fails closed", modules: []goModule{{path: "example.com/root", root: "one"}, {path: "example.com/root", root: "two"}}, importPath: "example.com/root/pkg", ok: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := modulePackageDir(tt.modules, tt.importPath)
			if got != tt.want || ok != tt.ok {
				t.Fatalf("modulePackageDir(%q) = (%q, %v), want (%q, %v)", tt.importPath, got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestGoParserAliasKeepsImportPathEvidence(t *testing.T) {
	parsed, err := goparser.New().Parse(context.Background(), "caller.go", []byte(`package main

import p "example.com/project/internal/parser"

func f() { p.NewRegistry() }
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Imports) != 1 || parsed.Imports[0] != "example.com/project/internal/parser" {
		t.Fatalf("Imports = %#v", parsed.Imports)
	}
	if len(parsed.Edges) != 1 || parsed.Edges[0].DstName != "example.com/project/internal/parser.NewRegistry" {
		t.Fatalf("Edges = %#v, want full import-qualified call", parsed.Edges)
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
	if _, err := s.db.ExecContext(ctx, `INSERT INTO file_imports(repo_id, file_id, import_path) VALUES (?, ?, ?)`, repo.ID, callerFile, "example.com/project/other"); err != nil {
		t.Fatal(err)
	}
	edge, err := insertTestEdge(ctx, s, repo.ID, callerFile, caller, "example.com/project/internal/parser.NewRegistry")
	if err != nil {
		t.Fatal(err)
	}
	otherEdge, err := insertTestEdge(ctx, s, repo.ID, callerFile, caller, "example.com/project/internal/parser.NewRegistry")
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
	if err := s.db.QueryRowContext(ctx, `SELECT dst_symbol_id FROM edges WHERE id = ?`, otherEdge).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got.Valid {
		t.Fatalf("edge with unrelated import resolved to %d", got.Int64)
	}
	for _, entrypoint := range []string{"paths", "names"} {
		t.Run(entrypoint, func(t *testing.T) {
			if _, err := s.db.ExecContext(ctx, `UPDATE edges SET dst_symbol_id = NULL, resolution_strategy = '', resolution_confidence = '' WHERE id = ?`, edge); err != nil {
				t.Fatal(err)
			}
			switch entrypoint {
			case "paths":
				if err := s.ResolveEdgesForPaths(ctx, repo.ID, []string{"cmd/main.go"}); err != nil {
					t.Fatal(err)
				}
			case "names":
				if _, err := s.ResolveEdgesForNames(ctx, repo.ID, []string{"NewRegistry"}); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.db.QueryRowContext(ctx, `SELECT dst_symbol_id FROM edges WHERE id = ?`, edge).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if got.Valid {
				t.Fatalf("edge resolved to %d after %s resolution", got.Int64, entrypoint)
			}
		})
	}
}

func TestResolveEdgesOwnModuleImportProductionPackageOnly(t *testing.T) {
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
	pkg := "example.com/project/pkg"
	targetFile, err := insertTestFileLang(ctx, s, repo.ID, "pkg/target.go", "go")
	if err != nil {
		t.Fatal(err)
	}
	production := insertTestSymbolMust(t, ctx, s, repo.ID, targetFile, "New", "pkg.New", "function", "pkg", "go")
	if _, err := insertTestSymbolKind(ctx, s, repo.ID, targetFile, "New", "pkg.T.New", "function", "T", "go"); err != nil {
		t.Fatal(err)
	}
	testFile, err := insertTestFileLang(ctx, s, repo.ID, "pkg/target_test.go", "go")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := insertTestSymbolKind(ctx, s, repo.ID, testFile, "New", "pkg.New", "function", "pkg", "go"); err != nil {
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
	if _, err := s.db.ExecContext(ctx, `INSERT INTO file_imports(repo_id, file_id, import_path) VALUES (?, ?, ?)`, repo.ID, callerFile, pkg); err != nil {
		t.Fatal(err)
	}
	edge, err := insertTestEdge(ctx, s, repo.ID, callerFile, caller, pkg+".New")
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
	if !got.Valid || got.Int64 != production {
		t.Fatalf("destination = (%v, %d), want production %d", got.Valid, got.Int64, production)
	}
	secondFile, err := insertTestFileLang(ctx, s, repo.ID, "pkg/other.go", "go")
	if err != nil {
		t.Fatal(err)
	}
	second := insertTestSymbolMust(t, ctx, s, repo.ID, secondFile, "New", "pkg.New", "function", "pkg", "go")
	if _, err := s.db.ExecContext(ctx, `UPDATE edges SET dst_symbol_id = NULL, resolution_strategy = '', resolution_confidence = '' WHERE id = ?`, edge); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveEdges(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT dst_symbol_id FROM edges WHERE id = ?`, edge).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got.Valid {
		t.Fatalf("ambiguous package candidates chose %d", got.Int64)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM symbols WHERE id = ?`, second); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE edges SET dst_symbol_id = NULL, resolution_strategy = '', resolution_confidence = '' WHERE id = ?`, edge); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveEdges(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT dst_symbol_id FROM edges WHERE id = ?`, edge).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !got.Valid || got.Int64 != production {
		t.Fatalf("remaining candidate = (%v, %d), want %d", got.Valid, got.Int64, production)
	}
	if _, err := s.db.ExecContext(ctx, `DELETE FROM file_imports WHERE file_id = ?`, callerFile); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE edges SET dst_symbol_id = NULL, resolution_strategy = '', resolution_confidence = '' WHERE id = ?`, edge); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveEdges(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.db.QueryRowContext(ctx, `SELECT dst_symbol_id FROM edges WHERE id = ?`, edge).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got.Valid {
		t.Fatalf("edge resolved without source import evidence to %d", got.Int64)
	}
}

func insertTestSymbolMust(t *testing.T, ctx context.Context, s *Store, repoID, fileID int64, name, qualified, kind, container, language string) int64 {
	t.Helper()
	id, err := insertTestSymbolKind(ctx, s, repoID, fileID, name, qualified, kind, container, language)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestResolveEdgesOwnModuleImportMalformedNestedGoModFailsClosed(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/root\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "tools", "sub", "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "tools", "sub", "go.mod"), []byte("module\n"), 0o644); err != nil {
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
	targetFile, err := insertTestFileLang(ctx, s, repo.ID, "tools/sub/pkg/new.go", "go")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := insertTestSymbolKind(ctx, s, repo.ID, targetFile, "New", "pkg.New", "function", "pkg", "go"); err != nil {
		t.Fatal(err)
	}
	callerFile, err := insertTestFileLang(ctx, s, repo.ID, "cmd/main.go", "go")
	if err != nil {
		t.Fatal(err)
	}
	caller := insertTestSymbolMust(t, ctx, s, repo.ID, callerFile, "main", "main", "function", "", "go")
	if _, err := s.db.ExecContext(ctx, `INSERT INTO file_imports(repo_id, file_id, import_path) VALUES (?, ?, ?)`, repo.ID, callerFile, "example.com/root/tools/sub/pkg"); err != nil {
		t.Fatal(err)
	}
	edge, err := insertTestEdge(ctx, s, repo.ID, callerFile, caller, "example.com/root/tools/sub/pkg.New")
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
		t.Fatalf("malformed nested module claimed subtree: destination %d", got.Int64)
	}
}
