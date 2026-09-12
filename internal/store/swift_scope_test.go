package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

type swiftScopeFixture struct {
	t        *testing.T
	ctx      context.Context
	store    *Store
	repoID   int64
	mainFile int64
}

func newSwiftScopeFixture(t *testing.T) *swiftScopeFixture {
	t.Helper()
	s, repoID := newQueryTestStore(t)
	file, err := insertTestFileLang(context.Background(), s, repoID, "Service.swift", "swift")
	if err != nil {
		t.Fatal(err)
	}
	return &swiftScopeFixture{t: t, ctx: context.Background(), store: s, repoID: repoID, mainFile: file}
}

func (f *swiftScopeFixture) file(path string) int64 {
	f.t.Helper()
	id, err := insertTestFileLang(f.ctx, f.store, f.repoID, path, "swift")
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f *swiftScopeFixture) symbol(file int64, name, owner, kind, signature string, static bool) int64 {
	f.t.Helper()
	qualified := name
	if owner != "" {
		qualified = owner + "." + name
	}
	value := 0
	if static {
		value = 1
	}
	res, err := f.store.db.ExecContext(f.ctx, `INSERT INTO symbols(
		repo_id,file_id,language,kind,name,qualified_name,container_name,signature,is_static,
		start_line,start_col,end_line,end_col,stable_key,qualified_suffix,dot_tail2,dot_tail3)
		VALUES(?,?,?,?,?,?,?,?,?,1,1,1,1,?,?,?,?)`,
		f.repoID, file, "swift", kind, name, qualified, owner, signature, value,
		fmt.Sprintf("%s|%d|%d", qualified, file, value), qualifiedSuffix(qualified), dotTail2(qualified), dotTail3(qualified))
	if err != nil {
		f.t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f *swiftScopeFixture) arity(symbol int64, min, max int64) {
	f.t.Helper()
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET arity_min=?,arity_max=? WHERE id=?`, min, max, symbol); err != nil {
		f.t.Fatal(err)
	}
}

func (f *swiftScopeFixture) call(file, source int64, dst, evidence string, arity, line int) int64 {
	f.t.Helper()
	res, err := f.store.db.ExecContext(f.ctx, `INSERT INTO edges(repo_id,src_symbol_id,dst_name,edge_kind,evidence,file_id,line,call_arity)
		VALUES(?,?,?,'calls',?,?,?,?)`, f.repoID, source, dst, evidence, file, line, arity)
	if err != nil {
		f.t.Fatal(err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f *swiftScopeFixture) reference(file, source int64, name string, line int) {
	f.t.Helper()
	if _, err := f.store.db.ExecContext(f.ctx, `INSERT INTO references_tbl(repo_id,file_id,ref_kind,name,qualified_name,start_line,start_col,end_line,end_col,context_symbol_id)
		VALUES(?,?, 'call',?,?,?,1,?,1,?)`, f.repoID, file, name, name, line, line, source); err != nil {
		f.t.Fatal(err)
	}
}

func (f *swiftScopeFixture) blocker(file int64, owner, name, kind string, static bool) {
	f.t.Helper()
	value := 0
	if static {
		value = 1
	}
	if _, err := f.store.db.ExecContext(f.ctx, `INSERT INTO scope_import_evidence(repo_id,file_id,language,source_specifier,local_name,import_kind,is_static,owner_module)
		VALUES(?,?, 'swift','',?,?,?,?)`, f.repoID, file, name, kind, value, owner); err != nil {
		f.t.Fatal(err)
	}
}

func (f *swiftScopeFixture) declarationFact(file, symbol int64, final bool) {
	f.dispatchFact(file, symbol, final, "instance")
}

func (f *swiftScopeFixture) dispatchFact(file, symbol int64, final bool, dispatch string) {
	f.t.Helper()
	v := 0
	if final {
		v = 1
	}
	if _, err := f.store.db.ExecContext(f.ctx, `INSERT INTO swift_declaration_facts(repo_id,file_id,symbol_id,is_final,dispatch_kind) VALUES(?,?,?,?,?)`, f.repoID, file, symbol, v, dispatch); err != nil {
		f.t.Fatal(err)
	}
}

func (f *swiftScopeFixture) relation(file int64, child, target, kind string, generic, constrained bool) {
	f.t.Helper()
	g, c := 0, 0
	if generic {
		g = 1
	}
	if constrained {
		c = 1
	}
	if _, err := f.store.db.ExecContext(f.ctx, `INSERT INTO swift_inheritance_relations(repo_id,file_id,child_qualified_name,target_qualified_name,relation_kind,start_line,start_col,end_line,end_col,is_generic,is_constrained) VALUES(?,?,?,?,?,1,1,1,1,?,?)`, f.repoID, file, child, target, kind, g, c); err != nil {
		f.t.Fatal(err)
	}
}

func (f *swiftScopeFixture) dst(edge int64) sql.NullInt64 {
	f.t.Helper()
	var dst sql.NullInt64
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT dst_symbol_id FROM edges WHERE id=?`, edge).Scan(&dst); err != nil {
		f.t.Fatal(err)
	}
	return dst
}

func (f *swiftScopeFixture) symbolID(qualified string) int64 {
	f.t.Helper()
	var id int64
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT id FROM symbols WHERE qualified_name=?`, qualified).Scan(&id); err != nil {
		f.t.Fatal(err)
	}
	return id
}

func (f *swiftScopeFixture) resolve() {
	f.t.Helper()
	if _, err := f.store.ResolveEdges(f.ctx, f.repoID); err != nil {
		f.t.Fatal(err)
	}
}

func (f *swiftScopeFixture) resolveNames(names ...string) ResolveEdgesForNamesStats {
	f.t.Helper()
	stats, err := f.store.ResolveEdgesForNamesWithStats(f.ctx, f.repoID, names)
	if err != nil {
		f.t.Fatal(err)
	}
	if got := stats.TargetsResolved + stats.TargetsUnresolved + stats.UnknownSrcLanguage; got != stats.TargetsSelected {
		f.t.Fatalf("stats invariant: resolved=%d unresolved=%d unknown=%d selected=%d", stats.TargetsResolved, stats.TargetsUnresolved, stats.UnknownSrcLanguage, stats.TargetsSelected)
	}
	return stats
}

func newSwiftMultilevelSelfFixture(t *testing.T, depth int) (*swiftScopeFixture, int64, int64) {
	t.Helper()
	f := newSwiftScopeFixture(t)
	owners := make([]string, depth)
	for i := range owners {
		owners[i] = []string{"Child", "Middle", "Middle2", "Middle3"}[i]
	}
	base := f.symbol(f.mainFile, "Base", "", "class", "", false)
	f.declarationFact(f.mainFile, base, false)
	target := f.symbol(f.mainFile, "run", "Base", "function", "run()", false)
	f.declarationFact(f.mainFile, target, true)
	for i, owner := range owners {
		id := f.symbol(f.mainFile, owner, "", "class", "", false)
		f.declarationFact(f.mainFile, id, false)
		if i > 0 {
			f.relation(f.mainFile, owners[i-1], owner, "superclass", false, false)
		}
	}
	f.relation(f.mainFile, owners[depth-1], "Base", "superclass", false, false)
	caller := f.symbol(f.mainFile, "f", "Child", "function", "f()", false)
	edge := f.call(f.mainFile, caller, "self.run", "swift:self", 0, 1)
	f.reference(f.mainFile, caller, "self.run", 1)
	return f, target, edge
}

func TestSwiftClassSelfMultilevelInheritedFinalMethodScope(t *testing.T) {
	for _, depth := range []int{2, 3, 4} {
		t.Run(fmt.Sprintf("%d-hop", depth), func(t *testing.T) {
			f, target, edge := newSwiftMultilevelSelfFixture(t, depth)
			f.resolve()
			assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfMultilevelInheritedFinalMethodScope)
		})
	}
}

func TestSwiftClassSelfMultilevelInheritedFinalMethodScopeTargetBounded(t *testing.T) {
	t.Run("unproven parent above target is ignored", func(t *testing.T) {
		f, target, edge := newSwiftMultilevelSelfFixture(t, 2)
		f.relation(f.mainFile, "Base", "ExternalRoot", "superclass", false, false)
		f.resolve()
		assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfMultilevelInheritedFinalMethodScope)
	})
	t.Run("valid parent above target is ignored", func(t *testing.T) {
		f, target, edge := newSwiftMultilevelSelfFixture(t, 2)
		root := f.symbol(f.mainFile, "GrandBase", "", "class", "", false)
		f.declarationFact(f.mainFile, root, false)
		f.relation(f.mainFile, "Base", "GrandBase", "superclass", false, false)
		f.resolve()
		assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfMultilevelInheritedFinalMethodScope)
	})
	t.Run("nearest candidate wins over ancestor", func(t *testing.T) {
		f, _, edge := newSwiftMultilevelSelfFixture(t, 2)
		candidate := f.symbol(f.mainFile, "run", "Middle", "function", "run()", false)
		f.declarationFact(f.mainFile, candidate, false)
		f.resolve()
		assertSwiftEdgeUnresolved(t, f, edge)
	})
}

func TestSwiftClassSelfMultilevelInheritedFinalMethodScopeIncrementalTransition(t *testing.T) {
	f := newSwiftScopeFixture(t)
	base := f.symbol(f.mainFile, "Base", "", "class", "", false)
	child := f.symbol(f.mainFile, "Child", "", "class", "", false)
	target := f.symbol(f.mainFile, "run", "Base", "function", "run()", false)
	caller := f.symbol(f.mainFile, "f", "Child", "function", "f()", false)
	edge := f.call(f.mainFile, caller, "self.run", "swift:self", 0, 1)
	f.reference(f.mainFile, caller, "self.run", 1)
	f.declarationFact(f.mainFile, base, false)
	f.declarationFact(f.mainFile, child, false)
	f.declarationFact(f.mainFile, target, true)
	f.relation(f.mainFile, "Child", "Base", "superclass", false, false)
	f.resolve()
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfInheritedFinalMethodScope)

	middle := f.symbol(f.mainFile, "Middle", "", "class", "", false)
	f.declarationFact(f.mainFile, middle, false)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET target_qualified_name='Middle' WHERE child_qualified_name='Child'`); err != nil {
		t.Fatal(err)
	}
	f.relation(f.mainFile, "Middle", "Base", "superclass", false, false)
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfMultilevelInheritedFinalMethodScope)

	if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM swift_inheritance_relations WHERE child_qualified_name='Middle'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET target_qualified_name='Base' WHERE child_qualified_name='Child'`); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfInheritedFinalMethodScope)
}

func TestSwiftClassSelfMultilevelInheritedFinalMethodScopeNamesAndStats(t *testing.T) {
	f, target, edge := newSwiftMultilevelSelfFixture(t, 2)
	stats := f.resolveNames("run")
	assertSwiftStats(t, stats, 1, 1, 0, 0)
	assertSwiftEdgeMetadata(t, f, edge, target, ResolutionStrategySwiftClassSelfMultilevelInheritedFinalMethodScope)

	f2, _, edge2 := newSwiftMultilevelSelfFixture(t, 2)
	if _, err := f2.store.db.ExecContext(f2.ctx, `UPDATE swift_declaration_facts SET is_final=0 WHERE symbol_id=(SELECT id FROM symbols WHERE qualified_name='Base.run')`); err != nil {
		t.Fatal(err)
	}
	assertSwiftStats(t, f2.resolveNames("run"), 1, 0, 1, 0)
	assertSwiftEdgeUnresolved(t, f2, edge2)
}

func TestSwiftClassSelfMultilevelInheritedFinalMethodScopeControls(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*swiftScopeFixture, int64, int64)
	}{
		{"non-final target", func(f *swiftScopeFixture, _, target int64) {
			f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET is_final=0 WHERE symbol_id=?`, target)
		}},
		{"static target", func(f *swiftScopeFixture, _, target int64) {
			f.store.db.ExecContext(f.ctx, `UPDATE symbols SET is_static=1 WHERE id=?`, target)
		}},
		{"missing target fact", func(f *swiftScopeFixture, _, target int64) {
			f.store.db.ExecContext(f.ctx, `DELETE FROM swift_declaration_facts WHERE symbol_id=?`, target)
		}},
		{"unproven intermediate", func(f *swiftScopeFixture, _, _ int64) {
			f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET relation_kind='unproven' WHERE child_qualified_name='Middle'`)
		}},
		{"generic intermediate", func(f *swiftScopeFixture, _, _ int64) {
			f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET is_generic=1 WHERE child_qualified_name='Middle'`)
		}},
		{"conformance intermediate", func(f *swiftScopeFixture, _, _ int64) {
			f.relation(f.mainFile, "Middle", "P", "conformance", false, false)
		}},
		{"cycle", func(f *swiftScopeFixture, _, _ int64) {
			f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET target_qualified_name='Child' WHERE child_qualified_name='Middle'`)
		}},
		{"intermediate candidate", func(f *swiftScopeFixture, _, _ int64) {
			candidate := f.symbol(f.mainFile, "run", "Middle", "function", "run()", false)
			f.declarationFact(f.mainFile, candidate, false)
		}},
		{"child candidate", func(f *swiftScopeFixture, _, _ int64) {
			candidate := f.symbol(f.mainFile, "run", "Child", "function", "run()", false)
			f.declarationFact(f.mainFile, candidate, false)
		}},
		{"private target", func(f *swiftScopeFixture, _, target int64) {
			f.store.db.ExecContext(f.ctx, `UPDATE symbols SET visibility='private' WHERE id=?`, target)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, edge := newSwiftMultilevelSelfFixture(t, 2)
			var target int64
			if err := f.store.db.QueryRowContext(f.ctx, `SELECT id FROM symbols WHERE qualified_name='Base.run'`).Scan(&target); err != nil {
				t.Fatal(err)
			}
			tc.edit(f, edge, target)
			f.resolve()
			assertSwiftEdgeUnresolved(t, f, edge)
		})
	}
}

func TestSwiftClassSelfMultilevelInheritedFinalMethodScopeCandidateMatrix(t *testing.T) {
	for _, tc := range []struct {
		name  string
		owner string
		setup func(*swiftScopeFixture, int64)
	}{
		{"intermediate static", "Middle", func(f *swiftScopeFixture, id int64) {
			f.store.db.ExecContext(f.ctx, `UPDATE symbols SET is_static=1 WHERE id=?`, id)
			f.dispatchFact(f.mainFile, id, false, "static")
		}},
		{"intermediate class", "Middle", func(f *swiftScopeFixture, id int64) {
			f.store.db.ExecContext(f.ctx, `UPDATE symbols SET is_static=1 WHERE id=?`, id)
			f.dispatchFact(f.mainFile, id, false, "class")
		}},
		{"intermediate missing fact", "Middle", func(*swiftScopeFixture, int64) {}},
		{"intermediate malformed fact", "Middle", func(f *swiftScopeFixture, id int64) {
			f.dispatchFact(f.mainFile, id, false, "")
		}},
		{"child candidate", "Child", func(f *swiftScopeFixture, id int64) {
			f.declarationFact(f.mainFile, id, false)
		}},
		{"target ordinary competitor", "Base", func(f *swiftScopeFixture, id int64) {
			f.declarationFact(f.mainFile, id, false)
		}},
		{"target static competitor", "Base", func(f *swiftScopeFixture, id int64) {
			f.store.db.ExecContext(f.ctx, `UPDATE symbols SET is_static=1 WHERE id=?`, id)
			f.dispatchFact(f.mainFile, id, false, "static")
		}},
		{"target malformed competitor", "Base", func(f *swiftScopeFixture, id int64) {
			f.dispatchFact(f.mainFile, id, false, "")
		}},
		{"target private competitor", "Base", func(f *swiftScopeFixture, id int64) {
			f.store.db.ExecContext(f.ctx, `UPDATE symbols SET visibility='private' WHERE id=?`, id)
			f.dispatchFact(f.mainFile, id, true, "instance")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, edge := newSwiftMultilevelSelfFixture(t, 2)
			candidate := f.symbol(f.mainFile, "run", tc.owner, "function", "run()", false)
			tc.setup(f, candidate)
			f.resolve()
			assertSwiftEdgeUnresolved(t, f, edge)
		})
	}
}

func TestSwiftClassSelfMultilevelInheritedFinalMethodScopeLifecycle(t *testing.T) {
	f, target, edge := newSwiftMultilevelSelfFixture(t, 2)
	f.resolve()
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfMultilevelInheritedFinalMethodScope)
	redecide := func() {
		t.Helper()
		if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET target_qualified_name='OtherBase' WHERE child_qualified_name='Middle'`); err != nil {
		t.Fatal(err)
	}
	redecide()
	assertSwiftEdgeUnresolved(t, f, edge)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET target_qualified_name='Base' WHERE child_qualified_name='Middle'`); err != nil {
		t.Fatal(err)
	}
	redecide()
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfMultilevelInheritedFinalMethodScope)

	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET is_final=0 WHERE symbol_id=?`, target); err != nil {
		t.Fatal(err)
	}
	redecide()
	assertSwiftEdgeUnresolved(t, f, edge)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET is_final=1 WHERE symbol_id=?`, target); err != nil {
		t.Fatal(err)
	}
	redecide()
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfMultilevelInheritedFinalMethodScope)
}

func TestSwiftClassSelfMultilevelInheritedFinalMethodScopeRepair(t *testing.T) {
	f, target, edge := newSwiftMultilevelSelfFixture(t, 2)
	if got := f.dst(edge); got.Valid {
		t.Fatalf("pre-repair dst=%v", got)
	}
	run, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID)
	if err != nil || !run {
		t.Fatalf("RepairResolverBindingsOnce=(%v,%v)", run, err)
	}
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfMultilevelInheritedFinalMethodScope)
	assertSwiftReferenceTarget(t, f, swiftReferenceID(t, f, edge), target)
	var marker string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT value FROM settings WHERE key=?`, swiftClassSelfMultilevelInheritedFinalMethodRepairSettingKey+fmt.Sprintf(".%d", f.repoID)).Scan(&marker); err != nil || marker != "1" {
		t.Fatalf("marker=%q err=%v", marker, err)
	}
	firstDst, firstStrategy, firstConfidence := edgeState(t, f, edge)
	firstReference := swiftReferenceSymbol(t, f, swiftReferenceID(t, f, edge))
	run, err = f.store.RepairResolverBindingsOnce(f.ctx, f.repoID)
	if err != nil || run {
		t.Fatalf("second RepairResolverBindingsOnce=(%v,%v)", run, err)
	}
	secondDst, secondStrategy, secondConfidence := edgeState(t, f, edge)
	secondReference := swiftReferenceSymbol(t, f, swiftReferenceID(t, f, edge))
	if firstDst != secondDst || firstStrategy != secondStrategy || firstConfidence != secondConfidence || firstReference != secondReference {
		t.Fatalf("second repair changed state: first=(%v,%q,%q,%v), second=(%v,%q,%q,%v)", firstDst, firstStrategy, firstConfidence, firstReference, secondDst, secondStrategy, secondConfidence, secondReference)
	}
}

func TestSwiftClassSelfMultilevelInheritedFinalMethodScopeUnsafeRepair(t *testing.T) {
	f, target, edge := newSwiftMultilevelSelfFixture(t, 2)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET dst_symbol_id=?,resolution_strategy=?,resolution_confidence=? WHERE id=?`, target, ResolutionStrategySwiftClassSelfMultilevelInheritedFinalMethodScope, ResolutionConfidenceHigh, edge); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE references_tbl SET symbol_id=? WHERE context_symbol_id=(SELECT src_symbol_id FROM edges WHERE id=?)`, target, edge); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET is_final=0 WHERE symbol_id=?`, target); err != nil {
		t.Fatal(err)
	}
	for _, repair := range resolverRepairs {
		if repair.key != swiftClassSelfMultilevelInheritedFinalMethodRepairSettingKey {
			if err := f.store.markRepairDone(f.ctx, repair.key, f.repoID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if ran, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID); err != nil || !ran {
		t.Fatalf("RepairResolverBindingsOnce=(%v,%v)", ran, err)
	}
	assertSwiftBindingCleared(t, f, edge)
}

func TestSwiftClassSelfMultilevelInheritedFinalMethodScopeHardenedStress(t *testing.T) {
	type stressCase struct{ name, strategy string }
	cases := []stressCase{
		{"two_hop", ResolutionStrategySwiftClassSelfMultilevelInheritedFinalMethodScope}, {"three_hop", ResolutionStrategySwiftClassSelfMultilevelInheritedFinalMethodScope},
		{"four_hop", ResolutionStrategySwiftClassSelfMultilevelInheritedFinalMethodScope}, {"final_child", ResolutionStrategySwiftClassSelfMultilevelInheritedFinalMethodScope},
		{"qualified", ResolutionStrategySwiftClassSelfMultilevelInheritedFinalMethodScope}, {"reverse", ResolutionStrategySwiftClassSelfMultilevelInheritedFinalMethodScope},
		{"labels", ResolutionStrategySwiftClassSelfMultilevelInheritedFinalMethodScope}, {"trailing", ResolutionStrategySwiftClassSelfMultilevelInheritedFinalMethodScope},
		{"same_owner_final_instance", ResolutionStrategySwiftClassSelfFinalMethodScope}, {"direct_p22_75", ResolutionStrategySwiftClassSelfInheritedFinalMethodScope}, {"same_owner_final_class", ResolutionStrategySwiftClassSelfFinalClassMethodScope},
		{"non_final", ""}, {"static_target", ""}, {"class_target", ""}, {"malformed_target", ""}, {"missing_target_fact", ""}, {"duplicate_target_fact", ""}, {"private_target", ""},
		{"missing_first_relation", ""}, {"missing_intermediate_relation", ""}, {"duplicate_first_relation", ""}, {"duplicate_intermediate_relation", ""}, {"unproven_first", ""}, {"unproven_intermediate", ""}, {"conformance_first", ""}, {"conformance_intermediate", ""}, {"generic_first", ""}, {"generic_intermediate", ""}, {"constrained_first", ""}, {"constrained_intermediate", ""}, {"cycle", ""}, {"competing_first", ""}, {"competing_intermediate", ""},
		{"missing_child", ""}, {"duplicate_child", ""}, {"missing_child_fact", ""}, {"duplicate_child_fact", ""}, {"missing_middle", ""}, {"duplicate_middle", ""}, {"missing_middle_fact", ""}, {"duplicate_middle_fact", ""}, {"missing_base", ""}, {"duplicate_base", ""},
		{"cross_middle", ""}, {"cross_base", ""}, {"cross_target", ""}, {"cross_first_relation", ""}, {"cross_intermediate_relation", ""},
		{"target_bounded_parent", ResolutionStrategySwiftClassSelfMultilevelInheritedFinalMethodScope},
		{"child_candidate", ""}, {"intermediate_instance_candidate", ""}, {"intermediate_static_candidate", ""}, {"intermediate_class_candidate", ""}, {"intermediate_malformed_candidate", ""}, {"intermediate_missing_fact_candidate", ""}, {"intermediate_private_candidate", ""},
		{"target_instance_competitor", ""}, {"target_static_competitor", ""}, {"target_class_competitor", ""}, {"target_malformed_competitor", ""}, {"target_missing_fact_competitor", ""}, {"target_duplicate_fact_competitor", ""}, {"target_private_competitor", ""},
		{"relevant_blocker", ""}, {"irrelevant_blocker", ResolutionStrategySwiftClassSelfMultilevelInheritedFinalMethodScope}, {"trailing_ambiguity", ""}, {"trailing_unsafe", ""}, {"trailing_intermediate", ""},
	}
	const perCase = 100
	f := newSwiftScopeFixture(t)
	samples := map[string]int64{}
	targets := map[string]int64{}
	for _, tc := range cases {
		prefix := "Stress_" + tc.name
		childName, middleName, baseName := prefix+"Child", prefix+"Middle", prefix+"Base"
		if tc.name == "qualified" {
			childName, middleName, baseName = "Outer."+childName, "Outer."+middleName, "Outer."+baseName
		}
		child := f.symbol(f.mainFile, childName, "", "class", "", false)
		middle := f.symbol(f.mainFile, middleName, "", "class", "", false)
		base := f.symbol(f.mainFile, baseName, "", "class", "", false)
		f.declarationFact(f.mainFile, child, false)
		f.declarationFact(f.mainFile, middle, false)
		f.declarationFact(f.mainFile, base, false)
		f.relation(f.mainFile, childName, middleName, "superclass", false, false)
		f.relation(f.mainFile, middleName, baseName, "superclass", false, false)
		if tc.name == "three_hop" || tc.name == "four_hop" {
			middle2 := prefix + "Middle2"
			id := f.symbol(f.mainFile, middle2, "", "class", "", false)
			f.declarationFact(f.mainFile, id, false)
			f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET target_qualified_name=? WHERE child_qualified_name=?`, middle2, childName)
			f.relation(f.mainFile, middle2, middleName, "superclass", false, false)
			if tc.name == "four_hop" {
				middle3 := prefix + "Middle3"
				id = f.symbol(f.mainFile, middle3, "", "class", "", false)
				f.declarationFact(f.mainFile, id, false)
				f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET target_qualified_name=? WHERE child_qualified_name=?`, middle3, childName)
				f.relation(f.mainFile, middle3, middle2, "superclass", false, false)
			}
		}
		method := "run_" + tc.name
		target := f.symbol(f.mainFile, method, baseName, "function", method+"()", false)
		f.declarationFact(f.mainFile, target, true)
		if tc.name == "final_child" {
			f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET is_final=1 WHERE symbol_id=?`, child)
		}
		if tc.name == "same_owner_final_instance" || tc.name == "same_owner_final_class" {
			if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM swift_inheritance_relations WHERE child_qualified_name=?`, childName); err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET container_name=?,qualified_name=? WHERE id=?`, childName, childName+"."+method, target); err != nil {
				t.Fatal(err)
			}
		}
		if tc.name == "direct_p22_75" {
			if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM swift_inheritance_relations WHERE child_qualified_name=?`, middleName); err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET target_qualified_name=? WHERE child_qualified_name=?`, baseName, childName); err != nil {
				t.Fatal(err)
			}
		}
		if tc.name == "same_owner_final_class" {
			f.store.db.ExecContext(f.ctx, `UPDATE symbols SET is_static=1 WHERE id=?`, target)
			f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET dispatch_kind='class' WHERE symbol_id=?`, target)
		}
		if tc.name == "target_bounded_parent" {
			f.relation(f.mainFile, baseName, prefix+"ExternalRoot", "superclass", false, false)
		}
		if tc.name == "reverse" {
			grandChild := f.symbol(f.mainFile, prefix+"GrandChild", "", "class", "", false)
			f.declarationFact(f.mainFile, grandChild, false)
			f.relation(f.mainFile, prefix+"GrandChild", childName, "superclass", false, false)
		}
		switch tc.name {
		case "non_final":
			f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET is_final=0 WHERE symbol_id=?`, target)
		case "static_target":
			f.store.db.ExecContext(f.ctx, `UPDATE symbols SET is_static=1 WHERE id=?`, target)
		case "class_target":
			f.dispatchFact(f.mainFile, target, false, "class")
		case "malformed_target":
			f.dispatchFact(f.mainFile, target, false, "")
		case "missing_target_fact":
			f.store.db.ExecContext(f.ctx, `DELETE FROM swift_declaration_facts WHERE symbol_id=?`, target)
		case "duplicate_target_fact":
			f.declarationFact(f.mainFile, target, true)
		case "private_target":
			f.store.db.ExecContext(f.ctx, `UPDATE symbols SET visibility='private' WHERE id=?`, target)
		case "missing_first_relation":
			f.store.db.ExecContext(f.ctx, `DELETE FROM swift_inheritance_relations WHERE child_qualified_name=?`, childName)
		case "missing_intermediate_relation":
			f.store.db.ExecContext(f.ctx, `DELETE FROM swift_inheritance_relations WHERE child_qualified_name=?`, middleName)
		case "duplicate_first_relation":
			f.relation(f.mainFile, childName, middleName, "superclass", false, false)
		case "duplicate_intermediate_relation":
			f.relation(f.mainFile, middleName, baseName, "superclass", false, false)
		case "unproven_first":
			f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET relation_kind='unproven' WHERE child_qualified_name=?`, childName)
		case "unproven_intermediate":
			f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET relation_kind='unproven' WHERE child_qualified_name=?`, middleName)
		case "conformance_first":
			f.relation(f.mainFile, childName, "P", "conformance", false, false)
		case "conformance_intermediate":
			f.relation(f.mainFile, middleName, "P", "conformance", false, false)
		case "generic_first":
			f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET is_generic=1 WHERE child_qualified_name=?`, childName)
		case "generic_intermediate":
			f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET is_generic=1 WHERE child_qualified_name=?`, middleName)
		case "constrained_first":
			f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET is_constrained=1 WHERE child_qualified_name=?`, childName)
		case "constrained_intermediate":
			f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET is_constrained=1 WHERE child_qualified_name=?`, middleName)
		case "cycle":
			f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET target_qualified_name=? WHERE child_qualified_name=?`, childName, middleName)
		case "competing_first":
			f.relation(f.mainFile, childName, prefix+"Other", "superclass", false, false)
		case "competing_intermediate":
			f.relation(f.mainFile, middleName, prefix+"Other", "superclass", false, false)
		case "missing_child", "missing_middle", "missing_base":
			f.store.db.ExecContext(f.ctx, `UPDATE symbols SET kind='struct' WHERE id=?`, map[string]int64{"missing_child": child, "missing_middle": middle, "missing_base": base}[tc.name])
		case "duplicate_child", "duplicate_middle", "duplicate_base":
			f.symbol(f.mainFile, map[string]string{"duplicate_child": childName, "duplicate_middle": middleName, "duplicate_base": baseName}[tc.name], "", "class", "", false)
		case "missing_child_fact", "missing_middle_fact", "missing_base_fact":
			f.store.db.ExecContext(f.ctx, `DELETE FROM swift_declaration_facts WHERE symbol_id=?`, map[string]int64{"missing_child_fact": child, "missing_middle_fact": middle, "missing_base_fact": base}[tc.name])
		case "duplicate_child_fact", "duplicate_middle_fact", "duplicate_base_fact":
			f.declarationFact(f.mainFile, map[string]int64{"duplicate_child_fact": child, "duplicate_middle_fact": middle, "duplicate_base_fact": base}[tc.name], false)
		case "child_candidate", "intermediate_instance_candidate", "intermediate_static_candidate", "intermediate_class_candidate", "intermediate_malformed_candidate", "intermediate_missing_fact_candidate", "intermediate_private_candidate":
			owner := middleName
			if tc.name == "child_candidate" {
				owner = childName
			}
			candidate := f.symbol(f.mainFile, method, owner, "function", method+"()", strings.Contains(tc.name, "static") || strings.Contains(tc.name, "class"))
			if tc.name != "intermediate_missing_fact_candidate" {
				dispatch := "instance"
				if tc.name == "intermediate_static_candidate" {
					dispatch = "static"
				} else if tc.name == "intermediate_class_candidate" {
					dispatch = "class"
				} else if tc.name == "intermediate_malformed_candidate" {
					dispatch = ""
				}
				f.dispatchFact(f.mainFile, candidate, false, dispatch)
			}
			if tc.name == "intermediate_private_candidate" {
				f.store.db.ExecContext(f.ctx, `UPDATE symbols SET visibility='private' WHERE id=?`, candidate)
			}
		case "relevant_blocker":
			f.blocker(f.mainFile, childName, method, graph.ScopeImportSwiftMemberValue, false)
		case "irrelevant_blocker":
			f.blocker(f.mainFile, childName, method, graph.ScopeImportSwiftMemberValue, true)
		case "cross_middle":
			file := f.file(prefix + "Middle.swift")
			f.store.db.ExecContext(f.ctx, `UPDATE symbols SET file_id=? WHERE id=?`, file, middle)
		case "cross_base":
			file := f.file(prefix + "Base.swift")
			f.store.db.ExecContext(f.ctx, `UPDATE symbols SET file_id=? WHERE id=?`, file, base)
		case "cross_target":
			file := f.file(prefix + "Target.swift")
			f.store.db.ExecContext(f.ctx, `UPDATE symbols SET file_id=? WHERE id=?`, file, target)
		case "cross_first_relation", "cross_intermediate_relation":
			file := f.file(prefix + "Relation.swift")
			owner := childName
			if tc.name == "cross_intermediate_relation" {
				owner = middleName
			}
			f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET file_id=? WHERE child_qualified_name=?`, file, owner)
		case "target_instance_competitor", "target_static_competitor", "target_class_competitor", "target_malformed_competitor", "target_missing_fact_competitor", "target_duplicate_fact_competitor", "target_private_competitor":
			candidate := f.symbol(f.mainFile, method, baseName, "function", method+"()", tc.name == "target_static_competitor" || tc.name == "target_class_competitor")
			switch tc.name {
			case "target_static_competitor":
				f.dispatchFact(f.mainFile, candidate, false, "static")
			case "target_class_competitor":
				f.dispatchFact(f.mainFile, candidate, false, "class")
			case "target_malformed_competitor":
				f.dispatchFact(f.mainFile, candidate, false, "")
			case "target_duplicate_fact_competitor":
				f.dispatchFact(f.mainFile, candidate, true, "instance")
				f.dispatchFact(f.mainFile, candidate, true, "instance")
			case "target_private_competitor":
				f.store.db.ExecContext(f.ctx, `UPDATE symbols SET visibility='private' WHERE id=?`, candidate)
				f.dispatchFact(f.mainFile, candidate, true, "instance")
			case "target_missing_fact_competitor":
			default:
				f.dispatchFact(f.mainFile, candidate, false, "instance")
			}
		case "trailing_ambiguity", "trailing_unsafe", "trailing_intermediate":
			owner := baseName
			if tc.name == "trailing_intermediate" {
				owner = middleName
			}
			candidate := f.symbol(f.mainFile, "perform_"+tc.name, owner, "function", "perform_"+tc.name+"(_:)", false)
			f.arity(candidate, 1, 1)
			if tc.name == "trailing_ambiguity" {
				f.declarationFact(f.mainFile, candidate, true)
			} else {
				f.dispatchFact(f.mainFile, candidate, false, "instance")
			}
		}
		for i := 0; i < perCase; i++ {
			caller := f.symbol(f.mainFile, fmt.Sprintf("%sCaller%d", prefix, i), childName, "function", "f()", tc.name == "same_owner_final_class")
			if tc.name == "same_owner_final_class" {
				f.dispatchFact(f.mainFile, caller, false, "class")
			}
			name, evidence, arity := "self."+method, "swift:self", 0
			if tc.name == "labels" {
				name, evidence, arity = "self."+method, "swift:self;labels=id:", 1
				f.arity(target, 1, 1)
				f.store.db.ExecContext(f.ctx, `UPDATE symbols SET signature=? WHERE id=?`, method+"(id:)", target)
			}
			if tc.name == "trailing" || strings.HasPrefix(tc.name, "trailing_") {
				name, evidence, arity = "self.perform_"+tc.name, "swift:self;trailing_labels=_", 1
				f.arity(target, 1, 1)
				f.store.db.ExecContext(f.ctx, `UPDATE symbols SET name=?,signature=? WHERE id=?`, "perform_"+tc.name, "perform_"+tc.name+"(_:)", target)
			}
			edge := f.call(f.mainFile, caller, name, evidence, arity, i+1)
			if i == 0 {
				samples[tc.name], targets[tc.name] = edge, target
			}
		}
	}
	f.resolve()
	var total, resolved, unresolved, badMetadata int
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*),COALESCE(SUM(dst_symbol_id IS NOT NULL),0),COALESCE(SUM(dst_symbol_id IS NULL),0),COALESCE(SUM(dst_symbol_id IS NULL AND (resolution_strategy<>'' OR resolution_confidence<>'')),0) FROM edges WHERE repo_id=?`, f.repoID).Scan(&total, &resolved, &unresolved, &badMetadata); err != nil {
		t.Fatal(err)
	}
	if total != len(cases)*perCase || badMetadata != 0 {
		t.Fatalf("state=(total %d,resolved %d,unresolved %d,badMetadata %d)", total, resolved, unresolved, badMetadata)
	}
	type stressState struct {
		total, resolved, unresolved, badMetadata int
		strategies                               map[string]int
	}
	readState := func() stressState {
		var out stressState
		if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*),COALESCE(SUM(dst_symbol_id IS NOT NULL),0),COALESCE(SUM(dst_symbol_id IS NULL),0),COALESCE(SUM(dst_symbol_id IS NULL AND (resolution_strategy<>'' OR resolution_confidence<>'')),0) FROM edges WHERE repo_id=?`, f.repoID).Scan(&out.total, &out.resolved, &out.unresolved, &out.badMetadata); err != nil {
			t.Fatal(err)
		}
		rows, err := f.store.db.QueryContext(f.ctx, `SELECT resolution_strategy,COUNT(*) FROM edges WHERE repo_id=? GROUP BY resolution_strategy ORDER BY resolution_strategy`, f.repoID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out.strategies = map[string]int{}
		for rows.Next() {
			var strategy string
			var count int
			if err := rows.Scan(&strategy, &count); err != nil {
				t.Fatal(err)
			}
			out.strategies[strategy] = count
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	first := readState()
	want := stressState{total: len(cases) * perCase, resolved: 1300, unresolved: 5500, badMetadata: 0, strategies: map[string]int{}}
	for _, tc := range cases {
		want.strategies[tc.strategy] += perCase
	}
	for _, tc := range cases {
		gotDst, gotStrategy, gotConfidence := edgeState(t, f, samples[tc.name])
		if gotStrategy != tc.strategy {
			t.Fatalf("category %s strategy=%q want=%q", tc.name, gotStrategy, tc.strategy)
		}
		if tc.strategy == "" {
			if gotDst.Valid || gotConfidence != "" {
				t.Fatalf("category %s unresolved state=(%v,%q,%q)", tc.name, gotDst, gotStrategy, gotConfidence)
			}
		} else if !gotDst.Valid || gotDst.Int64 != targets[tc.name] || gotConfidence != ResolutionConfidenceHigh {
			t.Fatalf("category %s resolved state=(%v,%q,%q), want target=%d", tc.name, gotDst, gotStrategy, gotConfidence, targets[tc.name])
		}
	}
	if !reflect.DeepEqual(first, want) {
		t.Fatalf("stress state=%#v want=%#v", first, want)
	}
	if got := f.dst(samples["two_hop"]); !got.Valid || got.Int64 != targets["two_hop"] {
		t.Fatalf("2-hop=%v", got)
	}
	if got := f.dst(samples["four_hop"]); !got.Valid || got.Int64 != targets["four_hop"] {
		t.Fatalf("4-hop=%v", got)
	}
	if got := f.dst(samples["direct_p22_75"]); !got.Valid || got.Int64 != targets["direct_p22_75"] {
		t.Fatalf("direct=%v", got)
	}
	if got := f.dst(samples["non_final"]); got.Valid {
		t.Fatalf("hazard=%v", got)
	}
	f.resolve()
	if second := readState(); !reflect.DeepEqual(second, first) {
		t.Fatalf("second strategy state=%v first=%v", second, first)
	}
}

func newSwiftInheritedStaticSelfFixture(t *testing.T, staticCaller bool) (*swiftScopeFixture, int64, int64) {
	t.Helper()
	f := newSwiftScopeFixture(t)
	base := f.symbol(f.mainFile, "Base", "", "class", "", false)
	child := f.symbol(f.mainFile, "Child", "", "class", "", false)
	target := f.symbol(f.mainFile, "make", "Base", "function", "make()", true)
	caller := f.symbol(f.mainFile, "f", "Child", "function", "f()", staticCaller)
	edge := f.call(f.mainFile, caller, "Self.make", "swift:Self", 0, 1)
	f.reference(f.mainFile, caller, "Self.make", 1)
	f.declarationFact(f.mainFile, base, false)
	f.declarationFact(f.mainFile, child, false)
	f.dispatchFact(f.mainFile, target, false, "static")
	f.relation(f.mainFile, "Child", "Base", "superclass", false, false)
	return f, target, edge
}

func TestSwiftClassSelfTypeInheritedStaticMethodScope(t *testing.T) {
	for _, staticCaller := range []bool{false, true} {
		f, target, edge := newSwiftInheritedStaticSelfFixture(t, staticCaller)
		f.resolve()
		assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeInheritedStaticMethodScope)
	}
	t.Run("final child", func(t *testing.T) {
		f, target, edge := newSwiftInheritedStaticSelfFixture(t, false)
		if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET is_final=1 WHERE symbol_id=(SELECT id FROM symbols WHERE qualified_name='Child')`); err != nil {
			t.Fatal(err)
		}
		f.resolve()
		assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeInheritedStaticMethodScope)
	})
}

func TestSwiftClassSelfTypeInheritedStaticMethodScopeSelectors(t *testing.T) {
	for _, tc := range []struct {
		name, symbolName, signature, evidence, dst string
		arity                                      int
	}{
		{"zero", "make", "make()", "swift:Self", "Self.make", 0},
		{"underscore", "make", "make(_:)", "swift:Self;labels=_", "Self.make", 1},
		{"named", "make", "make(id:)", "swift:Self;labels=id:", "Self.make", 1},
		{"two labels", "make", "make(id:,cache:)", "swift:Self;labels=id:,cache:", "Self.make", 2},
		{"trailing", "perform", "perform(_:)", "swift:Self;trailing_labels=_", "Self.perform", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, target, edge := newSwiftInheritedStaticSelfFixture(t, false)
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET name=?,qualified_name='Base.'||?,signature=?,arity_min=?,arity_max=? WHERE id=?`, tc.symbolName, tc.symbolName, tc.signature, tc.arity, tc.arity, target); err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET dst_name=?,evidence=?,call_arity=? WHERE id=?`, tc.dst, tc.evidence, tc.arity, edge); err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE references_tbl SET name=?,qualified_name=? WHERE repo_id=?`, tc.dst, tc.dst, f.repoID); err != nil {
				t.Fatal(err)
			}
			f.resolve()
			assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeInheritedStaticMethodScope)
		})
	}
}

func TestSwiftClassSelfTypeInheritedStaticMethodScopeControls(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*swiftScopeFixture, int64)
	}{
		{"ordinary class", func(f *swiftScopeFixture, target int64) {
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET dispatch_kind='class',is_final=0 WHERE symbol_id=?`, target); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"final class", func(f *swiftScopeFixture, target int64) {
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET dispatch_kind='class',is_final=1 WHERE symbol_id=?`, target); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"private", func(f *swiftScopeFixture, target int64) {
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET visibility='private' WHERE id=?`, target); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"grandparent", func(f *swiftScopeFixture, _ int64) {
			middle := f.symbol(f.mainFile, "Middle", "", "class", "", false)
			f.declarationFact(f.mainFile, middle, false)
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET target_qualified_name='Middle' WHERE child_qualified_name='Child'`); err != nil {
				f.t.Fatal(err)
			}
			f.relation(f.mainFile, "Middle", "Base", "superclass", false, false)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, target, edge := newSwiftInheritedStaticSelfFixture(t, false)
			tc.setup(f, target)
			f.resolve()
			if tc.name == "final class" {
				assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeInheritedFinalClassMethodScope)
			} else {
				assertSwiftInheritedStaticEdgeUnresolved(t, f, edge)
			}
		})
	}
}

func TestSwiftClassSelfTypeInheritedStaticMethodScopeNamesAndLifecycle(t *testing.T) {
	f, target, edge := newSwiftInheritedStaticSelfFixture(t, false)
	assertSwiftStats(t, f.resolveNames("make"), 1, 1, 0, 0)
	assertSwiftEdgeMetadata(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeInheritedStaticMethodScope)

	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET dispatch_kind='class' WHERE symbol_id=?`, target); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftInheritedStaticEdgeUnresolved(t, f, edge)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET dispatch_kind='static' WHERE symbol_id=?`, target); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeInheritedStaticMethodScope)

	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET target_qualified_name='OtherBase' WHERE child_qualified_name='Child'`); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftInheritedStaticEdgeUnresolved(t, f, edge)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET target_qualified_name='Base' WHERE child_qualified_name='Child'`); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeInheritedStaticMethodScope)
}

func TestSwiftClassSelfTypeInheritedStaticMethodScopeRejectsUnsafeCandidates(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*swiftScopeFixture, int64)
	}{
		{"child candidate", func(f *swiftScopeFixture, _ int64) {
			candidate := f.symbol(f.mainFile, "make", "Child", "function", "make()", true)
			f.dispatchFact(f.mainFile, candidate, false, "static")
		}},
		{"unsafe base competitor", func(f *swiftScopeFixture, _ int64) {
			candidate := f.symbol(f.mainFile, "make", "Base", "function", "make()", false)
			f.dispatchFact(f.mainFile, candidate, false, "instance")
		}},
		{"static blocker", func(f *swiftScopeFixture, _ int64) {
			f.blocker(f.mainFile, "Child", "make", graph.ScopeImportSwiftMemberValue, true)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, target, edge := newSwiftInheritedStaticSelfFixture(t, false)
			tc.setup(f, target)
			f.resolve()
			assertSwiftInheritedStaticEdgeUnresolved(t, f, edge)
		})
	}
}

func TestSwiftClassSelfTypeInheritedStaticMethodScopeUpgradeRepair(t *testing.T) {
	f, target, edge := newSwiftInheritedStaticSelfFixture(t, false)
	for _, repair := range resolverRepairs {
		if repair.key == swiftClassSelfTypeInheritedStaticMethodRepairSettingKey {
			continue
		}
		if err := f.store.markRepairDone(f.ctx, repair.key, f.repoID); err != nil {
			t.Fatal(err)
		}
	}
	assertSwiftInheritedStaticEdgeUnresolved(t, f, edge)
	refID := swiftReferenceID(t, f, edge)
	assertSwiftReferenceCleared(t, f, refID)
	var marker string
	markerErr := f.store.db.QueryRowContext(f.ctx, `SELECT value FROM settings WHERE key=?`, swiftClassSelfTypeInheritedStaticMethodRepairSettingKey+fmt.Sprintf(".%d", f.repoID)).Scan(&marker)
	if markerErr != sql.ErrNoRows {
		t.Fatalf("pre-repair marker=%q err=%v, want absent", marker, markerErr)
	}
	run, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID)
	if err != nil || !run {
		t.Fatalf("repair=(%v,%v)", run, err)
	}
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeInheritedStaticMethodScope)
	assertSwiftReferenceTarget(t, f, refID, target)
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT value FROM settings WHERE key=?`, swiftClassSelfTypeInheritedStaticMethodRepairSettingKey+fmt.Sprintf(".%d", f.repoID)).Scan(&marker); err != nil || marker != "1" {
		t.Fatalf("post-repair marker=%q err=%v", marker, err)
	}
	var firstDst sql.NullInt64
	var firstStrategy, firstConfidence string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT dst_symbol_id,resolution_strategy,resolution_confidence FROM edges WHERE id=?`, edge).Scan(&firstDst, &firstStrategy, &firstConfidence); err != nil {
		t.Fatal(err)
	}
	firstReference := swiftReferenceSymbol(t, f, refID)
	if !firstReference.Valid || firstReference.Int64 != target {
		t.Fatalf("first reference=%v, want %d", firstReference, target)
	}
	firstMarker := marker
	run, err = f.store.RepairResolverBindingsOnce(f.ctx, f.repoID)
	if err != nil || run {
		t.Fatalf("second repair=(%v,%v)", run, err)
	}
	var secondDst sql.NullInt64
	var secondStrategy, secondConfidence string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT dst_symbol_id,resolution_strategy,resolution_confidence FROM edges WHERE id=?`, edge).Scan(&secondDst, &secondStrategy, &secondConfidence); err != nil {
		t.Fatal(err)
	}
	secondReference := swiftReferenceSymbol(t, f, refID)
	var secondMarker string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT value FROM settings WHERE key=?`, swiftClassSelfTypeInheritedStaticMethodRepairSettingKey+fmt.Sprintf(".%d", f.repoID)).Scan(&secondMarker); err != nil {
		t.Fatal(err)
	}
	if firstDst != secondDst || firstStrategy != secondStrategy || firstConfidence != secondConfidence || firstReference != secondReference || firstMarker != secondMarker {
		t.Fatalf("second repair changed state: first=(%v,%q,%q,%v,%q), second=(%v,%q,%q,%v,%q)", firstDst, firstStrategy, firstConfidence, firstReference, firstMarker, secondDst, secondStrategy, secondConfidence, secondReference, secondMarker)
	}
}

func TestSwiftClassSelfTypeInheritedStaticMethodScopeUnsafeRepair(t *testing.T) {
	f, target, edge := newSwiftInheritedStaticSelfFixture(t, false)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET dst_symbol_id=?,resolution_strategy=?,resolution_confidence=? WHERE id=?`, target, ResolutionStrategySwiftClassSelfTypeInheritedStaticMethodScope, ResolutionConfidenceHigh, edge); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE references_tbl SET symbol_id=? WHERE repo_id=?`, target, f.repoID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET dispatch_kind='class' WHERE symbol_id=?`, target); err != nil {
		t.Fatal(err)
	}
	for _, repair := range resolverRepairs {
		if repair.key == swiftClassSelfTypeInheritedStaticMethodRepairSettingKey {
			continue
		}
		if err := f.store.markRepairDone(f.ctx, repair.key, f.repoID); err != nil {
			t.Fatal(err)
		}
	}
	if ran, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID); err != nil || !ran {
		t.Fatalf("repair=(%v,%v)", ran, err)
	}
	assertSwiftInheritedStaticEdgeUnresolved(t, f, edge)
	refID := swiftReferenceID(t, f, edge)
	assertSwiftReferenceCleared(t, f, refID)
	var marker string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT value FROM settings WHERE key=?`, swiftClassSelfTypeInheritedStaticMethodRepairSettingKey+fmt.Sprintf(".%d", f.repoID)).Scan(&marker); err != nil || marker != "1" {
		t.Fatalf("marker=%q err=%v", marker, err)
	}
}

func TestSwiftClassSelfTypeInheritedStaticMethodScopeDeletedBlockerLifecycle(t *testing.T) {
	f, target, edge := newSwiftInheritedStaticSelfFixture(t, false)
	f.resolve()
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeInheritedStaticMethodScope)
	blocker := f.file("Blocker.swift")
	f.blocker(blocker, "Child", "make", graph.ScopeImportSwiftMemberValue, true)
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Blocker.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftInheritedStaticEdgeUnresolved(t, f, edge)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE files SET is_deleted=1 WHERE id=?`, blocker); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Blocker.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeInheritedStaticMethodScope)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE files SET is_deleted=0 WHERE id=?`, blocker); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Blocker.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftInheritedStaticEdgeUnresolved(t, f, edge)
}

func TestSwiftClassSelfTypeInheritedStaticMethodScopeCallerAndOwnerFacts(t *testing.T) {
	for _, tc := range []struct {
		name, dispatch string
		static         bool
	}{
		{"instance", "instance", false},
		{"static", "static", true},
		{"class", "class", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, target, edge := newSwiftInheritedStaticSelfFixture(t, tc.static)
			var source int64
			if err := f.store.db.QueryRowContext(f.ctx, `SELECT src_symbol_id FROM edges WHERE id=?`, edge).Scan(&source); err != nil {
				t.Fatal(err)
			}
			f.dispatchFact(f.mainFile, source, false, tc.dispatch)
			f.resolve()
			assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeInheritedStaticMethodScope)
		})
	}
	for _, tc := range []struct {
		name  string
		setup func(*swiftScopeFixture)
	}{
		{"missing child fact", func(f *swiftScopeFixture) {
			f.store.db.ExecContext(f.ctx, `DELETE FROM swift_declaration_facts WHERE symbol_id=(SELECT id FROM symbols WHERE qualified_name='Child')`)
		}},
		{"duplicate child facts", func(f *swiftScopeFixture) {
			f.declarationFact(f.mainFile, f.symbolID("Child"), false)
		}},
		{"final child", func(f *swiftScopeFixture) {
			f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET is_final=1 WHERE symbol_id=(SELECT id FROM symbols WHERE qualified_name='Child')`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, target, edge := newSwiftInheritedStaticSelfFixture(t, false)
			tc.setup(f)
			f.resolve()
			if tc.name == "final child" {
				assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeInheritedStaticMethodScope)
			} else {
				assertSwiftEdgeUnresolved(t, f, edge)
			}
		})
	}
}

func TestSwiftClassSelfTypeInheritedStaticMethodScopeRelationFiltering(t *testing.T) {
	cases := []struct {
		name, callerPath, relationPath, hazardKind string
		want                                       bool
	}{
		{"production relation", "Service.swift", "Service.swift", "", true},
		{"test-only superclass", "Service.swift", "Tests/Relation.swift", "", false},
		{"test caller test relation", "Tests/Service.swift", "Tests/Service.swift", "", true},
		{"production caller test hazard", "Service.swift", "Tests/Hazard.swift", "conformance", true},
		{"production caller production hazard", "Service.swift", "Hazard.swift", "conformance", false},
		{"test caller production hazard", "Tests/Service.swift", "Hazard.swift", "conformance", false},
		{"test caller test hazard", "Tests/Service.swift", "Tests/Hazard.swift", "conformance", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwiftScopeFixture(t)
			callerFile := f.mainFile
			if tc.callerPath != "Service.swift" {
				callerFile = f.file(tc.callerPath)
			}
			base := f.symbol(callerFile, "Base", "", "class", "", false)
			child := f.symbol(callerFile, "Child", "", "class", "", false)
			target := f.symbol(callerFile, "make", "Base", "function", "make()", true)
			caller := f.symbol(callerFile, "f", "Child", "function", "f()", false)
			edge := f.call(callerFile, caller, "Self.make", "swift:Self", 0, 1)
			f.reference(callerFile, caller, "Self.make", 1)
			f.declarationFact(callerFile, base, false)
			f.declarationFact(callerFile, child, false)
			f.dispatchFact(callerFile, target, false, "static")
			if tc.hazardKind == "" && tc.relationPath == tc.callerPath {
				f.relation(callerFile, "Child", "Base", "superclass", false, false)
			} else if tc.hazardKind == "" {
				f.relation(f.file(tc.relationPath), "Child", "Base", "superclass", false, false)
			} else {
				f.relation(callerFile, "Child", "Base", "superclass", false, false)
				f.relation(f.file(tc.relationPath), "Child", "P", tc.hazardKind, false, false)
			}
			f.resolve()
			if tc.want {
				assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeInheritedStaticMethodScope)
			} else {
				assertSwiftEdgeUnresolved(t, f, edge)
			}
		})
	}
}

func TestSwiftClassSelfTypeInheritedStaticMethodScopeIdentityAndVisibility(t *testing.T) {
	for _, tc := range []struct {
		name, visibility string
		want             bool
	}{{"private", "private", false}, {"fileprivate", "fileprivate", true}, {"internal", "internal", true}, {"public", "public", true}, {"default", "", true}} {
		t.Run(tc.name, func(t *testing.T) {
			f, target, edge := newSwiftInheritedStaticSelfFixture(t, false)
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET visibility=? WHERE id=?`, tc.visibility, target); err != nil {
				t.Fatal(err)
			}
			f.resolve()
			if tc.want {
				assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeInheritedStaticMethodScope)
			} else {
				assertSwiftEdgeUnresolved(t, f, edge)
			}
		})
	}
	for _, tc := range []struct {
		name  string
		setup func(*swiftScopeFixture)
	}{
		{"cross-file method", func(f *swiftScopeFixture) {
			other := f.file("Extension.swift")
			target := f.symbolID("Base.make")
			f.store.db.ExecContext(f.ctx, `UPDATE symbols SET file_id=? WHERE id=?`, other, target)
			f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET file_id=? WHERE symbol_id=?`, other, target)
		}},
		{"cross-file base", func(f *swiftScopeFixture) {
			other := f.file("Base.swift")
			base := f.symbolID("Base")
			target := f.symbolID("Base.make")
			f.store.db.ExecContext(f.ctx, `UPDATE symbols SET file_id=? WHERE id IN (?,?)`, other, base, target)
			f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET file_id=? WHERE symbol_id=?`, other, target)
		}},
		{"cross-file relation", func(f *swiftScopeFixture) {
			other := f.file("Relation.swift")
			f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET file_id=?`, other)
		}},
		{"competing superclass", func(f *swiftScopeFixture) {
			f.relation(f.file("Other.swift"), "Child", "OtherBase", "superclass", false, false)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, target, edge := newSwiftInheritedStaticSelfFixture(t, false)
			tc.setup(f)
			f.resolve()
			assertSwiftInheritedStaticEdgeUnresolved(t, f, edge)
			_ = target
		})
	}
}

func TestSwiftClassSelfTypeInheritedStaticMethodScopeChildCandidatesAndCompetitors(t *testing.T) {
	for _, tc := range []struct {
		name string
		add  func(*swiftScopeFixture, string)
	}{
		{"static child", func(f *swiftScopeFixture, method string) {
			c := f.symbol(f.mainFile, method, "Child", "function", method+"()", true)
			f.dispatchFact(f.mainFile, c, false, "static")
		}},
		{"class child", func(f *swiftScopeFixture, method string) {
			c := f.symbol(f.mainFile, method, "Child", "function", method+"()", true)
			f.dispatchFact(f.mainFile, c, false, "class")
		}},
		{"malformed child", func(f *swiftScopeFixture, method string) {
			f.symbol(f.mainFile, method, "Child", "function", method+"()", true)
		}},
		{"private static child", func(f *swiftScopeFixture, method string) {
			c := f.symbol(f.mainFile, method, "Child", "function", method+"()", true)
			f.store.db.ExecContext(f.ctx, `UPDATE symbols SET visibility='private' WHERE id=?`, c)
			f.dispatchFact(f.mainFile, c, false, "static")
		}},
		{"instance base competitor", func(f *swiftScopeFixture, method string) {
			c := f.symbol(f.mainFile, method, "Base", "function", method+"()", false)
			f.dispatchFact(f.mainFile, c, false, "instance")
		}},
		{"class base competitor", func(f *swiftScopeFixture, method string) {
			c := f.symbol(f.mainFile, method, "Base", "function", method+"()", true)
			f.dispatchFact(f.mainFile, c, false, "class")
		}},
		{"malformed base competitor", func(f *swiftScopeFixture, method string) {
			c := f.symbol(f.mainFile, method, "Base", "function", method+"()", true)
			f.dispatchFact(f.mainFile, c, false, "")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, target, edge := newSwiftInheritedStaticSelfFixture(t, false)
			var method string
			if err := f.store.db.QueryRowContext(f.ctx, `SELECT name FROM symbols WHERE id=?`, target).Scan(&method); err != nil {
				t.Fatal(err)
			}
			tc.add(f, method)
			f.resolve()
			assertSwiftInheritedStaticEdgeUnresolved(t, f, edge)
		})
	}
}

func TestSwiftClassSelfTypeInheritedStaticMethodScopeHardenedStress(t *testing.T) {
	type stressCase struct{ name, strategy string }
	cases := []stressCase{
		{"instance", ResolutionStrategySwiftClassSelfTypeInheritedStaticMethodScope},
		{"static_caller", ResolutionStrategySwiftClassSelfTypeInheritedStaticMethodScope},
		{"class_caller", ResolutionStrategySwiftClassSelfTypeInheritedStaticMethodScope},
		{"final_child", ResolutionStrategySwiftClassSelfTypeInheritedStaticMethodScope},
		{"qualified", ResolutionStrategySwiftClassSelfTypeInheritedStaticMethodScope},
		{"reverse", ResolutionStrategySwiftClassSelfTypeInheritedStaticMethodScope},
		{"instance_blocker", ResolutionStrategySwiftClassSelfTypeInheritedStaticMethodScope},
		{"same_static", ResolutionStrategySwiftClassSelfTypeStaticMethodScope},
		{"same_final_class", ResolutionStrategySwiftClassSelfTypeFinalClassMethodScope},
		{"inherited_final_instance", ResolutionStrategySwiftClassSelfInheritedFinalMethodScope},
		{"ordinary_class", ""}, {"final_class", ""}, {"grandparent", ""}, {"missing_relation", ""},
		{"duplicate_relation", ""}, {"cross_competing", ""}, {"unproven", ""}, {"conformance", ""},
		{"generic", ""}, {"constrained", ""}, {"missing_base", ""}, {"duplicate_base", ""},
		{"cross_base", ""}, {"cross_relation", ""}, {"cross_method", ""}, {"private_base", ""},
		{"missing_fact", ""}, {"duplicate_fact", ""}, {"malformed", ""}, {"inherited_instance", ""},
		{"child_static", ""}, {"child_class", ""}, {"child_malformed", ""}, {"base_instance", ""},
		{"base_class", ""}, {"base_malformed", ""}, {"static_blocker", ""}, {"enum_blocker", ""},
		{"trailing_ambiguity", ""}, {"trailing_unsafe", ""},
	}
	const perCase = 100
	f := newSwiftScopeFixture(t)
	for _, tc := range cases {
		childName, baseName := "Child_"+tc.name, "Base_"+tc.name
		childOwner, baseOwner := childName, baseName
		if tc.name == "qualified" {
			childOwner, baseOwner = "Outer."+childName, "Outer."+baseName
		}
		childFile, baseFile, targetFile, relationFile := f.mainFile, f.mainFile, f.mainFile, f.mainFile
		if tc.name == "cross_base" {
			baseFile = f.file("Base_" + tc.name + ".swift")
			targetFile = baseFile
		}
		if tc.name == "cross_method" {
			targetFile = f.file("Extension_" + tc.name + ".swift")
		}
		if tc.name == "cross_relation" || tc.name == "cross_competing" {
			relationFile = f.file("Relation_" + tc.name + ".swift")
		}
		child := f.symbol(childFile, childName, func() string {
			if tc.name == "qualified" {
				return "Outer"
			}
			return ""
		}(), "class", "", false)
		f.declarationFact(childFile, child, tc.name == "final_child")
		if tc.name != "missing_base" {
			base := f.symbol(baseFile, baseName, func() string {
				if tc.name == "qualified" {
					return "Outer"
				}
				return ""
			}(), "class", "", false)
			f.declarationFact(baseFile, base, false)
			if tc.name == "duplicate_base" {
				f.symbol(f.mainFile, baseName, "", "class", "", false)
			}
		}
		if tc.name == "missing_relation" {
			// no relation
		} else if tc.name == "grandparent" {
			middle := "Middle_" + tc.name
			middleID := f.symbol(f.mainFile, middle, "", "class", "", false)
			f.declarationFact(f.mainFile, middleID, false)
			f.relation(f.mainFile, childOwner, middle, "superclass", false, false)
			f.relation(f.mainFile, middle, baseOwner, "superclass", false, false)
		} else {
			target := baseOwner
			if tc.name == "missing_base" {
				target = "Missing_" + tc.name
			}
			if tc.name != "cross_relation" && tc.name != "same_static" && tc.name != "same_final_class" {
				f.relation(f.mainFile, childOwner, target, "superclass", false, false)
			}
			if tc.name == "cross_relation" {
				f.relation(relationFile, childOwner, target, "superclass", false, false)
			}
			if tc.name == "duplicate_relation" {
				f.relation(f.mainFile, childOwner, target, "superclass", false, false)
			}
			if tc.name == "cross_competing" {
				f.relation(relationFile, childOwner, "OtherBase_"+tc.name, "superclass", false, false)
			}
			if tc.name == "unproven" {
				f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET relation_kind='unproven' WHERE child_qualified_name=?`, childOwner)
			}
			if tc.name == "conformance" {
				f.relation(relationFile, childOwner, "P_"+tc.name, "conformance", false, false)
			}
			if tc.name == "generic" {
				f.relation(relationFile, childOwner, "P_"+tc.name, "conformance", true, false)
			}
			if tc.name == "constrained" {
				f.relation(relationFile, childOwner, "P_"+tc.name, "conformance", false, true)
			}
		}
		if tc.name == "reverse" {
			f.relation(f.mainFile, "GrandChild_"+tc.name, childOwner, "superclass", false, false)
		}
		for i := 0; i < perCase; i++ {
			method := fmt.Sprintf("make_%s_%d", tc.name, i)
			owner := baseOwner
			targetStatic := true
			edgeName, evidence, arity := "Self."+method, "swift:Self", 0
			if tc.name == "same_static" || tc.name == "same_final_class" {
				owner = childOwner
			}
			if tc.name == "inherited_final_instance" {
				targetStatic, edgeName, evidence = false, "self."+method, "swift:self"
			}
			if strings.HasPrefix(tc.name, "trailing_") {
				method, edgeName, evidence, arity = "perform", "Self.perform", "swift:Self;trailing_labels=_", 1
			}
			target := f.symbol(targetFile, method, owner, "function", method+"()", targetStatic)
			if strings.HasPrefix(tc.name, "trailing_") {
				f.arity(target, 1, 1)
				if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET signature='perform(_:)' WHERE id=?`, target); err != nil {
					t.Fatal(err)
				}
			}
			switch tc.name {
			case "same_static":
				f.dispatchFact(targetFile, target, false, "static")
			case "same_final_class":
				f.dispatchFact(targetFile, target, true, "class")
			case "inherited_final_instance":
				f.dispatchFact(targetFile, target, true, "instance")
			case "missing_fact":
			case "malformed":
				f.dispatchFact(targetFile, target, false, "")
			case "ordinary_class":
				f.dispatchFact(targetFile, target, false, "class")
			case "final_class":
				f.dispatchFact(targetFile, target, true, "class")
			case "inherited_instance":
				f.store.db.ExecContext(f.ctx, `UPDATE symbols SET is_static=0 WHERE id=?`, target)
				f.dispatchFact(targetFile, target, false, "instance")
			default:
				f.dispatchFact(targetFile, target, false, "static")
			}
			if tc.name == "duplicate_fact" {
				f.dispatchFact(targetFile, target, false, "static")
			}
			if tc.name == "private_base" {
				f.store.db.ExecContext(f.ctx, `UPDATE symbols SET visibility='private' WHERE id=?`, target)
			}
			if tc.name == "child_static" || tc.name == "child_class" || tc.name == "child_malformed" {
				candidate := f.symbol(f.mainFile, method, childOwner, "function", method+"()", tc.name != "child_malformed")
				if tc.name == "child_class" {
					f.dispatchFact(f.mainFile, candidate, false, "class")
				} else if tc.name == "child_static" {
					f.dispatchFact(f.mainFile, candidate, false, "static")
				}
			}
			if tc.name == "base_instance" || tc.name == "base_class" || tc.name == "base_malformed" {
				candidate := f.symbol(f.mainFile, method, baseOwner, "function", method+"()", tc.name == "base_class" || tc.name == "base_malformed")
				if tc.name == "base_instance" {
					f.dispatchFact(f.mainFile, candidate, false, "instance")
				} else if tc.name == "base_class" {
					f.dispatchFact(f.mainFile, candidate, false, "class")
				} else {
					f.dispatchFact(f.mainFile, candidate, false, "")
				}
			}
			if tc.name == "trailing_ambiguity" {
				candidate := f.symbol(f.mainFile, method, baseOwner, "function", "perform(_:)", true)
				f.arity(candidate, 1, 1)
				f.dispatchFact(f.mainFile, candidate, false, "static")
			}
			if tc.name == "trailing_unsafe" {
				candidate := f.symbol(f.mainFile, method, baseOwner, "function", "perform(_:)", true)
				f.arity(candidate, 1, 1)
				f.dispatchFact(f.mainFile, candidate, false, "class")
			}
			callerStatic := tc.name == "static_caller" || tc.name == "class_caller"
			caller := f.symbol(f.mainFile, fmt.Sprintf("f_%s_%d", tc.name, i), childOwner, "function", "f()", callerStatic)
			if tc.name == "static_caller" {
				f.dispatchFact(f.mainFile, caller, false, "static")
			}
			if tc.name == "class_caller" {
				f.dispatchFact(f.mainFile, caller, false, "class")
			}
			f.call(f.mainFile, caller, edgeName, evidence, arity, i+1)
			if tc.name == "instance_blocker" {
				f.blocker(f.mainFile, childOwner, method, graph.ScopeImportSwiftMemberValue, false)
			}
			if tc.name == "static_blocker" {
				f.blocker(f.mainFile, childOwner, method, graph.ScopeImportSwiftMemberValue, true)
			}
			if tc.name == "enum_blocker" {
				f.blocker(f.mainFile, childOwner, method, graph.ScopeImportSwiftEnumCase, true)
			}
		}
	}
	type stressState struct {
		total, resolved, unresolved, badMetadata int
		strategies                               map[string]int
	}
	readState := func() stressState {
		var state stressState
		if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*),COALESCE(SUM(dst_symbol_id IS NOT NULL),0),COALESCE(SUM(dst_symbol_id IS NULL),0),COALESCE(SUM(dst_symbol_id IS NULL AND (resolution_strategy<>'' OR resolution_confidence<>'')),0) FROM edges WHERE repo_id=?`, f.repoID).Scan(&state.total, &state.resolved, &state.unresolved, &state.badMetadata); err != nil {
			t.Fatal(err)
		}
		state.strategies = map[string]int{}
		rows, err := f.store.db.QueryContext(f.ctx, `SELECT resolution_strategy,COUNT(*) FROM edges WHERE repo_id=? GROUP BY resolution_strategy`, f.repoID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var strategy string
			var count int
			if err := rows.Scan(&strategy, &count); err != nil {
				t.Fatal(err)
			}
			state.strategies[strategy] = count
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return state
	}
	f.resolve()
	first := readState()
	want := stressState{total: 4000, resolved: 1100, unresolved: 2900, badMetadata: 0, strategies: map[string]int{
		ResolutionStrategySwiftClassSelfTypeInheritedStaticMethodScope:     700,
		ResolutionStrategySwiftClassSelfTypeInheritedFinalClassMethodScope: 100,
		ResolutionStrategySwiftClassSelfTypeStaticMethodScope:              100,
		ResolutionStrategySwiftClassSelfTypeFinalClassMethodScope:          100,
		ResolutionStrategySwiftClassSelfInheritedFinalMethodScope:          100,
		"": 2900,
	}}
	if !reflect.DeepEqual(first, want) {
		t.Fatalf("first state=%+v, want %+v", first, want)
	}
	f.resolve()
	second := readState()
	if !reflect.DeepEqual(second, first) {
		t.Fatalf("second state=%+v, first=%+v", second, first)
	}
}

func assertSwiftStats(t *testing.T, stats ResolveEdgesForNamesStats, selected, resolved, unresolved, unknown int) {
	t.Helper()
	if stats.TargetsSelected != selected || stats.TargetsResolved != resolved || stats.TargetsUnresolved != unresolved || stats.UnknownSrcLanguage != unknown {
		t.Fatalf("stats=(selected %d,resolved %d,unresolved %d,unknown %d), want (%d,%d,%d,%d)", stats.TargetsSelected, stats.TargetsResolved, stats.TargetsUnresolved, stats.UnknownSrcLanguage, selected, resolved, unresolved, unknown)
	}
}

func TestSwiftSelfResolverStatsPositive(t *testing.T) {
	f := newSwiftScopeFixture(t)
	f.symbol(f.mainFile, "Service", "", "struct", "", false)
	target := f.symbol(f.mainFile, "run", "Service", "function", "run()", false)
	caller := f.symbol(f.mainFile, "caller", "Service", "function", "caller()", false)
	edge := f.call(f.mainFile, caller, "self.run", "swift:self", 0, 1)
	stats := f.resolveNames("run")
	assertSwiftStats(t, stats, 1, 1, 0, 0)
	if got := f.dst(edge); !got.Valid || got.Int64 != target {
		t.Fatalf("edge dst=%v, want %d", got, target)
	}
}

func TestSwiftSelfResolverStatsBlocked(t *testing.T) {
	f := newSwiftScopeFixture(t)
	f.symbol(f.mainFile, "Service", "", "struct", "", false)
	caller := f.symbol(f.mainFile, "caller", "Service", "function", "caller()", false)
	edge := f.call(f.mainFile, caller, "self.run", "swift:self", 0, 1)
	f.blocker(f.mainFile, "Service", "run", graph.ScopeImportSwiftMemberValue, false)
	stats := f.resolveNames("run")
	assertSwiftStats(t, stats, 1, 0, 1, 0)
	if got := f.dst(edge); got.Valid {
		t.Fatalf("blocked edge resolved to %d", got.Int64)
	}
}

func TestSwiftSelfResolverStatsMixed(t *testing.T) {
	f := newSwiftScopeFixture(t)
	f.symbol(f.mainFile, "Service", "", "struct", "", false)
	target := f.symbol(f.mainFile, "run", "Service", "function", "run()", false)
	caller := f.symbol(f.mainFile, "caller", "Service", "function", "caller()", false)
	bound := f.call(f.mainFile, caller, "self.run", "swift:self", 0, 1)
	unbound := f.call(f.mainFile, caller, "self.missing", "swift:self", 0, 2)
	stats := f.resolveNames("run", "missing")
	assertSwiftStats(t, stats, 2, 1, 1, 0)
	if got := f.dst(bound); !got.Valid || got.Int64 != target {
		t.Fatalf("bound edge dst=%v, want %d", got, target)
	}
	if got := f.dst(unbound); got.Valid {
		t.Fatalf("unbound edge resolved to %d", got.Int64)
	}
}

func TestSwiftSelfScopeResolveEdgesConsumesPersistedEdge(t *testing.T) {
	f := newSwiftScopeFixture(t)
	f.symbol(f.mainFile, "Service", "", "struct", "", false)
	run := f.symbol(f.mainFile, "run", "Service", "function", "run()", false)
	caller := f.symbol(f.mainFile, "caller", "Service", "function", "caller()", false)
	edgeID := f.call(f.mainFile, caller, "self.run", "swift:self", 0, 1)
	f.reference(f.mainFile, caller, "self.run", 1)
	f.resolve()
	var dst, ref int64
	var strategy, confidence string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT dst_symbol_id,resolution_strategy,resolution_confidence,
		(SELECT symbol_id FROM references_tbl WHERE repo_id=? AND name='self.run') FROM edges WHERE id=?`, f.repoID, edgeID).
		Scan(&dst, &strategy, &confidence, &ref); err != nil {
		t.Fatal(err)
	}
	if dst != run || strategy != ResolutionStrategySwiftSelfScope || confidence != ResolutionConfidenceHigh || ref != run {
		t.Fatalf("edge=(%d,%q,%q), reference=%d; want dst=%d and swift_self_scope/high", dst, strategy, confidence, ref, run)
	}
}

func TestSwiftSelfScopeStaticSelfSelfTypeAndLabels(t *testing.T) {
	f := newSwiftScopeFixture(t)
	f.symbol(f.mainFile, "Service", "", "struct", "", false)
	instanceRun := f.symbol(f.mainFile, "run", "Service", "function", "run()", false)
	underscoreRun := f.symbol(f.mainFile, "run", "Service", "function", "run(_:)", false)
	idRun := f.symbol(f.mainFile, "run", "Service", "function", "run(id:)", false)
	staticRun := f.symbol(f.mainFile, "make", "Service", "function", "make()", true)
	instanceCaller := f.symbol(f.mainFile, "caller", "Service", "function", "caller()", false)
	staticCaller := f.symbol(f.mainFile, "staticCaller", "Service", "function", "staticCaller()", true)
	checks := []struct {
		edge, want int64
		arity      int
		strategy   string
		ref        string
		line       int
	}{
		{f.call(f.mainFile, instanceCaller, "self.run", "swift:self", 0, 1), instanceRun, 0, ResolutionStrategySwiftSelfScope, "self.run", 1},
		{f.call(f.mainFile, instanceCaller, "self.run", "swift:self;labels=_", 1, 2), underscoreRun, 1, ResolutionStrategySwiftSelfScope, "self.run", 2},
		{f.call(f.mainFile, instanceCaller, "self.run", "swift:self;labels=id:", 1, 3), idRun, 1, ResolutionStrategySwiftSelfScope, "self.run", 3},
		{f.call(f.mainFile, staticCaller, "self.make", "swift:self", 0, 4), staticRun, 0, ResolutionStrategySwiftSelfScope, "self.make", 4},
		{f.call(f.mainFile, instanceCaller, "Self.make", "swift:Self", 0, 5), staticRun, 0, ResolutionStrategySwiftSelfTypeScope, "Self.make", 5},
	}
	for _, check := range checks {
		f.reference(f.mainFile, instanceCaller, check.ref, check.line)
	}
	f.resolve()
	for _, check := range checks {
		got := f.dst(check.edge)
		if !got.Valid || got.Int64 != check.want {
			t.Fatalf("edge %d dst=%v, want %d", check.edge, got, check.want)
		}
		var strategy, confidence string
		if err := f.store.db.QueryRowContext(f.ctx, `SELECT resolution_strategy,resolution_confidence FROM edges WHERE id=?`, check.edge).Scan(&strategy, &confidence); err != nil {
			t.Fatal(err)
		}
		if strategy != check.strategy || confidence != ResolutionConfidenceHigh {
			t.Fatalf("edge %d metadata=(%q,%q)", check.edge, strategy, confidence)
		}
		var ref sql.NullInt64
		if err := f.store.db.QueryRowContext(f.ctx, `SELECT symbol_id FROM references_tbl WHERE repo_id=? AND start_line=?`, f.repoID, check.line).Scan(&ref); err != nil {
			t.Fatal(err)
		}
		if !ref.Valid || ref.Int64 != check.want {
			t.Fatalf("reference line %d=%v, want %d", check.line, ref, check.want)
		}
	}
	var refs int
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM references_tbl WHERE repo_id=? AND symbol_id IS NOT NULL`, f.repoID).Scan(&refs); err != nil {
		t.Fatal(err)
	}
	if refs != len(checks) {
		t.Fatalf("bound references=%d, want %d", refs, len(checks))
	}
}

func TestSwiftSelfScopeOwnerMustBeOneAllowedNominal(t *testing.T) {
	for _, tc := range []struct {
		name  string
		kinds []string
		bound bool
	}{
		{"struct", []string{"struct"}, true},
		{"enum", []string{"enum"}, true},
		{"actor", []string{"actor"}, true},
		{"class", []string{"class"}, false},
		{"protocol", []string{"protocol"}, false},
		{"class+struct", []string{"class", "struct"}, false},
		{"duplicate-struct", []string{"struct", "struct"}, false},
		{"struct+enum", []string{"struct", "enum"}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwiftScopeFixture(t)
			for _, kind := range tc.kinds {
				f.symbol(f.mainFile, "Service", "", kind, "", false)
			}
			target := f.symbol(f.mainFile, "run", "Service", "function", "run()", false)
			caller := f.symbol(f.mainFile, "caller", "Service", "function", "caller()", false)
			edge := f.call(f.mainFile, caller, "self.run", "swift:self", 0, 1)
			f.resolve()
			got := f.dst(edge)
			if tc.bound != got.Valid || (tc.bound && got.Int64 != target) {
				t.Fatalf("dst=%v, want bound=%v target=%d", got, tc.bound, target)
			}
		})
	}
}

func TestSwiftClassSelfRequiresFinalFactAndNoHazard(t *testing.T) {
	for _, tc := range []struct {
		name      string
		final     bool
		relation  string
		wantBound bool
	}{
		{"final", true, "", true},
		{"non-final", false, "", false},
		{"superclass", true, "superclass", false},
		{"conformance", true, "conformance", false},
		{"unproven", true, "unproven", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwiftScopeFixture(t)
			owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
			target := f.symbol(f.mainFile, "run", "Service", "function", "run()", false)
			caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", false)
			edge := f.call(f.mainFile, caller, "self.run", "swift:self", 0, 1)
			f.declarationFact(f.mainFile, owner, tc.final)
			if tc.relation != "" {
				f.relation(f.mainFile, "Service", "Base", tc.relation, true, true)
			}
			f.resolve()
			got := f.dst(edge)
			if tc.wantBound != got.Valid || (tc.wantBound && got.Int64 != target) {
				t.Fatalf("dst=%v, want bound=%v target=%d", got, tc.wantBound, target)
			}
			if tc.wantBound {
				var strategy, confidence string
				if err := f.store.db.QueryRowContext(f.ctx, `SELECT resolution_strategy,resolution_confidence FROM edges WHERE id=?`, edge).Scan(&strategy, &confidence); err != nil {
					t.Fatal(err)
				}
				if strategy != ResolutionStrategySwiftClassSelfFinalScope || confidence != ResolutionConfidenceHigh {
					t.Fatalf("resolution=(%q,%q)", strategy, confidence)
				}
			}
		})
	}
}

func TestSwiftClassSelfFinalMethodScope(t *testing.T) {
	t.Run("non-final owner and final instance method", func(t *testing.T) {
		f := newSwiftScopeFixture(t)
		owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
		target := f.symbol(f.mainFile, "run", "Service", "function", "run()", false)
		caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", false)
		edge := f.call(f.mainFile, caller, "self.run", "swift:self", 0, 1)
		f.reference(f.mainFile, caller, "self.run", 1)
		f.declarationFact(f.mainFile, owner, false)
		f.declarationFact(f.mainFile, target, true)
		f.resolve()
		assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfFinalMethodScope)
	})

	for _, tc := range []struct {
		name, dispatch                    string
		finalMethod, staticCaller, hazard bool
	}{
		{"non-final method", "instance", false, false, false},
		{"missing method fact", "", true, false, false},
		{"static dispatch", "static", true, false, false},
		{"static caller", "instance", true, true, false},
		{"owner hazard", "instance", true, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwiftScopeFixture(t)
			owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
			target := f.symbol(f.mainFile, "run", "Service", "function", "run()", false)
			caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", tc.staticCaller)
			edge := f.call(f.mainFile, caller, "self.run", "swift:self", 0, 1)
			f.declarationFact(f.mainFile, owner, false)
			if tc.dispatch != "" {
				f.dispatchFact(f.mainFile, target, tc.finalMethod, tc.dispatch)
			}
			if tc.hazard {
				f.relation(f.mainFile, "Service", "Base", "superclass", false, false)
			}
			f.resolve()
			if got := f.dst(edge); got.Valid {
				t.Fatalf("unsafe binding=%d", got.Int64)
			}
		})
	}
}

func newSwiftInheritedFinalSelfFixture(t *testing.T, child, base string) (*swiftScopeFixture, int64, int64, int64, int64) {
	t.Helper()
	f := newSwiftScopeFixture(t)
	baseID := f.symbol(f.mainFile, base, "", "class", "", false)
	childID := f.symbol(f.mainFile, child, "", "class", "", false)
	target := f.symbol(f.mainFile, "run", base, "function", "run()", false)
	caller := f.symbol(f.mainFile, "f", child, "function", "f()", false)
	edge := f.call(f.mainFile, caller, "self.run", "swift:self", 0, 1)
	f.reference(f.mainFile, caller, "self.run", 1)
	f.declarationFact(f.mainFile, childID, false)
	f.dispatchFact(f.mainFile, target, true, "instance")
	f.relation(f.mainFile, child, base, "superclass", false, false)
	return f, baseID, target, caller, edge
}

func TestSwiftClassSelfInheritedFinalMethodScope(t *testing.T) {
	f, _, target, _, edge := newSwiftInheritedFinalSelfFixture(t, "Child", "Base")
	f.resolve()
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfInheritedFinalMethodScope)
}

func TestSwiftClassSelfInheritedFinalMethodScopeCrossFileHazardLifecycle(t *testing.T) {
	f, _, target, _, edge := newSwiftInheritedFinalSelfFixture(t, "Child", "Base")
	f.resolve()
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfInheritedFinalMethodScope)
	hazard := f.file("Conformance.swift")
	f.relation(hazard, "Child", "P", "conformance", false, false)
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Conformance.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftP22FinalClassEdgeUnresolved(t, f, edge)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE files SET is_deleted=1 WHERE id=?`, hazard); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Conformance.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfInheritedFinalMethodScope)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE files SET is_deleted=0 WHERE id=?`, hazard); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Conformance.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftP22FinalClassEdgeUnresolved(t, f, edge)
}

func TestSwiftClassSelfInheritedFinalMethodScopeCrossFileRelationHazards(t *testing.T) {
	t.Run("cross-file superclass alone is not positive proof", func(t *testing.T) {
		f, _, _, _, edge := newSwiftInheritedFinalSelfFixture(t, "Child", "Base")
		if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM swift_inheritance_relations WHERE child_qualified_name='Child'`); err != nil {
			t.Fatal(err)
		}
		hazard := f.file("CrossBase.swift")
		f.relation(hazard, "Child", "Base", "superclass", false, false)
		f.resolve()
		assertSwiftEdgeUnresolved(t, f, edge)
	})
	for _, tc := range []struct {
		name                 string
		kind                 string
		generic, constrained bool
		secondSuperclass     bool
	}{
		{"unproven", "unproven", false, false, false},
		{"generic conformance", "conformance", true, false, false},
		{"constrained conformance", "conformance", false, true, false},
		{"competing superclass", "superclass", false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, _, _, edge := newSwiftInheritedFinalSelfFixture(t, "Child", "Base")
			f.resolve()
			hazard := f.file("Hazard_" + tc.name + ".swift")
			if tc.secondSuperclass {
				f.relation(hazard, "Child", "OtherBase", tc.kind, tc.generic, tc.constrained)
			} else {
				f.relation(hazard, "Child", "P", tc.kind, tc.generic, tc.constrained)
			}
			if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Hazard_" + tc.name + ".swift"}); err != nil {
				t.Fatal(err)
			}
			assertSwiftEdgeUnresolved(t, f, edge)
		})
	}
}

func TestSwiftClassSelfInheritedFinalMethodScopeCrossFileRelationTestFiltering(t *testing.T) {
	for _, tc := range []struct {
		name, callerPath, relationPath      string
		callerTest, relationTest, wantBound bool
	}{
		{"production caller production relation", "Service.swift", "Conformance.swift", false, false, false},
		{"production caller test relation", "Service.swift", "Tests/Conformance.swift", false, true, true},
		{"test caller production relation", "Tests/Service.swift", "Conformance.swift", true, false, false},
		{"test caller test relation", "Tests/Service.swift", "Tests/Conformance.swift", true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwiftScopeFixture(t)
			callerFile := f.mainFile
			if tc.callerTest {
				callerFile = f.file(tc.callerPath)
			}
			f.symbol(callerFile, "Base", "", "class", "", false)
			child := f.symbol(callerFile, "Child", "", "class", "", false)
			target := f.symbol(callerFile, "run", "Base", "function", "run()", false)
			caller := f.symbol(callerFile, "f", "Child", "function", "f()", false)
			edge := f.call(callerFile, caller, "self.run", "swift:self", 0, 1)
			f.reference(callerFile, caller, "self.run", 1)
			f.declarationFact(callerFile, child, false)
			f.dispatchFact(callerFile, target, true, "instance")
			f.relation(callerFile, "Child", "Base", "superclass", false, false)
			hazard := f.file(tc.relationPath)
			f.relation(hazard, "Child", "P", "conformance", false, false)
			f.resolve()
			if tc.wantBound {
				assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfInheritedFinalMethodScope)
			} else {
				assertSwiftEdgeUnresolved(t, f, edge)
			}
		})
	}
}

func TestSwiftClassSelfInheritedFinalMethodScopeCrossFileTargetBoundaries(t *testing.T) {
	t.Run("cross-file method", func(t *testing.T) {
		f, _, target, _, edge := newSwiftInheritedFinalSelfFixture(t, "Child", "Base")
		other := f.file("Extension.swift")
		if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET file_id=? WHERE id=?`, other, target); err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET file_id=? WHERE symbol_id=?`, other, target); err != nil {
			t.Fatal(err)
		}
		f.resolve()
		assertSwiftEdgeUnresolved(t, f, edge)
	})
	t.Run("cross-file base", func(t *testing.T) {
		f := newSwiftScopeFixture(t)
		baseFile := f.file("Base.swift")
		f.symbol(baseFile, "Base", "", "class", "", false)
		child := f.symbol(f.mainFile, "Child", "", "class", "", false)
		caller := f.symbol(f.mainFile, "f", "Child", "function", "f()", false)
		target := f.symbol(baseFile, "run", "Base", "function", "run()", false)
		f.dispatchFact(baseFile, target, true, "instance")
		edge := f.call(f.mainFile, caller, "self.run", "swift:self", 0, 1)
		f.declarationFact(f.mainFile, child, false)
		f.relation(f.mainFile, "Child", "Base", "superclass", false, false)
		f.resolve()
		assertSwiftEdgeUnresolved(t, f, edge)
	})
	t.Run("private", func(t *testing.T) {
		f, _, target, _, edge := newSwiftInheritedFinalSelfFixture(t, "Child", "Base")
		if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET visibility='private' WHERE id=?`, target); err != nil {
			t.Fatal(err)
		}
		f.resolve()
		assertSwiftEdgeUnresolved(t, f, edge)
	})
	t.Run("inherited static and class", func(t *testing.T) {
		for _, dispatch := range []string{"static", "class"} {
			f := newSwiftScopeFixture(t)
			base := f.symbol(f.mainFile, "Base", "", "class", "", false)
			child := f.symbol(f.mainFile, "Child", "", "class", "", false)
			target := f.symbol(f.mainFile, "run", "Base", "function", "run()", true)
			caller := f.symbol(f.mainFile, "f", "Child", "function", "f()", false)
			edge := f.call(f.mainFile, caller, "self.run", "swift:self", 0, 1)
			f.declarationFact(f.mainFile, child, false)
			f.dispatchFact(f.mainFile, target, dispatch == "class", dispatch)
			f.relation(f.mainFile, "Child", "Base", "superclass", false, false)
			_ = base
			f.resolve()
			assertSwiftEdgeUnresolved(t, f, edge)
		}
	})
}

func TestSwiftClassSelfInheritedFinalMethodScopeVisibilityAndRedeclaration(t *testing.T) {
	for _, visibility := range []string{"", "internal", "fileprivate"} {
		t.Run("base "+visibility, func(t *testing.T) {
			f, _, target, _, edge := newSwiftInheritedFinalSelfFixture(t, "Child", "Base")
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET visibility=? WHERE id=?`, visibility, target); err != nil {
				t.Fatal(err)
			}
			f.resolve()
			assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfInheritedFinalMethodScope)
		})
	}
	for _, visibility := range []string{"", "internal", "fileprivate", "private"} {
		t.Run("child "+visibility, func(t *testing.T) {
			f, _, _, _, edge := newSwiftInheritedFinalSelfFixture(t, "Child", "Base")
			shadow := f.symbol(f.mainFile, "run", "Child", "function", "run()", false)
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET visibility=? WHERE id=?`, visibility, shadow); err != nil {
				t.Fatal(err)
			}
			f.dispatchFact(f.mainFile, shadow, false, "instance")
			f.resolve()
			assertSwiftP22FinalClassEdgeUnresolved(t, f, edge)
		})
	}
}

func TestSwiftClassSelfInheritedFinalMethodScopeUnsafeBaseCompetitors(t *testing.T) {
	for _, tc := range []struct {
		name, visibility, dispatch string
		facts, final               int
	}{
		{"ordinary", "", "instance", 1, 0},
		{"malformed", "", "", 1, 1},
		{"missing fact", "", "instance", 0, 0},
		{"duplicate facts", "", "instance", 2, 1},
		{"private final", "private", "instance", 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, target, _, edge := newSwiftInheritedFinalSelfFixture(t, "Child", "Base")
			competitor := f.symbol(f.mainFile, "run", "Base", "function", "run()", false)
			if tc.visibility != "" {
				if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET visibility=? WHERE id=?`, tc.visibility, competitor); err != nil {
					t.Fatal(err)
				}
			}
			for i := 0; i < tc.facts; i++ {
				f.dispatchFact(f.mainFile, competitor, tc.final == 1, tc.dispatch)
			}
			_ = target
			f.resolve()
			assertSwiftP22FinalClassEdgeUnresolved(t, f, edge)
		})
	}
}

func TestSwiftClassSelfInheritedFinalMethodScopeUnsafeTrailingCompetitor(t *testing.T) {
	f, _, target, _, edge := newSwiftInheritedFinalSelfFixture(t, "Child", "Base")
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET name='perform',signature='perform(_:)',arity_min=1,arity_max=1 WHERE id=?`, target); err != nil {
		t.Fatal(err)
	}
	competitor := f.symbol(f.mainFile, "perform", "Base", "function", "perform(_:)", false)
	f.arity(competitor, 1, 1)
	f.dispatchFact(f.mainFile, competitor, false, "instance")
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET dst_name='self.perform',evidence='swift:self;trailing_labels=_',call_arity=1 WHERE id=?`, edge); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE references_tbl SET name='self.perform',qualified_name='self.perform' WHERE repo_id=?`, f.repoID); err != nil {
		t.Fatal(err)
	}
	f.resolve()
	assertSwiftP22FinalClassEdgeUnresolved(t, f, edge)
}

func TestSwiftClassSelfInheritedFinalMethodScopeBlockerFiltering(t *testing.T) {
	for _, tc := range []struct {
		name, callerPath, blockerPath              string
		callerTest, blockerTest, static, wantBound bool
	}{
		{"production production instance", "Service.swift", "Blocker.swift", false, false, false, false},
		{"production test instance", "Service.swift", "Tests/Blocker.swift", false, true, false, true},
		{"test production instance", "Tests/Service.swift", "Blocker.swift", true, false, false, false},
		{"test test instance", "Tests/Service.swift", "Tests/Blocker.swift", true, true, false, false},
		{"production static", "Service.swift", "Blocker.swift", false, false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwiftScopeFixture(t)
			callerFile := f.mainFile
			if tc.callerTest {
				callerFile = f.file(tc.callerPath)
			}
			base := f.symbol(callerFile, "Base", "", "class", "", false)
			child := f.symbol(callerFile, "Child", "", "class", "", false)
			target := f.symbol(callerFile, "run", "Base", "function", "run()", false)
			caller := f.symbol(callerFile, "f", "Child", "function", "f()", false)
			edge := f.call(callerFile, caller, "self.run", "swift:self", 0, 1)
			f.reference(callerFile, caller, "self.run", 1)
			f.declarationFact(callerFile, child, false)
			f.dispatchFact(callerFile, target, true, "instance")
			f.relation(callerFile, "Child", "Base", "superclass", false, false)
			blockerFile := f.file(tc.blockerPath)
			f.blocker(blockerFile, "Child", "run", graph.ScopeImportSwiftMemberValue, tc.static)
			_ = base
			f.resolve()
			if tc.wantBound {
				assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfInheritedFinalMethodScope)
			} else {
				assertSwiftP22FinalClassEdgeUnresolved(t, f, edge)
			}
		})
	}
}

func TestSwiftClassSelfInheritedFinalMethodScopeNamesStats(t *testing.T) {
	f, _, target, _, edge := newSwiftInheritedFinalSelfFixture(t, "Child", "Base")
	assertSwiftStats(t, f.resolveNames("run"), 1, 1, 0, 0)
	assertSwiftEdgeMetadata(t, f, edge, target, ResolutionStrategySwiftClassSelfInheritedFinalMethodScope)

	f, _, _, _, edge = newSwiftInheritedFinalSelfFixture(t, "Child", "Base")
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET is_final=0 WHERE symbol_id=(SELECT id FROM symbols WHERE name='run')`); err != nil {
		t.Fatal(err)
	}
	assertSwiftStats(t, f.resolveNames("run"), 1, 0, 1, 0)
	assertSwiftEdgeUnresolved(t, f, edge)
}

func TestSwiftClassSelfInheritedFinalMethodScopeRejectsUnsafeProofs(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*swiftScopeFixture)
	}{
		{"ordinary method", func(f *swiftScopeFixture) {
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET is_final=0 WHERE symbol_id=(SELECT id FROM symbols WHERE name='run')`); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"grandparent only", func(f *swiftScopeFixture) {
			middle := f.symbol(f.mainFile, "Middle", "", "class", "", false)
			f.declarationFact(f.mainFile, middle, false)
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET target_qualified_name='Middle' WHERE child_qualified_name='Child'`); err != nil {
				f.t.Fatal(err)
			}
			f.relation(f.mainFile, "Middle", "Base", "superclass", false, false)
		}},
		{"conformance hazard", func(f *swiftScopeFixture) {
			f.relation(f.mainFile, "Child", "P", "conformance", false, false)
		}},
		{"same instance blocker", func(f *swiftScopeFixture) {
			f.blocker(f.mainFile, "Child", "run", graph.ScopeImportSwiftMemberValue, false)
		}},
		{"child redeclaration", func(f *swiftScopeFixture) {
			shadow := f.symbol(f.mainFile, "run", "Child", "function", "run()", false)
			f.dispatchFact(f.mainFile, shadow, false, "instance")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, target, _, edge := newSwiftInheritedFinalSelfFixture(t, "Child", "Base")
			tc.setup(f)
			f.resolve()
			assertSwiftEdgeUnresolved(t, f, edge)
			_ = target
		})
	}
}

func TestSwiftClassSelfInheritedFinalMethodScopeSelectorsAndControls(t *testing.T) {
	for _, tc := range []struct {
		name, signature, evidence string
		arity                     int
	}{
		{"zero", "run()", "swift:self", 0},
		{"underscore", "run(_:)", "swift:self;labels=_", 1},
		{"named", "run(id:)", "swift:self;labels=id:", 1},
		{"two labels", "run(id:,cache:)", "swift:self;labels=id:,cache:", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, target, caller, edge := newSwiftInheritedFinalSelfFixture(t, "Child", "Base")
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET signature=?,arity_min=?,arity_max=? WHERE id=?`, tc.signature, tc.arity, tc.arity, target); err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET evidence=?,call_arity=? WHERE id=?`, tc.evidence, tc.arity, edge); err != nil {
				t.Fatal(err)
			}
			_ = caller
			f.resolve()
			assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfInheritedFinalMethodScope)
		})
	}
	t.Run("trailing closure", func(t *testing.T) {
		f, _, target, _, edge := newSwiftInheritedFinalSelfFixture(t, "Child", "Base")
		if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET name='perform',signature='perform(_:)',arity_min=1,arity_max=1 WHERE id=?`, target); err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET dst_name='self.perform',evidence='swift:self;trailing_labels=_',call_arity=1 WHERE id=?`, edge); err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.db.ExecContext(f.ctx, `UPDATE references_tbl SET name='self.perform',qualified_name='self.perform' WHERE repo_id=?`, f.repoID); err != nil {
			t.Fatal(err)
		}
		f.resolve()
		assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfInheritedFinalMethodScope)
	})
	t.Run("trailing ambiguity", func(t *testing.T) {
		f, _, target, _, edge := newSwiftInheritedFinalSelfFixture(t, "Child", "Base")
		if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET name='perform',signature='perform(_:)',arity_min=1,arity_max=1 WHERE id=?`, target); err != nil {
			t.Fatal(err)
		}
		duplicate := f.symbol(f.mainFile, "perform", "Base", "function", "perform(_:)", false)
		f.arity(duplicate, 1, 1)
		f.dispatchFact(f.mainFile, duplicate, true, "instance")
		if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET dst_name='self.perform',evidence='swift:self;trailing_labels=_',call_arity=1 WHERE id=?`, edge); err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.db.ExecContext(f.ctx, `UPDATE references_tbl SET name='self.perform',qualified_name='self.perform' WHERE repo_id=?`, f.repoID); err != nil {
			t.Fatal(err)
		}
		f.resolve()
		assertSwiftEdgeUnresolved(t, f, edge)
	})
}

func TestSwiftClassSelfInheritedFinalMethodScopeLifecycleAndRepair(t *testing.T) {
	f, _, target, _, edge := newSwiftInheritedFinalSelfFixture(t, "Child", "Base")
	f.resolve()
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfInheritedFinalMethodScope)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET is_final=0 WHERE symbol_id=?`, target); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftEdgeUnresolved(t, f, edge)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET is_final=1 WHERE symbol_id=?`, target); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfInheritedFinalMethodScope)
	if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM swift_inheritance_relations WHERE child_qualified_name='Child'`); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftEdgeUnresolved(t, f, edge)
	f.relation(f.mainFile, "Child", "Base", "superclass", false, false)
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfInheritedFinalMethodScope)
	for _, repair := range resolverRepairs {
		if repair.key == swiftClassSelfInheritedFinalMethodRepairSettingKey {
			continue
		}
		if err := f.store.markRepairDone(f.ctx, repair.key, f.repoID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET dst_symbol_id=?,resolution_strategy=?,resolution_confidence=? WHERE id=?`, target, ResolutionStrategySwiftClassSelfInheritedFinalMethodScope, ResolutionConfidenceHigh, edge); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE references_tbl SET symbol_id=? WHERE repo_id=?`, target, f.repoID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET is_final=0 WHERE symbol_id=?`, target); err != nil {
		t.Fatal(err)
	}
	run, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID)
	if err != nil || !run {
		t.Fatalf("repair=(%v,%v)", run, err)
	}
	assertSwiftEdgeUnresolved(t, f, edge)
	var ref sql.NullInt64
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT symbol_id FROM references_tbl WHERE repo_id=?`, f.repoID).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	if ref.Valid {
		t.Fatalf("stale reference=%v", ref)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET is_final=1 WHERE symbol_id=?`, target); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM settings WHERE key=?`, swiftClassSelfInheritedFinalMethodRepairSettingKey+fmt.Sprintf(".%d", f.repoID)); err != nil {
		t.Fatal(err)
	}
	run, err = f.store.RepairResolverBindingsOnce(f.ctx, f.repoID)
	if err != nil || !run {
		t.Fatalf("rebind repair=(%v,%v)", run, err)
	}
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfInheritedFinalMethodScope)
	run, err = f.store.RepairResolverBindingsOnce(f.ctx, f.repoID)
	if err != nil || run {
		t.Fatalf("second repair=(%v,%v)", run, err)
	}
}

func TestSwiftClassSelfInheritedFinalMethodScopeRelationLifecycle(t *testing.T) {
	f, _, target, _, edge := newSwiftInheritedFinalSelfFixture(t, "Child", "Base")
	f.resolve()
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfInheritedFinalMethodScope)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET target_qualified_name='OtherBase' WHERE child_qualified_name='Child'`); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftEdgeUnresolved(t, f, edge)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET target_qualified_name='Base',relation_kind='unproven' WHERE child_qualified_name='Child'`); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftEdgeUnresolved(t, f, edge)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET relation_kind='superclass' WHERE child_qualified_name='Child'`); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfInheritedFinalMethodScope)
}

func TestSwiftClassSelfInheritedFinalMethodScopeUpgradeRepair(t *testing.T) {
	f, _, target, _, edge := newSwiftInheritedFinalSelfFixture(t, "Child", "Base")
	for _, repair := range resolverRepairs {
		if repair.key == swiftClassSelfInheritedFinalMethodRepairSettingKey {
			continue
		}
		if err := f.store.markRepairDone(f.ctx, repair.key, f.repoID); err != nil {
			t.Fatal(err)
		}
	}
	assertSwiftP22FinalClassEdgeUnresolved(t, f, edge)
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT value FROM settings WHERE key=?`, swiftClassSelfInheritedFinalMethodRepairSettingKey+fmt.Sprintf(".%d", f.repoID)).Scan(new(string)); err == nil {
		t.Fatal("new repair marker unexpectedly present")
	}
	run, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID)
	if err != nil || !run {
		t.Fatalf("repair=(%v,%v)", run, err)
	}
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfInheritedFinalMethodScope)
	var marker string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT value FROM settings WHERE key=?`, swiftClassSelfInheritedFinalMethodRepairSettingKey+fmt.Sprintf(".%d", f.repoID)).Scan(&marker); err != nil || marker != "1" {
		t.Fatalf("marker=%q err=%v", marker, err)
	}
	run, err = f.store.RepairResolverBindingsOnce(f.ctx, f.repoID)
	if err != nil || run {
		t.Fatalf("second repair=(%v,%v)", run, err)
	}
}

func TestSwiftClassSelfInheritedFinalMethodScopeUnsafeRepair(t *testing.T) {
	f, _, target, _, edge := newSwiftInheritedFinalSelfFixture(t, "Child", "Base")
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET dst_symbol_id=?,resolution_strategy=?,resolution_confidence=? WHERE id=?`, target, ResolutionStrategySwiftClassSelfInheritedFinalMethodScope, ResolutionConfidenceHigh, edge); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE references_tbl SET symbol_id=? WHERE repo_id=?`, target, f.repoID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET is_final=0 WHERE symbol_id=?`, target); err != nil {
		t.Fatal(err)
	}
	for _, repair := range resolverRepairs {
		if repair.key == swiftClassSelfInheritedFinalMethodRepairSettingKey {
			continue
		}
		if err := f.store.markRepairDone(f.ctx, repair.key, f.repoID); err != nil {
			t.Fatal(err)
		}
	}
	run, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID)
	if err != nil || !run {
		t.Fatalf("repair=(%v,%v)", run, err)
	}
	assertSwiftEdgeUnresolved(t, f, edge)
	var ref sql.NullInt64
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT symbol_id FROM references_tbl WHERE repo_id=?`, f.repoID).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	if ref.Valid {
		t.Fatalf("stale reference=%v", ref)
	}
	var marker string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT value FROM settings WHERE key=?`, swiftClassSelfInheritedFinalMethodRepairSettingKey+fmt.Sprintf(".%d", f.repoID)).Scan(&marker); err != nil || marker != "1" {
		t.Fatalf("marker=%q err=%v", marker, err)
	}
}

func TestSwiftClassSelfInheritedFinalMethodScopeMixedStress(t *testing.T) {
	type group struct {
		name, mode, strategy string
		count                int
	}
	groups := []group{
		{"direct", "direct", ResolutionStrategySwiftClassSelfInheritedFinalMethodScope, 100},
		{"same_final", "same_final", ResolutionStrategySwiftClassSelfFinalMethodScope, 100},
		{"same_static", "same_static", ResolutionStrategySwiftClassSelfStaticMethodScope, 100},
		{"ordinary", "ordinary", "", 100},
		{"grandparent", "grandparent", "", 100},
		{"missing_relation", "missing_relation", "", 100},
		{"duplicate_relation", "duplicate_relation", "", 100},
		{"unproven", "unproven", "", 100},
		{"conformance", "conformance", "", 100},
		{"constrained", "constrained", "", 100},
		{"generic", "generic", "", 100},
		{"missing_base", "missing_base", "", 100},
		{"duplicate_base", "duplicate_base", "", 100},
		{"missing_fact", "missing_fact", "", 100},
		{"duplicate_fact", "duplicate_fact", "", 100},
		{"malformed", "malformed", "", 100},
		{"cross_file", "cross_file", "", 100},
		{"cross_base", "cross_base", "", 100},
		{"cross_competing", "cross_competing", "", 100},
		{"private_base", "private_base", "", 100},
		{"private_child", "private_child", "", 100},
		{"unsafe_ordinary", "unsafe_ordinary", "", 100},
		{"unsafe_malformed", "unsafe_malformed", "", 100},
		{"qualified", "qualified", ResolutionStrategySwiftClassSelfInheritedFinalMethodScope, 100},
		{"reverse_subclass", "reverse_subclass", ResolutionStrategySwiftClassSelfInheritedFinalMethodScope, 100},
		{"same_blocker", "same_blocker", "", 100},
		{"opposite_static", "opposite_static", ResolutionStrategySwiftClassSelfInheritedFinalMethodScope, 100},
	}
	f := newSwiftScopeFixture(t)
	for _, g := range groups {
		child, base := "Child_"+g.name, "Base_"+g.name
		childOwner, baseOwner := child, base
		if g.mode == "qualified" {
			childOwner, baseOwner = "Outer."+child, "Outer."+base
		}
		childID := f.symbol(f.mainFile, child, "", "class", "", false)
		if g.mode == "qualified" {
			childID = f.symbol(f.mainFile, child, "Outer", "class", "", false)
		}
		f.declarationFact(f.mainFile, childID, false)
		baseDeclarationFile := f.mainFile
		if g.mode == "cross_base" {
			baseDeclarationFile = f.file("Base_" + g.name + ".swift")
		}
		if g.mode != "missing_relation" && g.mode != "cross_base" {
			f.symbol(baseDeclarationFile, base, "", "class", "", false)
			if g.mode == "qualified" {
				f.symbol(baseDeclarationFile, base, "Outer", "class", "", false)
			}
		} else if g.mode == "cross_base" {
			f.symbol(baseDeclarationFile, base, "", "class", "", false)
		}
		if g.mode == "duplicate_base" {
			f.symbol(f.mainFile, base, "", "class", "", false)
		}
		if g.mode == "grandparent" {
			middle := "Middle_" + g.name
			middleID := f.symbol(f.mainFile, middle, "", "class", "", false)
			f.declarationFact(f.mainFile, middleID, false)
			f.relation(f.mainFile, child, middle, "superclass", false, false)
			f.relation(f.mainFile, middle, base, "superclass", false, false)
		} else if g.mode == "direct" || g.mode == "ordinary" || g.mode == "duplicate_relation" || g.mode == "unproven" || g.mode == "conformance" || g.mode == "constrained" || g.mode == "generic" || g.mode == "missing_base" || g.mode == "duplicate_base" || g.mode == "cross_file" || g.mode == "cross_base" || g.mode == "cross_competing" || g.mode == "private_base" || g.mode == "private_child" || g.mode == "unsafe_ordinary" || g.mode == "unsafe_malformed" || g.mode == "qualified" || g.mode == "reverse_subclass" || g.mode == "same_blocker" || g.mode == "opposite_static" {
			relationTarget := baseOwner
			if g.mode == "missing_base" {
				relationTarget = "Missing_" + g.name
			}
			f.relation(f.mainFile, childOwner, relationTarget, "superclass", false, false)
			if g.mode == "duplicate_relation" {
				f.relation(f.mainFile, childOwner, relationTarget, "superclass", false, false)
			}
			if g.mode == "cross_competing" {
				hazard := f.file("Competing_" + g.name + ".swift")
				f.relation(hazard, childOwner, "OtherBase_"+g.name, "superclass", false, false)
			}
			if g.mode == "unproven" {
				f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET relation_kind='unproven' WHERE child_qualified_name=?`, childOwner)
			}
			if g.mode == "conformance" {
				f.relation(f.mainFile, childOwner, "P_"+g.name, "conformance", false, false)
			}
			if g.mode == "constrained" {
				f.relation(f.mainFile, childOwner, "P_"+g.name, "conformance", false, true)
			}
			if g.mode == "generic" {
				f.relation(f.mainFile, childOwner, "P_"+g.name, "conformance", true, false)
			}
			if g.mode == "reverse_subclass" {
				f.relation(f.mainFile, "GrandChild_"+g.name, childOwner, "superclass", false, false)
			}
		}
		for i := 0; i < g.count; i++ {
			method := fmt.Sprintf("run_%s_%d", g.name, i)
			owner := baseOwner
			staticCaller := false
			targetStatic := false
			if g.mode == "same_final" || g.mode == "same_static" {
				owner = childOwner
			}
			if g.mode == "same_static" {
				staticCaller, targetStatic = true, true
			}
			targetFile := f.mainFile
			if g.mode == "cross_file" {
				targetFile = f.file(fmt.Sprintf("Base_%s_%d.swift", g.name, i))
			}
			if g.mode == "cross_base" {
				targetFile = baseDeclarationFile
			}
			target := f.symbol(targetFile, method, owner, "function", method+"()", targetStatic)
			if g.mode == "private_base" {
				if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET visibility='private' WHERE id=?`, target); err != nil {
					t.Fatal(err)
				}
			}
			if g.mode == "private_child" {
				shadow := f.symbol(f.mainFile, method, childOwner, "function", method+"()", false)
				if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET visibility='private' WHERE id=?`, shadow); err != nil {
					t.Fatal(err)
				}
				f.dispatchFact(f.mainFile, shadow, false, "instance")
			}
			if g.mode == "unsafe_ordinary" || g.mode == "unsafe_malformed" {
				competitor := f.symbol(f.mainFile, method, baseOwner, "function", method+"()", false)
				dispatch := "instance"
				if g.mode == "unsafe_malformed" {
					dispatch = ""
				}
				f.dispatchFact(f.mainFile, competitor, g.mode == "unsafe_malformed", dispatch)
			}
			caller := f.symbol(f.mainFile, fmt.Sprintf("f_%s_%d", g.name, i), childOwner, "function", "f()", staticCaller)
			if g.mode == "same_final" {
				f.dispatchFact(targetFile, target, true, "instance")
			} else if g.mode == "same_static" {
				f.dispatchFact(targetFile, target, false, "static")
				f.dispatchFact(f.mainFile, caller, false, "static")
			} else if g.mode != "missing_fact" {
				dispatch := "instance"
				if g.mode == "malformed" {
					dispatch = ""
				}
				f.dispatchFact(targetFile, target, g.mode != "ordinary", dispatch)
				if g.mode == "duplicate_fact" {
					f.dispatchFact(targetFile, target, true, dispatch)
				}
			}
			f.call(f.mainFile, caller, "self."+method, "swift:self", 0, i+1)
			if g.mode == "same_blocker" {
				f.blocker(f.mainFile, childOwner, method, graph.ScopeImportSwiftMemberValue, false)
			}
			if g.mode == "opposite_static" {
				f.blocker(f.mainFile, childOwner, method, graph.ScopeImportSwiftMemberValue, true)
			}
		}
	}
	f.resolve()
	var resolved, unresolved int
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM edges WHERE repo_id=? AND dst_symbol_id IS NOT NULL`, f.repoID).Scan(&resolved); err != nil {
		t.Fatal(err)
	}
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM edges WHERE repo_id=? AND dst_symbol_id IS NULL`, f.repoID).Scan(&unresolved); err != nil {
		t.Fatal(err)
	}
	if resolved != 600 || unresolved != 2100 {
		t.Fatalf("state=(resolved %d,unresolved %d), want (600,2100)", resolved, unresolved)
	}
	rows, err := f.store.db.QueryContext(f.ctx, `SELECT resolution_strategy,COUNT(*) FROM edges WHERE repo_id=? GROUP BY resolution_strategy`, f.repoID)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	strategies := map[string]int{}
	for rows.Next() {
		var strategy string
		var count int
		if err := rows.Scan(&strategy, &count); err != nil {
			t.Fatal(err)
		}
		strategies[strategy] = count
	}
	for _, g := range groups {
		if g.strategy != "" && strategies[g.strategy] < g.count {
			t.Fatalf("strategy %q count=%d, want at least %d", g.strategy, strategies[g.strategy], g.count)
		}
	}
	want := map[string]int{
		"": 2100,
		ResolutionStrategySwiftClassSelfInheritedFinalMethodScope: 400,
		ResolutionStrategySwiftClassSelfFinalMethodScope:          100,
		ResolutionStrategySwiftClassSelfStaticMethodScope:         100,
	}
	if len(strategies) != len(want) {
		t.Fatalf("strategies=%v, want %v", strategies, want)
	}
	for strategy, count := range want {
		if strategies[strategy] != count {
			t.Fatalf("strategy %q count=%d, want %d", strategy, strategies[strategy], count)
		}
	}
	f.resolve()
	rows, err = f.store.db.QueryContext(f.ctx, `SELECT resolution_strategy,COUNT(*) FROM edges WHERE repo_id=? GROUP BY resolution_strategy`, f.repoID)
	if err != nil {
		t.Fatal(err)
	}
	second := map[string]int{}
	for rows.Next() {
		var strategy string
		var count int
		if err := rows.Scan(&strategy, &count); err != nil {
			t.Fatal(err)
		}
		second[strategy] = count
	}
	rows.Close()
	if len(second) != len(strategies) {
		t.Fatalf("second strategies=%v, want %v", second, strategies)
	}
	for strategy, count := range strategies {
		if second[strategy] != count {
			t.Fatalf("second strategy %q count=%d, want %d", strategy, second[strategy], count)
		}
	}
}

func TestSwiftClassSelfFinalMethodOwnerFactProof(t *testing.T) {
	for _, tc := range []struct {
		name         string
		ownerFacts   int
		ownerFinal   bool
		wantStrategy string
		wantBound    bool
	}{
		{"one non-final owner", 1, false, ResolutionStrategySwiftClassSelfFinalMethodScope, true},
		{"missing owner", 0, false, "", false},
		{"duplicate owner", 2, false, "", false},
		{"final owner uses existing strategy", 1, true, ResolutionStrategySwiftClassSelfFinalScope, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwiftScopeFixture(t)
			owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
			target := f.symbol(f.mainFile, "run", "Service", "function", "run()", false)
			caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", false)
			edge := f.call(f.mainFile, caller, "self.run", "swift:self", 0, 1)
			for i := 0; i < tc.ownerFacts; i++ {
				f.declarationFact(f.mainFile, owner, tc.ownerFinal)
			}
			f.declarationFact(f.mainFile, target, true)
			f.reference(f.mainFile, caller, "self.run", 1)
			f.resolve()
			if tc.wantBound {
				assertSwiftBinding(t, f, edge, target, tc.wantStrategy)
			} else {
				assertSwiftBindingCleared(t, f, edge)
			}
		})
	}
}

func TestSwiftClassSelfFinalMethodTargetFactProof(t *testing.T) {
	for _, tc := range []struct {
		name, dispatch   string
		final, duplicate bool
		wantBound        bool
	}{
		{"final instance", "instance", true, false, true},
		{"non-final instance", "instance", false, false, false},
		{"missing fact", "", false, false, false},
		{"duplicate final instance", "instance", true, true, false},
		{"final static", "static", true, false, false},
		{"final class", "class", true, false, false},
		{"final empty dispatch", "", true, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwiftScopeFixture(t)
			owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
			target := f.symbol(f.mainFile, "run", "Service", "function", "run()", false)
			caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", false)
			edge := f.call(f.mainFile, caller, "self.run", "swift:self", 0, 1)
			f.reference(f.mainFile, caller, "self.run", 1)
			f.declarationFact(f.mainFile, owner, false)
			if tc.dispatch != "" {
				f.dispatchFact(f.mainFile, target, tc.final, tc.dispatch)
				if tc.duplicate {
					f.dispatchFact(f.mainFile, target, tc.final, tc.dispatch)
				}
			}
			f.resolve()
			if tc.wantBound {
				assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfFinalMethodScope)
			} else {
				assertSwiftBindingCleared(t, f, edge)
			}
		})
	}
}

func TestSwiftClassSelfFinalMethodLifecycle(t *testing.T) {
	f := newSwiftScopeFixture(t)
	owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
	target := f.symbol(f.mainFile, "run", "Service", "function", "run()", false)
	caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", false)
	edge := f.call(f.mainFile, caller, "self.run", "swift:self", 0, 1)
	f.reference(f.mainFile, caller, "self.run", 1)
	f.declarationFact(f.mainFile, owner, false)
	f.declarationFact(f.mainFile, target, true)
	f.resolve()
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfFinalMethodScope)

	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET is_final=0 WHERE symbol_id=?`, target); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftBindingCleared(t, f, edge)

	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET is_final=1,dispatch_kind='instance' WHERE symbol_id=?`, target); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfFinalMethodScope)
}

func TestSwiftClassSelfFinalMethodCrossFileHazardLifecycle(t *testing.T) {
	f := newSwiftScopeFixture(t)
	owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
	target := f.symbol(f.mainFile, "run", "Service", "function", "run()", false)
	caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", false)
	edge := f.call(f.mainFile, caller, "self.run", "swift:self", 0, 1)
	f.reference(f.mainFile, caller, "self.run", 1)
	f.declarationFact(f.mainFile, owner, false)
	f.declarationFact(f.mainFile, target, true)
	hazard := f.file("Conformance.swift")
	f.resolve()
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfFinalMethodScope)
	f.relation(hazard, "Service", "P", "conformance", false, false)
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Conformance.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftBindingCleared(t, f, edge)
	if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM swift_inheritance_relations WHERE file_id=?`, hazard); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Conformance.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfFinalMethodScope)
}

func TestSwiftClassSelfFinalMethodSubclassPresenceDoesNotVeto(t *testing.T) {
	f := newSwiftScopeFixture(t)
	owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
	target := f.symbol(f.mainFile, "run", "Service", "function", "run()", false)
	caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", false)
	child := f.symbol(f.mainFile, "Child", "", "class", "", false)
	childRun := f.symbol(f.mainFile, "run", "Child", "function", "run(id:)", false)
	edge := f.call(f.mainFile, caller, "self.run", "swift:self", 0, 1)
	f.reference(f.mainFile, caller, "self.run", 1)
	f.declarationFact(f.mainFile, owner, false)
	f.declarationFact(f.mainFile, target, true)
	f.relation(f.mainFile, "Child", "Service", "superclass", false, false)
	f.declarationFact(f.mainFile, childRun, false)
	var relations int
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM swift_inheritance_relations WHERE child_qualified_name=? AND target_qualified_name=?`, "Child", "Service").Scan(&relations); err != nil {
		t.Fatal(err)
	}
	if relations != 1 {
		t.Fatalf("subclass relation count=%d, want 1", relations)
	}
	f.resolve()
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfFinalMethodScope)
	_ = child
}

func TestSwiftClassSelfFinalMethodCrossFileDestinationStaysUnresolved(t *testing.T) {
	f := newSwiftScopeFixture(t)
	owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
	caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", false)
	edge := f.call(f.mainFile, caller, "self.run", "swift:self", 0, 1)
	f.declarationFact(f.mainFile, owner, false)
	destination := f.file("Extension.swift")
	target := f.symbol(destination, "run", "Service", "function", "run()", false)
	f.declarationFact(destination, target, true)
	f.reference(f.mainFile, caller, "self.run", 1)
	f.resolve()
	assertSwiftBindingCleared(t, f, edge)
}

func TestSwiftClassSelfFinalMethodStats(t *testing.T) {
	for _, final := range []bool{true, false} {
		f := newSwiftScopeFixture(t)
		owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
		target := f.symbol(f.mainFile, "run", "Service", "function", "run()", false)
		caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", false)
		edge := f.call(f.mainFile, caller, "self.run", "swift:self", 0, 1)
		f.reference(f.mainFile, caller, "self.run", 1)
		f.declarationFact(f.mainFile, owner, false)
		f.declarationFact(f.mainFile, target, final)
		stats := f.resolveNames("run")
		assertSwiftStats(t, stats, 1, btoi(final), btoi(!final), 0)
		if final {
			assertSwiftEdgeMetadata(t, f, edge, target, ResolutionStrategySwiftClassSelfFinalMethodScope)
		} else {
			assertSwiftEdgeUnresolved(t, f, edge)
		}
	}
}

func btoi(v bool) int {
	if v {
		return 1
	}
	return 0
}

func TestSwiftClassSelfFinalMethodHazardAndBlockerParity(t *testing.T) {
	for _, tc := range []struct {
		name, callerPath, hazardPath, relation       string
		generic, constrained, blockTest, blockStatic bool
		wantBound                                    bool
	}{
		{"production test conformance", "Service.swift", "Tests/Hazard.swift", "conformance", false, false, false, false, true},
		{"production production conformance", "Service.swift", "Hazard.swift", "conformance", false, false, false, false, false},
		{"test test conformance", "Tests/Service.swift", "Tests/Hazard.swift", "conformance", false, false, false, false, false},
		{"test production conformance", "Tests/Service.swift", "Hazard.swift", "conformance", false, false, false, false, false},
		{"production superclass", "Service.swift", "Hazard.swift", "superclass", false, false, false, false, false},
		{"production unproven", "Service.swift", "Hazard.swift", "unproven", false, false, false, false, false},
		{"production constrained", "Service.swift", "Hazard.swift", "conformance", false, true, false, false, false},
		{"production generic", "Service.swift", "Hazard.swift", "conformance", true, false, false, false, false},
		{"production test blocker", "Service.swift", "", "", false, false, true, false, true},
		{"production blocker", "Service.swift", "", "", false, false, false, false, false},
		{"test test blocker", "Tests/Service.swift", "", "", false, false, true, false, false},
		{"opposite static blocker", "Service.swift", "", "", false, false, false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwiftScopeFixture(t)
			callerFile := f.mainFile
			if strings.HasPrefix(tc.callerPath, "Tests/") {
				callerFile = f.file(tc.callerPath)
			}
			owner := f.symbol(callerFile, "Service", "", "class", "", false)
			target := f.symbol(callerFile, "run", "Service", "function", "run()", false)
			caller := f.symbol(callerFile, "f", "Service", "function", "f()", false)
			edge := f.call(callerFile, caller, "self.run", "swift:self", 0, 1)
			f.reference(callerFile, caller, "self.run", 1)
			f.declarationFact(callerFile, owner, false)
			f.declarationFact(callerFile, target, true)
			if tc.relation != "" {
				f.relation(f.file(tc.hazardPath), "Service", "P", tc.relation, tc.generic, tc.constrained)
			}
			if tc.name == "production test blocker" || tc.name == "production blocker" || tc.name == "test test blocker" || tc.name == "opposite static blocker" {
				blockFile := callerFile
				if tc.blockTest {
					blockFile = f.file("Tests/Blocker.swift")
				}
				f.blocker(blockFile, "Service", "run", graph.ScopeImportSwiftMemberValue, tc.blockStatic)
			}
			f.resolve()
			if tc.wantBound {
				assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfFinalMethodScope)
			} else {
				assertSwiftBindingCleared(t, f, edge)
			}
		})
	}
}

func TestSwiftClassSelfFinalMethodRepairMarkerOnly(t *testing.T) {
	f := newSwiftScopeFixture(t)
	owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
	target := f.symbol(f.mainFile, "run", "Service", "function", "run()", false)
	caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", false)
	edge := f.call(f.mainFile, caller, "self.run", "swift:self", 0, 1)
	f.reference(f.mainFile, caller, "self.run", 1)
	f.declarationFact(f.mainFile, owner, false)
	f.declarationFact(f.mainFile, target, true)
	for _, repair := range resolverRepairs {
		if repair.key == swiftClassSelfFinalMethodRepairSettingKey {
			continue
		}
		if err := f.store.markRepairDone(f.ctx, repair.key, f.repoID); err != nil {
			t.Fatal(err)
		}
	}
	ran, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID)
	if err != nil || !ran {
		t.Fatalf("repair=(%v,%v)", ran, err)
	}
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfFinalMethodScope)
	var marker string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT value FROM settings WHERE key=?`, swiftClassSelfFinalMethodRepairSettingKey+fmt.Sprintf(".%d", f.repoID)).Scan(&marker); err != nil || marker != "1" {
		t.Fatalf("marker=%q err=%v", marker, err)
	}
	ran, err = f.store.RepairResolverBindingsOnce(f.ctx, f.repoID)
	if err != nil || ran {
		t.Fatalf("second repair=(%v,%v)", ran, err)
	}
}

func TestSwiftClassSelfFinalMethodRepairClearsUnsafeBinding(t *testing.T) {
	f := newSwiftScopeFixture(t)
	owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
	target := f.symbol(f.mainFile, "run", "Service", "function", "run()", false)
	caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", false)
	edge := f.call(f.mainFile, caller, "self.run", "swift:self", 0, 1)
	f.reference(f.mainFile, caller, "self.run", 1)
	f.declarationFact(f.mainFile, owner, false)
	f.declarationFact(f.mainFile, target, false)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET dst_symbol_id=?,resolution_strategy=?,resolution_confidence=? WHERE id=?`, target, ResolutionStrategySwiftClassSelfFinalMethodScope, ResolutionConfidenceHigh, edge); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE references_tbl SET symbol_id=? WHERE repo_id=?`, target, f.repoID); err != nil {
		t.Fatal(err)
	}
	for _, repair := range resolverRepairs {
		if repair.key == swiftClassSelfFinalMethodRepairSettingKey {
			continue
		}
		if err := f.store.markRepairDone(f.ctx, repair.key, f.repoID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID); err != nil {
		t.Fatal(err)
	}
	assertSwiftBindingCleared(t, f, edge)
}

func TestSwiftClassSelfFinalMethodMixedStress(t *testing.T) {
	f := newSwiftScopeFixture(t)
	type kind struct {
		name                                                  string
		count                                                 int
		owner, relation                                       string
		ownerFinal, methodFinal, blocker, opposite, duplicate bool
	}
	categories := []kind{
		{"final_nonfinal", 100, "", "", true, false, false, false, false},
		{"final_final", 100, "", "", true, true, false, false, false},
		{"new", 100, "", "", false, true, false, false, false},
		{"nonfinal", 100, "", "", false, false, false, false, false},
		{"superclass", 100, "", "superclass", false, true, false, false, false},
		{"conformance", 100, "", "conformance", false, true, false, false, false},
		{"unproven", 100, "", "unproven", false, true, false, false, false},
		{"constrained", 100, "", "conformance", false, true, false, false, false},
		{"generic", 100, "", "conformance", false, true, false, false, false},
		{"qualified", 100, "Outer", "", false, true, false, false, false},
		{"same_blocker", 100, "", "", false, true, true, false, false},
		{"opposite_blocker", 100, "", "", false, true, false, true, false},
		{"duplicate_target", 100, "", "", false, true, false, false, true},
	}
	const total = 1300
	for _, tc := range categories {
		ownerName := "Service_" + tc.name
		container := ownerName
		if tc.owner != "" {
			container = tc.owner + "." + ownerName
		}
		ownerID := f.symbol(f.mainFile, ownerName, tc.owner, "class", "", false)
		f.declarationFact(f.mainFile, ownerID, tc.ownerFinal)
		for i := 0; i < tc.count; i++ {
			method := fmt.Sprintf("run_%s_%d", tc.name, i)
			target := f.symbol(f.mainFile, method, container, "function", method+"()", false)
			f.declarationFact(f.mainFile, target, tc.methodFinal)
			if tc.duplicate {
				f.declarationFact(f.mainFile, target, tc.methodFinal)
			}
			caller := f.symbol(f.mainFile, fmt.Sprintf("f_%s_%d", tc.name, i), container, "function", "f()", false)
			edge := f.call(f.mainFile, caller, "self."+method, "swift:self", 0, i+1)
			if tc.relation != "" {
				f.relation(f.mainFile, ownerName, "P", tc.relation, tc.name == "generic", tc.name == "constrained")
			}
			if tc.blocker {
				f.blocker(f.mainFile, ownerName, method, graph.ScopeImportSwiftMemberValue, false)
			}
			if tc.opposite {
				f.blocker(f.mainFile, ownerName, method, graph.ScopeImportSwiftMemberValue, true)
			}
			_ = edge
		}
	}
	var edgeCount int
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM edges WHERE repo_id=? AND evidence='swift:self'`, f.repoID).Scan(&edgeCount); err != nil {
		t.Fatal(err)
	}
	if edgeCount != total {
		t.Fatalf("edges=%d want=%d", edgeCount, total)
	}
	f.resolve()
	check := func() (resolved, unresolved, old, method int) {
		if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM edges WHERE repo_id=? AND evidence='swift:self' AND dst_symbol_id IS NOT NULL`, f.repoID).Scan(&resolved); err != nil {
			t.Fatal(err)
		}
		if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM edges WHERE repo_id=? AND evidence='swift:self' AND dst_symbol_id IS NULL`, f.repoID).Scan(&unresolved); err != nil {
			t.Fatal(err)
		}
		if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM edges WHERE repo_id=? AND resolution_strategy=?`, f.repoID, ResolutionStrategySwiftClassSelfFinalScope).Scan(&old); err != nil {
			t.Fatal(err)
		}
		if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM edges WHERE repo_id=? AND resolution_strategy=?`, f.repoID, ResolutionStrategySwiftClassSelfFinalMethodScope).Scan(&method); err != nil {
			t.Fatal(err)
		}
		return
	}
	r1, u1, old1, method1 := check()
	if r1 != 500 || u1 != 800 || r1+u1 != total || old1 != 200 || method1 != 300 {
		t.Fatalf("counts=(%d,%d), strategies=(%d,%d)", r1, u1, old1, method1)
	}
	f.resolve()
	r2, u2, old2, method2 := check()
	if r2 != r1 || u2 != u1 || old2 != old1 || method2 != method1 {
		t.Fatalf("second counts=(%d,%d,%d,%d), first=(%d,%d,%d,%d)", r2, u2, old2, method2, r1, u1, old1, method1)
	}
}

func TestSwiftClassSelfControls(t *testing.T) {
	t.Run("uppercase Self accepts proven static and class dispatch", func(t *testing.T) {
		for _, dispatch := range []string{"static", "class"} {
			t.Run(dispatch, func(t *testing.T) {
				f := newSwiftScopeFixture(t)
				owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
				target := f.symbol(f.mainFile, "make", "Service", "function", "make()", true)
				caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", false)
				edge := f.call(f.mainFile, caller, "Self.make", "swift:Self", 0, 1)
				f.reference(f.mainFile, caller, "Self.make", 1)
				f.declarationFact(f.mainFile, owner, true)
				f.dispatchFact(f.mainFile, target, false, dispatch)
				f.resolve()
				assertSwiftClassSelfTypeBinding(t, f, edge, target)
			})
		}
	})
	t.Run("uppercase Self rejects missing, instance, and duplicate dispatch facts", func(t *testing.T) {
		for _, name := range []string{"missing", "instance", "duplicate"} {
			t.Run(name, func(t *testing.T) {
				f := newSwiftScopeFixture(t)
				owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
				target := f.symbol(f.mainFile, "make", "Service", "function", "make()", true)
				caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", false)
				edge := f.call(f.mainFile, caller, "Self.make", "swift:Self", 0, 1)
				f.declarationFact(f.mainFile, owner, true)
				if name != "missing" {
					f.dispatchFact(f.mainFile, target, false, map[string]string{"instance": "instance", "duplicate": "static"}[name])
				}
				if name == "duplicate" {
					f.dispatchFact(f.mainFile, target, false, "class")
				}
				f.resolve()
				if got := f.dst(edge); got.Valid {
					t.Fatalf("Self call resolved to %d", got.Int64)
				}
			})
		}
	})
	t.Run("final method also binds", func(t *testing.T) {
		f := newSwiftScopeFixture(t)
		owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
		target := f.symbol(f.mainFile, "run", "Service", "function", "run()", false)
		caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", false)
		edge := f.call(f.mainFile, caller, "self.run", "swift:self", 0, 1)
		f.declarationFact(f.mainFile, owner, true)
		f.declarationFact(f.mainFile, target, true)
		f.resolve()
		if got := f.dst(edge); !got.Valid || got.Int64 != target {
			t.Fatalf("final method binding=%v, want %d", got, target)
		}
	})
	t.Run("uppercase Self stays unresolved", func(t *testing.T) {
		f := newSwiftScopeFixture(t)
		owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
		target := f.symbol(f.mainFile, "make", "Service", "function", "make()", true)
		caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", true)
		edge := f.call(f.mainFile, caller, "Self.make", "swift:Self", 0, 1)
		f.declarationFact(f.mainFile, owner, true)
		f.resolve()
		if got := f.dst(edge); got.Valid {
			t.Fatalf("Self call resolved to %d", got.Int64)
		}
		_ = target
	})
	t.Run("duplicate owner fails closed", func(t *testing.T) {
		f := newSwiftScopeFixture(t)
		owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
		f.symbol(f.file("Other.swift"), "Service", "", "class", "", false)
		target := f.symbol(f.mainFile, "run", "Service", "function", "run()", false)
		caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", false)
		edge := f.call(f.mainFile, caller, "self.run", "swift:self", 0, 1)
		f.declarationFact(f.mainFile, owner, true)
		f.resolve()
		if got := f.dst(edge); got.Valid {
			t.Fatalf("duplicate owner resolved to %d (target %d)", got.Int64, target)
		}
	})
}

func TestSwiftClassSelfCrossFileHazardLifecycle(t *testing.T) {
	f := newSwiftScopeFixture(t)
	owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
	target := f.symbol(f.mainFile, "run", "Service", "function", "run()", false)
	caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", false)
	edge := f.call(f.mainFile, caller, "self.run", "swift:self", 0, 1)
	f.reference(f.mainFile, caller, "self.run", 1)
	f.declarationFact(f.mainFile, owner, true)
	hazard := f.file("Conformance.swift")
	f.resolve()
	if got := f.dst(edge); !got.Valid || got.Int64 != target {
		t.Fatalf("initial binding=%v, want %d", got, target)
	}
	var reference sql.NullInt64
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT symbol_id FROM references_tbl WHERE repo_id=?`, f.repoID).Scan(&reference); err != nil || !reference.Valid || reference.Int64 != target {
		t.Fatalf("initial reference=%v, err=%v", reference, err)
	}
	f.relation(hazard, "Service", "P", "conformance", false, false)
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Conformance.swift"}); err != nil {
		t.Fatal(err)
	}
	if got := f.dst(edge); got.Valid {
		t.Fatalf("hazard left binding=%d", got.Int64)
	}
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT symbol_id FROM references_tbl WHERE repo_id=?`, f.repoID).Scan(&reference); err != nil {
		t.Fatal(err)
	}
	if reference.Valid {
		t.Fatalf("hazard left reference=%d", reference.Int64)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM swift_inheritance_relations WHERE file_id=?`, hazard); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Conformance.swift"}); err != nil {
		t.Fatal(err)
	}
	if got := f.dst(edge); !got.Valid || got.Int64 != target {
		t.Fatalf("removed hazard binding=%v, want %d", got, target)
	}
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT symbol_id FROM references_tbl WHERE repo_id=?`, f.repoID).Scan(&reference); err != nil || !reference.Valid || reference.Int64 != target {
		t.Fatalf("removed hazard reference=%v, err=%v", reference, err)
	}
	f.relation(hazard, "Service", "P", "conformance", false, false)
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Conformance.swift"}); err != nil {
		t.Fatal(err)
	}
	if got := f.dst(edge); got.Valid {
		t.Fatalf("restored hazard edge=%v reference=%v", got, reference)
	}
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT symbol_id FROM references_tbl WHERE repo_id=?`, f.repoID).Scan(&reference); err != nil {
		t.Fatal(err)
	}
	if reference.Valid {
		t.Fatalf("restored hazard left reference=%d", reference.Int64)
	}
}

func TestSwiftClassSelfTestHazardsUseFileClassification(t *testing.T) {
	t.Run("test-only hazard does not block production", func(t *testing.T) {
		f := newSwiftScopeFixture(t)
		owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
		target := f.symbol(f.mainFile, "run", "Service", "function", "run()", false)
		caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", false)
		edge := f.call(f.mainFile, caller, "self.run", "swift:self", 0, 1)
		f.declarationFact(f.mainFile, owner, true)
		hazard := f.file("Tests/Conformance.swift")
		f.relation(hazard, "Service", "P", "conformance", false, false)
		f.resolve()
		if got := f.dst(edge); !got.Valid || got.Int64 != target {
			t.Fatalf("test-only hazard binding=%v, want %d", got, target)
		}
	})
	t.Run("test caller sees test hazard", func(t *testing.T) {
		f := newSwiftScopeFixture(t)
		file := f.file("Tests/Service.swift")
		owner := f.symbol(file, "Service", "", "class", "", false)
		target := f.symbol(file, "run", "Service", "function", "run()", false)
		caller := f.symbol(file, "f", "Service", "function", "f()", false)
		edge := f.call(file, caller, "self.run", "swift:self", 0, 1)
		f.declarationFact(file, owner, true)
		f.relation(file, "Service", "P", "conformance", false, false)
		f.resolve()
		if got := f.dst(edge); got.Valid {
			t.Fatalf("test hazard resolved to %d (target %d)", got.Int64, target)
		}
	})
}

func TestSwiftClassSelfTypeCrossFileHazardLifecycle(t *testing.T) {
	f := newSwiftScopeFixture(t)
	owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
	target := f.symbol(f.mainFile, "make", "Service", "function", "make()", true)
	caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", true)
	edge := f.call(f.mainFile, caller, "Self.make", "swift:Self", 0, 1)
	f.reference(f.mainFile, caller, "Self.make", 1)
	f.declarationFact(f.mainFile, owner, true)
	f.dispatchFact(f.mainFile, target, false, "static")
	hazard := f.file("Conformance.swift")
	f.resolve()
	assertSwiftClassSelfTypeBinding(t, f, edge, target)
	f.relation(hazard, "Service", "P", "conformance", false, false)
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Conformance.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftBindingCleared(t, f, edge)
	if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM swift_inheritance_relations WHERE file_id=?`, hazard); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Conformance.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftClassSelfTypeBinding(t, f, edge, target)
	f.relation(hazard, "Service", "P", "conformance", false, false)
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Conformance.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftBindingCleared(t, f, edge)
}

func TestSwiftClassSelfTypeHazardFiltering(t *testing.T) {
	for _, tc := range []struct {
		name, callerPath, hazardPath, relation string
		wantBound                              bool
	}{
		{"production-test-conformance", "Service.swift", "Tests/Conformance.swift", "conformance", true},
		{"production-production-conformance", "Service.swift", "Conformance.swift", "conformance", false},
		{"test-test-conformance", "Tests/Service.swift", "Tests/Conformance.swift", "conformance", false},
		{"production-test-unproven", "Service.swift", "Tests/Conformance.swift", "unproven", true},
		{"test-production-conformance", "Tests/Service.swift", "Conformance.swift", "conformance", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwiftScopeFixture(t)
			callerFile := f.mainFile
			if strings.HasPrefix(tc.callerPath, "Tests/") {
				callerFile = f.file(tc.callerPath)
			}
			owner := f.symbol(callerFile, "Service", "", "class", "", false)
			target := f.symbol(callerFile, "make", "Service", "function", "make()", true)
			caller := f.symbol(callerFile, "f", "Service", "function", "f()", true)
			edge := f.call(callerFile, caller, "Self.make", "swift:Self", 0, 1)
			f.reference(callerFile, caller, "Self.make", 1)
			f.declarationFact(callerFile, owner, true)
			f.dispatchFact(callerFile, target, false, "static")
			hazard := f.file(tc.hazardPath)
			f.relation(hazard, "Service", "P", tc.relation, false, false)
			f.resolve()
			if tc.wantBound {
				assertSwiftClassSelfTypeBinding(t, f, edge, target)
			} else {
				assertSwiftBindingCleared(t, f, edge)
			}
		})
	}
}

func TestSwiftClassSelfBlockersMatchStaticnessAndTestShadow(t *testing.T) {
	for _, tc := range []struct {
		name                      string
		callerTest, blockTest     bool
		callerStatic, blockStatic bool
		blockKind                 string
		wantBound                 bool
	}{
		{"production same-static", false, false, false, false, graph.ScopeImportSwiftMemberValue, false},
		{"production test-only same-static", false, true, false, false, graph.ScopeImportSwiftMemberValue, true},
		{"test same-static", true, true, false, false, graph.ScopeImportSwiftMemberValue, false},
		{"opposite-static", false, false, false, true, graph.ScopeImportSwiftMemberValue, true},
		{"enum case same-static", false, false, true, true, graph.ScopeImportSwiftEnumCase, false},
		{"enum case opposite-static", false, false, false, true, graph.ScopeImportSwiftEnumCase, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwiftScopeFixture(t)
			callerFile := f.mainFile
			if tc.callerTest {
				callerFile = f.file("Tests/Service.swift")
			}
			owner := f.symbol(callerFile, "Service", "", "class", "", false)
			target := f.symbol(callerFile, "run", "Service", "function", "run()", tc.callerStatic)
			caller := f.symbol(callerFile, "f", "Service", "function", "f()", tc.callerStatic)
			edge := f.call(callerFile, caller, "self.run", "swift:self", 0, 1)
			f.declarationFact(callerFile, owner, true)
			blockFile := callerFile
			if tc.blockTest {
				blockFile = f.file("Tests/Blocker.swift")
			}
			f.blocker(blockFile, "Service", "run", tc.blockKind, tc.blockStatic)
			f.resolve()
			got := f.dst(edge)
			if tc.wantBound != got.Valid || (tc.wantBound && got.Int64 != target) {
				t.Fatalf("dst=%v, want bound=%v target=%d", got, tc.wantBound, target)
			}
		})
	}
}

func TestSwiftClassSelfTypeBlockersMatchStaticnessAndTestShadow(t *testing.T) {
	for _, tc := range []struct {
		name                    string
		callerTest, blockerTest bool
		blockStatic             bool
		blockKind               string
		wantBound               bool
	}{
		{"production-static-member", false, false, true, graph.ScopeImportSwiftMemberValue, false},
		{"production-test-only-static-member", false, true, true, graph.ScopeImportSwiftMemberValue, true},
		{"test-test-static-member", true, true, true, graph.ScopeImportSwiftMemberValue, false},
		{"opposite-static-instance-member", false, false, false, graph.ScopeImportSwiftMemberValue, true},
		{"static-enum-case", false, false, true, graph.ScopeImportSwiftEnumCase, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwiftScopeFixture(t)
			callerFile := f.mainFile
			if tc.callerTest {
				callerFile = f.file("Tests/Service.swift")
			}
			owner := f.symbol(callerFile, "Service", "", "class", "", false)
			target := f.symbol(callerFile, "make", "Service", "function", "make()", true)
			caller := f.symbol(callerFile, "f", "Service", "function", "f()", false)
			edge := f.call(callerFile, caller, "Self.make", "swift:Self", 0, 1)
			f.reference(callerFile, caller, "Self.make", 1)
			f.declarationFact(callerFile, owner, true)
			f.dispatchFact(callerFile, target, false, "static")
			blockFile := callerFile
			if tc.blockerTest {
				blockFile = f.file("Tests/Blocker.swift")
			}
			f.blocker(blockFile, "Service", "make", tc.blockKind, tc.blockStatic)
			f.resolve()
			if tc.wantBound {
				assertSwiftClassSelfTypeBinding(t, f, edge, target)
			} else {
				assertSwiftBindingCleared(t, f, edge)
			}
		})
	}
}

func TestSwiftClassSelfTypeResolveEdgesForNames(t *testing.T) {
	for _, tc := range []struct {
		name       string
		hazard     bool
		resolved   int
		unresolved int
	}{
		{"positive", false, 1, 0},
		{"conformance-veto", true, 0, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwiftScopeFixture(t)
			owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
			target := f.symbol(f.mainFile, "make", "Service", "function", "make()", true)
			caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", false)
			edge := f.call(f.mainFile, caller, "Self.make", "swift:Self", 0, 1)
			f.reference(f.mainFile, caller, "Self.make", 1)
			f.declarationFact(f.mainFile, owner, true)
			f.dispatchFact(f.mainFile, target, false, "static")
			if tc.hazard {
				f.relation(f.mainFile, "Service", "P", "conformance", false, false)
			}
			stats := f.resolveNames("make")
			assertSwiftStats(t, stats, 1, tc.resolved, tc.unresolved, 0)
			if tc.resolved == 1 {
				assertSwiftEdgeMetadata(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeFinalScope)
			} else {
				assertSwiftBindingCleared(t, f, edge)
			}
		})
	}
}

func TestSwiftClassSelfQualifiedOwnerDoesNotUseSuffix(t *testing.T) {
	f := newSwiftScopeFixture(t)
	outer := f.symbol(f.mainFile, "Outer", "", "class", "", false)
	owner := f.symbol(f.mainFile, "Service", "Outer", "class", "", false)
	target := f.symbol(f.mainFile, "run", "Outer.Service", "function", "run()", false)
	caller := f.symbol(f.mainFile, "f", "Outer.Service", "function", "f()", false)
	edge := f.call(f.mainFile, caller, "self.run", "swift:self", 0, 1)
	f.declarationFact(f.mainFile, owner, true)
	f.relation(f.mainFile, "Service", "P", "conformance", false, false)
	f.resolve()
	if got := f.dst(edge); !got.Valid || got.Int64 != target {
		t.Fatalf("qualified owner binding=%v, want %d", got, target)
	}
	_ = outer
}

func TestSwiftClassSelfFinalMethodQualifiedOwnerUsesExactHazardKey(t *testing.T) {
	f := newSwiftScopeFixture(t)
	owner := f.symbol(f.mainFile, "Service", "Outer", "class", "", false)
	target := f.symbol(f.mainFile, "run", "Outer.Service", "function", "run()", false)
	caller := f.symbol(f.mainFile, "f", "Outer.Service", "function", "f()", false)
	edge := f.call(f.mainFile, caller, "self.run", "swift:self", 0, 1)
	f.reference(f.mainFile, caller, "self.run", 1)
	f.declarationFact(f.mainFile, owner, false)
	f.declarationFact(f.mainFile, target, true)
	// A relation for Service must not be treated as a relation for Outer.Service.
	f.relation(f.mainFile, "Service", "P", "conformance", false, false)
	f.resolve()
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfFinalMethodScope)
}

func TestSwiftClassSelfRepairBindsAndMarksOnce(t *testing.T) {
	f := newSwiftScopeFixture(t)
	owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
	target := f.symbol(f.mainFile, "run", "Service", "function", "run()", false)
	caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", false)
	edge := f.call(f.mainFile, caller, "self.run", "swift:self", 0, 1)
	f.declarationFact(f.mainFile, owner, true)
	didRun, err := f.store.runResolverRepairOnce(f.ctx, f.repoID, swiftClassSelfRepair)
	if err != nil || !didRun {
		t.Fatalf("repair=(%v,%v)", didRun, err)
	}
	if got := f.dst(edge); !got.Valid || got.Int64 != target {
		t.Fatalf("repaired binding=%v, want %d", got, target)
	}
	var marker string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT value FROM settings WHERE key=?`, swiftClassSelfRepairSettingKey+fmt.Sprintf(".%d", f.repoID)).Scan(&marker); err != nil {
		t.Fatal(err)
	}
	if marker != "1" {
		t.Fatalf("repair marker=%q", marker)
	}
	didRun, err = f.store.runResolverRepairOnce(f.ctx, f.repoID, swiftClassSelfRepair)
	if err != nil || didRun {
		t.Fatalf("second repair=(%v,%v)", didRun, err)
	}
}

func TestSwiftClassSelfPublicRepairBindsUnchangedSource(t *testing.T) {
	f := newSwiftScopeFixture(t)
	owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
	target := f.symbol(f.mainFile, "run", "Service", "function", "run()", false)
	caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", false)
	edge := f.call(f.mainFile, caller, "self.run", "swift:self", 0, 1)
	f.declarationFact(f.mainFile, owner, true)
	f.reference(f.mainFile, caller, "self.run", 1)
	if got := f.dst(edge); got.Valid {
		t.Fatalf("pre-repair edge unexpectedly bound=%d", got.Int64)
	}
	ran, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID)
	if err != nil || !ran {
		t.Fatalf("public repair=(%v,%v)", ran, err)
	}
	assertSwiftClassSelfBinding(t, f, edge, target)
	var marker string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT value FROM settings WHERE key=?`, swiftClassSelfRepairSettingKey+fmt.Sprintf(".%d", f.repoID)).Scan(&marker); err != nil {
		t.Fatal(err)
	}
	if marker != "1" {
		t.Fatalf("public repair marker=%q", marker)
	}
	ran, err = f.store.RepairResolverBindingsOnce(f.ctx, f.repoID)
	if err != nil || ran {
		t.Fatalf("second public repair=(%v,%v)", ran, err)
	}
}

func TestSwiftClassSelfTypeRepairRunsAfterEarlierMarkers(t *testing.T) {
	f := newSwiftScopeFixture(t)
	owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
	target := f.symbol(f.mainFile, "make", "Service", "function", "make()", true)
	caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", false)
	edge := f.call(f.mainFile, caller, "Self.make", "swift:Self", 0, 1)
	f.reference(f.mainFile, caller, "Self.make", 1)
	f.declarationFact(f.mainFile, owner, true)
	f.dispatchFact(f.mainFile, target, false, "static")
	for _, repair := range resolverRepairs {
		if repair.key == swiftClassSelfTypeRepairSettingKey {
			continue
		}
		if err := f.store.markRepairDone(f.ctx, repair.key, f.repoID); err != nil {
			t.Fatal(err)
		}
	}
	ran, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID)
	if err != nil || !ran {
		t.Fatalf("public repair=(%v,%v)", ran, err)
	}
	assertSwiftClassSelfTypeBinding(t, f, edge, target)
	var marker string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT value FROM settings WHERE key=?`, swiftClassSelfTypeRepairSettingKey+fmt.Sprintf(".%d", f.repoID)).Scan(&marker); err != nil || marker != "1" {
		t.Fatalf("new repair marker=%q err=%v", marker, err)
	}
	ran, err = f.store.RepairResolverBindingsOnce(f.ctx, f.repoID)
	if err != nil || ran {
		t.Fatalf("second public repair=(%v,%v)", ran, err)
	}
}

func TestSwiftClassSelfRepairClearsUnsafeBinding(t *testing.T) {
	f := newSwiftScopeFixture(t)
	owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
	target := f.symbol(f.mainFile, "run", "Service", "function", "run()", false)
	caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", false)
	edge := f.call(f.mainFile, caller, "self.run", "swift:self", 0, 1)
	f.declarationFact(f.mainFile, owner, true)
	f.reference(f.mainFile, caller, "self.run", 1)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET dst_symbol_id=?,resolution_strategy=?,resolution_confidence=? WHERE id=?`, target, ResolutionStrategySwiftClassSelfFinalScope, ResolutionConfidenceHigh, edge); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE references_tbl SET symbol_id=? WHERE repo_id=?`, target, f.repoID); err != nil {
		t.Fatal(err)
	}
	f.relation(f.mainFile, "Service", "P", "conformance", false, false)
	if _, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID); err != nil {
		t.Fatal(err)
	}
	assertSwiftBindingCleared(t, f, edge)
}

func TestSwiftClassSelfTypeRepairClearsUnsafeBinding(t *testing.T) {
	f := newSwiftScopeFixture(t)
	owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
	target := f.symbol(f.mainFile, "make", "Service", "function", "make()", true)
	caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", false)
	edge := f.call(f.mainFile, caller, "Self.make", "swift:Self", 0, 1)
	f.declarationFact(f.mainFile, owner, true)
	f.dispatchFact(f.mainFile, target, false, "static")
	f.reference(f.mainFile, caller, "Self.make", 1)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET dst_symbol_id=?,resolution_strategy=?,resolution_confidence=? WHERE id=?`, target, ResolutionStrategySwiftClassSelfTypeFinalScope, ResolutionConfidenceHigh, edge); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE references_tbl SET symbol_id=? WHERE repo_id=?`, target, f.repoID); err != nil {
		t.Fatal(err)
	}
	f.relation(f.mainFile, "Service", "P", "conformance", false, false)
	if _, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID); err != nil {
		t.Fatal(err)
	}
	var dst, strategy, confidence string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT COALESCE(CAST(dst_symbol_id AS TEXT),''),COALESCE(resolution_strategy,''),COALESCE(resolution_confidence,'') FROM edges WHERE id=?`, edge).Scan(&dst, &strategy, &confidence); err != nil {
		t.Fatal(err)
	}
	if dst != "" || strategy != "" || confidence != "" {
		t.Fatalf("unsafe binding survived=(%q,%q,%q)", dst, strategy, confidence)
	}
	var reference sql.NullInt64
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT symbol_id FROM references_tbl WHERE repo_id=?`, f.repoID).Scan(&reference); err != nil {
		t.Fatal(err)
	}
	if reference.Valid {
		t.Fatalf("unsafe reference survived=%d", reference.Int64)
	}
}

func assertSwiftBindingCleared(t *testing.T, f *swiftScopeFixture, edge int64) {
	t.Helper()
	var dst, strategy, confidence string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT COALESCE(CAST(dst_symbol_id AS TEXT),''),COALESCE(resolution_strategy,''),COALESCE(resolution_confidence,'') FROM edges WHERE id=?`, edge).Scan(&dst, &strategy, &confidence); err != nil {
		t.Fatal(err)
	}
	if dst != "" || strategy != "" || confidence != "" {
		t.Fatalf("unsafe binding survived=(%q,%q,%q)", dst, strategy, confidence)
	}
	assertSwiftReferenceCleared(t, f, swiftReferenceID(t, f, edge))
}

func TestSwiftClassSelfTypeBatchedMixedEdges(t *testing.T) {
	f := newSwiftScopeFixture(t)
	type category struct {
		name  string
		count int
		bound bool
	}
	categories := []category{
		{"static", 300, true}, {"class", 200, true}, {"nonfinal", 100, true},
		{"conformance", 100, false}, {"unproven", 100, false}, {"constrained", 100, false},
		{"qualified", 100, true}, {"same_blocker", 50, false}, {"opposite_blocker", 50, true},
		{"missing_dispatch", 50, false}, {"duplicate_dispatch", 50, false},
	}
	const total = 1200
	edges := make([]int64, 0, total)
	samples := make(map[string]int64, len(categories))
	sampleTargets := make(map[string]int64, len(categories))
	line := 1
	for _, tc := range categories {
		ownerName, owner := "Service_"+tc.name, ""
		if tc.name == "qualified" {
			ownerName, owner = "ServiceQualified", "Outer"
		}
		container := ownerName
		if owner != "" {
			container = owner + "." + ownerName
		}
		ownerID := f.symbol(f.mainFile, ownerName, owner, "class", "", false)
		final := tc.name != "nonfinal"
		f.declarationFact(f.mainFile, ownerID, final)
		for i := 0; i < tc.count; i++ {
			method := fmt.Sprintf("make_%s_%d", tc.name, i)
			target := f.symbol(f.mainFile, method, container, "function", method+"()", true)
			if tc.name != "missing_dispatch" {
				dispatch := "static"
				if tc.name == "class" {
					dispatch = "class"
				}
				f.dispatchFact(f.mainFile, target, false, dispatch)
				if tc.name == "duplicate_dispatch" {
					f.dispatchFact(f.mainFile, target, false, "class")
				}
			}
			caller := f.symbol(f.mainFile, fmt.Sprintf("f_%s_%d", tc.name, i), container, "function", "f()", false)
			dst := "Self." + method
			edge := f.call(f.mainFile, caller, dst, "swift:Self", 0, line)
			if i == 0 {
				samples[tc.name] = edge
				sampleTargets[tc.name] = target
			}
			edges = append(edges, edge)
			if tc.name == "conformance" || tc.name == "unproven" || tc.name == "constrained" {
				kind := tc.name
				if kind == "constrained" {
					kind = "conformance"
				}
				f.relation(f.mainFile, ownerName, "P", kind, false, tc.name == "constrained")
			}
			if tc.name == "same_blocker" {
				f.blocker(f.mainFile, ownerName, method, graph.ScopeImportSwiftMemberValue, true)
			}
			if tc.name == "opposite_blocker" {
				f.blocker(f.mainFile, ownerName, method, graph.ScopeImportSwiftMemberValue, false)
			}
			line++
		}
	}
	if len(edges) != total {
		t.Fatalf("total edges=%d, want %d", len(edges), total)
	}
	count := func() (resolved, unresolved int) {
		if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM edges WHERE repo_id=? AND evidence='swift:Self' AND dst_symbol_id IS NOT NULL`, f.repoID).Scan(&resolved); err != nil {
			t.Fatal(err)
		}
		if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM edges WHERE repo_id=? AND evidence='swift:Self' AND dst_symbol_id IS NULL`, f.repoID).Scan(&unresolved); err != nil {
			t.Fatal(err)
		}
		return
	}
	f.resolve()
	resolved1, unresolved1 := count()
	if resolved1 != 750 || unresolved1 != 450 || resolved1+unresolved1 != total {
		t.Fatalf("counts=(%d,%d), want (750,450)", resolved1, unresolved1)
	}
	for _, tc := range categories {
		if tc.bound {
			strategy := ResolutionStrategySwiftClassSelfTypeFinalScope
			if tc.name == "nonfinal" {
				strategy = ResolutionStrategySwiftClassSelfTypeStaticMethodScope
			}
			assertSwiftEdgeMetadata(t, f, samples[tc.name], sampleTargets[tc.name], strategy)
		} else {
			assertSwiftEdgeUnresolved(t, f, samples[tc.name])
		}
	}
	if got := f.dst(samples["static"]); !got.Valid {
		t.Fatal("static sample unresolved")
	}
	if got := f.dst(samples["class"]); !got.Valid {
		t.Fatal("class sample unresolved")
	}
	if got := f.dst(samples["nonfinal"]); !got.Valid {
		t.Fatal("non-final static sample unresolved")
	}
	if got := f.dst(samples["same_blocker"]); got.Valid {
		t.Fatalf("same-static blocker sample resolved=%d", got.Int64)
	}
	if got := f.dst(samples["opposite_blocker"]); !got.Valid {
		t.Fatal("opposite-static blocker sample unresolved")
	}
	f.resolve()
	resolved2, unresolved2 := count()
	if resolved2 != resolved1 || unresolved2 != unresolved1 {
		t.Fatalf("second counts=(%d,%d), want (%d,%d)", resolved2, unresolved2, resolved1, unresolved1)
	}
}

func assertSwiftClassSelfBinding(t *testing.T, f *swiftScopeFixture, edge, target int64) {
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfFinalScope)
}

func assertSwiftClassSelfTypeBinding(t *testing.T, f *swiftScopeFixture, edge, target int64) {
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeFinalScope)
}

func assertSwiftBinding(t *testing.T, f *swiftScopeFixture, edge, target int64, wantStrategy string) {
	t.Helper()
	var got int64
	var strategy, confidence string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT dst_symbol_id,resolution_strategy,resolution_confidence FROM edges WHERE id=?`, edge).Scan(&got, &strategy, &confidence); err != nil {
		t.Fatal(err)
	}
	if got != target || strategy != wantStrategy || confidence != ResolutionConfidenceHigh {
		t.Fatalf("binding=(%d,%q,%q), want (%d,%q,%q)", got, strategy, confidence, target, wantStrategy, ResolutionConfidenceHigh)
	}
	assertSwiftReferenceTarget(t, f, swiftReferenceID(t, f, edge), target)
}

func assertSwiftEdgeMetadata(t *testing.T, f *swiftScopeFixture, edge, target int64, wantStrategy string) {
	t.Helper()
	var got int64
	var strategy, confidence string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT dst_symbol_id,resolution_strategy,resolution_confidence FROM edges WHERE id=?`, edge).Scan(&got, &strategy, &confidence); err != nil {
		t.Fatal(err)
	}
	if got != target || strategy != wantStrategy || confidence != ResolutionConfidenceHigh {
		t.Fatalf("edge=(%d,%q,%q), want=(%d,%q,%q)", got, strategy, confidence, target, wantStrategy, ResolutionConfidenceHigh)
	}
}

func edgeState(t *testing.T, f *swiftScopeFixture, edge int64) (sql.NullInt64, string, string) {
	t.Helper()
	var dst sql.NullInt64
	var strategy, confidence string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT dst_symbol_id,resolution_strategy,resolution_confidence FROM edges WHERE id=?`, edge).Scan(&dst, &strategy, &confidence); err != nil {
		t.Fatal(err)
	}
	return dst, strategy, confidence
}

func assertSwiftEdgeUnresolved(t *testing.T, f *swiftScopeFixture, edge int64) {
	t.Helper()
	var dst sql.NullInt64
	var strategy, confidence string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT dst_symbol_id,resolution_strategy,resolution_confidence FROM edges WHERE id=?`, edge).Scan(&dst, &strategy, &confidence); err != nil {
		t.Fatal(err)
	}
	if dst.Valid || strategy != "" || confidence != "" {
		t.Fatalf("edge=(%v,%q,%q), want unresolved with empty metadata", dst, strategy, confidence)
	}
}

func assertSwiftInheritedStaticEdgeUnresolved(t *testing.T, f *swiftScopeFixture, edge int64) {
	t.Helper()
	assertSwiftEdgeUnresolved(t, f, edge)
	assertSwiftReferenceCleared(t, f, swiftReferenceID(t, f, edge))
}

func swiftReferenceID(t *testing.T, f *swiftScopeFixture, edge int64) int64 {
	t.Helper()
	var fileID, sourceID int64
	var name string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT file_id,src_symbol_id,dst_name FROM edges WHERE id=?`, edge).Scan(&fileID, &sourceID, &name); err != nil {
		t.Fatal(err)
	}
	var refID int64
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT id FROM references_tbl WHERE repo_id=? AND file_id=? AND context_symbol_id=? AND name=?`, f.repoID, fileID, sourceID, name).Scan(&refID); err != nil {
		t.Fatal(err)
	}
	return refID
}

func swiftReferenceSymbol(t *testing.T, f *swiftScopeFixture, refID int64) sql.NullInt64 {
	t.Helper()
	var symbol sql.NullInt64
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT symbol_id FROM references_tbl WHERE id=?`, refID).Scan(&symbol); err != nil {
		t.Fatal(err)
	}
	return symbol
}

func assertSwiftReferenceTarget(t *testing.T, f *swiftScopeFixture, refID, target int64) {
	t.Helper()
	got := swiftReferenceSymbol(t, f, refID)
	if !got.Valid || got.Int64 != target {
		t.Fatalf("reference %v, want %d", got, target)
	}
}

func assertSwiftReferenceCleared(t *testing.T, f *swiftScopeFixture, refID int64) {
	t.Helper()
	if got := swiftReferenceSymbol(t, f, refID); got.Valid {
		t.Fatalf("reference=%d, want NULL", got.Int64)
	}
}

func assertSwiftP22FinalClassEdgeUnresolved(t *testing.T, f *swiftScopeFixture, edge int64) {
	t.Helper()
	assertSwiftEdgeUnresolved(t, f, edge)
	assertSwiftReferenceCleared(t, f, swiftReferenceID(t, f, edge))
}

func TestSwiftClassSelfStaticMethodScope(t *testing.T) {
	for _, dispatch := range []string{"static", "class"} {
		t.Run(dispatch+" caller", func(t *testing.T) {
			f := newSwiftScopeFixture(t)
			owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
			target := f.symbol(f.mainFile, "make", "Service", "function", "make()", true)
			caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", true)
			edge := f.call(f.mainFile, caller, "self.make", "swift:self", 0, 1)
			f.declarationFact(f.mainFile, owner, false)
			f.dispatchFact(f.mainFile, caller, false, dispatch)
			f.dispatchFact(f.mainFile, target, false, "static")
			f.reference(f.mainFile, caller, "self.make", 1)
			f.resolve()
			assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfStaticMethodScope)
		})
	}
}

func TestSwiftClassSelfTypeStaticMethodScope(t *testing.T) {
	for _, dispatch := range []string{"instance", "class", "static"} {
		t.Run(dispatch+" caller", func(t *testing.T) {
			f := newSwiftScopeFixture(t)
			owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
			target := f.symbol(f.mainFile, "make", "Service", "function", "make()", true)
			caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", dispatch == "static")
			edge := f.call(f.mainFile, caller, "Self.make", "swift:Self", 0, 1)
			f.reference(f.mainFile, caller, "Self.make", 1)
			f.declarationFact(f.mainFile, owner, false)
			f.dispatchFact(f.mainFile, target, false, "static")
			f.dispatchFact(f.mainFile, caller, false, dispatch)
			f.resolve()
			assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeStaticMethodScope)
		})
	}
}

func TestSwiftClassSelfTypeStaticMethodScopeBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name, targetDispatch                   string
		ownerFinal, targetFinal, duplicateFact bool
		wantBound, finalOwner                  bool
	}{
		{"ordinary class", "class", false, false, false, false, false},
		{"final class method", "class", false, true, false, true, false},
		{"static", "static", false, false, false, true, false},
		{"final static", "static", false, true, false, true, false},
		{"missing fact", "", false, false, false, false, false},
		{"duplicate fact", "static", false, false, true, false, false},
		{"final owner precedence", "static", true, false, false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwiftScopeFixture(t)
			owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
			target := f.symbol(f.mainFile, "make", "Service", "function", "make()", true)
			caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", false)
			edge := f.call(f.mainFile, caller, "Self.make", "swift:Self", 0, 1)
			f.reference(f.mainFile, caller, "Self.make", 1)
			f.declarationFact(f.mainFile, owner, tc.ownerFinal)
			if tc.targetDispatch != "" {
				f.dispatchFact(f.mainFile, target, tc.targetFinal, tc.targetDispatch)
				if tc.duplicateFact {
					f.dispatchFact(f.mainFile, target, tc.targetFinal, tc.targetDispatch)
				}
			}
			f.resolve()
			if !tc.wantBound {
				assertSwiftEdgeUnresolved(t, f, edge)
				return
			}
			want := ResolutionStrategySwiftClassSelfTypeStaticMethodScope
			if tc.name == "final class method" {
				want = ResolutionStrategySwiftClassSelfTypeFinalClassMethodScope
			}
			if tc.finalOwner {
				want = ResolutionStrategySwiftClassSelfTypeFinalScope
			}
			assertSwiftBinding(t, f, edge, target, want)
		})
	}
}

func TestSwiftClassSelfTypeFinalClassMethodScope(t *testing.T) {
	for _, dispatch := range []string{"instance", "class", "static"} {
		t.Run(dispatch+" caller", func(t *testing.T) {
			f := newSwiftScopeFixture(t)
			owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
			target := f.symbol(f.mainFile, "make", "Service", "function", "make()", true)
			caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", dispatch == "static")
			edge := f.call(f.mainFile, caller, "Self.make", "swift:Self", 0, 1)
			f.reference(f.mainFile, caller, "Self.make", 1)
			f.declarationFact(f.mainFile, owner, false)
			f.dispatchFact(f.mainFile, target, true, "class")
			f.dispatchFact(f.mainFile, caller, false, dispatch)
			f.resolve()
			assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeFinalClassMethodScope)
		})
	}
}

func TestSwiftClassSelfTypeFinalClassMethodScopeLifecycleAndRepair(t *testing.T) {
	f, _, target, _, edge := newSwiftNonFinalStaticSelfFixture(t, "Service", "", "class", true)
	f.resolve()
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeFinalClassMethodScope)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET is_final=0 WHERE symbol_id=?`, target); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftEdgeUnresolved(t, f, edge)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET is_final=1 WHERE symbol_id=?`, target); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeFinalClassMethodScope)
	for _, repair := range resolverRepairs {
		if repair.key == swiftClassSelfTypeFinalClassMethodRepairSettingKey {
			continue
		}
		if err := f.store.markRepairDone(f.ctx, repair.key, f.repoID); err != nil {
			t.Fatal(err)
		}
	}
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT value FROM settings WHERE key=?`, swiftClassSelfTypeFinalClassMethodRepairSettingKey+fmt.Sprintf(".%d", f.repoID)).Scan(new(string)); err == nil {
		t.Fatal("new repair marker unexpectedly present")
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET dst_symbol_id=?,resolution_strategy=?,resolution_confidence=? WHERE id=?`, target, ResolutionStrategySwiftClassSelfTypeFinalClassMethodScope, ResolutionConfidenceHigh, edge); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE references_tbl SET symbol_id=? WHERE repo_id=?`, target, f.repoID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET is_final=0 WHERE symbol_id=?`, target); err != nil {
		t.Fatal(err)
	}
	run, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID)
	if err != nil || !run {
		t.Fatalf("repair=(%v,%v)", run, err)
	}
	assertSwiftEdgeUnresolved(t, f, edge)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET is_final=1 WHERE symbol_id=?`, target); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM settings WHERE key=?`, swiftClassSelfTypeFinalClassMethodRepairSettingKey+fmt.Sprintf(".%d", f.repoID)); err != nil {
		t.Fatal(err)
	}
	run, err = f.store.RepairResolverBindingsOnce(f.ctx, f.repoID)
	if err != nil || !run {
		t.Fatalf("rebind repair=(%v,%v)", run, err)
	}
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeFinalClassMethodScope)
	run, err = f.store.RepairResolverBindingsOnce(f.ctx, f.repoID)
	if err != nil || run {
		t.Fatalf("second repair=(%v,%v)", run, err)
	}
}

func TestSwiftClassSelfTypeStaticMethodScopeLifecycleAndRepair(t *testing.T) {
	f := newSwiftScopeFixture(t)
	owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
	target := f.symbol(f.mainFile, "make", "Service", "function", "make()", true)
	caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", false)
	edge := f.call(f.mainFile, caller, "Self.make", "swift:Self", 0, 1)
	f.reference(f.mainFile, caller, "Self.make", 1)
	f.declarationFact(f.mainFile, owner, false)
	f.dispatchFact(f.mainFile, target, false, "static")
	f.resolve()
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeStaticMethodScope)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET dispatch_kind='class' WHERE symbol_id=?`, target); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftEdgeUnresolved(t, f, edge)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET dispatch_kind='static' WHERE symbol_id=?`, target); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeStaticMethodScope)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET dst_symbol_id=?,resolution_strategy=?,resolution_confidence=? WHERE id=?`, target, ResolutionStrategySwiftClassSelfTypeStaticMethodScope, ResolutionConfidenceHigh, edge); err != nil {
		t.Fatal(err)
	}
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT value FROM settings WHERE key=?`, swiftClassSelfTypeStaticMethodRepairSettingKey+fmt.Sprintf(".%d", f.repoID)).Scan(new(string)); err == nil {
		t.Fatal("new repair marker unexpectedly present")
	}
	for _, repair := range resolverRepairs {
		if repair.key == swiftClassSelfTypeStaticMethodRepairSettingKey {
			continue
		}
		if err := f.store.markRepairDone(f.ctx, repair.key, f.repoID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET dispatch_kind='class' WHERE symbol_id=?`, target); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID); err != nil {
		t.Fatal(err)
	}
	assertSwiftEdgeUnresolved(t, f, edge)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET dispatch_kind='static' WHERE symbol_id=?`, target); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM settings WHERE key=?`, swiftClassSelfTypeStaticMethodRepairSettingKey+fmt.Sprintf(".%d", f.repoID)); err != nil {
		t.Fatal(err)
	}
	run, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID)
	if err != nil || !run {
		t.Fatalf("repair=(%v,%v)", run, err)
	}
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeStaticMethodScope)
	run, err = f.store.RepairResolverBindingsOnce(f.ctx, f.repoID)
	if err != nil || run {
		t.Fatalf("second repair=(%v,%v)", run, err)
	}
}

func TestSwiftClassSelfTypeStaticMethodScopeMixedStress(t *testing.T) {
	f := newSwiftScopeFixture(t)
	type group struct {
		name, dispatch, strategy string
		ownerFinal, bound        bool
		count                    int
	}
	groups := []group{
		{"final", "static", ResolutionStrategySwiftClassSelfTypeFinalScope, true, true, 300},
		{"static", "static", ResolutionStrategySwiftClassSelfTypeStaticMethodScope, false, true, 600},
		{"final_static", "static", ResolutionStrategySwiftClassSelfTypeStaticMethodScope, false, true, 300},
		{"class", "class", "", false, false, 300},
		{"missing", "", "", false, false, 100},
	}
	const total = 1600
	for _, g := range groups {
		owner := f.symbol(f.mainFile, "Service_"+g.name, "", "class", "", false)
		f.declarationFact(f.mainFile, owner, g.ownerFinal)
		for i := 0; i < g.count; i++ {
			method := fmt.Sprintf("make_%s_%d", g.name, i)
			target := f.symbol(f.mainFile, method, "Service_"+g.name, "function", method+"()", true)
			caller := f.symbol(f.mainFile, fmt.Sprintf("f_%s_%d", g.name, i), "Service_"+g.name, "function", "f()", false)
			if g.dispatch != "" {
				f.dispatchFact(f.mainFile, target, g.name == "final_static", g.dispatch)
			}
			f.call(f.mainFile, caller, "Self."+method, "swift:Self", 0, i+1)
		}
	}
	var gotTotal int
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM edges WHERE repo_id=?`, f.repoID).Scan(&gotTotal); err != nil {
		t.Fatal(err)
	}
	if gotTotal != total {
		t.Fatalf("edges=%d want %d", gotTotal, total)
	}
	counts := func() map[string]int {
		rows, err := f.store.db.QueryContext(f.ctx, `SELECT resolution_strategy,COUNT(*) FROM edges WHERE repo_id=? GROUP BY resolution_strategy`, f.repoID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]int{}
		for rows.Next() {
			var strategy string
			var count int
			if err := rows.Scan(&strategy, &count); err != nil {
				t.Fatal(err)
			}
			out[strategy] = count
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return out
	}
	f.resolve()
	first := counts()
	if first[ResolutionStrategySwiftClassSelfTypeFinalScope] != 300 || first[ResolutionStrategySwiftClassSelfTypeStaticMethodScope] != 900 {
		t.Fatalf("strategy counts=%v", first)
	}
	var resolved int
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM edges WHERE repo_id=? AND dst_symbol_id IS NOT NULL`, f.repoID).Scan(&resolved); err != nil {
		t.Fatal(err)
	}
	if resolved != 1200 {
		t.Fatalf("resolved=%d want 1200", resolved)
	}
	f.resolve()
	second := counts()
	if len(first) != len(second) {
		t.Fatalf("second counts=%v want %v", second, first)
	}
	for strategy, count := range first {
		if second[strategy] != count {
			t.Fatalf("second counts=%v want %v", second, first)
		}
	}
}

func TestSwiftClassSelfTypeStaticMethodScopeHardenedStress(t *testing.T) {
	f := newSwiftScopeFixture(t)
	type group struct {
		name, callerDispatch, targetDispatch string
		ownerFinal, targetFinal              bool
		relation, blocker                    string
		qualified                            bool
	}
	groups := []group{
		{"final", "instance", "static", true, false, "", "", false},
		{"instance", "instance", "static", false, false, "", "", false},
		{"class", "class", "static", false, false, "", "", false},
		{"static", "static", "static", false, false, "", "", false},
		{"final_static", "instance", "static", false, true, "", "", false},
		{"ordinary_class", "instance", "class", false, false, "", "", false},
		{"final_class", "instance", "class", false, true, "", "", false},
		{"final_class_class", "class", "class", false, true, "", "", false},
		{"missing_fact", "instance", "", false, false, "", "", false},
		{"duplicate_fact", "instance", "static", false, false, "duplicate", "", false},
		{"malformed_dispatch", "instance", "instance", false, false, "", "", false},
		{"superclass", "instance", "static", false, false, "superclass", "", false},
		{"conformance", "instance", "static", false, false, "conformance", "", false},
		{"unproven", "instance", "static", false, false, "unproven", "", false},
		{"constrained", "instance", "static", false, false, "conformance", "", false},
		{"qualified", "instance", "static", false, false, "", "", true},
		{"same_blocker", "instance", "static", false, false, "", "same", false},
		{"opposite_blocker", "instance", "static", false, false, "", "opposite", false},
	}
	const perGroup = 100
	for _, g := range groups {
		ownerName, ownerContainer := "Service_"+g.name, ""
		if g.qualified {
			ownerName, ownerContainer = "Service_qualified", "Outer"
		}
		qualifiedOwner := ownerName
		if ownerContainer != "" {
			qualifiedOwner = ownerContainer + "." + ownerName
		}
		owner := f.symbol(f.mainFile, ownerName, ownerContainer, "class", "", false)
		f.declarationFact(f.mainFile, owner, g.ownerFinal)
		for i := 0; i < perGroup; i++ {
			method := fmt.Sprintf("make_%s_%d", g.name, i)
			target := f.symbol(f.mainFile, method, qualifiedOwner, "function", method+"()", true)
			if g.targetDispatch != "" {
				f.dispatchFact(f.mainFile, target, g.targetFinal, g.targetDispatch)
				if g.relation == "duplicate" {
					f.dispatchFact(f.mainFile, target, g.targetFinal, g.targetDispatch)
				}
				if g.name == "malformed_dispatch" {
					f.dispatchFact(f.mainFile, target, g.targetFinal, "class")
				}
			}
			caller := f.symbol(f.mainFile, fmt.Sprintf("f_%s_%d", g.name, i), qualifiedOwner, "function", "f()", g.callerDispatch == "static")
			f.dispatchFact(f.mainFile, caller, false, g.callerDispatch)
			f.call(f.mainFile, caller, "Self."+method, "swift:Self", 0, i+1)
			if g.relation != "" && g.relation != "duplicate" {
				child := ownerName
				if g.qualified {
					child = "Service"
				}
				f.relation(f.mainFile, child, "P", g.relation, false, g.name == "constrained")
			}
			if g.blocker != "" {
				blockerOwner := qualifiedOwner
				if g.qualified {
					blockerOwner = "Service"
				}
				f.blocker(f.mainFile, blockerOwner, method, graph.ScopeImportSwiftMemberValue, g.blocker == "same")
			}
		}
	}
	total := len(groups) * perGroup
	var gotTotal int
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM edges WHERE repo_id=?`, f.repoID).Scan(&gotTotal); err != nil {
		t.Fatal(err)
	}
	if gotTotal != total {
		t.Fatalf("total=%d want %d", gotTotal, total)
	}
	state := func() (int, int, map[string]int) {
		var resolved, unresolved int
		if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM edges WHERE repo_id=? AND dst_symbol_id IS NOT NULL`, f.repoID).Scan(&resolved); err != nil {
			t.Fatal(err)
		}
		if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM edges WHERE repo_id=? AND dst_symbol_id IS NULL`, f.repoID).Scan(&unresolved); err != nil {
			t.Fatal(err)
		}
		rows, err := f.store.db.QueryContext(f.ctx, `SELECT resolution_strategy,COUNT(*) FROM edges WHERE repo_id=? GROUP BY resolution_strategy`, f.repoID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		strategies := map[string]int{}
		for rows.Next() {
			var strategy string
			var count int
			if err := rows.Scan(&strategy, &count); err != nil {
				t.Fatal(err)
			}
			strategies[strategy] = count
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return resolved, unresolved, strategies
	}
	f.resolve()
	resolved1, unresolved1, strategies1 := state()
	if resolved1 != 900 || unresolved1 != 900 || strategies1[ResolutionStrategySwiftClassSelfTypeFinalScope] != 100 || strategies1[ResolutionStrategySwiftClassSelfTypeStaticMethodScope] != 600 || strategies1[ResolutionStrategySwiftClassSelfTypeFinalClassMethodScope] != 200 {
		t.Fatalf("state=(%d,%d,%v)", resolved1, unresolved1, strategies1)
	}
	f.resolve()
	resolved2, unresolved2, strategies2 := state()
	if resolved1 != resolved2 || unresolved1 != unresolved2 || len(strategies1) != len(strategies2) {
		t.Fatalf("second state=(%d,%d,%v), first=(%d,%d,%v)", resolved2, unresolved2, strategies2, resolved1, unresolved1, strategies1)
	}
	for strategy, count := range strategies1 {
		if strategies2[strategy] != count {
			t.Fatalf("second strategies=%v want %v", strategies2, strategies1)
		}
	}
}

func TestSwiftClassSelfTypeStaticMethodScopeMalformedSingleDispatch(t *testing.T) {
	f, _, _, _, edge := newSwiftNonFinalStaticSelfFixture(t, "Service", "", "", false)
	var target int64
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT id FROM symbols WHERE repo_id=? AND name='make'`, f.repoID).Scan(&target); err != nil {
		t.Fatal(err)
	}
	f.dispatchFact(f.mainFile, target, false, "")
	var facts int
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM swift_declaration_facts WHERE symbol_id=?`, target).Scan(&facts); err != nil {
		t.Fatal(err)
	}
	if facts != 1 {
		t.Fatalf("dispatch facts=%d want 1", facts)
	}
	f.resolve()
	assertSwiftEdgeUnresolved(t, f, edge)
}

func newSwiftNonFinalStaticSelfFixture(t *testing.T, ownerName, ownerContainer, targetDispatch string, targetFinal bool) (*swiftScopeFixture, int64, int64, int64, int64) {
	t.Helper()
	f := newSwiftScopeFixture(t)
	return newSwiftNonFinalStaticSelfFixtureInFile(t, f, f.mainFile, ownerName, ownerContainer, targetDispatch, targetFinal)
}

func newSwiftNonFinalStaticSelfFixtureInFile(t *testing.T, f *swiftScopeFixture, file int64, ownerName, ownerContainer, targetDispatch string, targetFinal bool) (*swiftScopeFixture, int64, int64, int64, int64) {
	t.Helper()
	owner := f.symbol(file, ownerName, ownerContainer, "class", "", false)
	qualifiedOwner := ownerName
	if ownerContainer != "" {
		qualifiedOwner = ownerContainer + "." + ownerName
	}
	target := f.symbol(file, "make", qualifiedOwner, "function", "make()", true)
	caller := f.symbol(file, "f", qualifiedOwner, "function", "f()", false)
	edge := f.call(file, caller, "Self.make", "swift:Self", 0, 1)
	f.reference(file, caller, "Self.make", 1)
	f.declarationFact(f.mainFile, owner, false)
	if targetDispatch != "" {
		f.dispatchFact(file, target, targetFinal, targetDispatch)
	}
	return f, owner, target, caller, edge
}

func newSwiftFinalClassSelfFixture(t *testing.T, ownerName, ownerContainer string) (*swiftScopeFixture, int64, int64, int64, int64) {
	t.Helper()
	return newSwiftNonFinalStaticSelfFixture(t, ownerName, ownerContainer, "class", true)
}

func TestSwiftClassSelfTypeFinalClassMethodScopeHazards(t *testing.T) {
	for _, tc := range []struct {
		name, relationFile, relationKind            string
		generic, constrained, callerTest, wantBound bool
	}{
		{"superclass", "Hazard.swift", "superclass", false, false, false, false},
		{"conformance", "Hazard.swift", "conformance", false, false, false, false},
		{"unproven", "Hazard.swift", "unproven", false, false, false, false},
		{"constrained conformance", "Hazard.swift", "conformance", false, true, false, false},
		{"generic conformance", "Hazard.swift", "conformance", true, false, false, false},
		{"production test-only", "Tests/Hazard.swift", "conformance", false, false, false, true},
		{"test test-only", "Tests/Hazard.swift", "conformance", false, false, true, false},
		{"test production", "Hazard.swift", "conformance", false, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwiftScopeFixture(t)
			callerFile := f.mainFile
			if tc.callerTest {
				callerFile = f.file("Tests/Service.swift")
			}
			_, _, target, _, edge := newSwiftNonFinalStaticSelfFixtureInFile(t, f, callerFile, "Service", "", "class", true)
			f.resolve()
			assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeFinalClassMethodScope)
			hazardFile := f.file(tc.relationFile)
			f.relation(hazardFile, "Service", "P", tc.relationKind, tc.generic, tc.constrained)
			if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{tc.relationFile}); err != nil {
				t.Fatal(err)
			}
			if tc.wantBound {
				assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeFinalClassMethodScope)
			} else {
				assertSwiftP22FinalClassEdgeUnresolved(t, f, edge)
			}
		})
	}
}

func TestSwiftClassSelfTypeFinalClassMethodScopeBlockers(t *testing.T) {
	for _, tc := range []struct {
		name, kind              string
		callerTest, blockerTest bool
		static, wantBound       bool
	}{
		{"production static", graph.ScopeImportSwiftMemberValue, false, false, true, false},
		{"production test-only static", graph.ScopeImportSwiftMemberValue, false, true, true, true},
		{"test test-only static", graph.ScopeImportSwiftMemberValue, true, true, true, false},
		{"opposite instance", graph.ScopeImportSwiftMemberValue, false, false, false, true},
		{"static enum case", graph.ScopeImportSwiftEnumCase, false, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwiftScopeFixture(t)
			callerFile := f.mainFile
			if tc.callerTest {
				callerFile = f.file("Tests/Service.swift")
			}
			_, _, target, _, edge := newSwiftNonFinalStaticSelfFixtureInFile(t, f, callerFile, "Service", "", "class", true)
			f.resolve()
			assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeFinalClassMethodScope)
			blockerFile := f.mainFile
			blockerPath := "Service.swift"
			if tc.blockerTest {
				blockerFile = f.file("Tests/Blocker.swift")
				blockerPath = "Tests/Blocker.swift"
			}
			f.blocker(blockerFile, "Service", "make", tc.kind, tc.static)
			if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{blockerPath}); err != nil {
				t.Fatal(err)
			}
			if tc.wantBound {
				assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeFinalClassMethodScope)
			} else {
				assertSwiftP22FinalClassEdgeUnresolved(t, f, edge)
			}
		})
	}
}

func TestSwiftClassSelfTypeFinalClassMethodScopeIdentityAndFiles(t *testing.T) {
	t.Run("reverse subclass and unrelated suffix evidence", func(t *testing.T) {
		f, _, target, _, edge := newSwiftFinalClassSelfFixture(t, "Service", "Outer")
		f.relation(f.mainFile, "Child", "Outer.Service", "superclass", false, false)
		f.relation(f.mainFile, "Service", "P", "conformance", false, false)
		f.blocker(f.mainFile, "Service", "make", graph.ScopeImportSwiftMemberValue, true)
		f.resolve()
		assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeFinalClassMethodScope)
	})
	t.Run("cross file refused", func(t *testing.T) {
		f, _, _, _, edge := newSwiftNonFinalStaticSelfFixture(t, "Service", "", "", false)
		other := f.file("Extension.swift")
		target := f.symbol(other, "make", "Service", "function", "make()", true)
		f.dispatchFact(other, target, true, "class")
		f.resolve()
		assertSwiftP22FinalClassEdgeUnresolved(t, f, edge)
	})
	t.Run("same file extension representation", func(t *testing.T) {
		f, _, target, _, edge := newSwiftFinalClassSelfFixture(t, "Service", "")
		f.resolve()
		assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeFinalClassMethodScope)
	})
}

func TestSwiftClassSelfTypeFinalClassMethodScopeSelectorsTrailingAndAmbiguity(t *testing.T) {
	for _, tc := range []struct {
		name, evidence, signature string
		arity                     int
	}{
		{"zero", "swift:Self", "make()", 0},
		{"underscore", "swift:Self;labels=_", "make(_:)", 1},
		{"named", "swift:Self;labels=id:", "make(id:)", 1},
		{"two labels", "swift:Self;labels=id:,cache:", "make(id:,cache:)", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, target, caller, _ := newSwiftFinalClassSelfFixture(t, "Service", "")
			if tc.signature != "make()" {
				if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET signature=? WHERE id=?`, tc.signature, target); err != nil {
					t.Fatal(err)
				}
				f.arity(target, int64(tc.arity), int64(tc.arity))
			}
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET dst_name=?,evidence=?,call_arity=? WHERE src_symbol_id=?`, "Self.make", tc.evidence, tc.arity, caller); err != nil {
				t.Fatal(err)
			}
			f.resolve()
			var edge int64
			if err := f.store.db.QueryRowContext(f.ctx, `SELECT id FROM edges WHERE src_symbol_id=?`, caller).Scan(&edge); err != nil {
				t.Fatal(err)
			}
			assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeFinalClassMethodScope)
		})
	}
	t.Run("trailing closure", func(t *testing.T) {
		f, _, target, caller, edge := newSwiftFinalClassSelfFixture(t, "Service", "")
		if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET name='perform',signature='perform(_:)',arity_min=1,arity_max=1 WHERE id=?`, target); err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET dst_name='Self.perform',evidence='swift:Self;trailing_labels=_',call_arity=1 WHERE id=?`, edge); err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.db.ExecContext(f.ctx, `UPDATE references_tbl SET name='Self.perform',qualified_name='Self.perform' WHERE repo_id=?`, f.repoID); err != nil {
			t.Fatal(err)
		}
		_ = caller
		f.resolve()
		assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeFinalClassMethodScope)
	})
	t.Run("ambiguous", func(t *testing.T) {
		f, _, _, _, edge := newSwiftFinalClassSelfFixture(t, "Service", "")
		duplicate := f.symbol(f.mainFile, "make", "Service", "function", "make()", true)
		f.dispatchFact(f.mainFile, duplicate, true, "class")
		f.resolve()
		assertSwiftP22FinalClassEdgeUnresolved(t, f, edge)
	})
}

func TestSwiftClassSelfTypeFinalClassMethodScopeNamesAndNegative(t *testing.T) {
	f, _, target, _, edge := newSwiftFinalClassSelfFixture(t, "Service", "")
	stats := f.resolveNames("make")
	assertSwiftStats(t, stats, 1, 1, 0, 0)
	assertSwiftEdgeMetadata(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeFinalClassMethodScope)
	for _, dispatch := range []string{"class", "static"} {
		t.Run(dispatch+" ordinary control", func(t *testing.T) {
			f, _, target, _, edge := newSwiftNonFinalStaticSelfFixture(t, "Service", "", dispatch, false)
			f.resolve()
			if dispatch == "static" {
				assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeStaticMethodScope)
			} else {
				assertSwiftP22FinalClassEdgeUnresolved(t, f, edge)
			}
		})
	}
}

func TestSwiftClassSelfTypeFinalClassMethodScopePrecedenceAndTransition(t *testing.T) {
	t.Run("final owner with final class target", func(t *testing.T) {
		f := newSwiftScopeFixture(t)
		owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
		target := f.symbol(f.mainFile, "make", "Service", "function", "make()", true)
		caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", false)
		edge := f.call(f.mainFile, caller, "Self.make", "swift:Self", 0, 1)
		f.reference(f.mainFile, caller, "Self.make", 1)
		f.declarationFact(f.mainFile, owner, true)
		f.dispatchFact(f.mainFile, target, true, "class")
		f.resolve()
		assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeFinalScope)
	})
	f, _, target, _, edge := newSwiftFinalClassSelfFixture(t, "Service", "")
	f.resolve()
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeFinalClassMethodScope)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET dispatch_kind='static' WHERE symbol_id=?`, target); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeStaticMethodScope)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET dispatch_kind='class',is_final=1 WHERE symbol_id=?`, target); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeFinalClassMethodScope)
}

func TestSwiftClassSelfTypeFinalClassMethodScopeCrossFileHazardLifecycle(t *testing.T) {
	f, _, target, _, edge := newSwiftFinalClassSelfFixture(t, "Service", "")
	f.resolve()
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeFinalClassMethodScope)
	hazard := f.file("Conformance.swift")
	f.relation(hazard, "Service", "P", "conformance", false, false)
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Conformance.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftP22FinalClassEdgeUnresolved(t, f, edge)
	if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM swift_inheritance_relations WHERE file_id=?`, hazard); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Conformance.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeFinalClassMethodScope)
	f.relation(hazard, "Service", "P", "conformance", false, false)
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Conformance.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftP22FinalClassEdgeUnresolved(t, f, edge)
}

func TestSwiftClassSelfTypeFinalClassMethodScopeUpgradeRepair(t *testing.T) {
	f, _, target, _, edge := newSwiftFinalClassSelfFixture(t, "Service", "")
	for _, repair := range resolverRepairs {
		if repair.key == swiftClassSelfTypeFinalClassMethodRepairSettingKey {
			continue
		}
		if err := f.store.markRepairDone(f.ctx, repair.key, f.repoID); err != nil {
			t.Fatal(err)
		}
	}
	assertSwiftP22FinalClassEdgeUnresolved(t, f, edge)
	run, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID)
	if err != nil || !run {
		t.Fatalf("repair=(%v,%v)", run, err)
	}
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeFinalClassMethodScope)
	var marker string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT value FROM settings WHERE key=?`, swiftClassSelfTypeFinalClassMethodRepairSettingKey+fmt.Sprintf(".%d", f.repoID)).Scan(&marker); err != nil || marker != "1" {
		t.Fatalf("marker=%q err=%v", marker, err)
	}
	run, err = f.store.RepairResolverBindingsOnce(f.ctx, f.repoID)
	if err != nil || run {
		t.Fatalf("second repair=(%v,%v)", run, err)
	}
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeFinalClassMethodScope)
}

func TestSwiftClassSelfTypeFinalClassMethodScopeUnsafeExistingBindingRepair(t *testing.T) {
	f, _, target, _, edge := newSwiftFinalClassSelfFixture(t, "Service", "")
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET dst_symbol_id=?,resolution_strategy=?,resolution_confidence=? WHERE id=?`, target, ResolutionStrategySwiftClassSelfTypeFinalClassMethodScope, ResolutionConfidenceHigh, edge); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE references_tbl SET symbol_id=? WHERE repo_id=?`, target, f.repoID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET is_final=0 WHERE symbol_id=?`, target); err != nil {
		t.Fatal(err)
	}
	for _, repair := range resolverRepairs {
		if repair.key == swiftClassSelfTypeFinalClassMethodRepairSettingKey {
			continue
		}
		if err := f.store.markRepairDone(f.ctx, repair.key, f.repoID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID); err != nil {
		t.Fatal(err)
	}
	assertSwiftP22FinalClassEdgeUnresolved(t, f, edge)
	var marker string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT value FROM settings WHERE key=?`, swiftClassSelfTypeFinalClassMethodRepairSettingKey+fmt.Sprintf(".%d", f.repoID)).Scan(&marker); err != nil || marker != "1" {
		t.Fatalf("marker=%q err=%v", marker, err)
	}
}

func TestSwiftClassSelfTypeFinalClassMethodScopeMalformedAndDuplicateFacts(t *testing.T) {
	t.Run("single malformed dispatch", func(t *testing.T) {
		f, _, target, _, edge := newSwiftFinalClassSelfFixture(t, "Service", "")
		if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET dispatch_kind='' WHERE symbol_id=?`, target); err != nil {
			t.Fatal(err)
		}
		var facts int
		if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM swift_declaration_facts WHERE symbol_id=?`, target).Scan(&facts); err != nil {
			t.Fatal(err)
		}
		if facts != 1 {
			t.Fatalf("facts=%d want 1", facts)
		}
		f.resolve()
		assertSwiftP22FinalClassEdgeUnresolved(t, f, edge)
	})
	t.Run("duplicate final class facts", func(t *testing.T) {
		f, _, target, _, edge := newSwiftFinalClassSelfFixture(t, "Service", "")
		f.dispatchFact(f.mainFile, target, true, "class")
		var facts int
		if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM swift_declaration_facts WHERE symbol_id=?`, target).Scan(&facts); err != nil {
			t.Fatal(err)
		}
		if facts != 2 {
			t.Fatalf("facts=%d want 2", facts)
		}
		f.resolve()
		assertSwiftP22FinalClassEdgeUnresolved(t, f, edge)
	})
}

func TestSwiftClassSelfTypeFinalClassMethodScopeHardenedStress(t *testing.T) {
	f := newSwiftScopeFixture(t)
	type group struct {
		name, callerDispatch, targetDispatch, relation                                                       string
		ownerFinal, targetFinal, duplicate, missingFact, callerTest, qualified, sameBlocker, oppositeBlocker bool
		boundStrategy                                                                                        string
	}
	groups := []group{
		{"final_owner", "instance", "class", "", true, true, false, false, false, false, false, false, ResolutionStrategySwiftClassSelfTypeFinalScope},
		{"instance", "instance", "class", "", false, true, false, false, false, false, false, false, ResolutionStrategySwiftClassSelfTypeFinalClassMethodScope},
		{"class", "class", "class", "", false, true, false, false, false, false, false, false, ResolutionStrategySwiftClassSelfTypeFinalClassMethodScope},
		{"static", "static", "class", "", false, true, false, false, false, false, false, false, ResolutionStrategySwiftClassSelfTypeFinalClassMethodScope},
		{"static_target", "instance", "static", "", false, false, false, false, false, false, false, false, ResolutionStrategySwiftClassSelfTypeStaticMethodScope},
		{"ordinary", "instance", "class", "", false, false, false, false, false, false, false, false, ""},
		{"missing_finality", "instance", "class", "", false, false, false, false, false, false, false, false, ""},
		{"missing_fact", "instance", "", "", false, false, false, true, false, false, false, false, ""},
		{"duplicate_fact", "instance", "class", "", false, true, true, false, false, false, false, false, ""},
		{"malformed", "instance", "", "", false, true, false, false, false, false, false, false, ""},
		{"superclass", "instance", "class", "superclass", false, true, false, false, false, false, false, false, ""},
		{"conformance", "instance", "class", "conformance", false, true, false, false, false, false, false, false, ""},
		{"unproven", "instance", "class", "unproven", false, true, false, false, false, false, false, false, ""},
		{"constrained", "instance", "class", "constrained", false, true, false, false, false, false, false, false, ""},
		{"generic", "instance", "class", "generic", false, true, false, false, false, false, false, false, ""},
		{"qualified", "instance", "class", "reverse", false, true, false, false, false, true, false, false, ResolutionStrategySwiftClassSelfTypeFinalClassMethodScope},
		{"same_blocker", "instance", "class", "", false, true, false, false, false, false, true, false, ""},
		{"opposite_blocker", "instance", "class", "", false, true, false, false, false, false, false, true, ResolutionStrategySwiftClassSelfTypeFinalClassMethodScope},
	}
	const perGroup = 100
	for _, g := range groups {
		ownerName, ownerContainer := "Service_"+g.name, ""
		if g.qualified {
			ownerName, ownerContainer = "Service_qualified", "Outer"
		}
		qualifiedOwner := ownerName
		if ownerContainer != "" {
			qualifiedOwner = ownerContainer + "." + ownerName
		}
		owner := f.symbol(f.mainFile, ownerName, ownerContainer, "class", "", false)
		f.declarationFact(f.mainFile, owner, g.ownerFinal)
		for i := 0; i < perGroup; i++ {
			method := fmt.Sprintf("make_%s_%d", g.name, i)
			target := f.symbol(f.mainFile, method, qualifiedOwner, "function", method+"()", true)
			if !g.missingFact {
				f.dispatchFact(f.mainFile, target, g.targetFinal, g.targetDispatch)
				if g.duplicate {
					f.dispatchFact(f.mainFile, target, g.targetFinal, g.targetDispatch)
				}
			}
			caller := f.symbol(f.mainFile, fmt.Sprintf("f_%s_%d", g.name, i), qualifiedOwner, "function", "f()", g.callerDispatch == "static")
			f.call(f.mainFile, caller, "Self."+method, "swift:Self", 0, i+1)
			if g.sameBlocker || g.oppositeBlocker {
				f.blocker(f.mainFile, qualifiedOwner, method, graph.ScopeImportSwiftMemberValue, g.sameBlocker)
			}
		}
		if g.relation != "" {
			if g.relation == "reverse" {
				f.relation(f.mainFile, "Child", qualifiedOwner, "superclass", false, false)
			} else {
				generic, constrained := g.relation == "generic", g.relation == "constrained"
				relationKind := g.relation
				if generic || constrained {
					relationKind = "conformance"
				}
				f.relation(f.mainFile, qualifiedOwner, "P", relationKind, generic, constrained)
			}
		}
	}
	state := func() (int, int, map[string]int) {
		var resolved, unresolved int
		if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM edges WHERE repo_id=? AND dst_symbol_id IS NOT NULL`, f.repoID).Scan(&resolved); err != nil {
			t.Fatal(err)
		}
		if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM edges WHERE repo_id=? AND dst_symbol_id IS NULL`, f.repoID).Scan(&unresolved); err != nil {
			t.Fatal(err)
		}
		rows, err := f.store.db.QueryContext(f.ctx, `SELECT resolution_strategy,COUNT(*) FROM edges WHERE repo_id=? GROUP BY resolution_strategy`, f.repoID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		strategies := map[string]int{}
		for rows.Next() {
			var strategy string
			var count int
			if err := rows.Scan(&strategy, &count); err != nil {
				t.Fatal(err)
			}
			strategies[strategy] = count
		}
		return resolved, unresolved, strategies
	}
	f.resolve()
	resolved1, unresolved1, strategies1 := state()
	if resolved1 != 700 || unresolved1 != 1100 || strategies1[ResolutionStrategySwiftClassSelfTypeFinalScope] != 100 || strategies1[ResolutionStrategySwiftClassSelfTypeStaticMethodScope] != 100 || strategies1[ResolutionStrategySwiftClassSelfTypeFinalClassMethodScope] != 500 {
		t.Fatalf("state=(%d,%d,%v)", resolved1, unresolved1, strategies1)
	}
	f.resolve()
	resolved2, unresolved2, strategies2 := state()
	if resolved1 != resolved2 || unresolved1 != unresolved2 || len(strategies1) != len(strategies2) {
		t.Fatalf("second state=(%d,%d,%v), first=(%d,%d,%v)", resolved2, unresolved2, strategies2, resolved1, unresolved1, strategies1)
	}
	for strategy, count := range strategies1 {
		if strategies2[strategy] != count {
			t.Fatalf("second strategies=%v want %v", strategies2, strategies1)
		}
	}
}

func TestSwiftClassSelfTypeStaticMethodScopeNonFinalHazards(t *testing.T) {
	for _, tc := range []struct {
		name, relationFile, relationKind string
		generic, constrained, callerTest bool
		wantBound                        bool
	}{
		{"superclass", "Hazard.swift", "superclass", false, false, false, false},
		{"conformance", "Hazard.swift", "conformance", false, false, false, false},
		{"unproven", "Hazard.swift", "unproven", false, false, false, false},
		{"constrained", "Hazard.swift", "conformance", false, true, false, false},
		{"generic", "Hazard.swift", "conformance", true, false, false, false},
		{"production test-only", "Tests/Hazard.swift", "conformance", false, false, false, true},
		{"test test-only", "Tests/Hazard.swift", "conformance", false, false, true, false},
		{"test production", "Hazard.swift", "conformance", false, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwiftScopeFixture(t)
			var target, edge int64
			if tc.callerTest {
				_, _, target, _, edge = newSwiftNonFinalStaticSelfFixtureInFile(t, f, f.file("Tests/Service.swift"), "Service", "", "static", false)
			} else {
				_, _, target, _, edge = newSwiftNonFinalStaticSelfFixtureInFile(t, f, f.mainFile, "Service", "", "static", false)
			}
			f.resolve()
			assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeStaticMethodScope)
			relationFile := f.file(tc.relationFile)
			f.relation(relationFile, "Service", "P", tc.relationKind, tc.generic, tc.constrained)
			if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{tc.relationFile}); err != nil {
				t.Fatal(err)
			}
			if tc.wantBound {
				assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeStaticMethodScope)
			} else {
				assertSwiftEdgeUnresolved(t, f, edge)
			}
		})
	}
}

func TestSwiftClassSelfTypeStaticMethodScopeNonFinalBlockers(t *testing.T) {
	for _, tc := range []struct {
		name, kind                                 string
		callerTest, blockerTest, static, wantBound bool
	}{
		{"production static", graph.ScopeImportSwiftMemberValue, false, false, true, false},
		{"production test-only static", graph.ScopeImportSwiftMemberValue, false, true, true, true},
		{"test test-only static", graph.ScopeImportSwiftMemberValue, true, true, true, false},
		{"opposite instance", graph.ScopeImportSwiftMemberValue, false, false, false, true},
		{"static enum case", graph.ScopeImportSwiftEnumCase, false, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwiftScopeFixture(t)
			var target, edge int64
			if tc.callerTest {
				_, _, target, _, edge = newSwiftNonFinalStaticSelfFixtureInFile(t, f, f.file("Tests/Service.swift"), "Service", "", "static", false)
			} else {
				_, _, target, _, edge = newSwiftNonFinalStaticSelfFixtureInFile(t, f, f.mainFile, "Service", "", "static", false)
			}
			f.resolve()
			assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeStaticMethodScope)
			blockFile := f.mainFile
			if tc.blockerTest {
				blockFile = f.file("Tests/Blocker.swift")
			}
			f.blocker(blockFile, "Service", "make", tc.kind, tc.static)
			blockPath := "Service.swift"
			if tc.blockerTest {
				blockPath = "Tests/Blocker.swift"
			}
			if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{blockPath}); err != nil {
				t.Fatal(err)
			}
			if tc.wantBound {
				assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeStaticMethodScope)
			} else {
				assertSwiftEdgeUnresolved(t, f, edge)
			}
		})
	}
}

func TestSwiftClassSelfTypeStaticMethodScopeNonFinalIdentityAndFiles(t *testing.T) {
	t.Run("reverse subclass and qualified owner", func(t *testing.T) {
		f, owner, target, _, edge := newSwiftNonFinalStaticSelfFixture(t, "Service", "Outer", "static", false)
		f.relation(f.mainFile, "Child", "Outer.Service", "superclass", false, false)
		f.relation(f.mainFile, "Service", "P", "conformance", false, false)
		f.blocker(f.mainFile, "Service", "make", graph.ScopeImportSwiftMemberValue, true)
		_ = owner
		f.resolve()
		assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeStaticMethodScope)
	})
	t.Run("cross file refused", func(t *testing.T) {
		f, owner, _, caller, edge := newSwiftNonFinalStaticSelfFixture(t, "Service", "", "", false)
		other := f.file("Extension.swift")
		target := f.symbol(other, "make", "Service", "function", "make()", true)
		f.dispatchFact(other, target, false, "static")
		_ = owner
		_ = caller
		f.resolve()
		assertSwiftEdgeUnresolved(t, f, edge)
	})
	t.Run("same file extension representation", func(t *testing.T) {
		f, _, target, _, edge := newSwiftNonFinalStaticSelfFixture(t, "Service", "", "static", false)
		f.resolve()
		assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeStaticMethodScope)
	})
}

func TestSwiftClassSelfTypeStaticMethodScopeNonFinalSelectorsTrailingAndAmbiguity(t *testing.T) {
	for _, tc := range []struct {
		name, evidence, signature string
		arity                     int
	}{
		{"zero", "swift:Self", "make()", 0},
		{"underscore", "swift:Self;labels=_", "make(_:)", 1},
		{"named", "swift:Self;labels=id:", "make(id:)", 1},
		{"two labels", "swift:Self;labels=id:,cache:", "make(id:,cache:)", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, target, caller, _ := newSwiftNonFinalStaticSelfFixture(t, "Service", "", "static", false)
			if tc.signature != "make()" {
				if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET signature=? WHERE id=?`, tc.signature, target); err != nil {
					t.Fatal(err)
				}
				f.arity(target, int64(tc.arity), int64(tc.arity))
			}
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET dst_name=?,evidence=?,call_arity=? WHERE src_symbol_id=?`, "Self.make", tc.evidence, tc.arity, caller); err != nil {
				t.Fatal(err)
			}
			f.resolve()
			var edge int64
			if err := f.store.db.QueryRowContext(f.ctx, `SELECT id FROM edges WHERE src_symbol_id=?`, caller).Scan(&edge); err != nil {
				t.Fatal(err)
			}
			assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeStaticMethodScope)
		})
	}
	f, _, perform, _, edge := newSwiftNonFinalStaticSelfFixture(t, "Service", "", "static", false)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET name='perform',signature='perform(_:)',arity_min=1,arity_max=1 WHERE id=?`, perform); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET dst_name='Self.perform',evidence='swift:Self;trailing_labels=_',call_arity=1 WHERE id=?`, edge); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE references_tbl SET name='Self.perform',qualified_name='Self.perform' WHERE repo_id=?`, f.repoID); err != nil {
		t.Fatal(err)
	}
	f.resolve()
	assertSwiftBinding(t, f, edge, perform, ResolutionStrategySwiftClassSelfTypeStaticMethodScope)
	t.Run("ambiguous", func(t *testing.T) {
		f, _, _, _, edge := newSwiftNonFinalStaticSelfFixture(t, "Service", "", "static", false)
		duplicate := f.symbol(f.mainFile, "make", "Service", "function", "make()", true)
		f.dispatchFact(f.mainFile, duplicate, false, "static")
		f.resolve()
		assertSwiftEdgeUnresolved(t, f, edge)
	})
}

func TestSwiftClassSelfTypeStaticMethodScopeNamesAndValueTypeRegression(t *testing.T) {
	f, _, target, _, edge := newSwiftNonFinalStaticSelfFixture(t, "Service", "", "static", false)
	stats := f.resolveNames("make")
	assertSwiftStats(t, stats, 1, 1, 0, 0)
	assertSwiftEdgeMetadata(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeStaticMethodScope)
	if err := f.store.ReconcileReferenceIdentities(f.ctx, f.repoID); err != nil {
		t.Fatal(err)
	}
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeStaticMethodScope)
	for _, kind := range []string{"struct", "enum"} {
		t.Run(kind, func(t *testing.T) {
			f := newSwiftScopeFixture(t)
			owner := f.symbol(f.mainFile, "Value", "", kind, "", false)
			target := f.symbol(f.mainFile, "make", "Value", "function", "make()", true)
			caller := f.symbol(f.mainFile, "f", "Value", "function", "f()", false)
			edge := f.call(f.mainFile, caller, "Self.make", "swift:Self", 0, 1)
			f.reference(f.mainFile, caller, "Self.make", 1)
			f.declarationFact(f.mainFile, owner, false)
			f.dispatchFact(f.mainFile, target, false, "static")
			f.resolve()
			assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftSelfTypeScope)
		})
	}
}

func TestSwiftClassSelfTypeStaticMethodScopeNegativeNamesStats(t *testing.T) {
	f, _, target, _, edge := newSwiftNonFinalStaticSelfFixture(t, "Service", "", "class", false)
	_ = target
	stats := f.resolveNames("make")
	assertSwiftStats(t, stats, 1, 0, 1, 0)
	assertSwiftEdgeUnresolved(t, f, edge)
}

func TestSwiftClassSelfTypeStaticMethodScopeCrossFileHazardLifecycle(t *testing.T) {
	f, _, target, _, edge := newSwiftNonFinalStaticSelfFixture(t, "Service", "", "static", false)
	hazard := f.file("Conformance.swift")
	f.resolve()
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeStaticMethodScope)
	f.relation(hazard, "Service", "P", "conformance", false, false)
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Conformance.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftEdgeUnresolved(t, f, edge)
	if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM swift_inheritance_relations WHERE file_id=?`, hazard); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Conformance.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeStaticMethodScope)
	f.relation(hazard, "Service", "P", "conformance", false, false)
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Conformance.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftEdgeUnresolved(t, f, edge)
}

func TestSwiftClassSelfTypeStaticMethodScopeUpgradeRepair(t *testing.T) {
	f, _, target, _, edge := newSwiftNonFinalStaticSelfFixture(t, "Service", "", "static", false)
	for _, repair := range resolverRepairs {
		if repair.key == swiftClassSelfTypeStaticMethodRepairSettingKey {
			continue
		}
		if err := f.store.markRepairDone(f.ctx, repair.key, f.repoID); err != nil {
			t.Fatal(err)
		}
	}
	assertSwiftEdgeUnresolved(t, f, edge)
	run, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID)
	if err != nil || !run {
		t.Fatalf("repair=(%v,%v)", run, err)
	}
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeStaticMethodScope)
	var marker string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT value FROM settings WHERE key=?`, swiftClassSelfTypeStaticMethodRepairSettingKey+fmt.Sprintf(".%d", f.repoID)).Scan(&marker); err != nil || marker != "1" {
		t.Fatalf("marker=%q err=%v", marker, err)
	}
	run, err = f.store.RepairResolverBindingsOnce(f.ctx, f.repoID)
	if err != nil || run {
		t.Fatalf("second repair=(%v,%v)", run, err)
	}
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeStaticMethodScope)
}

func TestSwiftClassSelfTypeStaticMethodScopeUnsafeExistingBindingRepair(t *testing.T) {
	f, _, target, _, edge := newSwiftNonFinalStaticSelfFixture(t, "Service", "", "static", false)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET dst_symbol_id=?,resolution_strategy=?,resolution_confidence=? WHERE id=?`, target, ResolutionStrategySwiftClassSelfTypeStaticMethodScope, ResolutionConfidenceHigh, edge); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE references_tbl SET symbol_id=? WHERE repo_id=?`, target, f.repoID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET dispatch_kind='class' WHERE symbol_id=?`, target); err != nil {
		t.Fatal(err)
	}
	for _, repair := range resolverRepairs {
		if repair.key == swiftClassSelfTypeStaticMethodRepairSettingKey {
			continue
		}
		if err := f.store.markRepairDone(f.ctx, repair.key, f.repoID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID); err != nil {
		t.Fatal(err)
	}
	assertSwiftEdgeUnresolved(t, f, edge)
}

func TestSwiftClassSelfStaticMethodScopeFailsClosed(t *testing.T) {
	cases := []struct {
		name, sourceDispatch, targetDispatch string
		duplicateSource, duplicateTarget     bool
		sourceFact, targetFact               bool
		wantBound                            bool
	}{
		{"missing source", "", "static", false, false, false, true, false},
		{"duplicate source", "static", "static", true, false, true, true, false},
		{"malformed source", "", "static", false, false, true, true, false},
		{"instance source", "instance", "static", false, false, true, true, false},
		{"missing target", "static", "", false, false, true, false, false},
		{"duplicate target", "static", "static", false, true, true, true, false},
		{"malformed target", "static", "", false, false, true, true, false},
		{"class target", "static", "class", false, false, true, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwiftScopeFixture(t)
			owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
			target := f.symbol(f.mainFile, "make", "Service", "function", "make()", true)
			caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", true)
			edge := f.call(f.mainFile, caller, "self.make", "swift:self", 0, 1)
			f.declarationFact(f.mainFile, owner, false)
			if tc.sourceFact {
				f.dispatchFact(f.mainFile, caller, false, tc.sourceDispatch)
				if tc.duplicateSource {
					f.dispatchFact(f.mainFile, caller, false, tc.sourceDispatch)
				}
			}
			if tc.targetFact {
				f.dispatchFact(f.mainFile, target, false, tc.targetDispatch)
				if tc.duplicateTarget {
					f.dispatchFact(f.mainFile, target, false, tc.targetDispatch)
				}
			}
			f.resolve()
			if tc.wantBound {
				assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfStaticMethodScope)
			} else {
				assertSwiftEdgeUnresolved(t, f, edge)
			}
		})
	}
}

func TestSwiftClassSelfFinalClassMethodScope(t *testing.T) {
	for _, tc := range []struct {
		name, dispatch   string
		final, duplicate bool
		wantStrategy     string
		wantBound        bool
	}{
		{"final class", "class", true, false, ResolutionStrategySwiftClassSelfFinalClassMethodScope, true},
		{"ordinary class", "class", false, false, "", false},
		{"static target keeps P22.71", "static", false, false, ResolutionStrategySwiftClassSelfStaticMethodScope, true},
		{"final static target keeps P22.71", "static", true, false, ResolutionStrategySwiftClassSelfStaticMethodScope, true},
		{"instance target", "instance", true, false, "", false},
		{"missing fact", "", false, false, "", false},
		{"duplicate class fact", "class", true, true, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwiftScopeFixture(t)
			owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
			target := f.symbol(f.mainFile, "make", "Service", "function", "make()", true)
			caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", true)
			edge := f.call(f.mainFile, caller, "self.make", "swift:self", 0, 1)
			f.reference(f.mainFile, caller, "self.make", 1)
			f.declarationFact(f.mainFile, owner, false)
			f.dispatchFact(f.mainFile, caller, false, "class")
			if tc.dispatch != "" {
				f.dispatchFact(f.mainFile, target, tc.final, tc.dispatch)
				if tc.duplicate {
					f.dispatchFact(f.mainFile, target, tc.final, tc.dispatch)
				}
			}
			f.resolve()
			if tc.wantBound {
				assertSwiftBinding(t, f, edge, target, tc.wantStrategy)
			} else {
				assertSwiftEdgeUnresolved(t, f, edge)
			}
		})
	}
}

func TestSwiftClassSelfFinalClassMethodFinalOwnerPrecedence(t *testing.T) {
	f := newSwiftScopeFixture(t)
	owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
	target := f.symbol(f.mainFile, "make", "Service", "function", "make()", true)
	caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", true)
	edge := f.call(f.mainFile, caller, "self.make", "swift:self", 0, 1)
	f.reference(f.mainFile, caller, "self.make", 1)
	f.declarationFact(f.mainFile, owner, true)
	f.dispatchFact(f.mainFile, caller, false, "class")
	f.dispatchFact(f.mainFile, target, true, "class")
	f.resolve()
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfFinalScope)
}

func TestSwiftClassSelfFinalClassMethodHazards(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		generic, constrained bool
	}{
		{"superclass", false, false},
		{"conformance", false, false},
		{"unproven", false, false},
		{"constrained", false, true},
		{"generic", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwiftScopeFixture(t)
			owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
			target := f.symbol(f.mainFile, "make", "Service", "function", "make()", true)
			caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", true)
			edge := f.call(f.mainFile, caller, "self.make", "swift:self", 0, 1)
			f.reference(f.mainFile, caller, "self.make", 1)
			f.declarationFact(f.mainFile, owner, false)
			f.dispatchFact(f.mainFile, caller, false, "class")
			f.dispatchFact(f.mainFile, target, true, "class")
			relation := tc.name
			if tc.generic || tc.constrained {
				relation = "conformance"
			}
			f.relation(f.mainFile, "Service", "P", relation, tc.generic, tc.constrained)
			f.resolve()
			assertSwiftBindingCleared(t, f, edge)
		})
	}
}

func TestSwiftClassSelfFinalClassMethodHazardFiltering(t *testing.T) {
	for _, tc := range []struct {
		name, callerPath, hazardPath string
		wantBound                    bool
	}{
		{"production test", "Service.swift", "Tests/Hazard.swift", true},
		{"production production", "Service.swift", "Hazard.swift", false},
		{"test test", "Tests/Service.swift", "Tests/Hazard.swift", false},
		{"test production", "Tests/Service.swift", "Hazard.swift", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwiftScopeFixture(t)
			callerFile := f.mainFile
			if strings.HasPrefix(tc.callerPath, "Tests/") {
				callerFile = f.file(tc.callerPath)
			}
			owner := f.symbol(callerFile, "Service", "", "class", "", false)
			target := f.symbol(callerFile, "make", "Service", "function", "make()", true)
			caller := f.symbol(callerFile, "f", "Service", "function", "f()", true)
			edge := f.call(callerFile, caller, "self.make", "swift:self", 0, 1)
			f.reference(callerFile, caller, "self.make", 1)
			f.declarationFact(callerFile, owner, false)
			f.dispatchFact(callerFile, caller, false, "class")
			f.dispatchFact(callerFile, target, true, "class")
			f.relation(f.file(tc.hazardPath), "Service", "P", "conformance", false, false)
			f.resolve()
			if tc.wantBound {
				assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfFinalClassMethodScope)
			} else {
				assertSwiftBindingCleared(t, f, edge)
			}
		})
	}
}

func TestSwiftClassSelfFinalClassMethodBlockers(t *testing.T) {
	for _, tc := range []struct {
		name, kind        string
		callerTest        bool
		blockTest, static bool
		wantBound         bool
	}{
		{"production static", graph.ScopeImportSwiftMemberValue, false, false, true, false},
		{"production test-only static", graph.ScopeImportSwiftMemberValue, false, true, true, true},
		{"test static", graph.ScopeImportSwiftMemberValue, true, true, true, false},
		{"opposite instance", graph.ScopeImportSwiftMemberValue, false, false, false, true},
		{"static enum case", graph.ScopeImportSwiftEnumCase, false, false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwiftScopeFixture(t)
			callerFile := f.mainFile
			if tc.callerTest {
				callerFile = f.file("Tests/Service.swift")
			}
			owner := f.symbol(callerFile, "Service", "", "class", "", false)
			target := f.symbol(callerFile, "make", "Service", "function", "make()", true)
			caller := f.symbol(callerFile, "f", "Service", "function", "f()", true)
			edge := f.call(callerFile, caller, "self.make", "swift:self", 0, 1)
			f.reference(callerFile, caller, "self.make", 1)
			f.declarationFact(callerFile, owner, false)
			f.dispatchFact(callerFile, caller, false, "class")
			f.dispatchFact(callerFile, target, true, "class")
			blockFile := callerFile
			if tc.blockTest {
				blockFile = f.file("Tests/Blocker.swift")
			}
			f.blocker(blockFile, "Service", "make", tc.kind, tc.static)
			f.resolve()
			if tc.wantBound {
				assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfFinalClassMethodScope)
			} else {
				assertSwiftBindingCleared(t, f, edge)
			}
		})
	}
}

func TestSwiftClassSelfFinalClassMethodSubclassAndQualifiedOwner(t *testing.T) {
	f := newSwiftScopeFixture(t)
	owner := f.symbol(f.mainFile, "Service", "Outer", "class", "", false)
	target := f.symbol(f.mainFile, "make", "Outer.Service", "function", "make()", true)
	caller := f.symbol(f.mainFile, "f", "Outer.Service", "function", "f()", true)
	edge := f.call(f.mainFile, caller, "self.make", "swift:self", 0, 1)
	f.reference(f.mainFile, caller, "self.make", 1)
	f.declarationFact(f.mainFile, owner, false)
	f.dispatchFact(f.mainFile, caller, false, "class")
	f.dispatchFact(f.mainFile, target, true, "class")
	f.relation(f.mainFile, "Child", "Outer.Service", "superclass", false, false)
	f.relation(f.mainFile, "Service", "P", "conformance", false, false)
	f.blocker(f.mainFile, "Service", "make", graph.ScopeImportSwiftMemberValue, true)
	f.resolve()
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfFinalClassMethodScope)
}

func TestSwiftClassSelfFinalClassMethodSameAndCrossFile(t *testing.T) {
	t.Run("same file", func(t *testing.T) {
		f := newSwiftScopeFixture(t)
		owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
		target := f.symbol(f.mainFile, "make", "Service", "function", "make()", true)
		caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", true)
		edge := f.call(f.mainFile, caller, "self.make", "swift:self", 0, 1)
		f.reference(f.mainFile, caller, "self.make", 1)
		f.declarationFact(f.mainFile, owner, false)
		f.dispatchFact(f.mainFile, caller, false, "class")
		f.dispatchFact(f.mainFile, target, true, "class")
		f.resolve()
		assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfFinalClassMethodScope)
	})
	t.Run("cross file", func(t *testing.T) {
		f := newSwiftScopeFixture(t)
		owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
		caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", true)
		edge := f.call(f.mainFile, caller, "self.make", "swift:self", 0, 1)
		f.reference(f.mainFile, caller, "self.make", 1)
		f.declarationFact(f.mainFile, owner, false)
		f.dispatchFact(f.mainFile, caller, false, "class")
		other := f.file("Extension.swift")
		target := f.symbol(other, "make", "Service", "function", "make()", true)
		f.dispatchFact(other, target, true, "class")
		f.resolve()
		assertSwiftBindingCleared(t, f, edge)
	})
}

func TestSwiftClassSelfFinalClassMethodCrossFileHazardLifecycle(t *testing.T) {
	f := newSwiftScopeFixture(t)
	owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
	target := f.symbol(f.mainFile, "make", "Service", "function", "make()", true)
	caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", true)
	edge := f.call(f.mainFile, caller, "self.make", "swift:self", 0, 1)
	f.reference(f.mainFile, caller, "self.make", 1)
	f.declarationFact(f.mainFile, owner, false)
	f.dispatchFact(f.mainFile, caller, false, "class")
	f.dispatchFact(f.mainFile, target, true, "class")
	hazard := f.file("Conformance.swift")
	f.resolve()
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfFinalClassMethodScope)
	f.relation(hazard, "Service", "P", "conformance", false, false)
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Conformance.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftBindingCleared(t, f, edge)
	if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM swift_inheritance_relations WHERE file_id=?`, hazard); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Conformance.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfFinalClassMethodScope)
	f.relation(hazard, "Service", "P", "conformance", false, false)
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Conformance.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftBindingCleared(t, f, edge)
}

func TestSwiftClassSelfFinalClassMethodSelectorsTrailingAndAmbiguity(t *testing.T) {
	checks := []struct {
		name, evidence, signature string
		arity                     int
	}{
		{"zero", "swift:self", "make()", 0},
		{"underscore", "swift:self;labels=_", "make(_:)", 1},
		{"named", "swift:self;labels=id:", "make(id:)", 1},
		{"two labels", "swift:self;labels=id:,cache:", "make(id:,cache:)", 2},
	}
	for _, check := range checks {
		t.Run(check.name, func(t *testing.T) {
			f := newSwiftScopeFixture(t)
			owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
			caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", true)
			target := f.symbol(f.mainFile, "make", "Service", "function", check.signature, true)
			f.arity(target, int64(check.arity), int64(check.arity))
			f.declarationFact(f.mainFile, owner, false)
			f.dispatchFact(f.mainFile, caller, false, "class")
			f.dispatchFact(f.mainFile, target, true, "class")
			edge := f.call(f.mainFile, caller, "self.make", check.evidence, check.arity, 1)
			f.reference(f.mainFile, caller, "self.make", 1)
			f.resolve()
			assertSwiftEdgeMetadata(t, f, edge, target, ResolutionStrategySwiftClassSelfFinalClassMethodScope)
		})
	}
	f := newSwiftScopeFixture(t)
	owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
	caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", true)
	f.declarationFact(f.mainFile, owner, false)
	f.dispatchFact(f.mainFile, caller, false, "class")
	perform := f.symbol(f.mainFile, "perform", "Service", "function", "perform(_:)", true)
	f.arity(perform, 1, 1)
	f.dispatchFact(f.mainFile, perform, true, "class")
	trailing := f.call(f.mainFile, caller, "self.perform", "swift:self;trailing_labels=_", 1, 10)
	f.reference(f.mainFile, caller, "self.perform", 10)
	f.resolve()
	assertSwiftEdgeMetadata(t, f, trailing, perform, ResolutionStrategySwiftClassSelfFinalClassMethodScope)
	t.Run("ambiguous", func(t *testing.T) {
		f := newSwiftScopeFixture(t)
		owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
		caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", true)
		ambiguousA := f.symbol(f.mainFile, "ambiguous", "Service", "function", "ambiguous()", true)
		ambiguousB := f.symbol(f.mainFile, "ambiguous", "Service", "function", "ambiguous()", true)
		f.declarationFact(f.mainFile, owner, false)
		f.dispatchFact(f.mainFile, caller, false, "class")
		f.dispatchFact(f.mainFile, ambiguousA, true, "class")
		f.dispatchFact(f.mainFile, ambiguousB, true, "class")
		ambiguous := f.call(f.mainFile, caller, "self.ambiguous", "swift:self", 0, 20)
		f.reference(f.mainFile, caller, "self.ambiguous", 20)
		f.resolve()
		assertSwiftBindingCleared(t, f, ambiguous)
	})
}

func TestSwiftClassSelfFinalClassMethodLifecycle(t *testing.T) {
	f := newSwiftScopeFixture(t)
	owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
	target := f.symbol(f.mainFile, "make", "Service", "function", "make()", true)
	caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", true)
	edge := f.call(f.mainFile, caller, "self.make", "swift:self", 0, 1)
	f.reference(f.mainFile, caller, "self.make", 1)
	f.declarationFact(f.mainFile, owner, false)
	f.dispatchFact(f.mainFile, caller, false, "class")
	f.dispatchFact(f.mainFile, target, true, "class")
	f.resolve()
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfFinalClassMethodScope)

	update := func(sqlText string, args ...any) {
		t.Helper()
		if _, err := f.store.db.ExecContext(f.ctx, sqlText, args...); err != nil {
			t.Fatal(err)
		}
		if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
			t.Fatal(err)
		}
	}
	update(`UPDATE swift_declaration_facts SET is_final=0 WHERE symbol_id=?`, target)
	assertSwiftEdgeUnresolved(t, f, edge)
	update(`UPDATE swift_declaration_facts SET is_final=1 WHERE symbol_id=?`, target)
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfFinalClassMethodScope)
	update(`UPDATE swift_declaration_facts SET dispatch_kind='static' WHERE symbol_id=?`, target)
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfStaticMethodScope)
	update(`UPDATE swift_declaration_facts SET dispatch_kind='class' WHERE symbol_id=?`, target)
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfFinalClassMethodScope)
	update(`UPDATE swift_declaration_facts SET dispatch_kind='instance' WHERE symbol_id=?`, caller)
	assertSwiftEdgeUnresolved(t, f, edge)
	update(`UPDATE swift_declaration_facts SET dispatch_kind='class' WHERE symbol_id=?`, caller)
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfFinalClassMethodScope)
}

func TestSwiftClassSelfFinalClassMethodNames(t *testing.T) {
	for _, final := range []bool{true, false} {
		f := newSwiftScopeFixture(t)
		owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
		target := f.symbol(f.mainFile, "make", "Service", "function", "make()", true)
		caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", true)
		edge := f.call(f.mainFile, caller, "self.make", "swift:self", 0, 1)
		f.declarationFact(f.mainFile, owner, false)
		f.dispatchFact(f.mainFile, caller, false, "class")
		f.dispatchFact(f.mainFile, target, final, "class")
		stats := f.resolveNames("make")
		assertSwiftStats(t, stats, 1, btoi(final), btoi(!final), 0)
		if final {
			assertSwiftEdgeMetadata(t, f, edge, target, ResolutionStrategySwiftClassSelfFinalClassMethodScope)
		} else {
			assertSwiftEdgeUnresolved(t, f, edge)
		}
	}
}

func TestSwiftClassSelfFinalClassMethodRepair(t *testing.T) {
	f := newSwiftScopeFixture(t)
	owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
	target := f.symbol(f.mainFile, "make", "Service", "function", "make()", true)
	caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", true)
	edge := f.call(f.mainFile, caller, "self.make", "swift:self", 0, 1)
	f.reference(f.mainFile, caller, "self.make", 1)
	f.declarationFact(f.mainFile, owner, false)
	f.dispatchFact(f.mainFile, caller, false, "class")
	f.dispatchFact(f.mainFile, target, true, "class")
	for _, repair := range resolverRepairs {
		if repair.key == swiftClassSelfFinalClassMethodRepairSettingKey {
			continue
		}
		if err := f.store.markRepairDone(f.ctx, repair.key, f.repoID); err != nil {
			t.Fatal(err)
		}
	}
	run, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID)
	if err != nil || !run {
		t.Fatalf("repair=(%v,%v)", run, err)
	}
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfFinalClassMethodScope)
	var marker string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT value FROM settings WHERE key=?`, swiftClassSelfFinalClassMethodRepairSettingKey+fmt.Sprintf(".%d", f.repoID)).Scan(&marker); err != nil || marker != "1" {
		t.Fatalf("marker=%q err=%v", marker, err)
	}
	run, err = f.store.RepairResolverBindingsOnce(f.ctx, f.repoID)
	if err != nil || run {
		t.Fatalf("second repair=(%v,%v)", run, err)
	}
}

func TestSwiftClassSelfFinalClassMethodRepairClearsUnsafeExistingBinding(t *testing.T) {
	f := newSwiftScopeFixture(t)
	owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
	target := f.symbol(f.mainFile, "make", "Service", "function", "make()", true)
	caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", true)
	edge := f.call(f.mainFile, caller, "self.make", "swift:self", 0, 1)
	f.reference(f.mainFile, caller, "self.make", 1)
	f.declarationFact(f.mainFile, owner, false)
	f.dispatchFact(f.mainFile, caller, false, "class")
	f.dispatchFact(f.mainFile, target, false, "class")
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET dst_symbol_id=?,resolution_strategy=?,resolution_confidence=? WHERE id=?`, target, ResolutionStrategySwiftClassSelfFinalClassMethodScope, ResolutionConfidenceHigh, edge); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET is_final=0 WHERE symbol_id=?`, target); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM settings WHERE key=?`, swiftClassSelfFinalClassMethodRepairSettingKey+fmt.Sprintf(".%d", f.repoID)); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID); err != nil {
		t.Fatal(err)
	}
	assertSwiftBindingCleared(t, f, edge)
}

func TestSwiftClassSelfStaticMethodFinalPrecedenceAndLifecycle(t *testing.T) {
	f := newSwiftScopeFixture(t)
	owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
	target := f.symbol(f.mainFile, "make", "Service", "function", "make()", true)
	caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", true)
	edge := f.call(f.mainFile, caller, "self.make", "swift:self", 0, 1)
	f.reference(f.mainFile, caller, "self.make", 1)
	f.declarationFact(f.mainFile, owner, true)
	f.dispatchFact(f.mainFile, caller, false, "static")
	f.dispatchFact(f.mainFile, target, false, "static")
	f.resolve()
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfFinalScope)

}

func TestSwiftClassSelfStaticMethodUpgradeRepair(t *testing.T) {
	f := newSwiftScopeFixture(t)
	owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
	target := f.symbol(f.mainFile, "make", "Service", "function", "make()", true)
	caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", true)
	edge := f.call(f.mainFile, caller, "self.make", "swift:self", 0, 1)
	f.reference(f.mainFile, caller, "self.make", 1)
	f.declarationFact(f.mainFile, owner, false)
	f.dispatchFact(f.mainFile, caller, false, "class")
	f.dispatchFact(f.mainFile, target, false, "static")
	for _, repair := range resolverRepairs {
		if repair.key == swiftClassSelfStaticMethodRepairSettingKey {
			continue
		}
		if err := f.store.markRepairDone(f.ctx, repair.key, f.repoID); err != nil {
			t.Fatal(err)
		}
	}
	ran, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID)
	if err != nil || !ran {
		t.Fatalf("repair=(%v,%v)", ran, err)
	}
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfStaticMethodScope)
	var marker string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT value FROM settings WHERE key=?`, swiftClassSelfStaticMethodRepairSettingKey+fmt.Sprintf(".%d", f.repoID)).Scan(&marker); err != nil || marker != "1" {
		t.Fatalf("new repair marker=%q err=%v", marker, err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET dispatch_kind='class' WHERE symbol_id=?`, target); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftBindingCleared(t, f, edge)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET dispatch_kind='static' WHERE symbol_id=?`, target); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfStaticMethodScope)
	ran, err = f.store.RepairResolverBindingsOnce(f.ctx, f.repoID)
	if err != nil || ran {
		t.Fatalf("second repair=(%v,%v)", ran, err)
	}
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfStaticMethodScope)
}

func TestSwiftClassSelfStaticMethodRepairClearsUnsafeExistingBinding(t *testing.T) {
	f := newSwiftScopeFixture(t)
	owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
	target := f.symbol(f.mainFile, "make", "Service", "function", "make()", true)
	caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", true)
	edge := f.call(f.mainFile, caller, "self.make", "swift:self", 0, 1)
	f.reference(f.mainFile, caller, "self.make", 1)
	f.declarationFact(f.mainFile, owner, false)
	f.dispatchFact(f.mainFile, caller, false, "class")
	f.dispatchFact(f.mainFile, target, false, "class")
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET dst_symbol_id=?,resolution_strategy=?,resolution_confidence=? WHERE id=?`, target, ResolutionStrategySwiftClassSelfStaticMethodScope, ResolutionConfidenceHigh, edge); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE references_tbl SET symbol_id=? WHERE repo_id=?`, target, f.repoID); err != nil {
		t.Fatal(err)
	}
	for _, repair := range resolverRepairs {
		if repair.key == swiftClassSelfStaticMethodRepairSettingKey {
			continue
		}
		if err := f.store.markRepairDone(f.ctx, repair.key, f.repoID); err != nil {
			t.Fatal(err)
		}
	}
	if ran, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID); err != nil || !ran {
		t.Fatalf("repair=(%v,%v)", ran, err)
	}
	assertSwiftBindingCleared(t, f, edge)
}

func TestSwiftClassSelfStaticMethodRejectsActualInstanceCaller(t *testing.T) {
	f := newSwiftScopeFixture(t)
	owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
	target := f.symbol(f.mainFile, "make", "Service", "function", "make()", true)
	caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", false)
	edge := f.call(f.mainFile, caller, "self.make", "swift:self", 0, 1)
	f.declarationFact(f.mainFile, owner, false)
	f.dispatchFact(f.mainFile, caller, false, "instance")
	f.dispatchFact(f.mainFile, target, false, "static")
	f.resolve()
	assertSwiftEdgeUnresolved(t, f, edge)
}

func TestSwiftClassSelfStaticMethodSourceDispatchLifecycle(t *testing.T) {
	for _, valid := range []string{"static", "class"} {
		t.Run(valid, func(t *testing.T) {
			f := newSwiftScopeFixture(t)
			owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
			target := f.symbol(f.mainFile, "make", "Service", "function", "make()", true)
			caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", true)
			edge := f.call(f.mainFile, caller, "self.make", "swift:self", 0, 1)
			f.reference(f.mainFile, caller, "self.make", 1)
			f.declarationFact(f.mainFile, owner, false)
			f.dispatchFact(f.mainFile, caller, false, valid)
			f.dispatchFact(f.mainFile, target, false, "static")
			f.resolve()
			assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfStaticMethodScope)

			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET dispatch_kind='instance' WHERE symbol_id=?`, caller); err != nil {
				t.Fatal(err)
			}
			if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
				t.Fatal(err)
			}
			assertSwiftBindingCleared(t, f, edge)

			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET dispatch_kind=? WHERE symbol_id=?`, valid, caller); err != nil {
				t.Fatal(err)
			}
			if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
				t.Fatal(err)
			}
			assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfStaticMethodScope)
		})
	}
}

func TestSwiftClassSelfStaticMethodTargetDispatchLifecycle(t *testing.T) {
	f := newSwiftScopeFixture(t)
	owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
	target := f.symbol(f.mainFile, "make", "Service", "function", "make()", true)
	caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", true)
	edge := f.call(f.mainFile, caller, "self.make", "swift:self", 0, 1)
	f.reference(f.mainFile, caller, "self.make", 1)
	f.declarationFact(f.mainFile, owner, false)
	f.dispatchFact(f.mainFile, caller, false, "class")
	f.dispatchFact(f.mainFile, target, false, "static")
	f.resolve()
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfStaticMethodScope)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET dispatch_kind='class' WHERE symbol_id=?`, target); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftBindingCleared(t, f, edge)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET dispatch_kind='static' WHERE symbol_id=?`, target); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfStaticMethodScope)
}

func TestSwiftClassSelfStaticMethodCrossFileHazardLifecycle(t *testing.T) {
	f := newSwiftScopeFixture(t)
	owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
	target := f.symbol(f.mainFile, "make", "Service", "function", "make()", true)
	caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", true)
	edge := f.call(f.mainFile, caller, "self.make", "swift:self", 0, 1)
	f.reference(f.mainFile, caller, "self.make", 1)
	f.declarationFact(f.mainFile, owner, false)
	f.dispatchFact(f.mainFile, caller, false, "class")
	f.dispatchFact(f.mainFile, target, false, "static")
	hazard := f.file("Conformance.swift")
	f.resolve()
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfStaticMethodScope)
	f.relation(hazard, "Service", "P", "conformance", false, false)
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Conformance.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftBindingCleared(t, f, edge)
	if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM swift_inheritance_relations WHERE file_id=?`, hazard); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Conformance.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfStaticMethodScope)
	f.relation(hazard, "Service", "P", "conformance", false, false)
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Conformance.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftBindingCleared(t, f, edge)
}

func TestSwiftClassSelfStaticMethodHazardMatrix(t *testing.T) {
	for _, kind := range []string{"superclass", "conformance", "unproven"} {
		for _, flags := range [][2]bool{{false, false}, {true, false}, {false, true}} {
			t.Run(fmt.Sprintf("%s-generic=%t-constrained=%t", kind, flags[0], flags[1]), func(t *testing.T) {
				f := newSwiftScopeFixture(t)
				owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
				target := f.symbol(f.mainFile, "make", "Service", "function", "make()", true)
				caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", true)
				edge := f.call(f.mainFile, caller, "self.make", "swift:self", 0, 1)
				f.declarationFact(f.mainFile, owner, false)
				f.dispatchFact(f.mainFile, caller, false, "class")
				f.dispatchFact(f.mainFile, target, false, "static")
				f.relation(f.mainFile, "Service", "P", kind, flags[0], flags[1])
				f.resolve()
				assertSwiftEdgeUnresolved(t, f, edge)
			})
		}
	}
}

func TestSwiftClassSelfStaticMethodHazardAndBlockerFiltering(t *testing.T) {
	for _, tc := range []struct {
		name, callerPath, hazardPath                          string
		hazard, blocker, blockerTest, blockerStatic, enumCase bool
		wantBound                                             bool
	}{
		{"production test hazard", "Service.swift", "Tests/Hazard.swift", true, false, false, false, false, true},
		{"production production hazard", "Service.swift", "Hazard.swift", true, false, false, false, false, false},
		{"test test hazard", "Tests/Service.swift", "Tests/Hazard.swift", true, false, false, false, false, false},
		{"test production hazard", "Tests/Service.swift", "Hazard.swift", true, false, false, false, false, false},
		{"production static blocker", "Service.swift", "", false, true, false, true, false, false},
		{"production test static blocker", "Service.swift", "", false, true, true, true, false, true},
		{"test test static blocker", "Tests/Service.swift", "", false, true, true, true, false, false},
		{"opposite instance blocker", "Service.swift", "", false, true, false, false, false, true},
		{"static enum case blocker", "Service.swift", "", false, true, false, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwiftScopeFixture(t)
			callerFile := f.mainFile
			if strings.HasPrefix(tc.callerPath, "Tests/") {
				callerFile = f.file(tc.callerPath)
			}
			owner := f.symbol(callerFile, "Service", "", "class", "", false)
			target := f.symbol(callerFile, "make", "Service", "function", "make()", true)
			caller := f.symbol(callerFile, "f", "Service", "function", "f()", true)
			edge := f.call(callerFile, caller, "self.make", "swift:self", 0, 1)
			f.reference(callerFile, caller, "self.make", 1)
			f.declarationFact(callerFile, owner, false)
			f.dispatchFact(callerFile, caller, false, "class")
			f.dispatchFact(callerFile, target, false, "static")
			if tc.hazard {
				f.relation(f.file(tc.hazardPath), "Service", "P", "conformance", false, false)
			}
			if tc.blocker {
				blockFile := callerFile
				if tc.blockerTest {
					blockFile = f.file("Tests/Blocker.swift")
				}
				kind := graph.ScopeImportSwiftMemberValue
				if tc.enumCase {
					kind = graph.ScopeImportSwiftEnumCase
				}
				f.blocker(blockFile, "Service", "make", kind, tc.blockerStatic)
			}
			f.resolve()
			if tc.wantBound {
				assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfStaticMethodScope)
			} else {
				assertSwiftEdgeUnresolved(t, f, edge)
			}
		})
	}
}

func TestSwiftClassSelfStaticMethodReverseSubclassAndQualifiedOwner(t *testing.T) {
	f := newSwiftScopeFixture(t)
	owner := f.symbol(f.mainFile, "Service", "Outer", "class", "", false)
	target := f.symbol(f.mainFile, "make", "Outer.Service", "function", "make()", true)
	caller := f.symbol(f.mainFile, "f", "Outer.Service", "function", "f()", true)
	edge := f.call(f.mainFile, caller, "self.make", "swift:self", 0, 1)
	f.declarationFact(f.mainFile, owner, false)
	f.dispatchFact(f.mainFile, caller, false, "class")
	f.dispatchFact(f.mainFile, target, false, "static")
	f.relation(f.mainFile, "Child", "Outer.Service", "superclass", false, false)
	f.relation(f.mainFile, "Service", "P", "conformance", false, false)
	f.blocker(f.mainFile, "Service", "make", graph.ScopeImportSwiftMemberValue, true)
	f.reference(f.mainFile, caller, "self.make", 1)
	f.resolve()
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfStaticMethodScope)
}

func TestSwiftClassSelfStaticMethodSameFileAndCrossFileDestinations(t *testing.T) {
	t.Run("same-file extension shape", func(t *testing.T) {
		f := newSwiftScopeFixture(t)
		owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
		target := f.symbol(f.mainFile, "make", "Service", "function", "make()", true)
		caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", true)
		edge := f.call(f.mainFile, caller, "self.make", "swift:self", 0, 1)
		f.reference(f.mainFile, caller, "self.make", 1)
		f.declarationFact(f.mainFile, owner, false)
		f.dispatchFact(f.mainFile, caller, false, "class")
		f.dispatchFact(f.mainFile, target, false, "static")
		f.resolve()
		assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfStaticMethodScope)
	})
	t.Run("cross-file extension destination", func(t *testing.T) {
		f := newSwiftScopeFixture(t)
		owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
		caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", true)
		edge := f.call(f.mainFile, caller, "self.make", "swift:self", 0, 1)
		f.declarationFact(f.mainFile, owner, false)
		f.dispatchFact(f.mainFile, caller, false, "class")
		other := f.file("Extension.swift")
		target := f.symbol(other, "make", "Service", "function", "make()", true)
		f.dispatchFact(other, target, false, "static")
		f.resolve()
		assertSwiftEdgeUnresolved(t, f, edge)
	})
}

func TestSwiftClassSelfStaticMethodSelectorsTrailingAndAmbiguity(t *testing.T) {
	f := newSwiftScopeFixture(t)
	owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
	caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", true)
	make0 := f.symbol(f.mainFile, "make", "Service", "function", "make()", true)
	makeID := f.symbol(f.mainFile, "make", "Service", "function", "make(id:)", true)
	makeUnder := f.symbol(f.mainFile, "make", "Service", "function", "make(_:)", true)
	perform := f.symbol(f.mainFile, "perform", "Service", "function", "perform(_:)", true)
	for _, target := range []int64{make0, makeID, makeUnder, perform} {
		f.dispatchFact(f.mainFile, target, false, "static")
	}
	f.arity(makeID, 1, 1)
	f.arity(makeUnder, 1, 1)
	f.arity(perform, 1, 1)
	f.declarationFact(f.mainFile, owner, false)
	f.dispatchFact(f.mainFile, caller, false, "class")
	checks := []struct {
		name, evidence, dst string
		arity               int
		target              int64
	}{
		{"zero", "swift:self", "self.make", 0, make0},
		{"named", "swift:self;labels=id:", "self.make", 1, makeID},
		{"underscore", "swift:self;labels=_", "self.make", 1, makeUnder},
		{"trailing", "swift:self;trailing_labels=_", "self.perform", 1, perform},
	}
	for i, check := range checks {
		edge := f.call(f.mainFile, caller, check.dst, check.evidence, check.arity, i+1)
		f.resolve()
		assertSwiftEdgeMetadata(t, f, edge, check.target, ResolutionStrategySwiftClassSelfStaticMethodScope)
	}
	ambiguousA := f.symbol(f.mainFile, "ambiguous", "Service", "function", "ambiguous()", true)
	ambiguousB := f.symbol(f.mainFile, "ambiguous", "Service", "function", "ambiguous()", true)
	f.dispatchFact(f.mainFile, ambiguousA, false, "static")
	f.dispatchFact(f.mainFile, ambiguousB, false, "static")
	ambiguous := f.call(f.mainFile, caller, "self.ambiguous", "swift:self", 0, 20)
	f.resolve()
	assertSwiftEdgeUnresolved(t, f, ambiguous)
}

func TestSwiftClassSelfStaticMethodNamesStats(t *testing.T) {
	for _, tc := range []struct {
		name, targetDispatch string
		resolved             bool
	}{
		{"positive", "static", true},
		{"ordinary class target", "class", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwiftScopeFixture(t)
			owner := f.symbol(f.mainFile, "Service", "", "class", "", false)
			target := f.symbol(f.mainFile, "make", "Service", "function", "make()", true)
			caller := f.symbol(f.mainFile, "f", "Service", "function", "f()", true)
			edge := f.call(f.mainFile, caller, "self.make", "swift:self", 0, 1)
			f.declarationFact(f.mainFile, owner, false)
			f.dispatchFact(f.mainFile, caller, false, "class")
			f.dispatchFact(f.mainFile, target, false, tc.targetDispatch)
			stats := f.resolveNames("make")
			assertSwiftStats(t, stats, 1, btoi(tc.resolved), btoi(!tc.resolved), 0)
			if tc.resolved {
				assertSwiftEdgeMetadata(t, f, edge, target, ResolutionStrategySwiftClassSelfStaticMethodScope)
			} else {
				assertSwiftEdgeUnresolved(t, f, edge)
			}
		})
	}
}

func TestSwiftClassSelfStaticMethodMixedStress(t *testing.T) {
	f := newSwiftScopeFixture(t)
	type category struct {
		name, sourceDispatch, targetDispatch string
		count                                int
		ownerFinal                           bool
		callerStatic                         bool
		targetStatic                         bool
		want                                 int
	}
	categories := []category{
		{"final", "static", "static", 100, true, true, true, 100},
		{"static", "static", "static", 150, false, true, true, 150},
		{"class", "class", "static", 150, false, true, true, 150},
		{"final-class", "class", "class", 100, false, true, true, 100},
		{"final-class-superclass", "class", "class", 10, false, true, true, 0},
		{"final-class-conformance", "class", "class", 10, false, true, true, 0},
		{"final-class-unproven", "class", "class", 10, false, true, true, 0},
		{"final-class-same-blocker", "class", "class", 10, false, true, true, 0},
		{"final-class-opposite-blocker", "class", "class", 10, false, true, true, 10},
		{"final-class-duplicate", "class", "class", 10, false, true, true, 0},
		{"final-class-missing-final", "class", "class", 10, false, true, true, 0},
		{"qualified-final-class", "class", "class", 10, false, true, true, 10},
		{"instance-final", "instance", "instance", 100, false, false, false, 100},
		{"class-target", "class", "class", 100, false, true, true, 0},
		{"missing-source", "static", "static", 100, false, true, true, 0},
		{"duplicate-source", "static", "static", 100, false, true, true, 0},
		{"malformed-source", "", "static", 100, false, true, true, 0},
		{"missing-target", "static", "static", 100, false, true, true, 0},
		{"duplicate-target", "static", "static", 100, false, true, true, 0},
		{"superclass", "static", "static", 50, false, true, true, 0},
		{"conformance", "static", "static", 50, false, true, true, 0},
		{"unproven", "static", "static", 50, false, true, true, 0},
		{"qualified", "static", "static", 50, false, true, true, 50},
		{"same-blocker", "static", "static", 50, false, true, true, 0},
		{"opposite-blocker", "static", "static", 50, false, true, true, 50},
	}
	const total = 1580
	var edges []int64
	for _, tc := range categories {
		ownerName, ownerPrefix := "Service_"+tc.name, ""
		if tc.name == "qualified" {
			ownerName, ownerPrefix = "ServiceQualified", "Outer"
		} else if tc.name == "qualified-final-class" {
			ownerName, ownerPrefix = "ServiceFinalQualified", "Outer"
		}
		qualifiedOwner := ownerName
		if ownerPrefix != "" {
			qualifiedOwner = ownerPrefix + "." + ownerName
		}
		owner := f.symbol(f.mainFile, ownerName, ownerPrefix, "class", "", false)
		f.declarationFact(f.mainFile, owner, tc.ownerFinal)
		relation := ""
		switch tc.name {
		case "superclass", "conformance", "unproven":
			relation = tc.name
		case "final-class-superclass", "final-class-conformance", "final-class-unproven":
			relation = strings.TrimPrefix(tc.name, "final-class-")
		}
		if relation != "" {
			f.relation(f.mainFile, qualifiedOwner, "P", relation, false, false)
		}
		for i := 0; i < tc.count; i++ {
			safeName := strings.ReplaceAll(tc.name, "-", "_")
			name := fmt.Sprintf("make_%s_%d", safeName, i)
			target := f.symbol(f.mainFile, name, qualifiedOwner, "function", name+"()", tc.targetStatic)
			finalClass := tc.name == "final-class" || tc.name == "qualified-final-class" || strings.HasPrefix(tc.name, "final-class-")
			if tc.name != "missing-target" && tc.name != "final-class-missing-final" {
				f.dispatchFact(f.mainFile, target, tc.name == "instance-final" || finalClass, tc.targetDispatch)
			}
			if tc.name == "duplicate-target" {
				f.dispatchFact(f.mainFile, target, false, "static")
			}
			if tc.name == "final-class-duplicate" {
				f.dispatchFact(f.mainFile, target, true, "class")
			}
			caller := f.symbol(f.mainFile, fmt.Sprintf("f_%s_%d", safeName, i), qualifiedOwner, "function", "f()", tc.callerStatic)
			if tc.name != "missing-source" {
				f.dispatchFact(f.mainFile, caller, false, tc.sourceDispatch)
				if tc.name == "duplicate-source" {
					f.dispatchFact(f.mainFile, caller, false, tc.sourceDispatch)
				}
			}
			if tc.name == "malformed-source" {
				f.dispatchFact(f.mainFile, caller, false, "")
			}
			edges = append(edges, f.call(f.mainFile, caller, "self."+name, "swift:self", 0, len(edges)+1))
			if tc.name == "same-blocker" || tc.name == "final-class-same-blocker" {
				f.blocker(f.mainFile, qualifiedOwner, name, graph.ScopeImportSwiftMemberValue, true)
			}
			if tc.name == "opposite-blocker" || tc.name == "final-class-opposite-blocker" {
				f.blocker(f.mainFile, qualifiedOwner, name, graph.ScopeImportSwiftMemberValue, false)
			}
		}
	}
	if len(edges) != total {
		t.Fatalf("edges=%d, want %d", len(edges), total)
	}
	f.resolve()
	counts := func() [6]int {
		var result [6]int
		for _, query := range []struct {
			dst  *int
			sql  string
			args []any
		}{
			{&result[0], `SELECT COUNT(*) FROM edges WHERE repo_id=? AND evidence='swift:self' AND dst_symbol_id IS NOT NULL`, []any{f.repoID}},
			{&result[1], `SELECT COUNT(*) FROM edges WHERE repo_id=? AND evidence='swift:self' AND dst_symbol_id IS NULL`, []any{f.repoID}},
			{&result[2], `SELECT COUNT(*) FROM edges WHERE repo_id=? AND resolution_strategy=?`, []any{f.repoID, ResolutionStrategySwiftClassSelfFinalScope}},
			{&result[3], `SELECT COUNT(*) FROM edges WHERE repo_id=? AND resolution_strategy=?`, []any{f.repoID, ResolutionStrategySwiftClassSelfFinalMethodScope}},
			{&result[4], `SELECT COUNT(*) FROM edges WHERE repo_id=? AND resolution_strategy=?`, []any{f.repoID, ResolutionStrategySwiftClassSelfStaticMethodScope}},
			{&result[5], `SELECT COUNT(*) FROM edges WHERE repo_id=? AND resolution_strategy=?`, []any{f.repoID, ResolutionStrategySwiftClassSelfFinalClassMethodScope}},
		} {
			if err := f.store.db.QueryRowContext(f.ctx, query.sql, query.args...).Scan(query.dst); err != nil {
				t.Fatal(err)
			}
		}
		return result
	}
	first := counts()
	if first != [6]int{720, 860, 100, 100, 400, 120} {
		t.Fatalf("first counts=%v, want [720 860 100 100 400 120]", first)
	}
	f.resolve()
	second := counts()
	if second != first {
		t.Fatalf("second counts=%v, first=%v", second, first)
	}
}

func TestSwiftClassSelfBatchedMixedEdges(t *testing.T) {
	f := newSwiftScopeFixture(t)
	const perClass = 300
	specs := []struct {
		name, owner   string
		final, hazard bool
	}{
		{"Service0", "", true, false},
		{"Service1", "", false, false},
		{"Service2", "", true, true},
		{"Service", "Outer", true, false},
	}
	for classIndex, spec := range specs {
		ownerName := spec.name
		if spec.owner != "" {
			ownerName = spec.owner + "." + spec.name
		}
		owner := f.symbol(f.mainFile, spec.name, spec.owner, "class", "", false)
		f.declarationFact(f.mainFile, owner, spec.final)
		if spec.hazard {
			f.relation(f.mainFile, ownerName, "P", "unproven", false, false)
		}
		for i := 0; i < perClass; i++ {
			name := fmt.Sprintf("run%d_%d", classIndex, i)
			target := f.symbol(f.mainFile, name, ownerName, "function", name+"()", false)
			caller := f.symbol(f.mainFile, fmt.Sprintf("f%d_%d", classIndex, i), ownerName, "function", fmt.Sprintf("f%d_%d()", classIndex, i), false)
			f.call(f.mainFile, caller, "self."+name, "swift:self", 0, i+1)
			if classIndex == 0 && i == 0 {
				f.blocker(f.mainFile, ownerName, name, graph.ScopeImportSwiftMemberValue, true)
			}
			if classIndex == 1 && i == 0 {
				f.blocker(f.mainFile, ownerName, name, graph.ScopeImportSwiftMemberValue, false)
			}
			_ = target
		}
	}
	f.resolve()
	assertCounts := func() (int, int) {
		var bound, unresolved int
		if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM edges WHERE repo_id=? AND dst_symbol_id IS NOT NULL AND evidence='swift:self'`, f.repoID).Scan(&bound); err != nil {
			t.Fatal(err)
		}
		if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM edges WHERE repo_id=? AND dst_symbol_id IS NULL AND evidence='swift:self'`, f.repoID).Scan(&unresolved); err != nil {
			t.Fatal(err)
		}
		return bound, unresolved
	}
	if bound, unresolved := assertCounts(); bound != 2*perClass || unresolved != 2*perClass {
		t.Fatalf("counts=(%d,%d), want (%d,%d)", bound, unresolved, 2*perClass, 2*perClass)
	}
	beforeBound, beforeUnresolved := assertCounts()
	f.resolve()
	afterBound, afterUnresolved := assertCounts()
	if afterBound != beforeBound || afterUnresolved != beforeUnresolved {
		t.Fatalf("second resolve counts=(%d,%d), want (%d,%d)", afterBound, afterUnresolved, beforeBound, beforeUnresolved)
	}
}

func TestSwiftSelfScopeNegativeFactsStayUnresolved(t *testing.T) {
	f := newSwiftScopeFixture(t)
	f.symbol(f.mainFile, "Service", "", "struct", "", false)
	target := f.symbol(f.mainFile, "run", "Service", "function", "run()", false)
	caller := f.symbol(f.mainFile, "caller", "Service", "function", "caller()", false)
	var negative []int64
	for i, tc := range []struct{ dst, evidence string }{
		{"run", "swift:bare"},
		{"Service.run", "swift:member"},
		{"service.run", "swift:member"},
		{"super.run", "swift:super"},
		{"self.run", "swift:self;trailing_labels=_"},
		{"self.run", "swift:self;labels=bad"},
	} {
		negative = append(negative, f.call(f.mainFile, caller, tc.dst, tc.evidence, 0, i+1))
	}
	blocked := f.call(f.mainFile, caller, "self.run", "swift:self", 0, 20)
	negative = append(negative, blocked)
	f.blocker(f.mainFile, "Service", "run", graph.ScopeImportSwiftMemberValue, false)
	f.resolve()
	for _, edge := range negative {
		if got := f.dst(edge); got.Valid {
			t.Fatalf("blocked edge resolved to %d", got.Int64)
		}
	}
	_ = target
}

func TestSwiftSelfScopeSameSelectorAmbiguityStaysUnresolved(t *testing.T) {
	f := newSwiftScopeFixture(t)
	f.symbol(f.mainFile, "Service", "", "struct", "", false)
	f.symbol(f.mainFile, "run", "Service", "function", "run()", false)
	f.symbol(f.mainFile, "run", "Service", "function", "run()", false)
	caller := f.symbol(f.mainFile, "caller", "Service", "function", "caller()", false)
	edge := f.call(f.mainFile, caller, "self.run", "swift:self", 0, 1)
	f.resolve()
	if got := f.dst(edge); got.Valid {
		t.Fatalf("ambiguous edge resolved to %d", got.Int64)
	}
}

func TestSwiftSelfScopeConsumesLargeBatchedEdgeSet(t *testing.T) {
	f := newSwiftScopeFixture(t)
	f.symbol(f.mainFile, "Service", "", "struct", "", false)
	const count = 1200
	edges := make([]int64, 0, count)
	for i := 0; i < count; i++ {
		name := fmt.Sprintf("run%d", i)
		target := f.symbol(f.mainFile, name, "Service", "function", name+"()", false)
		caller := f.symbol(f.mainFile, fmt.Sprintf("caller%d", i), "Service", "function", fmt.Sprintf("caller%d()", i), false)
		edges = append(edges, f.call(f.mainFile, caller, "self."+name, "swift:self", 0, i+1))
		_ = target
	}
	f.resolve()
	for _, edge := range edges {
		got := f.dst(edge)
		if !got.Valid {
			t.Fatalf("edge %d unresolved", edge)
		}
	}
}

func TestSwiftSelfScopeIncrementalCrossFileVetoesRebind(t *testing.T) {
	f := newSwiftScopeFixture(t)
	f.symbol(f.mainFile, "Service", "", "struct", "", false)
	target := f.symbol(f.mainFile, "run", "Service", "function", "run()", false)
	caller := f.symbol(f.mainFile, "caller", "Service", "function", "caller()", false)
	edge := f.call(f.mainFile, caller, "self.run", "swift:self", 0, 1)
	f.reference(f.mainFile, caller, "self.run", 1)
	f.resolve()
	if got := f.dst(edge); !got.Valid || got.Int64 != target {
		t.Fatalf("initial binding=%v, want %d", got, target)
	}
	other := f.file("Other.swift")
	f.blocker(other, "Service", "run", graph.ScopeImportSwiftMemberValue, false)
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Other.swift"}); err != nil {
		t.Fatal(err)
	}
	if got := f.dst(edge); got.Valid {
		t.Fatalf("cross-file blocker left binding=%d", got.Int64)
	}
	var reference sql.NullInt64
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT symbol_id FROM references_tbl WHERE repo_id=?`, f.repoID).Scan(&reference); err != nil {
		t.Fatal(err)
	}
	if reference.Valid {
		t.Fatalf("cross-file blocker left reference=%d", reference.Int64)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE files SET is_deleted=1 WHERE id=?`, other); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Other.swift"}); err != nil {
		t.Fatal(err)
	}
	if got := f.dst(edge); !got.Valid || got.Int64 != target {
		t.Fatalf("after blocker removal=%v, want %d", got, target)
	}
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT symbol_id FROM references_tbl WHERE repo_id=?`, f.repoID).Scan(&reference); err != nil {
		t.Fatal(err)
	}
	if !reference.Valid || reference.Int64 != target {
		t.Fatalf("after blocker removal reference=%v, want %d", reference, target)
	}
	other = f.file("Competitor.swift")
	f.symbol(other, "run", "Service", "function", "run()", false)
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Competitor.swift"}); err != nil {
		t.Fatal(err)
	}
	if got := f.dst(edge); got.Valid {
		t.Fatalf("cross-file competitor left binding=%d", got.Int64)
	}
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT symbol_id FROM references_tbl WHERE repo_id=?`, f.repoID).Scan(&reference); err != nil {
		t.Fatal(err)
	}
	if reference.Valid {
		t.Fatalf("cross-file competitor left reference=%d", reference.Int64)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE files SET is_deleted=1 WHERE id=?`, other); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Competitor.swift"}); err != nil {
		t.Fatal(err)
	}
	if got := f.dst(edge); !got.Valid || got.Int64 != target {
		t.Fatalf("after competitor removal=%v, want %d", got, target)
	}
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT symbol_id FROM references_tbl WHERE repo_id=?`, f.repoID).Scan(&reference); err != nil {
		t.Fatal(err)
	}
	if !reference.Valid || reference.Int64 != target {
		t.Fatalf("after competitor removal reference=%v, want %d", reference, target)
	}
}

func TestSwiftSelfScopeRepairBindsAndMarksOnce(t *testing.T) {
	f := newSwiftScopeFixture(t)
	f.symbol(f.mainFile, "Service", "", "struct", "", false)
	target := f.symbol(f.mainFile, "run", "Service", "function", "run()", false)
	caller := f.symbol(f.mainFile, "caller", "Service", "function", "caller()", false)
	edge := f.call(f.mainFile, caller, "self.run", "swift:self", 0, 1)
	didRun, err := f.store.runResolverRepairOnce(f.ctx, f.repoID, swiftSelfRepair)
	if err != nil || !didRun {
		t.Fatalf("repair=(%v,%v), want (true,nil)", didRun, err)
	}
	if got := f.dst(edge); !got.Valid || got.Int64 != target {
		t.Fatalf("repaired binding=%v, want %d", got, target)
	}
	var marker string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT value FROM settings WHERE key=?`, swiftSelfRepairSettingKey+fmt.Sprintf(".%d", f.repoID)).Scan(&marker); err != nil {
		t.Fatal(err)
	}
	if marker != "1" {
		t.Fatalf("repair marker=%q", marker)
	}
	didRun, err = f.store.runResolverRepairOnce(f.ctx, f.repoID, swiftSelfRepair)
	if err != nil || didRun {
		t.Fatalf("second repair=(%v,%v), want (false,nil)", didRun, err)
	}
}

func TestSwiftSelfCallAcceptsExactRegularLabelsOnly(t *testing.T) {
	if got := swiftSelector("run", []string{"_", "cache:"}); got != "run(_:,cache:)" {
		t.Fatalf("selector = %q", got)
	}
	valid := []struct {
		evidence, dst string
		arity         int64
	}{
		{"swift:self", "self.run", 0},
		{"swift:self;labels=_", "self.run", 1},
		{"swift:self;labels=id:", "self.run", 1},
		{"swift:self;labels=_,cache:", "self.run", 2},
		{"swift:Self;labels=id:", "Self.run", 1},
	}
	for _, tc := range valid {
		method, _, labels, ok := swiftSelfCall(tc.evidence, tc.dst, sql.NullInt64{Int64: tc.arity, Valid: true})
		if !ok || method != "run" || len(labels) != int(tc.arity) {
			t.Fatalf("%+v: got %q %v %v", tc, method, labels, ok)
		}
	}
	invalid := []struct {
		evidence, dst string
		arity         int64
	}{
		{"swift:self;trailing_labels=_", "self.run", 1},
		{"swift:self;labels=;labels=id:", "self.run", 1},
		{"swift:self;labels=bad", "self.run", 1},
		{"swift:self;labels=id:", "self.foo.run", 1},
		{"swift:self", "self.run", 1},
		{"swift:Self", "Self.run", 1},
	}
	for _, tc := range invalid {
		if _, _, _, ok := swiftSelfCall(tc.evidence, tc.dst, sql.NullInt64{Int64: tc.arity, Valid: true}); ok {
			t.Fatalf("accepted invalid fact %+v", tc)
		}
	}
}

func TestSwiftInitializerScopeBindsExactSameFileInit(t *testing.T) {
	f := newSwiftScopeFixture(t)
	f.symbol(f.mainFile, "Service", "", "struct", "", false)
	target := f.symbol(f.mainFile, "init", "Service", "function", "init(id:)", false)
	f.arity(target, 1, 1)
	caller := f.symbol(f.mainFile, "make", "", "function", "make()", false)
	edge := f.call(f.mainFile, caller, "Service", "swift:initializer;labels=id:", 1, 1)
	f.resolve()
	if got := f.dst(edge); !got.Valid || got.Int64 != target {
		t.Fatalf("dst=%v want %d", got, target)
	}
	var strategy, confidence string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT resolution_strategy,resolution_confidence FROM edges WHERE id=?`, edge).Scan(&strategy, &confidence); err != nil {
		t.Fatal(err)
	}
	if strategy != ResolutionStrategySwiftInitializerScope || confidence != ResolutionConfidenceHigh {
		t.Fatalf("resolution=(%q,%q)", strategy, confidence)
	}
}

func TestSwiftInitializerScopeRefusesWrongEvidenceAndCrossFileCompetitor(t *testing.T) {
	f := newSwiftScopeFixture(t)
	f.symbol(f.mainFile, "Service", "", "struct", "", false)
	target := f.symbol(f.mainFile, "init", "Service", "function", "init(id:)", false)
	f.arity(target, 1, 1)
	caller := f.symbol(f.mainFile, "make", "", "function", "make()", false)
	wrong := f.call(f.mainFile, caller, "Service", "swift:initializer_unproven;generic_specialization=true", 1, 1)
	valid := f.call(f.mainFile, caller, "Service", "swift:initializer;labels=id:", 1, 2)
	other := f.file("Other.swift")
	competing := f.symbol(other, "init", "Service", "function", "init(id:)", false)
	f.arity(competing, 1, 1)
	f.resolve()
	for _, edge := range []int64{wrong, valid} {
		if got := f.dst(edge); got.Valid {
			t.Fatalf("edge %d resolved to %d", edge, got.Int64)
		}
	}
}

func TestSwiftInitializerResolverStatsTrackActualBindings(t *testing.T) {
	f := newSwiftScopeFixture(t)
	f.symbol(f.mainFile, "Service", "", "struct", "", false)
	target := f.symbol(f.mainFile, "init", "Service", "function", "init(id:)", false)
	f.arity(target, 1, 1)
	caller := f.symbol(f.mainFile, "make", "", "function", "make()", false)
	bound := f.call(f.mainFile, caller, "Service", "swift:initializer;labels=id:", 1, 1)
	unbound := f.call(f.mainFile, caller, "Service", "swift:initializer;labels=name:", 1, 2)
	stats := f.resolveNames("Service")
	assertSwiftStats(t, stats, 2, 1, 1, 0)
	if got := f.dst(bound); !got.Valid || got.Int64 != target {
		t.Fatalf("bound dst=%v want %d", got, target)
	}
	if got := f.dst(unbound); got.Valid {
		t.Fatalf("unbound dst=%d", got.Int64)
	}
}

func TestParseSwiftInitializerCallRejectsUnprovenAndMalformedFacts(t *testing.T) {
	valid := []struct {
		evidence, dst string
		arity         int64
	}{
		{"swift:initializer", "Service", 0},
		{"swift:initializer;labels=_", "Service", 1},
		{"swift:initializer;generic_specialization=true;labels=value:", "Box", 1},
		{"swift:initializer;trailing_labels=_", "Service", 1},
		{"swift:initializer;labels=id:;trailing_labels=_,completion:", "Service", 3},
	}
	for _, tc := range valid {
		if _, ok := parseSwiftInitializerCall(tc.evidence, tc.dst, sql.NullInt64{Int64: tc.arity, Valid: true}); !ok {
			t.Fatalf("rejected valid fact %+v", tc)
		}
	}
	invalid := []struct {
		evidence, dst string
		arity         sql.NullInt64
	}{
		{"swift:initializer_unproven;generic_specialization=true", "Service", sql.NullInt64{Int64: 0, Valid: true}},
		{"swift:initializer;labels=", "Service", sql.NullInt64{Int64: 0, Valid: true}},
		{"swift:initializer;generic_specialization=false", "Service", sql.NullInt64{Int64: 0, Valid: true}},
		{"swift:initializer;trailing_labels=completion:", "Service", sql.NullInt64{Int64: 1, Valid: true}},
		{"swift:initializer;trailing_labels=_,_", "Service", sql.NullInt64{Int64: 2, Valid: true}},
		{"swift:initializer;trailing_labels=", "Service", sql.NullInt64{Int64: 1, Valid: true}},
		{"swift:initializer;trailing_labels=_;generic_specialization=true", "Service", sql.NullInt64{Int64: 1, Valid: true}},
		{"swift:initializer", "Service.init", sql.NullInt64{Int64: 0, Valid: true}},
		{"swift:initializer;labels=id:", "Service", sql.NullInt64{Int64: 0, Valid: true}},
		{"swift:initializer;labels=id:,id:", "Service", sql.NullInt64{Int64: 2, Valid: true}},
		{"swift:initializer", "Sérvice", sql.NullInt64{Int64: 0, Valid: true}},
		{"swift:initializer", "Service", sql.NullInt64{}},
	}
	for _, tc := range invalid {
		if _, ok := parseSwiftInitializerCall(tc.evidence, tc.dst, tc.arity); ok {
			t.Fatalf("accepted invalid fact %+v", tc)
		}
	}
}

func TestSwiftTrailingClosureResolution(t *testing.T) {
	tests := []struct {
		name, evidence, signature string
		static                    bool
		labels                    []string
	}{
		{"underscore", "swift:self;trailing_labels=_", "run(_:) ", false, nil},
		{"named", "swift:self;trailing_labels=_", "run(completion:)", false, nil},
		{"regular-prefix", "swift:self;labels=id:;trailing_labels=_", "run(id:,completion:)", false, nil},
		{"multiple", "swift:self;trailing_labels=_,completion:", "run(first:,completion:)", false, nil},
		{"static-self", "swift:self;trailing_labels=_", "make(completion:)", true, nil},
		{"static-type", "swift:Self;trailing_labels=_", "make(completion:)", true, nil},
	}
	for i, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwiftScopeFixture(t)
			f.symbol(f.mainFile, "Service", "", "struct", "", false)
			caller := f.symbol(f.mainFile, "caller", "Service", "function", "caller()", tc.name == "static-self")
			name := "run"
			dst := "self.run"
			arity := int64(1)
			if tc.name == "regular-prefix" {
				arity, tc.signature = 2, "run(id:,completion:)"
			}
			if tc.name == "multiple" {
				arity, tc.signature = 2, "run(first:,completion:)"
			}
			if tc.name == "static-self" || strings.HasPrefix(tc.evidence, "swift:Self") {
				name = "make"
			}
			if strings.HasPrefix(tc.evidence, "swift:Self") {
				dst = "Self.make"
			}
			if tc.name == "static-self" {
				dst = "self.make"
			}
			target := f.symbol(f.mainFile, name, "Service", "function", strings.TrimSpace(tc.signature), tc.static)
			f.arity(target, arity, arity)
			edge := f.call(f.mainFile, caller, dst, tc.evidence, int(arity), i+1)
			f.resolve()
			if got := f.dst(edge); !got.Valid || got.Int64 != target {
				t.Fatalf("%s: dst=%v want %d", tc.name, got, target)
			}
		})
	}
}

func TestSwiftTrailingClosureRefusesAmbiguityAndUnsupportedFacts(t *testing.T) {
	f := newSwiftScopeFixture(t)
	f.symbol(f.mainFile, "Service", "", "struct", "", false)
	caller := f.symbol(f.mainFile, "caller", "Service", "function", "caller()", false)
	add := func(sig string, min, max int64) int64 {
		id := f.symbol(f.mainFile, "run", "Service", "function", sig, false)
		f.arity(id, min, max)
		return id
	}
	add("run(_:)", 1, 1)
	add("run(completion:)", 1, 1)
	ambiguous := f.call(f.mainFile, caller, "self.run", "swift:self;trailing_labels=_", 1, 1)
	defaulted := add("run(value:,completion:)", 1, 2)
	_ = defaulted
	defaultCall := f.call(f.mainFile, caller, "self.run", "swift:self;trailing_labels=_", 1, 2)
	variadic := add("run(items:,completion:)", 2, -1)
	_ = variadic
	variadicCall := f.call(f.mainFile, caller, "self.run", "swift:self;trailing_labels=_", 2, 3)
	malformed := f.call(f.mainFile, caller, "self.run", "swift:self;trailing_labels=completion:", 1, 4)
	f.resolve()
	for _, edge := range []int64{ambiguous, defaultCall, variadicCall, malformed} {
		if got := f.dst(edge); got.Valid {
			t.Fatalf("unsupported edge %d resolved to %d", edge, got.Int64)
		}
	}
}

func TestSwiftTrailingClosureStatsMixed(t *testing.T) {
	f := newSwiftScopeFixture(t)
	f.symbol(f.mainFile, "Service", "", "struct", "", false)
	one := f.symbol(f.mainFile, "run", "Service", "function", "run(completion:)", false)
	f.arity(one, 1, 1)
	two := f.symbol(f.mainFile, "finish", "Service", "function", "finish(_:)", false)
	f.arity(two, 1, 1)
	three := f.symbol(f.mainFile, "finish", "Service", "function", "finish(done:)", false)
	f.arity(three, 1, 1)
	caller := f.symbol(f.mainFile, "caller", "Service", "function", "caller()", false)
	f.call(f.mainFile, caller, "self.run", "swift:self;trailing_labels=_", 1, 1)
	f.call(f.mainFile, caller, "self.finish", "swift:self;trailing_labels=_", 1, 2)
	assertSwiftStats(t, f.resolveNames("run", "finish"), 2, 1, 1, 0)
}

func TestSwiftTrailingClosureSecondLabelAndBlocker(t *testing.T) {
	f := newSwiftScopeFixture(t)
	f.symbol(f.mainFile, "Service", "", "struct", "", false)
	caller := f.symbol(f.mainFile, "caller", "Service", "function", "caller()", false)
	target := f.symbol(f.mainFile, "run", "Service", "function", "run(first:,completion:)", false)
	f.arity(target, 2, 2)
	wrong := f.symbol(f.mainFile, "run", "Service", "function", "run(first:,failure:)", false)
	f.arity(wrong, 2, 2)
	edge := f.call(f.mainFile, caller, "self.run", "swift:self;trailing_labels=_,completion:", 2, 1)
	f.resolve()
	if got := f.dst(edge); !got.Valid || got.Int64 != target {
		t.Fatalf("second label dst=%v want %d", got, target)
	}
	f.blocker(f.mainFile, "Service", "run", graph.ScopeImportSwiftMemberValue, false)
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
		t.Fatal(err)
	}
	if got := f.dst(edge); got.Valid {
		t.Fatalf("member blocker left dst=%d", got.Int64)
	}
}

func TestSwiftTrailingClosureRepairBindsAndMarksOnce(t *testing.T) {
	f := newSwiftScopeFixture(t)
	f.symbol(f.mainFile, "Service", "", "struct", "", false)
	target := f.symbol(f.mainFile, "run", "Service", "function", "run(completion:)", false)
	f.arity(target, 1, 1)
	caller := f.symbol(f.mainFile, "caller", "Service", "function", "caller()", false)
	edge := f.call(f.mainFile, caller, "self.run", "swift:self;trailing_labels=_", 1, 1)
	didRun, err := f.store.runResolverRepairOnce(f.ctx, f.repoID, swiftTrailingRepair)
	if err != nil || !didRun {
		t.Fatalf("repair=(%v,%v)", didRun, err)
	}
	if got := f.dst(edge); !got.Valid || got.Int64 != target {
		t.Fatalf("repaired dst=%v want %d", got, target)
	}
	var marker string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT value FROM settings WHERE key=?`, swiftTrailingRepairSettingKey+fmt.Sprintf(".%d", f.repoID)).Scan(&marker); err != nil || marker != "1" {
		t.Fatalf("marker=%q err=%v", marker, err)
	}
	didRun, err = f.store.runResolverRepairOnce(f.ctx, f.repoID, swiftTrailingRepair)
	if err != nil || didRun {
		t.Fatalf("second repair=(%v,%v)", didRun, err)
	}
}

func TestSwiftTrailingRepairAppliesOnlyToTrailingCalls(t *testing.T) {
	noSwift, repoID := newQueryTestStore(t)
	if applies, err := noSwift.swiftTrailingRepairApplies(context.Background(), repoID); err != nil || applies {
		t.Fatalf("no-Swift applies=(%v,%v), want false", applies, err)
	}

	f := newSwiftScopeFixture(t)
	f.symbol(f.mainFile, "Service", "", "struct", "", false)
	target := f.symbol(f.mainFile, "run", "Service", "function", "run()", false)
	caller := f.symbol(f.mainFile, "caller", "Service", "function", "caller()", false)
	ordinary := f.call(f.mainFile, caller, "self.run", "swift:self", 0, 1)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET dst_symbol_id=?,resolution_strategy='sentinel' WHERE id=?`, target, ordinary); err != nil {
		t.Fatal(err)
	}
	if applies, err := f.store.swiftTrailingRepairApplies(f.ctx, f.repoID); err != nil || applies {
		t.Fatalf("ordinary-self applies=(%v,%v), want false", applies, err)
	}
	run, err := f.store.runResolverRepairOnce(f.ctx, f.repoID, swiftTrailingRepair)
	if err != nil || run {
		t.Fatalf("ordinary-self repair=(%v,%v), want (false,nil)", run, err)
	}
	var strategy string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT resolution_strategy FROM edges WHERE id=?`, ordinary).Scan(&strategy); err != nil {
		t.Fatal(err)
	}
	if strategy != "sentinel" {
		t.Fatalf("ordinary-self strategy=%q, repair executed", strategy)
	}
}

func TestSwiftTrailingRepairAppliesToUnresolvedAndBoundCalls(t *testing.T) {
	for _, tc := range []struct {
		name, evidence string
	}{
		{"unresolved", "swift:self;trailing_labels=_"},
		{"bound", "swift:Self;trailing_labels=_"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwiftScopeFixture(t)
			f.symbol(f.mainFile, "Service", "", "struct", "", false)
			target := f.symbol(f.mainFile, "run", "Service", "function", "run(completion:)", tc.name == "bound")
			f.arity(target, 1, 1)
			caller := f.symbol(f.mainFile, "caller", "Service", "function", "caller()", tc.name == "bound")
			dst := "self.run"
			if tc.name == "bound" {
				dst = "Self.run"
			}
			edge := f.call(f.mainFile, caller, dst, tc.evidence, 1, 1)
			if tc.name == "bound" {
				if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET dst_symbol_id=? WHERE id=?`, target, edge); err != nil {
					t.Fatal(err)
				}
			}
			applies, err := f.store.swiftTrailingRepairApplies(f.ctx, f.repoID)
			if err != nil || !applies {
				t.Fatalf("applies=(%v,%v), want true", applies, err)
			}
			didRun, err := f.store.runResolverRepairOnce(f.ctx, f.repoID, swiftTrailingRepair)
			if err != nil || !didRun {
				t.Fatalf("repair=(%v,%v), want (true,nil)", didRun, err)
			}
			if got := f.dst(edge); tc.name == "unresolved" && (!got.Valid || got.Int64 != target) {
				t.Fatalf("repaired dst=%v, want %d", got, target)
			}
			if tc.name == "bound" && (!f.dst(edge).Valid || f.dst(edge).Int64 != target) {
				t.Fatalf("bound dst changed: %v", f.dst(edge))
			}
		})
	}
}

func TestSwiftTrailingRepairFreshRepositoriesAvoidTax(t *testing.T) {
	s, _ := newQueryTestStore(t)
	root := t.TempDir()
	for i := 0; i < 16; i++ {
		repo, err := s.UpsertRepo(context.Background(), filepath.Join(root, fmt.Sprintf("repo-%d", i)))
		if err != nil {
			t.Fatal(err)
		}
		run, err := s.runResolverRepairOnce(context.Background(), repo.ID, swiftTrailingRepair)
		if err != nil || run {
			t.Fatalf("repo %d repair=(%v,%v), want (false,nil)", i, run, err)
		}
	}
}

func newSwiftInheritedFinalClassSelfFixture(t *testing.T, staticCaller bool) (*swiftScopeFixture, int64, int64) {
	return newSwiftInheritedFinalClassSelfFixtureWithDispatch(t, staticCaller, "")
}

func newSwiftInheritedFinalClassSelfFixtureWithDispatch(t *testing.T, staticCaller bool, callerDispatch string) (*swiftScopeFixture, int64, int64) {
	t.Helper()
	f := newSwiftScopeFixture(t)
	base := f.symbol(f.mainFile, "Base", "", "class", "", false)
	child := f.symbol(f.mainFile, "Child", "", "class", "", false)
	target := f.symbol(f.mainFile, "make", "Base", "function", "make()", true)
	caller := f.symbol(f.mainFile, "f", "Child", "function", "f()", staticCaller)
	edge := f.call(f.mainFile, caller, "Self.make", "swift:Self", 0, 1)
	f.reference(f.mainFile, caller, "Self.make", 1)
	f.declarationFact(f.mainFile, base, false)
	f.declarationFact(f.mainFile, child, false)
	if callerDispatch != "" {
		f.dispatchFact(f.mainFile, caller, false, callerDispatch)
	}
	f.dispatchFact(f.mainFile, target, true, "class")
	f.relation(f.mainFile, "Child", "Base", "superclass", false, false)
	return f, target, edge
}

func TestSwiftClassSelfTypeInheritedFinalClassMethodScopeCallerContexts(t *testing.T) {
	for _, tc := range []struct {
		name     string
		static   bool
		dispatch string
	}{
		{"instance", false, "instance"},
		{"static", true, "static"},
		{"class", true, "class"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, target, edge := newSwiftInheritedFinalClassSelfFixtureWithDispatch(t, tc.static, tc.dispatch)
			f.resolve()
			assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeInheritedFinalClassMethodScope)
		})
	}
}

func TestSwiftClassSelfTypeInheritedFinalClassMethodScopeOwnerFacts(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*swiftScopeFixture)
	}{
		{"one fact", func(*swiftScopeFixture) {}},
		{"missing fact", func(f *swiftScopeFixture) {
			if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM swift_declaration_facts WHERE symbol_id=(SELECT id FROM symbols WHERE qualified_name='Child')`); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"duplicate fact", func(f *swiftScopeFixture) {
			child := f.symbolID("Child")
			f.dispatchFact(f.mainFile, child, false, "class")
		}},
		{"final child", func(f *swiftScopeFixture) {
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET is_final=1 WHERE symbol_id=(SELECT id FROM symbols WHERE qualified_name='Child')`); err != nil {
				f.t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, target, edge := newSwiftInheritedFinalClassSelfFixture(t, false)
			tc.setup(f)
			f.resolve()
			if tc.name == "missing fact" || tc.name == "duplicate fact" {
				assertSwiftInheritedStaticEdgeUnresolved(t, f, edge)
			} else {
				assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeInheritedFinalClassMethodScope)
			}
		})
	}
}

func TestSwiftClassSelfTypeInheritedFinalClassMethodScopeTargetFacts(t *testing.T) {
	for _, tc := range []struct {
		name, dispatch                    string
		final, static, missing, duplicate bool
		strategy                          string
	}{
		{"final class", "class", true, true, false, false, ResolutionStrategySwiftClassSelfTypeInheritedFinalClassMethodScope},
		{"ordinary class", "class", false, true, false, false, ""},
		{"static", "static", false, true, false, false, ResolutionStrategySwiftClassSelfTypeInheritedStaticMethodScope},
		{"instance", "instance", true, false, false, false, ""},
		{"malformed", "", false, true, false, false, ""},
		{"missing fact", "", false, true, true, false, ""},
		{"duplicate fact", "class", true, true, false, true, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, target, edge := newSwiftInheritedFinalClassSelfFixture(t, false)
			if tc.missing {
				if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM swift_declaration_facts WHERE symbol_id=?`, target); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET is_static=? WHERE id=?`, boolInt(tc.static), target); err != nil {
					t.Fatal(err)
				}
				if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET dispatch_kind=?,is_final=? WHERE symbol_id=?`, tc.dispatch, boolInt(tc.final), target); err != nil {
					t.Fatal(err)
				}
				if tc.duplicate {
					f.dispatchFact(f.mainFile, target, true, "class")
				}
			}
			f.resolve()
			if tc.strategy == "" {
				assertSwiftInheritedStaticEdgeUnresolved(t, f, edge)
			} else {
				assertSwiftBinding(t, f, edge, target, tc.strategy)
			}
		})
	}
}

func TestSwiftClassSelfTypeInheritedFinalClassMethodScopeSelectors(t *testing.T) {
	for _, tc := range []struct {
		name, symbol, signature, evidence, dst string
		arity                                  int
	}{
		{"zero", "make", "make()", "swift:Self", "Self.make", 0},
		{"underscore", "make", "make(_:)", "swift:Self;labels=_", "Self.make", 1},
		{"named", "make", "make(id:)", "swift:Self;labels=id:", "Self.make", 1},
		{"two labels", "make", "make(id:,cache:)", "swift:Self;labels=id:,cache:", "Self.make", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, target, edge := newSwiftInheritedFinalClassSelfFixture(t, false)
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET name=?,qualified_name='Base.'||?,signature=?,arity_min=?,arity_max=? WHERE id=?`, tc.symbol, tc.symbol, tc.signature, tc.arity, tc.arity, target); err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET dst_name=?,evidence=?,call_arity=? WHERE id=?`, tc.dst, tc.evidence, tc.arity, edge); err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE references_tbl SET name=?,qualified_name=? WHERE repo_id=?`, tc.dst, tc.dst, f.repoID); err != nil {
				t.Fatal(err)
			}
			f.resolve()
			assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeInheritedFinalClassMethodScope)
		})
	}
}

func TestSwiftClassSelfTypeInheritedFinalClassMethodScopeTrailing(t *testing.T) {
	f, target, edge := newSwiftInheritedFinalClassSelfFixture(t, false)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET name='perform',qualified_name='Base.perform',signature='perform(_:)',arity_min=1,arity_max=1 WHERE id=?`, target); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET dst_name='Self.perform',evidence='swift:Self;trailing_labels=_',call_arity=1 WHERE id=?`, edge); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE references_tbl SET name='Self.perform',qualified_name='Self.perform' WHERE repo_id=?`, f.repoID); err != nil {
		t.Fatal(err)
	}
	f.resolve()
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeInheritedFinalClassMethodScope)

	for _, tc := range []struct {
		name       string
		competitor func(*swiftScopeFixture, int64)
	}{
		{"valid ambiguity", func(f *swiftScopeFixture, _ int64) {
			candidate := f.symbol(f.mainFile, "perform", "Base", "function", "perform(_:)", true)
			f.arity(candidate, 1, 1)
			f.dispatchFact(f.mainFile, candidate, true, "class")
		}},
		{"ordinary competitor", func(f *swiftScopeFixture, _ int64) {
			candidate := f.symbol(f.mainFile, "perform", "Base", "function", "perform(_:)", true)
			f.arity(candidate, 1, 1)
			f.dispatchFact(f.mainFile, candidate, false, "class")
		}},
		{"malformed competitor", func(f *swiftScopeFixture, _ int64) {
			candidate := f.symbol(f.mainFile, "perform", "Base", "function", "perform(_:)", true)
			f.arity(candidate, 1, 1)
			f.dispatchFact(f.mainFile, candidate, false, "")
		}},
		{"instance competitor", func(f *swiftScopeFixture, _ int64) {
			candidate := f.symbol(f.mainFile, "perform", "Base", "function", "perform(_:)", false)
			f.arity(candidate, 1, 1)
			f.dispatchFact(f.mainFile, candidate, false, "instance")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, e := newSwiftInheritedFinalClassSelfFixture(t, false)
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET name='perform',qualified_name='Base.perform',signature='perform(_:)',arity_min=1,arity_max=1 WHERE qualified_name='Base.make'`); err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET dst_name='Self.perform',evidence='swift:Self;trailing_labels=_',call_arity=1 WHERE id=?`, e); err != nil {
				t.Fatal(err)
			}
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE references_tbl SET name='Self.perform',qualified_name='Self.perform' WHERE repo_id=?`, f.repoID); err != nil {
				t.Fatal(err)
			}
			tc.competitor(f, 0)
			f.resolve()
			assertSwiftInheritedStaticEdgeUnresolved(t, f, e)
		})
	}
}

func TestSwiftClassSelfTypeInheritedFinalClassMethodScopeVisibilityAndRelations(t *testing.T) {
	for _, tc := range []struct {
		name, visibility, relation string
		generic, constrained       bool
		want                       bool
	}{
		{"private", "private", "superclass", false, false, false},
		{"fileprivate", "fileprivate", "superclass", false, false, true},
		{"internal", "internal", "superclass", false, false, true},
		{"public", "public", "superclass", false, false, true},
		{"conformance", "", "conformance", false, false, false},
		{"unproven", "", "unproven", false, false, false},
		{"generic", "", "superclass", true, false, false},
		{"constrained", "", "superclass", false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, target, edge := newSwiftInheritedFinalClassSelfFixture(t, false)
			if tc.visibility != "" {
				if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET visibility=? WHERE id=?`, tc.visibility, target); err != nil {
					t.Fatal(err)
				}
			}
			if tc.relation != "superclass" {
				if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET relation_kind=? WHERE child_qualified_name='Child'`, tc.relation); err != nil {
					t.Fatal(err)
				}
			} else if tc.generic || tc.constrained {
				if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET is_generic=?,is_constrained=? WHERE child_qualified_name='Child'`, boolInt(tc.generic), boolInt(tc.constrained)); err != nil {
					t.Fatal(err)
				}
			}
			f.resolve()
			if tc.want {
				assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeInheritedFinalClassMethodScope)
			} else {
				assertSwiftInheritedStaticEdgeUnresolved(t, f, edge)
			}
		})
	}
}

func TestSwiftClassSelfTypeInheritedFinalClassMethodScopeCompetingSuperclassAndReverse(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*swiftScopeFixture)
		want  bool
	}{
		{"competing superclass", func(f *swiftScopeFixture) {
			relationFile := f.file("Other.swift")
			f.relation(relationFile, "Child", "OtherBase", "superclass", false, false)
		}, false},
		{"reverse subclass", func(f *swiftScopeFixture) { f.relation(f.mainFile, "GrandChild", "Child", "superclass", false, false) }, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, target, edge := newSwiftInheritedFinalClassSelfFixture(t, false)
			tc.setup(f)
			f.resolve()
			if tc.want {
				assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeInheritedFinalClassMethodScope)
			} else {
				assertSwiftInheritedStaticEdgeUnresolved(t, f, edge)
			}
		})
	}
}

func TestSwiftClassSelfTypeInheritedFinalClassMethodScopeCandidateVetoes(t *testing.T) {
	for _, tc := range []struct {
		name, owner, visibility, dispatch string
		static, final, fact, duplicate    bool
	}{
		{"child static", "Child", "", "static", true, false, true, false},
		{"child class", "Child", "", "class", true, false, true, false},
		{"child final class", "Child", "", "class", true, true, true, false},
		{"child instance", "Child", "", "instance", false, false, true, false},
		{"child missing fact", "Child", "", "", true, false, false, false},
		{"base static", "Base", "", "static", true, false, true, false},
		{"base ordinary class", "Base", "", "class", true, false, true, false},
		{"base instance", "Base", "", "instance", false, false, true, false},
		{"base malformed", "Base", "", "", true, false, true, false},
		{"base missing fact", "Base", "", "", true, false, false, false},
		{"base duplicate facts", "Base", "", "class", true, true, true, true},
		{"base private final class", "Base", "private", "class", true, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, edge := newSwiftInheritedFinalClassSelfFixture(t, false)
			candidate := f.symbol(f.mainFile, "make", tc.owner, "function", "make()", tc.static)
			if tc.visibility != "" {
				if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET visibility=? WHERE id=?`, tc.visibility, candidate); err != nil {
					t.Fatal(err)
				}
			}
			if tc.fact {
				f.dispatchFact(f.mainFile, candidate, tc.final, tc.dispatch)
			}
			if tc.duplicate {
				f.dispatchFact(f.mainFile, candidate, tc.final, tc.dispatch)
			}
			f.resolve()
			assertSwiftInheritedStaticEdgeUnresolved(t, f, edge)
		})
	}
}

func TestSwiftClassSelfTypeInheritedFinalClassMethodScopeFiltering(t *testing.T) {
	for _, tc := range []struct {
		name, callerPath, hazardPath                                     string
		hazard, blocker, blockerTest, blockerStatic, enumCase, wantBound bool
	}{
		{"prod superclass", "Service.swift", "", false, false, false, false, false, true},
		{"prod test conformance", "Service.swift", "Tests/Hazard.swift", true, false, false, false, false, true},
		{"prod prod conformance", "Service.swift", "Hazard.swift", true, false, false, false, false, false},
		{"prod static blocker", "Service.swift", "", false, true, false, true, false, false},
		{"prod test blocker", "Service.swift", "", false, true, true, true, false, true},
		{"instance blocker", "Service.swift", "", false, true, false, false, false, true},
		{"enum blocker", "Service.swift", "", false, true, false, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwiftScopeFixture(t)
			callerFile := f.mainFile
			if strings.HasPrefix(tc.callerPath, "Tests/") {
				callerFile = f.file(tc.callerPath)
			}
			child := f.symbol(callerFile, "Child", "", "class", "", false)
			base := f.symbol(callerFile, "Base", "", "class", "", false)
			target := f.symbol(callerFile, "make", "Base", "function", "make()", true)
			caller := f.symbol(callerFile, "f", "Child", "function", "f()", false)
			edge := f.call(callerFile, caller, "Self.make", "swift:Self", 0, 1)
			f.reference(callerFile, caller, "Self.make", 1)
			f.declarationFact(callerFile, child, false)
			f.declarationFact(callerFile, base, false)
			f.dispatchFact(callerFile, target, true, "class")
			f.relation(callerFile, "Child", "Base", "superclass", false, false)
			if tc.hazard {
				f.relation(f.file(tc.hazardPath), "Child", "P", "conformance", false, false)
			}
			if tc.blocker {
				blockFile := callerFile
				if tc.blockerTest {
					blockFile = f.file("Tests/Blocker.swift")
				}
				kind := graph.ScopeImportSwiftMemberValue
				if tc.enumCase {
					kind = graph.ScopeImportSwiftEnumCase
				}
				f.blocker(blockFile, "Child", "make", kind, tc.blockerStatic)
			}
			f.resolve()
			if tc.wantBound {
				assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeInheritedFinalClassMethodScope)
			} else {
				assertSwiftInheritedStaticEdgeUnresolved(t, f, edge)
			}
		})
	}
}

func TestSwiftClassSelfTypeInheritedFinalClassMethodScopeProductionTestRelations(t *testing.T) {
	for _, tc := range []struct {
		name                                                     string
		callerTest, superclassTest, conformance, conformanceTest bool
		want                                                     bool
	}{
		{"prod caller prod superclass", false, false, false, false, true},
		{"prod caller test superclass", false, true, false, false, false},
		{"test caller test superclass", true, true, false, false, true},
		{"prod caller test conformance", false, false, true, true, true},
		{"prod caller prod conformance", false, false, true, false, false},
		{"test caller prod conformance", true, false, true, false, false},
		{"test caller test conformance", true, false, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwiftScopeFixture(t)
			callerFile := f.mainFile
			if tc.callerTest {
				callerFile = f.file("Tests/Service.swift")
			}
			child := f.symbol(callerFile, "Child", "", "class", "", false)
			base := f.symbol(callerFile, "Base", "", "class", "", false)
			target := f.symbol(callerFile, "make", "Base", "function", "make()", true)
			caller := f.symbol(callerFile, "f", "Child", "function", "f()", false)
			edge := f.call(callerFile, caller, "Self.make", "swift:Self", 0, 1)
			f.reference(callerFile, caller, "Self.make", 1)
			f.declarationFact(callerFile, child, false)
			f.declarationFact(callerFile, base, false)
			f.dispatchFact(callerFile, target, true, "class")
			relationFile := callerFile
			if tc.superclassTest && !tc.callerTest {
				relationFile = f.file("Tests/Relation.swift")
			}
			f.relation(relationFile, "Child", "Base", "superclass", false, false)
			if tc.conformance {
				hazardFile := f.file("Conformance.swift")
				if tc.conformanceTest {
					hazardFile = f.file("Tests/Conformance.swift")
				}
				f.relation(hazardFile, "Child", "P", "conformance", false, false)
			}
			f.resolve()
			if tc.want {
				assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeInheritedFinalClassMethodScope)
			} else {
				assertSwiftInheritedStaticEdgeUnresolved(t, f, edge)
			}
		})
	}
}

func TestSwiftClassSelfTypeInheritedFinalClassMethodScopeBlockers(t *testing.T) {
	for _, tc := range []struct {
		name                                            string
		callerTest, blockerTest, static, enumCase, want bool
	}{
		{"prod static", false, false, true, false, false},
		{"prod test static", false, true, true, false, true},
		{"test prod static", true, false, true, false, false},
		{"test test static", true, true, true, false, false},
		{"instance member", false, false, false, false, true},
		{"static enum case", false, false, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwiftScopeFixture(t)
			callerFile := f.mainFile
			if tc.callerTest {
				callerFile = f.file("Tests/Service.swift")
			}
			child := f.symbol(callerFile, "Child", "", "class", "", false)
			base := f.symbol(callerFile, "Base", "", "class", "", false)
			target := f.symbol(callerFile, "make", "Base", "function", "make()", true)
			caller := f.symbol(callerFile, "f", "Child", "function", "f()", false)
			edge := f.call(callerFile, caller, "Self.make", "swift:Self", 0, 1)
			f.reference(callerFile, caller, "Self.make", 1)
			f.declarationFact(callerFile, child, false)
			f.declarationFact(callerFile, base, false)
			f.dispatchFact(callerFile, target, true, "class")
			f.relation(callerFile, "Child", "Base", "superclass", false, false)
			blockerFile := callerFile
			if tc.blockerTest {
				blockerFile = f.file("Tests/Blocker.swift")
			}
			kind := graph.ScopeImportSwiftMemberValue
			if tc.enumCase {
				kind = graph.ScopeImportSwiftEnumCase
			}
			f.blocker(blockerFile, "Child", "make", kind, tc.static)
			f.resolve()
			if tc.want {
				assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeInheritedFinalClassMethodScope)
			} else {
				assertSwiftInheritedStaticEdgeUnresolved(t, f, edge)
			}
		})
	}
}

func TestSwiftClassSelfTypeInheritedFinalClassMethodScopeFileBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*swiftScopeFixture)
	}{
		{"cross-file method", func(f *swiftScopeFixture) {
			methodFile := f.file("Extension.swift")
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET file_id=? WHERE qualified_name='Base.make'`, methodFile); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"cross-file Base", func(f *swiftScopeFixture) {
			baseFile := f.file("Base.swift")
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET file_id=? WHERE qualified_name='Base'`, baseFile); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"cross-file relation", func(f *swiftScopeFixture) {
			if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM swift_inheritance_relations WHERE child_qualified_name='Child'`); err != nil {
				f.t.Fatal(err)
			}
			f.relation(f.file("Relation.swift"), "Child", "Base", "superclass", false, false)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, edge := newSwiftInheritedFinalClassSelfFixture(t, false)
			tc.setup(f)
			f.resolve()
			assertSwiftInheritedStaticEdgeUnresolved(t, f, edge)
		})
	}
}

func TestSwiftClassSelfTypeInheritedFinalClassMethodScopeQualifiedOwner(t *testing.T) {
	f := newSwiftScopeFixture(t)
	child := f.symbol(f.mainFile, "Child", "Outer", "class", "", false)
	base := f.symbol(f.mainFile, "Base", "Outer", "class", "", false)
	target := f.symbol(f.mainFile, "make", "Outer.Base", "function", "make()", true)
	caller := f.symbol(f.mainFile, "f", "Outer.Child", "function", "f()", false)
	edge := f.call(f.mainFile, caller, "Self.make", "swift:Self", 0, 1)
	f.reference(f.mainFile, caller, "Self.make", 1)
	f.declarationFact(f.mainFile, child, false)
	f.declarationFact(f.mainFile, base, false)
	f.dispatchFact(f.mainFile, target, true, "class")
	f.relation(f.mainFile, "Outer.Child", "Outer.Base", "superclass", false, false)
	f.resolve()
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeInheritedFinalClassMethodScope)

	unrelatedChild := f.symbol(f.mainFile, "Child", "", "class", "", false)
	unrelatedBase := f.symbol(f.mainFile, "Base", "", "class", "", false)
	f.declarationFact(f.mainFile, unrelatedChild, false)
	f.declarationFact(f.mainFile, unrelatedBase, false)
	f.relation(f.mainFile, "Child", "Base", "conformance", false, false)
	f.resolve()
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeInheritedFinalClassMethodScope)
}

func TestSwiftClassSelfTypeInheritedFinalClassMethodScopeLifecycle(t *testing.T) {
	f, target, edge := newSwiftInheritedFinalClassSelfFixture(t, false)
	f.resolve()
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeInheritedFinalClassMethodScope)

	hazard := f.file("Conformance.swift")
	f.relation(hazard, "Child", "P", "conformance", false, false)
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Conformance.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftInheritedStaticEdgeUnresolved(t, f, edge)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE files SET is_deleted=1 WHERE id=?`, hazard); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Conformance.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeInheritedFinalClassMethodScope)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE files SET is_deleted=0 WHERE id=?`, hazard); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Conformance.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftInheritedStaticEdgeUnresolved(t, f, edge)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE files SET is_deleted=1 WHERE id=?`, hazard); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Conformance.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeInheritedFinalClassMethodScope)

	blocker := f.file("Blocker.swift")
	f.blocker(blocker, "Child", "make", graph.ScopeImportSwiftMemberValue, true)
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Blocker.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftInheritedStaticEdgeUnresolved(t, f, edge)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE files SET is_deleted=1 WHERE id=?`, blocker); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Blocker.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeInheritedFinalClassMethodScope)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE files SET is_deleted=0 WHERE id=?`, blocker); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Blocker.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftInheritedStaticEdgeUnresolved(t, f, edge)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE files SET is_deleted=1 WHERE id=?`, blocker); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Blocker.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeInheritedFinalClassMethodScope)

	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET visibility='private' WHERE id=?`, target); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftInheritedStaticEdgeUnresolved(t, f, edge)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET visibility='fileprivate' WHERE id=?`, target); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeInheritedFinalClassMethodScope)

	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET is_final=0 WHERE symbol_id=?`, target); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftInheritedStaticEdgeUnresolved(t, f, edge)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET is_final=1 WHERE symbol_id=?`, target); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeInheritedFinalClassMethodScope)

	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET dispatch_kind='static' WHERE symbol_id=?`, target); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeInheritedStaticMethodScope)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET dispatch_kind='class',is_final=0 WHERE symbol_id=?`, target); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftInheritedStaticEdgeUnresolved(t, f, edge)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET dispatch_kind='class',is_final=1 WHERE symbol_id=?`, target); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeInheritedFinalClassMethodScope)

	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET target_qualified_name='OtherBase' WHERE child_qualified_name='Child'`); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftInheritedStaticEdgeUnresolved(t, f, edge)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET target_qualified_name='Base' WHERE child_qualified_name='Child'`); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeInheritedFinalClassMethodScope)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET relation_kind='unproven' WHERE child_qualified_name='Child'`); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftInheritedStaticEdgeUnresolved(t, f, edge)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET relation_kind='superclass' WHERE child_qualified_name='Child'`); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeInheritedFinalClassMethodScope)

	childCandidate := f.symbol(f.mainFile, "make", "Child", "function", "make()", true)
	f.dispatchFact(f.mainFile, childCandidate, false, "class")
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftInheritedStaticEdgeUnresolved(t, f, edge)
	if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM swift_declaration_facts WHERE symbol_id=?`, childCandidate); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM symbols WHERE id=?`, childCandidate); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeInheritedFinalClassMethodScope)
}

func TestSwiftClassSelfTypeInheritedFinalClassMethodScope(t *testing.T) {
	for _, staticCaller := range []bool{false, true} {
		f, target, edge := newSwiftInheritedFinalClassSelfFixture(t, staticCaller)
		f.resolve()
		assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeInheritedFinalClassMethodScope)
	}
	t.Run("final child", func(t *testing.T) {
		f, target, edge := newSwiftInheritedFinalClassSelfFixture(t, false)
		if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET is_final=1 WHERE symbol_id=(SELECT id FROM symbols WHERE qualified_name='Child')`); err != nil {
			t.Fatal(err)
		}
		f.resolve()
		assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeInheritedFinalClassMethodScope)
	})
}

func TestSwiftClassSelfTypeInheritedFinalClassMethodScopeNamesAndStats(t *testing.T) {
	t.Run("positive", func(t *testing.T) {
		f, target, edge := newSwiftInheritedFinalClassSelfFixture(t, false)
		stats := f.resolveNames("make")
		assertSwiftStats(t, stats, 1, 1, 0, 0)
		assertSwiftEdgeMetadata(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeInheritedFinalClassMethodScope)
	})
	t.Run("ordinary class", func(t *testing.T) {
		f, target, edge := newSwiftInheritedFinalClassSelfFixture(t, false)
		if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET is_final=0,dispatch_kind='class' WHERE symbol_id=?`, target); err != nil {
			t.Fatal(err)
		}
		stats := f.resolveNames("make")
		assertSwiftStats(t, stats, 1, 0, 1, 0)
		assertSwiftInheritedStaticEdgeUnresolved(t, f, edge)
	})
	t.Run("grandparent", func(t *testing.T) {
		f, _, edge := newSwiftInheritedFinalClassSelfFixture(t, false)
		middle := f.symbol(f.mainFile, "Middle", "", "class", "", false)
		f.declarationFact(f.mainFile, middle, false)
		if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET target_qualified_name='Middle' WHERE child_qualified_name='Child'`); err != nil {
			t.Fatal(err)
		}
		f.relation(f.mainFile, "Middle", "Base", "superclass", false, false)
		stats := f.resolveNames("make")
		assertSwiftStats(t, stats, 1, 0, 1, 0)
		assertSwiftInheritedStaticEdgeUnresolved(t, f, edge)
	})
	t.Run("inherited static", func(t *testing.T) {
		f, target, edge := newSwiftInheritedStaticSelfFixture(t, false)
		stats := f.resolveNames("make")
		assertSwiftStats(t, stats, 1, 1, 0, 0)
		assertSwiftEdgeMetadata(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeInheritedStaticMethodScope)
	})
}

func TestSwiftClassSelfTypeInheritedFinalClassMethodScopeControls(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(*swiftScopeFixture, int64)
	}{
		{"ordinary class", func(f *swiftScopeFixture, target int64) {
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET is_final=0,dispatch_kind='class' WHERE symbol_id=?`, target); err != nil {
				f.t.Fatal(err)
			}
		}},
		{"private", func(f *swiftScopeFixture, target int64) {
			_, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET visibility='private' WHERE id=?`, target)
			if err != nil {
				f.t.Fatal(err)
			}
		}},
		{"child candidate", func(f *swiftScopeFixture, _ int64) { f.symbol(f.mainFile, "make", "Child", "function", "make()", true) }},
		{"unsafe base competitor", func(f *swiftScopeFixture, _ int64) {
			competitor := f.symbol(f.mainFile, "make", "Base", "function", "make()", false)
			f.dispatchFact(f.mainFile, competitor, false, "instance")
		}},
		{"static blocker", func(f *swiftScopeFixture, _ int64) {
			f.blocker(f.mainFile, "Child", "make", graph.ScopeImportSwiftMemberValue, true)
		}},
		{"grandparent", func(f *swiftScopeFixture, _ int64) {
			middle := f.symbol(f.mainFile, "Middle", "", "class", "", false)
			f.declarationFact(f.mainFile, middle, false)
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET target_qualified_name='Middle' WHERE child_qualified_name='Child'`); err != nil {
				f.t.Fatal(err)
			}
			f.relation(f.mainFile, "Middle", "Base", "superclass", false, false)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, target, edge := newSwiftInheritedFinalClassSelfFixture(t, false)
			tc.setup(f, target)
			f.resolve()
			assertSwiftInheritedStaticEdgeUnresolved(t, f, edge)
		})
	}
}

func TestSwiftClassSelfTypeInheritedFinalClassMethodScopeUpgradeRepair(t *testing.T) {
	f, target, edge := newSwiftInheritedFinalClassSelfFixture(t, false)
	for _, repair := range resolverRepairs {
		if repair.key == swiftClassSelfTypeInheritedFinalClassMethodRepairSettingKey {
			continue
		}
		if err := f.store.markRepairDone(f.ctx, repair.key, f.repoID); err != nil {
			t.Fatal(err)
		}
	}
	refID := swiftReferenceID(t, f, edge)
	var preDst sql.NullInt64
	var preStrategy, preConfidence string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT dst_symbol_id,resolution_strategy,resolution_confidence FROM edges WHERE id=?`, edge).Scan(&preDst, &preStrategy, &preConfidence); err != nil {
		t.Fatal(err)
	}
	if preDst.Valid || preStrategy != "" || preConfidence != "" {
		t.Fatalf("pre-repair=(%v,%q,%q), want empty", preDst, preStrategy, preConfidence)
	}
	assertSwiftReferenceCleared(t, f, refID)
	var preMarker string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT value FROM settings WHERE key=?`, swiftClassSelfTypeInheritedFinalClassMethodRepairSettingKey+fmt.Sprintf(".%d", f.repoID)).Scan(&preMarker); err != sql.ErrNoRows {
		t.Fatalf("pre-repair marker=%q err=%v, want absent", preMarker, err)
	}
	run, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID)
	if err != nil || !run {
		t.Fatalf("repair=(%v,%v)", run, err)
	}
	assertSwiftBinding(t, f, edge, target, ResolutionStrategySwiftClassSelfTypeInheritedFinalClassMethodScope)
	assertSwiftReferenceTarget(t, f, refID, target)
	var marker string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT value FROM settings WHERE key=?`, swiftClassSelfTypeInheritedFinalClassMethodRepairSettingKey+fmt.Sprintf(".%d", f.repoID)).Scan(&marker); err != nil || marker != "1" {
		t.Fatalf("marker=%q err=%v", marker, err)
	}
	var firstDst sql.NullInt64
	var firstStrategy, firstConfidence string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT dst_symbol_id,resolution_strategy,resolution_confidence FROM edges WHERE id=?`, edge).Scan(&firstDst, &firstStrategy, &firstConfidence); err != nil {
		t.Fatal(err)
	}
	firstReference := swiftReferenceSymbol(t, f, refID)
	firstMarker := marker
	run, err = f.store.RepairResolverBindingsOnce(f.ctx, f.repoID)
	if err != nil || run {
		t.Fatalf("second repair=(%v,%v)", run, err)
	}
	var secondDst sql.NullInt64
	var secondStrategy, secondConfidence string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT dst_symbol_id,resolution_strategy,resolution_confidence FROM edges WHERE id=?`, edge).Scan(&secondDst, &secondStrategy, &secondConfidence); err != nil {
		t.Fatal(err)
	}
	secondReference := swiftReferenceSymbol(t, f, refID)
	var secondMarker string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT value FROM settings WHERE key=?`, swiftClassSelfTypeInheritedFinalClassMethodRepairSettingKey+fmt.Sprintf(".%d", f.repoID)).Scan(&secondMarker); err != nil {
		t.Fatal(err)
	}
	if firstDst != secondDst || firstStrategy != secondStrategy || firstConfidence != secondConfidence || firstReference != secondReference || firstMarker != secondMarker {
		t.Fatalf("second repair changed state: first=(%v,%q,%q,%v,%q), second=(%v,%q,%q,%v,%q)", firstDst, firstStrategy, firstConfidence, firstReference, firstMarker, secondDst, secondStrategy, secondConfidence, secondReference, secondMarker)
	}
}

func TestSwiftClassSelfTypeInheritedFinalClassMethodScopeUnsafeExistingBindingRepair(t *testing.T) {
	f, target, edge := newSwiftInheritedFinalClassSelfFixture(t, false)
	refID := swiftReferenceID(t, f, edge)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET dst_symbol_id=?,resolution_strategy=?,resolution_confidence=? WHERE id=?`, target, ResolutionStrategySwiftClassSelfTypeInheritedFinalClassMethodScope, ResolutionConfidenceHigh, edge); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE references_tbl SET symbol_id=? WHERE id=?`, target, refID); err != nil {
		t.Fatal(err)
	}
	for _, repair := range resolverRepairs {
		if repair.key == swiftClassSelfTypeInheritedFinalClassMethodRepairSettingKey {
			continue
		}
		if err := f.store.markRepairDone(f.ctx, repair.key, f.repoID); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET is_final=0 WHERE symbol_id=?`, target); err != nil {
		t.Fatal(err)
	}
	run, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID)
	if err != nil || !run {
		t.Fatalf("repair=(%v,%v)", run, err)
	}
	assertSwiftInheritedStaticEdgeUnresolved(t, f, edge)
	var marker string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT value FROM settings WHERE key=?`, swiftClassSelfTypeInheritedFinalClassMethodRepairSettingKey+fmt.Sprintf(".%d", f.repoID)).Scan(&marker); err != nil || marker != "1" {
		t.Fatalf("marker=%q err=%v", marker, err)
	}
}

func TestSwiftClassSelfTypeInheritedFinalClassMethodScopeHardenedStress(t *testing.T) {
	type stressCase struct{ name, strategy string }
	var cases = []stressCase{
		{"instance", ResolutionStrategySwiftClassSelfTypeInheritedFinalClassMethodScope},
		{"static_caller", ResolutionStrategySwiftClassSelfTypeInheritedFinalClassMethodScope},
		{"class_caller", ResolutionStrategySwiftClassSelfTypeInheritedFinalClassMethodScope},
		{"final_child", ResolutionStrategySwiftClassSelfTypeInheritedFinalClassMethodScope},
		{"qualified", ResolutionStrategySwiftClassSelfTypeInheritedFinalClassMethodScope},
		{"reverse", ResolutionStrategySwiftClassSelfTypeInheritedFinalClassMethodScope},
		{"instance_blocker", ResolutionStrategySwiftClassSelfTypeInheritedFinalClassMethodScope},
		{"same_static", ResolutionStrategySwiftClassSelfTypeStaticMethodScope},
		{"same_final_class", ResolutionStrategySwiftClassSelfTypeFinalClassMethodScope},
		{"inherited_final_instance", ResolutionStrategySwiftClassSelfInheritedFinalMethodScope},
		{"inherited_static", ResolutionStrategySwiftClassSelfTypeInheritedStaticMethodScope},
		{"ordinary_class", ""}, {"grandparent", ""}, {"missing_relation", ""}, {"duplicate_relation", ""},
		{"cross_competing", ""}, {"unproven", ""}, {"conformance", ""}, {"cross_conformance", ""},
		{"generic", ""}, {"constrained", ""}, {"missing_base", ""}, {"duplicate_base", ""},
		{"cross_base", ""}, {"cross_relation", ""}, {"cross_method", ""}, {"private", ""},
		{"missing_fact", ""}, {"duplicate_fact", ""}, {"malformed", ""}, {"inherited_instance", ""},
		{"child_static", ""}, {"child_class", ""}, {"child_final", ""}, {"child_malformed", ""},
		{"base_static", ""}, {"base_class", ""}, {"base_instance", ""}, {"base_malformed", ""}, {"base_private", ""},
		{"static_blocker", ""}, {"enum_blocker", ""}, {"trailing_ambiguity", ""}, {"trailing_unsafe", ""},
	}
	const perCase = 100
	f := newSwiftScopeFixture(t)
	for _, tc := range cases {
		childName, baseName := "Child_"+tc.name, "Base_"+tc.name
		childOwner, baseOwner := childName, baseName
		if tc.name == "qualified" {
			childOwner, baseOwner = "Outer."+childName, "Outer."+baseName
		}
		childFile, baseFile, targetFile, relationFile := f.mainFile, f.mainFile, f.mainFile, f.mainFile
		if tc.name == "cross_base" {
			baseFile = f.file("Base_" + tc.name + ".swift")
		}
		if tc.name == "cross_method" {
			targetFile = f.file("Extension_" + tc.name + ".swift")
		}
		if tc.name == "cross_relation" || tc.name == "cross_competing" || tc.name == "cross_conformance" {
			relationFile = f.file("Relation_" + tc.name + ".swift")
		}
		child := f.symbol(childFile, childName, func() string {
			if tc.name == "qualified" {
				return "Outer"
			}
			return ""
		}(), "class", "", false)
		f.declarationFact(childFile, child, tc.name == "final_child")
		if tc.name != "missing_base" {
			base := f.symbol(baseFile, baseName, func() string {
				if tc.name == "qualified" {
					return "Outer"
				}
				return ""
			}(), "class", "", false)
			f.declarationFact(baseFile, base, false)
			if tc.name == "duplicate_base" {
				duplicate := f.symbol(f.mainFile, baseName, "", "class", "", false)
				f.declarationFact(f.mainFile, duplicate, false)
			}
		}
		switch tc.name {
		case "missing_relation":
		case "grandparent":
			middle := f.symbol(f.mainFile, "Middle_"+tc.name, "", "class", "", false)
			f.declarationFact(f.mainFile, middle, false)
			f.relation(f.mainFile, childOwner, "Middle_"+tc.name, "superclass", false, false)
			f.relation(f.mainFile, "Middle_"+tc.name, baseOwner, "superclass", false, false)
		default:
			target := baseOwner
			if tc.name == "missing_base" {
				target = "Missing_" + tc.name
			}
			if tc.name != "cross_relation" && tc.name != "same_static" && tc.name != "same_final_class" {
				f.relation(f.mainFile, childOwner, target, "superclass", false, false)
			} else if tc.name == "cross_relation" {
				f.relation(relationFile, childOwner, target, "superclass", false, false)
			}
			if tc.name == "duplicate_relation" {
				f.relation(f.mainFile, childOwner, target, "superclass", false, false)
			}
			if tc.name == "cross_competing" {
				f.relation(relationFile, childOwner, "OtherBase_"+tc.name, "superclass", false, false)
			}
			if tc.name == "unproven" {
				if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET relation_kind='unproven' WHERE child_qualified_name=?`, childOwner); err != nil {
					t.Fatal(err)
				}
			}
			if tc.name == "conformance" {
				f.relation(f.mainFile, childOwner, "P_"+tc.name, "conformance", false, false)
			}
			if tc.name == "cross_conformance" {
				f.relation(relationFile, childOwner, "P_"+tc.name, "conformance", false, false)
			}
			if tc.name == "generic" {
				if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET is_generic=1 WHERE child_qualified_name=?`, childOwner); err != nil {
					t.Fatal(err)
				}
			}
			if tc.name == "constrained" {
				if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET is_constrained=1 WHERE child_qualified_name=?`, childOwner); err != nil {
					t.Fatal(err)
				}
			}
		}
		if tc.name == "reverse" {
			f.relation(f.mainFile, "GrandChild_"+tc.name, childOwner, "superclass", false, false)
		}
		for i := 0; i < perCase; i++ {
			method := fmt.Sprintf("make_%s_%d", tc.name, i)
			owner, static := baseOwner, true
			edgeName, evidence, arity := "Self."+method, "swift:Self", 0
			if tc.name == "same_static" || tc.name == "same_final_class" {
				owner = childOwner
			}
			if tc.name == "inherited_final_instance" {
				static, edgeName, evidence = false, "self."+method, "swift:self"
			}
			if tc.name == "inherited_static" {
				static = true
			}
			if strings.HasPrefix(tc.name, "trailing_") {
				method, edgeName, evidence, arity = "perform", "Self.perform", "swift:Self;trailing_labels=_", 1
			}
			target := f.symbol(targetFile, method, owner, "function", method+"()", static)
			if strings.HasPrefix(tc.name, "trailing_") {
				f.arity(target, 1, 1)
				if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET signature='perform(_:)' WHERE id=?`, target); err != nil {
					t.Fatal(err)
				}
			}
			switch tc.name {
			case "same_static", "inherited_static":
				f.dispatchFact(targetFile, target, false, "static")
			case "same_final_class":
				f.dispatchFact(targetFile, target, true, "class")
			case "inherited_final_instance":
				f.dispatchFact(targetFile, target, true, "instance")
			case "ordinary_class":
				f.dispatchFact(targetFile, target, false, "class")
			case "malformed":
				f.dispatchFact(targetFile, target, false, "")
			case "final_child":
				f.dispatchFact(targetFile, target, true, "class")
			case "missing_fact":
			case "duplicate_fact":
				f.dispatchFact(targetFile, target, true, "class")
				f.dispatchFact(targetFile, target, true, "class")
			case "inherited_instance":
				f.dispatchFact(targetFile, target, false, "instance")
				if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET is_static=0 WHERE id=?`, target); err != nil {
					t.Fatal(err)
				}
			default:
				f.dispatchFact(targetFile, target, true, "class")
			}
			if tc.name == "private" {
				if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET visibility='private' WHERE id=?`, target); err != nil {
					t.Fatal(err)
				}
			}
			if tc.name == "child_static" || tc.name == "child_class" || tc.name == "child_final" || tc.name == "child_malformed" {
				candidate := f.symbol(f.mainFile, method, childOwner, "function", method+"()", tc.name != "child_malformed")
				if tc.name == "child_static" {
					f.dispatchFact(f.mainFile, candidate, false, "static")
				}
				if tc.name == "child_class" || tc.name == "child_final" {
					f.dispatchFact(f.mainFile, candidate, tc.name == "child_final", "class")
				}
			}
			if tc.name == "base_static" || tc.name == "base_class" || tc.name == "base_instance" || tc.name == "base_malformed" || tc.name == "base_private" {
				candidate := f.symbol(f.mainFile, method, baseOwner, "function", method+"()", tc.name != "base_instance")
				dispatch := ""
				if tc.name == "base_static" {
					dispatch = "static"
				}
				if tc.name == "base_class" {
					dispatch = "class"
				}
				if tc.name == "base_instance" {
					dispatch = "instance"
				}
				if tc.name == "base_private" {
					dispatch = "class"
				}
				f.dispatchFact(f.mainFile, candidate, tc.name == "base_private", dispatch)
				if tc.name == "base_private" {
					if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET visibility='private' WHERE id=?`, candidate); err != nil {
						t.Fatal(err)
					}
				}
			}
			if tc.name == "trailing_ambiguity" || tc.name == "trailing_unsafe" {
				candidate := f.symbol(f.mainFile, "perform", baseOwner, "function", "perform(_:)", true)
				f.arity(candidate, 1, 1)
				f.dispatchFact(f.mainFile, candidate, tc.name == "trailing_ambiguity", "class")
			}
			callerStatic := tc.name == "static_caller" || tc.name == "class_caller"
			caller := f.symbol(f.mainFile, fmt.Sprintf("f_%s_%d", tc.name, i), childOwner, "function", "f()", callerStatic)
			if tc.name == "static_caller" {
				f.dispatchFact(f.mainFile, caller, false, "static")
			}
			if tc.name == "class_caller" {
				f.dispatchFact(f.mainFile, caller, false, "class")
			}
			f.call(f.mainFile, caller, edgeName, evidence, arity, i+1)
			if tc.name == "instance_blocker" {
				f.blocker(f.mainFile, childOwner, method, graph.ScopeImportSwiftMemberValue, false)
			}
			if tc.name == "static_blocker" {
				f.blocker(f.mainFile, childOwner, method, graph.ScopeImportSwiftMemberValue, true)
			}
			if tc.name == "enum_blocker" {
				f.blocker(f.mainFile, childOwner, method, graph.ScopeImportSwiftEnumCase, true)
			}
		}
	}
	type stressState struct {
		total, resolved, unresolved, badMetadata int
		strategies                               map[string]int
	}
	readState := func() stressState {
		var state stressState
		if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*),COALESCE(SUM(dst_symbol_id IS NOT NULL),0),COALESCE(SUM(dst_symbol_id IS NULL),0),COALESCE(SUM(dst_symbol_id IS NULL AND (resolution_strategy<>'' OR resolution_confidence<>'')),0) FROM edges WHERE repo_id=?`, f.repoID).Scan(&state.total, &state.resolved, &state.unresolved, &state.badMetadata); err != nil {
			t.Fatal(err)
		}
		state.strategies = map[string]int{}
		rows, err := f.store.db.QueryContext(f.ctx, `SELECT resolution_strategy,COUNT(*) FROM edges WHERE repo_id=? GROUP BY resolution_strategy`, f.repoID)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var strategy string
			var count int
			if err := rows.Scan(&strategy, &count); err != nil {
				t.Fatal(err)
			}
			state.strategies[strategy] = count
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return state
	}
	f.resolve()
	first := readState()
	want := stressState{total: 4400, resolved: 1100, unresolved: 3300, badMetadata: 0, strategies: map[string]int{
		ResolutionStrategySwiftClassSelfTypeInheritedFinalClassMethodScope: 700,
		ResolutionStrategySwiftClassSelfTypeInheritedStaticMethodScope:     100,
		ResolutionStrategySwiftClassSelfTypeStaticMethodScope:              100,
		ResolutionStrategySwiftClassSelfTypeFinalClassMethodScope:          100,
		ResolutionStrategySwiftClassSelfInheritedFinalMethodScope:          100,
		"": 3300,
	}}
	if !reflect.DeepEqual(first, want) {
		t.Fatalf("first state=%+v, want %+v", first, want)
	}
	f.resolve()
	second := readState()
	if !reflect.DeepEqual(second, first) {
		t.Fatalf("second state=%+v, first=%+v", second, first)
	}
}
