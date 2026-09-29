//go:build cgo

package treesitter

import (
	"context"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

// Declaration @JvmName evidence: one fact per renamed function, attached to
// its exact symbol index; Known only for the literal subset the parser proves
// without evaluating Kotlin expressions.
func TestKotlinJVMNameEvidence(t *testing.T) {
	type want struct {
		present, known bool
		name           string
		arity          int // -1 unknown fixed arity
	}
	for _, tc := range []struct {
		name, src string
		want      want
	}{
		{"no annotation", "fun run(x: kotlin.Int) {}", want{arity: 1}},
		{"simple", "@JvmName(\"execute\")\nfun run(x: kotlin.Int) {}", want{true, true, "execute", 1}},
		{"qualified", "@kotlin.jvm.JvmName(\"execute\")\nfun run(x: kotlin.Int) {}", want{true, true, "execute", 1}},
		{"aliased", "import kotlin.jvm.JvmName as JN\n@JN(\"execute\")\nfun run(x: kotlin.Int) {}", want{true, true, "execute", 1}},
		{"explicit import", "import kotlin.jvm.JvmName\n@JvmName(\"execute\")\nfun run(x: kotlin.Int) {}", want{true, true, "execute", 1}},
		{"bracket annotation", "@[JvmName(\"execute\") Suppress(\"x\")]\nfun run(x: kotlin.Int) {}", want{true, true, "execute", 1}},
		{"order JvmName first", "@JvmName(\"execute\") @JvmStatic\nfun run(x: kotlin.Int) {}", want{true, true, "execute", 1}},
		{"order JvmName last", "@JvmStatic @JvmName(\"execute\")\nfun run(x: kotlin.Int) {}", want{true, true, "execute", 1}},
		{"zero arg", "@JvmName(\"execute\")\nfun run() {}", want{true, true, "execute", 0}},
		{"Java reserved word stays known", "@JvmName(\"class\")\nfun run(x: kotlin.Int) {}", want{true, true, "class", 1}},
		{"constant", "const val N = \"execute\"\n@JvmName(N)\nfun run(x: kotlin.Int) {}", want{true, false, "", -1}},
		{"concatenation", "@JvmName(\"exe\" + \"cute\")\nfun run(x: kotlin.Int) {}", want{true, false, "", -1}},
		{"named argument", "@JvmName(name = \"execute\")\nfun run(x: kotlin.Int) {}", want{true, false, "", -1}},
		{"raw string", "@JvmName(\"\"\"execute\"\"\")\nfun run(x: kotlin.Int) {}", want{true, false, "", -1}},
		{"unicode escape", "@JvmName(\"exec\\u0075te\")\nfun run(x: kotlin.Int) {}", want{true, false, "", -1}},
		{"dollar escape", "@JvmName(\"exe\\$cute\")\nfun run(x: kotlin.Int) {}", want{true, false, "", -1}},
		{"template", "@JvmName(\"exe$cute\")\nfun run(x: kotlin.Int) {}", want{true, false, "", -1}},
		{"outside ASCII subset", "@JvmName(\"éxecute\")\nfun run(x: kotlin.Int) {}", want{true, false, "", -1}},
		{"no argument", "@JvmName\nfun run(x: kotlin.Int) {}", want{true, false, "", -1}},
		{"repeated", "@JvmName(\"a\") @JvmName(\"b\")\nfun run(x: kotlin.Int) {}", want{true, false, "", -1}},
		{"foreign import shadows", "import other.JvmName\n@JvmName(\"execute\")\nfun run(x: kotlin.Int) {}", want{true, false, "", -1}},
		{"alias hides simple name", "import kotlin.jvm.JvmName as JN\n@JvmName(\"execute\")\nfun run(x: kotlin.Int) {}", want{true, false, "", -1}},
		{"foreign alias is not ours", "import other.Named as JN\n@JN(\"execute\")\nfun run(x: kotlin.Int) {}", want{arity: 1}},
		{"string mention is not an annotation", "@Suppress(\"JvmName\")\nfun run(x: kotlin.Int) {}", want{arity: 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := NewKotlin().Parse(context.Background(), "lib/Actions.kt", []byte("package lib\n"+tc.src))
			if err != nil {
				t.Fatal(err)
			}
			run := -1
			for i, s := range p.Symbols {
				if s.Kind == "function" && s.Name == "run" {
					run = i
				}
			}
			if run < 0 {
				t.Fatalf("run not parsed: %+v", p.Symbols)
			}
			if got := len(p.KotlinJVMNameEvidence); got != map[bool]int{false: 0, true: 1}[tc.want.present] {
				t.Fatalf("name facts = %+v", p.KotlinJVMNameEvidence)
			}
			if tc.want.present {
				fact := p.KotlinJVMNameEvidence[0]
				if fact.SymbolIndex != run || fact.Known != tc.want.known || fact.JVMName != tc.want.name {
					t.Fatalf("fact = %+v, want run index %d known=%v name=%q", fact, run, tc.want.known, tc.want.name)
				}
			}
			arity := p.Symbols[run].ArityMin
			if tc.want.arity < 0 && arity != nil || tc.want.arity >= 0 && (arity == nil || *arity != tc.want.arity) {
				t.Fatalf("arity = %v, want %d", arity, tc.want.arity)
			}
			if p.Scope.JVMFacade.Class != "ActionsKt" {
				t.Fatalf("facade = %+v", p.Scope.JVMFacade)
			}
		})
	}
}

// Overloads that share name, qualified name and stable key each carry their
// own fact by symbol index.
func TestKotlinJVMNameEvidenceTargetsExactOverload(t *testing.T) {
	src := "package lib\nobject Service {\n    @JvmName(\"one\") fun run(x: kotlin.Int) {}\n    fun run(x: kotlin.Int, y: kotlin.Int) {}\n    @JvmName(\"three\") fun run(x: kotlin.Int, y: kotlin.Int, z: kotlin.Int) {}\n}\n"
	p, err := NewKotlin().Parse(context.Background(), "lib/Service.kt", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.KotlinJVMNameEvidence) != 2 {
		t.Fatalf("facts = %+v", p.KotlinJVMNameEvidence)
	}
	for _, fact := range p.KotlinJVMNameEvidence {
		s := p.Symbols[fact.SymbolIndex]
		if s.Name != "run" || s.ArityMin == nil || map[string]int{"one": 1, "three": 3}[fact.JVMName] != *s.ArityMin {
			t.Fatalf("fact %+v attached to %+v", fact, s)
		}
	}
}

// A .kt root may hold only preamble, declaration nodes and recovered detached
// annotation runs. The grammar silently parses some legal annotated
// zero-argument functions as a top-level expression, dropping the declaration;
// the facade must then be absent so the surviving sibling cannot bind a call
// javac finds ambiguous.
func TestKotlinJVMFacadeTopLevelGuard(t *testing.T) {
	facade := graph.JVMFileFacade{Class: "ActionsKt"}
	none := graph.JVMFileFacade{}
	for _, tc := range []struct {
		name, src string
		want      graph.JVMFileFacade
	}{
		{"preamble", "#!/usr/bin/env kotlin\n@file:Suppress(\"x\")\npackage lib\nimport a.b\nimport c.*\n// line\n/* block */\n/** doc */\nfun run() {}", facade},
		{"typealias", "package lib\ntypealias X = kotlin.Int\nfun run() {}", facade},
		{"properties and accessors", "package lib\nval a: kotlin.Int = 1\nvar b: kotlin.Int = 2\n    get() = 3\n    set(v) {}\nconst val C = \"x\"\nval d: kotlin.Int get() = 4\nfun run() {}", facade},
		{"class family", "package lib\nclass A\ninterface I\nobject O\nenum class E { X, Y }\nannotation class Ann\nsealed interface S\ndata class D(val x: kotlin.Int)\nvalue class V(val v: kotlin.Int)\ndata object DO\nabstract class AB\nopen class OP\nclass G<T : Any> where T : Comparable<T>\nfun run() {}", facade},
		{"function forms", "package lib\nexpect fun e()\nactual fun a() {}\nprivate fun p() {}\nfun run() {};\nfun <T> T.ext() {}\nsuspend fun s() {}\nfun expr() = 1\nval String.extProp: kotlin.Int get() = 1\ntailrec fun t(): kotlin.Int = t()\ninfix fun kotlin.Int.plus2(o: kotlin.Int) = 1", facade},
		{"own-line annotation before a modifier", "package lib\n@JvmName(\"execute\")\npublic fun run(x: kotlin.Int) {}\nfun other() {}", facade},
		{"own-line annotation before another annotation", "package lib\n@JvmName(\"execute\")\n@JvmOverloads\nfun run(x: kotlin.Int = 0) {}\nfun other() {}", facade},
		// The grammar splits `@Ann(args)` + newline + `fun` into a top-level
		// annotated parenthesized expression and an unannotated function; v7
		// recovers that exact shape (see kotlin_annotation_recovery_test.go).
		{"own-line annotation split from fun", "package lib\n@JvmName(\"execute\")\nfun run(x: kotlin.Int) {}\nfun other() {}", facade},
		{"own-line Deprecated split", "package lib\n@Deprecated(\"x\")\nfun run(x: kotlin.Int) {}\nfun other() {}", facade},
		{"own-line Throws split", "package lib\n@Throws(Exception::class)\nfun run(x: kotlin.Int) {}\nfun other() {}", facade},
		{"own-line Suppress split", "package lib\n@Suppress(\"UNUSED\")\nfun run(x: kotlin.Int) {}\nfun other() {}", facade},
		{"own-line OptIn split", "package lib\n@OptIn(ExperimentalStdlibApi::class)\nfun run(x: kotlin.Int) {}\nfun other() {}", facade},
		{"own-line last annotation split from fun", "package lib\n@JvmOverloads\n@JvmName(\"execute\")\nfun run(x: kotlin.Int = 0) {}\nfun other() {}", facade},
		{"annotated zero-arg split on own line", "package lib\n@Deprecated(\"x\")\nfun dep() {}\nfun run() {}", facade},
		// A declaration the grammar swallows whole cannot be rebuilt.
		{"annotated zero-arg misparse", "package lib\n@JvmName(\"execute\") fun a() { println() }\nfun execute(): kotlin.Int { return 1 }\n", none},
		{"two own-line argument annotations swallow fun", "package lib\n@Deprecated(\"x\")\n@JvmName(\"execute\")\nfun run(x: kotlin.Int) {}\nfun other() {}", none},
		{"top-level expression", "package lib\nprintln(1)\nfun run() {}", none},
		{"root error node", "package lib\nfun interface F { fun f() }\nfun run() {}", none},
		{"recovery inside a declaration", "package lib\nclass Api {\n    companion object { fun run() {} }\n}\nfun run() {}", facade},
		{"misplaced file annotation", "package lib\n@file:JvmName(\"late\")\nfun run() {}", none},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := NewKotlin().Parse(context.Background(), "lib/Actions.kt", []byte(tc.src))
			if err != nil {
				t.Fatal(err)
			}
			if p.Scope.JVMFacade != tc.want {
				t.Fatalf("facade = %+v, want %+v", p.Scope.JVMFacade, tc.want)
			}
		})
	}
}

