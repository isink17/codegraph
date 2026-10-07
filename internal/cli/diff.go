package cli

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/isink17/codegraph/internal/config"
	"github.com/isink17/codegraph/internal/diff"
)

func runDiff(ctx context.Context, stdout io.Writer, args []string) error {
	fs := flag.NewFlagSet("diff", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repoRoot := fs.String("repo-root", "", "repository root (defaults to current repository)")
	limit := fs.Int("limit", 0, "changes per page (default 20, max 500)")
	offset := fs.Int("offset", 0, "offset into the deterministic change list")
	var positional, flags []string
	for i := 0; i < len(args); i++ {
		if !strings.HasPrefix(args[i], "-") {
			positional = append(positional, args[i])
			continue
		}
		flags = append(flags, args[i])
		if args[i] == "--repo-root" || args[i] == "--limit" || args[i] == "--offset" {
			i++
			if i >= len(args) {
				return fmt.Errorf("missing value for %s", args[i-1])
			}
			flags = append(flags, args[i])
		}
	}
	if err := fs.Parse(flags); err != nil {
		return err
	}
	if len(positional) != 2 {
		return fmt.Errorf("usage: codegraph diff <base> <head> [--repo-root PATH] [--limit N --offset N]")
	}
	root, err := config.ResolveRepoRoot(*repoRoot, "")
	if err != nil {
		return err
	}
	res, err := diff.CompareCommits(ctx, root, positional[0], positional[1], newDefaultRegistry(), *offset, *limit)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(res)
}
