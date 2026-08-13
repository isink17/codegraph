package store

import "testing"

// Regression coverage for P7 test-shadow aware resolver ranking.
//
// The rule under test (resolver_testfile.go): a production call site may bind
// only a production candidate, so one production definition shadowed by any
// number of test definitions resolves; a test call site keeps plain P2/P3
// semantics. Every scenario runs through all four production entrypoints
// (repo-wide, path-scoped, name-targeted, and the path-then-name sequence an
// incremental update performs), so full and incremental resolution cannot drift
// into different definitions of the rule.

func testShadowScenarios() []ambiguityScenario {
	return []ambiguityScenario{
		{
			// The core case. One production definition, one test definition,
			// same language, same bare name, production caller. The test
			// definition is not a competitor for this caller, so the production
			// definition is uniquely valid and binds.
			name: "python_production_caller_binds_production_over_one_test",
			build: func(t *testing.T, f *gateFixture) (int64, int64, string, string) {
				production := f.file(t, "src/py/config_e.py", "python")
				want := f.symbol(t, production, "load_config_e", "config_e.load_config_e", "python")
				shadow := f.file(t, "tests/py/test_config_e.py", "python")
				f.symbol(t, shadow, "load_config_e", "test_config_e.load_config_e", "python")

				caller := f.file(t, "src/py/caller_e.py", "python")
				src := f.symbol(t, caller, "caller_e", "caller_e", "python")
				return f.edge(t, caller, src, "load_config_e"), want, "src/py/caller_e.py", "load_config_e"
			},
		},
		{
			// Same rule in Go, whose convention is a filename suffix rather than
			// a directory: the shadow lives beside the production definition.
			// Guards against the feature being secretly Python/directory-only.
			name: "go_production_caller_binds_production_over_sibling_test_file",
			build: func(t *testing.T, f *gateFixture) (int64, int64, string, string) {
				production := f.file(t, "internal/svc/loader.go", "go")
				want := f.symbol(t, production, "LoadSpec", "internal/svc.LoadSpec", "go")
				shadow := f.file(t, "internal/svc/loader_test.go", "go")
				f.symbol(t, shadow, "LoadSpec", "internal/svc_test.LoadSpec", "go")

				caller := f.file(t, "internal/svc/run.go", "go")
				src := f.symbol(t, caller, "Run", "internal/svc.Run", "go")
				return f.edge(t, caller, src, "LoadSpec"), want, "internal/svc/run.go", "LoadSpec"
			},
		},
		{
			// A third convention: TypeScript's `.spec.ts` secondary extension.
			name: "typescript_production_caller_binds_production_over_spec_file",
			build: func(t *testing.T, f *gateFixture) (int64, int64, string, string) {
				production := f.file(t, "src/app/parseUrl.ts", "typescript")
				want := f.symbol(t, production, "parseUrl", "app.parseUrl", "typescript")
				shadow := f.file(t, "src/app/parseUrl.spec.ts", "typescript")
				f.symbol(t, shadow, "parseUrl", "app.spec.parseUrl", "typescript")

				caller := f.file(t, "src/app/router.ts", "typescript")
				src := f.symbol(t, caller, "route", "app.route", "typescript")
				return f.edge(t, caller, src, "parseUrl"), want, "src/app/router.ts", "parseUrl"
			},
		},
		{
			// A fourth convention: Java's src/test/java directory plus the
			// FooTest class-name suffix.
			name: "java_production_caller_binds_production_over_test_class",
			build: func(t *testing.T, f *gateFixture) (int64, int64, string, string) {
				production := f.file(t, "src/main/java/com/acme/Loader.java", "java")
				want := f.symbol(t, production, "loadSpec", "Loader.loadSpec", "java")
				shadow := f.file(t, "src/test/java/com/acme/LoaderTest.java", "java")
				f.symbol(t, shadow, "loadSpec", "LoaderTest.loadSpec", "java")

				caller := f.file(t, "src/main/java/com/acme/Service.java", "java")
				src := f.symbol(t, caller, "run", "Service.run", "java")
				return f.edge(t, caller, src, "loadSpec"), want, "src/main/java/com/acme/Service.java", "loadSpec"
			},
		},
		{
			// Several test definitions do not add up to a competitor: the
			// production definition is still the only one this caller may bind.
			name: "production_caller_binds_production_over_several_tests",
			build: func(t *testing.T, f *gateFixture) (int64, int64, string, string) {
				production := f.file(t, "src/py/config_e.py", "python")
				want := f.symbol(t, production, "load_config_e", "config_e.load_config_e", "python")
				for _, path := range []string{
					"tests/py/test_config_e.py",
					"tests/py/test_config_e_more.py",
					"src/py/__tests__/config_e_extra.py",
				} {
					shadow := f.file(t, path, "python")
					f.symbol(t, shadow, "load_config_e", path+".load_config_e", "python")
				}

				caller := f.file(t, "src/py/caller_e.py", "python")
				src := f.symbol(t, caller, "caller_e", "caller_e", "python")
				return f.edge(t, caller, src, "load_config_e"), want, "src/py/caller_e.py", "load_config_e"
			},
		},
		{
			// The shadow is inserted first, so the production definition holds
			// the *higher* symbol id. A first-wins or MIN(id) rule would flip
			// here; selecting on which file declares the definition cannot.
			name: "production_caller_result_independent_of_insertion_order",
			build: func(t *testing.T, f *gateFixture) (int64, int64, string, string) {
				shadow := f.file(t, "tests/py/test_config_e.py", "python")
				f.symbol(t, shadow, "load_config_e", "test_config_e.load_config_e", "python")
				production := f.file(t, "src/py/config_e.py", "python")
				want := f.symbol(t, production, "load_config_e", "config_e.load_config_e", "python")

				caller := f.file(t, "src/py/caller_e.py", "python")
				src := f.symbol(t, caller, "caller_e", "caller_e", "python")
				return f.edge(t, caller, src, "load_config_e"), want, "src/py/caller_e.py", "load_config_e"
			},
		},
		{
			// Two production definitions stay ambiguous. A test definition in the
			// same group must not make one of the two look uniquely valid: P3 is
			// not weakened, only made precise about who competes.
			name: "two_production_candidates_stay_unresolved_despite_test_candidate",
			build: func(t *testing.T, f *gateFixture) (int64, int64, string, string) {
				first := f.file(t, "src/py/config_e.py", "python")
				f.symbol(t, first, "load_config_e", "config_e.load_config_e", "python")
				second := f.file(t, "src/py/config_e_alt.py", "python")
				f.symbol(t, second, "load_config_e", "config_e_alt.load_config_e", "python")
				shadow := f.file(t, "tests/py/test_config_e.py", "python")
				f.symbol(t, shadow, "load_config_e", "test_config_e.load_config_e", "python")

				caller := f.file(t, "src/py/caller_e.py", "python")
				src := f.symbol(t, caller, "caller_e", "caller_e", "python")
				return f.edge(t, caller, src, "load_config_e"), 0, "src/py/caller_e.py", "load_config_e"
			},
		},
		{
			// The only definition of the name is a test definition. Production
			// code is never wired into a test, so the edge stays unresolved --
			// this is the direction the rule refuses to trade for a higher
			// resolution rate.
			name: "production_caller_never_binds_test_only_candidate",
			build: func(t *testing.T, f *gateFixture) (int64, int64, string, string) {
				shadow := f.file(t, "tests/py/test_helpers_e.py", "python")
				f.symbol(t, shadow, "make_fixture_e", "test_helpers_e.make_fixture_e", "python")

				caller := f.file(t, "src/py/caller_e.py", "python")
				src := f.symbol(t, caller, "caller_e", "caller_e", "python")
				return f.edge(t, caller, src, "make_fixture_e"), 0, "src/py/caller_e.py", "make_fixture_e"
			},
		},
		{
			// A test caller gets no production preference: tests legitimately
			// call both production code and test helpers, so two candidates are
			// two candidates and the edge stays unresolved under plain P3.
			name: "test_caller_gets_no_production_preference",
			build: func(t *testing.T, f *gateFixture) (int64, int64, string, string) {
				production := f.file(t, "src/py/config_e.py", "python")
				f.symbol(t, production, "load_config_e", "config_e.load_config_e", "python")
				shadow := f.file(t, "tests/py/helpers_e.py", "python")
				f.symbol(t, shadow, "load_config_e", "helpers_e.load_config_e", "python")

				caller := f.file(t, "tests/py/test_caller_e.py", "python")
				src := f.symbol(t, caller, "test_caller_e", "test_caller_e", "python")
				return f.edge(t, caller, src, "load_config_e"), 0, "tests/py/test_caller_e.py", "load_config_e"
			},
		},
		{
			// The other half of the test-caller rule: a test caller still
			// resolves a uniquely named test helper. "Prefer production" must not
			// have turned into "never bind a test definition".
			name: "test_caller_still_resolves_unique_test_helper",
			build: func(t *testing.T, f *gateFixture) (int64, int64, string, string) {
				helpers := f.file(t, "tests/py/helpers_e.py", "python")
				want := f.symbol(t, helpers, "make_fixture_e", "helpers_e.make_fixture_e", "python")

				caller := f.file(t, "tests/py/test_caller_e.py", "python")
				src := f.symbol(t, caller, "test_caller_e", "test_caller_e", "python")
				return f.edge(t, caller, src, "make_fixture_e"), want, "tests/py/test_caller_e.py", "make_fixture_e"
			},
		},
		{
			// The commonest call in any repository: a test file calling the
			// production code it tests. Nothing shadows the name, so the test
			// caller resolves it -- P7 must not have made test callers pickier.
			name: "test_caller_resolves_unique_production_definition",
			build: func(t *testing.T, f *gateFixture) (int64, int64, string, string) {
				production := f.file(t, "src/py/config_e.py", "python")
				want := f.symbol(t, production, "load_config_e", "config_e.load_config_e", "python")

				caller := f.file(t, "tests/py/test_config_e.py", "python")
				src := f.symbol(t, caller, "test_load_config_e", "test_config_e.test_load_config_e", "python")
				return f.edge(t, caller, src, "load_config_e"), want, "tests/py/test_config_e.py", "load_config_e"
			},
		},
		{
			// P3 is intact for test callers in the other direction too: two test
			// definitions and no production one leave a test caller with two
			// equally valid candidates and nothing to separate them.
			name: "test_caller_with_two_test_candidates_stays_unresolved",
			build: func(t *testing.T, f *gateFixture) (int64, int64, string, string) {
				for _, path := range []string{"tests/py/helpers_a_e.py", "tests/py/helpers_b_e.py"} {
					helpers := f.file(t, path, "python")
					f.symbol(t, helpers, "make_fixture_e", path+".make_fixture_e", "python")
				}

				caller := f.file(t, "tests/py/test_caller_e.py", "python")
				src := f.symbol(t, caller, "test_caller_e", "test_caller_e", "python")
				return f.edge(t, caller, src, "make_fixture_e"), 0, "tests/py/test_caller_e.py", "make_fixture_e"
			},
		},
		{
			// Control: an ordinary same-language production call with no test
			// definition anywhere still resolves. Guards against "fixing"
			// shadowing by refusing to resolve.
			name: "production_control_still_resolves",
			build: func(t *testing.T, f *gateFixture) (int64, int64, string, string) {
				production := f.file(t, "src/py/helpers_e.py", "python")
				want := f.symbol(t, production, "normalize_e", "helpers_e.normalize_e", "python")

				caller := f.file(t, "src/py/caller_e.py", "python")
				src := f.symbol(t, caller, "caller_e", "caller_e", "python")
				return f.edge(t, caller, src, "normalize_e"), want, "src/py/caller_e.py", "normalize_e"
			},
		},
		{
			// P2 is untouched: a foreign-language definition is not a candidate,
			// so it can neither be preferred nor create competition. Here the
			// only same-language definition is a test one, so the edge stays
			// unresolved rather than crossing to the Java definition.
			name: "foreign_language_candidate_stays_blocked_when_only_test_matches",
			build: func(t *testing.T, f *gateFixture) (int64, int64, string, string) {
				foreign := f.file(t, "src/java/ConfigE.java", "java")
				f.symbol(t, foreign, "load_config_e", "ConfigE.load_config_e", "java")
				shadow := f.file(t, "tests/py/test_config_e.py", "python")
				f.symbol(t, shadow, "load_config_e", "test_config_e.load_config_e", "python")

				caller := f.file(t, "src/py/caller_e.py", "python")
				src := f.symbol(t, caller, "caller_e", "caller_e", "python")
				return f.edge(t, caller, src, "load_config_e"), 0, "src/py/caller_e.py", "load_config_e"
			},
		},
		{
			// P2 again, from the other side: a foreign definition and a
			// same-language test definition are both non-candidates for this
			// caller, and the one same-language production definition binds.
			name: "foreign_language_candidate_does_not_disturb_production_choice",
			build: func(t *testing.T, f *gateFixture) (int64, int64, string, string) {
				foreign := f.file(t, "src/java/ConfigE.java", "java")
				f.symbol(t, foreign, "load_config_e", "ConfigE.load_config_e", "java")
				production := f.file(t, "src/py/config_e.py", "python")
				want := f.symbol(t, production, "load_config_e", "config_e.load_config_e", "python")
				shadow := f.file(t, "tests/py/test_config_e.py", "python")
				f.symbol(t, shadow, "load_config_e", "test_config_e.load_config_e", "python")

				caller := f.file(t, "src/py/caller_e.py", "python")
				src := f.symbol(t, caller, "caller_e", "caller_e", "python")
				return f.edge(t, caller, src, "load_config_e"), want, "src/py/caller_e.py", "load_config_e"
			},
		},
		{
			// Stronger call-site evidence is not overridden. The call names a
			// test definition by its exact qualified name; an unrelated
			// production definition shares only the bare tail. Test-shadow
			// filtering must leave this unresolved, not retarget the call at the
			// production symbol the caller never named.
			name: "explicit_qualified_test_reference_is_not_retargeted_to_production",
			build: func(t *testing.T, f *gateFixture) (int64, int64, string, string) {
				shadow := f.file(t, "tests/py/test_helpers_e.py", "python")
				f.symbol(t, shadow, "build_e", "test_helpers_e.build_e", "python")
				unrelated := f.file(t, "src/py/factory_e.py", "python")
				f.symbol(t, unrelated, "build_e", "factory_e.build_e", "python")

				caller := f.file(t, "src/py/caller_e.py", "python")
				src := f.symbol(t, caller, "caller_e", "caller_e", "python")
				return f.edge(t, caller, src, "test_helpers_e.build_e"), 0, "src/py/caller_e.py", "build_e"
			},
		},
		{
			// Qualified evidence still outranks the bare-name veto, and the
			// test-shadow rule does not get in its way: two test definitions and
			// one production definition share the bare name, and the call names
			// the production definition's full qualified name.
			name: "qualified_evidence_resolves_production_through_shadowed_bare_name",
			build: func(t *testing.T, f *gateFixture) (int64, int64, string, string) {
				production := f.file(t, "src/py/config_e.py", "python")
				want := f.symbol(t, production, "load_config_e", "config_e.load_config_e", "python")
				for _, path := range []string{"tests/py/test_a_e.py", "tests/py/test_b_e.py"} {
					shadow := f.file(t, path, "python")
					f.symbol(t, shadow, "load_config_e", path+".load_config_e", "python")
				}

				caller := f.file(t, "src/py/caller_e.py", "python")
				src := f.symbol(t, caller, "caller_e", "caller_e", "python")
				return f.edge(t, caller, src, "config_e.load_config_e"), want, "src/py/caller_e.py", "load_config_e"
			},
		},
		{
			// Suffix evidence: the requested slash-suffix is claimed by one
			// production and one test definition. The suffix alone did not
			// separate them before P7; the caller's own kind does, so the
			// production definition binds.
			name: "slash_suffix_evidence_prefers_production_candidate",
			build: func(t *testing.T, f *gateFixture) (int64, int64, string, string) {
				production := f.file(t, "internal/pkg/util.go", "go")
				want := f.symbol(t, production, "FormatE", "github.com/org/repo/pkg.FormatE", "go")
				shadow := f.file(t, "internal/pkg/util_test.go", "go")
				f.symbol(t, shadow, "FormatE", "github.com/org/repo/testpkg/pkg.FormatE", "go")

				caller := f.file(t, "internal/pkg/caller.go", "go")
				src := f.symbol(t, caller, "CallerE", "CallerE", "go")
				return f.edge(t, caller, src, "pkg.FormatE"), want, "internal/pkg/caller.go", "pkg.FormatE"
			},
		},
	}
}

