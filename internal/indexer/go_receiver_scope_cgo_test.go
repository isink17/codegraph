//go:build cgo

package indexer

import (
	"testing"

	"github.com/isink17/codegraph/internal/parser"
	tsparser "github.com/isink17/codegraph/internal/parser/treesitter"
)

// The tree-sitter adapter must reach the same decisions as the go/ast adapter
// on the same tree. go_receiver_scope is a high-confidence strategy, so an
// adapter that decided differently would make the confidence depend on whether
// the binary was built with cgo.
func TestGoLocalQualifierNeverBecomesPackageImportTreeSitter(t *testing.T) {
	runGoLocalQualifierCases(t, parser.NewRegistry(tsparser.NewGo()))
}

func TestGoReceiverThroughFunctionResultTreeSitter(t *testing.T) {
	runGoReceiverThroughFunctionResult(t, parser.NewRegistry(tsparser.NewGo()))
}

func TestGoPackageIdentityIndexAndUpdateParityTreeSitter(t *testing.T) {
	runGoPackageIdentityParity(t, parser.NewRegistry(tsparser.NewGo()))
}

func TestGoSignatureProfileUpgradeTreeSitter(t *testing.T) {
	runGoSignatureProfileUpgrade(t, parser.NewRegistry(tsparser.NewGo()), "treesitter:go:v3", "treesitter:go:v4")
}

// Both Go parser families store the same declarations for the same tree, so a
// cgo and a non-cgo index differ only in the recorded parser profile.
func TestGoSignatureFamilyParity(t *testing.T) {
	declarations := func(reg *parser.Registry) string {
		g, _ := newGoRepo(t, reg, goSignatureProfileFiles).semanticJSON(t)
		return canonicalJSON(g.Declarations)
	}
	if a, b := declarations(goNativeRegistry()), declarations(parser.NewRegistry(tsparser.NewGo())); a != b {
		t.Fatalf("declarations differ:\n go/ast:      %s\n tree-sitter: %s", a, b)
	}
}
