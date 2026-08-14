package store_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/limits"
	"github.com/isink17/codegraph/internal/store"
)

// impactFixture indexes a small chain so that a traversal from Root reaches
// enough symbols to page through.
func impactFixture(t *testing.T) *reindexFixture {
	t.Helper()
	f := newReindexFixture(t)
	f.write("chain.go", `package pkg

func Leaf1() int { return 1 }
func Leaf2() int { return 2 }
func Leaf3() int { return 3 }
func Leaf4() int { return 4 }

func Mid1() int { return Leaf1() + Leaf2() }
func Mid2() int { return Leaf3() + Leaf4() }

func Root() int { return Mid1() + Mid2() }
`)
	f.index()
	return f
}

func impactSummary(t *testing.T, data map[string]any) map[string]any {
	t.Helper()
	summary, ok := data["summary"].(map[string]any)
	if !ok {
		t.Fatalf("summary = %T, want a map", data["summary"])
	}
	return summary
}

// TestImpactRadiusPagingIsConsistent pins the contract of the page itself: the
// summary describes the page and the closure separately, and the two halves of
// the response (symbols and files) always describe the same set.
func TestImpactRadiusPagingIsConsistent(t *testing.T) {
	f := impactFixture(t)

	whole, err := f.store.ImpactRadius(f.ctx, f.repoID, []string{"Root"}, nil, 5, limits.MaxPage, 0)
	if err != nil {
		t.Fatalf("ImpactRadius() error = %v", err)
	}
	all, _ := whole["symbols"].([]graph.Symbol)
	total := len(all)
	if total < 4 {
		t.Fatalf("fixture reached %d symbols, too few to page", total)
	}
	if got := impactSummary(t, whole)["truncated"]; got != false {
		t.Fatalf("a full page reported truncated = %v, want false", got)
	}

	// One row at a time must reproduce the whole closure, in order, with no
	// gaps and no repeats.
	var walked []int64
	for offset := 0; offset < total; offset++ {
		page, err := f.store.ImpactRadius(f.ctx, f.repoID, []string{"Root"}, nil, 5, 1, offset)
		if err != nil {
			t.Fatalf("ImpactRadius(offset=%d) error = %v", offset, err)
		}
		syms, _ := page["symbols"].([]graph.Symbol)
		if len(syms) != 1 {
			t.Fatalf("offset=%d returned %d symbols, want 1", offset, len(syms))
		}
		walked = append(walked, syms[0].ID)

		summary := impactSummary(t, page)
		if summary["affected_symbols"] != total {
			t.Fatalf("offset=%d affected_symbols = %v, want the whole closure %d", offset, summary["affected_symbols"], total)
		}
		if summary["returned_symbols"] != 1 {
			t.Fatalf("offset=%d returned_symbols = %v, want 1", offset, summary["returned_symbols"])
		}
		if summary["offset"] != offset {
			t.Fatalf("offset=%d reported offset %v", offset, summary["offset"])
		}
		wantTruncated := offset+1 < total
		if summary["truncated"] != wantTruncated {
			t.Fatalf("offset=%d truncated = %v, want %v", offset, summary["truncated"], wantTruncated)
		}

		// The files half is page-scoped and reports its own count, so a reader
		// can reconcile it instead of comparing it against the closure total.
		files, _ := page["files"].([]string)
		if summary["returned_files"] != len(files) {
			t.Fatalf("offset=%d returned_files = %v but files has %d entries", offset, summary["returned_files"], len(files))
		}
		if files[0] != syms[0].FilePath {
			t.Fatalf("offset=%d files %v do not match the page's symbol file %q", offset, files, syms[0].FilePath)
		}
	}
	for i, id := range walked {
		if id != all[i].ID {
			t.Fatalf("paged walk row %d = %d, single page = %d", i, id, all[i].ID)
		}
	}
}

