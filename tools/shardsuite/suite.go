package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const summarySchema = "codegraph.shardsuite/v1"

type summary struct {
	Schema      string        `json:"schema"`
	RunID       string        `json:"run_id"`
	Patterns    []string      `json:"patterns"`
	GOOS        string        `json:"goos"`
	GOARCH      string        `json:"goarch"`
	GoFlags     string        `json:"goflags"`
	CGOEnabled  string        `json:"cgo_enabled"`
	Timeout     string        `json:"timeout_per_binary"`
	JobTimeout  string        `json:"job_timeout"`
	MaxJobs     int           `json:"max_parallel_jobs"`
	Timings     string        `json:"timings,omitempty"`
	Packages    int           `json:"packages"`
	TopLevel    int           `json:"top_level_tests_listed"`
	WallSeconds float64       `json:"wall_seconds"`
	Interrupted bool          `json:"interrupted"`
	Jobs        []jobResult   `json:"jobs"`
	Parity      *parityReport `json:"baseline_parity,omitempty"`
	AllPassed   bool          `json:"all_passed"`
}

func goEnv(dir, key string) string {
	cmd := exec.Command("go", "env", key)
	cmd.Dir = dir
	out, _ := cmd.Output()
	return strings.TrimSpace(string(out))
}

// isolatedEnv points every temp-dir variable go and the tests read at dir.
func isolatedEnv(dir string) []string {
	return append(os.Environ(), "TMPDIR="+dir, "TMP="+dir, "TEMP="+dir, "GOTMPDIR="+filepath.Join(dir, "go"))
}

// Killing a whole go test process tree on timeout or Ctrl-C has only been
// verified on macOS and Linux.
func unsupportedOS(goos string) error {
	if goos == "windows" {
		return errors.New("Windows is not supported: ending a go test process tree on timeout or interrupt is unverified there; run go test ./... instead")
	}
	return nil
}

