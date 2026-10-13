package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/isink17/codegraph/internal/config"
	"github.com/isink17/codegraph/internal/store"
	"github.com/isink17/codegraph/internal/version"
)

func TestRunInstallCreatesConfigAndPrintsSnippets(t *testing.T) {
	home := filepath.Join(t.TempDir(), "codegraph-home")
	t.Setenv("CODEGRAPH_HOME", home)
	prev := startupVersionCheck
	startupVersionCheck = func(context.Context, io.Writer) {}
	t.Cleanup(func() {
		startupVersionCheck = prev
	})

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := Run(context.Background(), []string{"install"}, &stdout, &stderr); err != nil {
		t.Fatalf("Run(install) error = %v", err)
	}

	configPath := filepath.Join(home, "config", "config.json")
	if _, err := os.Stat(configPath); err != nil {
		t.Fatalf("expected config file at %q: %v", configPath, err)
	}

	out := stdout.String()
	for _, needle := range []string{
		"codegraph install complete",
		"Codex MCP snippet:",
		"startup_timeout_sec = 60",
		"Gemini CLI MCP snippet:",
		"Claude/Desktop MCP snippet:",
		"If `codegraph` is not found after `go install`",
	} {
		if !strings.Contains(out, needle) {
			t.Fatalf("install output missing %q\noutput:\n%s", needle, out)
		}
	}
}

func TestRunInitIndexesAndReportsCapabilityIdempotently(t *testing.T) {
	t.Setenv("CODEGRAPH_HOME", filepath.Join(t.TempDir(), "codegraph-home"))
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, "main.go"), []byte("package main\nfunc Main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	prev := startupVersionCheck
	startupVersionCheck = func(context.Context, io.Writer) {}
	t.Cleanup(func() { startupVersionCheck = prev })

	for run := 0; run < 2; run++ {
		var out bytes.Buffer
		if err := Run(context.Background(), []string{"init", repoRoot}, &out, io.Discard); err != nil {
			t.Fatalf("Run(init) error = %v", err)
		}
		var result map[string]json.RawMessage
		if err := json.Unmarshal(out.Bytes(), &result); err != nil {
			t.Fatalf("decode init result: %v", err)
		}
		for _, key := range []string{"repo_root", "summary", "stats", "capability"} {
			if _, ok := result[key]; !ok {
				t.Fatalf("init result missing %q: %s", key, out.String())
			}
		}
		var summary store.ScanSummary
		if err := json.Unmarshal(result["summary"], &summary); err != nil {
			t.Fatal(err)
		}
		if summary.FilesSeen != 1 || summary.FilesTotal != 1 {
			t.Fatalf("first init did not scan the source file: %+v", summary)
		}
		if run == 0 {
			var capability store.GraphCapability
			if err := json.Unmarshal(result["capability"], &capability); err != nil {
				t.Fatalf("decode capability: %v", err)
			}
			if capability.State == "" || len(capability.Languages) != 1 || capability.Languages[0].Language != "go" {
				t.Fatalf("unexpected graph capability: %+v", capability)
			}
			continue
		}
		var second store.ScanSummary
		if err := json.Unmarshal(result["summary"], &second); err != nil {
			t.Fatal(err)
		}
		if second.FilesChanged != 0 || second.FilesSeen != 1 {
			t.Fatalf("repeated init summary = %+v", second)
		}
	}
}

