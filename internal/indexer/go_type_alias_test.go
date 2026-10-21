package indexer

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/parser"
)

var goTypeAliasFiles = map[string]string{
	"go.mod": "module example.com/m\n\ngo 1.24\n",
	"q/q.go": "package q\n\ntype T struct{}\n",
	"p/a.go": "package p\n\nimport \"example.com/m/q\"\n\ntype Base struct{}\n\nfunc NewBase() Base { return Base{} }\n\ntype Plain = Base\n\ntype (\n\tGrouped = Base\n\tCount   int\n)\n\ntype Remote = q.T\n\ntype GenAlias[T any] = []T\n\nfunc Local() {\n\ttype LocalAlias = Base\n\tvar _ LocalAlias\n}\n",
	"p/b.go": "package p\n\nfunc Conv() Plain { return Plain(NewBase()) }\n",
}

// A go/ast index written before alias signatures were prefixed with `= `.
func TestGoTypeAliasProfileUpgrade(t *testing.T) {
	runGoTypeAliasProfileUpgrade(t, goNativeRegistry(), "go-ast:go:v5", "go-ast:go:v6",
		`UPDATE symbols SET signature=substr(signature, 3) WHERE language='go' AND signature LIKE '= %'`,
		`UPDATE symbols SET signature='[T any] []T' WHERE language='go' AND qualified_name='p.GenAlias'`)
}

// runGoTypeAliasProfileUpgrade rewrites a fresh index into what the previous
// profile stored, then checks that a path-scoped update reparses every Go
// file into the graph a fresh index has.
func runGoTypeAliasProfileUpgrade(t *testing.T, reg *parser.Registry, previous, current string, stale ...string) {
	t.Helper()
	r := newGoRepo(t, reg, goTypeAliasFiles)
	r.store.SetParserProfiles(reg.LanguageProfiles())
	_, freshJSON := r.semanticJSON(t)

	db, err := sql.Open("sqlite", r.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, stmt := range append(stale, `UPDATE files SET parser_profile='`+previous+`' WHERE language='go'`) {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	if _, stale := r.semanticJSON(t); stale == freshJSON {
		t.Fatal("the previous profile's rows equal the fresh graph")
	}
	f, err := r.store.FreshnessStatus(r.ctx, r.repoID)
	if err != nil {
		t.Fatal(err)
	}
	if c := f.ParserProfileCompatibility; c.State != "obsolete" || c.Reason != "older_version" || len(c.Languages) != 1 || c.Languages[0].Current != current {
		t.Fatalf("older index compatibility = %+v", c)
	}

	summary, err := r.idx.Update(r.ctx, Options{RepoRoot: r.root, ScanKind: "update", Paths: []string{"p/b.go"}})
	if err != nil {
		t.Fatal(err)
	}
	if summary.FilesChanged != 3 || strings.Join(summary.ParserProfileLanguages, ",") != "go" {
		t.Fatalf("path-scoped upgrade = %+v, want every Go file reparsed", summary)
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
}
