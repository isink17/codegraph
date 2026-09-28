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
		{"JvmName veto", `@JvmName("renamed") @JvmOverloads fun run(a: kotlin.Int = 0) {}`, false, 0, 0},
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
