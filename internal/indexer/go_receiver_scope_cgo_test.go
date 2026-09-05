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
