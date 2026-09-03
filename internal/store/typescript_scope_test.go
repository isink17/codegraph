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
}
