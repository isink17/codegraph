package cli

import (
	"context"
	"fmt"
	"io"
	"sync"

	"github.com/isink17/codegraph/internal/config"
)

type command struct {
	name        string
	aliases     []string
	description string
	usageLines  []string
	flags       []commandFlag
	examples    []string
	// invokedName is the actual token the user typed (could be an alias).
	run func(context.Context, config.Config, io.Writer, io.Writer, string, []string) error
}

type commandFlag struct {
	name        string
	description string
}

var (
	commandInitOnce sync.Once
	commandList     []*command
	commandByName   map[string]*command
)

func lookupCommand(name string) (*command, bool) {
	ensureCommandsInit()
	c, ok := commandByName[name]
	return c, ok
}

func commands() []*command {
	ensureCommandsInit()
	return commandList
}

func ensureCommandsInit() {
	commandInitOnce.Do(func() {
		commandList = newCommandList()
		commandByName = newCommandRegistry(commandList)
	})
}

func newCommandRegistry(cmds []*command) map[string]*command {
	reg := map[string]*command{}
	for _, c := range cmds {
		registerCommand(reg, c)
	}
	return reg
}

func registerCommand(reg map[string]*command, c *command) {
	if c == nil {
		panic("command registry: nil command")
	}
	if c.name == "" {
		panic("command registry: empty command name")
	}
	registerKey := func(key string) {
		if key == "" {
			panic(fmt.Sprintf("command registry: empty key for command %q", c.name))
		}
		if prev, exists := reg[key]; exists {
			panic(fmt.Sprintf("command registry: duplicate key %q for command %q (already used by %q)", key, c.name, prev.name))
		}
		reg[key] = c
	}

	registerKey(c.name)
	for _, a := range c.aliases {
		registerKey(a)
	}
}

