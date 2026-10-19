package diff

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/isink17/codegraph/internal/githistory/gittest"
	"github.com/isink17/codegraph/internal/store"
)

func merged(maps ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, m := range maps {
		for k, v := range m {
			out[k] = v
		}
	}
	return out
}

func TestCompareCommitsEmbedContinuity(t *testing.T) {
	const gomod = "module example.com/m\n\ngo 1.22\n"
	embedded := func(pattern string) string {
		return "package p\n\nimport _ \"embed\"\n\n//go:embed " + pattern + "\nvar Data string\n\nfunc A() int { return 1 }\n"
	}
	app := map[string]string{
		"go.mod":      gomod,
		"p/a.go":      embedded("data.txt"),
		"p/data.txt":  "one\n",
		"cmd/main.go": "package main\n\nimport \"example.com/m/p\"\n\nfunc main() { _ = p.A(); _ = p.Data }\n",
	}
	// A module root that moves with its package keeps its import paths.
	moved := map[string]string{
		"go.mod":          "module example.com/app\n\ngo 1.22\n",
		"main.go":         "package main\n\nimport \"example.com/lib/p\"\n\nfunc main() { _ = p.A(); _ = p.Data }\n",
		"libs/go.mod":     "module example.com/lib\n\ngo 1.22\n",
		"libs/p/a.go":     embedded("data.txt"),
		"libs/p/data.txt": "one\n",
	}
	for name, adapter := range testGoAdapters {
		t.Run(name, func(t *testing.T) {
			for _, tc := range []struct {
				name    string
				files   map[string]string
				change  func(r *gittest.Repo)
				pairs   []string
				refused []string
			}{
				{name: "exact rename, resource unchanged", files: app, change: func(r *gittest.Repo) { r.Git("mv", "p/a.go", "p/b.go") },
					pairs: []string{"p/a.go>p/b.go:p.A", "p/a.go>p/b.go:p.Data"}},
				{name: "exact rename, resource bytes changed", files: app, change: func(r *gittest.Repo) {
					r.Git("mv", "p/a.go", "p/b.go")
					r.Write("p/data.txt", "two\n")
				}, pairs: []string{"p/a.go>p/b.go:p.A"}, refused: []string{"p/a.go>p/b.go:" + refuseDeclEmbedUnproven}},
				{name: "exact rename, resource removed", files: app, change: func(r *gittest.Repo) {
					r.Git("mv", "p/a.go", "p/b.go")
					r.Git("rm", "-q", "p/data.txt")
				}, pairs: []string{"p/a.go>p/b.go:p.A"}, refused: []string{"p/a.go>p/b.go:" + refuseDeclEmbedUnproven}},
				{name: "exact rename, glob pattern", files: merged(app, map[string]string{"p/a.go": embedded("*.txt")}), change: func(r *gittest.Repo) { r.Git("mv", "p/a.go", "p/b.go") },
					pairs: []string{"p/a.go>p/b.go:p.A"}, refused: []string{"p/a.go>p/b.go:" + refuseDeclEmbedUnproven}},
				{name: "exact rename, resource is a directory", files: merged(app, map[string]string{"p/a.go": embedded("static"), "p/static/x.txt": "x\n"}), change: func(r *gittest.Repo) { r.Git("mv", "p/a.go", "p/b.go") },
					pairs: []string{"p/a.go>p/b.go:p.A"}, refused: []string{"p/a.go>p/b.go:" + refuseDeclEmbedUnproven}},
				{name: "exact rename, resource under a nested module", files: merged(app, map[string]string{"p/a.go": embedded("sub/data.txt"), "p/sub/data.txt": "one\n", "p/sub/go.mod": "module example.com/sub\n"}),
					change: func(r *gittest.Repo) { r.Git("mv", "p/a.go", "p/b.go") }, pairs: []string{"p/a.go>p/b.go:p.A"}, refused: []string{"p/a.go>p/b.go:" + refuseDeclEmbedUnproven}},
				{name: "exact rename, quoted pattern", files: merged(app, map[string]string{"p/a.go": embedded(`"data.txt"`)}), change: func(r *gittest.Repo) { r.Git("mv", "p/a.go", "p/b.go") },
					pairs: []string{"p/a.go>p/b.go:p.A"}, refused: []string{"p/a.go>p/b.go:" + refuseDeclEmbedUnproven}},
				{name: "exact rename, all: pattern", files: merged(app, map[string]string{"p/a.go": embedded("all:data.txt")}), change: func(r *gittest.Repo) { r.Git("mv", "p/a.go", "p/b.go") },
					pairs: []string{"p/a.go>p/b.go:p.A"}, refused: []string{"p/a.go>p/b.go:" + refuseDeclEmbedUnproven}},
				// The link's blob holds its target, so the tree hashes are equal.
				{name: "exact rename, resource is a symlink", files: merged(app, map[string]string{"p/data.txt": "real.txt", "p/real.txt": "one\n"}), change: func(r *gittest.Repo) {
					r.Git("mv", "p/a.go", "p/b.go")
					r.Git("rm", "-q", "p/data.txt")
					if err := os.Symlink("real.txt", filepath.Join(r.Dir, "p", "data.txt")); err != nil {
						t.Skip(err)
					}
				}, pairs: []string{"p/a.go>p/b.go:p.A"}, refused: []string{"p/a.go>p/b.go:" + refuseDeclEmbedUnproven}},
				{name: "module root moved with an equal resource", files: moved, change: func(r *gittest.Repo) { gitMove(t, r, "libs", "mods") },
					pairs: []string{"libs/p/a.go>mods/p/a.go:p.A", "libs/p/a.go>mods/p/a.go:p.Data"}},
				{name: "module root moved, resource at the new path differs", files: moved, change: func(r *gittest.Repo) {
					gitMove(t, r, "libs", "mods")
					r.Write("mods/p/data.txt", "two\n")
				}, pairs: []string{"libs/p/a.go>mods/p/a.go:p.A"}, refused: []string{"libs/p/a.go>mods/p/a.go:" + refuseDeclEmbedUnproven}},
				{name: "module root moved without its resource", files: moved, change: func(r *gittest.Repo) {
					gitMove(t, r, "libs/go.mod", "mods/go.mod")
					gitMove(t, r, "libs/p/a.go", "mods/p/a.go")
				}, pairs: []string{"libs/p/a.go>mods/p/a.go:p.A"}, refused: []string{"libs/p/a.go>mods/p/a.go:" + refuseDeclEmbedUnproven}},
				{name: "edited move, resource unchanged", files: app, change: func(r *gittest.Repo) {
					r.Git("rm", "-q", "p/a.go")
					r.Write("p/b.go", embedded("data.txt")+"\nfunc C() {}\n")
				}, pairs: []string{"p/a.go>p/b.go:p.A", "p/a.go>p/b.go:p.Data"}},
				{name: "edited move, resource bytes changed", files: app, change: func(r *gittest.Repo) {
					r.Git("rm", "-q", "p/a.go")
					r.Write("p/b.go", embedded("data.txt")+"\nfunc C() {}\n")
					r.Write("p/data.txt", "two\n")
				}, pairs: []string{"p/a.go>p/b.go:p.A"}, refused: []string{"p/a.go>p/b.go:" + refuseDeclEmbedUnproven}},
				{name: "edited move, resource removed", files: app, change: func(r *gittest.Repo) {
					r.Git("rm", "-q", "p/a.go", "p/data.txt")
					r.Write("p/b.go", embedded("data.txt")+"\nfunc C() {}\n")
				}, pairs: []string{"p/a.go>p/b.go:p.A"}, refused: []string{"p/a.go>p/b.go:" + refuseDeclEmbedUnproven}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					d := editScenario(t, adapter, tc.files, tc.change).Diff.DeclarationPairs
					if tc.refused == nil {
						tc.refused = []string{}
					}
					if !reflect.DeepEqual(pairedPaths(d), tc.pairs) || !reflect.DeepEqual(goRefusals(d), tc.refused) {
						t.Fatalf("pairs = %v, refused %v", pairedPaths(d), goRefusals(d))
					}
					for _, r := range d.Refused {
						if r.Reason == refuseDeclEmbedUnproven && (r.Declaration == nil || r.Declaration.Name != "Data") {
							t.Fatalf("embed refusal on %s", canonical(r))
						}
					}
				})
			}
		})
	}
}

