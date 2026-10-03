package store

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// factTwinFixture is one graph holding the edges every fact-decided rule is
// judged on: Go own-module import spellings, Go selector calls on a locally
// bound qualifier, and dotted spellings whose bare-name level is decided or
// undecided for a production and for a test caller.
type factTwinFixture struct {
	*parityFixture
	edges map[int64]edgeTarget
	label map[int64]string
}

func newFactTwinFixture(t *testing.T) *factTwinFixture {
	f := &factTwinFixture{parityFixture: newParityFixture(t, "module example.com/project\n"),
		edges: map[int64]edgeTarget{}, label: map[int64]string{}}
	add := func(fileID, caller int64, language, callerKind, spelling string) {
		id := f.edge(t, fileID, caller, spelling)
		f.edges[id] = edgeTarget{edgeID: id, srcLanguage: language, srcFileID: fileID, dstName: spelling, edgeKind: "call"}
		f.label[id] = fmt.Sprintf("%s %s %q", language, callerKind, spelling)
	}

	// Go: an own-module package, a caller importing it, and a local binding of
	// `store` on the caller's only line.
	pkg := f.file(t, "pkg/open.go", "go")
	f.symbolIn(t, pkg, "Open", "pkg.Open", "function", "pkg", "go")
	goFile := f.file(t, "cmd/main.go", "go")
	goCaller := f.symbolIn(t, goFile, "main", "main.main", "function", "main", "go")
	f.imports(t, goFile, "example.com/project/pkg")
	if _, err := f.store.db.ExecContext(f.ctx, `INSERT INTO go_local_binding_evidence(repo_id, file_id, name, scope_start_line, scope_end_line, type_name, type_package, type_import_path, is_pointer)
		VALUES(?, ?, 'store', 1, 1, 'Thing', '', '', 1)`, f.repoID, goFile); err != nil {
		t.Fatal(err)
	}
	for _, spelling := range []string{
		"example.com/project/pkg.Open",    // own module, one target
		"example.com/project/pkg.Missing", // own module, no target: still vetoed
		"example.com/project/zzz.Open",    // not an imported package
		"example.com/other/pkg.Open",      // not this module
		"store.Get", "store.inner.Get",    // locally bound qualifier
		"other.Get", "Open", "pkg.Open",
	} {
		add(goFile, goCaller, "go", "production", spelling)
	}

	// Python: dotted spellings and their bare-name level. Symbols whose name
	// itself carries the dot are what that level counts.
	lib := f.file(t, "app/lib.py", "python")
	fixtures := f.file(t, "tests/fixtures.py", "python")
	declare := func(file int64, name string) {
		f.symbol(t, file, name, "lib."+strings.ReplaceAll(name, ".", "_"), "function", "python")
	}
	declare(lib, "Two.run")
	declare(lib, "Two.run")
	declare(lib, "One.run")
	declare(fixtures, "Tst.run")
	declare(lib, "Mix.run")
	declare(fixtures, "Mix.run")
	declare(lib, "A.b.run")
	declare(lib, "A.b.run")
	declare(lib, "Two")
	declare(lib, "Two")
	// A qualified match takes the spelling out of the twin's domain: the
	// qualified level decides it, by selection.
	f.symbol(t, lib, "run", "Q.run", "function", "python")
	declare(lib, "Q.run")
	declare(lib, "Q.run")

	spellings := []string{"Two.run", "One.run", "Tst.run", "Mix.run", "None.run", "A.b.run", "Two", "Q.run"}
	prodFile := f.file(t, "app/main.py", "python")
	prodCaller := f.symbol(t, prodFile, "main", "main.main", "function", "python")
	testFile := f.file(t, "tests/test_main.py", "python")
	testCaller := f.symbol(t, testFile, "test_main", "test_main.test_main", "function", "python")
	tsFile := f.file(t, "web/main.ts", "typescript")
	tsCaller := f.symbol(t, tsFile, "main", "main.main", "function", "typescript")
	for _, spelling := range spellings {
		add(prodFile, prodCaller, "python", "production", spelling)
		add(testFile, testCaller, "python", "test", spelling)
		add(tsFile, tsCaller, "typescript", "production", spelling)
	}
	return f
}

