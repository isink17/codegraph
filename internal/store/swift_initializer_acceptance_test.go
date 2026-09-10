package store

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

func swiftInitCandidate(f *swiftScopeFixture, file int64, owner, signature string, min, max int64) int64 {
	id := f.symbol(file, "init", owner, "function", signature, false)
	f.arity(id, min, max)
	return id
}

func assertSwiftInitBinding(t *testing.T, f *swiftScopeFixture, edge, target int64) {
	t.Helper()
	got := f.dst(edge)
	if !got.Valid || got.Int64 != target {
		t.Fatalf("edge %d dst=%v want %d", edge, got, target)
	}
	var strategy, confidence string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT resolution_strategy,resolution_confidence FROM edges WHERE id=?`, edge).Scan(&strategy, &confidence); err != nil {
		t.Fatal(err)
	}
	if strategy != ResolutionStrategySwiftInitializerScope || confidence != ResolutionConfidenceHigh {
		t.Fatalf("edge %d resolution=(%q,%q)", edge, strategy, confidence)
	}
}

func assertSwiftInitReference(t *testing.T, f *swiftScopeFixture, target *int64) {
	t.Helper()
	var got sql.NullInt64
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT symbol_id FROM references_tbl WHERE repo_id=?`, f.repoID).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if target == nil {
		if got.Valid {
			t.Fatalf("reference=%d want NULL", got.Int64)
		}
		return
	}
	if !got.Valid || got.Int64 != *target {
		t.Fatalf("reference=%v want %d", got, *target)
	}
}

func TestSwiftInitializerAcceptancePositiveMatrix(t *testing.T) {
	tests := []struct {
		name, owner, dst, evidence, signature string
		kind                                  string
		arity                                 int64
	}{
		{"zero", "Service", "Service", "swift:initializer", "init()", "struct", 0},
		{"underscore", "Service", "Service", "swift:initializer;labels=_", "init(_:)", "struct", 1},
		{"named", "Service", "Service", "swift:initializer;labels=id:", "init(id:)", "struct", 1},
		{"nested", "Outer.Inner", "Outer.Inner", "swift:initializer;labels=id:", "init(id:)", "struct", 1},
		{"generic", "Box", "Box", "swift:initializer;generic_specialization=true", "init()", "struct", 0},
		{"enum", "Value", "Value", "swift:initializer;labels=id:", "init(id:)", "enum", 1},
		{"actor", "Worker", "Worker", "swift:initializer;labels=id:", "init(id:)", "actor", 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwiftScopeFixture(t)
			if tc.owner == "Outer.Inner" {
				f.symbol(f.mainFile, "Outer", "", "struct", "", false)
				f.symbol(f.mainFile, "Inner", "Outer", "struct", "", false)
			} else {
				f.symbol(f.mainFile, tc.owner, "", tc.kind, "", false)
			}
			target := swiftInitCandidate(f, f.mainFile, tc.owner, tc.signature, tc.arity, tc.arity)
			caller := f.symbol(f.mainFile, "make", "", "function", "make()", false)
			edge := f.call(f.mainFile, caller, tc.dst, tc.evidence, int(tc.arity), 1)
			f.resolve()
			assertSwiftInitBinding(t, f, edge, target)
		})
	}
}

func TestSwiftTrailingInitializerAcceptance(t *testing.T) {
	tests := []struct {
		name, owner, kind, evidence, signature string
		arity                                  int64
	}{
		{"underscore", "Builder", "struct", "swift:initializer;trailing_labels=_", "init(_:)", 1},
		{"named", "Builder", "struct", "swift:initializer;trailing_labels=_", "init(completion:)", 1},
		{"regular-prefix", "Builder", "struct", "swift:initializer;labels=id:;trailing_labels=_", "init(id:,completion:)", 2},
		{"multiple", "Builder", "struct", "swift:initializer;trailing_labels=_,completion:", "init(first:,completion:)", 2},
		{"generic", "GenericBuilder", "struct", "swift:initializer;generic_specialization=true;trailing_labels=_", "init(_:)", 1},
		{"nested", "Outer.Builder", "struct", "swift:initializer;trailing_labels=_", "init(completion:)", 1},
		{"enum", "Event", "enum", "swift:initializer;trailing_labels=_", "init(completion:)", 1},
		{"actor", "Worker", "actor", "swift:initializer;trailing_labels=_", "init(completion:)", 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwiftScopeFixture(t)
			if tc.owner == "Outer.Builder" {
				f.symbol(f.mainFile, "Outer", "", "struct", "", false)
				f.symbol(f.mainFile, "Builder", "Outer", tc.kind, "", false)
			} else {
				f.symbol(f.mainFile, tc.owner, "", tc.kind, "", false)
			}
			target := swiftInitCandidate(f, f.mainFile, tc.owner, tc.signature, tc.arity, tc.arity)
			caller := f.symbol(f.mainFile, "make", "", "function", "make()", false)
			edge := f.call(f.mainFile, caller, tc.owner, tc.evidence, int(tc.arity), 1)
			f.resolve()
			assertSwiftInitBinding(t, f, edge, target)
		})
	}
}

