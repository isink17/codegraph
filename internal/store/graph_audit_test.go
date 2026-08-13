package store

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// Tests for the P9 graph audit checks.
//
// Several of the states asserted here cannot be produced through the public
// API any more: the unbind paths added in P5/P6 clear resolution metadata
// whenever they drop a destination, so a "resolved" flag with no destination is
// unreachable through Store methods. That is precisely why direct SQL setup is
// the right fixture for these tests -- the audit exists to detect legacy and
// corrupt databases, and a fixture restricted to states current code can still
// produce would test none of them.

// auditFixture returns an open store with one repository and its capabilities.
func auditFixture(t *testing.T) (context.Context, *Store, int64, GraphAuditCapabilities) {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "graph.sqlite")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { s.Close() })

	repo, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	caps, err := s.GraphAuditCapabilitiesFor(ctx)
	if err != nil {
		t.Fatalf("GraphAuditCapabilitiesFor() error = %v", err)
	}
	if !caps.HasResolutionMetadata {
		t.Fatalf("a freshly migrated database must expose resolution metadata; caps = %+v", caps)
	}
	return ctx, s, repo.ID, caps
}

// bindEdge sets a destination and its provenance directly, bypassing the
// resolver. Audit must judge the row as persisted, not as the resolver would
// have written it.
func bindEdge(t *testing.T, ctx context.Context, s *Store, edgeID, dstSymbolID int64, strategy, confidence string) {
	t.Helper()
	if _, err := s.db.ExecContext(ctx, `
		UPDATE edges SET dst_symbol_id = ?, resolution_strategy = ?, resolution_confidence = ?
		WHERE id = ?
	`, dstSymbolID, strategy, confidence, edgeID); err != nil {
		t.Fatalf("bind edge %d: %v", edgeID, err)
	}
}

func runCheck(t *testing.T, ctx context.Context, s *Store, repoID int64, check EdgeAuditCheck, caps GraphAuditCapabilities, limit int) EdgeAuditResult {
	t.Helper()
	res, err := s.RunEdgeAuditCheck(ctx, repoID, check, caps, limit)
	if err != nil {
		t.Fatalf("RunEdgeAuditCheck(%v) error = %v", check, err)
	}
	return res
}

// TestAuditCleanGraphHasNoFindings pins the baseline: a graph built the way the
// indexer builds one trips no check. Without this, every other test here could
// pass while the checks fired on everything.
func TestAuditCleanGraphHasNoFindings(t *testing.T) {
	ctx, s, repoID, caps := auditFixture(t)

	fileID, err := insertTestFile(ctx, s, repoID, "a.go")
	if err != nil {
		t.Fatalf("insertTestFile: %v", err)
	}
	src, err := insertTestSymbol(ctx, s, repoID, fileID, "Caller", "pkg/a.Caller")
	if err != nil {
		t.Fatalf("insertTestSymbol(src): %v", err)
	}
	dst, err := insertTestSymbol(ctx, s, repoID, fileID, "Helper", "pkg/a.Helper")
	if err != nil {
		t.Fatalf("insertTestSymbol(dst): %v", err)
	}
	resolved, err := insertTestEdge(ctx, s, repoID, fileID, src, "pkg/a.Helper")
	if err != nil {
		t.Fatalf("insertTestEdge(resolved): %v", err)
	}
	bindEdge(t, ctx, s, resolved, dst, ResolutionStrategyExactQualified, ResolutionConfidenceHigh)
	if _, err := insertTestEdge(ctx, s, repoID, fileID, src, "fmt.Println"); err != nil {
		t.Fatalf("insertTestEdge(unresolved): %v", err)
	}

	for _, check := range []EdgeAuditCheck{
		EdgeCheckDanglingTarget,
		EdgeCheckDanglingSource,
		EdgeCheckInvalidResolutionMetadata,
		EdgeCheckImplicitCrossLanguage,
		EdgeCheckResolvedTargetDeletedFile,
		EdgeCheckResolvedMissingMetadata,
		EdgeCheckLowConfidenceResolution,
		EdgeCheckCrossLanguageImplicitStrategy,
	} {
		if got := runCheck(t, ctx, s, repoID, check, caps, 5); got.Count != 0 {
			t.Errorf("check %v on a clean graph = %d violations, want 0 (examples: %+v)", check, got.Count, got.Examples)
		}
	}

	links, err := s.RunDanglingTestLinkCheck(ctx, repoID, 5)
	if err != nil {
		t.Fatalf("RunDanglingTestLinkCheck: %v", err)
	}
	if links.Count != 0 {
		t.Errorf("dangling test links on a clean graph = %d, want 0", links.Count)
	}
}

func TestAuditDetectsDanglingEdgeTarget(t *testing.T) {
	ctx, s, repoID, caps := auditFixture(t)

	fileID, _ := insertTestFile(ctx, s, repoID, "a.go")
	src, _ := insertTestSymbol(ctx, s, repoID, fileID, "Caller", "pkg/a.Caller")
	edgeID, err := insertTestEdge(ctx, s, repoID, fileID, src, "Gone")
	if err != nil {
		t.Fatalf("insertTestEdge: %v", err)
	}
	// Bind to a symbol id that was never inserted.
	bindEdge(t, ctx, s, edgeID, 999999, ResolutionStrategyExactName, ResolutionConfidenceHigh)

	got := runCheck(t, ctx, s, repoID, EdgeCheckDanglingTarget, caps, 5)
	if got.Count != 1 {
		t.Fatalf("dangling target count = %d, want 1", got.Count)
	}
	if len(got.Examples) != 1 || got.Examples[0].EdgeID != edgeID {
		t.Fatalf("examples = %+v, want the one edge %d", got.Examples, edgeID)
	}
	if got.Examples[0].DstName != "Gone" {
		t.Errorf("example dst_name = %q, want %q", got.Examples[0].DstName, "Gone")
	}
	// A purged destination must not also be reported as a soft-deleted one.
	if other := runCheck(t, ctx, s, repoID, EdgeCheckResolvedTargetDeletedFile, caps, 5); other.Count != 0 {
		t.Errorf("deleted-file check also fired (%d); the two checks must be disjoint", other.Count)
	}
}