func newCommandList() []*command {
	return []*command{
		{
			name:        "help",
			description: "show help",
			usageLines:  []string{"  help [command]"},
			examples: []string{
				"codegraph help",
				"codegraph help index",
			},
			run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, invokedName string, args []string) error {
				if len(args) == 0 {
					printRootHelp(stdout)
					return nil
				}
				cmd, ok := lookupCommand(args[0])
				if !ok {
					return fmt.Errorf("unknown command %q", args[0])
				}
				printCommandHelp(stdout, cmd, args[0])
				return nil
			},
		},
		{
			name:        "install",
			description: "install codegraph",
			usageLines:  []string{"  install [--tool-mode full|gateway]"},
			flags: []commandFlag{
				{name: "--tool-mode MODE", description: "MCP tool surface to configure: full (default) or gateway"},
			},
			examples: []string{
				"codegraph install",
				"codegraph install --tool-mode gateway",
			},
			run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, invokedName string, args []string) error {
				return runInstall(stdout, args)
			},
		},
		{
			name:        "version",
			description: "print current version",
			usageLines:  []string{"  version"},
			examples:    []string{"codegraph version"},
			run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, invokedName string, args []string) error {
				return runVersion(stdout)
			},
		},
		{
			name:        "index",
			description: "index a repository",
			usageLines:  []string{"  index <repo-path>"},
			flags: []commandFlag{
				{name: "--force", description: "re-index files even if unchanged"},
				{name: "--rebuild", description: "remove the existing repo database before indexing (implies --force)"},
				{name: "--jsonl", description: "stream line-delimited JSON events"},
			},
			examples: []string{
				"codegraph index .",
				"codegraph index . --rebuild",
				"codegraph index . --jsonl",
			},
			run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, invokedName string, args []string) error {
				return runIndex(ctx, cfg, stdout, invokedName, args, false)
			},
		},
		{
			name:        "update_graph",
			aliases:     []string{"update"},
			description: "update only changed files",
			usageLines:  []string{"  update_graph <repo-path>"},
			flags: []commandFlag{
				{name: "--force", description: "re-index files even if unchanged"},
				{name: "--jsonl", description: "stream line-delimited JSON events"},
			},
			examples: []string{
				"codegraph update_graph .",
				"codegraph update_graph . --jsonl",
			},
			run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, invokedName string, args []string) error {
				return runIndex(ctx, cfg, stdout, invokedName, args, true)
			},
		},
		{
			name:        "index_smoke",
			aliases:     []string{"smoke_index", "smoke-index"},
			description: "run repeatable index/update smoke timing",
			usageLines:  []string{"  index_smoke <repo-path>"},
			flags: []commandFlag{
				{name: "--mode", description: "scan mode: index|update (default index)"},
				{name: "--runs", description: "number of measured runs"},
				{name: "--warmup", description: "number of warmup runs (not reported)"},
				{name: "--force", description: "re-index files even if unchanged"},
				{name: "--profile", description: "SQLite performance profile: balanced|durable|fast (default from config)"},
				{name: "--gomaxprocs", description: "set GOMAXPROCS for this process (0 = leave unchanged)"},
				{name: "--baseline", description: "read baseline JSON (optional)"},
				{name: "--save-baseline", description: "write baseline JSON (default true when --baseline is set)"},
				{name: "--json", description: "pretty JSON output instead of jsonl"},
			},
			examples: []string{
				"codegraph index_smoke .",
				"codegraph index_smoke . --mode update --force",
				"codegraph index_smoke . --runs 5 --baseline .codegraph/smoke-baseline.json",
			},
			run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, invokedName string, args []string) error {
				return runIndexSmoke(ctx, cfg, stdout, invokedName, args)
			},
		},
		{
			name:        "stats",
			description: "show index stats",
			usageLines:  []string{"  stats <repo-path>"},
			examples: []string{
				"codegraph stats .",
			},
			run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, invokedName string, args []string) error {
				return runStats(ctx, cfg, stdout, args)
			},
		},
		{
			name:        "find_symbol",
			aliases:     []string{"find-symbol"},
			description: "find symbols by name (substring match by default)",
			usageLines:  []string{"  find_symbol <repo-path> <query>"},
			flags: []commandFlag{
				{name: "--exact", description: "match symbol name exactly"},
				{name: "--limit", description: "limit results"},
				{name: "--offset", description: "offset into result set"},
				{name: "--detail", description: "detail level for symbol records: card|skeleton|excerpt|full"},
			},
			examples: []string{
				"codegraph find_symbol . HelloWorld",
				"codegraph find_symbol . HelloWorld --exact",
			},
			run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, invokedName string, args []string) error {
				return runQueryCommand(ctx, cfg, stdout, "find-symbol", invokedName, args)
			},
		},
		{
			name:        "find_callers",
			aliases:     []string{"callers"},
			description: "find callers of a symbol",
			usageLines:  []string{"  find_callers <repo-path> <symbol>"},
			flags: []commandFlag{
				{name: "--symbol", description: "symbol name to query (repeatable; first wins)"},
				{name: "--limit", description: "limit results"},
				{name: "--offset", description: "offset into result set"},
				{name: "--detail", description: "detail level for symbol records: card|skeleton|excerpt|full"},
			},
			examples: []string{
				"codegraph find_callers . HelloWorld",
				"codegraph find_callers . --symbol HelloWorld",
			},
			run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, invokedName string, args []string) error {
				return runQueryCommand(ctx, cfg, stdout, "callers", invokedName, args)
			},
		},
		{
			name:        "find_callees",
			aliases:     []string{"callees"},
			description: "find callees of a symbol",
			usageLines:  []string{"  find_callees <repo-path> <symbol>"},
			flags: []commandFlag{
				{name: "--symbol", description: "symbol name to query (repeatable; first wins)"},
				{name: "--limit", description: "limit results"},
				{name: "--offset", description: "offset into result set"},
				{name: "--detail", description: "detail level for symbol records: card|skeleton|excerpt|full"},
			},
			examples: []string{
				"codegraph find_callees . HelloWorld",
				"codegraph find_callees . --symbol HelloWorld",
			},
			run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, invokedName string, args []string) error {
				return runQueryCommand(ctx, cfg, stdout, "callees", invokedName, args)
			},
		},
		{
			name:        "get_impact_radius",
			aliases:     []string{"impact"},
			description: "compute impact radius",
			usageLines: []string{
				"  get_impact_radius <repo-path> <symbol>",
				"  get_impact_radius <repo-path> [--symbol <name>]... [--file <path>]... [--depth N]",
			},
			flags: []commandFlag{
				{name: "--symbol", description: "symbol name to query (repeatable)"},
				{name: "--file", description: "file path to query (repeatable)"},
				{name: "--depth", description: "limit traversal depth"},
				{name: "--detail", description: "detail level for symbol records: card|skeleton|excerpt|full"},
			},
			examples: []string{
				"codegraph get_impact_radius . HelloWorld",
				"codegraph get_impact_radius . --symbol HelloWorld",
				"codegraph get_impact_radius . --file main.go --depth 3",
			},
			run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, invokedName string, args []string) error {
				return runQueryCommand(ctx, cfg, stdout, "impact", invokedName, args)
			},
		},
		{
			name:        "search_symbols",
			aliases:     []string{"search"},
			description: "search symbols by name/signature/docs (FTS)",
			usageLines:  []string{"  search_symbols <repo-path> <query> [--limit N] [--offset N]"},
			flags: []commandFlag{
				{name: "--limit", description: "limit results"},
				{name: "--offset", description: "offset into result set"},
				{name: "--detail", description: "detail level for symbol records: card|skeleton|excerpt|full"},
			},
			examples: []string{
				"codegraph search_symbols . HelloWorld",
				"codegraph search_symbols . \"http handler\"",
			},
			run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, invokedName string, args []string) error {
				return runQueryCommand(ctx, cfg, stdout, "search", invokedName, args)
			},
		},
		{
			name:        "doctor",
			description: "run diagnostics",
			usageLines: []string{
				"  doctor",
				"    add --repo-root PATH to inspect a repo DB",
				"    add --fix for non-destructive autofixes",
				"    add --deep for slower DB checks (integrity_check)",
			},
			run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, invokedName string, args []string) error {
				return runDoctor(ctx, cfg, stdout, args)
			},
		},
		{
			// audit is deliberately separate from doctor: doctor reports on the
			// installation and the database file, audit reports on the graph
			// inside it. Neither reads what the other reads.
			name:        "audit",
			description: "audit the indexed graph for integrity and trust issues",
			usageLines: []string{
				"  audit [PATH]",
				"    add --repo-root PATH instead of the positional argument",
				"    add --examples N to cap examples per finding (0 for counts only)",
				"    add --fail-on error|warning to exit non-zero on findings",
			},
			flags: []commandFlag{
				{name: "--repo-root PATH", description: "repository root to audit (defaults to the git repo root)"},
				{name: "--examples N", description: "maximum examples per finding (default 5, 0 for counts only)"},
				{name: "--fail-on VALUE", description: "none (default), error, or warning; affects the exit code only"},
			},
			examples: []string{
				"codegraph audit .",
				"codegraph audit . --examples 0",
				"codegraph audit . --fail-on error",
			},
			run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, invokedName string, args []string) error {
				return runAudit(ctx, cfg, stdout, args)
			},
		},
		{
			// bench_queries measures; audit judges. Keeping them separate keeps
			// the audit report free of timing noise and this report free of
			// correctness findings.
			name:        "bench_queries",
			aliases:     []string{"bench-queries"},
			description: "benchmark local graph query latency on an indexed repository",
			usageLines: []string{
				"  bench_queries [PATH]",
				"    add --repo-root PATH instead of the positional argument",
				"    add --runs N to change the measured calls per scenario",
				"    add --warmup N to change the unmeasured warmup calls",
				"    add --budget-ms MS to change the reported p95 budget",
				"    add --fail-over-budget to exit non-zero when a scenario misses it",
			},
			flags: []commandFlag{
				{name: "--repo-root PATH", description: "repository root to benchmark (defaults to the git repo root)"},
				{name: "--runs N", description: "measured calls per scenario (default 20)"},
				{name: "--warmup N", description: "unmeasured warmup calls per scenario (default 3)"},
				{name: "--budget-ms MS", description: "p95 budget per scenario in milliseconds (default 50; 0 uses the default)"},
				{name: "--fail-over-budget", description: "exit non-zero after printing the report if any scenario is over budget"},
			},
			examples: []string{
				"codegraph bench-queries .",
				"codegraph bench-queries . --runs 50",
				"codegraph bench-queries . --fail-over-budget",
			},
			run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, invokedName string, args []string) error {
				return runBenchQueries(ctx, cfg, stdout, args)
			},
		},
		{
			name:        "config",
			description: "config commands",
			usageLines: []string{
				"  config <show|edit-path|validate|init>",
				"    config init [--repo PATH] [--force]",
			},
			run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, invokedName string, args []string) error {
				return runConfig(cfg, stdout, args)
			},
		},
		{
			name:        "benchmark",
			description: "benchmarks",
			usageLines:  []string{"  benchmark [--count N] [--benchtime DURATION] [--save-baseline] [--files N] [--sqlite-profile NAME] [--gomaxprocs N]"},
			flags: []commandFlag{
				{name: "--count", description: "number of benchmark runs"},
				{name: "--benchtime", description: "benchmark time per test"},
				{name: "--save-baseline", description: "save current benchmark result as baseline"},
				{name: "--files", description: "fixture file count (sets CODEGRAPH_BENCH_FILES)"},
				{name: "--sqlite-profile", description: "SQLite performance profile (sets CODEGRAPH_BENCH_SQLITE_PROFILE)"},
				{name: "--gomaxprocs", description: "GOMAXPROCS for benchmark subprocess (0 = default)"},
			},
			run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, invokedName string, args []string) error {
				return runBenchmark(ctx, stdout, args)
			},
		},
		{
			name:        "serve",
			description: "start MCP server",
			usageLines:  []string{"  serve [--repo-root <repo-path>] [--tool-mode full|gateway] [--usage-summary]"},
			flags: []commandFlag{
				{name: "--repo-root PATH", description: "repository root to serve (defaults to the git repo root)"},
				{name: "--tool-mode MODE", description: "MCP tool surface: full (default, every tool) or gateway (small core plus tool_search/tool_call)"},
				{name: "--usage-summary", description: "on exit, print this session's local context-usage report to stderr (never to stdout, never off-machine)"},
			},
			run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, invokedName string, args []string) error {
				return runServe(ctx, cfg, stdout, stderr, args)
			},
		},
		{
			name:        "watch",
			description: "watch repository and update index",
			usageLines: []string{
				"  watch <repo-path>",
				"    add --jsonl for streaming line-delimited JSON events",
			},
			run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, invokedName string, args []string) error {
				return runWatch(ctx, cfg, stdout, args)
			},
		},
		{
			name:        "graph",
			description: "graph commands",
			usageLines:  []string{"  graph export [--format json|dot] [--symbol name] [--focus-symbol name] [--limit N] [--offset N] [--jsonl] <repo-path>"},
			flags: []commandFlag{
				{name: "--format", description: "output format (json|dot)"},
				{name: "--symbol", description: "focus symbol"},
				{name: "--focus-symbol", description: "focus symbol (alias of --symbol)"},
				{name: "--limit", description: "page size for JSON export"},
				{name: "--offset", description: "offset for JSON export"},
				{name: "--jsonl", description: "stream graph as line-delimited JSON"},
			},
			examples: []string{
				"codegraph graph export .",
				"codegraph graph export --format dot .",
				"codegraph graph export --symbol HelloWorld --limit 100 .",
			},
			run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, invokedName string, args []string) error {
				return runGraph(ctx, cfg, stdout, args)
			},
		},
		{
			name:        "clean",
			description: "database maintenance",
			usageLines:  []string{"  clean [repo-path] [--vacuum] [--fts-optimize] [--analyze] [--wal-checkpoint-truncate] [--incremental-vacuum]"},
			flags: []commandFlag{
				{name: "--vacuum", description: "VACUUM the database after cleanup"},
				{name: "--fts-optimize", description: "run FTS optimize (symbol_fts)"},
				{name: "--analyze", description: "run ANALYZE"},
				{name: "--wal-checkpoint-truncate", description: "run PRAGMA wal_checkpoint(TRUNCATE)"},
				{name: "--incremental-vacuum", description: "run PRAGMA incremental_vacuum (requires auto_vacuum=INCREMENTAL)"},
			},
			examples: []string{
				"codegraph clean .",
				"codegraph clean . --vacuum",
				"codegraph clean . --fts-optimize",
				"codegraph clean . --analyze",
				"codegraph clean . --wal-checkpoint-truncate",
			},
			run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, invokedName string, args []string) error {
				return runClean(ctx, cfg, stdout, args)
			},
		},
		{
			name:        "find_related_tests",
			aliases:     []string{"affected-tests"},
			description: "find tests related to changed files",
			usageLines: []string{
				"  find_related_tests [--repo-root PATH] [--stdin] [--json] [--limit N] <file>...",
			},
			examples: []string{
				"codegraph find_related_tests main.go",
				"git diff --name-only | codegraph find_related_tests --stdin",
			},
			run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, invokedName string, args []string) error {
				return runAffectedTests(ctx, cfg, stdout, invokedName, args)
			},
		},
		{
			name:        "visualize",
			description: "generate interactive graph HTML",
			usageLines: []string{
				"  visualize [--repo-root PATH] [--symbol NAME] [--depth N] [--output FILE]",
				"    interactive D3.js graph visualization; opens browser or writes HTML file",
			},
			run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, invokedName string, args []string) error {
				return runVisualize(ctx, cfg, stdout, args)
			},
		},
	}
}
