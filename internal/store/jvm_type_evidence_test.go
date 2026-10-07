package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

func TestJVMTypeEvidenceReplacementLifecycle(t *testing.T) {
	f := newGateFixture(t)
	write := func(source string) {
		t.Helper()
		parsed := graph.ParsedFile{
			Language: "kotlin",
			Symbols: []graph.Symbol{
				{Language: "kotlin", Kind: "class", Name: "Token", QualifiedName: "api.Token", StableKey: "type:kotlin:api.Token"},
				{Language: "kotlin", Kind: "function", Name: "use", QualifiedName: "api.use", StableKey: "func:kotlin:api.use"},
			},
			JVMTypeEvidence: []graph.JVMTypeEvidence{
				{SymbolIndex: 0, Kind: "value_class", Modifiers: "value", SyntaxState: "known", Provenance: "test:tree"},
				{SymbolIndex: 1, Kind: "function", SyntaxState: "known", Provenance: "test:tree", Params: []graph.JVMCallableTypeEvidence{{Position: "parameter", Syntax: "Alias", SyntaxState: "known"}, {Position: "result", Syntax: "Token", SyntaxState: "known"}}},
			},
		}
		if source == "ordinary" {
			parsed.JVMTypeEvidence[0].Kind = "class"
			parsed.JVMTypeEvidence[0].Modifiers = ""
		}
		if source == "unknown" {
			parsed.JVMTypeEvidence[0].SyntaxState = "unknown"
		}
		if err := f.store.ReplaceFileGraph(f.ctx, f.repoID, 1, "Token.kt", "kotlin", int64(len(source)), 0, source, parsed); err != nil {
			t.Fatal(err)
		}
	}
	state := func() string {
		t.Helper()
		var got string
		if err := f.store.db.QueryRowContext(f.ctx, `SELECT declaration_kind||'|'||modifiers||'|'||syntax_state||'|'||provenance FROM jvm_type_evidence WHERE symbol_id=(SELECT id FROM symbols WHERE stable_key='type:kotlin:api.Token')`).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	var scopeState string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT evidence_domain||'|'||state||'|'||evidence_version||'|'||provenance FROM jvm_compilation_scope_evidence WHERE repo_id=?`, f.repoID).Scan(&scopeState); err != nil || scopeState != "jvm-type-identity|unknown|1|store:default-no-compilation-scope-evidence" {
		t.Fatalf("compilation scope default = %q, err=%v", scopeState, err)
	}
	for _, tc := range []struct{ input, want string }{{"value", "value_class|value|known|test:tree"}, {"ordinary", "class||known|test:tree"}, {"unknown", "value_class|value|unknown|test:tree"}, {"value", "value_class|value|known|test:tree"}} {
		write(tc.input)
		if got := state(); got != tc.want {
			t.Fatalf("state = %q, want %q", got, tc.want)
		}
		var signatures int
		if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM jvm_callable_type_evidence c JOIN symbols s ON s.id=c.symbol_id WHERE s.stable_key='func:kotlin:api.use'`).Scan(&signatures); err != nil || signatures != 2 {
			t.Fatalf("callable syntax facts = %d, err=%v", signatures, err)
		}
	}
	if _, err := f.store.MarkFilesDeletedBatch(f.ctx, f.repoID, 2, []string{"Token.kt"}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.PurgeDeletedFileGraphsForScan(f.ctx, f.repoID, 2); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM jvm_type_evidence`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("evidence after delete = %d, err=%v", n, err)
	}
}

func TestJVMCompilationScopeMigrationDefaultsExistingReposUnknown(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), RepoDatabaseFileName)
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	repo, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `DROP TABLE jvm_compilation_scope_evidence; DROP TABLE jvm_callable_type_evidence; DROP TABLE jvm_type_evidence; DELETE FROM schema_migrations WHERE version=6`); err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	scope, err := s.JVMCompilationScope(ctx, repo.ID)
	if err != nil || scope.Domain != "jvm-type-identity" || scope.Version != 1 || scope.State != "unknown" || scope.Provenance != "migration:006:no-compilation-scope-evidence" {
		t.Fatalf("historical scope = %+v, err=%v", scope, err)
	}
}
