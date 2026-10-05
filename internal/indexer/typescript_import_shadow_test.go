//go:build cgo

package indexer

import (
	"database/sql"
	"slices"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/store"
)

const tsShadowImport = "import { k } from \"./a\";\n"

// tsImportShadowCases pin which binding answers a call named like an import
// when a scope between the call and the module binds that name too. Every JS
// expressible case was checked against node v25.8.0 (oracle cases.mjs): a ""
// target means the runtime callee is not the import, so the call must stay
// unresolved. ts marks cases that need TypeScript-only syntax.
var tsImportShadowCases = []struct {
	name, body, call, target string
	ts                       bool
}{
	{"const local", "export function c(){ const k = () => 1; k(); }", "", "", false},
	{"let local", "export function c(){ let k = () => 1; k(); }", "", "", false},
	{"var local", "export function c(){ var k = () => 1; k(); }", "", "", false},
	{"parameter", "export function c(k){ k(); }", "", "", false},
	{"typed parameter", "export function c(k: () => number){ k(); }", "", "", true},
	{"optional parameter", "export function c(k?: () => number){ if (k) k(); }", "", "", true},
	{"arrow parameter", "export const c = (k) => k();", "", "", false},
	{"arrow bare parameter", "export const c = k => k();", "", "", false},
	{"method parameter", "export class C { m(k){ k(); } }", "", "", false},
	{"local function", "export function c(){ function k(){} k(); }", "", "", false},
	{"local class", "export function c(){ class k {} k(); }", "", "", false},
	{"block let encloses", "export function c(){ if (1) { let k = () => 1; k(); } }", "", "", false},
	{"block const not enclosing", "export function c(){ { const k = () => 1; } k(); }", "", "a.ts:a.k(", false},
	{"block class not enclosing", "export function c(){ { class k {} } k(); }", "", "a.ts:a.k(", false},
	{"var hoisted from block", "export function c(){ { var k = () => 1; } k(); }", "", "", false},
	{"var declared after call", "export function c(){ k(); var k = () => 1; }", "", "", false},
	{"const after call (TDZ)", "export function c(){ k(); const k = () => 1; }", "", "", false},
	{"nested function outer local", "export function c(){ const k = () => 1; function inner(){ k(); } }", "", "", false},
	{"nested function sibling local", "function f1(){ const k = () => 1; return k; }\nexport function f2(){ k(); }", "", "a.ts:a.k(", false},
	{"inner function var does not leak", "export function c(){ function inner(){ var k = 1; } k(); }", "", "a.ts:a.k(", false},
	{"destructured parameter", "export function c({ k }){ k(); }", "", "", false},
	{"renamed destructured local", "export function c(){ const { x: k } = { x: () => 1 }; k(); }", "", "", false},
	{"array destructured local", "export function c(){ const [k] = [() => 1]; k(); }", "", "", false},
	{"default parameter value uses import", "export function c(x = k()){ return x; }", "", "a.ts:a.k(", false},
	{"catch parameter", "export function c(){ try {} catch (k) { k(); } }", "", "", false},
	{"for of binding", "export function c(){ for (const k of []) k(); }", "", "", false},
	{"named function expression", "export const c = function k(){ k(); };", "", "", false},
	{"module redeclaration", "function k(){}\nexport function c(){ k(); }", "", "", false},
	{"switch case const", "export function c(x){ switch (x) { case 1: const k = () => 1; k(); } }", "", "", false},
	{"switch case let in earlier case", "export function c(x){ switch (x) { case 1: let k = () => 1; break; default: k(); } }", "", "", false},
	{"switch case class", "export function c(x){ switch (x) { default: class k {} k(); } }", "", "", false},
	{"namespace head parameter", "export function c(ns){ ns.k(); }", "ns.k", "", false},
	{"namespace head local", "export function c(){ const ns = { k(){} }; ns.k(); }", "ns.k", "", false},
	// -- controls: nothing shadows, the import answers
	{"unshadowed", "export function c(){ k(); }", "", "a.ts:a.k(", false},
	{"other name local", "export function c(){ const z = () => 1; k(); }", "", "a.ts:a.k(", false},
	{"namespace unshadowed", "export function c(){ ns.k(); }", "ns.k", "a.ts:a.k(", false},
	{"shadow of another parameter", "export function c(z: number){ k(); }", "", "a.ts:a.k(", true},
	{"type parameter is not a value", "export function c<k>(){ k(); }", "", "a.ts:a.k(", true},
}

func TestTypeScriptImportShadowing(t *testing.T) {
	for _, ext := range []string{".ts", ".js"} {
		for _, tc := range tsImportShadowCases {
			if tc.ts && ext == ".js" {
				continue
			}
			call := tc.call
			if call == "" {
				call = "k"
			}
			head := tsShadowImport + "import * as ns from \"./a\";\n"
			if ext == ".js" {
				head = strings.ReplaceAll(head, "\"./a\"", "\"./a.js\"")
			}
			r := newLifecycleRepo(t, tree{"a.ts": "export function k(){}\n", "b" + ext: head + tc.body + "\n"})
			got := r.edgeState(t, "b"+ext, call)
			if tc.target == "" {
				if !strings.Contains(got, ":: [/") {
					t.Errorf("%s %s: want unresolved, got %s", ext, tc.name, got)
				}
			} else if !strings.Contains(got, tc.target) {
				t.Errorf("%s %s: want %s, got %s", ext, tc.name, tc.target, got)
			}
		}
	}
}

