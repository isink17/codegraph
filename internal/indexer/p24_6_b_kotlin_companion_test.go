//go:build cgo

package indexer

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	tsparser "github.com/isink17/codegraph/internal/parser/treesitter"
	"github.com/isink17/codegraph/internal/store"
)

func TestP246BCompanionsStayOutOfB5ObjectABI(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"Service.kt": `package lib
class Service {
    companion object {
        fun run() {}
        @JvmStatic fun staticRun() {}
    }
}`,
		"NamedService.kt": `package lib
class NamedService {
    companion object Factory {
        fun run() {}
        @JvmStatic fun staticRun() {}
    }
}`,
		"ExplicitCompanionService.kt": `package lib
class ExplicitCompanionService {
    companion object Companion {
        fun run() {}
        @JvmStatic fun staticRun() {}
    }
}`,
		"ObjectService.kt": `package lib
object ObjectService { fun run() {}; @JvmStatic fun staticRun() {} }`,
		"KotlinCaller.kt": `package app
import lib.Service
import lib.NamedService
import lib.ExplicitCompanionService
fun implicitCompanion() { Service.run() }
fun defaultCompanionName() { Service.Companion.run() }
fun namedCompanionName() { NamedService.Factory.run() }
fun explicitCompanionName() { ExplicitCompanionService.Companion.run() }`,
		"Caller.java": `package app;
import lib.Service;
import lib.NamedService;
import lib.ExplicitCompanionService;
import lib.ObjectService;
class Caller {
    void companionField() { Service.Companion.INSTANCE.run(); }
    void companionInstance() { Service.INSTANCE.run(); }
    void companionDirect() { Service.run(); }
    void companionStatic() { Service.staticRun(); }
    void companionInstanceValid() { Service.Companion.run(); }
    void namedField() { NamedService.Factory.INSTANCE.run(); }
    void namedDirect() { NamedService.Factory.staticRun(); }
    void namedInstanceValid() { NamedService.Factory.run(); }
    void namedOuterInstance() { NamedService.INSTANCE.run(); }
    void namedOuterDirect() { NamedService.run(); }
    void explicitCompanionField() { ExplicitCompanionService.Companion.INSTANCE.run(); }
    void explicitCompanionOuterInstance() { ExplicitCompanionService.INSTANCE.run(); }
    void explicitCompanionDirect() { ExplicitCompanionService.run(); }
    void explicitCompanionNamedStatic() { ExplicitCompanionService.Companion.staticRun(); }
    void explicitCompanionInstanceValid() { ExplicitCompanionService.Companion.run(); }
    void objectInstance() { ObjectService.INSTANCE.run(); }
    void objectDirect() { ObjectService.run(); }
    void objectStatic() { ObjectService.staticRun(); }
    void objectStaticInstance() { ObjectService.INSTANCE.staticRun(); }
}`,
	})

	for _, name := range []string{
		"Service.Companion.INSTANCE.run", "Service.INSTANCE.run", "Service.run",
		"NamedService.Factory.INSTANCE.run", "NamedService.INSTANCE.run", "NamedService.run",
		"ExplicitCompanionService.Companion.INSTANCE.run", "ExplicitCompanionService.INSTANCE.run", "ExplicitCompanionService.run",
		"ObjectService.run", "ObjectService.INSTANCE.staticRun",
	} {
		assertJVMUnresolved(t, r, "Caller.java", name)
	}
	assertJVMResolved(t, r, "Caller.java", "ObjectService.INSTANCE.run", "ObjectService.kt", "java_import_scope")
	assertJVMResolved(t, r, "Caller.java", "ObjectService.staticRun", "ObjectService.kt", "java_import_scope")
	for _, call := range []struct{ name, file string }{
		{"Service.staticRun", "Service.kt"}, {"Service.Companion.run", "Service.kt"},
		{"NamedService.Factory.staticRun", "NamedService.kt"}, {"NamedService.Factory.run", "NamedService.kt"},
		{"ExplicitCompanionService.Companion.staticRun", "ExplicitCompanionService.kt"}, {"ExplicitCompanionService.Companion.run", "ExplicitCompanionService.kt"},
	} {
		assertJVMResolved(t, r, "Caller.java", call.name, call.file, "java_import_scope")
	}
	for _, name := range []string{"Service.run", "Service.Companion.run", "NamedService.Factory.run", "ExplicitCompanionService.Companion.run"} {
		assertJVMUnresolved(t, r, "KotlinCaller.kt", name)
	}
	for _, qname := range []struct{ name, kind string }{
		{"lib.Service.Companion", "companion_object"},
		{"lib.NamedService.Factory", "companion_object"},
		{"lib.ExplicitCompanionService.Companion", "companion_object"},
		{"lib.Service.Companion.run", "function"},
		{"lib.NamedService.Factory.run", "function"},
		{"lib.ExplicitCompanionService.Companion.run", "function"},
	} {
		matches, err := r.store.FindSymbolExact(r.ctx, r.repoID, qname.name, 10, 0)
		if err != nil || len(matches) != 1 || matches[0].Kind != qname.kind {
			t.Fatalf("FindSymbolExact(%q) = %#v, %v; want one %s", qname.name, matches, err, qname.kind)
		}
	}
	searched, err := r.store.SearchSymbols(r.ctx, r.repoID, "Factory", 10, 0)
	if err != nil || !hasSymbolQName(searched, "lib.NamedService.Factory.run") {
		t.Fatalf("SearchSymbols(Factory) = %#v, %v", searched, err)
	}
	callees, err := r.store.FindCallees(r.ctx, r.repoID, "app.KotlinCaller.explicitCompanionName", 0, 10, 0)
	if err != nil || hasSymbolQName(callees, "lib.ExplicitCompanionService.Companion.run") {
		t.Fatalf("FindCallees for unresolved companion call = %#v, %v", callees, err)
	}
	callers, err := r.store.FindCallers(r.ctx, r.repoID, "lib.ExplicitCompanionService.Companion.run", 0, 10, 0)
	if err != nil || len(callers) != 1 {
		t.Fatalf("FindCallers for canonical companion member = %d callers, %v; want Java ABI projection", len(callers), err)
	}
	r.assertFreshParity(t, "P24.6-B companion does not inherit B5 object ABI")
}

