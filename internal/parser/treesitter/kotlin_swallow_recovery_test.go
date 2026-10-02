//go:build cgo

package treesitter

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"testing"

	sitter "github.com/smacker/go-tree-sitter"
	kotlin "github.com/smacker/go-tree-sitter/kotlin"

	"github.com/isink17/codegraph/internal/graph"
)

func kotlinTestSexp(n *sitter.Node, src []byte) string {
	kind := n.Type()
	if n.IsError() || n.IsMissing() {
		kind = "!" + kind
	}
	if n.ChildCount() == 0 {
		if n.IsNamed() {
			return kind + ":" + string(src[n.StartByte():n.EndByte()])
		}
		return "'" + kind + "'"
	}
	parts := []string{kind}
	for i := range int(n.ChildCount()) {
		parts = append(parts, kotlinTestSexp(n.Child(i), src))
	}
	return "(" + strings.Join(parts, " ") + ")"
}

// The grammar shape v9 recovers. A grammar update that stops swallowing the
// declaration, or swallows it differently, fails here and forces the
// recovery to be re-proven.
func TestKotlinSwallowedPrivateFunctionGrammarShape(t *testing.T) {
	src := []byte("@Preview(showBackground = true)\n@Composable\nprivate fun Screen() {\n    Body()\n}\nfun later() {}\n")
	root, err := parse(context.Background(), kotlin.GetLanguage(), src)
	if err != nil {
		t.Fatal(err)
	}
	want := "(prefix_expression (annotation '@' (constructor_invocation (user_type type_identifier:Preview) (value_arguments '(' (value_argument simple_identifier:showBackground '=' (boolean_literal 'true')) ')')))" +
		" (prefix_expression (annotation '@' (user_type type_identifier:Composable))" +
		" (infix_expression simple_identifier:private simple_identifier:fun" +
		" (call_expression (call_expression simple_identifier:Screen (call_suffix (value_arguments '(' ')')))" +
		" (call_suffix (annotated_lambda (lambda_literal '{' (statements (call_expression simple_identifier:Body (call_suffix (value_arguments '(' ')')))) '}')))))))"
	if root.ChildCount() != 2 || root.Child(1).Type() != "function_declaration" {
		t.Fatalf("root = %s", kotlinTestSexp(root, src))
	}
	if got := kotlinTestSexp(root.Child(0), src); got != want || root.Child(0).HasError() {
		t.Fatalf("swallowed shape =\n%s\nwant\n%s", got, want)
	}
	views, clean := kotlinRecoveredViews(t, string(src))
	view, ok := views[0]
	if !clean || len(views) != 1 || !ok || view.swallowed == nil || nodeText(view.swallowed.name, src) != "Screen" || view.swallowed.body.EndByte() != root.Child(0).EndByte() {
		t.Fatalf("clean=%v views=%+v", clean, views)
	}
}

// kotlinSemicolonOracle ends every recovered swallowed declaration with `;`.
// The separator changes nothing the compiler sees, but the grammar then parses
// each declaration as a function_declaration, so the oracle's facts are the
// clean parse of the same source on the same lines.
func kotlinSemicolonOracle(t *testing.T, src string) (string, int) {
	t.Helper()
	views, _ := kotlinRecoveredViews(t, src)
	var ends []int
	for _, view := range views {
		if view.swallowed != nil {
			ends = append(ends, int(view.swallowed.root.EndByte()))
		}
	}
	sort.Ints(ends)
	var b strings.Builder
	last := 0
	for _, end := range ends {
		b.WriteString(src[last:end] + ";")
		last = end
	}
	b.WriteString(src[last:])
	oracle := b.String()
	oviews, clean := kotlinRecoveredViews(t, oracle)
	for _, view := range oviews {
		if view.swallowed != nil {
			t.Fatalf("oracle still swallowed: %q", oracle)
		}
	}
	if !clean {
		t.Fatalf("oracle root unclean: %q", oracle)
	}
	return oracle, len(ends)
}

