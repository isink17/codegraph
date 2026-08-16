package store_test

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"

	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/store"
)

// P10 determinism: the indexed graph must not depend on the order in which
// files are persisted.
//
// Before P10, `test_links` target binding happened inside the Pass-1 write
// transaction and read whatever `symbols` already contained, so a test file
// persisted before its target bound nothing -- permanently, because the target
// key was not stored. The indexer's file workers finish in a nondeterministic
// order, which is how that surfaced as a flaky
// TestIndexer_TestLinksPopulateTargetFileID rather than a hard failure.
//
// These tests do not rely on winning that race. They construct the bad order
// explicitly: persistInOrder writes the given files through the real store API,
// in the exact sequence asked for, and only then runs Pass 2. The
// "test/caller first" permutations fail against the pre-P10 implementation and
// pass against this one, on every platform, without stress.

type orderFile struct {
	path string
	src  string
}

// persistInOrder runs a full Pass 1 (persist the given files, in the given
// order) followed by a full Pass 2 (repo-wide resolution), then returns the
// id-free semantic graph.
//
// Each file gets its own ReplaceFileGraphsBatch call, which is the harshest
// version of the ordering question: separate write transactions, so a file
// persisted later is genuinely invisible while an earlier one is written.
func persistInOrder(t *testing.T, files []orderFile, order []int) []string {
	t.Helper()
	ctx := context.Background()

	s, err := store.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	repo, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	scanID, _, err := s.BeginScan(ctx, repo.ID, "index")
	if err != nil {
		t.Fatalf("BeginScan() error = %v", err)
	}

	adapter := goparser.New()
	for _, idx := range order {
		f := files[idx]
		parsed, err := adapter.Parse(ctx, f.path, []byte(f.src))
		if err != nil {
			t.Fatalf("Parse(%q) error = %v", f.path, err)
		}
		if _, err := s.ReplaceFileGraphsBatch(ctx, repo.ID, scanID, []store.ReplaceFileGraphInput{{
			Path:        f.path,
			Language:    parsed.Language,
			SizeBytes:   int64(len(f.src)),
			MtimeUnixNS: 1,
			ContentHash: f.path,
			Parsed:      parsed,
		}}); err != nil {
			t.Fatalf("ReplaceFileGraphsBatch(%q) error = %v", f.path, err)
		}
	}

	if _, err := s.ResolveEdges(ctx, repo.ID); err != nil {
		t.Fatalf("ResolveEdges() error = %v", err)
	}
	if _, err := s.ResolveTestLinks(ctx, repo.ID); err != nil {
		t.Fatalf("ResolveTestLinks() error = %v", err)
	}

	snapshot, err := s.SemanticGraphForTest(ctx, repo.ID)
	if err != nil {
		t.Fatalf("SemanticGraphForTest() error = %v", err)
	}
	return snapshot
}

// permutations returns every ordering of [0,n).
func permutations(n int) [][]int {
	if n == 0 {
		return [][]int{{}}
	}
	var out [][]int
	for i := range n {
		for _, rest := range permutations(n - 1) {
			perm := make([]int, 0, n)
			perm = append(perm, i)
			for _, v := range rest {
				if v >= i {
					v++
				}
				perm = append(perm, v)
			}
			out = append(out, perm)
		}
	}
	return out
}

// assertOrderIndependent persists files in every order and requires an
// identical semantic graph each time. It returns the shared snapshot.
func assertOrderIndependent(t *testing.T, files []orderFile) []string {
	t.Helper()
	var want []string
	var wantOrder []int
	for _, order := range permutations(len(files)) {
		got := persistInOrder(t, files, order)
		if want == nil {
			want, wantOrder = got, order
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("graph differs by persist order\norder %v:\n  %v\norder %v:\n  %v",
				wantOrder, want, order, got)
		}
	}
	return want
}

func requireLine(t *testing.T, snapshot []string, want string) {
	t.Helper()
	for _, line := range snapshot {
		if line == want {
			return
		}
	}
	t.Fatalf("snapshot missing %q:\n  %v", want, snapshot)
}

const orderHelperSrc = `package pkg

func Helper() {}
`

const orderHelperTestSrc = `package pkg

import "testing"

func TestHelper(t *testing.T) { Helper() }
`

