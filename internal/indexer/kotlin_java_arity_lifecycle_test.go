//go:build cgo

package indexer

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/store"
)

func TestKotlinJavaOverloadAndCallArityLifecycle(t *testing.T) {
	kotlin := func(extra string) string {
		return "package lib\nfun run(x: kotlin.Int) {}\n" + extra
	}
	caller := `package app;
import lib.ActionsKt;
class Caller {
    void one() { ActionsKt.run(1); }
    void two() { ActionsKt.run(1, 2); }
}`
	r := newLifecycleRepo(t, tree{"Caller.java": caller, "Actions.kt": kotlin("")})
	ids := func() (int64, int64) {
		t.Helper()
		var one, two int64
		db, err := sql.Open(store.SQLiteDriverName(), r.dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if err := db.QueryRow(`SELECT id FROM symbols WHERE repo_id=? AND qualified_name='lib.run' AND signature='fun run(x: kotlin.Int) {}'`, r.repoID).Scan(&one); err != nil {
			t.Fatal(err)
		}
		err = db.QueryRow(`SELECT id FROM symbols WHERE repo_id=? AND qualified_name='lib.run' AND signature='fun run(x: kotlin.Int, y: kotlin.Int) {}'`, r.repoID).Scan(&two)
		if err == sql.ErrNoRows {
			return one, 0
		}
		if err != nil {
			t.Fatal(err)
		}
		return one, two
	}
	assertTarget := func(line int, want int64) {
		t.Helper()
		var got sql.NullInt64
		db, err := sql.Open(store.SQLiteDriverName(), r.dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if err := db.QueryRow(`SELECT e.dst_symbol_id FROM edges e JOIN files f ON f.id=e.file_id WHERE e.repo_id=? AND f.path='Caller.java' AND e.line=? AND e.edge_kind='calls'`, r.repoID, line).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if want == 0 && got.Valid || want != 0 && (!got.Valid || got.Int64 != want) {
			t.Fatalf("line %d target=%v, want %d", line, got, want)
		}
	}
	assertReferenceTarget := func(line int, want int64) {
		t.Helper()
		var got sql.NullInt64
		db, err := sql.Open(store.SQLiteDriverName(), r.dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		if err := db.QueryRow(`SELECT r.symbol_id FROM references_tbl r JOIN files f ON f.id=r.file_id WHERE r.repo_id=? AND f.path='Caller.java' AND r.start_line=?`, r.repoID, line).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if want == 0 && got.Valid || want != 0 && (!got.Valid || got.Int64 != want) {
			t.Fatalf("reference line %d target=%v, want %d", line, got, want)
		}
	}
	one, two := ids()
	if two != 0 {
		t.Fatalf("unexpected arity-2 target %d", two)
	}
	assertTarget(4, one)
	assertTarget(5, 0)
	r.assertFreshParity(t, "initial arity-1 target")

	r.write(t, "Actions.kt", kotlin("fun run(x: kotlin.String) {}\n"))
	r.update(t, "Actions.kt")
	assertTarget(4, 0)
	r.assertFreshParity(t, "same-arity overload arrival unbinds")

	r.write(t, "Actions.kt", kotlin(""))
	r.update(t, "Actions.kt")
	one, _ = ids()
	assertTarget(4, one)
	r.assertFreshParity(t, "same-arity overload removal rebinds")

	r.write(t, "Actions.kt", kotlin("fun run(x: kotlin.Int, y: kotlin.Int) {}\n"))
	r.update(t, "Actions.kt")
	one, two = ids()
	if two == 0 {
		t.Fatal("arity-2 target missing")
	}
	assertTarget(4, one)
	assertTarget(5, two)
	assertReferenceTarget(4, one)
	assertReferenceTarget(5, two)
	assertJVMReference(t, r, "Caller.java", "ActionsKt.run", true)
	oneCallers, err := r.store.FindCallers(r.ctx, r.repoID, "lib.run", one, 10, 0)
	if err != nil || len(oneCallers) != 1 || oneCallers[0].QualifiedName != "app.Caller.one" {
		t.Fatalf("arity-1 callers=%#v, %v", oneCallers, err)
	}
	twoCallers, err := r.store.FindCallers(r.ctx, r.repoID, "lib.run", two, 10, 0)
	if err != nil || len(twoCallers) != 1 || twoCallers[0].QualifiedName != "app.Caller.two" {
		t.Fatalf("arity-2 callers=%#v, %v", twoCallers, err)
	}
	oneCallees, err := r.store.FindCallees(r.ctx, r.repoID, "app.Caller.one", 0, 10, 0)
	if err != nil || len(oneCallees) != 1 || oneCallees[0].ID != one {
		t.Fatalf("one callees=%#v, %v", oneCallees, err)
	}
	twoCallees, err := r.store.FindCallees(r.ctx, r.repoID, "app.Caller.two", 0, 10, 0)
	if err != nil || len(twoCallees) != 1 || twoCallees[0].ID != two {
		t.Fatalf("two callees=%#v, %v", twoCallees, err)
	}
	r.assertFreshParity(t, "different-arity overload selection")

	// Replacing the callsite AST must replace CallArity and redirect its edge.
	r.write(t, "Caller.java", strings.Replace(caller, "ActionsKt.run(1)", "ActionsKt.run(1, 2)", 1))
	r.update(t, "Caller.java")
	assertTarget(4, two)
	assertReferenceTarget(4, two)
	r.assertFreshParity(t, "callsite arity 1 to 2")
	r.write(t, "Caller.java", caller)
	r.update(t, "Caller.java")
	assertTarget(4, one)
	assertReferenceTarget(4, one)
	r.assertFreshParity(t, "callsite arity 2 to 1")
}

func TestKotlinArgumentBearingJvmABIFamilies(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"Caller.java": `package app;
import lib.Obj;
import lib.AliasObj;
import lib.Service;
import lib.NamedService;
import lib.API;
import lib.Utils;
class Caller {
	void call() {
	    Obj.INSTANCE.instance(1);
	    Obj.staticRun(1);
        AliasObj.run(1);
        Service.Companion.plain(1);
        Service.Companion.both(1);
        Service.both(1);
        NamedService.Factory.named(1);
        API.top(1);
        Utils.first(1);
        Utils.second(1);
    }
}`,
		"Obj.kt": `package lib
object Obj {
    fun instance(x: kotlin.Int) {}
    @JvmStatic fun staticRun(x: kotlin.Int) {}
}`,
		"AliasObj.kt": `package lib
import kotlin.jvm.JvmStatic as Static
object AliasObj {
    @Static fun run(x: kotlin.Int) {}
}`,
		"Service.kt": `package lib
class Service {
    companion object {
        fun plain(x: kotlin.Int) {}
        @JvmStatic fun both(x: kotlin.Int) {}
    }
}`,
		"NamedService.kt": `package lib
class NamedService {
    companion object Factory {
        fun named(x: kotlin.Int) {}
    }
}`,
		"Actions.kt": `@file:JvmName("API")
package lib
fun top(x: kotlin.Int) {}`,
		"A.kt": `@file:JvmName("Utils")
@file:JvmMultifileClass
package lib
fun first(x: kotlin.Int) {}`,
		"B.kt": `@file:JvmName("Utils")
@file:JvmMultifileClass
package lib
fun second(x: kotlin.Int) {}`,
	})
	calls := []struct{ name, path string }{
		{"Obj.INSTANCE.instance", "Obj.kt"}, {"Obj.staticRun", "Obj.kt"},
		{"Service.Companion.plain", "Service.kt"}, {"Service.Companion.both", "Service.kt"}, {"Service.both", "Service.kt"},
		{"NamedService.Factory.named", "NamedService.kt"}, {"API.top", "Actions.kt"}, {"Utils.first", "A.kt"}, {"Utils.second", "B.kt"},
	}
	for _, call := range calls {
		assertJVMResolved(t, r, "Caller.java", call.name, call.path, "java_import_scope")
		assertJVMReference(t, r, "Caller.java", call.name, true)
	}
	assertJVMUnresolved(t, r, "Caller.java", "AliasObj.run")
	assertJVMReference(t, r, "Caller.java", "AliasObj.run", false)
	assertNoFacadeSymbols(t, r)
	r.assertFreshParity(t, "argument-bearing JVM ABI families")
	for _, call := range calls {
		r.update(t, call.path)
		assertJVMResolved(t, r, "Caller.java", call.name, call.path, "java_import_scope")
		r.assertFreshParity(t, "incremental argument-bearing "+call.name)
	}
	r.update(t, "AliasObj.kt")
	assertJVMUnresolved(t, r, "Caller.java", "AliasObj.run")
	r.assertFreshParity(t, "aliased JvmStatic remains unknown")
}

func TestKotlinDefaultAndJvmOverloadsArityLifecycle(t *testing.T) {
	caller := `package app;
import lib.ActionsKt;
class Caller {
    void one() { ActionsKt.run(1); }
    void two() { ActionsKt.run(1, "x"); }
    void zero() { ActionsKt.zero(); }
}`
	decl := func(overloads bool, sibling string) string {
		annotation := ""
		if overloads {
			annotation = "@JvmOverloads "
		}
		return "package lib\n" + annotation + "fun run(a: kotlin.Int, b: kotlin.String = \"\") {}\n" + sibling + "\n" + annotation + "fun zero(a: kotlin.Int = 0) {}\n"
	}
	r := newLifecycleRepo(t, tree{"Caller.java": caller, "Actions.kt": decl(true, "")})
	var runID, zeroID int64
	db, err := sql.Open(store.SQLiteDriverName(), r.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.QueryRow(`SELECT id FROM symbols WHERE repo_id=? AND qualified_name='lib.run'`, r.repoID).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT id FROM symbols WHERE repo_id=? AND qualified_name='lib.zero'`, r.repoID).Scan(&zeroID); err != nil {
		t.Fatal(err)
	}
	refreshIDs := func() {
		t.Helper()
		if err := db.QueryRow(`SELECT id FROM symbols WHERE repo_id=? AND qualified_name='lib.run' AND signature LIKE '%fun run(a: kotlin.Int%'`, r.repoID).Scan(&runID); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(`SELECT id FROM symbols WHERE repo_id=? AND qualified_name='lib.zero'`, r.repoID).Scan(&zeroID); err != nil {
			t.Fatal(err)
		}
	}
	var known, minArity, maxArity int
	if err := db.QueryRow(`SELECT is_known,jvm_arity_min,jvm_arity_max FROM kotlin_jvm_callable_evidence WHERE repo_id=? AND symbol_id=?`, r.repoID, runID).Scan(&known, &minArity, &maxArity); err != nil || known != 1 || minArity != 1 || maxArity != 2 {
		t.Fatalf("run JVM evidence = %d,%d..%d, %v", known, minArity, maxArity, err)
	}
	assert := func(line int, want int64) {
		t.Helper()
		var got sql.NullInt64
		if err := db.QueryRow(`SELECT e.dst_symbol_id FROM edges e JOIN files f ON f.id=e.file_id WHERE e.repo_id=? AND f.path='Caller.java' AND e.line=?`, r.repoID, line).Scan(&got); err != nil {
			t.Fatal(err)
		}
		if want == 0 && got.Valid || want != 0 && (!got.Valid || got.Int64 != want) {
			t.Fatalf("line %d target=%v, want %d", line, got, want)
		}
	}
	assert(4, runID)
	assert(5, runID)
	assert(6, zeroID)
	assertJVMReference(t, r, "Caller.java", "ActionsKt.run", true)
	callers, err := r.store.FindCallers(r.ctx, r.repoID, "lib.run", runID, 10, 0)
	if err != nil || len(callers) != 2 {
		t.Fatalf("generated-arity callers=%#v, err=%v", callers, err)
	}
	for _, caller := range []string{"app.Caller.one", "app.Caller.two"} {
		callees, err := r.store.FindCallees(r.ctx, r.repoID, caller, 0, 10, 0)
		if err != nil || len(callees) != 1 || callees[0].ID != runID {
			t.Fatalf("%s generated-arity callees=%#v, err=%v", caller, callees, err)
		}
	}
	var symbols int
	if err := db.QueryRow(`SELECT COUNT(*) FROM symbols WHERE repo_id=? AND language='kotlin' AND name IN ('run','zero')`, r.repoID).Scan(&symbols); err != nil || symbols != 2 {
		t.Fatalf("canonical Kotlin symbols=%d, err=%v", symbols, err)
	}
	r.assertFreshParity(t, "default JVM arities")

	r.write(t, "Actions.kt", decl(false, ""))
	r.update(t, "Actions.kt")
	refreshIDs()
	assert(4, 0) // plain default does not expose shorter Java overload
	assert(5, runID)
	assert(6, 0) // removing JvmOverloads removes generated zero-arg entry
	r.assertFreshParity(t, "remove JvmOverloads")

	r.write(t, "Actions.kt", decl(true, "fun run(a: kotlin.Long) {}"))
	r.update(t, "Actions.kt")
	refreshIDs()
	assert(4, 0)     // same-arity Java candidates are ambiguous
	assert(5, runID) // fixed one-arg sibling cannot overlap Java's two-arg call
	r.assertFreshParity(t, "manual sibling ambiguity")

	r.write(t, "Actions.kt", decl(true, ""))
	r.update(t, "Actions.kt")
	refreshIDs()
	assert(4, runID)
	assert(5, runID)
	r.assertFreshParity(t, "remove ambiguous sibling")

	unsupported := strings.Replace(decl(true, ""), "a: kotlin.Int", "a: custom.Type", 1)
	r.write(t, "Actions.kt", unsupported)
	r.update(t, "Actions.kt")
	if err := db.QueryRow(`SELECT id FROM symbols WHERE repo_id=? AND qualified_name='lib.run' AND signature LIKE '%fun run(a: custom.Type%'`, r.repoID).Scan(&runID); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT id FROM symbols WHERE repo_id=? AND qualified_name='lib.zero'`, r.repoID).Scan(&zeroID); err != nil {
		t.Fatal(err)
	}
	var evidenceKnown sql.NullInt64
	var unknownMin, unknownMax sql.NullInt64
	if err := db.QueryRow(`SELECT is_known,jvm_arity_min,jvm_arity_max FROM kotlin_jvm_callable_evidence WHERE repo_id=? AND symbol_id=?`, r.repoID, runID).Scan(&evidenceKnown, &unknownMin, &unknownMax); err != nil || evidenceKnown.Int64 != 0 || unknownMin.Valid || unknownMax.Valid {
		t.Fatalf("unsupported callable evidence=%v,%v,%v, err=%v", evidenceKnown, unknownMin, unknownMax, err)
	}
	assert(4, 0)
	assert(5, 0)
	r.assertFreshParity(t, "known to unknown callable evidence")
	r.write(t, "Actions.kt", decl(true, ""))
	r.update(t, "Actions.kt")
	refreshIDs()
	assert(4, runID)
	assert(5, runID)
	r.assertFreshParity(t, "unknown to known callable evidence")

	r.remove(t, "Actions.kt")
	r.update(t)
	assert(4, 0)
	assert(5, 0)
	var evidenceRows int
	if err := db.QueryRow(`SELECT COUNT(*) FROM kotlin_jvm_callable_evidence WHERE repo_id=?`, r.repoID).Scan(&evidenceRows); err != nil || evidenceRows != 0 {
		t.Fatalf("deleted Kotlin evidence rows=%d, err=%v", evidenceRows, err)
	}
	r.assertFreshParity(t, "delete Kotlin evidence owner")
	r.write(t, "Actions.kt", decl(true, ""))
	r.update(t, "Actions.kt")
	refreshIDs()
	assert(4, runID)
	assert(5, runID)
	r.assertFreshParity(t, "restore Kotlin evidence owner")
}

func TestKotlinJvmOverloadsExistingABIFamilies(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"Caller.java": `package app;
import lib.Obj;
import lib.Service;
import lib.ActionsKt;
class Caller {
    void call() {
        Obj.INSTANCE.instance(1);
        Obj.staticRun(1);
        Obj.INSTANCE.staticRun(1);
        Service.Companion.companionRun(1);
        Service.Companion.staticRun(1);
        Service.staticRun(1);
        ActionsKt.top(1);
    }
}`,
		"Obj.kt": `package lib
object Obj {
    @JvmOverloads fun instance(a: kotlin.Int, b: kotlin.String = "") {}
    @JvmSynthetic @JvmOverloads internal fun instance(hidden: kotlin.String = "") {}
    @JvmStatic @JvmOverloads fun staticRun(a: kotlin.Int, b: kotlin.String = "") {}
}`,
		"Service.kt": `package lib
class Service {
    companion object {
        @JvmOverloads fun companionRun(a: kotlin.Int, b: kotlin.String = "") {}
        @JvmStatic @JvmOverloads fun staticRun(a: kotlin.Int, b: kotlin.String = "") {}
    }
}`,
		"Actions.kt": `package lib
@JvmOverloads fun top(a: kotlin.Int, b: kotlin.String = "") {}`,
	})
	for _, call := range []struct{ name, path string }{
		{"Obj.INSTANCE.instance", "Obj.kt"},
		{"Service.Companion.companionRun", "Service.kt"},
		{"Service.Companion.staticRun", "Service.kt"},
		{"Service.staticRun", "Service.kt"},
		{"ActionsKt.top", "Actions.kt"},
	} {
		assertJVMResolved(t, r, "Caller.java", call.name, call.path, "java_import_scope")
		assertJVMReference(t, r, "Caller.java", call.name, true)
	}
	assertJVMUnresolved(t, r, "Caller.java", "Obj.INSTANCE.staticRun") // preserve the Kotlin object ABI's @JvmStatic INSTANCE refusal
	assertJVMReference(t, r, "Caller.java", "Obj.INSTANCE.staticRun", false)
	r.assertFreshParity(t, "@JvmOverloads across object/facade/companion ABI families")
}

