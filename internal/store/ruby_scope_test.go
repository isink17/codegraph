package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"
)

// rubyFixture builds Ruby graphs directly in the Store with the P22.46 fact
// shapes: dotted semantic qnames with no filename in them, instance/singleton
// staticness on every method, and call edges carrying the parser's
// receiver-category evidence.
type rubyFixture struct {
	*parityFixture
}

func newRubyFixture(t *testing.T) *rubyFixture {
	return &rubyFixture{newParityFixture(t, "")}
}

func (f *rubyFixture) rb(t *testing.T, path string) int64 {
	return f.file(t, path, "ruby")
}

func (f *rubyFixture) insert(t *testing.T, fileID int64, kind, qname string, static sql.NullInt64) int64 {
	t.Helper()
	container, name := "", qname
	if dot := strings.LastIndexByte(qname, '.'); dot >= 0 {
		container, name = qname[:dot], qname[dot+1:]
	}
	nature := "instance"
	if static.Valid && static.Int64 == 1 {
		nature = "singleton"
	}
	owner := container
	if owner == "" {
		owner = "top"
	}
	// P22.46 stable keys are filename-free and separate the two natures.
	stable := kind + ":ruby:" + qname
	if kind == "function" {
		stable = "func:ruby:" + owner + ":" + nature + ":" + name
	}
	res, err := f.store.db.ExecContext(f.ctx, `
		INSERT INTO symbols(repo_id, file_id, language, kind, name, qualified_name, container_name, visibility, is_static,
			start_line, start_col, end_line, end_col, stable_key, qualified_suffix, dot_tail2, dot_tail3)
		VALUES(?, ?, 'ruby', ?, ?, ?, ?, '', ?, 1, 1, 1, 1, ?, ?, ?, ?)`,
		f.repoID, fileID, kind, name, qname, container, static,
		stable, qualifiedSuffix(qname), dotTail2(qname), dotTail3(qname))
	if err != nil {
		t.Fatalf("insert %s %s: %v", kind, qname, err)
	}
	id, _ := res.LastInsertId()
	return id
}

// typ declares a class or module; its container is its lexical parent.
func (f *rubyFixture) typ(t *testing.T, fileID int64, qname string) int64 {
	return f.insert(t, fileID, "type", qname, sql.NullInt64{})
}

// method declares `def name` (static=false) or `def self.name` (static=true).
func (f *rubyFixture) method(t *testing.T, fileID int64, qname string, static bool) int64 {
	v := int64(0)
	if static {
		v = 1
	}
	return f.insert(t, fileID, "function", qname, sql.NullInt64{Int64: v, Valid: true})
}

func (f *rubyFixture) call(t *testing.T, fileID int64, src sql.NullInt64, dst, evidence string, line int) int64 {
	t.Helper()
	res, err := f.store.db.ExecContext(f.ctx, `
		INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line)
		VALUES(?, ?, NULL, ?, 'calls', ?, ?, ?)`, f.repoID, src, dst, evidence, fileID, line)
	if err != nil {
		t.Fatalf("insert edge %s: %v", dst, err)
	}
	id, _ := res.LastInsertId()
	return id
}

func (f *rubyFixture) reference(t *testing.T, fileID int64, name string, line int) {
	t.Helper()
	if _, err := f.store.db.ExecContext(f.ctx, `
		INSERT INTO references_tbl(repo_id, file_id, ref_kind, name, qualified_name, start_line, start_col, end_line, end_col)
		VALUES (?, ?, 'call', ?, ?, ?, 1, ?, 1)`, f.repoID, fileID, name, name, line, line); err != nil {
		t.Fatal(err)
	}
}

func (f *rubyFixture) markerSet(t *testing.T, key string) bool {
	t.Helper()
	var value string
	err := f.store.db.QueryRowContext(f.ctx, `SELECT value FROM settings WHERE key = ?`, key+"."+strconv.FormatInt(f.repoID, 10)).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return value == "1"
}