// Type-only and re-export rows bind no value here, so they never answer a
// call, shadowed or not.
func TestTypeScriptImportShadowingTypeOnlyAndReExportControls(t *testing.T) {
	for name, b := range map[string]string{
		"type-only":           "import type { k } from \"./a\";\nexport function c(){ k(); }\n",
		"type-only, shadowed": "import type { k } from \"./a\";\nexport function c(k){ k(); }\n",
		"re-export":           "export { k } from \"./a\";\nexport function c(){ k(); }\n",
		"re-export, shadowed": "export { k } from \"./a\";\nexport function c(){ const k = () => 1; k(); }\n",
	} {
		r := newLifecycleRepo(t, tree{"a.ts": "export function k(){}\n", "b.ts": b})
		if got := r.edgeState(t, "b.ts", "k"); !strings.Contains(got, ":: [/") {
			t.Errorf("%s: want unresolved, got %s", name, got)
		}
	}
}

// Adding and removing the shadow flips the call through both update shapes;
// every step matches a fresh index and a repeated update changes nothing.
func TestTypeScriptImportShadowingFollowsIncrementalChanges(t *testing.T) {
	steps := []struct{ name, b, target string }{
		{"unshadowed", tsShadowImport + "export function c(){ k(); }\n", "a.ts:a.k("},
		{"local added", tsShadowImport + "export function c(){ const k = () => 1; k(); }\n", ""},
		{"local removed", tsShadowImport + "export function c(){ k(); }\n", "a.ts:a.k("},
		{"parameter added", tsShadowImport + "export function c(k){ k(); }\n", ""},
		{"parameter removed", tsShadowImport + "export function c(){ k(); }\n", "a.ts:a.k("},
	}
	for _, scoped := range []bool{true, false} {
		r := newLifecycleRepo(t, tree{"a.ts": "export function k(){}\n", "b.ts": steps[0].b})
		for _, step := range steps {
			r.write(t, "b.ts", step.b)
			if scoped {
				r.update(t, "b.ts")
			} else {
				r.update(t)
			}
			r.assertFreshParity(t, step.name)
			got := r.edgeState(t, "b.ts", "k")
			if (step.target == "" && !strings.Contains(got, ":: [/")) || (step.target != "" && !strings.Contains(got, step.target)) {
				t.Fatalf("scoped=%v %s: want %q, got %s", scoped, step.name, step.target, got)
			}
			before := r.projection(t)
			if again := r.update(t); again.FilesIndexed != 0 {
				t.Fatalf("scoped=%v %s: repeated update = %+v", scoped, step.name, again)
			}
			if after := r.projection(t); !slices.Equal(after, before) {
				t.Fatalf("scoped=%v %s: repeated update mutated the graph", scoped, step.name)
			}
		}
	}
}

// TestTypeScriptImportShadowingProfileUpgradeConverges rebuilds what a v1
// index left behind (no shadow evidence, the shadowed calls and their
// references bound to the import) and requires one ordinary update to clear
// both, match a fresh index, and a second update to change nothing.
func TestTypeScriptImportShadowingProfileUpgradeConverges(t *testing.T) {
	r := newLifecycleRepo(t, tree{
		"a.ts": "export function k(){}\n",
		"b.ts": tsShadowImport + "import * as ns from \"./a\";\nexport function c(){ const k = () => 1; k(); }\nexport function c2(k){ k(); }\nexport function c3(ns){ ns.k(); }\nexport function c4(){ k(); }\n",
	})
	db, err := sql.Open(store.SQLiteDriverName(), r.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("UPDATE files SET parser_profile='treesitter:typescript:v1' WHERE repo_id=? AND language='typescript'", r.repoID); err != nil {
		t.Fatal(err)
	}
	// What v1 persisted: the evidence was the callee name, and every call
	// bound to the import with its reference identity.
	if _, err := db.Exec("UPDATE edges SET evidence=dst_name WHERE repo_id=?", r.repoID); err != nil {
		t.Fatal(err)
	}
	ak := r.symbolID(t, "a.k")
	if _, err := db.Exec(`UPDATE edges SET dst_symbol_id=?, resolution_strategy='typescript_module_scope', resolution_confidence='high' WHERE repo_id=? AND dst_name IN ('k','ns.k')`, ak, r.repoID); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE references_tbl SET symbol_id=? WHERE repo_id=? AND ref_kind='call' AND name IN ('k','ns.k')`, ak, r.repoID); err != nil {
		t.Fatal(err)
	}
	if got := r.refTarget(t, "b.ts", "ns.k"); got != "a.k" {
		t.Fatalf("simulated v1 reference = %q", got)
	}
	summary := r.update(t)
	if len(summary.ParserProfileLanguages) != 1 || summary.ParserProfileLanguages[0] != "typescript" {
		t.Fatalf("profile update = %+v", summary)
	}
	if got := r.refTarget(t, "b.ts", "ns.k"); got != "" {
		t.Fatalf("shadowed ns.k reference = %q, want none", got)
	}
	ak = r.symbolID(t, "a.k")
	var bound, refs int
	if err := db.QueryRow(`SELECT COUNT(*) FROM edges WHERE repo_id=? AND dst_symbol_id=?`, r.repoID, ak).Scan(&bound); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM references_tbl WHERE repo_id=? AND symbol_id=?`, r.repoID, ak).Scan(&refs); err != nil {
		t.Fatal(err)
	}
	if bound != 1 || refs != 1 {
		t.Fatalf("after upgrade: %d edges and %d references bound to a.k, want 1 and 1 (the unshadowed call)", bound, refs)
	}
	r.assertFreshParity(t, "TypeScript v1 to v2 profile upgrade")
	before := r.projection(t)
	if again := r.update(t); again.FilesIndexed != 0 || len(again.ParserProfileLanguages) != 0 {
		t.Fatalf("second update = %+v", again)
	}
	if !slices.Equal(r.projection(t), before) {
		t.Fatal("second update mutated the graph")
	}
}
