package store

import (
	"database/sql"
	"reflect"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

type extensionStressState struct {
	total, resolved, unresolved, badMetadata int
	strategies                               map[string]int
}

func TestSwiftSuperExtensionFreshMissingMembershipUnresolved(t *testing.T) {
	f := newSwiftSuperAcceptanceFixture(t)
	swiftSetRange(t, f.swiftScopeFixture, f.child, 20, 1, 25, 20)
	swiftSetRange(t, f.swiftScopeFixture, f.caller, 6, 1, 8, 20)
	f.reference(f.mainFile, f.caller, "super.run", 7)
	var memberships int
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM swift_extension_memberships WHERE repo_id=? AND symbol_id=?`, f.repoID, f.caller).Scan(&memberships); err != nil {
		t.Fatal(err)
	}
	if memberships != 0 {
		t.Fatalf("membership rows=%d, want 0", memberships)
	}
	f.resolve()
	assertSwiftEdgeUnresolved(t, f.swiftScopeFixture, f.edge)
	assertSwiftReferenceCleared(t, f.swiftScopeFixture, swiftReferenceID(t, f.swiftScopeFixture, f.edge))
}

func TestSwiftSuperExtensionMembershipDeletionPublicRedecision(t *testing.T) {
	f := extensionStressFixture(t, "instance", false)
	f.reference(f.mainFile, f.caller, "super.run", 7)
	f.resolve()
	assertSwiftBinding(t, f.swiftScopeFixture, f.edge, f.baseRun, ResolutionStrategySwiftSuperExtensionScope)
	if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM swift_extension_memberships WHERE repo_id=? AND symbol_id=?`, f.repoID, f.caller); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftEdgeUnresolved(t, f.swiftScopeFixture, f.edge)
	assertSwiftReferenceCleared(t, f.swiftScopeFixture, swiftReferenceID(t, f.swiftScopeFixture, f.edge))
}

func TestSwiftSuperExtensionFreshDuplicateMembershipUnresolved(t *testing.T) {
	f := newSwiftSuperAcceptanceFixture(t)
	swiftSetRange(t, f.swiftScopeFixture, f.child, 20, 1, 25, 20)
	swiftSetRange(t, f.swiftScopeFixture, f.caller, 6, 1, 8, 20)
	swiftExtensionMembership(t, f.swiftScopeFixture, f.mainFile, f.caller, "Child", 5, 9, 0, 0)
	swiftExtensionMembership(t, f.swiftScopeFixture, f.mainFile, f.caller, "Child", 5, 9, 0, 0)
	f.reference(f.mainFile, f.caller, "super.run", 7)
	var count int
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM swift_extension_memberships WHERE repo_id=? AND symbol_id=?`, f.repoID, f.caller).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("membership rows=%d, want 2", count)
	}
	f.resolve()
	assertSwiftEdgeUnresolved(t, f.swiftScopeFixture, f.edge)
	assertSwiftReferenceCleared(t, f.swiftScopeFixture, swiftReferenceID(t, f.swiftScopeFixture, f.edge))
}

func TestSwiftSuperExtensionMultiEdgeDuplicateMembership(t *testing.T) {
	f := newSwiftSuperAcceptanceFixture(t)
	swiftSetRange(t, f.swiftScopeFixture, f.child, 20, 1, 25, 20)
	swiftSetRange(t, f.swiftScopeFixture, f.caller, 6, 1, 8, 20)
	swiftExtensionMembership(t, f.swiftScopeFixture, f.mainFile, f.caller, "Child", 5, 9, 0, 0)
	swiftExtensionMembership(t, f.swiftScopeFixture, f.mainFile, f.caller, "Child", 5, 9, 0, 0)
	edges := []int64{f.edge}
	for i := 1; i < 8; i++ {
		edges = append(edges, f.call(f.mainFile, f.caller, "super.run", "swift:super", 0, 7+i))
	}
	for i := range edges {
		f.reference(f.mainFile, f.caller, "super.run", 7+i)
	}
	f.resolve()
	for _, edge := range edges {
		dst, strategy, confidence := edgeState(t, f.swiftScopeFixture, edge)
		if dst.Valid || strategy != "" || confidence != "" {
			t.Fatalf("edge %d bound=(%v,%q,%q)", edge, dst, strategy, confidence)
		}
	}
	if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM swift_extension_memberships WHERE id=(SELECT MAX(id) FROM swift_extension_memberships WHERE repo_id=? AND symbol_id=?)`, f.repoID, f.caller); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET dst_symbol_id=NULL,resolution_strategy='',resolution_confidence='' WHERE repo_id=?`, f.repoID); err != nil {
		t.Fatal(err)
	}
	f.resolve()
	for _, edge := range edges {
		dst, strategy, confidence := edgeState(t, f.swiftScopeFixture, edge)
		if !dst.Valid || dst.Int64 != f.baseRun || strategy != ResolutionStrategySwiftSuperExtensionScope || confidence != ResolutionConfidenceHigh {
			t.Fatalf("edge %d valid=(%v,%q,%q)", edge, dst, strategy, confidence)
		}
	}
}

func extensionStressFixture(t *testing.T, mode string, multi bool, withMembership ...bool) *swiftSuperAcceptanceFixture {
	t.Helper()
	f := newSwiftSuperAcceptanceFixture(t)
	// Move the caller outside the nominal range and prove the extension range.
	swiftSetRange(t, f.swiftScopeFixture, f.child, 20, 1, 25, 20)
	swiftSetRange(t, f.swiftScopeFixture, f.caller, 6, 1, 8, 20)
	if len(withMembership) == 0 || withMembership[0] {
		swiftExtensionMembership(t, f.swiftScopeFixture, f.mainFile, f.caller, "Child", 5, 9, 0, 0)
	}
	if mode == "type" {
		if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET is_static=1 WHERE id IN (?,?)`, f.caller, f.baseRun); err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET dispatch_kind='class' WHERE id IN (SELECT id FROM swift_declaration_facts WHERE symbol_id IN (?,?))`, f.caller, f.baseRun); err != nil {
			t.Fatal(err)
		}
	}
	if multi {
		middle := f.symbol(f.mainFile, "Middle", "", "class", "", false)
		swiftSetRange(t, f.swiftScopeFixture, middle, 10, 1, 15, 20)
		if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET target_qualified_name='Middle' WHERE child_qualified_name='Child'`); err != nil {
			t.Fatal(err)
		}
		swiftSuperclass(t, f.swiftScopeFixture, f.mainFile, "Middle", "Base")
	}
	return f
}

