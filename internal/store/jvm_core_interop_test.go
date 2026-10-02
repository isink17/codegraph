package store

import (
	"testing"
)

func TestJVMCoreInteropScopes(t *testing.T) {
	t.Run("java_peer_kotlin_class_makes_type_identity_ambiguous", func(t *testing.T) {
		f := newGateFixture(t)
		callerFile := f.file(t, "Caller.java", "java")
		caller := f.symbolKind(t, callerFile, "call", "app.Caller.call", "function", "java")
		javaFile := f.file(t, "JavaService.java", "java")
		javaType := f.symbolKind(t, javaFile, "Service", "lib.Service", "type", "java")
		javaRun := f.symbolKind(t, javaFile, "run", "lib.Service.run", "function", "java")
		kotlinFile := f.file(t, "KotlinService.kt", "kotlin")
		f.symbolKind(t, kotlinFile, "Service", "lib.Service", "class", "kotlin")
		for _, x := range []struct {
			file int64
			lang string
			pkg  string
		}{{callerFile, "java", "app"}, {javaFile, "java", "lib"}, {kotlinFile, "kotlin", "lib"}} {
			if _, err := f.store.db.ExecContext(f.ctx, `INSERT INTO file_scope_evidence(repo_id,file_id,language,package_name) VALUES(?,?,?,?)`, f.repoID, x.file, x.lang, x.pkg); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET container_name='lib' WHERE id=?`, javaType); err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET visibility='public',is_static=1 WHERE id=?`, javaRun); err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.db.ExecContext(f.ctx, `INSERT INTO scope_import_evidence(repo_id,file_id,language,source_specifier,imported_name,local_name,import_kind) VALUES(?,?,?,?,?,?,?)`, f.repoID, callerFile, "java", "lib.Service", "Service", "Service", "named"); err != nil {
			t.Fatal(err)
		}
		edge := f.edge(t, callerFile, caller, "Service.run")
		if _, err := resolveJavaScope(f.ctx, f.store.db, f.repoID, nil); err != nil {
			t.Fatal(err)
		}
		if got, ok := f.dstSymbolID(t, edge); ok {
			t.Fatalf("Java peer Kotlin class did not make owner ambiguous: bound %d", got)
		}
	})

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

func TestJVMGeneratedCallableABIScopes(t *testing.T) {
	addScope := func(t *testing.T, f *gateFixture, file int64, language, pkg string) {
		t.Helper()
		if _, err := f.store.db.ExecContext(f.ctx, `INSERT INTO file_scope_evidence(repo_id,file_id,language,package_name) VALUES(?,?,?,?)`, f.repoID, file, language, pkg); err != nil {
			t.Fatal(err)
		}
	}
	addImport := func(t *testing.T, f *gateFixture, file int64, source, local string) {
		t.Helper()
		if _, err := f.store.db.ExecContext(f.ctx, `INSERT INTO scope_import_evidence(repo_id,file_id,language,source_specifier,imported_name,local_name,import_kind) VALUES(?,?,?,?,?,?,?)`, f.repoID, file, "java", source, local, local, "named"); err != nil {
			t.Fatal(err)
		}
	}
	setCallEvidence := func(t *testing.T, f *gateFixture, edge int64, evidence string) {
		t.Helper()
		if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET evidence=? WHERE id=?`, evidence, edge); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("object INSTANCE requires Kotlin object structure", func(t *testing.T) {
		for _, tc := range []struct {
			name, kind, signature string
			resolved              bool
		}{
			{"object", "object", "fun run() {}", true},
			{"object static method", "object", "@JvmStatic fun run() {}", false},
			{"ordinary class", "class", "fun run() {}", false},
			{"renamed JVM method", "object", `@JvmName("renamed") fun run() {}`, false},
		} {
			t.Run(tc.name, func(t *testing.T) {
				f := newGateFixture(t)
				callerFile := f.file(t, "app/Caller.java", "java")
				caller := f.symbolKind(t, callerFile, "call", "app.Caller.call", "function", "java")
				ownerFile := f.file(t, "lib/Service.kt", "kotlin")
				addScope(t, f, callerFile, "java", "app")
				addScope(t, f, ownerFile, "kotlin", "lib")
				addImport(t, f, callerFile, "lib.Service", "Service")
				f.symbolKind(t, ownerFile, "Service", "lib.Service", tc.kind, "kotlin")
				run := f.symbolKind(t, ownerFile, "run", "lib.Service.run", "function", "kotlin")
				if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET container_name='Service',visibility='public',signature=? WHERE id=?`, tc.signature, run); err != nil {
					t.Fatal(err)
				}
				edge := f.edge(t, callerFile, caller, "Service.INSTANCE.run")
				setCallEvidence(t, f, edge, "Service.INSTANCE.run()")
				if _, err := resolveJavaScope(f.ctx, f.store.db, f.repoID, nil); err != nil {
					t.Fatal(err)
				}
				dst, ok := f.dstSymbolID(t, edge)
				got := "<unresolved>"
				if ok {
					got = f.qualifiedNameOf(t, dst)
				}
				if ok != tc.resolved || ok && got != "lib.Service.run" {
					t.Fatalf("INSTANCE binding = %q, want resolved=%v", got, tc.resolved)
				}
			})
		}
	})

	t.Run("static object call requires persisted JvmStatic annotation", func(t *testing.T) {
		for _, tc := range []struct {
			name, signature, visibility string
			resolved, overload          bool
		}{
			{"annotated", "@JvmStatic fun run() {}", "public", true, false},
			{"unannotated", "fun run() {}", "public", false, false},
			{"qualified annotation", "@kotlin.jvm.JvmStatic fun run() {}", "public", true, false},
			{"private", "@JvmStatic fun run() {}", "private", false, false},
			{"overload", "@JvmStatic fun run() {}", "public", false, true},
		} {
			t.Run(tc.name, func(t *testing.T) {
				f := newGateFixture(t)
				callerFile := f.file(t, "app/Caller.java", "java")
				caller := f.symbolKind(t, callerFile, "call", "app.Caller.call", "function", "java")
				ownerFile := f.file(t, "lib/Service.kt", "kotlin")
				addScope(t, f, callerFile, "java", "app")
				addScope(t, f, ownerFile, "kotlin", "lib")
				addImport(t, f, callerFile, "lib.Service", "Service")
				f.symbolKind(t, ownerFile, "Service", "lib.Service", "object", "kotlin")
				run := f.symbolKind(t, ownerFile, "run", "lib.Service.run", "function", "kotlin")
				if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET container_name='Service',visibility=?,signature=? WHERE id=?`, tc.visibility, tc.signature, run); err != nil {
					t.Fatal(err)
				}
				if tc.overload {
					overload := f.symbolKind(t, ownerFile, "run", "lib.Service.run", "function", "kotlin")
					if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET container_name='Service',visibility='public',signature='@JvmStatic fun run(value: Int) {}' WHERE id=?`, overload); err != nil {
						t.Fatal(err)
					}
				}
				edge := f.edge(t, callerFile, caller, "Service.run")
				setCallEvidence(t, f, edge, "Service.run()")
				if _, err := resolveJavaScope(f.ctx, f.store.db, f.repoID, nil); err != nil {
					t.Fatal(err)
				}
				dst, ok := f.dstSymbolID(t, edge)
				got := "<unresolved>"
				if ok {
					got = f.qualifiedNameOf(t, dst)
				}
				if ok != tc.resolved || ok && got != "lib.Service.run" {
					t.Fatalf("static binding = %q, want resolved=%v", got, tc.resolved)
				}
			})
		}
	})
}

