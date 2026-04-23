package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/isink17/codegraph/internal/appname"
	"github.com/isink17/codegraph/internal/config"
	"github.com/isink17/codegraph/internal/doctor"
	"github.com/isink17/codegraph/internal/embedding"
	"github.com/isink17/codegraph/internal/export"
	"github.com/isink17/codegraph/internal/gotool"
	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/logging"
	"github.com/isink17/codegraph/internal/mcp"
	"github.com/isink17/codegraph/internal/platform"
	"github.com/isink17/codegraph/internal/query"
	"github.com/isink17/codegraph/internal/store"
	"github.com/isink17/codegraph/internal/versioncheck"
	"github.com/isink17/codegraph/internal/viz"
	"github.com/isink17/codegraph/internal/watcher"
)

var startupVersionCheck = versioncheck.NotifyIfOutdated

type stringListFlag []string

func (s *stringListFlag) String() string {
	return strings.Join(*s, ",")
}

func (s *stringListFlag) Set(value string) error {
	*s = append(*s, value)
	return nil
}

func Run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	startupVersionCheck(ctx, stderr)

	if len(args) == 0 || isRootHelpFlag(args[0]) {
		printRootHelp(stdout)
		return nil
	}

	globalCfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := logging.New(globalCfg.DefaultLogLevel, stderr)
	_ = logger

	invokedName := args[0]
	cmd, ok := lookupCommand(invokedName)
	if !ok {
		return fmt.Errorf("unknown command %q", invokedName)
	}

	// Per-command help: `<binary> <command> --help|-h`.
	if hasHelpFlag(args[1:]) {
		printCommandHelp(stdout, cmd, invokedName)
		return nil
	}

	if err := cmd.run(ctx, globalCfg, stdout, stderr, invokedName, args[1:]); err != nil {
		// Treat user-invoked flag help as success (handlers may return flag.ErrHelp).
		if errors.Is(err, flag.ErrHelp) {
			return nil
		}
		return err
	}
	return nil
}

func isRootHelpFlag(arg string) bool {
	switch arg {
	case "-h", "--help":
		return true
	default:
		return false
	}
}

func hasHelpFlag(args []string) bool {
	for _, a := range args {
		if isRootHelpFlag(a) {
			return true
		}
	}
	return false
}

func parseOptionalRepoRootArg(fs *flag.FlagSet, args []string, repoRootFlag *string, defaultRepoRoot string) (string, error) {
	repoRootArg := ""
	parseArgs := args
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		repoRootArg = args[0]
		parseArgs = args[1:]
	}
	if err := fs.Parse(parseArgs); err != nil {
		return "", err
	}
	repoRoot := strings.TrimSpace(*repoRootFlag)
	if repoRoot == "" {
		repoRoot = strings.TrimSpace(repoRootArg)
	}
	if repoRoot == "" {
		repoRoot = defaultRepoRoot
	}
	return repoRoot, nil
}

func runDoctor(ctx context.Context, cfg config.Config, stdout io.Writer, args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fix := fs.Bool("fix", false, "apply non-destructive fixes")
	repoRootFlag := fs.String("repo-root", "", "repository root to inspect (optional)")

	defaultRepoRoot := ""
	if config.IsRepoDBDir(cfg.DBDir) {
		defaultRepoRoot = "."
	}
	repoRoot, err := parseOptionalRepoRootArg(fs, args, repoRootFlag, defaultRepoRoot)
	if err != nil {
		return err
	}

	dbPath := ""
	if repoRoot != "" {
		canonical, err := store.CanonicalRepoPath(repoRoot)
		if err != nil {
			return err
		}
		p, err := dbPathForRepo(cfg, repoRoot, canonical)
		if err != nil {
			return err
		}
		dbPath = p
	}

	report, err := doctor.RunWithFix(*fix, dbPath)
	if err != nil {
		return err
	}
	return writeJSON(stdout, report)
}

func runConfig(cfg config.Config, stdout io.Writer, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: %s config <show|edit-path|validate|init>", appname.BinaryName)
	}
	switch args[0] {
	case "show":
		path, err := config.ConfigPath()
		if err != nil {
			return err
		}
		return writeJSON(stdout, map[string]any{
			"path":   path,
			"config": cfg,
		})
	case "edit-path":
		path, err := config.ConfigPath()
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout, path)
		return err
	case "validate":
		issues := validateConfig(cfg)
		if issues == nil {
			issues = []string{}
		}
		path, err := config.ConfigPath()
		if err != nil {
			return err
		}
		return writeJSON(stdout, map[string]any{
			"path":   path,
			"valid":  len(issues) == 0,
			"issues": issues,
		})
	case "init":
		fs := flag.NewFlagSet("config init", flag.ContinueOnError)
		fs.SetOutput(io.Discard)
		repoRootFlag := fs.String("repo", "", "repository root")
		force := fs.Bool("force", false, "overwrite existing repo config")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		repoRoot := strings.TrimSpace(*repoRootFlag)
		if repoRoot == "" && fs.NArg() > 0 {
			repoRoot = strings.TrimSpace(fs.Arg(0))
		}
		if repoRoot == "" {
			repoRoot = "."
		}
		absRepoRoot, err := filepath.Abs(repoRoot)
		if err != nil {
			return err
		}
		cfgPath := config.RepoConfigPath(absRepoRoot)
		if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
			return err
		}
		if !*force {
			if _, err := os.Stat(cfgPath); err == nil {
				return fmt.Errorf("repo config already exists: %s (use --force to overwrite)", cfgPath)
			} else if !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
		repoCfg := config.RepoConfig{
			Include:          []string{"**/*"},
			Exclude:          append(config.DefaultExcludes, config.HardcodedSkips...),
			Languages:        append([]string(nil), cfg.DefaultLanguages...),
			WatchDebounce:    cfg.WatchDebounce,
			SemanticMaxTerms: 8,
			MaxFileSizeBytes: 8 * 1024 * 1024,
			ParseErrorPolicy: "best_effort",
		}
		data, err := json.MarshalIndent(repoCfg, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(cfgPath, append(data, '\n'), 0o644); err != nil {
			return err
		}
		return writeJSON(stdout, map[string]any{
			"path":      cfgPath,
			"repo_root": absRepoRoot,
			"created":   true,
		})
	default:
		return fmt.Errorf("unknown config subcommand %q", args[0])
	}
}

