package store

import (
	"path"
	"slices"

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

func TestTypeScriptJSSpecifierFreshPrecedenceWinner(t *testing.T) {
	f := jsSpecifierFixture{newParityFixture(t, "")}
	f.module(t, "foo.ts", "foo")
	stranded := f.caller(t, "caller.ts", "./foo.js", "foo")
	f.module(t, "peer.js", "peer")
	f.module(t, "peer.ts", "peer")
	shadowed := f.caller(t, "peer-caller.ts", "./peer.js", "peer")
	f.module(t, "view.jsx", "view")
	jsx := f.caller(t, "view-caller.js", "./view.js", "view")
	f.module(t, "widget.jsx", "widget")
	f.module(t, "widget.tsx", "widget")
	widget := f.caller(t, "widget-caller.jsx", "./widget.jsx", "widget")
	f.module(t, "plain.js", "plain")
	plain := f.caller(t, "plain-caller.js", "./plain.js", "plain")

	if _, err := f.store.ResolveEdges(f.ctx, f.repoID); err != nil {
		t.Fatal(err)
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
			t.Fatalf("edge %d after fresh resolution = %q, want %q", tc.edge, got, tc.want)
		}
	}
	for edge, want := range map[int64]string{shadowed: "peer.ts", widget: "widget.tsx"} {
		var target string
		if err := f.store.db.QueryRowContext(f.ctx, `SELECT f.path FROM edges e JOIN symbols s ON s.id=e.dst_symbol_id JOIN files f ON f.id=s.file_id WHERE e.id=?`, edge).Scan(&target); err != nil || target != want {
			t.Fatalf("edge %d target = (%q,%v), want %s", edge, target, err, want)
		}
	}
}
