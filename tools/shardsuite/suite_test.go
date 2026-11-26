package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// These tests run real `go test` processes against testdata/fixture, a
// separate module, so nothing here depends on the size of the main suite.

func TestMain(m *testing.M) {
	if os.Getenv("SHARDSUITE_AS_MAIN") == "1" {
		main()
	}
	os.Exit(m.Run())
}

func fixtureDir(t *testing.T) string {
	t.Helper()
	if unsupportedOS(runtime.GOOS) != nil {
		t.Skip("the runner refuses this OS")
	}
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go command not on PATH")
	}
	d, err := filepath.Abs(filepath.Join("testdata", "fixture"))
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func fixtureConfig(t *testing.T) config {
	return config{
		dir: fixtureDir(t), out: t.TempDir(), patterns: []string{"./..."},
		shards:  []shardSpec{{Pattern: "./a", K: 3}},
		timeout: time.Minute, jobTimeout: 2 * time.Minute, listTimeout: 2 * time.Minute,
	}
}

func fixtureEnv(t *testing.T, kv ...string) {
	for _, k := range []string{"FIXTURE_FAIL", "FIXTURE_HANG", "FIXTURE_PIDFILE", "FIXTURE_HANG_MAIN", "GOFLAGS"} {
		t.Setenv(k, "")
	}
	t.Setenv("GOWORK", "off")
	for i := 0; i < len(kv); i += 2 {
		t.Setenv(kv[i], kv[i+1])
	}
}

func jobByResult(sum summary, result string) *jobResult {
	for i := range sum.Jobs {
		if sum.Jobs[i].Result == result {
			return &sum.Jobs[i]
		}
	}
	return nil
}

