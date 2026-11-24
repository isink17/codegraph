// Command shardsuite is an opt-in local runner that splits the slowest Go test
// packages across separate `go test` processes so the full suite finishes
// sooner on a many-core machine. It is a developer tool, not part of the
// codegraph binary. scripts/go-suite/run.sh stays the authoritative full-suite
// run; see docs/go-suite.md.
//
//	go run ./tools/shardsuite [flags] [packages]        (default ./...)
//	go run ./tools/shardsuite parity BASE.jsonl OUT_DIR
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"
)

const exitInterrupted = 130

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	code := run(ctx, os.Args[1:], os.Stdout, os.Stderr)
	stop()
	os.Exit(code)
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) > 0 && args[0] == "parity" {
		return runParity(args[1:], stdout, stderr)
	}
	fs := flag.NewFlagSet("shardsuite", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var cfg config
	shards := fs.String("shard", "./internal/store=3,./internal/indexer=3,./internal/diff=2",
		"comma-separated PACKAGE=K: split PACKAGE into K processes by top-level test")
	fs.StringVar(&cfg.out, "out", "", "output directory (default: a new temp dir)")
	fs.StringVar(&cfg.timings, "timings", "", "events.jsonl of an earlier run, used to balance shards by test time")
	fs.StringVar(&cfg.baseline, "baseline", "", "events.jsonl of an unsharded run (scripts/go-suite/run.sh) to compare test identities against")
	fs.DurationVar(&cfg.timeout, "timeout", 20*time.Minute, "go test -timeout for every test binary")
	fs.DurationVar(&cfg.jobTimeout, "job-timeout", 0, "wall-clock limit per process before it is killed (default: -timeout + 5m)")
	fs.StringVar(&cfg.dir, "C", ".", "module directory to run in")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	var err error
	if cfg.shards, err = parseShardSpec(*shards); err != nil {
		fmt.Fprintln(stderr, "shardsuite:", err)
		return 2
	}
	if cfg.jobTimeout == 0 {
		cfg.jobTimeout = cfg.timeout + 5*time.Minute
	}
	cfg.patterns = fs.Args()
	if len(cfg.patterns) == 0 {
		cfg.patterns = []string{"./..."}
	}
	sum, err := runSuite(ctx, cfg, stdout)
	if err != nil {
		fmt.Fprintln(stderr, "shardsuite:", err)
		if errors.Is(err, context.Canceled) {
			return exitInterrupted
		}
		return 1
	}
	switch {
	case sum.Interrupted:
		return exitInterrupted
	case sum.AllPassed:
		return 0
	}
	return 1
}

func runParity(args []string, stdout, stderr io.Writer) int {
	if len(args) != 2 {
		fmt.Fprintln(stderr, "usage: shardsuite parity BASE_EVENTS.jsonl SHARDED_OUT_DIR")
		return 2
	}
	files, err := shardEventFiles(args[1])
	if err != nil {
		fmt.Fprintln(stderr, "shardsuite:", err)
		return 2
	}
	p, err := compareBaseline(args[0], files)
	if err != nil {
		fmt.Fprintln(stderr, "shardsuite:", err)
		return 2
	}
	printParity(stdout, p)
	if !p.OK {
		return 1
	}
	return 0
}