// TestTwoPass_TestLinkBindsInEitherFileOrder is the direct regression for
// TestIndexer_TestLinksPopulateTargetFileID: cases A and B of the determinism
// matrix. The historically bad order (test file persisted first) must produce
// the same bound target as the historically lucky one.
func TestTwoPass_TestLinkBindsInEitherFileOrder(t *testing.T) {
	files := []orderFile{
		{path: "helper.go", src: orderHelperSrc},
		{path: "helper_test.go", src: orderHelperTestSrc},
	}

	targetFirst := persistInOrder(t, files, []int{0, 1})
	testFirst := persistInOrder(t, files, []int{1, 0})

	if !reflect.DeepEqual(targetFirst, testFirst) {
		t.Fatalf("graph depends on persist order\ntarget-first:\n  %v\ntest-first:\n  %v",
			targetFirst, testFirst)
	}
	// Spell the binding out, so a future change that makes *both* orders
	// equally unresolved cannot pass this test.
	requireLine(t, testFirst,
		`testlink helper_test.go:pkg.TestHelper -> "func:pkg::Helper" => helper.go:pkg.Helper [test_name_match/0.80]`)
}

// TestTwoPass_EdgeBindsInEitherFileOrder is cases C and D: caller before callee
// and callee before caller.
func TestTwoPass_EdgeBindsInEitherFileOrder(t *testing.T) {
	files := []orderFile{
		{path: "callee.go", src: "package pkg\n\nfunc Callee() {}\n"},
		{path: "caller.go", src: "package pkg\n\nfunc Caller() { Callee() }\n"},
	}

	snapshot := assertOrderIndependent(t, files)
	requireLine(t, snapshot,
		`edge caller.go:pkg.Caller -calls-> "Callee" => callee.go:pkg.Callee [go_package_scope/high]`)
}

// TestTwoPass_TestLinkAndEdgeAcrossEveryOrder walks all six orders of a
// three-file repo, so no single pairwise ordering can be special-cased.
func TestTwoPass_TestLinkAndEdgeAcrossEveryOrder(t *testing.T) {
	files := []orderFile{
		{path: "helper.go", src: orderHelperSrc},
		{path: "helper_test.go", src: orderHelperTestSrc},
		{path: "caller.go", src: "package pkg\n\nfunc Caller() { Helper() }\n"},
	}

	snapshot := assertOrderIndependent(t, files)
	requireLine(t, snapshot,
		`testlink helper_test.go:pkg.TestHelper -> "func:pkg::Helper" => helper.go:pkg.Helper [test_name_match/0.80]`)
	requireLine(t, snapshot,
		`edge caller.go:pkg.Caller -calls-> "Helper" => helper.go:pkg.Helper [go_package_scope/high]`)
}

// TestTwoPass_AmbiguousTargetUnresolvedInEveryOrder is case E: two directories
// declare the same Go package and the same function, so neither definition is
// symbol evidence. The point is not only that the symbol stays unresolved but
// that it stays unresolved *identically* in every order -- an order-sensitive
// resolver would have bound whichever definition happened to be persisted
// first. P22.2: the test file's own sibling still supplies the file-level
// relation, which must be equally order-independent.
func TestTwoPass_AmbiguousTargetUnresolvedInEveryOrder(t *testing.T) {
	files := []orderFile{
		{path: "a/helper.go", src: orderHelperSrc},
		{path: "b/helper.go", src: orderHelperSrc},
		{path: "a/helper_test.go", src: orderHelperTestSrc},
	}

	snapshot := assertOrderIndependent(t, files)
	requireLine(t, snapshot,
		`testlink a/helper_test.go:pkg.TestHelper -> "func:pkg::Helper" => a/helper.go: [test_file_name_match/0.80]`)
}

// TestTwoPass_ProductionTargetPreferredOverTestShadowInEveryOrder is case F:
// P7's production-over-test-shadow preference for a production caller must be
// reached by evidence, not by which file was written first.
func TestTwoPass_ProductionTargetPreferredOverTestShadowInEveryOrder(t *testing.T) {
	files := []orderFile{
		{path: "helper.go", src: "package pkg\n\nfunc Shared() {}\n"},
		{path: "shadow_test.go", src: "package pkg\n\nfunc Shared() {}\n"},
		{path: "caller.go", src: "package pkg\n\nfunc Caller() { Shared() }\n"},
	}

	snapshot := assertOrderIndependent(t, files)
	requireLine(t, snapshot,
		`edge caller.go:pkg.Caller -calls-> "Shared" => helper.go:pkg.Shared [go_package_scope/high]`)
}

// TestTwoPass_UnresolvedExternalTargetInEveryOrder is case H: a call to a
// symbol the repo does not define stays unresolved, with the same provenance,
// in every order.
func TestTwoPass_UnresolvedExternalTargetInEveryOrder(t *testing.T) {
	files := []orderFile{
		{path: "helper.go", src: orderHelperSrc},
		{path: "caller.go", src: "package pkg\n\nfunc Caller() { NotDefinedAnywhere() }\n"},
	}

	snapshot := assertOrderIndependent(t, files)
	requireLine(t, snapshot,
		`edge caller.go:pkg.Caller -calls-> "NotDefinedAnywhere" => : [/]`)
}

