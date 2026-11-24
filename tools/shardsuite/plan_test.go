package main

import (
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func TestParseShardSpec(t *testing.T) {
	got, err := parseShardSpec(" ./internal/store=3, ./internal/diff=2,")
	if err != nil {
		t.Fatal(err)
	}
	want := []shardSpec{{"./internal/store", 3}, {"./internal/diff", 2}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for _, bad := range []string{"./x", "./x=0", "=2", "./x=two"} {
		if _, err := parseShardSpec(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

func TestParseList(t *testing.T) {
	stream := strings.Join([]string{
		`{"Action":"start","Package":"m/a"}`,
		`{"Action":"output","Package":"m/a","Output":"TestB\n"}`,
		`{"Action":"output","Package":"m/a","Output":"TestA\n"}`,
		`{"Action":"output","Package":"m/a","Output":"BenchmarkA\n"}`,
		`{"Action":"output","Package":"m/a","Output":"ExampleA\n"}`,
		`{"Action":"output","Package":"m/a","Output":"FuzzA\n"}`,
		`{"Action":"output","Package":"m/a","Output":"ok  \tm/a\t0.1s\n"}`,
		`{"Action":"pass","Package":"m/a"}`,
		`{"Action":"start","Package":"m/empty"}`,
		`{"Action":"output","Package":"m/empty","Output":"?   \tm/empty\t[no test files]\n"}`,
		`{"Action":"skip","Package":"m/empty"}`,
		`{"Action":"fail","Package":"m/broken","FailedBuild":"m/broken"}`,
	}, "\n")
	pkgs, names, failed, err := parseList([]byte(stream))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"m/a", "m/broken", "m/empty"}; !reflect.DeepEqual(pkgs, want) {
		t.Errorf("pkgs = %v, want %v", pkgs, want)
	}
	if want := []string{"ExampleA", "FuzzA", "TestA", "TestB"}; !reflect.DeepEqual(names["m/a"], want) {
		t.Errorf("names = %v, want %v", names["m/a"], want)
	}
	if !failed["m/broken"] || failed["m/a"] {
		t.Errorf("failed = %v", failed)
	}
	dup := `{"Action":"output","Package":"m/a","Output":"TestA\n"}` + "\n" + `{"Action":"output","Package":"m/a","Output":"TestA\n"}` + "\n" + passListed
	if _, _, _, err := parseList([]byte(dup)); err == nil {
		t.Error("duplicate listed name: want error")
	}
	// A list cut short must never become a smaller inventory.
	for name, cut := range map[string]string{
		"half line":          stream + "\n" + `{"Action":"start","Pack`,
		"TestMain output":    strings.Replace(stream, `"Output":"TestA\n"`, `"Output":"setupTestA\n"`, 1),
		"unfinished package": stream + "\n" + `{"Action":"start","Package":"m/z"}` + "\n" + `{"Action":"output","Package":"m/z","Output":"TestZ\n"}`,
	} {
		if _, _, _, err := parseList([]byte(cut)); err == nil {
			t.Errorf("%s: want error", name)
		}
	}
}

const passListed = `{"Action":"pass","Package":"m/a"}`

func TestSamePackages(t *testing.T) {
	if err := samePackages([]string{"m/a", "m/b"}, []string{"m/b", "m/a"}); err != nil {
		t.Fatal(err)
	}
	if err := samePackages([]string{"m/a"}, []string{"m/a", "m/b"}); err == nil || !strings.Contains(err.Error(), "m/b") {
		t.Fatalf("missing package: %v", err)
	}
	if err := samePackages([]string{"m/a", "m/x"}, []string{"m/a"}); err == nil {
		t.Fatal("unexpected package: want error")
	}
}

func TestRunRegexIsAnchoredAndQuoted(t *testing.T) {
	re := regexp.MustCompile(runRegex([]string{"TestA", "TestÜber", "Test.x", "Test$y"}))
	for name, want := range map[string]bool{
		"TestA": true, "TestAB": false, "XTestA": false, "TestÜber": true,
		"Test.x": true, "TestZx": false, "Test$y": true, "Testy": false,
	} {
		if got := re.MatchString(name); got != want {
			t.Errorf("%s: match = %t, want %t", name, got, want)
		}
	}
	if strings.Contains(runRegex([]string{"TestA", "TestB"}), "/") {
		t.Error("a slash would be split into a subtest level by -run")
	}
}

func names(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("Test%03d", i)
	}
	return out
}

func assertCoversOnce(t *testing.T, bins [][]string, all []string) {
	t.Helper()
	var got []string
	for _, b := range bins {
		if len(b) == 0 {
			t.Error("empty bin")
		}
		got = append(got, b...)
	}
	sort.Strings(got)
	want := append([]string(nil), all...)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("bins do not cover every name exactly once: %v", got)
	}
}

func TestPartitionRoundRobinIsDeterministic(t *testing.T) {
	in := []string{"TestC", "TestA", "TestE", "TestB", "TestD"}
	bins, _ := partition(in, 2, nil)
	want := [][]string{{"TestA", "TestC", "TestE"}, {"TestB", "TestD"}}
	if !reflect.DeepEqual(bins, want) {
		t.Fatalf("got %v, want %v", bins, want)
	}
	again, _ := partition([]string{"TestE", "TestD", "TestC", "TestB", "TestA"}, 2, nil)
	if !reflect.DeepEqual(bins, again) {
		t.Fatal("input order changed the plan")
	}
}

func TestPartitionLPTBalancesByWeight(t *testing.T) {
	w := map[string]float64{"TestBig": 10, "TestM1": 5, "TestM2": 5, "TestS": 1}
	bins, load := partition([]string{"TestS", "TestM1", "TestBig", "TestM2"}, 2, w)
	want := [][]string{{"TestBig", "TestS"}, {"TestM1", "TestM2"}}
	if !reflect.DeepEqual(bins, want) {
		t.Fatalf("got %v, want %v", bins, want)
	}
	if load[0] != 11 || load[1] != 10 {
		t.Fatalf("load = %v", load)
	}
	// A test missing from the timings counts as one second.
	bins, _ = partition([]string{"TestNew", "TestBig"}, 2, w)
	if !reflect.DeepEqual(bins, [][]string{{"TestBig"}, {"TestNew"}}) {
		t.Fatalf("got %v", bins)
	}
}

func TestPartitionCoversEveryNameOnce(t *testing.T) {
	all := names(101)
	w := map[string]float64{}
	for i, n := range all {
		w[n] = float64(i % 7)
	}
	for _, k := range []int{1, 2, 3, 8, 101, 500} {
		bins, _ := partition(all, k, nil)
		assertCoversOnce(t, bins, all)
		bins, _ = partition(all, k, w)
		assertCoversOnce(t, bins, all)
		if want := min(k, len(all)); len(bins) != want {
			t.Errorf("k=%d: %d bins, want %d", k, len(bins), want)
		}
	}
	if bins, _ := partition(nil, 3, nil); bins != nil {
		t.Errorf("no names: got %v", bins)
	}
}

func TestBuildJobsAssignsEveryPackageOnce(t *testing.T) {
	pkgs := []string{"m/heavy", "m/light", "m/broken", "m/one", "m/empty"}
	listed := map[string][]string{
		"m/heavy": names(9), "m/light": {"TestL"}, "m/broken": nil, "m/one": {"TestOnly"},
	}
	heavy := map[string]int{"m/heavy": 3, "m/broken": 3, "m/one": 4}
	jobs, err := buildJobs(pkgs, listed, map[string]bool{"m/broken": true}, heavy, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(jobs) != 4 || jobs[0].Name != "rest" {
		t.Fatalf("jobs = %+v", jobs)
	}
	if want := []string{"m/light", "m/broken", "m/one", "m/empty"}; !reflect.DeepEqual(jobs[0].Packages, want) {
		t.Errorf("rest = %v, want %v (failed, single-test and testless packages run unsharded)", jobs[0].Packages, want)
	}
	if jobs[0].Run != "" {
		t.Error("rest job must not filter tests")
	}
	var shards [][]string
	for _, j := range jobs[1:] {
		if !reflect.DeepEqual(j.Packages, []string{"m/heavy"}) || j.Run != runRegex(j.Expected["m/heavy"]) {
			t.Errorf("shard %+v", j)
		}
		shards = append(shards, j.Expected["m/heavy"])
	}
	assertCoversOnce(t, shards, names(9))
}

func TestBuildJobsKeepsJobNamesUnique(t *testing.T) {
	two := []string{"TestA", "TestB"}
	jobs, err := buildJobs([]string{"x/a/b", "y/a/b"}, map[string][]string{"x/a/b": two, "y/a/b": two}, nil,
		map[string]int{"x/a/b": 2, "y/a/b": 2}, nil)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, j := range jobs {
		if seen[j.Name] {
			t.Fatalf("duplicate job name %s", j.Name)
		}
		seen[j.Name] = true
	}
	if len(seen) != 4 {
		t.Fatalf("jobs %v", seen)
	}
}