func kotlinParseForTest(t *testing.T, a *KotlinAdapter, path, src string) graph.ParsedFile {
	t.Helper()
	pf, err := a.Parse(context.Background(), path, []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	pf.FileTokens = nil // the oracle's `;` is a token of its own
	return pf
}

// Every recovered swallowed function carries exactly the facts of its clean
// parse: symbol, range, signature, visibility, documentation, arity, JVM
// evidence, facade and body calls -- and no call for its own head.
func TestKotlinSwallowedPrivateFunctionOracleParity(t *testing.T) {
	later := "\nfun later() {}\n"
	for _, tc := range []struct {
		name, src string
		recovered int
	}{
		{"minimal", "package lib\n@A\n@B\nprivate fun run() {}" + later, 1},
		{"argument and argument-less annotations", "package lib\n@Preview(showBackground = true)\n@Composable\nprivate fun run() {}" + later, 1},
		{"two own-line argument annotations with body call", "package lib\n@A(x)\n@B(y)\nprivate fun run() { target() }\nfun target() {}\n" + later, 1},
		{"body calls", "package lib\n@Preview\n@Composable\nprivate fun run() { target() }\nfun target() {}\n", 1},
		{"multiline body", "package lib\n@Preview\n@Composable\nprivate fun run() {\n    Theme {\n        target(1)\n    }\n}\nfun target(x: kotlin.Int) {}\nfun Theme(c: () -> Unit) {}\n", 1},
		{"body on the next line", "package lib\n@Preview\n@Composable\nprivate fun run()\n{\n    target()\n}" + later, 1},
		{"same-line annotations", "package lib\n@Preview @Composable private fun run() {}" + later, 1},
		{"three annotations", "package lib\n@Preview(name = \"a\")\n@Preview(name = \"b\")\n@Composable\nprivate fun run() {}" + later, 1},
		{"bracket and qualified annotations", "package lib\n@[Preview(name = \"a\") Preview]\n@androidx.compose.runtime.Composable\nprivate fun run() {}" + later, 1},
		{"aliased annotation", "package lib\nimport androidx.compose.runtime.Composable as C\n@Preview\n@C\nprivate fun run() {}" + later, 1},
		{"documented", "package lib\nfun before() {}\n// Doc of run.\n@Preview\n@Composable\nprivate fun run() {}" + later, 1},
		{"previous declaration keeps its comment", "package lib\n// Doc of before.\nfun before() {}\n@Preview\n@Composable\nprivate fun run() {}" + later, 1},
		{"known JvmName", "package lib\n@JvmName(\"execute\")\n@Composable\nprivate fun run() {}" + later, 1},
		{"unknown JvmName", "package lib\n@JvmName(\"exe\" + \"cute\")\n@Composable\nprivate fun run() {}" + later, 1},
		{"JvmSynthetic", "package lib\n@JvmSynthetic\n@Suppress(\"x\")\nprivate fun run() {}" + later, 1},
		{"JvmOverloads", "package lib\n@JvmOverloads\n@Composable\nprivate fun run() {}" + later, 1},
		{"JvmExposeBoxed", "package lib\n@JvmExposeBoxed\n@Composable\nprivate fun run() {}" + later, 1},
		{"file JvmName", "@file:JvmName(\"Facade\")\npackage lib\n@Preview\n@Composable\nprivate fun run() {}" + later, 1},
		{"only swallowed functions", "package lib\n@Preview\n@Composable\nprivate fun a() {}\n@Preview\n@Composable\nprivate fun b() {}\nclass K\n", 2},
		{"swallowed, ordinary, swallowed, class", "package lib\n@Preview\n@Composable\nprivate fun a() { b() }\n\n@Preview\n@Composable\nprivate fun b() {}\nfun c() {}\n@Preview\n@Composable\nprivate fun d() { c() }\nclass K\n", 3},
		{"newline before the name", "package lib\n@Preview\n@Composable\nprivate fun\nrun() {}" + later, 1},
		{"newline before the parameter list", "package lib\n@Preview\n@Composable\nprivate fun run\n() {}" + later, 1},
		{"swallowed then detached annotation", "package lib\n@Preview\n@Composable\nprivate fun screen() {}\n@JvmName(\"execute\")\nfun run(x: kotlin.Int) {}" + later, 1},
		{"with detached annotation recovery", "package lib\n@JvmName(\"execute\")\nfun run(x: kotlin.Int) {}\n@Preview\n@Composable\nprivate fun screen() {}\nfun other() {}\n", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			oracle, recovered := kotlinSemicolonOracle(t, tc.src)
			if recovered != tc.recovered {
				t.Fatalf("recovered %d swallowed functions, want %d", recovered, tc.recovered)
			}
			got := kotlinParseForTest(t, NewKotlin(), "lib/Screens.kt", tc.src)
			want := kotlinParseForTest(t, NewKotlin(), "lib/Screens.kt", oracle)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("recovered facts differ from the clean oracle:\n got %+v\nwant %+v", got, want)
			}
			// The same source was swallowed and fail closed under v8.
			old := kotlinParseForTest(t, NewKotlinV8(), "lib/Screens.kt", tc.src)
			if old.Scope.JVMFacade != (graph.JVMFileFacade{}) || len(old.Symbols) != len(got.Symbols)-recovered {
				t.Fatalf("v8 facade=%+v symbols=%d, want none and %d", old.Scope.JVMFacade, len(old.Symbols), len(got.Symbols)-recovered)
			}
		})
	}
}

