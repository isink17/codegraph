//go:build cgo

package indexer

import (
	"path"
	"strconv"
	"strings"
	"testing"
)

// TypeScript and JavaScript share one module graph (language `typescript`).
// TypeScript source names a sibling by its runtime spelling: a relative
// `./foo.js` is looked up as `foo.ts`, `foo.tsx`, `foo.js`, `foo.jsx` in that
// order, and `./view.jsx` (the `jsx: preserve` output of `.tsx`) as
// `view.tsx`, `view.jsx`; the first active file wins. `.d.ts` is never an
// implementation target. Every other explicit spelling names itself alone.

const tsInteropStrategy = "typescript_module_scope"

func TestTypeScriptJSSpecifierInteropResolvesSourcePeer(t *testing.T) {
	for _, tc := range []struct {
		name         string
		files        tree
		caller, edge string
		target       string
		targetQName  string
	}{
		{"TS named import of .js spelling to TS",
			tree{"foo.ts": "export function foo() {}\n", "caller.ts": "import { foo } from \"./foo.js\";\nexport function run() { foo(); }\n"},
			"caller.ts", "foo", "foo.ts:foo.foo", "foo.foo"},
		{"TS default import of .js spelling to TS",
			tree{"foo.ts": "export default function foo() {}\n", "caller.ts": "import f from \"./foo.js\";\nexport function run() { f(); }\n"},
			"caller.ts", "f", "foo.ts:foo.foo", "foo.foo"},
		{"TS namespace member of .js spelling to TS",
			tree{"foo.ts": "export function bar() {}\n", "caller.ts": "import * as ns from \"./foo.js\";\nexport function run() { ns.bar(); }\n"},
			"caller.ts", "ns.bar", "foo.ts:foo.bar", "foo.bar"},
		{"TS .js spelling to TSX",
			tree{"view.tsx": "export function view() {}\n", "caller.ts": "import { view } from \"./view.js\";\nexport function run() { view(); }\n"},
			"caller.ts", "view", "view.tsx:view.view", "view.view"},
		{"TS parent-directory .js spelling to TS",
			tree{"foo.ts": "export function foo() {}\n", "lib/caller.ts": "import { foo } from \"../foo.js\";\nexport function run() { foo(); }\n"},
			"lib/caller.ts", "foo", "foo.ts:foo.foo", "foo.foo"},
		{"TS re-export chain through .js spellings",
			tree{"foo.ts": "export function foo() {}\n", "index.ts": "export { foo } from \"./foo.js\";\n", "caller.ts": "import { foo } from \"./index.js\";\nexport function run() { foo(); }\n"},
			"caller.ts", "foo", "foo.ts:foo.foo", "foo.foo"},
		{"JS .js spelling to TS source",
			tree{"foo.ts": "export function foo() {}\n", "caller.js": "import { foo } from \"./foo.js\";\nexport function run() { foo(); }\n"},
			"caller.js", "foo", "foo.ts:foo.foo", "foo.foo"},
		{"JSX .js spelling to JSX source",
			tree{"view.jsx": "export function view() {}\n", "caller.js": "import { view } from \"./view.js\";\nexport function run() { view(); }\n"},
			"caller.js", "view", "view.jsx:view.view", "view.view"},
		{"JSX .jsx spelling to TSX source",
			tree{"view.tsx": "export function view() {}\n", "caller.jsx": "import { view } from \"./view.jsx\";\nexport function run() { view(); }\n"},
			"caller.jsx", "view", "view.tsx:view.view", "view.view"},
		{"JSX .jsx spelling to JSX source",
			tree{"view.jsx": "export function view() {}\n", "caller.jsx": "import { view } from \"./view.jsx\";\nexport function run() { view(); }\n"},
			"caller.jsx", "view", "view.jsx:view.view", "view.view"},
		{"TSX over JSX via .jsx spelling",
			tree{"view.jsx": "export function view() {}\n", "view.tsx": "export function view() {}\n", "caller.jsx": "import { view } from \"./view.jsx\";\nexport function run() { view(); }\n"},
			"caller.jsx", "view", "view.tsx:view.view", "view.view"},
		{"JS extensionless import of TS source",
			tree{"foo.ts": "export function foo() {}\n", "caller.js": "import { foo } from \"./foo\";\nexport function run() { foo(); }\n"},
			"caller.js", "foo", "foo.ts:foo.foo", "foo.foo"},
		{"TS .js spelling to JS source",
			tree{"foo.js": "export function foo() {}\n", "caller.ts": "import { foo } from \"./foo.js\";\nexport function run() { foo(); }\n"},
			"caller.ts", "foo", "foo.js:foo.foo", "foo.foo"},
		{"JS .js spelling to JS source",
			tree{"foo.js": "export function foo() {}\n", "caller.js": "import { foo } from \"./foo.js\";\nexport function run() { foo(); }\n"},
			"caller.js", "foo", "foo.js:foo.foo", "foo.foo"},
		{"TS extensionless import of TS source",
			tree{"foo.ts": "export function foo() {}\n", "caller.ts": "import { foo } from \"./foo\";\nexport function run() { foo(); }\n"},
			"caller.ts", "foo", "foo.ts:foo.foo", "foo.foo"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newLifecycleRepo(t, tc.files)
			callerQName := strings.TrimSuffix(tc.caller, path.Ext(tc.caller)) + ".run"
			assertJVMResolved(t, r, tc.caller, tc.edge, tc.target+"(function)", tsInteropStrategy)
			assertJVMReference(t, r, tc.caller, tc.edge, true)
			assertJVMQueryRelation(t, r, callerQName, tc.targetQName)
			r.assertFreshParity(t, tc.name)
		})
	}
}

