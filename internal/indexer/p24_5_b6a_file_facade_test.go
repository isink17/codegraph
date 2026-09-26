//go:build cgo

package indexer

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/parser"
	tsparser "github.com/isink17/codegraph/internal/parser/treesitter"
	"github.com/isink17/codegraph/internal/store"
)

// assertKotlinFacade checks the persisted facade facts for one Kotlin path;
// want "" means the path has no facade row (or no file).
func assertKotlinFacade(t *testing.T, dbPath string, repoID int64, path, want string) {
	t.Helper()
	db, err := sql.Open(store.SQLiteDriverName(), dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var got string
	err = db.QueryRowContext(context.Background(), `SELECT fs.package_name||'.'||fs.jvm_facade_class||'|explicit='||fs.jvm_facade_explicit||'|multifile='||fs.jvm_multifile
		FROM file_scope_evidence fs JOIN files f ON f.id=fs.file_id AND f.repo_id=fs.repo_id
		WHERE f.repo_id=? AND f.path=? AND f.is_deleted=0 AND fs.jvm_facade_class!=''`, repoID, path).Scan(&got)
	if err == sql.ErrNoRows {
		got = ""
	} else if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("facade(%s) = %q, want %q", path, got, want)
	}
}

func assertNoFacadeSymbols(t *testing.T, r *lifecycleRepo) {
	t.Helper()
	db, err := sql.Open(store.SQLiteDriverName(), r.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRowContext(r.ctx, `SELECT COUNT(*) FROM symbols WHERE repo_id=? AND (name LIKE '%Kt' OR name IN ('Actions','Other','API','Utils'))`, r.repoID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("facade symbols = %d, %v", n, err)
	}
}

const facadeCaller = `package app;
import lib.ActionsKt;
import lib.Actions;
import lib.Other;
class Caller {
    void viaDefault() { ActionsKt.run(); }
    void viaActions() { Actions.run(); }
    void viaOther() { Other.run(); }
}`

func TestJVMFileFacadeDefaultAndRefusal(t *testing.T) {
	r := newLifecycleRepo(t, tree{"Caller.java": facadeCaller, "lib/Actions.kt": "package lib\nfun run() {}"})
	assertKotlinFacade(t, r.dbPath, r.repoID, "lib/Actions.kt", "lib.ActionsKt|explicit=0|multifile=0")
	assertJVMResolved(t, r, "Caller.java", "ActionsKt.run", "lib/Actions.kt", "java_import_scope")
	assertJVMReference(t, r, "Caller.java", "ActionsKt.run", true)
	assertJVMQueryRelation(t, r, "app.Caller.viaDefault", "lib.run")
	assertJVMUnresolved(t, r, "Caller.java", "Actions.run")
	assertJVMReference(t, r, "Caller.java", "Actions.run", false)
	assertJVMNoQueryRelation(t, r, "app.Caller.viaActions", "lib.run")
	assertNoFacadeSymbols(t, r)
	r.assertFreshParity(t, "default facade")

	// A Kotlin class spelled like the facade makes the owner ambiguous.
	r.write(t, "lib/Model.kt", "package lib\nclass ActionsKt { fun run() {} }")
	r.update(t, "lib/Model.kt")
	assertJVMUnresolved(t, r, "Caller.java", "ActionsKt.run")
	assertJVMReference(t, r, "Caller.java", "ActionsKt.run", false)
	assertJVMNoQueryRelation(t, r, "app.Caller.viaDefault", "lib.run")
	r.assertFreshParity(t, "facade-spelled class competitor")

	r.remove(t, "lib/Model.kt")
	r.update(t, "lib/Model.kt")
	assertJVMResolved(t, r, "Caller.java", "ActionsKt.run", "lib/Actions.kt", "java_import_scope")
	r.assertFreshParity(t, "competitor removed")

	// So does a Java type of the same spelling arriving on update.
	r.write(t, "lib/ActionsKt.java", "package lib; public class ActionsKt { public static void run() {} }")
	r.update(t, "lib/ActionsKt.java")
	assertJVMUnresolved(t, r, "Caller.java", "ActionsKt.run")
	assertJVMReference(t, r, "Caller.java", "ActionsKt.run", false)
	r.assertFreshParity(t, "facade-spelled Java type competitor")

	r.remove(t, "lib/ActionsKt.java")
	r.update(t, "lib/ActionsKt.java")
	assertJVMResolved(t, r, "Caller.java", "ActionsKt.run", "lib/Actions.kt", "java_import_scope")
	r.assertFreshParity(t, "Java competitor removed")
}

func TestJVMFileFacadeJvmNameLifecycle(t *testing.T) {
	stateA := "package lib\nfun run() {}"
	stateB := "@file:JvmName(\"Actions\")\npackage lib\nfun run() {}"
	stateC := "@file:JvmName(\"Other\")\npackage lib\nfun run() {}"
	unrelated := "package app;\nimport lib.Missing;\nclass Unrelated { void call() { Missing.stop(); } }"
	r := newLifecycleRepo(t, tree{"Caller.java": facadeCaller, "Unrelated.java": unrelated, "lib/Actions.kt": stateA})

	type want struct{ viaDefault, viaActions, viaOther bool }
	check := func(step string, w want) {
		t.Helper()
		for _, c := range []struct {
			edge, caller string
			resolved     bool
		}{{"ActionsKt.run", "app.Caller.viaDefault", w.viaDefault}, {"Actions.run", "app.Caller.viaActions", w.viaActions}, {"Other.run", "app.Caller.viaOther", w.viaOther}} {
			if c.resolved {
				assertJVMResolved(t, r, "Caller.java", c.edge, "lib/Actions.kt", "java_import_scope")
				assertJVMQueryRelation(t, r, c.caller, "lib.run")
			} else {
				assertJVMUnresolved(t, r, "Caller.java", c.edge)
				assertJVMNoQueryRelation(t, r, c.caller, "lib.run")
			}
			assertJVMReference(t, r, "Caller.java", c.edge, c.resolved)
		}
		assertJVMUnresolved(t, r, "Unrelated.java", "Missing.stop")
		assertNoFacadeSymbols(t, r)
		r.assertFreshParity(t, step)
	}
	edit := func(content string) {
		t.Helper()
		r.write(t, "lib/Actions.kt", content)
		summary := r.update(t, "lib/Actions.kt")
		// Path/name scoped: only the `run` spellings are re-decided, never the
		// unrelated caller or the repository.
		if summary.ResolveMode != "paths+names" || summary.ResolveCrossFileTargets > 3 {
			t.Fatalf("facade edit summary mode=%q targets=%d", summary.ResolveMode, summary.ResolveCrossFileTargets)
		}
	}

	check("state A default", want{viaDefault: true})
	edit(stateB)
	assertKotlinFacade(t, r.dbPath, r.repoID, "lib/Actions.kt", "lib.Actions|explicit=1|multifile=0")
	check("state B JvmName Actions", want{viaActions: true})
	edit(stateC)
	assertKotlinFacade(t, r.dbPath, r.repoID, "lib/Actions.kt", "lib.Other|explicit=1|multifile=0")
	check("state C JvmName Other", want{viaOther: true})
	edit(stateA)
	assertKotlinFacade(t, r.dbPath, r.repoID, "lib/Actions.kt", "lib.ActionsKt|explicit=0|multifile=0")
	check("state D annotation removed", want{viaDefault: true})
}

func TestJVMFileFacadeRenameLifecycle(t *testing.T) {
	java := `package app;
import lib.ActionsKt;
import lib.CommandsKt;
import lib.API;
class Caller {
    void viaActions() { ActionsKt.run(); }
    void viaCommands() { CommandsKt.run(); }
    void viaApi() { API.go(); }
}`
	r := newLifecycleRepo(t, tree{"Caller.java": java, "lib/Actions.kt": "package lib\nfun run() {}", "lib/Named.kt": "@file:JvmName(\"API\")\npackage lib\nfun go() {}"})
	assertJVMResolved(t, r, "Caller.java", "ActionsKt.run", "lib/Actions.kt", "java_import_scope")
	assertJVMUnresolved(t, r, "Caller.java", "CommandsKt.run")
	assertJVMResolved(t, r, "Caller.java", "API.go", "lib/Named.kt", "java_import_scope")
	r.assertFreshParity(t, "before rename")

	r.remove(t, "lib/Actions.kt")
	r.write(t, "lib/Commands.kt", "package lib\nfun run() {}")
	r.update(t, "lib/Actions.kt", "lib/Commands.kt")
	assertKotlinFacade(t, r.dbPath, r.repoID, "lib/Actions.kt", "")
	assertKotlinFacade(t, r.dbPath, r.repoID, "lib/Commands.kt", "lib.CommandsKt|explicit=0|multifile=0")
	assertJVMUnresolved(t, r, "Caller.java", "ActionsKt.run")
	assertJVMReference(t, r, "Caller.java", "ActionsKt.run", false)
	assertJVMNoQueryRelation(t, r, "app.Caller.viaActions", "lib.run")
	assertJVMResolved(t, r, "Caller.java", "CommandsKt.run", "lib/Commands.kt", "java_import_scope")
	assertJVMReference(t, r, "Caller.java", "CommandsKt.run", true)
	assertJVMQueryRelation(t, r, "app.Caller.viaCommands", "lib.run")
	r.assertFreshParity(t, "default facade follows file rename")

	// An explicit name does not follow the file name.
	r.remove(t, "lib/Named.kt")
	r.write(t, "lib/Renamed.kt", "@file:JvmName(\"API\")\npackage lib\nfun go() {}")
	r.update(t, "lib/Named.kt", "lib/Renamed.kt")
	assertKotlinFacade(t, r.dbPath, r.repoID, "lib/Named.kt", "")
	assertKotlinFacade(t, r.dbPath, r.repoID, "lib/Renamed.kt", "lib.API|explicit=1|multifile=0")
	assertJVMResolved(t, r, "Caller.java", "API.go", "lib/Renamed.kt", "java_import_scope")
	assertJVMQueryRelation(t, r, "app.Caller.viaApi", "lib.go")
	r.assertFreshParity(t, "explicit facade survives file rename")
}

func TestJVMFileFacadeMultifileLifecycle(t *testing.T) {
	java := `package app;
import lib.Utils;
class Caller {
    void callFirst() { Utils.first(); }
    void callSecond() { Utils.second(); }
}`
	partA := "@file:JvmName(\"Utils\")\n@file:JvmMultifileClass\npackage lib\nfun first() {}"
	partB := "@file:JvmName(\"Utils\")\n@file:JvmMultifileClass\npackage lib\nfun second() {}"
	r := newLifecycleRepo(t, tree{"Caller.java": java, "lib/A.kt": partA})
	assertJVMResolved(t, r, "Caller.java", "Utils.first", "lib/A.kt", "java_import_scope")
	assertJVMUnresolved(t, r, "Caller.java", "Utils.second")
	r.assertFreshParity(t, "single part")

	both := func(step string) {
		t.Helper()
		assertJVMResolved(t, r, "Caller.java", "Utils.first", "lib/A.kt", "java_import_scope")
		assertJVMResolved(t, r, "Caller.java", "Utils.second", "lib/B.kt", "java_import_scope")
		assertJVMReference(t, r, "Caller.java", "Utils.first", true)
		assertJVMReference(t, r, "Caller.java", "Utils.second", true)
		assertJVMQueryRelation(t, r, "app.Caller.callFirst", "lib.first")
		assertJVMQueryRelation(t, r, "app.Caller.callSecond", "lib.second")
		r.assertFreshParity(t, step)
	}
	neither := func(step string) {
		t.Helper()
		assertJVMUnresolved(t, r, "Caller.java", "Utils.first")
		assertJVMUnresolved(t, r, "Caller.java", "Utils.second")
		assertJVMReference(t, r, "Caller.java", "Utils.first", false)
		assertJVMNoQueryRelation(t, r, "app.Caller.callFirst", "lib.first")
		r.assertFreshParity(t, step)
	}

	r.write(t, "lib/B.kt", partB)
	r.update(t, "lib/B.kt")
	assertKotlinFacade(t, r.dbPath, r.repoID, "lib/B.kt", "lib.Utils|explicit=1|multifile=1")
	both("second part added")

	r.remove(t, "lib/B.kt")
	r.update(t, "lib/B.kt")
	assertJVMResolved(t, r, "Caller.java", "Utils.first", "lib/A.kt", "java_import_scope")
	assertJVMUnresolved(t, r, "Caller.java", "Utils.second")
	r.assertFreshParity(t, "part deleted")

	r.write(t, "lib/B.kt", partB)
	r.update(t, "lib/B.kt")
	both("part restored")

	// B leaves the group: A keeps Utils, B becomes Tools.
	r.write(t, "lib/B.kt", strings.Replace(partB, `"Utils"`, `"Tools"`, 1))
	r.update(t, "lib/B.kt")
	assertJVMResolved(t, r, "Caller.java", "Utils.first", "lib/A.kt", "java_import_scope")
	assertJVMUnresolved(t, r, "Caller.java", "Utils.second")
	r.assertFreshParity(t, "part renamed")

	r.write(t, "lib/B.kt", partB)
	r.update(t, "lib/B.kt")
	both("shared name restored")

	// A same-named part without @JvmMultifileClass is a JVM class clash.
	r.write(t, "lib/B.kt", strings.Replace(partB, "@file:JvmMultifileClass\n", "", 1))
	r.update(t, "lib/B.kt")
	neither("part without JvmMultifileClass")

	// Deleting the clashing part in a whole-repository update (no paths) must
	// still re-decide A's callers: only B's old facade name reaches them.
	r.remove(t, "lib/B.kt")
	r.update(t)
	assertJVMResolved(t, r, "Caller.java", "Utils.first", "lib/A.kt", "java_import_scope")
	r.assertFreshParity(t, "clashing part deleted by scan")
	r.write(t, "lib/B.kt", strings.Replace(partB, "@file:JvmMultifileClass\n", "", 1))
	r.update(t)
	neither("clashing part added by scan")

	// A malformed name gives B no facade at all; A stands alone again.
	r.write(t, "lib/B.kt", strings.Replace(partB, `"Utils"`, "UTILS", 1))
	r.update(t, "lib/B.kt")
	assertKotlinFacade(t, r.dbPath, r.repoID, "lib/B.kt", "")
	assertJVMResolved(t, r, "Caller.java", "Utils.first", "lib/A.kt", "java_import_scope")
	assertJVMUnresolved(t, r, "Caller.java", "Utils.second")
	r.assertFreshParity(t, "malformed part")

	// A second declaration of first() across the parts is ambiguous.
	r.write(t, "lib/B.kt", partB+"\nfun first() {}")
	r.update(t, "lib/B.kt")
	assertJVMUnresolved(t, r, "Caller.java", "Utils.first")
	assertJVMResolved(t, r, "Caller.java", "Utils.second", "lib/B.kt", "java_import_scope")
	r.assertFreshParity(t, "duplicate member")

	// Another package's Utils part is a different owner.
	r.write(t, "lib/B.kt", strings.Replace(partB, "package lib", "package other", 1))
	r.update(t, "lib/B.kt")
	assertJVMResolved(t, r, "Caller.java", "Utils.first", "lib/A.kt", "java_import_scope")
	assertJVMUnresolved(t, r, "Caller.java", "Utils.second")
	r.assertFreshParity(t, "other package part")
}

// B5 object ABI is unaffected by facade evidence in the same file.
func TestJVMFileFacadeKeepsObjectABI(t *testing.T) {
	java := `package app;
import lib.Service;
import lib.ServiceKt;
class Caller {
    void instance() { Service.INSTANCE.run(); }
    void direct() { Service.run(); }
    void staticCall() { Service.staticRun(); }
    void staticInstance() { Service.INSTANCE.staticRun(); }
    void facade() { ServiceKt.top(); }
}`
	r := newLifecycleRepo(t, tree{"Caller.java": java, "Service.kt": `package lib
object Service {
    fun run() {}
    @JvmStatic fun staticRun() {}
}
fun top() {}`})
	assertKotlinFacade(t, r.dbPath, r.repoID, "Service.kt", "lib.ServiceKt|explicit=0|multifile=0")
	assertJVMResolved(t, r, "Caller.java", "Service.INSTANCE.run", "Service.kt", "java_import_scope")
	assertJVMUnresolved(t, r, "Caller.java", "Service.run")
	assertJVMResolved(t, r, "Caller.java", "Service.staticRun", "Service.kt", "java_import_scope")
	assertJVMUnresolved(t, r, "Caller.java", "Service.INSTANCE.staticRun")
	assertJVMResolved(t, r, "Caller.java", "ServiceKt.top", "Service.kt", "java_import_scope")
	assertJVMQueryRelation(t, r, "app.Caller.facade", "lib.top")
	assertJVMNoQueryRelation(t, r, "app.Caller.direct", "lib.Service.run")
	r.assertFreshParity(t, "object and facade in one file")
}

// kotlinV1Adapter reproduces genuine treesitter:kotlin:v1 output: the current
// parser minus the B6A facade facts it did not persist.
type kotlinV1Adapter struct {
	*tsparser.KotlinAdapter
}

func (kotlinV1Adapter) Profile() parser.Profile {
	return parser.Profile{ID: "treesitter:kotlin:v1", EmitsCallEdges: true}
}

func (a kotlinV1Adapter) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	p, err := a.KotlinAdapter.Parse(ctx, path, content)
	p.Scope.JVMFacade = graph.JVMFileFacade{}
	return p, err
}