func TestSwiftTrailingInitializerRefusesAmbiguityAndUnsupportedEvidence(t *testing.T) {
	f := newSwiftScopeFixture(t)
	f.symbol(f.mainFile, "Builder", "", "struct", "", false)
	slash := swiftInitCandidate(f, f.mainFile, "Builder", "init(_:)", 1, 1)
	completion := swiftInitCandidate(f, f.mainFile, "Builder", "init(completion:)", 1, 1)
	caller := f.symbol(f.mainFile, "make", "", "function", "make()", false)
	ambiguous := f.call(f.mainFile, caller, "Builder", "swift:initializer;trailing_labels=_", 1, 1)
	unproven := f.call(f.mainFile, caller, "Builder", "swift:initializer_unproven;trailing_labels=_", 1, 2)
	classOwner := f.symbol(f.mainFile, "ClassBuilder", "", "class", "", false)
	classInit := swiftInitCandidate(f, f.mainFile, "ClassBuilder", "init(completion:)", 1, 1)
	classEdge := f.call(f.mainFile, caller, "ClassBuilder", "swift:initializer;trailing_labels=_", 1, 3)
	defaultInit := swiftInitCandidate(f, f.mainFile, "Defaults", "init(completion:)", 0, 1)
	f.symbol(f.mainFile, "Defaults", "", "struct", "", false)
	defaultEdge := f.call(f.mainFile, caller, "Defaults", "swift:initializer;trailing_labels=_", 1, 4)
	f.resolve()
	for _, edge := range []int64{ambiguous, unproven, classEdge, defaultEdge} {
		if got := f.dst(edge); got.Valid {
			t.Fatalf("edge %d resolved to %d", edge, got.Int64)
		}
	}
	_, _, _, _ = slash, completion, classOwner, classInit
	_ = defaultInit
}

func TestSwiftTrailingInitializerCrossFileUsesShapeCompatibility(t *testing.T) {
	f := newSwiftScopeFixture(t)
	f.symbol(f.mainFile, "Builder", "", "struct", "", false)
	local := swiftInitCandidate(f, f.mainFile, "Builder", "init(first:,completion:)", 2, 2)
	caller := f.symbol(f.mainFile, "make", "", "function", "make()", false)
	edge := f.call(f.mainFile, caller, "Builder", "swift:initializer;trailing_labels=_,completion:", 2, 1)
	other := f.file("Other.swift")
	competitor := swiftInitCandidate(f, other, "Builder", "init(first:,failure:)", 2, 2)
	assertSwiftInitializerArity(t, f, local, 2, 2)
	assertSwiftInitializerArity(t, f, competitor, 2, 2)
	f.resolve()
	assertSwiftInitBinding(t, f, edge, local)
}

func assertSwiftInitializerArity(t *testing.T, f *swiftScopeFixture, symbol, min, max int64) {
	t.Helper()
	var gotMin, gotMax sql.NullInt64
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT arity_min,arity_max FROM symbols WHERE id=?`, symbol).Scan(&gotMin, &gotMax); err != nil {
		t.Fatal(err)
	}
	if !gotMin.Valid || gotMin.Int64 != min || !gotMax.Valid || gotMax.Int64 != max {
		t.Fatalf("symbol %d arity=(%v,%v), want (%d,%d)", symbol, gotMin, gotMax, min, max)
	}
}

func TestSwiftTrailingInitializerCrossFileHiddenFirstLabelVeto(t *testing.T) {
	f := newSwiftScopeFixture(t)
	f.symbol(f.mainFile, "Builder", "", "struct", "", false)
	swiftInitCandidate(f, f.mainFile, "Builder", "init(_:)", 1, 1)
	other := f.file("Other.swift")
	swiftInitCandidate(f, other, "Builder", "init(completion:)", 1, 1)
	caller := f.symbol(f.mainFile, "make", "", "function", "make()", false)
	edge := f.call(f.mainFile, caller, "Builder", "swift:initializer;trailing_labels=_", 1, 1)
	f.resolve()
	if got := f.dst(edge); got.Valid {
		t.Fatalf("hidden-first-label competitor resolved edge to %d", got.Int64)
	}
}

func TestSwiftTrailingInitializerCrossFileRegularPrefixIsExact(t *testing.T) {
	f := newSwiftScopeFixture(t)
	f.symbol(f.mainFile, "Builder", "", "struct", "", false)
	local := swiftInitCandidate(f, f.mainFile, "Builder", "init(id:,completion:)", 2, 2)
	other := f.file("Other.swift")
	competitor := swiftInitCandidate(f, other, "Builder", "init(name:,completion:)", 2, 2)
	assertSwiftInitializerArity(t, f, local, 2, 2)
	assertSwiftInitializerArity(t, f, competitor, 2, 2)
	caller := f.symbol(f.mainFile, "make", "", "function", "make()", false)
	edge := f.call(f.mainFile, caller, "Builder", "swift:initializer;labels=id:;trailing_labels=_", 2, 1)
	f.resolve()
	assertSwiftInitBinding(t, f, edge, local)
}

func TestSwiftTrailingInitializerRefusesVariadicCandidate(t *testing.T) {
	f := newSwiftScopeFixture(t)
	f.symbol(f.mainFile, "Builder", "", "struct", "", false)
	target := swiftInitCandidate(f, f.mainFile, "Builder", "init(completion:)", 1, 1)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET arity_max=NULL WHERE id=?`, target); err != nil {
		t.Fatal(err)
	}
	caller := f.symbol(f.mainFile, "make", "", "function", "make()", false)
	edge := f.call(f.mainFile, caller, "Builder", "swift:initializer;trailing_labels=_", 1, 1)
	f.resolve()
	if got := f.dst(edge); got.Valid {
		t.Fatalf("variadic trailing initializer resolved to %d", got.Int64)
	}
}