type benchmarkMetric struct {
	NsPerOp     float64 `json:"ns_per_op,omitempty"`
	BytesPerOp  float64 `json:"bytes_per_op,omitempty"`
	AllocsPerOp float64 `json:"allocs_per_op,omitempty"`
}

type benchmarkBaseline struct {
	CreatedAt  string                     `json:"created_at"`
	Command    []string                   `json:"command"`
	Count      int                        `json:"count"`
	Benchtime  string                     `json:"benchtime"`
	GoVersion  string                     `json:"go_version,omitempty"`
	GOOS       string                     `json:"goos,omitempty"`
	GOARCH     string                     `json:"goarch,omitempty"`
	GOMAXPROCS string                     `json:"gomaxprocs,omitempty"`
	SQLite     string                     `json:"sqlite_driver,omitempty"`
	BenchCtx   map[string]any             `json:"bench_ctx,omitempty"`
	Env        map[string]string          `json:"env,omitempty"`
	Benchmarks map[string]benchmarkMetric `json:"benchmarks"`
}

func parseBenchmarkMetrics(output string) map[string]benchmarkMetric {
	metrics := make(map[string]benchmarkMetric)
	for line := range strings.SplitSeq(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "Benchmark") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		name := fields[0]
		var m benchmarkMetric
		for i := 0; i+1 < len(fields); i++ {
			value, err := strconv.ParseFloat(fields[i], 64)
			if err != nil {
				continue
			}
			switch fields[i+1] {
			case "ns/op":
				m.NsPerOp = value
			case "B/op":
				m.BytesPerOp = value
			case "allocs/op":
				m.AllocsPerOp = value
			}
		}
		if m.NsPerOp != 0 || m.BytesPerOp != 0 || m.AllocsPerOp != 0 {
			metrics[name] = m
		}
	}
	return metrics
}

func computeMetricDelta(current, baseline benchmarkMetric) map[string]any {
	out := map[string]any{}
	add := func(name string, cur, base float64) {
		if cur == 0 && base == 0 {
			return
		}
		item := map[string]any{
			"current":  cur,
			"baseline": base,
		}
		if base != 0 {
			item["delta_pct"] = ((cur - base) / base) * 100.0
		}
		out[name] = item
	}
	add("ns_per_op", current.NsPerOp, baseline.NsPerOp)
	add("bytes_per_op", current.BytesPerOp, baseline.BytesPerOp)
	add("allocs_per_op", current.AllocsPerOp, baseline.AllocsPerOp)
	return out
}

