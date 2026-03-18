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
	"github.com/isink17/codegraph/internal/gotool"
	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/logging"
	"github.com/isink17/codegraph/internal/mcp"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	heuristicparser "github.com/isink17/codegraph/internal/parser/heuristic"
	pyparser "github.com/isink17/codegraph/internal/parser/python"
	"github.com/isink17/codegraph/internal/platform"
	"github.com/isink17/codegraph/internal/query"
	"github.com/isink17/codegraph/internal/store"
	"github.com/isink17/codegraph/internal/versioncheck"
	"github.com/isink17/codegraph/internal/watcher"
)

var startupVersionCheck = versioncheck.NotifyIfOutdated

func Run(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	startupVersionCheck(ctx, stderr)

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
	case "find-symbol":
		return runQueryCommand(ctx, globalCfg, stdout, "find-symbol", args[1:])
	case "callers":
		return runQueryCommand(ctx, globalCfg, stdout, "callers", args[1:])
	case "callees":
		return runQueryCommand(ctx, globalCfg, stdout, "callees", args[1:])
	case "impact":
		return runQueryCommand(ctx, globalCfg, stdout, "impact", args[1:])
	case "search":
		return runQueryCommand(ctx, globalCfg, stdout, "search", args[1:])
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
	case "clean":
		return runClean(ctx, globalCfg, stdout, args[1:])
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
	fmt.Fprintln(stdout)
	fmt.Fprintln(stdout, "If `codegraph` is not found after `go install`, your Go bin directory is probably not on PATH.")
	fmt.Fprintln(stdout, "Check expected Go bin locations:")
	for _, dir := range gotool.BinPathHints() {
		fmt.Fprintf(stdout, "  - %s\n", dir)
	}
	fmt.Fprintf(stdout, "Then reopen your shell and verify: %s\n", gotool.VerifyCommandHint(appname.BinaryName))
	return nil
}

