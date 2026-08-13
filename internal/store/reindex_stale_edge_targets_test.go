package store_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/store"
)

// reindexFixture drives the real indexing path (ReplaceFileGraphsBatchWithStats
// -> deleteFileGraphsBatch, then the normal resolve pass) so referential
// integrity is exercised end to end rather than through hand-written SQL.
type reindexFixture struct {
	t        *testing.T
	ctx      context.Context
	repoRoot string
	store    *store.Store
	idx      *indexer.Indexer
	repoID   int64
}

func newReindexFixture(t *testing.T) *reindexFixture {
	t.Helper()
	ctx := context.Background()
	repoRoot := t.TempDir()

	dbPath := filepath.Join(t.TempDir(), "graph.sqlite")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	return &reindexFixture{
		t:        t,
		ctx:      ctx,
		repoRoot: repoRoot,
		store:    s,
		idx:      indexer.New(s, parser.NewRegistry(goparser.New()), nil),
	}
}

func (f *reindexFixture) write(rel, content string) {
	f.t.Helper()
	if err := os.WriteFile(filepath.Join(f.repoRoot, rel), []byte(content), 0o644); err != nil {
		f.t.Fatalf("WriteFile(%q) error = %v", rel, err)
	}
}

func (f *reindexFixture) remove(rel string) {
	f.t.Helper()
	if err := os.Remove(filepath.Join(f.repoRoot, rel)); err != nil {
		f.t.Fatalf("Remove(%q) error = %v", rel, err)
	}
}

func (f *reindexFixture) index() {
	f.t.Helper()
	if _, err := f.idx.Index(f.ctx, indexer.Options{RepoRoot: f.repoRoot}); err != nil {
		f.t.Fatalf("Index() error = %v", err)
	}
	repo, err := f.store.UpsertRepo(f.ctx, f.repoRoot)
	if err != nil {
		f.t.Fatalf("UpsertRepo() error = %v", err)
	}
	f.repoID = repo.ID
}

// update re-runs incremental indexing. The sleep keeps the mtime/hash change
// detectable on filesystems with coarse timestamps.
func (f *reindexFixture) update() store.ScanSummary {
	f.t.Helper()
	time.Sleep(5 * time.Millisecond)
	summary, err := f.idx.Update(f.ctx, indexer.Options{RepoRoot: f.repoRoot})
	if err != nil {
		f.t.Fatalf("Update() error = %v", err)
	}
	return summary
}

func (f *reindexFixture) edges() []store.ExportEdge {
	f.t.Helper()
	edges, err := f.store.ExportEdgesPage(f.ctx, f.repoID, 1000, 0)
	if err != nil {
		f.t.Fatalf("ExportEdgesPage() error = %v", err)
	}
	return edges
}

// edge returns the single edge declared in file rel with the given dst_name.
func (f *reindexFixture) edge(rel, dstName string) store.ExportEdge {
	f.t.Helper()
	var found []store.ExportEdge
	for _, e := range f.edges() {
		if e.FilePath == rel && e.DstName == dstName {
			found = append(found, e)
		}
	}
	if len(found) != 1 {
		f.t.Fatalf("edges in %q with dst_name %q = %d, want 1 (%+v)", rel, dstName, len(found), found)
	}
	return found[0]
}

func (f *reindexFixture) assertNoDangling() {
	f.t.Helper()
	n, err := f.store.CountDanglingEdgeTargets(f.ctx, f.repoID)
	if err != nil {
		f.t.Fatalf("CountDanglingEdgeTargets() error = %v", err)
	}
	if n != 0 {
		f.t.Fatalf("dangling edge targets = %d, want 0", n)
	}
}

func assertResolved(t *testing.T, e store.ExportEdge, wantQualifiedSuffix string) int64 {
	t.Helper()
	if e.DstSymbolID == nil {
		t.Fatalf("edge %d (%s -> %s) dst_symbol_id = NULL, want bound", e.ID, e.FilePath, e.DstName)
	}
	if wantQualifiedSuffix != "" && !strings.HasSuffix(e.DstQualifiedName, wantQualifiedSuffix) {
		t.Fatalf("edge %d dst_qualified_name = %q, want suffix %q", e.ID, e.DstQualifiedName, wantQualifiedSuffix)
	}
	if e.ResolutionStrategy == "" || e.ResolutionConfidence == "" {
		t.Fatalf("edge %d resolution metadata = (%q, %q), want both set",
			e.ID, e.ResolutionStrategy, e.ResolutionConfidence)
	}
	return *e.DstSymbolID
}

