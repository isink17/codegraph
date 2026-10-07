//go:build cgo

package treesitter

import (
	"context"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

func TestKotlinJVMCallableArityEvidence(t *testing.T) {
	tests := []struct {
		name, source string
		known        bool
		min, max     int
	}{
		{"plain default", `fun run(a: kotlin.Int, b: kotlin.String = "") {}`, true, 2, 2},
		{"overloads", `@JvmOverloads fun run(a: kotlin.Int, b: kotlin.String = "", c: kotlin.Long = 0L) {}`, true, 1, 3},
		{"mixed defaults", `@JvmOverloads fun run(a: kotlin.Int = 0, b: kotlin.String, c: kotlin.Long = 0L) {}`, true, 1, 3},
		{"complex default", `@JvmOverloads fun run(a: kotlin.Int, b: kotlin.String = call(1, nested(2, 3))) {}`, true, 1, 2},
		{"generated zero arg", `@JvmOverloads fun run(a: kotlin.Int = 0) {}`, true, 0, 1},
		{"introduced veto", `fun run(@kotlin.IntroducedAt(2) a: kotlin.Int = 0) {}`, false, 0, 0},
		{"custom type veto", `@JvmOverloads fun run(a: custom.Type = custom.Type()) {}`, false, 0, 0},
		{"vararg veto", `@JvmOverloads fun run(vararg a: kotlin.Int) {}`, false, 0, 0},
		{"extension veto", `@JvmOverloads fun kotlin.String.run(a: kotlin.Int = 0) {}`, false, 0, 0},
		{"generic veto", `@JvmOverloads fun <T> run(a: kotlin.Int = 0) {}`, false, 0, 0},
		{"boxed veto", `@kotlin.jvm.JvmExposeBoxed @JvmOverloads fun run(a: kotlin.Int = 0) {}`, false, 0, 0},
		{"suspend veto", `@JvmOverloads suspend fun run(a: kotlin.Int = 0) {}`, false, 0, 0},
		{"internal veto", `@JvmOverloads internal fun run(a: kotlin.Int = 0) {}`, false, 0, 0},
		{"known JvmName keeps arity", `@JvmName("renamed") @JvmOverloads fun run(a: kotlin.Int = 0) {}`, true, 0, 1},
		{"known JvmName plain default", `@JvmName("renamed") fun run(a: kotlin.Int, b: kotlin.String = "") {}`, true, 2, 2},
		{"unknown JvmName veto", `@JvmName(NAME) @JvmOverloads fun run(a: kotlin.Int = 0) {}`, false, 0, 0},
		{"foreign JvmName veto", "import other.JvmName\n@JvmName(\"renamed\") @JvmOverloads fun run(a: kotlin.Int = 0) {}", false, 0, 0},
		{"overloads without defaults", `@JvmOverloads fun run(a: kotlin.Int) {}`, true, 1, 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed, err := NewKotlin().Parse(context.Background(), "Actions.kt", []byte(tt.source))
			if err != nil {
				t.Fatal(err)
			}
			if len(parsed.KotlinJVMCallableEvidence) != 1 {
				t.Fatalf("facts = %+v", parsed.KotlinJVMCallableEvidence)
			}
			fact := parsed.KotlinJVMCallableEvidence[0]
			if fact.SymbolIndex < 0 || fact.SymbolIndex >= len(parsed.Symbols) || parsed.Symbols[fact.SymbolIndex].Kind != "function" {
				t.Fatalf("fact does not target exact symbol: %+v", fact)
			}
			if fact.Known != tt.known || tt.known && (fact.ArityMin != tt.min || fact.ArityMax != tt.max) {
				t.Fatalf("fact = %+v", fact)
			}
		})
	}

	parsed, err := NewKotlin().Parse(context.Background(), "Actions.kt", []byte("import kotlin.jvm.JvmOverloads as JO\n@JO fun run(a: kotlin.Int, b: kotlin.String = \"\") {}"))
	if err != nil || len(parsed.KotlinJVMCallableEvidence) != 1 || !parsed.KotlinJVMCallableEvidence[0].Known || parsed.KotlinJVMCallableEvidence[0].ArityMin != 1 {
		t.Fatalf("aliased JvmOverloads evidence = %+v, err = %v", parsed.KotlinJVMCallableEvidence, err)
	}
	parsed, err = NewKotlin().Parse(context.Background(), "Actions.kt", []byte("import kotlin.jvm.JvmExposeBoxed as Boxed\nimport kotlin.jvm.JvmOverloads\n@Boxed @JvmOverloads fun run(a: kotlin.Int = 0) {}"))
	if err != nil || len(parsed.KotlinJVMCallableEvidence) != 1 || parsed.KotlinJVMCallableEvidence[0].Known {
		t.Fatalf("aliased unsupported JVM annotation = %+v, err = %v", parsed.KotlinJVMCallableEvidence, err)
	}
}

