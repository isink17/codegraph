package store

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
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
	var reference sql.NullInt64
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT symbol_id FROM references_tbl WHERE repo_id=?`, f.repoID).Scan(&reference); err != nil {
		t.Fatal(err)
	}
	if reference.Valid {
		t.Fatalf("unsafe reference survived=%d", reference.Int64)
	}
}

func TestSwiftClassSelfTypeBatchedMixedEdges(t *testing.T) {
	f := newSwiftScopeFixture(t)
	type category struct {
		name  string
		count int
		bound bool
	}
	categories := []category{
		{"static", 300, true}, {"class", 200, true}, {"nonfinal", 100, false},
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
	if resolved1 != 650 || unresolved1 != 550 || resolved1+unresolved1 != total {
		t.Fatalf("counts=(%d,%d), want (650,550)", resolved1, unresolved1)
	}
	for _, tc := range categories {
		if tc.bound {
			assertSwiftEdgeMetadata(t, f, samples[tc.name], sampleTargets[tc.name], ResolutionStrategySwiftClassSelfTypeFinalScope)
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
	if got := f.dst(samples["nonfinal"]); got.Valid {
		t.Fatalf("non-final sample resolved=%d", got.Int64)
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
	var reference sql.NullInt64
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT symbol_id FROM references_tbl WHERE repo_id=?`, f.repoID).Scan(&reference); err != nil {
		t.Fatal(err)
	}
	if !reference.Valid || reference.Int64 != target {
		t.Fatalf("reference=%v, want %d", reference, target)
	}
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
	const total = 1400
	var edges []int64
	for _, tc := range categories {
		ownerName, ownerPrefix := "Service_"+tc.name, ""
		if tc.name == "qualified" {
			ownerName, ownerPrefix = "ServiceQualified", "Outer"
		}
		qualifiedOwner := ownerName
		if ownerPrefix != "" {
			qualifiedOwner = ownerPrefix + "." + ownerName
		}
		owner := f.symbol(f.mainFile, ownerName, ownerPrefix, "class", "", false)
		f.declarationFact(f.mainFile, owner, tc.ownerFinal)
		if tc.name == "superclass" || tc.name == "conformance" || tc.name == "unproven" {
			f.relation(f.mainFile, qualifiedOwner, "P", tc.name, false, false)
		}
		for i := 0; i < tc.count; i++ {
			safeName := strings.ReplaceAll(tc.name, "-", "_")
			name := fmt.Sprintf("make_%s_%d", safeName, i)
			target := f.symbol(f.mainFile, name, qualifiedOwner, "function", name+"()", tc.targetStatic)
			if tc.name != "missing-target" {
				f.dispatchFact(f.mainFile, target, tc.name == "instance-final", tc.targetDispatch)
			}
			if tc.name == "duplicate-target" {
				f.dispatchFact(f.mainFile, target, false, "static")
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
			if tc.name == "same-blocker" {
				f.blocker(f.mainFile, qualifiedOwner, name, graph.ScopeImportSwiftMemberValue, true)
			}
			if tc.name == "opposite-blocker" {
				f.blocker(f.mainFile, qualifiedOwner, name, graph.ScopeImportSwiftMemberValue, false)
			}
		}
	}
	if len(edges) != total {
		t.Fatalf("edges=%d, want %d", len(edges), total)
	}
	f.resolve()
	counts := func() [5]int {
		var result [5]int
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
		} {
			if err := f.store.db.QueryRowContext(f.ctx, query.sql, query.args...).Scan(query.dst); err != nil {
				t.Fatal(err)
			}
		}
		return result
	}
	first := counts()
	if first != [5]int{600, 800, 100, 100, 400} {
		t.Fatalf("first counts=%v, want [600 800 100 100 400]", first)
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