func extractBenchmarkContext(output string) map[string]any {
	// Keep this intentionally narrow: only pull a few high-signal markers that help
	// interpret perf comparisons (fixture sizing, driver selection, etc.).
	keys := []string{
		"fixture_files",
		"sqlite_driver",
	}
	out := map[string]any{}
	for line := range strings.SplitSeq(output, "\n") {
		line = strings.TrimSpace(line)
		for _, k := range keys {
			needle := k + "="
			idx := strings.Index(line, needle)
			if idx == -1 {
				continue
			}
			raw := strings.TrimSpace(line[idx+len(needle):])
			if raw == "" {
				continue
			}
			switch k {
			case "fixture_files":
				if n, err := strconv.Atoi(raw); err == nil {
					out[k] = n
				}
			default:
				out[k] = raw
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func runBenchmark(ctx context.Context, stdout io.Writer, args []string) error {
	fs := flag.NewFlagSet("benchmark", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	count := fs.Int("count", 1, "number of benchmark runs")
	benchtime := fs.String("benchtime", "100ms", "benchmark time per test")
	saveBaseline := fs.Bool("save-baseline", true, "save current benchmark result as baseline")
	files := fs.Int("files", 0, "fixture file count (sets CODEGRAPH_BENCH_FILES)")
	gomaxprocs := fs.Int("gomaxprocs", 0, "GOMAXPROCS for benchmark subprocess (0 = default)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *count <= 0 {
		*count = 1
	}
	cmdArgs := []string{
		"test",
		"./internal/indexer",
		"./internal/store",
		"./internal/mcp",
		"-v",
		"-run", "^$",
		"-bench", ".",
		"-benchmem",
		"-count", strconv.Itoa(*count),
		"-benchtime", *benchtime,
	}
	cmd := exec.CommandContext(ctx, "go", cmdArgs...)
	env := append([]string(nil), os.Environ()...)
	gocachePath, err := filepath.Abs(filepath.Join(".", ".gocache"))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(gocachePath, 0o755); err != nil {
		return err
	}
	env = append(env, "GOCACHE="+gocachePath)
	benchEnv := map[string]string{}
	if *files > 0 {
		v := strconv.Itoa(*files)
		env = append(env, "CODEGRAPH_BENCH_FILES="+v)
		benchEnv["CODEGRAPH_BENCH_FILES"] = v
	} else if v := strings.TrimSpace(os.Getenv("CODEGRAPH_BENCH_FILES")); v != "" {
		benchEnv["CODEGRAPH_BENCH_FILES"] = v
	}
	if *gomaxprocs > 0 {
		v := strconv.Itoa(*gomaxprocs)
		env = append(env, "GOMAXPROCS="+v)
		benchEnv["GOMAXPROCS"] = v
	} else if v := strings.TrimSpace(os.Getenv("GOMAXPROCS")); v != "" {
		benchEnv["GOMAXPROCS"] = v
	}
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	benchCtx := extractBenchmarkContext(string(out))
	result := map[string]any{
		"command":       append([]string{"go"}, cmdArgs...),
		"count":         *count,
		"benchtime":     *benchtime,
		"go_version":    runtime.Version(),
		"goos":          runtime.GOOS,
		"goarch":        runtime.GOARCH,
		"sqlite_driver": store.SQLiteDriverName(),
		"env":           benchEnv,
		"bench_ctx":     benchCtx,
		"output":        string(out),
	}
	if err != nil {
		result["ok"] = false
		result["error"] = err.Error()
		return writeJSON(stdout, result)
	}
	parsed := parseBenchmarkMetrics(string(out))
	result["benchmarks"] = parsed
	paths, pathErr := platform.DefaultPaths()
	if pathErr == nil {
		baselinePath := filepath.Join(paths.CacheDir, "bench_baseline.json")
		result["baseline_path"] = baselinePath
		var baseline benchmarkBaseline
		if data, readErr := os.ReadFile(baselinePath); readErr == nil {
			if unmarshalErr := json.Unmarshal(data, &baseline); unmarshalErr == nil && len(baseline.Benchmarks) > 0 {
				deltas := map[string]any{}
				for name, current := range parsed {
					base, ok := baseline.Benchmarks[name]
					if !ok {
						continue
					}
					deltas[name] = computeMetricDelta(current, base)
				}
				if len(deltas) > 0 {
					result["delta_vs_baseline"] = deltas
				}
				result["baseline_created_at"] = baseline.CreatedAt
			}
		}
		if *saveBaseline {
			if mkdirErr := os.MkdirAll(filepath.Dir(baselinePath), 0o755); mkdirErr == nil {
				gomaxLabel := benchEnv["GOMAXPROCS"]
				if gomaxLabel == "" {
					gomaxLabel = "default"
				}
				payload := benchmarkBaseline{
					CreatedAt:  time.Now().UTC().Format(time.RFC3339),
					Command:    append([]string{"go"}, cmdArgs...),
					Count:      *count,
					Benchtime:  *benchtime,
					GoVersion:  runtime.Version(),
					GOOS:       runtime.GOOS,
					GOARCH:     runtime.GOARCH,
					GOMAXPROCS: gomaxLabel,
					SQLite:     store.SQLiteDriverName(),
					BenchCtx:   benchCtx,
					Env:        benchEnv,
					Benchmarks: parsed,
				}
				if data, marshalErr := json.MarshalIndent(payload, "", "  "); marshalErr == nil {
					if writeErr := os.WriteFile(baselinePath, append(data, '\n'), 0o644); writeErr == nil {
						result["baseline_saved"] = true
					}
				}
			}
		}
	}
	result["ok"] = true
	return writeJSON(stdout, result)
}

func validateConfig(cfg config.Config) []string {
	var issues []string
	switch strings.ToLower(strings.TrimSpace(cfg.DBPerformanceProfile)) {
	case "balanced", "durable", "fast":
	default:
		issues = append(issues, "db_performance_profile must be one of: balanced, durable, fast")
	}
	if strings.TrimSpace(cfg.DBDir) == "" {
		issues = append(issues, "db_dir must not be empty")
	}
	if strings.TrimSpace(cfg.CacheDir) == "" {
		issues = append(issues, "cache_dir must not be empty")
	}
	if cfg.WatchDebounce < 0 {
		issues = append(issues, "watch_debounce must be >= 0")
	}
	return issues
}

func runInstall(stdout io.Writer) error {
	cfg, err := config.Default()
	if err != nil {
		return err
	}
	paths, err := platform.DefaultPaths()
	if err != nil {
		return err
	}
	for _, dir := range []string{paths.ConfigDir, paths.DataDir, cfg.DBDir, paths.CacheDir} {
		if config.IsRepoDBDir(dir) {
			continue
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		testFile := filepath.Join(dir, ".write-test")
		if err := os.WriteFile(testFile, []byte("ok"), 0o644); err != nil {
			return fmt.Errorf("directory not writable: %s: %w", dir, err)
		}
		_ = os.Remove(testFile)
	}
	configPath, created, err := config.SaveIfMissing(cfg)
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "%s install complete\n\n", appname.BinaryName)
	fmt.Fprintf(stdout, "config: %s\n", configPath)
	if created {
		fmt.Fprintln(stdout, "default config: created")
	} else {
		fmt.Fprintln(stdout, "default config: already present")
	}

	// Auto-configure MCP for detected AI tools.
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Auto-configuring MCP for detected AI tools:")
	configured := autoConfigureMCP(stdout)
	if configured > 0 {
		fmt.Fprintf(stdout, "\n%d tool(s) auto-configured.\n", configured)
	}

	// Print Claude Code permissions snippet.
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Claude Code permissions snippet (add to .claude/settings.json):")
	fmt.Fprintln(stdout, `{
  "permissions": {
    "allow": [
      "mcp__codegraph__*"
    ]
  }
}`)

	// Always print manual snippets as a fallback / reference.
	codexSnippet := fmt.Sprintf("[mcp_servers.codegraph]\ncommand = %q\nargs = [\"serve\", \"--repo-root\", \"/absolute/path/to/repo\"]\nstartup_timeout_sec = 60", appname.BinaryName)
	clientSnippet := fmt.Sprintf(`{"mcpServers":{"codegraph":{"command":%q,"args":["serve","--repo-root","/absolute/path/to/repo"]}}}`, appname.BinaryName)
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Manual MCP snippets (if auto-configure did not apply):")
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Codex MCP snippet:")
	fmt.Fprintln(stdout, codexSnippet)
	for _, label := range []string{"Gemini CLI", "Claude/Desktop", "Cursor", "Windsurf"} {
		fmt.Fprintln(stdout)
		fmt.Fprintf(stdout, "%s MCP snippet:\n", label)
		fmt.Fprintln(stdout, clientSnippet)
	}
	fmt.Fprintln(stdout)
	fmt.Fprintf(stdout, "If `%s` is not found after `go install`, your Go bin directory is probably not on PATH.\n", appname.BinaryName)
	fmt.Fprintln(stdout, "Check expected Go bin locations:")
	for _, dir := range gotool.BinPathHints() {
		fmt.Fprintf(stdout, "  - %s\n", dir)
	}
	fmt.Fprintf(stdout, "Then reopen your shell and verify: %s\n", gotool.VerifyCommandHint(appname.BinaryName))
	return nil
}

func runIndex(ctx context.Context, cfg config.Config, stdout io.Writer, cmdName string, args []string, update bool) error {
	fs := flag.NewFlagSet(cmdName, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	force := fs.Bool("force", false, "re-index files even if unchanged")
	jsonl := false
	filtered := make([]string, 0, len(args))
	for _, arg := range args {
		if arg == "--jsonl" {
			jsonl = true
			continue
		}
		filtered = append(filtered, arg)
	}
	if err := fs.Parse(filtered); err != nil {
		return err
	}
	repoRoot := "."
	if fs.NArg() > 0 {
		repoRoot = fs.Arg(0)
	}
	app, repo, repoID, err := openApp(ctx, cfg, repoRoot)
	if err != nil {
		return err
	}
	defer app.Close()
	_ = repo
	opts := indexer.Options{RepoRoot: repo.RootPath, Force: *force}
	var summary store.ScanSummary
	if update {
		opts.ScanKind = "update"
		summary, err = app.Indexer.Update(ctx, opts)
	} else {
		opts.ScanKind = "index"
		summary, err = app.Indexer.Index(ctx, opts)
	}
	if err != nil {
		return err
	}
	stats, err := app.Query.Stats(ctx, repoID)
	if err != nil {
		return err
	}
	if jsonl {
		if err := writeJSONL(stdout, map[string]any{
			"type":    "scan_summary",
			"command": map[bool]string{true: "update", false: "index"}[update],
			"data":    summary,
		}); err != nil {
			return err
		}
		_ = writeJSONL(stdout, map[string]any{
			"type": "scan_phases",
			"data": map[string]any{
				"existing_load_ms":  summary.ExistingLoadMS,
				"walk_ms":           summary.WalkMS,
				"process_wall_ms":   summary.ProcessWallMS,
				"task_ms":           summary.TaskMS,
				"task_other_ms":     summary.TaskOtherMS,
				"read_ms":           summary.ReadMS,
				"hash_ms":           summary.HashMS,
				"adapter_parse_ms":  summary.AdapterParseMS,
				"write_ms":          summary.WriteMS,
				"write_metadata_ms": summary.WriteMetadataMS,
				"write_replace_ms":  summary.WriteReplaceMS,
				"embed_ms":          summary.EmbedMS,
				"mark_missing_ms":   summary.MarkMissingMS,
				"resolve_ms":        summary.ResolveMS,
				"duration_ms":       summary.DurationMS,
			},
		})
		return writeJSONL(stdout, map[string]any{
			"type": "scan_stats",
			"data": stats,
		})
	}
	return writeJSON(stdout, map[string]any{"summary": summary, "stats": stats})
}

func runStats(ctx context.Context, cfg config.Config, stdout io.Writer, args []string) error {
	fs := flag.NewFlagSet("stats", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repoRootFlag := fs.String("repo-root", "", "repository root")
	if err := fs.Parse(args); err != nil {
		return err
	}
	repoRoot := "."
	if *repoRootFlag != "" {
		repoRoot = *repoRootFlag
	} else if fs.NArg() > 0 {
		repoRoot = fs.Arg(0)
	}
	app, _, repoID, err := openApp(ctx, cfg, repoRoot)
	if err != nil {
		return err
	}
	defer app.Close()
	stats, err := app.Query.Stats(ctx, repoID)
	if err != nil {
		return err
	}
	return writeJSON(stdout, stats)
}

func runQueryCommand(ctx context.Context, cfg config.Config, stdout io.Writer, queryKind, cmdName string, args []string) error {
	fs := flag.NewFlagSet(cmdName, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repoRootFlag := fs.String("repo-root", "", "repository root")
	queryFlag := fs.String("query", "", "query text")
	exact := fs.Bool("exact", false, "match symbol name exactly (find_symbol)")
	depth := fs.Int("depth", 2, "impact depth")
	limit := fs.Int("limit", 20, "result limit")
	offset := fs.Int("offset", 0, "result offset")
	var symbols stringListFlag
	var files stringListFlag
	fs.Var(&symbols, "symbol", "symbol name (repeatable)")
	fs.Var(&files, "file", "file path (repeatable)")
	if err := fs.Parse(args); err != nil {
		return err
	}

	repoRoot := "."
	if *repoRootFlag != "" {
		repoRoot = *repoRootFlag
	}
	queryValue := *queryFlag
	symbol := ""
	rest := fs.Args()
	if *repoRootFlag == "" && len(rest) > 0 {
		repoRoot = rest[0]
		rest = rest[1:]
	}
	if queryValue == "" && len(rest) > 0 {
		queryValue = rest[0]
	}
	if symbol == "" && len(symbols) > 0 {
		symbol = symbols[0]
	}
	if queryValue == "" {
		queryValue = symbol
	}
	if symbol == "" && queryValue != "" {
		symbol = queryValue
	}

	app, _, repoID, err := openApp(ctx, cfg, repoRoot)
	if err != nil {
		return err
	}
	defer app.Close()

	switch queryKind {
	case "find-symbol":
		if queryValue == "" {
			return fmt.Errorf("usage: %s %s <repo-path> <query> [--limit N] [--offset N]", appname.BinaryName, cmdName)
		}
		var (
			items []graph.Symbol
			err   error
		)
		if *exact {
			items, err = app.Query.FindSymbolExact(ctx, repoID, queryValue, *limit, *offset)
		} else {
			items, err = app.Query.FindSymbol(ctx, repoID, queryValue, *limit, *offset)
		}
		if err != nil {
			return err
		}
		if items == nil {
			items = []graph.Symbol{}
		}
		return writeJSON(stdout, map[string]any{
			"matches": items,
			"count":   len(items),
		})
	case "search":
		if queryValue == "" {
			return fmt.Errorf("usage: %s %s <repo-path> <query> [--limit N] [--offset N]", appname.BinaryName, cmdName)
		}
		items, err := app.Query.SearchSymbols(ctx, repoID, queryValue, *limit, *offset)
		if err != nil {
			return err
		}
		if items == nil {
			items = []graph.Symbol{}
		}
		return writeJSON(stdout, map[string]any{
			"matches": items,
			"count":   len(items),
		})
	case "callers":
		if symbol == "" {
			return fmt.Errorf("usage: %s %s <repo-path> <symbol> [--limit N] [--offset N]", appname.BinaryName, cmdName)
		}
		items, err := app.Query.FindCallers(ctx, repoID, symbol, 0, *limit, *offset)
		if err != nil {
			return err
		}
		if items == nil {
			items = []graph.Symbol{}
		}
		return writeJSON(stdout, map[string]any{
			"callers": items,
			"count":   len(items),
		})
	case "callees":
		if symbol == "" {
			return fmt.Errorf("usage: %s %s <repo-path> <symbol> [--limit N] [--offset N]", appname.BinaryName, cmdName)
		}
		items, err := app.Query.FindCallees(ctx, repoID, symbol, 0, *limit, *offset)
		if err != nil {
			return err
		}
		if items == nil {
			items = []graph.Symbol{}
		}
		return writeJSON(stdout, map[string]any{
			"callees": items,
			"count":   len(items),
		})
	case "impact":
		// Allow a positional symbol alongside --file, but do not override explicit --symbol flags.
		if symbol != "" && len(symbols) == 0 {
			symbols = append(symbols, symbol)
		}
		if len(symbols) == 0 && len(files) == 0 {
			return fmt.Errorf("usage: %s %s <repo-path> <symbol> [--file <path>]... [--depth N]", appname.BinaryName, cmdName)
		}
		data, err := app.Query.ImpactRadius(ctx, repoID, symbols, files, *depth)
		if err != nil {
			return err
		}
		return writeJSON(stdout, data)
	default:
		return fmt.Errorf("unknown query command %q", queryKind)
	}
}

func runServe(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repoRootFlag := fs.String("repo-root", "", "repository root")
	if err := fs.Parse(args); err != nil {
		return err
	}
	repoRoot := "."
	if *repoRootFlag != "" {
		repoRoot = *repoRootFlag
	} else if fs.NArg() > 0 {
		repoRoot = fs.Arg(0)
	}
	app, repo, repoID, err := openApp(ctx, cfg, repoRoot)
	if err != nil {
		return err
	}
	defer app.Close()
	server := mcp.NewServer(repo.RootPath, repoID, app.Store, app.Indexer, app.Query, stderr)
	if repoCfg, err := config.LoadRepo(repo.RootPath); err == nil {
		if repoCfg.Agent.Enabled || repoCfg.Agent.BaseURL != "" || repoCfg.Agent.Model != "" {
			server.SetAgentConfig(repoCfg.Agent.BaseURL, repoCfg.Agent.Model)
		}
	}
	return server.Serve(ctx, os.Stdin, stdout, stderr)
}

func runWatch(ctx context.Context, cfg config.Config, stdout io.Writer, args []string) error {
	fs := flag.NewFlagSet("watch", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jsonl := fs.Bool("jsonl", false, "output line-delimited JSON events")
	repoRootFlag := fs.String("repo-root", "", "repository root")
	if err := fs.Parse(args); err != nil {
		return err
	}
	repoRoot := "."
	if *repoRootFlag != "" {
		repoRoot = *repoRootFlag
	} else if fs.NArg() > 0 {
		repoRoot = fs.Arg(0)
	}
	app, repo, repoID, err := openApp(ctx, cfg, repoRoot)
	if err != nil {
		return err
	}
	defer app.Close()
	if *jsonl {
		if err := writeJSONL(stdout, map[string]any{
			"type":      "watch_started",
			"repo_root": repo.RootPath,
			"repo_id":   repoID,
		}); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(stdout, "watching %s\n", repo.RootPath)
	}
	w := watcher.New(app.Store, app.Indexer)
	var reporterDone sync.WaitGroup
	reporterCtx, reporterCancel := context.WithCancel(ctx)
	var writeMu sync.Mutex
	writeEvent := func(event map[string]any) {
		writeMu.Lock()
		defer writeMu.Unlock()
		_ = writeJSONL(stdout, event)
	}
	if *jsonl {
		reporterDone.Go(func() {
			heartbeatEvery := 2 * time.Second
			if cfg.WatchDebounce > 0 && cfg.WatchDebounce > heartbeatEvery {
				heartbeatEvery = cfg.WatchDebounce
			}
			ticker := time.NewTicker(heartbeatEvery)
			defer ticker.Stop()
			var prev watcher.WatchStats
			for {
				select {
				case <-reporterCtx.Done():
					return
				case <-ticker.C:
					current := w.Stats()
					writeEvent(map[string]any{
						"type":  "watch_heartbeat",
						"stats": current,
					})
					flushDelta := current.FlushRuns - prev.FlushRuns
					if flushDelta > 0 || current.FlushErrors > prev.FlushErrors || current.QueueErrors > prev.QueueErrors {
						writeEvent(map[string]any{
							"type":               "watch_flush_summary",
							"stats":              current,
							"delta_flush_runs":   flushDelta,
							"delta_flush_errors": current.FlushErrors - prev.FlushErrors,
							"delta_queue_errors": current.QueueErrors - prev.QueueErrors,
						})
					}
					prev = current
				}
			}
		})
	}
	err = w.Run(ctx, repo.RootPath, repoID, cfg.WatchDebounce)
	reporterCancel()
	reporterDone.Wait()
	if *jsonl {
		event := map[string]any{
			"type":  "watch_stopped",
			"stats": w.Stats(),
		}
		if err != nil {
			event["error"] = err.Error()
		}
		_ = writeJSONL(stdout, event)
	}
	return err
}

func runGraph(ctx context.Context, cfg config.Config, stdout io.Writer, args []string) error {
	usage := func() error {
		// `flag` stops parsing at the first non-flag arg, so flags must come before <repo-path>.
		return fmt.Errorf("usage: %s graph export [--format json|dot] [--symbol name] [--focus-symbol name] [--limit N] [--offset N] [--jsonl] <repo-path>", appname.BinaryName)
	}
	if len(args) == 0 {
		return usage()
	}
	switch args[0] {
	case "export":
	default:
		return usage()
	}
	fs := flag.NewFlagSet("graph export", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	format := fs.String("format", "json", "output format (json|dot)")
	symbol := fs.String("symbol", "", "focus symbol")
	focusSymbol := fs.String("focus-symbol", "", "focus symbol")
	limit := fs.Int("limit", 0, "page size for JSON export")
	offset := fs.Int("offset", 0, "offset for JSON export")
	jsonl := fs.Bool("jsonl", false, "stream graph as line-delimited JSON")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	repoRoot := "."
	if fs.NArg() > 0 {
		repoRoot = fs.Arg(0)
	}
	app, _, repoID, err := openApp(ctx, cfg, repoRoot)
	if err != nil {
		return err
	}
	defer app.Close()
	exp := export.New(app.Query)
	selectedSymbol := strings.TrimSpace(*symbol)
	if selectedSymbol == "" {
		selectedSymbol = strings.TrimSpace(*focusSymbol)
	}
	if *jsonl {
		return exp.StreamJSONL(ctx, stdout, repoID, *limit)
	}
	if *format == "dot" {
		out, err := exp.DOT(ctx, repoID, selectedSymbol, 2)
		if err != nil {
			return err
		}
		_, err = stdout.Write(out)
		return err
	}
	out, err := exp.JSONPaged(ctx, repoID, selectedSymbol, 2, *limit, *offset)
	if err != nil {
		return err
	}
	_, err = stdout.Write(out)
	return err
}

func runVisualize(ctx context.Context, cfg config.Config, stdout io.Writer, args []string) error {
	fs := flag.NewFlagSet("visualize", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repoRoot := fs.String("repo-root", ".", "repository root")
	symbol := fs.String("symbol", "", "focus on a specific symbol")
	output := fs.String("output", "", "write HTML to this file instead of opening a browser")
	depth := fs.Int("depth", 2, "traversal depth from focus symbol")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 && *repoRoot == "." {
		*repoRoot = fs.Arg(0)
	}

	app, _, repoID, err := openApp(ctx, cfg, *repoRoot)
	if err != nil {
		return err
	}
	defer app.Close()

	symbols, edges, err := app.Query.GraphSnapshot(ctx, repoID, strings.TrimSpace(*symbol), *depth)
	if err != nil {
		return err
	}

	if *output != "" {
		f, err := os.Create(*output)
		if err != nil {
			return err
		}
		defer f.Close()
		if err := viz.GenerateHTML(f, symbols, edges); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "wrote %s\n", *output)
		return nil
	}

	tmp, err := os.CreateTemp("", appname.BinaryName+"-viz-*.html")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	if err := viz.GenerateHTML(tmp, symbols, edges); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()

	fmt.Fprintf(stdout, "opening %s\n", tmpPath)
	return openBrowser(tmpPath)
}

func openBrowser(url string) error {
	switch runtime.GOOS {
	case "darwin":
		return exec.Command("open", url).Start()
	case "linux":
		return exec.Command("xdg-open", url).Start()
	case "windows":
		return exec.Command("cmd", "/c", "start", url).Start()
	default:
		return fmt.Errorf("unsupported platform %s; open %s manually", runtime.GOOS, url)
	}
}

func runClean(ctx context.Context, cfg config.Config, stdout io.Writer, args []string) error {
	fs := flag.NewFlagSet("clean", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	vacuum := fs.Bool("vacuum", false, "run VACUUM on databases")
	ftsOptimize := fs.Bool("fts-optimize", false, "run FTS optimize on databases (symbol_fts)")
	repoRootFlag := fs.String("repo-root", "", "repository root to clean")

	defaultRepoRoot := ""
	if config.IsRepoDBDir(cfg.DBDir) {
		defaultRepoRoot = "."
	}
	repoRoot, err := parseOptionalRepoRootArg(fs, args, repoRootFlag, defaultRepoRoot)
	if err != nil {
		return err
	}

	type dbResult struct {
		Path           string `json:"path"`
		Action         string `json:"action"`
		SizeBefore     int64  `json:"size_before_bytes"`
		SizeAfter      int64  `json:"size_after_bytes"`
		ReclaimedBytes int64  `json:"reclaimed_bytes"`
		Vacuumed       bool   `json:"vacuumed,omitempty"`
		FTSOptimized   bool   `json:"fts_optimized,omitempty"`
		FTSOptimizeMS  int64  `json:"fts_optimize_ms,omitempty"`
		CanonicalRepo  string `json:"canonical_repo,omitempty"`
		Error          string `json:"error,omitempty"`
	}
	report := map[string]any{
		"vacuum":       *vacuum,
		"fts_optimize": *ftsOptimize,
		"dbs":          []dbResult{},
	}
	results := make([]dbResult, 0)
	var reclaimed int64

	if repoRoot != "" {
		canonical, err := store.CanonicalRepoPath(repoRoot)
		if err != nil {
			return err
		}
		dbPath, err := dbPathForRepo(cfg, repoRoot, canonical)
		if err != nil {
			return err
		}
		res := dbResult{Path: dbPath, CanonicalRepo: canonical}
		before := fileSize(dbPath)
		res.SizeBefore = before
		s, err := store.OpenWithOptions(dbPath, store.OpenOptions{PerformanceProfile: cfg.DBPerformanceProfile})
		if err != nil {
			return err
		}
		defer s.Close()
		if *ftsOptimize {
			dur, err := s.OptimizeFTS(ctx)
			if err != nil {
				return err
			}
			res.FTSOptimized = true
			res.FTSOptimizeMS = dur.Milliseconds()
		}
		if *vacuum {
			if err := s.Vacuum(ctx); err != nil {
				return err
			}
			res.Vacuumed = true
			res.Action = "vacuumed"
		} else if *ftsOptimize {
			res.Action = "fts_optimized"
		} else {
			res.Action = "inspected"
		}
		after := fileSize(dbPath)
		res.SizeAfter = after
		if before > after {
			res.ReclaimedBytes = before - after
			reclaimed += res.ReclaimedBytes
		}
		results = append(results, res)
		report["dbs"] = results
		report["reclaimed_bytes"] = reclaimed
		return writeJSON(stdout, report)
	}

	if err := os.MkdirAll(cfg.DBDir, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(cfg.DBDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(strings.ToLower(entry.Name()), ".sqlite") {
			continue
		}
		dbPath := filepath.Join(cfg.DBDir, entry.Name())
		sizeBefore := fileSize(dbPath)
		res := dbResult{Path: dbPath, SizeBefore: sizeBefore}
		s, err := store.OpenWithOptions(dbPath, store.OpenOptions{PerformanceProfile: cfg.DBPerformanceProfile})
		if err != nil {
			res.Action = "skipped"
			res.Error = err.Error()
			results = append(results, res)
			continue
		}
		repo, ok, repoErr := s.PrimaryRepo(ctx)
		if repoErr != nil {
			_ = s.Close()
			res.Action = "skipped"
			res.Error = repoErr.Error()
			results = append(results, res)
			continue
		}
		if !ok {
			_ = s.Close()
			if err := os.Remove(dbPath); err == nil {
				res.Action = "deleted_orphan"
				res.ReclaimedBytes = sizeBefore
				reclaimed += sizeBefore
			} else {
				res.Action = "skipped"
				res.Error = err.Error()
			}
			results = append(results, res)
			continue
		}
		res.CanonicalRepo = repo.CanonicalPath
		if _, err := os.Stat(repo.CanonicalPath); err != nil {
			_ = s.Close()
			if err := os.Remove(dbPath); err == nil {
				res.Action = "deleted_orphan"
				res.ReclaimedBytes = sizeBefore
				reclaimed += sizeBefore
			} else {
				res.Action = "skipped"
				res.Error = err.Error()
			}
			results = append(results, res)
			continue
		}
		if !*vacuum {
			res.Action = "kept"
		}
		if *ftsOptimize {
			dur, err := s.OptimizeFTS(ctx)
			if err != nil {
				_ = s.Close()
				res.Action = "skipped"
				res.Error = err.Error()
				results = append(results, res)
				continue
			}
			res.FTSOptimized = true
			res.FTSOptimizeMS = dur.Milliseconds()
			if !*vacuum && res.Action == "kept" {
				res.Action = "fts_optimized"
			}
		}
		if *vacuum {
			if err := s.Vacuum(ctx); err != nil {
				_ = s.Close()
				res.Action = "skipped"
				res.Error = err.Error()
				results = append(results, res)
				continue
			}
			res.Vacuumed = true
			res.Action = "vacuumed"
		}
		_ = s.Close()
		sizeAfter := fileSize(dbPath)
		res.SizeAfter = sizeAfter
		if sizeBefore > sizeAfter {
			res.ReclaimedBytes = sizeBefore - sizeAfter
			reclaimed += res.ReclaimedBytes
		}
		results = append(results, res)
	}

	report["dbs"] = results
	report["reclaimed_bytes"] = reclaimed
	return writeJSON(stdout, report)
}

func fileSize(path string) int64 {
	info, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return info.Size()
}

type App struct {
	Store   *store.Store
	Indexer *indexer.Indexer
	Query   *query.Service
}

func (a *App) Close() error {
	return a.Store.Close()
}

func openApp(ctx context.Context, cfg config.Config, repoRoot string) (*App, graphRepo, int64, error) {
	canonical, err := store.CanonicalRepoPath(repoRoot)
	if err != nil {
		return nil, graphRepo{}, 0, err
	}
	dbPath, err := dbPathForRepo(cfg, repoRoot, canonical)
	if err != nil {
		return nil, graphRepo{}, 0, err
	}
	s, err := store.OpenWithOptions(dbPath, store.OpenOptions{
		PerformanceProfile: cfg.DBPerformanceProfile,
	})
	if err != nil {
		return nil, graphRepo{}, 0, err
	}
	registry := newDefaultRegistry()
	repoCfg, _ := config.LoadRepo(canonical)
	embedder := newEmbedder(repoCfg.Embedding)
	idx := indexer.New(s, registry, embedder)
	repo, err := s.UpsertRepo(ctx, canonical)
	if err != nil {
		_ = s.Close()
		return nil, graphRepo{}, 0, err
	}
	app := &App{
		Store:   s,
		Indexer: idx,
		Query:   query.New(s, embedder),
	}
	return app, graphRepo{ID: repo.ID, RootPath: repo.RootPath}, repo.ID, nil
}

func newEmbedder(cfg config.EmbeddingConfig) embedding.Embedder {
	if !cfg.Enabled {
		return nil
	}
	return embedding.NewOllama(embedding.OllamaConfig{
		BaseURL:    cfg.BaseURL,
		Model:      cfg.Model,
		Dimensions: cfg.Dimensions,
	})
}

const repoDBFileName = "codegraph.sqlite"

func dbPathForRepo(cfg config.Config, repoRoot, canonical string) (string, error) {
	if config.IsRepoDBDir(cfg.DBDir) {
		absRoot, err := filepath.Abs(repoRoot)
		if err != nil {
			return "", err
		}
		return filepath.Join(absRoot, repoDBFileName), nil
	}
	return filepath.Join(cfg.DBDir, store.DBFileNameForRepo(canonical)), nil
}

type graphRepo struct {
	ID       int64
	RootPath string
}

func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func writeJSONL(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	return enc.Encode(v)
}

func runAffectedTests(ctx context.Context, cfg config.Config, stdout io.Writer, cmdName string, args []string) error {
	fs := flag.NewFlagSet(cmdName, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repoRootFlag := fs.String("repo-root", "", "repository root")
	stdinFlag := fs.Bool("stdin", false, "read file paths from stdin (one per line)")
	jsonFlag := fs.Bool("json", false, "output as JSON")
	limit := fs.Int("limit", 50, "maximum number of test results")
	if err := fs.Parse(args); err != nil {
		return err
	}

	repoRoot := "."
	if *repoRootFlag != "" {
		repoRoot = *repoRootFlag
	}

	var files []string
	if *stdinFlag {
		scanner := bufio.NewScanner(os.Stdin)
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if line != "" {
				files = append(files, line)
			}
		}
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("reading stdin: %w", err)
		}
	}
	files = append(files, fs.Args()...)

	if len(files) == 0 {
		return fmt.Errorf("usage: %s %s [--repo-root PATH] [--stdin] [--json] [--limit N] <file>...", appname.BinaryName, cmdName)
	}

	app, _, repoID, err := openApp(ctx, cfg, repoRoot)
	if err != nil {
		return err
	}
	defer app.Close()

	tests, err := app.Query.RelatedTestsForFiles(ctx, repoID, files, *limit, 0)
	if err != nil {
		return err
	}

	if *jsonFlag {
		if tests == nil {
			tests = []store.RelatedTest{}
		}
		return writeJSON(stdout, map[string]any{
			"affected_tests": tests,
			"count":          len(tests),
		})
	}

	seen := map[string]bool{}
	for _, t := range tests {
		if !seen[t.File] {
			seen[t.File] = true
			fmt.Fprintln(stdout, t.File)
		}
	}
	return nil
}

func printUsage(w io.Writer) { printRootHelp(w) }

func printRootHelp(w io.Writer) {
	fmt.Fprintf(w, "%s - local-first code context engine and MCP server\n\n", appname.BinaryName)
	fmt.Fprintln(w, "Usage:")
	fmt.Fprintf(w, "  %s <command> [args]\n", appname.BinaryName)
	fmt.Fprintf(w, "  %s --help\n", appname.BinaryName)
	fmt.Fprintf(w, "  %s help\n\n", appname.BinaryName)

	fmt.Fprintln(w, "Commands:")
	for _, cmd := range commands() {
		// Use the first usage line as the synopsis so help stays stable even if
		// additional notes exist below it (jsonl hints, etc.).
		synopsis := cmd.name
		if len(cmd.usageLines) > 0 {
			synopsis = strings.TrimSpace(cmd.usageLines[0])
		}
		if len(cmd.aliases) > 0 {
			synopsis = fmt.Sprintf("%s (aliases: %s)", synopsis, strings.Join(cmd.aliases, ", "))
		}
		if cmd.description != "" {
			fmt.Fprintf(w, "  %s  - %s\n", synopsis, cmd.description)
		} else {
			fmt.Fprintf(w, "  %s\n", synopsis)
		}
	}

	fmt.Fprintln(w, "\nExamples:")
	fmt.Fprintf(w, "  %s help index\n", appname.BinaryName)
	fmt.Fprintf(w, "  %s index .\n", appname.BinaryName)
	fmt.Fprintf(w, "  %s stats .\n", appname.BinaryName)
	fmt.Fprintf(w, "  %s find_symbol . MySymbol\n", appname.BinaryName)
	fmt.Fprintf(w, "  %s serve --repo-root .\n", appname.BinaryName)
}

func formatCommandExample(ex string) string {
	// Keep examples stable in source while still respecting renames of the binary.
	if ex == "codegraph" {
		return appname.BinaryName
	}
	if strings.HasPrefix(ex, "codegraph ") {
		return appname.BinaryName + ex[len("codegraph"):]
	}
	return ex
}

func formatCommandUsageLine(line, canonicalName, displayName string) string {
	if canonicalName == "" || displayName == "" || canonicalName == displayName {
		return line
	}
	// Only rewrite the top-level command token to match the invoked name.
	prefix := "  " + canonicalName
	if line == prefix || strings.HasPrefix(line, prefix+" ") {
		return "  " + displayName + line[len(prefix):]
	}
	return line
}

func printCommandHelp(w io.Writer, cmd *command, invokedName string) {
	displayName := strings.TrimSpace(invokedName)
	if displayName == "" {
		displayName = cmd.name
	}
	fmt.Fprintf(w, "%s %s\n", appname.BinaryName, displayName)
	if cmd.description != "" {
		fmt.Fprintf(w, "%s\n", cmd.description)
	}
	aliases := make([]string, 0, len(cmd.aliases)+1)
	if cmd.name != displayName {
		aliases = append(aliases, cmd.name)
	}
	for _, a := range cmd.aliases {
		if a != "" && a != displayName {
			aliases = append(aliases, a)
		}
	}
	if len(aliases) > 0 {
		fmt.Fprintf(w, "Aliases: %s\n", strings.Join(aliases, ", "))
	}

	fmt.Fprintln(w, "\nUsage:")
	if len(cmd.usageLines) > 0 {
		for _, line := range cmd.usageLines {
			fmt.Fprintln(w, formatCommandUsageLine(line, cmd.name, displayName))
		}
	} else {
		fmt.Fprintf(w, "  %s %s\n", appname.BinaryName, displayName)
	}

	if len(cmd.flags) > 0 {
		fmt.Fprintln(w, "\nFlags:")
		for _, f := range cmd.flags {
			if f.description != "" {
				fmt.Fprintf(w, "  %s  - %s\n", f.name, f.description)
			} else {
				fmt.Fprintf(w, "  %s\n", f.name)
			}
		}
	}

	if len(cmd.examples) > 0 {
		fmt.Fprintln(w, "\nExamples:")
		for _, ex := range cmd.examples {
			fmt.Fprintf(w, "  %s\n", formatCommandExample(ex))
		}
	}
}
