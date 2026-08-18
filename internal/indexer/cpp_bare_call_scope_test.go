//go:build cgo

package indexer

import (
	"context"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/store"
)

// C and C++ bare-call scope (P22.13).
//
// A bare call -- `foo()`, with no receiver, no `::` and no member qualifier --
// may bind only what the calling file can be shown to see: a declaration in the
// file itself, or one in a file the caller's own `#include` names. Repository-
// global uniqueness of the spelling is not visibility evidence and never binds.
//
// The fixtures below are one per rule, so a mutation to the rule fails the
// fixture that owns it rather than a general regression.

// targets returns every `dstName -> target` pair the projection holds for one
// destination spelling, so a fixture asserts what an edge bound to rather than
// only that something happened.
func cppTargets(r *cppRepo, dstName string) []string {
	r.t.Helper()
	var out []string
	for _, line := range r.projection() {
		fields := strings.Fields(line)
		if len(fields) < 2 || fields[1] != dstName {
			continue
		}
		if i := strings.Index(line, " -> "); i >= 0 {
			target := line[i+4:]
			if j := strings.Index(target, " ("); j >= 0 {
				target = target[:j]
			}
			out = append(out, target)
		}
	}
	sort.Strings(out)
	return out
}

// TestCppBareCallSameFileResolves is the same-file positive control. A
// declaration in the calling file is a scope every language admits, and this
// phase must not cost it (P22.13 section 7).
func TestCppBareCallSameFileResolves(t *testing.T) {
	r := newCppRepo(t)
	r.write("only.cpp", `void helper() {}

void caller() {
    helper();
}
`)
	r.run("index")
	if got, want := cppTargets(r, "helper"), []string{"helper"}; !equalStrings(got, want) {
		t.Fatalf("same-file bare call targets = %v, want %v", got, want)
	}
}

func TestCppBareCallNamespaceLifecycle(t *testing.T) {
	r := newCppRepo(t)
	r.write("target.h", `namespace a { inline void foo() {} }`)
	r.write("caller.cpp", `#include "target.h"
namespace b { void caller() { foo(); } }`)
	r.run("index")
	if got := cppTargets(r, "foo"); !equalStrings(got, []string{"<unresolved>"}) {
		t.Fatalf("initial foreign namespace target = %v, want unresolved", got)
	}
	if got := r.callers("a::foo"); len(got) != 0 {
		t.Fatalf("FindCallers rebuilt initial refusal: %v", got)
	}
	r.write("caller.cpp", `#include "target.h"
namespace a { void caller() { foo(); } }`)
	r.run("update")
	if got := cppTargets(r, "foo"); !equalStrings(got, []string{"a::foo"}) {
		t.Fatalf("same-namespace move target = %v, want [a::foo]", got)
	}
	r.write("caller.cpp", `#include "target.h"
namespace b { void caller() { foo(); } }`)
	r.run("update")
	if got := cppTargets(r, "foo"); !equalStrings(got, []string{"<unresolved>"}) {
		t.Fatalf("move back foreign namespace target = %v, want unresolved", got)
	}
	r.write("target.h", `namespace c { inline void foo() {} }`)
	r.run("update")
	if got := cppTargets(r, "foo"); !equalStrings(got, []string{"<unresolved>"}) {
		t.Fatalf("renamed namespace target = %v, want unresolved", got)
	}
	r.write("target.h", `namespace b { inline void foo() {} }`)
	r.run("update")
	if got := cppTargets(r, "foo"); !equalStrings(got, []string{"b::foo"}) {
		t.Fatalf("added same-namespace target = %v, want [b::foo]", got)
	}
	r.remove("target.h")
	r.run("update")
	if got := cppTargets(r, "foo"); !equalStrings(got, []string{"<unresolved>"}) {
		t.Fatalf("removed target fell through = %v, want unresolved", got)
	}
}

