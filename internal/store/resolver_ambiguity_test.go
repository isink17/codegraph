package store

import (
	"testing"
)

// Regression coverage for P3 resolver ambiguity determinism.
//
// When several same-language candidates survive a strategy and nothing at that
// strategy's evidence level tells them apart, the edge must stay unresolved --
// on every entrypoint, whatever order the symbols were inserted in. When
// evidence does single one out (a qualified name, a unique suffix, a single
// same-language candidate among foreign ones), the edge must still resolve.
//
// The scenarios reuse the P2 gate fixture so language gating and ambiguity
// refusal are exercised on the same graphs.

// ambiguityScenario builds a graph and states what every resolver entrypoint is
// expected to do with the edge under test.
type ambiguityScenario struct {
	name string
	// build returns the edge under test, the id it must resolve to (0 when it
	// must stay unresolved), the source file path a path-scoped run would be
	// given and the name a name-targeted run would be given.
	build func(t *testing.T, f *gateFixture) (edgeID, wantDstID int64, srcPath, targetName string)
}

func ambiguityScenarios() []ambiguityScenario {
	return []ambiguityScenario{
		{
			// A: two same-language definitions of one bare name. Nothing in the
			// call distinguishes them, so no edge may be created.
			name: "bare_name_ambiguity_unresolved",
			build: func(t *testing.T, f *gateFixture) (int64, int64, string, string) {
				first := f.file(t, "src/py/config.py", "python")
				f.symbol(t, first, "load_config", "config.load_config", "python")
				second := f.file(t, "tests/py/test_config.py", "python")
				f.symbol(t, second, "load_config", "test_config.load_config", "python")

				caller := f.file(t, "src/py/caller.py", "python")
				src := f.symbol(t, caller, "caller", "caller", "python")
				return f.edge(t, caller, src, "load_config"), 0, "src/py/caller.py", "load_config"
			},
		},
		{
			// A, reversed: the same candidate set with the insertion order (and
			// therefore the symbol ids) swapped. A MIN(id)/first-wins resolver
			// would flip its answer here; refusing to choose cannot.
			name: "bare_name_ambiguity_unresolved_reversed_insertion_order",
			build: func(t *testing.T, f *gateFixture) (int64, int64, string, string) {
				second := f.file(t, "tests/py/test_config.py", "python")
				f.symbol(t, second, "load_config", "test_config.load_config", "python")
				first := f.file(t, "src/py/config.py", "python")
				f.symbol(t, first, "load_config", "config.load_config", "python")

				caller := f.file(t, "src/py/caller.py", "python")
				src := f.symbol(t, caller, "caller", "caller", "python")
				return f.edge(t, caller, src, "load_config"), 0, "src/py/caller.py", "load_config"
			},
		},
		{
			// B: the same ambiguity again, with unrelated symbols inserted first
			// so both candidates carry different ids than in the cases above.
			// Symbol id is not evidence, so the verdict must not move.
			name: "bare_name_ambiguity_unresolved_shifted_symbol_ids",
			build: func(t *testing.T, f *gateFixture) (int64, int64, string, string) {
				decoys := f.file(t, "src/py/decoys.py", "python")
				for _, name := range []string{"decoy_one", "decoy_two", "decoy_three"} {
					f.symbol(t, decoys, name, "decoys."+name, "python")
				}
				first := f.file(t, "src/py/config.py", "python")
				f.symbol(t, first, "load_config", "config.load_config", "python")
				second := f.file(t, "tests/py/test_config.py", "python")
				f.symbol(t, second, "load_config", "test_config.load_config", "python")

				caller := f.file(t, "src/py/caller.py", "python")
				src := f.symbol(t, caller, "caller", "caller", "python")
				return f.edge(t, caller, src, "load_config"), 0, "src/py/caller.py", "load_config"
			},
		},
		{
			// C: the bare name collides, but the call carries qualification that
			// only one candidate matches. Ambiguity is strategy-local: a name
			// being ambiguous as a bare name must not veto a qualified match.
			name: "qualified_evidence_resolves_unique_candidate",
			build: func(t *testing.T, f *gateFixture) (int64, int64, string, string) {
				pkgA := f.file(t, "src/py/pkg_a.py", "python")
				want := f.symbol(t, pkgA, "foo", "pkg_a.foo", "python")
				pkgB := f.file(t, "src/py/pkg_b.py", "python")
				f.symbol(t, pkgB, "foo", "pkg_b.foo", "python")

				caller := f.file(t, "src/py/caller.py", "python")
				src := f.symbol(t, caller, "caller", "caller", "python")
				return f.edge(t, caller, src, "pkg_a.foo"), want, "src/py/caller.py", "pkg_a.foo"
			},
		},
		{
			// D: no exact match exists, but exactly one symbol carries the
			// requested suffix, so suffix evidence identifies it.
			name: "suffix_evidence_resolves_unique_candidate",
			build: func(t *testing.T, f *gateFixture) (int64, int64, string, string) {
				defs := f.file(t, "src/go/pkg/util.go", "go")
				want := f.symbol(t, defs, "Format", "github.com/org/repo/pkg.Format", "go")
				other := f.file(t, "src/go/other/util.go", "go")
				f.symbol(t, other, "Render", "github.com/org/repo/other.Render", "go")

				caller := f.file(t, "src/go/caller.go", "go")
				src := f.symbol(t, caller, "Caller", "Caller", "go")
				return f.edge(t, caller, src, "pkg.Format"), want, "src/go/caller.go", "pkg.Format"
			},
		},
		{
			// E: two symbols share the requested suffix. The suffix is the whole
			// evidence this strategy has and it does not separate them.
			name: "suffix_ambiguity_unresolved",
			build: func(t *testing.T, f *gateFixture) (int64, int64, string, string) {
				first := f.file(t, "src/go/a/util.go", "go")
				f.symbol(t, first, "Format", "github.com/org/a/pkg.Format", "go")
				second := f.file(t, "src/go/b/util.go", "go")
				f.symbol(t, second, "Format", "github.com/org/b/pkg.Format", "go")

				caller := f.file(t, "src/go/caller.go", "go")
				src := f.symbol(t, caller, "Caller", "Caller", "go")
				return f.edge(t, caller, src, "pkg.Format"), 0, "src/go/caller.go", "pkg.Format"
			},
		},
		{
			// Cross-strategy fall-through: the bare name has three candidates,
			// so the bare-name strategy refuses. Exactly one of them carries a
			// receiver, so the receiver strategy would find a "unique" candidate
			// -- but nothing in the call said the target was a method, so that
			// would be a winner picked by a filter, not by evidence.
			name: "receiver_strategy_does_not_rescue_bare_name_ambiguity",
			build: func(t *testing.T, f *gateFixture) (int64, int64, string, string) {
				modA := f.file(t, "src/py/mod_a.py", "python")
				f.symbol(t, modA, "process", "mod_a.process", "python")
				modB := f.file(t, "src/py/mod_b.py", "python")
				f.symbol(t, modB, "process", "mod_b.process", "python")
				methods := f.file(t, "src/py/cls.py", "python")
				f.method(t, methods, "process", "Cls.process", "Cls", "python")

				caller := f.file(t, "src/py/caller.py", "python")
				src := f.symbol(t, caller, "caller", "caller", "python")
				return f.edge(t, caller, src, "process"), 0, "src/py/caller.py", "process"
			},
		},
		{
			// Cross-strategy fall-through, suffix variant: two same-language
			// definitions share the bare name, and only one of them has a
			// slash-qualified name. The suffix strategy must not bind it: the
			// suffix describes the destination's own shape and says nothing
			// about which definition the caller meant.
			name: "suffix_strategy_does_not_rescue_bare_name_ambiguity",
			build: func(t *testing.T, f *gateFixture) (int64, int64, string, string) {
				slashy := f.file(t, "src/go/pkg/format.go", "go")
				f.symbol(t, slashy, "Format", "github.com/org/repo/Format", "go")
				plain := f.file(t, "src/go/other/format.go", "go")
				f.symbol(t, plain, "Format", "other.Format", "go")

				caller := f.file(t, "src/go/caller.go", "go")
				src := f.symbol(t, caller, "Caller", "Caller", "go")
				return f.edge(t, caller, src, "Format"), 0, "src/go/caller.go", "Format"
			},
		},
		{
			// Cross-strategy fall-through, qualified variant: the qualified name
			// is claimed by two definitions, so the tail of that same name is
			// weaker evidence about the same undecided set, not new evidence.
			name: "short_name_does_not_rescue_qualified_ambiguity",
			build: func(t *testing.T, f *gateFixture) (int64, int64, string, string) {
				first := f.file(t, "src/py/impl_one.py", "python")
				f.symbol(t, first, "handle", "shared.handle", "python")
				second := f.file(t, "src/py/impl_two.py", "python")
				f.symbol(t, second, "handle", "shared.handle", "python")

				caller := f.file(t, "src/py/caller.py", "python")
				src := f.symbol(t, caller, "caller", "caller", "python")
				return f.edge(t, caller, src, "shared.handle"), 0, "src/py/caller.py", "shared.handle"
			},
		},
		{
			// Same fall-through, but the bare tail *is* unique: only one of the
			// two definitions claiming the qualified name is reachable by tail
			// lookup. Binding it would answer a question the qualified name
			// already showed to be undecidable, using weaker evidence.
			name: "unique_short_tail_does_not_rescue_qualified_ambiguity",
			build: func(t *testing.T, f *gateFixture) (int64, int64, string, string) {
				first := f.file(t, "src/py/impl_one.py", "python")
				f.symbol(t, first, "handle", "shared.handle", "python")
				second := f.file(t, "src/py/impl_two.py", "python")
				f.symbol(t, second, "shared.handle", "shared.handle", "python")

				caller := f.file(t, "src/py/caller.py", "python")
				src := f.symbol(t, caller, "caller", "caller", "python")
				return f.edge(t, caller, src, "shared.handle"), 0, "src/py/caller.py", "shared.handle"
			},
		},
		{
			// A symbol kind the bare-name strategy ignores must still be
			// reachable through suffix evidence, and identically on every
			// entrypoint: P3 refuses ties, it does not narrow candidate sets.
			name: "non_callable_kind_still_resolves_on_unique_suffix",
			build: func(t *testing.T, f *gateFixture) (int64, int64, string, string) {
				defs := f.file(t, "src/go/pkg/handlers.go", "go")
				want := f.symbolKind(t, defs, "Handler", "github.com/org/repo/pkg.Handler", "value", "go")

				caller := f.file(t, "src/go/caller.go", "go")
				src := f.symbol(t, caller, "Caller", "Caller", "go")
				return f.edge(t, caller, src, "pkg.Handler"), want, "src/go/caller.go", "pkg.Handler"
			},
		},
		{
			// Exact qualified evidence outranks the bare-name veto: several
			// definitions share the bare name, but one of them *is* the fully
			// qualified name the call names, and that is the strongest evidence
			// the resolver has.
			name: "exact_qualified_name_beats_bare_name_ambiguity",
			build: func(t *testing.T, f *gateFixture) (int64, int64, string, string) {
				exact := f.file(t, "src/py/top.py", "python")
				want := f.symbol(t, exact, "widget", "widget", "python")
				other := f.file(t, "src/py/nested.py", "python")
				f.symbol(t, other, "widget", "nested.widget", "python")

				caller := f.file(t, "src/py/caller.py", "python")
				src := f.symbol(t, caller, "caller", "caller", "python")
				return f.edge(t, caller, src, "widget"), want, "src/py/caller.py", "widget"
			},
		},
		{
			// Same multi-dot strategies, two definitions sharing the requested
			// three-segment tail: nothing separates them, so nothing binds.
			name: "multi_dot_suffix_ambiguity_unresolved",
			build: func(t *testing.T, f *gateFixture) (int64, int64, string, string) {
				first := f.file(t, "src/py/one/mod.py", "python")
				f.symbol(t, first, "render", "app.views.page.render", "python")
				second := f.file(t, "src/py/two/mod.py", "python")
				f.symbol(t, second, "render", "web.views.page.render", "python")

				caller := f.file(t, "src/py/caller.py", "python")
				src := f.symbol(t, caller, "caller", "caller", "python")
				return f.edge(t, caller, src, "views.page.render"), 0, "src/py/caller.py", "views.page.render"
			},
		},
		{
			// F: P2 behaviour preserved. Foreign definitions are not candidates,
			// so they can neither win nor manufacture ambiguity: the single
			// same-language definition still resolves.
			name: "foreign_collisions_do_not_create_ambiguity",
			build: func(t *testing.T, f *gateFixture) (int64, int64, string, string) {
				javaFile := f.file(t, "src/java/Serializer.java", "java")
				f.symbol(t, javaFile, "serialize", "Serializer.serialize", "java")
				kotlinFile := f.file(t, "src/kotlin/serializer.kt", "kotlin")
				f.symbol(t, kotlinFile, "serialize", "serializer.serialize", "kotlin")
				rustFile := f.file(t, "src/rust/serializer.rs", "rust")
				f.symbol(t, rustFile, "serialize", "serializer.serialize", "rust")

				pyDefs := f.file(t, "src/py/serializer.py", "python")
				want := f.symbol(t, pyDefs, "serialize", "serialize", "python")

				caller := f.file(t, "src/py/caller.py", "python")
				src := f.symbol(t, caller, "caller", "caller", "python")
				return f.edge(t, caller, src, "serialize"), want, "src/py/caller.py", "serialize"
			},
		},
		{
			// F, harder: several foreign definitions AND several same-language
			// ones. The foreign rows must not change the verdict either way --
			// the same-language set is ambiguous on its own.
			name: "foreign_collisions_do_not_rescue_same_language_ambiguity",
			build: func(t *testing.T, f *gateFixture) (int64, int64, string, string) {
				javaFile := f.file(t, "src/java/Serializer.java", "java")
				f.symbol(t, javaFile, "serialize", "Serializer.serialize", "java")

				pyOne := f.file(t, "src/py/serializer_one.py", "python")
				f.symbol(t, pyOne, "serialize", "serializer_one.serialize", "python")
				pyTwo := f.file(t, "src/py/serializer_two.py", "python")
				f.symbol(t, pyTwo, "serialize", "serializer_two.serialize", "python")

				caller := f.file(t, "src/py/caller.py", "python")
				src := f.symbol(t, caller, "caller", "caller", "python")
				return f.edge(t, caller, src, "serialize"), 0, "src/py/caller.py", "serialize"
			},
		},
	}
}

