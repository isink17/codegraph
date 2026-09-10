//go:build cgo

package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
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

// swiftV2FixtureAdapter replays measured BASE v2 facts for the controlled
// source. It removes only v3 call-shape and member-value facts from current
// parser output, keeping fixture tied to real Swift syntax.
type swiftV2FixtureAdapter struct{}

// swiftV3FixtureAdapter replays v3 call facts from the v4 parser, removing
// only v4's new ordinary-constructor classification and lexical evidence.
type swiftV3FixtureAdapter struct{}

func (swiftV3FixtureAdapter) Language() string     { return "swift" }
func (swiftV3FixtureAdapter) Extensions() []string { return []string{".swift"} }
func (swiftV3FixtureAdapter) Supports(path string) bool {
	return filepath.Ext(path) == ".swift"
}
func (swiftV3FixtureAdapter) Profile() parser.Profile {
	return parser.Profile{ID: "treesitter:swift:v3", EmitsCallEdges: true}
}
func (swiftV3FixtureAdapter) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	p, err := tsparser.NewSwift().Parse(ctx, path, content)
	if err != nil {
		return graph.ParsedFile{}, err
	}
	for i := range p.Edges {
		if !strings.HasPrefix(p.Edges[i].Evidence, "swift:initializer") {
			continue
		}
		parts := strings.Split(p.Edges[i].Evidence, ";")
		generic := false
		shape := make([]string, 0, len(parts))
		for _, part := range parts[1:] {
			if part == "generic_specialization=true" {
				generic = true
				continue
			}
			shape = append(shape, part)
		}
		base := "swift:bare"
		if generic {
			base = "swift:initializer"
		}
		if len(shape) > 0 {
			base += ";" + strings.Join(shape, ";")
		}
		p.Edges[i].Evidence = base
	}
	p.Scope.SwiftLexicalBindings = nil
	return p, nil
}

func (swiftV2FixtureAdapter) Language() string     { return "swift" }
func (swiftV2FixtureAdapter) Extensions() []string { return []string{".swift"} }
func (swiftV2FixtureAdapter) Supports(path string) bool {
	return filepath.Ext(path) == ".swift"
}
func (swiftV2FixtureAdapter) Profile() parser.Profile {
	return parser.Profile{ID: "treesitter:swift:v2", EmitsCallEdges: true}
}
func (swiftV2FixtureAdapter) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	p, err := tsparser.NewSwift().Parse(ctx, path, content)
	if err != nil {
		return graph.ParsedFile{}, err
	}
	for i := range p.Edges {
		if marker := strings.Index(p.Edges[i].Evidence, ";trailing_labels="); marker >= 0 {
			labels := p.Edges[i].Evidence[marker+len(";trailing_labels="):]
			p.Edges[i].Evidence = p.Edges[i].Evidence[:marker]
			if p.Edges[i].CallArity != nil {
				*p.Edges[i].CallArity -= 1 + strings.Count(labels, ",")
			}
		}
	}
	p.Scope.Imports = slices.DeleteFunc(p.Scope.Imports, func(f graph.ScopeImport) bool {
		return f.Kind == graph.ScopeImportSwiftMemberValue || f.Kind == graph.ScopeImportSwiftEnumCase
	})
	return p, nil
}

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