// TestResolverTestShadow_AllEntrypoints is the parity test: every entrypoint
// must reach the same verdict on every scenario, so a repository cannot resolve
// a production destination on a full index and lose it on an incremental update.
func TestResolverTestShadow_AllEntrypoints(t *testing.T) {
	for _, scenario := range testShadowScenarios() {
		for _, resolver := range ambiguityResolvers() {
			t.Run(scenario.name+"/"+resolver.name, func(t *testing.T) {
				f := newGateFixture(t)
				edgeID, wantDstID, srcPath, targetName := scenario.build(t, f)
				resolver.run(t, f, srcPath, targetName)
				gotDstID, resolved := f.dstSymbolID(t, edgeID)
				switch {
				case wantDstID == 0 && resolved:
					t.Fatalf("edge resolved to %s; want unresolved", f.qualifiedNameOf(t, gotDstID))
				case wantDstID != 0 && !resolved:
					t.Fatalf("edge unresolved; want %s", f.qualifiedNameOf(t, wantDstID))
				case wantDstID != 0 && gotDstID != wantDstID:
					t.Fatalf("edge resolved to %s; want %s",
						f.qualifiedNameOf(t, gotDstID), f.qualifiedNameOf(t, wantDstID))
				}
			})
		}
	}
}

// TestResolverTestShadow_ProvenanceDescribesTheMatchingStrategy pins the P4
// contract: test-shadow ranking filters candidates, it does not match names, so
// the persisted explanation must name the strategy that actually matched and its
// unchanged confidence. There is no `test_shadow` strategy.
func TestResolverTestShadow_ProvenanceDescribesTheMatchingStrategy(t *testing.T) {
	f := newGateFixture(t)
	production := f.file(t, "src/py/config_e.py", "python")
	want := f.symbol(t, production, "load_config_e", "config_e.load_config_e", "python")
	shadow := f.file(t, "tests/py/test_config_e.py", "python")
	f.symbol(t, shadow, "load_config_e", "test_config_e.load_config_e", "python")
	caller := f.file(t, "src/py/caller_e.py", "python")
	src := f.symbol(t, caller, "caller_e", "caller_e", "python")
	edgeID := f.edge(t, caller, src, "load_config_e")

	if _, err := f.store.ResolveEdges(f.ctx, f.repoID); err != nil {
		t.Fatalf("ResolveEdges() error = %v", err)
	}
	// assertResolvedWith also checks the confidence registered for that strategy,
	// which P7 leaves untouched (high, as exact_name always was).
	f.assertResolvedWith(t, edgeID, want, ResolutionStrategyExactName)
	if got := resolutionConfidenceFor(ResolutionStrategyExactName); got != ResolutionConfidenceHigh {
		t.Errorf("exact_name confidence = %q, want %q; P7 must not remap confidence", got, ResolutionConfidenceHigh)
	}
}

