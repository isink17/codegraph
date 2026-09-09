package store

import (
	"database/sql"
	"fmt"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

// -- fixture extensions for P22.48 -------------------------------------------

// declare inserts a class or module declaration. Ruby's `class` and `module`
// keywords land on different symbol kinds, and a qname declared as both is the
// contradiction the resolver has to refuse.
func (f *rubyFixture) declare(t *testing.T, fileID int64, kind, qname string) int64 {
	return f.insert(t, fileID, kind, qname, sql.NullInt64{})
}

// singleton declares `def self.name` with an explicit definition-site
// visibility, which is what treesitter:ruby:v4 states for every singleton.
func (f *rubyFixture) singleton(t *testing.T, fileID int64, qname, visibility string) int64 {
	t.Helper()
	id := f.method(t, fileID, qname, true)
	if _, err := f.store.db.ExecContext(f.ctx,
		`UPDATE symbols SET visibility = ? WHERE id = ?`, visibility, id); err != nil {
		t.Fatalf("set visibility of %s: %v", qname, err)
	}
	return id
}

func (f *rubyFixture) scopeFact(t *testing.T, fileID int64, kind, owner, local, specifier string, static bool) {
	t.Helper()
	if _, err := f.store.db.ExecContext(f.ctx, `
		INSERT INTO scope_import_evidence(repo_id, file_id, language, source_specifier, imported_name,
			local_name, import_kind, wildcard, is_static, is_reexport, is_namespace_export, is_type_only, owner_module)
		VALUES(?, ?, 'ruby', ?, '', ?, ?, 0, ?, 0, 0, 0, ?)`,
		f.repoID, fileID, specifier, local, kind, static, owner); err != nil {
		t.Fatalf("insert %s fact for %s: %v", kind, owner, err)
	}
}

// lexical records the parser's `ruby_lexical_parent`: the constant nesting the
// declaration's own body sits in. An empty parent is the root boundary, not a
// missing fact.
func (f *rubyFixture) lexical(t *testing.T, fileID int64, owner, parent string) {
	f.scopeFact(t, fileID, graph.ScopeImportRubyLexicalParent, owner, "", parent, false)
}

func (f *rubyFixture) visibility(t *testing.T, fileID int64, owner, method, specifier string) {
	f.scopeFact(t, fileID, graph.ScopeImportRubySingletonVisibility, owner, method, specifier, true)
}

func (f *rubyFixture) visibilityHazard(t *testing.T, fileID int64, owner string) {
	f.scopeFact(t, fileID, graph.ScopeImportRubySingletonVisibilityUnknown, owner, "", "", true)
}

// nested declares `module A; class B` style containers and their lexical
// parents in one call, so a fixture reads like the source it stands for.
func (f *rubyFixture) nested(t *testing.T, fileID int64, kind, qname, parent string) int64 {
	id := f.declare(t, fileID, kind, qname)
	f.lexical(t, fileID, qname, parent)
	return id
}

// -- classifier ---------------------------------------------------------------

// Only a single unqualified constant token followed by one call operator is
// this phase's. Every other spelling asks a question P22.48 does not model, so
// it is rejected before any symbol is loaded.
func TestRubyConstantCallSpellings(t *testing.T) {
	for _, tc := range []struct {
		spelling, constant, method string
		ok                         bool
	}{
		{spelling: "Service.run", constant: "Service", method: "run", ok: true},
		{spelling: "Service::run", constant: "Service", method: "run", ok: true},
		{spelling: "Service2.run_now?", constant: "Service2", method: "run_now?", ok: true},
		{spelling: "Service.run!", constant: "Service", method: "run!", ok: true},
		{spelling: "A::Service.run"},
		{spelling: "A::B.run"},
		{spelling: "A::B::run"},
		{spelling: "::Service.run"},
		{spelling: "::A::B.run"},
		{spelling: "Service&.run"},
		{spelling: "service.run"},
		{spelling: "obj.run"},
		{spelling: "factory.service.run"},
		{spelling: "Service.build.run"},
		{spelling: "SERVICE::Nested.run"},
		{spelling: "Service"},
		{spelling: "Service."},
		{spelling: "Service::"},
		{spelling: "run"},
		{spelling: "self.run"},
		{spelling: "@ivar.run"},
	} {
		constant, method, ok := rubyConstantCall(tc.spelling)
		if ok != tc.ok || constant != tc.constant || method != tc.method {
			t.Errorf("rubyConstantCall(%q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.spelling, constant, method, ok, tc.constant, tc.method, tc.ok)
		}
	}
}

// The chain is the definition site's nesting, innermost first, read from one
// file's facts. Two distinct parents for one owner is a file that reopened the
// same constant under different nestings: neither spelling wins.
func TestRubyLexicalChainReconstruction(t *testing.T) {
	set := func(values ...string) map[string]struct{} {
		s := map[string]struct{}{}
		for _, v := range values {
			s[v] = struct{}{}
		}
		return s
	}
	nested := map[string]map[string]struct{}{
		"App":             set(""),
		"App.Feature":     set("App"),
		"App.Feature.Svc": set("App.Feature"),
	}
	if chain, ok := rubyLexicalChain(nested, "App.Feature.Svc"); !ok ||
		fmt.Sprint(chain) != "[App.Feature.Svc App.Feature App]" {
		t.Fatalf("nested chain = %v (%v)", chain, ok)
	}
	// `class App::Caller` pushes one frame and stops at the root.
	if chain, ok := rubyLexicalChain(map[string]map[string]struct{}{"App.Caller": set("")}, "App.Caller"); !ok ||
		fmt.Sprint(chain) != "[App.Caller]" {
		t.Fatalf("qualified-open chain = %v (%v)", chain, ok)
	}
	for name, tc := range map[string]struct {
		parents   map[string]map[string]struct{}
		container string
	}{
		"missing fact":     {map[string]map[string]struct{}{"App.Caller": set("App")}, "App.Caller"},
		"ambiguous parent": {map[string]map[string]struct{}{"App.B": set("App", "")}, "App.B"},
		"cycle":            {map[string]map[string]struct{}{"A": set("B"), "B": set("A")}, "A"},
		"no container":     {nested, ""},
	} {
		if chain, ok := rubyLexicalChain(tc.parents, tc.container); ok {
			t.Errorf("%s: chain = %v, want fail closed", name, chain)
		}
	}
}

// -- acceptance ---------------------------------------------------------------

// The acceptance fixture: both call operators reach the one public singleton
// method of the constant the caller's lexical scope owns, a private one is
// refused, and every spelling this phase does not model stays unresolved on
// every entry point.
func TestRubyLexicalConstantAcceptance(t *testing.T) {
	f := newRubyFixture(t)
	file := f.rb(t, "app/services.rb")
	f.nested(t, file, "type", "App", "")
	f.nested(t, file, "class", "App.PublicService", "App")
	public := f.singleton(t, file, "App.PublicService.run", "public")
	f.nested(t, file, "class", "App.PrivateService", "App")
	f.singleton(t, file, "App.PrivateService.run", "public")
	f.visibility(t, file, "App.PrivateService", "run", "private")
	f.nested(t, file, "class", "App.Caller", "App")
	caller := f.method(t, file, "App.Caller.f", false)

	bound := map[string]int64{
		"PublicService.run":  f.call(t, file, srcOf(caller), "PublicService.run", rubyConstantReceiver, 1),
		"PublicService::run": f.call(t, file, srcOf(caller), "PublicService::run", rubyConstantReceiver, 2),
	}
	nullEdges := map[string]int64{
		"private target":     f.call(t, file, srcOf(caller), "PrivateService.run", rubyConstantReceiver, 3),
		"missing constant":   f.call(t, file, srcOf(caller), "Missing.run", rubyConstantReceiver, 4),
		"multi segment":      f.call(t, file, srcOf(caller), "App::PublicService.run", rubyConstantReceiver, 5),
		"absolute":           f.call(t, file, srcOf(caller), "::App::PublicService.run", rubyConstantReceiver, 6),
		"multi segment cons": f.call(t, file, srcOf(caller), "App::PublicService::run", rubyConstantReceiver, 7),
	}
	names := []string{"run", "PublicService.run", "PublicService::run", "PrivateService.run",
		"Missing.run", "App::PublicService.run", "::App::PublicService.run", "App::PublicService::run"}
	want := "App.PublicService.run|" + ResolutionStrategyRubyLexicalConstant + "|" + ResolutionConfidenceHigh
	for _, entry := range rubyEntryPoints {
		f.clearAll(t)
		f.resolveVia(t, entry, []string{"app/services.rb"}, names)
		for name, edge := range bound {
			if got := f.binding(t, edge); got != want {
				t.Fatalf("%s: %s = %s, want %s", entry, name, got, want)
			}
			var dst int64
			if err := f.store.db.QueryRowContext(f.ctx, `SELECT dst_symbol_id FROM edges WHERE id = ?`, edge).Scan(&dst); err != nil {
				t.Fatal(err)
			}
			if dst != public {
				t.Fatalf("%s: %s bound symbol %d, want %d", entry, name, dst, public)
			}
		}
		for name, edge := range nullEdges {
			if got := f.binding(t, edge); got != "<unresolved>" {
				t.Fatalf("%s: %s = %s, want unresolved", entry, name, got)
			}
		}
	}
}

// rubyConstantCase is one fixture whose single constant-receiver call must end
// up bound to `wantQName`, or unresolved when it is empty.
type rubyConstantCase struct {
	name      string
	build     func(t *testing.T, f *rubyFixture) (edge int64, spelling string)
	wantQName string
}

func runRubyConstantCases(t *testing.T, cases []rubyConstantCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newRubyFixture(t)
			edge, spelling := tc.build(t, f)
			want := "<unresolved>"
			if tc.wantQName != "" {
				want = tc.wantQName + "|" + ResolutionStrategyRubyLexicalConstant + "|" + ResolutionConfidenceHigh
			}
			paths := f.rubyPaths(t)
			names := []string{spelling, "run", "build"}
			for _, entry := range rubyEntryPoints {
				f.clearAll(t)
				f.resolveVia(t, entry, paths, names)
				if got := f.binding(t, edge); got != want {
					t.Fatalf("%s: %s = %s, want %s", entry, spelling, got, want)
				}
			}
		})
	}
}