// Coexisting candidates are not ambiguous: the first in lookup order wins.
func TestTypeScriptJSSpecifierInteropPrecedence(t *testing.T) {
	const foo = "export function foo() {}\n"
	const importFoo = "import { foo } from \"./foo.js\";\nexport function run() { foo(); }\n"
	for _, tc := range []struct {
		name   string
		files  tree
		caller string
		target string
	}{
		{"TS over JS", tree{"foo.ts": foo, "foo.js": foo, "caller.ts": importFoo}, "caller.ts", "foo.ts"},
		{"TS over JS from JS caller", tree{"foo.ts": foo, "foo.js": foo, "caller.js": importFoo}, "caller.js", "foo.ts"},
		{"TS over TSX", tree{"foo.ts": foo, "foo.tsx": foo, "caller.ts": importFoo}, "caller.ts", "foo.ts"},
		{"TSX over JS", tree{"foo.tsx": foo, "foo.js": foo, "caller.ts": importFoo}, "caller.ts", "foo.tsx"},
		{"TSX over JSX", tree{"foo.tsx": foo, "foo.jsx": foo, "caller.ts": importFoo}, "caller.ts", "foo.tsx"},
		{"JS over JSX", tree{"foo.js": foo, "foo.jsx": foo, "caller.ts": importFoo}, "caller.ts", "foo.js"},
		{"JS only", tree{"foo.js": foo, "caller.ts": importFoo}, "caller.ts", "foo.js"},
		{"JSX only", tree{"foo.jsx": foo, "caller.ts": importFoo}, "caller.ts", "foo.jsx"},
		{"declaration file does not shadow JS", tree{"foo.d.ts": "export declare function foo(): void;\n", "foo.js": foo, "caller.ts": importFoo}, "caller.ts", "foo.js"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newLifecycleRepo(t, tc.files)
			callerQName := strings.TrimSuffix(tc.caller, path.Ext(tc.caller)) + ".run"
			assertJVMResolved(t, r, tc.caller, "foo", tc.target+":foo.foo(function)", tsInteropStrategy)
			assertJVMReference(t, r, tc.caller, "foo", true)
			assertJVMQueryRelation(t, r, callerQName, "foo.foo")
			r.assertFreshParity(t, tc.name)
		})
	}
}