func TestSwiftV1ToV3UnchangedSourceConverges(t *testing.T) {
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
	assertSwiftProfileFacts(t, legacy, repo, "treesitter:swift:v5")

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

func TestSwiftV2ToV3UnchangedSourceConverges(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	source := `struct Service {
    let handler: () -> Void, first: () -> Void = {}, second: () -> Void = {}
    func run() {}
    func run(_ body: () -> Void) {}
    func run(id: Int) {}
    func caller() {
        self.run()
        self.run() { work() }
        self.run(id: 1)
        self.run(id: 1) { work() }
    }
    func work() {}
}

enum Events { case build(Int), second(String) }
	`
	path := filepath.Join(root, "Service.swift")
	writeProfileFile(t, path, source)
	writeProfileFile(t, filepath.Join(root, "Extension.swift"), "extension External { var handler: () -> Void { {} } }\n")
	legacy := newProfileStore(t)
	if _, err := New(legacy.Store, parser.NewRegistry(swiftV2FixtureAdapter{}), nil).Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	repo := repoID(t, legacy, root)
	var collision int
	if err := legacy.raw(t).QueryRowContext(ctx, `SELECT COUNT(*) FROM edges WHERE repo_id=? AND dst_name='self.run' AND evidence='swift:self' AND call_arity=0`, repo).Scan(&collision); err != nil {
		t.Fatal(err)
	}
	if collision != 2 {
		t.Fatalf("v2 trailing collision=%d, want 2", collision)
	}
	var memberFacts int
	if err := legacy.raw(t).QueryRowContext(ctx, `SELECT COUNT(*) FROM scope_import_evidence WHERE repo_id=? AND import_kind=?`, repo, graph.ScopeImportSwiftMemberValue).Scan(&memberFacts); err != nil {
		t.Fatal(err)
	}
	if memberFacts != 0 {
		t.Fatalf("v2 member facts=%d, want 0", memberFacts)
	}
	var enumFacts int
	if err := legacy.raw(t).QueryRowContext(ctx, `SELECT COUNT(*) FROM scope_import_evidence WHERE repo_id=? AND import_kind=?`, repo, graph.ScopeImportSwiftEnumCase).Scan(&enumFacts); err != nil {
		t.Fatal(err)
	}
	if enumFacts != 0 {
		t.Fatalf("v2 enum facts=%d, want 0", enumFacts)
	}
	current := New(legacy.Store, parser.NewRegistry(tsparser.NewSwift()), nil)
	upgraded, err := current.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if upgraded.FilesChanged != 2 || upgraded.FilesIndexed != 2 || len(upgraded.ParserProfileLanguages) != 1 || upgraded.ParserProfileLanguages[0] != "swift" {
		t.Fatalf("upgrade summary=%+v", upgraded)
	}
	assertSwiftV3Facts(t, legacy, repo)
	fresh := newProfileStore(t)
	if _, err := New(fresh.Store, parser.NewRegistry(tsparser.NewSwift()), nil).Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	if got, want := swiftFactDigest(t, legacy, repo), swiftFactDigest(t, fresh, repoID(t, fresh, root)); got != want {
		t.Fatalf("upgraded facts=%q fresh facts=%q", got, want)
	}
	again, err := current.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if again.FilesChanged != 0 || again.FilesIndexed != 0 || len(again.ParserProfileLanguages) != 0 {
		t.Fatalf("second update=%+v", again)
	}
}

func TestSwiftV3ToV4UnchangedSourceConverges(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeProfileFile(t, filepath.Join(root, "Facts.swift"), `
struct Service { init() {} }
struct Box<T> {}
typealias BoxAlias<T> = Box<T>
func make() { Service(); Box<Int>(); BoxAlias<Int>() }
func shadow(_ Service: () -> Int) { Service() }
`)
	legacy := newProfileStore(t)
	if _, err := New(legacy.Store, parser.NewRegistry(swiftV3FixtureAdapter{}), nil).Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	repo := repoID(t, legacy, root)
	var oldLexical int
	if err := legacy.raw(t).QueryRow(`SELECT COUNT(*) FROM swift_lexical_binding_evidence WHERE repo_id=?`, repo).Scan(&oldLexical); err != nil {
		t.Fatal(err)
	}
	if oldLexical != 0 {
		t.Fatalf("v3 lexical facts=%d, want 0", oldLexical)
	}
	current := New(legacy.Store, parser.NewRegistry(tsparser.NewSwift()), nil)
	upgraded, err := current.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if upgraded.FilesChanged != 1 || upgraded.FilesIndexed != 1 {
		t.Fatalf("upgrade summary=%+v", upgraded)
	}
	fresh := newProfileStore(t)
	if _, err := New(fresh.Store, parser.NewRegistry(tsparser.NewSwift()), nil).Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	if got, want := swiftFactDigest(t, legacy, repo), swiftFactDigest(t, fresh, repoID(t, fresh, root)); got != want {
		t.Fatalf("upgraded facts=%q fresh facts=%q", got, want)
	}
	var newLexical int
	if err := legacy.raw(t).QueryRow(`SELECT COUNT(*) FROM swift_lexical_binding_evidence WHERE repo_id=?`, repo).Scan(&newLexical); err != nil {
		t.Fatal(err)
	}
	if newLexical == 0 {
		t.Fatal("v4 lexical facts missing after upgrade")
	}
	again, err := current.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if again.FilesChanged != 0 || again.FilesIndexed != 0 {
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

func TestSwiftMemberValueLifecycle(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "Service.swift")
	writeProfileFile(t, path, "struct Service { var a: () -> Void, b: () -> Void }\n")
	s := newProfileStore(t)
	idx := New(s.Store, parser.NewRegistry(tsparser.NewSwift()), nil)
	if _, err := idx.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	repo := repoID(t, s, root)
	countFact := func(name string) int {
		var count int
		if err := s.raw(t).QueryRow(`SELECT COUNT(*) FROM scope_import_evidence WHERE repo_id=? AND import_kind=? AND local_name=?`, repo, graph.ScopeImportSwiftMemberValue, name).Scan(&count); err != nil {
			t.Fatal(err)
		}
		return count
	}
	if countFact("a") != 1 || countFact("b") != 1 {
		t.Fatalf("initial facts a=%d b=%d", countFact("a"), countFact("b"))
	}
	writeProfileFile(t, path, "struct Service { var a: () -> Void, c: () -> Void }\n")
	if _, err := idx.Update(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	if countFact("a") != 1 || countFact("b") != 0 || countFact("c") != 1 {
		t.Fatalf("multi-binding facts a=%d b=%d c=%d", countFact("a"), countFact("b"), countFact("c"))
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Update(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	if countFact("a") != 0 || countFact("c") != 0 {
		t.Fatalf("deleted fact remains")
	}
	writeProfileFile(t, path, "struct Service { var restored: () -> Void }\n")
	if _, err := idx.Update(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	if countFact("restored") != 1 {
		t.Fatalf("restored fact missing")
	}
}

func TestSwiftInheritanceFactsPersistWithoutNestedOrMemberContamination(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "Facts.swift")
	writeProfileFile(t, path, `protocol P {}
class Base {}
class Child: Base {
    func helper<T>(_ value: T) {}
    struct Box<T> {}
}
class Outer {
    struct Inner: P {}
}`)
	s := newProfileStore(t)
	idx := New(s.Store, parser.NewRegistry(tsparser.NewSwift()), nil)
	if _, err := idx.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	repo := repoID(t, s, root)
	var child, outer int
	if err := s.raw(t).QueryRow(`SELECT COUNT(*) FROM swift_inheritance_relations WHERE repo_id=? AND child_qualified_name='Child' AND target_qualified_name='Base' AND relation_kind='superclass' AND is_generic=0 AND is_constrained=0`, repo).Scan(&child); err != nil { t.Fatal(err) }
	if err := s.raw(t).QueryRow(`SELECT COUNT(*) FROM swift_inheritance_relations WHERE repo_id=? AND child_qualified_name='Outer'`, repo).Scan(&outer); err != nil { t.Fatal(err) }
	if child != 1 || outer != 0 { t.Fatalf("persisted facts child=%d outer=%d", child, outer) }
	writeProfileFile(t, path, `class Base {}
class Child: Base {}`)
	if _, err := idx.Update(ctx, Options{RepoRoot: root}); err != nil { t.Fatal(err) }
	if err := s.raw(t).QueryRow(`SELECT COUNT(*) FROM swift_inheritance_relations WHERE repo_id=?`, repo).Scan(&child); err != nil { t.Fatal(err) }
	if child != 1 { t.Fatalf("stale inheritance facts=%d", child) }
}

func TestSwiftLexicalBindingLifecycle(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(root, "Facts.swift")
	writeProfileFile(t, path, "struct Service {}\nfunc f(_ Service: () -> Int) { Service() }\n")
	s := newProfileStore(t)
	idx := New(s.Store, parser.NewRegistry(tsparser.NewSwift()), nil)
	if _, err := idx.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	repo := repoID(t, s, root)
	count := func(name, kind string) int {
		var n int
		if err := s.raw(t).QueryRow(`SELECT COUNT(*) FROM swift_lexical_binding_evidence WHERE repo_id=? AND name=? AND binding_kind=?`, repo, name, kind).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	if count("Service", graph.SwiftLexicalParameter) != 1 {
		t.Fatalf("initial parameter facts=%d", count("Service", graph.SwiftLexicalParameter))
	}
	writeProfileFile(t, path, "struct Service {}\nfunc f(_ Other: () -> Int) { Other() }\n")
	if _, err := idx.Update(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	if count("Service", graph.SwiftLexicalParameter) != 0 || count("Other", graph.SwiftLexicalParameter) != 1 {
		t.Fatalf("renamed facts Service=%d Other=%d", count("Service", graph.SwiftLexicalParameter), count("Other", graph.SwiftLexicalParameter))
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Update(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	var remaining int
	if err := s.raw(t).QueryRow(`SELECT COUNT(*) FROM swift_lexical_binding_evidence WHERE repo_id=?`, repo).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("deleted lexical facts=%d", remaining)
	}
	writeProfileFile(t, path, "struct Service {}\nfunc f(_ Restored: () -> Int) { Restored() }\n")
	if _, err := idx.Update(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	if count("Restored", graph.SwiftLexicalParameter) != 1 {
		t.Fatalf("restored facts=%d", count("Restored", graph.SwiftLexicalParameter))
	}
}

func TestSwiftV2ProfileSafety(t *testing.T) {
	all := func(string) bool { return true }
	v3 := parser.Profile{ID: "treesitter:swift:v5", EmitsCallEdges: true}
	fallback := parser.Profile{ID: "heuristic:swift:v1", EmitsCallEdges: false}
	if _, err := planParserProfiles([]store.FileParserProfileGroup{{Language: "swift", Profile: v3.ID, CallEdges: true, Files: 1}}, map[string]parser.Profile{"swift": fallback}, all, false); !errors.Is(err, ErrParserDowngradeRefused) {
		t.Fatalf("downgrade error=%v, want refusal", err)
	}
	if _, err := planParserProfiles([]store.FileParserProfileGroup{{Language: "swift", Profile: "treesitter:swift:v2", CallEdges: true, Files: 1}}, map[string]parser.Profile{"swift": v3}, all, true); !errors.Is(err, ErrParserProfileTransitionRequired) {
		t.Fatalf("path transition error=%v, want refusal", err)
	}
}

func assertSwiftProfileFacts(t *testing.T, s *profileStore, repo int64, wantProfile string) {
	t.Helper()
	var profile string
	if err := s.raw(t).QueryRow(`SELECT parser_profile FROM files WHERE repo_id=? AND is_deleted=0 LIMIT 1`, repo).Scan(&profile); err != nil {
		t.Fatal(err)
	}
	if profile != wantProfile {
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
	if service != 1 || legacy != 0 || rawCall != 0 {
		t.Fatalf("profile facts service=%d legacy=%d raw_call=%d", service, legacy, rawCall)
	}
}

func assertSwiftV3Facts(t *testing.T, s *profileStore, repo int64) {
	t.Helper()
	db := s.raw(t)
	var profile string
	if err := db.QueryRow(`SELECT parser_profile FROM files WHERE repo_id=? AND is_deleted=0 LIMIT 1`, repo).Scan(&profile); err != nil {
		t.Fatal(err)
	}
	if profile != "treesitter:swift:v5" {
		t.Fatalf("profile=%q", profile)
	}
	var trailing, members, enumCases int
	if err := db.QueryRow(`SELECT COUNT(*) FROM edges WHERE repo_id=? AND evidence LIKE '%trailing_labels=%'`, repo).Scan(&trailing); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM scope_import_evidence WHERE repo_id=? AND import_kind=?`, repo, graph.ScopeImportSwiftMemberValue).Scan(&members); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM scope_import_evidence WHERE repo_id=? AND import_kind=?`, repo, graph.ScopeImportSwiftEnumCase).Scan(&enumCases); err != nil {
		t.Fatal(err)
	}
	if trailing != 2 || members != 4 || enumCases != 2 {
		t.Fatalf("v3 trailing=%d member_facts=%d enum_cases=%d", trailing, members, enumCases)
	}
	for _, want := range []struct{ kind, owner, name string }{
		{graph.ScopeImportSwiftMemberValue, "Service", "handler"},
		{graph.ScopeImportSwiftMemberValue, "Service", "first"},
		{graph.ScopeImportSwiftMemberValue, "Service", "second"},
		{graph.ScopeImportSwiftMemberValue, "External", "handler"},
		{graph.ScopeImportSwiftEnumCase, "Events", "build"},
		{graph.ScopeImportSwiftEnumCase, "Events", "second"},
	} {
		var count int
		if err := db.QueryRow(`SELECT COUNT(*) FROM scope_import_evidence WHERE repo_id=? AND import_kind=? AND owner_module=? AND local_name=?`, repo, want.kind, want.owner, want.name).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Fatalf("scope fact %+v count=%d", want, count)
		}
	}
}

func swiftFactDigest(t *testing.T, s *profileStore, repo int64) string {
	t.Helper()
	db := s.raw(t)
	rows, err := db.Query(`SELECT 's|'||kind||'|'||name||'|'||qualified_name||'|'||stable_key FROM symbols WHERE repo_id=? UNION ALL SELECT 'e|'||dst_name||'|'||evidence||'|'||COALESCE(CAST(call_arity AS TEXT),'') FROM edges WHERE repo_id=? UNION ALL SELECT 'r|'||name||'|'||qualified_name FROM references_tbl WHERE repo_id=? UNION ALL SELECT 'q|'||import_kind||'|'||owner_module||'|'||local_name||'|'||is_static FROM scope_import_evidence WHERE repo_id=? UNION ALL SELECT 'l|'||name||'|'||binding_kind||'|'||owner_module||'|'||scope_start_line||'|'||scope_end_line FROM swift_lexical_binding_evidence WHERE repo_id=? UNION ALL SELECT 'i|'||child_qualified_name||'|'||target_qualified_name||'|'||relation_kind||'|'||is_generic||'|'||is_constrained FROM swift_inheritance_relations WHERE repo_id=? UNION ALL SELECT 'd|'||s.stable_key||'|'||is_final||'|'||is_override||'|'||dispatch_kind FROM swift_declaration_facts d JOIN symbols s ON s.id=d.symbol_id WHERE d.repo_id=? ORDER BY 1`, repo, repo, repo, repo, repo, repo, repo, repo)
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
