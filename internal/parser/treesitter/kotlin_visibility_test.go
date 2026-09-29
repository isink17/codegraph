//go:build cgo

package treesitter

import (
	"context"
	"testing"
)

// Visibility comes from the declaration's own modifiers node, never from
// source text: an annotation argument can neither truncate a real modifier
// away (`@A(x = 1) private fun`, a Java false positive on a private function)
// nor contribute a visibility word of its own (`@Suppress("private") fun`).
func TestKotlinStructuralVisibility(t *testing.T) {
	for _, tc := range []struct {
		name, src string
		want      map[string]string
	}{
		{"function matrix", "package lib\nfun a() {}\npublic fun b() {}\nprivate fun c() {}\ninternal fun d() {}\nprotected fun e() {}\n",
			map[string]string{"lib.a": "public", "lib.b": "public", "lib.c": "private", "lib.d": "internal", "lib.e": "protected"}},
		{"annotation = before private", "package lib\n@Preview(showBackground = true)\nprivate fun a(x: kotlin.Int) {}\n@Suppress(names = [\"unused\"])\nprivate fun b(x: kotlin.Int) {}\n",
			map[string]string{"lib.a": "private", "lib.b": "private"}},
		{"annotation = before internal", "package lib\n@Preview(showBackground = true)\ninternal fun a(x: kotlin.Int) {}\n@Suppress(names = [\"unused\"])\ninternal fun b() {}\n",
			map[string]string{"lib.a": "internal", "lib.b": "internal"}},
		{"annotation = before zero-parameter private", "package lib\n@Preview(showBackground = true)\nprivate fun helper() {}\n",
			map[string]string{"lib.helper": "private"}},
		{"annotation { before private", "package lib\n@Suppress(\"{\")\nprivate fun a(x: kotlin.Int) {}\n",
			map[string]string{"lib.a": "private"}},
		{"annotation visibility words stay annotation text", "package lib\n@Suppress(\"private\")\nfun a(x: kotlin.Int) {}\n@Suppress(\"internal\")\npublic fun b(x: kotlin.Int) {}\n@Suppress(\"{private}\")\nfun c(x: kotlin.Int) {}\n@Suppress(names = [\"protected\"]) fun d() {}\n",
			map[string]string{"lib.a": "public", "lib.b": "public", "lib.c": "public", "lib.d": "public"}},
		{"multiple modifiers", "package lib\nprivate inline fun a() {}\ninternal external fun b()\n",
			map[string]string{"lib.a": "private", "lib.b": "internal"}},
		{"class matrix", "package lib\nclass A\npublic class B\nprivate class C\ninternal class D\n@Ann(a = 1) private sealed class E\n@Suppress(\"private\") data class F(val x: kotlin.Int)\nprivate interface G\nclass H private constructor(x: kotlin.Int)\nclass I(private val x: kotlin.Int)\nclass J internal constructor() {}\n",
			map[string]string{"lib.H": "public", "lib.I": "public", "lib.J": "public", "lib.A": "public", "lib.B": "public", "lib.C": "private", "lib.D": "internal", "lib.E": "private", "lib.F": "public", "lib.G": "private"}},
		{"object matrix", "package lib\nobject A\npublic object B\nprivate object C\ninternal object D\n@Ann(a = 1) private object E\n@Suppress(\"internal\") object F\n",
			map[string]string{"lib.A": "public", "lib.B": "public", "lib.C": "private", "lib.D": "internal", "lib.E": "private", "lib.F": "public"}},
		{"members and companions", "package lib\nclass S {\n    private companion object\n    protected fun a() {}\n    @Suppress(names = [\"x\"]) protected open fun b() {}\n    protected object O\n}\nclass T {\n    @Suppress(names = [\"x\"])\n    internal companion object Factory {\n        @Ann(a = 1) private fun c() {}\n    }\n}\nclass U {\n    @Suppress(\"private\") companion object\n}\n",
			map[string]string{"lib.S": "public", "lib.S.Companion": "private", "lib.S.a": "protected", "lib.S.b": "protected", "lib.S.O": "protected",
				"lib.T": "public", "lib.T.Factory": "internal", "lib.T.Factory.c": "private", "lib.U": "public", "lib.U.Companion": "public"}},
		// P24.7-H recovers only modifier-less declarations: the detached
		// annotations are not modifiers, so the default holds even when their
		// text spells a visibility.
		{"recovered detached annotations", "package lib\n@JvmName(\"execute\")\nfun a(x: kotlin.Int) {}\n@Suppress(\"private\")\nfun b(x: kotlin.Int) {}\nfun other() {}\n",
			map[string]string{"lib.a": "public", "lib.b": "public", "lib.other": "public"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p, err := NewKotlin().Parse(context.Background(), "lib/Actions.kt", []byte(tc.src))
			if err != nil {
				t.Fatal(err)
			}
			got := map[string]string{}
			for _, sym := range p.Symbols {
				got[sym.QualifiedName] = sym.Visibility
			}
			for qname, want := range tc.want {
				if got[qname] != want {
					t.Errorf("%s visibility = %q, want %q (all %v)", qname, got[qname], want, got)
				}
			}
			if len(got) != len(tc.want) {
				t.Errorf("symbols = %v, want %v", got, tc.want)
			}
		})
	}
}

// Correcting visibility changes no other fact: the declaration keeps its
// signature, range, fixed arity and declaration JVM name.
func TestKotlinStructuralVisibilityKeepsJVMFacts(t *testing.T) {
	src := "package lib\n@JvmName(\"execute\")\n@Ann(flag = true)\nprivate fun run(x: kotlin.Int) {}\n"
	p, err := NewKotlin().Parse(context.Background(), "lib/Actions.kt", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Symbols) != 1 {
		t.Fatalf("symbols = %+v", p.Symbols)
	}
	sym := p.Symbols[0]
	if sym.Visibility != "private" || sym.Signature != "@JvmName(\"execute\")\n@Ann(flag = true)\nprivate fun run(x: kotlin.Int) {}" || sym.StableKey != "func:kotlin:lib.run" ||
		sym.Range.StartLine != 2 || sym.Range.EndLine != 4 || sym.ArityMin == nil || *sym.ArityMin != 1 || sym.ArityMax == nil || *sym.ArityMax != 1 {
		t.Fatalf("symbol = %+v", sym)
	}
	if len(p.KotlinJVMNameEvidence) != 1 || !p.KotlinJVMNameEvidence[0].Known || p.KotlinJVMNameEvidence[0].JVMName != "execute" || p.KotlinJVMNameEvidence[0].SymbolIndex != 0 {
		t.Fatalf("name evidence = %+v", p.KotlinJVMNameEvidence)
	}
	if len(p.KotlinJVMCallableEvidence) != 0 || p.Scope.JVMFacade.Class != "ActionsKt" {
		t.Fatalf("callable evidence = %+v facade = %+v", p.KotlinJVMCallableEvidence, p.Scope.JVMFacade)
	}
}
