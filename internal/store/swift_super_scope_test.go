package store

import (
	"context"
	"database/sql"
	"reflect"
	"strconv"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

type stressState struct {
	total       int
	resolved    int
	unresolved  int
	badMetadata int
	strategies  map[string]int
}

func swiftSetRange(t *testing.T, f *swiftScopeFixture, id int64, startLine, startCol, endLine, endCol int64) {
	t.Helper()
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET start_line=?,start_col=?,end_line=?,end_col=? WHERE id=?`, startLine, startCol, endLine, endCol, id); err != nil {
		t.Fatal(err)
	}
}

func swiftFact(t *testing.T, f *swiftScopeFixture, file, symbol int64, dispatch string) {
	t.Helper()
	if _, err := f.store.db.ExecContext(f.ctx, `INSERT INTO swift_declaration_facts(repo_id,file_id,symbol_id,dispatch_kind) VALUES(?,?,?,?)`, f.repoID, file, symbol, dispatch); err != nil {
		t.Fatal(err)
	}
}

func swiftSuperclass(t *testing.T, f *swiftScopeFixture, file int64, child, base string) {
	t.Helper()
	if _, err := f.store.db.ExecContext(f.ctx, `INSERT INTO swift_inheritance_relations(repo_id,file_id,child_qualified_name,target_qualified_name,relation_kind,start_line,start_col,end_line,end_col) VALUES(?,?,?,?,?,?,?,?,?)`, f.repoID, file, child, base, "superclass", 1, 1, 1, 1); err != nil {
		t.Fatal(err)
	}
}

func TestSwiftSuperScopeBindsDirectNominalInstanceMethod(t *testing.T) {
	f := newSwiftScopeFixture(t)
	base := f.symbol(f.mainFile, "Base", "", "class", "", false)
	baseRun := f.symbol(f.mainFile, "run", "Base", "function", "run()", false)
	child := f.symbol(f.mainFile, "Child", "", "class", "", false)
	caller := f.symbol(f.mainFile, "f", "Child", "function", "f()", false)
	swiftSetRange(t, f, base, 1, 1, 3, 2)
	swiftSetRange(t, f, baseRun, 2, 1, 2, 15)
	f.arity(baseRun, 0, 0)
	swiftSetRange(t, f, child, 5, 1, 9, 2)
	swiftSetRange(t, f, caller, 6, 1, 8, 2)
	swiftSuperclass(t, f, f.mainFile, "Child", "Base")
	swiftFact(t, f, f.mainFile, baseRun, "instance")
	swiftFact(t, f, f.mainFile, caller, "instance")
	edge := f.call(f.mainFile, caller, "super.run", "swift:super", 0, 7)
	f.resolve()
	if got := f.dst(edge); !got.Valid || got.Int64 != baseRun {
		t.Fatalf("dst=%v, want %d", got, baseRun)
	}
	var strategy, confidence string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT resolution_strategy,resolution_confidence FROM edges WHERE id=?`, edge).Scan(&strategy, &confidence); err != nil {
		t.Fatal(err)
	}
	if strategy != ResolutionStrategySwiftSuperScope || confidence != ResolutionConfidenceHigh {
		t.Fatalf("metadata=(%q,%q)", strategy, confidence)
	}
}

func TestSwiftSuperScopeDeletedBlockerLifecycle(t *testing.T) {
	f := newSwiftSuperAcceptanceFixture(t)
	f.reference(f.mainFile, f.caller, "super.run", 7)
	blockerFile := f.file("Blocker.swift")
	f.blocker(blockerFile, "Base", "run", graph.ScopeImportSwiftMemberValue, false)
	resolve := func() {
		t.Helper()
		if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Blocker.swift"}); err != nil {
			t.Fatal(err)
		}
	}
	resolve()
	assertSwiftEdgeUnresolved(t, f.swiftScopeFixture, f.edge)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE files SET is_deleted=1 WHERE id=?`, blockerFile); err != nil {
		t.Fatal(err)
	}
	resolve()
	assertSwiftBinding(t, f.swiftScopeFixture, f.edge, f.baseRun, ResolutionStrategySwiftSuperScope)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE files SET is_deleted=0 WHERE id=?`, blockerFile); err != nil {
		t.Fatal(err)
	}
	resolve()
	assertSwiftEdgeUnresolved(t, f.swiftScopeFixture, f.edge)
}

func TestSwiftSuperCompatibleCompetitorVeto(t *testing.T) {
	f := newSwiftSuperAcceptanceFixture(t)
	competitor := f.symbol(f.mainFile, "run", "Base", "function", "run()", false)
	swiftSetRange(t, f.swiftScopeFixture, competitor, 2, 1, 2, 15)
	f.arity(competitor, 0, 0)
	swiftFact(t, f.swiftScopeFixture, f.mainFile, competitor, "static")
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET is_static=1 WHERE id=?`, competitor); err != nil {
		t.Fatal(err)
	}
	f.resolve()
	assertSwiftEdgeUnresolved(t, f.swiftScopeFixture, f.edge)
}

func TestSwiftSuperMultilevelInheritedMethodScope(t *testing.T) {
	f := newSwiftScopeFixture(t)
	base := f.symbol(f.mainFile, "Base", "", "class", "", false)
	middle := f.symbol(f.mainFile, "Middle", "", "class", "", false)
	child := f.symbol(f.mainFile, "Child", "", "class", "", false)
	caller := f.symbol(f.mainFile, "f", "Child", "function", "f()", false)
	baseRun := f.symbol(f.mainFile, "run", "Base", "function", "run()", false)
	for id, span := range map[int64][2]int64{base: {1, 3}, middle: {5, 7}, child: {9, 13}, caller: {10, 12}, baseRun: {2, 2}} {
		swiftSetRange(t, f, id, span[0], 1, span[1], 20)
	}
	f.arity(baseRun, 0, 0)
	swiftSuperclass(t, f, f.mainFile, "Child", "Middle")
	swiftSuperclass(t, f, f.mainFile, "Middle", "Base")
	swiftFact(t, f, f.mainFile, baseRun, "instance")
	swiftFact(t, f, f.mainFile, caller, "instance")
	edge := f.call(f.mainFile, caller, "super.run", "swift:super", 0, 11)
	f.resolve()
	if got := f.dst(edge); !got.Valid || got.Int64 != baseRun {
		t.Fatalf("dst=%v, want %d", got, baseRun)
	}
	var strategy, confidence string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT resolution_strategy,resolution_confidence FROM edges WHERE id=?`, edge).Scan(&strategy, &confidence); err != nil {
		t.Fatal(err)
	}
	if strategy != ResolutionStrategySwiftSuperMultilevelInheritedMethodScope || confidence != ResolutionConfidenceHigh {
		t.Fatalf("metadata=(%q,%q)", strategy, confidence)
	}

	middleRun := f.symbol(f.mainFile, "run", "Middle", "function", "run()", false)
	swiftSetRange(t, f, middleRun, 6, 1, 6, 20)
	f.arity(middleRun, 0, 0)
	swiftFact(t, f, f.mainFile, middleRun, "instance")
	if err := f.store.redecideSwiftSuperBindings(f.ctx, f.repoID); err != nil {
		t.Fatal(err)
	}
	if got := f.dst(edge); !got.Valid || got.Int64 != middleRun {
		t.Fatalf("override dst=%v, want %d", got, middleRun)
	}
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT resolution_strategy FROM edges WHERE id=?`, edge).Scan(&strategy); err != nil {
		t.Fatal(err)
	}
	if strategy != ResolutionStrategySwiftSuperScope {
		t.Fatalf("override strategy=%q", strategy)
	}
}

func TestSwiftSuperMultilevelInheritedMethodScopePublicLifecycle(t *testing.T) {
	f := newSwiftSuperAcceptanceFixture(t)
	f.reference(f.mainFile, f.caller, "super.run", 7)
	resolve := func() {
		t.Helper()
		if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
			t.Fatal(err)
		}
	}
	resolve()
	assertSwiftBinding(t, f.swiftScopeFixture, f.edge, f.baseRun, ResolutionStrategySwiftSuperScope)
	middle := f.symbol(f.mainFile, "Middle", "", "class", "", false)
	swiftSetRange(t, f.swiftScopeFixture, middle, 10, 1, 15, 20)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET target_qualified_name='Middle' WHERE child_qualified_name='Child'`); err != nil {
		t.Fatal(err)
	}
	swiftSuperclass(t, f.swiftScopeFixture, f.mainFile, "Middle", "Base")
	resolve()
	assertSwiftBinding(t, f.swiftScopeFixture, f.edge, f.baseRun, ResolutionStrategySwiftSuperMultilevelInheritedMethodScope)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET target_qualified_name='Base' WHERE child_qualified_name='Child'`); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM swift_inheritance_relations WHERE child_qualified_name='Middle'`); err != nil {
		t.Fatal(err)
	}
	resolve()
	assertSwiftBinding(t, f.swiftScopeFixture, f.edge, f.baseRun, ResolutionStrategySwiftSuperScope)
}

