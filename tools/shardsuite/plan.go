package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

type config struct {
	dir        string
	out        string
	patterns   []string
	shards     []shardSpec
	timings    string
	baseline   string
	timeout    time.Duration
	jobTimeout time.Duration
	// listTimeout bounds the list step; it defaults to jobTimeout.
	listTimeout time.Duration
}

type shardSpec struct {
	Pattern string `json:"pattern"`
	K       int    `json:"k"`
}

// job is one `go test` process: either a shard of one package (Run set) or
// every remaining package in a single ordinary run.
type job struct {
	Name     string              `json:"name"`
	Packages []string            `json:"packages"`
	Run      string              `json:"-"`
	Expected map[string][]string `json:"-"`
	Weight   float64             `json:"estimated_seconds,omitempty"`
	Tests    int                 `json:"tests_assigned"`
}

func parseShardSpec(s string) ([]shardSpec, error) {
	var out []shardSpec
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		pkg, k, ok := strings.Cut(part, "=")
		n, err := strconv.Atoi(k)
		if !ok || pkg == "" || err != nil || n < 1 {
			return nil, fmt.Errorf("bad -shard entry %q: want PACKAGE=K with K >= 1", part)
		}
		out = append(out, shardSpec{Pattern: pkg, K: n})
	}
	return out, nil
}

// testListRE matches what `go test -list .` prints for runnable top-level
// functions. Benchmarks are listed too but never run without -bench; the
// trailer is go test's per-package summary line.
var (
	testListRE    = regexp.MustCompile(`^(Test|Example|Fuzz)[^\s]*$`)
	benchListRE   = regexp.MustCompile(`^Benchmark[^\s]*$`)
	listTrailerRE = regexp.MustCompile(`^(ok  |\?   |FAIL)\t`)
)

// parseList reads `go test -json -list . PKGS` and returns every package seen
// with its top-level test names, plus packages that failed to build. A list
// that was cut short (a non-JSON line, or a package with no final event) is an
// error: trusting it would leave packages out of every job.
func parseList(data []byte) (pkgs []string, names map[string][]string, failed map[string]bool, err error) {
	names = map[string][]string{}
	failed = map[string]bool{}
	seen := map[string]bool{}
	done := map[string]bool{}
	stray := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var e event
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			return nil, nil, nil, fmt.Errorf("test list has a non-JSON line: %.80q", sc.Text())
		}
		if e.Package == "" {
			continue
		}
		if e.Test == "" && isTerminal(e.Action) {
			done[e.Package] = true
		}
		if !seen[e.Package] {
			seen[e.Package] = true
			pkgs = append(pkgs, e.Package)
		}
		switch e.Action {
		case "output":
			line := strings.TrimSuffix(e.Output, "\n")
			switch {
			case testListRE.MatchString(line):
				names[e.Package] = append(names[e.Package], line)
			case benchListRE.MatchString(line), listTrailerRE.MatchString(line):
			default:
				if _, ok := stray[e.Package]; !ok {
					stray[e.Package] = line
				}
			}
		case "fail":
			failed[e.Package] = true
		}
	}
	if err := sc.Err(); err != nil {
		return nil, nil, nil, err
	}
	sort.Strings(pkgs)
	for _, p := range pkgs {
		if !done[p] {
			return nil, nil, nil, fmt.Errorf("test list for %s never finished", p)
		}
		// Output from TestMain or init could have swallowed a test name, so a
		// listed test would silently drop out of every shard.
		if line, ok := stray[p]; ok && !failed[p] {
			return nil, nil, nil, fmt.Errorf("test list for %s has unexpected output %q; TestMain or init must not print", p, line)
		}
	}
	for p, ns := range names {
		sort.Strings(ns)
		for i := 1; i < len(ns); i++ {
			if ns[i] == ns[i-1] {
				return nil, nil, nil, fmt.Errorf("%s lists %s twice", p, ns[i])
			}
		}
	}
	return pkgs, names, failed, nil
}

// runRegex anchors and quotes names so -run matches exactly these top-level
// tests and no test that merely shares a prefix. -run splits on "/" per
// subtest level; a top-level Go identifier never contains one, so subtests
// still run inside their parent.
func runRegex(names []string) string {
	q := make([]string, len(names))
	for i, n := range names {
		q[i] = regexp.QuoteMeta(n)
	}
	return "^(" + strings.Join(q, "|") + ")$"
}

// partition splits names into at most k non-empty bins. With weights it is
// longest-processing-time first; without, sorted names dealt round-robin.
// Ties break by name, bin size and bin index, so the same input gives the same
// plan and zero-second tests still spread over every bin.
func partition(names []string, k int, weight map[string]float64) ([][]string, []float64) {
	sorted := append([]string(nil), names...)
	sort.Strings(sorted)
	if k > len(sorted) {
		k = len(sorted)
	}
	if k < 1 {
		return nil, nil
	}
	w := func(n string) float64 {
		if v, ok := weight[n]; ok {
			return v
		}
		return 1
	}
	bins := make([][]string, k)
	load := make([]float64, k)
	if len(weight) == 0 {
		for i, n := range sorted {
			bins[i%k] = append(bins[i%k], n)
			load[i%k] += w(n)
		}
	} else {
		sort.SliceStable(sorted, func(i, j int) bool { return w(sorted[i]) > w(sorted[j]) })
		for _, n := range sorted {
			b := 0
			for i := 1; i < k; i++ {
				if load[i] < load[b] || (load[i] == load[b] && len(bins[i]) < len(bins[b])) {
					b = i
				}
			}
			bins[b] = append(bins[b], n)
			load[b] += w(n)
		}
	}
	for _, b := range bins {
		sort.Strings(b)
	}
	return bins, load
}

