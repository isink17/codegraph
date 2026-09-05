package indexer

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/store"
)

// goRepo is a disposable on-disk Go repository, indexed end to end. Parser-only
// tests cannot see a resolution strategy, and the strategy is the thing this
// slice is about.
type goRepo struct {
	ctx    context.Context
	root   string
	store  *store.Store
	idx    *Indexer
	repoID int64
}

func newGoRepo(t *testing.T, reg *parser.Registry, files map[string]string) *goRepo {
	t.Helper()
	ctx := context.Background()
	s, err := store.Open(filepath.Join(t.TempDir(), "codegraph.sqlite"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	r := &goRepo{ctx: ctx, root: t.TempDir(), store: s, idx: New(s, reg, nil)}
	for rel, content := range files {
		r.write(t, rel, content)
	}
	if _, err := r.idx.Index(ctx, Options{RepoRoot: r.root, ScanKind: "index"}); err != nil {
		t.Fatalf("index: %v", err)
	}
	repo, err := s.UpsertRepo(ctx, r.root)
	if err != nil {
		t.Fatalf("upsert repo: %v", err)
	}
	r.repoID = repo.ID
	return r
}

func (r *goRepo) write(t *testing.T, rel, content string) {
	t.Helper()
	abs := filepath.Join(r.root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", rel, err)
	}
	if err := os.WriteFile(abs, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", rel, err)
	}
}

func (r *goRepo) remove(t *testing.T, rel string) {
	t.Helper()
	if err := os.Remove(filepath.Join(r.root, filepath.FromSlash(rel))); err != nil {
		t.Fatalf("remove %s: %v", rel, err)
	}
}

func (r *goRepo) update(t *testing.T) {
	t.Helper()
	if _, err := r.idx.Update(r.ctx, Options{RepoRoot: r.root, ScanKind: "update"}); err != nil {
		t.Fatalf("update: %v", err)
	}
}

func (r *goRepo) projection(t *testing.T) []string {
	t.Helper()
	symbols, err := r.store.ExportSymbolsPage(r.ctx, r.repoID, 100000, 0)
	if err != nil {
		t.Fatalf("export symbols: %v", err)
	}
	symbolFile := make(map[int64]string, len(symbols))
	for _, sym := range symbols {
		symbolFile[sym.ID] = filepath.ToSlash(sym.FilePath)
	}
	edges, err := r.store.ExportEdgesPage(r.ctx, r.repoID, 100000, 0)
	if err != nil {
		t.Fatalf("export edges: %v", err)
	}
	lines := make([]string, 0, len(edges))
	for _, e := range edges {
		dst := "<unresolved>"
		if e.DstSymbolID != nil {
			dst = symbolFile[*e.DstSymbolID] + ":" + e.DstQualifiedName
		}
		lines = append(lines, "edge "+filepath.ToSlash(e.FilePath)+` "`+e.DstName+`" => `+
			dst+" ["+e.ResolutionStrategy+"/"+e.ResolutionConfidence+"]")
	}
	sort.Strings(lines)
	return lines
}

// edgeState renders one caller's outgoing edge for a dst_name.
func (r *goRepo) edgeState(t *testing.T, srcPath, dstName string) string {
	t.Helper()
	prefix := "edge " + srcPath + " "
	needle := `"` + dstName + `" => `
	for _, line := range r.projection(t) {
		if strings.HasPrefix(line, prefix) && strings.Contains(line, needle) {
			return line[strings.Index(line, needle)+len(needle):]
		}
	}
	return "<no edge>"
}

func (r *goRepo) currentTree(t *testing.T) map[string]string {
	t.Helper()
	tree := map[string]string{}
	err := filepath.Walk(r.root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, relErr := filepath.Rel(r.root, p)
		if relErr != nil {
			return relErr
		}
		if strings.HasPrefix(filepath.ToSlash(rel), ".codegraph/") {
			return nil
		}
		data, readErr := os.ReadFile(p)
		if readErr != nil {
			return readErr
		}
		tree[filepath.ToSlash(rel)] = string(data)
		return nil
	})
	if err != nil {
		t.Fatalf("read tree: %v", err)
	}
	return tree
}

// assertFreshParity is the point of every incremental step: the updated
// database must project exactly what a fresh index of the same final tree
// projects. No stale go_receiver_scope binding may survive an update.
func (r *goRepo) assertFreshParity(t *testing.T, reg *parser.Registry, step string) {
	t.Helper()
	fresh := newGoRepo(t, reg, r.currentTree(t))
	want := fresh.projection(t)
	got := r.projection(t)
	if diff := goProjectionDiff(want, got); diff != "" {
		t.Fatalf("%s: update diverges from a fresh index of the same tree:\n%s", step, diff)
	}
}

func goProjectionDiff(want, got []string) string {
	counts := map[string]int{}
	for _, line := range want {
		counts[line]++
	}
	for _, line := range got {
		counts[line]--
	}
	keys := make([]string, 0, len(counts))
	for line, n := range counts {
		if n != 0 {
			keys = append(keys, line)
		}
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, line := range keys {
		if counts[line] > 0 {
			b.WriteString("  fresh-only: " + line + "\n")
		} else {
			b.WriteString("  update-only: " + line + "\n")
		}
	}
	return b.String()
}

func goNativeRegistry() *parser.Registry {
	return parser.NewRegistry(goparser.New())
}

const goShadowModule = `module example.com/project

go 1.22
`

const goShadowStorePkg = `package store

func Get() {}
`

// goShadowMain is the miswire this slice exists to remove, end to end: `store`
// is a *Thing parameter in one function and the imported package in the other.
const goShadowMain = `package main

import store "example.com/project/store"

type Thing struct{}

func (t *Thing) Get() {}

func shadow(store *Thing) {
	store.Get()
}

func real() {
	store.Get()
}
`

// TestGoLocalQualifierNeverBecomesPackageImport is the regression for the false
// edge: before this slice both adapters bound line 10 to the imported package's
// Get with module_import/high.
func TestGoLocalQualifierNeverBecomesPackageImport(t *testing.T) {
	runGoLocalQualifierCases(t, goNativeRegistry())
}

func runGoLocalQualifierCases(t *testing.T, reg *parser.Registry) {
	t.Helper()
	r := newGoRepo(t, reg, map[string]string{
		"go.mod":         goShadowModule,
		"store/store.go": goShadowStorePkg,
		"main.go":        goShadowMain,
	})

	// The shadowed call reaches the method on the local's proven type.
	if got, want := r.edgeState(t, "main.go", "store.Get"), "main.go:main.Thing.Get [go_receiver_scope/high]"; got != want {
		t.Errorf("shadowed call: %s, want %s", got, want)
	}
	// The genuine import binding is untouched.
	if got, want := r.edgeState(t, "main.go", "example.com/project/store.Get"), "store/store.go:store.Get [module_import/high]"; got != want {
		t.Errorf("unshadowed call: %s, want %s", got, want)
	}
	for _, line := range r.projection(t) {
		if strings.Contains(line, `"store.Get" =>`) && strings.Contains(line, "store/store.go") {
			t.Errorf("local qualifier reached the imported package: %s", line)
		}
	}
}

// TestGoUnknownLocalTypeStaysUnresolved is the veto. `s := factory()` proves s
// is local and proves nothing about its type, so the edge must resolve to
// nothing at all -- not to the same-named method that dot_tail2 would otherwise
// hand it.
func TestGoUnknownLocalTypeStaysUnresolved(t *testing.T) {
	reg := goNativeRegistry()
	r := newGoRepo(t, reg, map[string]string{
		"go.mod": goShadowModule,
		"main.go": `package main

type Thing struct{}

func (t *Thing) Only() {}

func factory() *Thing { return nil }

func run() {
	s := factory()
	s.Only()
}
`,
	})
	if got, want := r.edgeState(t, "main.go", "s.Only"), "<unresolved> [/]"; got != want {
		t.Errorf("unknown local type: %s, want %s", got, want)
	}
}

// TestGoAmbiguousReceiverCandidateStaysUnresolved: two methods this evidence
// cannot tell apart mean the evidence identified none.
func TestGoAmbiguousReceiverCandidateStaysUnresolved(t *testing.T) {
	reg := goNativeRegistry()
	r := newGoRepo(t, reg, map[string]string{
		"go.mod": goShadowModule,
		"a/a.go": `package a

type Thing struct{}

func (t *Thing) Do() {}

func run(x *Thing) { x.Do() }
`,
		"a/b.go": `package a

func (t *Thing) Do2() {}
`,
	})
	if got, want := r.edgeState(t, "a/a.go", "x.Do"), "a/a.go:a.Thing.Do [go_receiver_scope/high]"; got != want {
		t.Errorf("unique candidate: %s, want %s", got, want)
	}

	// Introducing a second Thing.Do in the same package makes the name
	// ambiguous, and the binding must go away rather than pick one.
	r.write(t, "a/c.go", `package a

func (t *Thing) Do() {}
`)
	r.update(t)
	if got, want := r.edgeState(t, "a/a.go", "x.Do"), "<unresolved> [/]"; got != want {
		t.Errorf("ambiguous candidate: %s, want %s", got, want)
	}
	r.assertFreshParity(t, reg, "duplicate method introduced")

	r.remove(t, "a/c.go")
	r.update(t)
	if got, want := r.edgeState(t, "a/a.go", "x.Do"), "a/a.go:a.Thing.Do [go_receiver_scope/high]"; got != want {
		t.Errorf("after duplicate removed: %s, want %s", got, want)
	}
	r.assertFreshParity(t, reg, "duplicate method removed")
}

// TestGoReceiverScopeIncrementalParity walks the lifecycle the spec enumerates
// and compares each step against a fresh index of the same tree.
func TestGoReceiverScopeIncrementalParity(t *testing.T) {
	reg := goNativeRegistry()
	r := newGoRepo(t, reg, map[string]string{
		"go.mod":         goShadowModule,
		"store/store.go": goShadowStorePkg,
		"main.go": `package main

import store "example.com/project/store"

type Thing struct{}
type Other struct{}

func (t *Thing) Get() {}
func (o *Other) Get() {}

func caller() {
	store.Get()
}
`,
	})
	if got, want := r.edgeState(t, "main.go", "example.com/project/store.Get"), "store/store.go:store.Get [module_import/high]"; got != want {
		t.Fatalf("baseline: %s, want %s", got, want)
	}

	steps := []struct {
		name    string
		content string
		dst     string
		want    string
	}{
		{
			name: "B: shadow the package-qualified call",
			content: `package main

import store "example.com/project/store"

type Thing struct{}
type Other struct{}

func (t *Thing) Get() {}
func (o *Other) Get() {}

func caller(store *Thing) {
	store.Get()
}
`,
			dst:  "store.Get",
			want: "main.go:main.Thing.Get [go_receiver_scope/high]",
		},
		{
			name: "D: parameter type changes *Thing -> *Other",
			content: `package main

import store "example.com/project/store"

type Thing struct{}
type Other struct{}

func (t *Thing) Get() {}
func (o *Other) Get() {}

func caller(store *Other) {
	store.Get()
}
`,
			dst:  "store.Get",
			want: "main.go:main.Other.Get [go_receiver_scope/high]",
		},
		{
			name: "L: the proven type becomes unknown",
			content: `package main

import store "example.com/project/store"

type Thing struct{}
type Other struct{}

func (t *Thing) Get() {}
func (o *Other) Get() {}

func build() *Other { return nil }

func caller() {
	store := build()
	store.Get()
}
`,
			dst:  "store.Get",
			want: "<unresolved> [/]",
		},
		{
			name: "C: remove the shadow, the import binding returns",
			content: `package main

import store "example.com/project/store"

type Thing struct{}
type Other struct{}

func (t *Thing) Get() {}
func (o *Other) Get() {}

func caller() {
	store.Get()
}
`,
			dst:  "example.com/project/store.Get",
			want: "store/store.go:store.Get [module_import/high]",
		},
	}

	for _, step := range steps {
		r.write(t, "main.go", step.content)
		r.update(t)
		if got := r.edgeState(t, "main.go", step.dst); got != step.want {
			t.Errorf("%s: %s, want %s", step.name, got, step.want)
		}
		r.assertFreshParity(t, reg, step.name)
	}
}

// TestGoReceiverScopeThirdFileInvalidation: the caller's own source never
// changes. Only the file declaring the method does, and the caller must be
// reconsidered anyway.
func TestGoReceiverScopeThirdFileInvalidation(t *testing.T) {
	reg := goNativeRegistry()
	caller := `package a

type Thing struct{}

func run(x *Thing) { x.Later() }
`
	r := newGoRepo(t, reg, map[string]string{
		"go.mod": goShadowModule,
		"a/a.go": caller,
	})
	if got, want := r.edgeState(t, "a/a.go", "x.Later"), "<unresolved> [/]"; got != want {
		t.Fatalf("before the method exists: %s, want %s", got, want)
	}

	// G: the method target is added in another file.
	r.write(t, "a/b.go", `package a

func (t *Thing) Later() {}
`)
	r.update(t)
	if got, want := r.edgeState(t, "a/a.go", "x.Later"), "a/b.go:a.Thing.Later [go_receiver_scope/high]"; got != want {
		t.Errorf("after the method is added elsewhere: %s, want %s", got, want)
	}
	r.assertFreshParity(t, reg, "method added in a third file")

	// H: the method target is deleted again.
	r.remove(t, "a/b.go")
	r.update(t)
	if got, want := r.edgeState(t, "a/a.go", "x.Later"), "<unresolved> [/]"; got != want {
		t.Errorf("after the method is deleted elsewhere: %s, want %s", got, want)
	}
	r.assertFreshParity(t, reg, "method deleted in a third file")
}

// TestGoReceiverScopeTestFileMethodStaysInItsPackage: a method declared in
// package a's test files is not visible to anything that imports a -- not even
// another package's tests -- so it may only answer a call from package a
// itself.
func TestGoReceiverScopeTestFileMethodStaysInItsPackage(t *testing.T) {
	reg := goNativeRegistry()
	r := newGoRepo(t, reg, map[string]string{
		"go.mod": goShadowModule,
		"a/a.go": `package a

type Thing struct{}
`,
		"a/a_test.go": `package a

func (t *Thing) Helper() {}

func runLocal(x *Thing) { x.Helper() }
`,
		"b/b_test.go": `package b

import "example.com/project/a"

func runForeign(x *a.Thing) { x.Helper() }
`,
	})
	// Same package, test caller, test target: allowed.
	if got, want := r.edgeState(t, "a/a_test.go", "x.Helper"), "a/a_test.go:a.Thing.Helper [go_receiver_scope/high]"; got != want {
		t.Errorf("same-package test caller: %s, want %s", got, want)
	}
	// Another package's test file cannot see it.
	if got, want := r.edgeState(t, "b/b_test.go", "x.Helper"), "<unresolved> [/]"; got != want {
		t.Errorf("cross-package test target: %s, want %s", got, want)
	}
}

// TestGoReceiverScopeRespectsPackageIdentity: a type name is not a package. A
// same-named type in another package must not answer this call.
func TestGoReceiverScopeRespectsPackageIdentity(t *testing.T) {
	reg := goNativeRegistry()
	r := newGoRepo(t, reg, map[string]string{
		"go.mod": goShadowModule,
		"a/a.go": `package a

type Thing struct{}

func run(x *Thing) { x.Only() }
`,
		"b/b.go": `package b

type Thing struct{}

func (t *Thing) Only() {}
`,
	})
	if got, want := r.edgeState(t, "a/a.go", "x.Only"), "<unresolved> [/]"; got != want {
		t.Errorf("cross-package same-named type: %s, want %s", got, want)
	}
}