func TestSwiftSuperMultilevelInheritedMethodScopeRelationAndProofLifecycle(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*swiftSuperAcceptanceFixture)
	}{
		{"missing_first", func(f *swiftSuperAcceptanceFixture) {
			f.store.db.ExecContext(f.ctx, `DELETE FROM swift_inheritance_relations WHERE child_qualified_name='Child'`)
		}},
		{"missing_intermediate", func(f *swiftSuperAcceptanceFixture) {
			f.store.db.ExecContext(f.ctx, `DELETE FROM swift_inheritance_relations WHERE child_qualified_name='Middle'`)
		}},
		{"duplicate_first", func(f *swiftSuperAcceptanceFixture) {
			swiftSuperclass(f.t, f.swiftScopeFixture, f.mainFile, "Child", "Other")
		}},
		{"duplicate_intermediate", func(f *swiftSuperAcceptanceFixture) {
			swiftSuperclass(f.t, f.swiftScopeFixture, f.mainFile, "Middle", "Other")
		}},
		{"unproven_first", func(f *swiftSuperAcceptanceFixture) {
			f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET relation_kind='unproven' WHERE child_qualified_name='Child'`)
		}},
		{"unproven_intermediate", func(f *swiftSuperAcceptanceFixture) {
			f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET relation_kind='unproven' WHERE child_qualified_name='Middle'`)
		}},
		{"generic_first", func(f *swiftSuperAcceptanceFixture) {
			f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET is_generic=1 WHERE child_qualified_name='Child'`)
		}},
		{"generic_intermediate", func(f *swiftSuperAcceptanceFixture) {
			f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET is_generic=1 WHERE child_qualified_name='Middle'`)
		}},
		{"constrained_first", func(f *swiftSuperAcceptanceFixture) {
			f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET is_constrained=1 WHERE child_qualified_name='Child'`)
		}},
		{"constrained_intermediate", func(f *swiftSuperAcceptanceFixture) {
			f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET is_constrained=1 WHERE child_qualified_name='Middle'`)
		}},
		{"cycle", func(f *swiftSuperAcceptanceFixture) {
			f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET target_qualified_name='Child' WHERE child_qualified_name='Middle'`)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := newSwiftSuperMultilevelAcceptanceFixture(t)
			tc.mutate(f)
			if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
				t.Fatal(err)
			}
			assertSwiftEdgeUnresolved(t, f.swiftScopeFixture, f.edge)
		})
	}
}

func TestSwiftSuperMultilevelInheritedMethodScopeNamesAndStats(t *testing.T) {
	for _, tc := range []struct {
		name, strategy string
		resolved       bool
	}{
		{"multilevel", ResolutionStrategySwiftSuperMultilevelInheritedMethodScope, true},
		{"direct", ResolutionStrategySwiftSuperScope, true},
		{"intermediate_blocker", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := newSwiftSuperMultilevelAcceptanceFixture(t)
			if tc.name == "direct" {
				f.store.db.ExecContext(f.ctx, `DELETE FROM swift_inheritance_relations WHERE child_qualified_name='Middle'`)
				f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET target_qualified_name='Base' WHERE child_qualified_name='Child'`)
			} else if tc.name == "intermediate_blocker" {
				f.blocker(f.mainFile, "Middle", "run", graph.ScopeImportSwiftMemberValue, false)
			}
			stats := f.resolveNames("super.run")
			if stats.TargetsSelected != 1 || stats.TargetsResolved != btoi(tc.resolved) || stats.TargetsUnresolved != btoi(!tc.resolved) {
				t.Fatalf("stats=%+v", stats)
			}
			if tc.resolved {
				assertSwiftEdgeMetadata(t, f.swiftScopeFixture, f.edge, f.baseRun, tc.strategy)
			} else {
				assertSwiftEdgeUnresolved(t, f.swiftScopeFixture, f.edge)
			}
		})
	}
}

func TestSwiftSuperMultilevelInheritedMethodScopeUpgradeRepair(t *testing.T) {
	f, _ := newSwiftSuperMultilevelAcceptanceFixture(t)
	for _, repair := range resolverRepairs {
		if repair.key != swiftSuperMultilevelInheritedMethodRepairSettingKey {
			if err := f.store.markRepairDone(f.ctx, repair.key, f.repoID); err != nil {
				t.Fatal(err)
			}
		}
	}
	assertSwiftEdgeUnresolved(t, f.swiftScopeFixture, f.edge)
	refID := swiftReferenceID(t, f.swiftScopeFixture, f.edge)
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT value FROM settings WHERE key=?`, swiftSuperMultilevelInheritedMethodRepairSettingKey+"."+strconv.FormatInt(f.repoID, 10)).Scan(new(string)); err == nil {
		t.Fatal("P22.81 marker unexpectedly present")
	}
	run, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID)
	if err != nil || !run {
		t.Fatalf("first repair=(%v,%v)", run, err)
	}
	assertSwiftBinding(t, f.swiftScopeFixture, f.edge, f.baseRun, ResolutionStrategySwiftSuperMultilevelInheritedMethodScope)
	var marker string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT value FROM settings WHERE key=?`, swiftSuperMultilevelInheritedMethodRepairSettingKey+"."+strconv.FormatInt(f.repoID, 10)).Scan(&marker); err != nil || marker != "1" {
		t.Fatalf("marker=%q err=%v", marker, err)
	}
	firstDst, firstStrategy, firstConfidence := edgeState(t, f.swiftScopeFixture, f.edge)
	firstRef := swiftReferenceSymbol(t, f.swiftScopeFixture, refID)
	run, err = f.store.RepairResolverBindingsOnce(f.ctx, f.repoID)
	if err != nil || run {
		t.Fatalf("second repair=(%v,%v)", run, err)
	}
	secondDst, secondStrategy, secondConfidence := edgeState(t, f.swiftScopeFixture, f.edge)
	if firstDst != secondDst || firstStrategy != secondStrategy || firstConfidence != secondConfidence || firstRef != swiftReferenceSymbol(t, f.swiftScopeFixture, refID) {
		t.Fatal("second repair changed one of the five persisted fields")
	}

	resetMarker := func() {
		t.Helper()
		if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM settings WHERE key=?`, swiftSuperMultilevelInheritedMethodRepairSettingKey+"."+strconv.FormatInt(f.repoID, 10)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET relation_kind='unproven' WHERE child_qualified_name='Middle'`); err != nil {
		t.Fatal(err)
	}
	resetMarker()
	if run, err = f.store.RepairResolverBindingsOnce(f.ctx, f.repoID); err != nil || !run {
		t.Fatalf("unsafe repair=(%v,%v)", run, err)
	}
	assertSwiftEdgeUnresolved(t, f.swiftScopeFixture, f.edge)
	assertSwiftReferenceCleared(t, f.swiftScopeFixture, refID)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET relation_kind='superclass' WHERE child_qualified_name='Middle'`); err != nil {
		t.Fatal(err)
	}
	middleRun := f.symbol(f.mainFile, "run", "Middle", "function", "run()", false)
	swiftSetRange(t, f.swiftScopeFixture, middleRun, 11, 1, 11, 20)
	f.arity(middleRun, 0, 0)
	swiftFact(t, f.swiftScopeFixture, f.mainFile, middleRun, "instance")
	resetMarker()
	if run, err = f.store.RepairResolverBindingsOnce(f.ctx, f.repoID); err != nil || !run {
		t.Fatalf("rebind repair=(%v,%v)", run, err)
	}
	assertSwiftBinding(t, f.swiftScopeFixture, f.edge, middleRun, ResolutionStrategySwiftSuperScope)
	if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM swift_declaration_facts WHERE symbol_id=?`, middleRun); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET is_static=1 WHERE id=?`, f.baseRun); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET dispatch_kind='static' WHERE symbol_id=?`, f.baseRun); err != nil {
		t.Fatal(err)
	}
	resetMarker()
	if run, err = f.store.RepairResolverBindingsOnce(f.ctx, f.repoID); err != nil || !run {
		t.Fatalf("malformed repair=(%v,%v)", run, err)
	}
	assertSwiftEdgeUnresolved(t, f.swiftScopeFixture, f.edge)
}

