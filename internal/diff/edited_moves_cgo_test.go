//go:build cgo

package diff

import (
	"github.com/isink17/codegraph/internal/parser"
	tsparser "github.com/isink17/codegraph/internal/parser/treesitter"
)

func init() {
	testGoAdapters["tree-sitter"] = func() parser.Adapter { return tsparser.NewGo() }
}
