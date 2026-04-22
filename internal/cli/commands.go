package cli

import (
	"context"
	"io"

	"github.com/isink17/codegraph/internal/config"
)

type command struct {
	name        string
	aliases     []string
	description string
	run         func(context.Context, config.Config, io.Writer, io.Writer, []string) error
}

var commandByName = newCommandRegistry()

func lookupCommand(name string) (*command, bool) {
	c, ok := commandByName[name]
	return c, ok
}

func newCommandRegistry() map[string]*command {
	reg := map[string]*command{}

	add := func(c *command) {
		reg[c.name] = c
		for _, a := range c.aliases {
			reg[a] = c
		}
	}

	add(&command{
		name:        "install",
		description: "install codegraph",
		run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, args []string) error {
			return runInstall(stdout)
		},
	})
	add(&command{
		name:        "index",
		description: "index a repository",
		run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, args []string) error {
			return runIndex(ctx, cfg, stdout, args, false)
		},
	})
	add(&command{
		name:        "update",
		description: "incrementally update an index",
		run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, args []string) error {
			return runIndex(ctx, cfg, stdout, args, true)
		},
	})
	add(&command{
		name:        "stats",
		description: "show index stats",
		run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, args []string) error {
			return runStats(ctx, cfg, stdout, args)
		},
	})
	add(&command{
		name:        "find-symbol",
		description: "find symbols by name",
		run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, args []string) error {
			return runQueryCommand(ctx, cfg, stdout, "find-symbol", args)
		},
	})
	add(&command{
		name:        "callers",
		description: "find callers of a symbol",
		run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, args []string) error {
			return runQueryCommand(ctx, cfg, stdout, "callers", args)
		},
	})
	add(&command{
		name:        "callees",
		description: "find callees of a symbol",
		run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, args []string) error {
			return runQueryCommand(ctx, cfg, stdout, "callees", args)
		},
	})
	add(&command{
		name:        "impact",
		description: "compute impact radius",
		run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, args []string) error {
			return runQueryCommand(ctx, cfg, stdout, "impact", args)
		},
	})
	add(&command{
		name:        "search",
		description: "search",
		run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, args []string) error {
			return runQueryCommand(ctx, cfg, stdout, "search", args)
		},
	})
	add(&command{
		name:        "doctor",
		description: "run diagnostics",
		run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, args []string) error {
			return runDoctor(stdout, args)
		},
	})
	add(&command{
		name:        "config",
		description: "config commands",
		run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, args []string) error {
			return runConfig(cfg, stdout, args)
		},
	})
	add(&command{
		name:        "benchmark",
		description: "benchmarks",
		run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, args []string) error {
			return runBenchmark(ctx, stdout, args)
		},
	})
	add(&command{
		name:        "serve",
		description: "start MCP server",
		run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, args []string) error {
			return runServe(ctx, cfg, stdout, stderr, args)
		},
	})
	add(&command{
		name:        "watch",
		description: "watch repository and update index",
		run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, args []string) error {
			return runWatch(ctx, cfg, stdout, args)
		},
	})
	add(&command{
		name:        "graph",
		description: "graph commands",
		run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, args []string) error {
			return runGraph(ctx, cfg, stdout, args)
		},
	})
	add(&command{
		name:        "clean",
		description: "clean index data",
		run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, args []string) error {
			return runClean(ctx, cfg, stdout, args)
		},
	})
	add(&command{
		name:        "affected-tests",
		description: "find affected tests",
		run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, args []string) error {
			return runAffectedTests(ctx, cfg, stdout, args)
		},
	})
	add(&command{
		name:        "visualize",
		description: "generate interactive graph HTML",
		run: func(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, args []string) error {
			return runVisualize(ctx, cfg, stdout, args)
		},
	})

	return reg
}
