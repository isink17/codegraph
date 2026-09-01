package store

import (
	"context"
	"testing"
)

func TestKnownTargetDoesNotPromoteUnresolvedCallerSpellings(t *testing.T) {
	f := newGateFixture(t)
	targetFile := f.file(t, "b/target.go", "go")
	callerFile := f.file(t, "a/caller.go", "go")
	target := f.symbol(t, targetFile, "Foo", "b.Foo", "go")
	caller := f.symbol(t, callerFile, "caller", "a.caller", "go")
	for _, spelling := range []string{"Foo", "b.Foo", "x.b.Foo", "App.Foo", "A::Foo"} {
		f.edge(t, callerFile, caller, spelling)
	}

	got, err := f.store.FindCallers(f.ctx, f.repoID, "b.Foo", target, 20, 0)
	if err != nil {
		t.Fatalf("FindCallers() error = %v", err)
	}
	assertQNames(t, "known target callers", got)
}

func TestKnownTargetKeepsResolvedCaller(t *testing.T) {
	f := newGateFixture(t)
	targetFile := f.file(t, "b/target.go", "go")
	callerFile := f.file(t, "a/caller.go", "go")
	target := f.symbol(t, targetFile, "Foo", "b.Foo", "go")
	caller := f.symbol(t, callerFile, "caller", "a.caller", "go")
	edge := f.edge(t, callerFile, caller, "Foo")
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET dst_symbol_id = ? WHERE id = ?`, target, edge); err != nil {
		t.Fatalf("bind edge: %v", err)
	}

	got, err := f.store.FindCallers(f.ctx, f.repoID, "b.Foo", target, 20, 0)
	if err != nil {
		t.Fatalf("FindCallers() error = %v", err)
	}
	assertQNames(t, "resolved callers", got, "a.caller")
}

func TestKnownSourceDoesNotPromoteUnresolvedCallee(t *testing.T) {
	f := newGateFixture(t)
	file := f.file(t, "a/caller.go", "go")
	source := f.symbol(t, file, "Caller", "a.Caller", "go")
	f.symbol(t, file, "Foo", "b.Foo", "go")
	f.edge(t, file, source, "Foo")

	got, err := f.store.FindCallees(f.ctx, f.repoID, "a.Caller", source, 20, 0)
	if err != nil {
		t.Fatalf("FindCallees() error = %v", err)
	}
	assertQNames(t, "unresolved callees", got)
}

func TestContextNeighborsUseBoundEdgesOnly(t *testing.T) {
	f := newAmbiguityFixture(t)
	f.unresolvedEdge(t, "caller.Qualified", "billing.Renew")
	f.unresolvedEdge(t, "caller.Short", "Renew")
	f.unresolvedEdge(t, "caller.Suffix", "foo.billing.Renew")
	f.unresolvedEdge(t, "caller.SettleShort", "Settle")
	f.unresolvedEdge(t, "caller.SettleSuffix", "foo.unique.Settle")
	f.unresolvedEdge(t, "unique.Settle", "Renew")

	got, err := f.store.FindContextNeighbors(context.Background(), f.repoID, []ContextSeed{f.seed("billing.Renew", true)}, 20)
	if err != nil {
		t.Fatalf("FindContextNeighbors() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("neighbors = %d, want 1", len(got))
	}
	if len(got[0].Callers) != 0 || len(got[0].Callees) != 0 {
		t.Fatalf("context neighbors = callers %v, callees %v; want both empty", callerNames(got[0]), calleeNames(got[0]))
	}
}