// TestResolverAmbiguity_AllEntrypoints is the G/H parity test: the repo-wide,
// path-scoped and name-targeted resolvers must reach the same verdict on every
// scenario, so the same graph cannot turn ambiguous into an arbitrary target
// just because a different entrypoint ran.
func TestResolverAmbiguity_AllEntrypoints(t *testing.T) {
	for _, scenario := range ambiguityScenarios() {
		for _, resolver := range ambiguityResolvers() {
			t.Run(scenario.name+"/"+resolver.name, func(t *testing.T) {
				f := newGateFixture(t)
				edgeID, wantDstID, srcPath, targetName := scenario.build(t, f)
				resolver.run(t, f, srcPath, targetName)
				gotDstID, resolved := f.dstSymbolID(t, edgeID)
				switch {
				case wantDstID == 0 && resolved:
					t.Fatalf("edge resolved to symbol %d; want unresolved", gotDstID)
				case wantDstID != 0 && !resolved:
					t.Fatalf("edge unresolved; want symbol %d", wantDstID)
				case wantDstID != 0 && gotDstID != wantDstID:
					t.Fatalf("edge resolved to symbol %d; want %d", gotDstID, wantDstID)
				}
			})
		}
	}
}

type ambiguityResolver struct {
	name string
	run  func(t *testing.T, f *gateFixture, srcPath, targetName string)
}

