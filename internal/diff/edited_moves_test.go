package diff

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"testing"

	"github.com/isink17/codegraph/internal/githistory/gittest"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/store"
)

// testGoAdapters lists every Go adapter this build has; the cgo build adds
// tree-sitter.
var testGoAdapters = map[string]func() parser.Adapter{"go-ast": func() parser.Adapter { return goparser.New() }}

func sourceOf(t *testing.T, content string) goSource {
	t.Helper()
	sum := sha256.Sum256([]byte(content))
	s, ok := parseGoSource([]byte(content), hex.EncodeToString(sum[:]))
	if !ok {
		t.Fatalf("parse %q", content)
	}
	return s
}

func lines(start, end int) store.SemanticDeclaration {
	return store.SemanticDeclaration{StartLine: start, EndLine: end}
}

func TestGoSourceDeclarationHash(t *testing.T) {
	hashAt := func(t *testing.T, content string, d store.SemanticDeclaration) string {
		top, ok := sourceOf(t, content).declarationHash(d)
		if !ok {
			t.Fatalf("no hash at %d..%d in %q", d.StartLine, d.EndLine, content)
		}
		return top.hash
	}
	t.Run("doc comment and position are not content", func(t *testing.T) {
		a := hashAt(t, "package p\n\nfunc A() {}\n", lines(3, 3))
		b := hashAt(t, "package p\n\nimport ()\n\n// A is documented.\nfunc A() {}\n", lines(6, 6))
		if a != b {
			t.Fatal("doc comment changed the hash")
		}
		if c := hashAt(t, "package p\n\nfunc A() { println() }\n", lines(3, 3)); c == a {
			t.Fatal("body change kept the hash")
		}
	})
	t.Run("directive moved to another declaration", func(t *testing.T) {
		base := "package p\n\n//go:noinline\nfunc A() {}\n\nfunc B() {}\n"
		head := "package p\n\nfunc A() {}\n\n//go:noinline\nfunc B() {}\n"
		if hashAt(t, base, lines(4, 4)) == hashAt(t, head, lines(3, 3)) || hashAt(t, base, lines(6, 6)) == hashAt(t, head, lines(6, 6)) {
			t.Fatal("directive move kept a hash")
		}
	})
	t.Run("context", func(t *testing.T) {
		ctx := func(content string) string { return sourceOf(t, content).context }
		plain := ctx("package p\n\nimport (\n\t\"a\"\n\t\"b\"\n)\n")
		if ctx("package p\n\nimport (\n\t\"b\"\n\t\"a\"\n)\n") != plain {
			t.Fatal("import order changed the context")
		}
		for _, other := range []string{
			"package p\n\nimport (\n\tx \"a\"\n\t\"b\"\n)\n",
			"package q\n\nimport (\n\t\"a\"\n\t\"b\"\n)\n",
			"package p\n\nimport (\n\t\"a\"\n\t\"b\"\n)\n\nfunc F() {}\n\n//go:linkname x runtime.x\n",
		} {
			if ctx(other) == plain {
				t.Fatalf("context of %q equals %q", other, plain)
			}
		}
		for _, pair := range [][2]string{
			{"package p\n\n// #include <a.h>\nimport \"C\"\n", "package p\n\n// #include <b.h>\nimport \"C\"\n"},
			{"package p\n\nimport (\n\t// #define X 1\n\t\"C\"\n)\n", "package p\n\nimport (\n\t// #define X 2\n\t\"C\"\n)\n"},
			{"package p\n\n//go:linkname x runtime.x\nimport \"unsafe\"\n\nvar _ unsafe.Pointer\n", "package p\n\nimport \"unsafe\"\n\nvar _ unsafe.Pointer\n"},
			{"package p\n\n//go:linkname x runtime.y\nfunc B() {}\n\nfunc A() {}\n", "package p\n\nfunc B() {}\n\nfunc A() {}\n"},
		} {
			if ctx(pair[0]) == ctx(pair[1]) {
				t.Fatalf("context of %q equals %q", pair[0], pair[1])
			}
		}
	})
	t.Run("var initializers", func(t *testing.T) {
		src := sourceOf(t, "package p\n\nvar A = f()\n\nvar B int\n\nconst C = 1\n\nfunc f() int { return 1 }\n")
		var got []bool
		for _, l := range []int{3, 5, 7, 9} {
			top, ok := src.declarationHash(lines(l, l))
			if !ok {
				t.Fatalf("line %d", l)
			}
			got = append(got, top.initializes)
		}
		if want := []bool{true, false, false, false}; !reflect.DeepEqual(got, want) {
			t.Fatalf("initializes = %v, want %v", got, want)
		}
	})
	t.Run("grouped constants hash their whole group", func(t *testing.T) {
		base := "package p\n\nconst (\n\tX = iota\n\tY\n)\n"
		head := "package p\n\nconst (\n\tW = iota\n\tX\n\tY\n)\n"
		if hashAt(t, base, lines(5, 5)) == hashAt(t, head, lines(6, 6)) {
			t.Fatal("iota position change kept the hash")
		}
	})
	t.Run("unavailable", func(t *testing.T) {
		if _, ok := sourceOf(t, "package p\n\nfunc A() {}; func B() {}\n").declarationHash(lines(3, 3)); ok {
			t.Fatal("two declarations on one line hashed")
		}
		if _, ok := sourceOf(t, "package p\n\nfunc A() {}\n").declarationHash(lines(1, 1)); ok {
			t.Fatal("declaration outside every top-level declaration hashed")
		}
		if _, ok := parseGoSource([]byte("package p\n"), "other"); ok {
			t.Fatal("bytes other than the indexed ones parsed")
		}
		if _, ok := parseGoSource([]byte("package p\nfunc {"), func() string { s := sha256.Sum256([]byte("package p\nfunc {")); return hex.EncodeToString(s[:]) }()); ok {
			t.Fatal("invalid file parsed")
		}
	})
}

