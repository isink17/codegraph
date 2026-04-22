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
	run         func(context.Context, config.Config, io.Writer, io.Writer, []string) error
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
			run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, args []string) error {
				if len(args) == 0 {
					printRootHelp(stdout)
					return nil
				}
				cmd, ok := lookupCommand(args[0])
				if !ok {
					return fmt.Errorf("unknown command %q", args[0])
				}
				printCommandHelp(stdout, cmd)
				return nil
			},
		},
		{
			name:        "install",
			description: "install codegraph",
			usageLines:  []string{"  install"},
			examples: []string{
				"codegraph install",
			},
			run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, args []string) error {
				return runInstall(stdout)
			},
		},
		{
			name:        "index",
			description: "index a repository",
			usageLines:  []string{"  index <repo-path>"},
			flags: []commandFlag{
				{name: "--jsonl", description: "stream line-delimited JSON events"},
			},
			examples: []string{
				"codegraph index .",
				"codegraph index . --jsonl",
			},
			run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, args []string) error {
				return runIndex(ctx, cfg, stdout, args, false)
			},
		},
		{
			name:        "update_graph",
			aliases:     []string{"update"},
			description: "update only changed files",
			usageLines:  []string{"  update_graph <repo-path>"},
			flags: []commandFlag{
				{name: "--jsonl", description: "stream line-delimited JSON events"},
			},
			examples: []string{
				"codegraph update_graph .",
				"codegraph update_graph . --jsonl",
			},
			run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, args []string) error {
				return runIndex(ctx, cfg, stdout, args, true)
			},
		},
		{
			name:        "stats",
			description: "show index stats",
			usageLines:  []string{"  stats <repo-path>"},
			run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, args []string) error {
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
			},
			examples: []string{
				"codegraph find_symbol . HelloWorld",
				"codegraph find_symbol . HelloWorld --exact",
			},
			run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, args []string) error {
				return runQueryCommand(ctx, cfg, stdout, "find-symbol", "find_symbol", args)
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
			},
			examples: []string{
				"codegraph find_callers . HelloWorld",
				"codegraph find_callers . --symbol HelloWorld",
			},
			run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, args []string) error {
				return runQueryCommand(ctx, cfg, stdout, "callers", "find_callers", args)
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
			},
			examples: []string{
				"codegraph find_callees . HelloWorld",
				"codegraph find_callees . --symbol HelloWorld",
			},
			run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, args []string) error {
				return runQueryCommand(ctx, cfg, stdout, "callees", "find_callees", args)
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
			},
			examples: []string{
				"codegraph get_impact_radius . HelloWorld",
				"codegraph get_impact_radius . --symbol HelloWorld",
				"codegraph get_impact_radius . --symbol HelloWorld --symbol OtherFunc",
				"codegraph get_impact_radius . --file main.go",
				"codegraph get_impact_radius . --file main.go --depth 3",
			},
			run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, args []string) error {
				return runQueryCommand(ctx, cfg, stdout, "impact", "get_impact_radius", args)
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
			},
			examples: []string{
				"codegraph search_symbols . HelloWorld",
				"codegraph search_symbols . \"http handler\"",
			},
			run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, args []string) error {
				return runQueryCommand(ctx, cfg, stdout, "search", "search_symbols", args)
			},
		},
		{
			name:        "doctor",
			description: "run diagnostics",
			usageLines: []string{
				"  doctor",
				"    add --fix for non-destructive autofixes",
			},
			run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, args []string) error {
				return runDoctor(stdout, args)
			},
		},
		{
			name:        "config",
			description: "config commands",
			usageLines: []string{
				"  config <show|edit-path|validate|init>",
				"    config init [--repo PATH] [--force]",
			},
			run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, args []string) error {
				return runConfig(cfg, stdout, args)
			},
		},
		{
			name:        "benchmark",
			description: "benchmarks",
			usageLines:  []string{"  benchmark [--count N] [--benchtime DURATION] [--save-baseline]"},
			run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, args []string) error {
				return runBenchmark(ctx, stdout, args)
			},
		},
		{
			name:        "serve",
			description: "start MCP server",
			usageLines:  []string{"  serve --repo-root <repo-path>"},
			run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, args []string) error {
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
			run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, args []string) error {
				return runWatch(ctx, cfg, stdout, args)
			},
		},
		{
			name:        "graph",
			description: "graph commands",
			usageLines:  []string{"  graph export <repo-path> [--format json|dot]"},
			run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, args []string) error {
				return runGraph(ctx, cfg, stdout, args)
			},
		},
		{
			name:        "clean",
			description: "clean index data",
			usageLines:  []string{"  clean [repo-path] [--vacuum]"},
			flags: []commandFlag{
				{name: "--vacuum", description: "VACUUM the database after cleanup"},
			},
			examples: []string{
				"codegraph clean .",
				"codegraph clean . --vacuum",
			},
			run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, args []string) error {
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
				"codegraph find_related_tests --repo-root . main.go",
				"git diff --name-only | codegraph find_related_tests --stdin --repo-root .",
			},
			run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, args []string) error {
				return runAffectedTests(ctx, cfg, stdout, "find_related_tests", args)
			},
		},
		{
			name:        "visualize",
			description: "generate interactive graph HTML",
			usageLines: []string{
				"  visualize [--repo-root PATH] [--symbol NAME] [--depth N] [--output FILE]",
				"    interactive D3.js graph visualization; opens browser or writes HTML file",
			},
			run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, args []string) error {
				return runVisualize(ctx, cfg, stdout, args)
			},
		},
	}
}