func (f *rubyFixture) rubyPaths(t *testing.T) []string {
	t.Helper()
	rows, err := f.store.db.QueryContext(f.ctx, `SELECT path FROM files WHERE repo_id = ? ORDER BY path`, f.repoID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var path string
		if err := rows.Scan(&path); err != nil {
			t.Fatal(err)
		}
		paths = append(paths, path)
	}
	return paths
}

// Stage 1 (which constant the receiver names) runs to completion before stage 2
// (which method that constant holds), and never backtracks into it.
func TestRubyLexicalConstantIdentity(t *testing.T) {
	runRubyConstantCases(t, []rubyConstantCase{
		{
			name:      "outer lexical match",
			wantQName: "App.Service.run",
			build: func(t *testing.T, f *rubyFixture) (int64, string) {
				file := f.rb(t, "app/a.rb")
				f.nested(t, file, "type", "App", "")
				f.nested(t, file, "class", "App.Service", "App")
				f.singleton(t, file, "App.Service.run", "public")
				f.nested(t, file, "class", "App.Caller", "App")
				caller := f.method(t, file, "App.Caller.f", false)
				return f.call(t, file, srcOf(caller), "Service.run", rubyConstantReceiver, 1), "Service.run"
			},
		},
		{
			name:      "deep nesting takes the nearest owner",
			wantQName: "App.Feature.Service.run",
			build: func(t *testing.T, f *rubyFixture) (int64, string) {
				file := f.rb(t, "app/a.rb")
				f.nested(t, file, "type", "App", "")
				f.nested(t, file, "class", "App.Service", "App")
				f.singleton(t, file, "App.Service.run", "public")
				f.nested(t, file, "type", "App.Feature", "App")
				f.nested(t, file, "class", "App.Feature.Service", "App.Feature")
				f.singleton(t, file, "App.Feature.Service.run", "public")
				f.nested(t, file, "class", "App.Feature.Caller", "App.Feature")
				caller := f.method(t, file, "App.Feature.Caller.f", false)
				return f.call(t, file, srcOf(caller), "Service.run", rubyConstantReceiver, 1), "Service.run"
			},
		},
		{
			// A `class << self` method's calls use the lexical scope of the
			// class body that encloses the eigenclass, not a scope of its own.
			name:      "eigenclass source keeps the container's scope",
			wantQName: "App.Service.run",
			build: func(t *testing.T, f *rubyFixture) (int64, string) {
				file := f.rb(t, "app/a.rb")
				f.nested(t, file, "type", "App", "")
				f.nested(t, file, "class", "App.Service", "App")
				f.singleton(t, file, "App.Service.run", "public")
				f.nested(t, file, "class", "App.Caller", "App")
				caller := f.method(t, file, "App.Caller.f", true)
				return f.call(t, file, srcOf(caller), "Service.run", rubyConstantReceiver, 1), "Service.run"
			},
		},
		{
			// The inner constant owns the name. It has no `run`, which is a
			// NoMethodError, not a licence to reach the outer `Service`.
			name: "shadowing inner constant with no target method",
			build: func(t *testing.T, f *rubyFixture) (int64, string) {
				file := f.rb(t, "app/a.rb")
				f.nested(t, file, "type", "App", "")
				f.nested(t, file, "class", "App.Service", "App")
				f.singleton(t, file, "App.Service.run", "public")
				f.nested(t, file, "class", "App.Caller", "App")
				f.nested(t, file, "class", "App.Caller.Service", "App.Caller")
				caller := f.method(t, file, "App.Caller.f", false)
				return f.call(t, file, srcOf(caller), "Service.run", rubyConstantReceiver, 1), "Service.run"
			},
		},
		{
			name:      "shadowing inner constant with the target method",
			wantQName: "App.Caller.Service.run",
			build: func(t *testing.T, f *rubyFixture) (int64, string) {
				file := f.rb(t, "app/a.rb")
				f.nested(t, file, "type", "App", "")
				f.nested(t, file, "class", "App.Service", "App")
				f.singleton(t, file, "App.Service.run", "public")
				f.nested(t, file, "class", "App.Caller", "App")
				f.nested(t, file, "class", "App.Caller.Service", "App.Caller")
				f.singleton(t, file, "App.Caller.Service.run", "public")
				caller := f.method(t, file, "App.Caller.f", false)
				return f.call(t, file, srcOf(caller), "Service.run", rubyConstantReceiver, 1), "Service.run"
			},
		},
		{
			// `class App::Caller` pushes one lexical frame. Deriving `App` from
			// the semantic container would invent a nesting Ruby never had.
			name: "qualified open does not inherit the semantic container",
			build: func(t *testing.T, f *rubyFixture) (int64, string) {
				file := f.rb(t, "app/a.rb")
				f.nested(t, file, "type", "App", "")
				f.nested(t, file, "class", "App.Service", "App")
				f.singleton(t, file, "App.Service.run", "public")
				f.nested(t, file, "class", "App.Caller", "")
				caller := f.method(t, file, "App.Caller.f", false)
				return f.call(t, file, srcOf(caller), "Service.run", rubyConstantReceiver, 1), "Service.run"
			},
		},
		{
			// One file reopening `App.Caller` both nested and qualified gives
			// its methods two different nestings. Neither row wins.
			name: "same file conflicting lexical parents",
			build: func(t *testing.T, f *rubyFixture) (int64, string) {
				file := f.rb(t, "app/a.rb")
				f.nested(t, file, "type", "App", "")
				f.nested(t, file, "class", "App.Service", "App")
				f.singleton(t, file, "App.Service.run", "public")
				f.nested(t, file, "class", "App.Caller", "App")
				f.declare(t, file, "class", "App.Caller")
				f.lexical(t, file, "App.Caller", "")
				caller := f.method(t, file, "App.Caller.f", false)
				return f.call(t, file, srcOf(caller), "Service.run", rubyConstantReceiver, 1), "Service.run"
			},
		},
		{
			// Ruby's runtime would reach the root constant through the cref's
			// ancestors. P22.48 models the nesting scan only.
			name: "no root fallback",
			build: func(t *testing.T, f *rubyFixture) (int64, string) {
				file := f.rb(t, "app/a.rb")
				f.nested(t, file, "class", "Service", "")
				f.singleton(t, file, "Service.run", "public")
				f.nested(t, file, "class", "Caller", "")
				caller := f.method(t, file, "Caller.f", false)
				return f.call(t, file, srcOf(caller), "Service.run", rubyConstantReceiver, 1), "Service.run"
			},
		},
		{
			name: "no superclass constant lookup",
			build: func(t *testing.T, f *rubyFixture) (int64, string) {
				file := f.rb(t, "app/a.rb")
				f.nested(t, file, "class", "Base", "")
				f.nested(t, file, "class", "Base.Service", "Base")
				f.singleton(t, file, "Base.Service.run", "public")
				f.nested(t, file, "class", "Caller", "")
				caller := f.method(t, file, "Caller.f", false)
				return f.call(t, file, srcOf(caller), "Service.run", rubyConstantReceiver, 1), "Service.run"
			},
		},
		{
			name: "missing lexical parent fact",
			build: func(t *testing.T, f *rubyFixture) (int64, string) {
				file := f.rb(t, "app/a.rb")
				f.nested(t, file, "type", "App", "")
				f.nested(t, file, "class", "App.Service", "App")
				f.singleton(t, file, "App.Service.run", "public")
				// No lexical fact for App.Caller: the nesting is unknown.
				f.declare(t, file, "class", "App.Caller")
				caller := f.method(t, file, "App.Caller.f", false)
				return f.call(t, file, srcOf(caller), "Service.run", rubyConstantReceiver, 1), "Service.run"
			},
		},
		{
			// A lexical fact from another file's reopening answers a question
			// about a definition site it never saw.
			name: "lexical parent from another file does not apply",
			build: func(t *testing.T, f *rubyFixture) (int64, string) {
				file := f.rb(t, "app/a.rb")
				other := f.rb(t, "app/b.rb")
				f.nested(t, file, "type", "App", "")
				f.nested(t, file, "class", "App.Service", "App")
				f.singleton(t, file, "App.Service.run", "public")
				f.declare(t, file, "class", "App.Caller")
				f.lexical(t, other, "App.Caller", "App")
				caller := f.method(t, file, "App.Caller.f", false)
				return f.call(t, file, srcOf(caller), "Service.run", rubyConstantReceiver, 1), "Service.run"
			},
		},
	})
}

