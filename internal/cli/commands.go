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
			name:        "update",
			description: "incrementally update an index",
			usageLines: []string{
				"  update <repo-path>",
				"    add --jsonl for streaming line-delimited JSON events",
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
			name:        "find-symbol",
			description: "find symbols by name",
			usageLines:  []string{"  find-symbol <repo-path> <query>"},
			flags: []commandFlag{
				{name: "--limit", description: "limit results"},
				{name: "--offset", description: "offset into result set"},
			},
			examples: []string{
				"codegraph find-symbol . HelloWorld",
			},
			run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, args []string) error {
				return runQueryCommand(ctx, cfg, stdout, "find-symbol", args)
			},
		},
		{
			name:        "callers",
			description: "find callers of a symbol",
			usageLines:  []string{"  callers <repo-path> --symbol <name>"},
			flags: []commandFlag{
				{name: "--symbol", description: "symbol name to query (required)"},
				{name: "--limit", description: "limit results"},
				{name: "--offset", description: "offset into result set"},
			},
			examples: []string{
				"codegraph callers . --symbol HelloWorld",
			},
			run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, args []string) error {
				return runQueryCommand(ctx, cfg, stdout, "callers", args)
			},
		},
		{
			name:        "callees",
			description: "find callees of a symbol",
			usageLines:  []string{"  callees <repo-path> --symbol <name>"},
			run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, args []string) error {
				return runQueryCommand(ctx, cfg, stdout, "callees", args)
			},
		},
		{
			name:        "impact",
			description: "compute impact radius",
			usageLines:  []string{"  impact <repo-path> [--symbol <name>] [--file <path>]"},
			run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, args []string) error {
				return runQueryCommand(ctx, cfg, stdout, "impact", args)
			},
		},
		{
			name:        "search",
			description: "search",
			usageLines:  []string{"  search <repo-path> <query>"},
			run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, args []string) error {
				return runQueryCommand(ctx, cfg, stdout, "search", args)
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
			name:        "affected-tests",
			description: "find affected tests",
			usageLines: []string{
				"  affected-tests [--repo-root PATH] [--stdin] [--json] [--limit N] <file>...",
				"    find tests affected by changed files; pipe from git diff --name-only",
			},
			run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, args []string) error {
				return runAffectedTests(ctx, cfg, stdout, args)
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
