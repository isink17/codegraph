package store

import "testing"

// Upgrade coverage for P22.12 (resolver_ambiguity.go).
//
// Both defects this phase closes leave a lasting mark on a database written by
// an older release, and neither heals on its own: no strategy reconsiders an
// already-bound edge, and a run over an unchanged tree contributes no names, so
// the incremental passes never look at the rows. RepairResolverBindingsOnce is
// what makes an upgraded database converge on the projection a fresh index of
// the same tree produces.

// setBinding forces an edge into the state an older binary would have left, so
// the repair has something real to fix.
func (f *parityFixture) setBinding(t *testing.T, edgeID, dstID int64, strategy, confidence string) {
	t.Helper()
	if _, err := f.store.db.ExecContext(f.ctx, `
		UPDATE edges SET dst_symbol_id = ?, resolution_strategy = ?, resolution_confidence = ?
		WHERE id = ?`, dstID, strategy, confidence, edgeID); err != nil {
		t.Fatalf("force binding: %v", err)
	}
}

// A database carrying the pre-P22.12 binder's answer -- a bare name bound to the
// callable while a container-bearing enum of the same name competes -- must be
// re-decided on the first scan after the upgrade, with no source change.
func TestUpgradeRepairClearsBindingTheBareNameLevelRefuses(t *testing.T) {
	f := newParityFixture(t, "")
	fnFile := f.file(t, "app/helpers.py", "python")
	fnID := f.symbol(t, fnFile, "helper", "helpers.helper", "function", "python")
	enumFile := f.file(t, "app/models.py", "python")
	f.symbolIn(t, enumFile, "helper", "Base.helper", "enum", "Base", "python")

	callerFile := f.file(t, "app/main.py", "python")
	caller := f.symbol(t, callerFile, "run", "main.run", "function", "python")
	edge := f.edge(t, callerFile, caller, "helper")
	f.setBinding(t, edge, fnID, ResolutionStrategyExactName, ResolutionConfidenceHigh)

	// An ordinary incremental pass over unrelated names does not reach it: this
	// is why a repair is needed rather than "the next update fixes it".
	f.resolveVia(t, "names", nil, []string{"unrelated"})
	if got := f.binding(t, edge); got == "<unresolved>" {
		t.Fatalf("precondition: the stale binding should survive an unrelated name pass, got %s", got)
	}

	if _, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID); err != nil {
		t.Fatalf("RepairResolverBindingsOnce: %v", err)
	}
	if got := f.binding(t, edge); got != "<unresolved>" {
		t.Fatalf("after repair: want unresolved, got %s", got)
	}

	// Idempotent: the marker means a second scan does no work and changes
	// nothing.
	if _, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID); err != nil {
		t.Fatalf("second RepairResolverBindingsOnce: %v", err)
	}
	if got := f.binding(t, edge); got != "<unresolved>" {
		t.Fatalf("after second repair: want unresolved, got %s", got)
	}
}

// The mirror case: a database left under-resolved because a competing
// declaration was removed by an older binary, on a tree that has not changed
// since. The repair must resolve it, and must give it the same strategy and
// confidence a fresh index would.
func TestUpgradeRepairResolvesEdgeStrandedByARemovedDeclaration(t *testing.T) {
	f := newParityFixture(t, "")
	fnFile := f.file(t, "app/helpers.py", "python")
	f.symbol(t, fnFile, "helper", "helpers.helper", "function", "python")
	callerFile := f.file(t, "app/main.py", "python")
	caller := f.symbol(t, callerFile, "run", "main.run", "function", "python")
	edge := f.edge(t, callerFile, caller, "helper")

	// The stranded state: one declaration left, edge still unresolved, and
	// nothing in the tree changing to wake it.
	f.resolveVia(t, "names", nil, []string{"unrelated"})
	if got := f.binding(t, edge); got != "<unresolved>" {
		t.Fatalf("precondition: want the edge still unresolved, got %s", got)
	}

	if _, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID); err != nil {
		t.Fatalf("RepairResolverBindingsOnce: %v", err)
	}
	if got, want := f.binding(t, edge), "helpers.helper|exact_name|high"; got != want {
		t.Fatalf("after repair: got %q, want %q", got, want)
	}
}

// The repair must not become a licence to rebind everything: a binding the
// current rules still allow keeps its exact destination, strategy and
// confidence across the clear-and-re-resolve.
func TestUpgradeRepairPreservesValidBindings(t *testing.T) {
	f := newParityFixture(t, "")
	fnFile := f.file(t, "app/helpers.py", "python")
	f.symbol(t, fnFile, "helper", "helpers.helper", "function", "python")
	callerFile := f.file(t, "app/main.py", "python")
	caller := f.symbol(t, callerFile, "run", "main.run", "function", "python")
	edge := f.edge(t, callerFile, caller, "helper")

	f.resolveVia(t, "full", nil, nil)
	before := f.binding(t, edge)
	if before == "<unresolved>" {
		t.Fatalf("precondition: want a binding, got %s", before)
	}
	if _, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID); err != nil {
		t.Fatalf("RepairResolverBindingsOnce: %v", err)
	}
	if got := f.binding(t, edge); got != before {
		t.Fatalf("repair changed a valid binding: %q -> %q", before, got)
	}
}