func TestP246BKotlinV2ProfileUpgradeAddsCompanionSymbols(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	files := map[string]string{
		"Service.kt": `package lib
class Service {
    companion object { fun run() {} }
}`,
		"Other.kt":    "package lib\nfun helper() {}",
		"Caller.java": "package app; import lib.Service; class Caller { void call() { Service.run(); } }",
		"main.go":     "package main\nfunc main() {}\n",
		"caller.ts":   "export function caller(): void {}\n",
	}
	for path, content := range files {
		writeProfileFile(t, filepath.Join(root, path), content)
	}
	s := newProfileStore(t)
	old := New(s.Store, p246Registry(kotlinV2WithoutCompanions{tsparser.NewKotlin()}), nil)
	r := &lifecycleRepo{ctx: ctx, root: root, dbPath: s.path, store: s.Store, idx: old}
	if _, err := old.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	repo := repoID(t, s, root)
	r.repoID = repo
	if got := p246CompanionRows(t, s.raw(t), repo, "Service.kt"); got != 0 {
		t.Fatalf("v2 companion symbols = %d, want 0", got)
	}
	if got := p246FileProfile(t, s.raw(t), repo, "Service.kt"); got != "treesitter:kotlin:v2" {
		t.Fatalf("v2 profile = %q", got)
	}
	profilesBefore := p246Profiles(t, s.raw(t), repo)

	upgraded := New(s.Store, lifecycleRegistry(), nil)
	if _, err := upgraded.Update(ctx, Options{RepoRoot: root, Paths: []string{"Service.kt"}}); !errors.Is(err, ErrParserProfileTransitionRequired) {
		t.Fatalf("path-scoped transition error = %v, want ErrParserProfileTransitionRequired", err)
	}
	if got := p246CompanionRows(t, s.raw(t), repo, "Service.kt"); got != 0 {
		t.Fatalf("path-scoped transition mutated v2 symbols: %d companion rows", got)
	}

	r.idx = upgraded
	summary, err := upgraded.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if summary.FilesChanged != 2 || summary.FilesIndexed != 2 || strings.Join(summary.ParserProfileLanguages, ",") != "kotlin" {
		t.Fatalf("v2 to v3 update = %+v, want only two Kotlin files reparsed", summary)
	}
	if got := p246CompanionRows(t, s.raw(t), repo, "Service.kt"); got != 1 {
		t.Fatalf("v3 companion symbols = %d, want 1", got)
	}
	if got := p246FileProfile(t, s.raw(t), repo, "Service.kt"); got != "treesitter:kotlin:v3" {
		t.Fatalf("upgraded profile = %q", got)
	}
	if got := p246FileProfile(t, s.raw(t), repo, "Other.kt"); got != "treesitter:kotlin:v3" {
		t.Fatalf("other Kotlin profile = %q", got)
	}
	assertJVMUnresolved(t, r, "Caller.java", "Service.run")
	if got := p246Profiles(t, s.raw(t), repo); !sameProfiles(profilesBefore, got, "Service.kt", "Other.kt") {
		t.Fatalf("unrelated Java/Go/TypeScript profiles changed: before=%v after=%v", profilesBefore, got)
	}
	r.assertFreshParity(t, "Kotlin v2 to v3 companion profile upgrade")

	again, err := upgraded.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if again.FilesChanged != 0 || again.FilesIndexed != 0 || len(again.ParserProfileLanguages) != 0 {
		t.Fatalf("second update = %+v, want no-op", again)
	}
}