func TestSwiftSuperMultilevelInheritedMethodScopeCandidateBlockerAndSourceLifecycle(t *testing.T) {
	f, _ := newSwiftSuperMultilevelAcceptanceFixture(t)
	resolve := func() {
		t.Helper()
		if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
			t.Fatal(err)
		}
	}
	resolve()
	assertSwiftBinding(t, f.swiftScopeFixture, f.edge, f.baseRun, ResolutionStrategySwiftSuperMultilevelInheritedMethodScope)

	middleRun := f.symbol(f.mainFile, "run", "Middle", "function", "run()", false)
	swiftSetRange(t, f.swiftScopeFixture, middleRun, 11, 1, 11, 20)
	f.arity(middleRun, 0, 0)
	swiftFact(t, f.swiftScopeFixture, f.mainFile, middleRun, "static")
	f.store.db.ExecContext(f.ctx, `UPDATE symbols SET is_static=1 WHERE id=?`, middleRun)
	resolve()
	assertSwiftEdgeUnresolved(t, f.swiftScopeFixture, f.edge)
	f.store.db.ExecContext(f.ctx, `UPDATE symbols SET is_static=0 WHERE id=?`, middleRun)
	f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET dispatch_kind='instance' WHERE symbol_id=?`, middleRun)
	resolve()
	assertSwiftBinding(t, f.swiftScopeFixture, f.edge, middleRun, ResolutionStrategySwiftSuperScope)
	f.store.db.ExecContext(f.ctx, `UPDATE symbols SET name='gone',qualified_name='Middle.gone' WHERE id=?`, middleRun)
	resolve()
	assertSwiftBinding(t, f.swiftScopeFixture, f.edge, f.baseRun, ResolutionStrategySwiftSuperMultilevelInheritedMethodScope)

	f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET is_final=1 WHERE symbol_id=?`, f.baseRun)
	resolve()
	assertSwiftBinding(t, f.swiftScopeFixture, f.edge, f.baseRun, ResolutionStrategySwiftSuperMultilevelInheritedMethodScope)
	f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET is_final=0 WHERE symbol_id=?`, f.baseRun)
	resolve()
	assertSwiftBinding(t, f.swiftScopeFixture, f.edge, f.baseRun, ResolutionStrategySwiftSuperMultilevelInheritedMethodScope)

	blockFile := f.file("Blocker.swift")
	f.blocker(blockFile, "Middle", "run", graph.ScopeImportSwiftMemberValue, false)
	resolve()
	assertSwiftEdgeUnresolved(t, f.swiftScopeFixture, f.edge)
	f.store.db.ExecContext(f.ctx, `UPDATE files SET is_deleted=1 WHERE id=?`, blockFile)
	resolve()
	assertSwiftBinding(t, f.swiftScopeFixture, f.edge, f.baseRun, ResolutionStrategySwiftSuperMultilevelInheritedMethodScope)
	f.store.db.ExecContext(f.ctx, `UPDATE files SET is_deleted=0 WHERE id=?`, blockFile)
	resolve()
	assertSwiftEdgeUnresolved(t, f.swiftScopeFixture, f.edge)
	f.store.db.ExecContext(f.ctx, `UPDATE files SET is_deleted=1 WHERE id=?`, blockFile)
	f.blocker(f.mainFile, "Base", "run", graph.ScopeImportSwiftMemberValue, true)
	resolve()
	assertSwiftBinding(t, f.swiftScopeFixture, f.edge, f.baseRun, ResolutionStrategySwiftSuperMultilevelInheritedMethodScope)

	f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET dispatch_kind='static' WHERE symbol_id=?`, f.caller)
	resolve()
	assertSwiftEdgeUnresolved(t, f.swiftScopeFixture, f.edge)
	f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET dispatch_kind='instance' WHERE symbol_id=?`, f.caller)
	resolve()
	assertSwiftBinding(t, f.swiftScopeFixture, f.edge, f.baseRun, ResolutionStrategySwiftSuperMultilevelInheritedMethodScope)
	if _, err := f.store.db.ExecContext(f.ctx, `INSERT INTO swift_declaration_facts(repo_id,file_id,symbol_id,dispatch_kind) VALUES(?,?,?,'instance')`, f.repoID, f.mainFile, f.caller); err != nil {
		t.Fatal(err)
	}
	resolve()
	assertSwiftEdgeUnresolved(t, f.swiftScopeFixture, f.edge)
}

func TestSwiftSuperMultilevelInheritedMethodScopeDeletedTargetBlocker(t *testing.T) {
	f, _ := newSwiftSuperMultilevelAcceptanceFixture(t)
	blockerFile := f.file("BaseBlocker.swift")
	f.blocker(blockerFile, "Base", "run", graph.ScopeImportSwiftMemberValue, false)
	resolve := func() {
		t.Helper()
		if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"BaseBlocker.swift"}); err != nil {
			t.Fatal(err)
		}
	}
	resolve()
	assertSwiftEdgeUnresolved(t, f.swiftScopeFixture, f.edge)
	f.store.db.ExecContext(f.ctx, `UPDATE files SET is_deleted=1 WHERE id=?`, blockerFile)
	resolve()
	assertSwiftBinding(t, f.swiftScopeFixture, f.edge, f.baseRun, ResolutionStrategySwiftSuperMultilevelInheritedMethodScope)
	f.store.db.ExecContext(f.ctx, `UPDATE files SET is_deleted=0 WHERE id=?`, blockerFile)
	resolve()
	assertSwiftEdgeUnresolved(t, f.swiftScopeFixture, f.edge)
}

func TestSwiftSuperMultilevelInheritedMethodScopeSelectorsAndTrailing(t *testing.T) {
	for _, tc := range []struct {
		name, evidence, signature string
		arity                     int
	}{
		{"zero", "swift:super", "run()", 0},
		{"unlabeled", "swift:super;labels=_", "run(_:)", 1},
		{"named", "swift:super;labels=id:", "run(id:)", 1},
		{"multiple", "swift:super;labels=id:,cache:", "run(id:,cache:)", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _ := newSwiftSuperMultilevelAcceptanceFixture(t)
			f.store.db.ExecContext(f.ctx, `UPDATE symbols SET signature=?,arity_min=?,arity_max=? WHERE id=?`, tc.signature, tc.arity, tc.arity, f.baseRun)
			f.store.db.ExecContext(f.ctx, `UPDATE edges SET evidence=?,call_arity=? WHERE id=?`, tc.evidence, tc.arity, f.edge)
			f.resolve()
			if dst, strategy, confidence := edgeState(t, f.swiftScopeFixture, f.edge); !dst.Valid || dst.Int64 != f.baseRun || strategy != ResolutionStrategySwiftSuperMultilevelInheritedMethodScope || confidence != ResolutionConfidenceHigh {
				t.Fatalf("state=(%v,%q,%q)", dst, strategy, confidence)
			}
		})
	}
	t.Run("trailing closure", func(t *testing.T) {
		f, _ := newSwiftSuperMultilevelAcceptanceFixture(t)
		f.store.db.ExecContext(f.ctx, `UPDATE symbols SET name='perform',qualified_name='Base.perform',signature='perform(body:)',arity_min=1,arity_max=1 WHERE id=?`, f.baseRun)
		f.store.db.ExecContext(f.ctx, `UPDATE edges SET dst_name='super.perform',evidence='swift:super;trailing_labels=_',call_arity=1 WHERE id=?`, f.edge)
		f.resolve()
		if dst, strategy, confidence := edgeState(t, f.swiftScopeFixture, f.edge); !dst.Valid || dst.Int64 != f.baseRun || strategy != ResolutionStrategySwiftSuperMultilevelInheritedMethodScope || confidence != ResolutionConfidenceHigh {
			t.Fatalf("state=(%v,%q,%q)", dst, strategy, confidence)
		}
	})
}

func TestSwiftSuperMultilevelInheritedMethodScopeHardenedStress(t *testing.T) {
	const perCase = 10
	names := []string{
		"two_hop", "three_hop", "four_hop", "final_target", "qualified_owners", "reverse_subclass", "target_bounded",
		"selector_zero", "selector_unlabeled", "selector_named", "selector_multi", "trailing", "incompatible_intermediate_overload", "irrelevant_static_blocker", "deleted_intermediate_blocker", "deleted_target_blocker", "test_only_blocker_prod_caller",
		"direct_super", "intermediate_valid_instance", "intermediate_valid_final_instance",
		"source_static_bit", "source_dispatch_static", "source_dispatch_class", "source_missing_fact", "source_duplicate_fact", "source_outside_class_body", "source_wrong_owner",
		"missing_first", "missing_intermediate", "duplicate_first_superclass", "duplicate_intermediate_superclass", "unproven_first", "unproven_intermediate", "generic_first", "generic_intermediate", "constrained_first", "constrained_intermediate", "competing_first_superclass", "competing_intermediate_superclass", "cycle", "cross_first_relation", "cross_intermediate_relation",
		"missing_child", "duplicate_child", "missing_middle", "duplicate_middle", "missing_base", "duplicate_base", "wrong_middle_kind", "wrong_base_kind", "cross_middle", "cross_base",
		"intermediate_static", "intermediate_class", "intermediate_malformed", "intermediate_missing_fact", "intermediate_duplicate_fact", "intermediate_private", "intermediate_extension_owned", "intermediate_known_incompatible_overload",
		"second_valid_instance", "static_competitor", "class_competitor", "malformed_competitor", "missing_fact_competitor", "duplicate_fact_competitor", "private_competitor", "extension_owned_competitor",
		"active_intermediate_blocker", "active_target_blocker", "deleted_intermediate_blocker_2", "deleted_target_blocker_2", "private_target", "fileprivate_target", "public_target",
		"trailing_second_valid_target", "trailing_static_competitor", "trailing_malformed_competitor", "trailing_intermediate_unsafe", "trailing_intermediate_blocker",
	}
	if len(names) < 55 {
		t.Fatalf("categories=%d, want >=55", len(names))
	}
	unsafe := map[string]bool{}
	for _, name := range []string{
		"source_static_bit", "source_dispatch_static", "source_dispatch_class", "source_missing_fact", "source_duplicate_fact", "source_outside_class_body", "source_wrong_owner",
		"missing_first", "missing_intermediate", "duplicate_first_superclass", "duplicate_intermediate_superclass", "unproven_first", "unproven_intermediate", "generic_first", "generic_intermediate", "constrained_first", "constrained_intermediate", "competing_first_superclass", "competing_intermediate_superclass", "cycle", "cross_first_relation", "cross_intermediate_relation",
		"missing_child", "duplicate_child", "missing_middle", "duplicate_middle", "missing_base", "duplicate_base", "wrong_middle_kind", "wrong_base_kind", "cross_middle", "cross_base", "intermediate_static", "intermediate_class", "intermediate_malformed", "intermediate_missing_fact", "intermediate_duplicate_fact", "intermediate_private", "intermediate_extension_owned", "second_valid_instance", "static_competitor", "class_competitor", "malformed_competitor", "missing_fact_competitor", "duplicate_fact_competitor", "private_competitor", "extension_owned_competitor", "active_intermediate_blocker", "active_target_blocker", "private_target", "trailing_second_valid_target", "trailing_static_competitor", "trailing_malformed_competitor", "trailing_intermediate_unsafe", "trailing_intermediate_blocker",
	} {
		unsafe[name] = true
	}

	mutate := func(name string, f *swiftSuperAcceptanceFixture, middle int64) {
		q := f.store.db
		if name == "three_hop" || name == "four_hop" {
			m2Name := name + "Middle2"
			m2 := f.symbol(f.mainFile, m2Name, "", "class", "", false)
			if name == "four_hop" {
				m3Name := name + "Middle3"
				f.symbol(f.mainFile, m3Name, "", "class", "", false)
				q.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET target_qualified_name=? WHERE child_qualified_name='Child'`, m3Name)
				swiftSuperclass(f.t, f.swiftScopeFixture, f.mainFile, m3Name, m2Name)
				swiftSuperclass(f.t, f.swiftScopeFixture, f.mainFile, m2Name, "Middle")
			} else {
				q.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET target_qualified_name=? WHERE child_qualified_name='Child'`, m2Name)
				swiftSuperclass(f.t, f.swiftScopeFixture, f.mainFile, m2Name, "Middle")
			}
			_ = m2
		}
		if name == "final_target" {
			q.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET is_final=1 WHERE symbol_id=?`, f.baseRun)
		}
		if name == "reverse_subclass" {
			child := f.symbol(f.mainFile, "GrandChild", "", "class", "", false)
			swiftSuperclass(f.t, f.swiftScopeFixture, f.mainFile, "GrandChild", "Child")
			_ = child
		}
		if name == "target_bounded" {
			swiftSuperclass(f.t, f.swiftScopeFixture, f.mainFile, "Base", "ExternalRoot")
		}
		if name == "qualified_owners" {
			q.ExecContext(f.ctx, `UPDATE symbols SET qualified_name=CASE qualified_name WHEN 'Base' THEN 'Outer.Base' WHEN 'Child' THEN 'Outer.Child' WHEN 'Middle' THEN 'Outer.Middle' WHEN 'Base.run' THEN 'Outer.Base.run' WHEN 'Child.f' THEN 'Outer.Child.f' END,container_name=CASE container_name WHEN 'Base' THEN 'Outer.Base' WHEN 'Child' THEN 'Outer.Child' ELSE container_name END WHERE qualified_name IN ('Base','Child','Middle','Base.run','Child.f')`)
			q.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET child_qualified_name='Outer.Child',target_qualified_name='Outer.Middle' WHERE child_qualified_name='Child'`)
			q.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET child_qualified_name='Outer.Middle',target_qualified_name='Outer.Base' WHERE child_qualified_name='Middle'`)
			q.ExecContext(f.ctx, `UPDATE edges SET dst_name='super.run' WHERE dst_name='super.run'`)
		}
		if len(name) >= 7 && name[:7] == "selector" {
			sig, ev, arity := "run()", "swift:super", 0
			if name == "selector_unlabeled" {
				sig, ev, arity = "run(_:)", "swift:super;labels=_", 1
			}
			if name == "selector_named" {
				sig, ev, arity = "run(id:)", "swift:super;labels=id:", 1
			}
			if name == "selector_multi" {
				sig, ev, arity = "run(id:,cache:)", "swift:super;labels=id:,cache:", 2
			}
			q.ExecContext(f.ctx, `UPDATE symbols SET signature=?,arity_min=?,arity_max=? WHERE id=?`, sig, arity, arity, f.baseRun)
			q.ExecContext(f.ctx, `UPDATE edges SET evidence=?,call_arity=? WHERE dst_name='super.run'`, ev, arity)
		}
		if name == "trailing" {
			q.ExecContext(f.ctx, `UPDATE symbols SET name='perform',qualified_name='Base.perform',signature='perform(body:)',arity_min=1,arity_max=1 WHERE id=?`, f.baseRun)
			q.ExecContext(f.ctx, `UPDATE edges SET dst_name='super.perform',evidence='swift:super;trailing_labels=_',call_arity=1 WHERE dst_name='super.run'`)
			q.ExecContext(f.ctx, `UPDATE references_tbl SET name='super.perform',qualified_name='super.perform' WHERE repo_id=?`, f.repoID)
		}
		if name == "direct_super" {
			q.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET target_qualified_name='Base' WHERE child_qualified_name='Child'`)
			q.ExecContext(f.ctx, `DELETE FROM swift_inheritance_relations WHERE child_qualified_name='Middle'`)
		}
		if name == "intermediate_valid_instance" || name == "intermediate_valid_final_instance" {
			id := f.symbol(f.mainFile, "run", "Middle", "function", "run()", false)
			swiftSetRange(f.t, f.swiftScopeFixture, id, 11, 1, 11, 15)
			f.arity(id, 0, 0)
			swiftFact(f.t, f.swiftScopeFixture, f.mainFile, id, "instance")
			if name == "intermediate_valid_final_instance" {
				q.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET is_final=1 WHERE symbol_id=?`, id)
			}
		}
		if name == "incompatible_intermediate_overload" || name == "intermediate_known_incompatible_overload" {
			id := f.symbol(f.mainFile, "run", "Middle", "function", "run(id:)", false)
			f.arity(id, 1, 1)
			swiftFact(f.t, f.swiftScopeFixture, f.mainFile, id, "instance")
		}
		if len(name) >= 7 && name[:7] == "source_" {
			switch name {
			case "source_static_bit":
				q.ExecContext(f.ctx, `UPDATE symbols SET is_static=1 WHERE id=?`, f.caller)
			case "source_dispatch_static":
				q.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET dispatch_kind='static' WHERE symbol_id=?`, f.caller)
			case "source_dispatch_class":
				q.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET dispatch_kind='class' WHERE symbol_id=?`, f.caller)
			case "source_missing_fact":
				q.ExecContext(f.ctx, `DELETE FROM swift_declaration_facts WHERE symbol_id=?`, f.caller)
			case "source_duplicate_fact":
				swiftFact(f.t, f.swiftScopeFixture, f.mainFile, f.caller, "instance")
			case "source_outside_class_body":
				swiftSetRange(f.t, f.swiftScopeFixture, f.caller, 300, 1, 301, 2)
			case "source_wrong_owner":
				q.ExecContext(f.ctx, `UPDATE symbols SET container_name='Other' WHERE id=?`, f.caller)
			}
		}
		if name == "missing_first" {
			q.ExecContext(f.ctx, `DELETE FROM swift_inheritance_relations WHERE child_qualified_name='Child'`)
		}
		if name == "missing_intermediate" {
			q.ExecContext(f.ctx, `DELETE FROM swift_inheritance_relations WHERE child_qualified_name='Middle'`)
		}
		if name == "duplicate_first_superclass" {
			swiftSuperclass(f.t, f.swiftScopeFixture, f.mainFile, "Child", "Other")
		}
		if name == "duplicate_intermediate_superclass" {
			swiftSuperclass(f.t, f.swiftScopeFixture, f.mainFile, "Middle", "Other")
		}
		if name == "unproven_first" {
			q.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET relation_kind='unproven' WHERE child_qualified_name='Child'`)
		}
		if name == "unproven_intermediate" {
			q.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET relation_kind='unproven' WHERE child_qualified_name='Middle'`)
		}
		if name == "generic_first" {
			q.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET is_generic=1 WHERE child_qualified_name='Child'`)
		}
		if name == "generic_intermediate" {
			q.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET is_generic=1 WHERE child_qualified_name='Middle'`)
		}
		if name == "constrained_first" {
			q.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET is_constrained=1 WHERE child_qualified_name='Child'`)
		}
		if name == "constrained_intermediate" {
			q.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET is_constrained=1 WHERE child_qualified_name='Middle'`)
		}
		if name == "competing_first_superclass" {
			swiftSuperclass(f.t, f.swiftScopeFixture, f.mainFile, "Child", "Other")
		}
		if name == "competing_intermediate_superclass" {
			swiftSuperclass(f.t, f.swiftScopeFixture, f.mainFile, "Middle", "Other")
		}
		if name == "cycle" {
			q.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET target_qualified_name='Child' WHERE child_qualified_name='Middle'`)
		}
		if len(name) >= 8 && name[:8] == "missing_" {
			if name == "missing_child" {
				q.ExecContext(f.ctx, `UPDATE symbols SET qualified_name='GoneChild' WHERE id=?`, f.child)
			}
			if name == "missing_middle" {
				q.ExecContext(f.ctx, `UPDATE symbols SET qualified_name='GoneMiddle' WHERE id=?`, middle)
			}
			if name == "missing_base" {
				q.ExecContext(f.ctx, `UPDATE symbols SET qualified_name='GoneBase' WHERE id=?`, f.base)
			}
		}
		if len(name) >= 10 && name[:10] == "duplicate_" && name != "duplicate_first_superclass" && name != "duplicate_intermediate_superclass" {
			qname := "Child"
			if name == "duplicate_middle" {
				qname = "Middle"
			}
			if name == "duplicate_base" {
				qname = "Base"
			}
			id := f.symbol(f.mainFile, qname, "", "class", "", false)
			if qname == "Child" {
				swiftSetRange(f.t, f.swiftScopeFixture, id, 5, 1, 9, 2)
			}
			if qname == "Middle" {
				swiftSetRange(f.t, f.swiftScopeFixture, id, 10, 1, 15, 20)
			}
			if qname == "Base" {
				swiftSetRange(f.t, f.swiftScopeFixture, id, 1, 1, 3, 2)
			}
			_ = id
		}
		if name == "wrong_middle_kind" {
			q.ExecContext(f.ctx, `UPDATE symbols SET kind='struct' WHERE id=?`, middle)
		}
		if name == "wrong_base_kind" {
			q.ExecContext(f.ctx, `UPDATE symbols SET kind='struct' WHERE id=?`, f.base)
		}
		if name == "intermediate_static" || name == "intermediate_class" || name == "intermediate_malformed" || name == "intermediate_missing_fact" || name == "intermediate_duplicate_fact" || name == "intermediate_private" || name == "intermediate_extension_owned" {
			id := f.symbol(f.mainFile, "run", "Middle", "function", "run()", false)
			f.arity(id, 0, 0)
			if name != "intermediate_missing_fact" {
				dispatch := "instance"
				if name == "intermediate_static" || name == "intermediate_malformed" {
					dispatch = "static"
				} else if name == "intermediate_class" {
					dispatch = "class"
				}
				swiftFact(f.t, f.swiftScopeFixture, f.mainFile, id, dispatch)
			}
			if name == "intermediate_duplicate_fact" {
				swiftFact(f.t, f.swiftScopeFixture, f.mainFile, id, "instance")
			}
			if name == "intermediate_static" || name == "intermediate_class" || name == "intermediate_malformed" {
				q.ExecContext(f.ctx, `UPDATE symbols SET is_static=1 WHERE id=?`, id)
			}
			if name == "intermediate_private" {
				q.ExecContext(f.ctx, `UPDATE symbols SET visibility='private' WHERE id=?`, id)
			}
			if name == "intermediate_extension_owned" {
				swiftSetRange(f.t, f.swiftScopeFixture, id, 300, 1, 300, 2)
			}
		}
		if name == "second_valid_instance" || name == "static_competitor" || name == "class_competitor" || name == "malformed_competitor" || name == "missing_fact_competitor" || name == "duplicate_fact_competitor" || name == "private_competitor" || name == "extension_owned_competitor" {
			id := f.symbol(f.mainFile, "run", "Base", "function", "run()", false)
			swiftSetRange(f.t, f.swiftScopeFixture, id, 2, 1, 2, 15)
			f.arity(id, 0, 0)
			if name != "missing_fact_competitor" {
				swiftFact(f.t, f.swiftScopeFixture, f.mainFile, id, "instance")
			}
			if name == "duplicate_fact_competitor" {
				swiftFact(f.t, f.swiftScopeFixture, f.mainFile, id, "instance")
			}
			if name == "static_competitor" || name == "class_competitor" || name == "malformed_competitor" {
				q.ExecContext(f.ctx, `UPDATE symbols SET is_static=1 WHERE id=?`, id)
			}
			if name == "private_competitor" {
				q.ExecContext(f.ctx, `UPDATE symbols SET visibility='private' WHERE id=?`, id)
			}
			if name == "extension_owned_competitor" {
				swiftSetRange(f.t, f.swiftScopeFixture, id, 300, 1, 300, 2)
			}
		}
		if name == "active_intermediate_blocker" || name == "active_target_blocker" || name == "deleted_intermediate_blocker" || name == "deleted_target_blocker" || name == "deleted_intermediate_blocker_2" || name == "deleted_target_blocker_2" || name == "irrelevant_static_blocker" || name == "test_only_blocker_prod_caller" || name == "trailing_intermediate_blocker" {
			fileName := name + ".swift"
			if name == "test_only_blocker_prod_caller" {
				fileName = "Tests/" + fileName
			}
			file := f.file(fileName)
			owner := "Middle"
			blockerName := "run"
			if name == "trailing_intermediate_blocker" {
				blockerName = "perform"
			}
			if name == "active_target_blocker" || name == "deleted_target_blocker" || name == "deleted_target_blocker_2" {
				owner = "Base"
			}
			f.blocker(file, owner, blockerName, graph.ScopeImportSwiftMemberValue, name == "irrelevant_static_blocker")
			if len(name) >= 8 && name[:8] == "deleted_" {
				q.ExecContext(f.ctx, `UPDATE files SET is_deleted=1 WHERE id=?`, file)
			}
		}
		if name == "private_target" {
			q.ExecContext(f.ctx, `UPDATE symbols SET visibility='private' WHERE id=?`, f.baseRun)
		}
		if name == "fileprivate_target" {
			q.ExecContext(f.ctx, `UPDATE symbols SET visibility='fileprivate' WHERE id=?`, f.baseRun)
		}
		if name == "public_target" {
			q.ExecContext(f.ctx, `UPDATE symbols SET visibility='public' WHERE id=?`, f.baseRun)
		}
		if name == "cross_first_relation" || name == "cross_intermediate_relation" {
			file := f.file(name + ".swift")
			child := "Child"
			if name == "cross_intermediate_relation" {
				child = "Middle"
			}
			q.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET file_id=? WHERE child_qualified_name=?`, file, child)
		}
		if name == "cross_middle" || name == "cross_base" {
			file := f.file(name + ".swift")
			id := middle
			if name == "cross_base" {
				id = f.base
			}
			q.ExecContext(f.ctx, `UPDATE symbols SET file_id=? WHERE id=?`, file, id)
		}
		if len(name) >= 9 && name[:9] == "trailing_" {
			q.ExecContext(f.ctx, `UPDATE symbols SET name='perform',qualified_name='Base.perform',signature='perform(body:)',arity_min=1,arity_max=1 WHERE id=?`, f.baseRun)
			q.ExecContext(f.ctx, `UPDATE edges SET dst_name='super.perform',evidence='swift:super;trailing_labels=_',call_arity=1 WHERE evidence LIKE 'swift:super%'`)
			q.ExecContext(f.ctx, `UPDATE references_tbl SET name='super.perform',qualified_name='super.perform' WHERE repo_id=?`, f.repoID)
			if name == "trailing_second_valid_target" || name == "trailing_static_competitor" || name == "trailing_malformed_competitor" {
				id := f.symbol(f.mainFile, "perform", "Base", "function", "perform(body:)", false)
				swiftSetRange(f.t, f.swiftScopeFixture, id, 2, 1, 2, 15)
				f.arity(id, 1, 1)
				if name != "trailing_malformed_competitor" {
					swiftFact(f.t, f.swiftScopeFixture, f.mainFile, id, "instance")
				}
				if name == "trailing_static_competitor" || name == "trailing_malformed_competitor" {
					q.ExecContext(f.ctx, `UPDATE symbols SET is_static=1 WHERE id=?`, id)
				}
			} else if name == "trailing_intermediate_unsafe" {
				id := f.symbol(f.mainFile, "perform", "Middle", "function", "perform(body:)", false)
				swiftSetRange(f.t, f.swiftScopeFixture, id, 11, 1, 11, 15)
				f.arity(id, 1, 1)
				swiftFact(f.t, f.swiftScopeFixture, f.mainFile, id, "instance")
				q.ExecContext(f.ctx, `UPDATE symbols SET is_static=1 WHERE id=?`, id)
			} else if name == "trailing_intermediate_blocker" {
				file := f.file("trailing-blocker.swift")
				f.blocker(file, "Middle", "perform", graph.ScopeImportSwiftMemberValue, false)
			}
		}
	}
	state := func(f *swiftScopeFixture) stressState {
		got := stressState{strategies: map[string]int{}}
		f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM edges WHERE repo_id=? AND evidence LIKE 'swift:super%'`, f.repoID).Scan(&got.total)
		f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM edges WHERE repo_id=? AND evidence LIKE 'swift:super%' AND dst_symbol_id IS NOT NULL`, f.repoID).Scan(&got.resolved)
		got.unresolved = got.total - got.resolved
		f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM edges WHERE repo_id=? AND evidence LIKE 'swift:super%' AND dst_symbol_id IS NULL AND (resolution_strategy<>'' OR resolution_confidence<>'')`, f.repoID).Scan(&got.badMetadata)
		rows, _ := f.store.db.QueryContext(f.ctx, `SELECT resolution_strategy,COUNT(*) FROM edges WHERE repo_id=? AND evidence LIKE 'swift:super%' AND dst_symbol_id IS NOT NULL GROUP BY resolution_strategy`, f.repoID)
		if rows != nil {
			defer rows.Close()
			for rows.Next() {
				var k string
				var n int
				rows.Scan(&k, &n)
				got.strategies[k] = n
			}
		}
		return got
	}
	want := stressState{total: len(names) * perCase, strategies: map[string]int{ResolutionStrategySwiftSuperMultilevelInheritedMethodScope: 0, ResolutionStrategySwiftSuperScope: 0}}
	var fixtures []*swiftScopeFixture
	var first stressState
	for _, name := range names {
		f := newSwiftSuperAcceptanceFixture(t)
		middle := f.symbol(f.mainFile, "Middle", "", "class", "", false)
		swiftSetRange(t, f.swiftScopeFixture, middle, 10, 1, 15, 20)
		f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET target_qualified_name='Middle' WHERE child_qualified_name='Child'`)
		swiftSuperclass(t, f.swiftScopeFixture, f.mainFile, "Middle", "Base")
		f.reference(f.mainFile, f.caller, "super.run", 7)
		edges := []int64{f.edge}
		for i := 1; i < perCase; i++ {
			edges = append(edges, f.call(f.mainFile, f.caller, "super.run", "swift:super", 0, 7+i))
		}
		mutate(name, f, middle)
		f.resolve()
		got := state(f.swiftScopeFixture)
		fixtures = append(fixtures, f.swiftScopeFixture)
		wantResolved := !unsafe[name]
		wantStrategy := ResolutionStrategySwiftSuperMultilevelInheritedMethodScope
		if name == "direct_super" || name == "intermediate_valid_instance" || name == "intermediate_valid_final_instance" {
			wantStrategy = ResolutionStrategySwiftSuperScope
		}
		if wantResolved {
			want.resolved += perCase
			want.strategies[wantStrategy] += perCase
			if edges[0] == 0 {
				t.Fatal("missing representative")
			}
			wantTarget := f.baseRun
			if name == "intermediate_valid_instance" || name == "intermediate_valid_final_instance" {
				wantTarget = f.symbolID("Middle.run")
			}
			if dst, strategy, confidence := edgeState(t, f.swiftScopeFixture, edges[0]); !dst.Valid {
				t.Fatalf("%s unresolved state=(%v,%q,%q)", name, dst, strategy, confidence)
			}
			assertSwiftBinding(t, f.swiftScopeFixture, edges[0], wantTarget, wantStrategy)
		} else {
			want.unresolved += perCase
			assertSwiftEdgeUnresolved(t, f.swiftScopeFixture, edges[0])
			assertSwiftReferenceCleared(t, f.swiftScopeFixture, swiftReferenceID(t, f.swiftScopeFixture, edges[0]))
		}
		if !reflect.DeepEqual(got, wantForOne(got.total, wantResolved, wantStrategy)) {
			t.Fatalf("%s state=%+v", name, got)
		}
		f.resolve()
		if second := state(f.swiftScopeFixture); !reflect.DeepEqual(second, got) {
			t.Fatalf("%s not idempotent: first=%+v second=%+v", name, got, second)
		}
		first.total += got.total
		first.resolved += got.resolved
		first.unresolved += got.unresolved
		first.badMetadata += got.badMetadata
		for k, n := range got.strategies {
			first.strategies = appendStrategy(first.strategies, k, n)
		}
	}
	if !reflect.DeepEqual(first, want) {
		t.Fatalf("stress first=%+v want=%+v", first, want)
	}
	second := stressState{strategies: map[string]int{}}
	for _, f := range fixtures {
		got := state(f)
		second.total += got.total
		second.resolved += got.resolved
		second.unresolved += got.unresolved
		second.badMetadata += got.badMetadata
		for k, n := range got.strategies {
			second.strategies = appendStrategy(second.strategies, k, n)
		}
	}
	if !reflect.DeepEqual(second, first) {
		t.Fatalf("stress second=%+v first=%+v", second, first)
	}
}

