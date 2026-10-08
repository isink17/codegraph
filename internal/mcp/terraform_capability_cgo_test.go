//go:build cgo

package mcp

import (
	"testing"

	tsparser "github.com/isink17/codegraph/internal/parser/treesitter"
)

// With the tree-sitter adapter the resource referencing a variable is its
// caller, and a graph holding Go and Terraform discloses no limitation.
func TestTerraformReferencesAnswerFindCallersWithoutLimitations(t *testing.T) {
	server, ctx := terraformCapabilityFixture(t, tsparser.NewHCL())
	got := findTerraformCallers(t, server, ctx)
	if len(got.Data.Callers) != 1 || got.Data.Callers[0].QualifiedName != "aws_s3_bucket.logs" {
		t.Fatalf("callers = %+v", got.Data)
	}
	if got.Limitations != nil {
		t.Fatalf("limitations = %+v, want none", got.Limitations)
	}
}