func TestKotlinSwallowedArgumentAnnotationShapesFailClosed(t *testing.T) {
	for _, tc := range []struct{ name, src string }{
		{"same-line argument annotation", "package lib\n@A(x) fun a() {}\nfun later() {}\n"},
		{"two own-line argument annotations", "package lib\n@A(x)\n@B(y)\nfun a() {}\nfun later() {}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, _ := parse(context.Background(), kotlin.GetLanguage(), []byte(tc.src))
			t.Log(kotlinTestSexp(root, []byte(tc.src)))
			if views, clean := kotlinRecoveredViews(t, tc.src); clean || views != nil {
				t.Fatalf("clean=%v views=%+v, want fail-closed", clean, views)
			}
			got := kotlinParseForTest(t, NewKotlin(), "lib/Screens.kt", tc.src)
			if got.Scope.JVMFacade != (graph.JVMFileFacade{}) {
				t.Fatalf("facade = %+v, want suppressed", got.Scope.JVMFacade)
			}
			for _, sym := range got.Symbols {
				if sym.Name == "a" {
					t.Fatalf("fabricated a declaration: %+v", sym)
				}
			}
		})
	}
}

// The detached-OptIn follow-up remains fail-closed until an exact source
// fixture proves ownership. These controls must not let a future modifier
// gate bind through intervening, malformed, or ambiguous roots.
func TestKotlinDetachedOptInCandidateControlsFailClosed(t *testing.T) {
	for _, tc := range []struct {
		name, body         string
		clean              bool
		annotated          int
		candidateAnnotated bool
		duplicatePair      bool
	}{
		{"intervening expression", "@OptIn(E::class)\nprintln(1)\n@Composable\nfun candidate() {}\nfun after() {}\n", false, 0, false, false},
		{"annotation on another declaration", "@OptIn(E::class)\nval other = 1\n@Composable\nfun candidate() {}\nfun after() {}\n", true, 0, false, false},
		{"malformed arguments", "@OptIn(E::class\n@Composable\nfun candidate() {}\nfun after() {}\n", false, 0, false, false},
		{"dangling annotation", "@OptIn(E::class)\n", false, 0, false, false},
		{"duplicate candidate names", "@OptIn(E::class)\n@Composable\nfun candidate() {}\n@Composable\nfun candidate() {}\n", true, 1, true, true},
		{"multiple modifiers", "@OptIn(E::class)\n@Composable\nprivate suspend fun candidate(value: Int) {}\nfun after() {}\n", true, 1, true, false},
		{"same-line modifiers", "@OptIn(E::class) @Composable private fun candidate(value: Int) {}\nfun after() {}\n", true, 1, true, false},
		{"unrelated adjacent declarations", "fun before() {}\n@OptIn(E::class)\n@Composable\nprivate fun candidate(value: Int) {}\nfun after() {}\n", true, 1, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := "package lib\n" + tc.body
			views, clean := kotlinRecoveredViews(t, src)
			if clean != tc.clean || len(views) != 0 {
				t.Fatalf("clean=%v views=%+v, want clean=%v and no recovery", clean, views, tc.clean)
			}
			pf := kotlinParseForTest(t, NewKotlin(), "lib/OptIn.kt", src)
			if !clean && pf.Scope.JVMFacade != (graph.JVMFileFacade{}) {
				t.Fatalf("facade=%+v, want suppressed", pf.Scope.JVMFacade)
			}
			annotated, candidateAnnotations := 0, []bool{}
			for _, symbol := range pf.Symbols {
				got := strings.Contains(symbol.Signature, "@OptIn")
				if got {
					annotated++
				}
				if symbol.Name == "candidate" {
					candidateAnnotations = append(candidateAnnotations, got)
				}
			}
			if annotated != tc.annotated {
				t.Fatalf("annotated symbols=%d, want %d: %+v", annotated, tc.annotated, pf.Symbols)
			}
			if tc.duplicatePair && (len(candidateAnnotations) != 2 || !candidateAnnotations[0] || candidateAnnotations[1]) {
				t.Fatalf("duplicate candidate annotation ownership=%v, want [true false]", candidateAnnotations)
			}
			if !tc.duplicatePair && len(candidateAnnotations) > 0 && candidateAnnotations[0] != tc.candidateAnnotated {
				t.Fatalf("candidate annotation ownership=%v, want %v", candidateAnnotations, tc.candidateAnnotated)
			}
			if len(pf.KotlinJVMNameEvidence) != 0 || len(pf.KotlinJVMCallableEvidence) != 0 {
				t.Fatalf("unproven JVM facts: name=%+v callable=%+v", pf.KotlinJVMNameEvidence, pf.KotlinJVMCallableEvidence)
			}
		})
	}
}

