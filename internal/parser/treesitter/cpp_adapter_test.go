//go:build cgo

package treesitter

import (
	"context"
	"strings"
	"testing"
)

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
