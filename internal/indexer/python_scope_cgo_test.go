//go:build cgo

package indexer

import (
	"testing"

	"github.com/isink17/codegraph/internal/parser"
	tsparser "github.com/isink17/codegraph/internal/parser/treesitter"
)

// The tree-sitter adapter must reach the same decisions as the regex adapter on
// the same tree: import evidence and call spellings are the contract between
// the two parser paths, not an implementation detail of either.
func TestPythonImportScopeTreeSitterAdapter(t *testing.T) {
	runPythonScopeCases(t, parser.NewRegistry(tsparser.NewPython()))
}
