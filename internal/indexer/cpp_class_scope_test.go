//go:build cgo

package indexer

import (
	"context"
	"database/sql"
	"reflect"
	"sort"
	"testing"
)

// P22.15 closeout: a bare C/C++ call may not claim a class member without
// class-scope evidence (internal/store/cpp_class_scope.go).
//
// Every fixture here is real source through the real C++ adapter, because the
// rule reads facts only the parser produces -- `container_name`, the class
// declaration's span, and the out-of-line declarator spelling. A hand-inserted
// symbol row would let the rule pass against a shape the adapter never emits.

// boundTargets returns the qualified names this repository bound the given bare
// spelling to, one entry per edge, sorted. Empty means the spelling is
// unresolved everywhere, which is what a refusal looks like.
func (r *cppRepo) boundTargets(dstName string) []string {
	r.t.Helper()
	// Same handle discipline as projection(): the raw reader opens the file the
	// Store owns, so the Store is released for the duration and reopened after.
	r.closeStore()
	defer r.openStore()

	db, err := sql.Open("sqlite", r.dbPath)
	if err != nil {
		r.t.Fatalf("sql.Open() error = %v", err)
	}
	defer db.Close()
	rows, err := db.QueryContext(context.Background(), `
		SELECT dst.qualified_name
		FROM edges e
		JOIN symbols dst ON dst.id = e.dst_symbol_id
		WHERE e.dst_name = ?`, dstName)
	if err != nil {
		r.t.Fatalf("boundTargets(%s) error = %v", dstName, err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var qualified string
		if err := rows.Scan(&qualified); err != nil {
			r.t.Fatalf("boundTargets(%s) scan error = %v", dstName, err)
		}
		out = append(out, qualified)
	}
	if err := rows.Err(); err != nil {
		r.t.Fatalf("boundTargets(%s) rows error = %v", dstName, err)
	}
	sort.Strings(out)
	return out
}

// callees is the callee-direction twin of cppRepo.callers.
func (r *cppRepo) callees(symbol string) []string {
	r.t.Helper()
	found, err := r.store.FindCallees(context.Background(), r.repo.ID, symbol, 0, 50, 0)
	if err != nil {
		r.t.Fatalf("FindCallees(%s) error = %v", symbol, err)
	}
	out := make([]string, 0, len(found))
	for _, sym := range found {
		out = append(out, sym.QualifiedName)
	}
	sort.Strings(out)
	return out
}

func wantBound(t *testing.T, got []string, want ...string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}
	if len(got) == 0 {
		got = []string{}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("bound targets = %v, want %v", got, want)
	}
}

// TestCppBareCallBindsOwnClassMember is the positive: the caller and the target
// are members of the same class, declared in the same class body, which is the
// one class fact the graph holds. Recall this rule must keep.
func TestCppBareCallBindsOwnClassMember(t *testing.T) {
	r := newCppRepo(t)
	r.write("a.cpp", "struct A {\n    void foo() {}\n    void caller() { foo(); }\n};\n")
	r.run("index")

	wantBound(t, r.boundTargets("foo"), "A::foo")
	if got := r.callers("A::foo"); len(got) != 1 || got[0] != "A::caller" {
		t.Fatalf("FindCallers(A::foo) = %v, want [A::caller]", got)
	}
}

// TestCppBareCallBindsOwnClassMemberOutOfLine is the same evidence reached the
// other way: the caller is defined out of line, so its class comes from the
// declarator spelling matched against the class this file declares.
func TestCppBareCallBindsOwnClassMemberOutOfLine(t *testing.T) {
	r := newCppRepo(t)
	r.write("a.cpp", "struct A {\n    void foo() {}\n    void caller();\n};\nvoid A::caller() {\n    foo();\n}\n")
	r.run("index")

	wantBound(t, r.boundTargets("foo"), "A::foo")
}

