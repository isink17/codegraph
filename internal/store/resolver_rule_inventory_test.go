package store

import (
	"context"
	"slices"
	"testing"
)

// The composed gate is pinned byte for byte against the hand-written
// concatenation it replaced, so moving rules into the typed inventory cannot
// change a single resolver statement.
func TestResolverGateSQLMatchesHandComposedGate(t *testing.T) {
	handBindable := resolverLanguageGateSQL + `
		AND (` + resolverChosenCandidateSQL + `) IS NOT NULL
		AND NOT (f.language = 'cpp' AND edges.evidence LIKE 'macro_unexpanded:%')
		AND NOT (f.language = 'cpp' AND instr(edges.dst_name, '.') = 0 AND instr(edges.dst_name, '::') = 0)
		AND NOT (f.language = 'cpp' AND (instr(edges.dst_name, '.') = 0 OR instr(edges.dst_name, '::') > 0))
		AND ` + resolverGoBareScopeSQL + `
		AND ` + resolverGoLocalQualifierSQL + `
		AND ` + resolverBareNameTypeScopeSQL + `
		AND ` + resolverCppBareNamespaceScopeSQL + `
		AND ` + resolverCppBareMemberScopeSQL + `
		AND ` + rubyScopeVetoSQL + `
		AND NOT ` + resolverJVMScopeVetoSQL + `
		AND NOT EXISTS (SELECT 1 FROM ` + csharpScopeVeto + ` csv WHERE csv.edge_id = edges.id)` + `
		AND NOT EXISTS (SELECT 1 FROM ` + tsScopeVeto + ` tsv WHERE tsv.edge_id = edges.id)` + `
		AND NOT EXISTS (SELECT 1 FROM ` + pyScopeVeto + ` psv WHERE psv.edge_id = edges.id)` + `
		AND ` + phpScopeVetoSQL + `
		AND ` + swiftScopeVetoSQL
	handGate := handBindable + `
		AND ` + resolverAmbiguousNamesSQL + `
		AND NOT EXISTS (
			SELECT 1 FROM tmp_resolver_own_module_veto v
			WHERE v.edge_id = edges.id
		)`
	if resolverBindableCandidateSQL != handBindable {
		t.Fatalf("resolverBindableCandidateSQL drifted:\n got: %q\nwant: %q", resolverBindableCandidateSQL, handBindable)
	}
	if resolverBindGateSQL != handGate {
		t.Fatalf("resolverBindGateSQL drifted:\n got: %q\nwant: %q", resolverBindGateSQL, handGate)
	}
}

// resolverRuleParity records, for every rule in the inventory, where its SQL
// and Go spellings are compared -- or why no edge-level comparison exists. A
// rule added to the inventory without an entry here fails
// TestResolverGateRuleInventory, so the parity question is answered when the
// rule is written rather than discovered later.
var resolverRuleParity = map[resolverRuleID]string{
	"language_gate":              "behavioural only: TestResolverLanguageGating_AllEntrypoints; the Go side keys candidates by language",
	"caller_kind_candidate":      "behavioural only: TestResolverTestShadow_AllEntrypoints; candidateGroup.chosen",
	"cpp_evidence_ownership":     "TestResolverOwnershipRulesMatchGoTwins, TestCppScopeVetoSQLMatchesGoTwin",
	"go_bare_package_scope":      "TestResolverOwnershipRulesMatchGoTwins",
	"go_local_qualifier":         "TestGoLocalQualifierVetoSQLMatchesGoTwin",
	"bare_type_scope":            "behavioural only: TestResolverTypeScope_AllEntrypoints; reads import scope facts",
	"cpp_bare_namespace_scope":   "no comparison: bare C++ edges never reach a generic strategy (cpp_evidence_ownership)",
	"cpp_bare_member_scope":      "behavioural only: TestCppBareMemberScope_AllEntrypoints; kind list in TestCppClassMemberKindsSQLMatchesGoTwin",
	"ruby_ownership":             "TestResolverOwnershipRulesMatchGoTwins, TestRubyScopeVetoSQLMatchesGoTwin",
	"jvm_scope_ownership":        "behavioural only: reads file_scope_evidence; JVM fresh/incremental parity tests",
	"csharp_scope_ownership":     "behavioural only: veto table filled by resolveCSharpScope",
	"typescript_scope_ownership": "behavioural only: veto table filled by resolveTypeScriptScope",
	"python_scope_claims":        "behavioural only: TestPythonScopeClaimsSurviveIntoTheWeakStrategy",
	"php_ownership":              "TestResolverOwnershipRulesMatchGoTwins",
	"swift_ownership":            "TestResolverOwnershipRulesMatchGoTwins",
	"broad_ambiguity":            "behavioural only: TestResolverAmbiguity_AllEntrypoints; candidateGroup.levelUndecidedFor",
	"own_module_import":          "behavioural only: TestOwnModuleImportResolvesOnEveryEntryPoint",
}

