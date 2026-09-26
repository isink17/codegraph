package store

import (
	"path"
	"slices"
	"strconv"
	"strings"
	"testing"
)

func TestTypeScriptExplicitModuleCandidatesAreOrdered(t *testing.T) {
	for _, tt := range []struct {
		in         string
		candidates []string
		spellings  []string
	}{
		{"src/foo.js", []string{"src/foo.ts", "src/foo.tsx", "src/foo.js", "src/foo.jsx"}, nil},
		{"src/foo.jsx", []string{"src/foo.tsx", "src/foo.jsx"}, []string{"src/foo.js"}},
		{"src/foo.ts", []string{"src/foo.ts"}, []string{"src/foo.js"}},
		{"src/foo.tsx", []string{"src/foo.tsx"}, []string{"src/foo.js", "src/foo.jsx"}},
		// A declaration file is never an implementation candidate.
		{"src/foo.d.js", []string{"src/foo.d.tsx", "src/foo.d.js", "src/foo.d.jsx"}, nil},
		{"src/foo.d.ts", []string{"src/foo.d.ts"}, nil},
		{"src/foo.mjs", []string{"src/foo.mjs"}, nil},
		{"src/foo.cjs", []string{"src/foo.cjs"}, nil},
		{"src/foo.JS", []string{"src/foo.JS"}, nil},
		{"src/foo.json", []string{"src/foo.json"}, nil},
	} {
		if got := typescriptExplicitModuleCandidates(tt.in); !slices.Equal(got, tt.candidates) {
			t.Errorf("typescriptExplicitModuleCandidates(%q) = %v, want %v", tt.in, got, tt.candidates)
		}
		if got := typescriptRuntimeSpellings(tt.in); !slices.Equal(got, tt.spellings) {
			t.Errorf("typescriptRuntimeSpellings(%q) = %v, want %v", tt.in, got, tt.spellings)
		}
	}
}

// jsSpecifierFixture declares public TypeScript-family modules and callers
// importing them through persisted scope evidence, as the parser writes them.
type jsSpecifierFixture struct{ *parityFixture }

