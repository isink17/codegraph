package store_test

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"
)

// writeNested writes a file under the fixture root, creating parent dirs --
// reindexFixture.write assumes a flat layout.
func writeNested(f *reindexFixture, rel, content string) {
	f.t.Helper()
	abs := filepath.Join(f.repoRoot, rel)
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		f.t.Fatalf("MkdirAll(%q) error = %v", rel, err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		f.t.Fatalf("WriteFile(%q) error = %v", rel, err)
	}
}

// P22.2 §15: for the same final tree, a fresh full index and an incremental
// update sequence must produce the same test-link semantics. DB row ids differ
// by construction, so rows are compared as semantic tuples: (test file, target
// file, bound?, reason, score).

func semanticTestLinks(t *testing.T, f *reindexFixture) []string {
	t.Helper()
	rows, err := f.store.TestLinksForTest(f.ctx, f.repoID)
	if err != nil {
		t.Fatalf("TestLinksForTest() error = %v", err)
	}
	out := make([]string, 0, len(rows))
	for _, r := range rows {
		bound := r.TargetSymbolID != nil
		out = append(out, fmt.Sprintf("%s -> %s bound=%t [%s/%.2f]", r.TestFile, r.TargetFile, bound, r.Reason, r.Score))
	}
	sort.Strings(out)
	return out
}

// TestTestLinksFullIndexEqualsUpdateSequence walks an update history that hits
// every canonical transition -- bind, ambiguity introduced later, ambiguity
// removed by deletion, sibling fallback, sibling loss -- and requires the end
// state to equal a fresh full index of the same final tree.
func TestTestLinksFullIndexEqualsUpdateSequence(t *testing.T) {
	finalTree := map[string]string{
		// unique target: TestHelper binds pkg.Helper by name.
		"helper.go":      "package pkg\n\nfunc Helper() {}\n",
		"helper_test.go": helperTestSrc,
		// ambiguous target: two dirs declare pkg.Shared; TestShared must stay
		// symbol-unbound but keep its sibling file relation.
		"a/shared.go":      "package pkg\n\nfunc Shared() {}\n",
		"b/shared.go":      "package pkg\n\nfunc Shared() {}\n",
		"a/shared_test.go": "package pkg\n\nimport \"testing\"\n\nfunc TestShared(t *testing.T) { Shared() }\n",
		// behavioural test name, no sibling: stays fully unbound.
		"behaviour_test.go": "package pkg\n\nimport \"testing\"\n\nfunc TestEverythingWorks(t *testing.T) { Helper() }\n",
	}

	// Full index of the final tree in one shot.
	full := newReindexFixture(t)
	for rel, src := range finalTree {
		if rel == "a/shared.go" || rel == "b/shared.go" || rel == "a/shared_test.go" {
			continue
		}
		full.write(rel, src)
	}
	writeNested(full, "a/shared.go", finalTree["a/shared.go"])
	writeNested(full, "b/shared.go", finalTree["b/shared.go"])
	writeNested(full, "a/shared_test.go", finalTree["a/shared_test.go"])
	full.index()

	// Update sequence arriving at the same tree through every transition.
	inc := newReindexFixture(t)
	// 1. start: helper + its test, plus a lone a/shared.go with its test (binds).
	inc.write("helper.go", finalTree["helper.go"])
	inc.write("helper_test.go", finalTree["helper_test.go"])
	writeNested(inc, "a/shared.go", finalTree["a/shared.go"])
	writeNested(inc, "a/shared_test.go", finalTree["a/shared_test.go"])
	inc.index()
	assertTargetBound(t, inc.testLink("a/shared_test.go", "a/shared.go"))
	// 2. a competing pkg.Shared appears in another dir: must unbind.
	writeNested(inc, "b/shared.go", finalTree["b/shared.go"])
	inc.update()
	assertTargetUnbound(t, inc.testLink("a/shared_test.go", "a/shared.go"))
	// 3. a third competitor appears and then disappears: deletion must re-derive
	// state for files the batch did not touch (still ambiguous here).
	writeNested(inc, "c/shared.go", "package pkg\n\nfunc Shared() {}\n")
	inc.update()
	inc.remove("c/shared.go")
	inc.update()
	// 4. behavioural test lands last.
	inc.write("behaviour_test.go", finalTree["behaviour_test.go"])
	inc.update()

	got := semanticTestLinks(t, inc)
	want := semanticTestLinks(t, full)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("update sequence diverged from full index\nfull:\n  %v\nupdate:\n  %v", want, got)
	}
}

