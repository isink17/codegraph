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

const pythonUnicodeLib = `def thé():
    pass
`

const pythonUnicodeSource = `from lib import thé


def café():
    pass


def caller():
    café()
    naïve_func()
    thé()


def shadow():
    café = make()
    return café()


def make():
    return café


def naïve_func():
    pass
`

func isASCII(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return r > unicode.MaxASCII }) < 0
}

// pythonV1Adapter reproduces, for pythonUnicodeSource, what an adapter wrote
// before Python names were read as Unicode: the shared import and local-binding
// helpers dropped every Unicode name, and with it the import's module. With
// regex set it also reproduces python-regex:python:v1, which additionally
// dropped every Unicode symbol and call and recorded `naïve_func()` as a call
// to `ve_func`. The bbe7c01 binaries wrote exactly these rows for this source.
// It stamps the old profile id, which is all planParserProfiles compares.
type pythonV1Adapter struct {
	parser.Adapter
	id    string
	regex bool
}

func (a pythonV1Adapter) Profile() parser.Profile {
	return parser.Profile{ID: a.id, EmitsCallEdges: true}
}

func (a pythonV1Adapter) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	pf, err := a.Adapter.Parse(ctx, path, content)
	if err != nil {
		return pf, err
	}
	var scope []graph.ScopeImport
	var imports []string
	seen := map[string]bool{}
	for _, b := range pf.Scope.Imports {
		if !isASCII(b.LocalName + b.ImportedName + b.SourceSpecifier) {
			continue
		}
		scope = append(scope, b)
		if b.SourceSpecifier != "" && !seen[b.SourceSpecifier] {
			seen[b.SourceSpecifier] = true
			imports = append(imports, b.SourceSpecifier)
		}
	}
	pf.Scope.Imports, pf.Imports = scope, imports
	if !a.regex {
		return pf, nil
	}
	var symbols []graph.Symbol
	for _, s := range pf.Symbols {
		if isASCII(s.Name) {
			symbols = append(symbols, s)
		}
	}
	var edges []graph.Edge
	for _, e := range pf.Edges {
		if e.DstName == "naïve_func" {
			e.DstName = "ve_func"
		}
		if isASCII(e.DstName) {
			edges = append(edges, e)
		}
	}
	var refs []graph.Reference
	for _, r := range pf.References {
		if r.Name == "naïve_func" {
			r.Name, r.QualifiedName = "ve_func", "ve_func"
		}
		if isASCII(r.Name) {
			refs = append(refs, r)
		}
	}
	pf.Symbols, pf.Edges, pf.References = symbols, edges, refs
	return pf, nil
}

// pythonGraph renders the parser-owned Python facts a profile change must
// converge: symbols with their spans, call edges with the symbol each binds
// to, references, import and local-binding evidence, and file provenance.
func pythonGraph(t *testing.T, s *profileStore) string {
	t.Helper()
	rows, err := s.raw(t).QueryContext(context.Background(), `
		SELECT 'sym|' || qualified_name || '|' || kind || '|' || start_line || '-' || end_line FROM symbols
		UNION ALL
		SELECT 'call|' || e.dst_name || '@' || e.line || '->' || COALESCE(d.qualified_name, '')
		FROM edges e LEFT JOIN symbols d ON d.id = e.dst_symbol_id WHERE e.edge_kind = 'calls'
		UNION ALL
		SELECT 'ref|' || name || '@' || start_line || '->' || COALESCE(symbol_id IS NOT NULL, 0) FROM references_tbl
		UNION ALL
		SELECT 'scope|' || f.path || '|' || s.owner_module || '|' || s.import_kind || '|' || s.source_specifier
		       || '|' || s.imported_name || '|' || s.local_name
		FROM scope_import_evidence s JOIN files f ON f.id = s.file_id
		UNION ALL
		SELECT 'import|' || f.path || '|' || i.import_path FROM file_imports i JOIN files f ON f.id = i.file_id
		UNION ALL
		SELECT 'prov|' || path || '=' || parser_profile || ':' || parser_call_edges FROM files`)
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

func requireRows(t *testing.T, graph, label string, rows ...string) {
	t.Helper()
	for _, row := range rows {
		if !strings.Contains("\n"+graph+"\n", "\n"+row+"\n") {
			t.Fatalf("%s lacks %q:\n%s", label, row, graph)
		}
	}
}

// Reading Python names as Unicode changes what an unchanged file produces, so
// the profile bump alone must reparse it: an upgraded old graph, a new graph an
// older binary later updates, and a new graph whose file then changes must each
// equal a from-scratch index by the parser that last ran.
func runPythonUnicodeProfileConvergence(t *testing.T, old pythonV1Adapter, current parser.Adapter) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	writeProfileFile(t, filepath.Join(root, "lib.py"), pythonUnicodeLib)
	path := filepath.Join(root, "mod.py")
	writeProfileFile(t, path, pythonUnicodeSource)
	currentID := current.(parser.ProfileProvider).Profile().ID

	s := newProfileStore(t)
	if _, err := New(s.Store, parser.NewRegistry(old), nil).Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("old index: %v", err)
	}
	oldGraph := pythonGraph(t, s)
	if strings.Contains(oldGraph, "scope|") || strings.Contains(oldGraph, "import|") {
		t.Fatalf("old fixture holds import or binding evidence it never wrote:\n%s", oldGraph)
	}
	requireRows(t, oldGraph, "old graph", "prov|mod.py="+old.id+":1")

	upgraded := New(s.Store, parser.NewRegistry(current), nil)
	summary, err := upgraded.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if summary.FilesChanged != 2 || strings.Join(summary.ParserProfileLanguages, ",") != "python" {
		t.Fatalf("profile bump did not reparse the unchanged files: changed=%d languages=%v",
			summary.FilesChanged, summary.ParserProfileLanguages)
	}
	got := pythonGraph(t, s)
	requireRows(t, got, "upgraded graph",
		"prov|mod.py="+currentID+":1",
		"scope|mod.py||named|lib|thé|thé",
		"import|mod.py|lib",
		"scope|mod.py|shadow|local_binding|||café",
		"call|café@9->mod.café",
		"call|naïve_func@10->mod.naïve_func",
		"call|thé@11->lib.thé",
		// The local `café` shadows the module def, so the call stays unbound.
		"call|café@16->")
	if strings.Contains(got, "|ve_func@") {
		t.Fatalf("the old fragment call survived the upgrade:\n%s", got)
	}
	if want := freshPythonGraph(t, root, current); got != want {
		t.Fatalf("upgraded graph:\n%s\nfrom-scratch graph:\n%s", got, want)
	}
	again, err := upgraded.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if again.FilesChanged != 0 || len(again.ParserProfileLanguages) != 0 {
		t.Fatalf("second update reparsed: changed=%d languages=%v", again.FilesChanged, again.ParserProfileLanguages)
	}

	// An older binary on the same graph: both profiles build call graphs, so
	// this is a reparse back to the old output, not a refused downgrade.
	if _, err := New(s.Store, parser.NewRegistry(old), nil).Update(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("old update over a new graph: %v", err)
	}
	if got := pythonGraph(t, s); got != oldGraph {
		t.Fatalf("old update over a new graph:\n%s\nfrom-scratch old graph:\n%s", got, oldGraph)
	}

	// Back on the current parser, then edit the file: the edit converges like
	// a fresh index.
	if _, err := upgraded.Update(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	writeProfileFile(t, path, pythonUnicodeSource+"\n\ndef ñu(ü):\n    return ü()\n")
	if _, err := upgraded.Update(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("update after edit: %v", err)
	}
	got = pythonGraph(t, s)
	requireRows(t, got, "edited graph", "scope|mod.py|ñu|local_binding|||ü", "call|ü@28->")
	if want := freshPythonGraph(t, root, current); got != want {
		t.Fatalf("edited graph:\n%s\nfrom-scratch graph:\n%s", got, want)
	}
}

