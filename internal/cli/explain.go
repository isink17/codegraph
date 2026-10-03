package cli

import (
	"context"
	"encoding/json"
	"flag"
	"io"

	"github.com/isink17/codegraph/internal/config"
	"github.com/isink17/codegraph/internal/store"
)

// runExplain prints the current-state explanation of the selected edges, the
// same document the MCP explain_edge tool returns as data. It opens the graph
// read-only.
func runExplain(ctx context.Context, cfg config.Config, stdout io.Writer, args []string) error {
	fs := flag.NewFlagSet("explain", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repoRootFlag := fs.String("repo-root", "", "repository root (optional)")
	var sel store.EdgeSelector
	fs.Int64Var(&sel.EdgeID, "edge-id", 0, "edge id")
	fs.StringVar(&sel.File, "file", "", "repository-relative file")
	fs.IntVar(&sel.Line, "line", 0, "line in --file")
	fs.StringVar(&sel.Name, "name", "", "exact destination name at --file:--line")
	fs.IntVar(&sel.Limit, "limit", 0, "edges per page")
	fs.IntVar(&sel.Offset, "offset", 0, "offset into the matching edges")

	repoRootCandidate, err := parseOptionalRepoRootArg(fs, args, repoRootFlag, "")
	if err != nil {
		return err
	}
	if err := sel.Validate(); err != nil {
		return err
	}
	repo, err := openIndexedRepoReadOnly(ctx, cfg, repoRootCandidate)
	if err != nil {
		return err
	}
	defer repo.Close()
	res, err := repo.Store.ExplainEdges(ctx, repo.Repo.ID, sel)
	if err != nil {
		return err
	}
	// Default HTML escaping, as in check_constraints: byte-equal to the MCP
	// tool's json.Marshal-encoded data.
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(res)
}