func TestAuditDetectsDanglingEdgeSource(t *testing.T) {
	ctx, s, repoID, caps := auditFixture(t)

	fileID, _ := insertTestFile(ctx, s, repoID, "a.go")
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line)
		VALUES(?, 424242, NULL, 'X', 'call', '', ?, 1)
	`, repoID, fileID); err != nil {
		t.Fatalf("insert orphan-source edge: %v", err)
	}

	got := runCheck(t, ctx, s, repoID, EdgeCheckDanglingSource, caps, 5)
	if got.Count != 1 {
		t.Fatalf("dangling source count = %d, want 1", got.Count)
	}
	if len(got.Examples) != 1 {
		t.Fatalf("examples = %+v, want 1", got.Examples)
	}
	// The source symbol is gone, so its name cannot be reported; the edge's own
	// columns still identify it.
	if got.Examples[0].DstName != "X" || got.Examples[0].SrcSymbol != "" {
		t.Errorf("example = %+v, want dst_name X and an empty src_symbol", got.Examples[0])
	}
	// The destination is NULL, so the target check must stay silent: the two
	// dangling checks address different columns.
	if other := runCheck(t, ctx, s, repoID, EdgeCheckDanglingTarget, caps, 5); other.Count != 0 {
		t.Errorf("dangling-target check fired on an unresolved edge: count = %d, want 0", other.Count)
	}
}

// TestAuditFlagsCrossRepoBinding covers the shared-database case: an edge bound
// to a symbol owned by another repository is a broken binding. Without
// repo-scoped joins the row would join successfully and read as healthy.
func TestAuditFlagsCrossRepoBinding(t *testing.T) {
	ctx, s, repoID, caps := auditFixture(t)

	other, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("UpsertRepo(other): %v", err)
	}
	otherFile, _ := insertTestFile(ctx, s, other.ID, "other.go")
	foreign, _ := insertTestSymbol(ctx, s, other.ID, otherFile, "Helper", "other.Helper")

	fileID, _ := insertTestFile(ctx, s, repoID, "a.go")
	src, _ := insertTestSymbol(ctx, s, repoID, fileID, "Caller", "pkg/a.Caller")
	edgeID, _ := insertTestEdge(ctx, s, repoID, fileID, src, "Helper")
	// A real symbol row, but one this repository does not own.
	bindEdge(t, ctx, s, edgeID, foreign, ResolutionStrategyExactName, ResolutionConfidenceHigh)

	if got := runCheck(t, ctx, s, repoID, EdgeCheckDanglingTarget, caps, 5); got.Count != 1 {
		t.Fatalf("cross-repo binding count = %d, want 1: the destination is not reachable within this repository", got.Count)
	}
}

// TestAuditToleratesFileLanguageDrift pins the false positive the file-language
// comparison would have caused. TouchFilesMetadataBatch rewrites files.language
// without re-parsing, so files.language can legitimately lead symbols.language
// after an extension-mapping change. That must not read as a resolver bypass.
func TestAuditToleratesFileLanguageDrift(t *testing.T) {
	ctx, s, repoID, caps := auditFixture(t)

	fileID, _ := insertTestFileLang(ctx, s, repoID, "a.c", "c")
	src, _ := insertTestSymbolLang(ctx, s, repoID, fileID, "caller", "a.caller", "c")
	dst, _ := insertTestSymbolLang(ctx, s, repoID, fileID, "helper", "a.helper", "c")
	edgeID, _ := insertTestEdge(ctx, s, repoID, fileID, src, "helper")
	bindEdge(t, ctx, s, edgeID, dst, ResolutionStrategyExactName, ResolutionConfidenceHigh)

	// The release now classifies this extension as cpp. The file row is
	// refreshed; its symbols are not re-parsed.
	if _, err := s.db.ExecContext(ctx, `UPDATE files SET language = 'cpp' WHERE id = ?`, fileID); err != nil {
		t.Fatalf("rewrite file language: %v", err)
	}

	if got := runCheck(t, ctx, s, repoID, EdgeCheckImplicitCrossLanguage, caps, 5); got.Count != 0 {
		t.Fatalf("file-language drift reported as an implicit cross-language binding: count = %d, want 0 (examples: %+v)", got.Count, got.Examples)
	}
}

// TestDistributionsBucketUnregisteredValues keeps the distributions bounded on
// the corrupt databases this command exists to describe: arbitrary strategy
// strings must fold into one key, not one key each.
func TestDistributionsBucketUnregisteredValues(t *testing.T) {
	ctx, s, repoID, caps := auditFixture(t)

	fileID, _ := insertTestFile(ctx, s, repoID, "a.go")
	src, _ := insertTestSymbol(ctx, s, repoID, fileID, "Caller", "pkg/a.Caller")
	dst, _ := insertTestSymbol(ctx, s, repoID, fileID, "Helper", "pkg/a.Helper")
	for i, garbage := range []string{"telepathy", "vibes", "coin_flip", "astrology"} {
		edgeID, err := insertTestEdge(ctx, s, repoID, fileID, src, "Helper")
		if err != nil {
			t.Fatalf("insertTestEdge(%d): %v", i, err)
		}
		bindEdge(t, ctx, s, edgeID, dst, garbage, "vibes")
	}
	// One legitimately bound edge, to prove real keys survive.
	good, _ := insertTestEdge(ctx, s, repoID, fileID, src, "Helper")
	bindEdge(t, ctx, s, good, dst, ResolutionStrategyExactName, ResolutionConfidenceHigh)

	strategies, err := s.ResolutionStrategyDistribution(ctx, repoID, caps)
	if err != nil {
		t.Fatalf("ResolutionStrategyDistribution: %v", err)
	}
	if strategies[DistributionUnregisteredKey] != 4 {
		t.Errorf("unregistered strategies = %d under %q, want 4 (got %+v)", strategies[DistributionUnregisteredKey], DistributionUnregisteredKey, strategies)
	}
	if strategies[ResolutionStrategyExactName] != 1 {
		t.Errorf("registered strategy count = %d, want 1 (got %+v)", strategies[ResolutionStrategyExactName], strategies)
	}
	if len(strategies) != 2 {
		t.Errorf("distribution has %d keys, want 2; unregistered values are not bounded: %+v", len(strategies), strategies)
	}

	confidences, err := s.ResolutionConfidenceDistribution(ctx, repoID, caps)
	if err != nil {
		t.Fatalf("ResolutionConfidenceDistribution: %v", err)
	}
	if confidences[DistributionUnregisteredKey] != 4 {
		t.Errorf("unregistered confidences = %d, want 4 (got %+v)", confidences[DistributionUnregisteredKey], confidences)
	}
}

// TestOpenReadOnlyWorksOnNonWALDatabase covers the journal-mode trap: applying
// journal_mode(WAL) needs a write, so a rollback-journal database (what
// VACUUM INTO and sqlite3 .backup produce) must not be opened with that pragma.
func TestOpenReadOnlyWorksOnNonWALDatabase(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "graph.sqlite")

	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	repo, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	if _, err := s.db.ExecContext(ctx, `PRAGMA journal_mode=DELETE`); err != nil {
		t.Fatalf("switch to rollback journal: %v", err)
	}
	s.Close()

	ro, err := OpenReadOnly(dbPath, OpenOptions{})
	if err != nil {
		t.Fatalf("OpenReadOnly() on a non-WAL database error = %v", err)
	}
	defer ro.Close()
	if _, _, err := ro.FindRepo(ctx, repo.RootPath); err != nil {
		t.Fatalf("read from a non-WAL read-only handle error = %v", err)
	}
}

func TestAuditDetectsDanglingTestLinkReference(t *testing.T) {
	ctx, s, repoID, _ := auditFixture(t)

	testFile, _ := insertTestFile(ctx, s, repoID, "a_test.go")
	testSym, _ := insertTestSymbol(ctx, s, repoID, testFile, "TestA", "pkg/a.TestA")
	// target_symbol_id and target_file_id both point at rows that do not exist.
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO test_links(repo_id, test_file_id, test_symbol_id, target_file_id, target_symbol_id, reason, score)
		VALUES(?, ?, ?, 777777, 888888, 'name_match', 0.9)
	`, repoID, testFile, testSym); err != nil {
		t.Fatalf("insert dangling test_link: %v", err)
	}

	got, err := s.RunDanglingTestLinkCheck(ctx, repoID, 5)
	if err != nil {
		t.Fatalf("RunDanglingTestLinkCheck: %v", err)
	}
	if got.Count != 1 {
		t.Fatalf("dangling test link count = %d, want 1", got.Count)
	}
	if len(got.Examples) != 1 || got.Examples[0].Detail == "" {
		t.Fatalf("examples = %+v, want one example carrying a detail", got.Examples)
	}
}