func TestPythonRegexProfileUnicodeIdentifiersConverge(t *testing.T) {
	runPythonUnicodeProfileConvergence(t,
		pythonV1Adapter{Adapter: pyparser.New(), id: "python-regex:python:v1", regex: true}, pyparser.New())
}

// pythonRegexV2Adapter reproduces python-regex:python:v2 for
// pythonKeywordSource: the same graph plus the calls v2 read out of keyword
// syntax. The v2 binary wrote exactly these extra call rows for this source.
type pythonRegexV2Adapter struct{ *pyparser.Adapter }

func (pythonRegexV2Adapter) Profile() parser.Profile {
	return parser.Profile{ID: "python-regex:python:v2", EmitsCallEdges: true}
}

func (a pythonRegexV2Adapter) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	pf, err := a.Adapter.Parse(ctx, path, content)
	if err != nil || filepath.Base(path) != "mod.py" {
		return pf, err
	}
	for _, c := range []struct {
		name string
		line int
	}{{"as", 6}, {"case", 9}, {"Point", 11}, {"Point", 11}, {"match", 13}} {
		pf.Edges = append(pf.Edges, graph.Edge{DstName: c.name, Kind: "calls", Line: c.line})
		pf.References = append(pf.References, graph.Reference{Kind: "call", Name: c.name, QualifiedName: c.name,
			Range: graph.Position{StartLine: c.line, EndLine: c.line}})
	}
	return pf, nil
}

const pythonKeywordSource = `class Point:
    pass


def run(value, cm):
    with cm as (a, b):
        pass
    match value:
        case (1, 2):
            pass
        case Point(x=0) | Point(x=1):
            pass
    match (
        value
    ):
        case _:
            pass
`

// v3 stops reading keyword syntax as calls; the bump alone must drop the v2
// rows from an unchanged file, and every transition must equal a fresh index.
func TestPythonRegexProfileKeywordSyntaxConverges(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "mod.py")
	writeProfileFile(t, path, pythonKeywordSource)
	old, current := pythonRegexV2Adapter{pyparser.New()}, pyparser.New()

	s := newProfileStore(t)
	if _, err := New(s.Store, parser.NewRegistry(old), nil).Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	oldGraph := pythonGraph(t, s)
	requireRows(t, oldGraph, "v2 graph", "call|as@6->", "call|case@9->", "call|match@13->")

	upgraded := New(s.Store, parser.NewRegistry(current), nil)
	summary, err := upgraded.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if summary.FilesChanged != 1 || strings.Join(summary.ParserProfileLanguages, ",") != "python" {
		t.Fatalf("profile bump did not reparse: changed=%d languages=%v", summary.FilesChanged, summary.ParserProfileLanguages)
	}
	got := pythonGraph(t, s)
	if strings.Contains(got, "call|") {
		t.Fatalf("keyword syntax still reads as calls after the upgrade:\n%s", got)
	}
	if want := freshPythonGraph(t, root, current); got != want {
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
	writeProfileFile(t, path, pythonKeywordSource+"    match(value)\n")
	if _, err := upgraded.Update(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	got = pythonGraph(t, s)
	requireRows(t, got, "edited graph", "call|match@18->")
	if want := freshPythonGraph(t, root, current); got != want {
		t.Fatalf("edited graph:\n%s\nfrom-scratch graph:\n%s", got, want)
	}
}