func (f *rubyFixture) assertReference(t *testing.T, line int, wantSymbol, wantContext sql.NullInt64) {
	t.Helper()
	var symbol, ctxID sql.NullInt64
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT symbol_id, context_symbol_id FROM references_tbl WHERE repo_id = ? AND start_line = ?`, f.repoID, line).Scan(&symbol, &ctxID); err != nil {
		t.Fatal(err)
	}
	if symbol != wantSymbol || ctxID != wantContext {
		t.Fatalf("reference line %d = (%v,%v), want (%v,%v)", line, symbol, ctxID, wantSymbol, wantContext)
	}
}

var rubyEntryPoints = []string{"full", "paths", "names", "paths+names"}

// The acceptance fixture: one container declaring an instance and a singleton
// method of each name, and one caller of each nature reaching both by implicit
// and by literal self. The caller's staticness alone decides which nature it
// reaches, on every resolver entry point, with no insertion-order dependency.
func TestRubyScopeInstanceAndSingletonSelf(t *testing.T) {
	f := newRubyFixture(t)
	file := f.rb(t, "app/service.rb")
	f.typ(t, file, "App")
	f.typ(t, file, "App.Service")
	instanceRun := f.method(t, file, "App.Service.run", false)
	instanceHelper := f.method(t, file, "App.Service.helper", false)
	singletonRun := f.method(t, file, "App.Service.run", true)
	singletonHelper := f.method(t, file, "App.Service.helper", true)
	instanceCaller := f.method(t, file, "App.Service.instance_caller", false)
	singletonCaller := f.method(t, file, "App.Service.singleton_caller", true)

	type want struct {
		edge int64
		dst  int64
		want string
	}
	cases := []want{
		{f.call(t, file, srcOf(instanceCaller), "run", rubyImplicitReceiver, 1), instanceRun, ResolutionStrategyRubyImplicitSelf},
		{f.call(t, file, srcOf(instanceCaller), "self.helper", rubySelfReceiver, 2), instanceHelper, ResolutionStrategyRubyExplicitSelf},
		{f.call(t, file, srcOf(instanceCaller), "self::helper", rubySelfReceiver, 3), instanceHelper, ResolutionStrategyRubyExplicitSelf},
		{f.call(t, file, srcOf(singletonCaller), "run", rubyImplicitReceiver, 4), singletonRun, ResolutionStrategyRubyImplicitSelf},
		{f.call(t, file, srcOf(singletonCaller), "self.helper", rubySelfReceiver, 5), singletonHelper, ResolutionStrategyRubyExplicitSelf},
	}
	for _, entry := range rubyEntryPoints {
		f.clearAll(t)
		f.resolveVia(t, entry, []string{"app/service.rb"}, []string{"run", "helper", "self.helper", "self::helper"})
		for _, c := range cases {
			want := f.qualifiedOf(t, c.dst) + "|" + c.want + "|" + ResolutionConfidenceHigh
			if got := f.binding(t, c.edge); got != want {
				t.Fatalf("%s: edge %d = %s, want %s (dst symbol %d)", entry, c.edge, got, want, c.dst)
			}
			var dst int64
			if err := f.store.db.QueryRowContext(f.ctx, `SELECT dst_symbol_id FROM edges WHERE id = ?`, c.edge).Scan(&dst); err != nil {
				t.Fatal(err)
			}
			if dst != c.dst {
				t.Fatalf("%s: edge %d bound symbol %d, want %d (wrong instance/singleton nature)", entry, c.edge, dst, c.dst)
			}
		}
	}
}

// Every Ruby receiver form this phase does not resolve stays unresolved on
// every entry point, even when a uniquely tempting generic candidate exists --
// a symbol whose qualified name IS the spelling, whose bare name is unique, and
// whose dot tails match. Ownership, not abstention, is what keeps them NULL.
func TestRubyScopeVetoSurvivesEveryGenericStrategy(t *testing.T) {
	f := newRubyFixture(t)
	callerFile := f.rb(t, "app/caller.rb")
	baitFile := f.rb(t, "app/bait.rb")
	f.typ(t, callerFile, "Caller")
	caller := f.method(t, callerFile, "Caller.f", false)
	// The one destination a generic strategy would love to reach.
	f.typ(t, baitFile, "Service")
	f.method(t, baitFile, "Service.run", true)

	unsupported := []struct {
		name, dst, evidence string
	}{
		{"constant dot", "Service.run", "ruby:constant_receiver"},
		{"constant scope", "Service::run", "ruby:constant_receiver"},
		{"nested constant", "A::B.run", "ruby:constant_receiver"},
		{"root constant", "::A::B.run", "ruby:constant_receiver"},
		{"value", "obj.run", "ruby:value_receiver"},
		{"safe navigation", "obj&.run", "ruby:safe_navigation"},
		{"self safe navigation", "self&.run", "ruby:safe_navigation"},
		{"chained", "factory.service.run", "ruby:chained_receiver"},
	}
	edges := map[string]int64{}
	for i, tc := range unsupported {
		f.insert(t, baitFile, "function", tc.dst, sql.NullInt64{Int64: 1, Valid: true})
		edges[tc.name] = f.call(t, callerFile, srcOf(caller), tc.dst, tc.evidence, i+1)
	}
	// A top-level call to the repository's only `run` is not modelled either:
	// Ruby's top-level self is not this phase's to claim.
	topFile := f.rb(t, "app/top.rb")
	topRun := f.insert(t, topFile, "function", "boot", sql.NullInt64{Int64: 0, Valid: true})
	topCaller := f.insert(t, topFile, "function", "main", sql.NullInt64{Int64: 0, Valid: true})
	edges["top level"] = f.call(t, topFile, srcOf(topCaller), "boot", rubyImplicitReceiver, 40)
	// A class-body call has a type, not a method, as its source symbol.
	bodyType := f.typ(t, topFile, "Configured")
	edges["class body"] = f.call(t, topFile, srcOf(bodyType), "boot", rubyImplicitReceiver, 41)
	// An edge whose source symbol is not a Ruby method cannot prove whose self
	// this is; repository uniqueness is not source evidence.
	odd := f.insert(t, callerFile, "variable", "Caller.config", sql.NullInt64{})
	edges["source-less"] = f.call(t, callerFile, srcOf(odd), "run", rubyImplicitReceiver, 42)
	_ = topRun

	names := []string{"run", "boot", "Service.run", "Service::run", "A::B.run", "::A::B.run", "obj.run", "obj&.run", "self&.run", "factory.service.run"}
	for _, entry := range rubyEntryPoints {
		f.clearAll(t)
		f.resolveVia(t, entry, []string{"app/caller.rb", "app/bait.rb", "app/top.rb"}, names)
		for name, id := range edges {
			if got := f.binding(t, id); got != "<unresolved>" {
				t.Fatalf("%s: %s bound to %s", entry, name, got)
			}
		}
	}
}

// A cross-language reference edge out of a Ruby file is not an ordinary call
// and keeps its generic answer: the Ruby ownership veto is scoped to calls.
func TestRubyScopeLeavesCrossLanguageReferencesAlone(t *testing.T) {
	f := newRubyFixture(t)
	rbFile := f.rb(t, "app/view.rb")
	f.typ(t, rbFile, "View")
	caller := f.method(t, rbFile, "View.render", false)
	goFile := f.file(t, "server/handler.go", "go")
	target := f.symbolIn(t, goFile, "Render", "handler.Render", "function", "handler", "go")
	var edge int64
	res, err := f.store.db.ExecContext(f.ctx, `
		INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line)
		VALUES(?, ?, ?, 'handler.Render', ?, 'http_route', ?, 1)`,
		f.repoID, caller, target, EdgeKindCrossLanguageRef, rbFile)
	if err != nil {
		t.Fatal(err)
	}
	edge, _ = res.LastInsertId()
	for _, entry := range rubyEntryPoints {
		f.resolveVia(t, entry, []string{"app/view.rb"}, []string{"Render", "handler.Render"})
		var dst sql.NullInt64
		if err := f.store.db.QueryRowContext(f.ctx, `SELECT dst_symbol_id FROM edges WHERE id = ?`, edge).Scan(&dst); err != nil {
			t.Fatal(err)
		}
		if !dst.Valid || dst.Int64 != target {
			t.Fatalf("%s: cross-language edge lost its destination: %v", entry, dst)
		}
	}
}

// Ruby classes reopen across files, so the semantic container name -- never a
// single type row or a filename -- is the identity. Two eligible declarations
// of the same nature are a redefinition whose runtime winner is decided by load
// order, which is not modelled: fail closed. The opposite nature is a different
// method and never makes the call ambiguous.
func TestRubyScopeReopenedContainerAndDuplicateMethods(t *testing.T) {
	f := newRubyFixture(t)
	a := f.rb(t, "app/a.rb")
	b := f.rb(t, "app/b.rb")
	c := f.rb(t, "app/c.rb")
	f.typ(t, a, "Service") // reopened in b and c: repeated type rows are normal
	f.typ(t, b, "Service")
	f.typ(t, c, "Service")
	caller := f.method(t, a, "Service.f", false)
	target := f.method(t, b, "Service.run", false)
	edge := f.call(t, a, srcOf(caller), "run", rubyImplicitReceiver, 1)

	paths := []string{"app/a.rb", "app/b.rb", "app/c.rb"}
	names := []string{"run"}
	want := "Service.run|" + ResolutionStrategyRubyImplicitSelf + "|" + ResolutionConfidenceHigh
	for _, entry := range rubyEntryPoints {
		f.clearAll(t)
		f.resolveVia(t, entry, paths, names)
		if got := f.binding(t, edge); got != want {
			t.Fatalf("%s: reopened container = %s, want %s", entry, got, want)
		}
	}

	// A singleton sibling of the same name is a different method.
	singleton := f.method(t, c, "Service.run", true)
	for _, entry := range rubyEntryPoints {
		f.clearAll(t)
		f.resolveVia(t, entry, paths, names)
		if got := f.binding(t, edge); got != want {
			t.Fatalf("%s: singleton sibling made the instance call ambiguous: %s", entry, got)
		}
	}

	// A second instance declaration is a redefinition: no load-order guess.
	dup := f.method(t, c, "Service.run", false)
	for _, entry := range rubyEntryPoints {
		f.clearAll(t)
		f.resolveVia(t, entry, paths, names)
		if got := f.binding(t, edge); got != "<unresolved>" {
			t.Fatalf("%s: duplicate declaration bound to %s", entry, got)
		}
	}

	// Removing the duplicate restores the unique answer; moving the survivor to
	// another file of the same reopened container changes nothing.
	if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM symbols WHERE id = ?`, dup); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET file_id = ? WHERE id = ?`, c, target); err != nil {
		t.Fatal(err)
	}
	for _, entry := range rubyEntryPoints {
		f.clearAll(t)
		f.resolveVia(t, entry, paths, names)
		if got := f.binding(t, edge); got != want {
			t.Fatalf("%s: after removing the duplicate = %s, want %s", entry, got, want)
		}
	}

	// Deleting the sole eligible target unbinds; the singleton must not stand in
	// for it, and restoring it rebinds with no stale id.
	if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM symbols WHERE id = ?`, target); err != nil {
		t.Fatal(err)
	}
	for _, entry := range rubyEntryPoints {
		f.clearAll(t)
		f.resolveVia(t, entry, paths, names)
		if got := f.binding(t, edge); got != "<unresolved>" {
			t.Fatalf("%s: instance caller bound singleton-only target %s", entry, got)
		}
	}
	_ = singleton
	restored := f.method(t, b, "Service.run", false)
	for _, entry := range rubyEntryPoints {
		f.clearAll(t)
		f.resolveVia(t, entry, paths, names)
		if got := f.binding(t, edge); got != want {
			t.Fatalf("%s: after restore = %s, want %s", entry, got, want)
		}
		var dst int64
		if err := f.store.db.QueryRowContext(f.ctx, `SELECT dst_symbol_id FROM edges WHERE id = ?`, edge).Scan(&dst); err != nil {
			t.Fatal(err)
		}
		if dst != restored {
			t.Fatalf("%s: stale destination %d, want %d", entry, dst, restored)
		}
	}
}

