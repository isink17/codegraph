//go:build cgo

package treesitter

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/parser/treesitter/dartgrammar"
)

func dartEdgeTargets(edges []graph.Edge) []string {
	out := []string{}
	for _, e := range edges {
		out = append(out, fmt.Sprintf("%s@%d:%d->%s", e.DstName, e.Line, e.Col, strings.TrimPrefix(e.Evidence, graph.DartLexicalFunctionEvidence)))
	}
	return out
}

func dartEdges(t *testing.T, src string) []string {
	t.Helper()
	pf, err := NewDart().Parse(context.Background(), "lib/sample.dart", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range pf.Edges {
		if e.Kind != "calls" || !strings.HasPrefix(e.Evidence, graph.DartLexicalFunctionEvidence) {
			t.Fatalf("unexpected edge %+v", e)
		}
	}
	return dartEdgeTargets(pf.Edges)
}

// Each case lists every edge the file yields as callee@call->declaration.
// Positive cases bind only where the spelling leaves one possible binding;
// each hazard case yields none. Cases marked conservative would bind under
// Dart's rules but the token proof cannot tell them apart from a shadowing
// declaration.
func TestDartLexicalCallResolution(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		want         []string
	}{
		// Positive bindings.
		{"top-level function", "void helper() {}\nvoid main() { helper(); }\n",
			[]string{"helper@2:15->1:1"}},
		{"top-level recursion", "int fact(int n) => n < 2 ? 1 : n * fact(n - 1);\n",
			[]string{"fact@1:36->1:1"}},
		{"generic call", "T id<T>(T v) => v;\nvoid main() { id<int>(1); }\n",
			[]string{"id@2:15->1:1"}},
		{"top-level function from a class, mixin, extension and enum", `void helper() {}
class C { void m() { helper(); } }
mixin M { void m() { helper(); } }
extension E on int { void m() { helper(); } }
enum K { a; void m() { helper(); } }
`, []string{"helper@2:22->1:1", "helper@3:22->1:1", "helper@4:33->1:1", "helper@5:24->1:1"}},
		// The Dart specification looks an identifier up lexically first and
		// only then as an inherited member, so a superclass method never
		// shadows a top-level function.
		{"inherited member does not shadow a top-level function", `void helper() {}
class B { void helper() {} }
class C extends B { void m() { helper(); } }
`, []string{"helper@3:32->1:1"}},
		// A library's own declarations shadow every imported name.
		{"top-level function shadows an unrestricted import", "import 'other.dart';\nvoid print(Object o) {}\nvoid main() { print(1); }\n",
			[]string{"print@3:15->2:1"}},
		{"local function", "void main() {\n  int sq(int n) => n * n;\n  sq(2);\n}\n",
			[]string{"sq@3:3->2:3"}},
		{"local recursion and call from a nested closure", `void main() {
  int fib(int n) => n < 2 ? n : fib(n - 1) + fib(n - 2);
  final g = () => fib(3);
  if (true) { fib(4); }
}
`, []string{"fib@2:33->2:3", "fib@2:46->2:3", "fib@3:19->2:3", "fib@4:15->2:3"}},
		{"local function shadows a top-level function only in its block", `void f() {}
void main() {
  void f() {}
  f();
}
void other() { f(); }
`, []string{"f@4:3->3:3", "f@6:16->1:1"}},
		{"local function in a method and a top-level initializer", `class C {
  void m() { void go() {} go(); }
}
final run = () { void go() {} go(); };
`, []string{"go@2:27->2:14", "go@4:31->4:18"}},

		// Hazards: none of these may bind.
		{"part directive", "part 'b.dart';\nvoid f() {}\nvoid main() { f(); }\n", []string{}},
		{"part of directive", "part of 'a.dart';\nvoid f() {}\nvoid main() { f(); }\n", []string{}},
		{"class member of the same name", "void f() {}\nclass C { void f() {} void m() { f(); } }\n", []string{}},
		{"static member of the same name", "void f() {}\nclass C { static void f() {} void m() { f(); } }\n", []string{}},
		{"field of the same name", "void f() {}\nclass C { final f = () {}; void m() { f(); } }\n", []string{}},
		{"getter of the same name", "void f() {}\nclass C { int get f => 1; void m() { f(); } }\n", []string{}},
		{"extension member of the same name", "void f() {}\nextension E on int { void f() {} void m() { f(); } }\n", []string{}},
		{"enum constant of the same name", "void f() {}\nenum K { f; void m() { f(); } }\n", []string{}},
		{"parameter of the same name", "void f() {}\nvoid main(void Function() f) { f(); }\n", []string{}},
		{"local closure variable", "void f() {}\nvoid main() { var f = () {}; f(); }\n", []string{}},
		{"local closure variable alone", "void main() { final g = () {}; g(); }\n", []string{}},
		{"pattern variable", "void f() {}\nvoid main(Object o) { if (o case (var f, 1)) { f(); } }\n", []string{}},
		{"catch and loop variables", "void f() {}\nvoid a() { try {} catch (f) { f(); } }\nvoid b(List xs) { for (final f in xs) { f(); } }\n", []string{}},
		{"top-level getter", "int get f => 1;\nvoid main() { f(); }\n", []string{}},
		{"top-level setter beside the function", "void f() {}\nset f(v) {}\nvoid main() { f(); }\n", []string{}},
		{"top-level variable", "var f = () {};\nvoid main() { f(); }\n", []string{}},
		{"duplicate top-level function", "void f() {}\nvoid f() {}\nvoid main() { f(); }\n", []string{}},
		{"external top-level function", "external void f();\nvoid main() { f(); }\n", []string{}},
		{"class of the same name", "class f {}\nvoid main() { f(); }\n", []string{}},
		{"import prefix of the same name", "import 'x.dart' as f;\nvoid f() {}\nvoid main() { f(); }\n", []string{}},
		{"typedef of the same name", "typedef f = void Function();\nvoid main() { f(); }\n", []string{}},
		// Conservative: the library declaration shadows the shown name, but a
		// directive token is not told apart from a declaration.
		{"show import of the same name (conservative)", "import 'x.dart' show f;\nvoid f() {}\nvoid main() { f(); }\n", []string{}},
		{"hide import of the same name (conservative)", "import 'x.dart' hide f;\nvoid f() {}\nvoid main() { f(); }\n", []string{}},
		{"imported name only", "import 'x.dart';\nvoid main() { f(); }\n", []string{}},
		{"undeclared name", "void main() { f(); }\n", []string{}},
		{"prefixed import call", "import 'x.dart' as p;\nvoid f() {}\nvoid main() { p.f(); }\n", []string{}},
		{"named and factory constructors", "class C { C(); C.make(); factory C.build() => C.make(); }\nvoid main() { C(); C.make(); C.build(); new C(); const C.make(); }\n", []string{}},
		{"static member call", "class C { static void s() {} }\nvoid s() {}\nvoid main() { C.s(); }\n", []string{}},
		{"extension method call", "extension E on int { int twice() => 2; }\nvoid main() { 1.twice(); }\n", []string{}},
		{"dynamic and nullable receivers", "void f() {}\nvoid main(dynamic d, C? c) { d.f(); c?.f(); }\n", []string{}},
		// Conservative: `d.f` is a member access, yet its token blocks the bare
		// call in the same declaration.
		{"member token blocks a bare call in its declaration (conservative)", "void f() {}\nvoid main(dynamic d) { d.f; f(); }\n", []string{}},
		{"cascade", "void f() {}\nvoid main() { final b = StringBuffer()..f()..write(1); }\n", []string{}},
		{"this and super calls", "void f() {}\nclass C extends B { void m() { this.f(); super.f(); } }\n", []string{}},
		{"dot shorthand calls", "class C { static C make() => C(); C(); }\nC make() => C();\nC a() => .make();\nC b() { const C x = .new(); return .make(); }\nvoid c(C v) { switch (v) { case .make: break; } }\n", []string{}},
		{"dot shorthand elsewhere leaves a bare call bound", "class C { static C make() => C(); }\nC make() => C();\nC a() => .make();\nC b() => make();\n",
			[]string{"make@4:10->2:1"}},
		// A comment between a signature and its body must not split them: the
		// parameters still shadow the top-level function.
		{"block comment before the body", "void g() {}\nvoid f(void Function() g) /* c */ { g(); }\n", []string{}},
		{"line comment before the body", "void g() {}\nvoid f(void Function() g) // ignore: x\n{ g(); }\n", []string{}},
		{"doc comment before the body", "void g() {}\nvoid f(void Function() g)\n/// doc\n{ g(); }\n", []string{}},
		{"comment before an arrow body", "void g() {}\nvoid f(void Function() g) /* c */ => g();\n", []string{}},
		{"comment after a shadowing type parameter", "void g() {}\nvoid f<g>() /* c */ { g(); }\n", []string{}},
		{"comment before a method body", "void g() {}\nclass C { void m(void Function() g) /* c */ { g(); } }\n", []string{}},
		{"comment before a setter body", "void g() {}\nset x(void Function() g) /* c */ { g(); }\n", []string{}},
		{"comment before a getter body, nothing shadows", "void g() {}\nint get x /* c */ { g(); return 1; }\n", []string{"g@2:21->1:1"}},
		{"comment before a local function body", "void g() {}\nvoid main() { void h(void Function() g) /* c */ { g(); } }\n", []string{}},
		{"comment before the body, nothing shadows", "void g() {}\nvoid f() /* c */ { g(); }\nvoid k()\n/// doc\n=> g();\n",
			[]string{"g@2:20->1:1", "g@5:4->1:1"}},
		{"comment before a local function body, nothing shadows", "void main() { void h() /* c */ { } h(); }\n",
			[]string{"h@1:36->1:15"}},
		{"wildcard local function", "void _() {}\nvoid main() { void _() {} _(); }\n", []string{}},
		{"wildcard top-level function", "void _() {}\nvoid main() { _(); }\n", []string{}},
		{"anonymous closures", "void main() { (() {})(); (() => 1)(); }\n", []string{}},
		{"call of a call result and an index", "int Function() f() => () => 1;\nvoid main(List xs) { f()(); xs[0](); }\n", []string{"f@2:22->1:1"}},
		{"local declared after the call", "void main() {\n  g();\n  void g() {}\n}\n", []string{}},
		{"local in a sibling block", "void main() {\n  { void g() {} }\n  g();\n}\n", []string{}},
		{"local beside a parameter of the same name", "void main(Function g) {\n  { void g() {} g(); }\n}\n", []string{}},
		{"two locals of the same name", "void main() {\n  { void g() {} g(); }\n  { void g() {} g(); }\n}\n", []string{}},
		{"local beside a class member of the same name (conservative)", "class C { void g() {} void m() { void g() {} g(); } }\n", []string{}},
		{"local in a switch case", "void main(int x) {\n  switch (x) {\n    case 1:\n      void g() {}\n      g();\n  }\n}\n", []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := dartEdges(t, tc.source); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("edges:\n got %q\nwant %q", got, tc.want)
			}
		})
	}
}

