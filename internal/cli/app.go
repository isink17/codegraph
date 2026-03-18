package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/isink17/codegraph/internal/appname"
	"github.com/isink17/codegraph/internal/config"
	"github.com/isink17/codegraph/internal/doctor"
	"github.com/isink17/codegraph/internal/export"
	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/logging"
	"github.com/isink17/codegraph/internal/mcp"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/platform"
	"github.com/isink17/codegraph/internal/query"
	"github.com/isink17/codegraph/internal/store"
	"github.com/isink17/codegraph/internal/watcher"
)

func Run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		printUsage(stdout)
		return nil
	}

	globalCfg, err := config.Load()
	if err != nil {
		return err
	}
	logger := logging.New(globalCfg.DefaultLogLevel, stderr)
	_ = logger

	switch args[0] {
	case "install":
		return runInstall(stdout)
	case "index":
		return runIndex(ctx, globalCfg, stdout, args[1:], false)
	case "update":
		return runIndex(ctx, globalCfg, stdout, args[1:], true)
	case "stats":
		return runStats(ctx, globalCfg, stdout, args[1:])
	case "doctor":
		report, err := doctor.Run()
		if err != nil {
			return err
		}
		return writeJSON(stdout, report)
	case "serve":
		return runServe(ctx, globalCfg, stdout, stderr, args[1:])
	case "watch":
		return runWatch(ctx, globalCfg, stdout, args[1:])
	case "graph":
		return runGraph(ctx, globalCfg, stdout, args[1:])
	default:
		return fmt.Errorf("unknown command %q", args[0])
	}
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
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Codex MCP snippet:")
	fmt.Fprintln(stdout, `{"mcpServers":{"codegraph":{"command":"codegraph","args":["serve","--repo-root","/absolute/path/to/repo"]}}}`)
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Gemini CLI MCP snippet:")
	fmt.Fprintln(stdout, `{"mcpServers":{"codegraph":{"command":"codegraph","args":["serve","--repo-root","/absolute/path/to/repo"]}}}`)
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "Claude/Desktop MCP snippet:")
	fmt.Fprintln(stdout, `{"mcpServers":{"codegraph":{"command":"codegraph","args":["serve","--repo-root","/absolute/path/to/repo"]}}}`)
	return nil
}

func runIndex(ctx context.Context, cfg config.Config, stdout io.Writer, args []string, update bool) error {
	repoRoot := "."
	for _, arg := range args {
		if !strings.HasPrefix(arg, "-") {
			repoRoot = arg
			break
		}
	}
	app, repo, repoID, err := openApp(ctx, cfg, repoRoot)
	if err != nil {
		return err
	}
	defer app.Close()
	_ = repo
	opts := indexer.Options{RepoRoot: repo.RootPath}
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
	return writeJSON(stdout, map[string]any{
		"summary": summary,
		"stats":   stats,
	})
}

func runStats(ctx context.Context, cfg config.Config, stdout io.Writer, args []string) error {
	repoRoot := "."
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		repoRoot = args[0]
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

func runServe(ctx context.Context, cfg config.Config, stdout, stderr io.Writer, args []string) error {
	repoRoot := "."
	for i := 0; i < len(args); i++ {
		if args[i] == "--repo-root" && i+1 < len(args) {
			repoRoot = args[i+1]
			i++
			continue
		}
		if !strings.HasPrefix(args[i], "-") {
			repoRoot = args[i]
		}
	}
	app, repo, repoID, err := openApp(ctx, cfg, repoRoot)
	if err != nil {
		return err
	}
	defer app.Close()
	server := mcp.NewServer(repo.RootPath, repoID, app.Store, app.Indexer, app.Query)
	return server.Serve(ctx, os.Stdin, stdout, stderr)
}

func runWatch(ctx context.Context, cfg config.Config, stdout io.Writer, args []string) error {
	repoRoot := "."
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		repoRoot = args[0]
	}
	app, repo, repoID, err := openApp(ctx, cfg, repoRoot)
	if err != nil {
		return err
	}
	defer app.Close()
	fmt.Fprintf(stdout, "watching %s\n", repo.RootPath)
	w := watcher.New(app.Store, app.Indexer)
	return w.Run(ctx, repo.RootPath, repoID, cfg.WatchDebounce)
}

func runGraph(ctx context.Context, cfg config.Config, stdout io.Writer, args []string) error {
	if len(args) == 0 || args[0] != "export" {
		return errors.New("usage: codegraph graph export <repo-path> [--format json|dot] [--symbol name]")
	}
	repoRoot := "."
	format := "json"
	symbol := ""
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--format":
			if i+1 < len(args) {
				format = args[i+1]
				i++
			}
		case "--symbol", "--focus-symbol":
			if i+1 < len(args) {
				symbol = args[i+1]
				i++
			}
		default:
			if !strings.HasPrefix(args[i], "-") {
				repoRoot = args[i]
			}
		}
	}
	app, _, repoID, err := openApp(ctx, cfg, repoRoot)
	if err != nil {
		return err
	}
	defer app.Close()
	exp := export.New(app.Query)
	if format == "dot" {
		out, err := exp.DOT(ctx, repoID, symbol, 2)
		if err != nil {
			return err
		}
		_, err = stdout.Write(out)
		return err
	}
	out, err := exp.JSON(ctx, repoID)
	if err != nil {
		return err
	}
	_, err = stdout.Write(out)
	return err
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
	dbPath := filepath.Join(cfg.DBDir, store.DBFileNameForRepo(canonical))
	s, err := store.Open(dbPath)
	if err != nil {
		return nil, graphRepo{}, 0, err
	}
	registry := parser.NewRegistry(goparser.New())
	idx := indexer.New(s, registry)
	repo, err := s.UpsertRepo(ctx, canonical)
	if err != nil {
		_ = s.Close()
		return nil, graphRepo{}, 0, err
	}
	app := &App{
		Store:   s,
		Indexer: idx,
		Query:   query.New(s),
	}
	return app, graphRepo{ID: repo.ID, RootPath: repo.RootPath}, repo.ID, nil
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

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "codegraph commands:")
	fmt.Fprintln(w, "  install")
	fmt.Fprintln(w, "  index <repo-path>")
	fmt.Fprintln(w, "  update <repo-path>")
	fmt.Fprintln(w, "  serve --repo-root <repo-path>")
	fmt.Fprintln(w, "  stats <repo-path>")
	fmt.Fprintln(w, "  doctor")
	fmt.Fprintln(w, "  graph export <repo-path> [--format json|dot]")
	fmt.Fprintln(w, "  watch <repo-path>")
}