func TestRunInitUsesCurrentDirectoryByDefault(t *testing.T) {
	t.Setenv("CODEGRAPH_HOME", filepath.Join(t.TempDir(), "codegraph-home"))
	repoRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(repoRoot, "main.go"), []byte("package main\nfunc Main() {}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repoRoot)
	prev := startupVersionCheck
	startupVersionCheck = func(context.Context, io.Writer) {}
	t.Cleanup(func() { startupVersionCheck = prev })

	var out bytes.Buffer
	if err := Run(context.Background(), []string{"init"}, &out, io.Discard); err != nil {
		t.Fatalf("Run(init) error = %v", err)
	}
	var result struct {
		RepoRoot string `json:"repo_root"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatalf("decode init result: %v", err)
	}
	canonicalRoot, err := store.CanonicalRepoPath(repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	if result.RepoRoot != canonicalRoot {
		t.Fatalf("repo_root = %q, want canonical current directory %q", result.RepoRoot, canonicalRoot)
	}
}

func TestRunInitRejectsMultipleRepoPaths(t *testing.T) {
	t.Setenv("CODEGRAPH_HOME", filepath.Join(t.TempDir(), "codegraph-home"))
	prev := startupVersionCheck
	startupVersionCheck = func(context.Context, io.Writer) {}
	t.Cleanup(func() { startupVersionCheck = prev })

	err := Run(context.Background(), []string{"init", "first", "second"}, io.Discard, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "init accepts at most one repo path") {
		t.Fatalf("Run(init with multiple paths) error = %v", err)
	}
}

func TestOrdinaryIndexDoesNotInvokeGradle(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake Gradle executable test uses a shell script")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "main.go"), []byte("package main\nfunc main() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "gradle-invoked")
	gradle := filepath.Join(bin, "gradle")
	if err := os.WriteFile(gradle, []byte("#!/bin/sh\nprintf invoked > \"$CODEGRAPH_GRADLE_MARKER\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CODEGRAPH_GRADLE_MARKER", marker)
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	cfg := config.Config{DBDir: config.RepoDBDir, DBPerformanceProfile: "balanced"}
	if err := runIndex(context.Background(), cfg, io.Discard, "index", []string{root, "--no-history"}, false); err != nil {
		t.Fatalf("runIndex(): %v", err)
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ordinary indexing invoked Gradle: stat marker error = %v", err)
	}
}

func TestRunFindSymbolQueryCommand(t *testing.T) {
	home := filepath.Join(t.TempDir(), "codegraph-home")
	t.Setenv("CODEGRAPH_HOME", home)
	repoRoot := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repoRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll(repoRoot) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "main.go"), []byte(`package main
func HelloWorld() {}
`), 0o644); err != nil {
		t.Fatalf("WriteFile(main.go) error = %v", err)
	}
	prev := startupVersionCheck
	startupVersionCheck = func(context.Context, io.Writer) {}
	t.Cleanup(func() {
		startupVersionCheck = prev
	})

	var out bytes.Buffer
	var errOut bytes.Buffer
	if err := Run(context.Background(), []string{"index", repoRoot}, &out, &errOut); err != nil {
		t.Fatalf("Run(index) error = %v", err)
	}

	out.Reset()
	if err := Run(context.Background(), []string{"find-symbol", repoRoot, "HelloWorld"}, &out, &errOut); err != nil {
		t.Fatalf("Run(find-symbol) error = %v", err)
	}
	if !strings.Contains(out.String(), "HelloWorld") {
		t.Fatalf("find-symbol output missing symbol, output:\n%s", out.String())
	}
}

func TestRunImpactAmbiguousSeedContract(t *testing.T) {
	t.Setenv("CODEGRAPH_HOME", filepath.Join(t.TempDir(), "codegraph-home"))
	repoRoot := t.TempDir()
	for _, pkg := range []string{"first", "second"} {
		dir := filepath.Join(repoRoot, pkg)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "helper.go"), []byte("package "+pkg+"\nfunc Helper() {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	prev := startupVersionCheck
	startupVersionCheck = func(context.Context, io.Writer) {}
	t.Cleanup(func() { startupVersionCheck = prev })
	ctx := context.Background()
	if err := Run(ctx, []string{"index", repoRoot}, io.Discard, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, command := range []string{"callers", "callees", "find_callers", "find_callees"} {
		var out bytes.Buffer
		if err := Run(ctx, []string{command, repoRoot, "Helper"}, &out, io.Discard); !errors.Is(err, store.ErrSymbolAmbiguous) || out.Len() != 0 {
			t.Fatalf("%s ambiguous target = %v, %q", command, err, out.String())
		}
		for _, symbol := range []string{"Absent", "first.Helper"} {
			out.Reset()
			if err := Run(ctx, []string{command, repoRoot, symbol}, &out, io.Discard); err != nil {
				t.Fatal(err)
			}
			var result map[string]any
			if err := json.Unmarshal(out.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			key := strings.TrimPrefix(command, "find_")
			if rows, ok := result[key].([]any); !ok || len(rows) != 0 || result["target_found"] != (symbol != "Absent") {
				t.Fatalf("%s(%s) = %s; want empty array with accurate presence", command, symbol, out.String())
			}
		}
	}
	for _, command := range []string{"impact", "get_impact_radius"} {
		var out bytes.Buffer
		if err := Run(ctx, []string{command, repoRoot, "Helper"}, &out, io.Discard); !errors.Is(err, store.ErrSymbolAmbiguous) || out.Len() != 0 {
			t.Fatalf("%s ambiguous target = %v, %q; want ambiguity without partial output", command, err, out.String())
		}
		if err := Run(ctx, []string{command, repoRoot, "first.Helper"}, &out, io.Discard); err != nil {
			t.Fatal(err)
		}
		var result map[string]any
		if err := json.Unmarshal(out.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if symbols := result["symbols"].([]any); len(symbols) != 1 || symbols[0].(map[string]any)["qualified_name"] != "first.Helper" {
			t.Fatalf("%s exact target = %s", command, out.String())
		}
		out.Reset()
		if err := Run(ctx, []string{command, repoRoot, "Absent"}, &out, io.Discard); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(out.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if presence := result["seed_presence"].(map[string]any); presence["found"] != float64(0) || len(presence["missing"].([]any)) != 1 {
			t.Fatalf("%s missing target = %s", command, out.String())
		}
	}
}

func TestRunIndexJSONL(t *testing.T) {
	home := filepath.Join(t.TempDir(), "codegraph-home")
	t.Setenv("CODEGRAPH_HOME", home)
	repoRoot := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repoRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll(repoRoot) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "main.go"), []byte(`package main
func main() {}
`), 0o644); err != nil {
		t.Fatalf("WriteFile(main.go) error = %v", err)
	}
	prev := startupVersionCheck
	startupVersionCheck = func(context.Context, io.Writer) {}
	t.Cleanup(func() {
		startupVersionCheck = prev
	})

	var out bytes.Buffer
	var errOut bytes.Buffer
	if err := Run(context.Background(), []string{"index", repoRoot, "--jsonl"}, &out, &errOut); err != nil {
		t.Fatalf("Run(index --jsonl) error = %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected at least 2 jsonl lines, got %d: %q", len(lines), out.String())
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("first line unmarshal error = %v", err)
	}
	if first["type"] != "scan_summary" {
		t.Fatalf("first event type = %v, want scan_summary", first["type"])
	}
}

func TestRunIndexRejectsUnknownFlag(t *testing.T) {
	home := filepath.Join(t.TempDir(), "codegraph-home")
	t.Setenv("CODEGRAPH_HOME", home)
	repoRoot := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repoRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll(repoRoot) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "main.go"), []byte(`package main
func main() {}
`), 0o644); err != nil {
		t.Fatalf("WriteFile(main.go) error = %v", err)
	}
	prev := startupVersionCheck
	startupVersionCheck = func(context.Context, io.Writer) {}
	t.Cleanup(func() {
		startupVersionCheck = prev
	})

	var out bytes.Buffer
	var errOut bytes.Buffer
	err := Run(context.Background(), []string{"index", repoRoot, "--does-not-exist"}, &out, &errOut)
	if err == nil {
		t.Fatalf("Run(index --does-not-exist) error = nil")
	}
	if !strings.Contains(err.Error(), "flag provided but not defined") {
		t.Fatalf("unexpected error for unknown flag: %v", err)
	}
}

func TestRunIndexSmokeJSONL(t *testing.T) {
	home := filepath.Join(t.TempDir(), "codegraph-home")
	t.Setenv("CODEGRAPH_HOME", home)
	repoRoot := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repoRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll(repoRoot) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "main.go"), []byte(`package main
func main() {}
`), 0o644); err != nil {
		t.Fatalf("WriteFile(main.go) error = %v", err)
	}
	prev := startupVersionCheck
	startupVersionCheck = func(context.Context, io.Writer) {}
	t.Cleanup(func() {
		startupVersionCheck = prev
	})

	var out bytes.Buffer
	var errOut bytes.Buffer
	if err := Run(context.Background(), []string{"index_smoke", repoRoot, "--runs", "1"}, &out, &errOut); err != nil {
		t.Fatalf("Run(index_smoke) error = %v", err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) < 2 {
		t.Fatalf("expected at least 2 jsonl lines, got %d: %q", len(lines), out.String())
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("first line unmarshal error = %v", err)
	}
	if first["type"] != "index_smoke_run" {
		t.Fatalf("first event type = %v, want index_smoke_run", first["type"])
	}
	var last map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &last); err != nil {
		t.Fatalf("last line unmarshal error = %v", err)
	}
	if last["type"] != "index_smoke_summary" {
		t.Fatalf("last event type = %v, want index_smoke_summary", last["type"])
	}
}

func TestBenchmarkBaselineIncludesContextFields(t *testing.T) {
	payload := benchmarkBaseline{
		CreatedAt:  "2026-01-02T03:04:05Z",
		Command:    []string{"go", "test"},
		Count:      1,
		Benchtime:  "10ms",
		GoVersion:  "go1.x",
		GOOS:       "windows",
		GOARCH:     "amd64",
		GOMAXPROCS: "default",
		NumCPU:     12,
		CWD:        "C:\\repo",
		SQLite:     "sqlite",
		Benchmarks: map[string]benchmarkMetric{"BenchmarkX": {NsPerOp: 1}},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal(payload) error = %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal(payload) error = %v", err)
	}
	if _, ok := decoded["num_cpu"]; !ok {
		t.Fatalf("payload missing num_cpu: %s", string(data))
	}
	if _, ok := decoded["cwd"]; !ok {
		t.Fatalf("payload missing cwd: %s", string(data))
	}
}

func TestRunIndexWithRepoDBDirSkipsRepoDatabaseFiles(t *testing.T) {
	home := filepath.Join(t.TempDir(), "codegraph-home")
	t.Setenv("CODEGRAPH_HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "config"), 0o755); err != nil {
		t.Fatalf("MkdirAll(config) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, "config", "config.json"), []byte("{\n  \"db_dir\": \"repo\"\n}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(config.json) error = %v", err)
	}

	repoRoot := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repoRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll(repoRoot) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "main.go"), []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(main.go) error = %v", err)
	}

	prev := startupVersionCheck
	startupVersionCheck = func(context.Context, io.Writer) {}
	t.Cleanup(func() {
		startupVersionCheck = prev
	})

	var out bytes.Buffer
	var errOut bytes.Buffer
	if err := Run(context.Background(), []string{"index", repoRoot}, &out, &errOut); err != nil {
		t.Fatalf("first Run(index) error = %v", err)
	}
	out.Reset()
	if err := Run(context.Background(), []string{"index", repoRoot}, &out, &errOut); err != nil {
		t.Fatalf("second Run(index) error = %v", err)
	}

	dbPath := filepath.Join(repoRoot, ".codegraph", store.RepoDatabaseFileName)
	if _, err := os.Stat(dbPath); err != nil {
		t.Fatalf("expected repo database at %q: %v", dbPath, err)
	}

	out.Reset()
	if err := Run(context.Background(), []string{"stats", repoRoot}, &out, &errOut); err != nil {
		t.Fatalf("Run(stats) error = %v", err)
	}
	var stats map[string]any
	if err := json.Unmarshal(out.Bytes(), &stats); err != nil {
		t.Fatalf("stats output json parse error = %v", err)
	}
	if got := int(stats["files"].(float64)); got != 1 {
		t.Fatalf("stats files = %d, want 1; output=%s", got, out.String())
	}
	freshness, ok := stats["freshness"].(map[string]any)
	if !ok || freshness["state"] != "no_known_staleness" || freshness["watcher"] != "not_in_this_process" {
		t.Fatalf("stats freshness = %v; output=%s", stats["freshness"], out.String())
	}
}

func TestRunIndexRebuildDropsStaleRows(t *testing.T) {
	home := filepath.Join(t.TempDir(), "codegraph-home")
	t.Setenv("CODEGRAPH_HOME", home)
	repoRoot := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repoRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll(repoRoot) error = %v", err)
	}
	firstFile := []byte(`package main

func OldThing() {}
`)
	if err := os.WriteFile(filepath.Join(repoRoot, "main.go"), firstFile, 0o644); err != nil {
		t.Fatalf("WriteFile(main.go) error = %v", err)
	}
	prev := startupVersionCheck
	startupVersionCheck = func(context.Context, io.Writer) {}
	t.Cleanup(func() {
		startupVersionCheck = prev
	})

	var out bytes.Buffer
	var errOut bytes.Buffer
	if err := Run(context.Background(), []string{"index", repoRoot}, &out, &errOut); err != nil {
		t.Fatalf("Run(index) error = %v", err)
	}
	if count := queryCount(t, repoRoot, "OldThing"); count == 0 {
		t.Fatalf("expected OldThing before rebuild")
	}

	secondFile := []byte(`package main

func NewThing() {}
`)
	if err := os.WriteFile(filepath.Join(repoRoot, "main.go"), secondFile, 0o644); err != nil {
		t.Fatalf("WriteFile(main.go) rewrite error = %v", err)
	}

	out.Reset()
	if err := Run(context.Background(), []string{"index", repoRoot, "--rebuild"}, &out, &errOut); err != nil {
		t.Fatalf("Run(index --rebuild) error = %v", err)
	}
	var rebuilt map[string]any
	if err := json.Unmarshal(out.Bytes(), &rebuilt); err != nil {
		t.Fatalf("rebuild output json parse error = %v", err)
	}
	summary, ok := rebuilt["summary"].(map[string]any)
	if !ok {
		t.Fatalf("rebuild output missing summary: %s", out.String())
	}
	if got, ok := summary["rebuild"].(bool); !ok || !got {
		t.Fatalf("rebuild output missing rebuild=true: %s", out.String())
	}
	removed, ok := summary["removed_db_files"].([]any)
	if !ok || len(removed) == 0 {
		t.Fatalf("rebuild output missing removed_db_files: %s", out.String())
	}
	if count := queryCount(t, repoRoot, "OldThing"); count != 0 {
		t.Fatalf("OldThing still indexed after rebuild: count=%d", count)
	}
	if count := queryCount(t, repoRoot, "NewThing"); count == 0 {
		t.Fatalf("NewThing missing after rebuild")
	}
}

func TestRemoveRepoDBFilesWrapsInUseError(t *testing.T) {
	home := filepath.Join(t.TempDir(), "codegraph-home")
	t.Setenv("CODEGRAPH_HOME", home)
	repoRoot := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(filepath.Join(repoRoot, ".codegraph"), 0o755); err != nil {
		t.Fatalf("MkdirAll(.codegraph) error = %v", err)
	}
	dbPath := filepath.Join(repoRoot, ".codegraph", store.RepoDatabaseFileName)
	if err := os.WriteFile(dbPath, []byte("db"), 0o644); err != nil {
		t.Fatalf("WriteFile(db) error = %v", err)
	}
	prevRemove := repoDBRemove
	repoDBRemove = func(string) error { return os.ErrPermission }
	t.Cleanup(func() {
		repoDBRemove = prevRemove
	})

	_, err := removeRepoDBFiles(config.Config{DBDir: config.RepoDBDir}, repoRoot, repoRoot)
	if err == nil {
		t.Fatalf("removeRepoDBFiles error = nil")
	}
	if !strings.Contains(err.Error(), "rebuild cannot remove repo database file") {
		t.Fatalf("error missing actionable rebuild text: %v", err)
	}
	if !strings.Contains(err.Error(), "codegraph serve") {
		t.Fatalf("error missing process hint: %v", err)
	}
}

func queryCount(t *testing.T, repoRoot, symbol string) int {
	t.Helper()
	var out bytes.Buffer
	var errOut bytes.Buffer
	if err := Run(context.Background(), []string{"find-symbol", repoRoot, symbol, "--exact"}, &out, &errOut); err != nil {
		t.Fatalf("Run(find-symbol %q) error = %v", symbol, err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("find-symbol %q output json parse error = %v", symbol, err)
	}
	return int(decoded["count"].(float64))
}

func TestRunConfigCommands(t *testing.T) {
	home := filepath.Join(t.TempDir(), "codegraph-home")
	t.Setenv("CODEGRAPH_HOME", home)
	prev := startupVersionCheck
	startupVersionCheck = func(context.Context, io.Writer) {}
	t.Cleanup(func() {
		startupVersionCheck = prev
	})

	var out bytes.Buffer
	var errOut bytes.Buffer
	if err := Run(context.Background(), []string{"config", "show"}, &out, &errOut); err != nil {
		t.Fatalf("Run(config show) error = %v", err)
	}
	if !strings.Contains(out.String(), `"config"`) {
		t.Fatalf("config show output missing config object: %s", out.String())
	}
	out.Reset()
	if err := Run(context.Background(), []string{"config", "edit-path"}, &out, &errOut); err != nil {
		t.Fatalf("Run(config edit-path) error = %v", err)
	}
	if strings.TrimSpace(out.String()) == "" {
		t.Fatalf("config edit-path output empty")
	}

	out.Reset()
	repoRoot := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repoRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll(repoRoot) error = %v", err)
	}
	if err := Run(context.Background(), []string{"config", "init", "--repo", repoRoot}, &out, &errOut); err != nil {
		t.Fatalf("Run(config init) error = %v", err)
	}
	repoCfgPath := filepath.Join(repoRoot, ".codegraph", "config.json")
	if _, err := os.Stat(repoCfgPath); err != nil {
		t.Fatalf("expected repo config at %q: %v", repoCfgPath, err)
	}
	var cfgData map[string]any
	if err := json.Unmarshal([]byte(out.String()), &cfgData); err != nil {
		t.Fatalf("config init output json parse error = %v", err)
	}
	if cfgData["path"] == "" {
		t.Fatalf("config init output missing path: %s", out.String())
	}
}

func TestRunDoctorFix(t *testing.T) {
	home := filepath.Join(t.TempDir(), "codegraph-home")
	t.Setenv("CODEGRAPH_HOME", home)
	prev := startupVersionCheck
	startupVersionCheck = func(context.Context, io.Writer) {}
	t.Cleanup(func() {
		startupVersionCheck = prev
	})

	var out bytes.Buffer
	var errOut bytes.Buffer
	if err := Run(context.Background(), []string{"doctor", "--fix"}, &out, &errOut); err != nil {
		t.Fatalf("Run(doctor --fix) error = %v", err)
	}
	if !strings.Contains(out.String(), `"applied_fixes"`) {
		t.Fatalf("doctor --fix output missing applied_fixes: %s", out.String())
	}
}

func TestRunRootHelp(t *testing.T) {
	prev := startupVersionCheck
	startupVersionCheck = func(context.Context, io.Writer) {}
	t.Cleanup(func() {
		startupVersionCheck = prev
	})

	for _, args := range [][]string{
		{},
		{"--help"},
		{"-h"},
	} {
		t.Run(strings.Join(append([]string{"root"}, args...), "_"), func(t *testing.T) {
			var out bytes.Buffer
			var errOut bytes.Buffer
			if err := Run(context.Background(), args, &out, &errOut); err != nil {
				t.Fatalf("Run(%v) error = %v", args, err)
			}
			if got := out.String(); !strings.Contains(got, "Usage:") || !strings.Contains(got, "Commands:") {
				t.Fatalf("help output missing sections, output:\n%s", got)
			}
			if !strings.Contains(out.String(), "--version") || !strings.Contains(out.String(), "-v") {
				t.Fatalf("root help missing version flags, output:\n%s", out.String())
			}
		})
	}
}

func TestRunHelpCommandWithSubcommand(t *testing.T) {
	prev := startupVersionCheck
	startupVersionCheck = func(context.Context, io.Writer) {}
	t.Cleanup(func() {
		startupVersionCheck = prev
	})

	var out bytes.Buffer
	var errOut bytes.Buffer
	if err := Run(context.Background(), []string{"help", "find_symbol"}, &out, &errOut); err != nil {
		t.Fatalf("Run(help find_symbol) error = %v", err)
	}
	if got := out.String(); !strings.Contains(got, "Usage:") || !strings.Contains(got, "find_symbol <repo-path> <query>") {
		t.Fatalf("help find_symbol output unexpected, output:\n%s", got)
	}
}

func TestRunCommandHelpFlag(t *testing.T) {
	prev := startupVersionCheck
	startupVersionCheck = func(context.Context, io.Writer) {}
	t.Cleanup(func() {
		startupVersionCheck = prev
	})

	var out bytes.Buffer
	var errOut bytes.Buffer
	if err := Run(context.Background(), []string{"find_symbol", ".", "--help"}, &out, &errOut); err != nil {
		t.Fatalf("Run(find_symbol --help) error = %v", err)
	}
	if got := out.String(); !strings.Contains(got, "Usage:") || !strings.Contains(got, "find_symbol <repo-path> <query>") {
		t.Fatalf("find_symbol --help output unexpected, output:\n%s", got)
	}
}

func TestRunVersionFlagsAndCommand(t *testing.T) {
	want := "codegraph " + version.Current()
	for _, args := range [][]string{
		{"--version"},
		{"-v"},
		{"version"},
	} {
		t.Run(strings.Join(args, "_"), func(t *testing.T) {
			called := false
			prev := startupVersionCheck
			startupVersionCheck = func(context.Context, io.Writer) { called = true }
			t.Cleanup(func() {
				startupVersionCheck = prev
			})

			var out bytes.Buffer
			var errOut bytes.Buffer
			if err := Run(context.Background(), args, &out, &errOut); err != nil {
				t.Fatalf("Run(%v) error = %v", args, err)
			}
			if called {
				t.Fatalf("startupVersionCheck called for %v", args)
			}
			got := out.String()
			if !strings.Contains(got, want) {
				t.Fatalf("version output missing %q, output:\n%s", want, got)
			}
			if strings.Contains(got, "Latest version") {
				t.Fatalf("version output leaked latest-version text, output:\n%s", got)
			}
		})
	}
}

func TestRunRemovedVersionCheckCommand(t *testing.T) {
	prev := startupVersionCheck
	startupVersionCheck = func(context.Context, io.Writer) {}
	t.Cleanup(func() {
		startupVersionCheck = prev
	})

	var out bytes.Buffer
	var errOut bytes.Buffer
	err := Run(context.Background(), []string{"version-check"}, &out, &errOut)
	if err == nil {
		t.Fatalf("Run(version-check) error = nil")
	}
	if !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("Run(version-check) error = %v", err)
	}
}

func TestParseBenchmarkMetrics(t *testing.T) {
	output := `
goos: windows
goarch: amd64
BenchmarkStoreQuery-16           165           7201 ns/op            256 B/op          4 allocs/op
BenchmarkIndexerUpdate-16         55          18123 ns/op            640 B/op         10 allocs/op
PASS
`
	parsed := parseBenchmarkMetrics(output)
	if len(parsed) != 2 {
		t.Fatalf("parsed benchmark count = %d, want 2", len(parsed))
	}
	if parsed["BenchmarkStoreQuery-16"].NsPerOp != 7201 {
		t.Fatalf("unexpected ns/op for BenchmarkStoreQuery-16: %#v", parsed["BenchmarkStoreQuery-16"])
	}
	if parsed["BenchmarkIndexerUpdate-16"].AllocsPerOp != 10 {
		t.Fatalf("unexpected allocs/op for BenchmarkIndexerUpdate-16: %#v", parsed["BenchmarkIndexerUpdate-16"])
	}
}

func TestComputeMetricDelta(t *testing.T) {
	delta := computeMetricDelta(
		benchmarkMetric{NsPerOp: 110, BytesPerOp: 55, AllocsPerOp: 11},
		benchmarkMetric{NsPerOp: 100, BytesPerOp: 50, AllocsPerOp: 10},
	)
	nsRaw, ok := delta["ns_per_op"]
	if !ok {
		t.Fatalf("missing ns_per_op delta: %#v", delta)
	}
	ns, ok := nsRaw.(map[string]any)
	if !ok {
		t.Fatalf("ns_per_op delta has unexpected type: %T", nsRaw)
	}
	if ns["delta_pct"] != 10.0 {
		t.Fatalf("ns_per_op delta_pct = %v, want 10", ns["delta_pct"])
	}
}

// eventWriter collects JSONL output and calls onEvent synchronously for each
// complete line, so a test can react to a lifecycle event without sleeping.
type eventWriter struct {
	mu      sync.Mutex
	partial []byte
	events  []map[string]any
	lines   []string
	onEvent func(map[string]any)
	t       *testing.T
}

func (w *eventWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	w.partial = append(w.partial, p...)
	var ready []map[string]any
	for {
		i := bytes.IndexByte(w.partial, '\n')
		if i < 0 {
			break
		}
		line := string(w.partial[:i])
		w.partial = w.partial[i+1:]
		var event map[string]any
		if err := json.Unmarshal([]byte(line), &event); err != nil {
			w.t.Errorf("JSONL line does not decode independently: %v: %q", err, line)
			continue
		}
		w.lines = append(w.lines, line)
		w.events = append(w.events, event)
		ready = append(ready, event)
	}
	w.mu.Unlock()
	if w.onEvent != nil {
		for _, event := range ready {
			w.onEvent(event)
		}
	}
	return len(p), nil
}

func (w *eventWriter) snapshot() ([]map[string]any, []string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]map[string]any(nil), w.events...), append([]string(nil), w.lines...)
}

func setupWatchRepo(t *testing.T) string {
	t.Helper()
	home := filepath.Join(t.TempDir(), "codegraph-home")
	t.Setenv("CODEGRAPH_HOME", home)
	repoRoot := filepath.Join(t.TempDir(), "repo")
	if err := os.MkdirAll(repoRoot, 0o755); err != nil {
		t.Fatalf("MkdirAll(repoRoot) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "main.go"), []byte("package main\nfunc main() {}\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(main.go) error = %v", err)
	}
	prev := startupVersionCheck
	startupVersionCheck = func(context.Context, io.Writer) {}
	t.Cleanup(func() { startupVersionCheck = prev })
	return repoRoot
}

// watchStatusStates returns the run_state sequence and checks the common
// watch_status fields on every status event.
func watchStatusStates(t *testing.T, events []map[string]any) []string {
	t.Helper()
	var states []string
	for _, event := range events {
		if event["type"] != "watch_status" {
			continue
		}
		for _, key := range []string{"repo_root", "repo_id", "run_state", "observed_at"} {
			if _, ok := event[key]; !ok {
				t.Fatalf("watch_status missing %q: %v", key, event)
			}
		}
		raw, _ := event["observed_at"].(string)
		at, err := time.Parse(time.RFC3339Nano, raw)
		if err != nil {
			t.Fatalf("observed_at %q is not RFC3339Nano: %v", raw, err)
		}
		if !strings.HasSuffix(raw, "Z") || at.Location() != time.UTC {
			t.Fatalf("observed_at %q is not UTC", raw)
		}
		states = append(states, event["run_state"].(string))
	}
	return states
}

// assertLegacyWatchEvents checks that legacy events keep their fields, that
// watch_started is the first event and watch_stopped the last, and that the
// terminal watch_status immediately precedes watch_stopped.
func assertLegacyWatchEvents(t *testing.T, events []map[string]any) {
	t.Helper()
	if len(events) < 3 {
		t.Fatalf("too few events: %v", events)
	}
	first, last := events[0], events[len(events)-1]
	if first["type"] != "watch_started" || first["repo_root"] == nil || first["repo_id"] == nil {
		t.Fatalf("first event = %v, want watch_started with repo fields", first)
	}
	if last["type"] != "watch_stopped" || last["repo_root"] == nil || last["repo_id"] == nil || last["stats"] == nil {
		t.Fatalf("last event = %v, want watch_stopped with repo fields and stats", last)
	}
	if events[1]["type"] != "watch_status" || events[1]["run_state"] != "starting" {
		t.Fatalf("second event = %v, want watch_status starting", events[1])
	}
	prev := events[len(events)-2]
	if prev["type"] != "watch_status" {
		t.Fatalf("event before watch_stopped = %v, want terminal watch_status", prev)
	}
	for _, event := range events[:len(events)-1] {
		if event["type"] == "watch_stopped" {
			t.Fatalf("watch_stopped is not the final event: %v", events)
		}
	}
}

func TestRunWatchJSONLLifecycleCancelOnWatching(t *testing.T) {
	repoRoot := setupWatchRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := &eventWriter{t: t}
	out.onEvent = func(event map[string]any) {
		if event["type"] == "watch_status" && event["run_state"] == "watching" {
			cancel()
		}
	}
	var errOut bytes.Buffer
	err := Run(ctx, []string{"watch", "--jsonl", repoRoot}, out, &errOut)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run(watch) error = %v, want context.Canceled", err)
	}
	events, _ := out.snapshot()
	states := watchStatusStates(t, events)
	if want := []string{"starting", "watching", "stopped"}; strings.Join(states, ",") != strings.Join(want, ",") {
		t.Fatalf("run states = %v, want %v", states, want)
	}
	assertLegacyWatchEvents(t, events)
	terminal := events[len(events)-2]
	if terminal["reason"] != "context_canceled" {
		t.Fatalf("terminal reason = %v, want context_canceled", terminal["reason"])
	}
	if _, ok := terminal["error"]; ok {
		t.Fatalf("stopped status must not carry error: %v", terminal)
	}
	if got := events[len(events)-1]["error"]; got != "context canceled" {
		t.Fatalf("legacy watch_stopped error = %v, want context canceled", got)
	}
}

func TestRunWatchJSONLCancelBeforeReadyNeverWatching(t *testing.T) {
	repoRoot := setupWatchRepo(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	out := &eventWriter{t: t}
	out.onEvent = func(event map[string]any) {
		if event["type"] == "watch_status" && event["run_state"] == "starting" {
			cancel()
		}
	}
	var errOut bytes.Buffer
	err := Run(ctx, []string{"watch", "--jsonl", repoRoot}, out, &errOut)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run(watch) error = %v, want context.Canceled", err)
	}
	events, _ := out.snapshot()
	states := watchStatusStates(t, events)
	if want := []string{"starting", "stopped"}; strings.Join(states, ",") != strings.Join(want, ",") {
		t.Fatalf("run states = %v, want %v", states, want)
	}
	assertLegacyWatchEvents(t, events)
}

func TestRunWatchJSONLStartupFailureIsSanitized(t *testing.T) {
	repoRoot := setupWatchRepo(t)
	// A directory where the repo config file belongs makes the watcher's
	// config load fail with an error that names the absolute path.
	if err := os.MkdirAll(config.RepoConfigPath(repoRoot), 0o755); err != nil {
		t.Fatalf("MkdirAll(config) error = %v", err)
	}
	out := &eventWriter{t: t}
	var errOut bytes.Buffer
	err := Run(context.Background(), []string{"watch", "--jsonl", repoRoot}, out, &errOut)
	if err == nil {
		t.Fatal("Run(watch) error = nil, want config load failure")
	}
	events, lines := out.snapshot()
	states := watchStatusStates(t, events)
	if want := []string{"starting", "failed"}; strings.Join(states, ",") != strings.Join(want, ",") {
		t.Fatalf("run states = %v, want %v", states, want)
	}
	assertLegacyWatchEvents(t, events)
	terminal, stopped := events[len(events)-2], events[len(events)-1]
	statusErr, _ := terminal["error"].(string)
	legacyErr, _ := stopped["error"].(string)
	for _, detail := range []string{statusErr, legacyErr} {
		if !strings.Contains(detail, "<repo>") || !strings.Contains(detail, "config.json") {
			t.Fatalf("sanitized error %q lost useful cause text", detail)
		}
	}
	canonical := events[0]["repo_root"].(string)
	for _, line := range lines[len(lines)-2:] {
		var event map[string]any
		_ = json.Unmarshal([]byte(line), &event)
		if strings.Contains(event["error"].(string), canonical) || strings.Contains(event["error"].(string), repoRoot) {
			t.Fatalf("error detail leaks repository path: %s", line)
		}
	}
}

func TestSanitizeWatchErrorRedactsAndBounds(t *testing.T) {
	home := filepath.Join(t.TempDir(), "user-home")
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	repoRoot := filepath.Join(home, "src", "repo")
	err := fmt.Errorf("open %s: permission denied; cache %s: busy",
		filepath.Join(repoRoot, "a.go"), filepath.Join(home, ".cache", "x"))
	got := sanitizeWatchError(err, repoRoot)
	if strings.Contains(got, home) {
		t.Fatalf("sanitized error leaks home path: %q", got)
	}
	for _, want := range []string{"<repo>", "permission denied", "~", "busy"} {
		if !strings.Contains(got, want) {
			t.Fatalf("sanitized error %q missing %q", got, want)
		}
	}
	long := sanitizeWatchError(errors.New(strings.Repeat("é", maxWatchErrorBytes)), repoRoot)
	if len(long) > maxWatchErrorBytes+len("...(truncated)") || !utf8.ValidString(long) {
		t.Fatalf("bounded error has length %d or invalid UTF-8", len(long))
	}
}