// TestCppBareCallRefusesOtherClassMember is the load-bearing negative: `B::foo`
// is the only `foo` in the repository, and a caller in `A` still may not have
// it. Repository-global uniqueness is not class scope.
//
// The caller INCLUDES the declaring header on purpose. Without the include,
// P22.13's file scope would refuse the edge on its own and the fixture would
// pass whatever this rule does; with it, the class rule is the only thing left
// standing between the call and the wrong member.
func TestCppBareCallRefusesOtherClassMember(t *testing.T) {
	r := newCppRepo(t)
	r.write("a.cpp", "#include \"b.h\"\n\nstruct A {\n    void caller() { foo(); }\n};\n")
	r.write("b.h", "struct B {\n    void foo() {}\n};\n")
	r.run("index")

	wantBound(t, r.boundTargets("foo"))
	if got := r.unresolved("foo"); got != 1 {
		t.Fatalf("unresolved foo = %d, want 1", got)
	}
	// P22.15 §18: the query surfaces may not rebuild what the resolver refused.
	if got := r.callers("B::foo"); len(got) != 0 {
		t.Fatalf("FindCallers(B::foo) = %v, want none", got)
	}
	if got := r.callees("A::caller"); len(got) != 0 {
		t.Fatalf("FindCallees(A::caller) = %v, want none", got)
	}
}

// TestCppBareCallRefusesOtherClassMemberSameFile is P22.13's boundary: same-file
// visibility is FILE evidence, and a class member is not reachable by it. Two
// classes in one translation unit see each other's names, not each other's
// members.
func TestCppBareCallRefusesOtherClassMemberSameFile(t *testing.T) {
	r := newCppRepo(t)
	r.write("a.cpp", "struct A {\n    void foo() {}\n};\nstruct B {\n    void caller() { foo(); }\n};\n")
	r.run("index")

	wantBound(t, r.boundTargets("foo"))
	if got := r.callers("A::foo"); len(got) != 0 {
		t.Fatalf("FindCallers(A::foo) = %v, want none", got)
	}
	if got := r.callees("B::caller"); len(got) != 0 {
		t.Fatalf("FindCallees(B::caller) = %v, want none", got)
	}
}

// TestCppBareCallRefusesClassMemberFromFreeFunction: a free function has no
// class at all, so it can reach no member however unique the spelling is.
func TestCppBareCallRefusesClassMemberFromFreeFunction(t *testing.T) {
	r := newCppRepo(t)
	r.write("a.cpp", "struct A {\n    void foo() {}\n};\nvoid caller() { foo(); }\n")
	r.run("index")

	wantBound(t, r.boundTargets("foo"))
	if got := r.callers("A::foo"); len(got) != 0 {
		t.Fatalf("FindCallers(A::foo) = %v, want none", got)
	}
}

// TestCppBareCallRefusesIncludedClassMember: an `#include` makes the class NAME
// visible, it does not put the includer inside the class. P22.13's include
// evidence is not class evidence.
func TestCppBareCallRefusesIncludedClassMember(t *testing.T) {
	r := newCppRepo(t)
	r.write("a.h", "struct A {\n    void foo() {}\n};\n")
	r.write("caller.cpp", "#include \"a.h\"\n\nvoid caller() { foo(); }\n")
	r.run("index")

	wantBound(t, r.boundTargets("foo"))
}

// TestCppBareCallRefusesSameNamedClassInAnotherFile is the P22.16 boundary
// control (§22): CodeGraph does not model namespaces, so two files may each
// declare a `class A` that the current identity cannot tell apart. Comparing the
// declaring FILE as well as the name is what makes that pair fail closed instead
// of letting an `#include` hand one `A` the other's member.
func TestCppBareCallRefusesSameNamedClassInAnotherFile(t *testing.T) {
	r := newCppRepo(t)
	r.write("b.h", "struct A {\n    void foo() {}\n};\n")
	r.write("a.cpp", "#include \"b.h\"\n\nstruct A {\n    void caller() { foo(); }\n};\n")
	r.run("index")

	wantBound(t, r.boundTargets("foo"))
	if got := r.callers("A::foo"); len(got) != 0 {
		t.Fatalf("FindCallers(A::foo) = %v, want none", got)
	}
}

