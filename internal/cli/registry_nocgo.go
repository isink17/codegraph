//go:build !cgo

package cli

import (
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	heuristicparser "github.com/isink17/codegraph/internal/parser/heuristic"
	pyparser "github.com/isink17/codegraph/internal/parser/python"
)

func newDefaultRegistry() *parser.Registry {
	return parser.NewRegistry(
		goparser.New(),
		pyparser.New(),
		heuristicparser.NewJava(),
		heuristicparser.NewKotlin(),
		heuristicparser.NewCSharp(),
		heuristicparser.NewTypeScriptJavaScript(),
		heuristicparser.NewRust(),
		heuristicparser.NewRuby(),
		heuristicparser.NewSwift(),
		heuristicparser.NewPHP(),
		heuristicparser.NewCAndCpp(),
	)
}