func appendStrategy(m map[string]int, key string, n int) map[string]int {
	if m == nil {
		m = map[string]int{}
	}
	m[key] += n
	return m
}

func wantForOne(total int, resolved bool, strategy string) stressState {
	s := stressState{total: total, strategies: map[string]int{}}
	if resolved {
		s.resolved = total
		s.strategies[strategy] = total
	} else {
		s.unresolved = total
	}
	return s
}

func TestSwiftSuperScopeRejectsExtensionOnlyTargetAndGrandparent(t *testing.T) {
	f := newSwiftScopeFixture(t)
	base := f.symbol(f.mainFile, "Base", "", "class", "", false)
	extensionRun := f.symbol(f.mainFile, "run", "Base", "function", "run()", false)
	child := f.symbol(f.mainFile, "Child", "", "class", "", false)
	caller := f.symbol(f.mainFile, "f", "Child", "function", "f()", false)
	swiftSetRange(t, f, base, 1, 1, 3, 2)
	swiftSetRange(t, f, extensionRun, 10, 1, 10, 15)
	f.arity(extensionRun, 0, 0)
	swiftSetRange(t, f, child, 5, 1, 9, 2)
	swiftSetRange(t, f, caller, 6, 1, 8, 2)
	swiftSuperclass(t, f, f.mainFile, "Child", "Base")
	swiftFact(t, f, f.mainFile, extensionRun, "instance")
	swiftFact(t, f, f.mainFile, caller, "instance")
	edge := f.call(f.mainFile, caller, "super.run", "swift:super", 0, 7)
	f.resolve()
	if got := f.dst(edge); got.Valid {
		t.Fatalf("extension target resolved to %d", got.Int64)
	}
}