func runSuite(ctx context.Context, cfg config, stdout io.Writer) (summary, error) {
	start := time.Now()
	if cfg.jobs < 1 {
		cfg.jobs = runtime.GOMAXPROCS(0)
	}
	jobTimeout := autoJobTimeout
	if cfg.jobTimeout > 0 {
		jobTimeout = cfg.jobTimeout.String()
	}
	sum := summary{
		Schema: summarySchema, Patterns: cfg.patterns, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
		GoFlags: goEnv(cfg.dir, "GOFLAGS"), CGOEnabled: goEnv(cfg.dir, "CGO_ENABLED"),
		Timeout: cfg.timeout.String(), JobTimeout: jobTimeout, MaxJobs: cfg.jobs, Timings: cfg.timings,
	}
	if err := unsupportedOS(runtime.GOOS); err != nil {
		return sum, err
	}
	out := cfg.out
	var err error
	if out == "" {
		if out, err = os.MkdirTemp("", "shardsuite."); err != nil {
			return sum, err
		}
	}
	if out, err = filepath.Abs(out); err != nil {
		return sum, err
	}
	inside, err := insideDir(cfg.dir, out)
	if err != nil {
		return sum, fmt.Errorf("cannot check -out %s against the module: %w", out, err)
	}
	if inside {
		return sum, fmt.Errorf("-out %s is inside the module; leftover shard temp trees would be picked up by ./...", out)
	}
	own, err := claimOut(out)
	if err != nil {
		return sum, err
	}
	sum.RunID = own.RunID

	// The list runs every test binary once, so it shares the run's build tags,
	// CGO setting and GOFLAGS, and each package's TestMain.
	listTmp := filepath.Join(out, "list-tmp")
	if err := os.MkdirAll(filepath.Join(listTmp, "go"), 0o755); err != nil {
		return sum, err
	}
	if cfg.listTimeout == 0 {
		cfg.listTimeout = cfg.jobTimeout
		if cfg.listTimeout == 0 {
			cfg.listTimeout = cfg.timeout + jobSlack
		}
	}
	lctx, lcancel := context.WithTimeout(ctx, cfg.listTimeout)
	defer lcancel()
	lc := exec.CommandContext(lctx, "go", append([]string{"test", "-json", "-p", strconv.Itoa(cfg.jobs), "-list", "."}, cfg.patterns...)...)
	listErrLog, err := os.Create(filepath.Join(out, "list-stderr.log"))
	if err != nil {
		return sum, err
	}
	lc.Dir, lc.Env, lc.Stderr = cfg.dir, isolatedEnv(listTmp), listErrLog
	setProcGroup(lc)
	lc.Cancel = func() error { return killTree(lc) }
	lc.WaitDelay = 10 * time.Second
	listOut, listErr := lc.Output()
	listErrLog.Close()
	if ctx.Err() != nil {
		sum.Interrupted = true
		_ = writeJSON(filepath.Join(out, "summary.json"), sum)
		fmt.Fprintf(stdout, "shardsuite: INCOMPLETE (interrupted while listing tests) out=%s\n", out)
		return sum, nil
	}
	_ = os.WriteFile(filepath.Join(out, "list.jsonl"), listOut, 0o644)
	if lctx.Err() != nil {
		return sum, fmt.Errorf("go test -list did not finish within %s (-job-timeout, or -timeout + 5m when it is not set)", cfg.listTimeout)
	}
	pkgs, names, failed, err := parseList(listOut)
	if err != nil {
		return sum, err
	}
	if len(pkgs) == 0 {
		return sum, fmt.Errorf("go test -list found no packages (%v); see %s", listErr, filepath.Join(out, "list-stderr.log"))
	}
	if listErr != nil && len(failed) == 0 {
		return sum, fmt.Errorf("go test -list failed without a failing package (%v); see %s", listErr, filepath.Join(out, "list-stderr.log"))
	}
	resolved, err := goList(cfg.dir, cfg.patterns...)
	if err != nil {
		return sum, err
	}
	if err := samePackages(pkgs, resolved); err != nil {
		return sum, err
	}
	sum.Packages = len(pkgs)
	for _, ns := range names {
		sum.TopLevel += len(ns)
	}
	heavy, err := resolvePackages(cfg.dir, cfg.shards)
	if err != nil {
		return sum, err
	}
	var timings map[string]map[string]float64
	if cfg.timings != "" {
		if timings, err = loadTimings(cfg.timings); err != nil {
			return sum, err
		}
	}
	jobs, err := buildJobs(pkgs, names, failed, heavy, timings)
	if err != nil {
		return sum, err
	}
	for i := range jobs {
		jobs[i].JobTimeout = jobDeadline(cfg, jobs[i]).String()
	}
	type planEntry struct {
		job
		Run      string              `json:"run,omitempty"`
		Expected map[string][]string `json:"expected_tests"`
	}
	plan := make([]planEntry, len(jobs))
	for i, j := range jobs {
		plan[i] = planEntry{j, j.Run, j.Expected}
	}
	if err := writeJSON(filepath.Join(out, "plan.json"), map[string]any{"run_id": own.RunID, "jobs": plan, "timings": cfg.timings}); err != nil {
		return sum, err
	}
	fmt.Fprintf(stdout, "shardsuite: out=%s packages=%d jobs=%d max_parallel=%d timeout=%s\n", out, len(pkgs), len(jobs), cfg.jobs, cfg.timeout)

	var mu sync.Mutex
	report := func(r jobResult) {
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(stdout, "%s\t%s\t%.1fs\t%d tests", strings.ToUpper(r.Result), r.Name, r.WallSeconds, r.Counts.Pass+r.Counts.Fail+r.Counts.Skip)
		if r.Reason != "" {
			fmt.Fprintf(stdout, "\t%s", r.Reason)
		}
		fmt.Fprintln(stdout)
	}
	results := runJobs(ctx, jobs, cfg.jobs, func(j job) jobResult {
		r := runJob(ctx, cfg, filepath.Join(out, "shards", j.Name), j)
		report(r)
		return r
	})
	for _, r := range results {
		if r.Reason == reasonNotStarted {
			report(r)
		}
	}
	sum.Jobs = results
	sum.Interrupted = ctx.Err() != nil
	sum.AllPassed = !sum.Interrupted && len(results) > 0
	for _, r := range results {
		sum.AllPassed = sum.AllPassed && r.passed()
	}
	if err := checkOwner(out, own.RunID); err != nil {
		return sum, err
	}
	if cfg.baseline != "" && !sum.Interrupted {
		var files []string
		for _, j := range jobs {
			files = append(files, filepath.Join(out, "shards", j.Name, "events.jsonl"))
		}
		p, err := compareBaseline(cfg.baseline, files)
		if err != nil {
			return sum, err
		}
		sum.Parity = &p
		sum.AllPassed = sum.AllPassed && p.OK
		printParity(stdout, p)
	}
	sum.WallSeconds = time.Since(start).Seconds()
	if err := writeJSON(filepath.Join(out, "summary.json"), sum); err != nil {
		return sum, err
	}
	status := "FAILED"
	switch {
	case sum.Interrupted:
		status = "INCOMPLETE (interrupted)"
	case sum.AllPassed:
		status = "passed"
	}
	fmt.Fprintf(stdout, "shardsuite: %s wall=%.1fs out=%s\n", status, sum.WallSeconds, out)
	for _, r := range results {
		if !r.passed() {
			fmt.Fprintf(stdout, "shardsuite: %s: %s %s\n", r.Result, r.Name, r.Reason)
			for _, p := range r.PkgResults {
				if p.Result != resPass && p.Result != resNoTests {
					fmt.Fprintf(stdout, "shardsuite:   %s: %s\n", p.Result, p.Package)
				}
			}
		}
	}
	return sum, nil
}