func TestP246BCompanionRenameAndDeleteLifecycle(t *testing.T) {
	unnamed := `package lib
class Service {
    @SomeAnnotation
    companion object {
        @JvmStatic fun run() {}
        private fun hidden() {}
        @JvmName("execute")
        internal fun internalOnly() {}
    }
}`
	r := newLifecycleRepo(t, tree{"Service.kt": unnamed, "Caller.java": "package app; import lib.Service; class Caller { void call() { Service.run(); } }"})
	assertPersistedCompanion(t, r, "Service.kt", "lib.Service.Companion", "Companion", "Service", "companion:kotlin:lib.Service.Companion")
	assertPersistedCompanionMember(t, r, "Service.kt", "lib.Service.Companion.run", "Service.Companion", "@JvmStatic fun run() {}", "public", "func:kotlin:lib.Service.Companion.run")
	assertPersistedCompanionMember(t, r, "Service.kt", "lib.Service.Companion.hidden", "Service.Companion", "private fun hidden() {}", "private", "func:kotlin:lib.Service.Companion.hidden")
	assertPersistedCompanionMember(t, r, "Service.kt", "lib.Service.Companion.internalOnly", "Service.Companion", "@JvmName(\"execute\")\n        internal fun internalOnly() {}", "internal", "func:kotlin:lib.Service.Companion.internalOnly")
	assertJVMResolved(t, r, "Caller.java", "Service.run", "Service.kt", "java_import_scope")

	stableID := p246SymbolID(t, r, "Service.kt", "companion:kotlin:lib.Service.Companion")
	r.write(t, "Other.java", "package app; class Other {}")
	r.update(t, "Other.java")
	if got := p246SymbolID(t, r, "Service.kt", "companion:kotlin:lib.Service.Companion"); got != stableID {
		t.Fatalf("unrelated update changed companion symbol id %d to %d", stableID, got)
	}

	named := `package lib
class Service {
    companion object Factory {
        @JvmStatic fun run() {}
    }
}`
	r.write(t, "Service.kt", named)
	r.update(t, "Service.kt")
	assertNoPersistedCompanion(t, r, "Service.kt", "lib.Service.Companion", "companion:kotlin:lib.Service.Companion")
	assertPersistedCompanion(t, r, "Service.kt", "lib.Service.Factory", "Factory", "Service", "companion:kotlin:lib.Service.Factory")
	assertPersistedCompanionMember(t, r, "Service.kt", "lib.Service.Factory.run", "Service.Factory", "@JvmStatic fun run() {}", "public", "func:kotlin:lib.Service.Factory.run")
	assertJVMResolved(t, r, "Caller.java", "Service.run", "Service.kt", "java_import_scope")
	r.assertFreshParity(t, "unnamed to named companion")

	r.write(t, "Service.kt", unnamed)
	r.update(t, "Service.kt")
	assertNoPersistedCompanion(t, r, "Service.kt", "lib.Service.Factory", "companion:kotlin:lib.Service.Factory")
	assertPersistedCompanion(t, r, "Service.kt", "lib.Service.Companion", "Companion", "Service", "companion:kotlin:lib.Service.Companion")
	r.assertFreshParity(t, "named to unnamed companion")

	r.remove(t, "Service.kt")
	r.update(t)
	if got := p246CompanionRows(t, p246DB(t, r), r.repoID, "Service.kt"); got != 0 {
		t.Fatalf("deleted file companion symbols = %d, want 0", got)
	}
	r.assertFreshParity(t, "companion source deletion")
}