// Scripts legitimately hold top-level statements; the .kt guard must not
// touch their symbols (they never own a file facade).
func TestKotlinScriptTopLevelUnaffectedByGuard(t *testing.T) {
	p, err := NewKotlin().Parse(context.Background(), "build.kts", []byte("println(1)\nfun run(x: kotlin.Int) {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Scope.JVMFacade != (graph.JVMFileFacade{}) {
		t.Fatalf("script facade = %+v", p.Scope.JVMFacade)
	}
	found := false
	for _, s := range p.Symbols {
		found = found || s.Kind == "function" && s.Name == "run"
	}
	if !found {
		t.Fatalf("script symbols = %+v", p.Symbols)
	}
}

// A declaration-level JvmName alias import no longer erases the default
// facade; an aliased @file: annotation stays unsupported and fails closed.
func TestKotlinJVMFacadeWithJvmNameAliasImport(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		want      graph.JVMFileFacade
	}{
		{"declaration alias", "package lib\nimport kotlin.jvm.JvmName as JN\n@JN(\"execute\")\nfun run() {}", graph.JVMFileFacade{Class: "ActionsKt"}},
		{"declaration alias with explicit facade", "@file:kotlin.jvm.JvmName(\"API\")\npackage lib\nimport kotlin.jvm.JvmName as JN\n@JN(\"execute\")\nfun run(x: kotlin.Int) {}", graph.JVMFileFacade{Class: "API", Explicit: true}},
		{"aliased file annotation", "@file:JN(\"API\")\npackage lib\nimport kotlin.jvm.JvmName as JN\nfun run() {}", graph.JVMFileFacade{}},
		{"alias hides simple file annotation", "@file:JvmName(\"API\")\npackage lib\nimport kotlin.jvm.JvmName as JN\nfun run() {}", graph.JVMFileFacade{}},
		{"aliased multifile annotation", "@file:JvmName(\"API\")\n@file:MF\npackage lib\nimport kotlin.jvm.JvmMultifileClass as MF\nfun run() {}", graph.JVMFileFacade{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := NewKotlin().Parse(context.Background(), "lib/Actions.kt", []byte(tc.src))
			if err != nil {
				t.Fatal(err)
			}
			if p.Scope.JVMFacade != tc.want {
				t.Fatalf("facade = %+v, want %+v", p.Scope.JVMFacade, tc.want)
			}
		})
	}
}
