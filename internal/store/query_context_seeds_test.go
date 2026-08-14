package store

import (
	"context"
	"path/filepath"
	"testing"
)

// seedRefsFixture holds two files that each declare a symbol named Renew, so the
// (file, qualified_name) join key has to tell them apart, plus a third file whose
// qualified name collides with the first file's package prefix.
func seedRefsFixture(t *testing.T) (*Store, int64) {
	t.Helper()
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	repo, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}

	billing, err := insertTestFile(ctx, s, repo.ID, "billing/renew.go")
	if err != nil {
		t.Fatalf("insertTestFile() error = %v", err)
	}
	subscription, err := insertTestFile(ctx, s, repo.ID, "subscription/renew.go")
	if err != nil {
		t.Fatalf("insertTestFile() error = %v", err)
	}
	if _, err := insertTestSymbol(ctx, s, repo.ID, billing, "Renew", "billing.Renew"); err != nil {
		t.Fatalf("insertTestSymbol() error = %v", err)
	}
	if _, err := insertTestSymbol(ctx, s, repo.ID, subscription, "Renew", "subscription.Renew"); err != nil {
		t.Fatalf("insertTestSymbol() error = %v", err)
	}
	if _, err := insertTestSymbol(ctx, s, repo.ID, billing, "Charge", "billing.Charge"); err != nil {
		t.Fatalf("insertTestSymbol() error = %v", err)
	}
	return s, repo.ID
}