// Reopened declarations of one kind are one semantic constant; a qname declared
// both `class` and `module` is a contradiction with no load order to settle it.
func TestRubyLexicalConstantReopenedAndConflicting(t *testing.T) {
	runRubyConstantCases(t, []rubyConstantCase{
		{
			name:      "reopened constant is one identity",
			wantQName: "App.Service.run",
			build: func(t *testing.T, f *rubyFixture) (int64, string) {
				a := f.rb(t, "app/a.rb")
				b := f.rb(t, "app/b.rb")
				f.nested(t, a, "type", "App", "")
				f.nested(t, a, "class", "App.Service", "App")
				f.nested(t, b, "class", "App.Service", "App")
				f.singleton(t, a, "App.Service.run", "public")
				f.nested(t, a, "class", "App.Caller", "App")
				caller := f.method(t, a, "App.Caller.f", false)
				return f.call(t, a, srcOf(caller), "Service.run", rubyConstantReceiver, 1), "Service.run"
			},
		},
		{
			name: "class and module for one qname",
			build: func(t *testing.T, f *rubyFixture) (int64, string) {
				a := f.rb(t, "app/a.rb")
				b := f.rb(t, "app/b.rb")
				f.nested(t, a, "type", "App", "")
				f.nested(t, a, "class", "App.Service", "App")
				f.nested(t, b, "type", "App.Service", "App")
				f.singleton(t, a, "App.Service.run", "public")
				f.nested(t, a, "class", "App.Caller", "App")
				caller := f.method(t, a, "App.Caller.f", false)
				return f.call(t, a, srcOf(caller), "Service.run", rubyConstantReceiver, 1), "Service.run"
			},
		},
		{
			// The inner constant is incoherent, so the lookup stops there
			// rather than continuing outward to a name it already claims.
			name: "conflicting inner constant does not fall through",
			build: func(t *testing.T, f *rubyFixture) (int64, string) {
				a := f.rb(t, "app/a.rb")
				f.nested(t, a, "type", "App", "")
				f.nested(t, a, "class", "App.Service", "App")
				f.singleton(t, a, "App.Service.run", "public")
				f.nested(t, a, "class", "App.Caller", "App")
				f.nested(t, a, "class", "App.Caller.Service", "App.Caller")
				f.declare(t, a, "type", "App.Caller.Service")
				caller := f.method(t, a, "App.Caller.f", false)
				return f.call(t, a, srcOf(caller), "Service.run", rubyConstantReceiver, 1), "Service.run"
			},
		},
		{
			// Two singleton declarations of one name: Ruby's winner is a load
			// order this graph does not model.
			name: "duplicate singleton definitions",
			build: func(t *testing.T, f *rubyFixture) (int64, string) {
				a := f.rb(t, "app/a.rb")
				b := f.rb(t, "app/b.rb")
				f.nested(t, a, "type", "App", "")
				f.nested(t, a, "class", "App.Service", "App")
				f.nested(t, b, "class", "App.Service", "App")
				f.singleton(t, a, "App.Service.run", "public")
				f.singleton(t, b, "App.Service.run", "public")
				f.nested(t, a, "class", "App.Caller", "App")
				caller := f.method(t, a, "App.Caller.f", false)
				return f.call(t, a, srcOf(caller), "Service.run", rubyConstantReceiver, 1), "Service.run"
			},
		},
	})
}

