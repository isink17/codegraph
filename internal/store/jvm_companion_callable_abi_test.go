package store

import (
	"database/sql"
	"strings"
	"testing"
)

type companionScopeFixture struct {
	*gateFixture
	callerFile, caller, target int64
}

func newCompanionScopeFixture(t *testing.T, field, signature, companionVisibility, memberVisibility string) *companionScopeFixture {
	t.Helper()
	f := &companionScopeFixture{gateFixture: newGateFixture(t)}
	f.callerFile = f.file(t, "app/Caller.java", "java")
	f.caller = f.symbolKind(t, f.callerFile, "call", "app.Caller.call", "function", "java")
	ownerFile := f.file(t, "lib/Service.kt", "kotlin")
	for _, scope := range []struct {
		file int64
		lang string
		pkg  string
	}{{f.callerFile, "java", "app"}, {ownerFile, "kotlin", "lib"}} {
		if _, err := f.store.db.ExecContext(f.ctx, `INSERT INTO file_scope_evidence(repo_id,file_id,language,package_name) VALUES(?,?,?,?)`, f.repoID, scope.file, scope.lang, scope.pkg); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.store.db.ExecContext(f.ctx, `INSERT INTO scope_import_evidence(repo_id,file_id,language,source_specifier,imported_name,local_name,import_kind) VALUES(?,?,?,?,?,?,?)`, f.repoID, f.callerFile, "java", "lib.Service", "Service", "Service", "named"); err != nil {
		t.Fatal(err)
	}
	outer := f.symbolKind(t, ownerFile, "Service", "lib.Service", "class", "kotlin")
	companion := f.symbolKind(t, ownerFile, field, "lib.Service."+field, "companion_object", "kotlin")
	f.target = f.symbolKind(t, ownerFile, "run", "lib.Service."+field+".run", "function", "kotlin")
	for id, values := range map[int64][]string{
		outer:     {"lib", "public", ""},
		companion: {"Service", companionVisibility, ""},
		f.target:  {"Service." + field, memberVisibility, signature},
	} {
		if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET container_name=?,visibility=?,signature=? WHERE id=?`, values[0], values[1], values[2], id); err != nil {
			t.Fatal(err)
		}
	}
	return f
}

func (f *companionScopeFixture) resolve(t *testing.T, name string) (int64, bool) {
	t.Helper()
	edge := f.edge(t, f.callerFile, f.caller, name)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET edge_kind='calls',evidence=? WHERE id=?`, name+"()", edge); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveJavaScope(f.ctx, f.store.db, f.repoID, nil); err != nil {
		t.Fatal(err)
	}
	return f.dstSymbolID(t, edge)
}

func TestJVMKotlinCompanionCallableABIScopes(t *testing.T) {
	for _, tc := range []struct {
		name, field, signature string
		spelling               []string
		resolved               []bool
	}{
		{"unnamed ordinary", "Companion", "fun run() {}", []string{"Service.Companion.run", "lib.Service.Companion.run", "Service.run", "Service.INSTANCE.run", "Service.Companion.INSTANCE.run"}, []bool{true, true, false, false, false}},
		{"unnamed JvmStatic", "Companion", "@JvmStatic fun run() {}", []string{"Service.Companion.run", "Service.run", "Service.INSTANCE.run", "Service.Companion.INSTANCE.run"}, []bool{true, true, false, false}},
		{"named ordinary", "Factory", "fun run() {}", []string{"Service.Factory.run", "lib.Service.Factory.run", "Service.run", "Service.Companion.run", "Service.Factory.INSTANCE.run"}, []bool{true, true, false, false, false}},
		{"named JvmStatic", "Factory", "@JvmStatic fun run() {}", []string{"Service.Factory.run", "Service.run", "Service.Factory.INSTANCE.run"}, []bool{true, true, false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for i, spelling := range tc.spelling {
				f := newCompanionScopeFixture(t, tc.field, tc.signature, "public", "public")
				got, ok := f.resolve(t, spelling)
				if ok != tc.resolved[i] || ok && got != f.target {
					t.Fatalf("%s binding=(%d,%v), want canonical target %d resolved=%v", spelling, got, ok, f.target, tc.resolved[i])
				}
				if ok {
					if qname := f.qualifiedNameOf(t, got); qname != "lib.Service."+tc.field+".run" {
						t.Fatalf("target = %q, want canonical companion member", qname)
					}
				}
			}
		})
	}
}

func TestJVMKotlinCompanionCallableABIRequiresOwnedVisibleEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, field, signature, companionVisibility, memberVisibility string
	}{
		{"wrong field", "Factory", "fun run() {}", "public", "public"},
		{"nested object Companion cannot replace companion", "Companion", "fun run() {}", "public", "public"},
		{"named nested object cannot replace companion", "Factory", "@JvmStatic fun run() {}", "public", "public"},
		{"private companion", "Companion", "fun run() {}", "private", "public"},
		{"internal companion", "Companion", "fun run() {}", "internal", "public"},
		{"private member", "Companion", "fun run() {}", "public", "private"},
		{"internal member", "Companion", "fun run() {}", "public", "internal"},
		{"renamed member", "Companion", `@JvmName("go") fun run() {}`, "public", "public"},
		{"synthetic member", "Companion", "@JvmSynthetic fun run() {}", "public", "public"},
		{"suspend member", "Companion", "suspend fun run() {}", "public", "public"},
		{"extension member", "Companion", "fun String.run() {}", "public", "public"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCompanionScopeFixture(t, tc.field, tc.signature, tc.companionVisibility, tc.memberVisibility)
			if strings.Contains(tc.name, "nested object") {
				if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET kind='object' WHERE qualified_name=?`, "lib.Service."+tc.field); err != nil {
					t.Fatal(err)
				}
			}
			spelling := "Service." + tc.field + ".run"
			if tc.name == "wrong field" {
				spelling = "Service.Companion.run"
			}
			if got, ok := f.resolve(t, spelling); ok {
				t.Fatalf("unsupported call resolved to %d", got)
			}
		})
	}
}

func TestJVMKotlinCompanionCallableABIRefusesOverloadsAndAmbiguousOwners(t *testing.T) {
	t.Run("overload", func(t *testing.T) {
		f := newCompanionScopeFixture(t, "Companion", "fun run() {}", "public", "public")
		extra := f.symbolKind(t, f.file(t, "lib/Other.kt", "kotlin"), "run", "lib.Service.Companion.run", "function", "kotlin")
		if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET container_name='Service.Companion',visibility='public',signature='fun run(value: Int) {}' WHERE id=?`, extra); err != nil {
			t.Fatal(err)
		}
		if _, ok := f.resolve(t, "Service.Companion.run"); ok {
			t.Fatal("overloaded companion member resolved")
		}
	})
	t.Run("Java peer makes outer ambiguous", func(t *testing.T) {
		f := newCompanionScopeFixture(t, "Companion", "fun run() {}", "public", "public")
		file := f.file(t, "lib/Service.java", "java")
		f.symbolKind(t, file, "Service", "lib.Service", "type", "java")
		if _, ok := f.resolve(t, "Service.Companion.run"); ok {
			t.Fatal("ambiguous Java and Kotlin outer resolved")
		}
	})
}

