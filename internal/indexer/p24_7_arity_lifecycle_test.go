//go:build cgo

package indexer

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/store"
)

func TestP247OverloadAndCallArityLifecycle(t *testing.T) {
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

func TestP247ArgumentBearingJVMABIFamilies(t *testing.T) {
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
