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
