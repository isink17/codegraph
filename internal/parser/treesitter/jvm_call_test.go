//go:build cgo

package treesitter

import (
	"context"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

func TestJavaChainedCallPreservesInnerCall(t *testing.T) {
	p, err := NewJava().Parse(context.Background(), "C.java", []byte("class C {\n void f() {\n  factory().run();\n }\n}"))
	if err != nil {
		t.Fatal(err)
	}
	var calls int
	for _, e := range p.Edges {
		if e.Kind != "calls" {
			continue
		}
		calls++
		if e.DstName != "factory" || e.Evidence != "factory()" || e.Line != 3 {
			t.Fatalf("call = %+v", e)
		}
		if strings.ContainsAny(e.DstName, "().") {
			t.Fatalf("malformed call = %+v", e)
		}
	}
	if calls != 1 {
		t.Fatalf("calls = %d, want 1", calls)
	}
}

func TestJavaConstructorInvocationIsDistinct(t *testing.T) {
	p, err := NewJava().Parse(context.Background(), "C.java", []byte(`class C { void f() { new a.b.Foo(1); } }`))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Edges) != 1 || p.Edges[0].Kind != "constructs" || p.Edges[0].DstName != "a.b.Foo" {
		t.Fatalf("constructor edges = %+v", p.Edges)
	}
}

func TestJavaMethodInvocationCallArityIsDirectASTArgumentCount(t *testing.T) {
	src := `class C { void f() {
 Service.run();
 Service.run(1);
 Service.run(1, 2);
 Service.run(foo(1, 2), x);
 Service.run("a,b", x);
 Service.run(',', x);
 Service.run(new Pair<>(1, 2), x);
 Service.run(new int[]{1, 2}, x);
 Service.run(flag ? a : b, x);
 Service.run((x, y) -> x + y, z);
 Service.run(List.of(1, 2), Map.of("a", 1, "b", 2));
 Service.<String>run(value);
} }`
	p, err := NewJava().Parse(context.Background(), "C.java", []byte(src))
	if err != nil {
		t.Fatal(err)
	}
	want := []int{0, 1, 2, 2, 2, 2, 2, 2, 2, 2, 2, 1}
	var calls []graph.Edge
	for _, edge := range p.Edges {
		if edge.DstName == "Service.run" {
			calls = append(calls, edge)
		}
	}
	if len(calls) != len(want) {
		t.Fatalf("Service.run edges = %d, want %d", len(calls), len(want))
	}
	for i, edge := range calls {
		if edge.CallArity == nil || *edge.CallArity != want[i] {
			t.Errorf("%q CallArity = %v, want %d", edge.Evidence, edge.CallArity, want[i])
		}
	}
	if calls[len(calls)-1].Evidence != "Service.<String>run(value)" {
		t.Fatalf("explicit type-argument evidence = %q", calls[len(calls)-1].Evidence)
	}
	var nestedFoo, nestedList, nestedMap *int
	for _, edge := range p.Edges {
		if edge.Kind != "calls" || edge.CallArity == nil {
			continue
		}
		switch edge.DstName {
		case "foo":
			nestedFoo = edge.CallArity
		case "List.of":
			nestedList = edge.CallArity
		case "Map.of":
			nestedMap = edge.CallArity
		}
	}
	if nestedFoo == nil || *nestedFoo != 2 || nestedList == nil || *nestedList != 2 || nestedMap == nil || *nestedMap != 4 {
		t.Fatalf("nested call arities foo=%v List.of=%v Map.of=%v", nestedFoo, nestedList, nestedMap)
	}
}

