//go:build cgo

package treesitter

import (
	"context"
	"fmt"
	"reflect"
	"strings"
	"testing"

	sitter "github.com/smacker/go-tree-sitter"
	cpp "github.com/smacker/go-tree-sitter/cpp"

	"github.com/isink17/codegraph/internal/graph"
)

// cppLanguageForTest exposes the same C++ grammar the adapter uses, so a fixture
// can assert on the raw parse (for example, that a recovery fixture still
// produces an ERROR node).
func cppLanguageForTest() *sitter.Language { return cpp.GetLanguage() }

// TestCppCallDstNameKeepsReceiver pins the persisted destination identity for
// every C++ call syntax the adapter emits (P22.11).
//
// The contract: a call written with a receiver keeps that receiver in
// `dst_name`. Before P22.11 the adapter returned the bare tail for `.` and `->`
// calls and kept the receiver only in `evidence`, so `v.size()` entered the
// resolver as `size` and bound whichever project method named `size` was
// unique. `::`-qualified, bare, and non-call syntax are unchanged.
func TestCppCallDstNameKeepsReceiver(t *testing.T) {
	src := `namespace ns { struct Type { void foo() {} static void sfoo() {} }; }
struct Base { void foo() {} };
struct Obj : Base {
    Obj* child() { return this; }
    Obj* kid;
    int field;
    void foo() {}
    void caller();
};
void bare() {}
void Obj::caller() {
    bare();
    Obj o;
    o.foo();
    Obj* p = &o;
    p->foo();
    ns::Type::sfoo();
    ::bare();
    this->foo();
    o.kid->foo();
    p->child()->foo();
    o
        .foo();
    this->Base::foo();
    int x = o.field;
    x = p->field;
}
`
	pf, err := NewCpp().Parse(context.Background(), "syn.cpp", []byte(src))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}

	var got []string
	for _, e := range pf.Edges {
		if e.Kind != "calls" {
			continue
		}
		if strings.HasPrefix(e.Evidence, "member_") && strings.ContainsAny(e.DstName, "\n\r") {
			t.Errorf("member-call dst_name %q contains a newline", e.DstName)
		}
		got = append(got, e.DstName)
	}

	want := []string{
		"bare",           // bare call
		"o.foo",          // instance dot
		"p.foo",          // pointer arrow, normalized to '.'
		"ns::Type::sfoo", // namespace + type, scope qualifier untouched
		"bare",           // leading '::' global qualification
		"this.foo",       // this-> receiver: kept, and not a type
		"o.kid.foo",      // chained member
		"p.child().foo",  // outer call of a chain (outer node first)
		"p.child",        // inner call of the same chain
		"o.foo",          // multi-line receiver, whitespace collapsed
		// Scope-qualified spelling: persisted verbatim, arrow and all. Predates
		// P22.11 and is left alone -- it already carries a "::" qualifier, so it
		// fails closed at every level the member rules gate on.
		"this->Base::foo",
	}
	if len(got) != len(want) {
		t.Fatalf("call dst_names = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("call dst_names = %q, want %q", got, want)
		}
	}

	// Field access is not a call: preserving receivers must not fabricate one.
	for _, e := range pf.Edges {
		if e.DstName == "o.field" || e.DstName == "p.field" {
			t.Fatalf("field access produced a %q edge to %q", e.Kind, e.DstName)
		}
	}
}

