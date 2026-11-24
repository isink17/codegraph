package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
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
	cfg.out = filepath.Join(cfg.dir, "out")
	if _, err := runSuite(context.Background(), cfg, io.Discard); err == nil || !strings.Contains(err.Error(), "inside the module") {
		t.Fatalf("got %v", err)
	}
	if _, err := os.Stat(cfg.out); err == nil {
		t.Error("refused -out was created")
	}
}

// A failed run into a reused -out must not leave the previous run's passing
// summary and shards behind for `parity` or a reader to pick up.
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
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		readPID(pidFile)
		cancel()
	}()
	cfg := fixtureConfig(t)
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
	assertGone(t, waitForPID(t, pidFile))
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
