package store

import (
	"testing"
)

// factProbe is one edge a fact-parity scenario asserts, with the binding the
// repo-wide SQL gate gives it. Pinning that binding keeps a scenario from
// passing vacuously when both pipelines happen to refuse everything.
type factProbe struct {
	edge int64
	want string
}

// factScenario is one repository state with the bindings every entrypoint must
// produce. dim is the acceptance-matrix dimension the scenario covers.
type factScenario struct {
	rule, name, dim string
	paths           []string
	names           []string
	seed            func(t *testing.T, f *parityFixture) []factProbe
}

// factParityScenarios seed a repository state that exercises one rule of the
// bind gate whose Go-side decision reads loaded facts rather than the edge
// alone. Each scenario is resolved once per entrypoint in a fresh store: the
// full resolve answers through the composed SQL gate, the path- and
// name-scoped resolves through the Go binder, and every answer must be the
// same.
var factParityScenarios = []factScenario{
	{
		rule: "language_gate", name: "same-language candidate binds", dim: "positive",
		paths: []string{"app/main.py"}, names: []string{"helper"},
		seed: func(t *testing.T, f *parityFixture) []factProbe {
			lib := f.file(t, "app/util.py", "python")
			f.symbol(t, lib, "helper", "util.helper", "function", "python")
			main := f.file(t, "app/main.py", "python")
			caller := f.symbol(t, main, "run", "main.run", "function", "python")
			return []factProbe{{f.edge(t, main, caller, "helper"), "util.helper|exact_name|high"}}
		},
	},
	{
		rule: "language_gate", name: "other-language candidate refused", dim: "negative",
		paths: []string{"app/main.py"}, names: []string{"helper"},
		seed: func(t *testing.T, f *parityFixture) []factProbe {
			lib := f.file(t, "pkg/util.go", "go")
			f.symbolIn(t, lib, "helper", "pkg.helper", "function", "pkg", "go")
			main := f.file(t, "app/main.py", "python")
			caller := f.symbol(t, main, "run", "main.run", "function", "python")
			return []factProbe{{f.edge(t, main, caller, "helper"), "<unresolved>"}}
		},
	},
	{
		rule: "language_gate", name: "unknown caller language refused", dim: "missing_fact",
		paths: []string{"app/main.x"}, names: []string{"helper"},
		seed: func(t *testing.T, f *parityFixture) []factProbe {
			lib := f.file(t, "app/util.py", "python")
			f.symbol(t, lib, "helper", "util.helper", "function", "python")
			main := f.file(t, "app/main.x", "")
			caller := f.symbol(t, main, "run", "main.run", "function", "")
			return []factProbe{{f.edge(t, main, caller, "helper"), "<unresolved>"}}
		},
	},
	{
		rule: "language_gate", name: "another repository's candidate is invisible", dim: "repo_isolation",
		paths: []string{"app/main.py"}, names: []string{"helper"},
		seed: func(t *testing.T, f *parityFixture) []factProbe {
			other, err := f.store.UpsertRepo(f.ctx, t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			otherLib, err := insertTestFileLang(f.ctx, f.store, other.ID, "app/util.py", "python")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := insertTestSymbolKind(f.ctx, f.store, other.ID, otherLib, "helper", "util.helper", "function", "", "python"); err != nil {
				t.Fatal(err)
			}
			main := f.file(t, "app/main.py", "python")
			caller := f.symbol(t, main, "run", "main.run", "function", "python")
			return []factProbe{{f.edge(t, main, caller, "helper"), "<unresolved>"}}
		},
	},
	{
		rule: "caller_kind_candidate", name: "production caller refuses a test-only candidate", dim: "negative",
		paths: []string{"app/main.py", "app/test_main.py"}, names: []string{"helper"},
		seed: func(t *testing.T, f *parityFixture) []factProbe {
			tests := f.file(t, "app/test_util.py", "python")
			f.symbol(t, tests, "helper", "test_util.helper", "function", "python")
			main := f.file(t, "app/main.py", "python")
			caller := f.symbol(t, main, "run", "main.run", "function", "python")
			testMain := f.file(t, "app/test_main.py", "python")
			testCaller := f.symbol(t, testMain, "test_run", "test_main.test_run", "function", "python")
			return []factProbe{
				{f.edge(t, main, caller, "helper"), "<unresolved>"},
				{f.edge(t, testMain, testCaller, "helper"), "test_util.helper|exact_name|high"},
			}
		},
	},
	{
		rule: "caller_kind_candidate", name: "production candidate beats a test shadow", dim: "test_shadow",
		paths: []string{"app/main.py", "app/test_main.py"}, names: []string{"helper"},
		seed: func(t *testing.T, f *parityFixture) []factProbe {
			lib := f.file(t, "app/util.py", "python")
			f.symbol(t, lib, "helper", "util.helper", "function", "python")
			tests := f.file(t, "app/test_util.py", "python")
			f.symbol(t, tests, "helper", "test_util.helper", "function", "python")
			main := f.file(t, "app/main.py", "python")
			caller := f.symbol(t, main, "run", "main.run", "function", "python")
			testMain := f.file(t, "app/test_main.py", "python")
			testCaller := f.symbol(t, testMain, "test_run", "test_main.test_run", "function", "python")
			return []factProbe{
				{f.edge(t, main, caller, "helper"), "util.helper|exact_name|high"},
				{f.edge(t, testMain, testCaller, "helper"), "<unresolved>"},
			}
		},
	},
	{
		rule: "broad_ambiguity", name: "two same-language candidates refuse the bare name", dim: "ambiguity",
		paths: []string{"app/main.py"}, names: []string{"helper", "util.helper"},
		seed: func(t *testing.T, f *parityFixture) []factProbe {
			a := f.file(t, "app/util.py", "python")
			f.symbol(t, a, "helper", "util.helper", "function", "python")
			b := f.file(t, "app/other.py", "python")
			f.symbol(t, b, "helper", "other.helper", "function", "python")
			main := f.file(t, "app/main.py", "python")
			caller := f.symbol(t, main, "run", "main.run", "function", "python")
			return []factProbe{
				{f.edge(t, main, caller, "helper"), "<unresolved>"},
				{f.edge(t, main, caller, "util.helper"), "util.helper|exact_qualified|high"},
			}
		},
	},
	{
		// The receiver strategy sees only the container-bearing candidate, so
		// without the broad-level veto it would resurrect a name the bare-name
		// level already found undecidable.
		rule: "broad_ambiguity", name: "a narrower strategy cannot resurrect an ambiguous bare name", dim: "ambiguity",
		paths: []string{"app/main.py"}, names: []string{"helper"},
		seed: func(t *testing.T, f *parityFixture) []factProbe {
			a := f.file(t, "app/util.py", "python")
			f.symbol(t, a, "helper", "util.helper", "function", "python")
			b := f.file(t, "app/other.py", "python")
			f.symbolIn(t, b, "helper", "other.Box.helper", "method", "Box", "python")
			main := f.file(t, "app/main.py", "python")
			caller := f.symbol(t, main, "run", "main.run", "function", "python")
			return []factProbe{{f.edge(t, main, caller, "helper"), "<unresolved>"}}
		},
	},
	{
		// The import maps into the module's own pkg directory, which declares
		// no Open; the claim keeps a same-named package elsewhere from
		// answering through a suffix.
		rule: "own_module_import", name: "a claimed import does not fall back to another directory", dim: "negative",
		paths: []string{"cmd/main.go"}, names: []string{"Open"},
		seed: func(t *testing.T, f *parityFixture) []factProbe {
			pkg := f.file(t, "pkg/close.go", "go")
			f.symbolIn(t, pkg, "Close", "pkg.Close", "function", "pkg", "go")
			third := f.file(t, "third/pkg/open.go", "go")
			f.symbolIn(t, third, "Open", "pkg.Open", "function", "pkg", "go")
			main := f.file(t, "cmd/main.go", "go")
			caller := f.symbol(t, main, "main", "main", "function", "go")
			f.imports(t, main, "example.com/project/pkg")
			return []factProbe{{f.edge(t, main, caller, "example.com/project/pkg.Open"), "<unresolved>"}}
		},
	},
	{
		rule: "own_module_import", name: "module path without an import fact fails closed", dim: "missing_fact",
		paths: []string{"cmd/main.go"}, names: []string{"Open"},
		seed: func(t *testing.T, f *parityFixture) []factProbe {
			pkg := f.file(t, "pkg/open.go", "go")
			f.symbolIn(t, pkg, "Open", "pkg.Open", "function", "pkg", "go")
			main := f.file(t, "cmd/main.go", "go")
			caller := f.symbol(t, main, "main", "main", "function", "go")
			return []factProbe{{f.edge(t, main, caller, "example.com/project/pkg.Open"), "<unresolved>"}}
		},
	},
	{
		rule: "own_module_import", name: "import fact binds the module package", dim: "positive",
		paths: []string{"cmd/main.go"}, names: []string{"Open"},
		seed: func(t *testing.T, f *parityFixture) []factProbe {
			pkg := f.file(t, "pkg/open.go", "go")
			f.symbolIn(t, pkg, "Open", "pkg.Open", "function", "pkg", "go")
			main := f.file(t, "cmd/main.go", "go")
			caller := f.symbol(t, main, "main", "main", "function", "go")
			f.imports(t, main, "example.com/project/pkg")
			return []factProbe{{f.edge(t, main, caller, "example.com/project/pkg.Open"), "pkg.Open|module_import|high"}}
		},
	},
	{
		rule: "typescript_scope_ownership", name: "an unimported cross-file candidate is not the generic strategies' to bind", dim: "missing_fact",
		paths: []string{"app/main.ts"}, names: []string{"helper"},
		seed: func(t *testing.T, f *parityFixture) []factProbe {
			lib := f.file(t, "app/util.ts", "typescript")
			f.symbol(t, lib, "helper", "util.helper", "function", "typescript")
			main := f.file(t, "app/main.ts", "typescript")
			caller := f.symbol(t, main, "run", "main.run", "function", "typescript")
			return []factProbe{{f.edge(t, main, caller, "helper"), "<unresolved>"}}
		},
	},
	{
		// The update entrypoint reruns the weak dotted strategies after its
		// language passes; they must not bind what the TypeScript pass left.
		rule: "typescript_scope_ownership", name: "a dotted spelling the pass leaves is not the weak strategies' to bind", dim: "negative",
		paths: []string{"app/main.ts"}, names: []string{"helper"},
		seed: func(t *testing.T, f *parityFixture) []factProbe {
			lib := f.file(t, "lib/x.ts", "typescript")
			f.symbol(t, lib, "helper", "lib/x.ns.mod.helper", "function", "typescript")
			f.symbol(t, lib, "helper", "x.a.b.c.helper", "method", "typescript")
			main := f.file(t, "app/main.ts", "typescript")
			caller := f.symbol(t, main, "run", "app/main.run", "function", "typescript")
			return []factProbe{
				{f.edge(t, main, caller, "ns.mod.helper"), "<unresolved>"},
				{f.edge(t, main, caller, "a.b.c.helper"), "<unresolved>"},
			}
		},
	},
	{
		rule: "csharp_scope_ownership", name: "an unproven C# call is not the generic strategies' to bind", dim: "negative",
		paths: []string{"App/Main.cs"}, names: []string{"Helper"},
		seed: func(t *testing.T, f *parityFixture) []factProbe {
			lib := f.file(t, "App/Util.cs", "csharp")
			f.symbol(t, lib, "Helper", "App.Util.Helper", "method", "csharp")
			main := f.file(t, "App/Main.cs", "csharp")
			caller := f.symbol(t, main, "Run", "App.Main.Run", "method", "csharp")
			return []factProbe{
				{f.edge(t, main, caller, "Helper"), "<unresolved>"},
				{f.edge(t, main, caller, "Util.Helper"), "<unresolved>"},
			}
		},
	},
	{
		// Repository-wide uniqueness is not scope evidence for a class name:
		// a bare call to a class the caller neither declares nor imports
		// stays unresolved.
		rule: "bare_type_scope", name: "an unimported class is not in scope", dim: "missing_fact",
		paths: []string{"app/main.py"}, names: []string{"Widget"},
		seed: func(t *testing.T, f *parityFixture) []factProbe {
			lib := f.file(t, "app/widgets.py", "python")
			f.symbol(t, lib, "Widget", "widgets.Widget", "class", "python")
			main := f.file(t, "app/main.py", "python")
			caller := f.symbol(t, main, "run", "main.run", "function", "python")
			return []factProbe{{f.edge(t, main, caller, "Widget"), "<unresolved>"}}
		},
	},
	{
		// The caller imports helper from a module that does not declare it:
		// Python claims the call and refuses it, so a unique helper elsewhere
		// is not the generic strategies' to bind.
		rule: "python_scope_claims", name: "a claimed import refusal does not fall back", dim: "negative",
		paths: []string{"app/main.py"}, names: []string{"helper"},
		seed: func(t *testing.T, f *parityFixture) []factProbe {
			f.file(t, "lib.py", "python")
			other := f.file(t, "app/other.py", "python")
			f.symbol(t, other, "helper", "other.helper", "function", "python")
			main := f.file(t, "app/main.py", "python")
			caller := f.symbol(t, main, "run", "main.run", "function", "python")
			if _, err := f.store.db.ExecContext(f.ctx,
				`INSERT INTO scope_import_evidence(repo_id,file_id,language,source_specifier,imported_name,local_name,import_kind,wildcard) VALUES(?,?,?,?,?,?,?,0)`,
				f.repoID, main, "python", "lib", "helper", "helper", "named"); err != nil {
				t.Fatal(err)
			}
			return []factProbe{{f.edge(t, main, caller, "helper"), "<unresolved>"}}
		},
	},
	{
		rule: "jvm_scope_ownership", name: "a Java file with scope evidence owns its refused calls", dim: "negative",
		paths: []string{"app/Main.java"}, names: []string{"x", "Other.x"},
		seed: func(t *testing.T, f *parityFixture) []factProbe {
			lib := f.file(t, "lib/Other.java", "java")
			f.symbolIn(t, lib, "x", "lib.Other.x", "function", "Other", "java")
			main := f.file(t, "app/Main.java", "java")
			caller := f.symbolIn(t, main, "run", "app.Main.run", "function", "Main", "java")
			if _, err := f.store.db.ExecContext(f.ctx, `INSERT INTO file_scope_evidence(repo_id,file_id,language,package_name) VALUES(?,?,?,?)`, f.repoID, main, "java", "app"); err != nil {
				t.Fatal(err)
			}
			return []factProbe{{f.edge(t, main, caller, "Other.x"), "<unresolved>"}}
		},
	},
	{
		rule: "jvm_scope_ownership", name: "a Java file without scope evidence is not owned", dim: "missing_fact",
		paths: []string{"app/Main.java"}, names: []string{"x", "Other.x"},
		seed: func(t *testing.T, f *parityFixture) []factProbe {
			lib := f.file(t, "lib/Other.java", "java")
			f.symbolIn(t, lib, "x", "lib.Other.x", "function", "Other", "java")
			main := f.file(t, "app/Main.java", "java")
			caller := f.symbolIn(t, main, "run", "app.Main.run", "function", "Main", "java")
			return []factProbe{{f.edge(t, main, caller, "Other.x"), "lib.Other.x|dot_tail2|medium"}}
		},
	},
}

func TestResolverGateRuleFactParity(t *testing.T) {
	known := map[resolverRuleID]bool{}
	for _, rule := range resolverBindGateRules {
		known[rule.id] = true
	}
	for _, sc := range allFactParityScenarios() {
		if !known[resolverRuleID(sc.rule)] {
			t.Fatalf("scenario %q names unknown rule %q", sc.name, sc.rule)
		}
		t.Run(sc.rule+"/"+sc.name, func(t *testing.T) {
			var full []string
			for _, entry := range []string{"full", "paths", "names", "paths+names"} {
				f := newParityFixture(t, "module example.com/project\n")
				probes := sc.seed(t, f)
				f.resolveVia(t, entry, sc.paths, sc.names)
				got := make([]string, len(probes))
				for i, p := range probes {
					got[i] = f.binding(t, p.edge)
				}
				if entry == "full" {
					full = got
					for i, p := range probes {
						if got[i] != p.want {
							t.Errorf("full: probe %d bound %q, want %q", i, got[i], p.want)
						}
					}
					continue
				}
				for i := range probes {
					if got[i] != full[i] {
						t.Errorf("%s: probe %d bound %q, full resolve bound %q", entry, i, got[i], full[i])
					}
				}
			}
		})
	}
}
