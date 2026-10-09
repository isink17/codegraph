//go:build cgo

package treesitter

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

// lexicalLanguage is one parser whose call edges carry the position of the
// declaration they bind as evidence.
type lexicalLanguage struct {
	name, path, evidence string
	parse                func([]byte) (graph.ParsedFile, error)
	// shadows each declare f in the scope of the call, which must then bind
	// nothing: a value, a pattern, an import or a second declaration of f
	// all make the original declaration unprovable.
	shadows []string
	seeds   []lexicalSeed
}

// lexicalSeed is a valid file whose calls of f bind exactly wantF times. The
// slot "@" sits in the innermost scope of every call of f, before the first
// one; sep ends a statement inserted there.
type lexicalSeed struct {
	name, src, sep, decl string
	wantF                int
}

// fEdges returns the calls of f, failing on any edge whose evidence does not
// name the start of a function symbol of the callee's name.
func fEdges(t *testing.T, lang lexicalLanguage, src string) []graph.Edge {
	t.Helper()
	pf, err := lang.parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	starts := map[string]bool{}
	for _, s := range pf.Symbols {
		if s.Kind == "function" {
			starts[fmt.Sprintf("%s@%d:%d", s.Name, s.Range.StartLine, s.Range.StartCol)] = true
		}
	}
	var out []graph.Edge
	for _, e := range pf.Edges {
		pos, ok := strings.CutPrefix(e.Evidence, lang.evidence)
		if !ok || !starts[e.DstName+"@"+pos] {
			t.Fatalf("%q: edge %s@%d:%d evidence %q names no function symbol", src, e.DstName, e.Line, e.Col, e.Evidence)
		}
		if e.DstName == "f" {
			out = append(out, e)
		}
	}
	return out
}