// TestAuditIgnoresNulledTestLinkReferences guards the most likely false
// positive: the unbind paths null these columns on purpose, and a NULL is not a
// dangling reference.
func TestAuditIgnoresNulledTestLinkReferences(t *testing.T) {
	ctx, s, repoID, _ := auditFixture(t)

	testFile, _ := insertTestFile(ctx, s, repoID, "a_test.go")
	testSym, _ := insertTestSymbol(ctx, s, repoID, testFile, "TestA", "pkg/a.TestA")
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO test_links(repo_id, test_file_id, test_symbol_id, target_file_id, target_symbol_id, reason, score)
		VALUES(?, ?, ?, NULL, NULL, 'name_match', 0.9)
	`, repoID, testFile, testSym); err != nil {
		t.Fatalf("insert unbound test_link: %v", err)
	}

	got, err := s.RunDanglingTestLinkCheck(ctx, repoID, 5)
	if err != nil {
		t.Fatalf("RunDanglingTestLinkCheck: %v", err)
	}
	if got.Count != 0 {
		t.Fatalf("unbound (NULL) test link reported as dangling: count = %d, want 0", got.Count)
	}
}

// TestAuditDetectsInvalidResolutionMetadata covers all four impossible states
// in one table, and asserts the detail label so a consumer can tell them apart.
func TestAuditDetectsInvalidResolutionMetadata(t *testing.T) {
	cases := []struct {
		name       string
		resolve    bool
		strategy   string
		confidence string
		wantDetail string
	}{
		{
			name:       "unresolved edge carrying provenance",
			resolve:    false,
			strategy:   ResolutionStrategyExactName,
			confidence: ResolutionConfidenceHigh,
			wantDetail: "unresolved_edge_carries_resolution_metadata",
		},
		{
			name:       "unregistered strategy",
			resolve:    true,
			strategy:   "telepathy",
			confidence: ResolutionConfidenceHigh,
			wantDetail: "unknown_resolution_strategy",
		},
		{
			name:       "confidence disagrees with strategy",
			resolve:    true,
			strategy:   ResolutionStrategyExactQualified,
			confidence: ResolutionConfidenceLow,
			wantDetail: "confidence_does_not_match_strategy",
		},
		{
			name:       "strategy without confidence",
			resolve:    true,
			strategy:   ResolutionStrategyExactName,
			confidence: "",
			wantDetail: "partial_resolution_metadata",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx, s, repoID, caps := auditFixture(t)
			fileID, _ := insertTestFile(ctx, s, repoID, "a.go")
			src, _ := insertTestSymbol(ctx, s, repoID, fileID, "Caller", "pkg/a.Caller")
			dst, _ := insertTestSymbol(ctx, s, repoID, fileID, "Helper", "pkg/a.Helper")
			edgeID, err := insertTestEdge(ctx, s, repoID, fileID, src, "Helper")
			if err != nil {
				t.Fatalf("insertTestEdge: %v", err)
			}
			target := any(nil)
			if tc.resolve {
				target = dst
			}
			if _, err := s.db.ExecContext(ctx, `
				UPDATE edges SET dst_symbol_id = ?, resolution_strategy = ?, resolution_confidence = ? WHERE id = ?
			`, target, tc.strategy, tc.confidence, edgeID); err != nil {
				t.Fatalf("corrupt edge: %v", err)
			}

			got := runCheck(t, ctx, s, repoID, EdgeCheckInvalidResolutionMetadata, caps, 5)
			if got.Count != 1 {
				t.Fatalf("invalid metadata count = %d, want 1", got.Count)
			}
			if len(got.Examples) != 1 {
				t.Fatalf("examples = %+v, want 1", got.Examples)
			}
			if got.Examples[0].Detail != tc.wantDetail {
				t.Errorf("detail = %q, want %q", got.Examples[0].Detail, tc.wantDetail)
			}
		})
	}
}

// TestAuditResolvedMissingMetadataIsNotCorruption separates the legacy pre-019
// state from the impossible states above: both columns empty on a resolved edge
// is a warning, not an error, and must not be counted twice.
func TestAuditResolvedMissingMetadataIsNotCorruption(t *testing.T) {
	ctx, s, repoID, caps := auditFixture(t)

	fileID, _ := insertTestFile(ctx, s, repoID, "a.go")
	src, _ := insertTestSymbol(ctx, s, repoID, fileID, "Caller", "pkg/a.Caller")
	dst, _ := insertTestSymbol(ctx, s, repoID, fileID, "Helper", "pkg/a.Helper")
	edgeID, _ := insertTestEdge(ctx, s, repoID, fileID, src, "Helper")
	bindEdge(t, ctx, s, edgeID, dst, "", "")

	if got := runCheck(t, ctx, s, repoID, EdgeCheckResolvedMissingMetadata, caps, 5); got.Count != 1 {
		t.Fatalf("resolved-missing-metadata count = %d, want 1", got.Count)
	}
	if got := runCheck(t, ctx, s, repoID, EdgeCheckInvalidResolutionMetadata, caps, 5); got.Count != 0 {
		t.Fatalf("legacy row also reported as invalid metadata: count = %d, want 0", got.Count)
	}
}

func TestAuditDetectsLowConfidenceResolution(t *testing.T) {
	ctx, s, repoID, caps := auditFixture(t)

	fileID, _ := insertTestFile(ctx, s, repoID, "a.go")
	src, _ := insertTestSymbol(ctx, s, repoID, fileID, "Caller", "pkg/a.Caller")
	dst, _ := insertTestSymbol(ctx, s, repoID, fileID, "Helper", "pkg/a.Helper")
	edgeID, _ := insertTestEdge(ctx, s, repoID, fileID, src, "Helper")
	bindEdge(t, ctx, s, edgeID, dst, ResolutionStrategyDotSuffix, ResolutionConfidenceLow)

	if got := runCheck(t, ctx, s, repoID, EdgeCheckLowConfidenceResolution, caps, 5); got.Count != 1 {
		t.Fatalf("low-confidence count = %d, want 1", got.Count)
	}
	// A low-confidence bind is legal, so it must not also read as corrupt.
	if got := runCheck(t, ctx, s, repoID, EdgeCheckInvalidResolutionMetadata, caps, 5); got.Count != 0 {
		t.Fatalf("low-confidence row reported as invalid metadata: count = %d, want 0", got.Count)
	}
}

// TestAuditCrossLanguageBindings is the resolver-correctness core: an explicit
// cross-language link must never be flagged merely for being cross-language,
// and an implicit one must always be.
func TestAuditCrossLanguageBindings(t *testing.T) {
	t.Run("explicit cross-language link is not a violation", func(t *testing.T) {
		ctx, s, repoID, caps := auditFixture(t)
		goFile, _ := insertTestFileLang(ctx, s, repoID, "a.go", "go")
		pyFile, _ := insertTestFileLang(ctx, s, repoID, "a.py", "python")
		src, _ := insertTestSymbolLang(ctx, s, repoID, goFile, "Handler", "pkg/a.Handler", "go")
		dst, _ := insertTestSymbolLang(ctx, s, repoID, pyFile, "Handler", "a.Handler", "python")

		var edgeID int64
		res, err := s.db.ExecContext(ctx, `
			INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line,
			                  resolution_strategy, resolution_confidence)
			VALUES(?, ?, ?, 'Handler', ?, 'shared_name:go->python', ?, 0, ?, ?)
		`, repoID, src, dst, EdgeKindCrossLanguageRef, goFile,
			ResolutionStrategyCrossLanguageSharedName, ResolutionConfidenceLow)
		if err != nil {
			t.Fatalf("insert cross-language edge: %v", err)
		}
		edgeID, _ = res.LastInsertId()

		if got := runCheck(t, ctx, s, repoID, EdgeCheckImplicitCrossLanguage, caps, 5); got.Count != 0 {
			t.Fatalf("explicit cross-language edge %d flagged as an implicit binding: count = %d, want 0", edgeID, got.Count)
		}
		if got := runCheck(t, ctx, s, repoID, EdgeCheckCrossLanguageImplicitStrategy, caps, 5); got.Count != 0 {
			t.Fatalf("explicit cross-language edge flagged for its strategy: count = %d, want 0", got.Count)
		}
		if got := runCheck(t, ctx, s, repoID, EdgeCheckInvalidResolutionMetadata, caps, 5); got.Count != 0 {
			t.Fatalf("explicit cross-language edge reported as invalid metadata: count = %d, want 0", got.Count)
		}
	})

	t.Run("implicit strategy across languages is a violation", func(t *testing.T) {
		ctx, s, repoID, caps := auditFixture(t)
		goFile, _ := insertTestFileLang(ctx, s, repoID, "a.go", "go")
		pyFile, _ := insertTestFileLang(ctx, s, repoID, "a.py", "python")
		src, _ := insertTestSymbolLang(ctx, s, repoID, goFile, "Caller", "pkg/a.Caller", "go")
		dst, _ := insertTestSymbolLang(ctx, s, repoID, pyFile, "Handler", "a.Handler", "python")
		edgeID, _ := insertTestEdge(ctx, s, repoID, goFile, src, "Handler")
		bindEdge(t, ctx, s, edgeID, dst, ResolutionStrategyExactName, ResolutionConfidenceHigh)

		got := runCheck(t, ctx, s, repoID, EdgeCheckImplicitCrossLanguage, caps, 5)
		if got.Count != 1 {
			t.Fatalf("implicit cross-language count = %d, want 1", got.Count)
		}
		if got.Examples[0].SrcLang != "go" || got.Examples[0].DstLang != "python" {
			t.Errorf("example languages = %q -> %q, want go -> python", got.Examples[0].SrcLang, got.Examples[0].DstLang)
		}
	})

	t.Run("empty language is not evidence of a mismatch", func(t *testing.T) {
		ctx, s, repoID, caps := auditFixture(t)
		goFile, _ := insertTestFileLang(ctx, s, repoID, "a.go", "go")
		unknownFile, _ := insertTestFileLang(ctx, s, repoID, "a.bin", "")
		src, _ := insertTestSymbolLang(ctx, s, repoID, goFile, "Caller", "pkg/a.Caller", "go")
		dst, _ := insertTestSymbolLang(ctx, s, repoID, unknownFile, "Handler", "Handler", "")
		edgeID, _ := insertTestEdge(ctx, s, repoID, goFile, src, "Handler")
		bindEdge(t, ctx, s, edgeID, dst, ResolutionStrategyExactName, ResolutionConfidenceHigh)

		if got := runCheck(t, ctx, s, repoID, EdgeCheckImplicitCrossLanguage, caps, 5); got.Count != 0 {
			t.Fatalf("unclassified language treated as a cross-language violation: count = %d, want 0", got.Count)
		}
	})

	t.Run("cross-language edge rebound by an implicit strategy is flagged", func(t *testing.T) {
		ctx, s, repoID, caps := auditFixture(t)
		goFile, _ := insertTestFileLang(ctx, s, repoID, "a.go", "go")
		src, _ := insertTestSymbolLang(ctx, s, repoID, goFile, "Caller", "pkg/a.Caller", "go")
		dst, _ := insertTestSymbolLang(ctx, s, repoID, goFile, "Handler", "pkg/a.Handler", "go")
		res, err := s.db.ExecContext(ctx, `
			INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line,
			                  resolution_strategy, resolution_confidence)
			VALUES(?, ?, ?, 'Handler', ?, 'shared_name:go->python', ?, 0, ?, ?)
		`, repoID, src, dst, EdgeKindCrossLanguageRef, goFile,
			ResolutionStrategyExactName, ResolutionConfidenceHigh)
		if err != nil {
			t.Fatalf("insert rebound cross-language edge: %v", err)
		}
		_ = res

		if got := runCheck(t, ctx, s, repoID, EdgeCheckCrossLanguageImplicitStrategy, caps, 5); got.Count != 1 {
			t.Fatalf("rebound cross-language edge count = %d, want 1", got.Count)
		}
		// Same language on both ends, so the language check must stay silent.
		if got := runCheck(t, ctx, s, repoID, EdgeCheckImplicitCrossLanguage, caps, 5); got.Count != 0 {
			t.Fatalf("same-language edge flagged as cross-language: count = %d, want 0", got.Count)
		}
	})
}

func TestAuditDetectsResolvedTargetInDeletedFile(t *testing.T) {
	ctx, s, repoID, caps := auditFixture(t)

	srcFile, _ := insertTestFile(ctx, s, repoID, "a.go")
	deletedFile, _ := insertTestFile(ctx, s, repoID, "b.go")
	src, _ := insertTestSymbol(ctx, s, repoID, srcFile, "Caller", "pkg/a.Caller")
	dst, _ := insertTestSymbol(ctx, s, repoID, deletedFile, "Helper", "pkg/b.Helper")
	edgeID, _ := insertTestEdge(ctx, s, repoID, srcFile, src, "Helper")
	bindEdge(t, ctx, s, edgeID, dst, ResolutionStrategyExactName, ResolutionConfidenceHigh)

	if got := runCheck(t, ctx, s, repoID, EdgeCheckResolvedTargetDeletedFile, caps, 5); got.Count != 0 {
		t.Fatalf("live destination reported as deleted: count = %d, want 0", got.Count)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE files SET is_deleted = 1 WHERE id = ?`, deletedFile); err != nil {
		t.Fatalf("mark file deleted: %v", err)
	}
	if got := runCheck(t, ctx, s, repoID, EdgeCheckResolvedTargetDeletedFile, caps, 5); got.Count != 1 {
		t.Fatalf("resolved-into-deleted-file count = %d, want 1", got.Count)
	}
}

