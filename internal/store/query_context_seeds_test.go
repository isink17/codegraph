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

// Persisted file paths are canonical. A canonical ref addresses that row, and
// the result remains keyed canonically when input uses the host separator.
func TestSymbolsForRefsMatchesCanonicalStoredPaths(t *testing.T) {
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

	storedPath := "billing/renew.go"
	fileID, err := insertTestFile(ctx, s, repo.ID, storedPath)
	if err != nil {
		t.Fatalf("insertTestFile(%q) error = %v", storedPath, err)
	}
	if _, err := insertTestSymbol(ctx, s, repo.ID, fileID, "Renew", "billing.Renew"); err != nil {
		t.Fatalf("insertTestSymbol() error = %v", err)
	}

	canonical := SymbolRef{File: "billing/renew.go", QualifiedName: "billing.Renew"}
	got, err := s.SymbolsForRefs(ctx, repo.ID, []SymbolRef{canonical})
	if err != nil {
		t.Fatalf("SymbolsForRefs() error = %v", err)
	}
	sym, ok := got[canonical]
	if !ok {
		t.Fatalf("canonical ref did not resolve a row stored as %q: %+v", storedPath, got)
	}
	if sym.FilePath != canonical.File {
		t.Fatalf("resolved symbol path = %q, want the canonical %q", sym.FilePath, canonical.File)
	}

	// And the same ref supplied in the stored (native) form resolves the same row,
	// still keyed canonically.
	native := SymbolRef{File: storedPath, QualifiedName: canonical.QualifiedName}
	fromNative, err := s.SymbolsForRefs(ctx, repo.ID, []SymbolRef{native})
	if err != nil {
		t.Fatalf("SymbolsForRefs(native) error = %v", err)
	}
	if fromNative[canonical].ID != sym.ID {
		t.Fatalf("native ref resolved %+v, want symbol %d", fromNative, sym.ID)
	}
}

func TestSymbolsForRefsPreservesLiteralBackslashResultKey(t *testing.T) {
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

	ref := SymbolRef{File: `pkg/weird\name.go`, QualifiedName: "pkg.Weird"}
	fileID, err := insertTestFile(ctx, s, repo.ID, ref.File)
	if err != nil {
		t.Fatalf("insertTestFile(%q) error = %v", ref.File, err)
	}
	if _, err := insertTestSymbol(ctx, s, repo.ID, fileID, "Weird", ref.QualifiedName); err != nil {
		t.Fatalf("insertTestSymbol() error = %v", err)
	}
	// The slash sibling is a second stored file declaring the same qualified
	// name. scanSymbol's former filepath.ToSlash rewrote the backslash row's
	// FilePath onto this spelling on Windows, so the backslash ref was dropped
	// from the result and the slash ref could be answered with the wrong row.
	// On POSIX ToSlash is the identity, so the old code passes there too.
	sibling := SymbolRef{File: "pkg/weird/name.go", QualifiedName: ref.QualifiedName}
	siblingFileID, err := insertTestFile(ctx, s, repo.ID, sibling.File)
	if err != nil {
		t.Fatalf("insertTestFile(%q) error = %v", sibling.File, err)
	}
	if _, err := insertTestSymbol(ctx, s, repo.ID, siblingFileID, "Weird", sibling.QualifiedName); err != nil {
		t.Fatalf("insertTestSymbol() error = %v", err)
	}

	got, err := s.SymbolsForRefs(ctx, repo.ID, []SymbolRef{ref})
	if err != nil {
		t.Fatalf("SymbolsForRefs() error = %v", err)
	}
	sym, ok := got[ref]
	if !ok {
		t.Fatalf("literal-backslash ref missing from result: %+v", got)
	}
	if sym.FilePath != ref.File || sym.FileID != fileID {
		t.Fatalf("literal-backslash ref returned FilePath %q file %d, want %q file %d", sym.FilePath, sym.FileID, ref.File, fileID)
	}
	if _, ok := got[sibling]; ok {
		t.Fatalf("literal-backslash path acquired a slash alias: %+v", got)
	}

	both, err := s.SymbolsForRefs(ctx, repo.ID, []SymbolRef{ref, sibling})
	if err != nil {
		t.Fatalf("SymbolsForRefs() error = %v", err)
	}
	if got := both[ref]; got.FileID != fileID || got.FilePath != ref.File {
		t.Fatalf("backslash ref = file %d %q, want file %d %q", got.FileID, got.FilePath, fileID, ref.File)
	}
	if got := both[sibling]; got.FileID != siblingFileID || got.FilePath != sibling.File {
		t.Fatalf("slash sibling ref = file %d %q, want file %d %q", got.FileID, got.FilePath, siblingFileID, sibling.File)
	}
}