func TestTypeScriptJSSpecifierInteropRefusesUnsupportedSpelling(t *testing.T) {
	const foo = "export function foo() {}\n"
	importFoo := func(spec string) string {
		return "import { foo } from \"" + spec + "\";\nexport function run() { foo(); }\n"
	}
	for _, tc := range []struct {
		name  string
		files tree
	}{
		{"winning TS module lacks the name", tree{"foo.ts": "export function unrelated() {}\n", "foo.js": foo, "caller.ts": importFoo("./foo.js")}},
		{"declaration file is not a target", tree{"foo.d.ts": "export declare function foo(): void;\n", "caller.ts": importFoo("./foo.js")}},
		{".jsx spelling does not name a .ts source", tree{"foo.ts": foo, "caller.ts": importFoo("./foo.jsx")}},
		{".mjs spelling is not substituted", tree{"foo.ts": foo, "caller.ts": importFoo("./foo.mjs")}},
		{".cjs spelling is not substituted", tree{"foo.ts": foo, "caller.ts": importFoo("./foo.cjs")}},
		{"upper-case .JS spelling is not substituted", tree{"foo.ts": foo, "caller.ts": importFoo("./foo.JS")}},
		{"extensionless import does not name JS source", tree{"foo.js": foo, "caller.ts": importFoo("./foo")}},
		{"competing re-exported symbols", tree{"a.ts": foo, "b.ts": foo, "index.ts": "export * from \"./a.js\";\nexport * from \"./b.js\";\n", "caller.ts": importFoo("./index.js")}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newLifecycleRepo(t, tc.files)
			caller := "caller.ts"
			if _, ok := tc.files["caller.js"]; ok {
				caller = "caller.js"
			}
			assertJVMUnresolved(t, r, caller, "foo")
			assertJVMReference(t, r, caller, "foo", false)
			for _, target := range []string{"foo.foo", "a/foo.foo", "b/foo.foo", "x/foo.foo", "a.foo", "b.foo"} {
				assertTSNoResolvedRelation(t, r, "caller.run", target)
			}
			r.assertFreshParity(t, tc.name)
		})
	}
}

func TestTypeScriptJSSpecifierInteropLifecycle(t *testing.T) {
	const foo = "export function foo() {}\n"
	callers := []string{"caller.ts", "legacy.js"}
	r := newLifecycleRepo(t, tree{
		"foo.ts":    foo,
		"caller.ts": "import { foo } from \"./foo.js\";\nexport function run() { foo(); }\n",
		"legacy.js": "import { foo } from \"./foo.js\";\nexport function run() { foo(); }\n",
		"other.ts":  "import { foo } from \"./other-dep\";\nexport function run() { foo(); }\n",
	})
	resolved := func(step, target string) {
		t.Helper()
		for _, c := range callers {
			assertJVMResolved(t, r, c, "foo", target+":foo.foo(function)", tsInteropStrategy)
			assertJVMReference(t, r, c, "foo", true)
			assertJVMQueryRelation(t, r, strings.TrimSuffix(c, path.Ext(c))+".run", "foo.foo")
		}
		r.assertFreshParity(t, step)
	}
	unresolved := func(step string) {
		t.Helper()
		for _, c := range callers {
			assertJVMUnresolved(t, r, c, "foo")
			assertJVMReference(t, r, c, "foo", false)
			assertTSNoResolvedRelation(t, r, strings.TrimSuffix(c, path.Ext(c))+".run", "foo.foo")
		}
		r.assertFreshParity(t, step)
	}

	resolved("initial", "foo.ts")

	r.remove(t, "foo.ts")
	r.update(t, "foo.ts")
	unresolved("target deleted")

	r.write(t, "foo.ts", foo)
	r.update(t, "foo.ts")
	resolved("target restored", "foo.ts")

	// A declaration file is never a candidate, so it cannot displace the winner.
	r.write(t, "foo.d.ts", "export declare function foo(): void;\n")
	r.update(t, "foo.d.ts")
	resolved("declaration file added", "foo.ts")
	r.remove(t, "foo.ts")
	r.update(t, "foo.ts")
	unresolved("TS winner deleted beside declaration file")
	r.write(t, "foo.ts", foo)
	r.update(t, "foo.ts")
	resolved("TS winner restored beside declaration file", "foo.ts")
	r.remove(t, "foo.d.ts")
	r.update(t, "foo.d.ts")
	resolved("declaration file deleted", "foo.ts")

	// Lower-priority candidates appearing never displace the winner.
	for _, p := range []string{"foo.js", "foo.jsx", "foo.tsx"} {
		r.write(t, p, foo)
		r.update(t, p)
		resolved("lower "+p+" added", "foo.ts")
	}

	// Deleting each winner redirects to the next candidate in order.
	r.remove(t, "foo.ts")
	r.update(t, "foo.ts")
	resolved("TS winner deleted", "foo.tsx")
	r.remove(t, "foo.tsx")
	r.update(t, "foo.tsx")
	resolved("TSX winner deleted", "foo.js")
	r.remove(t, "foo.js")
	r.update(t, "foo.js")
	resolved("JS winner deleted", "foo.jsx")

	// Restoring a higher-priority candidate redirects back to it.
	r.write(t, "foo.js", foo)
	r.update(t, "foo.js")
	resolved("JS restored over JSX", "foo.js")
	// A TSX competitor without the name reaches the caller only through the
	// `.js` path invalidation, not a changed name.
	r.write(t, "foo.tsx", "export function unrelated() {}\n")
	r.update(t, "foo.tsx")
	unresolved("TSX without the name added above JS")
	r.remove(t, "foo.tsx")
	r.update(t, "foo.tsx")
	resolved("TSX without the name deleted above JS", "foo.js")
	r.write(t, "foo.ts", foo)
	r.update(t, "foo.ts")
	resolved("TS restored over JS", "foo.ts")

	// Lower-priority candidates disappearing leave the winner in place.
	r.remove(t, "foo.jsx")
	r.remove(t, "foo.js")
	r.update(t, "foo.jsx", "foo.js")
	resolved("lower candidates deleted", "foo.ts")

	// The winner owns module identity even when it lacks the name.
	r.write(t, "foo.js", foo)
	r.write(t, "foo.ts", "export function unrelated() {}\n")
	r.update(t, "foo.js", "foo.ts")
	unresolved("TS winner without the name beside JS")

	r.remove(t, "foo.ts")
	r.update(t, "foo.ts")
	resolved("TS winner without the name deleted", "foo.js")

	r.remove(t, "foo.js")
	r.write(t, "foo.ts", foo)
	r.update(t, "foo.js", "foo.ts")
	resolved("renamed back to TS", "foo.ts")

	// An unrelated caller's refusal is untouched by every step above.
	assertJVMUnresolved(t, r, "other.ts", "foo")
}

