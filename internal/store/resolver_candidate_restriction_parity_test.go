package store

import (
	"fmt"
	"slices"
	"testing"
)

// candidateRestrictionFixture is one graph and every (edge, chosen candidate)
// pair the chosen-candidate restrictions are judged on. A pair is an edge whose
// dst_name names the candidate, bare or qualified, as a strategy would match it.
type candidateRestrictionFixture struct {
	*gateFixture
	pairs  []candidatePair
	names  []string
	edges  []int64
	byID   map[int64]edgeTarget
	labels map[int64]string // symbol id -> qualified name, for messages
}

type candidatePair struct {
	edgeID, candidateID int64
}

func (p candidatePair) key() string { return fmt.Sprintf("%d/%d", p.edgeID, p.candidateID) }

// newCandidateRestrictionFixture builds, per gated language and one ungated
// control, a calling file that imports one declaring file and not another, and
// declares types and functions in all three; for C/C++ it adds a second
// namespace and two classes with members, one of which the second caller
// belongs to.
func newCandidateRestrictionFixture(t *testing.T) *candidateRestrictionFixture {
	f := &candidateRestrictionFixture{gateFixture: newGateFixture(t), byID: map[int64]edgeTarget{}, labels: map[int64]string{}}
	type decl struct {
		file                             int64
		name, qualified, kind, container string
	}
	type layout struct {
		language, prefix string
		paths            [3]string // calling file, imported file, unimported file
		importSpec       string
	}
	layouts := []layout{
		{"python", "Py", [3]string{"app/a.py", "app/b.py", "app/c.py"}, "app.b"},
		{"typescript", "Ts", [3]string{"web/a.ts", "web/b.ts", "web/c.ts"}, "./b"},
		{"cpp", "Cc", [3]string{"cc/a.cc", "cc/b.h", "cc/c.h"}, "b.h"},
		{"kotlin", "Kt", [3]string{"kt/A.kt", "kt/B.kt", "kt/C.kt"}, ""},
	}
	for _, l := range layouts {
		var files [3]int64
		for i, path := range l.paths {
			files[i] = f.file(t, path, l.language)
		}
		if l.importSpec != "" {
			if _, err := f.store.db.ExecContext(f.ctx, `INSERT INTO file_imports(repo_id, file_id, import_path) VALUES (?, ?, ?)`,
				f.repoID, files[0], l.importSpec); err != nil {
				t.Fatal(err)
			}
		}
		var decls []decl
		for i, letter := range []string{"A", "B", "C"} {
			base := l.prefix + letter
			if l.language == "cpp" {
				decls = append(decls,
					decl{files[i], base + "Type", "ns1::" + base + "Type", "class", "ns1"},
					decl{files[i], base + "Fn", "ns1::" + base + "Fn", "function", "ns1"},
					decl{files[i], base + "Other", "ns2::" + base + "Other", "function", "ns2"},
					decl{files[i], base + "Box", "ns1::" + base + "Box", "class", "ns1"},
					decl{files[i], base + "BoxRun", "ns1::" + base + "Box::" + base + "BoxRun", "method", base + "Box"},
				)
				continue
			}
			module := l.paths[i][len(l.paths[i])-4 : len(l.paths[i])-3]
			decls = append(decls,
				decl{files[i], base + "Type", module + "." + base + "Type", "class", ""},
				decl{files[i], base + "Fn", module + "." + base + "Fn", "function", ""},
			)
		}
		var candidates []int64
		var spellings [][2]string // bare, qualified
		for _, d := range decls {
			id := f.symbolWithContainer(t, d.file, d.name, d.qualified, d.kind, d.container, l.language)
			f.labels[id] = d.qualified
			candidates = append(candidates, id)
			spellings = append(spellings, [2]string{d.name, d.qualified})
		}
		callers := []int64{f.symbolWithContainer(t, files[0], "caller", "caller."+l.prefix, "function", "ns1", l.language)}
		if l.language == "cpp" {
			callers = append(callers, f.symbolWithContainer(t, files[0], "boxCaller", "ns1::CcABox::boxCaller", "method", "CcABox", l.language))
		}
		for _, caller := range callers {
			for i, candidate := range candidates {
				for _, spelling := range spellings[i] {
					id := f.edge(t, files[0], caller, spelling)
					f.byID[id] = edgeTarget{edgeID: id, srcLanguage: l.language, srcFileID: files[0], dstName: spelling, edgeKind: "call"}
					f.edges = append(f.edges, id)
					f.pairs = append(f.pairs, candidatePair{edgeID: id, candidateID: candidate})
					f.names = append(f.names, spelling)
				}
			}
		}
	}
	return f
}