func TestFixtureShardedRunMatchesUnshardedRun(t *testing.T) {
	fixtureEnv(t, "GOFLAGS", "-tags=fixturetag")
	cfg := fixtureConfig(t)
	base := filepath.Join(t.TempDir(), "base.jsonl")
	cmd := exec.Command("go", "test", "-json", "-count=1", "./...")
	cmd.Dir = cfg.dir
	b, err := cmd.Output()
	if err != nil {
		t.Fatalf("unsharded fixture run: %v", err)
	}
	if err := os.WriteFile(base, b, 0o644); err != nil {
		t.Fatal(err)
	}
	cfg.baseline = base
	sum, err := runSuite(context.Background(), cfg, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !sum.AllPassed || sum.Parity == nil || !sum.Parity.OK {
		t.Fatalf("summary: %+v parity %+v", sum, sum.Parity)
	}
	// rest (tagged + empty) and three shards of package a.
	if len(sum.Jobs) != 4 || sum.Jobs[0].Name != "rest" {
		t.Fatalf("jobs: %+v", sum.Jobs)
	}
	seen := map[string]int{}
	for _, j := range sum.Jobs[1:] {
		for _, n := range j.Expected["example.com/fixture/a"] {
			seen[n]++
		}
	}
	for _, n := range []string{"TestA", "TestAB", "TestÜber", "Test_under", "TestSub", "TestSkip", "FuzzEcho", "ExampleHello"} {
		if seen[n] != 1 {
			t.Errorf("%s assigned %d times", n, seen[n])
		}
	}
	if seen["BenchmarkHello"] != 0 {
		t.Error("benchmarks are not tests")
	}
	if !strings.Contains(strings.Join(sum.Jobs[0].Expected["example.com/fixture/tagged"], " "), "TestTagged") {
		t.Error("GOFLAGS build tags were not honored by the list")
	}
	// Subtests run inside their parent's shard, including names with a slash.
	if sum.Parity.BaseTests != sum.Parity.CandTests || sum.Parity.BaseTests < 15 {
		t.Errorf("parity counts %d/%d", sum.Parity.BaseTests, sum.Parity.CandTests)
	}
	for _, j := range sum.Jobs {
		dir := filepath.Join(cfg.out, "shards", j.Name)
		for _, f := range []string{"events.jsonl", "stderr.log", "result.json"} {
			if !fileExists(filepath.Join(dir, f)) {
				t.Errorf("%s: missing %s", j.Name, f)
			}
		}
	}
	if !fileExists(filepath.Join(cfg.out, "summary.json")) || !fileExists(filepath.Join(cfg.out, "plan.json")) {
		t.Error("missing summary.json or plan.json")
	}
	if code := run(context.Background(), []string{"parity", base, cfg.out}, io.Discard, io.Discard); code != 0 {
		t.Errorf("parity subcommand exit %d", code)
	}
}

func TestFixtureParityMismatchFailsAPassingRun(t *testing.T) {
	fixtureEnv(t)
	cfg := fixtureConfig(t)
	cfg.baseline = write(t, t.TempDir(), "base.jsonl",
		`{"Action":"pass","Package":"example.com/fixture/a","Test":"TestGone"}`)
	sum, err := runSuite(context.Background(), cfg, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if sum.AllPassed || sum.Parity == nil || sum.Parity.OK {
		t.Fatalf("every shard passed, but parity failed: %+v", sum.Parity)
	}
	for _, j := range sum.Jobs {
		if !j.passed() {
			t.Errorf("%s: %s", j.Name, j.Result)
		}
	}
}

func TestFixtureHangingListIsBounded(t *testing.T) {
	fixtureEnv(t, "FIXTURE_HANG_MAIN", "1")
	cfg := fixtureConfig(t)
	cfg.listTimeout = 10 * time.Second
	_, err := runSuite(context.Background(), cfg, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "-job-timeout") {
		t.Fatalf("want a bounded list, got %v", err)
	}
}

// A list run that loses a package (for example killed partway) must not
// shrink the inventory into a pass.
func TestFixtureTruncatedListFailsTheRun(t *testing.T) {
	fixtureEnv(t)
	realGo, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go command not on PATH")
	}
	bin := t.TempDir()
	wrapper := "#!/bin/sh\ncase \" $* \" in *\" -list \"*) \"" + realGo + "\" \"$@\" | grep -v fixture/tagged; exit 0 ;; esac\nexec \"" + realGo + "\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	_, err = runSuite(context.Background(), fixtureConfig(t), io.Discard)
	if err == nil || !strings.Contains(err.Error(), "example.com/fixture/tagged") {
		t.Fatalf("want the missing package named, got %v", err)
	}
}

func TestOutInsideModuleIsRefused(t *testing.T) {
	cfg := fixtureConfig(t)
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(cfg.dir, link); err != nil {
		t.Fatal(err)
	}
	for _, out := range []string{
		filepath.Join(cfg.dir, "out"),
		filepath.Join("testdata", "fixture", "out"),
		filepath.Join(link, "new", "out"),
	} {
		c := cfg
		c.out = out
		if _, err := runSuite(context.Background(), c, io.Discard); err == nil || !strings.Contains(err.Error(), "inside the module") {
			t.Fatalf("%s: got %v", out, err)
		}
		if _, err := os.Lstat(out); err == nil {
			t.Errorf("refused -out %s was created", out)
		}
	}
	if _, err := os.Lstat(filepath.Join(cfg.dir, "new")); err == nil {
		t.Error("refused -out parents were created")
	}
}

func TestInsideDirComparesFileIdentity(t *testing.T) {
	root := t.TempDir()
	mod := filepath.Join(root, "mod")
	for _, d := range []string{filepath.Join(mod, "pkg"), filepath.Join(root, "mod2")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cases := map[string]bool{
		mod:                                      true,
		filepath.Join(mod, "pkg", "x", "y"):      true,
		filepath.Join(root, "mod2", "out"):       false,
		filepath.Join(root, "out", "mod", "pkg"): false,
		root:                                     false,
	}
	// Windows creates symlinks only with a privilege or developer mode.
	symlinks := true
	if err := os.Symlink(mod, filepath.Join(root, "to-mod")); err != nil {
		if runtime.GOOS != "windows" {
			t.Fatal(err)
		}
		t.Logf("cannot create symlinks (%v): checking only the cases without one", err)
		symlinks = false
	}
	if symlinks {
		for name, target := range map[string]string{"to-pkg": filepath.Join(mod, "pkg"), "to-root": root, "dangling": filepath.Join(root, "missing")} {
			if err := os.Symlink(target, filepath.Join(root, name)); err != nil {
				t.Fatal(err)
			}
		}
		cases[filepath.Join(root, "to-mod", "x")] = true
		cases[filepath.Join(root, "to-pkg", "x", "y")] = true
		cases[filepath.Join(root, "to-root", "mod")] = true
		cases[filepath.Join(root, "to-root", "mod2")] = false
	}
	// APFS and NTFS are case-insensitive by default, most Linux filesystems are not.
	upper := filepath.Join(root, "MOD")
	if a, err := os.Stat(upper); err == nil {
		if b, _ := os.Stat(mod); os.SameFile(a, b) {
			cases[filepath.Join(upper, "out")] = true
		}
	} else {
		t.Log("case-sensitive filesystem: no case alias to check")
	}
	for path, want := range cases {
		got, err := insideDir(mod, path)
		if err != nil || got != want {
			t.Errorf("insideDir(%s) = %v, %v; want %v", path, got, err, want)
		}
	}
	if !symlinks {
		return
	}
	if _, err := insideDir(mod, filepath.Join(root, "dangling", "out")); err == nil {
		t.Error("a dangling symlink must not be judged outside the module")
	}
}

func TestClaimOutIsExclusive(t *testing.T) {
	out := filepath.Join(t.TempDir(), "out")
	errs := make([]error, 16)
	var wg sync.WaitGroup
	for i := range errs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = claimOut(out)
		}()
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case !strings.Contains(err.Error(), "choose a new -out"):
			t.Error(err)
		}
	}
	if won != 1 {
		t.Fatalf("%d runs claimed one -out", won)
	}
}