// TestImpactRadiusOffsetPastTheEndIsEmptyNotAnError covers the boundary that
// would panic on a naive slice.
func TestImpactRadiusOffsetPastTheEndIsEmptyNotAnError(t *testing.T) {
	f := impactFixture(t)
	page, err := f.store.ImpactRadius(f.ctx, f.repoID, []string{"Root"}, nil, 5, 10, 100000)
	if err != nil {
		t.Fatalf("ImpactRadius() error = %v", err)
	}
	syms, _ := page["symbols"].([]graph.Symbol)
	if len(syms) != 0 {
		t.Fatalf("offset past the end returned %d symbols, want 0", len(syms))
	}
	summary := impactSummary(t, page)
	if summary["truncated"] != false {
		t.Fatalf("offset past the end reported truncated = %v, want false", summary["truncated"])
	}
	// The reported offset is where the page actually starts, not the requested
	// number, so it never claims rows were skipped beyond the result.
	if summary["offset"] != summary["affected_symbols"] {
		t.Fatalf("offset = %v, want it clamped to the closure size %v", summary["offset"], summary["affected_symbols"])
	}
}

// TestImpactRadiusUnknownSeedIsSkippedButDBFailureIsNot separates the two cases
// the old blanket `continue` merged. A seed that is absent contributes nothing;
// a broken database must not produce a plausible empty impact set.
func TestImpactRadiusUnknownSeedIsSkippedButDBFailureIsNot(t *testing.T) {
	f := impactFixture(t)

	mixed, err := f.store.ImpactRadius(f.ctx, f.repoID, []string{"Root", "NoSuchSymbol"}, nil, 5, limits.MaxPage, 0)
	if err != nil {
		t.Fatalf("a missing seed alongside a real one errored: %v", err)
	}
	if syms, _ := mixed["symbols"].([]graph.Symbol); len(syms) == 0 {
		t.Fatal("a missing seed suppressed the real seed's impact")
	}

	if err := f.store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if _, err := f.store.ImpactRadius(f.ctx, f.repoID, []string{"Root"}, nil, 5, limits.MaxPage, 0); err == nil {
		t.Fatal("a closed database produced an impact set instead of an error")
	}
}

// TestTraceDependenciesReportsTotalIndependentOfThePage is the property the
// truncated flag is built on.
func TestTraceDependenciesReportsTotalIndependentOfThePage(t *testing.T) {
	f := impactFixture(t)

	full, total, err := f.store.TraceDependencies(f.ctx, f.repoID, "Root", "downstream", 5, limits.MaxPage, 0)
	if err != nil {
		t.Fatalf("TraceDependencies() error = %v", err)
	}
	if len(full) != total {
		t.Fatalf("an unpaged call returned %d rows but reported total %d", len(full), total)
	}
	if total < 3 {
		t.Fatalf("fixture traced %d nodes, too few to page", total)
	}

	page, pagedTotal, err := f.store.TraceDependencies(f.ctx, f.repoID, "Root", "downstream", 5, 2, 0)
	if err != nil {
		t.Fatalf("TraceDependencies(limit=2) error = %v", err)
	}
	if len(page) != 2 {
		t.Fatalf("limit=2 returned %d rows", len(page))
	}
	if pagedTotal != total {
		t.Fatalf("a page reported total %d, want the whole chain %d", pagedTotal, total)
	}

	beyond, beyondTotal, err := f.store.TraceDependencies(f.ctx, f.repoID, "Root", "downstream", 5, 10, 100000)
	if err != nil {
		t.Fatalf("TraceDependencies(offset past end) error = %v", err)
	}
	if len(beyond) != 0 {
		t.Fatalf("offset past the end returned %d rows, want 0", len(beyond))
	}
	if beyondTotal != total {
		t.Fatalf("offset past the end reported total %d, want %d", beyondTotal, total)
	}
}