func TestSymbolsForRefsResolvesExactPairs(t *testing.T) {
	ctx := context.Background()
	s, repoID := seedRefsFixture(t)

	got, err := s.SymbolsForRefs(ctx, repoID, []SymbolRef{
		{File: "billing/renew.go", QualifiedName: "billing.Renew"},
		{File: "subscription/renew.go", QualifiedName: "subscription.Renew"},
	})
	if err != nil {
		t.Fatalf("SymbolsForRefs() error = %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("resolved %d refs, want 2: %+v", len(got), got)
	}
	billing := got[SymbolRef{File: "billing/renew.go", QualifiedName: "billing.Renew"}]
	subscription := got[SymbolRef{File: "subscription/renew.go", QualifiedName: "subscription.Renew"}]
	if billing.ID == 0 || subscription.ID == 0 || billing.ID == subscription.ID {
		t.Fatalf("same-name symbols did not resolve to distinct rows: %+v / %+v", billing, subscription)
	}
	if billing.FilePath != "billing/renew.go" || subscription.FilePath != "subscription/renew.go" {
		t.Fatalf("file paths crossed over: %+v / %+v", billing, subscription)
	}
	if billing.StableKey == "" {
		t.Fatalf("resolved symbol carries no stable key: %+v", billing)
	}
}

// The batched query uses two IN lists, whose cross product over-selects. A ref
// the caller never asked for must not appear in the result.
func TestSymbolsForRefsDoesNotReturnCrossProduct(t *testing.T) {
	ctx := context.Background()
	s, repoID := seedRefsFixture(t)

	got, err := s.SymbolsForRefs(ctx, repoID, []SymbolRef{
		{File: "billing/renew.go", QualifiedName: "billing.Renew"},
		{File: "subscription/renew.go", QualifiedName: "billing.Charge"}, // no such pair
	})
	if err != nil {
		t.Fatalf("SymbolsForRefs() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("resolved %d refs, want 1: %+v", len(got), got)
	}
	if _, ok := got[SymbolRef{File: "billing/renew.go", QualifiedName: "billing.Charge"}]; ok {
		t.Fatal("a pair the caller did not ask for was returned")
	}
}

func TestSymbolsForRefsIgnoresIncompleteRefs(t *testing.T) {
	ctx := context.Background()
	s, repoID := seedRefsFixture(t)

	got, err := s.SymbolsForRefs(ctx, repoID, []SymbolRef{
		{File: "", QualifiedName: "billing.Renew"},
		{File: "billing/renew.go", QualifiedName: ""},
	})
	if err != nil {
		t.Fatalf("SymbolsForRefs() error = %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("resolved %d refs, want 0: %+v", len(got), got)
	}
	empty, err := s.SymbolsForRefs(ctx, repoID, nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("SymbolsForRefs(nil) = %v, %v", empty, err)
	}
}

func TestSymbolNameCountsFindsAmbiguity(t *testing.T) {
	ctx := context.Background()
	s, repoID := seedRefsFixture(t)

	counts, err := s.SymbolNameCounts(ctx, repoID, []string{"Renew", "Charge", "Absent", ""})
	if err != nil {
		t.Fatalf("SymbolNameCounts() error = %v", err)
	}
	if counts["Renew"] != 2 {
		t.Fatalf("Renew count = %d, want 2", counts["Renew"])
	}
	if counts["Charge"] != 1 {
		t.Fatalf("Charge count = %d, want 1", counts["Charge"])
	}
	if _, ok := counts["Absent"]; ok {
		t.Fatalf("absent name reported: %v", counts)
	}
}

func TestLastScanIDTracksScans(t *testing.T) {
	ctx := context.Background()
	s, repoID := seedRefsFixture(t)

	id, err := s.LastScanID(ctx, repoID)
	if err != nil {
		t.Fatalf("LastScanID() error = %v", err)
	}
	if id != 0 {
		t.Fatalf("LastScanID() = %d for a repo with no scans, want 0", id)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO scans(repo_id, scan_kind, started_at, status) VALUES(?, 'full', '', 'ok')`, repoID); err != nil {
		t.Fatalf("insert scan error = %v", err)
	}
	first, err := s.LastScanID(ctx, repoID)
	if err != nil || first == 0 {
		t.Fatalf("LastScanID() = %d, %v; want a non-zero id", first, err)
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO scans(repo_id, scan_kind, started_at, status) VALUES(?, 'full', '', 'ok')`, repoID); err != nil {
		t.Fatalf("insert scan error = %v", err)
	}
	second, err := s.LastScanID(ctx, repoID)
	if err != nil {
		t.Fatalf("LastScanID() error = %v", err)
	}
	if second <= first {
		t.Fatalf("LastScanID() = %d after a second scan, want > %d", second, first)
	}
}

// Two rows for one (file, qualified_name) pair: the pick is deterministic and
// never the row that cannot be drilled into.
func TestSymbolsForRefsPrefersTheRowWithAStableKey(t *testing.T) {
	ctx := context.Background()
	s, repoID := seedRefsFixture(t)
	fileID, err := insertTestFile(ctx, s, repoID, "overload/pair.go")
	if err != nil {
		t.Fatalf("insertTestFile() error = %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO symbols(repo_id, file_id, language, kind, name, qualified_name, container_name,
			start_line, start_col, end_line, end_col, stable_key)
		VALUES(?, ?, 'go', 'function', 'Pair', 'overload.Pair', '', 10, 1, 12, 1, '')
	`, repoID, fileID); err != nil {
		t.Fatalf("insert unkeyed symbol error = %v", err)
	}
	if _, err := insertTestSymbol(ctx, s, repoID, fileID, "Pair", "overload.Pair"); err != nil {
		t.Fatalf("insertTestSymbol() error = %v", err)
	}

	ref := SymbolRef{File: "overload/pair.go", QualifiedName: "overload.Pair"}
	for i := 0; i < 5; i++ {
		got, err := s.SymbolsForRefs(ctx, repoID, []SymbolRef{ref})
		if err != nil {
			t.Fatalf("SymbolsForRefs() error = %v", err)
		}
		sym, ok := got[ref]
		if !ok {
			t.Fatalf("ref did not resolve: %+v", got)
		}
		if sym.StableKey == "" {
			t.Fatalf("run %d chose the row without a stable key: %+v", i, sym)
		}
	}
}
