package constraints

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/githistory/gittest"
	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/store"
)

func freshnessAt(head string, full *graph.FreshnessScan) graph.Freshness {
	return graph.Freshness{
		State:      graph.FreshnessNoKnownStaleness,
		Reasons:    []string{},
		LatestScan: &graph.FreshnessScan{ID: 7, Status: "completed"},
		Worktree:   graph.FreshnessWorktree{HeadNow: head},
		Coverage:   graph.FreshnessCoverage{Recorded: true, LastFullScan: full},
	}
}

func fullScan(start, finish, overlap string) *graph.FreshnessScan {
	return &graph.FreshnessScan{ID: 7, Kind: "index", Status: "completed", Scope: "full", HeadAtStart: start, HeadAtFinish: finish, Overlap: overlap}
}

// Every verdict and its exit code, from the freshness evidence alone.
func TestStrictFreshnessVerdicts(t *testing.T) {
	stale := freshnessAt("h1", fullScan("h1", "h1", "none"))
	stale.State, stale.Reasons = graph.FreshnessKnownStale, []string{"head_moved"}
	neverCompleted := freshnessAt("h1", nil)
	neverCompleted.State, neverCompleted.Reasons = graph.FreshnessUnknown, []string{"never_completed"}
	unrecorded := freshnessAt("h1", nil)
	unrecorded.Coverage.Recorded = false
	failedAfter := freshnessAt("h1", fullScan("h1", "h1", "none"))
	failedAfter.Coverage.FailedAfterLastFull = 1
	olderRunning := freshnessAt("h1", fullScan("h1", "h1", "none"))
	olderRunning.RunningScans = graph.FreshnessRunningScans{Count: 1, AfterLastCompleted: 0, Liveness: "unknown"}
	staleNoQueueReason := freshnessAt("h1", fullScan("h1", "h1", "none"))
	staleNoQueueReason.State, staleNoQueueReason.Reasons = graph.FreshnessKnownStale, []string{"head_moved"}
	// The first index still running: never completed plus a running row is
	// not a stale fact, so it is unknown. A scan left running (or abandoned)
	// and a later scan that failed before any completed is: the failed scan
	// may have committed batches.
	firstRunning := freshnessAt("h1", nil)
	firstRunning.LatestScan = &graph.FreshnessScan{ID: 7, Status: "running"}
	firstRunning.State, firstRunning.Reasons = graph.FreshnessKnownStale, []string{"never_completed", "scan_running_or_abandoned"}
	firstRunning.RunningScans = graph.FreshnessRunningScans{Count: 1, AfterLastCompleted: 1, Liveness: "unknown"}
	runningThenFailed := firstRunning
	runningThenFailed.LatestScan = &graph.FreshnessScan{ID: 7, Status: "failed"}
	runningThenFailed.Reasons = []string{"never_completed", "latest_scan_failed", "scan_running_or_abandoned"}
	both := freshnessAt("h1", fullScan("h1", "h1", "yes"))
	both.Coverage.FailedAfterLastFull = 1

	cases := []struct {
		name    string
		status  string
		f       graph.Freshness
		verdict string
		code    int
		reasons []string
	}{
		{"full coverage ok", StatusOK, freshnessAt("h1", fullScan("h1", "h1", "none")), VerdictFullCoverageAtHead, 0, []string{}},
		{"full coverage violations", StatusViolations, freshnessAt("h1", fullScan("h1", "h1", "none")), VerdictFullCoverageAtHead, 1, []string{}},
		{"dirty queue status", StatusStale, freshnessAt("h1", fullScan("h1", "h1", "none")), VerdictKnownStale, ExitKnownStale, []string{"dirty_queue_nonempty"}},
		{"older scan still running", StatusOK, olderRunning, VerdictUnknown, ExitFreshnessUnknown, []string{"scan_running_or_abandoned"}},
		{"stale status adds queue reason", StatusStale, staleNoQueueReason, VerdictKnownStale, ExitKnownStale, []string{"head_moved", "dirty_queue_nonempty"}},
		{"known stale state", StatusOK, stale, VerdictKnownStale, ExitKnownStale, []string{"head_moved"}},
		{"never completed", StatusOK, neverCompleted, VerdictUnknown, ExitFreshnessUnknown, []string{"never_completed"}},
		{"first index still running", StatusOK, firstRunning, VerdictUnknown, ExitFreshnessUnknown, []string{"never_completed", "scan_running_or_abandoned"}},
		{"running scan then a later failed scan", StatusOK, runningThenFailed, VerdictKnownStale, ExitKnownStale, []string{"never_completed", "latest_scan_failed", "scan_running_or_abandoned"}},
		{"pre-migration database", StatusOK, unrecorded, VerdictUnknown, ExitFreshnessUnknown, []string{"coverage_not_recorded"}},
		{"no full scan", StatusOK, freshnessAt("h1", nil), VerdictInsufficientCoverage, ExitInsufficientCoverage, []string{"no_recorded_full_scan"}},
		{"failed after full", StatusViolations, failedAfter, VerdictInsufficientCoverage, ExitInsufficientCoverage, []string{"scan_failed_after_full_scan"}},
		{"overlapped writer", StatusOK, freshnessAt("h1", fullScan("h1", "h1", "yes")), VerdictUnknown, ExitFreshnessUnknown, []string{"full_scan_overlapped_another_scan"}},
		{"overlap unrecorded", StatusOK, freshnessAt("h1", fullScan("h1", "h1", "unknown")), VerdictUnknown, ExitFreshnessUnknown, []string{"full_scan_overlap_not_recorded"}},
		{"head not recorded", StatusOK, freshnessAt("h1", fullScan("", "h1", "none")), VerdictUnknown, ExitFreshnessUnknown, []string{"full_scan_head_not_recorded"}},
		{"head moved during scan", StatusOK, freshnessAt("h2", fullScan("h1", "h2", "none")), VerdictInsufficientCoverage, ExitInsufficientCoverage, []string{"head_moved_during_full_scan"}},
		{"head now unknown", StatusOK, freshnessAt("unknown", fullScan("h1", "h1", "none")), VerdictUnknown, ExitFreshnessUnknown, []string{"head_now_unknown"}},
		{"head moved since scan", StatusOK, freshnessAt("h2", fullScan("h1", "h1", "none")), VerdictInsufficientCoverage, ExitInsufficientCoverage, []string{"head_moved_since_full_scan"}},
		{"gap outranks unknown", StatusOK, both, VerdictInsufficientCoverage, ExitInsufficientCoverage, []string{"scan_failed_after_full_scan", "full_scan_overlapped_another_scan"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := strictFreshness(tc.status, 7, tc.f, tc.f)
			if got.Verdict != tc.verdict || got.ExitCode != tc.code || !slices.Equal(got.Reasons, tc.reasons) {
				t.Fatalf("got %s exit %d reasons %v; want %s exit %d reasons %v", got.Verdict, got.ExitCode, got.Reasons, tc.verdict, tc.code, tc.reasons)
			}
			if got.Verdict != VerdictFullCoverageAtHead && got.ExitCode < 3 {
				t.Fatalf("an unproven verdict exits %d", got.ExitCode)
			}
			if got.State == "fresh" || got.Verdict == "fresh" {
				t.Fatal("strict mode claims fresh")
			}
		})
	}

	// A scan that starts, closes or queues work during the check, or a HEAD
	// that moves during it, makes an otherwise proven verdict unknown.
	proven := freshnessAt("h1", fullScan("h1", "h1", "none"))
	if got := strictFreshness(StatusOK, 8, proven, proven); got.Verdict != VerdictUnknown || !slices.Equal(got.Reasons, []string{"scan_activity_during_check"}) {
		t.Fatalf("graph read a newer scan than freshness: %+v", got)
	}
	for name, mutate := range map[string]func(*graph.Freshness){
		"new scan":    func(f *graph.Freshness) { f.LatestScan = &graph.FreshnessScan{ID: 8, Status: "running"} },
		"scan closed": func(f *graph.Freshness) { f.LatestScan = &graph.FreshnessScan{ID: 7, Status: "failed"} },
		"queued":      func(f *graph.Freshness) { f.DirtyQueue.Queued = 1 },
		"head moved":  func(f *graph.Freshness) { f.Worktree.HeadNow = "h2" },
	} {
		after := proven
		mutate(&after)
		got := strictFreshness(StatusOK, 7, proven, after)
		if got.Verdict != VerdictUnknown || got.ExitCode != ExitFreshnessUnknown || !slices.Equal(got.Reasons, []string{"scan_activity_during_check"}) {
			t.Fatalf("%s: got %+v", name, got)
		}
	}
}

