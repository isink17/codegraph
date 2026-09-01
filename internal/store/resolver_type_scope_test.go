package store

import (
	"context"
	"database/sql"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

// Regression coverage for P22.9 bare-name type-target scope.
//
// The two false positives P22.4 recorded and deferred are the first two
// scenarios below, reduced to the graph they produced:
//
//	kotlin/B.kt  `class B { fun cross() { A().first() } }`  -> class A.A
//	swift/b.swift `func cross() { Box().first() }`          -> class a.Box
//
// Both bound at `exact_name`/high on nothing but repository-global uniqueness
// of a bare class name. Every scenario runs against all three production
// entrypoints so the repo-wide resolver and the Go-side binder cannot drift.

// typeScopeFixture reuses the language-gating fixture's graph builders and adds
// the import evidence this rule reads.
type typeScopeFixture struct{ *gateFixture }

func newTypeScopeFixture(t *testing.T) *typeScopeFixture {
	t.Helper()
	return &typeScopeFixture{newGateFixture(t)}
}

// importPath records that a file's source named a specifier, exactly as the
// parsers persist it: the module path, never the imported symbol.
func (f *typeScopeFixture) importPath(t *testing.T, fileID int64, specifier string) {
	t.Helper()
	if _, err := f.store.db.ExecContext(f.ctx,
		`INSERT INTO file_imports(repo_id, file_id, import_path) VALUES (?, ?, ?)`,
		f.repoID, fileID, specifier); err != nil {
		t.Fatalf("insert file_imports(%q) error = %v", specifier, err)
	}
}

// class inserts a type declaration -- the kind this rule scopes.
func (f *typeScopeFixture) class(t *testing.T, fileID int64, name, qualified, language string) int64 {
	t.Helper()
	return f.symbolKind(t, fileID, name, qualified, "class", language)
}

type typeScopeScenario struct {
	name  string
	build func(t *testing.T, f *typeScopeFixture) (edgeID, wantDstID int64, srcPath, targetName string)
}

func typeScopeScenarios() []typeScopeScenario {
	return []typeScopeScenario{
		{
			// The first case P22.4 recorded, rebuilt from the corpus patch:
			// kotlin/A.kt declares `class A` with NO package, kotlin/B.kt
			// declares `class B { fun cross() { A().first() } }` with no package
			// and no import. Both files are in Kotlin's root package, so `A()`
			// names that class with no import required -- the binding is what
			// Kotlin itself resolves, and it must SURVIVE. It was scored a false
			// positive only because that phase's truth set listed a function
			// target for every call site.
			name: "p224_kotlin_same_package_still_resolves",
			build: func(t *testing.T, f *typeScopeFixture) (int64, int64, string, string) {
				defFile := f.file(t, "kotlin/A.kt", "kotlin")
				dst := f.class(t, defFile, "A", "A.A", "kotlin")
				f.symbolKind(t, defFile, "first", "A.first", "function", "kotlin")
				f.importPath(t, defFile, "kotlin.io.println")
				callFile := f.file(t, "kotlin/B.kt", "kotlin")
				src := f.symbolKind(t, callFile, "cross", "B.cross", "function", "kotlin")
				return f.edge(t, callFile, src, "A"), dst, "kotlin/B.kt", "A"
			},
		},
		{
			// The second: swift/b.swift and swift/a.swift are one Swift module,
			// where `Box()` needs no import either. Also survives.
			name: "p224_swift_same_module_still_resolves",
			build: func(t *testing.T, f *typeScopeFixture) (int64, int64, string, string) {
				defFile := f.file(t, "swift/a.swift", "swift")
				dst := f.class(t, defFile, "Box", "a.Box", "swift")
				f.importPath(t, defFile, "Foundation")
				callFile := f.file(t, "swift/b.swift", "swift")
				src := f.symbolKind(t, callFile, "cross", "b.cross", "function", "swift")
				return f.edge(t, callFile, src, "Box"), dst, "swift/b.swift", "Box"
			},
		},
		{
			// The real defect, in its natural habitat:
			// mitmproxy/addons/maplocal.py does `from pathlib import Path` and
			// calls `Path(x)`, and mitmproxy declares its own `class Path` in
			// mitmproxy/types.py. 84 edges of this exact shape on that repo.
			name: "stdlib_name_does_not_bind_project_class",
			build: func(t *testing.T, f *typeScopeFixture) (int64, int64, string, string) {
				defFile := f.file(t, "app/types.py", "python")
				f.class(t, defFile, "Path", "types.Path", "python")
				callFile := f.file(t, "app/addons/maplocal.py", "python")
				f.importPath(t, callFile, "pathlib")
				src := f.symbolKind(t, callFile, "load", "maplocal.load", "function", "python")
				return f.edge(t, callFile, src, "Path"), 0, "app/addons/maplocal.py", "Path"
			},
		},
		{
			// C++ has no cross-translation-unit visibility without an include:
			// googletest-message-test.cc `Message()` bound a DIFFERENT test
			// file's `class Message` in googlemock/. 23 edges on googletest.
			name: "cpp_without_include_does_not_bind_other_tu_class",
			build: func(t *testing.T, f *typeScopeFixture) (int64, int64, string, string) {
				defFile := f.file(t, "googlemock/test/utils_test.cc", "cpp")
				f.class(t, defFile, "Message", "utils_test::Message", "cpp")
				callFile := f.file(t, "googletest/test/message_test.cc", "cpp")
				f.importPath(t, callFile, "gtest/gtest.h")
				src := f.symbolKind(t, callFile, "run", "message_test::run", "function", "cpp")
				return f.edge(t, callFile, src, "Message"), 0, "googletest/test/message_test.cc", "Message"
			},
		},
		{
			// The include that DOES name the declaring file keeps its binding,
			// tail-matched under a vendored include root
			// (`#include "gtest/gtest.h"` naming test/gtest/gtest/gtest.h).
			name: "cpp_include_tail_grants_scope",
			build: func(t *testing.T, f *typeScopeFixture) (int64, int64, string, string) {
				defFile := f.file(t, "test/gtest/gtest/gtest.h", "cpp")
				dst := f.class(t, defFile, "AssertHelper", "gtest::AssertHelper", "cpp")
				callFile := f.file(t, "test/gtest/gmock-gtest-all.cc", "cpp")
				f.importPath(t, callFile, "gtest/gtest.h")
				src := f.symbolKind(t, callFile, "run", "all::run", "function", "cpp")
				return f.edge(t, callFile, src, "AssertHelper"), dst, "test/gtest/gmock-gtest-all.cc", "AssertHelper"
			},
		},
		{
			// A C++ top-level enum has a bare qualified_name, so it reaches the
			// bare-name level through exact_qualified, which carries no kind
			// filter. It is a type declaration and is scoped like one.
			name: "cpp_enum_without_include_unresolved",
			build: func(t *testing.T, f *typeScopeFixture) (int64, int64, string, string) {
				defFile := f.file(t, "src/palette.h", "cpp")
				f.symbolKind(t, defFile, "Color", "Color", "enum", "cpp")
				callFile := f.file(t, "src/paint.cc", "cpp")
				src := f.symbolKind(t, callFile, "paint", "paint::paint", "function", "cpp")
				return f.edge(t, callFile, src, "Color"), 0, "src/paint.cc", "Color"
			},
		},
		{
			// An ungated language keeps its cross-file type bindings for every
			// kind, with no import anywhere: a Swift `struct` in a sibling file
			// of the same module is visible, and P22.9 must not touch it.
			name: "swift_struct_cross_file_still_resolves",
			build: func(t *testing.T, f *typeScopeFixture) (int64, int64, string, string) {
				defFile := f.file(t, "app/Point.swift", "swift")
				dst := f.symbolKind(t, defFile, "Point", "Point.Point", "struct", "swift")
				callFile := f.file(t, "app/Draw.swift", "swift")
				src := f.symbolKind(t, callFile, "draw", "Draw.draw", "function", "swift")
				return f.edge(t, callFile, src, "Point"), dst, "app/Draw.swift", "Point"
			},
		},
		{
			// Recall control: a constructor call whose class the same file
			// declares keeps its edge. This is the common shape and the reason
			// the rule is a scope rule rather than a kind ban.
			name: "local_constructor_resolves",
			build: func(t *testing.T, f *typeScopeFixture) (int64, int64, string, string) {
				file := f.file(t, "app/widget.py", "python")
				dst := f.class(t, file, "Widget", "widget.Widget", "python")
				src := f.symbolKind(t, file, "make", "widget.make", "function", "python")
				return f.edge(t, file, src, "Widget"), dst, "app/widget.py", "Widget"
			},
		},
		{
			// Recall control: cross-file constructor call with an import naming
			// the declaring module.
			name: "cross_file_constructor_with_import_resolves",
			build: func(t *testing.T, f *typeScopeFixture) (int64, int64, string, string) {
				defFile := f.file(t, "app/models.py", "python")
				dst := f.class(t, defFile, "Client", "models.Client", "python")
				callFile := f.file(t, "app/caller.py", "python")
				f.importPath(t, callFile, "app.models")
				src := f.symbolKind(t, callFile, "go", "caller.go", "function", "python")
				return f.edge(t, callFile, src, "Client"), dst, "app/caller.py", "Client"
			},
		},
		{
			// Recall control: the barrel re-export hop. `app/pkg/__init__.py`
			// publishes `Layer` with a relative import, and the caller names the
			// package, not the module.
			name: "package_reexport_constructor_resolves",
			build: func(t *testing.T, f *typeScopeFixture) (int64, int64, string, string) {
				defFile := f.file(t, "app/pkg/tcp.py", "python")
				dst := f.class(t, defFile, "Layer", "tcp.Layer", "python")
				barrel := f.file(t, "app/pkg/__init__.py", "python")
				f.importPath(t, barrel, ".tcp")
				callFile := f.file(t, "app/caller.py", "python")
				f.importPath(t, callFile, "app.pkg")
				src := f.symbolKind(t, callFile, "go", "caller.go", "function", "python")
				return f.edge(t, callFile, src, "Layer"), dst, "app/caller.py", "Layer"
			},
		},
		{
			// §17: an external import must not bind a coincidentally named
			// project class. `from requests import Client` names no project file.
			name: "external_import_does_not_bind_project_class",
			build: func(t *testing.T, f *typeScopeFixture) (int64, int64, string, string) {
				defFile := f.file(t, "app/models.py", "python")
				f.class(t, defFile, "Client", "models.Client", "python")
				callFile := f.file(t, "app/caller.py", "python")
				f.importPath(t, callFile, "requests")
				src := f.symbolKind(t, callFile, "go", "caller.go", "function", "python")
				return f.edge(t, callFile, src, "Client"), 0, "app/caller.py", "Client"
			},
		},
		{
			// The barrel hop must not become transitive reachability. The package
			// file here imports the declaring module *absolutely*, which is a
			// plain dependency and not a re-export: `import app.types` publishes
			// nothing into `app`'s own namespace, so a caller that imports `app`
			// still cannot spell `Path` bare. This is `pathlib.Path` in
			// mitmproxy/addons/maplocal.py, reduced.
			name: "package_dependency_is_not_a_reexport",
			build: func(t *testing.T, f *typeScopeFixture) (int64, int64, string, string) {
				defFile := f.file(t, "app/types.py", "python")
				f.class(t, defFile, "Path", "types.Path", "python")
				pkg := f.file(t, "app/__init__.py", "python")
				f.importPath(t, pkg, "app.types")
				callFile := f.file(t, "app/addons/maplocal.py", "python")
				f.importPath(t, callFile, "app")
				f.importPath(t, callFile, "pathlib")
				src := f.symbolKind(t, callFile, "load", "maplocal.load", "function", "python")
				return f.edge(t, callFile, src, "Path"), 0, "app/addons/maplocal.py", "Path"
			},
		},
		{
			// §11: a class and a function of one name in one language. The call
			// syntax cannot tell them apart, so the answer is unresolved -- and
			// it must stay unresolved rather than the scope rule silently
			// promoting the survivor.
			name: "class_function_collision_unresolved",
			build: func(t *testing.T, f *typeScopeFixture) (int64, int64, string, string) {
				file := f.file(t, "app/dual.py", "python")
				f.class(t, file, "Handler", "dual.Handler", "python")
				f.symbolKind(t, file, "Handler", "dual.Handler_fn", "function", "python")
				src := f.symbolKind(t, file, "pick", "dual.pick", "function", "python")
				return f.edge(t, file, src, "Handler"), 0, "app/dual.py", "Handler"
			},
		},
		{
			// §12: two classes each declaring `constructor`. A bare `constructor`
			// spelling must never bind one because it is unique after some
			// unrelated filter -- here it is not unique, and the answer is
			// unresolved in either direction.
			name: "constructor_name_collision_unresolved",
			build: func(t *testing.T, f *typeScopeFixture) (int64, int64, string, string) {
				one := f.file(t, "src/one.ts", "typescript")
				f.class(t, one, "One", "one.One", "typescript")
				f.method(t, one, "constructor", "one.One.constructor", "One", "typescript")
				two := f.file(t, "src/two.ts", "typescript")
				f.class(t, two, "Two", "two.Two", "typescript")
				f.method(t, two, "constructor", "two.Two.constructor", "Two", "typescript")
				callFile := f.file(t, "src/build.ts", "typescript")
				f.importPath(t, callFile, "./one")
				src := f.symbolKind(t, callFile, "build", "build.build", "function", "typescript")
				return f.edge(t, callFile, src, "constructor"), 0, "src/build.ts", "constructor"
			},
		},
		{
			// A lone `constructor` symbol is unique repo-wide but is a method of
			// a class the caller never named. P22.1 already refuses the dotted
			// spelling; this pins the bare one, which reaches the same target
			// through the bare-name strategy.
			name: "sole_constructor_not_bound_by_bare_name",
			build: func(t *testing.T, f *typeScopeFixture) (int64, int64, string, string) {
				only := f.file(t, "src/only.ts", "typescript")
				f.class(t, only, "Only", "only.Only", "typescript")
				f.method(t, only, "constructor", "only.Only.constructor", "Only", "typescript")
				callFile := f.file(t, "src/build.ts", "typescript")
				src := f.symbolKind(t, callFile, "build", "build.build", "function", "typescript")
				// No import: `build.ts` cannot see only.ts at all.
				return f.edge(t, callFile, src, "Only"), 0, "src/build.ts", "Only"
			},
		},
		{
			// §18: the language gate stays load-bearing. A Python `Foo()` may not
			// reach a TypeScript class, import evidence or not.
			name: "language_gate_still_blocks_typed_target",
			build: func(t *testing.T, f *typeScopeFixture) (int64, int64, string, string) {
				tsFile := f.file(t, "src/foo.ts", "typescript")
				f.class(t, tsFile, "Foo", "foo.Foo", "typescript")
				callFile := f.file(t, "src/caller.py", "python")
				f.importPath(t, callFile, "src.foo")
				src := f.symbolKind(t, callFile, "go", "caller.go", "function", "python")
				return f.edge(t, callFile, src, "Foo"), 0, "src/caller.py", "Foo"
			},
		},
		{
			// §19: P7 test shadowing survives. A production caller that imports a
			// test module still may not bind the test-only class.
			name: "production_caller_does_not_bind_test_class",
			build: func(t *testing.T, f *typeScopeFixture) (int64, int64, string, string) {
				testFile := f.file(t, "test/support/test_helpers.py", "python")
				f.class(t, testFile, "Fixture", "test_helpers.Fixture", "python")
				callFile := f.file(t, "app/service.py", "python")
				f.importPath(t, callFile, "test.support.test_helpers")
				src := f.symbolKind(t, callFile, "run", "service.run", "function", "python")
				return f.edge(t, callFile, src, "Fixture"), 0, "app/service.py", "Fixture"
			},
		},
		{
			// A qualified spelling carries its own evidence and is untouched:
			// `gtest_test_utils.Subprocess(...)` in googletest keeps binding.
			name: "qualified_spelling_binds_type_across_files",
			build: func(t *testing.T, f *typeScopeFixture) (int64, int64, string, string) {
				defFile := f.file(t, "test/gtest_test_utils.py", "python")
				dst := f.class(t, defFile, "Subprocess", "gtest_test_utils.Subprocess", "python")
				callFile := f.file(t, "test/filter_unittest.py", "python")
				src := f.symbolKind(t, callFile, "main", "filter_unittest.main", "function", "python")
				return f.edge(t, callFile, src, "gtest_test_utils.Subprocess"), dst,
					"test/filter_unittest.py", "gtest_test_utils.Subprocess"
			},
		},
		{
			// Callables are deliberately out of scope for this rule: a bare
			// function name still resolves across files exactly as before, so
			// P22.9 cannot be mistaken for a general bare-name narrowing.
			name: "cross_file_function_unaffected",
			build: func(t *testing.T, f *typeScopeFixture) (int64, int64, string, string) {
				defFile := f.file(t, "app/helpers.py", "python")
				dst := f.symbolKind(t, defFile, "normalize", "helpers.normalize", "function", "python")
				callFile := f.file(t, "app/caller.py", "python")
				src := f.symbolKind(t, callFile, "go", "caller.go", "function", "python")
				return f.edge(t, callFile, src, "normalize"), dst, "app/caller.py", "normalize"
			},
		},
		{
			// P22.1 regression control: a member call that reaches the resolver
			// with its receiver intact never touches a bare-name level, so the
			// type-scope rule neither helps nor hinders it. (The C++ adapter is
			// the one adapter that still hands the resolver a bare tail instead;
			// that is `legacy c++ support` behaviour with its own tests, recorded
			// as a P22 finding rather than changed here.)
			name: "member_spelling_does_not_bind_other_class_method",
			build: func(t *testing.T, f *typeScopeFixture) (int64, int64, string, string) {
				defFile := f.file(t, "src/buffer.cpp", "cpp")
				f.class(t, defFile, "Buffer", "Buffer", "cpp")
				f.method(t, defFile, "size", "Buffer::size", "Buffer", "cpp")
				callFile := f.file(t, "src/count.cpp", "cpp")
				src := f.symbolKind(t, callFile, "countItems", "count::countItems", "function", "cpp")
				return f.edge(t, callFile, src, "v.size"), 0, "src/count.cpp", "size"
			},
		},
	}
}

func TestResolverTypeScope_AllEntrypoints(t *testing.T) {
	resolvers := []struct {
		name string
		run  func(t *testing.T, f *typeScopeFixture, srcPath, targetName string)
	}{
		{
			name: "repo",
			run: func(t *testing.T, f *typeScopeFixture, _, _ string) {
				if _, err := f.store.ResolveEdges(f.ctx, f.repoID); err != nil {
					t.Fatalf("ResolveEdges() error = %v", err)
				}
			},
		},
		{
			name: "paths",
			run: func(t *testing.T, f *typeScopeFixture, srcPath, _ string) {
				if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{srcPath}); err != nil {
					t.Fatalf("ResolveEdgesForPaths() error = %v", err)
				}
			},
		},
		{
			name: "names",
			run: func(t *testing.T, f *typeScopeFixture, _, targetName string) {
				if _, err := f.store.ResolveEdgesForNamesWithStats(f.ctx, f.repoID, []string{targetName}); err != nil {
					t.Fatalf("ResolveEdgesForNamesWithStats() error = %v", err)
				}
			},
		},
	}

	for _, scenario := range typeScopeScenarios() {
		for _, resolver := range resolvers {
			t.Run(scenario.name+"/"+resolver.name, func(t *testing.T) {
				f := newTypeScopeFixture(t)
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

// TestResolverTypeScope_InsertionOrderIndependent pins §34: the same semantic
// graph must answer identically however its rows were inserted. Two classes of
// one name are declared in two files, one of which the caller imports; the
// caller must bind the imported one whichever file was indexed first, and never
// the other.
func TestResolverTypeScope_InsertionOrderIndependent(t *testing.T) {
	type built struct {
		edgeID int64
		wantID int64
	}
	buildImportedFirst := func(t *testing.T, f *typeScopeFixture) built {
		seen := f.file(t, "app/seen.py", "python")
		want := f.class(t, seen, "Client", "seen.Client", "python")
		other := f.file(t, "vendor/other.py", "python")
		f.class(t, other, "Client", "other.Client", "python")
		callFile := f.file(t, "app/caller.py", "python")
		f.importPath(t, callFile, "app.seen")
		src := f.symbolKind(t, callFile, "go", "caller.go", "function", "python")
		return built{edgeID: f.edge(t, callFile, src, "Client"), wantID: want}
	}
	buildOtherFirst := func(t *testing.T, f *typeScopeFixture) built {
		other := f.file(t, "vendor/other.py", "python")
		f.class(t, other, "Client", "other.Client", "python")
		seen := f.file(t, "app/seen.py", "python")
		want := f.class(t, seen, "Client", "seen.Client", "python")
		callFile := f.file(t, "app/caller.py", "python")
		f.importPath(t, callFile, "app.seen")
		src := f.symbolKind(t, callFile, "go", "caller.go", "function", "python")
		return built{edgeID: f.edge(t, callFile, src, "Client"), wantID: want}
	}

	for _, order := range []struct {
		name  string
		build func(*testing.T, *typeScopeFixture) built
	}{
		{"imported_first", buildImportedFirst},
		{"other_first", buildOtherFirst},
	} {
		t.Run(order.name, func(t *testing.T) {
			f := newTypeScopeFixture(t)
			b := order.build(t, f)
			if _, err := f.store.ResolveEdges(f.ctx, f.repoID); err != nil {
				t.Fatalf("ResolveEdges() error = %v", err)
			}
			gotID, resolved := f.dstSymbolID(t, b.edgeID)
			// Two same-language declarations of one bare name are ambiguous at the
			// bare-name level (P3), so the answer is unresolved in both orders.
			// What must never happen is one order binding and the other not, or
			// either order picking the unimported declaration.
			if resolved {
				t.Fatalf("edge resolved to %s; want unresolved (two candidates)", f.qualifiedNameOf(t, gotID))
			}
		})
	}
}

// TestResolverTypeScope_FullIncrementalParity pins §31 for this rule: a fresh
// repo-wide resolve and an incremental name-targeted resolve must reach the
// same destination for every scenario, since they are two implementations of
// the same policy (resolverBareNameTypeScopeSQL and typeTargetOutOfScope).
func TestResolverTypeScope_FullIncrementalParity(t *testing.T) {
	for _, scenario := range typeScopeScenarios() {
		t.Run(scenario.name, func(t *testing.T) {
			full := newTypeScopeFixture(t)
			fullEdge, _, _, _ := scenario.build(t, full)
			if _, err := full.store.ResolveEdges(full.ctx, full.repoID); err != nil {
				t.Fatalf("ResolveEdges() error = %v", err)
			}
			fullID, fullResolved := full.dstSymbolID(t, fullEdge)

			inc := newTypeScopeFixture(t)
			incEdge, _, srcPath, targetName := scenario.build(t, inc)
			if err := inc.store.ResolveEdgesForPaths(inc.ctx, inc.repoID, []string{srcPath}); err != nil {
				t.Fatalf("ResolveEdgesForPaths() error = %v", err)
			}
			if _, err := inc.store.ResolveEdgesForNamesWithStats(inc.ctx, inc.repoID, []string{targetName}); err != nil {
				t.Fatalf("ResolveEdgesForNamesWithStats() error = %v", err)
			}
			incID, incResolved := inc.dstSymbolID(t, incEdge)

			if fullResolved != incResolved {
				t.Fatalf("full resolved = %v, incremental resolved = %v", fullResolved, incResolved)
			}
			if !fullResolved {
				return
			}
			if got, want := inc.qualifiedNameOf(t, incID), full.qualifiedNameOf(t, fullID); got != want {
				t.Fatalf("incremental bound %s; full bound %s", got, want)
			}
		})
	}
}

// TestImportSpecifierPath pins the specifier forms the scope rule has to
// understand. It is a pure function, so the cases are the documentation.
func TestImportSpecifierPath(t *testing.T) {
	cases := []struct {
		name      string
		importer  string
		specifier string
		language  string
		want      string
		ok        bool
	}{
		{"absolute_dotted_module", "app/caller.py", "mitmproxy.http", "python", "mitmproxy/http", true},
		{"bare_module", "app/caller.py", "os", "python", "os", true},
		{"python_relative_sibling", "app/pkg/__init__.py", ".tcp", "python", "app/pkg/tcp", true},
		{"python_relative_parent", "app/pkg/sub/mod.py", "..util", "python", "app/pkg/util", true},
		{"python_relative_package", "app/pkg/__init__.py", ".", "python", "app/pkg", true},
		{"js_relative", "src/build.ts", "./one", "typescript", "src/one", true},
		{"js_parent_relative", "src/sub/build.ts", "../one", "typescript", "src/one", true},
		{"path_shaped", "src/build.ts", "src/shared/model.ts", "typescript", "src/shared/model.ts", true},
		{"c_include", "test/all.cc", "gtest/gtest.h", "cpp", "gtest/gtest.h", true},
		// P22.13: a single-segment `#include` is a path, not a dotted module.
		{"c_include_single_segment", "src/caller.cpp", "foo.h", "cpp", "foo.h", true},
		{"c_include_dot_relative", "src/caller.cpp", "./foo.h", "cpp", "src/foo.h", true},
		{"java_package_qualified", "Main.java", "app.Ledger", "java", "app/Ledger", true},
		// The C-family reading is gated on the importing file's LANGUAGE: these
		// two carry a C-family extension by coincidence and must stay dotted.
		{"java_class_named_c", "Main.java", "app.C", "java", "app/C", true},
		{"python_module_named_cc", "app/caller.py", "pkg.cc", "python", "pkg/cc", true},
		{"empty", "a.py", "", "python", "", false},
		{"rust_scoped", "a.rs", "std::fs", "rust", "", false},
		{"quoted_noise", "a.py", `numpy as np`, "python", "", false},
		{"escapes_repo", "a.py", "../../outside", "python", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := importSpecifierPath(tc.importer, tc.specifier, tc.language)
			if ok != tc.ok {
				t.Fatalf("importSpecifierPath(%q, %q, %q) ok = %v; want %v", tc.importer, tc.specifier, tc.language, ok, tc.ok)
			}
			if ok && got != tc.want {
				t.Fatalf("importSpecifierPath(%q, %q, %q) = %q; want %q", tc.importer, tc.specifier, tc.language, got, tc.want)
			}
		})
	}
}

// TestTypeSymbolKindsSQLMatchesGoTwin keeps the two kind vocabularies from
// drifting: the repo-wide predicate reads typeSymbolKindsSQL and the binder
// reads typeSymbolKinds, and a kind in one but not the other would make the
// full and incremental paths disagree about what a type is.
func TestTypeSymbolKindsSQLMatchesGoTwin(t *testing.T) {
	ctx := context.Background()
	f := newTypeScopeFixture(t)
	rows, err := f.store.db.QueryContext(ctx,
		`WITH kinds(k) AS (VALUES `+typeScopeKindValuesSQL()+`) SELECT k FROM kinds WHERE k IN `+typeSymbolKindsSQL)
	if err != nil {
		t.Fatalf("query error = %v", err)
	}
	defer rows.Close()
	inSQL := map[string]struct{}{}
	for rows.Next() {
		var kind string
		if err := rows.Scan(&kind); err != nil {
			t.Fatalf("scan error = %v", err)
		}
		inSQL[kind] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows error = %v", err)
	}
	if len(inSQL) != len(typeSymbolKinds) {
		t.Fatalf("SQL kind set has %d members; Go twin has %d", len(inSQL), len(typeSymbolKinds))
	}
	for kind := range typeSymbolKinds {
		if _, ok := inSQL[kind]; !ok {
			t.Fatalf("kind %q is a type in Go but not in SQL", kind)
		}
	}
	for _, kind := range []string{"function", "method", "value"} {
		if isTypeSymbolKind(kind) {
			t.Fatalf("kind %q must not be a type kind", kind)
		}
	}
}

// typeScopeKindValuesSQL renders the Go kind set as a VALUES list so the test
// above probes SQL with exactly the kinds Go believes in.
func typeScopeKindValuesSQL() string {
	out := ""
	for kind := range typeSymbolKinds {
		if out != "" {
			out += ","
		}
		out += "('" + kind + "')"
	}
	return out
}

// -- query surfaces ---------------------------------------------------------

// TestTypeScopeQuerySurfacesCannotRebuildRefusedRelation is the P22.9 twin of
// the P22.6 and P22.7 query-side regressions. Every earlier evidence narrowing
// found that a safe persisted resolver is not enough, because the public
// surfaces rebuild relationships from `edges.dst_name` on unresolved edges.
//
// The graph is the measured defect: `app/addons/maplocal.py` does
// `from pathlib import Path` and calls `Path(...)`, while the project declares
// its own `class Path` in `app/types.py`. The resolver refuses the edge; no
// public surface may put it back.
func TestTypeScopeQuerySurfacesCannotRebuildRefusedRelation(t *testing.T) {
	f := newTypeScopeFixture(t)
	defFile := f.file(t, "app/types.py", "python")
	projectPath := f.class(t, defFile, "Path", "types.Path", "python")
	callFile := f.file(t, "app/addons/maplocal.py", "python")
	f.importPath(t, callFile, "pathlib")
	load := f.symbolKind(t, callFile, "load", "maplocal.load", "function", "python")
	edgeID := f.edge(t, callFile, load, "Path")

	if _, err := f.store.ResolveEdges(f.ctx, f.repoID); err != nil {
		t.Fatalf("ResolveEdges() error = %v", err)
	}
	if gotID, resolved := f.dstSymbolID(t, edgeID); resolved {
		t.Fatalf("resolver bound the edge to %s; want unresolved", f.qualifiedNameOf(t, gotID))
	}

	for _, probe := range []string{"Path", "types.Path"} {
		callers, err := f.store.FindCallers(f.ctx, f.repoID, probe, 0, 20, 0)
		if err != nil {
			t.Fatalf("FindCallers(%q) error = %v", probe, err)
		}
		if containsSymbolID(callers, load) {
			t.Fatalf("FindCallers(%q) returned maplocal.load; the resolver refused that relation", probe)
		}
	}

	callees, err := f.store.FindCallees(f.ctx, f.repoID, "maplocal.load", 0, 20, 0)
	if err != nil {
		t.Fatalf("FindCallees() error = %v", err)
	}
	if containsSymbolID(callees, projectPath) {
		t.Fatalf("FindCallees(maplocal.load) returned class types.Path; the resolver refused that relation")
	}

	neighbors, err := f.store.FindContextNeighbors(f.ctx, f.repoID, []ContextSeed{
		{SymbolID: load, QualifiedName: "maplocal.load", ShortName: "load", AllowShortEvidence: true},
		{SymbolID: projectPath, QualifiedName: "types.Path", ShortName: "Path", AllowShortEvidence: true},
	}, 20)
	if err != nil {
		t.Fatalf("FindContextNeighbors() error = %v", err)
	}
	if containsSymbolID(neighbors[0].Callees, projectPath) {
		t.Fatalf("context expansion pulled class types.Path in as a callee of maplocal.load")
	}
	if containsSymbolID(neighbors[1].Callers, load) {
		t.Fatalf("context expansion pulled maplocal.load in as a caller of class types.Path")
	}
}

// TestTypeScopeQuerySurfacesKeepUngatedLanguages pins the other half of the
// language restriction on the query side: the two cases P22.4 recorded are
// legitimate bindings, so a Kotlin caller must still be reachable from its
// class through every surface, name evidence included.
func TestTypeScopeQuerySurfacesKeepUngatedLanguages(t *testing.T) {
	f := newTypeScopeFixture(t)
	defFile := f.file(t, "kotlin/A.kt", "kotlin")
	classA := f.class(t, defFile, "A", "A.A", "kotlin")
	callFile := f.file(t, "kotlin/B.kt", "kotlin")
	cross := f.symbolKind(t, callFile, "cross", "B.cross", "function", "kotlin")
	f.edge(t, callFile, cross, "A")
	// A second, deliberately unresolvable spelling so the name legs are the only
	// thing that could answer for it.
	orphan := f.symbolKind(t, callFile, "other", "B.other", "function", "kotlin")
	f.edge(t, callFile, orphan, "A")

	if _, err := f.store.ResolveEdges(f.ctx, f.repoID); err != nil {
		t.Fatalf("ResolveEdges() error = %v", err)
	}
	callers, err := f.store.FindCallers(f.ctx, f.repoID, "A.A", 0, 20, 0)
	if err != nil {
		t.Fatalf("FindCallers(A.A) error = %v", err)
	}
	if !containsSymbolID(callers, cross) {
		t.Fatalf("FindCallers(A.A) lost the Kotlin same-package caller: %+v", callers)
	}
	callees, err := f.store.FindCallees(f.ctx, f.repoID, "B.cross", 0, 20, 0)
	if err != nil {
		t.Fatalf("FindCallees(B.cross) error = %v", err)
	}
	if !containsSymbolID(callees, classA) {
		t.Fatalf("FindCallees(B.cross) lost class A.A: %+v", callees)
	}
}

// TestTypeScopeQuerySurfacesKeepLegitimateEvidence is the recall half. A bound
// constructor edge must still surface on every one of those surfaces -- the
// gate removes a name-evidence leg, not the relationship -- and a bare name
// that denotes a FUNCTION keeps its name-evidence leg untouched.
func TestTypeScopeQuerySurfacesKeepLegitimateEvidence(t *testing.T) {
	f := newTypeScopeFixture(t)
	file := f.file(t, "app/widget.py", "python")
	widget := f.class(t, file, "Widget", "widget.Widget", "python")
	make := f.symbolKind(t, file, "make", "widget.make", "function", "python")
	f.edge(t, file, make, "Widget")

	helpers := f.file(t, "app/helpers.py", "python")
	f.symbolKind(t, helpers, "normalize", "helpers.normalize", "function", "python")
	caller := f.file(t, "app/caller.py", "python")
	go1 := f.symbolKind(t, caller, "go", "caller.go", "function", "python")
	f.edge(t, caller, go1, "normalize")

	// An unresolved bare spelling of a FUNCTION: the name leg must still serve
	// it, so the gate cannot be mistaken for a general bare-name narrowing.
	orphan := f.file(t, "app/orphan.py", "python")
	orphanFn := f.symbolKind(t, orphan, "orphan", "orphan.orphan", "function", "python")
	f.edge(t, orphan, orphanFn, "normalize_unbound")
	f.symbolKind(t, helpers, "normalize_unbound", "helpers.other.normalize_unbound", "function", "python")

	if _, err := f.store.ResolveEdges(f.ctx, f.repoID); err != nil {
		t.Fatalf("ResolveEdges() error = %v", err)
	}

	callers, err := f.store.FindCallers(f.ctx, f.repoID, "widget.Widget", 0, 20, 0)
	if err != nil {
		t.Fatalf("FindCallers(widget.Widget) error = %v", err)
	}
	if !containsSymbolID(callers, make) {
		t.Fatalf("FindCallers(widget.Widget) lost the local constructor caller: %+v", callers)
	}
	callees, err := f.store.FindCallees(f.ctx, f.repoID, "widget.make", 0, 20, 0)
	if err != nil {
		t.Fatalf("FindCallees(widget.make) error = %v", err)
	}
	if !containsSymbolID(callees, widget) {
		t.Fatalf("FindCallees(widget.make) lost class widget.Widget: %+v", callees)
	}
	fnCallers, err := f.store.FindCallers(f.ctx, f.repoID, "helpers.normalize", 0, 20, 0)
	if err != nil {
		t.Fatalf("FindCallers(helpers.normalize) error = %v", err)
	}
	if !containsSymbolID(fnCallers, go1) {
		t.Fatalf("FindCallers(helpers.normalize) lost a function caller: %+v", fnCallers)
	}
	unboundCallers, err := f.store.FindCallers(f.ctx, f.repoID, "normalize_unbound", 0, 20, 0)
	if err != nil {
		t.Fatalf("FindCallers(normalize_unbound) error = %v", err)
	}
	if !containsSymbolID(unboundCallers, orphanFn) {
		t.Fatalf("FindCallers(normalize_unbound) lost the unresolved bare-name leg for a function: %+v", unboundCallers)
	}
}

func containsSymbolID(symbols []graph.Symbol, id int64) bool {
	for _, sym := range symbols {
		if sym.ID == id {
			return true
		}
	}
	return false
}

// TestTypeScopeLifecycle walks the histories §31 asks for on one graph: the
// caller arrives before the class, the class arrives, a second declaration
// makes the name ambiguous, and the extra declaration goes away again. The
// answer at each point must be the one a fresh index of the same tree gives,
// which is what makes indexing history not evidence (P22.8).
func TestTypeScopeLifecycle(t *testing.T) {
	f := newTypeScopeFixture(t)
	callFile := f.file(t, "app/caller.py", "python")
	f.importPath(t, callFile, "app.models")
	caller := f.symbolKind(t, callFile, "go", "caller.go", "function", "python")
	edgeID := f.edge(t, callFile, caller, "Client")

	resolve := func(t *testing.T, names ...string) {
		t.Helper()
		if _, err := f.store.ResolveEdgesForNamesWithStats(f.ctx, f.repoID, names); err != nil {
			t.Fatalf("ResolveEdgesForNamesWithStats(%v) error = %v", names, err)
		}
	}
	assertUnresolved := func(t *testing.T, stage string) {
		t.Helper()
		if id, ok := f.dstSymbolID(t, edgeID); ok {
			t.Fatalf("%s: edge resolved to %s; want unresolved", stage, f.qualifiedNameOf(t, id))
		}
	}

	// 1. The class does not exist yet.
	resolve(t, "Client")
	assertUnresolved(t, "before the class exists")

	// 2. The imported module declares it: the edge binds.
	modelsFile := f.file(t, "app/models.py", "python")
	client := f.class(t, modelsFile, "Client", "models.Client", "python")
	resolve(t, "Client")
	gotID, ok := f.dstSymbolID(t, edgeID)
	if !ok || gotID != client {
		t.Fatalf("after the class arrives: got %v/%d, want %d", ok, gotID, client)
	}

	// 3. A second same-language declaration arrives elsewhere. The name is now
	// ambiguous at the bare-name level, so P22.8's invalidation has to
	// reconsider the binding it already made rather than leave it standing.
	otherFile := f.file(t, "vendor/other.py", "python")
	f.class(t, otherFile, "Client", "other.Client", "python")
	resolve(t, "Client")
	assertUnresolved(t, "after a second declaration arrives")

	// 4. The extra declaration is removed the way ReplaceFileGraph removes one.
	if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM symbols WHERE file_id = ?`, otherFile); err != nil {
		t.Fatalf("delete symbols error = %v", err)
	}
	resolve(t, "Client")
	gotID, ok = f.dstSymbolID(t, edgeID)
	if !ok || gotID != client {
		t.Fatalf("after the ambiguity is removed: got %v/%d, want %d", ok, gotID, client)
	}

	// 5. The declaring file itself goes away: the destination must go with it.
	if _, err := f.store.db.ExecContext(f.ctx,
		`UPDATE edges SET dst_symbol_id = NULL, resolution_strategy = '', resolution_confidence = ''
		 WHERE dst_symbol_id IN (SELECT id FROM symbols WHERE file_id = ?)`, modelsFile); err != nil {
		t.Fatalf("unbind error = %v", err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM symbols WHERE file_id = ?`, modelsFile); err != nil {
		t.Fatalf("delete symbols error = %v", err)
	}
	resolve(t, "Client")
	assertUnresolved(t, "after the declaring file is removed")
}

// TestTypeScopeGatedLanguagesSQLMatchesGoTwin keeps the two language sets from
// drifting. A language present in one but not the other would make the
// repo-wide resolver and the Go-side binder disagree about whether a whole
// language is governed, which is a parity break, not a narrowing.
func TestTypeScopeGatedLanguagesSQLMatchesGoTwin(t *testing.T) {
	f := newTypeScopeFixture(t)
	values := ""
	for language := range typeScopeGatedLanguages {
		if values != "" {
			values += ","
		}
		values += "('" + language + "')"
	}
	rows, err := f.store.db.QueryContext(context.Background(),
		`WITH langs(l) AS (VALUES `+values+`) SELECT l FROM langs WHERE l IN `+typeScopeGatedLanguagesSQL)
	if err != nil {
		t.Fatalf("query error = %v", err)
	}
	defer rows.Close()
	inSQL := 0
	for rows.Next() {
		inSQL++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows error = %v", err)
	}
	if inSQL != len(typeScopeGatedLanguages) {
		t.Fatalf("SQL gated-language set has %d of the Go twin's %d members", inSQL, len(typeScopeGatedLanguages))
	}
	for _, language := range []string{"kotlin", "swift", "java", "csharp", "php", "ruby", "rust", "go", ""} {
		if typeScopeGatedLanguage(language) {
			t.Fatalf("language %q must not be governed by the type-scope rule", language)
		}
	}
}

// TestTypeScopeAmbiguityLosesNameEvidence records a deliberate query-side
// consequence rather than discovering it later as a bug report.
//
// `app/a.py` declares `class Widget` and constructs it, so the relation is real
// and same-file. But `app/b.py` declares a second `class Widget`, which makes
// the bare name ambiguous (P3) and leaves the edge unresolved -- and with the
// edge unresolved, the query surfaces' bare-name legs are the only thing that
// could report it, which P22.9 closes for type targets. The caller therefore
// disappears from find_callers. That is the same trade P3 already makes in the
// graph, now applied consistently to the name evidence.
func TestTypeScopeAmbiguityLosesNameEvidence(t *testing.T) {
	f := newTypeScopeFixture(t)
	aFile := f.file(t, "app/a.py", "python")
	f.class(t, aFile, "Widget", "a.Widget", "python")
	make := f.symbolKind(t, aFile, "make", "a.make", "function", "python")
	edgeID := f.edge(t, aFile, make, "Widget")
	bFile := f.file(t, "app/b.py", "python")
	f.class(t, bFile, "Widget", "b.Widget", "python")

	if _, err := f.store.ResolveEdges(f.ctx, f.repoID); err != nil {
		t.Fatalf("ResolveEdges() error = %v", err)
	}
	if gotID, resolved := f.dstSymbolID(t, edgeID); resolved {
		t.Fatalf("edge resolved to %s; two same-language declarations are ambiguous (P3)",
			f.qualifiedNameOf(t, gotID))
	}
	callers, err := f.store.FindCallers(f.ctx, f.repoID, "a.Widget", 0, 20, 0)
	if err != nil {
		t.Fatalf("FindCallers(a.Widget) error = %v", err)
	}
	if containsSymbolID(callers, make) {
		t.Fatalf("FindCallers(a.Widget) reported a caller the graph refuses to bind: %+v", callers)
	}
}

// TestTypeScopeImportChangeInAnotherFileInvalidates is the P22.8 invariant
// applied to the evidence P22.9 introduced.
//
// `app/caller.py` imports the package `app.pkg` and calls `Layer()`; the class
// lives in `app/pkg/tcp.py` and is reachable only because `app/pkg/__init__.py`
// re-exports it with a relative import. Editing ONLY the barrel must change the
// caller's answer, in both directions, and must land where a fresh index of the
// same tree lands. Before typeScopeNamesForChangedPaths the batch carried no
// names at all for such an edit -- `__init__.py` declares no symbols -- so the
// stale binding survived the removal of its only evidence.
func TestTypeScopeImportChangeInAnotherFileInvalidates(t *testing.T) {
	f := newTypeScopeFixture(t)
	tcpFile := f.file(t, "app/pkg/tcp.py", "python")
	layer := f.class(t, tcpFile, "Layer", "tcp.Layer", "python")
	barrel := f.file(t, "app/pkg/__init__.py", "python")
	f.importPath(t, barrel, ".tcp")
	callFile := f.file(t, "app/caller.py", "python")
	f.importPath(t, callFile, "app.pkg")
	caller := f.symbolKind(t, callFile, "go", "caller.go", "function", "python")
	edgeID := f.edge(t, callFile, caller, "Layer")

	// Fresh index of this tree binds through the hop.
	if _, err := f.store.ResolveEdges(f.ctx, f.repoID); err != nil {
		t.Fatalf("ResolveEdges() error = %v", err)
	}
	if got, ok := f.dstSymbolID(t, edgeID); !ok || got != layer {
		t.Fatalf("initial resolve: got %v/%d, want %d", ok, got, layer)
	}

	// The barrel stops re-exporting. Only that file changed, and it declares no
	// symbols, so the batch's own name set is empty.
	if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM file_imports WHERE file_id = ?`, barrel); err != nil {
		t.Fatalf("delete file_imports error = %v", err)
	}
	if _, err := f.store.ResolveEdgesForPathsAndNames(f.ctx, f.repoID,
		[]string{"app/pkg/__init__.py"}, nil); err != nil {
		t.Fatalf("ResolveEdgesForPathsAndNames() error = %v", err)
	}
	if got, ok := f.dstSymbolID(t, edgeID); ok {
		t.Fatalf("binding survived removal of its only evidence: still bound to %s",
			f.qualifiedNameOf(t, got))
	}

	// Restoring the re-export must bind it again, from the same incremental
	// entrypoint, so the direction is not one-way.
	f.importPath(t, barrel, ".tcp")
	if _, err := f.store.ResolveEdgesForPathsAndNames(f.ctx, f.repoID,
		[]string{"app/pkg/__init__.py"}, nil); err != nil {
		t.Fatalf("ResolveEdgesForPathsAndNames() error = %v", err)
	}
	if got, ok := f.dstSymbolID(t, edgeID); !ok || got != layer {
		t.Fatalf("after restoring the re-export: got %v/%d, want %d", ok, got, layer)
	}
}

// TestTypeScopeRepairsExistingDatabase pins the upgrade path. A database
// written before this rule existed holds bindings the rule refuses, and no
// resolver strategy reconsiders them -- every strategy matches
// `dst_symbol_id IS NULL`. The repo-wide resolver therefore runs the rule's own
// negation first, so the next index converges instead of leaving the graph
// asserting a relationship its own resolver would refuse.
func TestTypeScopeRepairsExistingDatabase(t *testing.T) {
	f := newTypeScopeFixture(t)
	defFile := f.file(t, "app/types.py", "python")
	projectPath := f.class(t, defFile, "Path", "types.Path", "python")
	callFile := f.file(t, "app/addons/maplocal.py", "python")
	f.importPath(t, callFile, "pathlib")
	load := f.symbolKind(t, callFile, "load", "maplocal.load", "function", "python")
	edgeID := f.edge(t, callFile, load, "Path")

	// The pre-P22.9 state: bound, with the strategy an older release wrote.
	if _, err := f.store.db.ExecContext(f.ctx, `
		UPDATE edges SET dst_symbol_id = ?, resolution_strategy = ?, resolution_confidence = ?
		WHERE id = ?`, projectPath, ResolutionStrategyExactName, ResolutionConfidenceHigh, edgeID); err != nil {
		t.Fatalf("seed stale binding error = %v", err)
	}

	if _, err := f.store.ResolveEdges(f.ctx, f.repoID); err != nil {
		t.Fatalf("ResolveEdges() error = %v", err)
	}
	if got, ok := f.dstSymbolID(t, edgeID); ok {
		t.Fatalf("stale binding survived a full resolve: still bound to %s", f.qualifiedNameOf(t, got))
	}

	// Idempotent, and it does not touch a binding the rule allows: the same pass
	// over a legitimate same-file constructor leaves it alone.
	localFile := f.file(t, "app/widget.py", "python")
	widget := f.class(t, localFile, "Widget", "widget.Widget", "python")
	make := f.symbolKind(t, localFile, "make", "widget.make", "function", "python")
	localEdge := f.edge(t, localFile, make, "Widget")
	for range 2 {
		if _, err := f.store.ResolveEdges(f.ctx, f.repoID); err != nil {
			t.Fatalf("ResolveEdges() error = %v", err)
		}
	}
	if got, ok := f.dstSymbolID(t, localEdge); !ok || got != widget {
		t.Fatalf("repair cleared a binding the rule allows: got %v/%d, want %d", ok, got, widget)
	}
	if got, ok := f.dstSymbolID(t, edgeID); ok {
		t.Fatalf("stale binding came back: %s", f.qualifiedNameOf(t, got))
	}
}

// TestTypeScopeBareQualifiedNameSeedIsGated covers the seed spelling a C++
// top-level type actually has. `class Message` at file scope gets
// `qualified_name = "Message"` -- bare -- so context expansion's QUALIFIED-name
// leg carries it, not the short-name leg, and guarding only the short leg left
// the refused relation answerable through context_for_task.
func TestTypeScopeBareQualifiedNameSeedIsGated(t *testing.T) {
	f := newTypeScopeFixture(t)
	defFile := f.file(t, "googlemock/test/utils_test.cc", "cpp")
	message := f.symbolKind(t, defFile, "Message", "Message", "class", "cpp")
	callFile := f.file(t, "googletest/test/message_test.cc", "cpp")
	f.importPath(t, callFile, "gtest/gtest.h")
	run := f.symbolKind(t, callFile, "run", "message_test::run", "function", "cpp")
	edgeID := f.edge(t, callFile, run, "Message")

	if _, err := f.store.ResolveEdges(f.ctx, f.repoID); err != nil {
		t.Fatalf("ResolveEdges() error = %v", err)
	}
	if got, ok := f.dstSymbolID(t, edgeID); ok {
		t.Fatalf("resolver bound the edge to %s; want unresolved", f.qualifiedNameOf(t, got))
	}
	neighbors, err := f.store.FindContextNeighbors(f.ctx, f.repoID, []ContextSeed{
		// The seed a real context_for_task builds: QualifiedName == ShortName,
		// so the short leg is skipped entirely and only the qualified leg runs.
		{SymbolID: message, QualifiedName: "Message", ShortName: "Message", AllowShortEvidence: true},
	}, 20)
	if err != nil {
		t.Fatalf("FindContextNeighbors() error = %v", err)
	}
	if containsSymbolID(neighbors[0].Callers, run) {
		t.Fatalf("context expansion reported a caller of class Message that the resolver refused")
	}
}

// TestTypeScopeRepairClearsNonRedecidableStrategies covers a binding the
// incremental restriction deliberately spares but the repair must not.
//
// A nested `class Outer.Path` has both a name and a container, so
// `receiver_method` binds it -- a strategy P22.8 keeps an incremental pass from
// clearing, because an incremental pass could not rebuild it. The repo-wide
// repair has the opposite premise: this rule refuses the binding, so nothing
// should rebuild it, and sparing it would mark a repository repaired while it
// still asserts the relation.
func TestTypeScopeRepairClearsNonRedecidableStrategies(t *testing.T) {
	f := newTypeScopeFixture(t)
	defFile := f.file(t, "app/types.py", "python")
	nested, err := insertTestSymbolKind(f.ctx, f.store, f.repoID, defFile,
		"Path", "types.Outer.Path", "class", "Outer", "python")
	if err != nil {
		t.Fatalf("insertTestSymbolKind() error = %v", err)
	}
	callFile := f.file(t, "app/addons/maplocal.py", "python")
	f.importPath(t, callFile, "pathlib")
	load := f.symbolKind(t, callFile, "load", "maplocal.load", "function", "python")
	edgeID := f.edge(t, callFile, load, "Path")
	if _, err := f.store.db.ExecContext(f.ctx, `
		UPDATE edges SET dst_symbol_id = ?, resolution_strategy = ?, resolution_confidence = ?
		WHERE id = ?`, nested, ResolutionStrategyReceiverMethod, ResolutionConfidenceMedium, edgeID); err != nil {
		t.Fatalf("seed stale binding error = %v", err)
	}

	if _, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID); err != nil {
		t.Fatalf("RepairResolverBindingsOnce() error = %v", err)
	}
	if got, ok := f.dstSymbolID(t, edgeID); ok {
		t.Fatalf("repair spared a receiver_method binding the rule refuses: %s", f.qualifiedNameOf(t, got))
	}
}

// TestTypeScopeRepairSparesCrossLanguageLinks pins the one exemption. P19b
// inserts `cross_language_ref` edges already bound from import-bridge evidence
// and no resolver strategy considers them, so clearing one would delete a
// destination nothing rebuilds.
func TestTypeScopeRepairSparesCrossLanguageLinks(t *testing.T) {
	f := newTypeScopeFixture(t)
	tsFile := f.file(t, "web/model.ts", "typescript")
	model := f.class(t, tsFile, "Model", "model.Model", "typescript")
	pyFile := f.file(t, "app/caller.py", "python")
	caller := f.symbolKind(t, pyFile, "go", "caller.go", "function", "python")
	var edgeID int64
	if err := f.store.db.QueryRowContext(f.ctx, `
		INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line)
		VALUES(?, ?, ?, 'Model', ?, '', ?, 1) RETURNING id`,
		f.repoID, caller, model, EdgeKindCrossLanguageRef, pyFile).Scan(&edgeID); err != nil {
		t.Fatalf("insert cross-language edge error = %v", err)
	}

	if _, err := f.store.RepairResolverBindingsOnce(f.ctx, f.repoID); err != nil {
		t.Fatalf("RepairResolverBindingsOnce() error = %v", err)
	}
	if _, ok := f.dstSymbolID(t, edgeID); !ok {
		t.Fatalf("repair cleared an explicit cross_language_ref link")
	}
}

// TestTypeScopeUngatedFileOnTheHopInvalidates covers the early exit. The import
// index is language-blind and the re-export hop follows any file's relative
// specifiers, so a file in an ungated language can sit ON the hop: here a Ruby
// `pkg.rb` is what makes `tcp.py` visible to a Python caller. Testing the
// CHANGED file's own language would skip the pass for an edit to it.
func TestTypeScopeUngatedFileOnTheHopInvalidates(t *testing.T) {
	f := newTypeScopeFixture(t)
	tcpFile := f.file(t, "app/tcp.py", "python")
	layer := f.class(t, tcpFile, "Layer", "tcp.Layer", "python")
	bridge := f.file(t, "app/pkg.rb", "ruby")
	f.importPath(t, bridge, "./tcp")
	callFile := f.file(t, "app/caller.py", "python")
	f.importPath(t, callFile, "app.pkg")
	caller := f.symbolKind(t, callFile, "go", "caller.go", "function", "python")
	edgeID := f.edge(t, callFile, caller, "Layer")

	if _, err := f.store.ResolveEdges(f.ctx, f.repoID); err != nil {
		t.Fatalf("ResolveEdges() error = %v", err)
	}
	if got, ok := f.dstSymbolID(t, edgeID); !ok || got != layer {
		t.Fatalf("initial resolve: got %v/%d, want %d", ok, got, layer)
	}

	if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM file_imports WHERE file_id = ?`, bridge); err != nil {
		t.Fatalf("delete file_imports error = %v", err)
	}
	if _, err := f.store.ResolveEdgesForPathsAndNames(f.ctx, f.repoID, []string{"app/pkg.rb"}, nil); err != nil {
		t.Fatalf("ResolveEdgesForPathsAndNames() error = %v", err)
	}
	if got, ok := f.dstSymbolID(t, edgeID); ok {
		t.Fatalf("editing the ungated file on the hop left the binding standing: %s",
			f.qualifiedNameOf(t, got))
	}
}

// TestTypeScopeDeletedBarrelInvalidates covers the strongest import-list edit:
// deleting the file. A deleted file leaves the import index, so no specifier
// resolves to it and "the files that can see it" finds nobody -- which is why
// the indexer runs the repo-wide revalidation after a purge instead.
func TestTypeScopeDeletedBarrelInvalidates(t *testing.T) {
	f := newTypeScopeFixture(t)
	tcpFile := f.file(t, "app/pkg/tcp.py", "python")
	layer := f.class(t, tcpFile, "Layer", "tcp.Layer", "python")
	barrel := f.file(t, "app/pkg/__init__.py", "python")
	f.importPath(t, barrel, ".tcp")
	callFile := f.file(t, "app/caller.py", "python")
	f.importPath(t, callFile, "app.pkg")
	caller := f.symbolKind(t, callFile, "go", "caller.go", "function", "python")
	edgeID := f.edge(t, callFile, caller, "Layer")

	if _, err := f.store.ResolveEdges(f.ctx, f.repoID); err != nil {
		t.Fatalf("ResolveEdges() error = %v", err)
	}
	if got, ok := f.dstSymbolID(t, edgeID); !ok || got != layer {
		t.Fatalf("initial resolve: got %v/%d, want %d", ok, got, layer)
	}

	// What the indexer's delete + purge leaves behind.
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE files SET is_deleted = 1 WHERE id = ?`, barrel); err != nil {
		t.Fatalf("mark deleted error = %v", err)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM file_imports WHERE file_id = ?`, barrel); err != nil {
		t.Fatalf("purge imports error = %v", err)
	}
	if err := f.store.RevalidateTypeScopeAfterDeletion(f.ctx, f.repoID); err != nil {
		t.Fatalf("RevalidateTypeScopeAfterDeletion() error = %v", err)
	}
	if got, ok := f.dstSymbolID(t, edgeID); ok {
		t.Fatalf("deleting the barrel left the binding standing: %s", f.qualifiedNameOf(t, got))
	}
}