// The facts pinned directly, so the oracle comparison cannot pass by both
// sides losing them.
func TestKotlinSwallowedPrivateFunctionFacts(t *testing.T) {
	src := "package lib\nfun target() {}\n// Doc.\n@JvmName(\"execute\")\n@Preview(showBackground = true)\nprivate fun run() {\n    target()\n}\nfun later() {}\n"
	pf := kotlinParseForTest(t, NewKotlin(), "lib/Screens.kt", src)
	if len(pf.Symbols) != 3 || pf.Symbols[0].Name != "target" || pf.Symbols[2].Name != "later" {
		t.Fatalf("symbols = %+v", pf.Symbols)
	}
	run := pf.Symbols[1]
	if run.Kind != "function" || run.Name != "run" || run.QualifiedName != "lib.run" || run.ContainerName != "lib" || run.StableKey != "func:kotlin:lib.run" ||
		run.Visibility != "private" || run.DocSummary != "Doc." || run.Signature != "@JvmName(\"execute\")\n@Preview(showBackground = true)\nprivate fun run() {\n    target()\n}" ||
		run.Range != (graph.Position{StartLine: 4, StartCol: 1, EndLine: 8, EndCol: 2}) || run.ArityMin == nil || *run.ArityMin != 0 || run.ArityMax == nil || *run.ArityMax != 0 {
		t.Fatalf("run = %+v", run)
	}
	if !reflect.DeepEqual(pf.KotlinJVMNameEvidence, []graph.KotlinJVMNameEvidence{{SymbolIndex: 1, Known: true, JVMName: "execute"}}) || len(pf.KotlinJVMCallableEvidence) != 0 {
		t.Fatalf("evidence = %+v %+v", pf.KotlinJVMNameEvidence, pf.KotlinJVMCallableEvidence)
	}
	if pf.Scope.JVMFacade.Class != "ScreensKt" {
		t.Fatalf("facade = %+v", pf.Scope.JVMFacade)
	}
	var calls, refs []string
	for _, e := range pf.Edges {
		calls = append(calls, e.DstName)
	}
	for _, r := range pf.References {
		refs = append(refs, r.Name)
	}
	if !reflect.DeepEqual(calls, []string{"target"}) || !reflect.DeepEqual(refs, []string{"target"}) || pf.Edges[0].Line != 7 {
		t.Fatalf("calls = %v refs = %v", calls, refs)
	}
	// v8 extracted the head as two calls and found no function.
	old := kotlinParseForTest(t, NewKotlinV8(), "lib/Screens.kt", src)
	calls = nil
	for _, e := range old.Edges {
		calls = append(calls, e.DstName)
	}
	sort.Strings(calls)
	if NewKotlinV8().Profile().ID != "treesitter:kotlin:v8" || NewKotlin().Profile().ID != "treesitter:kotlin:v11" || len(old.Symbols) != 2 || !reflect.DeepEqual(calls, []string{"run", "run()", "target"}) {
		t.Fatalf("v8 symbols = %+v calls = %v", old.Symbols, calls)
	}
}

