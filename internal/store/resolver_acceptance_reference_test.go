package store

import (
	"maps"
	"slices"
	"testing"
)

// A rule fact that changes from binding to refusing, back, or to another
// binding must move the call-site reference with the edge: a refusal leaves the
// reference with no symbol, never the target of the previous binding, and a
// rebind gives it the new target. The reference keeps the caller as its context either way.
//
// The full resolve and the paths and paths+names entrypoints reconcile
// reference identities from the edges they leave behind. The names-only
// entrypoint does not: it re-decides edges and leaves references as they were,
// so a caller of it alone must run ReconcileReferenceIdentities afterwards. The
// indexer never calls it alone, so this pins that contract instead of the
// reconciled state, and checks that the explicit reconcile then converges.
func TestResolverGateRuleRefusalClearsReferenceIdentity(t *testing.T) {
	cases := slices.Concat(lifecycleCases, moreLifecycleCases)
	for _, lc := range cases {
		states := slices.Sorted(maps.Keys(lc.want))
		for _, from := range states {
			for _, to := range states {
				if from == to {
					continue
				}
				entries := []string{"full", "paths", "names", "paths+names"}
				if lc.declChange {
					entries = []string{"full", "names", "paths+names"}
				}
				for _, entry := range entries {
					t.Run(lc.rule+"/"+from+"->"+to+"/"+entry, func(t *testing.T) {
						f := newParityFixture(t, "module example.com/project\n")
						edge, set := lc.seed(t, f)
						// Production edges are 'calls'; the fixture helper writes 'call'.
						f.exec(t, `UPDATE edges SET edge_kind = 'calls' WHERE id = ?`, edge)
						ref := f.addCallReference(t, edge)
						set(from)
						f.resolveVia(t, "full", nil, nil)
						_, _, srcID, before := f.edgeSides(t, edge)
						if sym, ctx := f.referenceIdentity(t, ref); sym != before || ctx != srcID {
							t.Fatalf("%s: reference = (symbol %d, context %d), want (%d, %d)", from, sym, ctx, before, srcID)
						}
						set(to)
						if !lc.declChange {
							f.clearAll(t)
						}
						paths := lc.paths
						if lc.declChange {
							paths = lc.declPaths
						}
						f.resolveVia(t, entry, paths, lc.names)
						if got := f.binding(t, edge); got != lc.want[to] {
							t.Fatalf("bound %q, fresh %q", got, lc.want[to])
						}
						_, _, _, after := f.edgeSides(t, edge)
						sym, ctx := f.referenceIdentity(t, ref)
						if entry == "names" {
							if sym != before || ctx != srcID {
								t.Errorf("names-only changed the reference to (symbol %d, context %d); it does not reconcile references", sym, ctx)
							}
							if err := f.store.ReconcileReferenceIdentities(f.ctx, f.repoID); err != nil {
								t.Fatal(err)
							}
							sym, ctx = f.referenceIdentity(t, ref)
						}
						if sym != after || ctx != srcID {
							t.Errorf("reference = (symbol %d, context %d), edge = (dst %d, src %d)", sym, ctx, after, srcID)
						}
					})
				}
			}
		}
	}
}

// Reconciliation derives a reference's context from its matched edges alone: a
// stale context is cleared when no edge coherently matches (the edge is gone,
// or matching edges disagree on their source), and a refused edge keeps its
// caller as the context.
func TestReferenceReconcileClearsContextWithoutCoherentSource(t *testing.T) {
	setup := func(t *testing.T) (f *parityFixture, edge, ref, caller int64) {
		f = newParityFixture(t, "module example.com/project\n")
		lib := f.file(t, "app/util.py", "python")
		f.symbol(t, lib, "helper", "util.helper", "function", "python")
		main := f.file(t, "app/main.py", "python")
		caller = f.symbol(t, main, "run", "main.run", "function", "python")
		edge = f.edge(t, main, caller, "helper")
		f.exec(t, `UPDATE edges SET edge_kind = 'calls' WHERE id = ?`, edge)
		ref = f.addCallReference(t, edge)
		f.resolveVia(t, "full", nil, nil)
		if _, ctx := f.referenceIdentity(t, ref); ctx != caller {
			t.Fatalf("setup context = %d, want %d", ctx, caller)
		}
		return f, edge, ref, caller
	}
	reconcile := func(t *testing.T, f *parityFixture) {
		t.Helper()
		if err := f.store.ReconcileReferenceIdentities(f.ctx, f.repoID); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("matched edge removed", func(t *testing.T) {
		f, edge, ref, _ := setup(t)
		f.exec(t, `DELETE FROM edges WHERE id = ?`, edge)
		reconcile(t, f)
		if sym, ctx := f.referenceIdentity(t, ref); sym != 0 || ctx != 0 {
			t.Errorf("reference = (symbol %d, context %d), want both cleared", sym, ctx)
		}
	})
	t.Run("matched edges disagree on source", func(t *testing.T) {
		f, edge, ref, caller := setup(t)
		var file int64
		if err := f.store.db.QueryRowContext(f.ctx, `SELECT file_id FROM edges WHERE id = ?`, edge).Scan(&file); err != nil {
			t.Fatal(err)
		}
		other := f.symbol(t, file, "other", "main.other", "function", "python")
		twin := f.edge(t, file, other, "helper")
		f.exec(t, `UPDATE edges SET edge_kind = 'calls', line = (SELECT line FROM edges WHERE id = ?) WHERE id = ?`, edge, twin)
		reconcile(t, f)
		if _, ctx := f.referenceIdentity(t, ref); ctx != 0 {
			t.Errorf("context = %d with sources %d and %d, want cleared", ctx, caller, other)
		}
	})
	t.Run("non-call reference", func(t *testing.T) {
		f, _, ref, caller := setup(t)
		f.exec(t, `UPDATE references_tbl SET ref_kind = 'type' WHERE id = ?`, ref)
		f.exec(t, `UPDATE references_tbl SET context_symbol_id = ? WHERE id = ?`, caller, ref)
		reconcile(t, f)
		if sym, ctx := f.referenceIdentity(t, ref); sym != 0 || ctx != 0 {
			t.Errorf("reference = (symbol %d, context %d), want both cleared", sym, ctx)
		}
	})
	t.Run("refused edge keeps its caller", func(t *testing.T) {
		f, edge, ref, caller := setup(t)
		f.clearAll(t)
		reconcile(t, f)
		if sym, ctx := f.referenceIdentity(t, ref); sym != 0 || ctx != caller {
			t.Errorf("reference = (symbol %d, context %d), want (0, %d)", sym, ctx, caller)
		}
		_ = edge
	})
}
