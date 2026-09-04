package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

func TestTypeScriptModuleScopeImportsReExportsAndVeto(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	repo, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	file := func(p string) int64 {
		id, e := insertTestFileLang(ctx, s, repo.ID, p, "typescript")
		if e != nil {
			t.Fatal(e)
		}
		return id
	}
	sym := func(f int64, n string) int64 {
		id, e := insertTestSymbolLang(ctx, s, repo.ID, f, n, n, "typescript")
		if e != nil {
			t.Fatal(e)
		}
		if _, e = s.db.Exec(`UPDATE symbols SET visibility='public' WHERE id=?`, id); e != nil {
			t.Fatal(e)
		}
		return id
	}
	evidence := func(f int64, src, imp, local, kind string, wild, reexp bool) {
		_, e := s.db.Exec(`INSERT INTO scope_import_evidence(repo_id,file_id,language,source_specifier,imported_name,local_name,import_kind,wildcard,is_reexport) VALUES(?,?,?,?,?,?,?,?,?)`, repo.ID, f, "typescript", src, imp, local, kind, boolInt(wild), boolInt(reexp))
		if e != nil {
			t.Fatal(e)
		}
	}
	a := file("src/a.ts")
	target := sym(a, "foo")
	_ = sym(a, "Foo")
	evidence(a, "", "foo", "foo", "named", false, true)
	b := file("src/b.ts")
	evidence(b, "./a", "foo", "bar", "named", false, true)
	c := file("src/c.ts")
	evidence(c, "./b", "bar", "baz", "named", false, false)
	src := sym(c, "run")
	edge, err := insertTestEdge(ctx, s, repo.ID, c, src, "baz")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ResolveEdges(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	var got int64
	var strategy string
	if err = s.db.QueryRow(`SELECT dst_symbol_id,resolution_strategy FROM edges WHERE id=?`, edge).Scan(&got, &strategy); err != nil {
		t.Fatal(err)
	}
	if got != target || strategy != ResolutionStrategyTypeScriptModuleScope {
		t.Fatalf("got (%d,%q), want (%d,%q)", got, strategy, target, ResolutionStrategyTypeScriptModuleScope)
	}
	// A named default export with a stable declaration identity is importable;
	// wildcard traversal still excludes default below the resolver.
	evidence(a, "", "default", "foo", "default", false, true)
	df := file("src/default-use.ts")
	evidence(df, "./a", "default", "x", "default", false, false)
	ds := sym(df, "runDefault")
	de, _ := insertTestEdge(ctx, s, repo.ID, df, ds, "x")
	if _, err = s.ResolveEdges(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	var defaultGot sql.NullInt64
	if err = s.db.QueryRow(`SELECT dst_symbol_id FROM edges WHERE id=?`, de).Scan(&defaultGot); err != nil {
		t.Fatal(err)
	}
	if !defaultGot.Valid || defaultGot.Int64 != target {
		t.Fatalf("default import target = %v, want %d", defaultGot, target)
	}
	// A unique declaration without an import is still not module evidence.
	d := file("src/d.ts")
	other := sym(d, "unbound")
	_ = other
	e2, _ := insertTestEdge(ctx, s, repo.ID, c, src, "foo")
	if _, err = s.ResolveEdges(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	var nullGot sql.NullInt64
	if err = s.db.QueryRow(`SELECT dst_symbol_id FROM edges WHERE id=?`, e2).Scan(&nullGot); err != nil {
		t.Fatal(err)
	}
	if nullGot.Valid {
		t.Fatalf("unimported call bound to %d", nullGot.Int64)
	}
	// An aliased re-export exposes only its exported spelling.
	evidence(c, "./b", "foo", "foo", "named", false, false)
	if _, err = s.db.Exec(`INSERT INTO scope_module_candidate_evidence(repo_id,source_file_id,source_specifier,candidate_path) VALUES(?,?,?,?)`, repo.ID, c, "./b", "src/b.ts"); err != nil {
		t.Fatal(err)
	}
	e4, _ := insertTestEdge(ctx, s, repo.ID, c, src, "foo")
	if _, err = s.ResolveEdges(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow(`SELECT dst_symbol_id FROM edges WHERE id=?`, e4).Scan(&nullGot); err != nil {
		t.Fatal(err)
	}
	if nullGot.Valid {
		t.Fatalf("aliased re-export leaked original name to %d", nullGot.Int64)
	}
	// Extensionless ambiguity refuses the relation.
	_ = file("src/amb.ts")
	_ = file("src/amb.tsx")
	b2 := file("src/use.ts")
	evidence(b2, "./amb", "foo", "foo", "named", false, false)
	rs := sym(b2, "run")
	e3, _ := insertTestEdge(ctx, s, repo.ID, b2, rs, "foo")
	if _, err = s.ResolveEdges(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow(`SELECT dst_symbol_id FROM edges WHERE id=?`, e3).Scan(&nullGot); err != nil {
		t.Fatal(err)
	}
	if nullGot.Valid {
		t.Fatalf("ambiguous extension bound to %d", nullGot.Int64)
	}
	// Module dependency evidence alone does not create runtime bindings.
	side := file("src/side-use.ts")
	// The local spelling is intentionally present only to make a side-effect
	// binding mutant observable; the import kind remains authoritative.
	evidence(side, "./a", "foo", "foo", "side_effect", false, false)
	if _, err = s.db.Exec(`INSERT INTO scope_module_candidate_evidence(repo_id,source_file_id,source_specifier,candidate_path) VALUES(?,?,?,?)`, repo.ID, side, "./a", "src/a.ts"); err != nil {
		t.Fatal(err)
	}
	sideSrc := sym(side, "runSide")
	sideEdge, _ := insertTestEdge(ctx, s, repo.ID, side, sideSrc, "foo")
	typeUse := file("src/type-use.ts")
	evidence(typeUse, "./a", "Foo", "Foo", "named", false, false)
	if _, err = s.db.Exec(`INSERT INTO scope_module_candidate_evidence(repo_id,source_file_id,source_specifier,candidate_path) VALUES(?,?,?,?)`, repo.ID, typeUse, "./a", "src/a.ts"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE scope_import_evidence SET is_type_only=1 WHERE file_id=?`, typeUse); err != nil {
		t.Fatal(err)
	}
	typeSrc := sym(typeUse, "runType")
	typeEdge, _ := insertTestEdge(ctx, s, repo.ID, typeUse, typeSrc, "Foo")
	hidden, err := insertTestSymbolLang(ctx, s, repo.ID, a, "hidden", "a.hidden", "typescript")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE symbols SET visibility='private' WHERE id=?`, hidden); err != nil {
		t.Fatal(err)
	}
	hiddenUse := file("src/hidden-use.ts")
	evidence(hiddenUse, "./a", "hidden", "hidden", "named", false, false)
	if _, err = s.db.Exec(`INSERT INTO scope_module_candidate_evidence(repo_id,source_file_id,source_specifier,candidate_path) VALUES(?,?,?,?)`, repo.ID, hiddenUse, "./a", "src/a.ts"); err != nil {
		t.Fatal(err)
	}
	hiddenSrc := sym(hiddenUse, "runHidden")
	hiddenEdge, _ := insertTestEdge(ctx, s, repo.ID, hiddenUse, hiddenSrc, "hidden")
	if _, err = s.ResolveEdges(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	for _, id := range []int64{sideEdge, typeEdge, hiddenEdge} {
		if err = s.db.QueryRow(`SELECT dst_symbol_id FROM edges WHERE id=?`, id).Scan(&nullGot); err != nil {
			t.Fatal(err)
		}
		if nullGot.Valid {
			t.Fatalf("non-binding evidence resolved edge %d to %d", id, nullGot.Int64)
		}
	}
}

func TestTypeScriptModuleCandidatePaths(t *testing.T) {
	tests := []struct {
		file, spec string
		want       []string
	}{
		{"src/b.ts", "./a", []string{"src/a.ts", "src/a.tsx"}},
		{"src/lib/b.ts", "../a", []string{"src/a.ts", "src/a.tsx"}},
		{"src/b.ts", "./a.js", []string{"src/a.js"}},
		{"src/b.ts", "./a.jsx", []string{"src/a.jsx"}},
		{"src/b.ts", "react", nil},
		{"src/b.ts", "./dir/", nil},
	}
	for _, tt := range tests {
		got := typescriptModuleCandidatePaths(tt.file, tt.spec)
		if len(got) != len(tt.want) {
			t.Errorf("%s %s: got %v, want %v", tt.file, tt.spec, got, tt.want)
			continue
		}
		for i := range got {
			if got[i] != tt.want[i] {
				t.Errorf("%s %s: got %v, want %v", tt.file, tt.spec, got, tt.want)
				break
			}
		}
	}
}

func TestTypeScriptCandidateReverseLookupInvalidatesUnchangedCaller(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	repo, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	caller, err := insertTestFileLang(ctx, s, repo.ID, "src/b.ts", "typescript")
	if err != nil {
		t.Fatal(err)
	}
	targetFile, err := insertTestFileLang(ctx, s, repo.ID, "src/a.ts", "typescript")
	if err != nil {
		t.Fatal(err)
	}
	target, err := insertTestSymbolLang(ctx, s, repo.ID, targetFile, "foo", "a.foo", "typescript")
	if err != nil {
		t.Fatal(err)
	}
	source, err := insertTestSymbolLang(ctx, s, repo.ID, caller, "run", "b.run", "typescript")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO scope_module_candidate_evidence(repo_id,source_file_id,source_specifier,candidate_path) VALUES(?,?,?,?),(?,?,?,?)`, repo.ID, caller, "./a", "src/a.ts", repo.ID, caller, "./a", "src/a.tsx"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO scope_import_evidence(repo_id,file_id,language,source_specifier,imported_name,local_name,import_kind) VALUES(?,?,?,?,?,?,?)`, repo.ID, caller, "typescript", "./a", "foo", "foo", "named"); err != nil {
		t.Fatal(err)
	}
	edge, err := insertTestEdge(ctx, s, repo.ID, caller, source, "foo")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE edges SET dst_symbol_id=?,resolution_strategy=?,resolution_confidence='high' WHERE id=?`, target, ResolutionStrategyTypeScriptModuleScope, edge); err != nil {
		t.Fatal(err)
	}
	if _, err = s.invalidateTypeScriptScopeBindings(ctx, repo.ID, []string{"src/a.ts"}); err != nil {
		t.Fatal(err)
	}
	var dst sql.NullInt64
	if err = s.db.QueryRow(`SELECT dst_symbol_id FROM edges WHERE id=?`, edge).Scan(&dst); err != nil {
		t.Fatal(err)
	}
	if dst.Valid {
		t.Fatalf("reverse candidate lookup left caller bound to %d", dst.Int64)
	}
	unrelated, err := insertTestFileLang(ctx, s, repo.ID, "src/unrelated.ts", "typescript")
	if err != nil {
		t.Fatal(err)
	}
	otherSource, err := insertTestSymbolLang(ctx, s, repo.ID, unrelated, "runOther", "unrelated.runOther", "typescript")
	if err != nil {
		t.Fatal(err)
	}
	otherEdge, err := insertTestEdge(ctx, s, repo.ID, unrelated, otherSource, "foo")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE edges SET dst_symbol_id=?,resolution_strategy=?,resolution_confidence='high' WHERE id=?`, target, ResolutionStrategyTypeScriptModuleScope, otherEdge); err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO scope_module_candidate_evidence(repo_id,source_file_id,source_specifier,candidate_path) VALUES(?,?,?,?)`, repo.ID, unrelated, "./other", "src/other.ts"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.invalidateTypeScriptScopeBindings(ctx, repo.ID, []string{"src/a.ts"}); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow(`SELECT dst_symbol_id FROM edges WHERE id=?`, otherEdge).Scan(&dst); err != nil {
		t.Fatal(err)
	}
	if !dst.Valid {
		t.Fatal("unrelated caller was included in reverse invalidation")
	}
	repo2, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	caller2, err := insertTestFileLang(ctx, s, repo2.ID, "src/b.ts", "typescript")
	if err != nil {
		t.Fatal(err)
	}
	target2, err := insertTestSymbolLang(ctx, s, repo2.ID, caller2, "foo", "a.foo", "typescript")
	if err != nil {
		t.Fatal(err)
	}
	source2, err := insertTestSymbolLang(ctx, s, repo2.ID, caller2, "run", "b.run", "typescript")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO scope_module_candidate_evidence(repo_id,source_file_id,source_specifier,candidate_path) VALUES(?,?,?,?)`, repo2.ID, caller2, "./a", "src/a.ts"); err != nil {
		t.Fatal(err)
	}
	edge2, err := insertTestEdge(ctx, s, repo2.ID, caller2, source2, "foo")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE edges SET dst_symbol_id=?,resolution_strategy=?,resolution_confidence='high' WHERE id=?`, target2, ResolutionStrategyTypeScriptModuleScope, edge2); err != nil {
		t.Fatal(err)
	}
	if _, err = s.invalidateTypeScriptScopeBindings(ctx, repo.ID, []string{"src/a.ts"}); err != nil {
		t.Fatal(err)
	}
	if err = s.db.QueryRow(`SELECT dst_symbol_id FROM edges WHERE id=?`, edge2).Scan(&dst); err != nil {
		t.Fatal(err)
	}
	if !dst.Valid {
		t.Fatal("reverse invalidation crossed repository boundary")
	}
}

func TestTypeScriptStaleBindingClearIsIndependentOfReindex(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	repo, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	file, err := insertTestFileLang(ctx, s, repo.ID, "b.ts", "typescript")
	if err != nil {
		t.Fatal(err)
	}
	source, err := insertTestSymbolLang(ctx, s, repo.ID, file, "run", "b.run", "typescript")
	if err != nil {
		t.Fatal(err)
	}
	target, err := insertTestSymbolLang(ctx, s, repo.ID, file, "foo", "a.foo", "typescript")
	if err != nil {
		t.Fatal(err)
	}
	edge, err := insertTestEdge(ctx, s, repo.ID, file, source, "foo")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`UPDATE edges SET dst_symbol_id=?,resolution_strategy=?,resolution_confidence='high' WHERE id=?`, target, ResolutionStrategyTypeScriptModuleScope, edge); err != nil {
		t.Fatal(err)
	}
	if n, err := clearEdgeResolutions(ctx, s.db, []int64{edge}); err != nil || n != 1 {
		t.Fatalf("clear stale binding: n=%d err=%v", n, err)
	}
	var dst sql.NullInt64
	var strategy string
	if err = s.db.QueryRow(`SELECT dst_symbol_id,resolution_strategy FROM edges WHERE id=?`, edge).Scan(&dst, &strategy); err != nil {
		t.Fatal(err)
	}
	if dst.Valid || strategy != "" {
		t.Fatalf("stale binding retained: dst=%v strategy=%v", dst, strategy)
	}
}

func TestTypeScriptReExportDeduplicatesSameTarget(t *testing.T) {
	got := uniqueExport(tsScopeExport{symbols: []int64{7, 7}})
	if len(got.symbols) != 1 || got.symbols[0] != 7 {
		t.Fatalf("same target was not deduplicated: %+v", got.symbols)
	}
}

func TestTypeScriptCandidateEvidenceSurvivesTargetGraphDelete(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	repo, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	caller, err := insertTestFileLang(ctx, s, repo.ID, "src/b.ts", "typescript")
	if err != nil {
		t.Fatal(err)
	}
	target, err := insertTestFileLang(ctx, s, repo.ID, "src/a.ts", "typescript")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.db.Exec(`INSERT INTO scope_module_candidate_evidence(repo_id,source_file_id,source_specifier,candidate_path) VALUES(?,?,?,?)`, repo.ID, caller, "./a", "src/a.ts"); err != nil {
		t.Fatal(err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err = deleteFileGraphsBatch(ctx, tx, repo.ID, []int64{target}, nil); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	var n int
	if err = tx.QueryRow(`SELECT count(*) FROM scope_module_candidate_evidence WHERE repo_id=? AND source_file_id=?`, repo.ID, caller).Scan(&n); err != nil {
		tx.Rollback()
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("target deletion removed caller candidate evidence: %d", n)
	}
}