func TestKotlinJVMTypeSyntaxEvidence(t *testing.T) {
	source := `package api
import dep.Token as Alias
import dep.*
typealias Local = Alias
@JvmInline value class Value<T>(val value: T)
class Ordinary
fun use(value: Alias): Ordinary = Ordinary()`
	parsed, err := NewKotlin().Parse(context.Background(), "Types.kt", []byte(source))
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Scope.Imports) != 2 || parsed.Scope.Imports[0].LocalName != "Alias" || !parsed.Scope.Imports[1].Wildcard {
		t.Fatalf("imports = %+v", parsed.Scope.Imports)
	}
	if len(parsed.JVMTypeEvidence) < 4 || parsed.JVMTypeEvidence[0].Kind != "typealias" || parsed.JVMTypeEvidence[0].AliasTarget != "Alias" || parsed.JVMTypeEvidence[0].SyntaxState != "unknown" {
		t.Fatalf("typealias source evidence = %+v", parsed.JVMTypeEvidence)
	}
	var valueUnderlying string
	for _, fact := range parsed.JVMTypeEvidence {
		if fact.Kind == "class" && strings.Contains(fact.Modifiers, "value") {
			if fact.TypeParams && fact.UnderlyingState == "known" {
				valueUnderlying = fact.UnderlyingType
			}
		}
	}
	if valueUnderlying != "T" {
		t.Fatalf("value class underlying source syntax = %q; facts=%+v", valueUnderlying, parsed.JVMTypeEvidence)
	}
	aliases, err := NewKotlin().Parse(context.Background(), "Aliases.kt", []byte("typealias A = B\ntypealias B = A\ntypealias MissingTarget = Unknown\n"))
	if err != nil || len(aliases.JVMTypeEvidence) != 3 {
		t.Fatalf("cyclic/unknown aliases = %+v err=%v", aliases.JVMTypeEvidence, err)
	}
	for _, fact := range aliases.JVMTypeEvidence {
		if fact.SyntaxState != "unknown" || fact.AliasTarget == "" {
			t.Fatalf("alias target was treated as resolved: %+v", fact)
		}
	}
	for _, sym := range parsed.Symbols {
		if sym.Kind != "function" {
			continue
		}
		var fact *graph.JVMTypeEvidence
		for i := range parsed.JVMTypeEvidence {
			if parsed.JVMTypeEvidence[i].SymbolIndex >= 0 && parsed.Symbols[parsed.JVMTypeEvidence[i].SymbolIndex].StableKey == sym.StableKey {
				fact = &parsed.JVMTypeEvidence[i]
			}
		}
		if fact == nil || len(fact.Params) != 2 || fact.Params[0].Syntax != "Alias" || fact.Params[0].SyntaxState != "known" || fact.Params[1].Position != "result" {
			t.Fatalf("function type evidence = %+v", fact)
		}
	}
	bad, err := NewKotlin().Parse(context.Background(), "Bad.kt", []byte("fun broken(value: List<Alias>) {}"))
	if err != nil {
		t.Fatal(err)
	}
	if len(bad.JVMTypeEvidence) != 1 || bad.JVMTypeEvidence[0].Params[0].SyntaxState != "incomplete" {
		t.Fatalf("unsupported nested type evidence = %+v", bad.JVMTypeEvidence)
	}
	extension, err := NewKotlin().Parse(context.Background(), "Extension.kt", []byte("fun Token.extend(value: Alias): Token = this"))
	if err != nil || len(extension.JVMTypeEvidence) != 1 || extension.JVMTypeEvidence[0].SyntaxState != "incomplete" {
		t.Fatalf("extension receiver evidence = %+v, err=%v", extension.JVMTypeEvidence, err)
	}
	malformed, err := NewKotlin().Parse(context.Background(), "Bad.kt", []byte("fun broken(value: List<) {}"))
	if err != nil || len(malformed.JVMTypeEvidence) != 0 {
		t.Fatalf("malformed type evidence = %+v, err=%v", malformed.JVMTypeEvidence, err)
	}
	conflict, err := NewKotlin().Parse(context.Background(), "Conflict.kt", []byte("import a.Token as Same\nimport b.Token as Same\nfun use(x: Same) {}"))
	if err != nil || len(conflict.Scope.Imports) != 2 || len(conflict.JVMTypeEvidence) != 1 || conflict.JVMTypeEvidence[0].SyntaxState != "unknown" {
		t.Fatalf("conflicting import evidence = %+v imports=%+v err=%v", conflict.JVMTypeEvidence, conflict.Scope.Imports, err)
	}
}

