//go:build cgo

package indexer

import (
	"database/sql"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/store"
)

func TestKotlinPropertyJavaABIFailsClosed(t *testing.T) {
	// Pinned Gradle 9.3.1 / KGP 2.3.20 `javap -p -s` oracle for this fixture:
	// generated setCount(int) has descriptor (I)V; declared setCount(String)
	// has descriptor (Ljava/lang/String;)V. Both Java calls compile with javac.
	java := `package app;
import lib.Service;
class Caller {
    void positive() { Service.INSTANCE.run(); }
    void access() {
        Service.INSTANCE.getValue();
        Service.INSTANCE.getCount();
        Service.INSTANCE.setCount(3);
        Service.INSTANCE.setCount("x");
        int ignored = Service.raw;
    }
}`
	object := `package lib
object Service {
    fun run() {}
    val value: Int = 7
    var count: Int = 1
    fun setCount(value: String) {}
    @JvmField val raw: Int = 2
}`
	r := newLifecycleRepo(t, tree{"Caller.java": java, "Service.kt": object})
	assertPropertyFailClosedState(t, r, "initial")
	if summary := r.update(t); summary.FilesIndexed != 0 {
		t.Fatalf("no-op update indexed %d files", summary.FilesIndexed)
	}
	assertPropertyFailClosedState(t, r, "no-op update")

	r.write(t, "Service.kt", `package lib
object Service {
    fun run() {}
    val value: Int = 8
    var count: Int = 2
    fun setCount(value: String) {}
    @JvmField val raw: Int = 4
}`)
	r.update(t, "Service.kt")
	assertPropertyFailClosedState(t, r, "property value change")

	r.write(t, "Service.kt", `package lib
object Service {
    fun run() {}
    val renamed: Int = 8
    var count: Int = 2
    fun setCount(value: String) {}
    @JvmField val raw: Int = 4
}`)
	r.update(t, "Service.kt")
	assertPropertyFailClosedState(t, r, "property rename")

	r.write(t, "Service.kt", `package lib
object Service {
    fun run() {}
    var count: Int = 2
    fun setCount(value: String) {}
    @JvmField val raw: Int = 4
}`)
	r.update(t, "Service.kt")
	assertPropertyFailClosedState(t, r, "property removal")

	r.write(t, "Service.kt", object)
	r.update(t, "Service.kt")
	assertPropertyFailClosedState(t, r, "property restoration")
}

func assertPropertyFailClosedState(t *testing.T, r *lifecycleRepo, step string) {
	t.Helper()
	assertJVMResolved(t, r, "Caller.java", "Service.INSTANCE.run", "Service.kt", "java_import_scope")
	assertJVMReference(t, r, "Caller.java", "Service.INSTANCE.run", true)
	assertJVMQueryRelation(t, r, "app.Caller.positive", "lib.Service.run")

	for _, accessor := range []string{"Service.INSTANCE.getValue", "Service.INSTANCE.getCount", "Service.INSTANCE.setCount"} {
		assertJVMUnresolved(t, r, "Caller.java", accessor)
		assertJVMReference(t, r, "Caller.java", accessor, false)
		assertJVMNoQueryRelation(t, r, "app.Caller.access", "lib.Service."+accessor[strings.LastIndex(accessor, ".")+1:])
	}
	assertNoKotlinPropertyCallableSymbols(t, r)
	assertPropertyReferencesUnboundAndRecorded(t, r)
	assertPropertySetterCollisionUnbound(t, r)
	assertNoRawFieldCallBinding(t, r)
	r.assertFreshParity(t, step)
}