// Everything outside the proven private shape stays exactly as v8 parsed it:
// the root is unclean, so nothing is recovered and the file has no facade.
func TestKotlinSwallowedFunctionRefusals(t *testing.T) {
	later := "\nfun later() {}\n"
	chain := "package lib\n@Preview\n@Composable\n"
	for _, tc := range []struct {
		name, src string
		// nearMiss marks a root expression the grammar builds exactly like
		// the recovered one, so only the predicate's own checks refuse it.
		nearMiss bool
	}{
		{"public", chain + "public fun X() {}" + later, true},
		{"internal", chain + "internal fun X() {}" + later, true},
		{"protected", chain + "protected fun X() {}" + later, true},
		{"inline", chain + "inline fun X() {}" + later, true},
		{"suspend", chain + "suspend fun X() {}" + later, true},
		{"actual", chain + "actual fun X() {}" + later, true},
		{"arbitrary identifier", chain + "foo fun X() {}" + later, true},
		{"backticked private", chain + "`private` fun X() {}" + later, true},
		{"newline after private", chain + "private\nfun X() {}" + later, false},
		{"S2 same-line split annotation", "package lib\n@JvmName(\"execute\") fun X() {}" + later, false},
		{"S3 anonymous function", "package lib\n@Deprecated(\"x\")\n@JvmName(\"execute\")\nfun X(x: kotlin.Int) {}" + later, false},
		{"S4 object", "package lib\n@Suppress(\"x\") object X {}" + later, false},
		{"S4 class", "package lib\n@Deprecated(\"x\")\n@Suppress(\"y\")\nclass X {}" + later, false},
		{"S4 property", "package lib\n@Deprecated(\"x\")\n@Suppress(\"y\")\nval X = 1" + later, false},
		{"extension receiver", chain + "private fun kotlin.String.X() {}" + later, false},
		{"navigation receiver", chain + "private fun a.X() {}" + later, false},
		{"use-site target", "package lib\n@get:Preview\n@Composable\nprivate fun X() {}" + later, true},
		{"comment in the chain", "package lib\n@Preview\n// c\n@Composable\nprivate fun X() {}" + later, false},
		{"block comment in the head", chain + "private /* c */ fun X() {}" + later, true},
		{"no name", chain + "private fun() {}" + later, false},
		{"no parameter list", chain + "private fun X {}" + later, false},
		{"unclosed parameter list", chain + "private fun X( {}" + later, false},
		{"no body", chain + "private fun X()" + later, false},
		{"trailing nonsense", chain + "private fun X() nonsense" + later, false},
		{"additive wrapper", chain + "private fun X() {} + y" + later, false},
		{"trailing call", chain + "private fun X() {} foo()" + later, false},
		{"infix wrapper", chain + "private fun X() {} to y" + later, false},
		{"elvis wrapper", chain + "private fun X() {} ?: y" + later, false},
		{"navigation on the body", chain + "private fun X() {}.foo()" + later, false},
		{"argument", chain + "private fun X(1) {}" + later, false},
		{"lambda parameters", chain + "private fun X() { a -> a }" + later, false},
		{"lambda arrow", chain + "private fun X() { -> }" + later, false},
		{"second lambda", chain + "private fun X() {} {}" + later, false},
		{"labelled lambda", chain + "private fun X() l@{}" + later, false},
		{"type arguments", chain + "private fun X<kotlin.Int>() {}" + later, false},
		{"semicolon after annotation", "package lib\n@Preview\n@Composable;\nprivate fun X() {}" + later, false},
		{"unexplained root beside a swallowed function", chain + "private fun X() {}\nfoo()" + later, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.nearMiss {
				kotlinAssertSwallowNearMiss(t, tc.src)
			}
			if views, clean := kotlinRecoveredViews(t, tc.src); clean || views != nil {
				t.Fatalf("clean=%v views=%+v, want unclean", clean, views)
			}
			got := kotlinParseForTest(t, NewKotlin(), "lib/Screens.kt", tc.src)
			if old := kotlinParseForTest(t, NewKotlinV8(), "lib/Screens.kt", tc.src); !reflect.DeepEqual(got, old) {
				t.Fatalf("v9 facts differ from v8:\n got %+v\nwant %+v", got, old)
			}
			if got.Scope.JVMFacade != (graph.JVMFileFacade{}) {
				t.Fatalf("facade = %+v", got.Scope.JVMFacade)
			}
		})
	}
	// Scripts may hold top-level expressions: the same tree is never recovered.
	script := chain + "private fun X() {}" + later
	got := kotlinParseForTest(t, NewKotlin(), "build.kts", script)
	if old := kotlinParseForTest(t, NewKotlinV8(), "build.kts", script); !reflect.DeepEqual(got, old) {
		t.Fatalf("script facts differ from v8")
	}
	for _, sym := range got.Symbols {
		if sym.Name == "X" {
			t.Fatalf("script recovered %+v", sym)
		}
	}
}