func TestConcurrentRunsShareNoOut(t *testing.T) {
	fixtureEnv(t)
	cfg := fixtureConfig(t)
	sums := make([]summary, 2)
	errs := make([]error, 2)
	var wg sync.WaitGroup
	for i := range sums {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sums[i], errs[i] = runSuite(context.Background(), cfg, io.Discard)
		}()
	}
	wg.Wait()
	winner := 0
	if errs[0] != nil {
		winner = 1
	}
	if loser := errs[1-winner]; loser == nil || !strings.Contains(loser.Error(), "choose a new -out") {
		t.Fatalf("both runs used one -out: %v", errs)
	}
	if errs[winner] != nil || !sums[winner].AllPassed {
		t.Fatalf("the run that claimed -out: %v %+v", errs[winner], sums[winner])
	}
	o, err := readOwner(cfg.out)
	if err != nil || o.RunID != sums[winner].RunID {
		t.Fatalf("owner %+v %v, summary run %s", o, err, sums[winner].RunID)
	}
}

// An owner record is never taken over, even when its run is long gone or
// died before it finished writing the record.
func TestUsedOutIsNeverReclaimed(t *testing.T) {
	stale := `{"run_id":"0123","pid":999999,"host":"elsewhere","started":"2020-01-01T00:00:00Z"}`
	for name, tc := range map[string]struct{ owner, want string }{
		"stale owner":   {stale, "already belongs to shardsuite run 0123"},
		"partial owner": {`{"run_`, "not empty"},
	} {
		cfg := fixtureConfig(t)
		p := write(t, cfg.out, ownerFile, tc.owner)
		link := filepath.Join(t.TempDir(), "link")
		if err := os.Symlink(cfg.out, link); err != nil {
			t.Fatal(err)
		}
		for _, out := range []string{cfg.out, link} {
			cfg.out = out
			if _, err := runSuite(context.Background(), cfg, io.Discard); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("%s via %s: got %v", name, out, err)
			}
		}
		if b, err := os.ReadFile(p); err != nil || string(b) != tc.owner+"\n" {
			t.Errorf("%s: owner record changed: %q %v", name, b, err)
		}
	}
}

