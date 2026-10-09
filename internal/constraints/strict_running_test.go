package constraints

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/store"
)

// cancelingAdapter cancels the scan's context from inside a parse.
type cancelingAdapter struct {
	*goparser.Adapter
	cancel context.CancelFunc
}

func (a cancelingAdapter) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	a.cancel()
	return a.Adapter.Parse(ctx, path, content)
}

// beginRunning opens a scan row and leaves it running, as a live writer or a
// crashed process does.
func (s *strictRepo) beginRunning(t *testing.T) (int64, time.Time) {
	t.Helper()
	id, started, err := s.st.BeginScanWithScope(context.Background(), s.id, "update", store.ScanScopePaths, s.r.Git("rev-parse", "HEAD"))
	if err != nil {
		t.Fatal(err)
	}
	return id, started
}

// strictCase checks the strict verdict and that default mode is untouched:
// no freshness key, the status's own exit code, and the same bytes as the
// strict result without its freshness block.
func (s *strictRepo) strictCase(t *testing.T, status, verdict string, code int) Result {
	t.Helper()
	res := s.expect(t, status, verdict, code)
	def := s.check(t, false)
	if def.Freshness != nil || ResultExitCode(def) != ExitCode(status) || def.Status != status {
		t.Fatalf("default mode changed: status %s exit %d freshness %+v", def.Status, ResultExitCode(def), def.Freshness)
	}
	stripped := res
	stripped.Freshness = nil
	a, _ := json.Marshal(stripped)
	b, _ := json.Marshal(def)
	if string(a) != string(b) {
		t.Fatalf("strict mode changed default keys:\n%s\n%s", a, b)
	}
	return res
}

func hasReasons(t *testing.T, res Result, want ...string) {
	t.Helper()
	for _, r := range want {
		if !slices.Contains(res.Freshness.Reasons, r) {
			t.Fatalf("reasons %v lack %s", res.Freshness.Reasons, r)
		}
	}
}

// Writer-liveness uncertainty alone is unknown (4), whichever side of the
// last completed scan the running row is on; an independent stale fact
// alongside it is still known stale (3).
func TestStrictFreshnessRunningScans(t *testing.T) {
	ctx := context.Background()

	t.Run("newer running scan after a full scan", func(t *testing.T) {
		s := newStrictRepo(t)
		s.run(t, indexer.Options{})
		s.beginRunning(t)
		res := s.strictCase(t, StatusOK, VerdictUnknown, ExitFreshnessUnknown)
		hasReasons(t, res, "scan_running_or_abandoned")
		// The state field still reports graph_stats' state.
		if res.Freshness.State != graph.FreshnessKnownStale || !slices.Equal(res.Freshness.StateReasons, []string{"scan_running_or_abandoned"}) {
			t.Fatalf("state = %s %v", res.Freshness.State, res.Freshness.StateReasons)
		}
	})

	t.Run("newer running scan without a full scan", func(t *testing.T) {
		s := newStrictRepo(t)
		s.run(t, indexer.Options{Languages: []string{"go"}})
		s.beginRunning(t)
		res := s.strictCase(t, StatusOK, VerdictInsufficientCoverage, ExitInsufficientCoverage)
		if !slices.Equal(res.Freshness.Reasons, []string{"no_recorded_full_scan", "scan_running_or_abandoned"}) {
			t.Fatalf("reasons = %v", res.Freshness.Reasons)
		}
	})

	t.Run("newer running scan and HEAD moved", func(t *testing.T) {
		s := newStrictRepo(t)
		s.run(t, indexer.Options{})
		s.beginRunning(t)
		s.r.Commit("", "two")
		res := s.strictCase(t, StatusOK, VerdictKnownStale, ExitKnownStale)
		hasReasons(t, res, "head_moved", "scan_running_or_abandoned")
	})

	t.Run("newer running scan and queued work", func(t *testing.T) {
		s := newStrictRepo(t)
		s.run(t, indexer.Options{})
		s.beginRunning(t)
		if err := s.st.QueueDirtyFiles(ctx, s.id, []string{"internal/domain/a.go"}, "modified"); err != nil {
			t.Fatal(err)
		}
		res := s.strictCase(t, StatusStale, VerdictKnownStale, ExitKnownStale)
		hasReasons(t, res, "dirty_queue_nonempty", "scan_running_or_abandoned")
	})

	t.Run("older running scan below a full scan", func(t *testing.T) {
		s := newStrictRepo(t)
		s.run(t, indexer.Options{})
		s.beginRunning(t)
		s.run(t, indexer.Options{})
		res := s.strictCase(t, StatusOK, VerdictUnknown, ExitFreshnessUnknown)
		hasReasons(t, res, "scan_running_or_abandoned", "full_scan_overlapped_another_scan")
		if res.Freshness.State != graph.FreshnessNoKnownStaleness {
			t.Fatalf("state = %s", res.Freshness.State)
		}
	})

	t.Run("full scan overlapped a writer that then completed", func(t *testing.T) {
		s := newStrictRepo(t)
		s.run(t, indexer.Options{})
		id, started := s.beginRunning(t)
		s.run(t, indexer.Options{})
		if err := s.st.CompleteScan(ctx, id, store.ScanSummary{}, started, "completed", ""); err != nil {
			t.Fatal(err)
		}
		res := s.strictCase(t, StatusOK, VerdictUnknown, ExitFreshnessUnknown)
		if !slices.Equal(res.Freshness.Reasons, []string{"full_scan_overlapped_another_scan"}) {
			t.Fatalf("reasons = %v", res.Freshness.Reasons)
		}
	})

	// A cancelled scan closes as failed. Its batches may already have
	// committed, so the graph is no completed scan's result: latest_scan_failed
	// is an independent stale fact and stays known stale (3).
	t.Run("cancelled scan after a full scan", func(t *testing.T) {
		s := newStrictRepo(t)
		s.run(t, indexer.Options{})
		cctx, cancel := context.WithCancel(ctx)
		defer cancel()
		cancelling := indexer.New(s.st, parser.NewRegistry(cancelingAdapter{Adapter: goparser.New(), cancel: cancel}), nil)
		if _, err := cancelling.Index(cctx, indexer.Options{RepoRoot: s.r.Dir, Force: true}); err == nil {
			t.Fatal("cancelled scan succeeded")
		}
		res := s.strictCase(t, StatusOK, VerdictKnownStale, ExitKnownStale)
		if !slices.Equal(res.Freshness.Reasons, []string{"latest_scan_failed"}) {
			t.Fatalf("reasons = %v", res.Freshness.Reasons)
		}
	})

	t.Run("recovery", func(t *testing.T) {
		s := newStrictRepo(t)
		s.run(t, indexer.Options{})
		id, started := s.beginRunning(t)
		// A new full scan cannot prove anything while the row stays running.
		s.run(t, indexer.Options{})
		res := s.strictCase(t, StatusOK, VerdictUnknown, ExitFreshnessUnknown)
		hasReasons(t, res, "scan_running_or_abandoned")
		// The writer closes normally; the full scan that overlapped it still
		// does not count, the next one does.
		if err := s.st.CompleteScan(ctx, id, store.ScanSummary{}, started, "completed", ""); err != nil {
			t.Fatal(err)
		}
		s.strictCase(t, StatusOK, VerdictUnknown, ExitFreshnessUnknown)
		s.run(t, indexer.Options{})
		s.strictCase(t, StatusOK, VerdictFullCoverageAtHead, 0)
	})
}
