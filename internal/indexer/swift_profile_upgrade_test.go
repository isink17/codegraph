//go:build cgo

package indexer

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/parser"
	tsparser "github.com/isink17/codegraph/internal/parser/treesitter"
	"github.com/isink17/codegraph/internal/store"
)

// swiftV1FixtureAdapter is deliberately old: it records the filename as a
// module and emits the pre-v2 raw call fact. It exists only to prove profile
// replacement, not to provide a second production parser.
type swiftV1FixtureAdapter struct{}

func (swiftV1FixtureAdapter) Language() string     { return "swift" }
func (swiftV1FixtureAdapter) Extensions() []string { return []string{".swift"} }
func (swiftV1FixtureAdapter) Supports(path string) bool {
	return filepath.Ext(path) == ".swift"
}
func (swiftV1FixtureAdapter) Profile() parser.Profile {
	return parser.Profile{ID: "treesitter:swift:v1", EmitsCallEdges: true}
}
func (swiftV1FixtureAdapter) Parse(_ context.Context, path string, content []byte) (graph.ParsedFile, error) {
	module := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	p := graph.ParsedFile{Language: "swift"}
	addType := func(name, container string) {
		qname := module + "." + name
		p.Symbols = append(p.Symbols, graph.Symbol{Language: "swift", Kind: "class", Name: name, QualifiedName: qname, ContainerName: container, Visibility: "public", StableKey: "type:swift:" + module + ":" + name, Range: graph.Position{StartLine: 1, EndLine: 1}})
	}
	addFunc := func(name, container string, start, end int) {
		qname := module + "." + name
		if container != "" && container != module {
			qname = module + "." + container + "." + name
		}
		p.Symbols = append(p.Symbols, graph.Symbol{Language: "swift", Kind: "function", Name: name, QualifiedName: qname, ContainerName: container, Visibility: "module", StableKey: "func:swift:" + module + ":" + name, Range: graph.Position{StartLine: start, EndLine: end}})
	}
	if strings.Contains(string(content), "struct Outer") {
		addType("Outer", module)
		addType("Inner", module)
		addFunc("run", "Inner", 3, 3)
	}
	if strings.Contains(string(content), "struct Service") {
		addType("Service", module)
	}
	if strings.Contains(string(content), "extension Service") {
		addType("Service", module)
		addFunc("extensionRun", "Service", 6, 6)
	}
	if strings.Contains(string(content), "func helper") {
		addFunc("helper", module, 7, 7)
		p.Edges = append(p.Edges, graph.Edge{DstName: "helper()", Kind: "calls", Evidence: "helper()", Line: 3})
		p.References = append(p.References, graph.Reference{Kind: "call", Name: "helper()", QualifiedName: "helper()", Range: graph.Position{StartLine: 3, EndLine: 3}})
	}
	if strings.Contains(string(content), "func overloaded") {
		addFunc("overloaded", module, 8, 8)
		addFunc("overloaded", module, 9, 9)
	}
	if strings.Contains(string(content), "struct Visible") {
		addType("Visible", module)
	}
	return p, nil
}

func TestSwiftV1ToV2UnchangedSourceConverges(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "Foo.swift")
	writeProfileFile(t, path, `struct Outer {
    struct Inner {
        static func run() { helper() }
    }
}
struct Service { init() {} }
extension Service { func extensionRun() {} }
func helper() {}
func overloaded() {}
func overloaded(_ value: Int) {}
public struct Visible {}
`)

	legacy := newProfileStore(t)
	if _, err := New(legacy.Store, parser.NewRegistry(swiftV1FixtureAdapter{}), nil).Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	repo := repoID(t, legacy, root)
	var old int
	if err := legacy.raw(t).QueryRowContext(ctx, `SELECT COUNT(*) FROM symbols WHERE repo_id=? AND stable_key LIKE 'type:swift:Foo:%'`, repo).Scan(&old); err != nil {
		t.Fatal(err)
	}
	if old != 5 {
		t.Fatalf("genuine v1 type rows=%d, want 5", old)
	}
	var rawCall int
	if err := legacy.raw(t).QueryRowContext(ctx, `SELECT COUNT(*) FROM edges WHERE repo_id=? AND dst_name='helper()' AND evidence='helper()'`, repo).Scan(&rawCall); err != nil {
		t.Fatal(err)
	}
	if rawCall != 1 {
		t.Fatalf("genuine v1 raw calls=%d, want 1", rawCall)
	}

	v2 := New(legacy.Store, parser.NewRegistry(tsparser.NewSwift()), nil)
	summary, err := v2.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if summary.FilesChanged != 1 || summary.FilesIndexed != 1 || len(summary.ParserProfileLanguages) != 1 || summary.ParserProfileLanguages[0] != "swift" {
		t.Fatalf("upgrade summary=%+v", summary)
	}
	assertSwiftV2Facts(t, legacy, repo, true)

	fresh := newProfileStore(t)
	if _, err := New(fresh.Store, parser.NewRegistry(tsparser.NewSwift()), nil).Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	if got, want := swiftFactDigest(t, legacy, repo), swiftFactDigest(t, fresh, repoID(t, fresh, root)); got != want {
		t.Fatalf("upgraded facts=%q fresh facts=%q", got, want)
	}
	again, err := v2.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if again.FilesChanged != 0 || again.FilesIndexed != 0 || len(again.ParserProfileLanguages) != 0 {
		t.Fatalf("second update=%+v", again)
	}
}

