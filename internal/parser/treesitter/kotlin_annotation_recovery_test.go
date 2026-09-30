//go:build cgo

package treesitter

import (
	"context"
	"reflect"
	"strings"
	"testing"

	kotlin "github.com/smacker/go-tree-sitter/kotlin"

	"github.com/isink17/codegraph/internal/graph"
)

// kotlinDeclarationFacts is everything a recovered declaration must share with
// the same declaration parsed with its annotations attached. Range is left out:
// a recovered symbol keeps its declaration node's coordinates.
type kotlinDeclarationFacts struct {
	Kind, Name, QualifiedName, StableKey string
	Signature, Visibility, DocSummary    string
	ArityMin, ArityMax                   int // -1 unknown
	NameFact                             *graph.KotlinJVMNameEvidence
	CallableFact                         *graph.KotlinJVMCallableEvidence
	Facade                               graph.JVMFileFacade
}

func kotlinFactsFor(t *testing.T, p graph.ParsedFile, name string) kotlinDeclarationFacts {
	t.Helper()
	index := -1
	for i, s := range p.Symbols {
		if s.Name == name && (s.Kind == "function" || s.Kind == "object") {
			index = i
		}
	}
	if index < 0 {
		t.Fatalf("%s not parsed: %+v", name, p.Symbols)
	}
	s := p.Symbols[index]
	facts := kotlinDeclarationFacts{Kind: s.Kind, Name: s.Name, QualifiedName: s.QualifiedName, StableKey: s.StableKey, Signature: s.Signature, Visibility: s.Visibility, DocSummary: s.DocSummary, ArityMin: -1, ArityMax: -1, Facade: p.Scope.JVMFacade}
	if s.ArityMin != nil {
		facts.ArityMin = *s.ArityMin
	}
	if s.ArityMax != nil {
		facts.ArityMax = *s.ArityMax
	}
	for _, fact := range p.KotlinJVMNameEvidence {
		if fact.SymbolIndex == index {
			fact.SymbolIndex = 0
			facts.NameFact = &fact
		}
	}
	for _, fact := range p.KotlinJVMCallableEvidence {
		if fact.SymbolIndex == index {
			fact.SymbolIndex = 0
			facts.CallableFact = &fact
		}
	}
	return facts
}