// TestTypeScopeQueryGateIsPerLanguage pins the mixed case. One bare name that
// matches a Python class and a Kotlin class must lose the Python writers and
// keep the Kotlin ones: Kotlin resolves a bare class name across files with no
// import, Python does not.
func TestTypeScopeQueryGateIsPerLanguage(t *testing.T) {
	f := newTypeScopeFixture(t)
	pyDef := f.file(t, "app/types.py", "python")
	f.class(t, pyDef, "Foo", "types.Foo", "python")
	pyCaller := f.file(t, "app/caller.py", "python")
	f.importPath(t, pyCaller, "pathlib")
	pyWriter := f.symbolKind(t, pyCaller, "go", "caller.go", "function", "python")
	f.edge(t, pyCaller, pyWriter, "Foo")

	ktDef := f.file(t, "app/Foo.kt", "kotlin")
	f.class(t, ktDef, "Foo", "Foo.Foo", "kotlin")
	ktCaller := f.file(t, "app/Use.kt", "kotlin")
	ktWriter := f.symbolKind(t, ktCaller, "use", "Use.use", "function", "kotlin")
	f.edge(t, ktCaller, ktWriter, "Foo")
	// A second Kotlin declaration makes the name ambiguous there, so the Kotlin
	// edge stays unresolved and only the name leg can report it.
	other := f.file(t, "app/Other.kt", "kotlin")
	f.class(t, other, "Foo", "Other.Foo", "kotlin")

	if _, err := f.store.ResolveEdges(f.ctx, f.repoID); err != nil {
		t.Fatalf("ResolveEdges() error = %v", err)
	}
	callers, err := f.store.FindCallers(f.ctx, f.repoID, "Foo", 0, 20, 0)
	if err != nil {
		t.Fatalf("FindCallers(Foo) error = %v", err)
	}
	if containsSymbolID(callers, pyWriter) {
		t.Fatalf("FindCallers(Foo) kept the python writer the resolver refused: %+v", callers)
	}
}

