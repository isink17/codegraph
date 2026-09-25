package store

import (
	"fmt"
	"testing"
)

func TestJVMCoreInteropScopes(t *testing.T) {
	t.Run("java_refuses_kotlin_source_forms", func(t *testing.T) {
		f := newGateFixture(t)
		callerFile := f.file(t, "src/Caller.java", "java")
		caller := f.symbol(t, callerFile, "caller", "app.Caller.caller", "java")
		targetFile := f.file(t, "src/Service.kt", "kotlin")
		target := f.symbolKind(t, targetFile, "Service", "lib.Service", "class", "kotlin")
		method := f.symbolKind(t, targetFile, "run", "lib.Service.run", "function", "kotlin")
		for _, file := range []int64{callerFile, targetFile} {
			pkg := "lib"
			if file == callerFile {
				pkg = "app"
			}
			if _, err := f.store.db.ExecContext(f.ctx, `INSERT INTO file_scope_evidence(repo_id,file_id,language,package_name) VALUES(?,?,?,?)`, f.repoID, file, map[bool]string{true: "java", false: "kotlin"}[file == callerFile], pkg); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := f.store.db.ExecContext(f.ctx, `INSERT INTO scope_import_evidence(repo_id,file_id,language,source_specifier,imported_name,local_name,import_kind) VALUES(?,?,?,?,?,?,?)`, f.repoID, callerFile, "java", "lib.Service", "Service", "Service", "named"); err != nil {
			t.Fatal(err)
		}
		construct := f.edge(t, callerFile, caller, "Service")
		member := f.edge(t, callerFile, caller, "Service.run")
		if _, err := resolveJavaScope(f.ctx, f.store.db, f.repoID, nil); err != nil {
			t.Fatal(err)
		}
		if got, ok := f.dstSymbolID(t, construct); ok {
			t.Fatalf("Java construction bound Kotlin class %d, want unresolved", got)
		}
		if got, ok := f.dstSymbolID(t, member); ok {
			t.Fatalf("Java Type.member bound Kotlin source method %d, want unresolved", got)
		}
		_, _ = target, method
	})

	t.Run("kotlin_binds_unique_java_static_and_refuses_instance_and_overload", func(t *testing.T) {
		f := newGateFixture(t)
		callerFile := f.file(t, "src/Caller.kt", "kotlin")
		caller := f.symbol(t, callerFile, "caller", "app.caller", "kotlin")
		targetFile := f.file(t, "src/Service.java", "java")
		typeID := f.symbolKind(t, targetFile, "Service", "lib.Service", "type", "java")
		static := f.symbolKind(t, targetFile, "run", "lib.Service.run", "function", "java")
		for _, file := range []int64{callerFile, targetFile} {
			pkg := "lib"
			language := "java"
			if file == callerFile {
				pkg, language = "app", "kotlin"
			}
			if _, err := f.store.db.ExecContext(f.ctx, `INSERT INTO file_scope_evidence(repo_id,file_id,language,package_name) VALUES(?,?,?,?)`, f.repoID, file, language, pkg); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := f.store.db.ExecContext(f.ctx, `INSERT INTO scope_import_evidence(repo_id,file_id,language,source_specifier,imported_name,local_name,import_kind) VALUES(?,?,?,?,?,?,?)`, f.repoID, callerFile, "kotlin", "lib.Service", "Service", "Service", "named"); err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET visibility='public',is_static=1 WHERE id=?`, static); err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET container_name='lib' WHERE id=?`, typeID); err != nil {
			t.Fatal(err)
		}
		run := f.edge(t, callerFile, caller, "Service.run")
		construct := f.edge(t, callerFile, caller, "Service")
		if _, err := resolveKotlinScope(f.ctx, f.store.db, f.repoID, nil); err != nil {
			t.Fatal(err)
		}
		if got, ok := f.dstSymbolID(t, run); !ok || got != static {
			t.Fatalf("static target=(%d,%v), want (%d,true)", got, ok, static)
		}
		var strategy string
		if err := f.store.db.QueryRowContext(f.ctx, `SELECT resolution_strategy FROM edges WHERE id=?`, run).Scan(&strategy); err != nil || strategy != "kotlin_import_scope" {
			t.Fatalf("static strategy=(%q,%v), want kotlin_import_scope", strategy, err)
		}
		if got, ok := f.dstSymbolID(t, construct); !ok || got != typeID {
			t.Fatalf("constructor target=(%d,%v), want (%d,true)", got, ok, typeID)
		}
		if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET dst_symbol_id=NULL,resolution_strategy='',resolution_confidence='' WHERE id=?`, run); err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET is_static=0 WHERE id=?`, static); err != nil {
			t.Fatal(err)
		}
		if _, err := resolveKotlinScope(f.ctx, f.store.db, f.repoID, nil); err != nil {
			t.Fatal(err)
		}
		if _, ok := f.dstSymbolID(t, run); ok {
			t.Fatal("Kotlin bound Java instance method")
		}
	})
}

func TestJVMCoreInteropRepair(t *testing.T) {
	f := newGateFixture(t)
	callerFile := f.file(t, "Caller.kt", "kotlin")
	targetFile := f.file(t, "Service.java", "java")
	caller := f.symbolKind(t, callerFile, "call", "app.call", "function", "kotlin")
	typeID := f.symbolKind(t, targetFile, "Service", "lib.Service", "type", "java")
	target := f.symbolKind(t, targetFile, "run", "lib.Service.run", "function", "java")
	for _, x := range []struct {
		file int64
		lang string
		pkg  string
	}{{callerFile, "kotlin", "app"}, {targetFile, "java", "lib"}} {
		if _, err := f.store.db.ExecContext(f.ctx, `INSERT INTO file_scope_evidence(repo_id,file_id,language,package_name) VALUES(?,?,?,?)`, f.repoID, x.file, x.lang, x.pkg); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET container_name='lib' WHERE id=?`, typeID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET visibility='public',is_static=1 WHERE id=?`, target); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `INSERT INTO scope_import_evidence(repo_id,file_id,language,source_specifier,imported_name,local_name,import_kind) VALUES(?,?,?,?,?,?,?)`, f.repoID, callerFile, "kotlin", "lib.Service", "Service", "Service", "named"); err != nil {
		t.Fatal(err)
	}
	edge := f.edge(t, callerFile, caller, "Service.run")
	if err := f.store.markRepairDone(f.ctx, jvmScopePrecisionRepairSettingKey, f.repoID); err != nil {
		t.Fatal(err)
	}
	if ran, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID); err != nil || !ran {
		t.Fatalf("B2 repair=(%v,%v), want (true,nil)", ran, err)
	}
	if got, ok := f.dstSymbolID(t, edge); !ok || got != target {
		t.Fatalf("B2 repair target=(%d,%v), want (%d,true)", got, ok, target)
	}
	key := fmt.Sprintf("%s.%d", jvmCoreInteropRepairSettingKey, f.repoID)
	var value string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&value); err != nil || value != "1" {
		t.Fatalf("B2 marker=(%q,%v), want 1", value, err)
	}
	if ran, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID); err != nil || ran {
		t.Fatalf("second B2 repair=(%v,%v), want (false,nil)", ran, err)
	}
}