func runIndex(ctx context.Context, cfg config.Config, stdout io.Writer, args []string, update bool) error {
	repoRoot := "."
	jsonl := false
	for _, arg := range args {
		if arg == "--jsonl" {
			jsonl = true
			continue
		}
		if !strings.HasPrefix(arg, "-") {
			repoRoot = arg
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
	if jsonl {
		if err := writeJSONL(stdout, map[string]any{
			"type":    "scan_summary",
			"command": map[bool]string{true: "update", false: "index"}[update],
			"data":    summary,
		}); err != nil {
			return err
		}
		return writeJSONL(stdout, map[string]any{
			"type": "scan_stats",
			"data": stats,
		})
	}
	return writeJSON(stdout, map[string]any{"summary": summary, "stats": stats})
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

func runQueryCommand(ctx context.Context, cfg config.Config, stdout io.Writer, command string, args []string) error {
	repoRoot := "."
	queryValue := ""
	symbol := ""
	var symbols []string
	var files []string
	depth := 2
	limit := 20
	offset := 0

	for i := 0; i < len(args); i++ {
		arg := args[i]
		switch arg {
		case "--repo-root":
			if i+1 < len(args) {
				repoRoot = args[i+1]
				i++
			}
		case "--query":
			if i+1 < len(args) {
				queryValue = args[i+1]
				i++
			}
		case "--symbol":
			if i+1 < len(args) {
				symbol = args[i+1]
				symbols = append(symbols, args[i+1])
				i++
			}
		case "--file":
			if i+1 < len(args) {
				files = append(files, args[i+1])
				i++
			}
		case "--depth":
			if i+1 < len(args) {
				fmt.Sscanf(args[i+1], "%d", &depth)
				i++
			}
		case "--limit":
			if i+1 < len(args) {
				fmt.Sscanf(args[i+1], "%d", &limit)
				i++
			}
		case "--offset":
			if i+1 < len(args) {
				fmt.Sscanf(args[i+1], "%d", &offset)
				i++
			}
		default:
			if strings.HasPrefix(arg, "-") {
				continue
			}
			if repoRoot == "." {
				repoRoot = arg
				continue
			}
			if queryValue == "" {
				queryValue = arg
			}
		}
	}
	if symbol == "" && len(symbols) > 0 {
		symbol = symbols[0]
	}
	if queryValue == "" {
		queryValue = symbol
	}

	app, _, repoID, err := openApp(ctx, cfg, repoRoot)
	if err != nil {
		return err
	}
	defer app.Close()

	switch command {
	case "find-symbol":
		if queryValue == "" {
			return errors.New("usage: codegraph find-symbol <repo-path> <query> [--limit N] [--offset N]")
		}
		items, err := app.Query.FindSymbol(ctx, repoID, queryValue, limit, offset)
		if err != nil {
			return err
		}
		return writeJSON(stdout, map[string]any{"matches": items})
	case "search":
		if queryValue == "" {
			return errors.New("usage: codegraph search <repo-path> <query> [--limit N] [--offset N]")
		}
		items, err := app.Query.SearchSymbols(ctx, repoID, queryValue, limit, offset)
		if err != nil {
			return err
		}
		return writeJSON(stdout, map[string]any{"matches": items})
	case "callers":
		if symbol == "" {
			return errors.New("usage: codegraph callers <repo-path> --symbol <name> [--limit N] [--offset N]")
		}
		items, err := app.Query.FindCallers(ctx, repoID, symbol, 0, limit, offset)
		if err != nil {
			return err
		}
		return writeJSON(stdout, map[string]any{"callers": items})
	case "callees":
		if symbol == "" {
			return errors.New("usage: codegraph callees <repo-path> --symbol <name> [--limit N] [--offset N]")
		}
		items, err := app.Query.FindCallees(ctx, repoID, symbol, 0, limit, offset)
		if err != nil {
			return err
		}
		return writeJSON(stdout, map[string]any{"callees": items})
	case "impact":
		if len(symbols) == 0 && len(files) == 0 {
			if symbol != "" {
				symbols = append(symbols, symbol)
			} else {
				return errors.New("usage: codegraph impact <repo-path> [--symbol <name>]... [--file <path>]... [--depth N]")
			}
		}
		data, err := app.Query.ImpactRadius(ctx, repoID, symbols, files, depth)
		if err != nil {
			return err
		}
		return writeJSON(stdout, data)
	default:
		return fmt.Errorf("unknown query command %q", command)
	}
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
	jsonl := false
	for _, arg := range args {
		if arg == "--jsonl" {
			jsonl = true
			continue
		}
		if !strings.HasPrefix(arg, "-") {
			repoRoot = arg
		}
	}
	app, repo, repoID, err := openApp(ctx, cfg, repoRoot)
	if err != nil {
		return err
	}
	defer app.Close()
	if jsonl {
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
	err = w.Run(ctx, repo.RootPath, repoID, cfg.WatchDebounce)
	if jsonl {
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

func runClean(ctx context.Context, cfg config.Config, stdout io.Writer, args []string) error {
	repoRoot := ""
	vacuum := false
	for _, arg := range args {
		switch arg {
		case "--vacuum":
			vacuum = true
		default:
			if !strings.HasPrefix(arg, "-") {
				repoRoot = arg
			}
		}
	}

	type dbResult struct {
		Path           string `json:"path"`
		Action         string `json:"action"`
		ReclaimedBytes int64  `json:"reclaimed_bytes"`
		CanonicalRepo  string `json:"canonical_repo,omitempty"`
		Error          string `json:"error,omitempty"`
	}
	report := map[string]any{
		"vacuum": vacuum,
		"dbs":    []dbResult{},
	}
	var results []dbResult
	var reclaimed int64

	if repoRoot != "" {
		canonical, err := store.CanonicalRepoPath(repoRoot)
		if err != nil {
			return err
		}
		dbPath := filepath.Join(cfg.DBDir, store.DBFileNameForRepo(canonical))
		res := dbResult{Path: dbPath, CanonicalRepo: canonical}
		before := fileSize(dbPath)
		s, err := store.OpenWithOptions(dbPath, store.OpenOptions{PerformanceProfile: cfg.DBPerformanceProfile})
		if err != nil {
			return err
		}
		defer s.Close()
		if vacuum {
			if err := s.Vacuum(ctx); err != nil {
				return err
			}
			after := fileSize(dbPath)
			if before > after {
				res.ReclaimedBytes = before - after
				reclaimed += res.ReclaimedBytes
			}
			res.Action = "vacuumed"
		} else {
			res.Action = "inspected"
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
		res := dbResult{Path: dbPath}
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
		if vacuum {
			if err := s.Vacuum(ctx); err != nil {
				_ = s.Close()
				res.Action = "skipped"
				res.Error = err.Error()
				results = append(results, res)
				continue
			}
			after := fileSize(dbPath)
			if sizeBefore > after {
				res.ReclaimedBytes = sizeBefore - after
				reclaimed += res.ReclaimedBytes
			}
			res.Action = "vacuumed"
		} else {
			res.Action = "kept"
		}
		_ = s.Close()
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
	dbPath := filepath.Join(cfg.DBDir, store.DBFileNameForRepo(canonical))
	s, err := store.OpenWithOptions(dbPath, store.OpenOptions{
		PerformanceProfile: cfg.DBPerformanceProfile,
	})
	if err != nil {
		return nil, graphRepo{}, 0, err
	}
	registry := parser.NewRegistry(
		goparser.New(),
		pyparser.New(),
		heuristicparser.NewJava(),
		heuristicparser.NewKotlin(),
		heuristicparser.NewCSharp(),
		heuristicparser.NewTypeScriptJavaScript(),
		heuristicparser.NewRust(),
		heuristicparser.NewRuby(),
		heuristicparser.NewSwift(),
		heuristicparser.NewPHP(),
		heuristicparser.NewCAndCpp(),
	)
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

func writeJSONL(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	return enc.Encode(v)
}

func printUsage(w io.Writer) {
	fmt.Fprintln(w, "codegraph commands:")
	fmt.Fprintln(w, "  install")
	fmt.Fprintln(w, "  index <repo-path>")
	fmt.Fprintln(w, "  update <repo-path>")
	fmt.Fprintln(w, "    add --jsonl for streaming line-delimited JSON events")
	fmt.Fprintln(w, "  serve --repo-root <repo-path>")
	fmt.Fprintln(w, "  stats <repo-path>")
	fmt.Fprintln(w, "  find-symbol <repo-path> <query>")
	fmt.Fprintln(w, "  search <repo-path> <query>")
	fmt.Fprintln(w, "  callers <repo-path> --symbol <name>")
	fmt.Fprintln(w, "  callees <repo-path> --symbol <name>")
	fmt.Fprintln(w, "  impact <repo-path> [--symbol <name>] [--file <path>]")
	fmt.Fprintln(w, "  doctor")
	fmt.Fprintln(w, "  graph export <repo-path> [--format json|dot]")
	fmt.Fprintln(w, "  watch <repo-path>")
	fmt.Fprintln(w, "    add --jsonl for streaming line-delimited JSON events")
	fmt.Fprintln(w, "  clean [repo-path] [--vacuum]")
}
