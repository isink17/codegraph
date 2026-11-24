package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func evs(t *testing.T, lines ...string) ([]event, int) {
	t.Helper()
	e, m, err := parseEvents(strings.NewReader(strings.Join(lines, "\n")))
	if err != nil {
		t.Fatal(err)
	}
	return e, m
}

const (
	startA  = `{"Action":"start","Package":"m/a"}`
	runA    = `{"Action":"run","Package":"m/a","Test":"TestA"}`
	passA   = `{"Action":"pass","Package":"m/a","Test":"TestA"}`
	passSub = `{"Action":"pass","Package":"m/a","Test":"TestA/sub"}`
	passPkg = `{"Action":"pass","Package":"m/a","Elapsed":1.5}`
	failPkg = `{"Action":"fail","Package":"m/a","Elapsed":1.5}`
)

func TestClassify(t *testing.T) {
	one := job{Name: "s", Packages: []string{"m/a"}, Expected: map[string][]string{"m/a": {"TestA"}}}
	cases := []struct {
		name         string
		lines        []string
		exit         int
		killed, intr bool
		job          job
		want         string
	}{
		{"pass", []string{startA, runA, passSub, passA, passPkg}, 0, false, false, one, resPass},
		{"no test files", []string{startA, `{"Action":"skip","Package":"m/a"}`}, 0, false, false,
			job{Packages: []string{"m/a"}}, resNoTests},
		{"failing test", []string{startA, `{"Action":"fail","Package":"m/a","Test":"TestA"}`, failPkg}, 1, false, false, one, resTestFail},
		{"build failure", []string{`{"Action":"fail","Package":"m/a","FailedBuild":"m/a"}`}, 1, false, false, one, resBuildFail},
		{"timeout panic", []string{startA, runA,
			`{"Action":"output","Package":"m/a","Test":"TestA","Output":"panic: test timed out after 1s\n"}`, failPkg}, 1, false, false, one, resTimeout},
		{"started never finished", []string{startA, runA, passA}, 0, false, false, one, resIncomplete},
		{"package never started", nil, 0, false, false, one, resIncomplete},
		{"half line", []string{startA, passA, passPkg, `{"Action":"pa`}, 0, false, false, one, resIncomplete},
		{"listed test never reported", []string{startA, passPkg}, 0, false, false, one, resIncomplete},
		{"nonzero exit without failure", []string{startA, passA, passPkg}, 2, false, false, one, resTestFail},
		{"duplicate top-level", []string{startA, passA, passA, passPkg}, 0, false, false, one, resInventory},
		{"unlisted top-level", []string{startA, passA, `{"Action":"pass","Package":"m/a","Test":"TestZ"}`, passPkg}, 0, false, false, one, resInventory},
		{"killed at job timeout", []string{startA, passA, passPkg}, -1, true, false, one, resTimeout},
		{"interrupted beats everything", []string{startA, passA, passPkg}, 0, true, true, one, resIncomplete},
	}
	for _, c := range cases {
		e, m := evs(t, c.lines...)
		r := classify(c.job, e, m, c.exit, c.killed, c.intr)
		if r.Result != c.want {
			t.Errorf("%s: result %q (%s), want %q", c.name, r.Result, r.Reason, c.want)
		}
		if r.passed() != (c.want == resPass || c.want == resNoTests) {
			t.Errorf("%s: passed() = %t", c.name, r.passed())
		}
	}
}

func TestClassifyCountsSubtestsButMatchesTopLevel(t *testing.T) {
	e, m := evs(t, startA, passSub, passA, `{"Action":"skip","Package":"m/a","Test":"TestS"}`, passPkg)
	j := job{Packages: []string{"m/a"}, Expected: map[string][]string{"m/a": {"TestA", "TestS"}}}
	r := classify(j, e, m, 0, false, false)
	if r.Result != resPass || r.Counts != (counts{Pass: 2, Skip: 1}) || len(r.Missing) != 0 {
		t.Fatalf("got %+v", r)
	}
	if r.PkgResults[0].Elapsed != 1.5 {
		t.Errorf("elapsed = %v", r.PkgResults[0].Elapsed)
	}
}