func TestJavaJVMTypeSyntaxEvidence(t *testing.T) {
	parsed, err := NewJava().Parse(context.Background(), "Types.java", []byte(`package api;
import dep.Token;
import dep.*;
class Box<T> { Token run(Token input) { return input; } }`))
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Scope.Imports) != 2 || !parsed.Scope.Imports[1].Wildcard {
		t.Fatalf("imports = %+v", parsed.Scope.Imports)
	}
	var generic, method bool
	for _, fact := range parsed.JVMTypeEvidence {
		sym := parsed.Symbols[fact.SymbolIndex]
		if sym.Kind == "type" && fact.TypeParams {
			generic = true
		}
		if sym.Name == "run" && len(fact.Params) == 2 && fact.Params[0].Syntax == "Token" && fact.Params[1].Position == "result" {
			method = true
		}
	}
	if !generic || !method {
		t.Fatalf("JVM type evidence = %+v symbols=%+v", parsed.JVMTypeEvidence, parsed.Symbols)
	}
}

func TestKotlinCompanionSourceSymbols(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want []graph.Symbol
	}{
		{
			name: "unnamed companion",
			src: `package lib
class Service {
    companion object {
        fun run() {}
    }
}`,
			want: []graph.Symbol{
				{Kind: "companion_object", Name: "Companion", QualifiedName: "lib.Service.Companion", ContainerName: "Service", StableKey: "companion:kotlin:lib.Service.Companion"},
				{Kind: "function", Name: "run", QualifiedName: "lib.Service.Companion.run", ContainerName: "Service.Companion", Signature: "fun run() {}", StableKey: "func:kotlin:lib.Service.Companion.run"},
			},
		},
		{
			name: "named companion",
			src: `package lib
class Service {
    companion object Factory {
        fun run() {}
    }
}`,
			want: []graph.Symbol{
				{Kind: "companion_object", Name: "Factory", QualifiedName: "lib.Service.Factory", ContainerName: "Service", StableKey: "companion:kotlin:lib.Service.Factory"},
				{Kind: "function", Name: "run", QualifiedName: "lib.Service.Factory.run", ContainerName: "Service.Factory", Signature: "fun run() {}", StableKey: "func:kotlin:lib.Service.Factory.run"},
			},
		},
		{
			name: "explicit default spelling stays named",
			src: `package lib
class Service {
    companion object Companion {
        fun run() {}
    }
}`,
			want: []graph.Symbol{
				{Kind: "companion_object", Name: "Companion", QualifiedName: "lib.Service.Companion", ContainerName: "Service", StableKey: "companion:kotlin:lib.Service.Companion"},
				{Kind: "function", Name: "run", QualifiedName: "lib.Service.Companion.run", ContainerName: "Service.Companion", Signature: "fun run() {}", StableKey: "func:kotlin:lib.Service.Companion.run"},
			},
		},
		{
			name: "empty companion",
			src: `class Service {
    companion object
}`,
			want: []graph.Symbol{
				{Kind: "companion_object", Name: "Companion", QualifiedName: "Service.Companion", ContainerName: "Service", StableKey: "companion:kotlin:Service.Companion"},
			},
		},
		{
			name: "private companion declaration visibility",
			src: `class Service {
    private companion object {
        fun run() {}
    }
}`,
			want: []graph.Symbol{
				{Kind: "companion_object", Name: "Companion", QualifiedName: "Service.Companion", ContainerName: "Service", Visibility: "private", StableKey: "companion:kotlin:Service.Companion"},
				{Kind: "function", Name: "run", QualifiedName: "Service.Companion.run", ContainerName: "Service.Companion", Signature: "fun run() {}", Visibility: "public", StableKey: "func:kotlin:Service.Companion.run"},
			},
		},
		{
			name: "internal named companion declaration visibility",
			src: `class Service {
    internal companion object Factory {
        private fun run() {}
    }
}`,
			want: []graph.Symbol{
				{Kind: "companion_object", Name: "Factory", QualifiedName: "Service.Factory", ContainerName: "Service", Visibility: "internal", StableKey: "companion:kotlin:Service.Factory"},
				{Kind: "function", Name: "run", QualifiedName: "Service.Factory.run", ContainerName: "Service.Factory", Signature: "private fun run() {}", Visibility: "private", StableKey: "func:kotlin:Service.Factory.run"},
			},
		},
		{
			name: "multiple members and annotations",
			src: `class Service {
    @SomeAnnotation
    companion object {
        @JvmStatic
        private fun hidden() {}
        @JvmName("execute")
        internal fun run() {}
        fun second() {}
    }
}`,
			want: []graph.Symbol{
				{Kind: "companion_object", Name: "Companion", QualifiedName: "Service.Companion", ContainerName: "Service", StableKey: "companion:kotlin:Service.Companion"},
				{Kind: "function", Name: "hidden", QualifiedName: "Service.Companion.hidden", ContainerName: "Service.Companion", Signature: "@JvmStatic\n        private fun hidden() {}", Visibility: "private", StableKey: "func:kotlin:Service.Companion.hidden"},
				{Kind: "function", Name: "run", QualifiedName: "Service.Companion.run", ContainerName: "Service.Companion", Signature: "@JvmName(\"execute\")\n        internal fun run() {}", Visibility: "internal", StableKey: "func:kotlin:Service.Companion.run"},
				{Kind: "function", Name: "second", QualifiedName: "Service.Companion.second", ContainerName: "Service.Companion", Signature: "fun second() {}", Visibility: "public", StableKey: "func:kotlin:Service.Companion.second"},
			},
		},
		{
			name: "nested companion",
			src: `package lib
class Outer {
    class Inner {
        companion object {
            fun run() {}
        }
    }
}`,
			want: []graph.Symbol{
				{Kind: "companion_object", Name: "Companion", QualifiedName: "lib.Outer.Inner.Companion", ContainerName: "Outer.Inner", StableKey: "companion:kotlin:lib.Outer.Inner.Companion"},
				{Kind: "function", Name: "run", QualifiedName: "lib.Outer.Inner.Companion.run", ContainerName: "Outer.Inner.Companion", Signature: "fun run() {}", StableKey: "func:kotlin:lib.Outer.Inner.Companion.run"},
			},
		},
		{
			name: "ordinary objects remain objects",
			src: `package lib
class Service {
    object Companion { fun run() {} }
    object Factory { fun run() {} }
}`,
			want: []graph.Symbol{
				{Kind: "object", Name: "Companion", QualifiedName: "lib.Service.Companion", ContainerName: "Service", StableKey: "type:kotlin:lib.Service.Companion"},
				{Kind: "function", Name: "run", QualifiedName: "lib.Service.Companion.run", ContainerName: "Service.Companion", Signature: "fun run() {}", StableKey: "func:kotlin:lib.Service.Companion.run"},
				{Kind: "object", Name: "Factory", QualifiedName: "lib.Service.Factory", ContainerName: "Service", StableKey: "type:kotlin:lib.Service.Factory"},
				{Kind: "function", Name: "run", QualifiedName: "lib.Service.Factory.run", ContainerName: "Service.Factory", Signature: "fun run() {}", StableKey: "func:kotlin:lib.Service.Factory.run"},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			parsed, err := NewKotlin().Parse(context.Background(), "Sample.kt", []byte(tt.src))
			if err != nil {
				t.Fatal(err)
			}
			got := make([]graph.Symbol, 0, len(tt.want))
			for _, sym := range parsed.Symbols {
				for _, expected := range tt.want {
					if sym.StableKey == expected.StableKey {
						got = append(got, sym)
						if sym.Kind != expected.Kind || sym.Name != expected.Name || sym.QualifiedName != expected.QualifiedName || sym.ContainerName != expected.ContainerName || sym.StableKey != expected.StableKey {
							t.Errorf("symbol = %+v, want kind/name/qname/container/key = %q/%q/%q/%q/%q", sym, expected.Kind, expected.Name, expected.QualifiedName, expected.ContainerName, expected.StableKey)
						}
						if expected.Signature != "" && sym.Signature != expected.Signature {
							t.Errorf("%s signature = %q, want %q", sym.QualifiedName, sym.Signature, expected.Signature)
						}
						if expected.Visibility != "" && sym.Visibility != expected.Visibility {
							t.Errorf("%s visibility = %q, want %q", sym.QualifiedName, sym.Visibility, expected.Visibility)
						}
						if sym.Range.StartLine == 0 || sym.Range.EndLine < sym.Range.StartLine {
							t.Errorf("%s range = %+v", sym.QualifiedName, sym.Range)
						}
						break
					}
				}
			}
			if len(got) != len(tt.want) {
				var keys []string
				for _, sym := range parsed.Symbols {
					keys = append(keys, sym.StableKey)
				}
				t.Fatalf("matched %d symbols, want %d; parsed keys: %s", len(got), len(tt.want), strings.Join(keys, ", "))
			}
			if tt.name == "multiple members and annotations" {
				for _, sym := range parsed.Symbols {
					if sym.Kind == "companion_object" && sym.Signature != "" {
						t.Errorf("companion-level annotation leaked into symbol signature: %q", sym.Signature)
					}
				}
			}
		})
	}
}