func TestParseSwiftSuperCallShapeStrict(t *testing.T) {
	valid := []struct {
		evidence, dst string
		arity         int64
	}{
		{"swift:super", "super.run", 0},
		{"swift:super;labels=id:", "super.run", 1},
		{"swift:super;trailing_labels=_", "super.run", 1},
		{"swift:super;labels=id:;trailing_labels=_", "super.run", 2},
		{"swift:super;trailing_labels=_,completion:", "super.run", 2},
	}
	for _, tc := range valid {
		if _, ok := parseSwiftSuperCallShape(tc.evidence, tc.dst, sql.NullInt64{Int64: tc.arity, Valid: true}); !ok {
			t.Errorf("parse(%q,%q) rejected", tc.evidence, tc.dst)
		}
	}
	invalid := []struct {
		evidence, dst string
		arity         int64
	}{
		{"swift:super;foo=x", "super.run", 0},
		{"swift:super;trailing_labels=completion:", "super.run", 1},
		{"swift:super;labels=id:;labels=name:", "super.run", 2},
		{"swift:super", "super.init", 0},
		{"swift:super", "super.foo.bar", 0},
		{"swift:super", "Super.run", 0},
	}
	for _, tc := range invalid {
		if _, ok := parseSwiftSuperCallShape(tc.evidence, tc.dst, sql.NullInt64{Int64: tc.arity, Valid: true}); ok {
			t.Errorf("parse(%q,%q) accepted", tc.evidence, tc.dst)
		}
	}
}

