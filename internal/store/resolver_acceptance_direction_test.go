package store

import (
	"database/sql"
	"fmt"
	"slices"
	"testing"
)

// edgeSides reads an edge's persisted source and destination qualified names;
// dst is "" while the edge is unresolved.
func (f *parityFixture) edgeSides(t *testing.T, edge int64) (src, dst string, srcID, dstID int64) {
	t.Helper()
	var d sql.NullString
	var did sql.NullInt64
	if err := f.store.db.QueryRowContext(f.ctx, `
		SELECT s.qualified_name, s.id, d.qualified_name, d.id
		FROM edges e JOIN symbols s ON s.id = e.src_symbol_id LEFT JOIN symbols d ON d.id = e.dst_symbol_id
		WHERE e.id = ?`, edge).Scan(&src, &srcID, &d, &did); err != nil {
		t.Fatal(err)
	}
	return src, d.String, srcID, did.Int64
}

func (f *parityFixture) neighbours(t *testing.T, id int64, callers bool) []string {
	t.Helper()
	find := f.store.FindCallees
	if callers {
		find = f.store.FindCallers
	}
	syms, err := find(f.ctx, f.repoID, "", id, 200, 0)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, s := range syms {
		out = append(out, s.QualifiedName)
	}
	return out
}

// addCallReference records the syntax fact the parser writes for an edge's
// call site, so reference reconciliation has something to derive from.
func (f *parityFixture) addCallReference(t *testing.T, edge int64) int64 {
	t.Helper()
	res, err := f.store.db.ExecContext(f.ctx, `
		INSERT INTO references_tbl(repo_id,file_id,ref_kind,name,qualified_name,start_line,start_col,end_line,end_col)
		SELECT repo_id,file_id,'call',dst_name,'',line,1,line,1 FROM edges WHERE id = ?`, edge)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	return id
}