func TestJVMFileFacadeKotlinV1ProfileUpgrade(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	writeProfileFile(t, filepath.Join(root, "Caller.java"), facadeCaller)
	writeProfileFile(t, filepath.Join(root, "Other.java"), "package app;\nimport lib.Missing;\nclass Other { void call() { Missing.stop(); } }")
	writeProfileFile(t, filepath.Join(root, "Actions.kt"), "package lib\nfun run() {}")
	writeProfileFile(t, filepath.Join(root, "main.go"), "package main\nfunc main() {}\n")
	s := newProfileStore(t)
	old := New(s.Store, parser.NewRegistry(tsparser.NewJava(), kotlinV1Adapter{tsparser.NewKotlin()}, tsparser.NewGo()), nil)
	if _, err := old.Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	repo := repoID(t, s, root)
	assertKotlinFacade(t, s.path, repo, "Actions.kt", "")
	binding := func() string {
		t.Helper()
		var got string
		if err := s.raw(t).QueryRowContext(ctx, `SELECT COALESCE(d.qualified_name,'-')||'|'||COALESCE(e.resolution_strategy,'') FROM edges e JOIN files f ON f.id=e.file_id LEFT JOIN symbols d ON d.id=e.dst_symbol_id WHERE e.repo_id=? AND f.path='Caller.java' AND e.dst_name='ActionsKt.run'`, repo).Scan(&got); err != nil {
			t.Fatal(err)
		}
		return got
	}
	if got := binding(); got != "-|" {
		t.Fatalf("v1 binding = %q, want unresolved", got)
	}
	// Every resolver repair is already recorded: only the profile reparse can
	// bring the facade in.
	if err := s.Store.MarkResolverBindingsRepaired(ctx, repo); err != nil {
		t.Fatal(err)
	}
	upgraded := New(s.Store, parser.NewRegistry(tsparser.NewJava(), tsparser.NewKotlin(), tsparser.NewGo()), nil)
	summary, err := upgraded.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	// Only Kotlin reparses: Java and Go keep their profiles and bytes.
	if summary.FilesChanged != 1 || summary.FilesIndexed != 1 || strings.Join(summary.ParserProfileLanguages, ",") != "kotlin" {
		t.Fatalf("upgrade summary = %+v", summary)
	}
	assertKotlinFacade(t, s.path, repo, "Actions.kt", "lib.ActionsKt|explicit=0|multifile=0")
	if got := binding(); got != "lib.run|java_import_scope" {
		t.Fatalf("upgraded binding = %q", got)
	}
	var profile string
	if err := s.raw(t).QueryRowContext(ctx, `SELECT parser_profile FROM files WHERE repo_id=? AND path='Actions.kt'`, repo).Scan(&profile); err != nil || profile != "treesitter:kotlin:v2" {
		t.Fatalf("upgraded profile = %q, %v", profile, err)
	}
	fresh := newProfileStore(t)
	if _, err := New(fresh.Store, parser.NewRegistry(tsparser.NewJava(), tsparser.NewKotlin(), tsparser.NewGo()), nil).Index(ctx, Options{RepoRoot: root}); err != nil {
		t.Fatal(err)
	}
	freshRepo := repoID(t, fresh, root)
	assertKotlinFacade(t, fresh.path, freshRepo, "Actions.kt", "lib.ActionsKt|explicit=0|multifile=0")
	again, err := upgraded.Update(ctx, Options{RepoRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	if again.FilesChanged != 0 || again.FilesIndexed != 0 || len(again.ParserProfileLanguages) != 0 {
		t.Fatalf("second update = %+v", again)
	}
	if got := binding(); got != "lib.run|java_import_scope" {
		t.Fatalf("converged binding = %q", got)
	}
}