// A local function is a symbol whose stable key carries its position, so two
// local functions of one name stay distinct; every call edge names the start
// of the symbol it binds. Two declarations of g in one function refuse both
// calls of g.
func TestDartLocalFunctionSymbolsMatchEdgeEvidence(t *testing.T) {
	const src = `void top() {}
void main() {
  { void g() {} g(); }
  { void g() {} g(); top(); }
}
`
	pf, err := NewDart().Parse(context.Background(), "lib/sample.dart", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"function func:dart:local:g:3:5", "function func:dart:local:g:4:5", "function func:dart:main", "function func:dart:top"}
	if got := dartKeys(pf); !reflect.DeepEqual(got, want) {
		t.Fatalf("symbols:\n got %q\nwant %q", got, want)
	}
	starts := map[string]bool{}
	for _, s := range pf.Symbols {
		starts[fmt.Sprintf("%s:%d:%d", s.Name, s.Range.StartLine, s.Range.StartCol)] = true
	}
	for _, e := range pf.Edges {
		if !starts[e.DstName+":"+strings.TrimPrefix(e.Evidence, graph.DartLexicalFunctionEvidence)] {
			t.Fatalf("edge %+v names no symbol start", e)
		}
	}
	if got := dartEdgeTargets(pf.Edges); !reflect.DeepEqual(got, []string{"top@4:22->1:1"}) {
		t.Fatalf("edges = %q", got)
	}
}