// ambiguityResolvers covers every production entrypoint, including the
// path-then-name sequence the indexer runs for an incremental update.
func ambiguityResolvers() []ambiguityResolver {
	return []ambiguityResolver{
		{
			name: "repo",
			run: func(t *testing.T, f *gateFixture, _, _ string) {
				if _, err := f.store.ResolveEdges(f.ctx, f.repoID); err != nil {
					t.Fatalf("ResolveEdges() error = %v", err)
				}
			},
		},
		{
			name: "paths",
			run: func(t *testing.T, f *gateFixture, srcPath, _ string) {
				if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{srcPath}); err != nil {
					t.Fatalf("ResolveEdgesForPaths() error = %v", err)
				}
			},
		},
		{
			name: "names",
			run: func(t *testing.T, f *gateFixture, _, targetName string) {
				if _, err := f.store.ResolveEdgesForNamesWithStats(f.ctx, f.repoID, []string{targetName}); err != nil {
					t.Fatalf("ResolveEdgesForNamesWithStats() error = %v", err)
				}
			},
		},
		{
			// The incremental sequence the indexer actually runs: path-scoped
			// resolution for the changed files, then a name-targeted pass for
			// the changed symbol names.
			name: "incremental_paths_then_names",
			run: func(t *testing.T, f *gateFixture, srcPath, targetName string) {
				if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{srcPath}); err != nil {
					t.Fatalf("ResolveEdgesForPaths() error = %v", err)
				}
				if _, err := f.store.ResolveEdgesForNamesWithStats(f.ctx, f.repoID, []string{targetName}); err != nil {
					t.Fatalf("ResolveEdgesForNamesWithStats() error = %v", err)
				}
			},
		},
	}
}