// TestBareNameScopeAllKindsSQLMatchesGoTwin pins bareNameScopeAllKindsSQL
// against bareNameScopeAllKindsLanguages, and pins the kind predicate the two
// rules share: in C and C++ every kind is scoped, in Python and TypeScript only
// the type-denoting ones, and nowhere else at all.
func TestBareNameScopeAllKindsSQLMatchesGoTwin(t *testing.T) {
	f := newTypeScopeFixture(t)
	values := ""
	for language := range bareNameScopeAllKindsLanguages {
		if values != "" {
			values += ","
		}
		values += "('" + language + "')"
	}
	rows, err := f.store.db.QueryContext(context.Background(),
		`WITH langs(l) AS (VALUES `+values+`) SELECT l FROM langs WHERE l IN `+bareNameScopeAllKindsSQL)
	if err != nil {
		t.Fatalf("query error = %v", err)
	}
	defer rows.Close()
	inSQL := 0
	for rows.Next() {
		inSQL++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows error = %v", err)
	}
	if inSQL != len(bareNameScopeAllKindsLanguages) {
		t.Fatalf("SQL all-kinds set has %d of the Go twin's %d members", inSQL, len(bareNameScopeAllKindsLanguages))
	}

	for _, tc := range []struct {
		language, kind string
		want           bool
	}{
		{"cpp", "function", true},
		{"cpp", "method", true},
		{"cpp", "class", true},
		{"cpp", "variable", true},
		{"python", "class", true},
		{"python", "function", false},
		{"typescript", "interface", true},
		{"typescript", "function", false},
		{"go", "function", false},
		{"go", "class", false},
		{"kotlin", "class", false},
		{"", "function", false},
	} {
		if got := bareNameScopeCoversKind(tc.language, tc.kind); got != tc.want {
			t.Fatalf("bareNameScopeCoversKind(%q, %q) = %v, want %v", tc.language, tc.kind, got, tc.want)
		}
	}
}

