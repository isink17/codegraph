package store

import (
	"strings"
	"testing"
)

// Regression coverage for P22.12's first defect class: the bare-name evidence
// level was populated differently by the repo-wide resolver and by the
// incremental binder.
//
// The repo-wide bare-name level is reachable by two strategies -- `exact_name`,
// restricted to resolverBareNameKindsSQL, and `receiver_method`, restricted to
// container-bearing symbols of ANY kind -- so the cross-strategy veto counts
// their union. The binder only ever counted the `exact_name` half, so a name
// declared once as a callable and once as a container-bearing enum looked
// ambiguous to one pipeline and unique to the other, and the same tree resolved
// differently depending on whether it had been indexed or updated.
//
// The rule these tests pin: what a level's candidates ARE is one fact, shared by
// every entry point; what a strategy may BIND out of them is a separate,
// narrower fact. A candidate may therefore veto a level it cannot itself own.

// bareLevelKinds is the kind matrix for the bare-name evidence level: for each
// symbol kind, whether the repo-wide bare-name strategy can bind it, and whether
// it counts toward that level's ambiguity when it carries a container.
//
// It exists so a future edit to resolverBareNameKindsSQL cannot silently change
// what "ambiguous" means without a test naming the kind it changed.
var bareLevelKinds = []struct {
	kind     string
	bindable bool // resolverBareNameKindsSQL: `exact_name` may write it
	// isType marks the kinds P22.9's scope rule also governs. It matters here
	// because a distant type candidate is refused for a second, independent
	// reason, which can mask a defect in the level rule itself -- so the tests
	// that must isolate the level use a non-type kind.
	isType bool
}{
	{"function", true, false},
	{"method", true, false},
	{"class", true, true},
	{"type", true, true},
	{"struct", true, true},
	{"interface", true, true},
	{"enum", false, true},
	{"object", false, true},
	{"protocol", false, true},
	{"actor", false, true},
	{"trait", false, true},
	{"value", false, false},
	{"constant", false, false},
}

// A container-bearing symbol of a kind the bare-name strategy cannot bind is
// still a candidate at that level, because `receiver_method` can bind it. Every
// entry point must therefore refuse the name, not just the repo-wide one.
func TestBareNameLevelCountsContainerBearingKinds(t *testing.T) {
	for _, kc := range bareLevelKinds {
		if kc.bindable {
			continue
		}
		t.Run(kc.kind, func(t *testing.T) {
			for _, entry := range []string{"full", "paths", "names", "paths+names"} {
				t.Run(entry, func(t *testing.T) {
					f := newParityFixture(t, "")
					fnFile := f.file(t, "app/helpers.py", "python")
					f.symbol(t, fnFile, "helper", "helpers.helper", "function", "python")
					otherFile := f.file(t, "app/models.py", "python")
					f.symbolIn(t, otherFile, "helper", "Base.helper", kc.kind, "Base", "python")

					callerFile := f.file(t, "app/main.py", "python")
					caller := f.symbol(t, callerFile, "run", "main.run", "function", "python")
					edge := f.edge(t, callerFile, caller, "helper")

					f.resolveVia(t, entry, []string{"app/main.py"}, []string{"helper"})
					if got := f.binding(t, edge); got != "<unresolved>" {
						t.Fatalf("%s: bare `helper` bound %s while a %s of the same name also declares it",
							entry, got, kc.kind)
					}
				})
			}
		})
	}
}

// The same shape with the competing declaration removed: the level is decided
// again and every entry point binds the callable. This is the positive control
// for the test above -- refusing everything would pass it.
func TestBareNameLevelStillBindsWhenSoleCandidate(t *testing.T) {
	for _, entry := range []string{"full", "paths", "names", "paths+names"} {
		t.Run(entry, func(t *testing.T) {
			f := newParityFixture(t, "")
			fnFile := f.file(t, "app/helpers.py", "python")
			f.symbol(t, fnFile, "helper", "helpers.helper", "function", "python")
			callerFile := f.file(t, "app/main.py", "python")
			caller := f.symbol(t, callerFile, "run", "main.run", "function", "python")
			edge := f.edge(t, callerFile, caller, "helper")

			f.resolveVia(t, entry, []string{"app/main.py"}, []string{"helper"})
			if got, want := f.binding(t, edge), "helpers.helper|exact_name|high"; got != want {
				t.Fatalf("%s: bare `helper` bound %q, want %q", entry, got, want)
			}
		})
	}
}