// TestAuditIsCleanAfterMarkDeletedAndPurge runs the real delete lifecycle
// rather than setting is_deleted by hand, because that lifecycle is where a
// false positive would come from: if PurgeDeletedFileGraphsForScan left either
// a bound edge or a surviving symbol behind, the deleted-file and dangling
// checks would fire on an ordinary incremental update.
func TestAuditIsCleanAfterMarkDeletedAndPurge(t *testing.T) {
	ctx, s, repoID, caps := auditFixture(t)

	scanID, _, err := s.BeginScan(ctx, repoID, "index")
	if err != nil {
		t.Fatalf("BeginScan: %v", err)
	}
	srcFile, _ := insertTestFile(ctx, s, repoID, "a.go")
	goneFile, _ := insertTestFile(ctx, s, repoID, "b.go")
	src, _ := insertTestSymbol(ctx, s, repoID, srcFile, "Caller", "pkg/a.Caller")
	dst, _ := insertTestSymbol(ctx, s, repoID, goneFile, "Helper", "pkg/b.Helper")
	edgeID, _ := insertTestEdge(ctx, s, repoID, srcFile, src, "Helper")
	bindEdge(t, ctx, s, edgeID, dst, ResolutionStrategyExactName, ResolutionConfidenceHigh)

	if _, err := s.MarkFilesDeletedBatch(ctx, repoID, scanID, []string{"b.go"}); err != nil {
		t.Fatalf("MarkFilesDeletedBatch: %v", err)
	}
	if _, err := s.PurgeDeletedFileGraphsForScan(ctx, repoID, scanID); err != nil {
		t.Fatalf("PurgeDeletedFileGraphsForScan: %v", err)
	}

	for _, check := range []EdgeAuditCheck{
		EdgeCheckDanglingTarget,
		EdgeCheckDanglingSource,
		EdgeCheckResolvedTargetDeletedFile,
		EdgeCheckInvalidResolutionMetadata,
		EdgeCheckImplicitCrossLanguage,
	} {
		if got := runCheck(t, ctx, s, repoID, check, caps, 5); got.Count != 0 {
			t.Errorf("check %v after a normal delete+purge = %d violations, want 0 (examples: %+v)", check, got.Count, got.Examples)
		}
	}
	// The edge itself must now be unresolved, with its provenance cleared --
	// which is why the metadata check above stays silent.
	var strategy, confidence string
	var dstID *int64
	if err := s.db.QueryRowContext(ctx,
		`SELECT dst_symbol_id, resolution_strategy, resolution_confidence FROM edges WHERE id = ?`, edgeID,
	).Scan(&dstID, &strategy, &confidence); err != nil {
		t.Fatalf("read edge after purge: %v", err)
	}
	if dstID != nil || strategy != "" || confidence != "" {
		t.Errorf("edge after purge = (dst %v, strategy %q, confidence %q), want fully unbound", dstID, strategy, confidence)
	}
}

