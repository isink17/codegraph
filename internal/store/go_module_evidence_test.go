package store

import (
	"os"
	"path/filepath"
	"testing"
)

func tableSQL(t *testing.T, s *Store, name string) string {
	t.Helper()
	var sql string
	if err := s.db.QueryRow(`SELECT sql FROM sqlite_master WHERE name=?`, name).Scan(&sql); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return sql
}

// A database migrated to 11 has the tables a new one has, and its existing
// repository reads as having no recorded Go module evidence.
func TestGoPackageIdentityMigration(t *testing.T) {
	root := t.TempDir()
	migrated, err := Open(indexedDatabase(t, root))
	if err != nil {
		t.Fatal(err)
	}
	defer migrated.Close()
	fresh, err := Open(filepath.Join(t.TempDir(), RepoDatabaseFileName))
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	for _, table := range []string{"go_file_identity_evidence", "go_module_evidence"} {
		if got, want := tableSQL(t, migrated, table), tableSQL(t, fresh, table); got != want {
			t.Fatalf("%s:\nmigrated %s\nfresh    %s", table, got, want)
		}
	}
	repo, found, err := migrated.FindRepo(t.Context(), root)
	if err != nil || !found {
		t.Fatalf("FindRepo = %v, %v", found, err)
	}
	g, err := migrated.SemanticGraph(t.Context(), repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	if g.GoModules != nil {
		t.Fatalf("migrated repository reports module evidence: %+v", g.GoModules)
	}
	if err := migrated.ReplaceGoModuleEvidence(t.Context(), repo.ID, []GoModuleEvidence{}); err != nil {
		t.Fatal(err)
	}
	if g, err = migrated.SemanticGraph(t.Context(), repo.ID); err != nil || g.GoModules == nil || len(g.GoModules.Modules) != 0 {
		t.Fatalf("recorded empty evidence = %+v, %v", g.GoModules, err)
	}
}

func TestDiscoverGoModuleEvidence(t *testing.T) {
	root := t.TempDir()
	for rel, content := range map[string]string{
		"go.mod":       "module example.com/m\n",
		"a/go.mod":     "module example.com/a\n\nreplace example.com/b => ../b\n",
		"b/go.mod":     "module example.com/b\n\nreplace example.com/c => example.com/c v1.0.0\n",
		"bad/go.mod":   "not a module\n",
		"w/go.work":    "go 1.22\n",
		".git/go.mod":  "module ignored\n",
		".codegraph/x": "",
	} {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := DiscoverGoModuleEvidence(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []GoModuleEvidence{
		{Kind: "module", Root: ".", ModulePath: "example.com/m"},
		{Kind: "module", Root: "a", ModulePath: "example.com/a", LocalReplace: true},
		{Kind: "module", Root: "b", ModulePath: "example.com/b"},
		{Kind: "module", Root: "bad"},
		{Kind: "workspace", Root: "w"},
	}
	if len(got) != len(want) {
		t.Fatalf("evidence = %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("evidence[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestGoFileIdentityUsesTheNearestModule(t *testing.T) {
	mods := &SemanticGoModules{Modules: []SemanticGoModule{{Root: ".", Path: "example.com/m"}, {Root: "a", Path: "example.com/a"}, {Root: "a/b", Path: ""}}}
	for _, tc := range []struct{ file, state, root, importPath string }{
		{"x.go", GoModuleComplete, ".", "example.com/m"},
		{"ab/x.go", GoModuleComplete, ".", "example.com/m/ab"},
		{"a/x.go", GoModuleComplete, "a", "example.com/a"},
		{"a/c/x.go", GoModuleComplete, "a", "example.com/a/c"},
		{"a/b/x.go", GoModuleNotDeclared, "a/b", ""},
	} {
		got := goFileIdentity(tc.file, "p", "", mods)
		if got.ModuleState != tc.state || got.ModuleRoot != tc.root || got.ImportPath != tc.importPath {
			t.Errorf("%s: %+v", tc.file, got)
		}
	}
	if got := goFileIdentity("x.go", "p", "", &SemanticGoModules{}); got.ModuleState != GoModuleNone {
		t.Errorf("no module: %+v", got)
	}
	if got := goFileIdentity("x.go", "p", "", nil); got.ModuleState != GoModuleUnknown {
		t.Errorf("unrecorded: %+v", got)
	}
}