// A container-bearing non-callable that does NOT share its name with the call
// must not veto anything: ambiguity is name-local (P3).
func TestBareNameLevelIgnoresUnrelatedContainerBearingKinds(t *testing.T) {
	for _, entry := range []string{"full", "paths", "names", "paths+names"} {
		t.Run(entry, func(t *testing.T) {
			f := newParityFixture(t, "")
			fnFile := f.file(t, "app/helpers.py", "python")
			f.symbol(t, fnFile, "helper", "helpers.helper", "function", "python")
			otherFile := f.file(t, "app/models.py", "python")
			f.symbolIn(t, otherFile, "colour", "Base.colour", "enum", "Base", "python")

			callerFile := f.file(t, "app/main.py", "python")
			caller := f.symbol(t, callerFile, "run", "main.run", "function", "python")
			edge := f.edge(t, callerFile, caller, "helper")

			f.resolveVia(t, entry, []string{"app/main.py"}, []string{"helper"})
			if got, want := f.binding(t, edge), "helpers.helper|exact_name|high"; got != want {
				t.Fatalf("%s: unrelated enum changed the answer: %q, want %q", entry, got, want)
			}
		})
	}
}

// Which declaration was inserted first must not decide the outcome. This is the
// P3 invariant restated for the widened population: an insertion-order-derived
// answer would show up here as one order resolving and the other refusing.
func TestBareNameLevelVetoIsInsertionOrderIndependent(t *testing.T) {
	// Both a type kind and a non-type kind. Only the second isolates the
	// ordering question: P22.9's type-scope rule would refuse a distant `enum`
	// destination anyway, so an order-dependent chooser could hide behind it.
	for _, kind := range []string{"enum", "value"} {
		for _, order := range []string{"callable-first", "other-first"} {
			t.Run(kind+"/"+order, func(t *testing.T) {
				for _, entry := range []string{"full", "paths", "names", "paths+names"} {
					t.Run(entry, func(t *testing.T) {
						f := newParityFixture(t, "")
						fnFile := f.file(t, "app/helpers.py", "python")
						otherFile := f.file(t, "app/models.py", "python")
						declareCallable := func() {
							f.symbol(t, fnFile, "helper", "helpers.helper", "function", "python")
						}
						declareOther := func() {
							f.symbolIn(t, otherFile, "helper", "Base.helper", kind, "Base", "python")
						}
						if order == "callable-first" {
							declareCallable()
							declareOther()
						} else {
							declareOther()
							declareCallable()
						}
						callerFile := f.file(t, "app/main.py", "python")
						caller := f.symbol(t, callerFile, "run", "main.run", "function", "python")
						edge := f.edge(t, callerFile, caller, "helper")

						f.resolveVia(t, entry, []string{"app/main.py"}, []string{"helper"})
						if got := f.binding(t, edge); got != "<unresolved>" {
							t.Fatalf("%s/%s/%s: insertion order decided the answer: %s",
								kind, order, entry, got)
						}
					})
				}
			})
		}
	}
}

// The C++ shape the class was found on: `enum align` nested in one class and a
// member function `align` in another. Both declare the identifier `align`, and
// the bare spelling names neither, on any entry point.
func TestBareNameLevelCppEnumCompetesWithMemberFunction(t *testing.T) {
	for _, entry := range []string{"full", "paths", "names", "paths+names"} {
		t.Run(entry, func(t *testing.T) {
			f := newParityFixture(t, "")
			specsFile := f.file(t, "include/fmt/base.h", "cpp")
			f.symbolIn(t, specsFile, "align", "basic_specs::align", "function", "basic_specs", "cpp")
			f.symbolIn(t, specsFile, "align", "base::align", "enum", "base", "cpp")

			callerFile := f.file(t, "src/format.cc", "cpp")
			caller := f.symbolIn(t, callerFile, "write", "writer::write", "function", "writer", "cpp")
			edge := f.edge(t, callerFile, caller, "align")

			f.resolveVia(t, entry, []string{"src/format.cc"}, []string{"align"})
			if got := f.binding(t, edge); got != "<unresolved>" {
				t.Fatalf("%s: bare `align` bound %s; the enum declares the same identifier", entry, got)
			}
		})
	}
}