func TestSwiftTrailingInitializerRepairApplicabilityMatrix(t *testing.T) {
	tests := []struct {
		name, language, evidence, dst string
		arity                         int
		bound                         bool
		want                          bool
	}{
		{"no-swift", "go", "swift:initializer;trailing_labels=_", "Builder", 1, false, false},
		{"ordinary", "swift", "swift:initializer", "Builder", 0, false, false},
		{"unproven", "swift", "swift:initializer_unproven;trailing_labels=_", "Builder", 1, false, false},
		{"self", "swift", "swift:self;trailing_labels=_", "self.run", 1, false, false},
		{"Self", "swift", "swift:Self;trailing_labels=_", "Self.run", 1, false, false},
		{"trailing-unbound", "swift", "swift:initializer;trailing_labels=_", "Builder", 1, false, true},
		{"trailing-bound", "swift", "swift:initializer;trailing_labels=_", "Builder", 1, true, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwiftScopeFixture(t)
			if tc.language != "swift" {
				if _, err := f.store.db.ExecContext(f.ctx, `UPDATE files SET language=? WHERE id=?`, tc.language, f.mainFile); err != nil {
					t.Fatal(err)
				}
			}
			caller := f.symbol(f.mainFile, "make", "", "function", "make()", false)
			edge := f.call(f.mainFile, caller, tc.dst, tc.evidence, tc.arity, 1)
			if tc.bound {
				target := swiftInitCandidate(f, f.mainFile, "Builder", "init(completion:)", 1, 1)
				if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET dst_symbol_id=? WHERE id=?`, target, edge); err != nil {
					t.Fatal(err)
				}
			}
			got, err := f.store.swiftTrailingInitializerRepairApplies(f.ctx, f.repoID)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("applies=%v want %v", got, tc.want)
			}
		})
	}
}

func TestSwiftTrailingInitializerRepairBindsMarksAndSkips(t *testing.T) {
	f := newSwiftScopeFixture(t)
	f.symbol(f.mainFile, "Builder", "", "struct", "", false)
	target := swiftInitCandidate(f, f.mainFile, "Builder", "init(completion:)", 1, 1)
	caller := f.symbol(f.mainFile, "make", "", "function", "make()", false)
	edge := f.call(f.mainFile, caller, "Builder", "swift:initializer;trailing_labels=_", 1, 1)
	didRun, err := f.store.runResolverRepairOnce(f.ctx, f.repoID, swiftTrailingInitializerRepair)
	if err != nil || !didRun {
		t.Fatalf("first repair=(%v,%v)", didRun, err)
	}
	assertSwiftInitBinding(t, f, edge, target)
	assertSwiftRepairMarker(t, f, swiftTrailingInitializerRepairSettingKey)
	didRun, err = f.store.runResolverRepairOnce(f.ctx, f.repoID, swiftTrailingInitializerRepair)
	if err != nil || didRun {
		t.Fatalf("second repair=(%v,%v)", didRun, err)
	}
}

func TestSwiftTrailingInitializerRepairFailureRetries(t *testing.T) {
	f := newSwiftScopeFixture(t)
	f.symbol(f.mainFile, "Builder", "", "struct", "", false)
	target := swiftInitCandidate(f, f.mainFile, "Builder", "init(completion:)", 1, 1)
	caller := f.symbol(f.mainFile, "make", "", "function", "make()", false)
	edge := f.call(f.mainFile, caller, "Builder", "swift:initializer;trailing_labels=_", 1, 1)
	canceled, cancel := context.WithCancel(f.ctx)
	cancel()
	if _, err := f.store.runResolverRepairOnce(canceled, f.repoID, swiftTrailingInitializerRepair); err == nil {
		t.Fatal("canceled trailing repair unexpectedly succeeded")
	}
	assertSwiftRepairMarkerAbsent(t, f, swiftTrailingInitializerRepairSettingKey)
	didRun, err := f.store.runResolverRepairOnce(f.ctx, f.repoID, swiftTrailingInitializerRepair)
	if err != nil || !didRun {
		t.Fatalf("retry=(%v,%v)", didRun, err)
	}
	assertSwiftInitBinding(t, f, edge, target)
	assertSwiftRepairMarker(t, f, swiftTrailingInitializerRepairSettingKey)
}

func TestSwiftInitializerRepairsCoalesceOnlyWhenOldRepairRuns(t *testing.T) {
	var oldRuns, trailingRuns int
	repairs := resolverRepairs
	defer func() { resolverRepairs = repairs }()
	for i := range resolverRepairs {
		switch resolverRepairs[i].key {
		case swiftInitializerRepairSettingKey:
			run := resolverRepairs[i].run
			resolverRepairs[i].run = func(s *Store, ctx context.Context, repoID int64) error {
				oldRuns++
				return run(s, ctx, repoID)
			}
		case swiftTrailingInitializerRepairSettingKey:
			run := resolverRepairs[i].run
			resolverRepairs[i].run = func(s *Store, ctx context.Context, repoID int64) error {
				trailingRuns++
				return run(s, ctx, repoID)
			}
		}
	}

	f := newSwiftScopeFixture(t)
	f.symbol(f.mainFile, "Builder", "", "struct", "", false)
	ordinaryTarget := swiftInitCandidate(f, f.mainFile, "Builder", "init()", 0, 0)
	trailingTarget := swiftInitCandidate(f, f.mainFile, "Builder", "init(completion:)", 1, 1)
	caller := f.symbol(f.mainFile, "make", "", "function", "make()", false)
	ordinaryEdge := f.call(f.mainFile, caller, "Builder", "swift:initializer", 0, 1)
	trailingEdge := f.call(f.mainFile, caller, "Builder", "swift:initializer;trailing_labels=_", 1, 2)
	if _, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID); err != nil {
		t.Fatal(err)
	}
	if oldRuns != 1 || trailingRuns != 0 {
		t.Fatalf("repair runs old=%d trailing=%d, want 1,0", oldRuns, trailingRuns)
	}
	assertSwiftInitBinding(t, f, ordinaryEdge, ordinaryTarget)
	assertSwiftInitBinding(t, f, trailingEdge, trailingTarget)
	assertSwiftRepairMarker(t, f, swiftTrailingInitializerRepairSettingKey)

	g := newSwiftScopeFixture(t)
	g.symbol(g.mainFile, "Builder", "", "struct", "", false)
	swiftInitCandidate(g, g.mainFile, "Builder", "init(completion:)", 1, 1)
	gCaller := g.symbol(g.mainFile, "make", "", "function", "make()", false)
	g.call(g.mainFile, gCaller, "Builder", "swift:initializer;trailing_labels=_", 1, 1)
	if err := g.store.markRepairDone(g.ctx, swiftInitializerRepair.key, g.repoID); err != nil {
		t.Fatal(err)
	}
	if _, err := g.store.RepairResolverBindingsOnce(g.ctx, g.repoID); err != nil {
		t.Fatal(err)
	}
	if oldRuns != 1 || trailingRuns != 1 {
		t.Fatalf("marked-old repair runs old=%d trailing=%d, want 1,1", oldRuns, trailingRuns)
	}
}

func TestSwiftInitializerRepairCoalescingFailureLeavesTrailingMarkerAbsent(t *testing.T) {
	f := newSwiftScopeFixture(t)
	f.symbol(f.mainFile, "Builder", "", "struct", "", false)
	f.call(f.mainFile, f.symbol(f.mainFile, "make", "", "function", "make()", false), "Builder", "swift:initializer", 0, 1)
	f.call(f.mainFile, f.symbol(f.mainFile, "make2", "", "function", "make2()", false), "Builder", "swift:initializer;trailing_labels=_", 1, 2)
	for _, repair := range resolverRepairs {
		if repair.key == swiftInitializerRepairSettingKey || repair.key == swiftTrailingInitializerRepairSettingKey || repair.key == referenceIdentityRepairSettingKey {
			continue
		}
		if err := f.store.markRepairDone(f.ctx, repair.key, f.repoID); err != nil {
			t.Fatal(err)
		}
	}
	canceled, cancel := context.WithCancel(f.ctx)
	cancel()
	if _, err := f.store.RepairResolverBindingsOnce(canceled, f.repoID); err == nil {
		t.Fatal("canceled initializer repair unexpectedly succeeded")
	}
	assertSwiftRepairMarkerAbsent(t, f, swiftInitializerRepairSettingKey)
	assertSwiftRepairMarkerAbsent(t, f, swiftTrailingInitializerRepairSettingKey)
}

func assertSwiftRepairMarker(t *testing.T, f *swiftScopeFixture, key string) {
	t.Helper()
	var value string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT value FROM settings WHERE key=?`, key+fmt.Sprintf(".%d", f.repoID)).Scan(&value); err != nil || value != "1" {
		t.Fatalf("marker %s=%q err=%v", key, value, err)
	}
}

func assertSwiftRepairMarkerAbsent(t *testing.T, f *swiftScopeFixture, key string) {
	t.Helper()
	var value string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT value FROM settings WHERE key=?`, key+fmt.Sprintf(".%d", f.repoID)).Scan(&value); err == nil {
		t.Fatalf("marker %s unexpectedly set to %q", key, value)
	}
}

func TestSwiftTrailingInitializerAcceptanceBatch(t *testing.T) {
	f := newSwiftScopeFixture(t)
	caller := f.symbol(f.mainFile, "make", "", "function", "make()", false)
	const total = 1200
	for i := 0; i < total; i++ {
		owner := fmt.Sprintf("Batch%d", i)
		f.symbol(f.mainFile, owner, "", "struct", "", false)
		switch i % 6 {
		case 0:
			swiftInitCandidate(f, f.mainFile, owner, "init(id:)", 1, 1)
			f.call(f.mainFile, caller, owner, "swift:initializer;labels=id:", 1, i+1)
		case 1:
			swiftInitCandidate(f, f.mainFile, owner, "init(completion:)", 1, 1)
			f.call(f.mainFile, caller, owner, "swift:initializer;trailing_labels=_", 1, i+1)
		case 2:
			swiftInitCandidate(f, f.mainFile, owner, "init(id:,completion:)", 2, 2)
			f.call(f.mainFile, caller, owner, "swift:initializer;labels=id:;trailing_labels=_", 2, i+1)
		case 3:
			swiftInitCandidate(f, f.mainFile, owner, "init(first:,completion:)", 2, 2)
			f.call(f.mainFile, caller, owner, "swift:initializer;trailing_labels=_,completion:", 2, i+1)
		case 4:
			swiftInitCandidate(f, f.mainFile, owner, "init(_:)", 1, 1)
			swiftInitCandidate(f, f.mainFile, owner, "init(completion:)", 1, 1)
			f.call(f.mainFile, caller, owner, "swift:initializer;trailing_labels=_", 1, i+1)
		case 5:
			f.call(f.mainFile, caller, owner, "swift:initializer;trailing_labels=_", 1, i+1)
		}
	}
	f.resolve()
	var count int
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM edges WHERE repo_id=? AND dst_symbol_id IS NOT NULL AND resolution_strategy=?`, f.repoID, ResolutionStrategySwiftInitializerScope).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != total/6*4 {
		t.Fatalf("resolved initializer count=%d want %d", count, total/6*4)
	}
}

func TestSwiftInitializerAcceptanceOwnerRefusals(t *testing.T) {
	tests := []struct {
		name, kind string
		duplicate  bool
	}{
		{"class", "class", false},
		{"protocol", "protocol", false},
		{"recovery", "recovery", false},
		{"duplicate-struct", "struct", true},
		{"struct-class-mix", "struct", true},
		{"struct-enum-mix", "struct", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwiftScopeFixture(t)
			f.symbol(f.mainFile, "Service", "", tc.kind, "", false)
			if tc.duplicate {
				kind := "struct"
				if tc.name == "struct-class-mix" {
					kind = "class"
				}
				if tc.name == "struct-enum-mix" {
					kind = "enum"
				}
				f.symbol(f.mainFile, "Service", "", kind, "", false)
			}
			target := swiftInitCandidate(f, f.mainFile, "Service", "init()", 0, 0)
			caller := f.symbol(f.mainFile, "make", "", "function", "make()", false)
			edge := f.call(f.mainFile, caller, "Service", "swift:initializer", 0, 1)
			f.resolve()
			if got := f.dst(edge); got.Valid {
				t.Fatalf("resolved to %d, explicit target %d", got.Int64, target)
			}
		})
	}
}

