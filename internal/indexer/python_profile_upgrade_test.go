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

const pythonNFKCLib = `def full():
    return "lib.full"
`

// pythonNFKCSource is the d5 program CPython runs: the local `ｆｕｌｌ` binds
// `full` and shadows the import, and `def ｇｏ` defines `go`.
const pythonNFKCSource = `from lib import full


def ｇｏ():
    return "go"


def run():
    ｆｕｌｌ = lambda: "local"
    ｇｏ()
    return full()
`

// pythonSpellingAdapter reproduces, for pythonNFKCSource, what both Python
// adapters wrote before names were NFKC-folded: every name as spelled. The
// previous binaries wrote exactly these rows for this source. It stamps the
// old profile id, which is all planParserProfiles compares.
type pythonSpellingAdapter struct {
	parser.Adapter
	id string
}

func (a pythonSpellingAdapter) Profile() parser.Profile {
	return parser.Profile{ID: a.id, EmitsCallEdges: true}
}

func (a pythonSpellingAdapter) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	pf, err := a.Adapter.Parse(ctx, path, content)
	if err != nil || filepath.Base(path) != "mod.py" {
		return pf, err
	}
	spell := strings.NewReplacer("go", "ｇｏ").Replace
	for i := range pf.Symbols {
		if s := &pf.Symbols[i]; s.Name == "go" {
			s.Name, s.QualifiedName, s.StableKey = spell(s.Name), spell(s.QualifiedName), spell(s.StableKey)
		}
	}
	for i := range pf.Edges {
		if e := &pf.Edges[i]; e.DstName == "go" {
			e.DstName = spell(e.DstName)
			if e.Evidence == "go" {
				e.Evidence = e.DstName
			}
		}
	}
	for i := range pf.References {
		if r := &pf.References[i]; r.Name == "go" {
			r.Name, r.QualifiedName = spell(r.Name), spell(r.QualifiedName)
		}
	}
	for i := range pf.Scope.Imports {
		if b := &pf.Scope.Imports[i]; b.OwnerModule == "run" && b.LocalName == "full" {
			b.LocalName = "ｆｕｌｌ"
		}
	}
	return pf, nil
}

// NFKC-folding Python names changes what an unchanged file produces, so the
// profile bump alone must reparse it, and every transition must equal a
// from-scratch index by the parser that last ran.
func runPythonNFKCProfileConvergence(t *testing.T, old pythonSpellingAdapter, current parser.Adapter) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	writeProfileFile(t, filepath.Join(root, "lib.py"), pythonNFKCLib)
	path := filepath.Join(root, "mod.py")
	writeProfileFile(t, path, pythonNFKCSource)
	currentID := current.(parser.ProfileProvider).Profile().ID

	s := newProfileStore(t)
	if _, err := New(s.Store, parser.NewRegistry(old), nil).Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("old index: %v", err)
	}
	oldGraph := pythonGraph(t, s)
	requireRows(t, oldGraph, "old graph",
		"prov|mod.py="+old.id+":1",
		"sym|mod.ｇｏ|function|4-5",
		"scope|mod.py|run|local_binding|||ｆｕｌｌ",
		"call|ｇｏ@10->mod.ｇｏ",
		// The wrong edge: CPython calls the local.
		"call|full@11->lib.full")

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
		"sym|mod.go|function|4-5",
		"scope|mod.py|run|local_binding|||full",
		"call|go@10->mod.go",
		"call|full@11->")
	if strings.ContainsAny(got, "ｆｇ") {
		t.Fatalf("a fullwidth spelling survived the upgrade:\n%s", got)
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

	// An older binary on the same graph reparses back to its own output.
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
	writeProfileFile(t, path, pythonNFKCSource+"\n\ndef ﬁx(ｘ):\n    return x()\n")
	if _, err := upgraded.Update(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("update after edit: %v", err)
	}
	got = pythonGraph(t, s)
	requireRows(t, got, "edited graph", "sym|mod.fix|function|14-15", "scope|mod.py|fix|local_binding|||x", "call|x@15->")
	if want := freshPythonGraph(t, root, current); got != want {
		t.Fatalf("edited graph:\n%s\nfrom-scratch graph:\n%s", got, want)
	}
}

func TestPythonRegexProfileNFKCNamesConverge(t *testing.T) {
	runPythonNFKCProfileConvergence(t,
		pythonSpellingAdapter{Adapter: pyparser.New(), id: "python-regex:python:v3"}, pyparser.New())
}

// pythonLambdaMatchSource is a program CPython runs: lam()() calls the lambda's
// parameter and cap(f) calls the capture, never lib.full.
const pythonLambdaMatchSource = `from lib import full


def lam():
    g = lambda full: full()
    return g


def cap(x):
    match x:
        case full:
            return full()
`

// pythonBindingsV4Adapter reproduces, for pythonLambdaMatchSource, what both
// Python adapters wrote before lambda parameters and case captures were local
// bindings: the same rows without those two. The v4 binaries wrote exactly
// these rows for this source. It stamps the old profile id, which is all
// planParserProfiles compares.
type pythonBindingsV4Adapter struct {
	parser.Adapter
	id string
}

