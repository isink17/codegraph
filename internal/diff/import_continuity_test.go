package diff

import (
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/githistory/gittest"
)

func TestCompareCommitsImportAwareMoves(t *testing.T) {
	const fnB = "func B() int { return 1 }\n"
	pkg := func(imports ...string) string {
		if len(imports) == 0 {
			return "package p\n\n"
		}
		return "package p\n\nimport (\n\t" + strings.Join(imports, "\n\t") + "\n)\n\n"
	}
	upper := "func A() string { return strings.ToUpper(\"a\") }\n"
	for _, tc := range []struct {
		name string
		// fn moves from p/a.go (imports from) to p/b.go (imports to).
		fn, from, to []string
		// header replaces the package clause line of the head file; tail ends it.
		header  string
		tail    string
		extra   map[string]string
		gomod   string
		toPath  string
		pairs   []string
		refused []string
	}{
		{name: "unrelated imports added", fn: []string{upper},
			from: []string{`"strings"`}, to: []string{`"fmt"`, `"strings"`, `"example.com/m/x/util"`, `_ "embed"`},
			pairs: []string{"p/a.go>p/b.go:p.A"}},
		{name: "unrelated imports dropped", fn: []string{upper},
			from: []string{`"fmt"`, `"strings"`, `y "gopkg.in/yaml.v3"`}, to: []string{`"strings"`},
			pairs: []string{"p/a.go>p/b.go:p.A"}},
		{name: "explicit alias on both sides", fn: []string{"func A() ([]byte, error) { return y.Marshal(1) }\n"},
			from: []string{`y "gopkg.in/yaml.v3"`}, to: []string{`"fmt"`, `y "gopkg.in/yaml.v3"`},
			pairs: []string{"p/a.go>p/b.go:p.A"}},
		{name: "same name bound to another path", fn: []string{"func A() int { return util.F() }\n"},
			from: []string{`"example.com/m/x/util"`}, to: []string{`"example.com/m/y/util"`},
			refused: []string{"p/a.go>p/b.go:" + refuseDeclContextChanged}},
		{name: "same alias bound to another package", fn: []string{"func A() int { return u.F() }\n"},
			from: []string{`u "example.com/m/x/util"`}, to: []string{`u "example.com/m/y/util"`},
			refused: []string{"p/a.go>p/b.go:" + refuseDeclContextChanged}},
		{name: "alias dropped", fn: []string{"func A() string { return str.ToUpper(\"a\") }\n"},
			from: []string{`str "strings"`}, to: []string{`"strings"`},
			refused: []string{"p/a.go>p/b.go:" + refuseDeclContextChanged}},
		{name: "standard library import rebound to an own package", fn: []string{upper},
			from: []string{`"strings"`}, to: []string{`"example.com/m/strings"`},
			refused: []string{"p/a.go>p/b.go:" + refuseDeclContextChanged}},
		{name: "added third-party default name unknown", fn: []string{upper},
			from: []string{`"strings"`}, to: []string{`"strings"`, `"gopkg.in/yaml.v3"`},
			refused: []string{"p/a.go>p/b.go:" + refuseDeclContextChanged}},
		{name: "added hyphenated path", fn: []string{upper},
			from: []string{`"strings"`}, to: []string{`"strings"`, `"github.com/x/go-thing"`},
			refused: []string{"p/a.go>p/b.go:" + refuseDeclContextChanged}},
		{name: "added major version path", fn: []string{upper},
			from: []string{`"strings"`}, to: []string{`"strings"`, `"math/rand/v2"`},
			refused: []string{"p/a.go>p/b.go:" + refuseDeclContextChanged}},
		{name: "dotless path a required module provides", fn: []string{upper},
			gomod: "require tools v1.0.0\n",
			from:  []string{`"strings"`}, to: []string{`"strings"`, `"tools/fmt"`},
			refused: []string{"p/a.go>p/b.go:" + refuseDeclContextChanged}},
		{name: "own package named apart from its directory proven", fn: []string{upper},
			from: []string{`"strings"`}, to: []string{`"strings"`, `"example.com/m/w"`},
			pairs: []string{"p/a.go>p/b.go:p.A"}},
		{name: "own package named apart from its directory spelled", fn: []string{"func A() int { other := 1; return other }\n"},
			to:      []string{`"example.com/m/w"`},
			refused: []string{"p/a.go>p/b.go:" + refuseDeclContextChanged}},
		{name: "own package with two package clauses", fn: []string{upper},
			extra: map[string]string{"w/gen.go": "//go:build ignore\n\npackage main\n"},
			from:  []string{`"strings"`}, to: []string{`"strings"`, `"example.com/m/w"`},
			refused: []string{"p/a.go>p/b.go:" + refuseDeclContextChanged}},
		{name: "dot import added", fn: []string{upper},
			from: []string{`"strings"`}, to: []string{`"strings"`, `. "fmt"`},
			refused: []string{"p/a.go>p/b.go:" + refuseDeclContextChanged}},
		{name: "dot import on both sides", fn: []string{upper},
			from: []string{`"strings"`, `. "fmt"`}, to: []string{`"strings"`, `. "fmt"`, `"os"`},
			refused: []string{"p/a.go>p/b.go:" + refuseDeclContextChanged}},
		{name: "added import shadowed by a parameter", fn: []string{"func A(fmt string) string { return fmt }\n"},
			to:      []string{`"fmt"`},
			refused: []string{"p/a.go>p/b.go:" + refuseDeclContextChanged}},
		{name: "build constraint differs", fn: []string{upper},
			header: "//go:build linux\n\npackage p",
			from:   []string{`"strings"`}, to: []string{`"fmt"`, `"strings"`},
			pairs: []string{}},
		{name: "cgo import added", fn: []string{upper},
			from: []string{`"strings"`}, to: []string{`"strings"`, `"C"`},
			refused: []string{"p/a.go>p/b.go:" + refuseDeclContextChanged}},
		{name: "linkname in the destination", fn: []string{upper},
			from: []string{`"strings"`}, to: []string{`"strings"`, `_ "unsafe"`},
			tail:    "\n//go:linkname nanotime runtime.nanotime\nfunc nanotime() int64\n",
			refused: []string{"p/a.go>p/b.go:" + refuseDeclContextChanged}},
		{name: "embed", fn: []string{"//go:embed data.txt\nvar A string\n"},
			extra: map[string]string{"p/data.txt": "x"},
			from:  []string{`_ "embed"`}, to: []string{`_ "embed"`, `"fmt"`},
			refused: []string{"p/a.go>p/b.go:" + refuseDeclContextChanged}},
		{name: "initialized variable", fn: []string{"var A = strings.ToUpper(\"a\")\n"},
			from: []string{`"strings"`}, to: []string{`"fmt"`, `"strings"`},
			refused: []string{"p/a.go>p/b.go:" + refuseDeclOrderDependent}},
		{name: "moved to another package", fn: []string{upper},
			from: []string{`"strings"`}, to: []string{`"fmt"`, `"strings"`}, toPath: "q/b.go",
			pairs: []string{}},
		{name: "ambiguous candidate", fn: []string{upper},
			extra: map[string]string{"p/i.go": "package p\n\nimport \"strings\"\n\n" + upper},
			from:  []string{`"strings"`}, to: []string{`"fmt"`, `"strings"`},
			refused: []string{"p/a.go>p/b.go:" + refuseDeclarationNotUnique}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{
				"go.mod":           "module example.com/m\n\ngo 1.22\n" + tc.gomod,
				"x/util/u.go":      "package util\n\nfunc F() int { return 1 }\n",
				"y/util/u.go":      "package util\n\nfunc F() int { return 2 }\n",
				"w/w.go":           "package other\n\nfunc F() int { return 3 }\n",
				"strings/s.go":     "package strings\n\nfunc ToUpper(s string) string { return s }\n",
				"cmd/main/main.go": "package main\n\nimport \"example.com/m/p\"\n\nfunc main() { _ = p.A; _ = p.B() }\n",
				"p/a.go":           pkg(tc.from...) + strings.Join(tc.fn, "") + "\n" + fnB,
				"p/b.go":           pkg(tc.to...) + "func C() {}\n",
			}
			for k, v := range tc.extra {
				files[k] = v
			}
			toPath := tc.toPath
			if toPath == "" {
				toPath = "p/b.go"
			}
			header := func(s string) string {
				if tc.header == "" {
					return s
				}
				return strings.Replace(s, "package p\n", tc.header+"\n", 1)
			}
			if tc.refused == nil {
				tc.refused = []string{}
			}
			if tc.pairs == nil {
				tc.pairs = []string{}
			}
			for name, adapter := range testGoAdapters {
				got := editScenario(t, adapter, files, func(r *gittest.Repo) {
					r.Write("p/a.go", pkg(tc.from...)+fnB)
					r.Write(toPath, header(pkg(tc.to...)+"func C() {}\n\n"+strings.Join(tc.fn, "")+tc.tail))
				})
				d := got.Diff.DeclarationPairs
				if !reflect.DeepEqual(pairedPaths(d), tc.pairs) || !reflect.DeepEqual(goRefusals(d), tc.refused) {
					t.Fatalf("%s: pairs = %v, refused %v", name, pairedPaths(d), goRefusals(d))
				}
				if len(tc.pairs) > 0 && ruleCount(got.Diff.Risk, RuleDependentRetargeted) != 0 {
					t.Fatalf("%s: proven move cited: %s", name, canonical(got.Diff.Risk))
				}
			}
		})
	}
}

