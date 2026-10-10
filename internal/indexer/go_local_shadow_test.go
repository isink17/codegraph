//go:build cgo

package indexer

import (
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	tsparser "github.com/isink17/codegraph/internal/parser/treesitter"
)

const goShadowedClosure = `package p

func Shadow() int {
	NewMatcher := func() int { return 2 }
	return NewMatcher()
}
`

const goUnshadowed = `package p

func Shadow() int {
	return NewMatcher()
}
`

// A bare call of a local that shadows a package-level function is a call of
// the local (go/types resolves it to the closure), so it must never bind the
// package function; the same call without the local still does. Both parsers,
// fresh and incremental, both directions.
func TestGoBareCallShadowedByLocalStaysUnresolved(t *testing.T) {
	for _, tc := range []struct {
		name string
		reg  *parser.Registry
	}{
		{"go-ast", parser.NewRegistry(goparser.New())},
		{"tree-sitter", parser.NewRegistry(tsparser.NewGo())},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newGoRepo(t, tc.reg, map[string]string{
				"go.mod":      "module example.com/m\n\ngo 1.22\n",
				"p/match.go":  "package p\n\nfunc NewMatcher() int { return 1 }\n",
				"p/shadow.go": goShadowedClosure,
				"p/plain.go":  "package p\n\nfunc Plain() int { return NewMatcher() }\n",
				"p/param.go":  "package p\n\nfunc Param(NewMatcher func() int) int { return NewMatcher() }\n",
			})
			bound := "p/match.go:p.NewMatcher [go_package_scope/high]"
			check := func(step, file, want string) {
				t.Helper()
				if got := r.edgeState(t, file, "NewMatcher"); !strings.HasPrefix(got, want) {
					t.Fatalf("%s: %s NewMatcher => %s, want %s", step, file, got, want)
				}
			}
			check("fresh", "p/shadow.go", "<unresolved>")
			check("fresh", "p/param.go", "<unresolved>")
			check("fresh", "p/plain.go", bound)
			r.assertFreshParity(t, tc.reg, "fresh")

			r.write(t, "p/shadow.go", goUnshadowed)
			r.update(t)
			check("local removed", "p/shadow.go", bound)
			r.assertFreshParity(t, tc.reg, "local removed")

			r.write(t, "p/shadow.go", goShadowedClosure)
			r.update(t)
			check("local restored", "p/shadow.go", "<unresolved>")
			check("local restored", "p/plain.go", bound)
			r.assertFreshParity(t, tc.reg, "local restored")

			// A path-scoped update decides through the binder twin.
			r.write(t, "p/shadow.go", goUnshadowed)
			r.updatePaths(t, "p/shadow.go")
			check("path-scoped removed", "p/shadow.go", bound)
			r.write(t, "p/shadow.go", goShadowedClosure)
			r.updatePaths(t, "p/shadow.go")
			check("path-scoped restored", "p/shadow.go", "<unresolved>")
			r.assertFreshParity(t, tc.reg, "path-scoped restored")
		})
	}
}

func (r *goRepo) updatePaths(t *testing.T, paths ...string) {
	t.Helper()
	if _, err := r.idx.Update(r.ctx, Options{RepoRoot: r.root, ScanKind: "update", Paths: paths}); err != nil {
		t.Fatalf("update %v: %v", paths, err)
	}
}