func (a pythonBindingsV4Adapter) Profile() parser.Profile {
	return parser.Profile{ID: a.id, EmitsCallEdges: true}
}

func (a pythonBindingsV4Adapter) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	pf, err := a.Adapter.Parse(ctx, path, content)
	if err != nil || filepath.Base(path) != "mod.py" {
		return pf, err
	}
	kept := pf.Scope.Imports[:0]
	for _, b := range pf.Scope.Imports {
		if b.Kind == graph.ScopeImportLocalBinding && b.LocalName == "full" {
			continue
		}
		kept = append(kept, b)
	}
	pf.Scope.Imports = kept
	return pf, nil
}

// Recording lambda parameters and case captures changes what an unchanged
// file produces, so the profile bump alone must reparse it, and every
// transition must equal a from-scratch index by the parser that last ran.
func runPythonLambdaMatchProfileConvergence(t *testing.T, old pythonBindingsV4Adapter, current parser.Adapter) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	writeProfileFile(t, filepath.Join(root, "lib.py"), pythonNFKCLib)
	path := filepath.Join(root, "mod.py")
	writeProfileFile(t, path, pythonLambdaMatchSource)
	currentID := current.(parser.ProfileProvider).Profile().ID

	s := newProfileStore(t)
	if _, err := New(s.Store, parser.NewRegistry(old), nil).Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("old index: %v", err)
	}
	oldGraph := pythonGraph(t, s)
	requireRows(t, oldGraph, "old graph",
		"prov|mod.py="+old.id+":1",
		// The wrong edges: CPython calls the parameter and the capture.
		"call|full@5->lib.full",
		"call|full@12->lib.full")

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
		"scope|mod.py|lam|local_binding|||full",
		"scope|mod.py|cap|local_binding|||full",
		"call|full@5->",
		"call|full@12->")
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

	// An older binary on the same graph reparses back to its own output.
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
	writeProfileFile(t, path, pythonLambdaMatchSource+"\n\ndef pat(v):\n    match v:\n        case {**rest}:\n            return rest()\n")
	if _, err := upgraded.Update(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("update after edit: %v", err)
	}
	got = pythonGraph(t, s)
	requireRows(t, got, "edited graph", "scope|mod.py|pat|local_binding|||rest", "call|rest@18->")
	if want := freshPythonGraph(t, root, current); got != want {
		t.Fatalf("edited graph:\n%s\nfrom-scratch graph:\n%s", got, want)
	}
}

func TestPythonRegexProfileLambdaMatchBindingsConverge(t *testing.T) {
	runPythonLambdaMatchProfileConvergence(t,
		pythonBindingsV4Adapter{Adapter: pyparser.New(), id: "python-regex:python:v4"}, pyparser.New())
}

// pythonDefaultClassSource is a program CPython runs: run() returns 'param'
// and make() 'classattr', so neither call is lib.full.
const pythonDefaultClassSource = `from lib import full


def run(cb=lambda full: full()):
    return cb(lambda: "param")


def make():
    class C:
        full = lambda: "classattr"
        g = full()
    return C.g
`

// pythonBindingsV5Adapter reproduces, for pythonDefaultClassSource, what both
// Python adapters wrote before a def header's default lambdas and a class body
// in a function were scanned for bindings: the same rows without those. The v5
// binaries wrote exactly these rows for this source. It stamps the old
// profile id, which is all planParserProfiles compares.
type pythonBindingsV5Adapter struct {
	parser.Adapter
	id string
}

func (a pythonBindingsV5Adapter) Profile() parser.Profile {
	return parser.Profile{ID: a.id, EmitsCallEdges: true}
}

func (a pythonBindingsV5Adapter) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	pf, err := a.Adapter.Parse(ctx, path, content)
	if err != nil || filepath.Base(path) != "mod.py" {
		return pf, err
	}
	kept := pf.Scope.Imports[:0]
	for _, b := range pf.Scope.Imports {
		if b.Kind == graph.ScopeImportClassBodyBinding || b.Kind == graph.ScopeImportClassHeaderBinding ||
			b.Kind == graph.ScopeImportLocalBinding && b.LocalName == "full" {
			continue
		}
		kept = append(kept, b)
	}
	pf.Scope.Imports = kept
	return pf, nil
}

