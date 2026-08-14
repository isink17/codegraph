package store

import (
	"context"
	"testing"

	"github.com/isink17/codegraph/internal/limits"
)

// TestBigGraphFixtureShape pins the properties the P12 latency numbers depend
// on. A benchmark table is only evidence if the graph underneath it is the
// shape it claims to be: roughly 100k symbols, a genuinely non-uniform degree
// distribution, a real unresolved-edge population, and a hot hub. If a future
// change flattens the fixture, the latency claims silently stop meaning
// anything -- so the shape is asserted, not assumed.
func TestBigGraphFixtureShape(t *testing.T) {
	if testing.Short() {
		t.Skip("big fixture build is too slow for -short")
	}
	if raceEnabled {
		// Building ~100k symbols under the race detector takes minutes and
		// pushes the package past the default test timeout. The shape is
		// data, not concurrency: -race adds nothing here.
		t.Skip("big fixture build is too slow under -race")
	}
	ctx := context.Background()
	f := bigGraph(t)
	t.Logf("fixture: %s", f.Describe())

	if f.Symbols < 95_000 || f.Symbols > 115_000 {
		t.Fatalf("Symbols = %d, want ~100k", f.Symbols)
	}
	if f.Unresolved == 0 {
		t.Fatal("fixture has no unresolved edges; classification paths would be untested")
	}
	if ratio := float64(f.Resolved) / float64(f.Edges); ratio < 0.80 {
		t.Fatalf("resolved edge ratio = %.3f, want >= 0.80", ratio)
	}
	if f.TestLinks == 0 {
		t.Fatal("fixture has no test links")
	}

	degree := func(qname string, callers bool) int64 {
		t.Helper()
		var symID int64
		if err := f.store.db.QueryRowContext(ctx,
			`SELECT id FROM symbols WHERE repo_id = ? AND qualified_name = ?`, f.repoID, qname).Scan(&symID); err != nil {
			t.Fatalf("lookup %s: %v", qname, err)
		}
		col := "dst_symbol_id"
		if !callers {
			col = "src_symbol_id"
		}
		var n int64
		if err := f.store.db.QueryRowContext(ctx,
			`SELECT COUNT(1) FROM edges WHERE repo_id = ? AND `+col+` = ?`, f.repoID, symID).Scan(&n); err != nil {
			t.Fatalf("degree %s: %v", qname, err)
		}
		return n
	}

	distinctCallers := func(qname string) int64 {
		t.Helper()
		var n int64
		if err := f.store.db.QueryRowContext(ctx, `
			SELECT COUNT(DISTINCT e.src_symbol_id)
			FROM edges e JOIN symbols s ON s.id = e.dst_symbol_id
			WHERE e.repo_id = ? AND s.qualified_name = ?`, f.repoID, qname).Scan(&n); err != nil {
			t.Fatalf("distinct callers %s: %v", qname, err)
		}
		return n
	}

	hubIn := degree(f.HubTarget, true)
	medIn := degree(f.MediumTarget, true)
	lowIn := degree(f.LowTarget, true)
	hubOut := degree(f.HubSource, false)
	t.Logf("degrees: hub_in=%d medium_in=%d low_in=%d hub_out=%d", hubIn, medIn, lowIn, hubOut)

	if hubIn < 10_000 {
		t.Fatalf("hub in-degree = %d, want >= 10000", hubIn)
	}
	if medIn < 100 || medIn > 5_000 {
		t.Fatalf("medium in-degree = %d, want a genuine middle tier", medIn)
	}
	if lowIn == 0 || lowIn > 10 {
		t.Fatalf("low in-degree = %d, want a small non-zero degree", lowIn)
	}
	if hubOut < 1_000 {
		t.Fatalf("hub out-degree = %d, want >= 1000", hubOut)
	}
	// Fan-in has to come from distinct callers, not from one caller repeated:
	// FindCallers' cost tracks distinct callers, so a hub built the other way
	// would be fast for a reason no real repository provides.
	if callers := distinctCallers(f.HubTarget); callers < 10_000 {
		t.Fatalf("hub distinct callers = %d, want >= 10000 (in-degree %d)", callers, hubIn)
	}

	// The unresolved fan-out must include snake_case destinations: '_' is a
	// LIKE metacharacter, and a name-lookup path that special-cases it would
	// otherwise never be exercised.
	var snakeUnresolved int64
	if err := f.store.db.QueryRowContext(ctx, `
		SELECT COUNT(1) FROM edges
		WHERE repo_id = ? AND dst_symbol_id IS NULL AND dst_name LIKE '%\_%' ESCAPE '\'`,
		f.repoID).Scan(&snakeUnresolved); err != nil {
		t.Fatalf("count snake_case unresolved: %v", err)
	}
	if snakeUnresolved < 50 {
		t.Fatalf("fixture has %d unresolved snake_case destinations, want >= 50", snakeUnresolved)
	}

	// Ambiguity: the common name must genuinely resolve to many symbols.
	var common int64
	if err := f.store.db.QueryRowContext(ctx,
		`SELECT COUNT(1) FROM symbols WHERE repo_id = ? AND name = ?`, f.repoID, f.CommonName).Scan(&common); err != nil {
		t.Fatalf("count common name: %v", err)
	}
	if common < 100 {
		t.Fatalf("common name %q matches %d symbols, want >= 100", f.CommonName, common)
	}

	// The chain must be deep and bound end to end.
	chain, _, err := f.store.TraceDependencies(ctx, f.repoID, f.ChainHead, "downstream", 10, limits.MaxPage, 0)
	if err != nil {
		t.Fatalf("TraceDependencies: %v", err)
	}
	if len(chain) < 5 {
		t.Fatalf("chain trace returned %d nodes, want a deep chain", len(chain))
	}

	// Concentrated test-link fan-out on one file.
	var hotLinks int64
	if err := f.store.db.QueryRowContext(ctx, `
		SELECT COUNT(1) FROM test_links t
		JOIN files fl ON fl.id = t.target_file_id
		WHERE t.repo_id = ? AND fl.path = ?`, f.repoID, f.HotFile).Scan(&hotLinks); err != nil {
		t.Fatalf("count hot test links: %v", err)
	}
	if hotLinks < 100 {
		t.Fatalf("hot file has %d test links, want >= 100", hotLinks)
	}

	// The missing name must really be missing.
	if syms, err := f.store.FindSymbolExact(ctx, f.repoID, f.MissingName, 20, 0); err != nil {
		t.Fatalf("FindSymbolExact(missing): %v", err)
	} else if len(syms) != 0 {
		t.Fatalf("missing name matched %d symbols", len(syms))
	}
}