type strictRepo struct {
	r   *gittest.Repo
	st  *store.Store
	idx *indexer.Indexer
	id  int64
}

func newStrictRepo(t *testing.T) *strictRepo {
	t.Helper()
	r := gittest.Init(t)
	r.Write(ConfigFileName, parityConfig)
	r.Write("go.mod", "module example.com/m\n\ngo 1.22\n")
	r.Write("internal/domain/a.go", "package domain\n\nfunc A() {}\n")
	r.Write("internal/infra/b.go", "package infra\n\nfunc B() {}\n")
	r.Commit("", "one")
	st, err := store.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return &strictRepo{r: r, st: st, idx: indexer.New(st, parser.NewRegistry(goparser.New()), nil)}
}

func (s *strictRepo) run(t *testing.T, opts indexer.Options) {
	t.Helper()
	opts.RepoRoot = s.r.Dir
	if _, err := s.idx.Index(context.Background(), opts); err != nil {
		t.Fatal(err)
	}
	repo, _, err := s.st.FindRepo(context.Background(), s.r.Dir)
	if err != nil {
		t.Fatal(err)
	}
	s.id = repo.ID
}

func (s *strictRepo) check(t *testing.T, strict bool) Result {
	t.Helper()
	res, err := Check(context.Background(), Options{RepoRoot: s.r.Dir, StrictFreshness: strict}, func(context.Context) (*store.Store, int64, error) {
		return s.st, s.id, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func (s *strictRepo) expect(t *testing.T, status, verdict string, code int) Result {
	t.Helper()
	res := s.check(t, true)
	if res.Status != status || res.Freshness == nil || res.Freshness.Verdict != verdict || ResultExitCode(res) != code {
		t.Fatalf("status %s freshness %+v exit %d; want %s %s %d", res.Status, res.Freshness, ResultExitCode(res), status, verdict, code)
	}
	return res
}

// Exit-code transitions through real scans of a real Git repository.
func TestStrictFreshnessTransitions(t *testing.T) {
	s := newStrictRepo(t)
	s.run(t, indexer.Options{Languages: []string{"go"}})
	// A filtered walk is no whole-repository coverage.
	s.expect(t, StatusOK, VerdictInsufficientCoverage, ExitInsufficientCoverage)

	s.run(t, indexer.Options{})
	res := s.expect(t, StatusOK, VerdictFullCoverageAtHead, 0)
	if res.Freshness.State != graph.FreshnessNoKnownStaleness || res.Freshness.LastFullScan == nil {
		t.Fatalf("freshness = %+v", res.Freshness)
	}
	b, _ := json.Marshal(res)
	if strings.Contains(string(b), s.r.Dir) {
		t.Fatalf("strict result leaks the host path: %s", b)
	}

	s.r.Write("internal/domain/a.go", "package domain\n\nimport \"example.com/m/internal/infra\"\n\nfunc A() { infra.B() }\n")
	s.r.Commit("", "two")
	// HEAD moved past the Git history watermark: known stale.
	s.expect(t, StatusOK, VerdictKnownStale, ExitKnownStale)

	// A path-scoped update advances the watermark but proves no full coverage.
	s.run(t, indexer.Options{Paths: []string{"internal/domain/a.go"}})
	res = s.expect(t, StatusViolations, VerdictInsufficientCoverage, ExitInsufficientCoverage)
	if !slices.Equal(res.Freshness.Reasons, []string{"head_moved_since_full_scan"}) {
		t.Fatalf("reasons = %v", res.Freshness.Reasons)
	}

	s.run(t, indexer.Options{})
	s.expect(t, StatusViolations, VerdictFullCoverageAtHead, 1)

	if err := s.st.QueueDirtyFiles(context.Background(), s.id, []string{"internal/domain/a.go"}, "modified"); err != nil {
		t.Fatal(err)
	}
	res = s.expect(t, StatusStale, VerdictKnownStale, ExitKnownStale)
	// The reason comes from the freshness read itself, not the fallback.
	if !slices.Contains(res.Freshness.StateReasons, "dirty_queue_nonempty") || !slices.Equal(res.Freshness.Reasons, res.Freshness.StateReasons) {
		t.Fatalf("stale reasons = %v / %v", res.Freshness.Reasons, res.Freshness.StateReasons)
	}
}

// A scan completing after the strict freshness read but before the graph is
// read leaves the findings resting on rows the verdict did not see: unknown.
func TestStrictFreshnessScanDuringCheck(t *testing.T) {
	s := newStrictRepo(t)
	s.run(t, indexer.Options{})
	s.expect(t, StatusOK, VerdictFullCoverageAtHead, 0)
	strictBeforeHook = func() {
		strictBeforeHook = nil
		s.run(t, indexer.Options{})
	}
	t.Cleanup(func() { strictBeforeHook = nil })
	res := s.expect(t, StatusOK, VerdictUnknown, ExitFreshnessUnknown)
	if !slices.Equal(res.Freshness.Reasons, []string{"scan_activity_during_check"}) {
		t.Fatalf("reasons = %v", res.Freshness.Reasons)
	}
	// Once nothing runs during the check, the same graph is proven again.
	s.expect(t, StatusOK, VerdictFullCoverageAtHead, 0)
}

// Without strict mode the result has no freshness key and the exit code is
// the status's, whatever the coverage evidence says.
func TestStrictFreshnessOffKeepsDefaultContract(t *testing.T) {
	s := newStrictRepo(t)
	s.run(t, indexer.Options{Languages: []string{"go"}})
	res := s.check(t, false)
	b, err := json.Marshal(res)
	if err != nil {
		t.Fatal(err)
	}
	if res.Freshness != nil || strings.Contains(string(b), `"freshness"`) {
		t.Fatalf("default result carries freshness: %s", b)
	}
	if res.Status != StatusOK || ResultExitCode(res) != ExitCode(res.Status) || ResultExitCode(res) != 0 {
		t.Fatalf("status %s exit %d", res.Status, ResultExitCode(res))
	}
	if strict := s.check(t, true); strict.Freshness == nil || ResultExitCode(strict) == 0 {
		t.Fatalf("strict on the same graph = %+v", strict.Freshness)
	}
}

// Statuses decided before the graph is evaluated keep exit 2 and carry no
// freshness block in strict mode either.
func TestStrictFreshnessPreEvaluationStatuses(t *testing.T) {
	f := newFixture(t)
	if res := f.checkStrict(); res.Status != StatusNotConfigured || res.Freshness != nil || ResultExitCode(res) != 2 {
		t.Fatalf("not_configured: %+v", res)
	}
	f.config(`{"schema_version":2}`)
	if res := f.checkStrict(); res.Status != StatusConfigError || res.Freshness != nil || ResultExitCode(res) != 2 {
		t.Fatalf("config_error: %+v", res)
	}
	f.config(parityConfig)
	f.exec(`DELETE FROM scans`)
	if res := f.checkStrict(); res.Status != StatusNotIndexed || res.Freshness != nil || ResultExitCode(res) != 2 {
		t.Fatalf("not_indexed: %+v", res)
	}
}

func (f *fixture) checkStrict() Result {
	f.t.Helper()
	res, err := Check(context.Background(), Options{RepoRoot: f.root, StrictFreshness: true}, func(context.Context) (*store.Store, int64, error) {
		return f.st, f.repoID, nil
	})
	if err != nil {
		f.t.Fatal(err)
	}
	return res
}
