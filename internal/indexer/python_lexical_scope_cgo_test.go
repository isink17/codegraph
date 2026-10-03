//go:build cgo

package indexer

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/parser"
	pyparser "github.com/isink17/codegraph/internal/parser/python"
	tsparser "github.com/isink17/codegraph/internal/parser/treesitter"
)

func TestPythonLexicalScopeTreeSitterAdapter(t *testing.T) {
	runPythonLexicalCases(t, parser.NewRegistry(tsparser.NewPython()))
}

func TestPythonUnicodeScopeTreeSitterAdapter(t *testing.T) {
	runPythonUnicodeScopeCases(t, parser.NewRegistry(tsparser.NewPython()))
}

func TestTreeSitterPythonProfileUnicodeIdentifiersConverge(t *testing.T) {
	runPythonUnicodeProfileConvergence(t,
		pythonV1Adapter{Adapter: tsparser.NewPython(), id: "treesitter:python:v1"}, tsparser.NewPython())
}

const pythonConditionalDefsSource = `import sys

if sys.platform == "win32":
    def pick():
        return helper()
else:
    def pick():
        return helper()


class Service:
    try:
        import fast
    except ImportError:
        def run(self):
            return helper()


def helper():
    return 1
`

// pythonDefsAndCalls renders symbols with spans and every call edge with the
// symbol it is attributed to and the one it binds to.
func pythonDefsAndCalls(t *testing.T, s *profileStore) string {
	t.Helper()
	rows, err := s.raw(t).QueryContext(context.Background(), `
		SELECT 'sym|' || qualified_name || '|' || kind || '|' || start_line || '-' || end_line FROM symbols
		UNION ALL
		SELECT 'call|' || COALESCE(src.qualified_name, '') || '|' || e.dst_name || '@' || e.line || '->' || COALESCE(d.qualified_name, '')
		FROM edges e LEFT JOIN symbols src ON src.id = e.src_symbol_id LEFT JOIN symbols d ON d.id = e.dst_symbol_id
		WHERE e.edge_kind = 'calls'
		ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var line string
		if err := rows.Scan(&line); err != nil {
			t.Fatal(err)
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

func indexPython(t *testing.T, root string, adapter parser.Adapter) *profileStore {
	t.Helper()
	s := newProfileStore(t)
	if _, err := New(s.Store, parser.NewRegistry(adapter), nil).Index(context.Background(), Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	return s
}

// Defs under if/else and try/except are real definitions (CPython's ast:
// pick@4, pick@7, Service.run@15), and the calls in their bodies belong to
// them. The tree-sitter adapter now agrees with the regex adapter, which reads
// scopes from indentation and always kept them.
func TestPythonConditionalDefinitionsMatchRegexAdapter(t *testing.T) {
	root := t.TempDir()
	writeProfileFile(t, filepath.Join(root, "mod.py"), pythonConditionalDefsSource)
	rich := pythonDefsAndCalls(t, indexPython(t, root, tsparser.NewPython()))
	regex := pythonDefsAndCalls(t, indexPython(t, root, pyparser.New()))
	requireRows(t, rich, "tree-sitter graph",
		"sym|mod.pick|function|4-5", "sym|mod.pick|function|7-8", "sym|mod.Service.run|method|15-16",
		"call|mod.pick|helper@5->mod.helper", "call|mod.pick|helper@8->mod.helper",
		"call|mod.Service.run|helper@16->mod.helper")
	if rich != regex {
		t.Fatalf("tree-sitter graph:\n%s\nregex graph:\n%s", rich, regex)
	}
}

// treesitterPythonV2Adapter reproduces treesitter:python:v2 for
// pythonConditionalDefsSource: the same parse without the three defs v2 never
// descended to. The v2 binary wrote exactly that graph for this source.
type treesitterPythonV2Adapter struct{ *tsparser.PythonAdapter }

func (treesitterPythonV2Adapter) Profile() parser.Profile {
	return parser.Profile{ID: "treesitter:python:v2", EmitsCallEdges: true}
}

func (a treesitterPythonV2Adapter) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	pf, err := a.PythonAdapter.Parse(ctx, path, content)
	var kept []graph.Symbol
	for _, s := range pf.Symbols {
		if s.Range.StartLine != 4 && s.Range.StartLine != 7 && s.Range.StartLine != 15 {
			kept = append(kept, s)
		}
	}
	pf.Symbols = kept
	return pf, err
}

func TestTreeSitterPythonProfileConditionalDefsConverge(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "mod.py")
	writeProfileFile(t, path, pythonConditionalDefsSource)
	old, current := treesitterPythonV2Adapter{tsparser.NewPython()}, tsparser.NewPython()

	s := indexPython(t, root, old)
	oldGraph := pythonGraph(t, s)
	if strings.Contains(oldGraph, "mod.pick") || strings.Contains(oldGraph, "call|") {
		t.Fatalf("v2 fixture holds defs or calls v2 never wrote:\n%s", oldGraph)
	}
	upgraded := New(s.Store, parser.NewRegistry(current), nil)
	summary, err := upgraded.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if summary.FilesChanged != 1 || strings.Join(summary.ParserProfileLanguages, ",") != "python" {
		t.Fatalf("profile bump did not reparse: changed=%d languages=%v", summary.FilesChanged, summary.ParserProfileLanguages)
	}
	got := pythonGraph(t, s)
	requireRows(t, got, "upgraded graph", "sym|mod.pick|function|4-5", "call|helper@16->mod.helper")
	if want := pythonGraph(t, indexPython(t, root, current)); got != want {
		t.Fatalf("upgraded graph:\n%s\nfrom-scratch graph:\n%s", got, want)
	}
	if again, err := upgraded.Update(ctx, Options{RepoRoot: root}); err != nil || again.FilesChanged != 0 {
		t.Fatalf("second update: %v changed=%d", err, again.FilesChanged)
	}
	if _, err := New(s.Store, parser.NewRegistry(old), nil).Update(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	if got := pythonGraph(t, s); got != oldGraph {
		t.Fatalf("v2 update over a v3 graph:\n%s\nfrom-scratch v2 graph:\n%s", got, oldGraph)
	}
	if _, err := upgraded.Update(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	writeProfileFile(t, path, pythonConditionalDefsSource+"\n\nif sys.argv:\n    def late():\n        return helper()\n")
	if _, err := upgraded.Update(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	got = pythonGraph(t, s)
	requireRows(t, got, "edited graph", "sym|mod.late|function|24-25", "call|helper@25->mod.helper")
	if want := pythonGraph(t, indexPython(t, root, current)); got != want {
		t.Fatalf("edited graph:\n%s\nfrom-scratch graph:\n%s", got, want)
	}
}

func TestPythonNestedVisibilityTreeSitterAdapter(t *testing.T) {
	runPythonNestedVisibilityCases(t, parser.NewRegistry(tsparser.NewPython()))
}
