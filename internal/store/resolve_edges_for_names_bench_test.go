package store

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
)

func BenchmarkResolveEdgesForNames_CrossFileScale(b *testing.B) {
	ctx := context.Background()
	dbPath := filepath.Join(b.TempDir(), "graph.sqlite")
	s, err := Open(dbPath)
	if err != nil {
		b.Fatalf("Open() error = %v", err)
	}
	defer s.Close()

	repo, err := s.UpsertRepo(ctx, b.TempDir())
	if err != nil {
		b.Fatalf("UpsertRepo() error = %v", err)
	}

	const (
		numFiles  = 200
		numNames  = 1500
		numEdges  = 20000
		numPkgs   = 50
		dstPerPkg = numNames / numPkgs
	)

	fileIDs := make([]int64, 0, numFiles)
	for i := 0; i < numFiles; i++ {
		id, err := insertTestFile(ctx, s, repo.ID, fmt.Sprintf("f_%d.go", i))
		if err != nil {
			b.Fatalf("insertTestFile() error = %v", err)
		}
		fileIDs = append(fileIDs, id)
	}

	// One src symbol per file to attach edges to.
	srcIDs := make([]int64, 0, numFiles)
	for i, fileID := range fileIDs {
		id, err := insertTestSymbol(ctx, s, repo.ID, fileID, fmt.Sprintf("Src_%d", i), fmt.Sprintf("Src_%d", i))
		if err != nil {
			b.Fatalf("insertTestSymbol(src) error = %v", err)
		}
		srcIDs = append(srcIDs, id)
	}

	// Dst symbols spread across files/packages with stable qualified names.
	for i := 0; i < numNames; i++ {
		pkg := i % numPkgs
		name := fmt.Sprintf("Name_%d", i)
		qualified := fmt.Sprintf("pkg_%d.%s", pkg, name)
		fileID := fileIDs[i%len(fileIDs)]
		if _, err := insertTestSymbol(ctx, s, repo.ID, fileID, name, qualified); err != nil {
			b.Fatalf("insertTestSymbol(dst) error = %v", err)
		}
	}

	for i := 0; i < numEdges; i++ {
		pkg := i % numPkgs
		nameIdx := (i % dstPerPkg) + (pkg * dstPerPkg)
		if nameIdx >= numNames {
			nameIdx = i % numNames
		}
		dstName := fmt.Sprintf("pkg_%d.Name_%d", pkg, nameIdx)
		fileID := fileIDs[i%len(fileIDs)]
		srcID := srcIDs[i%len(srcIDs)]
		if _, err := insertTestEdge(ctx, s, repo.ID, fileID, srcID, dstName); err != nil {
			b.Fatalf("insertTestEdge() error = %v", err)
		}
	}

	names := make([]string, 0, numNames+(numNames/10))
	for i := 0; i < numNames; i++ {
		names = append(names, fmt.Sprintf("Name_%d", i))
	}
	// Add some duplicates/whitespace to exercise dedupe/trim without changing semantics.
	for i := 0; i < numNames/10; i++ {
		names = append(names, " "+fmt.Sprintf("Name_%d", i)+" ")
	}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		if _, err := s.db.ExecContext(ctx, `UPDATE edges SET dst_symbol_id = NULL WHERE repo_id = ?`, repo.ID); err != nil {
			b.Fatalf("reset edges error = %v", err)
		}
		b.StartTimer()

		if _, err := s.ResolveEdgesForNamesWithStats(ctx, repo.ID, names); err != nil {
			b.Fatalf("ResolveEdgesForNamesWithStats() error = %v", err)
		}
	}
}