func TestSwiftInitializerAcceptanceSynthesizedAndOverloadRefusals(t *testing.T) {
	f := newSwiftScopeFixture(t)
	f.symbol(f.mainFile, "Empty", "", "struct", "", false)
	caller := f.symbol(f.mainFile, "make", "", "function", "make()", false)
	synthesized := f.call(f.mainFile, caller, "Empty", "swift:initializer", 0, 1)
	f.symbol(f.mainFile, "User", "", "struct", "", false)
	memberwise := f.call(f.mainFile, caller, "User", "swift:initializer;labels=id:", 1, 2)
	f.symbol(f.mainFile, "Box", "", "struct", "", false)
	boxInit := swiftInitCandidate(f, f.mainFile, "Box", "init()", 0, 0)
	f.symbol(f.mainFile, "BoxAlias", "", "type", "", false)
	boxAliasInit := swiftInitCandidate(f, f.mainFile, "BoxAlias", "init()", 0, 0)
	unproven := f.call(f.mainFile, caller, "BoxAlias", "swift:initializer_unproven;generic_specialization=true", 0, 3)
	f.symbol(f.mainFile, "Service", "", "struct", "", false)
	first := swiftInitCandidate(f, f.mainFile, "Service", "init(id:)", 1, 1)
	second := swiftInitCandidate(f, f.mainFile, "Service", "init(id:)", 1, 1)
	ambiguous := f.call(f.mainFile, caller, "Service", "swift:initializer;labels=id:", 1, 4)
	named := swiftInitCandidate(f, f.mainFile, "Service", "init(name:)", 1, 1)
	distinct := f.call(f.mainFile, caller, "Service", "swift:initializer;labels=name:", 1, 5)
	f.resolve()
	for _, edge := range []int64{synthesized, memberwise, ambiguous, unproven} {
		if got := f.dst(edge); got.Valid {
			t.Fatalf("edge %d resolved to %d", edge, got.Int64)
		}
	}
	_ = first
	_ = second
	_ = boxInit
	_ = boxAliasInit
	assertSwiftInitBinding(t, f, distinct, named)
}