// The receiver is the class/module object, so the target is a singleton method
// and an instance method of the same name is not a candidate at all.
func TestRubyLexicalConstantTargetsSingletonsOnly(t *testing.T) {
	runRubyConstantCases(t, []rubyConstantCase{
		{
			name: "instance-only target",
			build: func(t *testing.T, f *rubyFixture) (int64, string) {
				a := f.rb(t, "app/a.rb")
				f.nested(t, a, "type", "App", "")
				f.nested(t, a, "class", "App.Service", "App")
				f.method(t, a, "App.Service.run", false)
				f.nested(t, a, "class", "App.Caller", "App")
				caller := f.method(t, a, "App.Caller.f", false)
				return f.call(t, a, srcOf(caller), "Service.run", rubyConstantReceiver, 1), "Service.run"
			},
		},
		{
			name:      "instance and singleton of one name",
			wantQName: "App.Service.run",
			build: func(t *testing.T, f *rubyFixture) (int64, string) {
				a := f.rb(t, "app/a.rb")
				f.nested(t, a, "type", "App", "")
				f.nested(t, a, "class", "App.Service", "App")
				f.method(t, a, "App.Service.run", false)
				f.singleton(t, a, "App.Service.run", "public")
				f.nested(t, a, "class", "App.Caller", "App")
				caller := f.method(t, a, "App.Caller.f", false)
				return f.call(t, a, srcOf(caller), "Service.run", rubyConstantReceiver, 1), "Service.run"
			},
		},
	})
}

