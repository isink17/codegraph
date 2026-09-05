package store

import (
	"context"
	"fmt"
	"testing"
)

// BenchmarkResolveEdgesByDotSuffix_ThreeDot stresses the part of
// `resolveEdgesByDotSuffix` the two older dot-suffix benchmarks do not reach.
//
// Those fixtures use dst_names with exactly two dots, which the `dot_tail3`
// prelude resolves by indexed equality before the LIKE pass ever sees them. A
// three-dot dst_name is outside `dot_tail3`'s seed (exactly-2-dot names only),
// so it lands on the `qualified_name LIKE '%.' || dst_name` join that used to
// scan every symbol once per distinct name.
//
// The two variants differ only in the shape of the name's last two segments,
// which is what decides the prefilter tier:
//
//	exact_tier  no LIKE wildcard in the tail -> indexed NOCASE equality on dot_tail2
//	scan_tier   '_' in the tail              -> unfiltered scan, i.e. the old cost
//
// The scan variant is deliberately kept: it is the floor the prefilter does not
// move, and snake_case names are exactly the shape that lands there. Noise
// symbols outnumber names, since the prefilter only pays off when the symbol
// table is larger than the set of names being looked up.
//
// Both must produce the same resolution counts as the unfiltered scan; that
// correctness is covered by the resolver tests, and this only measures what the
// prefilter is worth.
func BenchmarkResolveEdgesByDotSuffix_ThreeDot(b *testing.B) {
	cases := []struct {
		name string
		tail func(i int) string
	}{
		{"exact_tier", func(i int) string { return fmt.Sprintf("Pkg%d.Type%d.Method%d", i%50, i%25, i) }},
		{"scan_tier", func(i int) string { return fmt.Sprintf("pkg_%d.type_%d.method_%d", i%50, i%25, i) }},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			ctx := context.Background()
			s := openBenchStore(b)
			defer s.Close()

			repoID := upsertBenchRepo(ctx, b, s)

			const (
				numFiles     = 200
				numNames     = 1000
				numNoiseSyms = 6000
				numEdges     = 2000
			)

			fileIDs := makeBenchFiles(ctx, b, s, repoID, numFiles)
			srcIDs := makeBenchSrcSymbols(ctx, b, s, repoID, fileIDs)

			// dst_name carries three dots; the target's qualified_name adds a
			// leading segment so the LIKE suffix match has a '.' to anchor on.
			for i := 0; i < numNames; i++ {
				dstName := "mod" + fmt.Sprint(i%10) + "." + tc.tail(i)
				if _, err := insertTestSymbol(ctx, s, repoID, fileIDs[i%len(fileIDs)], fmt.Sprint(i), "root."+dstName); err != nil {
					b.Fatalf("insertTestSymbol(dst) error = %v", err)
				}
			}
			for i := 0; i < numNoiseSyms; i++ {
				qualified := fmt.Sprintf("noise%d.Noise.Other%d", i%10, i)
				if _, err := insertTestSymbol(ctx, s, repoID, fileIDs[i%len(fileIDs)], fmt.Sprintf("Other%d", i), qualified); err != nil {
					b.Fatalf("insertTestSymbol(noise) error = %v", err)
				}
			}
			for i := 0; i < numEdges; i++ {
				nameIdx := i % numNames
				dstName := "mod" + fmt.Sprint(nameIdx%10) + "." + tc.tail(nameIdx)
				if _, err := insertTestEdge(ctx, s, repoID, fileIDs[i%len(fileIDs)], srcIDs[i%len(srcIDs)], dstName); err != nil {
					b.Fatalf("insertTestEdge() error = %v", err)
				}
			}

			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				b.StopTimer()
				tx, err := s.db.BeginTx(ctx, nil)
				if err != nil {
					b.Fatalf("BeginTx() error = %v", err)
				}
				// The strategy reads the per-resolve veto relations, so a
				// benchmark that drives it directly has to build them exactly as
				// ResolveEdges does. Untimed: setup, not the strategy measured.
				if err := s.prepareResolverTables(ctx, tx, repoID); err != nil {
					b.Fatalf("prepareResolverTables() error = %v", err)
				}
				if _, err := tx.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS tmp_resolver_own_module_veto(edge_id INTEGER PRIMARY KEY)`); err != nil {
					b.Fatalf("create own-module veto: %v", err)
				}
				b.StartTimer()
				if _, err := s.resolveEdgesByDotSuffix(ctx, tx, repoID); err != nil {
					b.Fatalf("resolveEdgesByDotSuffix() error = %v", err)
				}
				b.StopTimer()
				_ = tx.Rollback()
			}
		})
	}
}