// TestResolverAmbiguity_MultiDotSuffixUniquenessIsEnforced covers the two
// multi-dot passes (dot_tail3 and the LIKE suffix fallback) that the shared
// scenarios never reach, and pins the one entrypoint difference P3 leaves in
// place: the repo-wide resolver has suffix strategies the incremental binder
// does not, so it can bind a three-segment tail the binder leaves unresolved.
// The difference must stay in that direction -- a missing edge, never a
// different destination.
func TestResolverAmbiguity_MultiDotSuffixUniquenessIsEnforced(t *testing.T) {
	unique := func(t *testing.T, f *gateFixture) (int64, int64) {
		t.Helper()
		defs := f.file(t, "src/py/deep/mod.py", "python")
		want := f.symbol(t, defs, "render", "app.views.page.render", "python")
		decoy := f.file(t, "src/py/other/mod.py", "python")
		f.symbol(t, decoy, "render", "app.views.other.render", "python")
		caller := f.file(t, "src/py/caller.py", "python")
		src := f.symbol(t, caller, "caller", "caller", "python")
		return f.edge(t, caller, src, "views.page.render"), want
	}
	ambiguous := func(t *testing.T, f *gateFixture) int64 {
		t.Helper()
		first := f.file(t, "src/py/one/mod.py", "python")
		f.symbol(t, first, "render", "app.views.page.render", "python")
		second := f.file(t, "src/py/two/mod.py", "python")
		f.symbol(t, second, "render", "web.views.page.render", "python")
		caller := f.file(t, "src/py/caller.py", "python")
		src := f.symbol(t, caller, "caller", "caller", "python")
		return f.edge(t, caller, src, "views.page.render")
	}

	t.Run("repo/unique_tail_resolves", func(t *testing.T) {
		f := newGateFixture(t)
		edgeID, want := unique(t, f)
		if _, err := f.store.ResolveEdges(f.ctx, f.repoID); err != nil {
			t.Fatalf("ResolveEdges() error = %v", err)
		}
		if got, resolved := f.dstSymbolID(t, edgeID); !resolved || got != want {
			t.Fatalf("dst_symbol_id = (%v,%d), want (%v,%d)", resolved, got, true, want)
		}
	})

	t.Run("repo/shared_tail_unresolved", func(t *testing.T) {
		f := newGateFixture(t)
		edgeID := ambiguous(t, f)
		if _, err := f.store.ResolveEdges(f.ctx, f.repoID); err != nil {
			t.Fatalf("ResolveEdges() error = %v", err)
		}
		if got, resolved := f.dstSymbolID(t, edgeID); resolved {
			t.Fatalf("edge resolved to %s; two definitions share the tail, so nothing may bind", f.qualifiedNameOf(t, got))
		}
	})

	t.Run("incremental/unique_tail_stays_unresolved_never_mistargets", func(t *testing.T) {
		f := newGateFixture(t)
		edgeID, want := unique(t, f)
		if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"src/py/caller.py"}); err != nil {
			t.Fatalf("ResolveEdgesForPaths() error = %v", err)
		}
		if _, err := f.store.ResolveEdgesForNamesWithStats(f.ctx, f.repoID, []string{"render"}); err != nil {
			t.Fatalf("ResolveEdgesForNamesWithStats() error = %v", err)
		}
		got, resolved := f.dstSymbolID(t, edgeID)
		if resolved && got != want {
			t.Fatalf("incremental resolver bound %s; the only acceptable outcomes are unresolved or %d",
				f.qualifiedNameOf(t, got), want)
		}
	})
}