func TestKotlinGeneratedOverloadAmbiguityAndPlainDefaultSibling(t *testing.T) {
	t.Run("two generated declarations remain ambiguous", func(t *testing.T) {
		r := newLifecycleRepo(t, tree{
			"Caller.java": `package app;
import lib.ActionsKt;
class Caller {
    void one() { ActionsKt.run(1); }
    void two() { ActionsKt.run(1, "x"); }
}`,
			"Actions.kt": `package lib
@JvmOverloads fun run(a: kotlin.Int, b: kotlin.String = "") {}
@JvmOverloads fun run(a: kotlin.Long, b: kotlin.Boolean = false) {}`,
		})
		assertJVMUnresolved(t, r, "Caller.java", "ActionsKt.run")
		assertJVMReference(t, r, "Caller.java", "ActionsKt.run", false)
		r.assertFreshParity(t, "overlapping generated overload ranges")
	})

	t.Run("plain default does not veto distinct fixed arity", func(t *testing.T) {
		r := newLifecycleRepo(t, tree{
			"Caller.java": `package app;
import lib.ActionsKt;
class Caller {
    void one() { ActionsKt.run(1); }
    void two() { ActionsKt.run(1, "x"); }
}`,
			"Actions.kt": `package lib
fun run(a: kotlin.Int, b: kotlin.String = "") {}
fun run(a: kotlin.Long) {}`,
		})
		db, err := sql.Open(store.SQLiteDriverName(), r.dbPath)
		if err != nil {
			t.Fatal(err)
		}
		defer db.Close()
		var fixedID, defaultID int64
		if err := db.QueryRow(`SELECT id FROM symbols WHERE repo_id=? AND signature='fun run(a: kotlin.Long) {}'`, r.repoID).Scan(&fixedID); err != nil {
			t.Fatal(err)
		}
		if err := db.QueryRow(`SELECT id FROM symbols WHERE repo_id=? AND signature='fun run(a: kotlin.Int, b: kotlin.String = "") {}'`, r.repoID).Scan(&defaultID); err != nil {
			t.Fatal(err)
		}
		var minArity, maxArity sql.NullInt64
		if err := db.QueryRow(`SELECT arity_min,arity_max FROM symbols WHERE id=?`, defaultID).Scan(&minArity, &maxArity); err != nil || minArity.Valid || maxArity.Valid {
			t.Fatalf("generic arity on default declaration=(%v,%v), err=%v", minArity, maxArity, err)
		}
		assertTarget := func(line int, want int64) {
			t.Helper()
			var got sql.NullInt64
			if err := db.QueryRow(`SELECT e.dst_symbol_id FROM edges e JOIN files f ON f.id=e.file_id WHERE e.repo_id=? AND f.path='Caller.java' AND e.line=?`, r.repoID, line).Scan(&got); err != nil || !got.Valid || got.Int64 != want {
				t.Fatalf("line %d target=%v, err=%v, want %d", line, got, err, want)
			}
		}
		assertTarget(4, fixedID)
		assertTarget(5, defaultID)
		r.assertFreshParity(t, "plain-default and fixed sibling disjoint arities")
	})
}