// sqlWithheld evaluates a rule's SQL against every edge inside the temp
// relations a repo-wide resolve prepares, after the own-module pass has
// written its veto. The transaction is rolled back.
func (f *factTwinFixture) sqlWithheld(t *testing.T, rule resolverGateRule) map[int64]bool {
	t.Helper()
	tx, err := f.store.db.BeginTx(f.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := f.store.prepareResolverTables(f.ctx, tx, f.repoID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.store.resolveOwnModuleImports(f.ctx, tx, f.repoID, nil); err != nil {
		t.Fatal(err)
	}
	rows, err := tx.QueryContext(f.ctx, `
		SELECT edges.id FROM edges
		JOIN files f ON f.id = edges.file_id
		`+resolverCallerTestJoinSQL+`
		WHERE edges.repo_id = ? AND NOT (`+rule.sql+`)`, f.repoID)
	if err != nil {
		t.Fatalf("%s: %v", rule.id, err)
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		out[id] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// binderFacts loads the facts with the loaders the binder itself uses. The
// own-module pass binds what it can, so it runs after every SQL evaluation.
func (f *factTwinFixture) binderFacts(t *testing.T) *binderCandidateFacts {
	t.Helper()
	ctx, s, repoID := f.ctx, f.store, f.repoID
	var facts binderCandidateFacts
	var err error
	if facts.testFileIDs, err = testFileIDsForRepo(ctx, s.db, repoID); err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, target := range f.edges {
		names = append(names, target.dstName)
	}
	names = slices.Compact(slices.Sorted(slices.Values(names)))
	if facts.byQualified, err = s.resolveSymbolsByQualifiedNames(ctx, repoID, names, facts.testFileIDs); err != nil {
		t.Fatal(err)
	}
	if facts.bareLevel, err = s.resolveSymbolCandidates(ctx, repoID, "name", names, facts.testFileIDs); err != nil {
		t.Fatal(err)
	}
	if facts.goLocalClaims, err = goLocalQualifierClaims(ctx, s.db, repoID); err != nil {
		t.Fatal(err)
	}
	if facts.ownModuleVeto, err = s.resolveOwnModuleImportsStandalone(ctx, repoID, nil); err != nil {
		t.Fatal(err)
	}
	return &facts
}

// inDomain reports whether a fact-decided twin claims to decide the edge.
// broad_ambiguity's twin covers only dotted spellings whose qualified name
// matched nothing in the caller's language; this restates that from the
// stored symbols rather than from the twin.
func (f *factTwinFixture) inDomain(t *testing.T, id resolverRuleID, target edgeTarget) bool {
	t.Helper()
	if id != ruleBroadAmbiguity {
		return true
	}
	dots := strings.Count(target.dstName, ".")
	if dots < 1 || dots > 2 || strings.ContainsAny(target.dstName, "/:") {
		return false
	}
	var qualified int
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM symbols WHERE repo_id = ? AND qualified_name = ? AND language = ?`,
		f.repoID, target.dstName, target.srcLanguage).Scan(&qualified); err != nil {
		t.Fatal(err)
	}
	return qualified == 0
}

// Every rule the binder decides from loaded facts is evaluated by its SQL and
// by the binder's twin over one shared edge matrix, each side reading the
// facts its own path loads. The twin may never withhold an edge the SQL
// admits; inside its domain it must withhold exactly what the SQL withholds,
// and both outcomes must occur there. The oracle rows pin the cases the
// rules exist for, so two sides wrong the same way fail too.
func TestResolverFactTwinsMatchSQL(t *testing.T) {
	f := newFactTwinFixture(t)
	bySQL := map[resolverRuleID]map[int64]bool{}
	for _, rule := range resolverBindGateRules {
		if rule.withholds != nil {
			bySQL[rule.id] = f.sqlWithheld(t, rule)
		}
	}
	facts := f.binderFacts(t)
	oracle := map[resolverRuleID]map[string]bool{
		ruleGoLocalQualifier: {
			`go production "store.Get"`:       true,
			`go production "store.inner.Get"`: true,
			`go production "other.Get"`:       false,
			`go production "pkg.Open"`:        false,
		},
		ruleOwnModuleImport: {
			`go production "example.com/project/pkg.Open"`:    true,
			`go production "example.com/project/pkg.Missing"`: true,
			`go production "example.com/project/zzz.Open"`:    false,
			`go production "example.com/other/pkg.Open"`:      false,
			`go production "pkg.Open"`:                        false,
		},
		ruleBroadAmbiguity: {
			`python production "Two.run"`:     true,
			`python test "Two.run"`:           true,
			`python production "One.run"`:     false,
			`python test "One.run"`:           false,
			`python production "Tst.run"`:     true,
			`python test "Tst.run"`:           false,
			`python production "Mix.run"`:     false,
			`python test "Mix.run"`:           true,
			`python production "None.run"`:    false,
			`python production "A.b.run"`:     true,
			`python production "Two"`:         false,
			`python production "Q.run"`:       false,
			`typescript production "Two.run"`: false,
		},
	}
	if _, ok := facts.testFileIDs[f.edgeByLabel(t, `python test "Two.run"`).srcFileID]; !ok {
		t.Fatal("the test caller's file is not a test file; the fixture does not separate caller kinds")
	}
	for _, rule := range resolverBindGateRules {
		if rule.withholds == nil {
			continue
		}
		withheld, admitted := 0, 0
		seen := map[string]bool{}
		for id, target := range f.edges {
			goWithholds := rule.withholds(target, facts)
			sqlWithholds := bySQL[rule.id][id]
			seen[f.label[id]] = goWithholds
			if goWithholds && !sqlWithholds {
				t.Errorf("%s: %s: the Go twin withholds an edge the SQL admits", rule.id, f.label[id])
			}
			if !f.inDomain(t, rule.id, target) {
				continue
			}
			if goWithholds != sqlWithholds {
				t.Errorf("%s: %s: SQL withholds=%v, Go withholds=%v", rule.id, f.label[id], sqlWithholds, goWithholds)
			}
			if goWithholds {
				withheld++
				if rule.languages != nil && !slices.Contains(rule.languages, target.srcLanguage) {
					t.Errorf("%s withholds %s outside its languages %v", rule.id, f.label[id], rule.languages)
				}
			} else {
				admitted++
			}
		}
		if withheld == 0 || admitted == 0 {
			t.Errorf("%s withholds %d and admits %d edges in its domain; the matrix does not exercise it", rule.id, withheld, admitted)
		}
		cases := oracle[rule.id]
		if len(cases) == 0 {
			t.Errorf("%s has no oracle rows", rule.id)
		}
		for label, want := range cases {
			got, ok := seen[label]
			if !ok {
				t.Errorf("%s: oracle row %s is not in the matrix", rule.id, label)
			} else if got != want {
				t.Errorf("%s: %s withheld=%v, want %v", rule.id, label, got, want)
			}
		}
	}
	if len(bySQL) != len(oracle) {
		t.Fatalf("%d rules carry a fact twin, %d have oracle rows", len(bySQL), len(oracle))
	}
}

func (f *factTwinFixture) edgeByLabel(t *testing.T, label string) edgeTarget {
	t.Helper()
	for id, l := range f.label {
		if l == label {
			return f.edges[id]
		}
	}
	t.Fatalf("no edge %s", label)
	return edgeTarget{}
}

// The binder routes exactly the inventory's fact-decided twins, and only
// rules that withhold an edge from the generic lookups carry one: ownership,
// broad ambiguity and own-module. caller_kind_candidate and the per-level
// uniqueness of broad_ambiguity are the binder's choice itself
// (candidateGroup.chosen), not a refusal of it.
func TestBinderFactTwinsComeFromInventory(t *testing.T) {
	for _, rule := range resolverBindGateRules {
		if rule.withholds == nil {
			continue
		}
		switch rule.stage {
		case resolverStageOwnership, resolverStageBroadAmbiguity, resolverStageOwnModule:
		default:
			t.Errorf("rule %s: a %d-stage rule has a fact twin", rule.id, rule.stage)
		}
		if rule.owns != nil || rule.refuses != nil {
			t.Errorf("rule %s carries more than one Go twin", rule.id)
		}
		if binderFactRoutes[rule.id] == nil {
			t.Errorf("rule %s has a fact twin the binder does not route", rule.id)
		}
	}
	for id, route := range binderFactRoutes {
		if route == nil {
			t.Errorf("binder route %s has no predicate", id)
		}
	}
	if len(binderFactRoutes) != 3 {
		t.Fatalf("binder routes %d fact twins, want go_local_qualifier, broad_ambiguity, own_module_import", len(binderFactRoutes))
	}
}

// A Go selector call whose qualifier the calling file binds locally is the
// receiver pass's to answer, and a package of the same name is no evidence:
// when the local's type proves no method, the edge stays unresolved on every
// entry point rather than binding the package function the spelling
// coincides with. The binder enforces this through the go_local_qualifier
// route alone; nothing else between the claim and the qualified lookup
// withholds the edge.
func TestGoLocalQualifierClaimWithholdsOnEveryEntryPoint(t *testing.T) {
	for _, entry := range []string{"full", "paths", "names", "paths+names"} {
		t.Run(entry, func(t *testing.T) {
			f := newParityFixture(t, "module example.com/project\n")
			pkg := f.file(t, "store/store.go", "go")
			f.symbolIn(t, pkg, "Get", "store.Get", "function", "store", "go")
			caller := f.file(t, "cmd/main.go", "go")
			src := f.symbolIn(t, caller, "main", "main.main", "function", "main", "go")
			if _, err := f.store.db.ExecContext(f.ctx, `INSERT INTO go_local_binding_evidence(repo_id, file_id, name, scope_start_line, scope_end_line, type_name, type_package, type_import_path, is_pointer)
				VALUES(?, ?, 'store', 1, 1, 'Thing', '', '', 1)`, f.repoID, caller); err != nil {
				t.Fatal(err)
			}
			edge := f.edge(t, caller, src, "store.Get")
			f.resolveVia(t, entry, []string{"cmd/main.go"}, []string{"Get"})
			if got := f.binding(t, edge); got != "<unresolved>" {
				t.Fatalf("%s: a locally bound qualifier bound the package function %s", entry, got)
			}
		})
	}
}

// edgeRefusalReasons reports, per unresolved edge, only rules whose SQL
// conjunct refuses that edge, names the expected rules on the cases they exist
// for, reports nothing it cannot decide without writing (own-module), skips a
// resolved edge, and leaves the database unchanged.
func TestEdgeRefusalReasonsAreDecideOnly(t *testing.T) {
	f := newFactTwinFixture(t)
	resolved := f.edge(t, f.edgeByLabel(t, `go production "Open"`).srcFileID, 0, "Open")
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET dst_symbol_id = (SELECT MIN(id) FROM symbols) WHERE id = ?`, resolved); err != nil {
		t.Fatal(err)
	}
	snapshot := func() string {
		var out string
		if err := f.store.db.QueryRowContext(f.ctx, `SELECT group_concat(id || ':' || ifnull(dst_symbol_id, '') || ':' || resolution_strategy, ',') FROM (SELECT * FROM edges ORDER BY id)`).Scan(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	before := snapshot()
	ids := []int64{resolved}
	for id := range f.edges {
		ids = append(ids, id)
	}
	reasons, err := f.store.edgeRefusalReasons(f.ctx, f.repoID, ids)
	if err != nil {
		t.Fatal(err)
	}
	if after := snapshot(); after != before {
		t.Fatalf("edgeRefusalReasons wrote to edges:\nbefore %s\nafter  %s", before, after)
	}
	if _, ok := reasons[resolved]; ok {
		t.Fatalf("a resolved edge has reasons %v", reasons[resolved])
	}

	rules := map[resolverRuleID]resolverGateRule{}
	for _, rule := range resolverBindGateRules {
		rules[rule.id] = rule
	}
	bySQL := map[resolverRuleID]map[int64]bool{}
	for id, ruleIDs := range reasons {
		for _, ruleID := range ruleIDs {
			if bySQL[ruleID] == nil {
				bySQL[ruleID] = f.sqlWithheld(t, rules[ruleID])
			}
			if !bySQL[ruleID][id] {
				t.Errorf("%s: reason %s, but its SQL admits the edge", f.label[id], ruleID)
			}
		}
	}
	want := map[string][]resolverRuleID{
		`go production "store.Get"`:                    {ruleGoLocalQualifier},
		`go production "Open"`:                         {ruleGoBarePackageScope},
		`go production "example.com/project/pkg.Open"`: nil, // own-module: not decidable read-only
		`go production "other.Get"`:                    nil,
		`python production "Two.run"`:                  {ruleBroadAmbiguity},
		`python test "Tst.run"`:                        nil,
		`python production "One.run"`:                  nil,
		`python production "Q.run"`:                    nil,
	}
	for label, wantIDs := range want {
		if got := reasons[f.edgeByLabel(t, label).edgeID]; !slices.Equal(got, wantIDs) {
			t.Errorf("%s: reasons %v, want %v", label, got, wantIDs)
		}
	}
}