func kotlinRecoveredViews(t *testing.T, src string) (map[uint32]kotlinDeclarationView, bool) {
	t.Helper()
	root, err := parse(context.Background(), kotlin.GetLanguage(), []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return kotlinRootDeclarations(root, []byte(src), kotlinRecovery{detached: true, swallowed: true})
}

// A later top-level declaration is what makes the grammar split an own-line
// `@Ann(arg)` from its `fun`/`object`; the last declaration of a file keeps it
// attached. Each source below is therefore its own oracle: parsed as the file's
// last declaration it is unsplit, and with a declaration appended it must be
// recovered to exactly the same facts. The recovery is annotation-name
// agnostic, so the table spans ABI-neutral annotations, JvmName's known and
// unknown forms, and the ABI-sensitive ones.
func TestKotlinDetachedAnnotationRecoveryMatchesAttachedParse(t *testing.T) {
	for _, tc := range []struct{ name, decl, target string }{
		{"JvmName", "@JvmName(\"execute\")\nfun run(x: kotlin.Int) {}", "run"},
		{"Deprecated", "@Deprecated(\"x\")\nfun run(x: kotlin.Int) {}", "run"},
		{"Throws", "@Throws(Exception::class)\nfun run(x: kotlin.Int) {}", "run"},
		{"Suppress", "@Suppress(\"UNUSED\")\nfun run(x: kotlin.Int) {}", "run"},
		{"OptIn", "@OptIn(ExperimentalStdlibApi::class)\nfun run(x: kotlin.Int) {}", "run"},
		{"zero parameters", "@JvmName(\"execute\")\nfun run() {}", "run"},
		{"expression body", "@JvmName(\"execute\")\nfun run(x: kotlin.Int): kotlin.Int = x", "run"},
		{"generic", "@JvmName(\"execute\")\nfun <T> run(x: T) {}", "run"},
		{"extension", "@JvmName(\"execute\")\nfun kotlin.String.run(x: kotlin.Int) {}", "run"},
		{"argument-less then argument", "@JvmOverloads\n@JvmName(\"execute\")\nfun run(x: kotlin.Int = 0) {}", "run"},
		{"argument, argument-less, argument", "@Deprecated(\"x\")\n@JvmOverloads\n@JvmName(\"execute\")\nfun run(x: kotlin.Int = 0) {}", "run"},
		{"three argument annotations", "@Deprecated(\"x\")\n@Suppress(\"UNUSED\")\n@JvmName(\"execute\")\nfun run(x: kotlin.Int) {}", "run"},
		{"JvmSynthetic then argument", "@JvmSynthetic\n@Suppress(\"x\")\nfun run() {}", "run"},
		{"JvmSynthetic then JvmName", "@JvmSynthetic\n@JvmName(\"execute\")\nfun run(x: kotlin.Int) {}", "run"},
		{"same-line pair before newline fun", "@JvmSynthetic @Suppress(\"x\")\nfun run() {}", "run"},
		{"blank line", "@JvmName(\"execute\")\n\nfun run(x: kotlin.Int) {}", "run"},
		{"blank lines", "@JvmName(\"execute\")\n\n\n\nfun run(x: kotlin.Int) {}", "run"},
		{"line comment", "@JvmName(\"execute\")\n// comment\nfun run(x: kotlin.Int) {}", "run"},
		{"block comment", "@JvmName(\"execute\")\n/* c */\nfun run(x: kotlin.Int) {}", "run"},
		{"KDoc between", "@JvmName(\"execute\")\n/** doc */\nfun run(x: kotlin.Int) {}", "run"},
		{"trailing comment", "@JvmName(\"execute\") // c\nfun run(x: kotlin.Int) {}", "run"},
		{"qualified JvmName", "@kotlin.jvm.JvmName(\"execute\")\nfun run(x: kotlin.Int) {}", "run"},
		{"aliased JvmName", "@JN(\"execute\")\nfun run(x: kotlin.Int) {}", "run"},
		{"raw string stays unknown", "@JvmName(\"\"\"execute\"\"\")\nfun run(x: kotlin.Int) {}", "run"},
		{"escape stays unknown", "@JvmName(\"exec\\u0075te\")\nfun run(x: kotlin.Int) {}", "run"},
		{"template stays unknown", "@JvmName(\"${N}x\")\nfun run(x: kotlin.Int) {}", "run"},
		{"concatenation stays unknown", "@JvmName(\"exe\" + \"cute\")\nfun run(x: kotlin.Int) {}", "run"},
		{"parenthesized argument stays unknown", "@JvmName((\"execute\"))\nfun run(x: kotlin.Int) {}", "run"},
		{"deep chain with argument-less link", "@Deprecated(\"x\")\n@Suppress(\"y\")\n@JvmOverloads\n@JvmName(\"execute\")\nfun run(x: kotlin.Int = 0) {}", "run"},
		{"repeated JvmName stays unknown", "@JvmName(\"a\") @JvmName(\"b\")\nfun run(x: kotlin.Int) {}", "run"},
		{"object", "@Deprecated(\"x\")\nobject O", "O"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Earlier declarations change how the grammar resolves the
			// split, so the preamble is only package and imports.
			preamble := "package lib\n"
			if strings.Contains(tc.decl, "@JN") {
				preamble += "import kotlin.jvm.JvmName as JN\n"
			}
			attached := preamble + tc.decl + "\n"
			split := attached + "fun other() {}\n"
			if views, clean := kotlinRecoveredViews(t, attached); !clean || len(views) != 0 {
				t.Fatalf("oracle is not an attached parse: clean=%v views=%d", clean, len(views))
			}
			if views, clean := kotlinRecoveredViews(t, split); !clean || len(views) != 1 {
				t.Fatalf("appended declaration did not produce one recovered split: clean=%v views=%d", clean, len(views))
			}
			want := kotlinParseFacts(t, NewKotlin(), attached, tc.target)
			got := kotlinParseFacts(t, NewKotlin(), split, tc.target)
			if tc.target == "O" {
				// An object alone owns no facade; the appended function does.
				want.Facade = graph.JVMFileFacade{Class: "ActionsKt"}
			}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("recovered facts differ from attached parse\n got %+v %+v %+v\nwant %+v %+v %+v", got, got.NameFact, got.CallableFact, want, want.NameFact, want.CallableFact)
			}
			if got.Facade.Class != "ActionsKt" {
				t.Fatalf("facade = %+v", got.Facade)
			}
		})
	}
}

