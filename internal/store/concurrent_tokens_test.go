package store

import (
	"context"
	"reflect"
	"testing"
	"time"
)

func TestSemanticSearchPinsOneSnapshotAcrossCommittedBatch(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)
	writer := openSameStore(t, s)
	fileID, err := insertTestFile(ctx, s, repoID, "a.go")
	if err != nil {
		t.Fatal(err)
	}
	seedScoredSymbol(t, ctx, s, repoID, fileID, "One", scoredToken{"renew", 1})
	id, started, err := writer.BeginScan(ctx, repoID, "update")
	if err != nil {
		t.Fatal(err)
	}
	s.semanticSearchAfterStats = func() {
		seedScoredSymbol(t, ctx, writer, repoID, fileID, "Two", scoredToken{"renew", 1}, scoredToken{"extra", 8})
	}
	got, err := s.SemanticSearch(ctx, repoID, "renew", 10, 0)
	s.semanticSearchAfterStats = nil
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("one search mixed a post-statistics commit into its result: got %d symbols, want pre-commit 1", len(got))
	}
	next, err := s.SemanticSearch(ctx, repoID, "renew", 10, 0)
	if err != nil || len(next) != 2 {
		t.Fatalf("next snapshot: count=%d err=%v", len(next), err)
	}
	if err := writer.CompleteScan(ctx, id, ScanSummary{}, started, "completed", ""); err != nil {
		t.Fatal(err)
	}
}

func TestCorpusStatsFollowVisibleWritesDuringScan(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)
	writer := openSameStore(t, s)
	fileID, err := insertTestFile(ctx, s, repoID, "a.go")
	if err != nil {
		t.Fatal(err)
	}
	seedScoredSymbol(t, ctx, s, repoID, fileID, "One", scoredToken{"renew", 1})
	before, err := s.corpusTokenStatsFor(ctx, repoID)
	if err != nil {
		t.Fatal(err)
	}
	id, started, err := writer.BeginScan(ctx, repoID, "update")
	if err != nil {
		t.Fatal(err)
	}
	seedScoredSymbol(t, ctx, writer, repoID, fileID, "Two", scoredToken{"renew", 1}, scoredToken{"extra", 8})
	during, err := s.corpusTokenStatsFor(ctx, repoID)
	if err != nil {
		t.Fatal(err)
	}
	if during.Symbols != 2 || during.AvgLen != 5 {
		t.Errorf("visible corpus has N=2 avg=5; cached before=%+v during=%+v", before, during)
	}
	cachedResult, err := s.SemanticSearch(ctx, repoID, "renew", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	liveResult, err := writer.SemanticSearch(ctx, repoID, "renew", 10, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(cachedResult, liveResult) {
		t.Errorf("same visible corpus scored differently with a warm cache: cached=%v live=%v", cachedResult, liveResult)
	}
	if err := writer.CompleteScan(ctx, id, ScanSummary{}, started, "completed", ""); err != nil {
		t.Fatal(err)
	}
}

func TestCorpusStatsFollowOutOfOrderScanCompletion(t *testing.T) {
	ctx := context.Background()
	s, repoID := newQueryTestStore(t)
	writer := openSameStore(t, s)
	fileID, err := insertTestFile(ctx, s, repoID, "a.go")
	if err != nil {
		t.Fatal(err)
	}
	seedScoredSymbol(t, ctx, s, repoID, fileID, "One", scoredToken{"renew", 1})
	older, olderStarted, err := writer.BeginScan(ctx, repoID, "update")
	if err != nil {
		t.Fatal(err)
	}
	newer, newerStarted, err := s.BeginScan(ctx, repoID, "update")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteScan(ctx, newer, ScanSummary{}, newerStarted, "completed", ""); err != nil {
		t.Fatal(err)
	}
	partial, err := s.corpusTokenStatsFor(ctx, repoID)
	if err != nil {
		t.Fatal(err)
	}
	seedScoredSymbol(t, ctx, writer, repoID, fileID, "Two", scoredToken{"renew", 1}, scoredToken{"extra", 8})
	if err := writer.CompleteScan(ctx, older, ScanSummary{}, olderStarted, "completed", ""); err != nil {
		t.Fatal(err)
	}
	after, err := s.corpusTokenStatsFor(ctx, repoID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Symbols != 2 || after.AvgLen != 5 {
		t.Fatalf("finished out of ID order: partial=%+v final=%+v, want N=2 avg=5", partial, after)
	}
}

func TestTokenRefreshRollbackAfterDelete(t *testing.T) {
	s, repoID := seedSemanticTokenRepo(t, true)
	before := dumpTokens(t, s, repoID)
	// Abort after DELETE has executed, at the first rewritten token insert.
	if _, err := s.db.Exec(`CREATE TRIGGER fail_token_refresh BEFORE INSERT ON symbol_tokens BEGIN SELECT RAISE(ABORT,'injected token write failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RefreshSemanticTokens(context.Background(), repoID); err == nil {
		t.Fatal("refresh did not fail")
	}
	if got := dumpTokens(t, s, repoID); got != before {
		t.Fatal("failed transaction changed token rows")
	}
	stale, err := s.SemanticTokensStale(context.Background(), repoID)
	if err != nil || !stale {
		t.Fatalf("stale=%v err=%v", stale, err)
	}
	if _, err := s.db.Exec(`DROP TRIGGER fail_token_refresh`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RefreshSemanticTokens(context.Background(), repoID); err != nil {
		t.Fatal(err)
	}
}

func TestFailedCompletionDoesNotUndoCommittedRefresh(t *testing.T) {
	s, repoID := seedSemanticTokenRepo(t, true)
	ctx := context.Background()
	id, started, err := s.BeginScan(ctx, repoID, "update")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RefreshSemanticTokens(ctx, repoID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`CREATE TRIGGER fail_scan_completion BEFORE UPDATE ON scans WHEN NEW.status='completed' BEGIN SELECT RAISE(ABORT,'injected completion failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteScan(ctx, id, ScanSummary{}, started, "completed", ""); err == nil {
		t.Fatal("completion did not fail")
	}
	stale, err := s.SemanticTokensStale(ctx, repoID)
	if err != nil || stale {
		t.Fatalf("committed refresh lost: stale=%v err=%v", stale, err)
	}
	if err := s.CompleteScan(ctx, id, ScanSummary{}, time.Now(), "failed", "injected"); err != nil {
		t.Fatal(err)
	}
	if err := s.requireCurrentSemanticTokens(ctx, repoID); err != nil {
		t.Fatal(err)
	}
}