func editScenario(t *testing.T, adapter func() parser.Adapter, files map[string]string, change func(r *gittest.Repo)) Result {
	t.Helper()
	r := gittest.Init(t)
	for p, c := range files {
		r.Write(p, c)
	}
	base := r.Commit("", "base")
	change(r)
	head := r.Commit("", "change")
	got, err := CompareCommits(t.Context(), r.Dir, base, head, parser.NewRegistry(adapter()), 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	again, err := CompareCommits(t.Context(), r.Dir, base, head, parser.NewRegistry(adapter()), 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	if !sameJSON(got, again) {
		t.Fatal("repeated diff differs")
	}
	return got
}

func TestCompareCommitsEditedMoves(t *testing.T) {
	const gomod = "module example.com/m\n\ngo 1.22\n"
	lib := "package p\n\nimport \"strings\"\n\nfunc A() string { return strings.ToUpper(\"a\") }\n\nfunc B() int { return 1 }\n"
	app := map[string]string{
		"go.mod":      gomod,
		"p/a.go":      lib,
		"cmd/main.go": "package main\n\nimport \"example.com/m/p\"\n\nfunc main() { p.A(); _ = p.B() }\n",
	}
	with := func(base map[string]string, extra map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range base {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	for name, adapter := range testGoAdapters {
		t.Run(name, func(t *testing.T) {
			t.Run("same-directory rename with an added declaration", func(t *testing.T) {
				got := editScenario(t, adapter, app, func(r *gittest.Repo) {
					r.Git("rm", "-q", "p/a.go")
					r.Write("p/b.go", lib+"\nfunc C() {}\n")
				})
				d := got.Diff.DeclarationPairs
				if want := []string{"p/a.go>p/b.go:p.A", "p/a.go>p/b.go:p.B"}; !reflect.DeepEqual(pairedPaths(d), want) || len(goRefusals(d)) != 0 {
					t.Fatalf("pairs = %v, refused %v", pairedPaths(d), goRefusals(d))
				}
				if got.Diff.Risk.Score == nil || *got.Diff.Risk.Score != 0 || ruleCount(got.Diff.Risk, RuleDependentRetargeted) != 0 {
					t.Fatalf("risk = %s", canonical(got.Diff.Risk))
				}
				for _, u := range got.Diff.Risk.Uncertainty {
					if u.Reason == "removed_declaration_without_resolved_dependents" {
						t.Fatalf("paired declaration left uncertainty: %s", canonical(u))
					}
				}
				// Pairs and risk are computed over the whole change set.
				page, err := Compare(got.From, got.To, 0, 1)
				if err != nil {
					t.Fatal(err)
				}
				if !sameJSON(page.Diff.DeclarationPairs, d) || !sameJSON(page.Diff.Risk, got.Diff.Risk) {
					t.Fatal("first page differs")
				}
			})
			t.Run("file split in two", func(t *testing.T) {
				got := editScenario(t, adapter, app, func(r *gittest.Repo) {
					r.Git("rm", "-q", "p/a.go")
					r.Write("p/x.go", "package p\n\nimport \"strings\"\n\nfunc A() string { return strings.ToUpper(\"a\") }\n")
					r.Write("p/y.go", "package p\n\nimport \"strings\"\n\nfunc B() int { return 1 }\n")
				})
				if want := []string{"p/a.go>p/x.go:p.A", "p/a.go>p/y.go:p.B"}; !reflect.DeepEqual(pairedPaths(got.Diff.DeclarationPairs), want) {
					t.Fatalf("pairs = %v, refused %v", pairedPaths(got.Diff.DeclarationPairs), goRefusals(got.Diff.DeclarationPairs))
				}
			})
			t.Run("body changed", func(t *testing.T) {
				got := editScenario(t, adapter, app, func(r *gittest.Repo) {
					r.Git("rm", "-q", "p/a.go")
					r.Write("p/b.go", "package p\n\nimport \"strings\"\n\nfunc A() string { return strings.ToUpper(\"a\") }\n\nfunc B() int { return 2 }\n")
				})
				d := got.Diff.DeclarationPairs
				if want := []string{"p/a.go>p/b.go:p.A"}; !reflect.DeepEqual(pairedPaths(d), want) {
					t.Fatalf("pairs = %v", pairedPaths(d))
				}
				if want := []string{"p/a.go>p/b.go:" + refuseDeclContentChanged}; !reflect.DeepEqual(goRefusals(d), want) || d.Refused[0].Declaration.QualifiedName != "p.B" {
					t.Fatalf("refused = %s", canonical(d.Refused))
				}
				// The unpaired declaration's caller is cited as before.
				if ruleCount(got.Diff.Risk, RuleDependentRetargeted) != 1 {
					t.Fatalf("risk = %s", canonical(got.Diff.Risk))
				}
			})
			t.Run("import rebound", func(t *testing.T) {
				got := editScenario(t, adapter, with(app, map[string]string{"strings/s.go": "package strings\n\nfunc ToUpper(s string) string { return s }\n"}), func(r *gittest.Repo) {
					r.Git("rm", "-q", "p/a.go")
					r.Write("p/b.go", "package p\n\nimport \"example.com/m/strings\"\n\nfunc A() string { return strings.ToUpper(\"a\") }\n\nfunc B() int { return 1 }\n")
				})
				d := got.Diff.DeclarationPairs
				// B spells no name the imports bind, so only A is refused.
				if want := []string{"p/a.go>p/b.go:p.B"}; !reflect.DeepEqual(pairedPaths(d), want) || !reflect.DeepEqual(goRefusals(d), []string{"p/a.go>p/b.go:" + refuseDeclContextChanged}) || d.Refused[0].Declaration.QualifiedName != "p.A" {
					t.Fatalf("declaration pairs = %s", canonical(d))
				}
			})
			t.Run("moved to another package", func(t *testing.T) {
				got := editScenario(t, adapter, app, func(r *gittest.Repo) {
					r.Git("rm", "-q", "p/a.go")
					r.Write("q/a.go", "package p\n\nimport \"strings\"\n\nfunc A() string { return strings.ToUpper(\"a\") }\n\nfunc B() int { return 1 }\n\nfunc C() {}\n")
				})
				if d := got.Diff.DeclarationPairs; len(d.Pairs) != 0 || len(goRefusals(d)) != 0 {
					t.Fatalf("declaration pairs = %s", canonical(d))
				}
			})
			t.Run("into the test role", func(t *testing.T) {
				got := editScenario(t, adapter, app, func(r *gittest.Repo) {
					r.Git("rm", "-q", "p/a.go")
					r.Write("p/a_test.go", lib+"\nfunc C() {}\n")
				})
				if d := got.Diff.DeclarationPairs; len(d.Pairs) != 0 {
					t.Fatalf("declaration pairs = %s", canonical(d))
				}
			})
			t.Run("go.mod edited", func(t *testing.T) {
				got := editScenario(t, adapter, app, func(r *gittest.Repo) {
					r.Git("rm", "-q", "p/a.go")
					r.Write("p/b.go", lib+"\nfunc C() {}\n")
					r.Write("go.mod", gomod+"\nrequire example.com/x v1.0.0\n")
				})
				d := got.Diff.DeclarationPairs
				if len(d.Pairs) != 0 || !reflect.DeepEqual(goRefusals(d), []string{"p/a.go>p/b.go:" + refuseModuleRootChanged, "p/a.go>p/b.go:" + refuseModuleRootChanged}) {
					t.Fatalf("declaration pairs = %s", canonical(d))
				}
			})
			t.Run("duplicate shape in a kept file", func(t *testing.T) {
				got := editScenario(t, adapter, with(app, map[string]string{"p/i.go": "package p\n\nfunc D() {}\n", "p/a.go": lib + "\nfunc D() {}\n"}), func(r *gittest.Repo) {
					r.Git("rm", "-q", "p/a.go")
					r.Write("p/b.go", lib+"\nfunc D() {}\n\nfunc C() {}\n")
				})
				d := got.Diff.DeclarationPairs
				if want := []string{"p/a.go>p/b.go:p.A", "p/a.go>p/b.go:p.B"}; !reflect.DeepEqual(pairedPaths(d), want) || !reflect.DeepEqual(goRefusals(d), []string{"p/a.go>p/b.go:" + refuseDeclarationNotUnique}) {
					t.Fatalf("pairs = %v, refused %v", pairedPaths(d), goRefusals(d))
				}
			})
			// An init elsewhere under another build constraint or in a test
			// file can run before or after the moved one.
			for _, sibling := range []string{"p/m_linux.go", "p/m_test.go"} {
				t.Run("init with a sibling in "+sibling, func(t *testing.T) {
					got := editScenario(t, adapter, with(app, map[string]string{sibling: "package p\n\nfunc init() {}\n", "p/a.go": lib + "\nfunc init() {}\n"}), func(r *gittest.Repo) {
						r.Git("rm", "-q", "p/a.go")
						r.Write("p/z.go", lib+"\nfunc init() {}\n\nfunc C() {}\n")
					})
					d := got.Diff.DeclarationPairs
					if want := []string{"p/a.go>p/z.go:p.A", "p/a.go>p/z.go:p.B"}; !reflect.DeepEqual(pairedPaths(d), want) || !reflect.DeepEqual(goRefusals(d), []string{"p/a.go>p/z.go:" + refuseDeclOrderDependent}) {
						t.Fatalf("pairs = %v, refused %v", pairedPaths(d), goRefusals(d))
					}
				})
			}
			t.Run("package variables", func(t *testing.T) {
				vars := "package p\n\nvar X = 1\n\nvar Y int\n"
				got := editScenario(t, adapter, with(app, map[string]string{"p/v.go": vars, "p/w.go": "package p\n\nvar W = X + 1\n"}), func(r *gittest.Repo) {
					r.Git("rm", "-q", "p/v.go")
					r.Write("p/zz.go", vars+"\nfunc C() {}\n")
				})
				d := got.Diff.DeclarationPairs
				if want := []string{"p/v.go>p/zz.go:p.Y"}; !reflect.DeepEqual(pairedPaths(d), want) || !reflect.DeepEqual(goRefusals(d), []string{"p/v.go>p/zz.go:" + refuseDeclOrderDependent}) {
					t.Fatalf("pairs = %v, refused %v", pairedPaths(d), goRefusals(d))
				}
			})
			t.Run("grouped cgo preamble edited", func(t *testing.T) {
				cgo := func(v string) string {
					return "package p\n\nimport (\n\t// #define X " + v + "\n\t\"C\"\n)\n\nfunc F() int { return int(C.X) }\n"
				}
				got := editScenario(t, adapter, with(app, map[string]string{"p/c.go": cgo("1")}), func(r *gittest.Repo) {
					r.Git("rm", "-q", "p/c.go")
					r.Write("p/d.go", cgo("2"))
				})
				if d := got.Diff.DeclarationPairs; len(d.Pairs) != 0 || !reflect.DeepEqual(goRefusals(d), []string{"p/c.go>p/d.go:" + refuseDeclContextChanged}) {
					t.Fatalf("declaration pairs = %s", canonical(d))
				}
			})
			for name, base := range map[string]string{
				"linkname before the imports":      "package p\n\n//go:linkname v runtime.v\nimport _ \"unsafe\"\n\nvar v int\n\nfunc F() int { return v }\n",
				"linkname before another function": "package p\n\nimport _ \"unsafe\"\n\n//go:linkname v runtime.v\nfunc G() {}\n\nvar v int\n\nfunc F() int { return v }\n",
			} {
				t.Run(name+" dropped", func(t *testing.T) {
					got := editScenario(t, adapter, with(app, map[string]string{"p/l.go": base}), func(r *gittest.Repo) {
						r.Git("rm", "-q", "p/l.go")
						r.Write("p/m.go", "package p\n\nimport _ \"unsafe\"\n\nfunc G() {}\n\nvar v int\n\nfunc F() int { return v }\n")
					})
					d := got.Diff.DeclarationPairs
					for _, p := range d.Pairs {
						if p.FromPath == "p/l.go" {
							t.Fatalf("paired %s", canonical(p))
						}
					}
					if len(goRefusals(d)) == 0 {
						t.Fatalf("declaration pairs = %s", canonical(d))
					}
				})
			}
		})
	}
}

func TestCompareCommitsScopedWorkspaceAndReplace(t *testing.T) {
	split := map[string]string{
		"go.mod":      "module example.com/app\n\ngo 1.22\n",
		"main.go":     "package main\n\nimport \"example.com/lib/p\"\n\nfunc main() { p.A() }\n",
		"libs/go.mod": "module example.com/lib\n\ngo 1.22\n",
		"libs/p/a.go": "package p\n\nfunc A() {}\n",
		"tool/go.mod": "module example.com/tool\n\ngo 1.22\n",
	}
	with := func(extra map[string]string) map[string]string {
		out := map[string]string{}
		for k, v := range split {
			out[k] = v
		}
		for k, v := range extra {
			out[k] = v
		}
		return out
	}
	moveLib := func(r *gittest.Repo) { gitMove(t, r, "libs", "lib") }
	for _, tc := range []struct {
		name   string
		files  map[string]string
		change func(r *gittest.Repo)
		want   string // refusal reason, or "" for a pair
	}{
		{name: "go.work uses other modules only", files: with(map[string]string{"go.work": "go 1.22\n\nuse (\n\t.\n\t./tool\n)\n"}), change: moveLib},
		{name: "go.work single-line use of another module", files: with(map[string]string{"go.work": "go 1.22\n\nuse ./tool\n"}), change: moveLib},
		{name: "go.work uses the moved module", files: with(map[string]string{"go.work": "go 1.22\n\nuse ./libs\n"}), change: func(r *gittest.Repo) {
			moveLib(r)
			r.Write("go.work", "go 1.22\n\nuse ./lib\n")
		}, want: refuseWorkspaceUncertain},
		{name: "go.work starts using the moved module", files: with(map[string]string{"go.work": "go 1.22\n\nuse .\n"}), change: func(r *gittest.Repo) {
			moveLib(r)
			r.Write("go.work", "go 1.22\n\nuse (\n\t.\n\t./lib\n)\n")
		}, want: refuseWorkspaceUncertain},
		{name: "go.work in a nested directory uses it", files: with(map[string]string{"libs/go.work": "go 1.22\n\nuse .\n"}), change: moveLib, want: refuseWorkspaceUncertain},
		{name: "go.work does not parse", files: with(map[string]string{"go.work": "use (\n"}), change: moveLib, want: refuseWorkspaceUncertain},
		{name: "go.work replaces by the moved directory", files: with(map[string]string{"go.work": "go 1.22\n\nuse .\n\nreplace example.com/x => ./libs\n"}), change: func(r *gittest.Repo) {
			moveLib(r)
			r.Write("go.work", "go 1.22\n\nuse .\n\nreplace example.com/x => ./lib\n")
		}, want: refuseModuleReplaceInRepo},
		{name: "another module replaces by another directory", files: with(map[string]string{"tool/go.mod": "module example.com/tool\n\ngo 1.22\n\nreplace example.com/app => ../\n"}), change: moveLib},
		{name: "the moved module is a replace target", files: with(map[string]string{"go.mod": "module example.com/app\n\ngo 1.22\n\nreplace example.com/lib => ./libs\n"}), change: func(r *gittest.Repo) {
			moveLib(r)
			r.Write("go.mod", "module example.com/app\n\ngo 1.22\n\nreplace example.com/lib => ./lib\n")
		}, want: refuseModuleReplaceInRepo},
		{name: "the moved module declares a replace", files: with(map[string]string{"libs/go.mod": "module example.com/lib\n\ngo 1.22\n\nreplace example.com/x => ../tool\n"}), change: moveLib, want: refuseModuleReplaceInRepo},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := moveScenario(t, tc.files, tc.change)
			d := got.Diff.DeclarationPairs
			if tc.want == "" {
				if want := []string{"libs/p/a.go>lib/p/a.go:p.A"}; !reflect.DeepEqual(pairedPaths(d), want) {
					t.Fatalf("pairs = %v, refused %v", pairedPaths(d), goRefusals(d))
				}
				if ruleCount(got.Diff.Risk, RuleDependentRetargeted) != 0 {
					t.Fatalf("paired move cited: %s", canonical(got.Diff.Risk))
				}
				return
			}
			if want := []string{"libs/p/a.go>lib/p/a.go:" + tc.want}; len(d.Pairs) != 0 || !reflect.DeepEqual(goRefusals(d), want) {
				t.Fatalf("pairs = %v, refused %v, want %v", pairedPaths(d), goRefusals(d), want)
			}
		})
	}
}

func TestGoModuleScope(t *testing.T) {
	files := map[string]string{}
	var tree []SourceFile
	add := func(p, content string) {
		sum := sha256.Sum256([]byte(content))
		files[p] = content
		tree = append(tree, SourceFile{Path: p, ContentHash: hex.EncodeToString(sum[:])})
	}
	source := func(p string) ([]byte, bool) {
		c, ok := files[p]
		return []byte(c), ok
	}
	add("go.work", "go 1.22\n\nuse ./a\nuse ../outside\n")
	add("b/go.mod", "module example.com/b\n\nreplace example.com/c => ../c\n")
	modules := &store.SemanticGoModules{
		Modules:    []store.SemanticGoModule{{Root: "a", Path: "example.com/a"}, {Root: "b", Path: "example.com/b", LocalReplace: true}, {Root: "c", Path: "example.com/c"}, {Root: "d", Path: "example.com/d"}},
		Workspaces: []string{"."},
	}
	s := newGoModuleScope(modules, tree, source)
	got := map[string]string{}
	for _, m := range modules.Modules {
		got[m.Root] = s.refusal(m.Root)
	}
	if want := map[string]string{"a": refuseWorkspaceUncertain, "b": refuseModuleReplaceInRepo, "c": refuseModuleReplaceInRepo, "d": ""}; !reflect.DeepEqual(got, want) {
		t.Fatalf("refusals = %v, want %v", got, want)
	}
	for name, change := range map[string]func(){
		"absolute use":     func() { files["go.work"] = "go 1.22\n\nuse /abs\n" },
		"go.work unread":   func() { delete(files, "go.work") },
		"go.work modified": func() { files["go.work"] += "\n" },
	} {
		saved := files["go.work"]
		change()
		if r := newGoModuleScope(modules, tree, source).refusal("d"); r != refuseWorkspaceUncertain {
			t.Fatalf("%s: refusal = %q", name, r)
		}
		files["go.work"] = saved
	}
	if r := newGoModuleScope(modules, tree, nil).refusal("d"); r != refuseWorkspaceUncertain {
		t.Fatalf("no source: refusal = %q", r)
	}
	files["b/go.mod"] = "module example.com/b\n\nreplace example.com/c => /abs/c\n"
	tree[1].ContentHash = func() string { s := sha256.Sum256([]byte(files["b/go.mod"])); return hex.EncodeToString(s[:]) }()
	if r := newGoModuleScope(modules, tree, source).refusal("d"); r != refuseModuleReplaceInRepo {
		t.Fatalf("absolute replace: refusal = %q", r)
	}
}

func TestCompareSnapshotsCancelledWhileReadingBlobs(t *testing.T) {
	r := gittest.Init(t)
	r.Write("go.mod", "module example.com/m\n")
	r.Write("p/a.go", "package p\n\nfunc A() {}\n")
	base := r.Commit("", "base")
	r.Git("rm", "-q", "p/a.go")
	r.Write("p/b.go", "package p\n\nfunc A() {}\n\nfunc B() {}\n")
	head := r.Commit("", "change")
	ctx, cancel := context.WithCancel(t.Context())
	registry := parser.NewRegistry(goparser.New())
	from, err := snapshot(ctx, r.Dir, base, registry)
	if err != nil {
		t.Fatal(err)
	}
	defer from.blobs.close()
	to, err := snapshot(ctx, r.Dir, head, registry)
	if err != nil {
		t.Fatal(err)
	}
	defer to.blobs.close()
	cancel()
	if _, err := compareSnapshots(ctx, from, to, 0, 100); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}

func TestCompareCommitsMovesBetweenKeptFiles(t *testing.T) {
	const (
		fnA     = "func A() string { return strings.ToUpper(\"a\") }\n"
		fnB     = "func B() int { return 1 }\n"
		fnC     = "func C() string { return strings.ToLower(\"c\") }\n"
		strs    = "package p\n\nimport \"strings\"\n\n"
		initFn  = "func init() {}\n"
		changed = "func A() string { return strings.ToUpper(\"A\") }\n"
	)
	app := map[string]string{
		"go.mod":      "module example.com/m\n\ngo 1.22\n",
		"p/a.go":      strs + fnA + "\n" + fnB + "\n" + initFn,
		"p/b.go":      strs + fnC,
		"cmd/main.go": "package main\n\nimport \"example.com/m/p\"\n\nfunc main() { _ = p.A(); _ = p.B(); _ = p.C() }\n",
	}
	for name, adapter := range testGoAdapters {
		t.Run(name, func(t *testing.T) {
			for _, tc := range []struct {
				name      string
				change    func(r *gittest.Repo)
				pairs     []string
				refused   []string
				retargets int
			}{
				{name: "cut and pasted between kept files", change: func(r *gittest.Repo) {
					r.Write("p/a.go", "package p\n\n"+fnB+"\n"+initFn)
					r.Write("p/b.go", strs+fnC+"\n"+fnA)
				}, pairs: []string{"p/a.go>p/b.go:p.A"}},
				{name: "removed file merged into a kept file", change: func(r *gittest.Repo) {
					r.Git("rm", "-q", "p/a.go")
					r.Write("p/b.go", strs+fnA+"\n"+fnB+"\n"+fnC+"\n"+initFn)
				}, pairs: []string{"p/a.go>p/b.go:p.A", "p/a.go>p/b.go:p.B"}, refused: []string{"p/a.go>p/b.go:" + refuseDeclOrderDependent}},
				{name: "kept file split into an added file", change: func(r *gittest.Repo) {
					r.Write("p/a.go", "package p\n\n"+fnB+"\n"+initFn)
					r.Write("p/c.go", strs+fnA)
				}, pairs: []string{"p/a.go>p/c.go:p.A"}},
				{name: "moved and edited", change: func(r *gittest.Repo) {
					r.Write("p/a.go", "package p\n\n"+fnB+"\n"+initFn)
					r.Write("p/b.go", strs+fnC+"\n"+changed)
				}, pairs: []string{}, refused: []string{"p/a.go>p/b.go:" + refuseDeclContentChanged}, retargets: 1},
				{name: "moved into a file with other imports", change: func(r *gittest.Repo) {
					r.Write("p/a.go", "package p\n\n"+fnB+"\n"+initFn)
					r.Write("p/b.go", "package p\n\nimport (\n\t\"fmt\"\n\t\"strings\"\n)\n\n"+fnC+"\n"+fnA+"\nvar _ = fmt.Sprint\n")
				}, pairs: []string{"p/a.go>p/b.go:p.A"}},
				{name: "init moved", change: func(r *gittest.Repo) {
					r.Write("p/a.go", strs+fnA+"\n"+fnB)
					r.Write("p/b.go", strs+fnC+"\n"+initFn)
				}, pairs: []string{}, refused: []string{"p/a.go>p/b.go:" + refuseDeclOrderDependent}},
				{name: "moved to another package", change: func(r *gittest.Repo) {
					r.Write("p/a.go", "package p\n\n"+fnB+"\n"+initFn)
					r.Write("q/b.go", strs+fnA)
				}, pairs: []string{}},
				{name: "edited in place", change: func(r *gittest.Repo) {
					r.Write("p/a.go", strs+changed+"\n"+fnB+"\n"+initFn)
					r.Write("p/b.go", strs+fnC+"\nfunc D() {}\n")
				}, pairs: []string{}},
			} {
				t.Run(tc.name, func(t *testing.T) {
					got := editScenario(t, adapter, app, tc.change)
					d := got.Diff.DeclarationPairs
					if tc.refused == nil {
						tc.refused = []string{}
					}
					if !reflect.DeepEqual(pairedPaths(d), tc.pairs) || !reflect.DeepEqual(goRefusals(d), tc.refused) {
						t.Fatalf("pairs = %v, refused %v", pairedPaths(d), goRefusals(d))
					}
					if n := ruleCount(got.Diff.Risk, RuleDependentRetargeted); n != tc.retargets {
						t.Fatalf("retargeted = %d, risk = %s", n, canonical(got.Diff.Risk))
					}
					if len(tc.refused) == 0 && len(tc.pairs) > 0 && (got.Diff.Risk.Score == nil || *got.Diff.Risk.Score != 0) {
						t.Fatalf("proven move scored: %s", canonical(got.Diff.Risk))
					}
					paired := map[string]bool{}
					for _, p := range d.Pairs {
						paired[declIdentity(p.Before)] = true
					}
					for _, u := range got.Diff.Risk.Uncertainty {
						if paired[u.Identity] {
							t.Fatalf("paired declaration left uncertainty: %s", canonical(u))
						}
					}
				})
			}
		})
	}
}