// A constant receiver may not reach a private or protected singleton method, in
// any spelling, in any file, and facts that disagree fail closed.
func TestRubyLexicalConstantVisibilityVeto(t *testing.T) {
	build := func(declared string, facts ...[3]string) func(*testing.T, *rubyFixture) (int64, string) {
		return func(t *testing.T, f *rubyFixture) (int64, string) {
			a := f.rb(t, "app/a.rb")
			other := f.rb(t, "app/b.rb")
			f.nested(t, a, "type", "App", "")
			f.nested(t, a, "class", "App.Service", "App")
			f.singleton(t, a, "App.Service.run", declared)
			for _, fact := range facts {
				file := a
				if fact[2] == "other" {
					file = other
				}
				if fact[1] == "" {
					f.visibilityHazard(t, file, fact[0])
					continue
				}
				f.visibility(t, file, fact[0], "run", fact[1])
			}
			f.nested(t, a, "class", "App.Caller", "App")
			caller := f.method(t, a, "App.Caller.f", false)
			return f.call(t, a, srcOf(caller), "Service.run", rubyConstantReceiver, 1), "Service.run"
		}
	}
	runRubyConstantCases(t, []rubyConstantCase{
		{name: "public default", wantQName: "App.Service.run", build: build("public")},
		{name: "public override", wantQName: "App.Service.run", build: build("public", [3]string{"App.Service", "public", ""})},
		{name: "repeated public override", wantQName: "App.Service.run",
			build: build("public", [3]string{"App.Service", "public", ""}, [3]string{"App.Service", "public", "other"})},
		{name: "eigenclass private definition", build: build("private")},
		{name: "eigenclass protected definition", build: build("protected")},
		{name: "unstated definition", build: build("")},
		{name: "private_class_method override", build: build("public", [3]string{"App.Service", "private", ""})},
		{name: "protected override", build: build("public", [3]string{"App.Service", "protected", ""})},
		{name: "cross-file private override", build: build("public", [3]string{"App.Service", "private", "other"})},
		{name: "conflicting overrides", build: build("public",
			[3]string{"App.Service", "public", ""}, [3]string{"App.Service", "private", "other"})},
		{name: "owner hazard", build: build("public", [3]string{"App.Service", "", ""})},
		{name: "cross-file owner hazard", build: build("public", [3]string{"App.Service", "", "other"})},
		{
			// A private override on the shadowing constant does not release the
			// name back to the outer one.
			name: "private inner target does not fall through",
			build: func(t *testing.T, f *rubyFixture) (int64, string) {
				a := f.rb(t, "app/a.rb")
				f.nested(t, a, "type", "App", "")
				f.nested(t, a, "class", "App.Service", "App")
				f.singleton(t, a, "App.Service.run", "public")
				f.nested(t, a, "class", "App.Caller", "App")
				f.nested(t, a, "class", "App.Caller.Service", "App.Caller")
				f.singleton(t, a, "App.Caller.Service.run", "public")
				f.visibility(t, a, "App.Caller.Service", "run", "private")
				caller := f.method(t, a, "App.Caller.f", false)
				return f.call(t, a, srcOf(caller), "Service.run", rubyConstantReceiver, 1), "Service.run"
			},
		},
		{
			// A private declaration must not silently vanish: the public
			// sibling it duplicates is still an unmodelled load order.
			name: "public and private declarations of one name",
			build: func(t *testing.T, f *rubyFixture) (int64, string) {
				a := f.rb(t, "app/a.rb")
				b := f.rb(t, "app/b.rb")
				f.nested(t, a, "type", "App", "")
				f.nested(t, a, "class", "App.Service", "App")
				f.nested(t, b, "class", "App.Service", "App")
				f.singleton(t, a, "App.Service.run", "public")
				f.singleton(t, b, "App.Service.run", "private")
				f.nested(t, a, "class", "App.Caller", "App")
				caller := f.method(t, a, "App.Caller.f", false)
				return f.call(t, a, srcOf(caller), "Service.run", rubyConstantReceiver, 1), "Service.run"
			},
		},
	})
}