func extensionStressStateOf(t *testing.T, f *swiftScopeFixture) extensionStressState {
	t.Helper()
	state := extensionStressState{strategies: map[string]int{}}
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM edges WHERE repo_id=? AND evidence LIKE 'swift:super%'`, f.repoID).Scan(&state.total); err != nil {
		t.Fatal(err)
	}
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM edges WHERE repo_id=? AND evidence LIKE 'swift:super%' AND dst_symbol_id IS NOT NULL`, f.repoID).Scan(&state.resolved); err != nil {
		t.Fatal(err)
	}
	state.unresolved = state.total - state.resolved
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM edges WHERE repo_id=? AND evidence LIKE 'swift:super%' AND dst_symbol_id IS NULL AND (resolution_strategy<>'' OR resolution_confidence<>'')`, f.repoID).Scan(&state.badMetadata); err != nil {
		t.Fatal(err)
	}
	rows, err := f.store.db.QueryContext(f.ctx, `SELECT resolution_strategy,COUNT(*) FROM edges WHERE repo_id=? AND evidence LIKE 'swift:super%' AND dst_symbol_id IS NOT NULL GROUP BY resolution_strategy`, f.repoID)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var strategy string
		var count int
		if err := rows.Scan(&strategy, &count); err != nil {
			t.Fatal(err)
		}
		state.strategies[strategy] = count
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestSwiftSuperExtensionScopeHardenedStressDuplicateAlone(t *testing.T) {
	runSwiftSuperExtensionScopeHardenedStress(t, "duplicate_membership", -1)
}

func TestSwiftSuperExtensionScopeHardenedStressCycleAlone(t *testing.T) {
	runSwiftSuperExtensionScopeHardenedStress(t, "cycle", -1)
}

func TestSwiftSuperExtensionScopeHardenedStressSelectedFinalAlone(t *testing.T) {
	runSwiftSuperExtensionScopeHardenedStress(t, "type_selected_target_final", -1)
}

func TestSwiftSuperExtensionScopeHardenedStressSelectedStaticAlone(t *testing.T) {
	runSwiftSuperExtensionScopeHardenedStress(t, "type_selected_target_static", -1)
}

func TestSwiftSuperExtensionScopeHardenedStressFinalAncestorOverrideAlone(t *testing.T) {
	runSwiftSuperExtensionScopeHardenedStress(t, "type_final_ancestor_override", -1)
}

func TestSwiftSuperExtensionScopeHardenedStressStaticAncestorOverrideAlone(t *testing.T) {
	runSwiftSuperExtensionScopeHardenedStress(t, "type_static_ancestor_override", -1)
}

func TestSwiftSuperExtensionScopeHardenedStressLegalOverrideAlone(t *testing.T) {
	runSwiftSuperExtensionScopeHardenedStress(t, "type_legal_override_control", -1)
}

func TestSwiftSuperExtensionScopeHardenedStressNominalControls(t *testing.T) {
	for _, name := range []string{"nominal_instance_control", "nominal_multilevel_control", "nominal_type_control", "nominal_type_multilevel_control"} {
		t.Run(name, func(t *testing.T) {
			runSwiftSuperExtensionScopeHardenedStress(t, name, -1)
		})
	}
}

func TestSwiftSuperExtensionScopeHardenedStressExtensionStrategyControls(t *testing.T) {
	for _, name := range []string{"direct_instance", "multilevel_instance", "direct_class", "multilevel_class"} {
		t.Run(name, func(t *testing.T) {
			runSwiftSuperExtensionScopeHardenedStress(t, name, -1)
		})
	}
}

func TestSwiftSuperExtensionScopeHardenedStressPrivateVisibilityControls(t *testing.T) {
	for _, name := range []string{"source_owner_visibility_private", "target_visibility_private"} {
		t.Run(name, func(t *testing.T) {
			runSwiftSuperExtensionScopeHardenedStress(t, name, -1)
		})
	}
}

func TestSwiftSuperExtensionDirectCycleNominal(t *testing.T) {
	f := newSwiftSuperAcceptanceFixture(t)
	swiftSuperclass(t, f.swiftScopeFixture, f.mainFile, "Base", "Child")
	f.reference(f.mainFile, f.caller, "super.run", 7)
	f.store.ResolveEdges(f.ctx, f.repoID)
	assertSwiftEdgeUnresolved(t, f.swiftScopeFixture, f.edge)
}

func TestSwiftSuperExtensionDirectCycle(t *testing.T) {
	f := extensionStressFixture(t, "instance", false)
	swiftSuperclass(t, f.swiftScopeFixture, f.mainFile, "Base", "Child")
	f.reference(f.mainFile, f.caller, "super.run", 7)
	f.resolve()
	assertSwiftEdgeUnresolved(t, f.swiftScopeFixture, f.edge)
}

func TestSwiftSuperExtensionPostTargetCyclesAndControls(t *testing.T) {
	for _, tc := range []struct {
		name, back string
		want       bool
	}{
		{"base-grand-base", "Base", false},
		{"base-grand-child", "Child", false},
		{"acyclic", "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := extensionStressFixture(t, "instance", false)
			grand := f.symbol(f.mainFile, "Grand", "", "class", "", false)
			swiftSetRange(t, f.swiftScopeFixture, grand, 30, 1, 34, 20)
			swiftSuperclass(t, f.swiftScopeFixture, f.mainFile, "Base", "Grand")
			if tc.back != "" {
				swiftSuperclass(t, f.swiftScopeFixture, f.mainFile, "Grand", tc.back)
			}
			f.reference(f.mainFile, f.caller, "super.run", 7)
			f.resolve()
			if tc.want {
				assertSwiftBinding(t, f.swiftScopeFixture, f.edge, f.baseRun, ResolutionStrategySwiftSuperExtensionScope)
			} else {
				assertSwiftEdgeUnresolved(t, f.swiftScopeFixture, f.edge)
			}
		})
	}
}

func TestSwiftSuperExtensionSelectedOwnerConcreteAncestry(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*swiftSuperAcceptanceFixture)
		valid  bool
	}{
		{"cycle_with_unproven_noise", func(f *swiftSuperAcceptanceFixture) {
			swiftSuperclass(t, f.swiftScopeFixture, f.mainFile, "Base", "Child")
			swiftRelation(t, f.swiftScopeFixture, f.mainFile, "Base", "P", "unproven")
		}, false},
		{"multiple_concrete_superclasses", func(f *swiftSuperAcceptanceFixture) {
			swiftSuperclass(t, f.swiftScopeFixture, f.mainFile, "Base", "GrandA")
			swiftSuperclass(t, f.swiftScopeFixture, f.mainFile, "Base", "GrandB")
		}, false},
		{"empty_concrete_target", func(f *swiftSuperAcceptanceFixture) {
			swiftSuperclass(t, f.swiftScopeFixture, f.mainFile, "Base", "")
		}, false},
		{"unknown_only_tail", func(f *swiftSuperAcceptanceFixture) {
			swiftRelation(t, f.swiftScopeFixture, f.mainFile, "Base", "P", "unproven")
		}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := extensionStressFixture(t, "instance", false)
			tc.mutate(f)
			f.reference(f.mainFile, f.caller, "super.run", 7)
			f.resolve()
			if tc.valid {
				assertSwiftBinding(t, f.swiftScopeFixture, f.edge, f.baseRun, ResolutionStrategySwiftSuperExtensionScope)
			} else {
				assertSwiftEdgeUnresolved(t, f.swiftScopeFixture, f.edge)
			}
		})
	}
}

func TestSwiftSuperExtensionUnknownTailPreservesDirectBinding(t *testing.T) {
	f := extensionStressFixture(t, "instance", false)
	grand := f.symbol(f.mainFile, "Grand", "", "class", "", false)
	swiftSetRange(t, f.swiftScopeFixture, grand, 30, 1, 34, 20)
	swiftSuperclass(t, f.swiftScopeFixture, f.mainFile, "Base", "Grand")
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET is_generic=1 WHERE child_qualified_name='Base'`); err != nil {
		t.Fatal(err)
	}
	f.reference(f.mainFile, f.caller, "super.run", 7)
	f.resolve()
	assertSwiftBinding(t, f.swiftScopeFixture, f.edge, f.baseRun, ResolutionStrategySwiftSuperExtensionScope)
}