type swiftSuperAcceptanceFixture struct {
	*swiftScopeFixture
	base, baseRun, child, caller int64
	edge                         int64
}

func newSwiftSuperAcceptanceFixture(t *testing.T) *swiftSuperAcceptanceFixture {
	t.Helper()
	f := newSwiftScopeFixture(t)
	x := &swiftSuperAcceptanceFixture{swiftScopeFixture: f}
	x.base = f.symbol(f.mainFile, "Base", "", "class", "", false)
	x.baseRun = f.symbol(f.mainFile, "run", "Base", "function", "run()", false)
	x.child = f.symbol(f.mainFile, "Child", "", "class", "", false)
	x.caller = f.symbol(f.mainFile, "f", "Child", "function", "f()", false)
	swiftSetRange(t, f, x.base, 1, 1, 3, 2)
	swiftSetRange(t, f, x.baseRun, 2, 1, 2, 15)
	f.arity(x.baseRun, 0, 0)
	swiftSetRange(t, f, x.child, 5, 1, 9, 2)
	swiftSetRange(t, f, x.caller, 6, 1, 8, 2)
	swiftSuperclass(t, f, f.mainFile, "Child", "Base")
	swiftFact(t, f, f.mainFile, x.baseRun, "instance")
	swiftFact(t, f, f.mainFile, x.caller, "instance")
	x.edge = f.call(f.mainFile, x.caller, "super.run", "swift:super", 0, 7)
	return x
}

func newSwiftSuperMultilevelAcceptanceFixture(t *testing.T) (*swiftSuperAcceptanceFixture, int64) {
	t.Helper()
	f := newSwiftSuperAcceptanceFixture(t)
	middle := f.symbol(f.mainFile, "Middle", "", "class", "", false)
	swiftSetRange(t, f.swiftScopeFixture, middle, 10, 1, 15, 20)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET target_qualified_name='Middle' WHERE child_qualified_name='Child'`); err != nil {
		t.Fatal(err)
	}
	swiftSuperclass(t, f.swiftScopeFixture, f.mainFile, "Middle", "Base")
	f.reference(f.mainFile, f.caller, "super.run", 7)
	return f, middle
}

func TestSwiftSuperScopeDirectOnlyGrandparentAndIncrementalBase(t *testing.T) {
	f := newSwiftSuperAcceptanceFixture(t)
	a := f.symbol(f.mainFile, "A", "", "class", "", false)
	aRun := f.symbol(f.mainFile, "run", "A", "function", "run()", false)
	b := f.symbol(f.mainFile, "B", "", "class", "", false)
	swiftSetRange(t, f.swiftScopeFixture, a, 12, 1, 14, 2)
	swiftSetRange(t, f.swiftScopeFixture, aRun, 13, 1, 13, 15)
	f.arity(aRun, 0, 0)
	swiftSetRange(t, f.swiftScopeFixture, b, 16, 1, 18, 2)
	swiftSuperclass(t, f.swiftScopeFixture, f.mainFile, "B", "A")
	// Replace Child's direct relation with B, leaving B -> A as a real chain.
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET target_qualified_name='B' WHERE child_qualified_name='Child'`); err != nil {
		t.Fatal(err)
	}
	f.resolve()
	if got := f.dst(f.edge); got.Valid {
		t.Fatalf("grandparent method resolved to %d", got.Int64)
	}

	bRun := f.symbol(f.mainFile, "run", "B", "function", "run()", false)
	swiftSetRange(t, f.swiftScopeFixture, bRun, 17, 1, 17, 15)
	f.arity(bRun, 0, 0)
	swiftFact(t, f.swiftScopeFixture, f.mainFile, bRun, "instance")
	f.resolve()
	if got := f.dst(f.edge); !got.Valid || got.Int64 != bRun {
		t.Fatalf("direct B method dst=%v, want %d", got, bRun)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET name='gone',qualified_name='B.gone' WHERE id=?`, bRun); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
		t.Fatal(err)
	}
	if got := f.dst(f.edge); got.Valid {
		t.Fatalf("removed direct method resolved to %d", got.Int64)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET name='run',qualified_name='B.run' WHERE id=?`, bRun); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
		t.Fatal(err)
	}
	if got := f.dst(f.edge); !got.Valid || got.Int64 != bRun {
		t.Fatalf("restored direct method dst=%v, want %d", got, bRun)
	}
}

func TestSwiftSuperScopeSourceRangeAndDispatchFacts(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*swiftSuperAcceptanceFixture)
		bound bool
	}{
		{"outside nominal body", func(f *swiftSuperAcceptanceFixture) {
			swiftSetRange(f.t, f.swiftScopeFixture, f.caller, 20, 1, 22, 2)
		}, false},
		{"class dispatch", func(f *swiftSuperAcceptanceFixture) {
			f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET dispatch_kind='class' WHERE symbol_id=?`, f.caller)
		}, false},
		{"static dispatch", func(f *swiftSuperAcceptanceFixture) {
			f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET dispatch_kind='static' WHERE symbol_id=?`, f.caller)
		}, false},
		{"contradictory static bit", func(f *swiftSuperAcceptanceFixture) {
			f.store.db.ExecContext(f.ctx, `UPDATE symbols SET is_static=1 WHERE id=?`, f.caller)
		}, false},
		{"missing fact", func(f *swiftSuperAcceptanceFixture) {
			f.store.db.ExecContext(f.ctx, `DELETE FROM swift_declaration_facts WHERE symbol_id=?`, f.caller)
		}, false},
		{"duplicate facts", func(f *swiftSuperAcceptanceFixture) {
			swiftFact(f.t, f.swiftScopeFixture, f.mainFile, f.caller, "instance")
		}, false},
		{"instance", func(*swiftSuperAcceptanceFixture) {}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwiftSuperAcceptanceFixture(t)
			tc.setup(f)
			f.resolve()
			got := f.dst(f.edge).Valid
			if got != tc.bound {
				t.Fatalf("bound=%v, want %v", got, tc.bound)
			}
			if tc.name == "outside nominal body" {
				swiftSetRange(t, f.swiftScopeFixture, f.caller, 6, 1, 8, 2)
				if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
					t.Fatal(err)
				}
				if got := f.dst(f.edge); !got.Valid || got.Int64 != f.baseRun {
					t.Fatalf("moved source dst=%v, want %d", got, f.baseRun)
				}
			}
		})
	}
}

func TestSwiftSuperScopeRelationRefusalMatrix(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*swiftSuperAcceptanceFixture)
	}{
		{"no superclass", func(f *swiftSuperAcceptanceFixture) {
			f.store.db.ExecContext(f.ctx, `DELETE FROM swift_inheritance_relations WHERE child_qualified_name='Child'`)
		}},
		{"duplicate superclass", func(f *swiftSuperAcceptanceFixture) {
			swiftSuperclass(f.t, f.swiftScopeFixture, f.mainFile, "Child", "Other")
		}},
		{"unproven relation", func(f *swiftSuperAcceptanceFixture) {
			f.store.db.ExecContext(f.ctx, `INSERT INTO swift_inheritance_relations(repo_id,file_id,child_qualified_name,target_qualified_name,relation_kind,start_line,start_col,end_line,end_col) VALUES(?,?,?,?,?,?,?,?,?)`, f.repoID, f.mainFile, "Child", "External", "unproven", 1, 1, 1, 1)
		}},
		{"generic", func(f *swiftSuperAcceptanceFixture) {
			f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET is_generic=1 WHERE child_qualified_name='Child'`)
		}},
		{"constrained", func(f *swiftSuperAcceptanceFixture) {
			f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET is_constrained=1 WHERE child_qualified_name='Child'`)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwiftSuperAcceptanceFixture(t)
			tc.mutate(f)
			f.resolve()
			if got := f.dst(f.edge); got.Valid {
				t.Fatalf("refusal resolved to %d", got.Int64)
			}
		})
	}
}

