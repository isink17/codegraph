package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
)

// event is one `go test -json` record.
type event struct {
	Action      string
	Package     string
	Test        string
	Output      string
	Elapsed     float64
	FailedBuild string
}

func isTerminal(action string) bool {
	return action == "pass" || action == "fail" || action == "skip"
}

// readEvents parses a go test -json stream. A line that is not JSON, such as
// the half line of a killed process, is counted rather than skipped silently.
func readEvents(path string) ([]event, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	return parseEvents(f)
}

func parseEvents(r io.Reader) ([]event, int, error) {
	var evs []event
	malformed := 0
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1024*1024), 64*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(strings.TrimSpace(string(line))) == 0 {
			continue
		}
		var e event
		if json.Unmarshal(line, &e) != nil {
			malformed++
			continue
		}
		evs = append(evs, e)
	}
	return evs, malformed, sc.Err()
}

type counts struct {
	Pass int `json:"pass"`
	Fail int `json:"fail"`
	Skip int `json:"skip"`
}

type pkgResult struct {
	Package string  `json:"package"`
	Result  string  `json:"result"`
	Elapsed float64 `json:"elapsed"`
	Tests   counts  `json:"tests"`
}

// Job results. A job passes only as pass or no_tests.
const (
	resPass       = "pass"
	resNoTests    = "no_tests"
	resTestFail   = "test_fail"
	resBuildFail  = "build_fail"
	resTimeout    = "timeout"
	resIncomplete = "incomplete"
	resInventory  = "inventory_mismatch"
)

type jobResult struct {
	job
	Result      string      `json:"result"`
	Reason      string      `json:"reason,omitempty"`
	Exit        int         `json:"exit"`
	WallSeconds float64     `json:"wall_seconds"`
	Malformed   int         `json:"malformed_lines"`
	Counts      counts      `json:"tests"`
	PkgResults  []pkgResult `json:"package_results"`
	Missing     []string    `json:"missing_tests,omitempty"`
	Duplicate   []string    `json:"duplicate_tests,omitempty"`
	Unexpected  []string    `json:"unexpected_tests,omitempty"`
}

func (r jobResult) passed() bool { return r.Result == resPass || r.Result == resNoTests }

// classify turns one process's events and exit into a single result. The
// order matters: a run that was cut short is never reported as anything that
// could read as complete.
func classify(j job, evs []event, malformed, exit int, killed, interrupted bool) jobResult {
	r := jobResult{job: j, Exit: exit, Malformed: malformed}
	type st struct {
		started, timedOut, build bool
		terminal                 string
		elapsed                  float64
		tests                    counts
	}
	pk := map[string]*st{}
	for _, p := range j.Packages {
		pk[p] = &st{}
	}
	get := func(p string) *st {
		if pk[p] == nil {
			pk[p] = &st{}
		}
		return pk[p]
	}
	seen := map[string]int{}
	for _, e := range evs {
		if e.Package == "" {
			continue
		}
		s := get(e.Package)
		switch {
		case e.Action == "start":
			s.started = true
		case e.Action == "output" && strings.HasPrefix(e.Output, "panic: test timed out after"):
			s.timedOut = true
		case e.Test != "" && isTerminal(e.Action):
			switch e.Action {
			case "pass":
				s.tests.Pass++
			case "fail":
				s.tests.Fail++
			case "skip":
				s.tests.Skip++
			}
			if !strings.Contains(e.Test, "/") {
				seen[e.Package+" "+e.Test]++
			}
		case e.Test == "" && isTerminal(e.Action):
			s.started = true
			s.terminal = e.Action
			s.elapsed = e.Elapsed
			s.build = e.FailedBuild != ""
		}
	}
	seenRes := map[string]bool{}
	names := make([]string, 0, len(pk))
	for p := range pk {
		names = append(names, p)
	}
	sort.Strings(names)
	for _, p := range names {
		s := pk[p]
		res := resIncomplete
		switch {
		case s.terminal == "pass":
			res = resPass
		case s.terminal == "skip":
			res = resNoTests
		case s.terminal == "fail" && s.build:
			res = resBuildFail
		case s.terminal == "fail" && s.timedOut:
			res = resTimeout
		case s.terminal == "fail":
			res = resTestFail
		case s.timedOut:
			res = resTimeout
		}
		seenRes[res] = true
		r.PkgResults = append(r.PkgResults, pkgResult{Package: p, Result: res, Elapsed: s.elapsed, Tests: s.tests})
		r.Counts.Pass += s.tests.Pass
		r.Counts.Fail += s.tests.Fail
		r.Counts.Skip += s.tests.Skip
	}
	expected := map[string]bool{}
	for p, ns := range j.Expected {
		for _, n := range ns {
			key := p + " " + n
			expected[key] = true
			switch seen[key] {
			case 0:
				r.Missing = append(r.Missing, key)
			case 1:
			default:
				r.Duplicate = append(r.Duplicate, key)
			}
		}
	}
	for key, c := range seen {
		if !expected[key] {
			r.Unexpected = append(r.Unexpected, key)
		} else if c > 1 && !contains(r.Duplicate, key) {
			r.Duplicate = append(r.Duplicate, key)
		}
	}
	sort.Strings(r.Missing)
	sort.Strings(r.Duplicate)
	sort.Strings(r.Unexpected)

	switch {
	case interrupted:
		r.Result, r.Reason = resIncomplete, "interrupted"
	case killed:
		r.Result, r.Reason = resTimeout, "killed at -job-timeout"
	case malformed > 0:
		r.Result, r.Reason = resIncomplete, fmt.Sprintf("%d malformed event lines", malformed)
	case seenRes[resTimeout]:
		r.Result = resTimeout
	case seenRes[resBuildFail]:
		r.Result = resBuildFail
	case seenRes[resTestFail]:
		r.Result = resTestFail
	case seenRes[resIncomplete]:
		r.Result, r.Reason = resIncomplete, "a package never finished"
	case len(r.Missing) > 0:
		r.Result, r.Reason = resIncomplete, fmt.Sprintf("%d listed tests never reported", len(r.Missing))
	case exit != 0:
		r.Result, r.Reason = resTestFail, fmt.Sprintf("go test exited %d without a failing package", exit)
	case len(r.Duplicate) > 0 || len(r.Unexpected) > 0:
		r.Result = resInventory
		r.Reason = fmt.Sprintf("%d duplicate, %d unlisted top-level tests", len(r.Duplicate), len(r.Unexpected))
	case len(r.PkgResults) > 0 && !seenRes[resPass]:
		r.Result = resNoTests
	case len(r.PkgResults) == 0:
		r.Result, r.Reason = resIncomplete, "no packages"
	default:
		r.Result = resPass
	}
	return r
}