func TestJVMObjectFixedArityCallable(t *testing.T) {
	for _, tc := range []struct {
		name, signature, call, secondName, secondSignature string
		secondArity                                        any
		want                                               bool
	}{
		{"INSTANCE", "fun run(x: kotlin.Int) {}", "Service.INSTANCE.run(1)", "", "", nil, true},
		{"static JvmStatic", "@JvmStatic fun run(x: kotlin.Int) {}", "Service.run(1)", "", "", nil, true},
		{"static without JvmStatic", "fun run(x: kotlin.Int) {}", "Service.run(1)", "", "", nil, false},
		{"INSTANCE refuses JvmStatic", "@JvmStatic fun run(x: kotlin.Int) {}", "Service.INSTANCE.run(1)", "", "", nil, false},
		{"different arity sibling", "fun run(x: kotlin.Int) {}", "Service.INSTANCE.run(1)", "run", "fun run(x: kotlin.Int, y: kotlin.Int) {}", 2, true},
		{"same arity sibling", "fun run(x: kotlin.Int) {}", "Service.INSTANCE.run(1)", "run", "fun run(x: kotlin.String) {}", 1, false},
		{"unsupported sibling veto", "fun run(x: kotlin.Int) {}", "Service.INSTANCE.run(1)", "run", `@JvmOverloads fun run(x: kotlin.String = "") {}`, nil, false},
		{"renamed source sibling veto", "fun run(x: kotlin.Int) {}", "Service.INSTANCE.run(1)", "other", `@JvmName("run") fun other(x: kotlin.String) {}`, nil, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newGateFixture(t)
			callerFile := f.file(t, "app/Caller.java", "java")
			caller := f.symbolKind(t, callerFile, "call", "app.Caller.call", "function", "java")
			ownerFile := f.file(t, "lib/Service.kt", "kotlin")
			for _, x := range []struct {
				file      int64
				lang, pkg string
			}{{callerFile, "java", "app"}, {ownerFile, "kotlin", "lib"}} {
				if _, err := f.store.db.ExecContext(f.ctx, `INSERT INTO file_scope_evidence(repo_id,file_id,language,package_name) VALUES(?,?,?,?)`, f.repoID, x.file, x.lang, x.pkg); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := f.store.db.ExecContext(f.ctx, `INSERT INTO scope_import_evidence(repo_id,file_id,language,source_specifier,imported_name,local_name,import_kind) VALUES(?,?,?,?,?,?,?)`, f.repoID, callerFile, "java", "lib.Service", "Service", "Service", "named"); err != nil {
				t.Fatal(err)
			}
			f.symbolKind(t, ownerFile, "Service", "lib.Service", "object", "kotlin")
			target := f.symbolKind(t, ownerFile, "run", "lib.Service.run", "function", "kotlin")
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET container_name='Service',visibility='public',signature=?,arity_min=1,arity_max=1 WHERE id=?`, tc.signature, target); err != nil {
				t.Fatal(err)
			}
			if tc.secondSignature != "" {
				extra := f.symbolKind(t, ownerFile, tc.secondName, "lib.Service."+tc.secondName, "function", "kotlin")
				if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET container_name='Service',visibility='public',signature=?,arity_min=?,arity_max=? WHERE id=?`, tc.secondSignature, tc.secondArity, tc.secondArity, extra); err != nil {
					t.Fatal(err)
				}
			}
			edge := f.edge(t, callerFile, caller, tc.call[:len(tc.call)-3])
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET edge_kind='calls',evidence=?,call_arity=1 WHERE id=?`, tc.call, edge); err != nil {
				t.Fatal(err)
			}
			if _, err := resolveJavaScope(f.ctx, f.store.db, f.repoID, nil); err != nil {
				t.Fatal(err)
			}
			got, ok := f.dstSymbolID(t, edge)
			if ok != tc.want || ok && got != target {
				t.Fatalf("binding=(%d,%v), want target=%d resolved=%v", got, ok, target, tc.want)
			}
		})
	}
}
