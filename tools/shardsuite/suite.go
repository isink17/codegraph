package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"
)

const summarySchema = "codegraph.shardsuite/v1"

type summary struct {
	Schema      string        `json:"schema"`
	Patterns    []string      `json:"patterns"`
	GOOS        string        `json:"goos"`
	GOARCH      string        `json:"goarch"`
	GoFlags     string        `json:"goflags"`
	CGOEnabled  string        `json:"cgo_enabled"`
	Timeout     string        `json:"timeout_per_binary"`
	JobTimeout  string        `json:"job_timeout"`
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

func runSuite(ctx context.Context, cfg config, stdout io.Writer) (summary, error) {
	start := time.Now()
	sum := summary{
		Schema: summarySchema, Patterns: cfg.patterns, GOOS: runtime.GOOS, GOARCH: runtime.GOARCH,
		GoFlags: goEnv(cfg.dir, "GOFLAGS"), CGOEnabled: goEnv(cfg.dir, "CGO_ENABLED"),
		Timeout: cfg.timeout.String(), JobTimeout: cfg.jobTimeout.String(), Timings: cfg.timings,
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
	if dir, err := filepath.Abs(cfg.dir); err == nil {
		if rel, err := filepath.Rel(dir, out); err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return sum, fmt.Errorf("-out %s is inside the module; leftover shard temp trees would be picked up by ./...", out)
		}
	}
	if ents, err := os.ReadDir(out); err == nil && len(ents) > 0 {
		return sum, fmt.Errorf("-out %s is not empty; an earlier run's summary.json, plan.json and shards would read as this run's", out)
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return sum, err
	}

	// The list runs every test binary once, so it shares the run's build tags,
	// CGO setting and GOFLAGS, and each package's TestMain.
	listTmp := filepath.Join(out, "list-tmp")
	if err := os.MkdirAll(filepath.Join(listTmp, "go"), 0o755); err != nil {
		return sum, err
	}
	if cfg.listTimeout == 0 {
		cfg.listTimeout = cfg.jobTimeout
	}
	lctx, lcancel := context.WithTimeout(ctx, cfg.listTimeout)
	defer lcancel()
	lc := exec.CommandContext(lctx, "go", append([]string{"test", "-json", "-list", "."}, cfg.patterns...)...)
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
		return sum, fmt.Errorf("go test -list did not finish within -job-timeout %s", cfg.listTimeout)
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
	type planEntry struct {
		job
		Run      string              `json:"run,omitempty"`
		Expected map[string][]string `json:"expected_tests"`
	}
	plan := make([]planEntry, len(jobs))
	for i, j := range jobs {
		plan[i] = planEntry{j, j.Run, j.Expected}
	}
	if err := writeJSON(filepath.Join(out, "plan.json"), map[string]any{"jobs": plan, "timings": cfg.timings}); err != nil {
		return sum, err
	}
	fmt.Fprintf(stdout, "shardsuite: out=%s packages=%d jobs=%d timeout=%s\n", out, len(pkgs), len(jobs), cfg.timeout)

	results := make([]jobResult, len(jobs))
	var wg sync.WaitGroup
	var mu sync.Mutex
	for i, j := range jobs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = runJob(ctx, cfg, filepath.Join(out, "shards", j.Name), j)
			r := results[i]
			mu.Lock()
			fmt.Fprintf(stdout, "%s\t%s\t%.1fs\t%d tests", strings.ToUpper(r.Result), r.Name, r.WallSeconds, r.Counts.Pass+r.Counts.Fail+r.Counts.Skip)
			if r.Reason != "" {
				fmt.Fprintf(stdout, "\t%s", r.Reason)
			}
			fmt.Fprintln(stdout)
			mu.Unlock()
		}()
	}
	wg.Wait()
	sum.Jobs = results
	sum.Interrupted = ctx.Err() != nil
	sum.AllPassed = !sum.Interrupted && len(results) > 0
	for _, r := range results {
		sum.AllPassed = sum.AllPassed && r.passed()
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
	args := []string{"test", "-json", "-count=1", "-timeout", cfg.timeout.String()}
	if j.Run != "" {
		args = append(args, "-run", j.Run)
	}
	args = append(args, j.Packages...)
	jctx, cancel := context.WithTimeout(ctx, cfg.jobTimeout)
	defer cancel()
	cmd := exec.CommandContext(jctx, "go", args...)
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
	r.WallSeconds = wall
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