func (f *parityFixture) referenceIdentity(t *testing.T, ref int64) (symbol, context int64) {
	t.Helper()
	var s, c sql.NullInt64
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT symbol_id, context_symbol_id FROM references_tbl WHERE id = ?`, ref).Scan(&s, &c); err != nil {
		t.Fatal(err)
	}
	return s.Int64, c.Int64
}

// Every fact-parity scenario is also checked from the query side: a binding is
// visible as callee of its caller and caller of its target, a refusal is
// visible from neither side, and the call-site reference carries the edge's
// destination and context -- or nothing for a refusal. Reference identities
// are derived by the full resolve and by the paths and paths+names
// entrypoints (the indexer calls only the latter); the names-only entrypoint is not used by the indexer and does
// not reconcile references, so it is asserted for direction only.
func TestResolverGateRuleBindingsHaveDirectionAndReferenceIdentity(t *testing.T) {
	for _, sc := range allFactParityScenarios() {
		t.Run(sc.rule+"/"+sc.name, func(t *testing.T) {
			for _, entry := range []string{"full", "paths", "names", "paths+names"} {
				f := newParityFixture(t, "module example.com/project\n")
				probes := sc.seed(t, f)
				refs := make([]int64, len(probes))
				for i, p := range probes {
					// Production edges are 'calls'; the fixture helper writes 'call'.
					f.exec(t, `UPDATE edges SET edge_kind = 'calls' WHERE id = ?`, p.edge)
					refs[i] = f.addCallReference(t, p.edge)
				}
				f.resolveVia(t, entry, sc.paths, sc.names)
				for i, p := range probes {
					if got := f.binding(t, p.edge); got != p.want {
						t.Errorf("%s: probe %d bound %q, want %q", entry, i, got, p.want)
					}
				}
				for i, p := range probes {
					src, dst, srcID, dstID := f.edgeSides(t, p.edge)
					callees := f.neighbours(t, srcID, false)
					if dst != "" {
						if !slices.Contains(callees, dst) {
							t.Errorf("%s: probe %d bound %s but FindCallees(%s) = %v", entry, i, dst, src, callees)
						}
						if callers := f.neighbours(t, dstID, true); !slices.Contains(callers, src) {
							t.Errorf("%s: probe %d bound %s but FindCallers(%s) = %v", entry, i, dst, dst, callers)
						}
					}
					// The caller's callees are exactly the destinations its probes
					// bound: a refused probe contributes none.
					var want []string
					for _, q := range probes {
						if qs, qd, _, _ := f.edgeSides(t, q.edge); qs == src && qd != "" {
							want = append(want, qd)
						}
					}
					if !slices.Equal(slices.Sorted(slices.Values(callees)), slices.Sorted(slices.Values(want))) {
						t.Errorf("%s: probe %d: FindCallees(%s) = %v, want exactly %v", entry, i, src, callees, want)
					}
					if entry == "names" {
						continue
					}
					refSym, refCtx := f.referenceIdentity(t, refs[i])
					if refSym != dstID || refCtx != srcID {
						t.Errorf("%s: probe %d reference = (symbol %d, context %d), edge = (dst %d, src %d)", entry, i, refSym, refCtx, dstID, srcID)
					}
				}
			}
		})
	}
}

// A mixed batch: one repository holding a block per language rule, each with
// its own names so the blocks do not interfere. The blocks are created in every
// rotation (and with every two-candidate block declared in both orders), which
// shifts every row id; each incremental entrypoint, with all names and paths
// at once and with one name at a time, must give every edge the binding a
// fresh resolve gives it, and that binding is pinned.
type batchBlock struct {
	name  string
	paths []string
	names []string
	seed  func(t *testing.T, f *parityFixture, rev bool) []factProbe
}

func twoDecls(t *testing.T, rev bool, a, b func()) {
	t.Helper()
	if rev {
		a, b = b, a
	}
	a()
	b()
}

var batchBlocks = []batchBlock{
	{
		name: "python import claim", paths: []string{"py1/main.py"}, names: []string{"claimed"},
		seed: func(t *testing.T, f *parityFixture, rev bool) []factProbe {
			lib := f.file(t, "lib1.py", "python")
			f.symbol(t, lib, "claimed", "lib1.claimed", "function", "python")
			other := f.file(t, "py1/other.py", "python")
			f.symbol(t, other, "claimed", "other1.claimed", "function", "python")
			main := f.file(t, "py1/main.py", "python")
			caller := f.symbol(t, main, "run", "main1.run", "function", "python")
			f.namedImport(t, main, "python", "lib1", "claimed")
			return []factProbe{{f.edge(t, main, caller, "claimed"), "lib1.claimed|python_import_scope|high"}}
		},
	},
	{
		name: "python ambiguity", paths: []string{"py2/main.py"}, names: []string{"twin"},
		seed: func(t *testing.T, f *parityFixture, rev bool) []factProbe {
			a := f.file(t, "py2/a.py", "python")
			b := f.file(t, "py2/b.py", "python")
			twoDecls(t, rev,
				func() { f.symbol(t, a, "twin", "a2.twin", "function", "python") },
				func() { f.symbol(t, b, "twin", "b2.twin", "function", "python") })
			main := f.file(t, "py2/main.py", "python")
			caller := f.symbol(t, main, "run", "main2.run", "function", "python")
			return []factProbe{{f.edge(t, main, caller, "twin"), "<unresolved>"}}
		},
	},
	{
		name: "python test shadow", paths: []string{"py3/main.py", "py3/test_main.py"}, names: []string{"shadowed"},
		seed: func(t *testing.T, f *parityFixture, rev bool) []factProbe {
			lib := f.file(t, "py3/util.py", "python")
			tests := f.file(t, "py3/test_util.py", "python")
			twoDecls(t, rev,
				func() { f.symbol(t, lib, "shadowed", "util3.shadowed", "function", "python") },
				func() { f.symbol(t, tests, "shadowed", "test_util3.shadowed", "function", "python") })
			main := f.file(t, "py3/main.py", "python")
			caller := f.symbol(t, main, "run", "main3.run", "function", "python")
			testMain := f.file(t, "py3/test_main.py", "python")
			testCaller := f.symbol(t, testMain, "test_run", "test_main3.test_run", "function", "python")
			return []factProbe{
				{f.edge(t, main, caller, "shadowed"), "util3.shadowed|exact_name|high"},
				{f.edge(t, testMain, testCaller, "shadowed"), "<unresolved>"},
			}
		},
	},
	{
		name: "typescript import", paths: []string{"ts1/main.ts"}, names: []string{"tsHelper"},
		seed: func(t *testing.T, f *parityFixture, rev bool) []factProbe {
			lib := f.file(t, "ts1/util.ts", "typescript")
			h := f.symbol(t, lib, "tsHelper", "ts1/util.tsHelper", "function", "typescript")
			f.exec(t, `UPDATE symbols SET visibility='public' WHERE id=?`, h)
			stray := f.file(t, "ts1/stray.ts", "typescript")
			f.symbol(t, stray, "tsStray", "ts1/stray.tsStray", "function", "typescript")
			main := f.file(t, "ts1/main.ts", "typescript")
			caller := f.symbol(t, main, "run", "main.run", "function", "typescript")
			f.namedImport(t, main, "typescript", "./util", "tsHelper")
			return []factProbe{
				{f.edge(t, main, caller, "tsHelper"), "ts1/util.tsHelper|typescript_module_scope|high"},
				{f.edge(t, main, caller, "tsStray"), "<unresolved>"},
			}
		},
	},
	{
		name: "go own module", paths: []string{"cmd/main.go"}, names: []string{"OpenBatch"},
		seed: func(t *testing.T, f *parityFixture, rev bool) []factProbe {
			pkg := f.file(t, "pkgb/open.go", "go")
			third := f.file(t, "third/pkgb/open.go", "go")
			twoDecls(t, rev,
				func() { f.symbolIn(t, pkg, "OpenBatch", "pkgb.OpenBatch", "function", "pkgb", "go") },
				func() { f.symbolIn(t, third, "OpenBatch", "pkgb.OpenBatch", "function", "pkgb", "go") })
			main := f.file(t, "cmd/main.go", "go")
			caller := f.symbol(t, main, "main", "main", "function", "go")
			f.imports(t, main, "example.com/project/pkgb")
			return []factProbe{{f.edge(t, main, caller, "example.com/project/pkgb.OpenBatch"), "pkgb.OpenBatch|module_import|high"}}
		},
	},
	{
		name: "java same class", paths: []string{"app/Main.java"}, names: []string{"javaHelper"},
		seed: func(t *testing.T, f *parityFixture, rev bool) []factProbe {
			main := f.file(t, "app/Main.java", "java")
			f.symbolIn(t, main, "javaHelper", "app.Main.javaHelper", "function", "Main", "java")
			caller := f.symbolIn(t, main, "run", "app.Main.run", "function", "Main", "java")
			f.exec(t, `INSERT INTO file_scope_evidence(repo_id,file_id,language,package_name) VALUES(?,?,?,?)`, f.repoID, main, "java", "app")
			return []factProbe{{f.edge(t, main, caller, "javaHelper"), "app.Main.javaHelper|java_package_scope|high"}}
		},
	},
	{
		name: "csharp unproven call", paths: []string{"App/Main.cs"}, names: []string{"CsHelper"},
		seed: func(t *testing.T, f *parityFixture, rev bool) []factProbe {
			lib := f.file(t, "App/Util.cs", "csharp")
			f.symbol(t, lib, "CsHelper", "App.Util.CsHelper", "method", "csharp")
			main := f.file(t, "App/Main.cs", "csharp")
			caller := f.symbol(t, main, "Run", "App.Main.Run", "method", "csharp")
			return []factProbe{{f.edge(t, main, caller, "CsHelper"), "<unresolved>"}}
		},
	},
}

func rotate[T any](s []T, k int) []T {
	k %= len(s)
	return append(slices.Clone(s[k:]), s[:k]...)
}

func TestResolverGateMixedBatchIsIndependentOfInsertionOrder(t *testing.T) {
	type located struct {
		probe factProbe
		key   string
	}
	for rot := 0; rot < len(batchBlocks); rot++ {
		for _, rev := range []bool{false, true} {
			var paths, names []string
			for _, b := range batchBlocks {
				paths = append(paths, b.paths...)
				names = append(names, b.names...)
			}
			// Each case is one resolve variant over an identically seeded store.
			variants := []struct {
				name string
				run  func(t *testing.T, f *parityFixture)
			}{
				{"full", func(t *testing.T, f *parityFixture) { f.resolveVia(t, "full", nil, nil) }},
				{"paths+names", func(t *testing.T, f *parityFixture) { f.resolveVia(t, "paths+names", paths, names) }},
				{"names", func(t *testing.T, f *parityFixture) { f.resolveVia(t, "names", nil, names) }},
				{"paths", func(t *testing.T, f *parityFixture) { f.resolveVia(t, "paths", paths, nil) }},
				{"one block at a time", func(t *testing.T, f *parityFixture) {
					for _, b := range batchBlocks {
						f.resolveVia(t, "paths+names", b.paths, b.names)
					}
				}},
			}
			for _, v := range variants {
				t.Run(fmt.Sprintf("rot%d/rev=%v/%s", rot, rev, v.name), func(t *testing.T) {
					f := newParityFixture(t, "module example.com/project\n")
					var all []located
					for _, b := range rotate(batchBlocks, rot) {
						for i, p := range b.seed(t, f, rev) {
							all = append(all, located{p, fmt.Sprintf("%s#%d", b.name, i)})
						}
					}
					v.run(t, f)
					for _, l := range all {
						if got := f.binding(t, l.probe.edge); got != l.probe.want {
							t.Errorf("%s bound %q, want %q", l.key, got, l.probe.want)
						}
					}
				})
			}
		}
	}
}

// More gate-level lifecycle transitions: visibility, declaration rename and
// move, local-binding evidence, caller path rename (production <-> test),
// package evidence and unit-level declaration removal.
var moreLifecycleCases = []lifecycleCase{
	{
		rule: "typescript_scope_ownership", paths: []string{"app/main.ts"}, names: []string{"helper"}, declChange: true, declPaths: []string{"app/util.ts"},
		want: map[string]string{"public": "app/util.helper|typescript_module_scope|high", "private": "<unresolved>"},
		seed: func(t *testing.T, f *parityFixture) (int64, func(string)) {
			lib := f.file(t, "app/util.ts", "typescript")
			helper := f.symbol(t, lib, "helper", "app/util.helper", "function", "typescript")
			main := f.file(t, "app/main.ts", "typescript")
			caller := f.symbol(t, main, "run", "main.run", "function", "typescript")
			f.namedImport(t, main, "typescript", "./util", "helper")
			edge := f.edge(t, main, caller, "helper")
			return edge, func(state string) {
				f.exec(t, `UPDATE symbols SET visibility=? WHERE id=?`, state, helper)
			}
		},
	},
	{
		// The imported module's declaration is renamed away and back.
		rule: "python_scope_claims", paths: []string{"app/main.py"}, names: []string{"helper"}, declChange: true, declPaths: []string{"lib.py"},
		want: map[string]string{"declared": "lib.helper|python_import_scope|high", "renamed": "<unresolved>"},
		seed: func(t *testing.T, f *parityFixture) (int64, func(string)) {
			lib := f.file(t, "lib.py", "python")
			helper := f.symbol(t, lib, "helper", "lib.helper", "function", "python")
			main := f.file(t, "app/main.py", "python")
			caller := f.symbol(t, main, "run", "main.run", "function", "python")
			f.namedImport(t, main, "python", "lib", "helper")
			edge := f.edge(t, main, caller, "helper")
			return edge, func(state string) {
				name := "helper"
				if state == "renamed" {
					name = "helper2"
				}
				qualified := "lib." + name
				f.exec(t, `UPDATE symbols SET name=?, qualified_name=?, stable_key=?, qualified_suffix=?, dot_tail2=?, dot_tail3=? WHERE id=?`,
					name, qualified, qualified+"|python", qualifiedSuffix(qualified), dotTail2(qualified), dotTail3(qualified), helper)
			}
		},
	},
	{
		// The imported package's declaration moves to another directory.
		rule: "own_module_import", paths: []string{"cmd/main.go"}, names: []string{"Open"}, declChange: true, declPaths: []string{"pkg/open.go", "elsewhere/open.go"},
		want: map[string]string{"in-package": "pkg.Open|module_import|high", "moved": "<unresolved>"},
		seed: func(t *testing.T, f *parityFixture) (int64, func(string)) {
			decl := f.file(t, "pkg/open.go", "go")
			f.symbolIn(t, decl, "Open", "pkg.Open", "function", "pkg", "go")
			main := f.file(t, "cmd/main.go", "go")
			caller := f.symbol(t, main, "main", "main", "function", "go")
			f.imports(t, main, "example.com/project/pkg")
			edge := f.edge(t, main, caller, "example.com/project/pkg.Open")
			return edge, func(state string) {
				path := "pkg/open.go"
				if state == "moved" {
					path = "elsewhere/open.go"
				}
				f.exec(t, `UPDATE files SET path=? WHERE id=?`, path, decl)
			}
		},
	},
	{
		rule: "go_local_qualifier", paths: []string{"cmd/main.go"}, names: []string{"Get"},
		want: map[string]string{"no local": "store.Get|exact_qualified|high", "local shadows": "<unresolved>"},
		seed: func(t *testing.T, f *parityFixture) (int64, func(string)) {
			decl := f.file(t, "store/store.go", "go")
			f.symbolIn(t, decl, "Get", "store.Get", "function", "store", "go")
			main := f.file(t, "cmd/main.go", "go")
			caller := f.symbol(t, main, "main", "main", "function", "go")
			edge := f.edge(t, main, caller, "store.Get")
			return edge, func(state string) {
				f.exec(t, `DELETE FROM go_local_binding_evidence WHERE file_id=?`, main)
				if state == "local shadows" {
					f.exec(t, `INSERT INTO go_local_binding_evidence(repo_id,file_id,name,scope_start_line,scope_end_line) VALUES(?,?,?,?,?)`, f.repoID, main, "store", 1, 9)
				}
			}
		},
	},
	{
		// Renaming the caller's file moves it between production and test.
		rule: "caller_kind_candidate", paths: []string{"app/main.py", "app/test_main.py"}, names: []string{"helper"},
		want: map[string]string{"production": "<unresolved>", "test": "test_util.helper|exact_name|high"},
		seed: func(t *testing.T, f *parityFixture) (int64, func(string)) {
			tests := f.file(t, "app/test_util.py", "python")
			f.symbol(t, tests, "helper", "test_util.helper", "function", "python")
			main := f.file(t, "app/main.py", "python")
			caller := f.symbol(t, main, "run", "main.run", "function", "python")
			edge := f.edge(t, main, caller, "helper")
			return edge, func(state string) {
				path := "app/main.py"
				if state == "test" {
					path = "app/test_main.py"
				}
				f.exec(t, `UPDATE files SET path=? WHERE id=?`, path, main)
			}
		},
	},
	{
		// The caller's own type loses and regains the method it calls.
		rule: "csharp_scope_ownership", paths: []string{"App/Main.cs"}, names: []string{"Helper"},
		want: map[string]string{"declared": "App.Main.Helper|csharp_same_type|high", "removed": "<unresolved>"},
		seed: func(t *testing.T, f *parityFixture) (int64, func(string)) {
			main := f.file(t, "App/Main.cs", "csharp")
			caller := f.symbolIn(t, main, "Run", "App.Main.Run", "function", "Main", "csharp")
			f.exec(t, `UPDATE symbols SET visibility='public', is_static=0 WHERE id=?`, caller)
			edge := f.edge(t, main, caller, "Helper")
			return edge, func(state string) {
				f.exec(t, `DELETE FROM symbols WHERE file_id=? AND name='Helper'`, main)
				if state == "declared" {
					helper := f.symbolIn(t, main, "Helper", "App.Main.Helper", "function", "Main", "csharp")
					f.exec(t, `UPDATE symbols SET visibility='public', is_static=0 WHERE id=?`, helper)
				}
			}
		},
	},
	{
		rule: "bare_type_scope", paths: []string{"app/main.py"}, names: []string{"Widget"},
		want: map[string]string{"imported": "widgets.Widget|python_import_scope|high", "absent": "<unresolved>"},
		seed: func(t *testing.T, f *parityFixture) (int64, func(string)) {
			lib := f.file(t, "widgets.py", "python")
			f.symbol(t, lib, "Widget", "widgets.Widget", "class", "python")
			main := f.file(t, "app/main.py", "python")
			caller := f.symbol(t, main, "run", "main.run", "function", "python")
			edge := f.edge(t, main, caller, "Widget")
			return edge, func(state string) {
				f.exec(t, `DELETE FROM scope_import_evidence WHERE file_id=?`, main)
				if state == "imported" {
					f.namedImport(t, main, "python", "widgets", "Widget")
				}
			}
		},
	},
	{
		// The caller file's language is detected later: unknown refuses, then
		// the candidate's language binds, then another language refuses.
		rule: "language_gate", paths: []string{"app/main.x"}, names: []string{"helper"},
		want: map[string]string{"unknown": "<unresolved>", "python": "util.helper|exact_name|high", "go": "<unresolved>"},
		seed: func(t *testing.T, f *parityFixture) (int64, func(string)) {
			lib := f.file(t, "app/util.py", "python")
			f.symbol(t, lib, "helper", "util.helper", "function", "python")
			main := f.file(t, "app/main.x", "")
			caller := f.symbol(t, main, "run", "main.run", "function", "")
			edge := f.edge(t, main, caller, "helper")
			return edge, func(state string) {
				language := state
				if state == "unknown" {
					language = ""
				}
				f.exec(t, `UPDATE files SET language=? WHERE id=?`, language, main)
				f.exec(t, `UPDATE symbols SET language=? WHERE id=?`, language, caller)
			}
		},
	},
}

func TestResolverGateRuleFactLifecycleConvergenceMore(t *testing.T) {
	checkLifecycleConvergence(t, moreLifecycleCases)
}
