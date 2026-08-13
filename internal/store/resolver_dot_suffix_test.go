package store

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// TestDotSuffixPrefilterTiers pins the classification the dot-suffix strategy's
// prefilter depends on. The exact tier applies an indexed necessary condition
// on `symbols.dot_tail2`, and misclassifying a name is silent: the resolver
// would simply stop finding candidates it used to find.
func TestDotSuffixPrefilterTiers(t *testing.T) {
	cases := []struct {
		name     string
		dstName  string
		wantTier int
		wantTail string
	}{
		{"camel tail is exact", "mod.Pkg.Type.Method", dotSuffixTierExact, "Type.Method"},
		{"underscore in the tail forces a scan", "mod.pkg.my_type.my_func", dotSuffixTierScan, ""},
		{"underscore outside the tail stays exact", "mod.my_pkg.Type.Method", dotSuffixTierExact, "Type.Method"},
		{"percent in the tail forces a scan", "mod.pkg.Type.Meth%d", dotSuffixTierScan, ""},
		{"percent outside the tail stays exact", "mod.pk%g.Type.Method", dotSuffixTierExact, "Type.Method"},
		// dotSuffixNames never yields a slashed name, but the classification
		// is sound on one anyway: dotTail2 drops everything up to the last
		// '/' on this side exactly as migration 017 did on the symbol side.
		{"slash is dropped on both sides", "mod.pkg/sub.Method", dotSuffixTierExact, "sub.Method"},
		{"nothing after the slash to pair", "mod.pkg/sub", dotSuffixTierScan, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tier, tail := dotSuffixPrefilter(tc.dstName)
			if tier != tc.wantTier || tail != tc.wantTail {
				t.Fatalf("dotSuffixPrefilter(%q) = (%d, %q), want (%d, %q)", tc.dstName, tier, tail, tc.wantTier, tc.wantTail)
			}
		})
	}
}

// TestResolveEdgesByDotSuffixPrefilterKeepsLikeSemantics drives the real
// strategy over dst_names whose resolution depends on LIKE behaviour the
// prefilter must not quietly tighten.
//
// The wildcard cases are the load-bearing ones: SQLite's `_` matches any single
// character *including '.' and '/'*, so a symbol matching `mod.a_b.c` may carry
// a `dot_tail2` of `b.c` (the `_` matched a '.') or none at all (it matched a '/').
// Any prefilter that constrains the symbol's tail — by content or by length —
// drops those, which is why such names must run unfiltered.
//
// Each case has exactly one candidate, so P3/P7 leave the binding to the
// strategy and any lost candidate shows up as an unresolved edge.
func TestResolveEdgesByDotSuffixPrefilterKeepsLikeSemantics(t *testing.T) {
	cases := []struct {
		name      string
		qualified string
		dstName   string
	}{
		{"case insensitive tail", "root.mod.type1.method1", "mod.Type1.Method1"},
		{"underscore matches any character", "root.mod.aXb2.cYd2", "mod.a_b2.c_d2"},
		{"underscore matches a dot", "root.mod3.a.b.c", "mod3.a_b.c"},
		{"underscore matches a slash", "root.mod4.a/b.c", "mod4.a_b.c"},
		{"percent spans characters", "root.mod.aZZZb5.cd5", "mod.a%b5.cd5"},
		{"percent spans a slash", "root.mod6.a/x/b.c", "mod6.a%b.c"},
		{"plain literal tail", "root.mod.Type7.Method7", "mod.Type7.Method7"},
		// Exact tier with a slash in qualified_name: dot_tail2 comes from the
		// after-slash portion, which is exactly what the prefilter compares.
		{"slash before the matched tail", "pkg/root.mod8.Type8.Method8", "mod8.Type8.Method8"},
		{"percent spans the slash before the tail", "x.pkg/root.mod9.Type9.Method9", "pkg%root.mod9.Type9.Method9"},
	}

	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer s.Close()

	repo, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	fileID, err := insertTestFile(ctx, s, repo.ID, "caller.go")
	if err != nil {
		t.Fatalf("insertTestFile() error = %v", err)
	}
	srcID, err := insertTestSymbol(ctx, s, repo.ID, fileID, "Caller", "Caller")
	if err != nil {
		t.Fatalf("insertTestSymbol(src) error = %v", err)
	}

	wantTargets := make(map[string]int64, len(cases))
	for _, tc := range cases {
		targetID, err := insertTestSymbol(ctx, s, repo.ID, fileID, tc.name, tc.qualified)
		if err != nil {
			t.Fatalf("insertTestSymbol(%q) error = %v", tc.qualified, err)
		}
		if _, err := insertTestEdge(ctx, s, repo.ID, fileID, srcID, tc.dstName); err != nil {
			t.Fatalf("insertTestEdge(%q) error = %v", tc.dstName, err)
		}
		wantTargets[tc.dstName] = targetID
	}

	if _, err := s.ResolveEdges(ctx, repo.ID); err != nil {
		t.Fatalf("ResolveEdges() error = %v", err)
	}

	for _, tc := range cases {
		got := dotSuffixEdgeTarget(ctx, t, s, repo.ID, tc.dstName)
		want := wantTargets[tc.dstName]
		if got == nil {
			t.Fatalf("case %q: dst_name %q left unresolved, want symbol %d", tc.name, tc.dstName, want)
		}
		if *got != want {
			t.Fatalf("case %q: dst_name %q bound symbol %d, want %d", tc.name, tc.dstName, *got, want)
		}
	}
}