// TestResolverTestShadow_StatsReportShadowBlockingSeparately checks the
// name-targeted counters stay honest: a production call whose only candidate is
// a test definition is neither ambiguous nor language-blocked, and it is not
// quietly dropped from the totals either.
func TestResolverTestShadow_StatsReportShadowBlockingSeparately(t *testing.T) {
	f := newGateFixture(t)
	shadow := f.file(t, "tests/py/test_helpers_e.py", "python")
	f.symbol(t, shadow, "make_fixture_e", "test_helpers_e.make_fixture_e", "python")
	production := f.file(t, "src/py/helpers_e.py", "python")
	wantDst := f.symbol(t, production, "normalize_e", "helpers_e.normalize_e", "python")

	caller := f.file(t, "src/py/caller_e.py", "python")
	src := f.symbol(t, caller, "caller_e", "caller_e", "python")
	shadowedEdge := f.edge(t, caller, src, "make_fixture_e")
	resolvableEdge := f.edge(t, caller, src, "normalize_e")

	stats, err := f.store.ResolveEdgesForNamesWithStats(f.ctx, f.repoID, []string{"make_fixture_e", "normalize_e"})
	if err != nil {
		t.Fatalf("ResolveEdgesForNamesWithStats() error = %v", err)
	}
	if dst, resolved := f.dstSymbolID(t, shadowedEdge); resolved {
		t.Errorf("production call bound test definition %s; want unresolved", f.qualifiedNameOf(t, dst))
	}
	if dst, resolved := f.dstSymbolID(t, resolvableEdge); !resolved || dst != wantDst {
		t.Errorf("production call resolved to (%v,%d); want (%v,%d)", resolved, dst, true, wantDst)
	}
	if stats.TestShadowBlocked != 1 {
		t.Errorf("TestShadowBlocked = %d, want 1", stats.TestShadowBlocked)
	}
	if stats.AmbiguityBlocked != 0 {
		t.Errorf("AmbiguityBlocked = %d, want 0; the name had exactly one candidate", stats.AmbiguityBlocked)
	}
	if stats.LanguageBlocked != 0 {
		t.Errorf("LanguageBlocked = %d, want 0; the candidate was same-language", stats.LanguageBlocked)
	}
	if got := stats.TargetsResolved + stats.TargetsUnresolved + stats.UnknownSrcLanguage; got != stats.TargetsSelected {
		t.Errorf("resolved(%d) + unresolved(%d) + unknown_src(%d) = %d, want selected(%d)",
			stats.TargetsResolved, stats.TargetsUnresolved, stats.UnknownSrcLanguage, got, stats.TargetsSelected)
	}
}