func TestSwiftSourceAttributionAndLocalFunctionSafety(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeProfileFile(t, filepath.Join(root, "Calls.swift"), `func helper() {}
func outer() {
    func inner() { helper() }
    let f = { helper() }
    outerHelper()
}

func outerHelper() {}
struct A {
    func first() { helper() }
    func second() { outerHelper() }
}

`)
	s := newProfileStore(t)
	if _, err := New(s.Store, parser.NewRegistry(tsparser.NewSwift()), nil).Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	repo := repoID(t, s, root)
	rows, err := s.raw(t).Query(`SELECT s.qualified_name, e.dst_name FROM edges e JOIN symbols s ON s.id=e.src_symbol_id WHERE e.repo_id=? ORDER BY e.line, e.dst_name`, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var src, dst string
		if err := rows.Scan(&src, &dst); err != nil {
			t.Fatal(err)
		}
		got = append(got, src+"->"+dst)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{"outer->helper", "outer->outerHelper", "A.first->helper", "A.second->outerHelper"}
	if len(got) != len(want) {
		t.Fatalf("attributed edges=%v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("attributed edges=%v, want %v", got, want)
		}
	}
}

func TestSwiftV2ProfileSafety(t *testing.T) {
	all := func(string) bool { return true }
	v2 := parser.Profile{ID: "treesitter:swift:v2", EmitsCallEdges: true}
	fallback := parser.Profile{ID: "heuristic:swift:v1", EmitsCallEdges: false}
	if _, err := planParserProfiles([]store.FileParserProfileGroup{{Language: "swift", Profile: v2.ID, CallEdges: true, Files: 1}}, map[string]parser.Profile{"swift": fallback}, all, false); !errors.Is(err, ErrParserDowngradeRefused) {
		t.Fatalf("downgrade error=%v, want refusal", err)
	}
	if _, err := planParserProfiles([]store.FileParserProfileGroup{{Language: "swift", Profile: "treesitter:swift:v1", CallEdges: true, Files: 1}}, map[string]parser.Profile{"swift": v2}, all, true); !errors.Is(err, ErrParserProfileTransitionRequired) {
		t.Fatalf("path transition error=%v, want refusal", err)
	}
}

func assertSwiftV2Facts(t *testing.T, s *profileStore, repo int64, wantV2 bool) {
	t.Helper()
	var profile string
	if err := s.raw(t).QueryRow(`SELECT parser_profile FROM files WHERE repo_id=? AND is_deleted=0 LIMIT 1`, repo).Scan(&profile); err != nil {
		t.Fatal(err)
	}
	if profile != "treesitter:swift:v2" {
		t.Fatalf("profile=%q", profile)
	}
	var service, legacy, rawCall int
	db := s.raw(t)
	if err := db.QueryRow(`SELECT COUNT(*) FROM symbols WHERE repo_id=? AND qualified_name='Service'`, repo).Scan(&service); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM symbols WHERE repo_id=? AND qualified_name LIKE 'Legacy.%'`, repo).Scan(&legacy); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM edges WHERE repo_id=? AND dst_name='helper()'`, repo).Scan(&rawCall); err != nil {
		t.Fatal(err)
	}
	if wantV2 && (service != 1 || legacy != 0 || rawCall != 0) {
		t.Fatalf("v2 facts service=%d legacy=%d raw_call=%d", service, legacy, rawCall)
	}
}

func swiftFactDigest(t *testing.T, s *profileStore, repo int64) string {
	t.Helper()
	db := s.raw(t)
	rows, err := db.Query(`SELECT 's|'||kind||'|'||name||'|'||qualified_name||'|'||stable_key FROM symbols WHERE repo_id=? UNION ALL SELECT 'e|'||dst_name||'|'||evidence FROM edges WHERE repo_id=? UNION ALL SELECT 'r|'||name||'|'||qualified_name FROM references_tbl WHERE repo_id=? ORDER BY 1`, repo, repo, repo)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out string
	for rows.Next() {
		var row string
		if err := rows.Scan(&row); err != nil {
			t.Fatal(err)
		}
		out += row + "\n"
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}