// TestBareScopeKeySQLMatchesGoTwin pins sqlBareScopeKey against bareScopeKey on
// the shapes both sides have to agree about: a Go package, a C/C++ file, an
// ungated language, and the abstentions.
func TestBareScopeKeySQLMatchesGoTwin(t *testing.T) {
	f := newTypeScopeFixture(t)
	cases := []struct{ path, qname, language string }{
		{"internal/store/store.go", "store.Open", "go"},
		{"store.go", "store.Open", "go"},
		{"internal/store/store.go", "Open", "go"},
		{"src/gtest.cc", "gtest::ShouldUseColor", "cpp"},
		{"only.c", "only::helper", "cpp"},
		{"", "only::helper", "cpp"},
		{"app/models.py", "models.Thing", "python"},
		{"A.kt", "A", "kotlin"},
		{"x.go", "store.Open", ""},
	}
	for _, tc := range cases {
		var got string
		if err := f.store.db.QueryRowContext(context.Background(),
			`SELECT `+sqlBareScopeKey(":path", ":qname", ":lang"),
			sql.Named("path", tc.path), sql.Named("qname", tc.qname),
			sql.Named("lang", tc.language)).Scan(&got); err != nil {
			t.Fatalf("sqlBareScopeKey(%q, %q, %q) error = %v", tc.path, tc.qname, tc.language, err)
		}
		if want := bareScopeKey(tc.language, tc.path, tc.qname); got != want {
			t.Fatalf("sqlBareScopeKey(%q, %q, %q) = %q, Go twin = %q",
				tc.path, tc.qname, tc.language, got, want)
		}
	}
}