// A test-only competing declaration must keep its P7 meaning after the
// population widened: a production caller still binds the sole production
// candidate, and a test caller still faces two.
func TestBareNameLevelWidenedPopulationKeepsTestShadowRule(t *testing.T) {
	for _, tc := range []struct {
		name       string
		callerPath string
		want       string
	}{
		{"production caller", "app/main.py", "helpers.helper|exact_name|high"},
		{"test caller", "app/test_main.py", "<unresolved>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, entry := range []string{"full", "paths", "names", "paths+names"} {
				t.Run(entry, func(t *testing.T) {
					f := newParityFixture(t, "")
					fnFile := f.file(t, "app/helpers.py", "python")
					f.symbol(t, fnFile, "helper", "helpers.helper", "function", "python")
					testFile := f.file(t, "app/test_helpers.py", "python")
					f.symbolIn(t, testFile, "helper", "Fake.helper", "enum", "Fake", "python")

					callerFile := f.file(t, tc.callerPath, "python")
					caller := f.symbol(t, callerFile, "run", "main.run", "function", "python")
					edge := f.edge(t, callerFile, caller, "helper")

					f.resolveVia(t, entry, []string{tc.callerPath}, []string{"helper"})
					if got := f.binding(t, edge); got != tc.want {
						t.Fatalf("%s: bound %q, want %q", entry, got, tc.want)
					}
				})
			}
		})
	}
}

// The widened population is per language: a foreign-language declaration of the
// same name is not a candidate and cannot veto (P2/P22.7).
func TestBareNameLevelVetoDoesNotCrossLanguages(t *testing.T) {
	for _, entry := range []string{"full", "paths", "names", "paths+names"} {
		t.Run(entry, func(t *testing.T) {
			f := newParityFixture(t, "")
			fnFile := f.file(t, "app/helpers.py", "python")
			f.symbol(t, fnFile, "helper", "helpers.helper", "function", "python")
			goFile := f.file(t, "svc/models.go", "go")
			f.symbolIn(t, goFile, "helper", "models.helper", "enum", "models", "go")

			callerFile := f.file(t, "app/main.py", "python")
			caller := f.symbol(t, callerFile, "run", "main.run", "function", "python")
			edge := f.edge(t, callerFile, caller, "helper")

			f.resolveVia(t, entry, []string{"app/main.py"}, []string{"helper"})
			if got, want := f.binding(t, edge), "helpers.helper|exact_name|high"; got != want {
				t.Fatalf("%s: a Go enum vetoed a Python call: %q, want %q", entry, got, want)
			}
		})
	}
}

// A non-callable kind with NO container is reachable by no strategy at the
// bare-name level -- `exact_name` refuses the kind and `receiver_method`
// requires a container -- so it is not a candidate and vetoes nothing. This is
// the boundary of the widened population; without it the population could creep
// to "every symbol with this name" and start refusing legitimate bindings.
func TestBareNameLevelIgnoresContainerlessNonCallableKinds(t *testing.T) {
	for _, kc := range bareLevelKinds {
		if kc.bindable {
			continue
		}
		t.Run(kc.kind, func(t *testing.T) {
			for _, entry := range []string{"full", "paths", "names", "paths+names"} {
				t.Run(entry, func(t *testing.T) {
					f := newParityFixture(t, "")
					fnFile := f.file(t, "app/helpers.py", "python")
					f.symbol(t, fnFile, "helper", "helpers.helper", "function", "python")
					otherFile := f.file(t, "app/models.py", "python")
					f.symbol(t, otherFile, "helper", "models.helper", kc.kind, "python")

					callerFile := f.file(t, "app/main.py", "python")
					caller := f.symbol(t, callerFile, "run", "main.run", "function", "python")
					edge := f.edge(t, callerFile, caller, "helper")

					f.resolveVia(t, entry, []string{"app/main.py"}, []string{"helper"})
					if got, want := f.binding(t, edge), "helpers.helper|exact_name|high"; got != want {
						t.Fatalf("%s: a containerless %s changed the answer: %q, want %q",
							entry, kc.kind, got, want)
					}
				})
			}
		})
	}
}

// The Go binder's kind predicate, the SQL strategy's kind list, the P22.9 type
// set and the matrix above are four spellings of two rules. This pins them
// against each other so a future edit to any of them has to touch the matrix --
// including an ADDED kind, which the per-row loop alone would not notice.
func TestBareNameBindableKindsMatchSQLList(t *testing.T) {
	wantBindable := 0
	for _, kc := range bareLevelKinds {
		if kc.bindable {
			wantBindable++
		}
		if got := resolverBareNameKindBindable(kc.kind); got != kc.bindable {
			t.Errorf("resolverBareNameKindBindable(%q) = %v, want %v", kc.kind, got, kc.bindable)
		}
		if got := strings.Contains(resolverBareNameKindsSQL, "'"+kc.kind+"'"); got != kc.bindable {
			t.Errorf("resolverBareNameKindsSQL lists %q = %v, want %v", kc.kind, got, kc.bindable)
		}
		if got := isTypeSymbolKind(kc.kind); got != kc.isType {
			t.Errorf("isTypeSymbolKind(%q) = %v, want %v", kc.kind, got, kc.isType)
		}
	}
	if len(resolverBareNameKinds) != wantBindable {
		t.Fatalf("resolverBareNameKinds has %d kinds but the matrix names %d bindable ones (%v); "+
			"a kind was added or removed without deciding what it means for the level",
			len(resolverBareNameKinds), wantBindable, resolverBareNameKinds)
	}
	if !strings.Contains(resolverBareNameLevelKindsSQL("s."), "s.container_name != ''") {
		t.Fatalf("the bare-name level must still admit container-bearing kinds")
	}
}

