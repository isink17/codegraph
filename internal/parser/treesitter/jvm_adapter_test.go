//go:build cgo

package treesitter

import (
	"context"
	"strings"
	"testing"
)

func TestJavaDeclarationEvidenceKeepsLexicalContainersAndSignatures(t *testing.T) {
	src := `class Outer { class Inner { class Deep { void f() {} } } }
class Calculator { int add(int value) { return value; } String add(String value) { return value; } }
class Foo { Foo(int x) {} void make() { new Foo(1); factory().run(); } }`
	p, err := NewJava().Parse(context.Background(), "Sample.java", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"Outer.Inner.Deep":      "Outer.Inner",
		"Outer.Inner.Deep.f":    "Outer.Inner.Deep",
		"Calculator.add#int":    "int add(int value)",
		"Calculator.add#String": "String add(String value)",
		"Foo.Foo":               "Foo",
	}
	seen := map[string]bool{}
	seenNested := map[string]bool{}
	for _, s := range p.Symbols {
		q := strings.TrimPrefix(s.QualifiedName, "Sample.")
		if strings.HasPrefix(q, "Calculator.add") {
			if s.Signature == "" {
				t.Errorf("empty Java signature: %+v", s)
			}
			if strings.Contains(s.Signature, "return") {
				t.Errorf("body leaked into signature: %q", s.Signature)
			}
			key := "Calculator.add#" + strings.TrimSpace(strings.TrimPrefix(strings.TrimSuffix(s.Signature, ")"), "String add(String value"))
			if strings.Contains(s.Signature, "int add") {
				key = "Calculator.add#int"
			}
			if strings.Contains(s.Signature, "String add") {
				key = "Calculator.add#String"
			}
			seen[key] = s.ContainerName == "Calculator"
			continue
		}
		if got, ok := want[q]; ok {
			seenNested[q] = true
			if s.ContainerName != got {
				t.Errorf("%s container = %q, want %q", q, s.ContainerName, got)
			}
			if q == "Outer.Inner.Deep.f" && s.Signature == "" {
				t.Error("empty nested method signature")
			}
			if q == "Foo.Foo" && s.Signature == "" {
				t.Error("empty constructor signature")
			}
			if q == "Foo.Foo" && (s.Kind != "function" || s.Name != "Foo" || s.QualifiedName != "Sample.Foo.Foo" || s.Signature != "Foo(int x)") {
				t.Errorf("constructor metadata = %+v", s)
			}
		}
	}
	for _, q := range []string{"Outer.Inner.Deep", "Outer.Inner.Deep.f", "Foo.Foo"} {
		if !seenNested[q] {
			t.Errorf("missing declaration %s", q)
		}
	}
	if !seen["Calculator.add#int"] || !seen["Calculator.add#String"] {
		t.Fatalf("overload signatures missing: %v", seen)
	}
	var calls []string
	for _, e := range p.Edges {
		if e.Kind == "calls" {
			calls = append(calls, e.DstName)
		}
	}
	for _, got := range calls {
		if got == "factory().run" {
			t.Fatalf("raw chained receiver emitted: %v", calls)
		}
	}
	if len(calls) != 1 || calls[0] != "factory" {
		t.Fatalf("Java calls = %v, want [factory]", calls)
	}
}

func TestKotlinDeclarationEvidenceKeepsLexicalContainersAndSignatures(t *testing.T) {
	src := `class Outer {
  class Inner {
    class Deep {
      fun f() {}
    }
  }
}
class Calculator {
  fun add(value: Int): Int = value
  fun add(value: String): String = value
}`
	p, err := NewKotlin().Parse(context.Background(), "Sample.kt", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	declarations := map[string]bool{}
	for _, s := range p.Symbols {
		q := strings.TrimPrefix(s.QualifiedName, "Sample.")
		declarations[q] = true
		if q == "Outer.Inner.Deep" && s.ContainerName != "Outer.Inner" {
			t.Errorf("Deep container = %q", s.ContainerName)
		}
		if q == "Outer.Inner.Deep.f" && s.ContainerName != "Outer.Inner.Deep" {
			t.Errorf("f container = %q", s.ContainerName)
		}
		if strings.HasPrefix(q, "Calculator.add") {
			if s.Signature == "" || strings.Contains(s.Signature, "value; fun") {
				t.Errorf("bad Kotlin signature: %q", s.Signature)
			}
			found[s.Signature] = true
		}
	}
	if len(found) != 2 {
		t.Fatalf("Kotlin overload signatures = %v", found)
	}
	for _, q := range []string{"Outer.Inner.Deep", "Outer.Inner.Deep.f"} {
		if !declarations[q] {
			t.Errorf("missing Kotlin declaration %s", q)
		}
	}
}
