package indexer

import "testing"

// An import of the repository-root module's own path names the root package.
// The external test package next to it is the common case: package m_test
// imports "example.com/m" and calls its exported functions and methods.

func goModuleRootTree() map[string]string {
	return map[string]string{
		"go.mod": "module example.com/m\n\ngo 1.21\n",
		"m.go": `package m

type T struct{}

func (T) Bar() {}

func Foo() {}

func New() T { return T{} }
`,
		"m_ext_test.go": `package m_test

import (
	"errors"
	"testing"

	"example.com/m"
	"example.com/m/inner"
	third "github.com/other/m"
)

func TestX(t *testing.T) {
	m.Foo()
	m.New().Bar()
	inner.Helper()
	third.Foo()
	_ = errors.New("x")
}

func shadow(m fooer) { m.Foo() }

type fooer struct{}

func (fooer) Foo() {}
`,
		"inner/inner.go": `package inner

import stderrors "errors"

func Helper() {}

func Is(err error) bool { return stderrors.Is(err, nil) }
`,
		// A decoy with the same names in another package: a name fallback
		// would be visible here.
		"other/other.go": `package other

func Foo() {}

func Is(err, target error) bool { return false }
`,
	}
}

func assertGoModuleRootGraph(t *testing.T, step string, edge func(src, dst string) string) {
	t.Helper()
	for _, c := range []struct{ src, dst, want string }{
		{"m_ext_test.go", "example.com/m.Foo", "m.go:m.Foo [module_import/high]"},
		{"m_ext_test.go", "example.com/m.New", "m.go:m.New [module_import/high]"},
		{"m_ext_test.go", "example.com/m/inner.Helper", "inner/inner.go:inner.Helper [module_import/high]"},
		// A third-party import whose last segment spells the root package.
		{"m_ext_test.go", "github.com/other/m.Foo", "<unresolved> [/]"},
		// The standard library, unaliased and aliased.
		{"m_ext_test.go", "errors.New", "<unresolved> [/]"},
		{"inner/inner.go", "errors.Is", "<unresolved> [/]"},
		// A parameter shadowing the import is the local, not the root package.
		{"m_ext_test.go", "m.Foo", "m_ext_test.go:m_test.fooer.Foo [go_receiver_scope/high]"},
	} {
		if got := edge(c.src, c.dst); got != c.want {
			t.Errorf("%s: %s %q => %s, want %s", step, c.src, c.dst, got, c.want)
		}
	}
}

func TestGoModuleRootImportBindsRootPackage(t *testing.T) {
	r := newGoRepo(t, goNativeRegistry(), goModuleRootTree())
	edge := func(src, dst string) string { return r.edgeState(t, src, dst) }
	assertGoModuleRootGraph(t, "fresh", edge)

	r.write(t, "m_ext_test.go", goModuleRootTree()["m_ext_test.go"]+"\n// touched\n")
	r.update(t)
	assertGoModuleRootGraph(t, "caller update", edge)
	r.write(t, "m.go", goModuleRootTree()["m.go"]+"\n// touched\n")
	r.update(t)
	assertGoModuleRootGraph(t, "target update", edge)
	r.update(t)
	assertGoModuleRootGraph(t, "no-op", edge)
	r.assertFreshParity(t, goNativeRegistry(), "after updates")
}

// Without the root package's files nothing may bind, and the root path does
// not fall back to a same-named function elsewhere.
func TestGoModuleRootImportRefusals(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(map[string]string)
	}{
		// GOPATH-style tree: no module path is known.
		{"no go.mod", func(f map[string]string) { delete(f, "go.mod") }},
		// The module path is someone else's; the import is not the root.
		{"different module path", func(f map[string]string) { f["go.mod"] = "module example.com/notm\n\ngo 1.21\n" }},
		// A replace directive is not followed: the import stays the module's own.
		{"replace ignored", func(f map[string]string) {
			f["go.mod"] = "module example.com/m\n\ngo 1.21\n\nreplace example.com/m => ./other\n"
		}},
		// Two go.mod files declaring the root path fail closed.
		{"duplicate module path", func(f map[string]string) { f["other/go.mod"] = "module example.com/m\n\ngo 1.21\n" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := goModuleRootTree()
			tc.mutate(files)
			r := newGoRepo(t, goNativeRegistry(), files)
			got := r.edgeState(t, "m_ext_test.go", "example.com/m.Foo")
			want := "<unresolved> [/]"
			if tc.name == "replace ignored" {
				want = "m.go:m.Foo [module_import/high]"
			}
			if got != want {
				t.Errorf("example.com/m.Foo => %s, want %s", got, want)
			}
			if got := r.edgeState(t, "m_ext_test.go", "github.com/other/m.Foo"); got != "<unresolved> [/]" {
				t.Errorf("third-party Foo => %s", got)
			}
		})
	}
}