func TestSwiftSuperExtensionScopeHardenedStress(t *testing.T) {
	runSwiftSuperExtensionScopeHardenedStress(t, "", -1)
}

func swiftStressExec(f *swiftSuperAcceptanceFixture, query string, args ...any) {
	f.t.Helper()
	result, err := f.store.db.ExecContext(f.ctx, query, args...)
	if err != nil {
		f.t.Fatal(err)
	}
	if changed, err := result.RowsAffected(); err != nil || changed == 0 {
		f.t.Fatalf("stress mutation rows=%d err=%v", changed, err)
	}
}

func runSwiftSuperExtensionScopeHardenedStress(t *testing.T, only string, prefix int) {
	const perCategory = 8
	categories := []struct {
		name, mode   string
		multi, valid bool
		mutate       func(*swiftSuperAcceptanceFixture)
	}{
		{"direct_instance", "instance", false, true, nil}, {"multilevel_instance", "instance", true, true, nil},
		{"direct_class", "type", false, true, nil}, {"multilevel_class", "type", true, true, nil},
		{"direct_static", "type", false, true, func(f *swiftSuperAcceptanceFixture) {
			swiftStressExec(f, `UPDATE swift_declaration_facts SET dispatch_kind='static' WHERE symbol_id=?`, f.caller)
		}},
		{"multilevel_static", "type", true, true, func(f *swiftSuperAcceptanceFixture) {
			swiftStressExec(f, `UPDATE swift_declaration_facts SET dispatch_kind='static' WHERE symbol_id=?`, f.caller)
		}},
		{"selector_zero", "instance", false, true, nil}, {"selector_label", "instance", false, true, func(f *swiftSuperAcceptanceFixture) {
			swiftStressExec(f, `UPDATE symbols SET signature='run(id:)',arity_min=1,arity_max=1 WHERE id=?`, f.baseRun)
			swiftStressExec(f, `UPDATE edges SET evidence='swift:super;labels=id:',call_arity=1 WHERE id=?`, f.edge)
		}},
		{"selector_multi", "instance", false, true, func(f *swiftSuperAcceptanceFixture) {
			swiftStressExec(f, `UPDATE symbols SET signature='run(id:,cache:)',arity_min=2,arity_max=2 WHERE id=?`, f.baseRun)
			swiftStressExec(f, `UPDATE edges SET evidence='swift:super;labels=id:,cache:',call_arity=2 WHERE id=?`, f.edge)
		}},
		{"selector_trailing", "instance", false, true, func(f *swiftSuperAcceptanceFixture) {
			swiftStressExec(f, `UPDATE symbols SET name='perform',qualified_name='Base.perform',signature='perform(body:)',arity_min=1,arity_max=1 WHERE id=?`, f.baseRun)
			swiftStressExec(f, `UPDATE edges SET dst_name='super.perform',evidence='swift:super;trailing_labels=_',call_arity=1 WHERE id=?`, f.edge)
		}},
		{"selector_labels_trailing", "instance", false, true, func(f *swiftSuperAcceptanceFixture) {
			swiftStressExec(f, `UPDATE symbols SET name='perform',qualified_name='Base.perform',signature='perform(id:,body:)',arity_min=2,arity_max=2 WHERE id=?`, f.baseRun)
			swiftStressExec(f, `UPDATE edges SET dst_name='super.perform',evidence='swift:super;labels=id:;trailing_labels=_',call_arity=2 WHERE id=?`, f.edge)
		}},
		{"missing_membership", "instance", false, false, nil},
		{"duplicate_membership", "instance", false, false, func(f *swiftSuperAcceptanceFixture) {
			swiftExtensionMembership(f.t, f.swiftScopeFixture, f.mainFile, f.caller, "Child", 5, 9, 0, 0)
		}},
		{"empty_target", "instance", false, false, func(f *swiftSuperAcceptanceFixture) {
			swiftStressExec(f, `UPDATE swift_extension_memberships SET target_qualified_name='' WHERE symbol_id=?`, f.caller)
		}},
		{"mismatched_target", "instance", false, false, func(f *swiftSuperAcceptanceFixture) {
			swiftStressExec(f, `UPDATE swift_extension_memberships SET target_qualified_name='Other' WHERE symbol_id=?`, f.caller)
		}},
		{"wrong_file", "instance", false, false, func(f *swiftSuperAcceptanceFixture) {
			file := f.file("Other.swift")
			swiftStressExec(f, `UPDATE swift_extension_memberships SET file_id=? WHERE symbol_id=?`, file, f.caller)
		}},
		{"range_before", "instance", false, false, func(f *swiftSuperAcceptanceFixture) {
			swiftStressExec(f, `UPDATE swift_extension_memberships SET extension_start_line=9 WHERE symbol_id=?`, f.caller)
		}},
		{"range_after", "instance", false, false, func(f *swiftSuperAcceptanceFixture) {
			swiftStressExec(f, `UPDATE swift_extension_memberships SET extension_end_line=5 WHERE symbol_id=?`, f.caller)
		}},
		{"generic_membership", "instance", false, false, func(f *swiftSuperAcceptanceFixture) {
			swiftStressExec(f, `UPDATE swift_extension_memberships SET is_generic=1 WHERE symbol_id=?`, f.caller)
		}},
		{"constrained_membership", "instance", false, false, func(f *swiftSuperAcceptanceFixture) {
			swiftStressExec(f, `UPDATE swift_extension_memberships SET is_constrained=1 WHERE symbol_id=?`, f.caller)
		}},
		{"malformed_generic", "instance", false, false, func(f *swiftSuperAcceptanceFixture) {
			swiftStressExec(f, `UPDATE swift_extension_memberships SET is_generic=2 WHERE symbol_id=?`, f.caller)
		}},
		{"malformed_constrained", "instance", false, false, func(f *swiftSuperAcceptanceFixture) {
			swiftStressExec(f, `UPDATE swift_extension_memberships SET is_constrained=2 WHERE symbol_id=?`, f.caller)
		}},
		{"missing_owner", "instance", false, false, func(f *swiftSuperAcceptanceFixture) {
			swiftStressExec(f, `UPDATE symbols SET qualified_name='Gone' WHERE id=?`, f.child)
		}},
		{"duplicate_owner", "instance", false, false, func(f *swiftSuperAcceptanceFixture) { f.symbol(f.mainFile, "Child", "", "class", "", false) }},
		{"protocol_owner", "instance", false, false, func(f *swiftSuperAcceptanceFixture) {
			swiftStressExec(f, `UPDATE symbols SET kind='protocol' WHERE id=?`, f.child)
		}},
		{"struct_owner", "instance", false, false, func(f *swiftSuperAcceptanceFixture) {
			swiftStressExec(f, `UPDATE symbols SET kind='struct' WHERE id=?`, f.child)
		}},
		{"enum_owner", "instance", false, false, func(f *swiftSuperAcceptanceFixture) {
			swiftStressExec(f, `UPDATE symbols SET kind='enum' WHERE id=?`, f.child)
		}},
		{"cross_file_owner", "instance", false, false, func(f *swiftSuperAcceptanceFixture) {
			file := f.file("Other.swift")
			swiftStressExec(f, `UPDATE symbols SET file_id=? WHERE id=?`, file, f.child)
		}},
		{"missing_source_fact", "instance", false, false, func(f *swiftSuperAcceptanceFixture) {
			swiftStressExec(f, `DELETE FROM swift_declaration_facts WHERE symbol_id=?`, f.caller)
		}},
		{"duplicate_source_fact", "instance", false, false, func(f *swiftSuperAcceptanceFixture) {
			swiftFact(f.t, f.swiftScopeFixture, f.mainFile, f.caller, "instance")
		}},
		{"invalid_dispatch", "instance", false, false, func(f *swiftSuperAcceptanceFixture) {
			swiftStressExec(f, `UPDATE swift_declaration_facts SET dispatch_kind='class' WHERE symbol_id=?`, f.caller)
		}},
		{"invalid_static_pair", "instance", false, false, func(f *swiftSuperAcceptanceFixture) {
			swiftStressExec(f, `UPDATE symbols SET is_static=1 WHERE id=?`, f.caller)
		}},
		{"instance_blocker", "instance", false, false, func(f *swiftSuperAcceptanceFixture) {
			f.blocker(f.mainFile, "Base", "run", graph.ScopeImportSwiftMemberValue, false)
		}},
		{"static_blocker_irrelevant", "instance", false, true, func(f *swiftSuperAcceptanceFixture) {
			f.blocker(f.mainFile, "Base", "run", graph.ScopeImportSwiftMemberValue, true)
		}},
		{"deleted_blocker", "instance", false, true, func(f *swiftSuperAcceptanceFixture) {
			file := f.file("deleted.swift")
			f.blocker(file, "Base", "run", graph.ScopeImportSwiftMemberValue, false)
			swiftStressExec(f, `UPDATE files SET is_deleted=1 WHERE id=?`, file)
		}},
		{"multilevel_blocker", "instance", true, false, func(f *swiftSuperAcceptanceFixture) {
			f.blocker(f.mainFile, "Middle", "run", graph.ScopeImportSwiftMemberValue, false)
		}},
		{"target_owner_blocker", "instance", false, false, func(f *swiftSuperAcceptanceFixture) {
			f.blocker(f.mainFile, "Base", "run", graph.ScopeImportSwiftMemberValue, false)
		}},
		{"type_static_blocker", "type", false, false, func(f *swiftSuperAcceptanceFixture) {
			f.blocker(f.mainFile, "Base", "run", graph.ScopeImportSwiftMemberValue, true)
		}},
		{"type_instance_blocker_irrelevant", "type", false, true, func(f *swiftSuperAcceptanceFixture) {
			f.blocker(f.mainFile, "Base", "run", graph.ScopeImportSwiftMemberValue, false)
		}},
		{"prod_test_candidate", "instance", false, true, func(f *swiftSuperAcceptanceFixture) {
			file := f.file("Base_test.swift")
			swiftStressExec(f, `UPDATE files SET path='Base_test.swift' WHERE id=?`, file)
		}},
		{"extension_owned_target", "type", false, false, func(f *swiftSuperAcceptanceFixture) {
			swiftSetRange(f.t, f.swiftScopeFixture, f.baseRun, 30, 1, 30, 15)
		}},
		{"cycle", "instance", false, false, func(f *swiftSuperAcceptanceFixture) {
			swiftSuperclass(f.t, f.swiftScopeFixture, f.mainFile, "Base", "Child")
		}},
		{"cycle_with_unproven_noise", "instance", false, false, func(f *swiftSuperAcceptanceFixture) {
			swiftSuperclass(f.t, f.swiftScopeFixture, f.mainFile, "Base", "Child")
			swiftRelation(f.t, f.swiftScopeFixture, f.mainFile, "Base", "P", "unproven")
		}},
		{"selected_owner_multiple_superclass", "instance", false, false, func(f *swiftSuperAcceptanceFixture) {
			swiftSuperclass(f.t, f.swiftScopeFixture, f.mainFile, "Base", "GrandA")
			swiftSuperclass(f.t, f.swiftScopeFixture, f.mainFile, "Base", "GrandB")
		}},
		{"multiple_superclass", "instance", false, false, func(f *swiftSuperAcceptanceFixture) {
			swiftSuperclass(f.t, f.swiftScopeFixture, f.mainFile, "Child", "Other")
		}},
		{"generic_superclass", "instance", false, false, func(f *swiftSuperAcceptanceFixture) {
			swiftStressExec(f, `UPDATE swift_inheritance_relations SET is_generic=1 WHERE child_qualified_name='Child'`)
		}},
		{"constrained_superclass", "instance", false, false, func(f *swiftSuperAcceptanceFixture) {
			swiftStressExec(f, `UPDATE swift_inheritance_relations SET is_constrained=1 WHERE child_qualified_name='Child'`)
		}},
		{"cross_file_superclass", "instance", false, false, func(f *swiftSuperAcceptanceFixture) {
			file := f.file("Other.swift")
			swiftStressExec(f, `UPDATE swift_inheritance_relations SET file_id=? WHERE child_qualified_name='Child'`, file)
		}},
		{"conformance_control", "instance", false, true, func(f *swiftSuperAcceptanceFixture) {
			swiftRelation(f.t, f.swiftScopeFixture, f.mainFile, "Child", "P", "conformance")
		}},
		{"unknown_ancestry", "instance", false, true, func(f *swiftSuperAcceptanceFixture) {
			swiftRelation(f.t, f.swiftScopeFixture, f.mainFile, "Base", "P", "unproven")
		}},
		{"type_missing_override", "type", true, false, func(f *swiftSuperAcceptanceFixture) {
			middle := f.symbol(f.mainFile, "Middle", "", "class", "", false)
			_ = middle
			swiftStressExec(f, `UPDATE swift_inheritance_relations SET target_qualified_name='Middle' WHERE child_qualified_name='Child'`)
			swiftSuperclass(f.t, f.swiftScopeFixture, f.mainFile, "Middle", "Base")
			target := f.symbol(f.mainFile, "run", "Middle", "function", "run()", true)
			swiftStressExec(f, `UPDATE symbols SET is_static=1 WHERE id=?`, target)
			swiftFact(f.t, f.swiftScopeFixture, f.mainFile, target, "class")
		}},
		{"type_selected_target_final", "type", false, true, func(f *swiftSuperAcceptanceFixture) {
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET is_final=1 WHERE symbol_id=?`, f.baseRun); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"type_final_ancestor_override", "type", false, false, func(f *swiftSuperAcceptanceFixture) {
			grand := f.symbol(f.mainFile, "Grand", "", "class", "", false)
			grandRun := f.symbol(f.mainFile, "run", "Grand", "function", "run()", true)
			swiftSetRange(f.t, f.swiftScopeFixture, grand, 30, 1, 34, 20)
			swiftSetRange(f.t, f.swiftScopeFixture, grandRun, 31, 1, 31, 15)
			f.arity(grandRun, 0, 0)
			swiftFact(f.t, f.swiftScopeFixture, f.mainFile, grandRun, "class")
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET is_final=1 WHERE symbol_id=?`, grandRun); err != nil {
				f.t.Fatal(err)
			}
			swiftSuperclass(f.t, f.swiftScopeFixture, f.mainFile, "Base", "Grand")
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET is_override=1 WHERE symbol_id=?`, f.baseRun); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"type_orphan_override", "type", false, false, func(f *swiftSuperAcceptanceFixture) {
			swiftStressExec(f, `UPDATE swift_declaration_facts SET is_override=1 WHERE symbol_id=?`, f.baseRun)
		}},
		{"type_selected_target_static", "type", false, true, func(f *swiftSuperAcceptanceFixture) {
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET dispatch_kind='static' WHERE symbol_id=?`, f.baseRun); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"type_static_ancestor_override", "type", false, false, func(f *swiftSuperAcceptanceFixture) {
			grand := f.symbol(f.mainFile, "Grand", "", "class", "", false)
			grandRun := f.symbol(f.mainFile, "run", "Grand", "function", "run()", true)
			swiftSetRange(f.t, f.swiftScopeFixture, grand, 30, 1, 34, 20)
			swiftSetRange(f.t, f.swiftScopeFixture, grandRun, 31, 1, 31, 15)
			f.arity(grandRun, 0, 0)
			swiftFact(f.t, f.swiftScopeFixture, f.mainFile, grandRun, "static")
			swiftSuperclass(f.t, f.swiftScopeFixture, f.mainFile, "Base", "Grand")
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET is_override=1 WHERE symbol_id=?`, f.baseRun); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"type_transitive_missing_override", "type", true, false, func(f *swiftSuperAcceptanceFixture) {
			swiftStressExec(f, `UPDATE swift_inheritance_relations SET target_qualified_name='Middle' WHERE child_qualified_name='Child'`)
			swiftSuperclass(f.t, f.swiftScopeFixture, f.mainFile, "Middle", "Base")
			target := f.symbol(f.mainFile, "run", "Middle", "function", "run()", true)
			swiftFact(f.t, f.swiftScopeFixture, f.mainFile, target, "class")
			swiftStressExec(f, `UPDATE symbols SET is_static=1 WHERE id=?`, target)
		}},
		{"type_legal_override_control", "type", true, true, func(f *swiftSuperAcceptanceFixture) {
			target := f.symbol(f.mainFile, "run", "Middle", "function", "run()", true)
			swiftSetRange(f.t, f.swiftScopeFixture, target, 11, 1, 11, 15)
			f.arity(target, 0, 0)
			swiftFact(f.t, f.swiftScopeFixture, f.mainFile, target, "class")
			for _, id := range []int64{f.baseRun, target} {
				result, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET is_static=1 WHERE id=?`, id)
				if err != nil {
					f.t.Fatal(err)
				}
				if changed, err := result.RowsAffected(); err != nil || changed != 1 {
					f.t.Fatalf("static symbol %d rows=%d err=%v", id, changed, err)
				}
			}
			result, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET dispatch_kind='class',is_override=0,is_final=0 WHERE symbol_id=?`, f.baseRun)
			if err != nil {
				f.t.Fatal(err)
			}
			if changed, err := result.RowsAffected(); err != nil || changed != 1 {
				f.t.Fatalf("base fact rows=%d err=%v", changed, err)
			}
			result, err = f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET dispatch_kind='class',is_override=1,is_final=0 WHERE symbol_id=?`, target)
			if err != nil {
				f.t.Fatal(err)
			}
			if changed, err := result.RowsAffected(); err != nil || changed != 1 {
				f.t.Fatalf("middle fact rows=%d err=%v", changed, err)
			}
		}},
		{"nominal_instance_control", "instance", false, true, func(f *swiftSuperAcceptanceFixture) {
			swiftStressExec(f, `DELETE FROM swift_extension_memberships WHERE symbol_id=?`, f.caller)
			swiftSetRange(f.t, f.swiftScopeFixture, f.child, 5, 1, 9, 20)
		}},
		{"nominal_multilevel_control", "instance", true, true, func(f *swiftSuperAcceptanceFixture) {
			swiftStressExec(f, `DELETE FROM swift_extension_memberships WHERE symbol_id=?`, f.caller)
			swiftSetRange(f.t, f.swiftScopeFixture, f.child, 5, 1, 9, 20)
		}},
		{"nominal_type_control", "type", false, true, func(f *swiftSuperAcceptanceFixture) {
			swiftStressExec(f, `DELETE FROM swift_extension_memberships WHERE symbol_id=?`, f.caller)
			swiftSetRange(f.t, f.swiftScopeFixture, f.child, 5, 1, 9, 20)
		}},
		{"nominal_type_multilevel_control", "type", true, true, func(f *swiftSuperAcceptanceFixture) {
			swiftStressExec(f, `DELETE FROM swift_extension_memberships WHERE symbol_id=?`, f.caller)
			swiftSetRange(f.t, f.swiftScopeFixture, f.child, 5, 1, 9, 20)
		}},
		{"source_owner_visibility_private", "instance", false, true, func(f *swiftSuperAcceptanceFixture) {
			result, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET visibility='private' WHERE id=?`, f.child)
			if err != nil {
				f.t.Fatal(err)
			}
			if changed, err := result.RowsAffected(); err != nil || changed != 1 {
				f.t.Fatalf("source owner visibility rows=%d err=%v", changed, err)
			}
			assertSwiftSuperPrivateSourceControl(f.t, f, true)
		}},
		{"target_visibility_private", "instance", false, false, func(f *swiftSuperAcceptanceFixture) {
			swiftStressExec(f, `UPDATE symbols SET visibility='private' WHERE id=?`, f.baseRun)
		}},
		{"source_range_before", "instance", false, false, func(f *swiftSuperAcceptanceFixture) { swiftSetRange(f.t, f.swiftScopeFixture, f.caller, 4, 1, 8, 20) }},
		{"source_range_after", "instance", false, false, func(f *swiftSuperAcceptanceFixture) { swiftSetRange(f.t, f.swiftScopeFixture, f.caller, 6, 1, 10, 20) }},
	}
	if len(categories) < 60 {
		t.Fatalf("categories=%d, want >=60", len(categories))
	}
	want := extensionStressState{strategies: map[string]int{}}
	first := extensionStressState{strategies: map[string]int{}}
	categoryCount := 0
	for categoryIndex, category := range categories {
		if only != "" && category.name != only {
			continue
		}
		if only == "" && prefix >= 0 && categoryIndex > prefix && category.name != "duplicate_membership" {
			continue
		}
		categoryCount++
		f := extensionStressFixture(t, category.mode, category.multi, category.name != "missing_membership")
		edges := make([]int64, perCategory)
		edges[0] = f.edge
		for i := 1; i < len(edges); i++ {
			edges[i] = f.call(f.mainFile, f.caller, "super.run", "swift:super", 0, 7+i)
		}
		for i := range edges {
			f.reference(f.mainFile, f.caller, "super.run", 7+i)
		}
		if category.mutate != nil {
			category.mutate(f)
		}
		if strings.HasPrefix(category.name, "selector_") {
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET dst_name=(SELECT dst_name FROM edges WHERE id=?), evidence=(SELECT evidence FROM edges WHERE id=?), call_arity=(SELECT call_arity FROM edges WHERE id=?) WHERE repo_id=? AND edge_kind='calls' AND evidence LIKE 'swift:super%'`, f.edge, f.edge, f.edge, f.repoID); err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE references_tbl SET name=(SELECT dst_name FROM edges WHERE id=?) WHERE repo_id=? AND file_id=? AND context_symbol_id=?`, f.edge, f.repoID, f.mainFile, f.caller); err != nil {
				t.Fatal(err)
			}
		}
		if category.name == "cycle" {
			assertStressDirectCycle(t, f)
		}
		if category.valid {
			nominal := strings.HasPrefix(category.name, "nominal_")
			assertSwiftSuperStressSourceMode(t, f, nominal)
			if category.name == "nominal_instance_control" {
				assertSwiftSuperNominalInstanceControl(t, f)
			}
		}
		f.resolve()
		nominal := strings.HasPrefix(category.name, "nominal_")
		strategy := ResolutionStrategySwiftSuperExtensionScope
		if nominal {
			strategy = ResolutionStrategySwiftSuperScope
		}
		if category.mode == "type" {
			strategy = ResolutionStrategySwiftSuperExtensionTypeScope
			if nominal {
				strategy = ResolutionStrategySwiftSuperTypeScope
			}
		}
		if category.multi && category.name != "type_legal_override_control" {
			if category.mode == "type" {
				strategy = ResolutionStrategySwiftSuperExtensionMultilevelTypeScope
				if nominal {
					strategy = ResolutionStrategySwiftSuperMultilevelTypeScope
				}
			} else {
				strategy = ResolutionStrategySwiftSuperExtensionMultilevelInheritedMethodScope
				if nominal {
					strategy = ResolutionStrategySwiftSuperMultilevelInheritedMethodScope
				}
			}
		}
		if category.valid {
			want.resolved += perCategory
			want.strategies[strategy] += perCategory
			wantDst := f.baseRun
			if category.name == "type_legal_override_control" {
				wantDst = f.symbolID("Middle.run")
			}
			dst, gotStrategy, confidence := edgeState(t, f.swiftScopeFixture, edges[0])
			if !dst.Valid || dst.Int64 != wantDst || gotStrategy != strategy || confidence != ResolutionConfidenceHigh {
				t.Fatalf("%s edge=(%v,%q,%q)", category.name, dst, gotStrategy, confidence)
			}
		} else {
			want.unresolved += perCategory
			dst, gotStrategy, confidence := edgeState(t, f.swiftScopeFixture, edges[0])
			if dst.Valid || gotStrategy != "" || confidence != "" {
				t.Fatalf("%s edge=%v strategy=%q confidence=%q, want unresolved", category.name, dst, gotStrategy, confidence)
			}
			assertSwiftReferenceCleared(t, f.swiftScopeFixture, swiftReferenceID(t, f.swiftScopeFixture, edges[0]))
		}
		got := extensionStressStateOf(t, f.swiftScopeFixture)
		wantOne := extensionStressState{total: perCategory, strategies: map[string]int{}}
		if category.valid {
			wantOne.resolved = perCategory
			wantOne.strategies[strategy] = perCategory
		} else {
			wantOne.unresolved = perCategory
		}
		if !reflect.DeepEqual(got, wantOne) {
			t.Fatalf("%s state=%+v", category.name, got)
		}
		first.total += got.total
		first.resolved += got.resolved
		first.unresolved += got.unresolved
		first.badMetadata += got.badMetadata
		for k, n := range got.strategies {
			first.strategies[k] += n
		}
		f.resolve()
		if second := extensionStressStateOf(t, f.swiftScopeFixture); !reflect.DeepEqual(second, got) {
			t.Fatalf("%s not idempotent", category.name)
		}
	}
	for k, n := range want.strategies {
		if first.strategies[k] != n {
			t.Fatalf("strategy %s=%d want %d", k, first.strategies[k], n)
		}
	}
	if first.total != categoryCount*perCategory || first.resolved != want.resolved || first.unresolved != want.unresolved || first.badMetadata != 0 {
		t.Fatalf("first=%+v want=%+v", first, want)
	}
}

func assertSwiftSuperStressSourceMode(t *testing.T, f *swiftSuperAcceptanceFixture, nominal bool) {
	t.Helper()
	var contained, memberships int
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM symbols child JOIN symbols caller ON caller.id=? WHERE child.id=? AND child.file_id=caller.file_id AND child.kind='class' AND child.start_line<=caller.start_line AND caller.end_line<=child.end_line`, f.caller, f.child).Scan(&contained); err != nil {
		t.Fatal(err)
	}
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM swift_extension_memberships WHERE repo_id=? AND symbol_id=?`, f.repoID, f.caller).Scan(&memberships); err != nil {
		t.Fatal(err)
	}
	if nominal && (contained != 1 || memberships != 0) {
		t.Fatalf("nominal source contained=%d memberships=%d", contained, memberships)
	}
	if !nominal && (contained != 0 || memberships != 1) {
		t.Fatalf("extension source contained=%d memberships=%d", contained, memberships)
	}
	if !nominal {
		var proven int
		if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM swift_extension_memberships m JOIN symbols caller ON caller.id=m.symbol_id JOIN symbols owner ON owner.id=? WHERE m.repo_id=? AND m.symbol_id=? AND m.file_id=caller.file_id AND m.target_qualified_name='Child' AND m.is_generic=0 AND m.is_constrained=0 AND m.extension_start_line<=caller.start_line AND caller.end_line<=m.extension_end_line`, f.child, f.repoID, f.caller).Scan(&proven); err != nil {
			t.Fatal(err)
		}
		if proven != 1 {
			t.Fatalf("extension source proof rows=%d", proven)
		}
	}
}