// TestTwoPass_Pass1LeavesTargetsUnbound pins the stage boundary itself rather
// than its effect: after Pass 1 alone, no cross-file relationship is bound. If
// a future change resolves anything during persistence, that is where order
// dependence comes back, and this fails before the order tests above do.
func TestTwoPass_Pass1LeavesTargetsUnbound(t *testing.T) {
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	defer s.Close()

	repo, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	scanID, _, err := s.BeginScan(ctx, repo.ID, "index")
	if err != nil {
		t.Fatalf("BeginScan() error = %v", err)
	}

	adapter := goparser.New()
	inputs := make([]store.ReplaceFileGraphInput, 0, 2)
	// Target first: the order that *would* have bound under the old resolver.
	for _, f := range []orderFile{
		{path: "helper.go", src: orderHelperSrc},
		{path: "helper_test.go", src: orderHelperTestSrc},
	} {
		parsed, err := adapter.Parse(ctx, f.path, []byte(f.src))
		if err != nil {
			t.Fatalf("Parse(%q) error = %v", f.path, err)
		}
		inputs = append(inputs, store.ReplaceFileGraphInput{
			Path: f.path, Language: parsed.Language, SizeBytes: int64(len(f.src)),
			MtimeUnixNS: 1, ContentHash: f.path, Parsed: parsed,
		})
	}
	if _, err := s.ReplaceFileGraphsBatch(ctx, repo.ID, scanID, inputs); err != nil {
		t.Fatalf("ReplaceFileGraphsBatch() error = %v", err)
	}

	snapshot, err := s.SemanticGraphForTest(ctx, repo.ID)
	if err != nil {
		t.Fatalf("SemanticGraphForTest() error = %v", err)
	}
	requireLine(t, snapshot,
		`testlink helper_test.go:pkg.TestHelper -> "func:pkg::Helper" => : [test_name_match/0.80]`)
	requireLine(t, snapshot,
		`edge helper_test.go:pkg.TestHelper -calls-> "Helper" => : [/]`)

	// And Pass 2 alone is enough to bind both.
	if _, err := s.ResolveEdges(ctx, repo.ID); err != nil {
		t.Fatalf("ResolveEdges() error = %v", err)
	}
	if _, err := s.ResolveTestLinks(ctx, repo.ID); err != nil {
		t.Fatalf("ResolveTestLinks() error = %v", err)
	}
	resolved, err := s.SemanticGraphForTest(ctx, repo.ID)
	if err != nil {
		t.Fatalf("SemanticGraphForTest() error = %v", err)
	}
	requireLine(t, resolved,
		`testlink helper_test.go:pkg.TestHelper -> "func:pkg::Helper" => helper.go:pkg.Helper [test_name_match/0.80]`)
}

// ---------------------------------------------------------------------------
// Incremental Pass-2 semantics (matrix cases I and J), through the real indexer.
// ---------------------------------------------------------------------------

// TestTwoPass_IncrementalTargetInLaterChangedFileResolves is case I: within one
// incremental batch, the target arrives in a file the batch also changed. The
// link must bind against the completed batch, not against whatever existed when
// the test file's own write transaction ran.
func TestTwoPass_IncrementalTargetInLaterChangedFileResolves(t *testing.T) {
	f := newReindexFixture(t)
	// First index: the test file exists, its target does not. The link is a real
	// unresolved fact, exactly as Pass 1 records it.
	f.write("helper_test.go", helperTestSrc)
	f.index()
	assertTargetUnbound(t, f.testLink("helper_test.go", ""))

	// One incremental batch introduces the target in a *different* file.
	f.write("helper.go", "package pkg\n\nfunc Helper() {}\n")
	f.update()

	f.assertNoDanglingTestLinks()
	after := f.testLink("helper_test.go", "helper.go")
	assertTargetBound(t, after)

	// The regular edge from the same test file must bind in the same batch.
	if e := f.edge("helper_test.go", "Helper"); e.DstQualifiedName != "pkg.Helper" {
		t.Fatalf("edge dst = %q, want pkg.Helper", e.DstQualifiedName)
	}
}

// TestTwoPass_IncrementalTargetRenameDropsStaleTargets is case J: the target is
// renamed away in an incremental batch, so both the test link and the regular
// edge must lose their target rather than survive against a deleted symbol row.
func TestTwoPass_IncrementalTargetRenameDropsStaleTargets(t *testing.T) {
	f := newReindexFixture(t)
	f.write("helper.go", "package pkg\n\nfunc Helper() {}\n")
	f.write("helper_test.go", helperTestSrc)
	f.index()

	assertTargetBound(t, f.testLink("helper_test.go", "helper.go"))
	if e := f.edge("helper_test.go", "Helper"); e.DstQualifiedName != "pkg.Helper" {
		t.Fatalf("edge dst = %q, want pkg.Helper before rename", e.DstQualifiedName)
	}

	// Rename the target away. Pass 1 unbinds, and Pass 2 must not re-bind it to
	// anything: the key no longer resolves.
	f.write("helper.go", "package pkg\n\nfunc Renamed() {}\n")
	f.update()

	f.assertNoDanglingTestLinks()
	assertTargetUnbound(t, f.testLink("helper_test.go", "helper.go"))
	if e := f.edge("helper_test.go", "Helper"); e.DstSymbolID != nil {
		t.Fatalf("edge dst symbol = %d after rename, want unresolved", *e.DstSymbolID)
	}
}