func TestResolverGateRuleInventory(t *testing.T) {
	seen := map[resolverRuleID]bool{}
	for _, rule := range resolverBindGateRules {
		if rule.id == "" || seen[rule.id] {
			t.Errorf("rule id %q empty or duplicated", rule.id)
		}
		seen[rule.id] = true
		if rule.stage == 0 || rule.disposition == 0 || rule.sql == "" {
			t.Errorf("rule %s: stage, disposition and sql are required", rule.id)
		}
		if rule.owns != nil && rule.stage != resolverStageOwnership {
			t.Errorf("rule %s: only an ownership rule has an edge-local Go twin", rule.id)
		}
		if _, ok := resolverRuleParity[rule.id]; !ok {
			t.Errorf("rule %s has no parity record", rule.id)
		}
	}
	for id := range resolverRuleParity {
		if !seen[id] {
			t.Errorf("parity record for %s names no rule", id)
		}
	}
	if len(resolverBindableCandidateRules) >= len(resolverBindGateRules) {
		t.Fatal("the bind gate must extend the bindable-candidate rules")
	}
}

// Every edge-local ownership rule is evaluated by SQL and by its Go twin over
// one shared edge matrix, so the repo-wide and incremental paths withhold the
// same edges from the generic strategies.
func TestResolverOwnershipRulesMatchGoTwins(t *testing.T) {
	ctx := context.Background()
	s, repo := openBudgetStore(t)
	languages := []string{"", "cpp", "go", "php", "python", "ruby", "rust", "swift"}
	files := map[string]int64{}
	for i, language := range languages {
		id, err := insertTestFileLang(ctx, s, repo.ID, "src"+string(rune('a'+i)), language)
		if err != nil {
			t.Fatal(err)
		}
		files[language] = id
	}
	caller, err := insertTestSymbolLang(ctx, s, repo.ID, files["go"], "caller", "main.caller", "go")
	if err != nil {
		t.Fatal(err)
	}
	// Edges with an empty spelling are excluded: no strategy on either side
	// binds one, and the Go twins state their domain as named calls.
	names := []string{"foo", "pkg.Foo", "a/b.Foo", "a/b", "A::foo", "::foo", "x:y", "$o->foo", "obj.foo<int>", "a.b.c"}
	kinds := []string{EdgeKindCalls, EdgeKindCrossLanguageRef}
	evidences := []string{"", "direct:foo", "swift:call", "macro_unexpanded:M"}
	byID := map[int64]edgeTarget{}
	for _, language := range languages {
		for _, name := range names {
			for _, kind := range kinds {
				for _, evidence := range evidences {
					res, err := s.db.ExecContext(ctx, `
						INSERT INTO edges(repo_id, src_symbol_id, dst_name, edge_kind, evidence, file_id, line)
						VALUES(?, ?, ?, ?, ?, ?, 1)`, repo.ID, caller, name, kind, evidence, files[language])
					if err != nil {
						t.Fatal(err)
					}
					id, _ := res.LastInsertId()
					byID[id] = edgeTarget{edgeID: id, srcLanguage: language, dstName: name, edgeKind: kind, evidence: evidence}
				}
			}
		}
	}

	twins := 0
	for _, rule := range resolverBindGateRules {
		if rule.owns == nil {
			continue
		}
		twins++
		rows, err := s.db.QueryContext(ctx, `
			SELECT edges.id FROM edges JOIN files f ON f.id = edges.file_id
			WHERE edges.repo_id = ? AND NOT (`+rule.sql+`)`, repo.ID)
		if err != nil {
			t.Fatalf("%s: %v", rule.id, err)
		}
		ownedBySQL := map[int64]bool{}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			ownedBySQL[id] = true
		}
		if err := rows.Close(); err != nil {
			t.Fatal(err)
		}
		owned := 0
		for id, target := range byID {
			goOwned := rule.owns(target)
			if ownedBySQL[id] != goOwned {
				t.Errorf("%s: %q %q %s [%s]: SQL owned=%v, Go owned=%v", rule.id,
					target.srcLanguage, target.dstName, target.edgeKind, target.evidence, ownedBySQL[id], goOwned)
			}
			if goOwned {
				owned++
				if !slices.Contains(rule.languages, target.srcLanguage) {
					t.Errorf("%s owns a %q edge outside its languages %v", rule.id, target.srcLanguage, rule.languages)
				}
			}
		}
		if owned == 0 || owned == len(byID) {
			t.Errorf("%s owns %d of %d edges; the matrix does not separate the two sides", rule.id, owned, len(byID))
		}
	}
	if twins == 0 {
		t.Fatal("no rule carries a Go twin; the test would pass vacuously")
	}
}