// A database written before this pass holds Ruby call targets the generic
// strategies decided. Ordinary Ruby calls are now owned here, so the repair
// clears them -- including the non-NULL ones -- and re-decides in the same
// transaction, with the marker written only after success.
func TestRubyScopeUpgradeRepairOldDatabase(t *testing.T) {
	f := newRubyFixture(t)
	file := f.rb(t, "app/service.rb")
	f.typ(t, file, "Service")
	run := f.method(t, file, "Service.run", false)
	caller := f.method(t, file, "Service.f", false)
	provable := f.call(t, file, srcOf(caller), "self.run", rubySelfReceiver, 1)
	f.reference(t, file, "self.run", 1)
	// The old generic answer for a receiver form this phase does not support.
	topFile := f.rb(t, "app/top.rb")
	topCaller := f.insert(t, topFile, "function", "main", sql.NullInt64{Int64: 0, Valid: true})
	stale := f.call(t, topFile, srcOf(topCaller), "run", rubyImplicitReceiver, 2)
	f.reference(t, topFile, "run", 2)
	f.setBinding(t, stale, run, ResolutionStrategyExactName, ResolutionConfidenceHigh)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE references_tbl SET symbol_id = ?, context_symbol_id = ? WHERE start_line = 2`, run, topCaller); err != nil {
		t.Fatal(err)
	}
	for _, repair := range resolverRepairs {
		if repair.key != rubyScopeRepairSettingKey {
			if err := f.store.markRepairDone(f.ctx, repair.key, f.repoID); err != nil {
				t.Fatal(err)
			}
		}
	}

	// A failed pass must not mark the repository repaired or commit a
	// half-cleared graph.
	canceled, cancel := context.WithCancel(f.ctx)
	cancel()
	if _, err := f.store.RepairResolverBindingsOnce(canceled, f.repoID); err == nil {
		t.Fatal("repair under a canceled context succeeded")
	}
	if f.markerSet(t, rubyScopeRepairSettingKey) {
		t.Fatal("marker written after a failed repair")
	}
	if got := f.binding(t, stale); got != "Service.run|"+ResolutionStrategyExactName+"|high" {
		t.Fatalf("failed repair left half-cleared state: %s", got)
	}

	resolvedRepoWide, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID)
	if err != nil {
		t.Fatal(err)
	}
	if !resolvedRepoWide {
		t.Fatal("Ruby scope repair did not report a repo-wide resolve")
	}
	if got, want := f.binding(t, provable), "Service.run|"+ResolutionStrategyRubyExplicitSelf+"|high"; got != want {
		t.Fatalf("provable edge after repair = %s, want %s", got, want)
	}
	if got := f.binding(t, stale); got != "<unresolved>" {
		t.Fatalf("top-level edge kept its old generic target: %s", got)
	}
	f.assertReference(t, 1, srcOf(run), srcOf(caller))
	f.assertReference(t, 2, sql.NullInt64{}, srcOf(topCaller))
	if !f.markerSet(t, rubyScopeRepairSettingKey) || !f.markerSet(t, referenceIdentityRepairSettingKey) {
		t.Fatal("repair markers not set after success")
	}

	// Second run: nothing runs.
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET resolution_confidence = 'probe' WHERE id = ?`, provable); err != nil {
		t.Fatal(err)
	}
	if resolvedRepoWide, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID); err != nil || resolvedRepoWide {
		t.Fatalf("second repair: resolvedRepoWide=%v err=%v", resolvedRepoWide, err)
	}
	if got := f.binding(t, provable); got != "Service.run|"+ResolutionStrategyRubyExplicitSelf+"|probe" {
		t.Fatalf("second repair rewrote edges: %s", got)
	}
}

