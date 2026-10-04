package store

import (
	"go/ast"
	"go/parser"
	"go/token"
	"maps"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// otherRepoFile declares a symbol in a second repository of the same store.
func (f *parityFixture) otherRepoFile(t *testing.T, path, language, name, qualified, kind, container string) {
	t.Helper()
	other, err := f.store.UpsertRepo(f.ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	file, err := insertTestFileLang(f.ctx, f.store, other.ID, path, language)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := insertTestSymbolKind(f.ctx, f.store, other.ID, file, name, qualified, kind, container, language); err != nil {
		t.Fatal(err)
	}
}

// namedImport records a named import fact for a file.
func (f *parityFixture) namedImport(t *testing.T, file int64, language, source, name string) {
	t.Helper()
	if _, err := f.store.db.ExecContext(f.ctx,
		`INSERT INTO scope_import_evidence(repo_id,file_id,language,source_specifier,imported_name,local_name,import_kind,wildcard) VALUES(?,?,?,?,?,?,?,0)`,
		f.repoID, file, language, source, name, name, "named"); err != nil {
		t.Fatal(err)
	}
}

// acceptanceScenarios fill the cells of the rule x dimension matrix that the
// original fact-parity scenarios leave open.
var acceptanceScenarios = []factScenario{
	{
		rule: "caller_kind_candidate", name: "production caller binds a production candidate", dim: "positive",
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
		rule: "broad_ambiguity", name: "a unique bare name binds", dim: "positive",
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
		rule: "broad_ambiguity", name: "a test-only twin does not make a production name ambiguous", dim: "test_shadow",
		paths: []string{"app/main.py"}, names: []string{"helper"},
		seed: func(t *testing.T, f *parityFixture) []factProbe {
			lib := f.file(t, "app/util.py", "python")
			f.symbol(t, lib, "helper", "util.helper", "function", "python")
			tests := f.file(t, "app/test_util.py", "python")
			f.symbol(t, tests, "helper", "test_util.helper", "function", "python")
			main := f.file(t, "app/main.py", "python")
			caller := f.symbol(t, main, "run", "main.run", "function", "python")
			return []factProbe{{f.edge(t, main, caller, "helper"), "util.helper|exact_name|high"}}
		},
	},
	{
		rule: "broad_ambiguity", name: "another repository's twin does not make a name ambiguous", dim: "repo_isolation",
		paths: []string{"app/main.py"}, names: []string{"helper"},
		seed: func(t *testing.T, f *parityFixture) []factProbe {
			f.otherRepoFile(t, "app/other.py", "python", "helper", "other.helper", "function", "")
			lib := f.file(t, "app/util.py", "python")
			f.symbol(t, lib, "helper", "util.helper", "function", "python")
			main := f.file(t, "app/main.py", "python")
			caller := f.symbol(t, main, "run", "main.run", "function", "python")
			return []factProbe{{f.edge(t, main, caller, "helper"), "util.helper|exact_name|high"}}
		},
	},
	{
		rule: "bare_type_scope", name: "a class in the caller's own file is in scope", dim: "positive",
		paths: []string{"app/main.py"}, names: []string{"Widget"},
		seed: func(t *testing.T, f *parityFixture) []factProbe {
			main := f.file(t, "app/main.py", "python")
			f.symbol(t, main, "Widget", "main.Widget", "class", "python")
			caller := f.symbol(t, main, "run", "main.run", "function", "python")
			return []factProbe{{f.edge(t, main, caller, "Widget"), "main.Widget|exact_name|high"}}
		},
	},
	{
		rule: "bare_type_scope", name: "another repository's class is not in scope", dim: "repo_isolation",
		paths: []string{"app/main.py"}, names: []string{"Widget"},
		seed: func(t *testing.T, f *parityFixture) []factProbe {
			f.otherRepoFile(t, "app/widgets.py", "python", "Widget", "widgets.Widget", "class", "")
			main := f.file(t, "app/main.py", "python")
			caller := f.symbol(t, main, "run", "main.run", "function", "python")
			return []factProbe{{f.edge(t, main, caller, "Widget"), "<unresolved>"}}
		},
	},
	{
		rule: "own_module_import", name: "another repository's package is not the module's", dim: "repo_isolation",
		paths: []string{"cmd/main.go"}, names: []string{"Open"},
		seed: func(t *testing.T, f *parityFixture) []factProbe {
			f.otherRepoFile(t, "pkg/open.go", "go", "Open", "pkg.Open", "function", "pkg")
			main := f.file(t, "cmd/main.go", "go")
			caller := f.symbol(t, main, "main", "main", "function", "go")
			f.imports(t, main, "example.com/project/pkg")
			return []factProbe{{f.edge(t, main, caller, "example.com/project/pkg.Open"), "<unresolved>"}}
		},
	},
	{
		rule: "own_module_import", name: "two declarations in the imported package stay ambiguous", dim: "ambiguity",
		paths: []string{"cmd/main.go"}, names: []string{"Open"},
		seed: func(t *testing.T, f *parityFixture) []factProbe {
			a := f.file(t, "pkg/a.go", "go")
			f.symbolIn(t, a, "Open", "pkg.Open", "function", "pkg", "go")
			b := f.file(t, "pkg/b.go", "go")
			f.symbolIn(t, b, "Open", "pkg.Open", "function", "pkg", "go")
			main := f.file(t, "cmd/main.go", "go")
			caller := f.symbol(t, main, "main", "main", "function", "go")
			f.imports(t, main, "example.com/project/pkg")
			return []factProbe{{f.edge(t, main, caller, "example.com/project/pkg.Open"), "<unresolved>"}}
		},
	},
	{
		rule: "python_scope_claims", name: "an imported helper binds through the claim", dim: "positive",
		paths: []string{"app/main.py"}, names: []string{"helper"},
		seed: func(t *testing.T, f *parityFixture) []factProbe {
			lib := f.file(t, "lib.py", "python")
			f.symbol(t, lib, "helper", "lib.helper", "function", "python")
			main := f.file(t, "app/main.py", "python")
			caller := f.symbol(t, main, "run", "main.run", "function", "python")
			f.namedImport(t, main, "python", "lib", "helper")
			return []factProbe{{f.edge(t, main, caller, "helper"), "lib.helper|python_import_scope|high"}}
		},
	},
	{
		rule: "python_scope_claims", name: "another repository's module is not imported", dim: "repo_isolation",
		paths: []string{"app/main.py"}, names: []string{"helper"},
		seed: func(t *testing.T, f *parityFixture) []factProbe {
			f.otherRepoFile(t, "lib.py", "python", "helper", "lib.helper", "function", "")
			main := f.file(t, "app/main.py", "python")
			caller := f.symbol(t, main, "run", "main.run", "function", "python")
			f.namedImport(t, main, "python", "lib", "helper")
			return []factProbe{{f.edge(t, main, caller, "helper"), "<unresolved>"}}
		},
	},
	{
		rule: "typescript_scope_ownership", name: "an imported helper binds through the import", dim: "positive",
		paths: []string{"app/main.ts"}, names: []string{"helper"},
		seed: func(t *testing.T, f *parityFixture) []factProbe {
			lib := f.file(t, "app/util.ts", "typescript")
			helper := f.symbol(t, lib, "helper", "app/util.helper", "function", "typescript")
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET visibility='public' WHERE id=?`, helper); err != nil {
				t.Fatal(err)
			}
			main := f.file(t, "app/main.ts", "typescript")
			caller := f.symbol(t, main, "run", "main.run", "function", "typescript")
			f.namedImport(t, main, "typescript", "./util", "helper")
			return []factProbe{{f.edge(t, main, caller, "helper"), "app/util.helper|typescript_module_scope|high"}}
		},
	},
	{
		rule: "typescript_scope_ownership", name: "another repository's module is not imported", dim: "repo_isolation",
		paths: []string{"app/main.ts"}, names: []string{"helper"},
		seed: func(t *testing.T, f *parityFixture) []factProbe {
			f.otherRepoFile(t, "app/util.ts", "typescript", "helper", "util.helper", "function", "")
			main := f.file(t, "app/main.ts", "typescript")
			caller := f.symbol(t, main, "run", "main.run", "function", "typescript")
			f.namedImport(t, main, "typescript", "./util", "helper")
			return []factProbe{{f.edge(t, main, caller, "helper"), "<unresolved>"}}
		},
	},
	{
		rule: "jvm_scope_ownership", name: "another repository's class is not in the package", dim: "repo_isolation",
		paths: []string{"app/Main.java"}, names: []string{"x", "Other.x"},
		seed: func(t *testing.T, f *parityFixture) []factProbe {
			f.otherRepoFile(t, "lib/Other.java", "java", "x", "lib.Other.x", "function", "Other")
			main := f.file(t, "app/Main.java", "java")
			caller := f.symbolIn(t, main, "run", "app.Main.run", "function", "Main", "java")
			return []factProbe{{f.edge(t, main, caller, "Other.x"), "<unresolved>"}}
		},
	},
	{
		rule: "csharp_scope_ownership", name: "a method of the caller's own type binds", dim: "positive",
		paths: []string{"App/Main.cs"}, names: []string{"Helper"},
		seed: func(t *testing.T, f *parityFixture) []factProbe {
			main := f.file(t, "App/Main.cs", "csharp")
			helper := f.symbolIn(t, main, "Helper", "App.Main.Helper", "function", "Main", "csharp")
			caller := f.symbolIn(t, main, "Run", "App.Main.Run", "function", "Main", "csharp")
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET visibility='public', is_static=0 WHERE id IN (?, ?)`, helper, caller); err != nil {
				t.Fatal(err)
			}
			return []factProbe{{f.edge(t, main, caller, "Helper"), "App.Main.Helper|csharp_same_type|high"}}
		},
	},
	{
		rule: "csharp_scope_ownership", name: "another repository's method is not the caller's own", dim: "repo_isolation",
		paths: []string{"App/Main.cs"}, names: []string{"Helper"},
		seed: func(t *testing.T, f *parityFixture) []factProbe {
			f.otherRepoFile(t, "App/Main.cs", "csharp", "Helper", "App.Main.Helper", "method", "Main")
			main := f.file(t, "App/Main.cs", "csharp")
			caller := f.symbolIn(t, main, "Run", "App.Main.Run", "method", "Main", "csharp")
			return []factProbe{{f.edge(t, main, caller, "Helper"), "<unresolved>"}}
		},
	},
	{
		rule: "python_scope_claims", name: "no import fact leaves the name to the generic strategies", dim: "missing_fact",
		paths: []string{"app/main.py"}, names: []string{"helper"},
		seed: func(t *testing.T, f *parityFixture) []factProbe {
			lib := f.file(t, "lib.py", "python")
			f.symbol(t, lib, "helper", "lib.helper", "function", "python")
			main := f.file(t, "app/main.py", "python")
			caller := f.symbol(t, main, "run", "main.run", "function", "python")
			return []factProbe{{f.edge(t, main, caller, "helper"), "lib.helper|exact_name|high"}}
		},
	},
	{
		rule: "jvm_scope_ownership", name: "a same-class call binds through the scope pass", dim: "positive",
		paths: []string{"app/Main.java"}, names: []string{"helper"},
		seed: func(t *testing.T, f *parityFixture) []factProbe {
			main := f.file(t, "app/Main.java", "java")
			f.symbolIn(t, main, "helper", "app.Main.helper", "function", "Main", "java")
			caller := f.symbolIn(t, main, "run", "app.Main.run", "function", "Main", "java")
			if _, err := f.store.db.ExecContext(f.ctx, `INSERT INTO file_scope_evidence(repo_id,file_id,language,package_name) VALUES(?,?,?,?)`, f.repoID, main, "java", "app"); err != nil {
				t.Fatal(err)
			}
			return []factProbe{{f.edge(t, main, caller, "helper"), "app.Main.helper|java_package_scope|high"}}
		},
	},
	{
		rule: "typescript_scope_ownership", name: "an import of a module that lacks the name does not fall back", dim: "negative",
		paths: []string{"app/main.ts"}, names: []string{"helper"},
		seed: func(t *testing.T, f *parityFixture) []factProbe {
			f.file(t, "app/util.ts", "typescript")
			other := f.file(t, "app/other.ts", "typescript")
			helper := f.symbol(t, other, "helper", "app/other.helper", "function", "typescript")
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET visibility='public' WHERE id=?`, helper); err != nil {
				t.Fatal(err)
			}
			main := f.file(t, "app/main.ts", "typescript")
			caller := f.symbol(t, main, "run", "main.run", "function", "typescript")
			f.namedImport(t, main, "typescript", "./util", "helper")
			return []factProbe{{f.edge(t, main, caller, "helper"), "<unresolved>"}}
		},
	},
	{
		rule: "bare_type_scope", name: "importing a different class leaves the name out of scope", dim: "negative",
		paths: []string{"app/main.py"}, names: []string{"Widget"},
		seed: func(t *testing.T, f *parityFixture) []factProbe {
			lib := f.file(t, "app/widgets.py", "python")
			f.symbol(t, lib, "Widget", "widgets.Widget", "class", "python")
			gadgets := f.file(t, "gadgets.py", "python")
			f.symbol(t, gadgets, "Gadget", "gadgets.Gadget", "class", "python")
			main := f.file(t, "app/main.py", "python")
			caller := f.symbol(t, main, "run", "main.run", "function", "python")
			f.namedImport(t, main, "python", "gadgets", "Gadget")
			return []factProbe{{f.edge(t, main, caller, "Widget"), "<unresolved>"}}
		},
	},
}

func allFactParityScenarios() []factScenario {
	return append(append([]factScenario(nil), factParityScenarios...), acceptanceScenarios...)
}

// The acceptance dimensions every gate rule answers for.
var acceptanceDims = []string{"positive", "negative", "missing_fact", "ambiguity", "test_shadow", "repo_isolation"}

// Cell values: "scenario" is backed by factParityScenarios entries carrying
// the rule and dimension; "test:A,B" names tests that exist in this package
// or in internal/indexer; "na: reason" says why the dimension cannot apply.
const (
	cellScenario = "scenario"
	notAmbiguity = "na: ownership is decided before candidate selection, so no ambiguity reaches the rule"
	notShadow    = "na: the rule does not look at production/test kind; TestResolverTestShadow_AllEntrypoints covers the choice"
)

func edgeLocalCells(tests string) map[string]string {
	return map[string]string{
		"positive":       "test:" + tests,
		"negative":       "test:" + tests,
		"missing_fact":   "na: edge-local, the rule reads only the edge's language, kind, spelling and evidence",
		"ambiguity":      notAmbiguity,
		"test_shadow":    notShadow,
		"repo_isolation": "na: edge-local, the rule reads no repository rows",
	}
}

func predicateOnlyCells(tests string) map[string]string {
	const reason = "na: bare C++ edges are owned by cpp_evidence_ownership and reach no generic strategy on either path, so only the predicate is comparable"
	return map[string]string{
		"positive": "test:" + tests, "negative": "test:" + tests,
		"missing_fact": reason, "ambiguity": reason, "test_shadow": reason, "repo_isolation": reason,
	}
}

var resolverAcceptanceMatrix = map[resolverRuleID]map[string]string{
	"language_gate": {
		"positive": cellScenario, "negative": cellScenario, "missing_fact": cellScenario, "repo_isolation": cellScenario,
		"ambiguity":   "na: the gate admits candidates by language; counting them is broad_ambiguity",
		"test_shadow": "na: production/test kind is caller_kind_candidate",
	},
	"caller_kind_candidate": {
		"positive": cellScenario, "negative": cellScenario, "test_shadow": cellScenario,
		"missing_fact":   "na: caller kind comes from the caller file's path, which always exists",
		"ambiguity":      "na: uniqueness is broad_ambiguity",
		"repo_isolation": "na: candidates come from the shared repo-scoped candidate relation, pinned by language_gate repo_isolation",
	},
	"cpp_evidence_ownership":   edgeLocalCells("TestResolverOwnershipRulesMatchGoTwins,TestCppScopeVetoSQLMatchesGoTwin,TestCppEvidenceOwnershipAllEntrypoints"),
	"go_bare_package_scope":    edgeLocalCells("TestResolverOwnershipRulesMatchGoTwins,TestGoPackageScopeSQLMatchesGo"),
	"ruby_ownership":           edgeLocalCells("TestResolverOwnershipRulesMatchGoTwins,TestRubyScopeVetoSQLMatchesGoTwin"),
	"php_ownership":            edgeLocalCells("TestResolverOwnershipRulesMatchGoTwins"),
	"swift_ownership":          edgeLocalCells("TestResolverOwnershipRulesMatchGoTwins"),
	"cpp_bare_namespace_scope": predicateOnlyCells("TestResolverCandidateRestrictionsMatchGoTwins"),
	"cpp_bare_member_scope":    predicateOnlyCells("TestResolverCandidateRestrictionsMatchGoTwins,TestCppBareMemberScope_AllEntrypoints"),
	"go_local_qualifier": {
		"positive":     "test:TestResolverFactTwinsMatchSQL,TestGoLocalQualifierClaimWithholdsOnEveryEntryPoint",
		"negative":     "test:TestResolverFactTwinsMatchSQL,TestGoLocalQualifierVetoSQLMatchesGoTwin",
		"missing_fact": "test:TestResolverFactTwinsMatchSQL",
		"ambiguity":    notAmbiguity, "test_shadow": notShadow,
		"repo_isolation": "na: the claim reads go_local_binding_evidence keyed by the edge's own repo_id and file_id",
	},
	"bare_type_scope": {
		"positive": cellScenario, "negative": cellScenario, "missing_fact": cellScenario, "repo_isolation": cellScenario,
		"ambiguity":   "na: the restriction refuses the chosen candidate; choosing among several is per-strategy uniqueness",
		"test_shadow": notShadow,
	},
	"jvm_scope_ownership": {
		"positive": cellScenario, "negative": cellScenario, "missing_fact": cellScenario, "repo_isolation": cellScenario,
		"ambiguity":   notAmbiguity,
		"test_shadow": notShadow,
	},
	"csharp_scope_ownership": {
		"positive": cellScenario, "missing_fact": cellScenario, "repo_isolation": cellScenario,
		"negative":    "test:TestCSharpBareCallUnknownSourceStaticnessKeepsOnlyStaticCandidates",
		"ambiguity":   notAmbiguity,
		"test_shadow": notShadow,
	},
	"typescript_scope_ownership": {
		"positive": cellScenario, "negative": cellScenario, "missing_fact": cellScenario, "repo_isolation": cellScenario,
		"ambiguity":   notAmbiguity,
		"test_shadow": notShadow,
	},
	"python_scope_claims": {
		"positive": cellScenario, "negative": cellScenario, "missing_fact": cellScenario, "repo_isolation": cellScenario,
		"ambiguity":   "test:TestPythonScopeClaimsSurviveIntoTheWeakStrategy",
		"test_shadow": notShadow,
	},
	"broad_ambiguity": {
		"positive": cellScenario, "ambiguity": cellScenario, "test_shadow": cellScenario, "repo_isolation": cellScenario,
		"negative":     "na: the rule's refusal is ambiguity itself, covered by the ambiguity dimension",
		"missing_fact": "na: the rule reads declarations only, no scope fact",
	},
	"own_module_import": {
		"positive": cellScenario, "negative": cellScenario, "missing_fact": cellScenario, "ambiguity": cellScenario, "repo_isolation": cellScenario,
		"test_shadow": "na: the claim picks a package directory; production/test choice among its files is caller_kind_candidate",
	},
}

// Every gate rule answers every dimension, every scenario backs a cell that
// says so, and every named test exists.
func TestResolverAcceptanceMatrix(t *testing.T) {
	covered := map[resolverRuleID]map[string]bool{}
	for _, sc := range allFactParityScenarios() {
		id := resolverRuleID(sc.rule)
		if covered[id] == nil {
			covered[id] = map[string]bool{}
		}
		if !slices.Contains(acceptanceDims, sc.dim) {
			t.Errorf("scenario %q has unknown dimension %q", sc.name, sc.dim)
		}
		covered[id][sc.dim] = true
	}
	tests := packageTestNames(t, ".", "../indexer")
	for _, rule := range resolverBindGateRules {
		cells, ok := resolverAcceptanceMatrix[rule.id]
		if !ok {
			t.Errorf("rule %s has no acceptance row", rule.id)
			continue
		}
		for _, dim := range acceptanceDims {
			cell, ok := cells[dim]
			switch {
			case !ok:
				t.Errorf("rule %s: dimension %s unanswered", rule.id, dim)
			case cell == cellScenario:
				if !covered[rule.id][dim] {
					t.Errorf("rule %s: dimension %s claims a scenario but none exists", rule.id, dim)
				}
			case strings.HasPrefix(cell, "test:"):
				for _, name := range strings.Split(strings.TrimPrefix(cell, "test:"), ",") {
					if !tests[name] {
						t.Errorf("rule %s: dimension %s names missing test %s", rule.id, dim, name)
					}
				}
				if covered[rule.id][dim] {
					t.Errorf("rule %s: dimension %s has a scenario but the cell names tests", rule.id, dim)
				}
			case strings.HasPrefix(cell, "na: "):
				if covered[rule.id][dim] {
					t.Errorf("rule %s: dimension %s has a scenario but is marked not applicable", rule.id, dim)
				}
			default:
				t.Errorf("rule %s: dimension %s has unrecognised cell %q", rule.id, dim, cell)
			}
		}
		for dim := range cells {
			if !slices.Contains(acceptanceDims, dim) {
				t.Errorf("rule %s: unknown dimension %s", rule.id, dim)
			}
		}
	}
	if len(resolverAcceptanceMatrix) != len(resolverBindGateRules) {
		t.Errorf("matrix has %d rows for %d gate rules", len(resolverAcceptanceMatrix), len(resolverBindGateRules))
	}
}

// packageTestNames lists the Test functions of the given package directories.
func packageTestNames(t *testing.T, dirs ...string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, dir := range dirs {
		files, err := filepath.Glob(filepath.Join(dir, "*_test.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			parsed, err := parser.ParseFile(token.NewFileSet(), file, nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			for _, decl := range parsed.Decls {
				if fn, ok := decl.(*ast.FuncDecl); ok && fn.Recv == nil && strings.HasPrefix(fn.Name.Name, "Test") {
					out[fn.Name.Name] = true
				}
			}
		}
	}
	return out
}

// lifecycleCase seeds a repository with one edge whose binding depends on one
// fact, and returns the function that puts that fact into a named state.
type lifecycleCase struct {
	rule  string
	paths []string
	names []string
	// want is the binding a fresh resolve gives the edge in each state.
	want map[string]string
	// declChange marks a fact that lives in another file's declarations: the
	// edge's own file is not reindexed, so only the name-scoped entrypoints
	// can see the change and the edge keeps its binding until they run.
	declChange bool
	seed       func(t *testing.T, f *parityFixture) (edge int64, set func(state string))
}

func (f *parityFixture) exec(t *testing.T, query string, args ...any) {
	t.Helper()
	if _, err := f.store.db.ExecContext(f.ctx, query, args...); err != nil {
		t.Fatal(err)
	}
}

var lifecycleCases = []lifecycleCase{
	{
		rule: "own_module_import", paths: []string{"cmd/main.go"}, names: []string{"Open"},
		want: map[string]string{"imported": "pkg.Open|module_import|high", "absent": "<unresolved>"},
		seed: func(t *testing.T, f *parityFixture) (int64, func(string)) {
			pkg := f.file(t, "pkg/open.go", "go")
			f.symbolIn(t, pkg, "Open", "pkg.Open", "function", "pkg", "go")
			main := f.file(t, "cmd/main.go", "go")
			caller := f.symbol(t, main, "main", "main", "function", "go")
			edge := f.edge(t, main, caller, "example.com/project/pkg.Open")
			return edge, func(state string) {
				f.exec(t, `DELETE FROM file_imports WHERE file_id = ?`, main)
				if state == "imported" {
					f.imports(t, main, "example.com/project/pkg")
				}
			}
		},
	},
	{
		rule: "python_scope_claims", paths: []string{"app/main.py"}, names: []string{"helper"},
		want: map[string]string{
			"imported":     "lib.helper|python_import_scope|high",
			"absent":       "lib.helper|exact_name|high",
			"other-name":   "<unresolved>",
			"other-module": "<unresolved>",
		},
		seed: func(t *testing.T, f *parityFixture) (int64, func(string)) {
			lib := f.file(t, "lib.py", "python")
			f.symbol(t, lib, "helper", "lib.helper", "function", "python")
			f.file(t, "empty.py", "python")
			main := f.file(t, "app/main.py", "python")
			caller := f.symbol(t, main, "run", "main.run", "function", "python")
			edge := f.edge(t, main, caller, "helper")
			return edge, func(state string) {
				f.exec(t, `DELETE FROM scope_import_evidence WHERE file_id = ?`, main)
				switch state {
				case "imported":
					f.namedImport(t, main, "python", "lib", "helper")
				case "other-name":
					f.namedImport(t, main, "python", "lib", "other")
					f.exec(t, `UPDATE scope_import_evidence SET local_name = 'helper' WHERE file_id = ?`, main)
				case "other-module":
					f.namedImport(t, main, "python", "empty", "helper")
				}
			}
		},
	},
	{
		rule: "typescript_scope_ownership", paths: []string{"app/main.ts"}, names: []string{"helper"},
		want: map[string]string{"imported": "app/util.helper|typescript_module_scope|high", "absent": "<unresolved>"},
		seed: func(t *testing.T, f *parityFixture) (int64, func(string)) {
			lib := f.file(t, "app/util.ts", "typescript")
			helper := f.symbol(t, lib, "helper", "app/util.helper", "function", "typescript")
			f.exec(t, `UPDATE symbols SET visibility='public' WHERE id=?`, helper)
			main := f.file(t, "app/main.ts", "typescript")
			caller := f.symbol(t, main, "run", "main.run", "function", "typescript")
			edge := f.edge(t, main, caller, "helper")
			return edge, func(state string) {
				f.exec(t, `DELETE FROM scope_import_evidence WHERE file_id = ?`, main)
				if state == "imported" {
					f.namedImport(t, main, "typescript", "./util", "helper")
				}
			}
		},
	},
	{
		rule: "jvm_scope_ownership", paths: []string{"app/Main.java"}, names: []string{"helper"},
		want: map[string]string{"scoped": "app.Main.helper|java_package_scope|high", "absent": "app.Main.helper|exact_name|high"},
		seed: func(t *testing.T, f *parityFixture) (int64, func(string)) {
			main := f.file(t, "app/Main.java", "java")
			f.symbolIn(t, main, "helper", "app.Main.helper", "function", "Main", "java")
			caller := f.symbolIn(t, main, "run", "app.Main.run", "function", "Main", "java")
			edge := f.edge(t, main, caller, "helper")
			return edge, func(state string) {
				f.exec(t, `DELETE FROM file_scope_evidence WHERE file_id = ?`, main)
				if state == "scoped" {
					f.exec(t, `INSERT INTO file_scope_evidence(repo_id,file_id,language,package_name) VALUES(?,?,?,?)`, f.repoID, main, "java", "app")
				}
			}
		},
	},
	{
		rule: "broad_ambiguity", paths: []string{"app/main.py"}, names: []string{"helper"}, declChange: true,
		want: map[string]string{"unique": "util.helper|exact_name|high", "competing": "<unresolved>"},
		seed: func(t *testing.T, f *parityFixture) (int64, func(string)) {
			lib := f.file(t, "app/util.py", "python")
			f.symbol(t, lib, "helper", "util.helper", "function", "python")
			other := f.file(t, "app/other.py", "python")
			main := f.file(t, "app/main.py", "python")
			caller := f.symbol(t, main, "run", "main.run", "function", "python")
			edge := f.edge(t, main, caller, "helper")
			return edge, func(state string) {
				f.exec(t, `DELETE FROM symbols WHERE file_id = ?`, other)
				if state == "competing" {
					f.symbol(t, other, "helper", "other.helper", "function", "python")
				}
			}
		},
	},
}

// Changing a fact and resolving incrementally must give the binding a fresh
// resolve gives the changed state, for every transition between states and
// every incremental entrypoint. A fact of the caller's file changes together
// with a reindex of that file, which re-extracts its edges unresolved.
func TestResolverGateRuleFactLifecycleConvergence(t *testing.T) {
	known := map[resolverRuleID]bool{}
	for _, rule := range resolverBindGateRules {
		known[rule.id] = true
	}
	for _, lc := range lifecycleCases {
		if !known[resolverRuleID(lc.rule)] {
			t.Fatalf("lifecycle case names unknown rule %q", lc.rule)
		}
		states := slices.Sorted(maps.Keys(lc.want))
		fresh := func(state string) string {
			f := newParityFixture(t, "module example.com/project\n")
			edge, set := lc.seed(t, f)
			set(state)
			f.resolveVia(t, "full", nil, nil)
			return f.binding(t, edge)
		}
		for _, state := range states {
			if got := fresh(state); got != lc.want[state] {
				t.Errorf("%s: fresh %q bound %q, want %q", lc.rule, state, got, lc.want[state])
			}
		}
		for _, from := range states {
			for _, to := range states {
				if from == to {
					continue
				}
				entries := []string{"paths", "names", "paths+names"}
				if lc.declChange {
					entries = []string{"names", "paths+names"}
				}
				for _, entry := range entries {
					f := newParityFixture(t, "module example.com/project\n")
					edge, set := lc.seed(t, f)
					set(from)
					f.resolveVia(t, "full", nil, nil)
					set(to)
					if !lc.declChange {
						f.clearAll(t)
					}
					f.resolveVia(t, entry, lc.paths, lc.names)
					if got := f.binding(t, edge); got != lc.want[to] {
						t.Errorf("%s: %s -> %s via %s bound %q, fresh %q", lc.rule, from, to, entry, got, lc.want[to])
					}
				}
			}
		}
	}
}