// TestAuditExamplesAreCappedAndDeterministic covers three separate promises of
// the report contract at once: the count is the whole population, the examples
// are capped, and the sample is the same on every run.
func TestAuditExamplesAreCappedAndDeterministic(t *testing.T) {
	ctx, s, repoID, caps := auditFixture(t)

	fileID, _ := insertTestFile(ctx, s, repoID, "a.go")
	src, _ := insertTestSymbol(ctx, s, repoID, fileID, "Caller", "pkg/a.Caller")
	const total = 50
	for i := 0; i < total; i++ {
		edgeID, err := insertTestEdge(ctx, s, repoID, fileID, src, "Gone")
		if err != nil {
			t.Fatalf("insertTestEdge(%d): %v", i, err)
		}
		bindEdge(t, ctx, s, edgeID, 999999, ResolutionStrategyExactName, ResolutionConfidenceHigh)
	}

	got := runCheck(t, ctx, s, repoID, EdgeCheckDanglingTarget, caps, 5)
	if got.Count != total {
		t.Fatalf("count = %d, want the full population %d even though examples are capped", got.Count, total)
	}
	if len(got.Examples) != 5 {
		t.Fatalf("examples = %d, want the cap of 5", len(got.Examples))
	}
	for i := 1; i < len(got.Examples); i++ {
		if got.Examples[i-1].EdgeID >= got.Examples[i].EdgeID {
			t.Fatalf("examples are not ordered by edge id: %+v", got.Examples)
		}
	}
	again := runCheck(t, ctx, s, repoID, EdgeCheckDanglingTarget, caps, 5)
	for i := range got.Examples {
		if got.Examples[i].EdgeID != again.Examples[i].EdgeID {
			t.Fatalf("example ordering is not stable across runs: %+v vs %+v", got.Examples, again.Examples)
		}
	}

	// Zero and negative limits mean counts only, and must not change the count.
	countsOnly := runCheck(t, ctx, s, repoID, EdgeCheckDanglingTarget, caps, 0)
	if countsOnly.Count != total || len(countsOnly.Examples) != 0 {
		t.Fatalf("limit 0 = %d violations with %d examples, want %d with 0", countsOnly.Count, len(countsOnly.Examples), total)
	}
}