func (f jsSpecifierFixture) module(t *testing.T, p, name string) (int64, int64) {
	t.Helper()
	file := f.file(t, p, "typescript")
	sym := f.symbol(t, file, name, strings.TrimSuffix(p, path.Ext(p))+"."+name, "function", "typescript")
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET visibility='public' WHERE id=?`, sym); err != nil {
		t.Fatal(err)
	}
	return file, sym
}

func (f jsSpecifierFixture) caller(t *testing.T, path, spec, name string) int64 {
	t.Helper()
	file := f.file(t, path, "typescript")
	src := f.symbol(t, file, "run", "caller.run", "function", "typescript")
	if _, err := f.store.db.ExecContext(f.ctx, `INSERT INTO scope_import_evidence(repo_id,file_id,language,source_specifier,imported_name,local_name,import_kind) VALUES(?,?,'typescript',?,?,?,'named')`, f.repoID, file, spec, name, name); err != nil {
		t.Fatal(err)
	}
	return f.edge(t, file, src, name)
}

func TestTypeScriptJSSpecifierRepairAppliesOnlyWhereTheRuleCanDecide(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build func(t *testing.T, f jsSpecifierFixture)
		want  bool
	}{
		{"pure JavaScript with .js specifiers", func(t *testing.T, f jsSpecifierFixture) {
			f.module(t, "foo.js", "foo")
			f.caller(t, "caller.js", "./foo.js", "foo")
		}, false},
		{"pure TypeScript without runtime specifiers", func(t *testing.T, f jsSpecifierFixture) {
			f.module(t, "foo.ts", "foo")
			f.caller(t, "caller.ts", "./foo", "foo")
		}, false},
		{".mjs spelling is never substituted", func(t *testing.T, f jsSpecifierFixture) {
			f.module(t, "foo.ts", "foo")
			f.caller(t, "caller.ts", "./foo.mjs", "foo")
		}, false},
		{"upper-case .JS spelling is never substituted", func(t *testing.T, f jsSpecifierFixture) {
			f.module(t, "foo.ts", "foo")
			f.caller(t, "caller.ts", "./foo.JS", "foo")
		}, false},
		{"bare package spelled .js", func(t *testing.T, f jsSpecifierFixture) {
			f.caller(t, "caller.ts", "chart.js", "Chart")
		}, false},
		{"deleted TypeScript file does not make the repo mixed", func(t *testing.T, f jsSpecifierFixture) {
			f.module(t, "foo.js", "foo")
			f.caller(t, "caller.js", "./foo.js", "foo")
			old, _ := f.module(t, "old.ts", "old")
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE files SET is_deleted=1 WHERE id=?`, old); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"deleted importer does not make the repo mixed", func(t *testing.T, f jsSpecifierFixture) {
			f.module(t, "foo.ts", "foo")
			f.caller(t, "caller.ts", "./foo.js", "foo")
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE files SET is_deleted=1 WHERE path='caller.ts'`); err != nil {
				t.Fatal(err)
			}
		}, false},
		{"JavaScript with declaration files only", func(t *testing.T, f jsSpecifierFixture) {
			f.module(t, "foo.js", "foo")
			f.file(t, "foo.d.ts", "typescript")
			f.caller(t, "caller.js", "./foo.js", "foo")
		}, false},
		{"JSX spelling with a TS source only", func(t *testing.T, f jsSpecifierFixture) {
			f.module(t, "view.ts", "View")
			f.caller(t, "caller.ts", "./view.jsx", "View")
		}, true},
		{"TSX source imported by .jsx spelling", func(t *testing.T, f jsSpecifierFixture) {
			f.module(t, "view.tsx", "View")
			f.caller(t, "caller.jsx", "./view.jsx", "View")
		}, true},
		{"TypeScript source imported by runtime spelling", func(t *testing.T, f jsSpecifierFixture) {
			f.module(t, "foo.ts", "foo")
			f.caller(t, "caller.ts", "./foo.js", "foo")
		}, true},
		{"TSX source imported by runtime spelling", func(t *testing.T, f jsSpecifierFixture) {
			f.module(t, "view.tsx", "View")
			f.caller(t, "lib/caller.js", "../view.js", "View")
		}, true},
		{"JSX source imported by runtime spelling", func(t *testing.T, f jsSpecifierFixture) {
			f.module(t, "view.jsx", "View")
			f.caller(t, "caller.js", "./view.js", "View")
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := jsSpecifierFixture{newParityFixture(t, "")}
			tc.build(t, f)
			got, err := f.store.typescriptJSSpecifierRepairApplies(f.ctx, f.repoID)
			if err != nil || got != tc.want {
				t.Fatalf("applies = (%v,%v), want %v", got, err, tc.want)
			}
		})
	}
}

func TestTypeScriptJSSpecifierRepairRebindsToPrecedenceWinner(t *testing.T) {
	f := jsSpecifierFixture{newParityFixture(t, "")}
	f.module(t, "foo.ts", "foo")
	stranded := f.caller(t, "caller.ts", "./foo.js", "foo")
	_, legacy := f.module(t, "peer.js", "peer")
	f.module(t, "peer.ts", "peer")
	shadowed := f.caller(t, "peer-caller.ts", "./peer.js", "peer")
	f.setBinding(t, shadowed, legacy, ResolutionStrategyTypeScriptModuleScope, ResolutionConfidenceHigh)
	f.module(t, "view.jsx", "view")
	jsx := f.caller(t, "view-caller.js", "./view.js", "view")
	_, oldWidget := f.module(t, "widget.jsx", "widget")
	f.module(t, "widget.tsx", "widget")
	widget := f.caller(t, "widget-caller.jsx", "./widget.jsx", "widget")
	f.setBinding(t, widget, oldWidget, ResolutionStrategyTypeScriptModuleScope, ResolutionConfidenceHigh)
	_, js := f.module(t, "plain.js", "plain")
	plain := f.caller(t, "plain-caller.js", "./plain.js", "plain")
	f.setBinding(t, plain, js, ResolutionStrategyTypeScriptModuleScope, ResolutionConfidenceHigh)

	if ran, err := f.store.runResolverRepairOnce(f.ctx, f.repoID, typescriptJSSpecifierRepair); err != nil || !ran {
		t.Fatalf("repair = (%v,%v), want (true,nil)", ran, err)
	}
	for _, tc := range []struct {
		edge int64
		want string
	}{
		{stranded, "foo.foo|typescript_module_scope|high"},
		{shadowed, "peer.peer|typescript_module_scope|high"},
		{jsx, "view.view|typescript_module_scope|high"},
		{widget, "widget.widget|typescript_module_scope|high"},
		{plain, "plain.plain|typescript_module_scope|high"},
	} {
		if got := f.binding(t, tc.edge); got != tc.want {
			t.Fatalf("edge %d after repair = %q, want %q", tc.edge, got, tc.want)
		}
	}
	for edge, want := range map[int64]string{shadowed: "peer.ts", widget: "widget.tsx"} {
		var target string
		if err := f.store.db.QueryRowContext(f.ctx, `SELECT f.path FROM edges e JOIN symbols s ON s.id=e.dst_symbol_id JOIN files f ON f.id=s.file_id WHERE e.id=?`, edge).Scan(&target); err != nil || target != want {
			t.Fatalf("edge %d target = (%q,%v), want %s", edge, target, err, want)
		}
	}
	if ran, err := f.store.runResolverRepairOnce(f.ctx, f.repoID, typescriptJSSpecifierRepair); err != nil || ran {
		t.Fatalf("second repair = (%v,%v), want (false,nil)", ran, err)
	}
}

func TestTypeScriptJSSpecifierRepairMarksWithoutWorkWhereRuleCannotDecide(t *testing.T) {
	f := jsSpecifierFixture{newParityFixture(t, "")}
	_, js := f.module(t, "plain.js", "plain")
	plain := f.caller(t, "plain-caller.js", "./plain.js", "plain")
	f.setBinding(t, plain, js, ResolutionStrategyTypeScriptModuleScope, ResolutionConfidenceHigh)
	if ran, err := f.store.runResolverRepairOnce(f.ctx, f.repoID, typescriptJSSpecifierRepair); err != nil || ran {
		t.Fatalf("repair = (%v,%v), want (false,nil)", ran, err)
	}
	var marked bool
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT EXISTS(SELECT 1 FROM settings WHERE key=?)`, typescriptJSSpecifierRepairSettingKey+"."+strconv.FormatInt(f.repoID, 10)).Scan(&marked); err != nil || !marked {
		t.Fatalf("marker = (%v,%v), want set", marked, err)
	}
	if got := f.binding(t, plain); got != "plain.plain|typescript_module_scope|high" {
		t.Fatalf("binding after no-work repair = %q", got)
	}
}