// TestCppCallEvidenceFormat pins `evidence` for the three call shapes. The
// P22.11 upgrade path deliberately does NOT parse this string -- it reparses
// the file instead -- but the audits that classified the receiver-discard
// bindings read it, so its shape is a contract.
func TestCppCallEvidenceFormat(t *testing.T) {
	src := `struct A { void m() {} void c(); };
void free_fn() {}
void A::c() {
    free_fn();
    A a;
    a.m();
    A* p = &a;
    p->m();
    A::c();
}
`
	pf, err := NewCpp().Parse(context.Background(), "a.cpp", []byte(src))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	want := map[string]string{
		"free_fn": "direct:free_fn",
		"a.m":     "member_value:a:a.m",
		"p.m":     "member_pointer:p:p->m",
		"A::c":    "static_qualified:A::c",
	}
	seen := map[string]string{}
	for _, e := range pf.Edges {
		if e.Kind == "calls" {
			seen[e.DstName] = e.Evidence
		}
	}
	for name, ev := range want {
		if seen[name] != ev {
			t.Errorf("evidence for %q = %q, want %q", name, seen[name], ev)
		}
	}
}

// TestCppCFieldAccessIsNotACall guards the C control: the adapter also serves
// `.c`, where `x.field` is a struct member read and never a call.
func TestCppCFieldAccessIsNotACall(t *testing.T) {
	src := `struct S { int n; };
int get(struct S* s) { return s->n; }
int use(struct S v) { return v.n + get(&v); }
`
	pf, err := NewCpp().Parse(context.Background(), "s.c", []byte(src))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	var calls []string
	for _, e := range pf.Edges {
		if e.Kind == "calls" {
			calls = append(calls, e.DstName)
		}
	}
	if len(calls) != 1 || calls[0] != "get" {
		t.Fatalf("C call dst_names = %q, want [get]", calls)
	}
}

// symbolIdentity renders one emitted symbol as
// `kind name qualified_name container_name start-end` so a fixture can pin
// identity, ownership and range together.
func symbolIdentity(sym graph.Symbol) string {
	return fmt.Sprintf("%s %s %s %s %d:%d-%d:%d",
		sym.Kind, sym.Name, sym.QualifiedName, sym.ContainerName,
		sym.Range.StartLine, sym.Range.StartCol, sym.Range.EndLine, sym.Range.EndCol)
}

func parseCppSymbols(t *testing.T, path, src string) []graph.Symbol {
	t.Helper()
	pf, err := NewCpp().Parse(context.Background(), path, []byte(src))
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	return pf.Symbols
}

// TestCppPreprocessorArmsEmitDeclarations pins the preprocessor half of the
// P22.15 wrapper contract: every arm of every conditional is a declaration
// scope, and none of the four arm node types hides its declarations.
func TestCppPreprocessorArmsEmitDeclarations(t *testing.T) {
	src := `#if FLAG
void one();
#endif
#ifdef FLAG
void two();
#else
void three();
#endif
#ifndef FLAG
void four();
#endif
#if A
void five();
#elif B
void six();
#else
void seven();
#endif
`
	var got []string
	for _, sym := range parseCppSymbols(t, "pp.cpp", src) {
		got = append(got, sym.Name)
	}
	want := []string{"one", "two", "three", "four", "five", "six", "seven"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("preprocessor-arm symbols = %q, want %q", got, want)
	}
}

// TestCppTemplateWrapperEmitsWrappedDeclaration pins the template half: the
// wrapped declaration is the symbol, the `template <...>` wrapper never becomes
// one, and a templated class still owns its members.
func TestCppTemplateWrapperEmitsWrappedDeclaration(t *testing.T) {
	src := `// doc for plain
template <typename T>
void plain(T);
// doc for Box
template <typename T>
class Box {
  void run() {}
};
template <typename T>
void defined(T) {}
`
	syms := parseCppSymbols(t, "tpl.cpp", src)
	var got []string
	for _, sym := range syms {
		got = append(got, symbolIdentity(sym))
	}
	want := []string{
		// The range and the doc comment anchor on the `template <...>` wrapper,
		// because that is where the declaration -- and its documentation --
		// actually starts in the source.
		"function plain tpl::plain tpl 2:1-3:15",
		"class Box tpl::Box tpl 5:1-8:3",
		// Member of a templated class: the class still owns it.
		"function run Box::run Box 7:3-7:16",
		// A definition keeps its body range.
		"function defined tpl::defined tpl 9:1-10:19",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("template symbols = %#v, want %#v", got, want)
	}
	docs := map[string]string{}
	for _, sym := range syms {
		docs[sym.Name] = sym.DocSummary
		if strings.HasPrefix(sym.Name, "template") {
			t.Errorf("template wrapper emitted a symbol: %+v", sym)
		}
	}
	// Anchoring on the inner declaration instead would make its previous sibling
	// the `template_parameter_list`, silently dropping every doc comment.
	if docs["plain"] != "doc for plain" || docs["Box"] != "doc for Box" {
		t.Errorf("template doc summaries = %#v, want plain/Box docs preserved", docs)
	}
}