// TestAuditIsScopedToOneRepository guards the multi-repo case: nothing prevents
// two repositories sharing a database file, and attributing one repo's
// corruption to another would be worse than not reporting it.
func TestAuditIsScopedToOneRepository(t *testing.T) {
	ctx, s, repoID, caps := auditFixture(t)

	other, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("UpsertRepo(other): %v", err)
	}
	otherFile, _ := insertTestFile(ctx, s, other.ID, "other.go")
	otherSrc, _ := insertTestSymbol(ctx, s, other.ID, otherFile, "Caller", "other.Caller")
	otherEdge, _ := insertTestEdge(ctx, s, other.ID, otherFile, otherSrc, "Gone")
	bindEdge(t, ctx, s, otherEdge, 999999, ResolutionStrategyExactName, ResolutionConfidenceHigh)

	if got := runCheck(t, ctx, s, repoID, EdgeCheckDanglingTarget, caps, 5); got.Count != 0 {
		t.Fatalf("audit of repo %d saw repo %d's corruption: count = %d, want 0", repoID, other.ID, got.Count)
	}
	if got := runCheck(t, ctx, s, other.ID, EdgeCheckDanglingTarget, caps, 5); got.Count != 1 {
		t.Fatalf("audit of repo %d missed its own corruption: count = %d, want 1", other.ID, got.Count)
	}

	summary, err := s.GraphAuditSummaryFor(ctx, repoID)
	if err != nil {
		t.Fatalf("GraphAuditSummaryFor: %v", err)
	}
	if summary.Edges != 0 {
		t.Errorf("summary for repo %d counted %d edges from another repository", repoID, summary.Edges)
	}
}