func assertUnbound(t *testing.T, e store.ExportEdge) {
	t.Helper()
	if e.DstSymbolID != nil {
		t.Fatalf("edge %d (%s -> %s) dst_symbol_id = %d, want NULL",
			e.ID, e.FilePath, e.DstName, *e.DstSymbolID)
	}
	if e.ResolutionStrategy != "" || e.ResolutionConfidence != "" {
		t.Fatalf("edge %d kept stale resolution metadata (%q, %q), want both cleared",
			e.ID, e.ResolutionStrategy, e.ResolutionConfidence)
	}
}

// TestReindexUnbindsEdgeWhenTargetSymbolDisappears is the core regression: file
// A drops Foo while staying indexed, so B's inbound edge must lose its
// destination (and its P4 provenance) instead of pointing at a deleted row.
// It also covers "one removed + one preserved symbol" via Keep.
func TestReindexUnbindsEdgeWhenTargetSymbolDisappears(t *testing.T) {
	f := newReindexFixture(t)
	f.write("a.go", "package main\n\nfunc Foo() {}\n\nfunc Keep() {}\n")
	f.write("b.go", "package main\n\nfunc CallFoo() { Foo() }\n\nfunc CallKeep() { Keep() }\n")
	f.index()

	fooEdge := f.edge("b.go", "Foo")
	assertResolved(t, fooEdge, "Foo")
	keepID := assertResolved(t, f.edge("b.go", "Keep"), "Keep")
	f.assertNoDangling()

	// Re-index a.go without Foo. The file itself is not deleted.
	f.write("a.go", "package main\n\nfunc Keep() {}\n")
	f.update()

	// Checked before the per-edge assertions so the integrity probe itself is
	// the first thing that fires when the invariant breaks.
	f.assertNoDangling()

	after := f.edge("b.go", "Foo")
	if after.ID != fooEdge.ID {
		t.Fatalf("edge id changed %d -> %d: the calling edge must survive", fooEdge.ID, after.ID)
	}
	if after.DstName != "Foo" || after.Kind != fooEdge.Kind {
		t.Fatalf("edge payload changed: %+v, want dst_name/kind preserved from %+v", after, fooEdge)
	}
	assertUnbound(t, after)

	// The preserved symbol's edge must end up bound again (its row was replaced too).
	keepAfter := f.edge("b.go", "Keep")
	newKeepID := assertResolved(t, keepAfter, "Keep")
	// Symbol ids are AUTOINCREMENT, so a replaced row always gets a new id.
	if newKeepID == keepID {
		t.Fatalf("Keep symbol id = %d unchanged; the row was expected to be replaced", keepID)
	}
}

// TestReindexRebindsEdgeWhenTargetSymbolIsRecreated covers the replacement case:
// the destination still exists by name after re-index but lives in a new symbol
// row, so the edge must end up bound to the new id with fresh provenance.
func TestReindexRebindsEdgeWhenTargetSymbolIsRecreated(t *testing.T) {
	f := newReindexFixture(t)
	f.write("a.go", "package main\n\nfunc Foo() {}\n")
	f.write("b.go", "package main\n\nfunc CallFoo() { Foo() }\n")
	f.index()

	oldID := assertResolved(t, f.edge("b.go", "Foo"), "Foo")

	// Re-index a.go: Foo survives by name, but its symbol row is replaced.
	f.write("a.go", "package main\n\n// Foo does nothing.\nfunc Foo() {}\n")
	f.update()

	after := f.edge("b.go", "Foo")
	newID := assertResolved(t, after, "Foo")
	if newID == oldID {
		t.Fatalf("dst_symbol_id = %d unchanged; symbol row was expected to be replaced", newID)
	}
	if after.DstQualifiedName == "" {
		t.Fatalf("edge %d resolved to symbol %d but has no qualified name", after.ID, newID)
	}
	f.assertNoDangling()
}

// TestReindexUnbindsAllCallersOfRemovedSymbol covers multiple callers pointing
// at one removed symbol.
func TestReindexUnbindsAllCallersOfRemovedSymbol(t *testing.T) {
	f := newReindexFixture(t)
	f.write("a.go", "package main\n\nfunc Foo() {}\n")
	f.write("b.go", "package main\n\nfunc CallFooB() { Foo() }\n")
	f.write("c.go", "package main\n\nfunc CallFooC() { Foo() }\n")
	f.index()

	assertResolved(t, f.edge("b.go", "Foo"), "Foo")
	assertResolved(t, f.edge("c.go", "Foo"), "Foo")

	f.write("a.go", "package main\n\nfunc Other() {}\n")
	f.update()

	assertUnbound(t, f.edge("b.go", "Foo"))
	assertUnbound(t, f.edge("c.go", "Foo"))
	f.assertNoDangling()
}