// A repository without Ruby has nothing to re-decide: the marker is written, no
// repo-wide resolve is claimed.
func TestRubyScopeRepairSkipsRepositoriesWithoutRuby(t *testing.T) {
	f := newRubyFixture(t)
	goFile := f.file(t, "main.go", "go")
	f.symbol(t, goFile, "main", "main.main", "function", "go")
	for _, repair := range resolverRepairs {
		if repair.key != rubyScopeRepairSettingKey {
			if err := f.store.markRepairDone(f.ctx, repair.key, f.repoID); err != nil {
				t.Fatal(err)
			}
		}
	}
	resolvedRepoWide, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID)
	if err != nil {
		t.Fatal(err)
	}
	if resolvedRepoWide {
		t.Fatal("Ruby repair claimed a repo-wide resolve on a repository without Ruby")
	}
	if !f.markerSet(t, rubyScopeRepairSettingKey) || !f.markerSet(t, referenceIdentityRepairSettingKey) {
		t.Fatal("markers after a skipped Ruby repair are not all set")
	}
}

// Both Ruby strategies are registered at one tier, redecidable incrementally,
// and the generic bind gate carries the ownership veto.
func TestRubyScopeStrategiesRegistered(t *testing.T) {
	for _, strategy := range rubyScopeStrategies {
		if got := resolutionConfidenceFor(strategy); got != ResolutionConfidenceHigh {
			t.Fatalf("%s confidence = %s", strategy, got)
		}
		found := false
		for _, s := range incrementallyRedecidableStrategies {
			found = found || s == strategy
		}
		if !found {
			t.Fatalf("%s is not incrementally redecidable", strategy)
		}
	}
	if !strings.Contains(resolverBindableCandidateSQL, rubyScopeVetoSQL) {
		t.Fatal("generic bind gate does not carry the Ruby ownership veto")
	}
}