// A file with a parse error records no local function and no edge, whatever
// the damage: ownership after recovery is not the ownership the source spells.
func TestDartLexicalCallsRefuseDamagedSource(t *testing.T) {
	const src = `void helper() {}
void main() {
  void g() {}
  g();
  helper();
}
`
	if got := dartEdges(t, src); len(got) != 2 {
		t.Fatalf("intact file edges = %q", got)
	}
	for i := range len(src) {
		for _, damaged := range []string{src[:i] + src[i+1:], src[:i] + "{" + src[i:], src[:i] + "}" + src[i:]} {
			pf, err := NewDart().Parse(context.Background(), "damaged.dart", []byte(damaged))
			if err != nil {
				t.Fatal(err)
			}
			root, err := parse(context.Background(), dartgrammar.GetLanguage(), []byte(damaged))
			if err != nil {
				t.Fatal(err)
			}
			if !root.HasError() {
				continue
			}
			if len(pf.Edges) != 0 {
				t.Fatalf("damaged source %q yields edges %+v", damaged, pf.Edges)
			}
			for _, s := range pf.Symbols {
				if strings.HasPrefix(s.StableKey, "func:dart:local:") {
					t.Fatalf("damaged source %q records local %s", damaged, s.StableKey)
				}
			}
		}
	}
}

// A comment between a signature and its body leaves the body in the
// declaration's range, top-level and member alike.
func TestDartSymbolRangeSpansCommentAndBody(t *testing.T) {
	pf := parseDart(t, "void f() /* c */ {\n}\nclass C {\n  void m() // c\n  {\n  }\n}\n")
	if f := dartSymbolByKey(t, pf, "func:dart:f"); f.Range.EndLine != 2 {
		t.Fatalf("f range = %+v", f.Range)
	}
	if m := dartSymbolByKey(t, pf, "func:dart:C.m"); m.Range.EndLine != 6 {
		t.Fatalf("C.m range = %+v", m.Range)
	}
}