// TestCppBareCallDirectIncludeResolves is the include positive control: the
// caller's own `#include` names the file that declares the target, which is
// evidence the source itself wrote (P22.13 section 8).
func TestCppBareCallDirectIncludeResolves(t *testing.T) {
	r := newCppRepo(t)
	r.write("foo.h", `inline void foo() {}
`)
	r.write("caller.cpp", `#include "foo.h"

void caller() {
    foo();
}
`)
	r.run("index")
	if got, want := cppTargets(r, "foo"), []string{"foo"}; !equalStrings(got, want) {
		t.Fatalf("direct-include bare call targets = %v, want %v", got, want)
	}
}

// TestCppBareCallSameDirectoryIncludeResolves is the commonest C and C++ form:
// a quoted single-segment include names the includer's OWN directory, which is
// an exact path rather than a repository-wide tail match.
//
// The unrelated `vendor/foo.h` is the point of the fixture: before P22.13 a
// single-segment specifier was answered by the extension-stripped suffix
// bucket, so every `*/foo.*` in the repository became visible and granted bind
// scope. Only `src/foo.h` may answer here.
func TestCppBareCallSameDirectoryIncludeResolves(t *testing.T) {
	r := newCppRepo(t)
	r.write("src/foo.h", `inline void near() {}

inline void near2() {}
`)
	r.write("vendor/foo.h", `inline void far() {}
`)
	// Both spellings of the same statement, so neither can take a different path
	// through the specifier rules.
	r.write("src/caller.cpp", `#include "foo.h"

void caller() {
    near();
    far();
}
`)
	r.write("src/dotted.cpp", `#include "./foo.h"

void dotted() {
    near2();
}
`)
	r.run("index")
	if got, want := cppTargets(r, "near2"), []string{"near2"}; !equalStrings(got, want) {
		t.Fatalf("`./foo.h` include targets = %v, want %v", got, want)
	}
	if got, want := cppTargets(r, "near"), []string{"near"}; !equalStrings(got, want) {
		t.Fatalf("same-directory include targets = %v, want %v", got, want)
	}
	if got := cppTargets(r, "far"); !equalStrings(got, []string{"<unresolved>"}) {
		t.Fatalf("unrelated `vendor/foo.h` became visible through the suffix bucket: %v", got)
	}
}

// TestCppBareCallUnrelatedFileStaysUnresolved is the load-bearing negative: the
// declaration is the ONLY one in the repository, and the caller still cannot
// see it, because nothing connects the two files (P22.13 section 9).
//
// Restoring a repository-global unique fallback fails exactly here.
func TestCppBareCallUnrelatedFileStaysUnresolved(t *testing.T) {
	r := newCppRepo(t)
	r.write("other.h", `inline void lonely() {}
`)
	r.write("caller.cpp", `void caller() {
    lonely();
}
`)
	r.run("index")
	if got := cppTargets(r, "lonely"); !equalStrings(got, []string{"<unresolved>"}) {
		t.Fatalf("unrelated cross-file bare call targets = %v, want [<unresolved>]", got)
	}
	// The query surfaces must not rebuild what the resolver refused (section 19).
	if got := r.callers("lonely"); len(got) != 0 {
		t.Fatalf("FindCallers rebuilt the refused relation: %v", got)
	}
	callees, err := r.store.FindCallees(context.Background(), r.repo.ID, "caller", 0, 20, 0)
	if err != nil {
		t.Fatalf("FindCallees() error = %v", err)
	}
	for _, sym := range callees {
		if sym.QualifiedName == "other::lonely" {
			t.Fatalf("FindCallees rebuilt the refused relation: %+v", callees)
		}
	}
}