func assertSwiftSuperNominalInstanceControl(t *testing.T, f *swiftSuperAcceptanceFixture) {
	t.Helper()
	var relations, static int
	var dispatch string
	var dst sql.NullInt64
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM swift_inheritance_relations WHERE repo_id=? AND child_qualified_name='Child' AND target_qualified_name='Base' AND relation_kind='superclass' AND is_generic=0 AND is_constrained=0`, f.repoID).Scan(&relations); err != nil {
		t.Fatal(err)
	}
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT s.is_static,d.dispatch_kind FROM symbols s JOIN swift_declaration_facts d ON d.symbol_id=s.id WHERE s.id=?`, f.baseRun).Scan(&static, &dispatch); err != nil {
		t.Fatal(err)
	}
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT dst_symbol_id FROM edges WHERE id=?`, f.edge).Scan(&dst); err != nil {
		t.Fatal(err)
	}
	if relations != 1 || static != 0 || dispatch != "instance" || dst.Valid {
		t.Fatalf("nominal instance relations=%d static=%d dispatch=%q dst=%v", relations, static, dispatch, dst)
	}
}

func assertSwiftSuperPrivateSourceControl(t *testing.T, f *swiftSuperAcceptanceFixture, extension bool) {
	t.Helper()
	var child, base, method string
	var memberships int
	var dst sql.NullInt64
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT visibility FROM symbols WHERE id=?`, f.child).Scan(&child); err != nil {
		t.Fatal(err)
	}
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT visibility FROM symbols WHERE id=?`, f.base).Scan(&base); err != nil {
		t.Fatal(err)
	}
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT visibility FROM symbols WHERE id=?`, f.baseRun).Scan(&method); err != nil {
		t.Fatal(err)
	}
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM swift_extension_memberships WHERE repo_id=? AND symbol_id=?`, f.repoID, f.caller).Scan(&memberships); err != nil {
		t.Fatal(err)
	}
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT dst_symbol_id FROM edges WHERE id=?`, f.edge).Scan(&dst); err != nil {
		t.Fatal(err)
	}
	if child != "private" || base == "private" || method == "private" || memberships != boolInt(extension) || dst.Valid {
		t.Fatalf("private source child=%q base=%q method=%q memberships=%d dst=%v", child, base, method, memberships, dst)
	}
}

