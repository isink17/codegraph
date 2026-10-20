package store

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/parser"
)

func completeFullScan(t *testing.T, s *Store, repoID int64) {
	t.Helper()
	ctx := context.Background()
	id, started, err := s.BeginScanWithScope(ctx, repoID, "index", ScanScopeFull, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CompleteScan(ctx, id, ScanSummary{RepoID: repoID, ScanID: id}, started, "completed", ""); err != nil {
		t.Fatal(err)
	}
}

// The live watcher state follows the lock protocol a watcher runs, for its
// own repository only, as seen from another store handle on the database.
func TestLiveWatcherFollowsLocks(t *testing.T) {
	ctx := context.Background()
	s, repoID := freshnessStore(t, t.TempDir())
	other, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	completeFullScan(t, s, repoID)
	completeFullScan(t, s, other.ID)
	reader := openSameStore(t, s)
	check := func(step, want string, pending bool) {
		t.Helper()
		f := freshness(t, reader, repoID)
		if f.LiveWatcher != want || slices.Contains(f.Reasons, "reconcile_pending") != pending {
			t.Fatalf("%s: live_watcher %s reasons %v, want %s pending=%v", step, f.LiveWatcher, f.Reasons, want, pending)
		}
		if o := freshness(t, reader, other.ID); o.LiveWatcher != LiveWatcherNone || slices.Contains(o.Reasons, "reconcile_pending") {
			t.Fatalf("%s: other repository reads live_watcher %s reasons %v", step, o.LiveWatcher, o.Reasons)
		}
	}
	check("never watched", LiveWatcherNone, false)

	lock, err := s.OpenWatcherLock(repoID)
	if err != nil {
		t.Fatal(err)
	}
	check("opened", LiveWatcherNone, false)
	if err := lock.Hold(); err != nil {
		t.Fatal(err)
	}
	check("starting", LiveWatcherUnverified, true)
	if err := lock.HoldLive(); err != nil {
		t.Fatal(err)
	}
	check("reconciling", LiveWatcherUnverified, true)
	lock.Release()
	check("watching", LiveWatcherWatching, false)
	if err := lock.Hold(); err != nil {
		t.Fatal(err)
	}
	check("recovering", LiveWatcherUnverified, true)
	lock.Close()
	check("exited", LiveWatcherNone, false)
}

// A second watcher of the same repository waits out the first's live lock;
// it is never reported watching while it is not, and without the live lock it
// keeps its unverified lock after it reconciles.
func TestLiveWatcherSecondWatcherNeverClaimsWatching(t *testing.T) {
	old := scanLockProbeWait
	scanLockProbeWait = time.Millisecond
	t.Cleanup(func() { scanLockProbeWait = old })
	s, repoID := freshnessStore(t, t.TempDir())
	completeFullScan(t, s, repoID)
	first, _ := s.OpenWatcherLock(repoID)
	second, _ := s.OpenWatcherLock(repoID)
	t.Cleanup(first.Close)
	t.Cleanup(second.Close)
	for _, l := range []*WatcherLock{first, second} {
		if err := l.Hold(); err != nil {
			t.Fatal(err)
		}
		if err := l.HoldLive(); err != nil {
			t.Fatal(err)
		}
	}
	first.Release()
	if f := freshness(t, s, repoID); f.LiveWatcher != LiveWatcherUnverified {
		t.Fatalf("second still reconciling: %s", f.LiveWatcher)
	}
	second.Release()
	if f := freshness(t, s, repoID); f.LiveWatcher != LiveWatcherUnverified || !slices.Contains(f.Reasons, "reconcile_pending") {
		t.Fatalf("both watching, second without the live lock: %s %v", f.LiveWatcher, f.Reasons)
	}
}

// A second watcher that never got the live lock stays visible once the first
// exits: unverified with reconcile_pending, which a full scan by another
// handle cannot clear, until it reconciles again and takes the live lock.
func TestLiveWatcherSecondVisibleAfterFirstExits(t *testing.T) {
	old := scanLockProbeWait
	scanLockProbeWait = time.Millisecond
	t.Cleanup(func() { scanLockProbeWait = old })
	s, repoID := freshnessStore(t, t.TempDir())
	completeFullScan(t, s, repoID)
	reader := openSameStore(t, s)
	first, _ := s.OpenWatcherLock(repoID)
	second, _ := s.OpenWatcherLock(repoID)
	t.Cleanup(first.Close)
	t.Cleanup(second.Close)
	for _, l := range []*WatcherLock{first, second} {
		if err := l.Hold(); err != nil {
			t.Fatal(err)
		}
		if err := l.HoldLive(); err != nil {
			t.Fatal(err)
		}
		l.Release()
	}
	first.Close()
	completeFullScan(t, s, repoID)
	f := freshness(t, reader, repoID)
	if f.LiveWatcher != LiveWatcherUnverified || !slices.Contains(f.Reasons, "reconcile_pending") || f.Verdict == graph.FreshnessVerdictConsistentWithLastScan {
		t.Fatalf("first exited, second runs: live_watcher %s verdict %s reasons %v", f.LiveWatcher, f.Verdict, f.Reasons)
	}
	if err := second.Hold(); err != nil {
		t.Fatal(err)
	}
	second.Release()
	if f := freshness(t, reader, repoID); f.LiveWatcher != LiveWatcherWatching || slices.Contains(f.Reasons, "reconcile_pending") {
		t.Fatalf("second reconciled again: live_watcher %s reasons %v", f.LiveWatcher, f.Reasons)
	}
	second.Close()
	if got := liveState(reader, repoID); got != LiveWatcherNone {
		t.Fatalf("both exited: %s", got)
	}
}

// Two watcher processes of one repository, killed in either order: while
// one runs, status never reads none, and a full scan by another process
// never clears reconcile_pending for the one that runs without the live lock.
func TestLiveWatcherTwoProcessesKilled(t *testing.T) {
	for _, killFirst := range []bool{true, false} {
		name := "second killed first"
		if killFirst {
			name = "first killed first"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			s, repoID := freshnessStore(t, root)
			completeFullScan(t, s, repoID)
			first := startWatchLockChild(t, s.path, root)
			if got := liveState(s, repoID); got != LiveWatcherWatching {
				t.Fatalf("first alone: %s", got)
			}
			second := startWatchLockChild(t, s.path, root)
			f := freshness(t, s, repoID)
			if f.LiveWatcher != LiveWatcherUnverified || !slices.Contains(f.Reasons, "reconcile_pending") {
				t.Fatalf("both running: live_watcher %s reasons %v", f.LiveWatcher, f.Reasons)
			}
			killed, survivor := second, LiveWatcherWatching
			if killFirst {
				killed, survivor = first, LiveWatcherUnverified
			}
			_ = killed.Process.Kill()
			_ = killed.Wait()
			completeFullScan(t, s, repoID)
			f = freshness(t, s, repoID)
			if f.LiveWatcher != survivor || slices.Contains(f.Reasons, "reconcile_pending") != (survivor == LiveWatcherUnverified) {
				t.Fatalf("one killed: live_watcher %s reasons %v, want %s", f.LiveWatcher, f.Reasons, survivor)
			}
		})
	}
}