// An upgraded database carries decisions an older resolver made: the `.js`
// spelling left unresolved, `./peer.js` bound to `peer.js` beside a `peer.ts`
// that now wins, and `./widget.jsx` bound to `widget.jsx` beside a
// `widget.tsx` that now wins. One ordinary update converges every edge and its
// reference to a fresh index.
func TestTypeScriptJSSpecifierInteropUpgradeRepair(t *testing.T) {
	const foo = "export function foo() {}\n"
	r := newLifecycleRepo(t, tree{
		"foo.ts":            foo,
		"caller.ts":         "import { foo } from \"./foo.js\";\nexport function run() { foo(); }\n",
		"peer.js":           foo,
		"peer.ts":           foo,
		"peer-caller.ts":    "import { foo } from \"./peer.js\";\nexport function run() { foo(); }\n",
		"widget.jsx":        foo,
		"widget.tsx":        foo,
		"widget-caller.jsx": "import { foo } from \"./widget.jsx\";\nexport function run() { foo(); }\n",
		"unrelated.ts":      "export function unrelated() {}\n",
	})
	db := r.raw(t)
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := db.ExecContext(r.ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	edgeIn := `SELECT e.id FROM edges e JOIN files f ON f.id=e.file_id WHERE f.path=? AND e.dst_name='foo'`
	symbolIn := `SELECT s.id FROM symbols s JOIN files f ON f.id=s.file_id WHERE f.path=? AND s.name='foo'`
	exec(`UPDATE edges SET dst_symbol_id=NULL,resolution_strategy='',resolution_confidence='' WHERE id=(`+edgeIn+`)`, "caller.ts")
	exec(`UPDATE references_tbl SET symbol_id=NULL WHERE file_id=(SELECT id FROM files WHERE path='caller.ts') AND qualified_name='foo'`)
	for caller, old := range map[string]string{"peer-caller.ts": "peer.js", "widget-caller.jsx": "widget.jsx"} {
		exec(`UPDATE edges SET dst_symbol_id=(`+symbolIn+`),resolution_strategy='`+tsInteropStrategy+`',resolution_confidence='high' WHERE id=(`+edgeIn+`)`, old, caller)
		exec(`UPDATE references_tbl SET symbol_id=(`+symbolIn+`) WHERE file_id=(SELECT id FROM files WHERE path=?) AND qualified_name='foo'`, old, caller)
	}
	exec(`DELETE FROM settings WHERE key=?`, "resolver.typescript_js_specifier_repaired.v1."+strconv.FormatInt(r.repoID, 10))
	assertJVMUnresolved(t, r, "caller.ts", "foo")
	assertJVMResolved(t, r, "peer-caller.ts", "foo", "peer.js:peer.foo(function)", tsInteropStrategy)
	assertJVMResolved(t, r, "widget-caller.jsx", "foo", "widget.jsx:widget.foo(function)", tsInteropStrategy)

	r.write(t, "unrelated.ts", "export function unrelated() { return 1; }\n")
	r.update(t, "unrelated.ts")
	assertJVMResolved(t, r, "caller.ts", "foo", "foo.ts:foo.foo(function)", tsInteropStrategy)
	assertJVMReference(t, r, "caller.ts", "foo", true)
	assertJVMResolved(t, r, "peer-caller.ts", "foo", "peer.ts:peer.foo(function)", tsInteropStrategy)
	assertJVMReference(t, r, "peer-caller.ts", "foo", true)
	assertJVMResolved(t, r, "widget-caller.jsx", "foo", "widget.tsx:widget.foo(function)", tsInteropStrategy)
	assertJVMReference(t, r, "widget-caller.jsx", "foo", true)
	r.assertFreshParity(t, "upgrade repair")
}

// assertTSNoResolvedRelation checks the resolved query surfaces only. With the
// target absent, FindCallers deliberately answers unresolved spelling hints;
// the Result form separates those from resolved callers.
func assertTSNoResolvedRelation(t *testing.T, r *lifecycleRepo, caller, target string) {
	t.Helper()
	callees, err := r.store.FindCalleesResult(r.ctx, r.repoID, caller, 0, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if hasSymbolQName(callees.Callees, target) {
		t.Fatalf("FindCallees(%s) resolved %s: %#v", caller, target, callees.Callees)
	}
	callers, err := r.store.FindCallersResult(r.ctx, r.repoID, target, 0, 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if hasSymbolQName(callers.Callers, caller) {
		t.Fatalf("FindCallers(%s) resolved %s: %#v", target, caller, callers.Callers)
	}
}

// The scoped frontier loads only the caller's module component, so a same
// basename elsewhere is neither a candidate nor a competitor on update.
func TestTypeScriptJSSpecifierInteropScopedUpdateSelectsImportedDirectory(t *testing.T) {
	const foo = "export function foo() {}\n"
	r := newLifecycleRepo(t, tree{"a/foo.ts": foo, "b/foo.ts": foo, "caller.ts": "import { foo } from \"./a/foo.js\";\nexport function run() { foo(); }\n"})
	assertJVMResolved(t, r, "caller.ts", "foo", "a/foo.ts:a/foo.foo(function)", tsInteropStrategy)
	r.write(t, "caller.ts", "import { foo } from \"./b/foo.js\";\nexport function run() { foo(); }\n")
	r.update(t, "caller.ts")
	assertJVMResolved(t, r, "caller.ts", "foo", "b/foo.ts:b/foo.foo(function)", tsInteropStrategy)
	assertJVMReference(t, r, "caller.ts", "foo", true)
	assertJVMQueryRelation(t, r, "caller.run", "b/foo.foo")
	assertTSNoResolvedRelation(t, r, "caller.run", "a/foo.foo")
	r.assertFreshParity(t, "import redirected")
	r.write(t, "caller.ts", "export function run() { foo(); }\n")
	r.update(t, "caller.ts")
	assertJVMUnresolved(t, r, "caller.ts", "foo")
	assertJVMReference(t, r, "caller.ts", "foo", false)
	r.assertFreshParity(t, "import removed")
}

// Invalidation walks dependents through runtime spellings at every level: a
// barrel re-exporting `./foo.js` and a caller importing the barrel as
// `./index.js`, including after the barrel itself becomes `.tsx` and when a
// higher-priority barrel candidate appears.
func TestTypeScriptJSSpecifierInteropReExportChainLifecycle(t *testing.T) {
	const foo = "export function foo() {}\n"
	const barrel = "export { foo } from \"./foo.js\";\n"
	r := newLifecycleRepo(t, tree{"foo.ts": foo, "index.ts": barrel, "caller.ts": "import { foo } from \"./index.js\";\nexport function run() { foo(); }\n"})
	resolved := func(step string) {
		t.Helper()
		assertJVMResolved(t, r, "caller.ts", "foo", "foo.ts:foo.foo(function)", tsInteropStrategy)
		assertJVMReference(t, r, "caller.ts", "foo", true)
		assertJVMQueryRelation(t, r, "caller.run", "foo.foo")
		r.assertFreshParity(t, step)
	}
	unresolved := func(step string) {
		t.Helper()
		assertJVMUnresolved(t, r, "caller.ts", "foo")
		assertJVMReference(t, r, "caller.ts", "foo", false)
		assertTSNoResolvedRelation(t, r, "caller.run", "foo.foo")
		r.assertFreshParity(t, step)
	}
	resolved("initial")
	r.write(t, "foo.js", "export function unrelated() {}\n")
	r.update(t, "foo.js")
	resolved("lower leaf candidate added")
	r.remove(t, "foo.js")
	r.update(t, "foo.js")
	resolved("lower leaf candidate deleted")
	r.remove(t, "index.ts")
	r.write(t, "index.tsx", barrel)
	r.update(t, "index.ts", "index.tsx")
	resolved("barrel renamed to TSX")
	r.write(t, "index.js", "export const unrelated = 1;\n")
	r.update(t, "index.js")
	resolved("lower barrel candidate added")
	r.remove(t, "index.js")
	r.update(t, "index.js")
	resolved("lower barrel candidate deleted")
	r.write(t, "index.ts", "export const unrelated = 1;\n")
	r.update(t, "index.ts")
	unresolved("higher barrel candidate without the re-export added")
	r.remove(t, "index.ts")
	r.update(t, "index.ts")
	resolved("higher barrel candidate deleted")
	r.remove(t, "foo.ts")
	r.update(t, "foo.ts")
	unresolved("leaf deleted")
}

// `./view.jsx` names `view.tsx` before `view.jsx`; `view.ts` never answers it.
func TestTypeScriptJSSpecifierInteropJSXLifecycle(t *testing.T) {
	const view = "export function view() {}\n"
	r := newLifecycleRepo(t, tree{
		"view.jsx":   view,
		"caller.jsx": "import { view } from \"./view.jsx\";\nexport function run() { view(); }\n",
		"other.ts":   "export function other() {}\n",
	})
	resolved := func(step, target string) {
		t.Helper()
		assertJVMResolved(t, r, "caller.jsx", "view", target+":view.view(function)", tsInteropStrategy)
		assertJVMReference(t, r, "caller.jsx", "view", true)
		assertJVMQueryRelation(t, r, "caller.run", "view.view")
		r.assertFreshParity(t, step)
	}
	unresolved := func(step string) {
		t.Helper()
		assertJVMUnresolved(t, r, "caller.jsx", "view")
		assertJVMReference(t, r, "caller.jsx", "view", false)
		assertTSNoResolvedRelation(t, r, "caller.run", "view.view")
		r.assertFreshParity(t, step)
	}
	resolved("initial", "view.jsx")

	// The competitor declares no `view`, so only the `.jsx` path invalidation
	// (not a changed name) can reach the caller.
	r.write(t, "view.tsx", "export function other() {}\n")
	r.update(t, "view.tsx")
	unresolved("TSX without the name added")
	r.remove(t, "view.tsx")
	r.update(t, "view.tsx")
	resolved("TSX without the name deleted", "view.jsx")

	r.write(t, "view.tsx", view)
	r.update(t, "view.tsx")
	resolved("TSX added redirects", "view.tsx")

	// Unrelated files and non-candidates leave the winner in place.
	r.write(t, "other.ts", "export function other() { return 1; }\n")
	r.update(t, "other.ts")
	resolved("unrelated file changed", "view.tsx")
	for _, p := range []string{"view.ts", "view.js"} {
		r.write(t, p, view)
		r.update(t, p)
		resolved("non-candidate "+p+" added", "view.tsx")
	}

	r.remove(t, "view.tsx")
	r.update(t, "view.tsx")
	resolved("TSX deleted falls back", "view.jsx")

	r.write(t, "view.tsx", view)
	r.update(t, "view.tsx")
	resolved("TSX restored redirects back", "view.tsx")

	r.remove(t, "view.jsx")
	r.update(t, "view.jsx")
	resolved("lower JSX deleted", "view.tsx")

	// The winner owns module identity even when it lacks the name.
	r.write(t, "view.jsx", view)
	r.write(t, "view.tsx", "export function other() {}\n")
	r.update(t, "view.jsx", "view.tsx")
	unresolved("TSX winner without the name beside JSX")

	r.remove(t, "view.tsx")
	r.remove(t, "view.jsx")
	r.update(t, "view.tsx", "view.jsx")
	unresolved("TS and JS only")
}