// TestUnresolvedClassificationUsesP8Semantics asserts the counts come from
// internal/classify rather than from a SQL restatement of it. The Python `len`
// case is the discriminator: it is a builtin, but only until the project
// defines a same-language symbol claiming that name, at which point the
// classifier must abstain to `unknown`. No SQL-only counter would do that.
func TestUnresolvedClassificationUsesP8Semantics(t *testing.T) {
	ctx, s, repoID, caps := auditFixture(t)

	pyFile, _ := insertTestFileLang(ctx, s, repoID, "a.py", "python")
	src, _ := insertTestSymbolLang(ctx, s, repoID, pyFile, "caller", "a.caller", "python")
	if _, err := insertTestEdge(ctx, s, repoID, pyFile, src, "len"); err != nil {
		t.Fatalf("insertTestEdge(len): %v", err)
	}

	counts, err := s.UnresolvedTargetClassificationCounts(ctx, repoID, caps)
	if err != nil {
		t.Fatalf("UnresolvedTargetClassificationCounts: %v", err)
	}
	if counts["builtin"] != 1 {
		t.Fatalf("counts = %+v, want one builtin for the Python len() call", counts)
	}

	// Now let the project define `len` in the same language. P8 abstains.
	if _, err := insertTestSymbolLang(ctx, s, repoID, pyFile, "len", "a.len", "python"); err != nil {
		t.Fatalf("insertTestSymbolLang(len): %v", err)
	}
	counts, err = s.UnresolvedTargetClassificationCounts(ctx, repoID, caps)
	if err != nil {
		t.Fatalf("UnresolvedTargetClassificationCounts (shadowed): %v", err)
	}
	if counts["builtin"] != 0 || counts["unknown"] != 1 {
		t.Fatalf("counts after project shadows len = %+v, want builtin 0 and unknown 1 (P8 abstention)", counts)
	}
}

// TestUnresolvedClassificationPagesBeyondOnePage exercises the keyset walk past
// its page boundary, where an off-by-one would silently drop or double-count
// the tail.
func TestUnresolvedClassificationPagesBeyondOnePage(t *testing.T) {
	ctx, s, repoID, caps := auditFixture(t)

	pyFile, _ := insertTestFileLang(ctx, s, repoID, "a.py", "python")
	src, _ := insertTestSymbolLang(ctx, s, repoID, pyFile, "caller", "a.caller", "python")
	total := auditClassificationPageSize + 7
	for i := 0; i < total; i++ {
		if _, err := insertTestEdge(ctx, s, repoID, pyFile, src, "len"); err != nil {
			t.Fatalf("insertTestEdge(%d): %v", i, err)
		}
	}

	counts, err := s.UnresolvedTargetClassificationCounts(ctx, repoID, caps)
	if err != nil {
		t.Fatalf("UnresolvedTargetClassificationCounts: %v", err)
	}
	var sum int64
	for _, n := range counts {
		sum += n
	}
	if sum != int64(total) {
		t.Fatalf("classified %d unresolved edges across pages, want %d (counts: %+v)", sum, total, counts)
	}
}

// TestAuditChecksRejectPreP4Schema pins the legacy path: without the migration
// 019 columns the P4-dependent checks report ErrAuditCheckUnsupported rather
// than failing the run or, worse, silently reporting zero violations.
func TestAuditChecksRejectPreP4Schema(t *testing.T) {
	ctx, s, repoID, _ := auditFixture(t)
	legacy := GraphAuditCapabilities{SchemaVersion: 18, HasResolutionMetadata: false}

	for _, check := range []EdgeAuditCheck{
		EdgeCheckInvalidResolutionMetadata,
		EdgeCheckResolvedMissingMetadata,
		EdgeCheckLowConfidenceResolution,
		EdgeCheckCrossLanguageImplicitStrategy,
	} {
		if _, err := s.RunEdgeAuditCheck(ctx, repoID, check, legacy, 5); !errors.Is(err, ErrAuditCheckUnsupported) {
			t.Errorf("check %v on a pre-019 schema: err = %v, want ErrAuditCheckUnsupported", check, err)
		}
	}
	// The schema-independent checks must still run.
	for _, check := range []EdgeAuditCheck{
		EdgeCheckDanglingTarget,
		EdgeCheckDanglingSource,
		EdgeCheckResolvedTargetDeletedFile,
		EdgeCheckImplicitCrossLanguage,
	} {
		if _, err := s.RunEdgeAuditCheck(ctx, repoID, check, legacy, 5); err != nil {
			t.Errorf("check %v on a pre-019 schema: err = %v, want it to run", check, err)
		}
	}
	if _, err := s.ResolutionStrategyDistribution(ctx, repoID, legacy); !errors.Is(err, ErrAuditCheckUnsupported) {
		t.Errorf("strategy distribution on a pre-019 schema: err = %v, want ErrAuditCheckUnsupported", err)
	}
}