func runJob(ctx context.Context, cfg config, dir string, j job) jobResult {
	tmp := filepath.Join(dir, "tmp")
	if err := os.MkdirAll(filepath.Join(tmp, "go"), 0o755); err != nil {
		return jobResult{job: j, Result: resIncomplete, Reason: err.Error(), Exit: -1}
	}
	jctx, cancel := context.WithTimeout(ctx, jobDeadline(cfg, j))
	defer cancel()
	cmd := exec.CommandContext(jctx, "go", testArgs(cfg, j)...)
	cmd.Dir, cmd.Env = cfg.dir, isolatedEnv(tmp)
	setProcGroup(cmd)
	cmd.Cancel = func() error { return killTree(cmd) }
	cmd.WaitDelay = 10 * time.Second
	evPath := filepath.Join(dir, "events.jsonl")
	ev, err1 := os.Create(evPath)
	se, err2 := os.Create(filepath.Join(dir, "stderr.log"))
	if err := errors.Join(err1, err2); err != nil {
		return jobResult{job: j, Result: resIncomplete, Reason: err.Error(), Exit: -1}
	}
	cmd.Stdout, cmd.Stderr = ev, se
	start := time.Now()
	runErr := cmd.Run()
	wall := time.Since(start).Seconds()
	ev.Close()
	se.Close()
	exit := 0
	if cmd.ProcessState != nil {
		exit = cmd.ProcessState.ExitCode()
	} else if runErr != nil {
		exit = -1
	}
	interrupted := ctx.Err() != nil
	killed := !interrupted && errors.Is(jctx.Err(), context.DeadlineExceeded)
	evs, malformed, err := readEvents(evPath)
	if err != nil {
		return jobResult{job: j, Result: resIncomplete, Reason: err.Error(), Exit: exit}
	}
	r := classify(j, evs, malformed, exit, killed, interrupted)
	r.Started, r.WallSeconds = start, wall
	_ = writeJSON(filepath.Join(dir, "result.json"), r)
	return r
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

// jobSlack is the time a go test process gets beyond its packages' slots,
// for building and linking.
const jobSlack = 5 * time.Minute

const autoJobTimeout = "auto: (-timeout + 1m) x ceil(packages / -jobs) + 5m per process"

// jobDeadline bounds one go test process. Unless -job-timeout is set, it lets
// every package of the process run until go test itself gives up on it
// (-timeout, plus the minute go test waits before killing the binary), -jobs
// (go test -p) at a time, so a multi-package process is not cut short of its
// per-binary limit.
func jobDeadline(cfg config, j job) time.Duration {
	if cfg.jobTimeout > 0 {
		return cfg.jobTimeout
	}
	waves := time.Duration((max(len(j.Packages), 1) + cfg.jobs - 1) / cfg.jobs)
	slot := cfg.timeout + time.Minute
	if slot > (math.MaxInt64-jobSlack)/waves {
		return math.MaxInt64
	}
	return waves*slot + jobSlack
}

func testArgs(cfg config, j job) []string {
	args := []string{"test", "-json", "-count=1", "-p", strconv.Itoa(cfg.jobs), "-timeout", cfg.timeout.String()}
	if j.Run != "" {
		args = append(args, "-run", j.Run)
	}
	return append(args, j.Packages...)
}

const reasonNotStarted = "interrupted before it started"

// runJobs runs at most limit jobs at once, starting them in plan order. A job
// still waiting when ctx ends never starts and is reported incomplete.
func runJobs(ctx context.Context, jobs []job, limit int, run func(job) jobResult) []jobResult {
	results := make([]jobResult, len(jobs))
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i, j := range jobs {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
		}
		if ctx.Err() != nil {
			results[i] = jobResult{job: j, Result: resIncomplete, Reason: reasonNotStarted, Exit: -1}
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			results[i] = run(j)
		}()
	}
	wg.Wait()
	return results
}