// TestIsTestFilePath pins the classifier. The negatives matter more than the
// positives: a false positive demotes real production code, which can turn a
// resolvable edge into a missing one.
func TestIsTestFilePath(t *testing.T) {
	testPaths := []string{
		// Directory conventions.
		"tests/py/anything.py",
		"test/support.rb",
		"testdata/gen/sample.go",
		"src/app/__tests__/helper.ts",
		"src/test/java/com/acme/Support.java",
		"Tests/AppTests/Helper.swift",
		// Filename conventions.
		"internal/store/store_test.go",
		"pkg/util/test_helper.py",
		"pkg/util/helper_test.py",
		"src/app/parseUrl.test.ts",
		"src/app/parseUrl.spec.tsx",
		"src/main/java/com/acme/LoaderTest.java",
		"app/src/LoaderTests.kt",
		"Acme/LoaderTests.cs",
		"Sources/App/LoaderTests.swift",
		"src/Acme/LoaderTest.php",
		"lib/loader_test.rb",
		"lib/loader_spec.rb",
		"src/loader_test.cc",
		"src/loader_test.rs",
	}
	for _, path := range testPaths {
		if !IsTestFilePath(path) {
			t.Errorf("IsTestFilePath(%q) = false, want true", path)
		}
	}

	productionPaths := []string{
		// Substrings that are not the convention.
		"src/latest/loader.go",
		"src/contest/loader.go",
		"internal/protest/handler.py",
		"src/app/TestingFramework.java",
		"src/app/testable.ts",
		"src/attestation.rb",
		// Right marker, wrong language.
		"src/app/loader_test.java",
		"src/app/LoaderTest.go",
		"src/app/test_loader.go",
		"src/app/loader.test.py",
		// `spec/` is a production package name at least as often as it is
		// RSpec's directory, so the directory rule does not claim it; Ruby specs
		// are still classified by their `_spec.rb` filename.
		"spec/models/user.rb",
		"internal/spec/openapi.go",
		// Conventions this phase deliberately does not claim.
		"src/mocks/loader.py",
		"src/py/mock_loader.py",
		"src/fixtures/loader.py",
		"src/py/conftest.py",
		// Ordinary production code.
		"internal/store/store.go",
		"src/py/config.py",
		"",
		"README.md",
	}
	for _, path := range productionPaths {
		if IsTestFilePath(path) {
			t.Errorf("IsTestFilePath(%q) = true, want false", path)
		}
	}
}

// TestIsTestFilePathIgnoresPathSeparatorStyle guards the one platform detail
// that could make the classifier disagree with itself: `files.path` is written
// with the host separator, so a Windows-style path must classify the same way.
func TestIsTestFilePathIgnoresPathSeparatorStyle(t *testing.T) {
	if !IsTestFilePath(`internal\store\store_test.go`) {
		t.Error(`IsTestFilePath("internal\\store\\store_test.go") = false, want true`)
	}
	if !IsTestFilePath(`tests\py\helper.py`) {
		t.Error(`IsTestFilePath("tests\\py\\helper.py") = false, want true`)
	}
}