func TestNormalizeTestName(t *testing.T) {
	a := "TestEscape//private/tmp/x/s1/tmp/TestEscape462961359/001/repo-b/secret.go"
	b := "TestEscape//var/folders/zz/TestEscape4211070298/001/repo-b/secret.go"
	sp := "TestEscape//Users/John Doe/tmp/TestEscape98765432/001/repo-b/secret.go"
	if normalizeTestName(sp) != normalizeTestName(a) {
		t.Errorf("temp path with a space: %q", normalizeTestName(sp))
	}
	w := `TestEscape/C:\Users\me\AppData\Local\Temp\TestEscape12345678\001\repo-b/secret.go`
	if normalizeTestName(a) != normalizeTestName(b) || normalizeTestName(a) != normalizeTestName(w) {
		t.Fatalf("%q / %q / %q", normalizeTestName(a), normalizeTestName(b), normalizeTestName(w))
	}
	for _, keep := range []string{"TestA/case_12345", "TestA/x/y"} {
		if normalizeTestName(keep) != keep {
			t.Errorf("%q changed to %q", keep, normalizeTestName(keep))
		}
	}
}

func write(t *testing.T, dir, name string, lines ...string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCompareBaseline(t *testing.T) {
	dir := t.TempDir()
	pass := func(test string) string { return `{"Action":"pass","Package":"m/a","Test":"` + test + `"}` }
	base := write(t, dir, "base.jsonl", pass("TestA"), pass("TestA/sub"), pass("TestB"), pass("TestC"), passPkg)

	s1 := write(t, dir, "s1.jsonl", pass("TestA"), pass("TestA/sub"), passPkg)
	s2 := write(t, dir, "s2.jsonl", pass("TestB"), pass("TestC"), passPkg)
	p, err := compareBaseline(base, []string{s1, s2})
	if err != nil {
		t.Fatal(err)
	}
	if !p.OK || p.BaseTests != 4 || p.CandTests != 4 {
		t.Fatalf("identical split: %+v", p)
	}

	// Same pass count, different identities: TestA/sub lost, TestB ran twice,
	// TestC skipped, TestD appeared. Counts alone would not show any of this.
	s3 := write(t, dir, "s3.jsonl", pass("TestA"), pass("TestB"), pass("TestD"), passPkg)
	s4 := write(t, dir, "s4.jsonl", pass("TestB"), `{"Action":"skip","Package":"m/a","Test":"TestC"}`, passPkg)
	p, err = compareBaseline(base, []string{s3, s4})
	if err != nil {
		t.Fatal(err)
	}
	if p.OK || len(p.Missing) != 1 || len(p.Duplicates) != 1 || len(p.Changed) != 1 || len(p.Unexpected) != 1 {
		t.Fatalf("got %+v", p)
	}
	if p.Missing[0] != "m/a TestA/sub" || p.Duplicates[0] != "m/a TestB" || p.Unexpected[0] != "m/a TestD" {
		t.Fatalf("got %+v", p)
	}

	empty := write(t, dir, "empty.jsonl", "")
	if p, _ := compareBaseline(empty, []string{empty}); p.OK {
		t.Error("an empty baseline proves nothing")
	}
	bad := write(t, dir, "bad.jsonl", pass("TestA"), pass("TestA/sub"), pass("TestB"), pass("TestC"), `{"Act`)
	if p, _ := compareBaseline(base, []string{bad}); p.OK {
		t.Error("a malformed line must fail parity")
	}
}

func TestEventFilesAcceptsRunAndShardDirs(t *testing.T) {
	runOut := t.TempDir()
	write(t, runOut, "events.jsonl", passPkg)
	if f, err := eventFiles(runOut); err != nil || len(f) != 1 {
		t.Fatalf("run.sh dir: %v %v", f, err)
	}
	shardOut := t.TempDir()
	write(t, shardOut, "shards/b/events.jsonl", passPkg)
	write(t, shardOut, "shards/a/events.jsonl", passPkg)
	f, err := eventFiles(shardOut)
	if err != nil || len(f) != 2 || !strings.HasSuffix(f[0], filepath.Join("a", "events.jsonl")) {
		t.Fatalf("shard dir: %v %v", f, err)
	}
	if _, err := eventFiles(t.TempDir()); err == nil {
		t.Error("empty dir: want error")
	}
}

func TestLoadTimingsUsesTopLevelOnly(t *testing.T) {
	p := write(t, t.TempDir(), "e.jsonl",
		`{"Action":"pass","Package":"m/a","Test":"TestA","Elapsed":2.5}`,
		`{"Action":"pass","Package":"m/a","Test":"TestA/sub","Elapsed":9}`,
		`{"Action":"pass","Package":"m/a","Elapsed":3}`)
	tm, err := loadTimings(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(tm["m/a"]) != 1 || tm["m/a"]["TestA"] != 2.5 {
		t.Fatalf("got %v", tm)
	}
}