// Only the two self evidences bind, and only on a bare or literal-self
// spelling: evidence, not punctuation, decides the receiver category.
func TestRubyScopeMethodRequiresSelfEvidenceAndSpelling(t *testing.T) {
	for _, tc := range []struct {
		evidence, dstName, method, strategy string
		ok                                  bool
	}{
		{rubyImplicitReceiver, "run", "run", ResolutionStrategyRubyImplicitSelf, true},
		{rubySelfReceiver, "self.run", "run", ResolutionStrategyRubyExplicitSelf, true},
		{rubySelfReceiver, "self::run", "run", ResolutionStrategyRubyExplicitSelf, true},
		{rubySelfReceiver, "self&.run", "", "", false},
		{rubySelfReceiver, "self.a.run", "", "", false},
		{rubySelfReceiver, "run", "", "", false},
		{rubySelfReceiver, "myself.run", "", "", false},
		{rubyImplicitReceiver, "obj.run", "", "", false},
		{rubyImplicitReceiver, "", "", "", false},
		{"ruby:constant_receiver", "Service.run", "", "", false},
		{"ruby:value_receiver", "obj.run", "", "", false},
		{"ruby:safe_navigation", "obj&.run", "", "", false},
		{"ruby:chained_receiver", "a.b.run", "", "", false},
	} {
		method, strategy, ok := rubyScopeMethod(tc.evidence, tc.dstName)
		if ok != tc.ok || method != tc.method || strategy != tc.strategy {
			t.Fatalf("rubyScopeMethod(%q, %q) = (%q, %q, %v), want (%q, %q, %v)",
				tc.evidence, tc.dstName, method, strategy, ok, tc.method, tc.strategy, tc.ok)
		}
	}
}

