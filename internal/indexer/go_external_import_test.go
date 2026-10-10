package indexer

import "testing"

// A Go selector call whose qualifier is an import binding names a symbol of
// the imported package. When that package lies outside every module of the
// repository (the standard library, a third-party dependency) nothing indexed
// can be its target, so the call must stay unresolved -- even when the import
// path, which the parser substitutes for the alias, spells a local package's
// qualified name. pkg/errors v0.9.1 is the real case: package errors imports
// the standard library as stderrors and defines its own Is, As and Unwrap.

const goExternalImportModule = "module example.com/errors\n\ngo 1.21\n"

const goExternalImportGo113 = `package errors

import stderrors "errors"

func Is(err, target error) bool { return stderrors.Is(err, target) }

func As(err error, target interface{}) bool { return stderrors.As(err, target) }

func Unwrap(err error) error { return stderrors.Unwrap(err) }
`

func goExternalImportTree() map[string]string {
	return map[string]string{
		"go.mod":   goExternalImportModule,
		"go113.go": goExternalImportGo113,
		"wrap.go": `package errors

import (
	pkgerr "github.com/other/errors"
	"example.com/errors/sub"
)

func Wrap(err error) error { return pkgerr.Wrap(err) }

func Local(err error) bool {
	sub.Helper()
	return Is(err, nil)
}
`,
		"sub/sub.go": `package sub

func Helper() {}
`,
		"other/other.go": `package other

import "errors"

type checker struct{}

func (checker) Is(err, target error) bool { return false }

func New(s string) error { return errors.New(s) }

func Shadow(errors checker) bool { return errors.Is(nil, nil) }
`,
		"go113_test.go": `package errors_test

import (
	stderrors "errors"
	pkg "example.com/errors"
)

func check() bool { return stderrors.Is(nil, nil) && pkg.Is(nil, nil) }
`,
	}
}

func assertGoExternalImportGraph(t *testing.T, step string, edge func(src, dst string) string) {
	t.Helper()
	for _, c := range []struct{ src, dst, want string }{
		// The standard library through an alias: no local Is, As or Unwrap.
		{"go113.go", "errors.Is", "<unresolved> [/]"},
		{"go113.go", "errors.As", "<unresolved> [/]"},
		{"go113.go", "errors.Unwrap", "<unresolved> [/]"},
		// The standard library unaliased, from a package that is not errors.
		{"other/other.go", "errors.New", "<unresolved> [/]"},
		// A renamed third-party import whose tail matches the local Wrap.
		{"wrap.go", "github.com/other/errors.Wrap", "<unresolved> [/]"},
		// Own-module import and bare same-package call keep their bindings.
		{"wrap.go", "example.com/errors/sub.Helper", "sub/sub.go:sub.Helper [module_import/high]"},
		{"wrap.go", "Is", "go113.go:errors.Is [go_package_scope/high]"},
		// A parameter shadowing the import is the local, not the package.
		{"other/other.go", "errors.Is", "other/other.go:other.checker.Is [go_receiver_scope/high]"},
		// External test package importing the module root binds the root
		// package, not the same-named Is of extra/ or the standard library.
		{"go113_test.go", "example.com/errors.Is", "go113.go:errors.Is [module_import/high]"},
	} {
		if got := edge(c.src, c.dst); got != c.want {
			t.Errorf("%s: %s %q => %s, want %s", step, c.src, c.dst, got, c.want)
		}
	}
	// The external test file's stderrors.Is shares the spelling errors.Is
	// with nothing it may bind.
	if got := edge("go113_test.go", "errors.Is"); got != "<unresolved> [/]" {
		t.Errorf("%s: go113_test.go \"errors.Is\" => %s, want <unresolved>", step, got)
	}
}

func TestGoExternalImportNeverBindsLocalPackage(t *testing.T) {
	r := newGoRepo(t, goNativeRegistry(), goExternalImportTree())
	edge := func(src, dst string) string { return r.edgeState(t, src, dst) }
	assertGoExternalImportGraph(t, "fresh", edge)

	// Path-scoped update of the caller, name-targeted redecision of Is via a
	// new same-named declaration elsewhere, and the reverse mutation.
	r.write(t, "go113.go", goExternalImportGo113+"\n// touched\n")
	r.update(t)
	assertGoExternalImportGraph(t, "path update", edge)
	r.write(t, "extra/extra.go", "package extra\n\nfunc Is(err, target error) bool { return false }\nfunc Unwrap(err error) error { return nil }\n")
	r.update(t)
	assertGoExternalImportGraph(t, "name redecision", edge)
	r.remove(t, "extra/extra.go")
	r.update(t)
	assertGoExternalImportGraph(t, "reverse", edge)
	r.update(t)
	assertGoExternalImportGraph(t, "no-op", edge)
}

// TestGoExternalImportVetoWithoutGoMod is the same corpus in a GOPATH-style
// tree: pkg/errors v0.9.1 ships no go.mod. No import path can be mapped to a
// directory of the repository, so none binds, and the standard library's
// spelling still never reaches the local package.
func TestGoExternalImportVetoWithoutGoMod(t *testing.T) {
	files := goExternalImportTree()
	delete(files, "go.mod")
	r := newGoRepo(t, goNativeRegistry(), files)
	for _, c := range []struct{ src, dst, want string }{
		{"go113.go", "errors.Is", "<unresolved> [/]"},
		{"go113.go", "errors.As", "<unresolved> [/]"},
		{"go113.go", "errors.Unwrap", "<unresolved> [/]"},
		{"wrap.go", "github.com/other/errors.Wrap", "<unresolved> [/]"},
		{"wrap.go", "example.com/errors/sub.Helper", "<unresolved> [/]"},
		{"wrap.go", "Is", "go113.go:errors.Is [go_package_scope/high]"},
	} {
		if got := r.edgeState(t, c.src, c.dst); got != c.want {
			t.Errorf("%s %q => %s, want %s", c.src, c.dst, got, c.want)
		}
	}
}