func TestSwiftInitializerAcceptanceDefaultsVariadicsAndVisibility(t *testing.T) {
	tests := []struct {
		name, ownerVisibility, initVisibility string
		min, max                              int64
	}{
		{"default", "", "", 0, 1},
		{"variadic", "", "", 0, -1},
		{"private-owner", "private", "", 0, 0},
		{"private-init", "", "private", 0, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwiftScopeFixture(t)
			owner := f.symbol(f.mainFile, "Service", "", "struct", "", false)
			if tc.ownerVisibility != "" {
				_, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET visibility=? WHERE id=?`, tc.ownerVisibility, owner)
				if err != nil {
					t.Fatal(err)
				}
			}
			init := swiftInitCandidate(f, f.mainFile, "Service", "init()", tc.min, tc.max)
			if tc.initVisibility != "" {
				_, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET visibility=? WHERE id=?`, tc.initVisibility, init)
				if err != nil {
					t.Fatal(err)
				}
			}
			caller := f.symbol(f.mainFile, "make", "", "function", "make()", false)
			edge := f.call(f.mainFile, caller, "Service", "swift:initializer", 0, 1)
			f.resolve()
			if got := f.dst(edge); got.Valid {
				t.Fatalf("resolved to %d", got.Int64)
			}
		})
	}
	fileprivate := newSwiftScopeFixture(t)
	owner := fileprivate.symbol(fileprivate.mainFile, "Service", "", "struct", "", false)
	init := swiftInitCandidate(fileprivate, fileprivate.mainFile, "Service", "init()", 0, 0)
	for id := range map[int64]struct{}{owner: {}, init: {}} {
		if _, err := fileprivate.store.db.ExecContext(fileprivate.ctx, `UPDATE symbols SET visibility='fileprivate' WHERE id=?`, id); err != nil {
			t.Fatal(err)
		}
	}
	caller := fileprivate.symbol(fileprivate.mainFile, "make", "", "function", "make()", false)
	edge := fileprivate.call(fileprivate.mainFile, caller, "Service", "swift:initializer", 0, 1)
	fileprivate.resolve()
	assertSwiftInitBinding(t, fileprivate, edge, init)
}

