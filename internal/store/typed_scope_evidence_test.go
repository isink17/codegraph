package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

func TestTypedScopeEvidenceReplacementAndRepoIsolation(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	repoA, err := s.UpsertRepo(ctx, filepath.Join(t.TempDir(), "a"))
	if err != nil {
		t.Fatal(err)
	}
	repoB, err := s.UpsertRepo(ctx, filepath.Join(t.TempDir(), "b"))
	if err != nil {
		t.Fatal(err)
	}
	pf := graph.ParsedFile{Language: "java", Scope: graph.ScopeEvidence{Package: "a.b", Imports: []graph.ScopeImport{{SourceSpecifier: "x.Foo", ImportedName: "Foo", LocalName: "Bar", Kind: graph.ScopeImportNamed, Static: true}}}}
	if err := s.ReplaceFileGraph(ctx, repoA.ID, 1, "src/A.java", "java", 1, 1, "a", pf); err != nil {
		t.Fatal(err)
	}
	if err := s.ReplaceFileGraph(ctx, repoB.ID, 1, "src/A.java", "java", 1, 1, "b", pf); err != nil {
		t.Fatal(err)
	}
	check := func(repo int64, pkg, local string, n int) {
		var gotPkg, gotLocal string
		if err := s.db.QueryRow(`SELECT f.package_name, i.local_name FROM file_scope_evidence f JOIN scope_import_evidence i ON i.repo_id=f.repo_id AND i.file_id=f.file_id WHERE f.repo_id=?`, repo).Scan(&gotPkg, &gotLocal); err != nil {
			t.Fatal(err)
		}
		if gotPkg != pkg || gotLocal != local {
			t.Fatalf("repo %d evidence=(%q,%q)", repo, gotPkg, gotLocal)
		}
		var count int
		if err := s.db.QueryRow(`SELECT count(*) FROM scope_import_evidence WHERE repo_id=?`, repo).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != n {
			t.Fatalf("repo %d count=%d want %d", repo, count, n)
		}
	}
	check(repoA.ID, "a.b", "Bar", 1)
	check(repoB.ID, "a.b", "Bar", 1)
	pf.Scope.Package = "c.d"
	pf.Scope.Imports[0].LocalName = "Baz"
	if err := s.ReplaceFileGraph(ctx, repoA.ID, 2, "src/A.java", "java", 1, 2, "c", pf); err != nil {
		t.Fatal(err)
	}
	check(repoA.ID, "c.d", "Baz", 1)
	check(repoB.ID, "a.b", "Bar", 1)
	if _, err := s.db.Exec(`UPDATE files SET is_deleted=1, last_scan_id=3 WHERE repo_id=? AND path=?`, repoA.ID, "src/A.java"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.PurgeDeletedFileGraphsForScan(ctx, repoA.ID, 3); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM scope_import_evidence WHERE repo_id=?`, repoA.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("deleted file left %d typed rows", count)
	}
}