func contains(s []string, v string) bool {
	for _, x := range s {
		if x == v {
			return true
		}
	}
	return false
}

// tempPathRE matches a t.TempDir path embedded in a subtest name; each process
// gets a different random directory, so it cannot be part of the identity.
var tempPathRE = regexp.MustCompile(`[/\\][^\n]*[/\\](Test\w*?)\d{5,}[/\\](\d{3})[/\\]`)

func normalizeTestName(n string) string {
	return tempPathRE.ReplaceAllString(n, "/<TEMPDIR>/$1/$2/")
}

type parityReport struct {
	Baseline   string   `json:"baseline"`
	BaseTests  int      `json:"baseline_tests"`
	CandTests  int      `json:"sharded_tests"`
	Missing    []string `json:"missing"`
	Unexpected []string `json:"unexpected"`
	Duplicates []string `json:"duplicates"`
	Changed    []string `json:"changed_action"`
	Malformed  int      `json:"malformed_lines"`
	OK         bool     `json:"ok"`
}

// identities maps package+test (subtests included) to the terminal actions
// reported for it.
func identities(files []string) (map[string][]string, int, error) {
	ids := map[string][]string{}
	bad := 0
	for _, f := range files {
		evs, m, err := readEvents(f)
		if err != nil {
			return nil, 0, err
		}
		bad += m
		for _, e := range evs {
			if e.Test != "" && isTerminal(e.Action) {
				k := e.Package + " " + normalizeTestName(e.Test)
				ids[k] = append(ids[k], e.Action)
			}
		}
	}
	return ids, bad, nil
}

// compareBaseline checks that every test identity of an unsharded run was
// reported exactly once, with the same action, across the shards. Equal pass
// counts are not enough: this compares names.
func compareBaseline(baseline string, files []string) (parityReport, error) {
	p := parityReport{Baseline: baseline}
	bfiles, err := eventFiles(baseline)
	if err != nil {
		return p, err
	}
	base, bm, err := identities(bfiles)
	if err != nil {
		return p, err
	}
	cand, cm, err := identities(files)
	if err != nil {
		return p, err
	}
	p.Malformed = bm + cm
	for k, acts := range base {
		p.BaseTests += len(acts)
		c := cand[k]
		switch {
		case len(c) == 0:
			p.Missing = append(p.Missing, k)
		case len(c) != len(acts):
			p.Duplicates = append(p.Duplicates, k)
		case strings.Join(c, ",") != strings.Join(acts, ","):
			p.Changed = append(p.Changed, fmt.Sprintf("%s: %s -> %s", k, strings.Join(acts, ","), strings.Join(c, ",")))
		}
	}
	for k, acts := range cand {
		p.CandTests += len(acts)
		if _, ok := base[k]; !ok {
			p.Unexpected = append(p.Unexpected, k)
		}
	}
	sort.Strings(p.Missing)
	sort.Strings(p.Unexpected)
	sort.Strings(p.Duplicates)
	sort.Strings(p.Changed)
	p.OK = p.Malformed == 0 && p.BaseTests > 0 && len(p.Missing)+len(p.Unexpected)+len(p.Duplicates)+len(p.Changed) == 0
	return p, nil
}

func printParity(w io.Writer, p parityReport) {
	fmt.Fprintf(w, "shardsuite: parity ok=%t baseline=%d sharded=%d missing=%d unexpected=%d duplicates=%d changed=%d malformed=%d\n",
		p.OK, p.BaseTests, p.CandTests, len(p.Missing), len(p.Unexpected), len(p.Duplicates), len(p.Changed), p.Malformed)
	for _, l := range [][]string{p.Missing, p.Unexpected, p.Duplicates, p.Changed} {
		for i, s := range l {
			if i == 10 {
				fmt.Fprintf(w, "  ... %d more\n", len(l)-10)
				break
			}
			fmt.Fprintln(w, "  ", s)
		}
	}
}