// TestRubyScopeBatchBudget drives the pass past the SQLite bound-variable
// ceiling on both dynamic sets it binds -- edge ids (incremental `only`) and
// candidate qualified names -- on every entry point.
func TestRubyScopeBatchBudget(t *testing.T) {
	f := newRubyFixture(t)
	file := f.rb(t, "app/service.rb")
	const n = 1200
	edges := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		q := fmt.Sprintf("C%d", i)
		f.typ(t, file, q)
		f.method(t, file, q+".run", false)
		caller := f.method(t, file, q+".f", false)
		edges = append(edges, f.call(t, file, srcOf(caller), "run", rubyImplicitReceiver, i+1))
	}
	for _, entry := range rubyEntryPoints {
		f.clearAll(t)
		f.resolveVia(t, entry, []string{"app/service.rb"}, []string{"run"})
		for i, id := range edges {
			want := fmt.Sprintf("C%d.run|%s|high", i, ResolutionStrategyRubyImplicitSelf)
			if got := f.binding(t, id); got != want {
				t.Fatalf("%s: edge %d = %s, want %s", entry, i, got, want)
			}
		}
	}
}

// Ruby classes reopen freely, so a spec file may add methods to a production
// class. A production caller must never be bound into one (P7), and the extra
// declaration must not make the production answer ambiguous either. A test
// caller may still reach the sole declaration wherever it lives.
func TestRubyScopeTestDeclarationsDoNotShadowProduction(t *testing.T) {
	f := newRubyFixture(t)
	app := f.rb(t, "app/service.rb")
	spec := f.rb(t, "app/service_spec.rb")
	f.typ(t, app, "Service")
	f.typ(t, spec, "Service")
	caller := f.method(t, app, "Service.go", false)
	specCaller := f.method(t, spec, "Service.expects", false)
	specOnly := f.method(t, spec, "Service.run", false)
	edge := f.call(t, app, srcOf(caller), "run", rubyImplicitReceiver, 1)
	specEdge := f.call(t, spec, srcOf(specCaller), "run", rubyImplicitReceiver, 2)

	paths := []string{"app/service.rb", "app/service_spec.rb"}
	names := []string{"run"}
	for _, entry := range rubyEntryPoints {
		f.clearAll(t)
		f.resolveVia(t, entry, paths, names)
		if got := f.binding(t, edge); got != "<unresolved>" {
			t.Fatalf("%s: production caller bound a test-only declaration: %s", entry, got)
		}
		if got := f.binding(t, specEdge); got != "Service.run|"+ResolutionStrategyRubyImplicitSelf+"|high" {
			t.Fatalf("%s: test caller = %s", entry, got)
		}
	}

	// The production declaration arrives: the production caller binds it, and
	// the test-only sibling neither shadows it nor makes it ambiguous.
	production := f.method(t, app, "Service.run", false)
	for _, entry := range rubyEntryPoints {
		f.clearAll(t)
		f.resolveVia(t, entry, paths, names)
		if got := f.binding(t, edge); got != "Service.run|"+ResolutionStrategyRubyImplicitSelf+"|high" {
			t.Fatalf("%s: production caller = %s", entry, got)
		}
		var dst int64
		if err := f.store.db.QueryRowContext(f.ctx, `SELECT dst_symbol_id FROM edges WHERE id = ?`, edge).Scan(&dst); err != nil {
			t.Fatal(err)
		}
		if dst != production {
			t.Fatalf("%s: production caller bound symbol %d, want %d", entry, dst, production)
		}
		// Two declarations now: the test caller may reach neither.
		if got := f.binding(t, specEdge); got != "<unresolved>" {
			t.Fatalf("%s: test caller bound %s with two declarations", entry, got)
		}
	}
	_ = specOnly
}