// TestReindexUnbindsMultipleRemovedSymbolsInOneBatch re-indexes two definition
// files in a single replacement batch, each dropping a symbol.
func TestReindexUnbindsMultipleRemovedSymbolsInOneBatch(t *testing.T) {
	f := newReindexFixture(t)
	f.write("a.go", "package main\n\nfunc Foo() {}\n")
	f.write("d.go", "package main\n\nfunc Baz() {}\n")
	f.write("b.go", "package main\n\nfunc CallBoth() { Foo(); Baz() }\n")
	f.index()

	assertResolved(t, f.edge("b.go", "Foo"), "Foo")
	assertResolved(t, f.edge("b.go", "Baz"), "Baz")

	f.write("a.go", "package main\n\nfunc FooGone() {}\n")
	f.write("d.go", "package main\n\nfunc BazGone() {}\n")
	f.update()

	assertUnbound(t, f.edge("b.go", "Foo"))
	assertUnbound(t, f.edge("b.go", "Baz"))
	f.assertNoDangling()
}

// TestReindexSameFileReferenceSurvivesReplacement covers same-file references.
// Edges declared in the replaced file are deleted and re-inserted unresolved by
// the same transaction, so they cannot carry a stale destination; this asserts
// that property instead of assuming it.
func TestReindexSameFileReferenceSurvivesReplacement(t *testing.T) {
	f := newReindexFixture(t)
	f.write("a.go", "package main\n\nfunc Foo() {}\n\nfunc Bar() { Foo() }\n")
	f.index()

	assertResolved(t, f.edge("a.go", "Foo"), "Foo")

	// Drop Foo; Bar still calls it.
	f.write("a.go", "package main\n\nfunc Bar() { Foo() }\n")
	f.update()

	assertUnbound(t, f.edge("a.go", "Foo"))
	f.assertNoDangling()

	// Reintroduce Foo: the same-file edge binds to the new symbol row.
	f.write("a.go", "package main\n\nfunc Foo() {}\n\nfunc Bar() { Foo() }\n")
	f.update()

	assertResolved(t, f.edge("a.go", "Foo"), "Foo")
	f.assertNoDangling()
}

// TestReindexUnbindOnlyTouchesOwnRepo guards the repo_id filter on the unbind
// UPDATE: re-indexing one repo must not disturb another repo in the same DB.
func TestReindexUnbindOnlyTouchesOwnRepo(t *testing.T) {
	a := newReindexFixture(t)
	a.write("a.go", "package main\n\nfunc Foo() {}\n")
	a.write("b.go", "package main\n\nfunc CallFoo() { Foo() }\n")
	a.index()

	// Second repo, same store/DB.
	b := &reindexFixture{t: t, ctx: a.ctx, repoRoot: t.TempDir(), store: a.store, idx: a.idx}
	b.write("a.go", "package main\n\nfunc Foo() {}\n")
	b.write("b.go", "package main\n\nfunc CallFoo() { Foo() }\n")
	b.index()
	if b.repoID == a.repoID {
		t.Fatalf("both fixtures resolved to repo %d, want distinct repos", a.repoID)
	}
	otherID := assertResolved(t, b.edge("b.go", "Foo"), "Foo")

	a.write("a.go", "package main\n\nfunc Other() {}\n")
	a.update()

	assertUnbound(t, a.edge("b.go", "Foo"))
	a.assertNoDangling()

	stillBound := assertResolved(t, b.edge("b.go", "Foo"), "Foo")
	if stillBound != otherID {
		t.Fatalf("other repo edge rebound %d -> %d, want untouched", otherID, stillBound)
	}
	b.assertNoDangling()
}

// TestDeletedFilePurgeStillUnbindsInboundEdges guards the deleted-file path,
// whose inbound-edge cleanup moved into deleteFileGraphsBatch.
func TestDeletedFilePurgeStillUnbindsInboundEdges(t *testing.T) {
	f := newReindexFixture(t)
	f.write("a.go", "package main\n\nfunc Foo() {}\n")
	f.write("b.go", "package main\n\nfunc CallFoo() { Foo() }\n")
	f.index()

	assertResolved(t, f.edge("b.go", "Foo"), "Foo")

	f.remove("a.go")
	summary := f.update()
	if summary.FilesDeleted != 1 {
		t.Fatalf("FilesDeleted = %d, want 1", summary.FilesDeleted)
	}

	assertUnbound(t, f.edge("b.go", "Foo"))
	f.assertNoDangling()
}