// TestCppLinkageSpecificationEmitsDeclarations pins `extern "C"`: both the
// braced and the single-declaration form emit, and `"C"` never becomes a
// container.
func TestCppLinkageSpecificationEmitsDeclarations(t *testing.T) {
	src := `extern "C" {
void braced();
}
extern "C" void single();
`
	var got []string
	for _, sym := range parseCppSymbols(t, "ext.cpp", src) {
		got = append(got, symbolIdentity(sym))
		if strings.Contains(sym.QualifiedName, `"C"`) || sym.ContainerName == "C" {
			t.Errorf(`linkage label leaked into identity: %+v`, sym)
		}
	}
	want := []string{
		"function braced ext::braced ext 2:1-2:15",
		"function single ext::single ext 4:12-4:26",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("linkage symbols = %#v, want %#v", got, want)
	}
}

// TestCppWrapperPreservesSemanticContainer pins the ownership half: a class,
// struct or namespace member reached through a transparent wrapper keeps the
// same owner it would have without the wrapper. No fallback to file scope.
func TestCppWrapperPreservesSemanticContainer(t *testing.T) {
	src := `class Cls {
#if X
  void wrapped() {}
#endif
  void plain() {}
};
struct Str {
#ifdef X
  void wrapped() {}
#endif
};
namespace ns {
#if X
void nsFn();
#endif
}
`
	var got []string
	for _, sym := range parseCppSymbols(t, "own.cpp", src) {
		got = append(got, sym.Kind+" "+sym.QualifiedName+" in "+sym.ContainerName)
	}
	want := []string{
		"class own::Cls in own",
		"function Cls::wrapped in Cls",
		"function Cls::plain in Cls",
		"struct own::Str in own",
		"function Str::wrapped in Str",
		// P22.16 owns real namespace identity; the current model files a
		// namespace member under the module, wrapped or not.
		"function own::nsFn in own",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("container ownership = %#v, want %#v", got, want)
	}
}

// TestCppClassMemberDeclarationStaysOut records the boundary of the ownership
// contract above: it holds for a member *definition*, because that is a
// `function_definition`. A bodiless member declaration is a `field_declaration`,
// which the adapter does not emit at all -- wrapped or not -- so the wrapper
// traversal cannot restore it either.
//
// That is the header-declaration / `.cc`-definition gap P22.17 owns, reached
// through a different node than the `declaration` case. Pinned here so it is a
// recorded limit rather than something the container fixture quietly avoids by
// only ever using inline definitions.
func TestCppClassMemberDeclarationStaysOut(t *testing.T) {
	src := `class Cls {
#if X
  void wrappedDecl();
#endif
  void plainDecl();
  void definition() {}
};
`
	var got []string
	for _, sym := range parseCppSymbols(t, "decl.cpp", src) {
		got = append(got, sym.Kind+" "+sym.QualifiedName)
	}
	want := []string{"class decl::Cls", "function Cls::definition"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("class-body symbols = %#v, want %#v (member declarations are P22.17)", got, want)
	}
}