func kotlinParseFacts(t *testing.T, a *KotlinAdapter, src, target string) kotlinDeclarationFacts {
	t.Helper()
	p, err := a.Parse(context.Background(), "lib/Actions.kt", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	return kotlinFactsFor(t, p, target)
}

// Recovered facts that the attached oracle also proves, pinned so the parity
// test above cannot pass by both sides losing them.
func TestKotlinDetachedAnnotationRecoveredFacts(t *testing.T) {
	split := func(decl string) string { return "package lib\n" + decl + "\nfun other() {}\n" }
	run := kotlinParseFacts(t, NewKotlin(), split("@JvmName(\"execute\")\nfun run(x: kotlin.Int) {}"), "run")
	if run.NameFact == nil || !run.NameFact.Known || run.NameFact.JVMName != "execute" || run.ArityMin != 1 || run.Signature != "@JvmName(\"execute\")\nfun run(x: kotlin.Int) {}" {
		t.Fatalf("JvmName recovery = %+v %+v", run, run.NameFact)
	}
	over := kotlinParseFacts(t, NewKotlin(), split("@JvmOverloads\n@JvmName(\"execute\")\nfun run(x: kotlin.Int = 0) {}"), "run")
	if over.NameFact == nil || over.NameFact.JVMName != "execute" || over.CallableFact == nil || !over.CallableFact.Known || over.CallableFact.ArityMin != 0 || over.CallableFact.ArityMax != 1 {
		t.Fatalf("JvmOverloads recovery = %+v %+v %+v", over, over.NameFact, over.CallableFact)
	}
	hidden := kotlinParseFacts(t, NewKotlin(), split("@JvmSynthetic\n@Suppress(\"x\")\nfun run() {}"), "run")
	if !strings.HasPrefix(hidden.Signature, "@JvmSynthetic") || hidden.ArityMin != -1 {
		t.Fatalf("JvmSynthetic recovery = %+v", hidden)
	}
	// A comment directly after the package header is part of that node, so
	// the documented declaration follows another one.
	doc := kotlinParseFacts(t, NewKotlin(), split("fun first() {}\n// Runs it.\n@Deprecated(\"x\")\nfun run(x: kotlin.Int) {}"), "run")
	between := kotlinParseFacts(t, NewKotlin(), split("fun first() {}\n@Deprecated(\"x\")\n// not a doc\nfun run(x: kotlin.Int) {}"), "run")
	attached := kotlinParseFacts(t, NewKotlin(), split("fun first() {}\n// Runs it.\n@Deprecated(\"x\") fun run(x: kotlin.Int) {}"), "run")
	if attached.DocSummary != "Runs it." || doc.Signature != "@Deprecated(\"x\")\nfun run(x: kotlin.Int) {}" {
		t.Fatalf("attached doc = %q, recovered signature = %q", attached.DocSummary, doc.Signature)
	}
	if doc.DocSummary != "Runs it." || between.DocSummary != "" {
		t.Fatalf("doc anchors = %q, %q", doc.DocSummary, between.DocSummary)
	}
	for _, tc := range []struct{ name, imports string }{
		{"alias hides simple name", "import kotlin.jvm.JvmName as JN\n"},
		{"foreign import shadows", "import other.JvmName\n"},
	} {
		p, err := NewKotlin().Parse(context.Background(), "lib/Actions.kt", []byte("package lib\n"+tc.imports+"@JvmName(\"execute\")\nfun run(x: kotlin.Int) {}\nfun other() {}\n"))
		if err != nil {
			t.Fatal(err)
		}
		facts := kotlinFactsFor(t, p, "run")
		if facts.NameFact == nil || facts.NameFact.Known || facts.ArityMin != -1 {
			t.Fatalf("%s = %+v %+v", tc.name, facts, facts.NameFact)
		}
	}
	foreign := kotlinParseFacts(t, NewKotlin(), "package lib\nimport other.Named as JN\n@JN(\"execute\")\nfun run(x: kotlin.Int) {}\nfun other() {}\n", "run")
	if foreign.NameFact != nil || foreign.ArityMin != 1 || foreign.Facade.Class != "ActionsKt" {
		t.Fatalf("foreign alias = %+v %+v", foreign, foreign.NameFact)
	}
	// Coordinates stay on the declaration node; only the signature starts at
	// the recovered annotation.
	p, err := NewKotlin().Parse(context.Background(), "lib/Actions.kt", []byte(split("@Deprecated(\"x\")\nfun run(x: kotlin.Int) {}")))
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range p.Symbols {
		if s.Name == "run" && (s.Range.StartLine != 3 || s.StableKey != "func:kotlin:lib.run" || s.QualifiedName != "lib.run") {
			t.Fatalf("recovered symbol identity = %+v", s)
		}
	}
}

// Everything outside the proven split stays fail closed: no facade, and the
// declaration keeps the facts the unrecovered tree gives it.
func TestKotlinDetachedAnnotationRecoveryRefusals(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"top-level call", "println(1)\nfun run() {}"},
		{"top-level navigation call", "foo.bar()\nfun run() {}"},
		{"top-level arithmetic", "1 + 2\nfun run() {}"},
		{"annotation on unary expression", "@Ann(\"x\") + y\nfun run() {}"},
		{"annotation on call", "@A(\"x\") foo()\nfun run() {}"},
		{"annotation on next-line call", "@A(\"x\")\nfoo()\nfun run() {}"},
		{"annotation on identifier", "@JvmName(\"a\") nonsense\nfun run() {}"},
		{"annotation on lambda", "@A(\"x\") {}\nfun run() {}"},
		{"annotation on parenthesized expression", "@Deprecated(\"x\") (1)\nfun run() {}"},
		{"label", "l@ (\"x\")\nfun run() {}"},
		{"anonymous function", "@Deprecated(\"x\")\nfun() {}"},
		{"object literal", "interface I\n@Deprecated(\"x\")\nobject : I {}"},
		{"unterminated annotation", "@JvmName(\"execute\"\nfun run() {}"},
		{"dangling annotation at EOF", "fun a() {}\n\n@Deprecated(\"x\")\n"},
		{"dangling annotation without newline", "fun a() {}\n@Deprecated(\"x\")"},
		{"semicolon same line", "@Deprecated(\"x\"); fun run() {}"},
		{"semicolon own line", "@Deprecated(\"x\");\nfun run() {}"},
		{"space after at sign", "@ Deprecated(\"x\")\nfun run() {}"},
		{"proven split in an unclean file", "println(1)\n@JvmName(\"execute\")\nfun run(x: kotlin.Int) {}"},
		{"space before arguments", "@Deprecated (\"x\")\nfun run() {}"},
		{"newline before arguments", "@Deprecated\n(\"x\")\nfun run() {}"},
		{"receiver use-site target", "@receiver:Suppress(\"x\")\nfun String.run() {}"},
		{"misplaced file annotation", "fun a() {}\n@file:JvmName(\"late\")\nfun run() {}"},
		{"same-line zero-parameter swallow", "@JvmName(\"execute\") fun a() { println() }\nfun execute(): kotlin.Int { return 1 }"},
		{"two own-line argument annotations swallow", "@Deprecated(\"x\")\n@JvmName(\"execute\")\nfun run(x: kotlin.Int) {}"},
		{"fun interface error", "@Deprecated(\"x\")\nfun interface F { fun f() }"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := "package lib\n" + tc.src
			if !strings.HasPrefix(tc.name, "dangling") { // nothing may follow
				src += "\nfun other() {}\n"
			}
			views, clean := kotlinRecoveredViews(t, src)
			if clean {
				t.Fatalf("root reported clean; views=%d", len(views))
			}
			p, err := NewKotlin().Parse(context.Background(), "lib/Actions.kt", []byte(src))
			if err != nil {
				t.Fatal(err)
			}
			if p.Scope.JVMFacade != (graph.JVMFileFacade{}) {
				t.Fatalf("facade = %+v", p.Scope.JVMFacade)
			}
			for _, s := range p.Symbols {
				if s.Kind == "function" && strings.HasPrefix(s.Signature, "@") {
					t.Fatalf("unrecovered %s carries annotations: %q", s.Name, s.Signature)
				}
			}
			if len(p.KotlinJVMNameEvidence) != 0 {
				t.Fatalf("unrecovered name facts = %+v", p.KotlinJVMNameEvidence)
			}
		})
	}
}

