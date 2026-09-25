package store

import "testing"

func TestJVMCoreInteropScopes(t *testing.T) {
	t.Run("java_constructs_kotlin_class_and_refuses_kotlin_static_form", func(t *testing.T) {
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
		if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET edge_kind='constructs' WHERE id=?`, construct); err != nil {
			t.Fatal(err)
		}
		member := f.edge(t, callerFile, caller, "Service.run")
		if _, err := resolveJavaScope(f.ctx, f.store.db, f.repoID, nil); err != nil {
			t.Fatal(err)
		}
		if got, ok := f.dstSymbolID(t, construct); !ok || got != target {
			t.Fatalf("construct target=(%d,%v), want (%d,true)", got, ok, target)
		}
		if got, ok := f.dstSymbolID(t, member); ok {
			t.Fatalf("Java Type.member bound Kotlin source method %d, want unresolved", got)
		}
		_ = method
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