func TestJVMKotlinCompanionFixedAritySelectionAndVeto(t *testing.T) {
	for _, tc := range []struct {
		name          string
		secondName    string
		secondSig     string
		secondArity   any
		wantResolved  bool
		wantQualified string
	}{
		{name: "unique arity", wantResolved: true, wantQualified: "lib.Service.Companion.run"},
		{name: "different arity sibling", secondName: "run", secondSig: "fun run(x: kotlin.Int, y: kotlin.Int) {}", secondArity: 2, wantResolved: true, wantQualified: "lib.Service.Companion.run"},
		{name: "same arity overload", secondName: "run", secondSig: "fun run(x: kotlin.String) {}", secondArity: 1},
		{name: "unknown overload veto", secondName: "run", secondSig: `@JvmOverloads fun run(x: kotlin.String = "") {}`},
		{name: "suspend sibling veto", secondName: "run", secondSig: "suspend fun run(x: kotlin.String) {}"},
		{name: "renamed source sibling veto", secondName: "other", secondSig: `@JvmName("run") fun other(x: kotlin.String) {}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCompanionScopeFixture(t, "Companion", "fun run(x: kotlin.Int) {}", "public", "public")
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET arity_min=1,arity_max=1 WHERE id=?`, f.target); err != nil {
				t.Fatal(err)
			}
			if tc.secondSig != "" {
				var ownerFile int64
				if err := f.store.db.QueryRowContext(f.ctx, `SELECT file_id FROM symbols WHERE id=?`, f.target).Scan(&ownerFile); err != nil {
					t.Fatal(err)
				}
				extra := f.symbolKind(t, ownerFile, tc.secondName, "lib.Service.Companion."+tc.secondName, "function", "kotlin")
				if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET container_name='Service.Companion',visibility='public',signature=?,arity_min=?,arity_max=? WHERE id=?`, tc.secondSig, tc.secondArity, tc.secondArity, extra); err != nil {
					t.Fatal(err)
				}
			}
			edge := f.edge(t, f.callerFile, f.caller, "Service.Companion.run")
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET edge_kind='calls',evidence='Service.Companion.run(1)',call_arity=1 WHERE id=?`, edge); err != nil {
				t.Fatal(err)
			}
			if _, err := resolveJavaScope(f.ctx, f.store.db, f.repoID, nil); err != nil {
				t.Fatal(err)
			}
			got, ok := f.dstSymbolID(t, edge)
			if ok != tc.wantResolved {
				t.Fatalf("resolved=%v target=%d, want %v", ok, got, tc.wantResolved)
			}
			if ok && f.qualifiedNameOf(t, got) != tc.wantQualified {
				t.Fatalf("target=%q", f.qualifiedNameOf(t, got))
			}
		})
	}
}