func TestSwiftSuperPrivateSourceOwnerNominalControl(t *testing.T) {
	f := newSwiftSuperAcceptanceFixture(t)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET visibility='private' WHERE id=?`, f.child); err != nil {
		t.Fatal(err)
	}
	f.reference(f.mainFile, f.caller, "super.run", 7)
	assertSwiftSuperPrivateSourceControl(t, f, false)
	f.resolve()
	assertSwiftBinding(t, f.swiftScopeFixture, f.edge, f.baseRun, ResolutionStrategySwiftSuperScope)
}

func TestSwiftSuperExtensionFinalSelectedControls(t *testing.T) {
	for _, tc := range []struct {
		name      string
		extension bool
		ancestor  bool
		final     bool
		want      bool
		strategy  string
	}{
		{"nominal_selected_final", false, false, true, true, ResolutionStrategySwiftSuperTypeScope},
		{"extension_selected_final", true, false, true, true, ResolutionStrategySwiftSuperExtensionTypeScope},
		{"extension_final_ancestor_override", true, true, true, false, ""},
		{"extension_nonfinal_ancestor_override", true, true, false, true, ResolutionStrategySwiftSuperExtensionTypeScope},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var f *swiftSuperAcceptanceFixture
			if tc.extension {
				f = extensionStressFixture(t, "type", false)
			} else {
				f = newSwiftSuperAcceptanceFixture(t)
				if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET is_static=1 WHERE id IN (?,?)`, f.caller, f.baseRun); err != nil {
					t.Fatal(err)
				}
				if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET dispatch_kind='class' WHERE symbol_id IN (?,?)`, f.caller, f.baseRun); err != nil {
					t.Fatal(err)
				}
			}
			if tc.final {
				if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET is_final=1 WHERE symbol_id=?`, f.baseRun); err != nil {
					t.Fatal(err)
				}
			}
			if tc.ancestor {
				grand := f.symbol(f.mainFile, "Grand", "", "class", "", false)
				grandRun := f.symbol(f.mainFile, "run", "Grand", "function", "run()", true)
				swiftSetRange(t, f.swiftScopeFixture, grand, 30, 1, 34, 20)
				swiftSetRange(t, f.swiftScopeFixture, grandRun, 31, 1, 31, 15)
				f.arity(grandRun, 0, 0)
				swiftFact(t, f.swiftScopeFixture, f.mainFile, grandRun, "class")
				if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET is_final=?,is_override=0 WHERE symbol_id=?`, boolInt(tc.final), grandRun); err != nil {
					t.Fatal(err)
				}
				swiftSuperclass(t, f.swiftScopeFixture, f.mainFile, "Base", "Grand")
				if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET is_override=1 WHERE symbol_id=?`, f.baseRun); err != nil {
					t.Fatal(err)
				}
			}
			f.reference(f.mainFile, f.caller, "super.run", 7)
			f.resolve()
			if tc.want {
				assertSwiftBinding(t, f.swiftScopeFixture, f.edge, f.baseRun, tc.strategy)
			} else {
				assertSwiftEdgeUnresolved(t, f.swiftScopeFixture, f.edge)
			}
		})
	}
}