// Annotations before every other declaration kind stay attached, so the
// ordinary parser path handles them without recovery.
func TestKotlinDetachedAnnotationRecoveryLeavesOtherKindsAttached(t *testing.T) {
	for _, decl := range []string{
		"@Deprecated(\"x\")\nval x = 1",
		"@Deprecated(\"x\")\nvar y = 1",
		"@Deprecated(\"x\")\nconst val Z = 1",
		"@get:JvmName(\"getX2\")\nval x = 1",
		"@set:JvmName(\"setX2\")\nvar x = 1",
		"@property:Deprecated(\"x\")\nval x = 1",
		"@Deprecated(\"x\")\nclass C",
		"@Deprecated(\"x\")\ninterface I",
		"@Deprecated(\"x\")\nenum class E { A }",
		"@Target(AnnotationTarget.FUNCTION)\nannotation class A",
		"@Deprecated(\"x\")\ndata class D(val a: Int)",
		"@Deprecated(\"x\")\nsealed class S",
		"@JvmInline\n@Deprecated(\"x\")\nvalue class V(val a: Int)",
		"@Deprecated(\"x\")\ntypealias T = Int",
		"@Deprecated(\"x\")\nprivate fun run() {}",
		"@JvmName(\"execute\")\npublic fun run(x: kotlin.Int) {}",
		"@JvmName(\"execute\")\n@JvmOverloads\nfun run(x: kotlin.Int = 0) {}",
		"@JvmName(\"execute\") fun run(x: kotlin.Int) {}",
		"@[Deprecated(\"x\") JvmName(\"execute\")]\nfun run(x: kotlin.Int) {}",
		"@Suppress(\"A\", \"B\")\nfun run() {}",
		"@JvmName(name = \"execute\")\nfun run(x: kotlin.Int) {}",
	} {
		src := "package lib\n" + decl + "\nfun other() {}\n"
		if views, clean := kotlinRecoveredViews(t, src); !clean || len(views) != 0 {
			t.Fatalf("%q: clean=%v views=%d, want attached parse", decl, clean, len(views))
		}
	}
}