// TestResolverAmbiguity_RepeatedRunsAgree runs each entrypoint several times on
// a freshly built graph and asserts the verdict never moves. Insertion order is
// fixed here; what is being checked is that nothing inside a single resolve
// (chunking, map iteration, unordered SQL) leaks into the decision.
func TestResolverAmbiguity_RepeatedRunsAgree(t *testing.T) {
	for _, scenario := range ambiguityScenarios() {
		for _, resolver := range ambiguityResolvers() {
			t.Run(scenario.name+"/"+resolver.name, func(t *testing.T) {
				const repeats = 8
				var first string
				for i := range repeats {
					f := newGateFixture(t)
					edgeID, _, srcPath, targetName := scenario.build(t, f)
					resolver.run(t, f, srcPath, targetName)
					got := "<unresolved>"
					if dstID, resolved := f.dstSymbolID(t, edgeID); resolved {
						got = f.qualifiedNameOf(t, dstID)
					}
					if i == 0 {
						first = got
						continue
					}
					if got != first {
						t.Fatalf("repeat %d selected %s; repeat 1 selected %s", i+1, got, first)
					}
				}
			})
		}
	}
}

// TestResolverAmbiguity_FullAndNameTargetedAgreeOnAmbiguousGraph is the focused
// G case: one ambiguous graph, built identically, run through the full resolver
// and through the name-targeted resolver. Both must leave the edge unresolved.
func TestResolverAmbiguity_FullAndNameTargetedAgreeOnAmbiguousGraph(t *testing.T) {
	build := func(t *testing.T, f *gateFixture) int64 {
		t.Helper()
		production := f.file(t, "src/py/config_e.py", "python")
		f.symbol(t, production, "load_config_e", "config_e.load_config_e", "python")
		shadow := f.file(t, "tests/py/test_config_e.py", "python")
		f.symbol(t, shadow, "load_config_e", "test_config_e.load_config_e", "python")
		caller := f.file(t, "src/py/caller_e.py", "python")
		src := f.symbol(t, caller, "caller_e", "caller_e", "python")
		return f.edge(t, caller, src, "load_config_e")
	}

	full := newGateFixture(t)
	fullEdge := build(t, full)
	if _, err := full.store.ResolveEdges(full.ctx, full.repoID); err != nil {
		t.Fatalf("ResolveEdges() error = %v", err)
	}
	if dst, resolved := full.dstSymbolID(t, fullEdge); resolved {
		t.Fatalf("full resolver bound ambiguous destination %s; want unresolved", full.qualifiedNameOf(t, dst))
	}

	incremental := newGateFixture(t)
	incrementalEdge := build(t, incremental)
	if err := incremental.store.ResolveEdgesForPaths(incremental.ctx, incremental.repoID, []string{"src/py/caller_e.py"}); err != nil {
		t.Fatalf("ResolveEdgesForPaths() error = %v", err)
	}
	stats, err := incremental.store.ResolveEdgesForNamesWithStats(incremental.ctx, incremental.repoID, []string{"load_config_e"})
	if err != nil {
		t.Fatalf("ResolveEdgesForNamesWithStats() error = %v", err)
	}
	if dst, resolved := incremental.dstSymbolID(t, incrementalEdge); resolved {
		t.Fatalf("incremental resolver bound ambiguous destination %s; want unresolved",
			incremental.qualifiedNameOf(t, dst))
	}

	// I: an ambiguous target is reported as selected and unresolved, never
	// dropped from the counters. It is not language-blocked: the competing
	// definitions are in the calling language.
	if stats.TargetsSelected != 1 {
		t.Errorf("TargetsSelected = %d, want 1", stats.TargetsSelected)
	}
	if stats.TargetsResolved != 0 {
		t.Errorf("TargetsResolved = %d, want 0", stats.TargetsResolved)
	}
	if stats.TargetsUnresolved != 1 {
		t.Errorf("TargetsUnresolved = %d, want 1", stats.TargetsUnresolved)
	}
	if stats.LanguageBlocked != 0 {
		t.Errorf("LanguageBlocked = %d, want 0; the competing definitions are same-language", stats.LanguageBlocked)
	}
	if got := stats.TargetsResolved + stats.TargetsUnresolved + stats.UnknownSrcLanguage; got != stats.TargetsSelected {
		t.Errorf("resolved(%d) + unresolved(%d) + unknown_src(%d) = %d, want selected(%d)",
			stats.TargetsResolved, stats.TargetsUnresolved, stats.UnknownSrcLanguage, got, stats.TargetsSelected)
	}
}