func TestSwiftSuperExtensionStaticSelectedAndAncestorControls(t *testing.T) {
	t.Run("nominal_selected_static", func(t *testing.T) {
		f := newSwiftSuperAcceptanceFixture(t)
		if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET is_static=1 WHERE id IN (?,?)`, f.caller, f.baseRun); err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET dispatch_kind='static' WHERE symbol_id IN (?,?)`, f.caller, f.baseRun); err != nil {
			t.Fatal(err)
		}
		f.reference(f.mainFile, f.caller, "super.run", 7)
		f.resolve()
		assertSwiftBinding(t, f.swiftScopeFixture, f.edge, f.baseRun, ResolutionStrategySwiftSuperTypeScope)
	})

	t.Run("extension_static_ancestor_override", func(t *testing.T) {
		f := extensionStressFixture(t, "type", false)
		grand := f.symbol(f.mainFile, "Grand", "", "class", "", false)
		grandRun := f.symbol(f.mainFile, "run", "Grand", "function", "run()", true)
		swiftSetRange(t, f.swiftScopeFixture, grand, 30, 1, 34, 20)
		swiftSetRange(t, f.swiftScopeFixture, grandRun, 31, 1, 31, 15)
		f.arity(grandRun, 0, 0)
		swiftFact(t, f.swiftScopeFixture, f.mainFile, grandRun, "static")
		swiftSuperclass(t, f.swiftScopeFixture, f.mainFile, "Base", "Grand")
		if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET is_override=1 WHERE symbol_id=?`, f.baseRun); err != nil {
			t.Fatal(err)
		}
		f.reference(f.mainFile, f.caller, "super.run", 7)
		f.resolve()
		assertSwiftEdgeUnresolved(t, f.swiftScopeFixture, f.edge)
	})
}

func TestSwiftSuperTypeLegalOverrideControls(t *testing.T) {
	for _, tc := range []struct {
		name      string
		extension bool
		strategy  string
	}{
		{"nominal", false, ResolutionStrategySwiftSuperTypeScope},
		{"extension", true, ResolutionStrategySwiftSuperExtensionTypeScope},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwiftSuperAcceptanceFixture(t)
			middle := f.symbol(f.mainFile, "Middle", "", "class", "", false)
			middleRun := f.symbol(f.mainFile, "run", "Middle", "function", "run()", true)
			swiftSetRange(t, f.swiftScopeFixture, middle, 10, 1, 15, 20)
			swiftSetRange(t, f.swiftScopeFixture, middleRun, 11, 1, 11, 15)
			f.arity(middleRun, 0, 0)
			result, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET target_qualified_name='Middle' WHERE child_qualified_name='Child'`)
			if err != nil {
				t.Fatal(err)
			}
			if changed, err := result.RowsAffected(); err != nil || changed != 1 {
				t.Fatalf("child superclass rows=%d err=%v", changed, err)
			}
			swiftSuperclass(t, f.swiftScopeFixture, f.mainFile, "Middle", "Base")
			swiftFact(t, f.swiftScopeFixture, f.mainFile, middleRun, "class")
			for _, id := range []int64{f.caller, f.baseRun, middleRun} {
				result, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET is_static=1 WHERE id=?`, id)
				if err != nil {
					t.Fatal(err)
				}
				if changed, err := result.RowsAffected(); err != nil || changed != 1 {
					t.Fatalf("static symbol %d rows=%d err=%v", id, changed, err)
				}
			}
			for _, fact := range []struct {
				id, override int64
			}{{f.baseRun, 0}, {middleRun, 1}, {f.caller, 0}} {
				result, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET dispatch_kind='class',is_override=?,is_final=0 WHERE symbol_id=?`, fact.override, fact.id)
				if err != nil {
					t.Fatal(err)
				}
				if changed, err := result.RowsAffected(); err != nil || changed != 1 {
					t.Fatalf("fact %d rows=%d err=%v", fact.id, changed, err)
				}
			}
			if tc.extension {
				swiftSetRange(t, f.swiftScopeFixture, f.child, 20, 1, 25, 20)
				swiftExtensionMembership(t, f.swiftScopeFixture, f.mainFile, f.caller, "Child", 5, 9, 0, 0)
			}
			f.reference(f.mainFile, f.caller, "super.run", 7)
			f.resolve()
			assertSwiftBinding(t, f.swiftScopeFixture, f.edge, middleRun, tc.strategy)
		})
	}
}

