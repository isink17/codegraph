package cli

import (
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/limits"
)

// TestCheckQueryBoundsMatchesTheMCPPolicy pins CLI/MCP parity at the policy
// level: the same semantic query must not be bounded differently depending on
// whether an agent or a shell asked it.
func TestCheckQueryBoundsMatchesTheMCPPolicy(t *testing.T) {
	cases := []struct {
		name    string
		limit   int
		offset  int
		depth   int
		detail  string
		wantErr string
	}{
		{name: "defaults", limit: limits.DefaultPage, depth: 2},
		{name: "zero limit is the default", limit: 0, depth: 2},
		{name: "one row", limit: 1, depth: 2},
		{name: "one below the maximum", limit: limits.MaxPage - 1, depth: 2},
		{name: "the maximum", limit: limits.MaxPage, depth: 2},
		{name: "one above the maximum", limit: limits.MaxPage + 1, depth: 2, wantErr: "--limit"},
		{name: "absurd", limit: 100000000, depth: 2, wantErr: "--limit"},
		{name: "negative", limit: -1, depth: 2, wantErr: "--limit"},
		{name: "source detail lowers the ceiling", limit: limits.MaxPageSource + 1, depth: 2, detail: "full", wantErr: "--limit"},
		{name: "source detail at its ceiling", limit: limits.MaxPageSource, depth: 2, detail: "full"},
		{name: "negative offset", limit: 20, offset: -1, depth: 2, wantErr: "--offset"},
		{name: "large offset is legitimate", limit: 20, offset: 100000000, depth: 2},
		{name: "maximum depth", limit: 20, depth: limits.MaxDepth},
		{name: "one above maximum depth", limit: 20, depth: limits.MaxDepth + 1, wantErr: "--depth"},
		{name: "negative depth", limit: 20, depth: -1, wantErr: "--depth"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkQueryBounds(tc.limit, tc.offset, tc.depth, tc.detail)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("checkQueryBounds() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("checkQueryBounds() = nil, want an error naming %s", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("checkQueryBounds() error = %q, want it to name %s", err, tc.wantErr)
			}
		})
	}
}

// TestQueryBoundErrorNamesTheDetailLevelOnlyWhenChosen keeps the message honest:
// with no --detail, the ceiling is not the fault of an empty string.
func TestQueryBoundErrorNamesTheDetailLevelOnlyWhenChosen(t *testing.T) {
	err := checkQueryBounds(limits.MaxPage+1, 0, 2, "")
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), `""`) {
		t.Fatalf("error mentions an empty detail level: %q", err)
	}

	err = checkQueryBounds(limits.MaxPageSource+1, 0, 2, "full")
	if err == nil || !strings.Contains(err.Error(), "full") {
		t.Fatalf("error = %v, want it to name the chosen detail level", err)
	}
}

// TestCLIRejectsAbsurdLimitBeforeOpeningAnything drives the real command. The
// repository path is deliberately one that is not indexed: a bound checked
// before the store is opened cannot report a missing index.
func TestCLIRejectsAbsurdLimitBeforeOpeningAnything(t *testing.T) {
	quietStartup(t)
	_, _, err := runCLI(t, "search", t.TempDir(), "anything", "--limit", "100000000")
	if err == nil {
		t.Fatal("search --limit 100000000 was accepted")
	}
	if !strings.Contains(err.Error(), "--limit") {
		t.Fatalf("error = %q, want it to name --limit", err)
	}
	if strings.Contains(err.Error(), "not indexed") {
		t.Fatalf("the bound was checked after opening the repository: %q", err)
	}
}

// TestCLIExportKeepsItsOwnLimitFamily records that bulk export is not a query
// page: --limit 0 still means the whole repository, and the ceiling that does
// apply points at the cheaper streaming path.
func TestCLIExportKeepsItsOwnLimitFamily(t *testing.T) {
	quietStartup(t)
	_, _, err := runCLI(t, "graph", "export", "--limit", "100000000", t.TempDir())
	if err == nil {
		t.Fatal("graph export --limit 100000000 was accepted")
	}
	if !strings.Contains(err.Error(), "--limit 0") {
		t.Fatalf("error = %q, want it to point at the streaming alternative", err)
	}
	// A page far above the query ceiling is still a legitimate export page.
	if limits.MaxExportPage <= limits.MaxPage {
		t.Fatalf("MaxExportPage = %d must exceed the query ceiling %d", limits.MaxExportPage, limits.MaxPage)
	}
}

// TestCLIAuditExamplesMatchesTheMCPCeiling closes an MCP/CLI divergence: the
// argument multiplies per-finding example queries on both surfaces, so it is
// bounded on both. Zero still means counts-only and negative is still an error.
func TestCLIAuditExamplesMatchesTheMCPCeiling(t *testing.T) {
	quietStartup(t)
	repo := t.TempDir()

	_, _, err := runCLI(t, "audit", repo, "--examples", "100000000")
	if err == nil || !strings.Contains(err.Error(), "--examples") {
		t.Fatalf("audit --examples 100000000 error = %v, want it to name --examples", err)
	}
	if strings.Contains(err.Error(), "not indexed") {
		t.Fatalf("the bound was checked after opening the repository: %q", err)
	}

	_, _, err = runCLI(t, "audit", repo, "--examples", "-1")
	if err == nil || !strings.Contains(err.Error(), "0 or more") {
		t.Fatalf("audit --examples -1 error = %v, want the existing negative message", err)
	}

	// At the ceiling the bound must not fire; the command then fails for the
	// ordinary reason that this directory is not an indexed repository.
	_, _, err = runCLI(t, "audit", repo, "--examples", "100")
	if err != nil && strings.Contains(err.Error(), "--examples") {
		t.Fatalf("audit --examples 100 was rejected by the bound: %v", err)
	}
}
