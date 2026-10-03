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

func TestPythonUnicodeScopeTreeSitterAdapter(t *testing.T) {
	runPythonUnicodeScopeCases(t, parser.NewRegistry(tsparser.NewPython()))
}

func TestTreeSitterPythonProfileUnicodeIdentifiersConverge(t *testing.T) {
	runPythonUnicodeProfileConvergence(t,
		pythonV1Adapter{Adapter: tsparser.NewPython(), id: "treesitter:python:v1"}, tsparser.NewPython())
}
