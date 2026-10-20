package indexer

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/parser"
	"github.com/isink17/codegraph/internal/store"
)

var goSignatureProfileFiles = map[string]string{
	"go.mod": "module example.com/m\n\ngo 1.22\n",
	"p/a.go": "package p\n\ntype Box[T comparable] struct{ v T }\n\nfunc A[K comparable, V ~int | ~string](n int, s ...string) (string, error) { return \"\", nil }\n",
	"p/b.go": "package p\n\nfunc B() { _, _ = A[int, int](1) }\n",
}

// The signatures the previous profile of either Go parser family stored for
// goSignatureProfileFiles: no type parameter lists.
var goSignaturesWithoutTypeParams = map[string]string{
	"p.A":   "func A(n int, s ...string) (string, error)",
	"p.Box": "struct",
}

func TestGoSignatureProfileUpgrade(t *testing.T) {
	runGoSignatureProfileUpgrade(t, goNativeRegistry(), "go-ast:go:v4", "go-ast:go:v6")
}

// An index whose Go signatures were written by the previous profile is
// reported obsolete, and even a path-scoped update reparses every Go file into
// the fresh index's graph, so no mixed-profile graph is left behind.
func runGoSignatureProfileUpgrade(t *testing.T, reg *parser.Registry, previous, current string) {
	t.Helper()
	files := map[string]string{}
	for k, v := range goSignatureProfileFiles {
		files[k] = v
	}
	r := newGoRepo(t, reg, files)
	r.store.SetParserProfiles(reg.LanguageProfiles())
	fresh, freshJSON := r.semanticJSON(t)
	if got := goSignatureSet(fresh); got != "p.A: func A[K comparable, V ~int | ~string](n int, s ...string) (string, error)|p.Box: [T comparable] struct|p.B: func B()" {
		t.Fatalf("fresh signatures = %s", got)
	}

	db, err := sql.Open("sqlite", r.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for name, sig := range goSignaturesWithoutTypeParams {
		if _, err := db.Exec(`UPDATE symbols SET signature=? WHERE language='go' AND qualified_name=?`, sig, name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`UPDATE files SET parser_profile=? WHERE language='go'`, previous); err != nil {
		t.Fatal(err)
	}
	f, err := r.store.FreshnessStatus(r.ctx, r.repoID)
	if err != nil {
		t.Fatal(err)
	}
	if c := f.ParserProfileCompatibility; c.State != "obsolete" || c.Reason != "older_version" || len(c.Languages) != 1 ||
		strings.Join(c.Languages[0].Stored, ",") != previous || c.Languages[0].Current != current {
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

	edited := "package p\n\ntype Box[T any] struct{ v T }\n\nfunc A[K comparable, V any](n int, s ...string) (string, error) { return \"\", nil }\n"
	r.write(t, "p/a.go", edited)
	if _, err := r.idx.Update(r.ctx, Options{RepoRoot: r.root, ScanKind: "update", Paths: []string{"p/a.go"}}); err != nil {
		t.Fatal(err)
	}
	g, incremental := r.semanticJSON(t)
	files["p/a.go"] = edited
	_, reindexed := newGoRepo(t, reg, files).semanticJSON(t)
	if incremental != reindexed {
		t.Fatalf("incremental update differs from a fresh index:\n got %s\nwant %s", incremental, reindexed)
	}
	if got := goSignatureSet(g); got != "p.A: func A[K comparable, V any](n int, s ...string) (string, error)|p.Box: [T any] struct|p.B: func B()" {
		t.Fatalf("edited signatures = %s", got)
	}

	// An index a newer build wrote is not claimed as current or older, and an
	// update reparses it into this build's graph rather than keeping its rows.
	newer := previous[:strings.LastIndexByte(previous, ':')] + ":v99"
	if _, err := db.Exec(`UPDATE files SET parser_profile=? WHERE language='go'`, newer); err != nil {
		t.Fatal(err)
	}
	if f, err = r.store.FreshnessStatus(r.ctx, r.repoID); err != nil || f.ParserProfileCompatibility.Reason != "newer_version" {
		t.Fatalf("newer index compatibility = %+v, %v", f.ParserProfileCompatibility, err)
	}
	if _, err := r.idx.Update(r.ctx, Options{RepoRoot: r.root, ScanKind: "update", Paths: []string{"p/b.go"}}); err != nil {
		t.Fatal(err)
	}
	if _, got := r.semanticJSON(t); got != reindexed {
		t.Fatalf("index after a newer build differs from a fresh one:\n got %s\nwant %s", got, reindexed)
	}
}

func goSignatureSet(g store.SemanticGraph) string {
	var out []string
	for _, d := range g.Declarations {
		if d.Language == "go" {
			out = append(out, d.QualifiedName+": "+d.Signature)
		}
	}
	return strings.Join(out, "|")
}

// Both Go parser families keep one semantic epoch, so switching between a cgo
// and a non-cgo build is never refused as a semantic downgrade.
func TestGoParserFamiliesShareSemanticEpoch(t *testing.T) {
	if a, b := parser.SemanticEpochForProfile("go-ast:go:v6"), parser.SemanticEpochForProfile("treesitter:go:v6"); a == 0 || a != b {
		t.Fatalf("go-ast epoch %d, tree-sitter epoch %d", a, b)
	}
}