// A KNOWN, DELIBERATE divergence, pinned so it is not mistaken for parity.
//
// P22.12 made the two pipelines agree on what the bare-name level's candidates
// ARE. It did not give the incremental binder the `receiver_method` strategy,
// which is the one that can BIND a container-bearing non-callable. So a name
// whose only declaration is such a symbol resolves on a full index and stays
// unresolved on an incremental one -- the same shape as the repo-wide-only
// strategy class P22.8 recorded and deferred, and unchanged by this phase
// (master answers identically, because its binder never loaded the candidate
// at all).
//
// A non-type kind is used on purpose: for `enum` and friends P22.9's scope rule
// refuses the binding for its own reason and hides the gap.
func TestBareNameLevelSoleContainerBearingNonCallableIsRepoWideOnly(t *testing.T) {
	build := func(t *testing.T) (*parityFixture, int64) {
		f := newParityFixture(t, "")
		// A converged repository, which is what an incremental entrypoint always
		// runs against in production: a first index marks the one-time repairs
		// (RepoHasExistingGraph / MarkResolverBindingsRepaired). Without this the
		// paths+names entrypoint would pay the P22.13 type-scope repair, which
		// re-resolves repo-wide and would answer with the FULL pipeline's binding
		// rather than the binder's -- measuring the repair, not the divergence
		// this test exists to pin.
		if err := f.store.MarkResolverBindingsRepaired(f.ctx, f.repoID); err != nil {
			t.Fatalf("MarkResolverBindingsRepaired: %v", err)
		}
		defs := f.file(t, "app/models.py", "python")
		f.symbolIn(t, defs, "helper", "Base.helper", "value", "Base", "python")
		callerFile := f.file(t, "app/main.py", "python")
		caller := f.symbol(t, callerFile, "run", "main.run", "function", "python")
		return f, f.edge(t, callerFile, caller, "helper")
	}

	f, edge := build(t)
	f.resolveVia(t, "full", []string{"app/main.py"}, []string{"helper"})
	if got, want := f.binding(t, edge), "Base.helper|receiver_method|medium"; got != want {
		t.Fatalf("full index: got %q, want %q -- the fixture no longer reaches receiver_method", got, want)
	}

	for _, entry := range []string{"paths", "names", "paths+names"} {
		t.Run(entry, func(t *testing.T) {
			f, edge := build(t)
			f.resolveVia(t, entry, []string{"app/main.py"}, []string{"helper"})
			if got := f.binding(t, edge); got != "<unresolved>" {
				t.Fatalf("%s: got %q. If the binder gained the receiver_method level, this "+
					"deferred divergence is closed -- delete this test and say so.", entry, got)
			}
		})
	}
}

// Ambiguity is symbol-level, not file-level: two declarations of a name inside
// ONE file are two candidates. Nothing in the level's population is keyed by
// file, and P22.12's old-name collection returns DISTINCT names, so a file that
// goes from declaring `Foo` twice to declaring it once has to be re-decided the
// same way two files becoming one is.
func TestBareNameLevelCountsDuplicateDeclarationsInOneFile(t *testing.T) {
	for _, entry := range []string{"full", "paths", "names", "paths+names"} {
		t.Run(entry, func(t *testing.T) {
			f := newParityFixture(t, "")
			defs := f.file(t, "app/helpers.py", "python")
			f.symbol(t, defs, "helper", "helpers.helper", "function", "python")
			f.symbol(t, defs, "helper", "helpers.helper", "function", "python")

			callerFile := f.file(t, "app/main.py", "python")
			caller := f.symbol(t, callerFile, "run", "main.run", "function", "python")
			edge := f.edge(t, callerFile, caller, "helper")

			f.resolveVia(t, entry, []string{"app/main.py"}, []string{"helper"})
			if got := f.binding(t, edge); got != "<unresolved>" {
				t.Fatalf("%s: two declarations in one file bound %s", entry, got)
			}
		})
	}
}
