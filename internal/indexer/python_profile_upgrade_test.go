package indexer

import (
	"context"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"unicode"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/parser"
	pyparser "github.com/isink17/codegraph/internal/parser/python"
)

const pythonUnicodeSource = `def café():
    pass

def caller():
    café()
    naïve_func()

def naïve_func():
    pass
`

// pythonRegexV1Adapter reproduces what python-regex:python:v1, whose identifier
// class was ASCII-only, wrote for pythonUnicodeSource: no Unicode-named symbol
// or call, and the ASCII tail of `naïve_func()` as a call to `ve_func`. The v1
// binary indexed that exact source to those rows. It stamps the v1 profile id,
// which is all planParserProfiles compares.
type pythonRegexV1Adapter struct{ *pyparser.Adapter }

func (pythonRegexV1Adapter) Profile() parser.Profile {
	return parser.Profile{ID: "python-regex:python:v1", EmitsCallEdges: true}
}

func isASCII(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return r > unicode.MaxASCII }) < 0
}

func (a pythonRegexV1Adapter) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	pf, err := a.Adapter.Parse(ctx, path, content)
	if err != nil {
		return pf, err
	}
	var symbols []graph.Symbol
	for _, s := range pf.Symbols {
		if isASCII(s.Name) {
			symbols = append(symbols, s)
		}
	}
	var edges []graph.Edge
	for _, e := range pf.Edges {
		if isASCII(e.DstName) {
			edges = append(edges, e)
		} else if e.DstName == "naïve_func" {
			e.DstName = "ve_func"
			edges = append(edges, e)
		}
	}
	var refs []graph.Reference
	for _, r := range pf.References {
		if isASCII(r.Name) {
			refs = append(refs, r)
		} else if r.Name == "naïve_func" {
			r.Name, r.QualifiedName = "ve_func", "ve_func"
			refs = append(refs, r)
		}
	}
	pf.Symbols, pf.Edges, pf.References = symbols, edges, refs
	return pf, nil
}

// pythonGraph renders the parser-owned Python facts a profile change must
// converge: symbols with their spans, and call edges with the symbol each one
// binds to.
func pythonGraph(t *testing.T, s *profileStore) string {
	t.Helper()
	rows, err := s.raw(t).QueryContext(context.Background(), `
		SELECT 'sym|' || qualified_name || '|' || kind || '|' || start_line || '-' || end_line FROM symbols
		UNION ALL
		SELECT 'call|' || e.dst_name || '@' || e.line || '->' || COALESCE(d.qualified_name, '')
		FROM edges e LEFT JOIN symbols d ON d.id = e.dst_symbol_id WHERE e.edge_kind = 'calls'
		UNION ALL
		SELECT 'ref|' || name || '@' || start_line || '->' || COALESCE(symbol_id IS NOT NULL, 0) FROM references_tbl`)
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
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return strings.Join(out, "\n")
}

func freshPythonGraph(t *testing.T, root string, adapter parser.Adapter) string {
	t.Helper()
	fresh := newProfileStore(t)
	if _, err := New(fresh.Store, parser.NewRegistry(adapter), nil).Index(context.Background(), Options{RepoRoot: root}); err != nil {
		t.Fatalf("fresh index: %v", err)
	}
	return pythonGraph(t, fresh)
}

// The Unicode identifier fix changes what an unchanged Python file produces, so
// the profile bump alone must reparse it: an upgraded v1 graph, a v2 graph a v1
// binary later updates, and a v2 graph whose file then changes must each equal
// a from-scratch index by the parser that last ran.
func TestPythonRegexProfileUnicodeIdentifiersConverge(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "mod.py")
	writeProfileFile(t, path, pythonUnicodeSource)
	v1, v2 := pythonRegexV1Adapter{pyparser.New()}, pyparser.New()

	s := newProfileStore(t)
	if _, err := New(s.Store, parser.NewRegistry(v1), nil).Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("v1 index: %v", err)
	}
	v1Graph := pythonGraph(t, s)
	if !strings.Contains(v1Graph, "call|ve_func@6->") || strings.Contains(v1Graph, "café") {
		t.Fatalf("v1 fixture does not hold the v1 defect:\n%s", v1Graph)
	}

	upgraded := New(s.Store, parser.NewRegistry(v2), nil)
	summary, err := upgraded.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatalf("v2 update: %v", err)
	}
	if summary.FilesChanged != 1 || strings.Join(summary.ParserProfileLanguages, ",") != "python" {
		t.Fatalf("profile bump did not reparse the unchanged file: changed=%d languages=%v",
			summary.FilesChanged, summary.ParserProfileLanguages)
	}
	got := pythonGraph(t, s)
	for _, want := range []string{"sym|mod.café|function|1-2", "sym|mod.naïve_func|function|8-9",
		"call|café@5->mod.café", "call|naïve_func@6->mod.naïve_func"} {
		if !strings.Contains("\n"+got+"\n", "\n"+want+"\n") {
			t.Fatalf("upgraded graph lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "|ve_func@") {
		t.Fatalf("the v1 fragment call survived the upgrade:\n%s", got)
	}
	if want := freshPythonGraph(t, root, v2); got != want {
		t.Fatalf("upgraded graph:\n%s\nfrom-scratch v2 graph:\n%s", got, want)
	}
	again, err := upgraded.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if again.FilesChanged != 0 || len(again.ParserProfileLanguages) != 0 {
		t.Fatalf("second update reparsed: changed=%d languages=%v", again.FilesChanged, again.ParserProfileLanguages)
	}

	// An older binary on the same graph: both profiles build call graphs, so
	// this is a reparse back to v1 output, not a refused downgrade.
	if _, err := New(s.Store, parser.NewRegistry(v1), nil).Update(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("v1 update over a v2 graph: %v", err)
	}
	if got := pythonGraph(t, s); got != v1Graph {
		t.Fatalf("v1 update over a v2 graph:\n%s\nfrom-scratch v1 graph:\n%s", got, v1Graph)
	}

	// Back on v2, then edit the file: the edit converges like a fresh index.
	if _, err := upgraded.Update(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	writeProfileFile(t, path, pythonUnicodeSource+"\ndef ñu():\n    café()\n")
	if _, err := upgraded.Update(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("v2 update after edit: %v", err)
	}
	got = pythonGraph(t, s)
	if !strings.Contains(got, "call|café@12->mod.café") {
		t.Fatalf("edited graph lacks the new call:\n%s", got)
	}
	if want := freshPythonGraph(t, root, v2); got != want {
		t.Fatalf("edited graph:\n%s\nfrom-scratch v2 graph:\n%s", got, want)
	}
}
