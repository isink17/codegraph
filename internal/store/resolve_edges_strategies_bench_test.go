package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

// BenchmarkResolveEdgesBySlashSuffix_SlashOnly drives only the slash-suffix
// branch inside resolveEdgesBySlashSuffix: dst_name has zero dots so the
// dot-tail2 needed-set stays empty, and symbol qualified_names contain a slash
// so the slash-suffix map is populated.
func BenchmarkResolveEdgesBySlashSuffix_SlashOnly(b *testing.B) {
	ctx := context.Background()
	s := openBenchStore(b)
	defer s.Close()

	repoID := upsertBenchRepo(ctx, b, s)

	const (
		numFiles       = 100
		numNames       = 1000
		numNoiseSyms   = 1000
		numEdges       = 5000
		dstQNamePrefix = "github.com/org/repo/pkg/"
	)

	fileIDs := makeBenchFiles(ctx, b, s, repoID, numFiles)
	srcIDs := makeBenchSrcSymbols(ctx, b, s, repoID, fileIDs)

	// Slash-qualified target symbols: qualified_name has a '/', name part has no dot.
	for i := 0; i < numNames; i++ {
		name := fmt.Sprintf("Func_%d", i)
		qualified := fmt.Sprintf("%spkg_%d/%s", dstQNamePrefix, i%50, name)
		fileID := fileIDs[i%len(fileIDs)]
		if _, err := insertTestSymbol(ctx, s, repoID, fileID, name, qualified); err != nil {
			b.Fatalf("insertTestSymbol(slash dst) error = %v", err)
		}
	}
	// Noise symbols (no slash, irrelevant to this strategy).
	for i := 0; i < numNoiseSyms; i++ {
		name := fmt.Sprintf("Noise_%d", i)
		qualified := fmt.Sprintf("noise.%s", name)
		fileID := fileIDs[i%len(fileIDs)]
		if _, err := insertTestSymbol(ctx, s, repoID, fileID, name, qualified); err != nil {
			b.Fatalf("insertTestSymbol(noise) error = %v", err)
		}
	}

	// Unresolved edges: dst_name = simple "Func_<i>" with no dot, no slash.
	for i := 0; i < numEdges; i++ {
		dstName := fmt.Sprintf("Func_%d", i%numNames)
		fileID := fileIDs[i%len(fileIDs)]
		srcID := srcIDs[i%len(srcIDs)]
		if _, err := insertTestEdge(ctx, s, repoID, fileID, srcID, dstName); err != nil {
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
		b.StartTimer()
		if _, err := s.resolveEdgesBySlashSuffix(ctx, tx, repoID); err != nil {
			b.Fatalf("resolveEdgesBySlashSuffix() error = %v", err)
		}
		b.StopTimer()
		_ = tx.Rollback()
	}
}

// BenchmarkResolveEdgesBySlashSuffix_DotTail2 drives only the dot-tail2 branch
// inside resolveEdgesBySlashSuffix: dst_name has exactly one dot (so it lands
// in neededTail2), and symbol qualified_names have no slash but enough dot
// segments that only the tail2 logic resolves them.
func BenchmarkResolveEdgesBySlashSuffix_DotTail2(b *testing.B) {
	ctx := context.Background()
	s := openBenchStore(b)
	defer s.Close()

	repoID := upsertBenchRepo(ctx, b, s)

	const (
		numFiles     = 100
		numNames     = 1000
		numNoiseSyms = 1000
		numEdges     = 5000
	)

	fileIDs := makeBenchFiles(ctx, b, s, repoID, numFiles)
	srcIDs := makeBenchSrcSymbols(ctx, b, s, repoID, fileIDs)

	// Dot-tail2 target symbols: no slash, qualified_name has a leading segment
	// before the matched 2-segment tail (e.g., "io.pkg_3.Func_42").
	for i := 0; i < numNames; i++ {
		name := fmt.Sprintf("Func_%d", i)
		qualified := fmt.Sprintf("io.pkg_%d.%s", i%50, name)
		fileID := fileIDs[i%len(fileIDs)]
		if _, err := insertTestSymbol(ctx, s, repoID, fileID, name, qualified); err != nil {
			b.Fatalf("insertTestSymbol(dot-tail2 dst) error = %v", err)
		}
	}
	// Noise symbols with slashes (excluded from dot-tail2 by the slash-branch
	// `continue` because afterSlash isn't in neededSuffix).
	for i := 0; i < numNoiseSyms; i++ {
		name := fmt.Sprintf("Noise_%d", i)
		qualified := fmt.Sprintf("github.com/org/repo/noise/%s", name)
		fileID := fileIDs[i%len(fileIDs)]
		if _, err := insertTestSymbol(ctx, s, repoID, fileID, name, qualified); err != nil {
			b.Fatalf("insertTestSymbol(noise) error = %v", err)
		}
	}

	// Unresolved edges: dst_name = "pkg_X.Func_Y" (exactly one dot, no slash).
	for i := 0; i < numEdges; i++ {
		dstName := fmt.Sprintf("pkg_%d.Func_%d", i%50, i%numNames)
		fileID := fileIDs[i%len(fileIDs)]
		srcID := srcIDs[i%len(srcIDs)]
		if _, err := insertTestEdge(ctx, s, repoID, fileID, srcID, dstName); err != nil {
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
		b.StartTimer()
		if _, err := s.resolveEdgesBySlashSuffix(ctx, tx, repoID); err != nil {
			b.Fatalf("resolveEdgesBySlashSuffix() error = %v", err)
		}
		b.StopTimer()
		_ = tx.Rollback()
	}
}

// BenchmarkResolveEdgesByDotSuffix drives resolveEdgesByDotSuffix: dst_name has
// at least two dots and no slash, so the multi-dot pre-filter accepts it and
// the LIKE '%.' || dst_name suffix scan is exercised.
func BenchmarkResolveEdgesByDotSuffix(b *testing.B) {
	ctx := context.Background()
	s := openBenchStore(b)
	defer s.Close()

	repoID := upsertBenchRepo(ctx, b, s)

	const (
		numFiles     = 100
		numNames     = 1000
		numNoiseSyms = 1000
		numEdges     = 5000
	)

	fileIDs := makeBenchFiles(ctx, b, s, repoID, numFiles)
	srcIDs := makeBenchSrcSymbols(ctx, b, s, repoID, fileIDs)

	// Multi-dot target symbols: qualified_name = "x.<dst_name>" so the LIKE
	// '%.<dst_name>' suffix match resolves them.
	for i := 0; i < numNames; i++ {
		dstSuffix := fmt.Sprintf("a_%d.b_%d.c_%d", i%50, i%25, i)
		qualified := "x." + dstSuffix
		fileID := fileIDs[i%len(fileIDs)]
		if _, err := insertTestSymbol(ctx, s, repoID, fileID, fmt.Sprintf("c_%d", i), qualified); err != nil {
			b.Fatalf("insertTestSymbol(dot-suffix dst) error = %v", err)
		}
	}
	// Noise symbols with single-dot or slash names (skipped by the strategy filters).
	for i := 0; i < numNoiseSyms; i++ {
		name := fmt.Sprintf("Noise_%d", i)
		qualified := fmt.Sprintf("noise.%s", name)
		fileID := fileIDs[i%len(fileIDs)]
		if _, err := insertTestSymbol(ctx, s, repoID, fileID, name, qualified); err != nil {
			b.Fatalf("insertTestSymbol(noise) error = %v", err)
		}
	}

	// Unresolved edges: dst_name has exactly two dots and no slash so it passes
	// the strategy's multi-dot pre-filter.
	for i := 0; i < numEdges; i++ {
		nameIdx := i % numNames
		dstName := fmt.Sprintf("a_%d.b_%d.c_%d", nameIdx%50, nameIdx%25, nameIdx)
		fileID := fileIDs[i%len(fileIDs)]
		srcID := srcIDs[i%len(srcIDs)]
		if _, err := insertTestEdge(ctx, s, repoID, fileID, srcID, dstName); err != nil {
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
		b.StartTimer()
		if _, err := s.resolveEdgesByDotSuffix(ctx, tx, repoID); err != nil {
			b.Fatalf("resolveEdgesByDotSuffix() error = %v", err)
		}
		b.StopTimer()
		_ = tx.Rollback()
	}
}

// BenchmarkResolveEdgesBySlashSuffix_NoUnresolved measures the steady-state
// floor cost when every edge is already resolved (no candidate dst_names for
// either suffix strategy). Captures the win from skipping the full symbols
// scan when the needed-set is empty.
func BenchmarkResolveEdgesBySlashSuffix_NoUnresolved(b *testing.B) {
	ctx := context.Background()
	s := openBenchStore(b)
	defer s.Close()

	repoID := upsertBenchRepo(ctx, b, s)

	const (
		numFiles     = 100
		numSyms      = 2000
		numNoiseSyms = 1000
	)

	fileIDs := makeBenchFiles(ctx, b, s, repoID, numFiles)
	_ = makeBenchSrcSymbols(ctx, b, s, repoID, fileIDs)
	for i := 0; i < numSyms; i++ {
		name := fmt.Sprintf("Func_%d", i)
		qualified := fmt.Sprintf("github.com/org/repo/pkg_%d/%s", i%50, name)
		fileID := fileIDs[i%len(fileIDs)]
		if _, err := insertTestSymbol(ctx, s, repoID, fileID, name, qualified); err != nil {
			b.Fatalf("insertTestSymbol() error = %v", err)
		}
	}
	for i := 0; i < numNoiseSyms; i++ {
		name := fmt.Sprintf("Noise_%d", i)
		qualified := fmt.Sprintf("noise.%s", name)
		fileID := fileIDs[i%len(fileIDs)]
		if _, err := insertTestSymbol(ctx, s, repoID, fileID, name, qualified); err != nil {
			b.Fatalf("insertTestSymbol(noise) error = %v", err)
		}
	}

	// Intentionally insert no unresolved edges: the SELECT DISTINCT dst_name
	// query returns zero rows, so neededSuffix and neededTail2 stay empty.

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			b.Fatalf("BeginTx() error = %v", err)
		}
		b.StartTimer()
		if _, err := s.resolveEdgesBySlashSuffix(ctx, tx, repoID); err != nil {
			b.Fatalf("resolveEdgesBySlashSuffix() error = %v", err)
		}
		b.StopTimer()
		_ = tx.Rollback()
	}
}

// openBenchStore opens a fresh sqlite store under b.TempDir().
func openBenchStore(b *testing.B) *Store {
	b.Helper()
	dbPath := filepath.Join(b.TempDir(), "graph.sqlite")
	s, err := Open(dbPath)
	if err != nil {
		b.Fatalf("Open() error = %v", err)
	}
	return s
}

func upsertBenchRepo(ctx context.Context, b *testing.B, s *Store) int64 {
	b.Helper()
	repo, err := s.UpsertRepo(ctx, b.TempDir())
	if err != nil {
		b.Fatalf("UpsertRepo() error = %v", err)
	}
	return repo.ID
}

func makeBenchFiles(ctx context.Context, b *testing.B, s *Store, repoID int64, n int) []int64 {
	b.Helper()
	out := make([]int64, 0, n)
	for i := 0; i < n; i++ {
		id, err := insertTestFile(ctx, s, repoID, fmt.Sprintf("f_%d.go", i))
		if err != nil {
			b.Fatalf("insertTestFile() error = %v", err)
		}
		out = append(out, id)
	}
	return out
}

func makeBenchSrcSymbols(ctx context.Context, b *testing.B, s *Store, repoID int64, fileIDs []int64) []int64 {
	b.Helper()
	out := make([]int64, 0, len(fileIDs))
	for i, fileID := range fileIDs {
		id, err := insertTestSymbol(ctx, s, repoID, fileID, fmt.Sprintf("Src_%d", i), fmt.Sprintf("Src_%d", i))
		if err != nil {
			b.Fatalf("insertTestSymbol(src) error = %v", err)
		}
		out = append(out, id)
	}
	return out
}