func setHook(t *testing.T, hook *func(string), f func(string)) {
	old := *hook
	*hook = f
	t.Cleanup(func() { *hook = old })
}

func tree(t *testing.T, dir string) string {
	t.Helper()
	var paths []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err == nil {
			paths = append(paths, p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(paths, "\n")
}

// replaceOut moves the claimed -out aside and puts a directory owned by
// another run in its place.
func replaceOut(t *testing.T, out string) {
	t.Helper()
	if err := os.Rename(out, out+".claimed"); err != nil {
		t.Fatal(err)
	}
	write(t, out, ownerFile, `{"run_id":"foreign","pid":1,"host":"h","started":"s"}`)
}

// The claim is checked again once owner.json exists: a file that appeared in
// -out, or -out now leading to another directory, refuses the run before it
// writes anything there.
func TestOutChangedAfterClaimIsRefused(t *testing.T) {
	fixtureEnv(t)
	for name, change := range map[string]func(t *testing.T, cfg config){
		"foreign file": func(t *testing.T, cfg config) {
			write(t, cfg.out, "summary.json", `{"all_passed":true}`)
		},
		"alias into the module": func(t *testing.T, cfg config) {
			if err := os.Rename(cfg.out, cfg.out+".claimed"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(cfg.dir, cfg.out); err != nil {
				t.Fatal(err)
			}
		},
		"another run's directory": func(t *testing.T, cfg config) { replaceOut(t, cfg.out) },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := fixtureConfig(t)
			cfg.dir = filepath.Join(t.TempDir(), "module")
			if err := os.CopyFS(cfg.dir, os.DirFS(fixtureDir(t))); err != nil {
				t.Fatal(err)
			}
			cfg.out = filepath.Join(t.TempDir(), "out")
			module := tree(t, cfg.dir)
			setHook(t, &afterOwnerCreated, func(string) { change(t, cfg) })
			if _, err := runSuite(context.Background(), cfg, io.Discard); err == nil || !strings.Contains(err.Error(), "choose a new -out") {
				t.Errorf("got %v", err)
			}
			if got := tree(t, cfg.dir); got != module {
				t.Errorf("the module changed:\n%s", got)
			}
			for _, f := range []string{"plan.json", "list.jsonl", "shards"} {
				for _, dir := range []string{cfg.out, cfg.out + ".claimed"} {
					if _, err := os.Stat(filepath.Join(dir, f)); err == nil {
						t.Errorf("refused run wrote %s", filepath.Join(dir, f))
					}
				}
			}
			if b, err := os.ReadFile(filepath.Join(cfg.out, "summary.json")); err == nil && string(b) != "{\"all_passed\":true}\n" {
				t.Errorf("refused run wrote a summary: %s", b)
			}
		})
	}
}

// A run whose -out was taken over while it ran reports an error and writes no
// summary, neither into the new directory nor into the one it claimed.
func TestOwnerChangedDuringRunWritesNoSummary(t *testing.T) {
	fixtureEnv(t)
	cfg := fixtureConfig(t)
	cfg.out = filepath.Join(t.TempDir(), "out")
	setHook(t, &afterJobs, func(out string) { replaceOut(t, out) })
	if _, err := runSuite(context.Background(), cfg, io.Discard); err == nil || !strings.Contains(err.Error(), "not this run") {
		t.Fatalf("got %v", err)
	}
	for _, dir := range []string{cfg.out, cfg.out + ".claimed"} {
		if fileExists(filepath.Join(dir, "summary.json")) {
			t.Errorf("summary.json written in %s", dir)
		}
	}
}

func TestWindowsIsRefused(t *testing.T) {
	if unsupportedOS("windows") == nil {
		t.Error("windows: want a refusal")
	}
	for _, goos := range []string{"darwin", "linux"} {
		if err := unsupportedOS(goos); err != nil {
			t.Errorf("%s: %v", goos, err)
		}
	}
}

// A failed run into a reused -out must not leave the previous run's passing
// summary and shards behind for `parity` or a reader to pick up.
func TestNonEmptyOutIsRefused(t *testing.T) {
	cfg := fixtureConfig(t)
	stale := write(t, cfg.out, "summary.json", `{"all_passed":true}`)
	if _, err := runSuite(context.Background(), cfg, io.Discard); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("got %v", err)
	}
	if b, err := os.ReadFile(stale); err != nil || string(b) != "{\"all_passed\":true}\n" {
		t.Errorf("refused -out was modified: %q %v", b, err)
	}
}