// The standard library's own package list proves goStdName on this toolchain.
func TestGoStdNameMatchesStandardLibrary(t *testing.T) {
	out, err := exec.Command("go", "list", "-f", "{{.ImportPath}} {{.Name}}", "std").Output()
	if err != nil {
		t.Skipf("go list std: %v", err)
	}
	checked := 0
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		importPath, name, _ := strings.Cut(line, " ")
		if got, ok := goStdName(importPath, nil); ok {
			checked++
			if got != name {
				t.Errorf("goStdName(%q) = %q, package is %q", importPath, got, name)
			}
		}
	}
	if checked < 100 {
		t.Fatalf("only %d standard packages named", checked)
	}
	for _, p := range []string{"gopkg.in/yaml.v3", "math/rand/v2", "github.com/x/go-thing", "example.com/m/v2"} {
		if n, ok := goStdName(p, nil); ok {
			t.Errorf("goStdName(%q) = %q", p, n)
		}
	}
	if n, ok := goStdName("tools/fmt", []string{"tools"}); ok {
		t.Errorf("required module path named %q", n)
	}
}

func TestImportsAgree(t *testing.T) {
	std := func(im goImport) (string, bool) { return goStdName(im.path, nil) }
	agree := func(base, head string) bool {
		b, h := sourceOf(t, base), sourceOf(t, head)
		l := strings.Count(base[:strings.Index(base, "func A")], "\n") + 1
		top, ok := b.declarationHash(lines(l, l))
		if !ok {
			t.Fatalf("no declaration in %q", base)
		}
		return importsAgree(&b, &h, top.idents, std, std)
	}
	fn := "func A() string { return strings.ToUpper(\"a\") }\n"
	if !agree("package p\n\nimport \"strings\"\n\n"+fn, "package p\n\nimport (\n\t\"fmt\"\n\t\"strings\"\n)\n\n"+fn) {
		t.Fatal("unrelated import refused")
	}
	linkname := "\n//go:linkname nanotime runtime.nanotime\nfunc nanotime() int64\n"
	if agree("package p\n\nimport \"strings\"\n\n"+fn+linkname, "package p\n\nimport (\n\t\"strings\"\n\t_ \"unsafe\"\n)\n\n"+fn+linkname) {
		t.Fatal("linkname on both sides agreed")
	}
	if agree("// Copyright a\n\npackage p\n\nimport \"strings\"\n\n"+fn, "package p\n\nimport (\n\t\"fmt\"\n\t\"strings\"\n)\n\n"+fn) {
		t.Fatal("header comment change agreed")
	}
}