// Canonicalization must not widen what a ref addresses: a different repository,
// a parent-directory escape, or a case variant stays unresolved.
func TestSymbolsForRefsDoesNotWidenPathMatching(t *testing.T) {
	ctx := context.Background()
	s, repoID := seedRefsFixture(t)
	other, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}

	ref := SymbolRef{File: "billing/renew.go", QualifiedName: "billing.Renew"}
	if got, err := s.SymbolsForRefs(ctx, other.ID, []SymbolRef{ref}); err != nil || len(got) != 0 {
		t.Fatalf("ref resolved against another repo: %v, %v", got, err)
	}
	for _, bad := range []string{"../billing/renew.go", "BILLING/renew.go", "/billing/renew.go"} {
		got, err := s.SymbolsForRefs(ctx, repoID, []SymbolRef{{File: bad, QualifiedName: "billing.Renew"}})
		if err != nil {
			t.Fatalf("SymbolsForRefs(%q) error = %v", bad, err)
		}
		if len(got) != 0 {
			t.Fatalf("path %q resolved to %v; canonicalization must not widen matching", bad, got)
		}
	}
}

// A ref names one stored file. `a/x/y.go` and `a/x\y.go` are two logical
// identities under P23, and a leading "./" is a third spelling that addresses
// no row stored without it. The former CanonicalRelPath call collapsed the
// backslash pair on Windows and stripped "./" on every host, so a ref built
// from `files.path` -- which is what the only production caller hands over --
// resolved the wrong row or none at all.
func TestSymbolsForRefsMatchesStoredPathIdentityExactly(t *testing.T) {
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

	rows := []struct {
		path string
		name string
	}{
		{"a/x/y.go", "a.Slash"},
		{`a/x\y.go`, "a.Backslash"},
		{"./b.go", "b.Dotted"},
		{"b.go", "b.Plain"},
	}
	ids := map[string]int64{}
	for _, r := range rows {
		fileID, err := insertTestFile(ctx, s, repo.ID, r.path)
		if err != nil {
			t.Fatalf("insertTestFile(%q) error = %v", r.path, err)
		}
		symID, err := insertTestSymbol(ctx, s, repo.ID, fileID, r.name, r.name)
		if err != nil {
			t.Fatalf("insertTestSymbol(%q) error = %v", r.name, err)
		}
		ids[r.path] = symID
	}

	for _, r := range rows {
		ref := SymbolRef{File: r.path, QualifiedName: r.name}
		got, err := s.SymbolsForRefs(ctx, repo.ID, []SymbolRef{ref})
		if err != nil {
			t.Fatalf("SymbolsForRefs(%q) error = %v", r.path, err)
		}
		if len(got) != 1 {
			t.Fatalf("ref %q resolved %d rows, want exactly 1: %+v", r.path, len(got), got)
		}
		sym, ok := got[ref]
		if !ok {
			t.Fatalf("ref %q is not the result key: %+v", r.path, got)
		}
		if sym.ID != ids[r.path] {
			t.Fatalf("ref %q resolved symbol %d, want %d", r.path, sym.ID, ids[r.path])
		}
		if sym.FilePath != r.path {
			t.Fatalf("ref %q returned path %q; stored bytes must survive", r.path, sym.FilePath)
		}
	}

	// Negative control: the symbol names are distinct per row, so a path folded
	// onto its sibling cannot resolve at all. Asking for the backslash file's
	// symbol under the slash spelling (and the reverse) must stay empty, and the
	// same for the "./" pair -- the local POSIX reverse-failure of the old code.
	for _, bad := range []SymbolRef{
		{File: "a/x/y.go", QualifiedName: "a.Backslash"},
		{File: `a/x\y.go`, QualifiedName: "a.Slash"},
		{File: "b.go", QualifiedName: "b.Dotted"},
		{File: "./b.go", QualifiedName: "b.Plain"},
	} {
		got, err := s.SymbolsForRefs(ctx, repo.ID, []SymbolRef{bad})
		if err != nil {
			t.Fatalf("SymbolsForRefs(%+v) error = %v", bad, err)
		}
		if len(got) != 0 {
			t.Fatalf("ref %+v crossed a path identity boundary: %+v", bad, got)
		}
	}
}