// TestTwoPass_IncrementalTargetMovedToAnotherFileRebinds covers the purge side
// of the same invariant: the target's *file* is deleted while the definition
// reappears elsewhere in the same batch.
//
// The purge path used to delete the whole test_links row (its only defence
// against a target_file_id pointing at a purged file), which destroyed the
// parser fact along with it -- so the link could never rebind until the test
// file itself changed, which is the failure mode P10 exists to remove. Purge now
// unbinds instead, and Pass 2 rebinds against the moved definition.
func TestTwoPass_IncrementalTargetMovedToAnotherFileRebinds(t *testing.T) {
	f := newReindexFixture(t)
	f.write("helper.go", "package pkg\n\nfunc Helper() {}\n")
	f.write("helper_test.go", helperTestSrc)
	f.index()
	assertTargetBound(t, f.testLink("helper_test.go", "helper.go"))

	// One batch: the target file goes away, the definition reappears in another
	// file. The test file itself is untouched.
	f.remove("helper.go")
	f.write("moved.go", "package pkg\n\nfunc Helper() {}\n")
	f.update()

	f.assertNoDanglingTestLinks()
	after := f.testLink("helper_test.go", "moved.go")
	assertTargetBound(t, after)
}

// TestTwoPass_IncrementalTargetFileDeletedLeavesNoGhostTarget is the other half:
// when the definition does *not* reappear, unbinding must leave no target at
// all, so RelatedTests(file=...) cannot surface a purged file.
func TestTwoPass_IncrementalTargetFileDeletedLeavesNoGhostTarget(t *testing.T) {
	f := newReindexFixture(t)
	f.write("helper.go", "package pkg\n\nfunc Helper() {}\n")
	f.write("helper_test.go", helperTestSrc)
	f.index()
	assertTargetBound(t, f.testLink("helper_test.go", "helper.go"))

	f.remove("helper.go")
	f.update()

	f.assertNoDanglingTestLinks()
	after := f.testLink("helper_test.go", "")
	assertTargetUnbound(t, after)
	if after.TargetFile != "" {
		t.Fatalf("target file = %q after purge, want none", after.TargetFile)
	}
	related, err := f.store.RelatedTests(f.ctx, f.repoID, "", "helper.go", 10, 0)
	if err != nil {
		t.Fatalf("RelatedTests(file=helper.go) error = %v", err)
	}
	if len(related) != 0 {
		t.Fatalf("RelatedTests(file=helper.go) = %d rows after purge, want 0", len(related))
	}
}

// TestTwoPass_FullIndexIsOrderIndependentUnderRepeat drives the real indexer
// (concurrent file workers, nondeterministic completion order) repeatedly and
// requires an identical semantic graph every time. This is the property the
// flaky test was accidentally sampling; here it is asserted directly.
func TestTwoPass_FullIndexIsOrderIndependentUnderRepeat(t *testing.T) {
	sources := map[string]string{
		"helper.go":      orderHelperSrc,
		"helper_test.go": orderHelperTestSrc,
		"caller.go":      "package pkg\n\nfunc Caller() { Helper() }\n",
		"worker.go":      "package pkg\n\nfunc Worker() { Caller() }\n",
		"worker_test.go": "package pkg\n\nimport \"testing\"\n\nfunc TestWorker(t *testing.T) { Worker() }\n",
	}

	var want []string
	for range 20 {
		f := newReindexFixture(t)
		for rel, src := range sources {
			f.write(rel, src)
		}
		f.index()
		got, err := f.store.SemanticGraphForTest(f.ctx, f.repoID)
		if err != nil {
			t.Fatalf("SemanticGraphForTest() error = %v", err)
		}
		if want == nil {
			want = got
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("index run produced a different graph\nfirst:\n  %v\nlater:\n  %v", want, got)
		}
	}
	requireLine(t, want,
		`testlink helper_test.go:pkg.TestHelper -> "func:pkg::Helper" => helper.go:pkg.Helper [test_name_match/0.80]`)
	requireLine(t, want,
		`testlink worker_test.go:pkg.TestWorker -> "func:pkg::Worker" => worker.go:pkg.Worker [test_name_match/0.80]`)
}