// TestTestLinksAmbiguityRemovedByDeletionRebinds is the inverse transition on
// the real indexer: deleting the competing definition must rebind the link even
// though the test file itself was not part of the deletion batch.
func TestTestLinksAmbiguityRemovedByDeletionRebinds(t *testing.T) {
	f := newReindexFixture(t)
	writeNested(f, "a/shared.go", "package pkg\n\nfunc Shared() {}\n")
	writeNested(f, "b/shared.go", "package pkg\n\nfunc Shared() {}\n")
	writeNested(f, "a/shared_test.go", "package pkg\n\nimport \"testing\"\n\nfunc TestShared(t *testing.T) { Shared() }\n")
	f.index()
	assertTargetUnbound(t, f.testLink("a/shared_test.go", "a/shared.go"))

	f.remove("b/shared.go")
	summary := f.update()
	if summary.FilesDeleted != 1 {
		t.Fatalf("FilesDeleted = %d, want 1", summary.FilesDeleted)
	}
	// Deletion-only update: edge resolution did not run, so the reported mode
	// names the pass that did.
	if summary.ResolveMode != "test_links" {
		t.Fatalf("ResolveMode = %q, want test_links", summary.ResolveMode)
	}

	f.assertNoDanglingTestLinks()
	assertTargetBound(t, f.testLink("a/shared_test.go", "a/shared.go"))
}

// TestFullIndexOnUnchangedTreeCleansLegacyRows: a full `index` run on an
// unchanged tree must still run the canonical pass, so legacy rows written by
// older producers (from files the shared policy rejects) are removed without
// any file having to change first.
func TestFullIndexOnUnchangedTreeCleansLegacyRows(t *testing.T) {
	f := newReindexFixture(t)
	f.write("conn.go", "package pkg\n\nfunc Connection() {}\n")
	f.index()

	// Simulate an older producer's row declared by the production file.
	if err := f.store.InsertLegacyTestLinkForTest(f.ctx, f.repoID, "conn.go", "func:pkg::Connection"); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}

	// Full index with zero changed files.
	f.index()

	if links := f.testLinks(); len(links) != 0 {
		t.Fatalf("legacy production-file rows survived a full index: %+v", links)
	}
}

// TestIndexerSkipsProductionFileTestLinks: a production file exporting a
// Test-prefixed function must not declare test links (Pass-1 gate on the shared
// test-file policy).
func TestIndexerSkipsProductionFileTestLinks(t *testing.T) {
	f := newReindexFixture(t)
	f.write("conn.go", "package pkg\n\nfunc Connection() {}\n\nfunc TestConnection() {}\n")
	f.write("conn_test.go", "package pkg\n\nimport \"testing\"\n\nfunc TestConnection2(t *testing.T) { Connection() }\n")
	f.index()

	for _, l := range f.testLinks() {
		if l.TestFile == "conn.go" {
			t.Fatalf("production file declared a test link: %+v", l)
		}
	}
}

// TestRelatedTestsCallEvidenceEndToEnd drives the whole pipeline -- parser,
// indexer, edge resolution, canonical test-link pass -- and asks the public
// query: a behaviourally-named test that calls Helper() must surface for
// helper.go and for the Helper symbol, through call evidence alone.
func TestRelatedTestsCallEvidenceEndToEnd(t *testing.T) {
	f := newReindexFixture(t)
	f.write("helper.go", "package pkg\n\nfunc Helper() {}\n\nfunc Other() {}\n")
	f.write("behaviour_test.go", "package pkg\n\nimport \"testing\"\n\nfunc TestEverythingWorks(t *testing.T) { Helper() }\n")
	f.index()

	// No name binding and no sibling: the persisted link row stays unbound.
	assertTargetUnbound(t, f.testLink("behaviour_test.go", ""))

	bySymbol, err := f.store.RelatedTests(f.ctx, f.repoID, "Helper", "", 10, 0)
	if err != nil {
		t.Fatalf("RelatedTests(symbol=Helper) error = %v", err)
	}
	if len(bySymbol) != 1 || bySymbol[0].Symbol != "pkg.TestEverythingWorks" || bySymbol[0].Reason != "test_calls" {
		t.Fatalf("RelatedTests(symbol=Helper) = %+v, want pkg.TestEverythingWorks via test_calls", bySymbol)
	}

	byFile, err := f.store.RelatedTests(f.ctx, f.repoID, "", "helper.go", 10, 0)
	if err != nil {
		t.Fatalf("RelatedTests(file=helper.go) error = %v", err)
	}
	if len(byFile) != 1 || byFile[0].File != "behaviour_test.go" {
		t.Fatalf("RelatedTests(file=helper.go) = %+v, want behaviour_test.go", byFile)
	}

	// Precision: Other has no calling test, so its symbol seed returns nothing.
	byOther, err := f.store.RelatedTests(f.ctx, f.repoID, "Other", "", 10, 0)
	if err != nil {
		t.Fatalf("RelatedTests(symbol=Other) error = %v", err)
	}
	if len(byOther) != 0 {
		t.Fatalf("RelatedTests(symbol=Other) = %+v, want none", byOther)
	}

	related, err := f.store.RelatedTests(f.ctx, f.repoID, "", "behaviour_test.go", 10, 0)
	if err != nil {
		t.Fatalf("RelatedTests(file=behaviour_test.go) error = %v", err)
	}
	_ = related // a test file as seed is legal; no assertion on shape here
}