func TestSwiftInitializerAcceptanceCrossFileAndSoftDelete(t *testing.T) {
	f := newSwiftScopeFixture(t)
	f.symbol(f.mainFile, "Service", "", "struct", "", false)
	local := swiftInitCandidate(f, f.mainFile, "Service", "init()", 0, 0)
	caller := f.symbol(f.mainFile, "make", "", "function", "make()", false)
	edge := f.call(f.mainFile, caller, "Service", "swift:initializer", 0, 1)
	other := f.file("Other.swift")
	competitor := swiftInitCandidate(f, other, "Service", "init()", 0, 0)
	f.resolve()
	if got := f.dst(edge); got.Valid {
		t.Fatalf("cross-file competitor left %d", got.Int64)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE files SET is_deleted=1 WHERE id=?`, other); err != nil {
		t.Fatal(err)
	}
	f.resolve()
	assertSwiftInitBinding(t, f, edge, local)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE files SET is_deleted=0 WHERE id=?`, other); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET signature='init(name:)' WHERE id=?`, competitor); err != nil {
		t.Fatal(err)
	}
	f.resolve()
	assertSwiftInitBinding(t, f, edge, local)
}

func TestSwiftInitializerAcceptanceCrossFileOnlyRefuses(t *testing.T) {
	f := newSwiftScopeFixture(t)
	f.symbol(f.mainFile, "Service", "", "struct", "", false)
	other := f.file("Other.swift")
	swiftInitCandidate(f, other, "Service", "init(id:)", 1, 1)
	caller := f.symbol(f.mainFile, "make", "", "function", "make()", false)
	edge := f.call(f.mainFile, caller, "Service", "swift:initializer;labels=id:", 1, 1)
	f.resolve()
	if got := f.dst(edge); got.Valid {
		t.Fatalf("cross-file-only initializer resolved to %d", got.Int64)
	}
}

func TestSwiftInitializerAcceptanceP7AndTrailingVeto(t *testing.T) {
	f := newSwiftScopeFixture(t)
	f.symbol(f.mainFile, "Service", "", "struct", "", false)
	local := swiftInitCandidate(f, f.mainFile, "Service", "init()", 0, 0)
	testFile := f.file("Tests/ServiceTests.swift")
	// The test caller also needs a same-file nominal owner: that is the Store's
	// persisted owner proof, while the production candidate remains cross-file.
	testCandidate := swiftInitCandidate(f, testFile, "Service", "init()", 0, 0)
	prodCaller := f.symbol(f.mainFile, "make", "", "function", "make()", false)
	prodEdge := f.call(f.mainFile, prodCaller, "Service", "swift:initializer", 0, 1)
	testOwner := f.symbol(testFile, "Service", "", "struct", "", false)
	testCaller := f.symbol(testFile, "testMake", "", "function", "testMake()", false)
	testEdge := f.call(testFile, testCaller, "Service", "swift:initializer", 0, 1)
	trailing := f.call(f.mainFile, prodCaller, "Service", "swift:initializer;trailing_labels=_", 1, 2)
	mixedTrailing := f.call(f.mainFile, prodCaller, "Service", "swift:initializer;labels=id:;trailing_labels=_", 2, 3)
	f.resolve()
	assertSwiftInitBinding(t, f, prodEdge, local)
	if got := f.dst(testEdge); got.Valid {
		t.Fatalf("test caller resolved to %d", got.Int64)
	}
	if got := f.dst(trailing); got.Valid {
		t.Fatalf("trailing initializer resolved to %d", got.Int64)
	}
	if got := f.dst(mixedTrailing); got.Valid {
		t.Fatalf("mixed trailing initializer resolved to %d", got.Int64)
	}
	_ = testCandidate
	_ = testOwner
}

func TestSwiftInitializerAcceptanceReferencesRepairAndRetry(t *testing.T) {
	f := newSwiftScopeFixture(t)
	f.symbol(f.mainFile, "Service", "", "struct", "", false)
	target := swiftInitCandidate(f, f.mainFile, "Service", "init()", 0, 0)
	caller := f.symbol(f.mainFile, "make", "", "function", "make()", false)
	edge := f.call(f.mainFile, caller, "Service", "swift:initializer", 0, 1)
	f.reference(f.mainFile, caller, "Service", 1)
	if ran, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID); err != nil || !ran {
		t.Fatalf("repair=(%v,%v)", ran, err)
	}
	assertSwiftInitBinding(t, f, edge, target)
	var ref sql.NullInt64
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT symbol_id FROM references_tbl WHERE repo_id=?`, f.repoID).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	if !ref.Valid || ref.Int64 != target {
		t.Fatalf("reference=%v want %d", ref, target)
	}
	if ran, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID); err != nil || ran {
		t.Fatalf("second repair=(%v,%v)", ran, err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET arity_max=1 WHERE id=?`, target); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ResolveEdgesForPathsAndNames(f.ctx, f.repoID, []string{"Service.swift"}, []string{"init"}); err != nil {
		t.Fatal(err)
	}
	if got := f.dst(edge); got.Valid {
		t.Fatalf("defaulted target remained %d", got.Int64)
	}
	if err := f.store.ReconcileReferenceIdentities(f.ctx, f.repoID); err != nil {
		t.Fatal(err)
	}
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT symbol_id FROM references_tbl WHERE repo_id=?`, f.repoID).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	if ref.Valid {
		t.Fatalf("reference remained %d", ref.Int64)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET arity_max=0 WHERE id=?`, target); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ResolveEdgesForPathsAndNames(f.ctx, f.repoID, []string{"Service.swift"}, []string{"init"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftInitBinding(t, f, edge, target)
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT symbol_id FROM references_tbl WHERE repo_id=?`, f.repoID).Scan(&ref); err != nil {
		t.Fatal(err)
	}
	if !ref.Valid || ref.Int64 != target {
		t.Fatalf("restored reference=%v want %d", ref, target)
	}
}