func TestKotlinFixedJavaCallableArity(t *testing.T) {
	src := `package lib
typealias Alias = Token
@JvmInline value class Token(val value: kotlin.Int)
object Service {
    fun zero() {}
    fun run(value: kotlin.Int) {}
    fun run(value: kotlin.Int, other: kotlin.String) {}
    @JvmStatic fun staticRun(value: kotlin.Int) {}
    fun nullable(value: kotlin.Int?) {}
    fun noResult(value: kotlin.String) {}
    fun explicitUnit(value: kotlin.Int): kotlin.Unit {}
    inline fun inlineRun(value: kotlin.Int) {}
    fun unqualified(value: Int) {}
    fun custom(value: Token) {}
    fun valueClassReturn(value: kotlin.Int): Token { return Token(value) }
    fun valueClassAlias(value: Alias) {}
    fun nestedValueClass(value: kotlin.collections.List<Token>) {}
    fun generic(value: kotlin.collections.List<kotlin.Int>) {}
    fun defaulted(value: kotlin.Int = 1) {}
    @JvmOverloads fun overloaded(value: kotlin.Int = 1) {}
    fun variadic(vararg values: kotlin.Int) {}
    fun kotlin.String.extension(value: kotlin.Int) {}
    suspend fun suspended(value: kotlin.Int) {}
    @JvmName("renamed") fun renamed(value: kotlin.Int) {}
    @JvmName(NAME) fun unknownRenamed(value: kotlin.Int) {}
    @JvmSynthetic fun hidden(value: kotlin.Int) {}
    inline fun <reified T> reified(value: kotlin.Int) {}
    fun inferred(value: kotlin.Int) = Token(value)
    @JvmExposeBoxed("boxed") fun exposed(value: Token) {}
    fun multiline(
        value: kotlin.Int,
        other: kotlin.String,
    ) {}
	}`
	p, err := NewKotlin().Parse(context.Background(), "Service.kt", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]*[2]int{
		"zero":         arityPair(0),
		"staticRun":    arityPair(1),
		"nullable":     arityPair(1),
		"noResult":     arityPair(1),
		"explicitUnit": arityPair(1),
		"inlineRun":    arityPair(1),
		"multiline":    arityPair(2),
		"renamed":      arityPair(1),
	}
	for _, symbol := range p.Symbols {
		if symbol.Kind != "function" {
			continue
		}
		if symbol.Name == "run" {
			wantArity := 1
			if strings.Contains(symbol.Signature, "other: kotlin.String") {
				wantArity = 2
			}
			if symbol.ArityMin == nil || symbol.ArityMax == nil || *symbol.ArityMin != wantArity || *symbol.ArityMax != wantArity {
				t.Errorf("%s arity = (%v,%v), want (%d,%d)", symbol.Signature, symbol.ArityMin, symbol.ArityMax, wantArity, wantArity)
			}
			continue
		}
		if pair, ok := want[symbol.Name]; ok && symbol.Name != "run" {
			if pair == nil || symbol.ArityMin == nil || symbol.ArityMax == nil || *symbol.ArityMin != pair[0] || *symbol.ArityMax != pair[1] {
				t.Errorf("%s arity = (%v,%v), want (%d,%d)", symbol.Signature, symbol.ArityMin, symbol.ArityMax, pair[0], pair[1])
			}
			continue
		}
		if symbol.ArityMin != nil || symbol.ArityMax != nil {
			t.Errorf("unsupported %s has arity=(%v,%v), want unknown", symbol.Signature, symbol.ArityMin, symbol.ArityMax)
		}
	}
}

func TestKotlinFixedArityLeavesAliasedJvmStaticUnknown(t *testing.T) {
	p, err := NewKotlin().Parse(context.Background(), "Service.kt", []byte(`package lib
import kotlin.jvm.JvmStatic as Static
object Service {
    @Static fun run(value: kotlin.Int) {}
}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, symbol := range p.Symbols {
		if symbol.Kind == "function" && symbol.Name == "run" {
			if symbol.ArityMin != nil || symbol.ArityMax != nil {
				t.Fatalf("aliased JvmStatic arity=(%v,%v), want unknown", symbol.ArityMin, symbol.ArityMax)
			}
			return
		}
	}
	t.Fatal("aliased JvmStatic method not parsed")
}

func arityPair(n int) *[2]int { return &[2]int{n, n} }
