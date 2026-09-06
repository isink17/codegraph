package store

import (
	"context"
	"fmt"
	"testing"
)

// TestCppEvidenceCandidatesSurviveNameBatching pins the two-column IN list this
// package chunks: one symbol can match on `name` in one batch and on
// `qualified_name` in another. Counting it twice makes the unique-candidate
// rule read the edge as ambiguous, so the resolution disappears once the name
// set grows past a single batch.
func TestCppEvidenceCandidatesSurviveNameBatching(t *testing.T) {
	ctx := context.Background()
	resolved := func(fillerNames int) int {
		s, repo := openBudgetStore(t)
		owner, err := insertTestFileLang(ctx, s, repo.ID, "src/a.cpp", "cpp")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := insertTestSymbolKind(ctx, s, repo.ID, owner, "foo", "A::foo", "function", "A", "cpp"); err != nil {
			t.Fatal(err)
		}
		src, err := insertTestSymbolKind(ctx, s, repo.ID, owner, "caller", "caller", "function", "", "cpp")
		if err != nil {
			t.Fatal(err)
		}
		targets := []edgeTarget{}
		// The definition is reachable from two entries of the name set: its bare
		// `name` and its `qualified_name`. Sorted, those two spellings land in
		// different batches once the set is wide enough.
		for _, name := range []string{"foo", "A::foo"} {
			id, err := insertTestEdge(ctx, s, repo.ID, owner, src, name)
			if err != nil {
				t.Fatal(err)
			}
			targets = append(targets, edgeTarget{
				edgeID: id, srcLanguage: "cpp", srcFileID: owner, dstName: name, evidence: "call",
			})
		}
		// Filler names widen the set until the two spellings land in different
		// batches of the chunked two-column IN list.
		for i := range fillerNames {
			id, err := insertTestEdge(ctx, s, repo.ID, owner, src, fmt.Sprintf("absent%04d", i))
			if err != nil {
				t.Fatal(err)
			}
			targets = append(targets, edgeTarget{
				edgeID: id, srcLanguage: "cpp", srcFileID: owner, dstName: fmt.Sprintf("absent%04d", i), evidence: "call",
			})
		}
		n, err := s.resolveCppEvidenceEdges(ctx, repo.ID, targets)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	small := resolved(0)
	if small == 0 {
		t.Fatal("the one-batch pass resolved nothing; the fixture proves nothing")
	}
	if large := resolved(1200); large != small {
		t.Fatalf("one batch resolved %d edges, several batches %d", small, large)
	}
}