// Recording a def header's default lambdas and a nested class body changes
// what an unchanged file produces, so the profile bump alone must reparse it,
// and every transition must equal a from-scratch index by the parser that
// last ran.
func runPythonDefaultClassProfileConvergence(t *testing.T, old pythonBindingsV5Adapter, current parser.Adapter) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	writeProfileFile(t, filepath.Join(root, "lib.py"), pythonNFKCLib)
	path := filepath.Join(root, "mod.py")
	writeProfileFile(t, path, pythonDefaultClassSource)
	currentID := current.(parser.ProfileProvider).Profile().ID

	s := newProfileStore(t)
	if _, err := New(s.Store, parser.NewRegistry(old), nil).Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("old index: %v", err)
	}
	oldGraph := pythonGraph(t, s)
	// The wrong edge: CPython calls the class attribute. (The tree-sitter
	// adapter also bound the default lambda's call on line 4; the regex
	// adapter reads no calls from a def header.)
	requireRows(t, oldGraph, "old graph", "prov|mod.py="+old.id+":1", "call|full@11->lib.full")

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
		"scope|mod.py|run|local_binding|||full",
		"scope|mod.py|make|class_body_binding|||full",
		"call|full@11->")
	if strings.Contains(got, "->lib.full") {
		t.Fatalf("a call CPython does not make to lib.full is still bound:\n%s", got)
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

	// An older binary on the same graph reparses back to its own output.
	if _, err := New(s.Store, parser.NewRegistry(old), nil).Update(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("old update over a new graph: %v", err)
	}
	if got := pythonGraph(t, s); got != oldGraph {
		t.Fatalf("old update over a new graph:\n%s\nfrom-scratch old graph:\n%s", got, oldGraph)
	}

	// Back on the current parser, then edit the file: the edit converges like
	// a fresh index. CPython: other() returns 0, len([]).
	if _, err := upgraded.Update(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	writeProfileFile(t, path, pythonDefaultClassSource+
		"\n\ndef other():\n    class D:\n        for full in [len]:\n            pass\n        n = full([])\n    return D.n\n")
	if _, err := upgraded.Update(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("update after edit: %v", err)
	}
	got = pythonGraph(t, s)
	requireRows(t, got, "edited graph", "scope|mod.py|other|class_body_binding|||full", "call|full@19->")
	if want := freshPythonGraph(t, root, current); got != want {
		t.Fatalf("edited graph:\n%s\nfrom-scratch graph:\n%s", got, want)
	}
}

func TestPythonRegexProfileDefaultClassBindingsConverge(t *testing.T) {
	runPythonDefaultClassProfileConvergence(t,
		pythonBindingsV5Adapter{Adapter: pyparser.New(), id: "python-regex:python:v5"}, pyparser.New())
}

// pythonNFKCHeaderSource is a program CPython runs: run() returns 'cls', the
// class attribute the fullwidth default spells, not lib.full.
const pythonNFKCHeaderSource = `from lib import full


def run():
    class C:
        full = lambda: "cls"

        def m(self, x=ｆｕｌｌ()):
            return x
    return C().m()
`

// pythonHeaderBindingsV6Adapter reproduces, for pythonNFKCHeaderSource, what
// both Python adapters wrote before a method header's names were matched by
// their NFKC form: the same rows without the class_header_binding the
// fullwidth spelling now records. It stamps the old profile id, which is all
// planParserProfiles compares.
type pythonHeaderBindingsV6Adapter struct {
	parser.Adapter
	id string
}

func (a pythonHeaderBindingsV6Adapter) Profile() parser.Profile {
	return parser.Profile{ID: a.id, EmitsCallEdges: true}
}

func (a pythonHeaderBindingsV6Adapter) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	pf, err := a.Adapter.Parse(ctx, path, content)
	if err != nil || filepath.Base(path) != "mod.py" {
		return pf, err
	}
	kept := pf.Scope.Imports[:0]
	for _, b := range pf.Scope.Imports {
		if b.Kind != graph.ScopeImportClassHeaderBinding {
			kept = append(kept, b)
		}
	}
	pf.Scope.Imports = kept
	return pf, nil
}

// Matching a method header's names by their NFKC form records a
// class_header_binding the previous profile did not, so the bump alone must
// reparse the unchanged file and converge with a from-scratch index.
// headerCalls says whether the adapter reads calls written in a def header.
func runPythonNFKCHeaderProfileConvergence(t *testing.T, old pythonHeaderBindingsV6Adapter, current parser.Adapter, headerCalls bool) {
	t.Helper()
	ctx := context.Background()
	root := t.TempDir()
	writeProfileFile(t, filepath.Join(root, "lib.py"), pythonNFKCLib)
	writeProfileFile(t, filepath.Join(root, "mod.py"), pythonNFKCHeaderSource)
	currentID := current.(parser.ProfileProvider).Profile().ID

	s := newProfileStore(t)
	if _, err := New(s.Store, parser.NewRegistry(old), nil).Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatalf("old index: %v", err)
	}
	if headerCalls {
		requireRows(t, pythonGraph(t, s), "old graph", "prov|mod.py="+old.id+":1", "call|full@8->lib.full")
	}

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
	requireRows(t, got, "upgraded graph", "prov|mod.py="+currentID+":1", "scope|mod.py|run.C.m|class_header_binding||8|full")
	if headerCalls {
		requireRows(t, got, "upgraded graph", "call|full@8->")
	}
	if strings.Contains(got, "->lib.full") {
		t.Fatalf("a call CPython does not make to lib.full is still bound:\n%s", got)
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
}

func TestPythonRegexProfileNFKCHeaderBindingsConverge(t *testing.T) {
	runPythonNFKCHeaderProfileConvergence(t,
		pythonHeaderBindingsV6Adapter{Adapter: pyparser.New(), id: "python-regex:python:v6"}, pyparser.New(), false)
}