// TestResolverAmbiguity_StatsSeparateAmbiguityFromLanguageBlocking pins the
// distinction the counters must keep: an edge that failed because its own
// language holds several definitions is not a language-gate rejection, even
// though a foreign definition of the same name also exists.
func TestResolverAmbiguity_StatsSeparateAmbiguityFromLanguageBlocking(t *testing.T) {
	f := newGateFixture(t)

	javaFile := f.file(t, "src/java/Serializer.java", "java")
	f.symbol(t, javaFile, "serialize", "Serializer.serialize", "java")
	pyOne := f.file(t, "src/py/serializer_one.py", "python")
	f.symbol(t, pyOne, "serialize", "serializer_one.serialize", "python")
	pyTwo := f.file(t, "src/py/serializer_two.py", "python")
	f.symbol(t, pyTwo, "serialize", "serializer_two.serialize", "python")

	caller := f.file(t, "src/py/caller.py", "python")
	src := f.symbol(t, caller, "caller", "caller", "python")
	edgeID := f.edge(t, caller, src, "serialize")

	stats, err := f.store.ResolveEdgesForNamesWithStats(f.ctx, f.repoID, []string{"serialize"})
	if err != nil {
		t.Fatalf("ResolveEdgesForNamesWithStats() error = %v", err)
	}
	if dst, resolved := f.dstSymbolID(t, edgeID); resolved {
		t.Fatalf("edge resolved to %s; want unresolved", f.qualifiedNameOf(t, dst))
	}
	if stats.AmbiguityBlocked != 1 {
		t.Errorf("AmbiguityBlocked = %d, want 1", stats.AmbiguityBlocked)
	}
	if stats.LanguageBlocked != 0 {
		t.Errorf("LanguageBlocked = %d, want 0; the edge failed on same-language ambiguity, not on the language gate", stats.LanguageBlocked)
	}
	if got := stats.AmbiguityBlocked + stats.LanguageBlocked; got > stats.TargetsUnresolved {
		t.Errorf("ambiguity_blocked(%d) + language_blocked(%d) exceeds unresolved(%d)",
			stats.AmbiguityBlocked, stats.LanguageBlocked, stats.TargetsUnresolved)
	}
}

