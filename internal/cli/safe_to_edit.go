package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"

	"github.com/isink17/codegraph/internal/config"
	"github.com/isink17/codegraph/internal/editsafety"
)

// runSafeToEdit assesses one exact symbol or file. Like check_constraints its
// exit code is a contract (see editsafety.ExitCode); invalid input, a missing
// or ambiguous target and an unindexed repository exit 2 with no result.
func runSafeToEdit(ctx context.Context, cfg config.Config, stdout io.Writer, args []string) error {
	fs := flag.NewFlagSet("safe_to_edit", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repoRootFlag := fs.String("repo-root", "", "repository root (optional)")
	symbol := fs.String("symbol", "", "exact qualified symbol name")
	symbolID := fs.Int64("symbol-id", 0, "exact symbol id")
	file := fs.String("file", "", "repository-relative file path")

	repoRootCandidate, err := parseOptionalRepoRootArg(fs, args, repoRootFlag, "")
	if err != nil {
		return &ExitError{Code: editsafety.ExitInvalid, Err: err}
	}
	repoRoot, err := config.ResolveRepoRoot(repoRootCandidate, "")
	if err != nil {
		return &ExitError{Code: editsafety.ExitInvalid, Err: err}
	}
	opened, err := openIndexedRepoReadOnly(ctx, cfg, repoRoot)
	if err != nil {
		return &ExitError{Code: editsafety.ExitInvalid, Err: err}
	}
	defer opened.Close()
	res, err := editsafety.Assess(ctx, opened.Store, opened.Repo.ID, editsafety.Options{
		RepoRoot: repoRoot, Symbol: *symbol, SymbolID: *symbolID, File: *file,
	})
	if err != nil {
		return &ExitError{Code: editsafety.ExitInvalid, Err: fmt.Errorf("safe_to_edit: %w", err)}
	}
	// Default HTML escaping, as the MCP tool's data; only the indentation differs.
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(res); err != nil {
		return err
	}
	if code := editsafety.ExitCode(res); code != 0 {
		return &ExitError{Code: code}
	}
	return nil
}