func assertPropertySetterCollisionUnbound(t *testing.T, r *lifecycleRepo) {
	t.Helper()
	db, err := sql.Open(store.SQLiteDriverName(), r.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.QueryContext(r.ctx, `SELECT e.line,e.dst_name,e.dst_symbol_id,e.resolution_strategy,r.id,r.name,r.qualified_name,r.ref_kind,r.symbol_id
		FROM edges e JOIN files f ON f.id=e.file_id
		LEFT JOIN references_tbl r ON r.repo_id=e.repo_id AND r.file_id=e.file_id AND r.ref_kind='call' AND r.start_line=e.line
		WHERE e.repo_id=? AND f.path='Caller.java' AND e.edge_kind='calls' AND e.dst_name='Service.INSTANCE.setCount'
		ORDER BY e.line`, r.repoID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got int
	lineCounts := map[int]int{8: 0, 9: 0}
	for rows.Next() {
		var line int
		var dstName, strategy string
		var edgeTarget, refID, refTarget sql.NullInt64
		var refName, refQualifiedName, refKind sql.NullString
		if err := rows.Scan(&line, &dstName, &edgeTarget, &strategy, &refID, &refName, &refQualifiedName, &refKind, &refTarget); err != nil {
			t.Fatal(err)
		}
		if line != 8 && line != 9 {
			t.Errorf("unexpected setCount call line %d", line)
		}
		lineCounts[line]++
		if !refID.Valid || !refName.Valid || !refQualifiedName.Valid || !refKind.Valid || refKind.String != "call" || refName.String != dstName || refQualifiedName.String != dstName {
			t.Errorf("setCount line %d reference id=%v name=%v qualified_name=%v kind=%v; want exactly one matching persisted call reference", line, refID, refName, refQualifiedName, refKind)
		}
		if edgeTarget.Valid || refTarget.Valid || edgeTarget != refTarget || strategy != "" {
			t.Errorf("setCount line %d edge target=%v reference target=%v strategy=%q; want consistent unresolved state", line, edgeTarget, refTarget, strategy)
		}
		got++
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if got != 2 {
		t.Fatalf("persisted setCount call edges=%d, want int and String call edges", got)
	}
	if lineCounts[8] != 1 || lineCounts[9] != 1 {
		t.Fatalf("persisted setCount edges by line=%v, want exactly one int call on line 8 and one String call on line 9", lineCounts)
	}
	callees, err := r.store.FindCallees(r.ctx, r.repoID, "app.Caller.access", 0, 50, 0)
	if err != nil || len(callees) != 0 {
		t.Fatalf("accessor caller callees=%+v, err=%v; want no unsupported accessor relation", callees, err)
	}
	var targetID int64
	if err := db.QueryRowContext(r.ctx, `SELECT id FROM symbols WHERE repo_id=? AND language='kotlin' AND kind='function' AND qualified_name='lib.Service.setCount'`, r.repoID).Scan(&targetID); err != nil {
		t.Fatal(err)
	}
	callers, err := r.store.FindCallers(r.ctx, r.repoID, "lib.Service.setCount", targetID, 50, 0)
	if err != nil || len(callers) != 0 {
		t.Fatalf("setCount callers=%+v, err=%v; want no falsely associated caller", callers, err)
	}
}

func assertNoKotlinPropertyCallableSymbols(t *testing.T, r *lifecycleRepo) {
	t.Helper()
	db, err := sql.Open(store.SQLiteDriverName(), r.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err := db.QueryRowContext(r.ctx, `SELECT COUNT(*) FROM symbols WHERE repo_id=? AND language='kotlin' AND qualified_name IN ('lib.Service.value','lib.Service.count','lib.Service.raw') AND kind IN ('function','method')`, r.repoID).Scan(&n); err != nil || n != 0 {
		t.Fatalf("property callable symbols=%d, err=%v", n, err)
	}
}

func assertPropertyReferencesUnboundAndRecorded(t *testing.T, r *lifecycleRepo) {
	t.Helper()
	db, err := sql.Open(store.SQLiteDriverName(), r.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, name := range []string{"Service.INSTANCE.getValue", "Service.INSTANCE.getCount", "Service.INSTANCE.setCount"} {
		var count, bound int
		err := db.QueryRowContext(r.ctx, `SELECT COUNT(*),COALESCE(SUM(CASE WHEN symbol_id IS NOT NULL THEN 1 ELSE 0 END),0) FROM references_tbl r JOIN files f ON f.id=r.file_id WHERE r.repo_id=? AND f.path='Caller.java' AND r.ref_kind='call' AND r.qualified_name=?`, r.repoID, name).Scan(&count, &bound)
		if err != nil || count == 0 || bound != 0 {
			t.Errorf("accessor call refs %s count=%d bound=%d err=%v; want recorded and unbound", name, count, bound, err)
		}
	}
}

func assertNoRawFieldCallBinding(t *testing.T, r *lifecycleRepo) {
	t.Helper()
	db, err := sql.Open(store.SQLiteDriverName(), r.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var calls int
	if err := db.QueryRowContext(r.ctx, `SELECT COUNT(*) FROM edges e JOIN files f ON f.id=e.file_id WHERE e.repo_id=? AND f.path='Caller.java' AND e.edge_kind='calls' AND e.dst_name LIKE '%raw%'`, r.repoID).Scan(&calls); err != nil || calls != 0 {
		t.Fatalf("raw field call edges=%d err=%v", calls, err)
	}
	var bound int
	if err := db.QueryRowContext(r.ctx, `SELECT COUNT(*) FROM references_tbl r JOIN files f ON f.id=r.file_id WHERE r.repo_id=? AND f.path='Caller.java' AND r.qualified_name LIKE '%raw%' AND r.symbol_id IS NOT NULL`, r.repoID).Scan(&bound); err != nil || bound != 0 {
		t.Fatalf("raw field bound refs=%d err=%v", bound, err)
	}
}
