//go:build cgo

package cli

import (
	"github.com/isink17/codegraph/internal/parser"
	tsparser "github.com/isink17/codegraph/internal/parser/treesitter"
)

func newDefaultRegistry() *parser.Registry {
	r := parser.NewRegistry(
		tsparser.NewGo(),
		tsparser.NewPython(),
		tsparser.NewJava(),
		tsparser.NewKotlin(),
		tsparser.NewCSharp(),
		tsparser.NewTypeScript(),
		tsparser.NewRust(),
		tsparser.NewRuby(),
		tsparser.NewSwift(),
		tsparser.NewPHP(),
		tsparser.NewCpp(),
		tsparser.NewLua(),
		tsparser.NewScala(),
		tsparser.NewHCL(),
		tsparser.NewDart(),
	)
	r.SetNoCGOCapabilities(noCGOCapabilities())
	return r
}