// Visibility facts and constants alike stop existing when their file does.
func TestRubyLexicalConstantIgnoresSoftDeletedFiles(t *testing.T) {
	setDeleted := func(t *testing.T, f *rubyFixture, path string) {
		t.Helper()
		if _, err := f.store.db.ExecContext(f.ctx,
			`UPDATE files SET is_deleted = 1 WHERE repo_id = ? AND path = ?`, f.repoID, path); err != nil {
			t.Fatal(err)
		}
	}
	runRubyConstantCases(t, []rubyConstantCase{
		{
			name:      "deleted private override stops vetoing",
			wantQName: "App.Service.run",
			build: func(t *testing.T, f *rubyFixture) (int64, string) {
				a := f.rb(t, "app/a.rb")
				stale := f.rb(t, "app/stale.rb")
				f.nested(t, a, "type", "App", "")
				f.nested(t, a, "class", "App.Service", "App")
				f.singleton(t, a, "App.Service.run", "public")
				f.visibility(t, stale, "App.Service", "run", "private")
				f.visibilityHazard(t, stale, "App.Service")
				f.nested(t, a, "class", "App.Caller", "App")
				caller := f.method(t, a, "App.Caller.f", false)
				edge := f.call(t, a, srcOf(caller), "Service.run", rubyConstantReceiver, 1)
				setDeleted(t, f, "app/stale.rb")
				return edge, "Service.run"
			},
		},
		{
			name: "deleted constant establishes nothing",
			build: func(t *testing.T, f *rubyFixture) (int64, string) {
				a := f.rb(t, "app/a.rb")
				stale := f.rb(t, "app/stale.rb")
				f.nested(t, a, "type", "App", "")
				f.nested(t, stale, "class", "App.Service", "App")
				f.singleton(t, stale, "App.Service.run", "public")
				f.nested(t, a, "class", "App.Caller", "App")
				caller := f.method(t, a, "App.Caller.f", false)
				edge := f.call(t, a, srcOf(caller), "Service.run", rubyConstantReceiver, 1)
				setDeleted(t, f, "app/stale.rb")
				return edge, "Service.run"
			},
		},
		{
			// A deleted duplicate stops making the survivor ambiguous.
			name:      "deleted duplicate singleton",
			wantQName: "App.Service.run",
			build: func(t *testing.T, f *rubyFixture) (int64, string) {
				a := f.rb(t, "app/a.rb")
				stale := f.rb(t, "app/stale.rb")
				f.nested(t, a, "type", "App", "")
				f.nested(t, a, "class", "App.Service", "App")
				f.singleton(t, a, "App.Service.run", "public")
				f.nested(t, stale, "class", "App.Service", "App")
				f.singleton(t, stale, "App.Service.run", "public")
				f.nested(t, a, "class", "App.Caller", "App")
				caller := f.method(t, a, "App.Caller.f", false)
				edge := f.call(t, a, srcOf(caller), "Service.run", rubyConstantReceiver, 1)
				setDeleted(t, f, "app/stale.rb")
				return edge, "Service.run"
			},
		},
	})
}