// TestCppBareCallContextExpansionHonoursTheFileScope is the third query
// surface. `context_for_task` builds its own caller legs, and a C++ type's
// qualified_name is BARE (`Message`), so the leg that carries it is the
// qualified one -- which is why both legs have to take the scope. A writer in
// the seed's own file is a real caller and must survive; a writer in an
// unrelated file must not.
func TestCppBareCallContextExpansionHonoursTheFileScope(t *testing.T) {
	r := newCppRepo(t)
	// Two declarations of `Message` keep the resolver honestly undecided, so the
	// relation can only ever come from a name leg.
	r.write("own.cpp", `class Message {};

void near() {
    Message();
}
`)
	r.write("other.cpp", `class Message {};

void far() {
    Message();
}
`)
	r.run("index")
	sym, err := r.store.FindSymbolExact(context.Background(), r.repo.ID, "Message", 5, 0)
	if err != nil {
		t.Fatalf("FindSymbolExact() error = %v", err)
	}
	if len(sym) == 0 {
		t.Fatalf("fixture did not produce Message")
	}
	var ownID int64
	for _, candidate := range sym {
		if candidate.FilePath == "own.cpp" {
			ownID = candidate.ID
			break
		}
	}
	if ownID == 0 {
		t.Fatalf("fixture did not produce own.cpp Message: %+v", sym)
	}
	neighbors, err := r.store.FindContextNeighbors(context.Background(), r.repo.ID, []store.ContextSeed{{
		SymbolID:           ownID,
		QualifiedName:      "Message",
		ShortName:          "Message",
		AllowShortEvidence: true,
	}}, 20)
	if err != nil {
		t.Fatalf("FindContextNeighbors() error = %v", err)
	}
	var callers []string
	for _, c := range neighbors[0].Callers {
		callers = append(callers, c.QualifiedName)
	}
	sort.Strings(callers)
	for _, c := range callers {
		if c == "other::far" {
			t.Fatalf("context expansion credited a writer outside the seed's file: %v", callers)
		}
	}
	found := false
	for _, c := range callers {
		if c == "near" {
			found = true
		}
	}
	if !found {
		t.Fatalf("context expansion lost the same-file writer: %v", callers)
	}

	// FindCallers builds its own legs, and a cpp TYPE target reaches them through
	// typeOnlyGatedLanguages: subtracting cpp there would drop the bare leg
	// outright and lose the same-file writer this scope exists to keep.
	direct := r.callers("Message")
	if !slices.Contains(direct, "near") {
		t.Fatalf("FindCallers lost the same-file writer of a cpp type: %v", direct)
	}
	if slices.Contains(direct, "other::far") {
		t.Fatalf("FindCallers credited a writer outside the seed's file: %v", direct)
	}
}

// TestCppBareCallSystemIncludeDoesNotReachProject is the external-name control:
// a system include is not a project file, so a project symbol that happens to
// share the name of something the external header declares stays unreachable
// (P22.13 section 10).
func TestCppBareCallSystemIncludeDoesNotReachProject(t *testing.T) {
	r := newCppRepo(t)
	r.write("helpers.h", `inline void helper() {}
`)
	r.write("caller.cpp", `#include <external/lib.h>

void caller() {
    helper();
}
`)
	r.run("index")
	if got := cppTargets(r, "helper"); !equalStrings(got, []string{"<unresolved>"}) {
		t.Fatalf("system-include bare call targets = %v, want [<unresolved>]", got)
	}
}

// TestCppBareCallAmbiguousIncludesStayUnresolved: two included headers both
// declare the name, so nothing the call site wrote tells them apart and the
// edge fails closed rather than picking a row (P22.13 section 11).
func TestCppBareCallAmbiguousIncludesStayUnresolved(t *testing.T) {
	r := newCppRepo(t)
	r.write("a.h", `inline void twice() {}
`)
	r.write("b.h", `inline void twice() {}
`)
	r.write("caller.cpp", `#include "a.h"
#include "b.h"

void caller() {
    twice();
}
`)
	r.run("index")
	if got := cppTargets(r, "twice"); !equalStrings(got, []string{"<unresolved>"}) {
		t.Fatalf("ambiguous-include bare call targets = %v, want [<unresolved>]", got)
	}
}

// TestCppBareCallIncludedPlusUnrelatedCandidate proves ambiguity is evaluated
// after C/C++ visibility: the included declaration is eligible and the other
// repository candidate is not.
func TestCppBareCallIncludedPlusUnrelatedCandidate(t *testing.T) {
	r := newCppRepo(t)
	r.write("a.h", `void shared();
`)
	r.write("a.cpp", `#include "a.h"
void shared() {}
`)
	r.write("z.cpp", `namespace b { void shared() {} }
`)
	r.write("caller.cpp", `#include "a.h"

void caller() {
    shared();
}
`)
	r.run("index")
	if got, want := cppTargets(r, "shared"), []string{"shared"}; !equalStrings(got, want) {
		t.Fatalf("included-plus-unrelated bare call targets = %v, want %v", got, want)
	}
}

