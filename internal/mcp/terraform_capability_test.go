package mcp

import (
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	heuristicparser "github.com/isink17/codegraph/internal/parser/heuristic"
	"github.com/isink17/codegraph/internal/query"
	"github.com/isink17/codegraph/internal/store"
)

// terraformCapabilityFixture indexes a Go file and a Terraform module with the
// given HCL adapter.
func terraformCapabilityFixture(t *testing.T, hcl parser.Adapter) (*Server, context.Context) {
	t.Helper()
	ctx := context.Background()
	repoRoot := t.TempDir()
	writeRepoFile(t, repoRoot, "lib/lib.go", "package lib\n\nfunc Helper() int { return 1 }\n\nfunc Run() int { return Helper() }\n")
	writeRepoFile(t, repoRoot, "infra/main.tf", "variable \"region\" {}\n\nresource \"aws_s3_bucket\" \"logs\" {\n  bucket = var.region\n}\n")
	s := openTestStore(t)
	t.Cleanup(func() { s.Close() })
	idx := indexer.New(s, parser.NewRegistry(goparser.New(), hcl), nil)
	if _, err := idx.Index(ctx, indexer.Options{RepoRoot: repoRoot}); err != nil {
		t.Fatalf("Index: %v", err)
	}
	repo, err := s.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatalf("UpsertRepo: %v", err)
	}
	return NewServer(repoRoot, repo.ID, s, idx, query.New(s, nil), io.Discard), ctx
}

type terraformCallers struct {
	Data struct {
		Callers []struct {
			QualifiedName string `json:"qualified_name"`
		} `json:"callers"`
		TargetFound bool `json:"target_found"`
	} `json:"data"`
	Limitations []store.GraphLimitation `json:"limitations"`
}

func findTerraformCallers(t *testing.T, server *Server, ctx context.Context) terraformCallers {
	t.Helper()
	text, isErr := callToolRaw(t, server, ctx, "find_callers", map[string]any{"symbol": "var.region"})
	if isErr {
		t.Fatalf("find_callers: %s", text)
	}
	var out terraformCallers
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("%v: %s", err, text)
	}
	return out
}

// The non-cgo fallback records Terraform declarations only, so a reference
// query discloses HCL as symbols-only.
func TestSymbolsOnlyLanguageTerraformFallback(t *testing.T) {
	server, ctx := terraformCapabilityFixture(t, heuristicparser.NewHCL())
	got := findTerraformCallers(t, server, ctx)
	if !got.Data.TargetFound || len(got.Data.Callers) != 0 {
		t.Fatalf("callers = %+v", got.Data)
	}
	if len(got.Limitations) != 1 || got.Limitations[0].Language != "hcl" || got.Limitations[0].Capability != store.GraphSymbolsOnly {
		t.Fatalf("limitations = %+v, want hcl/symbols_only", got.Limitations)
	}
}
