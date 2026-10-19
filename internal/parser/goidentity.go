package parser

import (
	"go/build/constraint"
	goparser "go/parser"
	"go/token"
	"strings"

	"github.com/isink17/codegraph/internal/graph"
)

// GoFileIdentity reads a Go file's package clause and the build constraint in
// the comments above it. It returns nil when the header does not parse.
func GoFileIdentity(content []byte) *graph.GoFileIdentity {
	fset := token.NewFileSet()
	f, err := goparser.ParseFile(fset, "", content, goparser.PackageClauseOnly|goparser.ParseComments)
	if err != nil || f.Name == nil || f.Name.Name == "" {
		return nil
	}
	var goBuild constraint.Expr
	var plusBuild []constraint.Expr
	for _, group := range f.Comments {
		if group.Pos() >= f.Package {
			break
		}
		for _, c := range group.List {
			switch {
			case constraint.IsGoBuild(c.Text):
				expr, err := constraint.Parse(c.Text)
				if err != nil || goBuild != nil {
					// The toolchain rejects such a file; keep it distinct from
					// every well-formed one.
					return &graph.GoFileIdentity{Package: f.Name.Name, BuildConstraint: "invalid:" + strings.TrimSpace(c.Text)}
				}
				goBuild = expr
			case constraint.IsPlusBuild(c.Text):
				expr, err := constraint.Parse(c.Text)
				if err != nil {
					return &graph.GoFileIdentity{Package: f.Name.Name, BuildConstraint: "invalid:" + strings.TrimSpace(c.Text)}
				}
				plusBuild = append(plusBuild, expr)
			}
		}
	}
	if goBuild == nil {
		for _, expr := range plusBuild {
			if goBuild == nil {
				goBuild = expr
			} else {
				goBuild = &constraint.AndExpr{X: goBuild, Y: expr}
			}
		}
	}
	out := &graph.GoFileIdentity{Package: f.Name.Name}
	if goBuild != nil {
		out.BuildConstraint = goBuild.String()
	}
	return out
}
