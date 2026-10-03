package cli

import (
	"context"
	"flag"
	"fmt"
	"io"

	"github.com/isink17/codegraph/internal/config"
	"github.com/isink17/codegraph/internal/constraints"
	"github.com/isink17/codegraph/internal/store"
)

// ExitError asks main to exit with Code. A nil Err exits silently; the command
// has already written its result.
type ExitError struct {
	Code int
	Err  error
}

func (e *ExitError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("exit status %d", e.Code)
	}
	return e.Err.Error()
}

func (e *ExitError) Unwrap() error { return e.Err }

// runCheckConstraints evaluates the repository's architectural constraints.
//
// It opens the graph read-only and only after the config has parsed, so a
// missing or invalid config is reported without an index. The JSON result is
// always written before the exit code is decided.
func runCheckConstraints(ctx context.Context, cfg config.Config, stdout io.Writer, args []string) error {
	fs := flag.NewFlagSet("check_constraints", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repoRootFlag := fs.String("repo-root", "", "repository root (optional)")
	configFlag := fs.String("config", "", "constraints document (optional)")
	limit := fs.Int("limit", 0, "findings per page")
	offset := fs.Int("offset", 0, "offset into the findings")

	repoRootCandidate, err := parseOptionalRepoRootArg(fs, args, repoRootFlag, "")
	if err != nil {
		return &ExitError{Code: 2, Err: err}
	}
	if err := constraints.ValidatePage(*limit, *offset); err != nil {
		return &ExitError{Code: 2, Err: err}
	}
	repoRoot, err := config.ResolveRepoRoot(repoRootCandidate, "")
	if err != nil {
		return &ExitError{Code: 2, Err: err}
	}

	var opened *readOnlyRepo
	defer func() {
		if opened != nil {
			opened.Close()
		}
	}()
	res, err := constraints.Check(ctx, constraints.Options{
		RepoRoot:   repoRoot,
		ConfigPath: *configFlag,
		Limit:      *limit,
		Offset:     *offset,
	}, func(ctx context.Context) (*store.Store, int64, error) {
		r, err := openIndexedRepoReadOnly(ctx, cfg, repoRoot)
		if err != nil {
			return nil, 0, err
		}
		opened = r
		return r.Store, r.Repo.ID, nil
	})
	if err != nil {
		return &ExitError{Code: 2, Err: err}
	}
	if err := writeJSON(stdout, res); err != nil {
		return err
	}
	if code := constraints.ExitCode(res.Status); code != 0 {
		return &ExitError{Code: code, Err: fmt.Errorf("check_constraints: status %s", res.Status)}
	}
	return nil
}
