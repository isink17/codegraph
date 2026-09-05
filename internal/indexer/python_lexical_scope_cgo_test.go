//go:build cgo

package indexer

import (
	"testing"

	"github.com/isink17/codegraph/internal/parser"
	tsparser "github.com/isink17/codegraph/internal/parser/treesitter"
)

func TestPythonLexicalScopeTreeSitterAdapter(t *testing.T) {
	runPythonLexicalCases(t, parser.NewRegistry(tsparser.NewPython()))
}