// TestResolverAmbiguity_StatsAccountForMixedBatch checks the counters stay
// consistent when one batch contains a resolvable target and an ambiguous one:
// the ambiguous edge must show up as unresolved rather than vanish.
func TestResolverAmbiguity_StatsAccountForMixedBatch(t *testing.T) {
	f := newGateFixture(t)

	uniqueDef := f.file(t, "src/py/helpers.py", "python")
	wantDst := f.symbol(t, uniqueDef, "normalize", "helpers.normalize", "python")
	firstAmbiguous := f.file(t, "src/py/config.py", "python")
	f.symbol(t, firstAmbiguous, "load_config", "config.load_config", "python")
	secondAmbiguous := f.file(t, "tests/py/test_config.py", "python")
	f.symbol(t, secondAmbiguous, "load_config", "test_config.load_config", "python")

	caller := f.file(t, "src/py/caller.py", "python")
	src := f.symbol(t, caller, "caller", "caller", "python")
	resolvableEdge := f.edge(t, caller, src, "normalize")
	ambiguousEdge := f.edge(t, caller, src, "load_config")

	stats, err := f.store.ResolveEdgesForNamesWithStats(f.ctx, f.repoID, []string{"normalize", "load_config"})
	if err != nil {
		t.Fatalf("ResolveEdgesForNamesWithStats() error = %v", err)
	}

	if dst, resolved := f.dstSymbolID(t, resolvableEdge); !resolved || dst != wantDst {
		t.Errorf("unambiguous edge resolved to (%v,%d); want (%v,%d)", resolved, dst, true, wantDst)
	}
	if dst, resolved := f.dstSymbolID(t, ambiguousEdge); resolved {
		t.Errorf("ambiguous edge resolved to %s; want unresolved", f.qualifiedNameOf(t, dst))
	}
	if stats.TargetsSelected != 2 {
		t.Errorf("TargetsSelected = %d, want 2", stats.TargetsSelected)
	}
	if stats.TargetsResolved != 1 {
		t.Errorf("TargetsResolved = %d, want 1", stats.TargetsResolved)
	}
	if stats.TargetsUnresolved != 1 {
		t.Errorf("TargetsUnresolved = %d, want 1", stats.TargetsUnresolved)
	}
	if got := stats.TargetsResolved + stats.TargetsUnresolved + stats.UnknownSrcLanguage; got != stats.TargetsSelected {
		t.Errorf("resolved(%d) + unresolved(%d) + unknown_src(%d) = %d, want selected(%d)",
			stats.TargetsResolved, stats.TargetsUnresolved, stats.UnknownSrcLanguage, got, stats.TargetsSelected)
	}
}