// P7: a spec file reopening a production constant must neither become the
// production caller's target nor make the production constant ambiguous.
func TestRubyLexicalConstantTestFileRule(t *testing.T) {
	f := newRubyFixture(t)
	app := f.rb(t, "app/service.rb")
	spec := f.rb(t, "spec/service_spec.rb")
	f.nested(t, app, "type", "App", "")
	f.nested(t, app, "class", "App.Service", "App")
	production := f.singleton(t, app, "App.Service.run", "public")
	// The spec reopens the same semantic constant and adds a singleton of the
	// same name, plus a test-only constant that would shadow it.
	f.nested(t, spec, "class", "App.Service", "App")
	specRun := f.singleton(t, spec, "App.Service.run", "public")
	f.nested(t, app, "class", "App.Caller", "App")
	f.nested(t, spec, "class", "App.Caller.Service", "App.Caller")
	f.singleton(t, spec, "App.Caller.Service.run", "public")
	prodCaller := f.method(t, app, "App.Caller.f", false)
	prodEdge := f.call(t, app, srcOf(prodCaller), "Service.run", rubyConstantReceiver, 1)
	f.nested(t, spec, "class", "Spec", "")
	f.nested(t, spec, "class", "Spec.Inner", "Spec")
	specCaller := f.method(t, spec, "App.Caller.g", false)
	f.lexical(t, spec, "App.Caller", "App")
	specEdge := f.call(t, spec, srcOf(specCaller), "Service.run", rubyConstantReceiver, 2)

	prodWant := "App.Service.run|" + ResolutionStrategyRubyLexicalConstant + "|" + ResolutionConfidenceHigh
	for _, entry := range rubyEntryPoints {
		f.clearAll(t)
		f.resolveVia(t, entry, []string{"app/service.rb", "spec/service_spec.rb"}, []string{"Service.run", "run"})
		if got := f.binding(t, prodEdge); got != prodWant {
			t.Fatalf("%s: production caller = %s, want %s", entry, got, prodWant)
		}
		var dst int64
		if err := f.store.db.QueryRowContext(f.ctx, `SELECT dst_symbol_id FROM edges WHERE id = ?`, prodEdge).Scan(&dst); err != nil {
			t.Fatal(err)
		}
		if dst != production {
			t.Fatalf("%s: production caller bound %d, want the production declaration %d", entry, dst, production)
		}
		// The test caller sees both declarations and has no load order either.
		if got := f.binding(t, specEdge); got != "<unresolved>" {
			t.Fatalf("%s: test caller = %s, want unresolved (two declarations)", entry, got)
		}
		_ = specRun
	}
}

// Every unsupported receiver shape stays Ruby-owned and unresolved even when a
// uniquely tempting target exists at exactly the qualified name it spells.
func TestRubyLexicalConstantWrongEdgeFixtures(t *testing.T) {
	f := newRubyFixture(t)
	app := f.rb(t, "app/a.rb")
	bait := f.rb(t, "app/bait.rb")
	f.nested(t, app, "type", "App", "")
	f.nested(t, app, "class", "App.Service", "App")
	f.singleton(t, app, "App.Service.run", "public")
	f.nested(t, app, "class", "App.Caller", "App")
	caller := f.method(t, app, "App.Caller.f", false)

	spellings := []struct {
		name, dst, evidence string
	}{
		{"multi segment dot", "App::Service.run", rubyConstantReceiver},
		{"multi segment scope", "App::Service::run", rubyConstantReceiver},
		{"absolute", "::Service.run", rubyConstantReceiver},
		{"absolute multi segment", "::App::Service.run", rubyConstantReceiver},
		{"value receiver", "obj.run", "ruby:value_receiver"},
		{"safe navigation", "Service&.run", "ruby:safe_navigation"},
		{"chained", "Service.build.run", "ruby:chained_receiver"},
	}
	edges := map[string]int64{}
	names := []string{"run"}
	for i, tc := range spellings {
		// The bait is the destination a generic strategy would love: its
		// qualified name IS the spelling and its bare name is unique.
		f.singleton(t, bait, tc.dst, "public")
		edges[tc.name] = f.call(t, app, srcOf(caller), tc.dst, tc.evidence, i+1)
		names = append(names, tc.dst)
	}
	// A constant call whose source is a type, not a method, has no definition
	// site to read a nesting from.
	bodyEdge := f.call(t, app, srcOf(f.declare(t, app, "class", "App.Configured")), "Service.run", rubyConstantReceiver, 50)
	edges["class body source"] = bodyEdge
	// A top-level method has no container and so no lexical scope.
	top := f.rb(t, "app/top.rb")
	topCaller := f.insert(t, top, "function", "main", sql.NullInt64{Int64: 0, Valid: true})
	edges["top level source"] = f.call(t, top, srcOf(topCaller), "Service.run", rubyConstantReceiver, 51)
	// A source symbol that is not a Ruby method proves nothing.
	odd := f.insert(t, app, "variable", "App.Caller.config", sql.NullInt64{})
	edges["source-less"] = f.call(t, app, srcOf(odd), "Service.run", rubyConstantReceiver, 52)
	names = append(names, "Service.run")

	paths := []string{"app/a.rb", "app/bait.rb", "app/top.rb"}
	for _, entry := range rubyEntryPoints {
		f.clearAll(t)
		f.resolveVia(t, entry, paths, names)
		for name, edge := range edges {
			if got := f.binding(t, edge); got != "<unresolved>" {
				t.Fatalf("%s: %s = %s, want unresolved", entry, name, got)
			}
		}
	}
}

// Nothing here depends on row order: the same facts inserted in the reverse
// order produce the same answers.
func TestRubyLexicalConstantIsInsertionOrderIndependent(t *testing.T) {
	for _, reversed := range []bool{false, true} {
		f := newRubyFixture(t)
		a := f.rb(t, "app/a.rb")
		b := f.rb(t, "app/b.rb")
		first, second := a, b
		if reversed {
			first, second = b, a
		}
		f.nested(t, a, "type", "App", "")
		f.nested(t, first, "class", "App.Service", "App")
		f.nested(t, second, "type", "App.Service", "App")
		f.singleton(t, first, "App.Service.run", "public")
		f.nested(t, a, "class", "App.Caller", "App")
		caller := f.method(t, a, "App.Caller.f", false)
		edge := f.call(t, a, srcOf(caller), "Service.run", rubyConstantReceiver, 1)
		f.resolveVia(t, "full", []string{"app/a.rb", "app/b.rb"}, []string{"Service.run"})
		if got := f.binding(t, edge); got != "<unresolved>" {
			t.Fatalf("reversed=%v: %s, want unresolved for a class/module conflict", reversed, got)
		}
	}
}