// TestExportPagesAreNotBoundByTheQueryCeiling records why export has its own
// limit family: a bulk export legitimately asks for more rows in one page than
// any tool response may return.
func TestExportPagesAreNotBoundByTheQueryCeiling(t *testing.T) {
	f := impactFixture(t)
	syms, err := f.store.ExportSymbolsPage(f.ctx, f.repoID, limits.MaxPage*3, 0)
	if err != nil {
		t.Fatalf("ExportSymbolsPage() error = %v", err)
	}
	// The fixture is small, so the assertion is that the call is accepted and
	// returns everything rather than being trimmed to a query page.
	all, err := f.store.ExportSymbolsPage(f.ctx, f.repoID, limits.MaxExportPage, 0)
	if err != nil {
		t.Fatalf("ExportSymbolsPage(MaxExportPage) error = %v", err)
	}
	if len(syms) != len(all) {
		t.Fatalf("a %d-row export page returned %d symbols, a %d-row page returned %d", limits.MaxPage*3, len(syms), limits.MaxExportPage, len(all))
	}
}

// TestRelatedTestsSurfacesADatabaseFailureRatherThanNotFound guards the error
// shadowing that used to drop a query error on the symbol branch and leave a
// nil rows handle behind it.
func TestRelatedTestsSurfacesADatabaseFailureRatherThanNotFound(t *testing.T) {
	f := impactFixture(t)
	if err := f.store.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	_, err := f.store.RelatedTests(f.ctx, f.repoID, "Root", "", 10, 0)
	if err == nil {
		t.Fatal("a closed database produced a result instead of an error")
	}
	if strings.Contains(err.Error(), store.ErrSymbolNotFound.Error()) {
		t.Fatalf("a database failure was reported as a missing symbol: %v", err)
	}
}

// wideFixture builds a closure larger than the public page ceiling, which is
// the only size at which the tool bound and the export path can be told apart.
func wideFixture(t *testing.T) *reindexFixture {
	t.Helper()
	const leaves = limits.MaxPage + 120

	var src strings.Builder
	src.WriteString("package pkg\n\n")
	for i := 0; i < leaves; i++ {
		fmt.Fprintf(&src, "func Leaf%d() int { return %d }\n", i, i)
	}
	src.WriteString("\nfunc Root() int {\n\ttotal := 0\n")
	for i := 0; i < leaves; i++ {
		fmt.Fprintf(&src, "\ttotal += Leaf%d()\n", i)
	}
	src.WriteString("\treturn total\n}\n")

	f := newReindexFixture(t)
	f.write("wide.go", src.String())
	f.index()
	return f
}

// TestExportIsUnboundedWhileTheToolIsPaged is the P18 thesis in one test: the
// same traversal answers a tool with a bounded page and answers a bulk export
// with the whole subgraph.
//
// GraphSnapshot briefly asked ImpactRadius for StoreMaxRows rows, which capped
// a focused `graph export --symbol` at 2000 nodes with nothing in the output
// saying so. Export is the family that is allowed to be large.
func TestExportIsUnboundedWhileTheToolIsPaged(t *testing.T) {
	f := wideFixture(t)

	page, err := f.store.ImpactRadius(f.ctx, f.repoID, []string{"Root"}, nil, 2, limits.MaxPage, 0)
	if err != nil {
		t.Fatalf("ImpactRadius() error = %v", err)
	}
	paged, _ := page["symbols"].([]graph.Symbol)
	summary := impactSummary(t, page)
	closure, _ := summary["affected_symbols"].(int)

	if closure <= limits.MaxPage {
		t.Fatalf("fixture closure is %d symbols, not larger than the %d ceiling", closure, limits.MaxPage)
	}
	if len(paged) != limits.MaxPage {
		t.Fatalf("the tool page returned %d symbols, want the ceiling %d", len(paged), limits.MaxPage)
	}
	if summary["truncated"] != true {
		t.Fatalf("a page of %d from %d symbols reported truncated = %v", len(paged), closure, summary["truncated"])
	}

	// The export path sees the whole closure, with no ceiling applied.
	exported, _, err := f.store.GraphSnapshot(f.ctx, f.repoID, "Root", 2)
	if err != nil {
		t.Fatalf("GraphSnapshot() error = %v", err)
	}
	if len(exported) != closure {
		t.Fatalf("focused export returned %d symbols, want the whole closure %d", len(exported), closure)
	}
}
