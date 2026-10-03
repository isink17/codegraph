//go:build cgo

package indexer

import (
	"reflect"
	"testing"
)

// A C++ spelling the evidence pass owns (qualified, bare, macro-unexpanded) is
// answered by that pass or stays unresolved. An incremental resolve must not
// hand an edge the pass refused to a repository-wide name lookup: the answer
// may not depend on whether the declaring file arrived before or after the
// caller, or on which file an update touched.
func TestCppEvidenceOwnershipAllEntrypoints(t *testing.T) {
	cases := []struct {
		name   string
		caller string            // a.cpp
		others map[string]string // every other file
		dst    string
		want   []string
	}{
		{
			name:   "namespace function without include",
			caller: "void caller() { ns::foo(); }\n",
			others: map[string]string{"b.cpp": "namespace ns {\nvoid foo() {}\n}\n"},
			dst:    "ns::foo",
		},
		{
			name:   "static member without include",
			caller: "void caller() { A::foo(); }\n",
			others: map[string]string{"b.cpp": "struct A {\n    static void foo() {}\n};\n"},
			dst:    "A::foo",
		},
		{
			name:   "bare call without include",
			caller: "void caller() { foo(); }\n",
			others: map[string]string{"b.cpp": "void foo() {}\n"},
			dst:    "foo",
		},
		{
			name:   "namespace function arity mismatch",
			caller: "namespace ns { void foo(int); }\nvoid caller() { ns::foo(); }\n",
			others: map[string]string{"b.cpp": "namespace ns {\nvoid foo(int) {}\n}\n"},
			dst:    "ns::foo",
		},
		{
			name:   "included namespace function",
			caller: "#include \"b.h\"\nvoid caller() { ns::foo(); }\n",
			others: map[string]string{"b.h": "namespace ns {\nvoid foo() {}\n}\n"},
			dst:    "ns::foo",
			want:   []string{"ns::foo"},
		},
		// The evidence pass does not bind this class-qualified static call even
		// with the header included, and a fresh index leaves it unresolved. The
		// incremental paths used to bind it through the generic qualified lookup;
		// they now agree with the fresh answer. Supporting the form is a recall
		// change for the evidence pass, not for this ownership rule.
		{
			name:   "included static member",
			caller: "#include \"b.h\"\nvoid caller() { A::foo(); }\n",
			others: map[string]string{"b.h": "struct A {\n    static void foo() {}\n};\n"},
			dst:    "A::foo",
		},
	}
	writeOthers := func(r *cppRepo, files map[string]string) {
		for name, src := range files {
			r.write(name, src)
		}
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			entrypoints := map[string]func() *cppRepo{
				"fresh": func() *cppRepo {
					r := newCppRepo(t)
					r.write("a.cpp", c.caller)
					writeOthers(r, c.others)
					r.run("index")
					return r
				},
				// The declaring file arrives after the caller: a name-targeted update.
				"names": func() *cppRepo {
					r := newCppRepo(t)
					r.write("a.cpp", c.caller)
					r.run("index")
					writeOthers(r, c.others)
					r.run("update")
					return r
				},
				// Only the caller changes: a path-scoped update.
				"paths": func() *cppRepo {
					r := newCppRepo(t)
					r.write("a.cpp", c.caller)
					writeOthers(r, c.others)
					r.run("index")
					r.write("a.cpp", "// touched\n"+c.caller)
					r.run("update")
					return r
				},
				"combined": func() *cppRepo {
					r := newCppRepo(t)
					r.write("a.cpp", c.caller)
					r.run("index")
					writeOthers(r, c.others)
					r.write("a.cpp", "// touched\n"+c.caller)
					r.run("update")
					return r
				},
			}
			for _, entry := range []string{"fresh", "names", "paths", "combined"} {
				r := entrypoints[entry]()
				got := r.boundTargets(c.dst)
				want := c.want
				if want == nil {
					want = []string{}
				}
				if got == nil {
					got = []string{}
				}
				if !reflect.DeepEqual(got, want) {
					t.Errorf("%s: bound %s = %v, want %v", entry, c.dst, got, want)
				}
			}
		})
	}
}