func TestCppBareCallHeaderDeclarationReachesDefinition(t *testing.T) {
	r := newCppRepo(t)
	r.write("foo.h", "void foo();\n")
	r.write("foo.cpp", "#include \"foo.h\"\nvoid foo() {}\n")
	r.write("caller.cpp", "#include \"foo.h\"\nvoid caller() { foo(); }\n")
	r.run("index")
	if got, want := cppTargets(r, "foo"), []string{"foo"}; !equalStrings(got, want) {
		t.Fatalf("header declaration targets = %v, want %v", got, want)
	}
}

func TestCppBareCallHeaderDoesNotBridgeSameStemImplementation(t *testing.T) {
	r := newCppRepo(t)
	r.write("foo.h", "void foo();\n")
	r.write("foo.cpp", "void unrelated() {}\n")
	r.write("caller.cpp", "#include \"foo.h\"\nvoid caller() { foo(); }\n")
	r.run("index")
	if got := cppTargets(r, "foo"); !equalStrings(got, []string{"<unresolved>"}) {
		t.Fatalf("header-only declaration incorrectly reached same-stem implementation: %v", got)
	}
}

func TestCppBareCallHeaderClassDeclarationReachesOutOfLineDefinition(t *testing.T) {
	r := newCppRepo(t)
	r.write("a.h", "class A { public: void foo(); void caller(); };\n")
	r.write("a.cpp", "#include \"a.h\"\nvoid A::foo() {}\nvoid A::caller() { foo(); }\n")
	r.run("index")
	if got, want := cppTargets(r, "foo"), []string{"A::foo"}; !equalStrings(got, want) {
		t.Fatalf("header class declaration targets = %v, want %v", got, want)
	}
}

// TestCBareCallScopeAppliesToC is the C control: the adapter serves C too, and
// the same visibility rule holds there through declarations and includes --
// with no namespace or member semantics involved (P22.13 section 17).
func TestCBareCallScopeAppliesToC(t *testing.T) {
	r := newCppRepo(t)
	r.write("util.c", `void ctarget(void) {}
`)
	r.write("caller.c", `void ccaller(void) {
    ctarget();
}
`)
	r.write("local.c", `void localhelper(void) {}

void localcaller(void) {
    localhelper();
}
`)
	r.run("index")
	if got := cppTargets(r, "ctarget"); !equalStrings(got, []string{"<unresolved>"}) {
		t.Fatalf("C cross-file bare call targets = %v, want [<unresolved>]", got)
	}
	if got, want := cppTargets(r, "localhelper"), []string{"localhelper"}; !equalStrings(got, want) {
		t.Fatalf("C same-file bare call targets = %v, want %v", got, want)
	}
}

// TestCppBareCallScopeLeavesQualifiedSpellingsAlone is the P22.11/P22.1
// no-regression control: this rule governs BARE spellings only. A receiver-
// bearing member call keeps its own (refusing) path, and a `::`-qualified call
// keeps its own (binding) one.
func TestCppBareCallScopeLeavesQualifiedSpellingsAlone(t *testing.T) {
	r := newCppRepo(t)
	r.write("api.h", `class Api {
public:
    static bool run();
    bool member();
};
`)
	r.write("api.cpp", `#include "api.h"

bool Api::run() { return true; }
bool Api::member() { return true; }
`)
	// `Other::run` has no visible declaration, so qualified identity alone does
	// not bridge translation units.
	r.write("far.cpp", `bool Other::run() { return true; }
`)
	r.write("caller.cpp", `#include "api.h"

void caller(Api* p, Api& v) {
    Api::run();
    Other::run();
    p->member();
    v.member();
}
`)
	r.run("index")
	if got, want := cppTargets(r, "Api::run"), []string{"Api::run"}; !equalStrings(got, want) {
		t.Fatalf("qualified call Api::run targets = %v, want %v", got, want)
	}
	if got := cppTargets(r, "Other::run"); !equalStrings(got, []string{"<unresolved>"}) {
		t.Fatalf("qualified call without declaration targets = %v, want unresolved", got)
	}
	for _, spelling := range []string{"p.member", "v.member"} {
		if got := cppTargets(r, spelling); !equalStrings(got, []string{"<unresolved>"}) {
			t.Fatalf("member call %q targets = %v, want [<unresolved>]", spelling, got)
		}
	}
}

