package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
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

	dbPath := filepath.Join(repoRoot, "codegraph.sqlite")
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
		{"help"},
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
	if err := Run(context.Background(), []string{"help", "index"}, &out, &errOut); err != nil {
		t.Fatalf("Run(help index) error = %v", err)
	}
	if got := out.String(); !strings.Contains(got, "Usage:") || !strings.Contains(got, "index <repo-path>") {
		t.Fatalf("help index output unexpected, output:\n%s", got)
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
	if err := Run(context.Background(), []string{"find-symbol", ".", "--help"}, &out, &errOut); err != nil {
		t.Fatalf("Run(find-symbol --help) error = %v", err)
	}
	if got := out.String(); !strings.Contains(got, "Usage:") || !strings.Contains(got, "find-symbol <repo-path> <query>") {
		t.Fatalf("find-symbol --help output unexpected, output:\n%s", got)
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
