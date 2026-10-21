//go:build cgo

package indexer

import (
	"fmt"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/parser"
	tsparser "github.com/isink17/codegraph/internal/parser/treesitter"
)

var goFuncLitValueFiles = map[string]string{
	"go.mod": "module example.com/m\n\ngo 1.22\n",
	"p/a.go": `package p

var Table = func() []int {
	var out []int
	const k = 2
	type local int
	short := 1
	return append(out, k, short, int(local(3)))
}()

var Nested = func() func() int {
	var outer int
	return func() int {
		var inner = outer
		const deep = 1
		return inner + deep
	}
}()

var Handlers = map[string]func() int{
	"x": func() int {
		const c = 3
		var d int
		return c + d
	},
}

var (
	G1 = func() int { var g int; return g }()
	G2, G3 int
)

const (
	K1 = 1
	K2
)

func Plain() int {
	var body int
	const bk = 1
	return body + bk
}

const K3, K4 = 3, 4

var M1, M2 = func() int { var z int; return z }(), 2
`,
	"p/b.go": "package p\n\nfunc Use() int { return Table[0] + Nested() + Handlers[\"x\"]() + G1 + Plain() }\n",
}

// Declarations inside a function literal that initializes a package-level
// value are locals: neither Go parser family stores them as package values.
// Every name of a multi-name spec is a package value in both.
func TestGoFuncLitValueFamilyParity(t *testing.T) {
	native, _ := newGoRepo(t, goNativeRegistry(), goFuncLitValueFiles).semanticJSON(t)
	g, _ := newGoRepo(t, parser.NewRegistry(tsparser.NewGo()), goFuncLitValueFiles).semanticJSON(t)
	nd, ne := canonicalJSON(native.Declarations), canonicalJSON(native.Edges)
	td, te := canonicalJSON(g.Declarations), canonicalJSON(g.Edges)
	if nd != td {
		t.Fatalf("declarations differ:\n go/ast:      %s\n tree-sitter: %s", nd, td)
	}
	if ne != te {
		t.Fatalf("edges differ:\n go/ast:      %s\n tree-sitter: %s", ne, te)
	}
	for _, want := range []string{"p.Table", "p.Nested", "p.Handlers", "p.G1", "p.G2", "p.G3", "p.K1", "p.K2", "p.K3", "p.K4", "p.M1", "p.M2", "p.Plain", "p.Use"} {
		if !strings.Contains(td, `"qualified_name":"`+want+`"`) {
			t.Errorf("declarations lack %s: %s", want, td)
		}
	}
	for _, local := range []string{"out", "k", "local", "short", "outer", "inner", "deep", "c", "d", "g", "z", "body", "bk"} {
		if strings.Contains(td, `"qualified_name":"p.`+local+`"`) {
			t.Errorf("function-local %s became a declaration: %s", local, td)
		}
	}
}

// A tree-sitter index written by the previous profile holds the locals of a
// function literal initializer as package values and only the first name of a
// multi-name spec.
func TestGoFuncLitValueProfileUpgradeTreeSitter(t *testing.T) {
	stale := []string{`DELETE FROM symbols WHERE language='go' AND qualified_name IN ('p.G3','p.K4','p.M2')`}
	for _, local := range []struct {
		name      string
		line, col int
	}{{"out", 4, 6}, {"k", 5, 8}, {"inner", 14, 7}, {"c", 22, 9}, {"g", 29, 24}} {
		stale = append(stale, fmt.Sprintf(`INSERT INTO symbols (repo_id, file_id, language, kind, name, qualified_name, container_name,
			signature, visibility, start_line, start_col, end_line, end_col, doc_summary, stable_key)
			SELECT repo_id, file_id, language, kind, '%[1]s', 'p.%[1]s', container_name, '', 'package', %[2]d, %[3]d, %[2]d, %[4]d, '', 'value:p:%[1]s'
			FROM symbols WHERE language='go' AND qualified_name='p.Table'`, local.name, local.line, local.col, local.col+len(local.name)))
	}
	runGoProfileUpgrade(t, parser.NewRegistry(tsparser.NewGo()), goFuncLitValueFiles, "treesitter:go:v5", "treesitter:go:v6", stale...)
}