func TestSwiftSuperScopeRelationChangesRebindDirectTarget(t *testing.T) {
	f := newSwiftSuperAcceptanceFixture(t)
	f.resolve()
	if got := f.dst(f.edge); !got.Valid || got.Int64 != f.baseRun {
		t.Fatalf("initial dst=%v, want %d", got, f.baseRun)
	}
	otherClass := f.symbol(f.mainFile, "OtherBase", "", "class", "", false)
	otherRun := f.symbol(f.mainFile, "run", "OtherBase", "function", "run()", false)
	swiftSetRange(t, f.swiftScopeFixture, otherClass, 24, 1, 26, 2)
	swiftSetRange(t, f.swiftScopeFixture, otherRun, 25, 1, 25, 15)
	f.arity(otherRun, 0, 0)
	swiftFact(t, f.swiftScopeFixture, f.mainFile, otherRun, "instance")
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET target_qualified_name='OtherBase' WHERE child_qualified_name='Child'`); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
		t.Fatal(err)
	}
	if got := f.dst(f.edge); !got.Valid || got.Int64 != otherRun {
		t.Fatalf("relation rebind dst=%v, want %d", got, otherRun)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET is_generic=1 WHERE child_qualified_name='Child'`); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
		t.Fatal(err)
	}
	if got := f.dst(f.edge); got.Valid {
		t.Fatalf("generic relation resolved to %d", got.Int64)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE swift_inheritance_relations SET is_generic=0 WHERE child_qualified_name='Child'`); err != nil {
		t.Fatal(err)
	}
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
		t.Fatal(err)
	}
	if got := f.dst(f.edge); !got.Valid || got.Int64 != otherRun {
		t.Fatalf("restored relation dst=%v, want %d", got, otherRun)
	}
}

func TestSwiftSuperScopeTargetDispatchAndOwnership(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*swiftSuperAcceptanceFixture)
	}{
		{"class target", func(f *swiftSuperAcceptanceFixture) {
			f.store.db.ExecContext(f.ctx, `UPDATE symbols SET is_static=1 WHERE id=?`, f.baseRun)
			f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET dispatch_kind='class' WHERE symbol_id=?`, f.baseRun)
		}},
		{"static target", func(f *swiftSuperAcceptanceFixture) {
			f.store.db.ExecContext(f.ctx, `UPDATE symbols SET is_static=1 WHERE id=?`, f.baseRun)
			f.store.db.ExecContext(f.ctx, `UPDATE swift_declaration_facts SET dispatch_kind='static' WHERE symbol_id=?`, f.baseRun)
		}},
		{"missing target fact", func(f *swiftSuperAcceptanceFixture) {
			f.store.db.ExecContext(f.ctx, `DELETE FROM swift_declaration_facts WHERE symbol_id=?`, f.baseRun)
		}},
		{"duplicate target facts", func(f *swiftSuperAcceptanceFixture) {
			swiftFact(f.t, f.swiftScopeFixture, f.mainFile, f.baseRun, "instance")
		}},
		{"extension target", func(f *swiftSuperAcceptanceFixture) {
			swiftSetRange(f.t, f.swiftScopeFixture, f.baseRun, 20, 1, 20, 15)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwiftSuperAcceptanceFixture(t)
			tc.mutate(f)
			f.resolve()
			if got := f.dst(f.edge); got.Valid {
				t.Fatalf("refusal resolved to %d", got.Int64)
			}
		})
	}
}

func TestSwiftSuperScopeEvidenceApplicabilityAndRepairTax(t *testing.T) {
	f := newSwiftScopeFixture(t)
	caller := f.symbol(f.mainFile, "caller", "Child", "function", "caller()", false)
	for i, evidence := range []string{"swift:self", "swift:Self", "swift:bare", "swift:member", "swift:initializer", "swift:superficial"} {
		f.call(f.mainFile, caller, "run", evidence, 0, i+1)
	}
	if applies, err := f.store.swiftSuperEvidenceApplies(f.ctx, f.repoID); err != nil || applies {
		t.Fatalf("zero-super applies=(%v,%v)", applies, err)
	}
	f.call(f.mainFile, caller, "super.run", "swift:super", 0, 20)
	if applies, err := f.store.swiftSuperEvidenceApplies(f.ctx, f.repoID); err != nil || !applies {
		t.Fatalf("super applies=(%v,%v)", applies, err)
	}
}

func TestSwiftSuperScopeSelectorsOverloadsAndTrailingLabels(t *testing.T) {
	t.Run("distinct selector", func(t *testing.T) {
		f := newSwiftSuperAcceptanceFixture(t)
		if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET signature='run(id:)',arity_min=1,arity_max=1 WHERE id=?`, f.baseRun); err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET dst_name='super.run',evidence='swift:super;labels=id:',call_arity=1 WHERE id=?`, f.edge); err != nil {
			t.Fatal(err)
		}
		nameRun := f.symbol(f.mainFile, "run", "Base", "function", "run(name:)", false)
		swiftSetRange(t, f.swiftScopeFixture, nameRun, 2, 20, 2, 35)
		f.arity(nameRun, 1, 1)
		swiftFact(t, f.swiftScopeFixture, f.mainFile, nameRun, "instance")
		f.resolve()
		if got := f.dst(f.edge); !got.Valid || got.Int64 != f.baseRun {
			t.Fatalf("dst=%v, want %d", got, f.baseRun)
		}
	})

	t.Run("same selector overload", func(t *testing.T) {
		f := newSwiftSuperAcceptanceFixture(t)
		if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET signature='run(id:)',arity_min=1,arity_max=1 WHERE id=?`, f.baseRun); err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET evidence='swift:super;labels=id:',call_arity=1 WHERE id=?`, f.edge); err != nil {
			t.Fatal(err)
		}
		other := f.symbol(f.mainFile, "run", "Base", "function", "run(id:)", false)
		swiftSetRange(t, f.swiftScopeFixture, other, 2, 20, 2, 35)
		f.arity(other, 1, 1)
		swiftFact(t, f.swiftScopeFixture, f.mainFile, other, "instance")
		if got := f.resolveNames("run"); got.TargetsSelected != 1 || got.TargetsResolved != 0 || got.TargetsUnresolved != 1 {
			t.Fatalf("stats=%+v", got)
		}
		if got := f.dst(f.edge); got.Valid {
			t.Fatalf("overload resolved to %d", got.Int64)
		}
	})

	t.Run("trailing wildcard", func(t *testing.T) {
		f := newSwiftSuperAcceptanceFixture(t)
		if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET signature='run(completion:)',arity_min=1,arity_max=1 WHERE id=?`, f.baseRun); err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET evidence='swift:super;trailing_labels=_',call_arity=1 WHERE id=?`, f.edge); err != nil {
			t.Fatal(err)
		}
		f.resolve()
		if got := f.dst(f.edge); !got.Valid || got.Int64 != f.baseRun {
			t.Fatalf("dst=%v, want %d", got, f.baseRun)
		}
	})

	t.Run("hidden first label ambiguity", func(t *testing.T) {
		f := newSwiftSuperAcceptanceFixture(t)
		if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET signature='run(completion:)',arity_min=1,arity_max=1 WHERE id=?`, f.baseRun); err != nil {
			t.Fatal(err)
		}
		other := f.symbol(f.mainFile, "run", "Base", "function", "run(_:)", false)
		swiftSetRange(t, f.swiftScopeFixture, other, 2, 20, 2, 35)
		f.arity(other, 1, 1)
		swiftFact(t, f.swiftScopeFixture, f.mainFile, other, "instance")
		if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET evidence='swift:super;trailing_labels=_',call_arity=1 WHERE id=?`, f.edge); err != nil {
			t.Fatal(err)
		}
		f.resolve()
		if got := f.dst(f.edge); got.Valid {
			t.Fatalf("ambiguous trailing call resolved to %d", got.Int64)
		}
	})

	t.Run("later trailing label", func(t *testing.T) {
		f := newSwiftSuperAcceptanceFixture(t)
		if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET signature='run(first:,completion:)',arity_min=2,arity_max=2 WHERE id=?`, f.baseRun); err != nil {
			t.Fatal(err)
		}
		if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET evidence='swift:super;trailing_labels=_,completion:',call_arity=2 WHERE id=?`, f.edge); err != nil {
			t.Fatal(err)
		}
		f.resolve()
		if got := f.dst(f.edge); !got.Valid || got.Int64 != f.baseRun {
			t.Fatalf("dst=%v, want %d", got, f.baseRun)
		}
		if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET signature='run(first:,failure:)' WHERE id=?`, f.baseRun); err != nil {
			t.Fatal(err)
		}
		if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Service.swift"}); err != nil {
			t.Fatal(err)
		}
		if got := f.dst(f.edge); got.Valid {
			t.Fatalf("wrong later label resolved to %d", got.Int64)
		}
	})
}

func TestSwiftSuperScopeUnsupportedShapesAndBlockers(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*swiftSuperAcceptanceFixture)
	}{
		{"default candidate", func(f *swiftSuperAcceptanceFixture) {
			f.store.db.ExecContext(f.ctx, `UPDATE symbols SET arity_min=0,arity_max=1 WHERE id=?`, f.baseRun)
		}},
		{"variadic candidate", func(f *swiftSuperAcceptanceFixture) {
			f.store.db.ExecContext(f.ctx, `UPDATE symbols SET arity_max=NULL WHERE id=?`, f.baseRun)
		}},
		{"fixed plus unsupported overload", func(f *swiftSuperAcceptanceFixture) {
			other := f.symbol(f.mainFile, "run", "Base", "function", "run()", false)
			swiftSetRange(f.t, f.swiftScopeFixture, other, 2, 20, 2, 35)
			f.arity(other, 0, 1)
			swiftFact(f.t, f.swiftScopeFixture, f.mainFile, other, "instance")
		}},
		{"member value blocker", func(f *swiftSuperAcceptanceFixture) {
			f.blocker(f.mainFile, "Base", "run", graph.ScopeImportSwiftMemberValue, false)
		}},
		{"enum case blocker", func(f *swiftSuperAcceptanceFixture) {
			f.blocker(f.mainFile, "Base", "run", graph.ScopeImportSwiftEnumCase, true)
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwiftSuperAcceptanceFixture(t)
			tc.mutate(f)
			f.resolve()
			if got := f.dst(f.edge); got.Valid {
				t.Fatalf("unsupported/blocker case resolved to %d", got.Int64)
			}
		})
	}
}

func TestSwiftSuperScopeCrossFileCompetitorAndReferences(t *testing.T) {
	f := newSwiftSuperAcceptanceFixture(t)
	f.reference(f.mainFile, f.caller, "super.run", 7)
	f.resolve()
	var ref sql.NullInt64
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT symbol_id FROM references_tbl WHERE start_line=7`).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	if !ref.Valid || ref.Int64 != f.baseRun {
		t.Fatalf("reference=%v, want %d", ref, f.baseRun)
	}

	otherFile := f.file("Other.swift")
	competitor := f.symbol(otherFile, "run", "Base", "function", "run()", false)
	swiftSetRange(t, f.swiftScopeFixture, competitor, 1, 1, 1, 15)
	f.arity(competitor, 0, 0)
	swiftFact(t, f.swiftScopeFixture, otherFile, competitor, "instance")
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Other.swift"}); err != nil {
		t.Fatal(err)
	}
	if got := f.dst(f.edge); got.Valid {
		t.Fatalf("competitor did not veto: %d", got.Int64)
	}
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT symbol_id FROM references_tbl WHERE start_line=7`).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	if ref.Valid {
		t.Fatalf("reference remained bound to %d", ref.Int64)
	}

	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET signature='run(name:)' WHERE id=?`, competitor); err != nil {
		t.Fatal(err)
	}
	f.arity(competitor, 1, 1)
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Other.swift"}); err != nil {
		t.Fatal(err)
	}
	if got := f.dst(f.edge); !got.Valid || got.Int64 != f.baseRun {
		t.Fatalf("incompatible competitor dst=%v, want %d", got, f.baseRun)
	}
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT symbol_id FROM references_tbl WHERE start_line=7`).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	if !ref.Valid || ref.Int64 != f.baseRun {
		t.Fatalf("rebound reference=%v, want %d", ref, f.baseRun)
	}
}

func TestSwiftSuperScopeP7AndCrossFileVisibility(t *testing.T) {
	t.Run("test competitor does not poison production", func(t *testing.T) {
		f := newSwiftSuperAcceptanceFixture(t)
		testFile := f.file("Tests/BaseTests.swift")
		competitor := f.symbol(testFile, "run", "Base", "function", "run()", false)
		swiftSetRange(t, f.swiftScopeFixture, competitor, 1, 1, 1, 15)
		f.arity(competitor, 0, 0)
		swiftFact(t, f.swiftScopeFixture, testFile, competitor, "instance")
		if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Tests/BaseTests.swift"}); err != nil {
			t.Fatal(err)
		}
		if got := f.dst(f.edge); !got.Valid || got.Int64 != f.baseRun {
			t.Fatalf("production dst=%v, want %d", got, f.baseRun)
		}
	})

	t.Run("test caller sees production and test candidates", func(t *testing.T) {
		f := newSwiftSuperAcceptanceFixture(t)
		testFile := f.file("Tests/BaseTests.swift")
		base := f.symbol(testFile, "Base", "", "class", "", false)
		testRun := f.symbol(testFile, "run", "Base", "function", "run()", false)
		child := f.symbol(testFile, "Child", "", "class", "", false)
		caller := f.symbol(testFile, "f", "Child", "function", "f()", false)
		swiftSetRange(t, f.swiftScopeFixture, base, 1, 1, 3, 2)
		swiftSetRange(t, f.swiftScopeFixture, testRun, 2, 1, 2, 15)
		swiftSetRange(t, f.swiftScopeFixture, child, 5, 1, 9, 2)
		swiftSetRange(t, f.swiftScopeFixture, caller, 6, 1, 8, 2)
		f.arity(testRun, 0, 0)
		swiftSuperclass(t, f.swiftScopeFixture, testFile, "Child", "Base")
		swiftFact(t, f.swiftScopeFixture, testFile, testRun, "instance")
		swiftFact(t, f.swiftScopeFixture, testFile, caller, "instance")
		edge := f.call(testFile, caller, "super.run", "swift:super", 0, 7)
		if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Tests/BaseTests.swift"}); err != nil {
			t.Fatal(err)
		}
		if got := f.dst(edge); got.Valid {
			t.Fatalf("test caller resolved to %d", got.Int64)
		}
	})

	for _, visibility := range []string{"private", "fileprivate", "internal", "public"} {
		t.Run("competitor-"+visibility, func(t *testing.T) {
			f := newSwiftSuperAcceptanceFixture(t)
			other := f.file("Other.swift")
			competitor := f.symbol(other, "run", "Base", "function", "run()", false)
			swiftSetRange(t, f.swiftScopeFixture, competitor, 1, 1, 1, 15)
			f.arity(competitor, 0, 0)
			swiftFact(t, f.swiftScopeFixture, other, competitor, "instance")
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET visibility=? WHERE id=?`, visibility, competitor); err != nil {
				t.Fatal(err)
			}
			if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"Other.swift"}); err != nil {
				t.Fatal(err)
			}
			got := f.dst(f.edge).Valid
			want := visibility == "private" || visibility == "fileprivate"
			if got != want {
				t.Fatalf("bound=%v, want %v", got, want)
			}
		})
	}
}