// TestCppBareCallIncludeLifecycle walks the import lifecycle of section 28: an
// unrelated declaration exists, the caller adds the include that makes it
// visible, then removes it again. Every step's incremental result must equal a
// fresh index of the same tree.
func TestCppBareCallIncludeLifecycle(t *testing.T) {
	const target = `inline void visible() {}
`
	noInclude := `void caller() {
    visible();
}
`
	withInclude := `#include "dep.h"

void caller() {
    visible();
}
`
	r := newCppRepo(t)
	r.write("dep.h", target)
	r.write("caller.cpp", noInclude)
	r.run("index")
	if got := cppTargets(r, "visible"); !equalStrings(got, []string{"<unresolved>"}) {
		t.Fatalf("step 1 targets = %v, want [<unresolved>]", got)
	}

	r.write("caller.cpp", withInclude)
	r.run("update")
	if got, want := cppTargets(r, "visible"), []string{"visible"}; !equalStrings(got, want) {
		t.Fatalf("step 2 (include added) targets = %v, want %v", got, want)
	}
	assertCppFreshParity(t, "include added", r, map[string]string{"dep.h": target, "caller.cpp": withInclude})

	r.write("caller.cpp", noInclude)
	r.run("update")
	if got := cppTargets(r, "visible"); !equalStrings(got, []string{"<unresolved>"}) {
		t.Fatalf("step 3 (include removed) targets = %v, want [<unresolved>]", got)
	}
	assertCppFreshParity(t, "include removed", r, map[string]string{"dep.h": target, "caller.cpp": noInclude})
}

// TestCppBareCallTargetVisibilityLifecycle is the other direction of section
// 29: the include never changes, the DECLARATION moves.
func TestCppBareCallTargetVisibilityLifecycle(t *testing.T) {
	const caller = `#include "dep.h"

void caller() {
    moved();
}
`
	r := newCppRepo(t)
	r.write("dep.h", "inline void moved() {}\n")
	r.write("caller.cpp", caller)
	r.run("index")
	if got, want := cppTargets(r, "moved"), []string{"moved"}; !equalStrings(got, want) {
		t.Fatalf("step 1 targets = %v, want %v", got, want)
	}

	// The included header stops declaring it; an unrelated file picks it up.
	r.write("dep.h", "inline void other() {}\n")
	r.write("far.h", "inline void moved() {}\n")
	r.run("update")
	if got := cppTargets(r, "moved"); !equalStrings(got, []string{"<unresolved>"}) {
		t.Fatalf("step 2 (declaration moved out of scope) targets = %v, want [<unresolved>]", got)
	}
	assertCppFreshParity(t, "declaration moved", r, map[string]string{
		"dep.h":      "inline void other() {}\n",
		"far.h":      "inline void moved() {}\n",
		"caller.cpp": caller,
	})
}

// assertCppFreshParity indexes `files` into a brand-new repository and compares
// its semantic projection with the one the update history produced.
func assertCppFreshParity(t *testing.T, step string, updated *cppRepo, files map[string]string) {
	t.Helper()
	fresh := newCppRepo(t)
	for name, src := range files {
		fresh.write(name, src)
	}
	fresh.run("index")
	want, got := fresh.projection(), updated.projection()
	if diff := projectionDiff(want, got); diff != "" {
		t.Fatalf("%s: update projection differs from a fresh index:\n%s", step, diff)
	}
}
