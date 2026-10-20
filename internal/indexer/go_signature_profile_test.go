package indexer

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/parser"
)

// An index whose Go signatures were written by the previous go-ast profile is
// reported obsolete, and even a path-scoped update reparses every Go file into
// the fresh index's graph, so no mixed-profile graph is left behind.
func TestGoSignatureProfileUpgrade(t *testing.T) {
	reg := goNativeRegistry()
	files := map[string]string{
		"go.mod": "module example.com/m\n\ngo 1.22\n",
		"p/a.go": "package p\n\nfunc A(n int, s ...string) (string, error) { return \"\", nil }\n",
		"p/b.go": "package p\n\nfunc B() { _, _ = A(1) }\n",
	}
	r := newGoRepo(t, reg, files)
	r.store.SetParserProfiles(reg.LanguageProfiles())
	fresh, freshJSON := r.semanticJSON(t)
	sigs := map[string]string{}
	for _, d := range fresh.Declarations {
		sigs[d.QualifiedName] = d.Signature
	}
	if got := sigs["p.A"]; got != "func A(n int, s ...string) (string, error)" {
		t.Fatalf("fresh signature = %q", got)
	}

	db, err := sql.Open("sqlite", r.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range []string{
		`UPDATE symbols SET signature='func ' || name || '*ast.FuncType' WHERE language='go' AND kind='function'`,
		`UPDATE files SET parser_profile='go-ast:go:v3' WHERE language='go'`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	f, err := r.store.FreshnessStatus(r.ctx, r.repoID)
	if err != nil {
		t.Fatal(err)
	}
	if c := f.ParserProfileCompatibility; c.State != "obsolete" || c.Reason != "older_version" || len(c.Languages) != 1 ||
		strings.Join(c.Languages[0].Stored, ",") != "go-ast:go:v3" || c.Languages[0].Current != "go-ast:go:v4" {
		t.Fatalf("older index compatibility = %+v", c)
	}

	summary, err := r.idx.Update(r.ctx, Options{RepoRoot: r.root, ScanKind: "update", Paths: []string{"p/b.go"}})
	if err != nil {
		t.Fatal(err)
	}
	if summary.FilesChanged != 2 || strings.Join(summary.ParserProfileLanguages, ",") != "go" {
		t.Fatalf("path-scoped upgrade = %+v, want both Go files reparsed", summary)
	}
	if _, got := r.semanticJSON(t); got != freshJSON {
		t.Fatalf("upgraded index differs from a fresh one:\n got %s\nwant %s", got, freshJSON)
	}
	if f, err = r.store.FreshnessStatus(r.ctx, r.repoID); err != nil || f.ParserProfileCompatibility.State != "current" {
		t.Fatalf("after upgrade compatibility = %+v, %v", f.ParserProfileCompatibility, err)
	}
	if again, err := r.idx.Update(r.ctx, Options{RepoRoot: r.root, ScanKind: "update"}); err != nil || again.FilesChanged != 0 {
		t.Fatalf("second update = %+v, %v; want a no-op", again, err)
	}

	edited := "package p\n\nfunc A(n int64, s []string) string { return \"\" }\n"
	r.write(t, "p/a.go", edited)
	if _, err := r.idx.Update(r.ctx, Options{RepoRoot: r.root, ScanKind: "update", Paths: []string{"p/a.go"}}); err != nil {
		t.Fatal(err)
	}
	_, incremental := r.semanticJSON(t)
	files["p/a.go"] = edited
	_, reindexed := newGoRepo(t, reg, files).semanticJSON(t)
	if incremental != reindexed {
		t.Fatalf("incremental update differs from a fresh index:\n got %s\nwant %s", incremental, reindexed)
	}
	if !strings.Contains(incremental, `"func A(n int64, s []string) string"`) {
		t.Fatalf("edited signature not stored: %s", incremental)
	}
}

// The signature fix changes the go-ast profile ID only. Both Go parser
// families keep one semantic epoch, so switching between a cgo and a non-cgo
// build is never refused as a semantic downgrade.
func TestGoParserFamiliesShareSemanticEpoch(t *testing.T) {
	if a, b := parser.SemanticEpochForProfile("go-ast:go:v4"), parser.SemanticEpochForProfile("treesitter:go:v3"); a == 0 || a != b {
		t.Fatalf("go-ast epoch %d, tree-sitter epoch %d", a, b)
	}
}