func TestCompareCommitsOrderDependentContinuity(t *testing.T) {
	const gomod = "module example.com/m\n\ngo 1.22\n"
	lib := "package p\n\nfunc f() int { return 1 }\n\nvar X = f()\n\nvar Y int\n\nfunc init() {}\n\nfunc A() {}\n"
	tests := "package p\n\nimport \"testing\"\n\nfunc TestA(t *testing.T) {}\n\nfunc BenchmarkA(b *testing.B) {}\n\nfunc FuzzA(f *testing.F) {}\n\nfunc ExampleA() {}\n\nfunc TestMain(m *testing.M) {}\n\nfunc Testing() {}\n"
	app := map[string]string{
		"go.mod":      gomod,
		"p/c.go":      lib,
		"p/m.go":      "package p\n\nvar M = X\n",
		"p/c_test.go": tests,
		"p/m_test.go": "package p\n\nimport \"testing\"\n\nfunc TestM(t *testing.T) {}\n",
		"cmd/main.go": "package main\n\nimport \"example.com/m/p\"\n\nfunc main() { p.A() }\n",
	}
	all := func(from, to string, names ...string) []string {
		out := []string{}
		for _, n := range names {
			out = append(out, from+">"+to+":p."+n)
		}
		return out
	}
	order := func(from, to string, n int) []string {
		out := []string{}
		for i := 0; i < n; i++ {
			out = append(out, from+">"+to+":"+refuseDeclOrderDependent)
		}
		return out
	}
	for name, adapter := range testGoAdapters {
		t.Run(name, func(t *testing.T) {
			for _, tc := range []struct {
				name    string
				change  func(r *gittest.Repo)
				pairs   []string
				refused []string
			}{
				{name: "rename keeps the file order", change: func(r *gittest.Repo) { r.Git("mv", "p/c.go", "p/d.go") },
					pairs: all("p/c.go", "p/d.go", "A", "f", "init", "X", "Y")},
				{name: "rename passes another file", change: func(r *gittest.Repo) { r.Git("mv", "p/c.go", "p/z.go") },
					pairs: all("p/c.go", "p/z.go", "A", "f", "Y"), refused: order("p/c.go", "p/z.go", 2)},
				{name: "rename next to an added file", change: func(r *gittest.Repo) {
					r.Git("mv", "p/c.go", "p/d.go")
					r.Write("p/cc.go", "package p\n\nfunc init() {}\n")
				}, pairs: all("p/c.go", "p/d.go", "A", "f", "Y"), refused: order("p/c.go", "p/d.go", 2)},
				{name: "test rename keeps the file order", change: func(r *gittest.Repo) { r.Git("mv", "p/c_test.go", "p/d_test.go") },
					pairs: all("p/c_test.go", "p/d_test.go", "BenchmarkA", "ExampleA", "FuzzA", "TestA", "TestMain", "Testing")},
				{name: "test rename passes another test file", change: func(r *gittest.Repo) { r.Git("mv", "p/c_test.go", "p/z_test.go") },
					pairs: all("p/c_test.go", "p/z_test.go", "TestMain", "Testing"), refused: order("p/c_test.go", "p/z_test.go", 4)},
				{name: "edited test move", change: func(r *gittest.Repo) {
					r.Git("rm", "-q", "p/c_test.go")
					r.Write("p/d_test.go", tests+"\nfunc helper() {}\n")
				}, pairs: all("p/c_test.go", "p/d_test.go", "TestMain", "Testing"), refused: order("p/c_test.go", "p/d_test.go", 4)},
			} {
				t.Run(tc.name, func(t *testing.T) {
					d := editScenario(t, adapter, app, tc.change).Diff.DeclarationPairs
					if tc.refused == nil {
						tc.refused = []string{}
					}
					if !reflect.DeepEqual(pairedPaths(d), tc.pairs) || !reflect.DeepEqual(goRefusals(d), tc.refused) {
						t.Fatalf("pairs = %v, refused %v", pairedPaths(d), goRefusals(d))
					}
				})
			}
		})
	}
}

func TestOrderDependent(t *testing.T) {
	for _, tc := range []struct {
		path, kind, name string
		want             bool
	}{
		{"p/a.go", "function", "init", true},
		{"p/a.go", "method", "init", false},
		{"p/a.go", "function", "TestA", false},
		{"p/a_test.go", "function", "TestA", true},
		{"p/a_test.go", "function", "Test", true},
		{"p/a_test.go", "function", "Test_a", true},
		{"p/a_test.go", "function", "Testing", false},
		{"p/a_test.go", "function", "TestMain", false},
		{"p/a_test.go", "function", "Example", true},
		{"p/a_test.go", "function", "ExampleT_m", true},
		{"p/a_test.go", "function", "Benchmarks", false},
		{"p/a_test.go", "function", "FuzzX", true},
		{"p/a_test.go", "method", "TestA", false},
	} {
		if got := orderDependent(store.SemanticDeclaration{Path: tc.path, Kind: tc.kind, Name: tc.name}, false); got != tc.want {
			t.Errorf("%s %s %s = %v", tc.path, tc.kind, tc.name, got)
		}
	}
}