// TestCppNestedWrappersComposeAndEmitOnce pins §11 and §14 together: wrappers
// nest four deep in a syntactically valid combination, the declaration inside
// is reached, and it is emitted exactly once.
func TestCppNestedWrappersComposeAndEmitOnce(t *testing.T) {
	src := `#if OUTER
#ifdef INNER
namespace ns {
extern "C" {
template <typename T>
void deep(T);
}
}
#endif
#endif
`
	syms := parseCppSymbols(t, "nest.cpp", src)
	var got []string
	for _, sym := range syms {
		got = append(got, symbolIdentity(sym))
	}
	want := []string{"function deep nest::deep nest 5:1-6:14"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("nested-wrapper symbols = %#v, want %#v", got, want)
	}
}

// TestCppWrapperDeclarationsEmitOnce pins that no declaration reachable through
// a wrapper is emitted twice, across every wrapper class and both container
// kinds. Duplicates are counted by semantic identity plus position, which is
// exactly the key the store uses for a symbol row.
func TestCppWrapperDeclarationsEmitOnce(t *testing.T) {
	src := `namespace ns {
#if X
template <typename T>
void inNamespace(T);
#endif
}
extern "C" {
#ifdef Y
void inLinkage();
#endif
}
class Cls {
#if X
template <typename T>
void inClass(T);
#endif
};
`
	seen := map[string]int{}
	for _, sym := range parseCppSymbols(t, "once.cpp", src) {
		seen[symbolIdentity(sym)]++
	}
	if len(seen) == 0 {
		t.Fatal("no symbols emitted")
	}
	for identity, n := range seen {
		if n != 1 {
			t.Errorf("symbol %q emitted %d times, want 1", identity, n)
		}
	}
	for _, name := range []string{"inNamespace", "inLinkage", "inClass"} {
		found := false
		for identity := range seen {
			if strings.Contains(identity, " "+name+" ") {
				found = true
			}
		}
		if !found {
			t.Errorf("declaration %q not emitted", name)
		}
	}
}

// TestCppMutuallyExclusiveArmsStayOrderIndependent pins §20: two arms declaring
// the same name both emit, and reversing the arms produces the same set of
// symbol identities modulo position. Nothing downstream may pick "the first
// branch" -- the resolver's bare-name ambiguity veto sees two same-language
// candidates and refuses the call.
func TestCppMutuallyExclusiveArmsStayOrderIndependent(t *testing.T) {
	// The arm bodies are deliberately distinguishable (`first`/`second`) so the
	// order comparison is not satisfied by identical text: reversing the arms
	// swaps which body the parser meets first, and the assertion below is that
	// the emitted set is unchanged rather than merely equal by construction.
	forward := `#if A
void dup() { first(); }
#else
void dup() { second(); }
#endif
`
	reversed := `#if A
void dup() { second(); }
#else
void dup() { first(); }
#endif
`
	// Identity without the position, so the two arms are compared on what the
	// resolver actually keys on rather than on which line they sit.
	identities := func(src string) []string {
		var out []string
		for _, sym := range parseCppSymbols(t, "amb.cpp", src) {
			out = append(out, sym.Kind+" "+sym.QualifiedName+" "+sym.ContainerName+" "+sym.StableKey)
		}
		return out
	}
	fwd, rev := identities(forward), identities(reversed)
	dupCount := func(ids []string) int {
		n := 0
		for _, id := range ids {
			if strings.Contains(id, "::dup ") {
				n++
			}
		}
		return n
	}
	if got := dupCount(fwd); got != 2 {
		t.Fatalf("forward arms emitted %d `dup` symbols, want 2 (got %#v)", got, fwd)
	}
	if !reflect.DeepEqual(fwd, rev) {
		t.Fatalf("arm order changed symbol identities:\n forward  = %#v\n reversed = %#v", fwd, rev)
	}
	// Both arms must produce the *same* stable key, so nothing downstream can
	// tell them apart by identity and quietly prefer one. Two rows sharing a
	// key is what makes the name ambiguous at the resolver's bare-name level.
	if fwd[0] != fwd[1] {
		t.Fatalf("mutually exclusive arms produced distinguishable identities: %#v", fwd[:2])
	}
	// Both arm bodies must be walked, in either order: if one arm were skipped,
	// its call would vanish and the sets would differ.
	callSet := func(src string) map[string]int {
		pf, err := NewCpp().Parse(context.Background(), "amb.cpp", []byte(src))
		if err != nil {
			t.Fatalf("Parse() error = %v", err)
		}
		out := map[string]int{}
		for _, e := range pf.Edges {
			if e.Kind == "calls" {
				out[e.DstName]++
			}
		}
		return out
	}
	wantCalls := map[string]int{"first": 1, "second": 1}
	if got := callSet(forward); !reflect.DeepEqual(got, wantCalls) {
		t.Fatalf("forward arm calls = %v, want %v", got, wantCalls)
	}
	if got := callSet(reversed); !reflect.DeepEqual(got, wantCalls) {
		t.Fatalf("reversed arm calls = %v, want %v", got, wantCalls)
	}
}