const ownerFile = "owner.json"

// owner identifies the run that claimed an output directory.
type owner struct {
	RunID   string `json:"run_id"`
	PID     int    `json:"pid"`
	Host    string `json:"host"`
	Started string `json:"started"`
}

// claimOut makes out this run's own. owner.json is created exclusively before
// any other artifact, so two runs can never share one -out, and a used -out is
// never taken over, even when the run that claimed it is gone.
func claimOut(out string) (owner, error) {
	if ents, err := os.ReadDir(out); err == nil && len(ents) > 0 {
		return owner{}, usedOut(out)
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return owner{}, err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return owner{}, err
	}
	host, _ := os.Hostname()
	o := owner{RunID: hex.EncodeToString(nonce[:]), PID: os.Getpid(), Host: host, Started: time.Now().UTC().Format(time.RFC3339Nano)}
	f, err := os.OpenFile(filepath.Join(out, ownerFile), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, fs.ErrExist) {
		return owner{}, usedOut(out)
	}
	if err != nil {
		return owner{}, err
	}
	b, _ := json.Marshal(o)
	_, werr := f.Write(append(b, '\n'))
	if err := errors.Join(werr, f.Sync(), f.Close()); err != nil {
		return owner{}, err
	}
	// Something other than a runner may have written here after the check.
	if ents, err := os.ReadDir(out); err != nil || len(ents) != 1 {
		return owner{}, errors.Join(usedOut(out), err)
	}
	return o, nil
}

func usedOut(out string) error {
	msg := fmt.Sprintf("-out %s is not empty; an earlier run's summary.json, plan.json and shards would read as this run's", out)
	if o, err := readOwner(out); err == nil {
		msg = fmt.Sprintf("-out %s already belongs to shardsuite run %s (pid %d on %s, started %s); an output directory is never reused, even after its run ended", out, o.RunID, o.PID, o.Host, o.Started)
	}
	return errors.New(msg + "; choose a new -out")
}

func readOwner(out string) (owner, error) {
	var o owner
	b, err := os.ReadFile(filepath.Join(out, ownerFile))
	if err == nil {
		err = json.Unmarshal(b, &o)
	}
	if err == nil && o.RunID == "" {
		err = errors.New("no run_id")
	}
	if err != nil {
		return o, fmt.Errorf("%s has no complete %s, so it is not a claimed shardsuite output: %w", out, ownerFile, err)
	}
	return o, nil
}

func checkOwner(out, runID string) error {
	o, err := readOwner(out)
	if err != nil {
		return err
	}
	if o.RunID != runID {
		return fmt.Errorf("%s now belongs to run %s, not this run %s; its files are not this run's results", out, o.RunID, runID)
	}
	return nil
}

// insideDir reports whether path, or the part of it that exists, lies inside
// dir. It compares filesystem identity rather than spelling, so a symlink, a
// relative path or a different letter case on a case-insensitive filesystem
// cannot hide that path is in dir.
func insideDir(dir, path string) (bool, error) {
	di, err := os.Stat(dir)
	if err != nil {
		return false, err
	}
	p := filepath.Clean(path)
	for {
		if _, err := os.Lstat(p); err == nil {
			break
		}
		parent := filepath.Dir(p)
		if parent == p {
			break
		}
		p = parent
	}
	if p, err = filepath.EvalSymlinks(p); err != nil {
		return false, err
	}
	for {
		if st, err := os.Stat(p); err == nil && os.SameFile(di, st) {
			return true, nil
		}
		parent := filepath.Dir(p)
		if parent == p {
			return false, nil
		}
		p = parent
	}
}