// buildJobs assigns every listed package to exactly one job. A sharded
// package that failed to list (for example a build failure) runs unsharded so
// go test reports the failure under its own name.
func buildJobs(pkgs []string, names map[string][]string, failed map[string]bool, heavy map[string]int, timings map[string]map[string]float64) ([]job, error) {
	var jobs []job
	rest := job{Name: "rest", Expected: map[string][]string{}}
	slugs := map[string]string{}
	for _, p := range pkgs {
		k := heavy[p]
		if k < 2 || failed[p] || len(names[p]) < 2 {
			rest.Packages = append(rest.Packages, p)
			rest.Expected[p] = names[p]
			rest.Tests += len(names[p])
			continue
		}
		slug := jobSlug(p)
		if other, dup := slugs[slug]; dup && other != p {
			slug = strings.NewReplacer("/", "_", ".", "_").Replace(p)
		}
		slugs[slug] = p
		bins, load := partition(names[p], k, timings[p])
		for i, b := range bins {
			jobs = append(jobs, job{
				Name: fmt.Sprintf("%s.%d", slug, i), Packages: []string{p}, Run: runRegex(b),
				Expected: map[string][]string{p: b}, Weight: load[i], Tests: len(b),
			})
		}
	}
	if len(rest.Packages) > 0 {
		jobs = append([]job{rest}, jobs...)
	}
	return jobs, nil
}

func jobSlug(pkg string) string {
	parts := strings.Split(pkg, "/")
	if len(parts) > 2 {
		parts = parts[len(parts)-2:]
	}
	return strings.Join(parts, "_")
}

// goList returns the import paths go list resolves for patterns.
func goList(dir string, patterns ...string) ([]string, error) {
	cmd := exec.Command("go", append([]string{"list"}, patterns...)...)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("go list %s: %w", strings.Join(patterns, " "), err)
	}
	return strings.Fields(string(out)), nil
}

// samePackages reports the packages go list resolves but the test list lacks,
// and the reverse.
func samePackages(listed, resolved []string) error {
	in := map[string]bool{}
	for _, p := range listed {
		in[p] = true
	}
	var missing []string
	for _, p := range resolved {
		if !in[p] {
			missing = append(missing, p)
		}
		delete(in, p)
	}
	if len(missing) > 0 || len(in) > 0 {
		var extra []string
		for p := range in {
			extra = append(extra, p)
		}
		sort.Strings(extra)
		return fmt.Errorf("test list does not match go list: missing %v, unexpected %v", missing, extra)
	}
	return nil
}

// resolvePackages maps -shard patterns to import paths with go list.
func resolvePackages(dir string, specs []shardSpec) (map[string]int, error) {
	heavy := map[string]int{}
	for _, s := range specs {
		pkgs, err := goList(dir, s.Pattern)
		if err != nil {
			return nil, err
		}
		for _, p := range pkgs {
			heavy[p] = s.K
		}
	}
	return heavy, nil
}

// eventFiles accepts an events.jsonl, a run.sh OUT dir or a shardsuite OUT dir.
func eventFiles(path string) ([]string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !st.IsDir() {
		return []string{path}, nil
	}
	if f := filepath.Join(path, "events.jsonl"); fileExists(f) {
		return []string{f}, nil
	}
	return shardEventFiles(path)
}

// shardEventFiles takes the jobs from OUT/plan.json when it exists, so stale
// shard directories from an earlier run into the same OUT are not counted.
func shardEventFiles(out string) ([]string, error) {
	if b, err := os.ReadFile(filepath.Join(out, "plan.json")); err == nil {
		var plan struct{ Jobs []struct{ Name string } }
		if err := json.Unmarshal(b, &plan); err != nil {
			return nil, fmt.Errorf("%s/plan.json: %w", out, err)
		}
		var files []string
		for _, j := range plan.Jobs {
			files = append(files, filepath.Join(out, "shards", j.Name, "events.jsonl"))
		}
		return files, nil
	}
	files, err := filepath.Glob(filepath.Join(out, "shards", "*", "events.jsonl"))
	if err != nil {
		return nil, err
	}
	if len(files) == 0 {
		return nil, fmt.Errorf("no shards/*/events.jsonl under %s", out)
	}
	sort.Strings(files)
	return files, nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

// loadTimings returns the elapsed seconds of each top-level test.
func loadTimings(path string) (map[string]map[string]float64, error) {
	files, err := eventFiles(path)
	if err != nil {
		return nil, err
	}
	t := map[string]map[string]float64{}
	for _, f := range files {
		evs, _, err := readEvents(f)
		if err != nil {
			return nil, err
		}
		for _, e := range evs {
			if e.Test == "" || strings.Contains(e.Test, "/") || !isTerminal(e.Action) {
				continue
			}
			if t[e.Package] == nil {
				t[e.Package] = map[string]float64{}
			}
			t[e.Package][e.Test] = e.Elapsed
		}
	}
	return t, nil
}