// TestCppErrorRecoveryEmitsDeclarations pins the ERROR wrapper, which is what
// hid 7500 lines of gtest_unittest.cc: the file's anonymous namespace is
// reparented under one ERROR node spanning lines 317-7870.
//
// The fixture is the real shape from `googlemock/src/gmock_main.cc`: a function
// signature split across `#ifdef`/`#else`, so the opening brace belongs to one
// arm and the body to neither. No grammar can represent that, and tree-sitter
// answers with an ERROR node holding the declarations the source really has.
// Skipping the node is what loses them.
func TestCppErrorRecoveryEmitsDeclarations(t *testing.T) {
	src := `#ifdef WIDE
int tmain(int argc, char** argv) {
#else
int main(int argc, char** argv) {
#endif
  return 0;
}
`
	root, err := parse(context.Background(), cppLanguageForTest(), []byte(src))
	if err != nil {
		t.Fatalf("parse() error = %v", err)
	}
	if !root.HasError() {
		t.Fatal("fixture no longer produces a parse ERROR; pick another recovery shape")
	}
	if firstChild(root, "ERROR") == nil {
		t.Fatal("fixture no longer puts its declarations under a top-level ERROR node")
	}

	var got []string
	for _, sym := range parseCppSymbols(t, "recover.cpp", src) {
		got = append(got, symbolIdentity(sym))
	}
	want := []string{"function main recover::main recover 4:1-7:2"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("recovered symbols = %#v, want %#v", got, want)
	}
}

// TestCppCLanguageWrappersEmitDeclarations is the C control: the adapter serves
// `.c` with the C grammar, where the preprocessor and `extern` wrappers are the
// same shapes and must behave the same way.
func TestCppCLanguageWrappersEmitDeclarations(t *testing.T) {
	src := `#if FLAG
int guarded(void);
#endif
#ifdef OTHER
int ifdefed(void);
#else
int elsed(void);
#endif
extern int externed(void);
struct S {
  int n;
};
`
	var got []string
	for _, sym := range parseCppSymbols(t, "c.c", src) {
		got = append(got, sym.Kind+" "+sym.QualifiedName+" in "+sym.ContainerName)
	}
	want := []string{
		"function c::guarded in c",
		"function c::ifdefed in c",
		"function c::elsed in c",
		"function c::externed in c",
		"struct c::S in c",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("C symbols = %#v, want %#v", got, want)
	}
}

// TestCppBodyLocalDeclarationsStayLocal pins §13: the walk must not descend
// into executable bodies. A local class or function inside a function body is
// not a file-scope declaration, and a wrapper inside a body does not make it
// one.
func TestCppBodyLocalDeclarationsStayLocal(t *testing.T) {
	src := `void host() {
  struct Local { void member() {} };
#if X
  struct Guarded { void guardedMember() {} };
#endif
  int local = 0;
}
`
	var got []string
	for _, sym := range parseCppSymbols(t, "body.cpp", src) {
		got = append(got, sym.Name)
	}
	want := []string{"host"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("body-local symbols = %q, want %q", got, want)
	}
}