// startWatchLockChild runs TestLiveWatcherAfterProcessKill's child in the
// watching state and returns once it reports ready; it is killed when the
// test ends.
func startWatchLockChild(t *testing.T, db, root string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestLiveWatcherAfterProcessKill$")
	cmd.Env = append(os.Environ(), "CODEGRAPH_WATCH_LOCK_CHILD_DB="+db, "CODEGRAPH_WATCH_LOCK_CHILD_ROOT="+root, "CODEGRAPH_WATCH_LOCK_CHILD_STATE=watching")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("child: %q %v", line, err)
	}
	return cmd
}

// A lock that cannot be opened proves nothing: unknown, and an unverified
// lock that cannot be probed keeps reconcile_pending.
func TestLiveWatcherUnprobeableIsUnknown(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a permission-denied open")
	}
	s, repoID := freshnessStore(t, t.TempDir())
	completeFullScan(t, s, repoID)
	lock, err := s.OpenWatcherLock(repoID)
	if err != nil {
		t.Fatal(err)
	}
	lock.Close()
	for _, path := range []string{watchLivePath(s.path, repoID), watchLockPath(s.path, repoID)} {
		if err := os.Chmod(path, 0); err != nil {
			t.Fatal(err)
		}
		f := freshness(t, s, repoID)
		pending := path == watchLockPath(s.path, repoID)
		if f.LiveWatcher != freshnessUnknown || slices.Contains(f.Reasons, "reconcile_pending") != pending {
			t.Fatalf("%s unprobeable: live_watcher %s reasons %v", path, f.LiveWatcher, f.Reasons)
		}
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

// Concurrent status reads in one process must not take each other's
// probe of the unverified lock for a watcher.
func TestLiveWatcherConcurrentReadsDoNotReadHolder(t *testing.T) {
	s, repoID := freshnessStore(t, t.TempDir())
	completeFullScan(t, s, repoID)
	lock, err := s.OpenWatcherLock(repoID)
	if err != nil {
		t.Fatal(err)
	}
	lock.Close()
	// Each probe keeps what it takes for a while, as on a slow disk.
	probeLockFile = func(f *os.File, exclusive bool) (bool, error) {
		ok, err := lockFile(f, exclusive)
		if ok {
			time.Sleep(3 * scanLockProbeWait)
		}
		return ok, err
	}
	t.Cleanup(func() { probeLockFile = lockFile })
	var wg sync.WaitGroup
	errs := make(chan string, 4)
	for range 4 {
		wg.Go(func() {
			for range 5 {
				f, err := s.FreshnessStatus(context.Background(), repoID)
				if err != nil {
					errs <- err.Error()
					return
				}
				if f.LiveWatcher != LiveWatcherNone || slices.Contains(f.Reasons, "reconcile_pending") {
					errs <- f.LiveWatcher + " " + f.Verdict
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatalf("concurrent reads saw a watcher: %s", e)
	}
}

// TestLiveWatcherAfterProcessKill runs the watcher lock protocol in a child
// process and kills it: a crashed watcher leaves no claim, only the persisted
// reconcile_pending marker when it had not reconciled.
func TestLiveWatcherAfterProcessKill(t *testing.T) {
	if db := os.Getenv("CODEGRAPH_WATCH_LOCK_CHILD_DB"); db != "" {
		s, err := Open(db)
		if err != nil {
			os.Exit(2)
		}
		repo, found, err := s.FindRepo(context.Background(), os.Getenv("CODEGRAPH_WATCH_LOCK_CHILD_ROOT"))
		if err != nil || !found {
			os.Exit(2)
		}
		lock, err := s.OpenWatcherLock(repo.ID)
		if err != nil || lock.Hold() != nil || lock.HoldLive() != nil {
			os.Exit(2)
		}
		if os.Getenv("CODEGRAPH_WATCH_LOCK_CHILD_STATE") == "watching" {
			lock.Release()
		} else if s.MarkReconcilePending(context.Background(), repo.ID) != nil {
			os.Exit(2)
		}
		os.Stdout.WriteString("ready\n")
		// Unreachable, lock and s would be finalized and their lock fds closed while sleeping.
		time.Sleep(time.Minute)
		runtime.KeepAlive(lock)
		runtime.KeepAlive(s)
		os.Exit(0)
	}
	for _, state := range []string{"watching", "unverified"} {
		t.Run(state, func(t *testing.T) {
			root := t.TempDir()
			s, repoID := freshnessStore(t, root)
			completeFullScan(t, s, repoID)
			cmd := exec.Command(os.Args[0], "-test.run=^TestLiveWatcherAfterProcessKill$")
			cmd.Env = append(os.Environ(), "CODEGRAPH_WATCH_LOCK_CHILD_DB="+s.path, "CODEGRAPH_WATCH_LOCK_CHILD_ROOT="+root, "CODEGRAPH_WATCH_LOCK_CHILD_STATE="+state)
			out, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
			if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "ready\n" {
				t.Fatalf("child: %q %v", line, err)
			}
			f := freshness(t, s, repoID)
			if f.LiveWatcher != state || slices.Contains(f.Reasons, "reconcile_pending") != (state == "unverified") {
				t.Fatalf("live child: live_watcher %s reasons %v", f.LiveWatcher, f.Reasons)
			}
			if err := cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			_ = cmd.Wait()
			f = freshness(t, s, repoID)
			if f.LiveWatcher != LiveWatcherNone || slices.Contains(f.Reasons, "reconcile_pending") != (state == "unverified") {
				t.Fatalf("killed child: live_watcher %s reasons %v", f.LiveWatcher, f.Reasons)
			}
			completeFullScan(t, s, repoID)
			if f := freshness(t, s, repoID); f.Verdict != graph.FreshnessVerdictConsistentWithLastScan {
				t.Fatalf("after a full scan: verdict %s reasons %v", f.Verdict, f.Reasons)
			}
		})
	}
}

func TestParserProfileCompatibilityClassification(t *testing.T) {
	current := map[string]parser.Profile{
		"go":     {ID: "treesitter:go:v3", EmitsCallEdges: true},
		"java":   {ID: "heuristic:java:v1"},
		"python": {ID: "treesitter:python:v2", EmitsCallEdges: true},
		"rust":   {ID: "treesitter:rust:v2", EmitsCallEdges: true},
		"ruby":   {ID: "treesitter:ruby:v1", EmitsCallEdges: true},
		"swift":  {ID: "treesitter:swift:v6", EmitsCallEdges: true},
	}
	cases := []struct {
		name          string
		groups        []FileParserProfileGroup
		state, reason string
	}{
		{"same", []FileParserProfileGroup{{Language: "go", Profile: "treesitter:go:v3", CallEdges: true, Files: 2}}, "current", "matches_this_binary"},
		{"empty", nil, "current", "matches_this_binary"},
		{"older version", []FileParserProfileGroup{{Language: "go", Profile: "treesitter:go:v2", CallEdges: true, Files: 2}}, "obsolete", "older_version"},
		{"newer version", []FileParserProfileGroup{{Language: "go", Profile: "treesitter:go:v4", CallEdges: true, Files: 2}}, "unknown", "newer_version"},
		{"other family", []FileParserProfileGroup{{Language: "go", Profile: "go-ast:go:v3", CallEdges: true, Files: 2}}, "unknown", "other_parser_family"},
		{"call-capable data, symbols-only binary", []FileParserProfileGroup{{Language: "java", Profile: "treesitter:java:v5", CallEdges: true, Files: 2}}, "unknown", "call_capable_build_required"},
		{"unrecorded, symbols-only binary", []FileParserProfileGroup{{Language: "java", Profile: "", Files: 2}}, "unknown", "call_capable_build_required"},
		{"unrecorded", []FileParserProfileGroup{{Language: "rust", Profile: "", Files: 2}}, "unknown", "unrecorded_provenance"},
		{"no adapter", []FileParserProfileGroup{{Language: "dart", Profile: "treesitter:dart:v1", CallEdges: true, Files: 2}}, "unknown", "no_adapter"},
		{"obsolete outranks unknown", []FileParserProfileGroup{
			{Language: "dart", Profile: "treesitter:dart:v1", Files: 1},
			{Language: "swift", Profile: "treesitter:swift:v5", CallEdges: true, Files: 1},
		}, "obsolete", "older_version"},
		{"mixed with current", []FileParserProfileGroup{
			{Language: "python", Profile: "treesitter:python:v2", CallEdges: true, Files: 3},
			{Language: "python", Profile: "treesitter:python:v1", CallEdges: true, Files: 1},
		}, "obsolete", "older_version"},
		{"older and other family", []FileParserProfileGroup{
			{Language: "go", Profile: "treesitter:go:v2", CallEdges: true, Files: 3},
			{Language: "go", Profile: "regex:go:v1", Files: 1},
		}, "unknown", "other_parser_family"},
		{"older and newer", []FileParserProfileGroup{
			{Language: "go", Profile: "treesitter:go:v2", CallEdges: true, Files: 3},
			{Language: "go", Profile: "treesitter:go:v4", CallEdges: true, Files: 1},
		}, "unknown", "newer_version"},
		{"older and unrecorded", []FileParserProfileGroup{
			{Language: "rust", Profile: "treesitter:rust:v1", CallEdges: true, Files: 3},
			{Language: "rust", Profile: "", Files: 1},
		}, "unknown", "unrecorded_provenance"},
		{"older and call-capable data, symbols-only binary", []FileParserProfileGroup{
			{Language: "java", Profile: "heuristic:java:v0", Files: 3},
			{Language: "java", Profile: "treesitter:java:v5", CallEdges: true, Files: 1},
		}, "unknown", "call_capable_build_required"},
		{"refused downgrade outranks every other reason", []FileParserProfileGroup{
			{Language: "java", Profile: "", Files: 1},
			{Language: "java", Profile: "heuristic:java:v9", Files: 1},
			{Language: "java", Profile: "regex:java:v1", Files: 1},
			{Language: "java", Profile: "treesitter:java:v5", CallEdges: true, Files: 1},
		}, "unknown", "call_capable_build_required"},
		{"other family outranks newer", []FileParserProfileGroup{
			{Language: "go", Profile: "treesitter:go:v4", CallEdges: true, Files: 1},
			{Language: "go", Profile: "go-ast:go:v3", CallEdges: true, Files: 1},
			{Language: "go", Profile: "", Files: 1},
		}, "unknown", "other_parser_family"},
		{"symbols-only data for a symbols-only binary", []FileParserProfileGroup{{Language: "java", Profile: "heuristic:java:v0", Files: 1}}, "obsolete", "older_version"},
		{"no files", []FileParserProfileGroup{{Language: "ruby", Profile: "treesitter:ruby:v0", Files: 0}}, "current", "matches_this_binary"},
	}
	for _, c := range cases {
		// Every order of the groups classifies alike.
		for _, groups := range permutations(c.groups) {
			got := parserProfileCompatibility(groups, current)
			if got.State != c.state || got.Reason != c.reason {
				t.Errorf("%s: %s/%s, want %s/%s (%+v)", c.name, got.State, got.Reason, c.state, c.reason, got.Languages)
			}
			if (got.State == "current") != (len(got.Languages) == 0) {
				t.Errorf("%s: languages %+v for state %s", c.name, got.Languages, got.State)
			}
		}
	}
}

func permutations[T any](in []T) [][]T {
	if len(in) <= 1 {
		return [][]T{slices.Clone(in)}
	}
	var out [][]T
	for i := range in {
		rest := append(slices.Clone(in[:i]), in[i+1:]...)
		for _, p := range permutations(rest) {
			out = append(out, append([]T{in[i]}, p...))
		}
	}
	return out
}

// Stored provenance is judged against the parsers of the binary reading it:
// an older version of its own parser is known staleness, while data from a
// richer build read by a build without cgo is never called obsolete or
// current.
func TestFreshnessParserProfileCompatibility(t *testing.T) {
	ctx := context.Background()
	s, repoID := freshnessStore(t, t.TempDir())
	scanID, started, err := s.BeginScanWithScope(ctx, repoID, "index", ScanScopeFull, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range []struct{ path, language string }{{"a.go", "go"}, {"A.java", "java"}} {
		parsed := graph.ParsedFile{Symbols: []graph.Symbol{{Language: file.language, Kind: "function", Name: "f", QualifiedName: "f" + file.path, StableKey: "f" + file.path, Range: graph.Position{StartLine: 1, EndLine: 2}}}}
		if err := s.ReplaceFileGraph(ctx, repoID, scanID, file.path, file.language, 1, 1, file.path, parsed); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.CompleteScan(ctx, scanID, ScanSummary{RepoID: repoID, ScanID: scanID}, started, "completed", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx, `UPDATE files SET parser_profile = 'treesitter:' || language || ':v2', parser_call_edges = 1`); err != nil {
		t.Fatal(err)
	}
	if f := freshness(t, s, repoID); f.ParserProfileCompatibility.State != freshnessUnknown || f.ParserProfileCompatibility.Reason != "no_parser_registry" || f.Verdict != graph.FreshnessVerdictConsistentWithLastScan {
		t.Fatalf("no registry: %+v verdict %s", f.ParserProfileCompatibility, f.Verdict)
	}
	cgo := map[string]parser.Profile{"go": {ID: "treesitter:go:v2", EmitsCallEdges: true}, "java": {ID: "treesitter:java:v2", EmitsCallEdges: true}}
	noCGO := map[string]parser.Profile{"go": {ID: "go-ast:go:v3", EmitsCallEdges: true}, "java": {ID: "heuristic:java:v1"}}
	upgraded := map[string]parser.Profile{"go": {ID: "treesitter:go:v2", EmitsCallEdges: true}, "java": {ID: "treesitter:java:v3", EmitsCallEdges: true}}

	s.SetParserProfiles(cgo)
	if f := freshness(t, s, repoID); f.ParserProfileCompatibility.State != "current" || f.Verdict != graph.FreshnessVerdictConsistentWithLastScan {
		t.Fatalf("same build: %+v verdict %s", f.ParserProfileCompatibility, f.Verdict)
	}
	s.SetParserProfiles(noCGO)
	f := freshness(t, s, repoID)
	if f.ParserProfileCompatibility.State != freshnessUnknown || len(f.ParserProfileCompatibility.Languages) != 2 || f.Verdict != graph.FreshnessVerdictConsistentWithLastScan {
		t.Fatalf("build without cgo: %+v verdict %s", f.ParserProfileCompatibility, f.Verdict)
	}
	s.SetParserProfiles(upgraded)
	f = freshness(t, s, repoID)
	want := []graph.FreshnessParserLanguage{{Language: "java", State: "obsolete", Reason: "older_version", Stored: []string{"treesitter:java:v2"}, Current: "treesitter:java:v3"}}
	if f.ParserProfileCompatibility.State != "obsolete" || !slices.EqualFunc(f.ParserProfileCompatibility.Languages, want, func(a, b graph.FreshnessParserLanguage) bool {
		return a.Language == b.Language && a.State == b.State && a.Reason == b.Reason && slices.Equal(a.Stored, b.Stored) && a.Current == b.Current
	}) {
		t.Fatalf("upgraded build: %+v", f.ParserProfileCompatibility)
	}
	if f.Verdict != graph.FreshnessVerdictStaleKnown || f.State != graph.FreshnessKnownStale || !slices.Contains(f.Reasons, "parser_profile_obsolete") || f.ParserProfile != "converged" {
		t.Fatalf("upgraded build: verdict %s state %s reasons %v parser_profile %s", f.Verdict, f.State, f.Reasons, f.ParserProfile)
	}
}

// A read-only store serves a snapshot, yet it probes the live database's
// watcher locks, never creating them.
func TestLiveWatcherSeenByReadOnlyStore(t *testing.T) {
	s, repoID := freshnessStore(t, t.TempDir())
	completeFullScan(t, s, repoID)
	readOnly := func() graph.Freshness {
		t.Helper()
		ro, err := OpenReadOnly(s.path, OpenOptions{})
		if err != nil {
			t.Fatal(err)
		}
		defer ro.Close()
		f, err := ro.FreshnessStatus(context.Background(), repoID)
		if err != nil {
			t.Fatal(err)
		}
		return f
	}
	if f := readOnly(); f.LiveWatcher != LiveWatcherNone {
		t.Fatalf("no watcher: %s", f.LiveWatcher)
	}
	for _, path := range []string{watchLockPath(s.path, repoID), watchLivePath(s.path, repoID)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("a read created %s: %v", path, err)
		}
	}
	lock, err := s.OpenWatcherLock(repoID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(lock.Close)
	if err := lock.Hold(); err != nil {
		t.Fatal(err)
	}
	if err := lock.HoldLive(); err != nil {
		t.Fatal(err)
	}
	if f := readOnly(); f.LiveWatcher != LiveWatcherUnverified || !slices.Contains(f.Reasons, "reconcile_pending") {
		t.Fatalf("reconciling watcher: %s %v", f.LiveWatcher, f.Reasons)
	}
	lock.Release()
	if f := readOnly(); f.LiveWatcher != LiveWatcherWatching || f.Verdict != graph.FreshnessVerdictConsistentWithLastScan {
		t.Fatalf("watching: %s %s %v", f.LiveWatcher, f.Verdict, f.Reasons)
	}
}

// A watcher keeps holding its unverified lock file after the file is removed,
// out of sight of status reads. A live lock file without the unverified one
// is never written by a watcher, so the read fails closed instead of
// reporting a reconciled watcher, even after a full scan by another handle.
func TestLiveWatcherMissingUnverifiedLockIsUnknown(t *testing.T) {
	for _, state := range []string{"recovering", "exited"} {
		t.Run(state, func(t *testing.T) {
			s, repoID := freshnessStore(t, t.TempDir())
			completeFullScan(t, s, repoID)
			reader := openSameStore(t, s)
			lock, err := s.OpenWatcherLock(repoID)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(lock.Close)
			if err := lock.Hold(); err != nil {
				t.Fatal(err)
			}
			if err := lock.HoldLive(); err != nil {
				t.Fatal(err)
			}
			if err := os.Remove(watchLockPath(s.path, repoID)); err != nil {
				t.Skipf("cannot remove a held lock file here: %v", err)
			}
			if state == "exited" {
				lock.Close()
			}
			completeFullScan(t, s, repoID)
			f := freshness(t, reader, repoID)
			if f.LiveWatcher != freshnessUnknown || !slices.Contains(f.Reasons, "reconcile_pending") || f.Verdict == graph.FreshnessVerdictConsistentWithLastScan {
				t.Fatalf("live_watcher %s verdict %s reasons %v", f.LiveWatcher, f.Verdict, f.Reasons)
			}
		})
	}
}

// A lock call that fails, as on a file system without OS locks, proves
// nothing about a watcher: unknown, and reconcile_pending is kept.
func TestLiveWatcherLockCallErrorIsUnknown(t *testing.T) {
	s, repoID := freshnessStore(t, t.TempDir())
	completeFullScan(t, s, repoID)
	lock, err := s.OpenWatcherLock(repoID)
	if err != nil {
		t.Fatal(err)
	}
	lock.Close()
	probeLockFile = func(*os.File, bool) (bool, error) { return false, errors.New("no locks here") }
	t.Cleanup(func() { probeLockFile = lockFile })
	f := freshness(t, s, repoID)
	if f.LiveWatcher != freshnessUnknown || !slices.Contains(f.Reasons, "reconcile_pending") || f.Verdict == graph.FreshnessVerdictConsistentWithLastScan {
		t.Fatalf("live_watcher %s verdict %s reasons %v", f.LiveWatcher, f.Verdict, f.Reasons)
	}
}

// A second watcher waiting for the live lock takes it when the first exits
// within its wait, and is then reported like any other watcher.
func TestLiveWatcherSecondTakesOverWhenFirstExits(t *testing.T) {
	s, repoID := freshnessStore(t, t.TempDir())
	completeFullScan(t, s, repoID)
	first, _ := s.OpenWatcherLock(repoID)
	second, _ := s.OpenWatcherLock(repoID)
	t.Cleanup(first.Close)
	t.Cleanup(second.Close)
	if err := first.Hold(); err != nil {
		t.Fatal(err)
	}
	if err := first.HoldLive(); err != nil {
		t.Fatal(err)
	}
	first.Release()
	if err := second.Hold(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- second.HoldLive() }()
	first.Close()
	if err := <-done; err != nil || !second.liveHeld {
		t.Fatalf("second watcher did not take the live lock: %v", err)
	}
	if got := liveState(s, repoID); got != LiveWatcherUnverified {
		t.Fatalf("second reconciling: %s", got)
	}
	second.Release()
	if got := liveState(s, repoID); got != LiveWatcherWatching {
		t.Fatalf("second watching: %s", got)
	}
}
