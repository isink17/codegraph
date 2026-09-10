package store

import (
	"context"
	"database/sql"
	"fmt"
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