func TestSwiftSuperTypeStaticAncestorNominalControls(t *testing.T) {
	for _, tc := range []struct {
		name     string
		dispatch string
		want     bool
	}{
		{"static_ancestor_override", "static", false},
		{"class_ancestor_override", "class", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwiftSuperAcceptanceFixture(t)
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET is_static=1 WHERE id IN (?,?)`, f.caller, f.baseRun); err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET dispatch_kind='class' WHERE symbol_id IN (?,?)`, f.caller, f.baseRun); err != nil {
				t.Fatal(err)
			}
			grand := f.symbol(f.mainFile, "Grand", "", "class", "", false)
			grandRun := f.symbol(f.mainFile, "run", "Grand", "function", "run()", true)
			swiftSetRange(t, f.swiftScopeFixture, grand, 30, 1, 34, 20)
			swiftSetRange(t, f.swiftScopeFixture, grandRun, 31, 1, 31, 15)
			f.arity(grandRun, 0, 0)
			swiftFact(t, f.swiftScopeFixture, f.mainFile, grandRun, tc.dispatch)
			swiftSuperclass(t, f.swiftScopeFixture, f.mainFile, "Base", "Grand")
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET is_override=1 WHERE symbol_id=?`, f.baseRun); err != nil {
				t.Fatal(err)
			}
			f.reference(f.mainFile, f.caller, "super.run", 7)
			f.resolve()
			if tc.want {
				assertSwiftBinding(t, f.swiftScopeFixture, f.edge, f.baseRun, ResolutionStrategySwiftSuperTypeScope)
			} else {
				assertSwiftEdgeUnresolved(t, f.swiftScopeFixture, f.edge)
			}
		})
	}
}

func assertStressDirectCycle(t *testing.T, f *swiftSuperAcceptanceFixture) {
	t.Helper()
	rows, err := f.store.db.QueryContext(f.ctx, `SELECT child_qualified_name,target_qualified_name,relation_kind,is_generic,is_constrained FROM swift_inheritance_relations WHERE repo_id=? AND file_id=? ORDER BY child_qualified_name,target_qualified_name`, f.repoID, f.mainFile)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got []string
	for rows.Next() {
		var child, target, kind string
		var generic, constrained int
		if err := rows.Scan(&child, &target, &kind, &generic, &constrained); err != nil {
			t.Fatal(err)
		}
		if kind != "superclass" || generic != 0 || constrained != 0 {
			t.Fatalf("cycle relation=%s->%s kind=%q generic=%d constrained=%d", child, target, kind, generic, constrained)
		}
		got = append(got, child+"->"+target)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, []string{"Base->Child", "Child->Base"}) {
		t.Fatalf("cycle relations=%v, want [Base->Child Child->Base]", got)
	}
}