// Bare and `this.` calls inside a class body the adapter extracts no members
// of are marked, so the resolver never answers them from an enclosing class.
// Lambdas and member classes are not such bodies; qualified calls keep their
// text.
func TestJavaNestedClassBodyCallsAreMarked(t *testing.T) {
	src := `package app;
class C {
    void m() {
        new Runnable() { public void run() { anon(); this.anonThis(); C.qualified(); } };
        class Local { void go() { local(); } }
        record R(int x) { void go() { rec(); } }
        interface I { default void go() { iface(); } }
        Runnable r = () -> lambda();
        plain();
    }
    class Member { void go() { member(); } }
    enum E { A { void go() { constant(); } }; void go() { enumMember(); } }
}
`
	marked := map[string]bool{"anon": true, "this.anonThis": true, "local": true, "rec": true, "iface": true, "constant": true}
	for _, adapter := range []*JavaAdapter{NewJava(), NewJavaV4()} {
		p, err := adapter.Parse(context.Background(), "C.java", []byte(src))
		if err != nil {
			t.Fatal(err)
		}
		seen := map[string]bool{}
		current := adapter.Profile().ID != "treesitter:java:v4"
		for _, e := range p.Edges {
			if e.Kind != "calls" {
				continue
			}
			seen[e.DstName] = true
			want := current && marked[e.DstName]
			if got := e.Evidence == graph.JavaCallNestedClassScopeEvidence; got != want {
				t.Errorf("%s %s evidence = %q, marked=%v want %v", adapter.Profile().ID, e.DstName, e.Evidence, got, want)
			}
		}
		for _, name := range []string{"anon", "this.anonThis", "C.qualified", "local", "rec", "iface", "lambda", "plain", "member", "constant", "enumMember"} {
			if !seen[name] {
				t.Errorf("%s: no call edge for %s", adapter.Profile().ID, name)
			}
		}
	}
}

// TestJavaConstructionSpellsRawClass pins the class a construction names:
// trailing type arguments are not part of it (v5 kept them). Type arguments
// on an outer segment, which javac rejects, and a qualified creation, whose
// class only outer's type can name, keep their source text.
func TestJavaConstructionSpellsRawClass(t *testing.T) {
	src := `package app; class C { void x() {
		new Box<>(1); new Box<String>(1); new a.b.Box<>(); new Map<String, List<Integer>>();
		new Outer<String>.Inner<Integer>(); new Box<>(1) {}; new Box(); new Box /* c */ <>(); new a.b.Raw();
		o.new Inner<>(); new Outer.Inner<>(); new @A Box<>();
	} }`
	want := []string{"Box", "Box", "a.b.Box", "Map", "Outer<String>.Inner<Integer>", "Box", "Box", "Box", "a.b.Raw", "Inner<>", "Outer.Inner", "Box"}
	legacy := []string{"Box<>", "Box<String>", "a.b.Box<>", "Map<String, List<Integer>>", "Outer<String>.Inner<Integer>", "Box<>", "Box", "Box /* c */ <>", "a.b.Raw", "Inner<>", "Outer.Inner<>", "Box<>"}
	for _, tc := range []struct {
		adapter *JavaAdapter
		want    []string
	}{{NewJava(), want}, {NewJavaV5(), legacy}} {
		p, err := tc.adapter.Parse(context.Background(), "C.java", []byte(src))
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, e := range p.Edges {
			if e.Kind == "constructs" {
				got = append(got, e.DstName)
			}
		}
		if strings.Join(got, "|") != strings.Join(tc.want, "|") {
			t.Errorf("%s: constructs %q, want %q", tc.adapter.Profile().ID, got, tc.want)
		}
	}
}

// TestJavaLocalTypeConstructionsAreMarked pins which constructions and
// qualified calls name a type declared in an enclosing block or an unmodeled
// class body. Member and top-level types are recorded symbols and stay
// unmarked, as does a local type in a block that does not enclose the call.
// v5 marked none.
func TestJavaLocalTypeConstructionsAreMarked(t *testing.T) {
	src := `package app;
class Top {}
class C {
    class Member {}
    void m() {
        class L {}
        new L(); new L.In(); L.make(); new Member(); new Top(); Member.make(); l.go();
        Runnable r = () -> { record R() {} new R(); };
        new Object() { class A {} void f() { new A(); } };
        if (true) { interface Gone {} }
        Gone.go();
        this.go(); new Later(); enum Later {}
    }
}
`
	marked := map[string]bool{"L": true, "L.In": true, "L.make": true, "R": true, "A": true, "Later": true}
	for _, adapter := range []*JavaAdapter{NewJava(), NewJavaV5()} {
		p, err := adapter.Parse(context.Background(), "C.java", []byte(src))
		if err != nil {
			t.Fatal(err)
		}
		current := adapter.Profile().ID == "treesitter:java:v6"
		seen := 0
		for _, e := range p.Edges {
			if e.Kind != "calls" && e.Kind != "constructs" {
				continue
			}
			seen++
			want := current && marked[e.DstName]
			if got := e.Evidence == graph.JavaLocalTypeScopeEvidence; got != want {
				t.Errorf("%s %s %s evidence = %q, want marked %v", adapter.Profile().ID, e.Kind, e.DstName, e.Evidence, want)
			}
		}
		if seen != 13 {
			t.Errorf("%s: %d call and construct edges, want 13", adapter.Profile().ID, seen)
		}
	}
}