type kotlinV2WithoutCompanions struct{ base *tsparser.KotlinAdapter }

func (a kotlinV2WithoutCompanions) Language() string          { return a.base.Language() }
func (a kotlinV2WithoutCompanions) Extensions() []string      { return a.base.Extensions() }
func (a kotlinV2WithoutCompanions) Supports(path string) bool { return a.base.Supports(path) }
func (a kotlinV2WithoutCompanions) Profile() parser.Profile {
	return parser.Profile{ID: "treesitter:kotlin:v2", EmitsCallEdges: true}
}
func (a kotlinV2WithoutCompanions) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	parsed, err := a.base.Parse(ctx, path, content)
	if err != nil {
		return graph.ParsedFile{}, err
	}
	kept := parsed.Symbols[:0]
	for _, sym := range parsed.Symbols {
		if sym.Kind == "companion_object" || strings.Contains(sym.QualifiedName, ".Companion.") {
			continue
		}
		kept = append(kept, sym)
	}
	parsed.Symbols = kept
	return parsed, nil
}

func p246Registry(kotlin parser.Adapter) *parser.Registry {
	return parser.NewRegistry(goparser.New(), tsparser.NewTypeScript(), tsparser.NewPython(), tsparser.NewCpp(), tsparser.NewJava(), kotlin, tsparser.NewRust())
}