func TestSwiftInitializerAcceptanceRepairFailureRetries(t *testing.T) {
	f := newSwiftScopeFixture(t)
	f.symbol(f.mainFile, "Service", "", "struct", "", false)
	target := swiftInitCandidate(f, f.mainFile, "Service", "init()", 0, 0)
	caller := f.symbol(f.mainFile, "make", "", "function", "make()", false)
	edge := f.call(f.mainFile, caller, "Service", "swift:initializer", 0, 1)
	canceled, cancel := context.WithCancel(f.ctx)
	cancel()
	if _, err := f.store.RepairResolverBindingsOnce(canceled, f.repoID); err == nil {
		t.Fatal("canceled repair unexpectedly succeeded")
	}
	var marker string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT value FROM settings WHERE key=?`, swiftInitializerRepairSettingKey+fmt.Sprintf(".%d", f.repoID)).Scan(&marker); err == nil {
		t.Fatalf("failed repair wrote marker %q", marker)
	}
	if ran, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID); err != nil || !ran {
		t.Fatalf("retry=(%v,%v)", ran, err)
	}
	assertSwiftInitBinding(t, f, edge, target)
}

func TestSwiftInitializerAcceptanceIncrementalTransitions(t *testing.T) {
	f := newSwiftScopeFixture(t)
	f.symbol(f.mainFile, "Service", "", "struct", "", false)
	caller := f.symbol(f.mainFile, "make", "", "function", "make()", false)
	edge := f.call(f.mainFile, caller, "Service", "swift:initializer", 0, 1)
	f.resolve()
	if got := f.dst(edge); got.Valid {
		t.Fatalf("synthesized call resolved to %d", got.Int64)
	}
	target := swiftInitCandidate(f, f.mainFile, "Service", "init()", 0, 0)
	if _, err := f.store.ResolveEdgesForPathsAndNames(f.ctx, f.repoID, []string{"Service.swift"}, []string{"init"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftInitBinding(t, f, edge, target)
	deletedFile := f.file("Deleted.swift")
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET file_id=? WHERE id=?`, deletedFile, target); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE files SET is_deleted=1 WHERE id=?`, deletedFile); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ResolveEdgesForPathsAndNames(f.ctx, f.repoID, []string{"Service.swift", "Deleted.swift"}, []string{"init"}); err != nil {
		t.Fatal(err)
	}
	if got := f.dst(edge); got.Valid {
		t.Fatalf("deleted init remained %d", got.Int64)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET file_id=? WHERE id=?`, f.mainFile, target); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE files SET is_deleted=0 WHERE id=?`, deletedFile); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ResolveEdgesForPathsAndNames(f.ctx, f.repoID, []string{"Service.swift", "Deleted.swift"}, []string{"init"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftInitBinding(t, f, edge, target)
}

func TestSwiftInitializerAcceptanceIncrementalCompetitorAndShapeTransitions(t *testing.T) {
	f := newSwiftScopeFixture(t)
	f.symbol(f.mainFile, "Service", "", "struct", "", false)
	local := swiftInitCandidate(f, f.mainFile, "Service", "init()", 0, 0)
	caller := f.symbol(f.mainFile, "make", "", "function", "make()", false)
	edge := f.call(f.mainFile, caller, "Service", "swift:initializer", 0, 1)
	f.reference(f.mainFile, caller, "Service", 1)
	f.resolve()
	assertSwiftInitBinding(t, f, edge, local)
	other := f.file("Other.swift")
	competitor := swiftInitCandidate(f, other, "Service", "init()", 0, 0)
	if _, err := f.store.ResolveEdgesForPathsAndNames(f.ctx, f.repoID, []string{"Other.swift"}, []string{"init"}); err != nil {
		t.Fatal(err)
	}
	if got := f.dst(edge); got.Valid {
		t.Fatalf("competitor left binding %d", got.Int64)
	}
	assertSwiftInitReference(t, f, nil)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE files SET is_deleted=1 WHERE id=?`, other); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ResolveEdgesForPathsAndNames(f.ctx, f.repoID, []string{"Other.swift"}, []string{"init"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftInitBinding(t, f, edge, local)
	assertSwiftInitReference(t, f, &local)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE files SET is_deleted=0 WHERE id=?`, other); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET signature='init(name:)' WHERE id=?`, competitor); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ResolveEdgesForPathsAndNames(f.ctx, f.repoID, []string{"Other.swift"}, []string{"init"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftInitBinding(t, f, edge, local)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET arity_max=1 WHERE id=?`, local); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ResolveEdgesForPathsAndNames(f.ctx, f.repoID, []string{"Service.swift"}, []string{"init"}); err != nil {
		t.Fatal(err)
	}
	if got := f.dst(edge); got.Valid {
		t.Fatalf("defaulted local remained %d", got.Int64)
	}
	assertSwiftInitReference(t, f, nil)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET arity_max=0 WHERE id=?`, local); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ResolveEdgesForPathsAndNames(f.ctx, f.repoID, []string{"Service.swift"}, []string{"init"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftInitBinding(t, f, edge, local)
	assertSwiftInitReference(t, f, &local)
}

func TestSwiftInitializerAcceptanceFreshEqualsIncremental(t *testing.T) {
	fresh := newSwiftScopeFixture(t)
	fresh.symbol(fresh.mainFile, "Service", "", "struct", "", false)
	freshTarget := swiftInitCandidate(fresh, fresh.mainFile, "Service", "init()", 0, 0)
	freshCaller := fresh.symbol(fresh.mainFile, "make", "", "function", "make()", false)
	freshEdge := fresh.call(fresh.mainFile, freshCaller, "Service", "swift:initializer", 0, 1)
	fresh.resolve()
	assertSwiftInitBinding(t, fresh, freshEdge, freshTarget)

	incremental := newSwiftScopeFixture(t)
	incremental.symbol(incremental.mainFile, "Service", "", "struct", "", false)
	incrementalCaller := incremental.symbol(incremental.mainFile, "make", "", "function", "make()", false)
	incrementalEdge := incremental.call(incremental.mainFile, incrementalCaller, "Service", "swift:initializer", 0, 1)
	incremental.resolve()
	incrementalTarget := swiftInitCandidate(incremental, incremental.mainFile, "Service", "init()", 0, 0)
	if _, err := incremental.store.ResolveEdgesForPathsAndNames(incremental.ctx, incremental.repoID, []string{"Service.swift"}, []string{"init"}); err != nil {
		t.Fatal(err)
	}
	assertSwiftInitBinding(t, incremental, incrementalEdge, incrementalTarget)
	var freshName, incrementalName string
	if err := fresh.store.db.QueryRowContext(fresh.ctx, `SELECT s.qualified_name FROM symbols s JOIN edges e ON e.dst_symbol_id=s.id WHERE e.id=?`, freshEdge).Scan(&freshName); err != nil {
		t.Fatal(err)
	}
	if err := incremental.store.db.QueryRowContext(incremental.ctx, `SELECT s.qualified_name FROM symbols s JOIN edges e ON e.dst_symbol_id=s.id WHERE e.id=?`, incrementalEdge).Scan(&incrementalName); err != nil {
		t.Fatal(err)
	}
	if freshName != incrementalName || freshName != "Service.init" {
		t.Fatalf("fresh=%q incremental=%q", freshName, incrementalName)
	}
}

func TestSwiftInitializerAcceptanceRepairApplicability(t *testing.T) {
	for _, tc := range []struct {
		name, evidence string
		want           bool
	}{
		{"no-swift", "", false},
		{"self-only", "swift:self", false},
		{"unproven-only", "swift:initializer_unproven", false},
		{"positive", "swift:initializer", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwiftScopeFixture(t)
			if tc.name == "no-swift" {
				_, err := f.store.db.ExecContext(f.ctx, `UPDATE files SET language='go' WHERE id=?`, f.mainFile)
				if err != nil {
					t.Fatal(err)
				}
			}
			caller := f.symbol(f.mainFile, "make", "", "function", "make()", false)
			f.call(f.mainFile, caller, "Service", tc.evidence, 0, 1)
			got, err := f.store.swiftInitializerRepairApplies(f.ctx, f.repoID)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("applies=%v want %v", got, tc.want)
			}
		})
	}
	f := newSwiftScopeFixture(t)
	f.symbol(f.mainFile, "Service", "", "struct", "", false)
	target := swiftInitCandidate(f, f.mainFile, "Service", "init()", 0, 0)
	caller := f.symbol(f.mainFile, "make", "", "function", "make()", false)
	edge := f.call(f.mainFile, caller, "Service", "swift:initializer", 0, 1)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET dst_symbol_id=? WHERE id=?`, target, edge); err != nil {
		t.Fatal(err)
	}
	got, err := f.store.swiftInitializerRepairApplies(f.ctx, f.repoID)
	if err != nil || !got {
		t.Fatalf("bound positive applies=(%v,%v)", got, err)
	}
}

func TestSwiftInitializerAcceptanceStatsIndividualCases(t *testing.T) {
	for _, tc := range []struct {
		name, evidence string
		candidate      bool
		wantResolved   int
	}{
		{"bound", "swift:initializer", true, 1},
		{"unbound", "swift:initializer", false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSwiftScopeFixture(t)
			f.symbol(f.mainFile, "Service", "", "struct", "", false)
			if tc.candidate {
				swiftInitCandidate(f, f.mainFile, "Service", "init()", 0, 0)
			}
			caller := f.symbol(f.mainFile, "make", "", "function", "make()", false)
			f.call(f.mainFile, caller, "Service", tc.evidence, 0, 1)
			stats := f.resolveNames("Service")
			assertSwiftStats(t, stats, 1, tc.wantResolved, 1-tc.wantResolved, 0)
		})
	}
}

func TestSwiftInitializerAcceptanceBatch(t *testing.T) {
	f := newSwiftScopeFixture(t)
	caller := f.symbol(f.mainFile, "make", "", "function", "make()", false)
	other := f.file("Other.swift")
	const total = 1205
	for i := 0; i < total; i++ {
		owner := fmt.Sprintf("Service%d", i)
		if i == 0 {
			owner = "Outer0.Inner"
			f.symbol(f.mainFile, "Outer0", "", "struct", "", false)
			f.symbol(f.mainFile, "Inner", "Outer0", "struct", "", false)
		} else {
			f.symbol(f.mainFile, owner, "", "struct", "", false)
		}
		if i%4 == 0 {
			if i == 0 {
				swiftInitCandidate(f, f.mainFile, owner, "init()", 0, 0)
				f.call(f.mainFile, caller, owner, "swift:initializer;generic_specialization=true", 0, i+1)
			} else {
				selector := fmt.Sprintf("init(id%d:)", i)
				swiftInitCandidate(f, f.mainFile, owner, selector, 1, 1)
				f.call(f.mainFile, caller, owner, "swift:initializer;labels="+fmt.Sprintf("id%d:", i), 1, i+1)
			}
		} else if i%4 == 2 {
			selector := fmt.Sprintf("init(id%d:)", i)
			swiftInitCandidate(f, f.mainFile, owner, selector, 1, 1)
			swiftInitCandidate(f, f.mainFile, owner, selector, 1, 1)
			f.call(f.mainFile, caller, owner, "swift:initializer;labels="+fmt.Sprintf("id%d:", i), 1, i+1)
		} else if i%4 == 3 {
			selector := fmt.Sprintf("init(id%d:)", i)
			swiftInitCandidate(f, f.mainFile, owner, selector, 1, 1)
			swiftInitCandidate(f, other, owner, selector, 1, 1)
			f.call(f.mainFile, caller, owner, "swift:initializer;labels="+fmt.Sprintf("id%d:", i), 1, i+1)
		} else {
			f.call(f.mainFile, caller, owner, "swift:initializer;labels="+fmt.Sprintf("id%d:", i), 1, i+1)
		}
	}
	f.resolve()
	var count int
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT COUNT(*) FROM edges WHERE repo_id=? AND dst_symbol_id IS NOT NULL AND resolution_strategy=?`, f.repoID, ResolutionStrategySwiftInitializerScope).Scan(&count); err != nil {
		t.Fatal(err)
	}
	want := (total + 3) / 4
	if count != want {
		t.Fatalf("resolved initializer count=%d want %d", count, want)
	}
}