// References follow the edge: a bound constant call gives the caller's own
// occurrence a destination, and losing the bind clears it again.
func TestRubyLexicalConstantReferenceIdentity(t *testing.T) {
	f := newRubyFixture(t)
	file := f.rb(t, "app/a.rb")
	f.nested(t, file, "type", "App", "")
	f.nested(t, file, "class", "App.Service", "App")
	target := f.singleton(t, file, "App.Service.run", "public")
	f.nested(t, file, "class", "App.Caller", "App")
	caller := f.method(t, file, "App.Caller.f", false)
	edge := f.call(t, file, srcOf(caller), "Service.run", rubyConstantReceiver, 7)
	if _, err := f.store.db.ExecContext(f.ctx, `
		INSERT INTO references_tbl(repo_id, file_id, symbol_id, name, qualified_name, ref_kind,
			start_line, start_col, end_line, end_col, context_symbol_id)
		VALUES(?, ?, NULL, 'Service.run', 'Service.run', 'call', 7, 1, 7, 12, ?)`, f.repoID, file, caller); err != nil {
		t.Fatal(err)
	}
	f.resolveVia(t, "full", []string{"app/a.rb"}, []string{"Service.run"})
	if err := f.store.ReconcileReferenceIdentities(f.ctx, f.repoID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	var symbol sql.NullInt64
	var context sql.NullInt64
	row := func() {
		t.Helper()
		if err := f.store.db.QueryRowContext(f.ctx,
			`SELECT symbol_id, context_symbol_id FROM references_tbl WHERE repo_id = ? AND name = 'Service.run'`,
			f.repoID).Scan(&symbol, &context); err != nil {
			t.Fatal(err)
		}
	}
	row()
	if !symbol.Valid || symbol.Int64 != target {
		t.Fatalf("reference symbol = %v, want the bound target %d", symbol, target)
	}
	if !context.Valid || context.Int64 != caller {
		t.Fatalf("reference context = %v, want the calling method %d", context, caller)
	}
	// A private override withdraws the target, and the reference follows.
	f.visibility(t, file, "App.Service", "run", "private")
	f.clearAll(t)
	f.resolveVia(t, "full", []string{"app/a.rb"}, []string{"Service.run"})
	if err := f.store.ReconcileReferenceIdentities(f.ctx, f.repoID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if got := f.binding(t, edge); got != "<unresolved>" {
		t.Fatalf("edge after private override = %s, want unresolved", got)
	}
	row()
	if symbol.Valid {
		t.Fatalf("reference symbol = %v, want cleared", symbol)
	}
}

// The new strategy is registered wherever a strategy has to be, and Ruby's
// SQL-side ownership veto still matches its Go twin.
func TestRubyLexicalConstantStrategyRegistered(t *testing.T) {
	if got := resolutionConfidenceFor(ResolutionStrategyRubyLexicalConstant); got != ResolutionConfidenceHigh {
		t.Fatalf("confidence = %q, want high (rubyScopeApply interpolates one constant)", got)
	}
	for _, list := range [][]string{rubyScopeStrategies, incrementallyRedecidableStrategies} {
		found := false
		for _, s := range list {
			found = found || s == ResolutionStrategyRubyLexicalConstant
		}
		if !found {
			t.Fatalf("%v does not carry %s", list, ResolutionStrategyRubyLexicalConstant)
		}
	}
}

// The pass keeps its query count bounded when the edge set, the candidate
// constant set and the target set all exceed SQLite's variable ceiling.
func TestRubyLexicalConstantBatchBudget(t *testing.T) {
	const n = 1200
	f := newRubyFixture(t)
	file := f.rb(t, "app/wide.rb")
	f.nested(t, file, "type", "App", "")
	edges := make([]int64, 0, n)
	names := make([]string, 0, n)
	for i := range n {
		container := fmt.Sprintf("App.C%d", i)
		f.nested(t, file, "class", container, "App")
		f.nested(t, file, "class", container+".Service", container)
		f.singleton(t, file, container+".Service.run", "public")
		caller := f.method(t, file, container+".f", false)
		edges = append(edges, f.call(t, file, srcOf(caller), "Service.run", rubyConstantReceiver, i+1))
		names = append(names, fmt.Sprintf("C%d.run", i))
	}
	names = append(names, "Service.run")
	for _, entry := range rubyEntryPoints {
		f.clearAll(t)
		f.resolveVia(t, entry, []string{"app/wide.rb"}, names)
		for i, edge := range edges {
			want := fmt.Sprintf("App.C%d.Service.run|%s|high", i, ResolutionStrategyRubyLexicalConstant)
			if got := f.binding(t, edge); got != want {
				t.Fatalf("%s: edge %d = %s, want %s", entry, i, got, want)
			}
		}
	}
}