func p246DB(t *testing.T, r *lifecycleRepo) *sql.DB {
	t.Helper()
	db, err := sql.Open(store.SQLiteDriverName(), r.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func p246CompanionRows(t *testing.T, db *sql.DB, repoID int64, path string) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM symbols s JOIN files f ON f.id=s.file_id WHERE s.repo_id=? AND f.path=? AND s.kind='companion_object'`, repoID, path).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func p246FileProfile(t *testing.T, db *sql.DB, repoID int64, path string) string {
	t.Helper()
	var profile string
	if err := db.QueryRow(`SELECT parser_profile FROM files WHERE repo_id=? AND path=?`, repoID, path).Scan(&profile); err != nil {
		t.Fatal(err)
	}
	return profile
}

func p246Profiles(t *testing.T, db *sql.DB, repoID int64) map[string]string {
	t.Helper()
	rows, err := db.Query(`SELECT path,parser_profile FROM files WHERE repo_id=?`, repoID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var path, profile string
		if err := rows.Scan(&path, &profile); err != nil {
			t.Fatal(err)
		}
		out[path] = profile
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func sameProfiles(before, after map[string]string, changed ...string) bool {
	changedSet := map[string]bool{}
	for _, path := range changed {
		changedSet[path] = true
	}
	if len(before) != len(after) {
		return false
	}
	for path, profile := range before {
		if !changedSet[path] && after[path] != profile {
			return false
		}
	}
	return true
}

func p246SymbolID(t *testing.T, r *lifecycleRepo, path, stableKey string) int64 {
	t.Helper()
	var id int64
	if err := p246DB(t, r).QueryRow(`SELECT s.id FROM symbols s JOIN files f ON f.id=s.file_id WHERE s.repo_id=? AND f.path=? AND s.stable_key=?`, r.repoID, path, stableKey).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

func assertPersistedCompanion(t *testing.T, r *lifecycleRepo, path, qname, name, container, stableKey string) {
	t.Helper()
	var count int
	if err := p246DB(t, r).QueryRow(`SELECT COUNT(*) FROM symbols s JOIN files f ON f.id=s.file_id WHERE s.repo_id=? AND f.path=? AND s.kind='companion_object' AND s.qualified_name=?`, r.repoID, path, qname).Scan(&count); err != nil || count != 1 {
		t.Fatalf("companion rows for %s = %d, %v; want exactly one", qname, count, err)
	}
	var gotName, gotQName, gotContainer, gotKey, kind string
	if err := p246DB(t, r).QueryRow(`SELECT s.name,s.qualified_name,s.container_name,s.stable_key,s.kind FROM symbols s JOIN files f ON f.id=s.file_id WHERE s.repo_id=? AND f.path=? AND s.qualified_name=?`, r.repoID, path, qname).Scan(&gotName, &gotQName, &gotContainer, &gotKey, &kind); err != nil {
		t.Fatal(err)
	}
	if kind != "companion_object" || gotName != name || gotQName != qname || gotContainer != container || gotKey != stableKey {
		t.Fatalf("companion = (%q,%q,%q,%q,%q), want (%q,%q,%q,%q,%q)", kind, gotName, gotQName, gotContainer, gotKey, "companion_object", name, qname, container, stableKey)
	}
}

func assertPersistedCompanionMember(t *testing.T, r *lifecycleRepo, path, qname, container, signature, visibility, stableKey string) {
	t.Helper()
	var count int
	if err := p246DB(t, r).QueryRow(`SELECT COUNT(*) FROM symbols s JOIN files f ON f.id=s.file_id WHERE s.repo_id=? AND f.path=? AND s.kind='function' AND s.qualified_name=?`, r.repoID, path, qname).Scan(&count); err != nil || count != 1 {
		t.Fatalf("companion member rows for %s = %d, %v; want exactly one", qname, count, err)
	}
	var gotContainer, gotSignature, gotVisibility, gotKey, kind string
	if err := p246DB(t, r).QueryRow(`SELECT s.container_name,s.signature,s.visibility,s.stable_key,s.kind FROM symbols s JOIN files f ON f.id=s.file_id WHERE s.repo_id=? AND f.path=? AND s.qualified_name=?`, r.repoID, path, qname).Scan(&gotContainer, &gotSignature, &gotVisibility, &gotKey, &kind); err != nil {
		t.Fatal(err)
	}
	if kind != "function" || gotContainer != container || gotSignature != signature || gotVisibility != visibility || gotKey != stableKey {
		t.Fatalf("companion member = (%q,%q,%q,%q,%q), want (%q,%q,%q,%q,%q)", kind, gotContainer, gotSignature, gotVisibility, gotKey, "function", container, signature, visibility, stableKey)
	}
}

func assertNoPersistedCompanion(t *testing.T, r *lifecycleRepo, path, qname, stableKey string) {
	t.Helper()
	var n int
	if err := p246DB(t, r).QueryRow(`SELECT COUNT(*) FROM symbols s JOIN files f ON f.id=s.file_id WHERE s.repo_id=? AND f.path=? AND (s.qualified_name=? OR s.qualified_name LIKE ? OR s.stable_key=?)`, r.repoID, path, qname, qname+".%", stableKey).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("stale companion identity %q/%q remains in %s", qname, stableKey, path)
	}
}