// Each seed is mutated at its slot and as a whole. A layout mutation keeps
// every binding, with call and declaration positions still agreeing; a
// shadowing, renaming or duplicating mutation leaves no call of f bound.
func TestLexicalCallMutations(t *testing.T) {
	scala := lexicalLanguage{
		name: "scala", path: "M.scala", evidence: graph.ScalaLocalFunctionEvidence,
		parse: func(b []byte) (graph.ParsedFile, error) { return NewScala().Parse(context.Background(), "M.scala", b) },
		shadows: []string{
			"val f = () => 2", "var f = () => 2", "lazy val f = () => 2",
			"def f() = 3", "def f(x: Int) = x", "val (f, z) = (() => 2, 1)", "val Some(f) = Some(() => 2)",
			"import X.f", "import X._", "import X.*", "import X.{g => f}",
			"given f: (() => Int) = () => 2", "object f { def apply() = 2 }", "case class f()",
		},
		seeds: []lexicalSeed{
			{"braced", "object A {\n  def m = { def f() = 1; @f() }\n}\n", "; ", "def f(", 1},
			{"indented", "object A:\n  def m =\n    def f() = 1\n    @f()\n", "\n    ", "def f(", 1},
			{"nested block", "object A {\n  def m = {\n    def f() = 1\n    val r = { @f() + f() }\n    r\n  }\n}\n", "; ", "def f(", 2},
			{"case body", "object A {\n  def m(x: Int) = x match {\n    case 1 =>\n      def f() = 1\n      @f()\n    case _ => 0\n  }\n}\n", "\n      ", "def f(", 1},
			{"lambda body", "object A {\n  def m = { def f() = 1; List(1).map { x => @f() } }\n}\n", "; ", "def f(", 1},
		},
	}
	dart := lexicalLanguage{
		name: "dart", path: "lib/m.dart", evidence: graph.DartLexicalFunctionEvidence,
		parse: func(b []byte) (graph.ParsedFile, error) {
			return NewDart().Parse(context.Background(), "lib/m.dart", b)
		},
		shadows: []string{
			"var f = () {}", "final f = () {}", "late final f = () {}", "dynamic f",
			"void f() {}", "var (f, z) = (() {}, 1)", "final (:f) = (f: () {})",
		},
		seeds: []lexicalSeed{
			{"top-level", "void f() {}\nvoid g() { @f(); }\n", "; ", "void f(", 1},
			{"local", "void g() {\n  void f() {}\n  @f();\n  f();\n}\n", ";\n  ", "void f(", 2},
			{"closure", "void f() {}\nvoid g() { [1].forEach((x) { @f(); }); }\n", "; ", "void f(", 1},
			{"method", "void f() {}\nclass C { void g() { @f(); } }\n", "; ", "void f(", 1},
			{"generic", "T f<T>(T v) => v;\nvoid g() { @f<int>(1); }\n", "; ", "T f<", 1},
		},
	}
	for _, lang := range []lexicalLanguage{scala, dart} {
		for _, seed := range lang.seeds {
			at := strings.Index(seed.src, "@")
			slot := func(ins string) string { return seed.src[:at] + ins + seed.src[at+1:] }
			t.Run(lang.name+"/"+seed.name, func(t *testing.T) {
				if got := fEdges(t, lang, slot("")); len(got) != seed.wantF {
					t.Fatalf("seed binds %d calls of f, want %d", len(got), seed.wantF)
				}
				layout := map[string]string{
					"comment at slot":       slot("/* c */ "),
					"unicode comment":       slot("/* üé日本 */ "),
					"unicode string":        slot(`"üé日本"` + seed.sep),
					"line split at slot":    slot("\n" + strings.Repeat(" ", 4)),
					"leading unicode line":  "// ünïcödé 日本\n" + slot(""),
					"CRLF":                  strings.ReplaceAll(slot(""), "\n", "\r\n"),
					"trailing declarations": slot("") + "\n// tail\n",
				}
				if lang.name == "scala" {
					delete(layout, "line split at slot") // indentation is syntax
				}
				for what, src := range layout {
					if got := fEdges(t, lang, src); len(got) != seed.wantF {
						t.Errorf("%s: %d calls of f bound, want %d\n%s", what, len(got), seed.wantF, src)
					}
				}
				hazards := map[string]string{
					"renamed declaration": strings.Replace(slot(""), seed.decl, strings.Replace(seed.decl, "f", "q", 1), 1),
					"deleted declaration": strings.Replace(slot(""), seed.decl, strings.Replace(seed.decl, "f", "_", 1), 1),
				}
				for what, src := range hazards {
					if got := fEdges(t, lang, src); len(got) != 0 {
						t.Errorf("%s: calls of f still bound %+v\n%s", what, got, src)
					}
				}
				// A shadow may only take the calls itself: a function
				// declared at the slot is the innermost binding when the slot
				// opens a nested scope, and ambiguous beside the original.
				line := strings.Count(seed.src[:at], "\n") + 1
				slotPos := fmt.Sprintf("%s%d:%d", lang.evidence, line, at-strings.LastIndex(seed.src[:at], "\n"))
				for _, sh := range lang.shadows {
					src := slot(sh + seed.sep)
					for _, e := range fEdges(t, lang, src) {
						if e.Evidence != slotPos {
							t.Errorf("shadow %s: call of f at %d:%d binds %s, not the shadow at %s\n%s", sh, e.Line, e.Col, e.Evidence, slotPos, src)
						}
					}
				}
			})
		}
	}
}