// TestImplicitCrossLanguageFallsBackToEdgeKind covers the pre-019 variant of
// the cross-language check, where edge_kind stands in for missing provenance.
func TestImplicitCrossLanguageFallsBackToEdgeKind(t *testing.T) {
	ctx, s, repoID, _ := auditFixture(t)
	legacy := GraphAuditCapabilities{SchemaVersion: 18, HasResolutionMetadata: false}

	goFile, _ := insertTestFileLang(ctx, s, repoID, "a.go", "go")
	pyFile, _ := insertTestFileLang(ctx, s, repoID, "a.py", "python")
	src, _ := insertTestSymbolLang(ctx, s, repoID, goFile, "Caller", "pkg/a.Caller", "go")
	dst, _ := insertTestSymbolLang(ctx, s, repoID, pyFile, "Handler", "a.Handler", "python")

	// An explicit cross-language row: excluded by kind even with no provenance.
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line)
		VALUES(?, ?, ?, 'Handler', ?, 'shared_name:go->python', ?, 0)
	`, repoID, src, dst, EdgeKindCrossLanguageRef, goFile); err != nil {
		t.Fatalf("insert explicit cross-language edge: %v", err)
	}
	got, err := s.RunEdgeAuditCheck(ctx, repoID, EdgeCheckImplicitCrossLanguage, legacy, 5)
	if err != nil {
		t.Fatalf("RunEdgeAuditCheck: %v", err)
	}
	if got.Count != 0 {
		t.Fatalf("explicit cross-language edge flagged on a pre-019 schema: count = %d, want 0", got.Count)
	}

	// A parser-derived call edge across languages: still a violation.
	edgeID, _ := insertTestEdge(ctx, s, repoID, goFile, src, "Handler")
	if _, err := s.db.ExecContext(ctx, `UPDATE edges SET dst_symbol_id = ? WHERE id = ?`, dst, edgeID); err != nil {
		t.Fatalf("bind call edge: %v", err)
	}
	got, err = s.RunEdgeAuditCheck(ctx, repoID, EdgeCheckImplicitCrossLanguage, legacy, 5)
	if err != nil {
		t.Fatalf("RunEdgeAuditCheck: %v", err)
	}
	if got.Count != 1 {
		t.Fatalf("implicit cross-language call on a pre-019 schema: count = %d, want 1", got.Count)
	}
	if got.Examples[0].Strategy != "" {
		t.Errorf("pre-019 example reported a strategy %q, want empty", got.Examples[0].Strategy)
	}
}

// TestClassificationColumnsStayPositionallyAligned guards the one silent
// corruption risk in this file: unresolvedClassificationColumnsFor hand-copies
// exportEdgeColumnsSQL with two columns replaced by literals, and
// scanExportEdges reads the result BY POSITION. If a column is ever added to
// exportEdgeColumnsSQL without updating the copy, every field after the
// insertion point would be scanned from the wrong column -- silently, with no
// SQL error. Comparing the shapes here turns that into a failing test.
func TestClassificationColumnsStayPositionallyAligned(t *testing.T) {
	legacy := unresolvedClassificationColumnsFor(GraphAuditCapabilities{HasResolutionMetadata: false})
	current := unresolvedClassificationColumnsFor(GraphAuditCapabilities{HasResolutionMetadata: true})

	if current != exportEdgeColumnsSQL {
		t.Fatalf("the current-schema projection diverged from exportEdgeColumnsSQL:\ngot:  %s\nwant: %s", current, exportEdgeColumnsSQL)
	}
	if got, want := len(splitSQLColumns(legacy)), len(splitSQLColumns(exportEdgeColumnsSQL)); got != want {
		t.Fatalf("legacy projection has %d columns, exportEdgeColumnsSQL has %d; scanExportEdges reads by position", got, want)
	}
	legacyCols := splitSQLColumns(legacy)
	currentCols := splitSQLColumns(exportEdgeColumnsSQL)
	for i := range legacyCols {
		l, c := strings.TrimSpace(legacyCols[i]), strings.TrimSpace(currentCols[i])
		if l == c {
			continue
		}
		// The only permitted divergence is the two resolution columns, which
		// the legacy projection replaces with empty-string literals.
		if l == "''" && strings.HasPrefix(c, "e.resolution_") {
			continue
		}
		t.Errorf("column %d differs: legacy %q vs current %q; only the resolution columns may be substituted", i, l, c)
	}
}

// TestAuditExampleColumnsMatchScanArity pins the same property for the example
// projection, whose two variants must stay the same width as the Scan in
// RunEdgeAuditCheck.
func TestAuditExampleColumnsMatchScanArity(t *testing.T) {
	withMetadata := auditEdgeExampleColumnsFor(GraphAuditCapabilities{HasResolutionMetadata: true})
	without := auditEdgeExampleColumnsFor(GraphAuditCapabilities{HasResolutionMetadata: false})
	if got, want := len(splitSQLColumns(without)), len(splitSQLColumns(withMetadata)); got != want {
		t.Fatalf("example projections differ in width: %d vs %d", got, want)
	}
	// 11 projected columns plus the detail expression appended by the caller
	// equals the 12 destinations RunEdgeAuditCheck scans into.
	if got := len(splitSQLColumns(withMetadata)); got != 11 {
		t.Fatalf("example projection has %d columns, want 11 (Scan reads 11 + detail)", got)
	}
}

// splitSQLColumns splits a SELECT projection on its top-level commas, ignoring
// commas nested inside call parentheses such as COALESCE(x, ''). A naive
// strings.Split would count those and make the arity assertions meaningless.
func splitSQLColumns(list string) []string {
	var out []string
	depth, start := 0, 0
	for i, r := range list {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				out = append(out, strings.TrimSpace(list[start:i]))
				start = i + 1
			}
		}
	}
	return append(out, strings.TrimSpace(list[start:]))
}

// TestOpenReadOnlyRefusesUnindexedRepo pins the "not indexed" contract: no
// database is created and the error is identifiable.
func TestOpenReadOnlyRefusesUnindexedRepo(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "missing.sqlite")
	if _, err := OpenReadOnly(dbPath, OpenOptions{}); !errors.Is(err, ErrRepoNotIndexed) {
		t.Fatalf("OpenReadOnly(missing) err = %v, want ErrRepoNotIndexed", err)
	}
	// Open is expected to create it; this only proves the path was free. The
	// handle must be closed before t.TempDir cleanup, or Windows refuses to
	// remove the still-open database file.
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open() after OpenReadOnly error = %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

// TestOpenReadOnlyCannotWrite proves the read-only guarantee at the driver,
// not by inspection of the audit code.
func TestOpenReadOnlyCannotWrite(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "graph.sqlite")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	repo, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	s.Close()

	ro, err := OpenReadOnly(dbPath, OpenOptions{})
	if err != nil {
		t.Fatalf("OpenReadOnly() error = %v", err)
	}
	defer ro.Close()

	if _, err := ro.db.ExecContext(ctx, `INSERT INTO files(repo_id, path, language, indexed_at) VALUES(?, 'x.go', 'go', '')`, repo.ID); err == nil {
		t.Fatal("write through a read-only handle succeeded; mode=ro is not in effect")
	}
	// Reads still work, and FindRepo does not insert.
	found, ok, err := ro.FindRepo(ctx, repo.RootPath)
	if err != nil || !ok || found.ID != repo.ID {
		t.Fatalf("FindRepo() = (%+v, %v, %v), want the existing repo", found, ok, err)
	}
}