func TestJVMCompanionRepairGateAndConvergence(t *testing.T) {
	f := newCompanionScopeFixture(t, "Factory", "fun run() {}", "public", "public")
	edge := f.edge(t, f.callerFile, f.caller, "Service.Factory.run")
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET edge_kind='calls',evidence='Service.Factory.run()' WHERE id=?`, edge); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `INSERT INTO references_tbl(repo_id,file_id,ref_kind,name,qualified_name,start_line,start_col,end_line,end_col,context_symbol_id) VALUES(?,?,'call','run','Service.Factory.run',1,1,1,1,?)`, f.repoID, f.callerFile, f.caller); err != nil {
		t.Fatal(err)
	}
	if applies, err := f.store.jvmCompanionCallableABIRepairApplies(f.ctx, f.repoID); err != nil || !applies {
		t.Fatalf("repair gate=(%v,%v), want active companion evidence", applies, err)
	}
	for _, marker := range []string{typeScopeRepairSettingKey, bareNameLevelRepairSettingKey, dotTailAmbiguityRepairSettingKey, jvmScopePrecisionRepairSettingKey, jvmCoreInteropRepairSettingKey, jvmCommonCallableABIRepairSettingKey} {
		if err := f.store.markRepairDone(f.ctx, marker, f.repoID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID); err != nil {
		t.Fatal(err)
	}
	if got, ok := f.dstSymbolID(t, edge); !ok || got != f.target {
		t.Fatalf("repaired edge=(%d,%v), want target %d", got, ok, f.target)
	}
	var reference sql.NullInt64
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT symbol_id FROM references_tbl WHERE repo_id=?`, f.repoID).Scan(&reference); err != nil {
		t.Fatal(err)
	}
	if !reference.Valid || reference.Int64 != f.target {
		t.Fatalf("repaired reference=%v, want target %d", reference, f.target)
	}
	if ran, err := f.store.runResolverRepairOnce(f.ctx, f.repoID, jvmCompanionCallableABIRepair); err != nil || ran {
		t.Fatalf("second repair=(%v,%v), want no-op", ran, err)
	}
}

func TestJVMCompanionRepairGateSkipsUnsupportedEvidence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(t *testing.T, f *companionScopeFixture)
	}{
		{"argument-bearing call", func(t *testing.T, f *companionScopeFixture) {
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET edge_kind='calls',evidence='Service.Factory.run(1)' WHERE repo_id=?`, f.repoID); err != nil {
				t.Fatal(err)
			}
		}},
		{"renamed method", func(t *testing.T, f *companionScopeFixture) {
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET signature='@JvmName("go") fun run() {}' WHERE id=?`, f.target); err != nil {
				t.Fatal(err)
			}
		}},
		{"private companion", func(t *testing.T, f *companionScopeFixture) {
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET visibility='private' WHERE qualified_name='lib.Service.Factory'`); err != nil {
				t.Fatal(err)
			}
		}},
		{"ordinary object", func(t *testing.T, f *companionScopeFixture) {
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET kind='object' WHERE qualified_name='lib.Service.Factory'`); err != nil {
				t.Fatal(err)
			}
		}},
		{"deleted caller", func(t *testing.T, f *companionScopeFixture) {
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE files SET is_deleted=1 WHERE id=?`, f.callerFile); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCompanionScopeFixture(t, "Factory", "fun run() {}", "public", "public")
			edge := f.edge(t, f.callerFile, f.caller, "Service.Factory.run")
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET edge_kind='calls',evidence='Service.Factory.run()' WHERE id=?`, edge); err != nil {
				t.Fatal(err)
			}
			tc.mutate(t, f)
			if applies, err := f.store.jvmCompanionCallableABIRepairApplies(f.ctx, f.repoID); err != nil || applies {
				t.Fatalf("repair gate=(%v,%v), want false", applies, err)
			}
		})
	}
}
