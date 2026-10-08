//go:build cgo

package treesitter

import (
	"context"
	"fmt"
	"reflect"
	"runtime"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/parser/treesitter/dartgrammar"
)

func parseDart(t *testing.T, src string) graph.ParsedFile {
	t.Helper()
	pf, err := NewDart().Parse(context.Background(), "lib/sample.dart", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return pf
}

// dartKeys renders each symbol as "kind stable_key".
func dartKeys(pf graph.ParsedFile) []string {
	out := make([]string, 0, len(pf.Symbols))
	for _, s := range pf.Symbols {
		out = append(out, s.Kind+" "+s.StableKey)
	}
	sort.Strings(out)
	return out
}

func dartRefs(pf graph.ParsedFile) []string {
	out := make([]string, 0, len(pf.References))
	for _, r := range pf.References {
		if r.Kind != "call" || r.QualifiedName != r.Name {
			out = append(out, "bad:"+r.Kind+":"+r.Name)
			continue
		}
		out = append(out, r.Name)
	}
	sort.Strings(out)
	return out
}

func dartSymbolByKey(t *testing.T, pf graph.ParsedFile, key string) graph.Symbol {
	t.Helper()
	for _, s := range pf.Symbols {
		if s.StableKey == key {
			return s
		}
	}
	t.Fatalf("no symbol %s in %v", key, dartKeys(pf))
	return graph.Symbol{}
}

func assertDartKeys(t *testing.T, pf graph.ParsedFile, want []string) {
	t.Helper()
	sort.Strings(want)
	if got := dartKeys(pf); !reflect.DeepEqual(got, want) {
		t.Fatalf("symbols:\n got %q\nwant %q", got, want)
	}
}

func TestDartAdapterDart3Declarations(t *testing.T) {
	const src = `library shapes;

/// A shape.
sealed class Shape {}
final class Square extends Shape { final double side; Square(this.side); }
base class Circle extends Shape { final double r; const Circle(this.r); }
interface class Named { String get name => 'n'; }
abstract mixin class Logger { void log(String m) => print(m); }
mixin Walker on Shape { void walk() {} }
extension type UserId(int value) implements int {
  bool get isValid => value > 0;
}
extension StringX on String { String shout() => toUpperCase(); }
extension on int { int get twice => this * 2; }
enum Color with Walker implements Named {
  red('r'), green('g');
  final String code;
  const Color(this.code);
  static Color parse(String s) => red;
  @override
  String get name => code;
}
typedef IntList = List<int>;
typedef int Compare(Object a, Object b);
const answer = 42;
late final String _greeting;
int get total => 1;
set total(int v) {}
(int, {String name}) pair() => (1, name: 'a');
double area(Shape s) => switch (s) {
  Square(side: var a) => a * a,
  Circle(:final r) when r > 0 => 3.14 * r * r,
  _ => 0,
};
void main() {
  var (a, b) = (1, 2);
  final [first, ...rest] = [1, 2, 3];
  if (pair() case (var n, name: var nm)) { print(nm); }
}
class Box<T> {
  int operator +(Box<T> o) => 0;
  T? _item;
  set item(T v) { _item = v; }
  T? get item => _item;
}
`
	pf := parseDart(t, src)
	assertDartKeys(t, pf, []string{
		"class type:dart:Shape", "class type:dart:Square", "field value:dart:Square.side", "constructor func:dart:Square.Square",
		"class type:dart:Circle", "field value:dart:Circle.r", "constructor func:dart:Circle.Circle",
		"class type:dart:Named", "method func:dart:Named.name",
		"class type:dart:Logger", "method func:dart:Logger.log",
		"mixin type:dart:Walker", "method func:dart:Walker.walk",
		"extension_type type:dart:UserId", "field value:dart:UserId.value", "method func:dart:UserId.isValid",
		"extension type:dart:StringX", "method func:dart:StringX.shout",
		"enum type:dart:Color", "enum_value value:dart:Color.red", "enum_value value:dart:Color.green",
		"field value:dart:Color.code", "constructor func:dart:Color.Color", "method func:dart:Color.parse", "method func:dart:Color.name",
		"typedef type:dart:IntList", "typedef type:dart:Compare",
		"variable value:dart:answer", "variable value:dart:_greeting",
		"function func:dart:total", "function func:dart:total=",
		"function func:dart:pair", "function func:dart:area", "function func:dart:main",
		"class type:dart:Box", "method func:dart:Box.operator+", "field value:dart:Box._item",
		"method func:dart:Box.item=", "method func:dart:Box.item",
	})
	shape := dartSymbolByKey(t, pf, "type:dart:Shape")
	if shape.Signature != "sealed class Shape" || shape.DocSummary != "A shape." || shape.Visibility != "public" {
		t.Fatalf("Shape = %+v", shape)
	}
	if s := dartSymbolByKey(t, pf, "type:dart:Logger"); s.Signature != "abstract mixin class Logger" {
		t.Fatalf("Logger signature = %q", s.Signature)
	}
	if s := dartSymbolByKey(t, pf, "value:dart:_greeting"); s.Visibility != "private" || s.Signature != "late final String _greeting" {
		t.Fatalf("_greeting = %+v", s)
	}
	setter := dartSymbolByKey(t, pf, "func:dart:Box.item=")
	if setter.Name != "item" || setter.QualifiedName != "Box.item" || setter.ContainerName != "Box" || setter.Signature != "set item(T v)" {
		t.Fatalf("setter = %+v", setter)
	}
	// A method spans its signature and its body, so references inside the
	// body fall within it.
	if m := dartSymbolByKey(t, pf, "func:dart:main"); m.Range.StartLine != 35 || m.Range.EndLine != 39 {
		t.Fatalf("main range = %+v", m.Range)
	}
	if m := dartSymbolByKey(t, pf, "func:dart:Color.name"); m.Signature != "String get name" {
		t.Fatalf("annotated getter = %+v", m)
	}
}

func TestDartAdapterConstructors(t *testing.T) {
	const src = `class Point {
  final int x, y;
  Point(this.x, this.y);
  Point.origin() : this(0, 0);
  const Point.fixed() : x = 0, y = 0;
  factory Point.fromJson(Map<String, dynamic> j) => Point(j['x'] as int, j['y'] as int);
  factory Point.redirect(int x) = Point.named;
  Point.named(this.x) : y = 0;
  Point._internal(this.x, this.y);
  external factory Point.native();
}
class Other {
  Other.new();
}
`
	pf := parseDart(t, src)
	assertDartKeys(t, pf, []string{
		"class type:dart:Point", "field value:dart:Point.x", "field value:dart:Point.y",
		"constructor func:dart:Point.Point", "constructor func:dart:Point.origin", "constructor func:dart:Point.fixed",
		"constructor func:dart:Point.fromJson", "constructor func:dart:Point.redirect", "constructor func:dart:Point.named",
		"constructor func:dart:Point._internal", "constructor func:dart:Point.native",
		"class type:dart:Other", "constructor func:dart:Other.Other",
	})
	for key, want := range map[string]string{
		"func:dart:Point.Point":     "Point.Point|Point|Point(this.x, this.y)|public",
		"func:dart:Point.fromJson":  "Point.fromJson|Point|factory Point.fromJson(Map<String, dynamic> j)|public",
		"func:dart:Point.redirect":  "Point.redirect|Point|factory Point.redirect(int x) = Point.named|public",
		"func:dart:Point._internal": "Point._internal|Point|Point._internal(this.x, this.y)|private",
	} {
		s := dartSymbolByKey(t, pf, key)
		if got := strings.Join([]string{s.QualifiedName, s.ContainerName, s.Signature, s.Visibility}, "|"); got != want {
			t.Errorf("%s = %q, want %q", key, got, want)
		}
	}
	// Constructor calls are references; the redirect target is not a call.
	if got := dartRefs(pf); !reflect.DeepEqual(got, []string{"Point"}) {
		t.Fatalf("references = %q", got)
	}
}

func TestDartAdapterDirectives(t *testing.T) {
	const src = `library app;
import 'package:flutter/material.dart' as m show Widget, State;
import 'dart:math' deferred as math;
import 'src/io_stub.dart' if (dart.library.io) 'src/io.dart' hide Platform;
import "../util.dart";
export 'src/model.dart' show Model hide Hidden;
part 'app.g.dart';
`
	pf := parseDart(t, src)
	wantImports := []string{"package:flutter/material.dart", "dart:math", "src/io_stub.dart", "src/io.dart", "../util.dart", "src/model.dart", "app.g.dart"}
	if !reflect.DeepEqual(pf.Imports, wantImports) {
		t.Fatalf("imports = %q", pf.Imports)
	}
	var scope []string
	for _, imp := range pf.Scope.Imports {
		scope = append(scope, strings.Join([]string{imp.Kind, imp.SourceSpecifier, imp.ImportedName, imp.LocalName, boolString(imp.Wildcard), boolString(imp.ReExport)}, "|"))
	}
	want := []string{
		"namespace|package:flutter/material.dart||m|false|false",
		"named|package:flutter/material.dart|Widget|Widget|false|false",
		"named|package:flutter/material.dart|State|State|false|false",
		"dart_deferred|dart:math||math|true|false",
		"namespace|src/io_stub.dart|||true|false",
		"dart_hide|src/io_stub.dart|Platform||false|false",
		"namespace|src/io.dart|||true|false",
		"dart_hide|src/io.dart|Platform||false|false",
		"namespace|../util.dart|||true|false",
		"namespace|src/model.dart|||false|true",
		"named|src/model.dart|Model|Model|false|true",
		"dart_hide|src/model.dart|Hidden||false|true",
	}
	if !reflect.DeepEqual(scope, want) {
		t.Fatalf("scope imports:\n got %q\nwant %q", scope, want)
	}
	if len(pf.Symbols) != 0 {
		t.Fatalf("directives produced symbols: %v", dartKeys(pf))
	}

	part := parseDart(t, "part of 'app.dart';\nclass _Gen {}\n")
	if !reflect.DeepEqual(part.Imports, []string{"app.dart"}) || len(part.Scope.Imports) != 0 {
		t.Fatalf("part of = %q %+v", part.Imports, part.Scope.Imports)
	}
	// A library name is not a URI.
	named := parseDart(t, "part of app.models;\n")
	if len(named.Imports) != 0 {
		t.Fatalf("part of library name = %q", named.Imports)
	}
}

func boolString(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

func TestDartAdapterCallReferences(t *testing.T) {
	const src = `class Base {
  void go() {
    helper();
    this.run();
    super.toString();
    obj.a.b(1);
    fn<int>(2);
    new Foo();
    const Bar.named();
    Foo.named();
    widget?.call();
    'x'.shout();
    xs.map(f).toList();
    make()();
    table[0]();
    final sb = StringBuffer()
      ..write('a')
      ..writeln();
    print(widget.count);
  }
}
`
	pf := parseDart(t, src)
	want := []string{"Bar.named", "Foo", "Foo.named", "StringBuffer", "call", "fn", "helper", "make", "obj.a.b", "print", "run", "shout", "toList", "toString", "write", "writeln", "xs.map"}
	if got := dartRefs(pf); !reflect.DeepEqual(got, want) {
		t.Fatalf("references:\n got %q\nwant %q", got, want)
	}
	for _, r := range pf.References {
		if r.Name == "obj.a.b" && (r.Range.StartLine != 6 || r.Range.EndLine != 6) {
			t.Fatalf("obj.a.b range = %+v", r.Range)
		}
	}
}

// Error recovery can swallow later declarations into a broken one, so a
// declaration with an error inside it is dropped with everything it holds;
// a class whose error stays inside one closed member keeps its other members.
func TestDartAdapterSyntaxErrorsDoNotMisattributeMembers(t *testing.T) {
	swallowed := parseDart(t, `class A {
  void ok() {}
  void broken( {
  void after() {}
}
void tail() {}
`)
	for _, s := range swallowed.Symbols {
		if s.Name == "after" || s.Name == "tail" || s.Name == "broken" {
			t.Fatalf("recovered declaration recorded: %+v", s)
		}
	}

	// Only declarations that end before the first error are recorded, even
	// when the error looks contained: recovery after it is not trusted.
	contained := parseDart(t, `void before() { first(); }
class A {
  void ok() {}
  void broken() { var x = ; }
  void after() {}
}
void tail() { third(); }
`)
	assertDartKeys(t, contained, []string{"function func:dart:before"})
	if got := dartRefs(contained); !reflect.DeepEqual(got, []string{"first", "third"}) {
		t.Fatalf("references = %q", got)
	}

	// Recovery leaves declarations without their closing part; none of them
	// is recorded, nor anything swallowed by an unclosed or misspelt type.
	for _, broken := range []string{
		"int get class X {\n  void m() {}\n}\n",
		"int top  1;\nvoid after() {}\n",
		"lclass ibrary app;\n",
		"void helper(int n) {\n  if (n > 0 {\n}\n",
		"clas A {\n  void m1() {}\n}\n",
		"class A {\n  void a() {}\n\nvoid top() {}\nmixin M on B {\n  void mm() {}\n}\n",
		"enum E { one, two; void em() {}\nclass C {\n  void cm() {}\n}\n",
	} {
		if got := dartKeys(parseDart(t, broken)); len(got) != 0 {
			t.Errorf("%q recorded %q", broken, got)
		}
	}
	external := parseDart(t, "external void ext();\nint total = 1;\nint get g => 1;\n")
	assertDartKeys(t, external, []string{"function func:dart:ext", "variable value:dart:total", "function func:dart:g"})

	// Words error recovery leaves behind are not top-level variables.
	stray := parseDart(t, "part of 'lib.dart';\nimport 'a.dart' show X, Y hide Z;\nconst after = 1;\n")
	assertDartKeys(t, stray, []string{})
}

// Dart 3.8 null-aware elements, Dart 3.10 dot shorthands, `get`/`set` used as
// identifiers and labeled statements parse without an error, so declarations
// after them are recorded. A dot shorthand names no type: a shorthand call
// yields no reference rather than a guessed name.
func TestDartAdapterRecentSyntax(t *testing.T) {
	const src = `enum Color { red, green }
class P {
  P();
  P.make();
  int get get => 1;
  set set(int v) {}
  void m() { var get = 1; var set = 2; print(get + set); }
}
List<int> list(int? x, int? y) => [?x, 1, ?y];
Set<int> set1(int? x) => {?x};
Map<String, int> map(String? k, int? v) => {?k: 1, 'a': ?v, ?k: ?v};
Color c() => .red;
P p() => .new();
P q() => .make();
P r() => const .new();
int w(Color c) => switch (c) { .red => 1, .green => 2 };
void u(Color c) {
  if (c == .green) {}
  switch (c) { case .red: break; default: }
  outer: for (var i = 0; i < 3; i++) { while (true) { break outer; } }
  after();
}
var filled = List<int>.filled(3, 0);
void tail() {}
`
	root, err := parse(context.Background(), dartgrammar.GetLanguage(), []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if root.HasError() {
		t.Fatalf("parse error: %s", root.String())
	}
	pf := parseDart(t, src)
	assertDartKeys(t, pf, []string{
		"enum type:dart:Color", "enum_value value:dart:Color.red", "enum_value value:dart:Color.green",
		"class type:dart:P", "constructor func:dart:P.P", "constructor func:dart:P.make",
		"method func:dart:P.get", "method func:dart:P.set=", "method func:dart:P.m",
		"function func:dart:list", "function func:dart:set1", "function func:dart:map",
		"function func:dart:c", "function func:dart:p", "function func:dart:q", "function func:dart:r",
		"function func:dart:w", "function func:dart:u", "variable value:dart:filled", "function func:dart:tail",
	})
	if got := dartRefs(pf); !reflect.DeepEqual(got, []string{"after", "print"}) {
		t.Fatalf("references = %q", got)
	}
}

func TestDartAdapterFlutterWidget(t *testing.T) {
	const src = `import 'package:flutter/material.dart';

/// Shows a count and a button that bumps it.
class CounterPage extends StatefulWidget {
  const CounterPage({super.key, required this.title});

  final String title;

  @override
  State<CounterPage> createState() => _CounterPageState();
}

class _CounterPageState extends State<CounterPage> {
  int _count = 0;
  late final TextEditingController _controller;

  @override
  void initState() {
    super.initState();
    _controller = TextEditingController(text: '$_count');
  }

  void _increment() => setState(() => _count++);

  @override
  Widget build(BuildContext context) {
    final theme = Theme.of(context);
    return Scaffold(
      appBar: AppBar(title: Text(widget.title)),
      body: Center(
        child: Column(
          mainAxisAlignment: MainAxisAlignment.center,
          children: [
            Text('Pressed ${_count} times', style: theme.textTheme.headlineMedium),
            for (final tag in const ['a', 'b']) Chip(label: Text(tag)),
          ],
        ),
      ),
      floatingActionButton: FloatingActionButton(
        onPressed: _increment,
        child: const Icon(Icons.add),
      ),
    );
  }

  @override
  void dispose() {
    _controller.dispose();
    super.dispose();
  }
}
`
	pf := parseDart(t, src)
	assertDartKeys(t, pf, []string{
		"class type:dart:CounterPage", "constructor func:dart:CounterPage.CounterPage", "field value:dart:CounterPage.title",
		"method func:dart:CounterPage.createState",
		"class type:dart:_CounterPageState", "field value:dart:_CounterPageState._count", "field value:dart:_CounterPageState._controller",
		"method func:dart:_CounterPageState.initState", "method func:dart:_CounterPageState._increment",
		"method func:dart:_CounterPageState.build", "method func:dart:_CounterPageState.dispose",
	})
	want := []string{
		"AppBar", "Center", "Chip", "Column", "FloatingActionButton", "Icon", "Scaffold", "Text", "Text", "Text",
		"TextEditingController", "Theme.of", "_CounterPageState", "_controller.dispose", "dispose", "initState", "setState",
	}
	if got := dartRefs(pf); !reflect.DeepEqual(got, want) {
		t.Fatalf("references:\n got %q\nwant %q", got, want)
	}
	page := dartSymbolByKey(t, pf, "type:dart:CounterPage")
	if page.DocSummary != "Shows a count and a button that bumps it." || page.Signature != "class CounterPage extends StatefulWidget" {
		t.Fatalf("CounterPage = %+v", page)
	}
	if s := dartSymbolByKey(t, pf, "type:dart:_CounterPageState"); s.Visibility != "private" {
		t.Fatalf("_CounterPageState visibility = %q", s.Visibility)
	}
	build := dartSymbolByKey(t, pf, "func:dart:_CounterPageState.build")
	for _, r := range pf.References {
		if r.Name == "Scaffold" && (r.Range.StartLine < build.Range.StartLine || r.Range.EndLine > build.Range.EndLine) {
			t.Fatalf("Scaffold %+v outside build %+v", r.Range, build.Range)
		}
	}
}

func TestDartAdapterProfile(t *testing.T) {
	a := NewDart()
	if p := a.Profile(); p.ID != "treesitter:dart:v3" || !p.EmitsCallEdges {
		t.Fatalf("profile = %+v", p)
	}
	if !a.Supports("lib/main.dart") || !a.Supports("A.DART") || a.Supports("main.dart.js") {
		t.Fatal("Supports mismatch")
	}
}

// Damaging a file by one edit -- deleting any byte, or inserting a brace,
// parenthesis, newline or `class ` -- must never give a declaration a
// different owner, invent a name or truncate an import: every symbol and
// scope import a damaged file yields is one the intact file has, with the
// same kind and owner. Edits that only rename an identifier are skipped, and
// so is an edit that leaves a valid program, which declares what it spells.
func TestDartAdapterDamageNeverMisattributes(t *testing.T) {
	const src = `library app;
import 'package:a/a.dart' as a show B, C hide D;
export 'src/b.dart' show E;

/// Docs.
abstract class Shape<T> extends Base with Mix implements I {
  final int x, y;
  static const k = 1;
  Shape(this.x, this.y);
  Shape.origin() : this(0, 0);
  factory Shape.make(int v) => Shape(v, v);
  int get area => x * y;
  set area(int v) {}
  int operator +(Shape o) => 0;
  void draw() { helper(x); }
}
mixin Mix on Base {
  void mixed() {}
}
enum Color { red, green; void paint() {} }
extension Ext on String {
  int twice() => 2;
}
extension type Id(int v) {
  bool get ok => v > 0;
}
typedef Fn = int Function(int);
int top = 1;
int get total => top;
void helper(int n) {
  final list = [for (var i = 0; i < n; i++) i];
  print('n=${n} {}');
  print(list);
}
class Last {
  void end() {}
}
`
	key := func(sym graph.Symbol) string { return sym.Kind + " " + sym.StableKey + " @" + sym.ContainerName }
	imp := func(i graph.ScopeImport) string {
		return fmt.Sprintf("import %s %s as %s %s %v %v", i.SourceSpecifier, i.ImportedName, i.LocalName, i.Kind, i.Wildcard, i.ReExport)
	}
	clean := map[string]bool{}
	cleanPF := parseDart(t, src)
	for _, sym := range cleanPF.Symbols {
		clean[key(sym)] = true
	}
	for _, i := range cleanPF.Scope.Imports {
		clean[imp(i)] = true
	}
	ident := func(b byte) bool {
		return b == '_' || b == '$' || b >= '0' && b <= '9' || b >= 'a' && b <= 'z' || b >= 'A' && b <= 'Z'
	}
	type edit struct{ what, src string }
	var edits []edit
	for i := range len(src) {
		if !ident(src[i]) && !(i > 0 && i+1 < len(src) && ident(src[i-1]) && ident(src[i+1])) {
			edits = append(edits, edit{fmt.Sprintf("deleting byte %d (%q)", i, src[i]), src[:i] + src[i+1:]})
		}
		if i > 0 && ident(src[i-1]) && ident(src[i]) {
			continue // an insertion inside an identifier only splits it
		}
		for _, ins := range []string{"{", "}", "(", ")", "\n", "class "} {
			if ins == "class " && i > 0 && ident(src[i-1]) {
				continue // "class" would extend the identifier before it
			}
			edits = append(edits, edit{fmt.Sprintf("inserting %q at %d", ins, i), src[:i] + ins + src[i:]})
		}
	}
	check := func(e edit) {
		pf, err := NewDart().Parse(context.Background(), "damaged.dart", []byte(e.src))
		if err != nil {
			t.Error(err)
			return
		}
		var wrong []string
		for _, sym := range pf.Symbols {
			if k := key(sym); !clean[k] {
				wrong = append(wrong, k)
			}
		}
		for _, i := range pf.Scope.Imports {
			if k := imp(i); !clean[k] {
				wrong = append(wrong, k)
			}
		}
		if len(wrong) == 0 {
			return
		}
		if root, err := parse(context.Background(), dartgrammar.GetLanguage(), []byte(e.src)); err == nil && !root.HasError() {
			return // a valid program declares what it spells
		}
		t.Errorf("%s yields %q", e.what, wrong)
	}
	// Each parse is independent; spreading them keeps the test short.
	var wg sync.WaitGroup
	workers := runtime.GOMAXPROCS(0)
	for w := range workers {
		wg.Go(func() {
			for j := w; j < len(edits); j += workers {
				check(edits[j])
			}
		})
	}
	wg.Wait()
}