// The retained v6 parser refuses the facade on the same split v7 recovers and
// persists the declaration without its detached annotation.
func TestKotlinV6ParserKeepsDetachedAnnotationFailClosed(t *testing.T) {
	src := "package lib\n@JvmName(\"execute\")\nfun run(x: kotlin.Int) {}\nfun other() {}\n"
	if got := NewKotlinV6().Profile().ID; got != "treesitter:kotlin:v6" {
		t.Fatalf("v6 profile = %q", got)
	}
	if got := NewKotlin().Profile().ID; got != "treesitter:kotlin:v10" {
		t.Fatalf("current profile = %q", got)
	}
	old := kotlinParseFacts(t, NewKotlinV6(), src, "run")
	if old.Facade != (graph.JVMFileFacade{}) || old.NameFact != nil || old.Signature != "fun run(x: kotlin.Int) {}" {
		t.Fatalf("v6 facts = %+v %+v", old, old.NameFact)
	}
	current := kotlinParseFacts(t, NewKotlin(), src, "run")
	if current.Facade.Class != "ActionsKt" || current.NameFact == nil || !current.NameFact.Known {
		t.Fatalf("current facts = %+v %+v", current, current.NameFact)
	}
	// Scripts are never recovered: top-level statements are legal there.
	p, err := NewKotlin().Parse(context.Background(), "build.kts", []byte("@JvmName(\"execute\")\nfun run(x: kotlin.Int) {}\nfun other() {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.KotlinJVMNameEvidence) != 0 {
		t.Fatalf("script name facts = %+v", p.KotlinJVMNameEvidence)
	}
}
