//go:build cgo

package indexer

import (
	"database/sql"
	"testing"

	"github.com/isink17/codegraph/internal/store"
)

func TestP245B5PersistedSourceTruth(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"Actions.kt": "package lib\nfun run() {}",
		"Object.kt": `package lib
object Service {
    fun run() {}
    @JvmStatic fun staticRun() {}
}`,
		"Companion.kt": `package lib
class CompanionService {
    companion object {
        fun run() {}
        @JvmStatic fun staticRun() {}
    }
}`,
		"Caller.java": `package app;
import lib.ActionsKt;
import lib.Service;
import lib.CompanionService;
class Caller { void call() {
    ActionsKt.run();
    Service.INSTANCE.run();
    Service.staticRun();
    CompanionService.Companion.run();
    CompanionService.staticRun();
} }`,
	})
	db, err := sql.Open(store.SQLiteDriverName(), r.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.QueryContext(r.ctx, `SELECT f.path,f.language,s.name,s.qualified_name,s.container_name,s.kind,s.signature,s.visibility,s.is_static,COALESCE(fs.package_name,'')
		FROM symbols s JOIN files f ON f.id=s.file_id LEFT JOIN file_scope_evidence fs ON fs.file_id=f.id AND fs.repo_id=f.repo_id
		WHERE f.repo_id=? AND f.language IN ('java','kotlin') ORDER BY f.path,s.start_line,s.id`, r.repoID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	wantSymbols := map[string]struct {
		language, qname, container, kind, signature, visibility, pkg string
		static                                                       sql.NullInt64
	}{
		"Actions.kt|run":                {"kotlin", "lib.run", "lib", "function", "fun run() {}", "public", "lib", sql.NullInt64{}},
		"Object.kt|Service":             {"kotlin", "lib.Service", "lib", "object", "", "public", "lib", sql.NullInt64{}},
		"Object.kt|run":                 {"kotlin", "lib.Service.run", "Service", "function", "fun run() {}", "public", "lib", sql.NullInt64{}},
		"Object.kt|staticRun":           {"kotlin", "lib.Service.staticRun", "Service", "function", "@JvmStatic fun staticRun() {}", "public", "lib", sql.NullInt64{}},
		"Companion.kt|CompanionService": {"kotlin", "lib.CompanionService", "lib", "class", "", "public", "lib", sql.NullInt64{}},
		"Caller.java|Caller":            {"java", "app.Caller", "app", "type", "", "package", "app", sql.NullInt64{}},
		"Caller.java|call":              {"java", "app.Caller.call", "Caller", "function", "void call()", "package", "app", sql.NullInt64{Int64: 0, Valid: true}},
	}
	for rows.Next() {
		var path, lang, name, qname, container, kind, sig, visibility, pkg string
		var static sql.NullInt64
		if err := rows.Scan(&path, &lang, &name, &qname, &container, &kind, &sig, &visibility, &static, &pkg); err != nil {
			t.Fatal(err)
		}
		key := path + "|" + name
		want, ok := wantSymbols[key]
		if !ok || lang != want.language || qname != want.qname || container != want.container || kind != want.kind || sig != want.signature || visibility != want.visibility || pkg != want.pkg || static != want.static {
			t.Fatalf("persisted symbol %s = (%q,%q,%q,%q,%q,%q,is_static=%v,package=%q), unexpected", key, qname, container, kind, sig, visibility, lang, static, pkg)
		}
		delete(wantSymbols, key)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(wantSymbols) != 0 {
		t.Fatalf("missing persisted Kotlin symbols: %v", wantSymbols)
	}
	rows, err = db.QueryContext(r.ctx, `SELECT f.path,e.edge_kind,e.dst_name,e.evidence,e.dst_symbol_id,e.resolution_strategy,r.name,r.qualified_name,r.symbol_id
		FROM edges e JOIN files f ON f.id=e.file_id LEFT JOIN references_tbl r ON r.file_id=e.file_id AND r.start_line=e.line AND r.ref_kind='call'
		WHERE f.repo_id=? AND f.language='java' ORDER BY e.line,e.id`, r.repoID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	wantEdges := map[string]struct {
		qname, strategy string
		resolved        bool
	}{
		"ActionsKt.run":                  {"", "", false},
		"Service.INSTANCE.run":           {"lib.Service.run", "java_import_scope", true},
		"Service.staticRun":              {"lib.Service.staticRun", "java_import_scope", true},
		"CompanionService.Companion.run": {"", "", false},
		"CompanionService.staticRun":     {"", "", false},
	}
	for rows.Next() {
		var path, kind, dstName, evidence, strategy, refName, refQName string
		var dst, ref sql.NullInt64
		if err := rows.Scan(&path, &kind, &dstName, &evidence, &dst, &strategy, &refName, &refQName, &ref); err != nil {
			t.Fatal(err)
		}
		want, ok := wantEdges[dstName]
		if !ok || path != "Caller.java" || kind != "calls" || evidence != dstName+"()" || refName != dstName || refQName != dstName || dst.Valid != want.resolved || ref.Valid != want.resolved || strategy != want.strategy {
			t.Fatalf("persisted Java edge %q = (kind=%q,evidence=%q,dst=%v,strategy=%q,reference=%q/%q:%v), unexpected", dstName, kind, evidence, dst, strategy, refName, refQName, ref)
		}
		if want.resolved {
			var qname string
			if err := db.QueryRowContext(r.ctx, `SELECT qualified_name FROM symbols WHERE id=?`, dst.Int64).Scan(&qname); err != nil || qname != want.qname {
				t.Fatalf("Java edge %q target = (%q,%v), want %q", dstName, qname, err, want.qname)
			}
		}
		delete(wantEdges, dstName)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(wantEdges) != 0 {
		t.Fatalf("missing persisted Java edges: %v", wantEdges)
	}
}