// Hazards found by mutating the seeds above, each with every edge the file
// yields. A nil want is a refusal; most guard a binding the call does not
// have, a few are conservative refusals of a binding Scala or Dart would make.
func TestLexicalCallHazardCorpus(t *testing.T) {
	scala := []struct {
		name, src string
		want      []string
	}{
		{"lambda parameter", "object A {\n  def m = { def f() = 1; List(1).map(f => f()) }\n}\n", nil},
		{"case binding", "object A {\n  def m(x: Any) = { def f() = 1; x match { case Some(f) => f() } }\n}\n", nil},
		{"catch binding", "object A {\n  def m = { def f() = 1; try 1 catch { case f: Throwable => f() } }\n}\n", nil},
		{"for generator", "object A {\n  def m = { def f() = 1; for (f <- List(() => 1)) yield f() }\n}\n", nil},
		{"for value definition", "object A {\n  def m = { def f() = 1; for (x <- List(1); f = () => x) yield f() }\n}\n", nil},
		{"class parameter", "object A {\n  def m = { def f() = 1; class C(f: () => Int) { def k = f() }; 0 }\n}\n", nil},
		{"local class member", "object A {\n  def m = { def f() = 1; class C { def f() = 2; def k = f() }; 0 }\n}\n", nil},
		{"inherited member of a local class", "object A {\n  def m = { def f() = 1; class C extends B { def k = f() }; 0 }\n}\n", nil},
		{"anonymous class", "object A {\n  def m = { def f() = 1; new B { def k = f() } }\n}\n", nil},
		{"self alias", "object A {\n  def m = { def f() = 1; class C { f => def k = f() }; 0 }\n}\n", nil},
		{"using parameter", "object A {\n  def m = { def f() = 1; def g(using f: () => Int) = f(); 0 }\n}\n", nil},
		{"extension receiver", "object A {\n  def m = { def f() = 1; extension (f: () => Int) def run = f(); 0 }\n}\n", nil},
		{"local extension method", "object A {\n  def m = { extension (x: Int) def f() = 1; f() }\n}\n", nil},
		{"context function parameter", "object A {\n  def m = { def f() = 1; (f: Int) ?=> f() }\n}\n", nil},
		{"quote", "object A {\n  def m = { def f() = 1; '{ f() } }\n}\n", nil},
		{"splice", "object A {\n  def m = { def f() = 1; ${ f() } }\n}\n", nil},
		{"val after the call in the same block", "object A {\n  def m = { def f() = 1; f(); val f = () => 2; f() }\n}\n", nil},
		{"sibling block", "object A {\n  def m = { { def f() = 1 }; { f() } }\n}\n", nil},
		{"selection", "object A {\n  def m = { def f() = 1; this.f(); A.f() }\n}\n", nil},
		{"indented val after def", "object A:\n  def m =\n    def f() = 1\n    val f = () => 2\n    f()\n", nil},
		{"indented case binding", "object A:\n  def m(x: Any) =\n    def f() = 1\n    x match\n      case f: (() => Int) => f()\n", nil},
		{"call after the def's block", "object A:\n  def m =\n    def f() = 1\n    1\n  def n =\n    f()\n", nil},
		{"forward reference in a block", "object A {\n  def m = { f(); def f() = 1 }\n}\n", []string{"f@2:13->2:18"}},
		{"inner def declared after the call", "object A {\n  def m = { def f() = 1; { f(); def f() = 2 } }\n}\n", []string{"f@2:28->2:33"}},
		{"backticked declaration", "object A {\n  def m = { def `f`() = 1; f() }\n}\n", []string{"f@2:28->2:13"}},
		{"local def shadows outer parameter", "object A {\n  def m(f: () => Int) = { def f() = 1; f() }\n}\n", []string{"f@2:40->2:27"}},
		{"sibling methods", "object A {\n  def m = { def f() = 1; f() }\n  def n = { def f() = 2; f() }\n}\n", []string{"f@2:26->2:13", "f@3:26->3:13"}},
		{"multibyte before the call", "object A {\n  def m = { val é = \"ü\"; def f() = 1; f() }\n}\n", []string{"f@2:41->2:28"}},
	}
	for _, tc := range scala {
		if got := scalaEdges(parseScala(t, "A.scala", tc.src)); !slices.Equal(got, tc.want) {
			t.Errorf("scala %s: edges = %q, want %q", tc.name, got, tc.want)
		}
	}
	dart := []struct {
		name, src string
		want      []string
	}{
		{"parameter", "void f() {}\nvoid g(void Function() f) { f(); }\n", nil},
		{"function-typed parameter", "void f() {}\nvoid g(void f()) { f(); }\n", nil},
		{"named parameter", "void f() {}\nvoid g({required void Function() f}) { f(); }\n", nil},
		{"closure parameter", "void f() {}\nvoid g() { [1].forEach((f) { f(); }); }\n", nil},
		{"type parameter", "void f() {}\nvoid g<f>() { f(); }\n", nil},
		{"local after the call", "void f() {}\nvoid g() { f(); var f = () {}; }\n", nil},
		{"for-in variable", "void f() {}\nvoid g() { for (var f in [() {}]) { f(); } }\n", nil},
		{"catch variable", "void f() {}\nvoid g() { try {} catch (e, f) { f(); } }\n", nil},
		{"switch pattern", "void f() {}\nvoid g(Object o) { switch (o) { case void Function() f: f(); } }\n", nil},
		{"switch expression pattern", "void f() {}\nint g(Object o) => switch (o) { void Function() f => f(), _ => 0 };\n", nil},
		{"if-case pattern", "void f() {}\nvoid g(Object o) { if (o case void Function() f) { f(); } }\n", nil},
		{"collection for", "void f() {}\nvar l = [for (var f in []) f()];\n", nil},
		{"method of the same name", "void f() {}\nclass C { void f() {} void g() { f(); } }\n", nil},
		{"field of the same name", "void f() {}\nclass C { var f = () {}; void g() { f(); } }\n", nil},
		{"named constructor of the same name", "void f() {}\nclass C { void g() { f(); } C.f(); }\n", nil},
		{"enum value of the same name", "void f() {}\nenum K { f; void g() { f(); } }\n", nil},
		{"extension type representation", "void f() {}\nextension type X(int f) { void g() { f(); } }\n", nil},
		{"initializing formal", "void f() {}\nclass C { final Function f; C(this.f) { f(); } }\n", nil},
		{"class named like the callee", "class f {}\nvoid g() { f(); }\n", nil},
		{"typedef named like the callee", "typedef f = void Function();\nvoid g() { f(); }\n", nil},
		{"top-level variable after the function", "void f() {}\nvoid g() { f(); }\nint f = 1;\n", nil},
		{"import prefix", "import 'a.dart' as f;\nvoid g() { f.h(); }\n", nil},
		{"part file", "part 'b.dart';\nvoid f() {}\nvoid g() { f(); }\n", nil},
		{"part of", "part of 'a.dart';\nvoid f() {}\nvoid g() { f(); }\n", nil},
		{"null-aware call", "void f() {}\nvoid g() { f?.call(); }\n", nil},
		{"sibling blocks", "void g() { { void f() {} } { f(); } }\n", nil},
		{"library declaration over an import", "import 'a.dart';\nvoid f() {}\nvoid g() { f(); }\n", []string{"f@3:12->2:1"}},
		{"library declaration over an inherited member", "void f() {}\nclass C extends B { void g() { f(); } }\n", []string{"f@2:32->1:1"}},
		{"initializer list", "void f() {}\nclass C { final int x; C() : x = f(); }\n", []string{"f@2:34->1:1"}},
		{"adjacent declarations", "void f(){}void g(){f();}\n", []string{"f@1:20->1:1"}},
		{"multiline call", "void f() {}\nvoid g() {\n  f(\n    );\n}\n", []string{"f@3:3->1:1"}},
		{"multibyte before the call", "void f() {}\nvoid g() { var s = 'üü'; f(); }\n", []string{"f@2:28->1:1"}},
		{"local function in a closure", "void g() { void f() {} [1].forEach((x) { f(); }); }\n", []string{"f@1:42->1:12"}},
	}
	for _, tc := range dart {
		if got := dartEdges(t, tc.src); !slices.Equal(got, tc.want) {
			t.Errorf("dart %s: edges = %q, want %q", tc.name, got, tc.want)
		}
	}
}
