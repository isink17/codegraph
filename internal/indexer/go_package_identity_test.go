package indexer

import (
	"database/sql"
	"encoding/json"
	"testing"

	"github.com/isink17/codegraph/internal/parser"
	"github.com/isink17/codegraph/internal/store"
)

func (r *goRepo) semanticJSON(t *testing.T) (store.SemanticGraph, string) {
	t.Helper()
	g, err := r.store.SemanticGraph(r.ctx, r.repoID)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	return g, string(b)
}

func goIdentities(g store.SemanticGraph) map[string]store.SemanticGoFile {
	out := map[string]store.SemanticGoFile{}
	for _, f := range g.Files {
		if f.Go != nil {
			out[f.Path] = *f.Go
		}
	}
	return out
}

func TestGoPackageIdentityIndexAndUpdateParity(t *testing.T) {
	runGoPackageIdentityParity(t, goNativeRegistry())
}

func runGoPackageIdentityParity(t *testing.T, reg *parser.Registry) {
	r := newGoRepo(t, reg, map[string]string{
		"go.mod":           "module example.com/m\n\ngo 1.22\n",
		"main.go":          "package main\n\nfunc main() {}\n",
		"p/a.go":           "package p\n\nfunc A() {}\n",
		"p/a_test.go":      "//go:build linux\n\npackage p_test\n",
		"nested/go.mod":    "module example.com/nested\n",
		"nested/q/b.go":    "package q\n",
		"broken/go.mod":    "this is not a go.mod\n",
		"broken/c.go":      "package broken\n",
		"replacing/go.mod": "module example.com/r\n\nreplace example.com/x => ../x\n",
		"replacing/r.go":   "package r\n",
	})
	check := func(step string) store.SemanticGraph {
		t.Helper()
		g, got := r.semanticJSON(t)
		_, want := newGoRepo(t, reg, r.currentTree(t)).semanticJSON(t)
		if got != want {
			t.Fatalf("%s: update diverges from a fresh index:\n got %s\nwant %s", step, got, want)
		}
		return g
	}
	g := check("index")
	if g.GoModules == nil || len(g.GoModules.Workspaces) != 0 || canonicalJSON(g.GoModules.Modules) != `[{"root":".","path":"example.com/m"},{"root":"broken","path":""},{"root":"nested","path":"example.com/nested"},{"root":"replacing","path":"example.com/r","local_replace":true}]` {
		t.Fatalf("modules = %s", canonicalJSON(g.GoModules))
	}
	ids := goIdentities(g)
	want := map[string]store.SemanticGoFile{
		"main.go":        {PackageClause: "main", ModuleState: store.GoModuleComplete, ModuleRoot: ".", ModulePath: "example.com/m", ImportPath: "example.com/m"},
		"p/a.go":         {PackageClause: "p", ModuleState: store.GoModuleComplete, ModuleRoot: ".", ModulePath: "example.com/m", ImportPath: "example.com/m/p"},
		"p/a_test.go":    {PackageClause: "p_test", BuildConstraint: "linux", ExternalTest: true, ModuleState: store.GoModuleComplete, ModuleRoot: ".", ModulePath: "example.com/m", ImportPath: "example.com/m/p"},
		"nested/q/b.go":  {PackageClause: "q", ModuleState: store.GoModuleComplete, ModuleRoot: "nested", ModulePath: "example.com/nested", ImportPath: "example.com/nested/q"},
		"broken/c.go":    {PackageClause: "broken", ModuleState: store.GoModuleNotDeclared, ModuleRoot: "broken"},
		"replacing/r.go": {PackageClause: "r", ModuleState: store.GoModuleComplete, ModuleRoot: "replacing", ModulePath: "example.com/r", ImportPath: "example.com/r"},
	}
	if canonicalJSON(ids) != canonicalJSON(want) {
		t.Fatalf("identities:\n got %s\nwant %s", canonicalJSON(ids), canonicalJSON(want))
	}
	if summary, err := r.idx.Update(r.ctx, Options{RepoRoot: r.root, ScanKind: "update"}); err != nil {
		t.Fatal(err)
	} else if summary.FilesIndexed != 0 {
		t.Fatalf("no-op update indexed %d files", summary.FilesIndexed)
	}
	if _, again := r.semanticJSON(t); again != canonicalJSON(g) {
		t.Fatal("no-op update changed the semantic graph")
	}

	r.write(t, "go.mod", "module example.com/renamed\n\ngo 1.22\n")
	r.update(t)
	if got := goIdentities(check("module path renamed"))["p/a.go"].ImportPath; got != "example.com/renamed/p" {
		t.Fatalf("import path after go.mod edit = %q", got)
	}
	r.write(t, "go.work", "go 1.22\n\nuse .\n")
	r.update(t)
	if g := check("go.work added"); len(g.GoModules.Workspaces) != 1 {
		t.Fatalf("workspaces = %v", g.GoModules.Workspaces)
	}
	r.remove(t, "go.work")
	r.write(t, "p/a.go", "package pp\n\nfunc A() {}\n")
	r.update(t)
	if got := goIdentities(check("package clause edited"))["p/a.go"].PackageClause; got != "pp" {
		t.Fatalf("package clause = %q", got)
	}
	r.remove(t, "go.mod")
	r.update(t)
	if got := goIdentities(check("root module removed"))["p/a.go"]; got.ModuleState != store.GoModuleNone || got.ImportPath != "" {
		t.Fatalf("identity without a module = %+v", got)
	}
}

func canonicalJSON(v any) string {
	b, _ := json.Marshal(v)
	return string(b)
}

// An index written before package identity was recorded reads as unknown,
// and the next update reparses it into the fresh index's identity.
func TestGoPackageIdentityAfterOlderIndex(t *testing.T) {
	reg := goNativeRegistry()
	r := newGoRepo(t, reg, map[string]string{
		"go.mod": "module example.com/m\n",
		"p/a.go": "package p\n\nfunc A() {}\n",
	})
	_, fresh := r.semanticJSON(t)
	db, err := sql.Open("sqlite", r.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		`DELETE FROM go_file_identity_evidence`,
		`DELETE FROM go_module_evidence`,
		`DELETE FROM settings WHERE key LIKE 'go.module_evidence.%'`,
		`UPDATE files SET parser_profile='go-ast:go:v2' WHERE language='go'`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	old, _ := r.semanticJSON(t)
	if old.GoModules != nil || len(goIdentities(old)) != 0 {
		t.Fatalf("older index reports identity: %s", canonicalJSON(old))
	}
	r.update(t)
	if _, got := r.semanticJSON(t); got != fresh {
		t.Fatalf("update of an older index:\n got %s\nwant %s", got, fresh)
	}
}