// TestResolveEdgesByDotSuffixPrefilterPreservesAmbiguity is the sharper half of
// the invariant above. Losing a candidate is not merely a missed resolution: if
// the lost one was what made a group ambiguous, P3's refusal collapses into a
// confident binding of the surviving symbol, which is a *wrong* edge carrying
// full DotSuffix provenance.
//
// The two candidates here are reachable only through different readings of the
// `_`: one where it matched an ordinary character (tail intact) and one where
// it matched a '.' (tail shifted). Any tail-constraining prefilter sees just
// one of them and binds it.
func TestResolveEdgesByDotSuffixPrefilterPreservesAmbiguity(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer s.Close()

	repo, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	fileID, err := insertTestFile(ctx, s, repo.ID, "caller.go")
	if err != nil {
		t.Fatalf("insertTestFile() error = %v", err)
	}
	srcID, err := insertTestSymbol(ctx, s, repo.ID, fileID, "Caller", "Caller")
	if err != nil {
		t.Fatalf("insertTestSymbol(src) error = %v", err)
	}

	// dot_tail2 = "aXb.c": the reading the prefilter would keep.
	if _, err := insertTestSymbol(ctx, s, repo.ID, fileID, "Intact", "root.mod.aXb.c"); err != nil {
		t.Fatalf("insertTestSymbol(intact) error = %v", err)
	}
	if _, err := insertTestEdge(ctx, s, repo.ID, fileID, srcID, "mod.a_b.c"); err != nil {
		t.Fatalf("insertTestEdge() error = %v", err)
	}
	// dot_tail2 = "b.c": only reachable when the '_' is read as a '.'.
	if _, err := insertTestSymbol(ctx, s, repo.ID, fileID, "Shifted", "root.mod.a.b.c"); err != nil {
		t.Fatalf("insertTestSymbol(shifted) error = %v", err)
	}

	if _, err := s.ResolveEdges(ctx, repo.ID); err != nil {
		t.Fatalf("ResolveEdges() error = %v", err)
	}

	if got := dotSuffixEdgeTarget(ctx, t, s, repo.ID, "mod.a_b.c"); got != nil {
		t.Fatalf("dst_name %q bound symbol %d; two same-language candidates match the LIKE pattern, so it must stay unresolved", "mod.a_b.c", *got)
	}
}

func dotSuffixEdgeTarget(ctx context.Context, t *testing.T, s *Store, repoID int64, dstName string) *int64 {
	t.Helper()
	var got *int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT dst_symbol_id FROM edges WHERE repo_id = ? AND dst_name = ?`, repoID, dstName,
	).Scan(&got); err != nil {
		t.Fatalf("reading edge for dst_name %q: %v", dstName, err)
	}
	return got
}

// TestResolveEdgesByDotSuffixSeedsAcrossStatementBatches drives more distinct
// dst_names than one seeding INSERT can carry, so the name table is filled by
// several statements plus a short final one. A dropped or double-counted batch
// boundary shows up as names missing from the table, which the strategy would
// report only as edges quietly left unresolved.
func TestResolveEdgesByDotSuffixSeedsAcrossStatementBatches(t *testing.T) {
	// Two full statements plus a one-row remainder.
	const numNames = 2*dotSuffixNamesPerStatement + 1

	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	defer s.Close()

	repo, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	fileID, err := insertTestFile(ctx, s, repo.ID, "caller.go")
	if err != nil {
		t.Fatalf("insertTestFile() error = %v", err)
	}
	srcID, err := insertTestSymbol(ctx, s, repo.ID, fileID, "Caller", "Caller")
	if err != nil {
		t.Fatalf("insertTestSymbol(src) error = %v", err)
	}

	want := make(map[string]int64, numNames)
	for i := 0; i < numNames; i++ {
		// Alternate the tiers so both passes have to see their share of the
		// seeded rows, not just the first batch.
		// Three dots, so the exactly-2-dot `dot_tail3` prelude does not claim
		// these before the dot-suffix pass runs.
		dstName := fmt.Sprintf("mod%d.Pkg%d.Type%d.Method%d", i, i, i, i)
		if i%2 == 1 {
			dstName = fmt.Sprintf("mod%d.Pkg%d.Type%d.Meth%%d%d", i, i, i, i)
		}
		targetID, err := insertTestSymbol(ctx, s, repo.ID, fileID, fmt.Sprint(i), "root."+strings.ReplaceAll(dstName, "%", ""))
		if err != nil {
			t.Fatalf("insertTestSymbol(%d) error = %v", i, err)
		}
		if _, err := insertTestEdge(ctx, s, repo.ID, fileID, srcID, dstName); err != nil {
			t.Fatalf("insertTestEdge(%d) error = %v", i, err)
		}
		want[dstName] = targetID
	}

	if _, err := s.ResolveEdges(ctx, repo.ID); err != nil {
		t.Fatalf("ResolveEdges() error = %v", err)
	}

	for dstName, targetID := range want {
		got := dotSuffixEdgeTarget(ctx, t, s, repo.ID, dstName)
		if got == nil {
			t.Fatalf("dst_name %q left unresolved, want symbol %d", dstName, targetID)
		}
		if *got != targetID {
			t.Fatalf("dst_name %q bound symbol %d, want %d", dstName, *got, targetID)
		}
	}
}
