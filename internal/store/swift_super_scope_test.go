package store

import (
	"context"
	"database/sql"
	"strconv"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

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