// TestCppBareCallRefusesInheritedMember records the recall this rule knowingly
// loses. `Derived::caller` calling `base()` is source-correct C++, and CodeGraph
// models no base-class relation, so admitting it would mean admitting every
// same-named member anywhere. This is where googletest's 36 source-correct
// `GetParam` -> `WithParamInterface::GetParam` bindings went.
func TestCppBareCallRefusesInheritedMember(t *testing.T) {
	r := newCppRepo(t)
	r.write("a.cpp", "struct Base {\n    void base() {}\n};\nstruct Derived : Base {\n    void caller() { base(); }\n};\n")
	r.run("index")

	wantBound(t, r.boundTargets("base"))
}

// TestCppBareCallKeepsFreeFunctionRecall is the P22.13 control: this closeout
// governs class members and nothing else, so an ordinary same-file free-function
// call still binds -- including from inside a class body, which is the case a
// rule written as "a member may only call its own class" would have broken.
func TestCppBareCallKeepsFreeFunctionRecall(t *testing.T) {
	r := newCppRepo(t)
	r.write("a.cpp", "void helper() {}\nvoid caller() { helper(); }\n"+
		"struct A {\n    void member() { helper(); }\n};\n")
	r.run("index")

	wantBound(t, r.boundTargets("helper"), "helper", "helper")
	callers := r.callers("helper")
	if len(callers) != 2 || callers[0] != "A::member" || callers[1] != "caller" {
		t.Fatalf("FindCallers(helper) = %v, want [A::member caller]", callers)
	}
}

// TestCppBareCallMemberAndFreeFunctionFailsClosed is §15 and §16 together, and
// it records a limitation rather than hiding it.
//
// Source-wise a free `caller()` reaches the free `foo`; the member is not an
// eligible candidate and must not veto it.
func TestCppBareCallMemberAndFreeFunctionFailsClosed(t *testing.T) {
	r := newCppRepo(t)
	r.write("a.cpp", "struct A {\n    void foo() {}\n};\nvoid foo() {}\nvoid caller() { foo(); }\n")
	r.run("index")

	wantBound(t, r.boundTargets("foo"), "foo")
	if got := r.callers("A::foo"); len(got) != 0 {
		t.Fatalf("FindCallers(A::foo) = %v, want none", got)
	}
}

// TestCppBareCallScopedCandidateNotFirstRow pins §17's safe half: with an
// eligible `A::foo` and an ineligible `B::foo`, the answer is never `B::foo` and
// never depends on which file was indexed first. (That the eligible one is not
// bound either is the repo-wide ambiguity level documented above.)
func TestCppBareCallScopedCandidateNotFirstRow(t *testing.T) {
	for _, order := range [][]string{{"a.cpp", "b.h"}, {"b.h", "a.cpp"}} {
		r := newCppRepo(t)
		sources := map[string]string{
			// The include is what makes `B::foo` reachable at P22.13's file
			// level, so the only thing refusing it is its class.
			"a.cpp": "#include \"b.h\"\n\nstruct A {\n    void foo() {}\n    void caller() { foo(); }\n};\n",
			"b.h":   "struct B {\n    void foo() {}\n};\n",
		}
		for _, name := range order {
			r.write(name, sources[name])
		}
		r.run("index")
		for _, bound := range r.boundTargets("foo") {
			if bound == "B::foo" {
				t.Fatalf("order %v bound the ineligible B::foo", order)
			}
		}
		if got := r.callers("B::foo"); len(got) != 0 {
			t.Fatalf("order %v: FindCallers(B::foo) = %v, want none", order, got)
		}
	}
}