// kotlinAssertSwallowNearMiss proves the root holds an error-free annotation
// chain ending in an infix head spelling `fun` as an identifier, so its
// refusal comes from the predicate and not from a parse error.
func kotlinAssertSwallowNearMiss(t *testing.T, src string) {
	t.Helper()
	root, err := parse(context.Background(), kotlin.GetLanguage(), []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	for i := range int(root.ChildCount()) {
		node := root.Child(i)
		if node.Type() != "prefix_expression" || node.HasError() {
			continue
		}
		for node.Type() == "prefix_expression" {
			node = node.Child(int(node.ChildCount()) - 1)
		}
		for j := 0; node.Type() == "infix_expression" && j < int(node.ChildCount()); j++ {
			if child := node.Child(j); child.Type() == "simple_identifier" && nodeText(child, []byte(src)) == "fun" {
				return
			}
		}
	}
	t.Fatalf("no error-free swallowed head in %s", kotlinTestSexp(root, []byte(src)))
}

// A parse error nested anywhere in the candidate refuses it, although every
// node the predicate inspects keeps the recovered shape and the candidate
// itself is not an ERROR node: the error check covers the whole subtree.
func TestKotlinSwallowedFunctionNestedErrorRefusals(t *testing.T) {
	later := "\nfun later() {}\n"
	chain := "package lib\n@Preview\n@Composable\n"
	for _, tc := range []struct{ name, src string }{
		{"annotation argument", "package lib\n@Preview(x = )\n@Composable\nprivate fun X() {}" + later},
		{"missing node in the body", chain + "private fun X() { if (a) }" + later},
		{"nested call argument", chain + "private fun X() { foo(1 2) }" + later},
		{"body statement", chain + "private fun X() { foo( }" + later},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, err := parse(context.Background(), kotlin.GetLanguage(), []byte(tc.src))
			if err != nil {
				t.Fatal(err)
			}
			candidates := 0
			for i := range int(root.ChildCount()) {
				child := root.Child(i)
				if child.Type() == "prefix_expression" {
					candidates++
					if child.IsError() || !child.HasError() {
						t.Fatalf("candidate = %s, want a nested error only", kotlinTestSexp(child, []byte(tc.src)))
					}
				} else if child.HasError() {
					t.Fatalf("root child %s has an error outside the candidate", child.Type())
				}
			}
			if candidates != 1 {
				t.Fatalf("root = %s", kotlinTestSexp(root, []byte(tc.src)))
			}
			if views, clean := kotlinRecoveredViews(t, tc.src); clean || views != nil {
				t.Fatalf("clean=%v views=%+v, want unclean", clean, views)
			}
			got := kotlinParseForTest(t, NewKotlin(), "lib/Screens.kt", tc.src)
			if old := kotlinParseForTest(t, NewKotlinV8(), "lib/Screens.kt", tc.src); !reflect.DeepEqual(got, old) {
				t.Fatalf("v9 facts differ from v8:\n got %+v\nwant %+v", got, old)
			}
		})
	}
}