// Bindings the repair must never touch: an explicit cross-language link, and a
// destination reached through evidence the bare-name level does not own.
func TestUpgradeRepairLeavesOtherStrategiesAlone(t *testing.T) {
	f := newParityFixture(t, "module example.com/project\n")
	pkgFile := f.file(t, "pkg/open.go", "go")
	f.symbolIn(t, pkgFile, "Open", "pkg.Open", "function", "pkg", "go")
	callerFile := f.file(t, "cmd/main.go", "go")
	caller := f.symbol(t, callerFile, "main", "main", "function", "go")
	f.imports(t, callerFile, "example.com/project/pkg")
	edge := f.edge(t, callerFile, caller, "example.com/project/pkg.Open")

	f.resolveVia(t, "full", nil, nil)
	if got, want := f.binding(t, edge), "pkg.Open|module_import|high"; got != want {
		t.Fatalf("precondition: got %q, want %q", got, want)
	}
	if _, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID); err != nil {
		t.Fatalf("RepairResolverBindingsOnce: %v", err)
	}
	if got, want := f.binding(t, edge), "pkg.Open|module_import|high"; got != want {
		t.Fatalf("after repair: got %q, want %q", got, want)
	}
}

// The gate that keeps a first index from paying for a repair it cannot need.
// It is load-bearing for performance and invisible in the graph, so it needs a
// test of its own: without it every fresh index runs a second repo-wide resolve.
func TestRepoWithNoGraphIsMarkedRepairedWithoutWork(t *testing.T) {
	f := newParityFixture(t, "")
	has, err := f.store.RepoHasExistingGraph(f.ctx, f.repoID)
	if err != nil {
		t.Fatalf("RepoHasExistingGraph: %v", err)
	}
	if has {
		t.Fatalf("a repository with no edges reported an existing graph")
	}
	if err := f.store.MarkResolverBindingsRepaired(f.ctx, f.repoID); err != nil {
		t.Fatalf("MarkResolverBindingsRepaired: %v", err)
	}
	// Every repair must now consider itself done, so no later scan re-resolves.
	for _, repair := range resolverRepairs {
		ran, err := f.store.runResolverRepairOnce(f.ctx, f.repoID, repair)
		if err != nil {
			t.Fatalf("runResolverRepairOnce(%s): %v", repair.key, err)
		}
		if ran {
			t.Fatalf("repair %s ran despite the marker written by MarkResolverBindingsRepaired", repair.key)
		}
	}
	if _, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID); err != nil {
		t.Fatalf("RepairResolverBindingsOnce: %v", err)
	}
}

// The other half: a repository that already holds relationships is recognised
// as one, and its first repair reports that it re-resolved repo-wide so the
// caller can skip its own pass.
func TestRepoWithGraphRepairsOnceAndReportsRepoWideResolve(t *testing.T) {
	f := newParityFixture(t, "")
	fnFile := f.file(t, "app/helpers.py", "python")
	f.symbol(t, fnFile, "helper", "helpers.helper", "function", "python")
	callerFile := f.file(t, "app/main.py", "python")
	caller := f.symbol(t, callerFile, "run", "main.run", "function", "python")
	f.edge(t, callerFile, caller, "helper")

	has, err := f.store.RepoHasExistingGraph(f.ctx, f.repoID)
	if err != nil {
		t.Fatalf("RepoHasExistingGraph: %v", err)
	}
	if !has {
		t.Fatalf("a repository holding edges reported no existing graph")
	}
	resolvedRepoWide, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID)
	if err != nil {
		t.Fatalf("RepairResolverBindingsOnce: %v", err)
	}
	if !resolvedRepoWide {
		t.Fatalf("the first repair re-resolves the whole repository and must say so")
	}
	resolvedRepoWide, err = f.store.RepairResolverBindingsOnce(f.ctx, f.repoID)
	if err != nil {
		t.Fatalf("second RepairResolverBindingsOnce: %v", err)
	}
	if resolvedRepoWide {
		t.Fatalf("a repository already repaired must not claim a repo-wide resolve")
	}
}

// Every repair must appear in the one list both the runner and the marker
// writer walk. A repair enumerated in only one of them would silently make
// every freshly indexed repository pay a repo-wide resolve on its second scan.
func TestResolverRepairsShareOneList(t *testing.T) {
	seen := map[string]struct{}{}
	for _, repair := range resolverRepairs {
		if repair.key == "" || repair.run == nil {
			t.Fatalf("repair %+v is incomplete", repair)
		}
		if _, dup := seen[repair.key]; dup {
			t.Fatalf("repair key %q listed twice", repair.key)
		}
		seen[repair.key] = struct{}{}
	}
	for _, key := range []string{typeScopeRepairSettingKey, bareNameLevelRepairSettingKey} {
		if _, ok := seen[key]; !ok {
			t.Fatalf("repair key %q is not in resolverRepairs", key)
		}
	}
}