// TestCppUnsupportedDeclarationContainersStayOut pins the OUT half of the
// P22.15 taxonomy audit, so a later phase that decides to support one of these
// changes this fixture deliberately instead of discovering the behaviour.
func TestCppUnsupportedDeclarationContainersStayOut(t *testing.T) {
	src := `struct Embedded { int n; } instance;
typedef struct { int n; } Typedefed;
template <typename T> class Tpl { public: void m() {} };
template class Tpl<int>;
class Host {
  friend void friended() {}
};
`
	names := map[string]bool{}
	for _, sym := range parseCppSymbols(t, "out.cpp", src) {
		names[sym.Name] = true
	}
	// Embedded/Typedefed sit inside a leaf `declaration`/`type_definition`;
	// `friended` is declared at the enclosing namespace scope, not on Host, so
	// emitting it here would attach a wrong container (P22.16 owns namespaces).
	for _, name := range []string{"Embedded", "Typedefed", "friended"} {
		if names[name] {
			t.Errorf("%q is emitted; the taxonomy records it as out of scope", name)
		}
	}
	// The explicit instantiation must not duplicate the template it names.
	if got := len(names); !names["Tpl"] || !names["m"] || !names["Host"] || got != 3 {
		t.Errorf("emitted names = %v, want exactly Tpl, m, Host", names)
	}
}

// benchCppSource is a wrapper-heavy C++ file shaped like the real headers this
// phase measured: preprocessor arms nested inside namespaces and classes,
// templates, and a linkage block, with call sites so the whole adapter runs.
// It also opens with the gmock_main shape -- a function signature split across
// `#ifdef`/`#else` -- so the whole file lands under one wide-fanout `ERROR`
// recovery node. That is the path P22.15 made hot and the one where a
// per-symbol scan of the parent's children turns quadratic, so a regression
// there shows up here rather than only on a real 7870-line file.
func benchCppSource(units int) []byte {
	var b strings.Builder
	b.WriteString("#include \"dep.h\"\n")
	b.WriteString("#ifdef WIDE\nint tmain(int argc, char** argv) {\n#else\nint main(int argc, char** argv) {\n#endif\n  return 0;\n}\n")
	// Top-level declarations after the unparseable signature: these become
	// direct children of the one ERROR node, which is what gives it the wide
	// fanout the real gtest_unittest.cc ERROR has (914 direct children).
	for i := 0; i < units; i++ {
		fmt.Fprintf(&b, "// doc for flat%[1]d\nstruct Flat%[1]d {\n  void member() {}\n};\nvoid flat%[1]d() { linked%[1]d(); }\n", i)
	}
	for i := 0; i < units; i++ {
		fmt.Fprintf(&b, `
namespace ns%[1]d {
#if CONFIG_A
template <typename T>
class Holder%[1]d {
 public:
#ifdef FEATURE
  void guarded() { helper%[1]d(); }
#else
  void alternate() { helper%[1]d(); }
#endif
  void plain() { guarded(); }
};
extern "C" {
void linked%[1]d();
}
#elif CONFIG_B
void fallback%[1]d() { linked%[1]d(); }
#else
struct Plain%[1]d {
  void method() {}
};
#endif
void driver%[1]d() {
  Holder%[1]d<int> h;
  h.plain();
  fallback%[1]d();
}
}
`, i)
	}
	return []byte(b.String())
}

func BenchmarkCppAdapterParse(b *testing.B) {
	src := benchCppSource(120)
	adapter := NewCpp()
	ctx := context.Background()
	b.SetBytes(int64(len(src)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		pf, err := adapter.Parse(ctx, "bench.cpp", src)
		if err != nil {
			b.Fatal(err)
		}
		if len(pf.Symbols) == 0 {
			b.Fatal("no symbols")
		}
	}
}