// A candidate whose file is soft-deleted but not yet purged is not a
// destination: the resolve window between marking and purging must not bind
// into a file that is about to disappear.
func TestRubyScopeIgnoresSoftDeletedFiles(t *testing.T) {
	f := newRubyFixture(t)
	app := f.rb(t, "app/service.rb")
	old := f.rb(t, "app/legacy.rb")
	f.typ(t, app, "Service")
	f.typ(t, old, "Service")
	caller := f.method(t, app, "Service.go", false)
	f.method(t, old, "Service.run", false)
	edge := f.call(t, app, srcOf(caller), "run", rubyImplicitReceiver, 1)
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE files SET is_deleted = 1 WHERE id = ?`, old); err != nil {
		t.Fatal(err)
	}
	for _, entry := range rubyEntryPoints {
		f.clearAll(t)
		f.resolveVia(t, entry, []string{"app/service.rb"}, []string{"run"})
		if got := f.binding(t, edge); got != "<unresolved>" {
			t.Fatalf("%s: bound a symbol in a soft-deleted file: %s", entry, got)
		}
	}
}

// The Ruby ownership predicate has two spellings -- one SQL, one Go -- and the
// repo-wide and incremental paths would silently disagree if they drifted.
func TestRubyScopeVetoSQLMatchesGoTwin(t *testing.T) {
	f := newRubyFixture(t)
	rbFile := f.rb(t, "app/service.rb")
	goFile := f.file(t, "main.go", "go")
	target := f.symbolIn(t, goFile, "Render", "handler.Render", "function", "handler", "go")
	f.typ(t, rbFile, "Service")
	caller := f.method(t, rbFile, "Service.f", false)
	goCaller := f.symbolIn(t, goFile, "run", "main.run", "function", "main", "go")

	type row struct {
		id       int64
		language string
		kind     string
	}
	var rows []row
	add := func(fileID int64, src int64, language, kind string) {
		res, err := f.store.db.ExecContext(f.ctx, `
			INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line)
			VALUES(?, ?, ?, 'run', ?, '', ?, 1)`,
			f.repoID, src, sql.NullInt64{Int64: target, Valid: kind == EdgeKindCrossLanguageRef}, kind, fileID)
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		rows = append(rows, row{id: id, language: language, kind: kind})
	}
	add(rbFile, caller, "ruby", EdgeKindCalls)
	add(rbFile, caller, "ruby", EdgeKindCrossLanguageRef)
	add(goFile, goCaller, "go", EdgeKindCalls)
	add(goFile, goCaller, "go", EdgeKindCrossLanguageRef)

	// The SQL twin: the veto is a NOT, so its negation is the owned set.
	ownedBySQL := map[int64]struct{}{}
	sqlRows, err := f.store.db.QueryContext(f.ctx, `
		SELECT edges.id FROM edges JOIN files f ON f.id = edges.file_id
		WHERE edges.repo_id = ? AND NOT (`+rubyScopeVetoSQL+`)`, f.repoID)
	if err != nil {
		t.Fatal(err)
	}
	defer sqlRows.Close()
	for sqlRows.Next() {
		var id int64
		if err := sqlRows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ownedBySQL[id] = struct{}{}
	}
	if err := sqlRows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		_, sqlOwned := ownedBySQL[r.id]
		goOwned := rubyScopeOwned(edgeTarget{srcLanguage: r.language, edgeKind: r.kind})
		if sqlOwned != goOwned {
			t.Fatalf("%s/%s: rubyScopeVetoSQL owned=%v, rubyScopeOwned=%v", r.language, r.kind, sqlOwned, goOwned)
		}
		if want := r.language == "ruby" && r.kind == EdgeKindCalls; goOwned != want {
			t.Fatalf("%s/%s: owned=%v, want %v", r.language, r.kind, goOwned, want)
		}
	}
}

// A `self::helper` binding carries a tail but no dot, so it is invalidated by
// its own selection rather than by the dotted-name scan. When a second
// declaration of the same nature arrives, the name pass must unbind it.
func TestRubyScopeSelfScopeSpellingIsInvalidatedByName(t *testing.T) {
	f := newRubyFixture(t)
	app := f.rb(t, "app/service.rb")
	other := f.rb(t, "app/other.rb")
	f.typ(t, app, "Service")
	f.typ(t, other, "Service")
	caller := f.method(t, app, "Service.f", false)
	f.method(t, app, "Service.helper", false)
	edge := f.call(t, app, srcOf(caller), "self::helper", rubySelfReceiver, 1)

	f.resolveVia(t, "paths", []string{"app/service.rb"}, nil)
	want := "Service.helper|" + ResolutionStrategyRubyExplicitSelf + "|high"
	if got := f.binding(t, edge); got != want {
		t.Fatalf("initial binding = %s, want %s", got, want)
	}
	// A redefinition arrives in another file: no load-order winner exists.
	f.method(t, other, "Service.helper", false)
	if _, err := f.store.ResolveEdgesForNames(f.ctx, f.repoID, []string{"helper"}); err != nil {
		t.Fatal(err)
	}
	if got := f.binding(t, edge); got != "<unresolved>" {
		t.Fatalf("stale self:: binding survived the name pass: %s", got)
	}
}