// TestCppBareCallClassScopeLifecycle is §20: every state an edit sequence puts
// the graph through must answer the way a fresh index of that same tree does,
// and a deletion must never fall through to an ineligible class's member.
func TestCppBareCallClassScopeLifecycle(t *testing.T) {
	const withFoo = "struct A {\n    void foo() {}\n    void caller() { foo(); }\n};\n"
	const withoutFoo = "struct A {\n    void caller() { foo(); }\n};\n"
	const otherClass = "struct B {\n    void foo() {}\n};\n"
	const callerInB = "struct A {\n    void foo() {}\n};\nstruct B {\n    void caller() { foo(); }\n};\n"

	steps := []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{"a: own class only", map[string]string{"a.cpp": withFoo}, []string{"A::foo"}},
		// b/d: an unrelated class does not veto the eligible same-class member.
		{"b: other class appears", map[string]string{"a.cpp": withFoo, "b.cpp": otherClass}, []string{"A::foo"}},
		{"c: own member removed", map[string]string{"a.cpp": withoutFoo, "b.cpp": otherClass}, nil},
		{"d: own member restored", map[string]string{"a.cpp": withFoo, "b.cpp": otherClass}, []string{"A::foo"}},
		{"e: other class gone", map[string]string{"a.cpp": withFoo}, []string{"A::foo"}},
		{"f: caller moved to another class", map[string]string{"a.cpp": callerInB}, nil},
	}

	incremental := newCppRepo(t)
	for i, step := range steps {
		for name, src := range step.files {
			incremental.write(name, src)
		}
		for _, name := range []string{"a.cpp", "b.cpp"} {
			if _, kept := step.files[name]; !kept && i > 0 {
				if _, existed := steps[i-1].files[name]; existed {
					incremental.remove(name)
				}
			}
		}
		kind := "update"
		if i == 0 {
			kind = "index"
		}
		incremental.run(kind)

		got := incremental.boundTargets("foo")
		wantBound(t, got, step.want...)
		for _, bound := range got {
			if bound == "B::foo" {
				t.Fatalf("step %q bound B::foo", step.name)
			}
		}

		// Fresh index of the same tree, same answer.
		fresh := newCppRepo(t)
		for name, src := range step.files {
			fresh.write(name, src)
		}
		fresh.run("index")
		if a, b := incremental.projection(), fresh.projection(); !reflect.DeepEqual(a, b) {
			t.Fatalf("step %q: incremental projection\n%v\nfresh projection\n%v", step.name, a, b)
		}
	}
}

// TestCppBareCallClassRenameRedecides is §21: renaming the class both symbols
// belong to must leave the binding decided by the NEW class identity, and the
// incremental path must reach the same answer a fresh index does.
func TestCppBareCallClassRenameRedecides(t *testing.T) {
	r := newCppRepo(t)
	r.write("a.cpp", "struct A {\n    void foo() {}\n    void caller() { foo(); }\n};\n")
	r.run("index")
	wantBound(t, r.boundTargets("foo"), "A::foo")

	r.write("a.cpp", "struct C {\n    void foo() {}\n    void caller() { foo(); }\n};\n")
	r.run("update")
	wantBound(t, r.boundTargets("foo"), "C::foo")

	fresh := newCppRepo(t)
	fresh.write("a.cpp", "struct C {\n    void foo() {}\n    void caller() { foo(); }\n};\n")
	fresh.run("index")
	if a, b := r.projection(), fresh.projection(); !reflect.DeepEqual(a, b) {
		t.Fatalf("rename: incremental projection\n%v\nfresh projection\n%v", a, b)
	}
}

// TestCppBareCallClassScopeReceiverControlsUnchanged is §32: this rule governs
// the BARE spelling only. A call that names a qualifier still binds on that
// qualifier, and a receiver CodeGraph cannot type still stays unresolved
// (P22.11) -- neither answer moves.
func TestCppBareCallClassScopeReceiverControlsUnchanged(t *testing.T) {
	r := newCppRepo(t)
	r.write("a.cpp", "struct A {\n    static void foo() {}\n};\n"+
		"void caller() {\n    A::foo();\n}\n")
	r.write("b.cpp", "struct B {\n    void foo() {}\n};\n"+
		"void other(B* b, B& r) {\n    b->foo();\n    r.foo();\n}\n")
	r.run("index")

	wantBound(t, r.boundTargets("A::foo"), "A::foo")
	for _, spelling := range []string{"b.foo", "r.foo"} {
		if got := r.unresolved(spelling); got != 1 {
			t.Fatalf("unresolved %s = %d, want 1 (P22.11 receiver kept, not bound)", spelling, got)
		}
	}
}