func TestFixtureFailingTestFailsTheRun(t *testing.T) {
	fixtureEnv(t, "FIXTURE_FAIL", "1")
	sum, err := runSuite(context.Background(), fixtureConfig(t), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if sum.AllPassed || jobByResult(sum, resTestFail) == nil {
		t.Fatalf("want a test_fail job: %+v", sum.Jobs)
	}
}

func TestFixtureBuildFailureFailsTheRun(t *testing.T) {
	fixtureEnv(t, "GOFLAGS", "-tags=fixturebroken")
	sum, err := runSuite(context.Background(), fixtureConfig(t), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	j := jobByResult(sum, resBuildFail)
	if sum.AllPassed || j == nil || j.Name != "rest" {
		t.Fatalf("want the unlistable package to fail its build in the rest job: %+v", sum.Jobs)
	}
}

func TestFixtureTestTimeoutIsATimeout(t *testing.T) {
	fixtureEnv(t, "FIXTURE_HANG", "1")
	cfg := fixtureConfig(t)
	cfg.timeout = 3 * time.Second
	sum, err := runSuite(context.Background(), cfg, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if j := jobByResult(sum, resTimeout); sum.AllPassed || j == nil || j.Reason != "" {
		t.Fatalf("want a go test timeout: %+v", sum.Jobs)
	}
}

func readPID(path string) int {
	deadline := time.Now().Add(time.Minute)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(string(b)); err == nil {
				return pid
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return 0
}

// onFile runs fn once path has content, unless ctx ends first. The poller is
// joined before the test ends, so a skipped or failed test leaves none behind.
func onFile(t *testing.T, ctx context.Context, path string, fn func()) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			if b, err := os.ReadFile(path); err == nil && len(b) > 0 {
				fn()
				return
			}
			select {
			case <-ctx.Done():
			case <-time.After(20 * time.Millisecond):
			}
		}
	}()
	t.Cleanup(func() { <-done })
}

func waitForPID(t *testing.T, path string) int {
	t.Helper()
	pid := readPID(path)
	if pid == 0 {
		t.Fatal("hanging test never started")
	}
	return pid
}

func TestFixtureJobTimeoutKillsTheTestBinary(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	fixtureEnv(t, "FIXTURE_HANG", "1", "FIXTURE_PIDFILE", pidFile)
	cfg := fixtureConfig(t)
	// Covers linking the fixture binary too, so it leaves room for a loaded machine.
	cfg.jobTimeout = 25 * time.Second
	sum, err := runSuite(context.Background(), cfg, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if j := jobByResult(sum, resTimeout); sum.AllPassed || j == nil || j.Reason == "" {
		t.Fatalf("want a killed job: %+v", sum.Jobs)
	}
	// Under heavy load the kill can land before TestHang starts; the
	// classification above still holds, there is just no binary to check.
	b, err := os.ReadFile(pidFile)
	if err != nil {
		t.Log("killed before the hanging test started; skipping the process check")
		return
	}
	pid, err := strconv.Atoi(string(b))
	if err != nil {
		t.Fatal(err)
	}
	assertGone(t, pid)
}

func TestFixtureInterruptReportsIncomplete(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	fixtureEnv(t, "FIXTURE_HANG", "1", "FIXTURE_PIDFILE", pidFile)
	cfg := fixtureConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	onFile(t, ctx, pidFile, cancel)
	sum, err := runSuite(ctx, cfg, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !sum.Interrupted || sum.AllPassed {
		t.Fatalf("summary: %+v", sum)
	}
	// Shards that finished before the interrupt keep their result; the one
	// that was cut off is incomplete, and the run as a whole is not a pass.
	if j := jobByResult(sum, resIncomplete); j == nil || j.Reason != "interrupted" {
		t.Errorf("no interrupted job: %+v", sum.Jobs)
	}
	var onDisk summary
	b, err := os.ReadFile(filepath.Join(cfg.out, "summary.json"))
	if err != nil || json.Unmarshal(b, &onDisk) != nil || !onDisk.Interrupted || onDisk.AllPassed {
		t.Fatalf("summary.json after interrupt: %s %v", b, err)
	}
	if o, err := readOwner(cfg.out); err != nil || onDisk.RunID == "" || o.RunID != onDisk.RunID {
		t.Errorf("summary run %q, owner %+v %v", onDisk.RunID, o, err)
	}
	assertGone(t, waitForPID(t, pidFile))
	if _, err := runSuite(context.Background(), cfg, io.Discard); err == nil || !strings.Contains(err.Error(), onDisk.RunID) {
		t.Errorf("rerun into an interrupted run's -out: %v", err)
	}
}

func TestFixtureInterruptLeavesQueuedJobsUnstarted(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")
	fixtureEnv(t, "FIXTURE_HANG", "1", "FIXTURE_PIDFILE", pidFile)
	cfg := fixtureConfig(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	onFile(t, ctx, pidFile, cancel)
	// Round-robin over sorted names puts TestHang, the sixth, in a.0.
	cfg.shards, cfg.jobs = []shardSpec{{Pattern: "./a", K: 5}}, 1
	sum, err := runSuite(ctx, cfg, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, j := range sum.Jobs {
		names = append(names, j.Name+":"+j.Result+":"+j.Reason)
	}
	want := []string{"rest:pass:", "fixture_a.0:incomplete:interrupted"}
	for i := 1; i < 5; i++ {
		want = append(want, "fixture_a."+strconv.Itoa(i)+":incomplete:"+reasonNotStarted)
	}
	if strings.Join(names, " ") != strings.Join(want, " ") || sum.AllPassed {
		t.Fatalf("jobs:\n%s\nwant:\n%s", strings.Join(names, "\n"), strings.Join(want, "\n"))
	}
	for _, j := range sum.Jobs[2:] {
		if fileExists(filepath.Join(cfg.out, "shards", j.Name, "events.jsonl")) {
			t.Errorf("%s ran", j.Name)
		}
	}
}

func TestFixtureJobsLimitIsHonored(t *testing.T) {
	fixtureEnv(t)
	for _, limit := range []int{1, 2} {
		cfg := fixtureConfig(t)
		cfg.jobs = limit
		sum, err := runSuite(context.Background(), cfg, io.Discard)
		if err != nil || !sum.AllPassed || sum.MaxJobs != limit || len(sum.Jobs) != 4 {
			t.Fatalf("-jobs=%d: %v %+v", limit, err, sum)
		}
		for _, a := range sum.Jobs {
			running := 0
			for _, b := range sum.Jobs {
				end := b.Started.Add(time.Duration(b.WallSeconds * float64(time.Second)))
				if !a.Started.Before(b.Started) && a.Started.Before(end) {
					running++
				}
			}
			if running > limit {
				t.Errorf("-jobs=%d: %d processes ran at once when %s started", limit, running, a.Name)
			}
		}
	}
}

func TestSIGINTKillsChildrenAndExitsIncomplete(t *testing.T) {
	dir := fixtureDir(t)
	pidFile := filepath.Join(t.TempDir(), "pid")
	out := t.TempDir()
	cmd := exec.Command(os.Args[0], "-C", dir, "-shard", "./a=2", "-out", out, "./...")
	cmd.Env = append(os.Environ(), "SHARDSUITE_AS_MAIN=1", "FIXTURE_HANG=1", "FIXTURE_PIDFILE="+pidFile, "FIXTURE_FAIL=", "FIXTURE_HANG_MAIN=", "GOFLAGS=", "GOWORK=off")
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	pid := waitForPID(t, pidFile)
	if err := cmd.Process.Signal(os.Interrupt); err != nil {
		t.Fatal(err)
	}
	err := cmd.Wait()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.ExitCode() != exitInterrupted {
		t.Fatalf("exit: %v\n%s", err, stdout.String())
	}
	if !strings.Contains(stdout.String(), "INCOMPLETE") {
		t.Errorf("stdout does not say incomplete:\n%s", stdout.String())
	}
	assertGone(t, pid)
}

func TestRunJobsBoundsConcurrency(t *testing.T) {
	jobs := make([]job, 7)
	for i := range jobs {
		jobs[i].Name = strconv.Itoa(i)
	}
	for _, limit := range []int{1, 2, 3} {
		var mu sync.Mutex
		running, peak, done, order := 0, 0, 0, []string{}
		runJobs(context.Background(), jobs, limit, func(j job) jobResult {
			mu.Lock()
			running++
			peak = max(peak, running)
			order = append(order, j.Name)
			mu.Unlock()
			// Hold the slot until every slot is taken (or no job is left to
			// take one), so the peak does not depend on scheduling.
			deadline := time.Now().Add(10 * time.Second)
			for time.Now().Before(deadline) {
				mu.Lock()
				full := running == min(limit, len(jobs)-done)
				mu.Unlock()
				if full {
					break
				}
				time.Sleep(time.Millisecond)
			}
			mu.Lock()
			running--
			done++
			mu.Unlock()
			return jobResult{job: j, Result: resPass}
		})
		if peak != limit {
			t.Errorf("limit %d: peak %d", limit, peak)
		}
		if limit == 1 && strings.Join(order, "") != "0123456" {
			t.Errorf("limit 1 started out of plan order: %v", order)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, r := range runJobs(ctx, jobs, 2, func(j job) jobResult { t.Error("started after cancel"); return jobResult{} }) {
		if r.Result != resIncomplete || r.Reason != reasonNotStarted {
			t.Errorf("%s: %s %s", r.Name, r.Result, r.Reason)
		}
	}
}

func TestJobDeadlineCoversEveryPackageTimeout(t *testing.T) {
	pkgs := func(n int) job { return job{Packages: make([]string, n)} }
	cfg := config{timeout: 20 * time.Minute, jobs: 11}
	for _, tc := range []struct {
		j    job
		jobs int
		want time.Duration
	}{
		{pkgs(1), 11, 26 * time.Minute},
		{pkgs(11), 11, 26 * time.Minute},
		{pkgs(50), 11, 5*21*time.Minute + 5*time.Minute},
		{pkgs(50), 1, 50*21*time.Minute + 5*time.Minute},
	} {
		cfg.jobs = tc.jobs
		if got := jobDeadline(cfg, tc.j); got != tc.want {
			t.Errorf("%d packages, -jobs=%d: %s, want %s", len(tc.j.Packages), tc.jobs, got, tc.want)
		}
	}
	for _, timeout := range []time.Duration{math.MaxInt64 / 10, math.MaxInt64 - time.Second} {
		cfg.timeout = timeout
		if got := jobDeadline(cfg, pkgs(50)); got != math.MaxInt64 {
			t.Errorf("-timeout %s: a deadline past the largest duration must saturate, got %s", timeout, got)
		}
	}
	cfg.jobTimeout = time.Hour
	if got := jobDeadline(cfg, pkgs(50)); got != time.Hour {
		t.Errorf("explicit -job-timeout: %s", got)
	}
}

func TestTestArgsPassJobsAsP(t *testing.T) {
	got := strings.Join(testArgs(config{jobs: 3, timeout: time.Minute}, job{Packages: []string{"./a"}, Run: "^(TestA)$"}), " ")
	if got != "test -json -count=1 -p 3 -timeout 1m0s -run ^(TestA)$ ./a" {
		t.Error(got)
	}
}

// An interrupt during the test listing writes its summary only while -out
// still belongs to the run, as at the end of a run.
func TestInterruptWhileListingChecksOwner(t *testing.T) {
	fixtureEnv(t)
	cfg := fixtureConfig(t)
	realGo, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go command not on PATH")
	}
	cfg.out = filepath.Join(t.TempDir(), "out")
	marker := filepath.Join(t.TempDir(), "listing")
	bin := t.TempDir()
	wrapper := "#!/bin/sh\ncase \" $* \" in *\" -list \"*) echo up > \"" + marker + "\"; exec sleep 60 ;; esac\nexec \"" + realGo + "\" \"$@\"\n"
	if err := os.WriteFile(filepath.Join(bin, "go"), []byte(wrapper), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var replaceErr error
	onFile(t, ctx, marker, func() {
		if replaceErr = os.Rename(cfg.out, cfg.out+".claimed"); replaceErr == nil {
			replaceErr = os.MkdirAll(cfg.out, 0o755)
		}
		if replaceErr == nil {
			replaceErr = os.WriteFile(filepath.Join(cfg.out, ownerFile), []byte(`{"run_id":"foreign","pid":1,"host":"h","started":"s"}`), 0o644)
		}
		cancel()
	})
	_, err = runSuite(ctx, cfg, io.Discard)
	cancel()
	if replaceErr != nil {
		t.Fatal(replaceErr)
	}
	if err == nil || !strings.Contains(err.Error(), "not this run") {
		t.Fatalf("got %v", err)
	}
	for _, dir := range []string{cfg.out, cfg.out + ".claimed"} {
		if fileExists(filepath.Join(dir, "summary.json")) {
			t.Errorf("summary.json written in %s", dir)
		}
	}
}

// A job whose second log cannot be created closes the first one; neither
// order may leak a handle (on Windows it would block removing -out).
func TestRunJobClosesLogsWhenOneCannotBeCreated(t *testing.T) {
	for _, blocked := range []string{"events.jsonl", "stderr.log"} {
		t.Run(blocked, func(t *testing.T) {
			var opened []*os.File
			old := createLog
			createLog = func(name string) (*os.File, error) {
				f, err := old(name)
				if err == nil {
					opened = append(opened, f)
				}
				return f, err
			}
			t.Cleanup(func() { createLog = old })
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, blocked), 0o755); err != nil {
				t.Fatal(err)
			}
			cfg := config{dir: t.TempDir(), timeout: time.Minute, jobs: 1}
			r := runJob(context.Background(), cfg, dir, job{Name: "j", Packages: []string{"./x"}})
			if r.Result != resIncomplete || !strings.Contains(r.Reason, blocked) {
				t.Fatalf("result %+v", r)
			}
			for _, f := range opened {
				if err := f.Close(); !errors.Is(err, os.ErrClosed) {
					t.Errorf("%s left open (close: %v)", f.Name(), err)
				}
			}
		})
	}
}