func TestSwiftSuperScopeMalformedStoreEvidenceRefuses(t *testing.T) {
	for _, tc := range []struct {
		dst, evidence string
		arity         int
	}{
		{"super.init", "swift:super", 0},
		{"super.deinit", "swift:super", 0},
		{"super.subscript", "swift:super", 0},
		{"super.foo.bar", "swift:super", 0},
		{"Super.run", "swift:super", 0},
		{"super.run", "swift:super;labels=id:;labels=name:", 2},
		{"super.run", "swift:super;trailing_labels=completion:", 1},
		{"super.run", "swift:super;foo=x", 0},
		{"super.run", "swift:super", -1},
	} {
		t.Run(tc.dst+"/"+tc.evidence, func(t *testing.T) {
			f := newSwiftSuperAcceptanceFixture(t)
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET dst_name=?,evidence=?,call_arity=? WHERE id=?`, tc.dst, tc.evidence, tc.arity, f.edge); err != nil {
				t.Fatal(err)
			}
			f.resolve()
			if got := f.dst(f.edge); got.Valid {
				t.Fatalf("malformed evidence resolved to %d", got.Int64)
			}
		})
	}
}

func TestSwiftSuperScopeVisibilityAndRepairFramework(t *testing.T) {
	t.Run("private target refuses and fileprivate binds", func(t *testing.T) {
		f := newSwiftSuperAcceptanceFixture(t)
		if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET visibility='private' WHERE id=?`, f.baseRun); err != nil {
			t.Fatal(err)
		}
		f.resolve()
		if got := f.dst(f.edge); got.Valid {
			t.Fatalf("private target resolved to %d", got.Int64)
		}
		if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET visibility='fileprivate' WHERE id=?`, f.baseRun); err != nil {
			t.Fatal(err)
		}
		f.resolve()
		if got := f.dst(f.edge); !got.Valid || got.Int64 != f.baseRun {
			t.Fatalf("fileprivate dst=%v, want %d", got, f.baseRun)
		}
	})
	t.Run("repair binds and marks once", func(t *testing.T) {
		f := newSwiftSuperAcceptanceFixture(t)
		if ran, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID); err != nil || !ran {
			t.Fatalf("repair=(%v,%v)", ran, err)
		}
		if got := f.dst(f.edge); !got.Valid || got.Int64 != f.baseRun {
			t.Fatalf("repair dst=%v, want %d", got, f.baseRun)
		}
		var marker string
		if err := f.store.db.QueryRowContext(f.ctx, `SELECT value FROM settings WHERE key=?`, swiftSuperRepairSettingKey+"."+strconv.FormatInt(f.repoID, 10)).Scan(&marker); err != nil {
			t.Fatal(err)
		}
		if marker != "1" {
			t.Fatalf("marker=%q", marker)
		}
		if ran, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID); err != nil || ran {
			t.Fatalf("second repair=(%v,%v)", ran, err)
		}
	})
	t.Run("repair cancellation retries", func(t *testing.T) {
		f := newSwiftSuperAcceptanceFixture(t)
		cancelled, cancel := context.WithCancel(f.ctx)
		cancel()
		if _, err := f.store.RepairResolverBindingsOnce(cancelled, f.repoID); err == nil {
			t.Fatal("canceled repair succeeded")
		}
		var marker string
		if err := f.store.db.QueryRowContext(f.ctx, `SELECT value FROM settings WHERE key=?`, swiftSuperRepairSettingKey+"."+strconv.FormatInt(f.repoID, 10)).Scan(&marker); err == nil {
			t.Fatalf("canceled repair wrote marker %q", marker)
		}
		if ran, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID); err != nil || !ran {
			t.Fatalf("retry=(%v,%v)", ran, err)
		}
		if got := f.dst(f.edge); !got.Valid || got.Int64 != f.baseRun {
			t.Fatalf("retry dst=%v, want %d", got, f.baseRun)
		}
	})
}

func TestSwiftSuperScopeStatsInvariantMixed(t *testing.T) {
	f := newSwiftSuperAcceptanceFixture(t)
	missing := f.call(f.mainFile, f.caller, "super.missing", "swift:super", 0, 8)
	stats := f.resolveNames("run", "missing")
	assertSwiftStats(t, stats, 2, 1, 1, 0)
	if got := f.dst(f.edge); !got.Valid || got.Int64 != f.baseRun {
		t.Fatalf("positive dst=%v, want %d", got, f.baseRun)
	}
	if got := f.dst(missing); got.Valid {
		t.Fatalf("missing dst=%d", got.Int64)
	}
}

func TestSwiftSuperScopeBatchesOverSQLiteVariableBudget(t *testing.T) {
	f := newSwiftScopeFixture(t)
	const total = 1200
	edges := make([]int64, 0, total)
	for i := 0; i < total; i++ {
		baseName := "Base" + strconv.Itoa(i)
		childName := "Child" + strconv.Itoa(i)
		f.symbol(f.mainFile, baseName, "", "class", "", false)
		target := f.symbol(f.mainFile, "run", baseName, "function", "run()", false)
		f.symbol(f.mainFile, childName, "", "class", "", false)
		caller := f.symbol(f.mainFile, "f", childName, "function", "f()", false)
		f.arity(target, 0, 0)
		swiftSuperclass(t, f, f.mainFile, childName, baseName)
		swiftFact(t, f, f.mainFile, target, "instance")
		swiftFact(t, f, f.mainFile, caller, "instance")
		edges = append(edges, f.call(f.mainFile, caller, "super.run", "swift:super", 0, i+1))
	}
	f.resolve()
	var bound int
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM edges WHERE repo_id=? AND dst_symbol_id IS NOT NULL AND resolution_strategy=?`, f.repoID, ResolutionStrategySwiftSuperScope).Scan(&bound); err != nil {
		t.Fatal(err)
	}
	if bound != total {
		t.Fatalf("bound=%d, want %d", bound, total)
	}
}