// sqlRefusals evaluates a rule's SQL against every pair, with the candidate
// relation `r` holding the pair's candidate as the sole choice for either
// caller kind, inside the temp relations a repo-wide resolve prepares.
func (f *candidateRestrictionFixture) sqlRefusals(t *testing.T, rule resolverGateRule) map[string]bool {
	t.Helper()
	tx, err := f.store.db.BeginTx(f.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := f.store.prepareResolverTables(f.ctx, tx, f.repoID); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(f.ctx, `CREATE TEMP TABLE tmp_candidate_pairs(edge_id INTEGER NOT NULL, cand_id INTEGER NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	for _, p := range f.pairs {
		if _, err := tx.ExecContext(f.ctx, `INSERT INTO tmp_candidate_pairs VALUES (?, ?)`, p.edgeID, p.candidateID); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := tx.QueryContext(f.ctx, `
		SELECT r.edge_id, r.cand_id
		FROM (SELECT edge_id, cand_id, cand_id AS any_symbol_id, cand_id AS production_symbol_id FROM tmp_candidate_pairs) r
		JOIN edges ON edges.id = r.edge_id
		JOIN files f ON f.id = edges.file_id
		`+resolverCallerTestJoinSQL+`
		WHERE NOT (`+rule.sql+`)`)
	if err != nil {
		t.Fatalf("%s: %v", rule.id, err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var p candidatePair
		if err := rows.Scan(&p.edgeID, &p.candidateID); err != nil {
			t.Fatal(err)
		}
		out[p.key()] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// binderFacts loads the facts with the loaders the binder itself uses.
func (f *candidateRestrictionFixture) binderFacts(t *testing.T) *binderCandidateFacts {
	t.Helper()
	ctx, s, repoID := f.ctx, f.store, f.repoID
	testFiles, err := testFileIDsForRepo(ctx, s.db, repoID)
	if err != nil {
		t.Fatal(err)
	}
	names := slices.Compact(slices.Sorted(slices.Values(f.names)))
	var facts binderCandidateFacts
	if facts.byQualified, err = s.resolveSymbolsByQualifiedNames(ctx, repoID, names, testFiles); err != nil {
		t.Fatal(err)
	}
	if facts.byShort, err = s.resolveUniqueSymbolsByNames(ctx, repoID, names, testFiles); err != nil {
		t.Fatal(err)
	}
	if facts.importScope, err = newImportScopeCache(s, repoID).get(ctx, s, repoID); err != nil {
		t.Fatal(err)
	}
	if facts.cppMemberTargets, err = cppClassScopesByName(ctx, s.db, repoID, names); err != nil {
		t.Fatal(err)
	}
	if facts.cppCallerClasses, err = cppCallerClassScopesByEdge(ctx, s.db, repoID, f.edges); err != nil {
		t.Fatal(err)
	}
	if facts.cppNamespaceTargets, err = cppNamespaceScopesByName(ctx, s.db, repoID, names); err != nil {
		t.Fatal(err)
	}
	if facts.cppCallerNamespaces, err = cppNamespaceScopesByEdge(ctx, s.db, repoID, f.edges); err != nil {
		t.Fatal(err)
	}
	return &facts
}

func (f *candidateRestrictionFixture) describe(p candidatePair) string {
	target := f.byID[p.edgeID]
	return fmt.Sprintf("%s edge %q -> %s", target.srcLanguage, target.dstName, f.labels[p.candidateID])
}

// Every chosen-candidate restriction with a Go twin is evaluated by its SQL
// and by the binder's twin over one shared pair matrix, with each side
// reading the facts its own path loads. Each rule must refuse and admit some
// pairs, refuse only inside its languages, and agree with the oracle rows
// below, so a matrix that never exercises a rule, or two sides wrong the same
// way on the named cases, fails.
func TestResolverCandidateRestrictionsMatchGoTwins(t *testing.T) {
	f := newCandidateRestrictionFixture(t)
	facts := f.binderFacts(t)
	oracle := map[resolverRuleID]map[string]bool{
		// A bare type spelling binds a type the caller's file declares or
		// imports, and nothing else; a function or a qualified spelling is
		// never refused, and kotlin is not gated.
		"bare_type_scope": {
			"python edge \"PyAType\" -> a.PyAType":     false,
			"python edge \"PyBType\" -> b.PyBType":     false,
			"python edge \"PyCType\" -> c.PyCType":     true,
			"python edge \"c.PyCType\" -> c.PyCType":   false,
			"python edge \"PyCFn\" -> c.PyCFn":         false,
			"typescript edge \"TsCType\" -> c.TsCType": true,
			"typescript edge \"TsBType\" -> b.TsBType": false,
			"cpp edge \"CcCType\" -> ns1::CcCType":     true,
			"cpp edge \"CcBType\" -> ns1::CcBType":     false,
			"kotlin edge \"KtCType\" -> C.KtCType":     false,
		},
		// A bare C/C++ spelling binds only inside the caller's namespace.
		"cpp_bare_namespace_scope": {
			"cpp edge \"CcAFn\" -> ns1::CcAFn":            false,
			"cpp edge \"CcAOther\" -> ns2::CcAOther":      true,
			"cpp edge \"ns2::CcAOther\" -> ns2::CcAOther": false,
		},
		// A bare C/C++ spelling binds a class member only from a member of
		// that class.
		"cpp_bare_member_scope": {
			"cpp edge \"CcBBoxRun\" -> ns1::CcBBox::CcBBoxRun": true,
			"cpp edge \"CcAFn\" -> ns1::CcAFn":                 false,
		},
	}
	twins := 0
	for _, rule := range resolverBindGateRules {
		if rule.refuses == nil {
			continue
		}
		twins++
		bySQL := f.sqlRefusals(t, rule)
		refused, admitted := 0, 0
		seen := map[string][]bool{}
		for _, p := range f.pairs {
			target := f.byID[p.edgeID]
			goRefuses := rule.refuses(binderChosenCandidate{target: target, dstID: p.candidateID, facts: facts})
			if bySQL[p.key()] != goRefuses {
				t.Errorf("%s: %s: SQL refuses=%v, Go refuses=%v", rule.id, f.describe(p), bySQL[p.key()], goRefuses)
			}
			if goRefuses {
				refused++
				if !slices.Contains(rule.languages, target.srcLanguage) {
					t.Errorf("%s refuses %s outside its languages %v", rule.id, f.describe(p), rule.languages)
				}
			} else if slices.Contains(rule.languages, target.srcLanguage) {
				admitted++
			}
			seen[f.describe(p)] = append(seen[f.describe(p)], goRefuses)
		}
		if refused == 0 || admitted == 0 {
			t.Errorf("%s refuses %d and admits %d in-language pairs; the matrix does not exercise it", rule.id, refused, admitted)
		}
		cases := oracle[rule.id]
		if len(cases) == 0 {
			t.Errorf("%s has no oracle rows", rule.id)
		}
		for description, want := range cases {
			got, ok := seen[description]
			if !ok {
				t.Errorf("%s: oracle row %s is not in the matrix", rule.id, description)
				continue
			}
			for _, refusedHere := range got {
				if refusedHere != want {
					t.Errorf("%s: %s refused=%v, want %v", rule.id, description, refusedHere, want)
				}
			}
		}
	}
	if twins != len(oracle) {
		t.Fatalf("%d rules carry a candidate twin, %d have oracle rows", twins, len(oracle))
	}
}

// The binder applies exactly the inventory's chosen-candidate twins, in
// inventory order, and only chosen-candidate rules carry one. The caller-kind
// rule is the choice itself (candidateGroup.chosen), not a restriction on it.
func TestBinderCandidateRestrictionsComeFromInventory(t *testing.T) {
	var want []resolverRuleID
	for _, rule := range resolverBindGateRules {
		if rule.refuses != nil && rule.stage != resolverStageChosenCandidate {
			t.Errorf("rule %s: only a chosen-candidate rule has a candidate twin", rule.id)
		}
		if rule.stage == resolverStageChosenCandidate && rule.refuses == nil && rule.id != "caller_kind_candidate" {
			t.Errorf("chosen-candidate rule %s has no Go twin; the binder would not apply it", rule.id)
		}
		if rule.refuses != nil {
			want = append(want, rule.id)
		}
	}
	var got []resolverRuleID
	for _, rule := range binderCandidateRestrictions {
		got = append(got, rule.id)
	}
	if !slices.Equal(got, want) || len(got) == 0 {
		t.Fatalf("binder restrictions %v, want %v", got, want)
	}
}
