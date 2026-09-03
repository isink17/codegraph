package store

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Regression coverage for P22.8 full-index / incremental resolution parity.
//
// The core invariant: indexing history is never semantic evidence. The same
// final semantic facts must produce the same destination, strategy and
// confidence, whichever pipeline reached them and in whatever order files
// arrived.
//
// Three defect classes are pinned here, each reproduced from a real repository
// before the fix:
//
//	A. an import-path spelling (`database/sql.Open`) degraded to its bare tail
//	   in the incremental binder and bound an unrelated project `Open`. The
//	   repo-wide resolver has no such stage and abstained.
//	B. the binder's bare-name fallback used a wider candidate set than the
//	   repo-wide bare-name strategy (no symbol-kind restriction) and reported a
//	   different strategy for the same evidence.
//	C. a binding made while a name was unique survived the arrival of a second
//	   declaration, because an edge is only ever unbound when its destination
//	   disappears.

// parityFixture is a repository with a go.mod, so own-module import evidence is
// available to the resolver.
type parityFixture struct {
	ctx    context.Context
	store  *Store
	repoID int64
	root   string
}

func newParityFixture(t *testing.T, moduleLine string) *parityFixture {
	t.Helper()
	root := t.TempDir()
	if moduleLine != "" {
		if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte(moduleLine), 0o644); err != nil {
			t.Fatalf("write go.mod: %v", err)
		}
	}
	s, err := OpenWithOptions(filepath.Join(t.TempDir(), "graph.sqlite"), OpenOptions{PerformanceProfile: "fast"})
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	repo, err := s.UpsertRepo(ctx, root)
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	return &parityFixture{ctx: ctx, store: s, repoID: repo.ID, root: root}
}

func (f *parityFixture) file(t *testing.T, path, language string) int64 {
	t.Helper()
	id, err := insertTestFileLang(f.ctx, f.store, f.repoID, path, language)
	if err != nil {
		t.Fatalf("insertTestFileLang(%s): %v", path, err)
	}
	return id
}

func (f *parityFixture) symbol(t *testing.T, fileID int64, name, qualified, kind, language string) int64 {
	t.Helper()
	return f.symbolIn(t, fileID, name, qualified, kind, "", language)
}

// symbolIn declares a symbol with an explicit container, which own-module
// import evidence requires: a package-level Go declaration is spelled
// `<package>.<name>` with the package as its container.
func (f *parityFixture) symbolIn(t *testing.T, fileID int64, name, qualified, kind, container, language string) int64 {
	t.Helper()
	id, err := insertTestSymbolKind(f.ctx, f.store, f.repoID, fileID, name, qualified, kind, container, language)
	if err != nil {
		t.Fatalf("insertTestSymbolKind(%s): %v", qualified, err)
	}
	return id
}

func (f *parityFixture) imports(t *testing.T, fileID int64, importPath string) {
	t.Helper()
	if _, err := f.store.db.ExecContext(f.ctx,
		`INSERT INTO file_imports(repo_id, file_id, import_path) VALUES (?, ?, ?)`,
		f.repoID, fileID, importPath); err != nil {
		t.Fatalf("insert file_import(%s): %v", importPath, err)
	}
}

func (f *parityFixture) edge(t *testing.T, fileID, srcSymbolID int64, dstName string) int64 {
	t.Helper()
	id, err := insertTestEdge(f.ctx, f.store, f.repoID, fileID, srcSymbolID, dstName)
	if err != nil {
		t.Fatalf("insertTestEdge(%s): %v", dstName, err)
	}
	return id
}

// binding renders an edge's resolution semantically: destination qualified name
// plus the provenance, never a row id.
func (f *parityFixture) binding(t *testing.T, edgeID int64) string {
	t.Helper()
	var qname sql.NullString
	var strategy, confidence string
	if err := f.store.db.QueryRowContext(f.ctx, `
		SELECT d.qualified_name, e.resolution_strategy, e.resolution_confidence
		FROM edges e LEFT JOIN symbols d ON d.id = e.dst_symbol_id
		WHERE e.id = ?`, edgeID).Scan(&qname, &strategy, &confidence); err != nil {
		t.Fatalf("read binding: %v", err)
	}
	if !qname.Valid {
		return "<unresolved>"
	}
	return qname.String + "|" + strategy + "|" + confidence
}

func (f *parityFixture) clearAll(t *testing.T) {
	t.Helper()
	if err := f.store.ClearEdgeResolutionsForTest(f.ctx, f.repoID); err != nil {
		t.Fatalf("clear resolutions: %v", err)
	}
}

// resolveVia runs one resolver entry point over the whole fixture.
func (f *parityFixture) resolveVia(t *testing.T, entry string, paths, names []string) {
	t.Helper()
	var err error
	switch entry {
	case "full":
		_, err = f.store.ResolveEdges(f.ctx, f.repoID)
	case "paths":
		err = f.store.ResolveEdgesForPaths(f.ctx, f.repoID, paths)
	case "names":
		_, err = f.store.ResolveEdgesForNames(f.ctx, f.repoID, names)
	case "paths+names":
		_, err = f.store.ResolveEdgesForPathsAndNames(f.ctx, f.repoID, paths, names)
	default:
		t.Fatalf("unknown entry point %q", entry)
	}
	if err != nil {
		t.Fatalf("resolve via %s: %v", entry, err)
	}
}

// -- A. import-path spellings -------------------------------------------------

// A Go source importing the standard library and calling through it must never
// bind a project symbol that merely shares the call's tail, on ANY entry point.
// This is the `database/sql.Open` -> `store.Open` bind the incremental binder
// produced on CodeGraph itself.
func TestImportPathSpellingNeverBindsProjectTail(t *testing.T) {
	for _, tc := range []struct {
		name       string
		importPath string
		dstName    string
	}{
		{"stdlib", "database/sql", "database/sql.Open"},
		{"external", "github.com/acme/lib", "github.com/acme/lib.Open"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, entry := range []string{"full", "paths", "names", "paths+names"} {
				t.Run(entry, func(t *testing.T) {
					f := newParityFixture(t, "module example.com/project\n")
					storeFile := f.file(t, "internal/store/store.go", "go")
					// The project's own, unique, unrelated Open.
					f.symbol(t, storeFile, "Open", "store.Open", "function", "go")

					callerFile := f.file(t, "internal/doctor/doctor.go", "go")
					caller := f.symbol(t, callerFile, "inspectDB", "doctor.inspectDB", "function", "go")
					f.imports(t, callerFile, tc.importPath)
					edge := f.edge(t, callerFile, caller, tc.dstName)

					f.resolveVia(t, entry, []string{"internal/doctor/doctor.go"}, []string{"Open"})
					if got := f.binding(t, edge); got != "<unresolved>" {
						t.Fatalf("%s: %s bound %s; an import outside the module names nothing in it",
							entry, tc.dstName, got)
					}
				})
			}
		})
	}
}

// A template-like C++ spelling is valid evidence for a type-like target even
// though punctuation makes it unlike a bare identifier. Scoped resolution must
// route it through the same C++ evidence binder as a full resolve.
func TestCppTemplateLikeSpellingParityAcrossEntryPoints(t *testing.T) {
	const spelling = "Action<R(Args...)>"
	entries := []string{"full", "paths", "names", "paths+names"}
	var want string
	for _, entry := range entries {
		t.Run(entry, func(t *testing.T) {
			f := newCppClassFixture(t)
			file := f.file(t, "action.cpp", "cpp")
			dst := f.declare(t, file, spelling, "ns::"+spelling, "class", "ns", 1, 4)
			caller := f.declare(t, file, "caller", "ns::caller", "function", "ns", 5, 8)
			edge := f.edge(t, file, caller, spelling)
			var err error
			switch entry {
			case "full":
				_, err = f.store.ResolveEdges(f.ctx, f.repoID)
			case "paths":
				err = f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"action.cpp"})
			case "names":
				_, err = f.store.ResolveEdgesForNames(f.ctx, f.repoID, []string{spelling})
			case "paths+names":
				_, err = f.store.ResolveEdgesForPathsAndNames(f.ctx, f.repoID, []string{"action.cpp"}, []string{spelling})
			}
			if err != nil {
				t.Fatalf("%s failed: %v", entry, err)
			}
			gotID, resolved := f.dstSymbolID(t, edge)
			if !resolved {
				t.Fatalf("%s failed to resolve template-like C++ evidence", entry)
			}
			strategy, confidence := f.resolutionMetadata(t, edge)
			got := f.qualifiedNameOf(t, gotID) + "|" + strategy + "|" + confidence
			if want == "" {
				want = got
			} else if got != want {
				t.Fatalf("%s produced %q, want %q", entry, got, want)
			}
			if gotID != dst {
				t.Fatalf("%s resolved to %s, want %s", entry, f.qualifiedNameOf(t, gotID), f.qualifiedNameOf(t, dst))
			}
		})
	}
}

// The same spelling shape, but the import IS the module's own package: P22.5's
// evidence must keep resolving it, identically on every entry point.
func TestOwnModuleImportResolvesOnEveryEntryPoint(t *testing.T) {
	for _, entry := range []string{"full", "paths", "names", "paths+names"} {
		t.Run(entry, func(t *testing.T) {
			f := newParityFixture(t, "module example.com/project\n")
			pkgFile := f.file(t, "pkg/open.go", "go")
			f.symbolIn(t, pkgFile, "Open", "pkg.Open", "function", "pkg", "go")
			// A decoy in another package, to prove the package evidence and not
			// repo-wide uniqueness is what selects the target.
			otherFile := f.file(t, "other/open.go", "go")
			f.symbolIn(t, otherFile, "Open", "other.Open", "function", "other", "go")

			callerFile := f.file(t, "cmd/main.go", "go")
			caller := f.symbol(t, callerFile, "main", "main", "function", "go")
			f.imports(t, callerFile, "example.com/project/pkg")
			edge := f.edge(t, callerFile, caller, "example.com/project/pkg.Open")

			f.resolveVia(t, entry, []string{"cmd/main.go"}, []string{"Open"})
			if got, want := f.binding(t, edge), "pkg.Open|module_import|high"; got != want {
				t.Fatalf("%s: own-module import bound %q, want %q", entry, got, want)
			}
		})
	}
}

// A slash-bearing spelling with no import fact behind it proves nothing about
// any project symbol, and must fail closed rather than degrade to its tail.
// Non-Go parsers emit these for ordinary expressions containing '/'.
func TestSlashSpellingWithoutImportFactFailsClosed(t *testing.T) {
	for _, entry := range []string{"full", "paths", "names", "paths+names"} {
		t.Run(entry, func(t *testing.T) {
			f := newParityFixture(t, "")
			libFile := f.file(t, "app/lib.py", "python")
			f.symbol(t, libFile, "open", "lib.open", "function", "python")

			callerFile := f.file(t, "app/main.py", "python")
			caller := f.symbol(t, callerFile, "run", "main.run", "function", "python")
			edge := f.edge(t, callerFile, caller, `(self.dir / "x.curl").open`)

			f.resolveVia(t, entry, []string{"app/main.py"}, []string{"open"})
			if got := f.binding(t, edge); got != "<unresolved>" {
				t.Fatalf("%s: slash spelling with no import evidence bound %s", entry, got)
			}
		})
	}
}

// -- B. one evidence rule per name level --------------------------------------

// The bare-name level must admit exactly the same candidates and report the
// same provenance whichever entry point resolves it. Before P22.8 the binder
// applied no symbol-kind restriction and reported `bare_tail|medium` where the
// repo-wide pass reported `exact_name|high`.
func TestBareNameEvidenceIsIdenticalOnEveryEntryPoint(t *testing.T) {
	build := func(t *testing.T) (*parityFixture, int64) {
		f := newParityFixture(t, "")
		target := f.file(t, "app/helpers.py", "python")
		f.symbol(t, target, "helper", "helpers.helper", "function", "python")
		callerFile := f.file(t, "app/main.py", "python")
		caller := f.symbol(t, callerFile, "run", "main.run", "function", "python")
		return f, f.edge(t, callerFile, caller, "helper")
	}

	var want string
	for _, entry := range []string{"full", "paths", "names", "paths+names"} {
		f, edge := build(t)
		f.resolveVia(t, entry, []string{"app/main.py"}, []string{"helper"})
		got := f.binding(t, edge)
		if want == "" {
			want = got
			if got == "<unresolved>" {
				t.Fatalf("%s: bare name did not resolve at all", entry)
			}
			continue
		}
		if got != want {
			t.Fatalf("%s resolved the same evidence as %q, but another entry point said %q", entry, got, want)
		}
	}
}

// A call edge may only denote a callable kind. The repo-wide pass has always
// restricted bare-name candidates that way; the binder must agree, or the two
// pipelines disagree about whether a name is ambiguous at all.
func TestBareNameCandidateKindsAreIdenticalOnEveryEntryPoint(t *testing.T) {
	for _, entry := range []string{"full", "paths", "names", "paths+names"} {
		t.Run(entry, func(t *testing.T) {
			f := newParityFixture(t, "")
			valueFile := f.file(t, "app/consts.py", "python")
			f.symbol(t, valueFile, "Text", "consts.Text", "value", "python")
			callerFile := f.file(t, "app/main.py", "python")
			caller := f.symbol(t, callerFile, "run", "main.run", "function", "python")
			edge := f.edge(t, callerFile, caller, "Text")

			f.resolveVia(t, entry, []string{"app/main.py"}, []string{"Text"})
			if got := f.binding(t, edge); got != "<unresolved>" {
				t.Fatalf("%s: a call edge bound the non-callable %s", entry, got)
			}
		})
	}
}

// -- C. evidence that stops being unique --------------------------------------

// A binding made while a name was unique must not survive the arrival of a
// second declaration. This is CodeGraph's own build-tagged `isSQLiteBusy` pair
// and mitmproxy's duplicated `meta`.
func TestNewCompetingDeclarationInvalidatesUniquenessBinding(t *testing.T) {
	f := newParityFixture(t, "")
	firstFile := f.file(t, "app/one.py", "python")
	f.symbol(t, firstFile, "helper", "one.helper", "function", "python")
	callerFile := f.file(t, "app/main.py", "python")
	caller := f.symbol(t, callerFile, "run", "main.run", "function", "python")
	edge := f.edge(t, callerFile, caller, "helper")

	f.resolveVia(t, "full", nil, nil)
	if got, want := f.binding(t, edge), "one.helper|exact_name|high"; got != want {
		t.Fatalf("initial binding = %q, want %q", got, want)
	}

	// A second declaration of the same name arrives in a later batch. The name
	// is now ambiguous, so the edge must go back to unresolved.
	secondFile := f.file(t, "app/two.py", "python")
	f.symbol(t, secondFile, "helper", "two.helper", "function", "python")
	f.resolveVia(t, "paths+names", []string{"app/two.py"}, []string{"helper"})
	if got := f.binding(t, edge); got != "<unresolved>" {
		t.Fatalf("binding after a competing declaration arrived = %q, want <unresolved>", got)
	}

	// A fresh resolve of the same final state agrees.
	f.clearAll(t)
	f.resolveVia(t, "full", nil, nil)
	if got := f.binding(t, edge); got != "<unresolved>" {
		t.Fatalf("fresh resolve of the final state = %q, want <unresolved>", got)
	}
}

// The mirror case: an external-import edge must not start binding merely
// because a project symbol with the same tail appears later.
func TestLaterProjectSymbolCannotStealExternalEdge(t *testing.T) {
	f := newParityFixture(t, "module example.com/project\n")
	callerFile := f.file(t, "cmd/main.go", "go")
	caller := f.symbol(t, callerFile, "main", "main", "function", "go")
	f.imports(t, callerFile, "github.com/acme/lib")
	edge := f.edge(t, callerFile, caller, "github.com/acme/lib.Open")

	f.resolveVia(t, "full", nil, nil)
	if got := f.binding(t, edge); got != "<unresolved>" {
		t.Fatalf("external edge bound %q before any project Open existed", got)
	}

	storeFile := f.file(t, "internal/store/store.go", "go")
	f.symbol(t, storeFile, "Open", "store.Open", "function", "go")
	f.resolveVia(t, "paths+names", []string{"internal/store/store.go"}, []string{"Open"})
	if got := f.binding(t, edge); got != "<unresolved>" {
		t.Fatalf("external edge bound %q after a project Open appeared", got)
	}
}

// -- parity across histories --------------------------------------------------

// The same final semantic state, reached by different indexing histories, must
// produce the same bindings. Each history ends with the same three files.
func TestResolutionIsIndependentOfIndexingHistory(t *testing.T) {
	type step struct {
		files []string
		paths []string
		names []string
	}
	histories := map[string][]step{
		"all at once":     {{files: []string{"target", "decoy", "caller"}}},
		"caller first":    {{files: []string{"caller"}}, {files: []string{"target"}, paths: []string{"app/target.py"}, names: []string{"helper"}}, {files: []string{"decoy"}, paths: []string{"app/decoy.py"}, names: []string{"helper"}}},
		"target first":    {{files: []string{"target"}}, {files: []string{"decoy"}, paths: []string{"app/decoy.py"}, names: []string{"helper"}}, {files: []string{"caller"}, paths: []string{"app/main.py"}, names: []string{"run"}}},
		"decoy last":      {{files: []string{"target", "caller"}}, {files: []string{"decoy"}, paths: []string{"app/decoy.py"}, names: []string{"helper"}}},
		"decoy interleav": {{files: []string{"decoy"}}, {files: []string{"caller"}, paths: []string{"app/main.py"}, names: []string{"run"}}, {files: []string{"target"}, paths: []string{"app/target.py"}, names: []string{"helper"}}},
	}

	var want string
	names := make([]string, 0, len(histories))
	for name := range histories {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, hname := range names {
		f := newParityFixture(t, "")
		var edge int64
		add := func(which string) {
			switch which {
			case "target":
				id := f.file(t, "app/target.py", "python")
				f.symbol(t, id, "helper", "target.helper", "function", "python")
			case "decoy":
				id := f.file(t, "app/decoy.py", "python")
				f.symbol(t, id, "helper", "decoy.helper", "function", "python")
			case "caller":
				id := f.file(t, "app/main.py", "python")
				caller := f.symbol(t, id, "run", "main.run", "function", "python")
				edge = f.edge(t, id, caller, "helper")
			}
		}
		for i, st := range histories[hname] {
			for _, which := range st.files {
				add(which)
			}
			if i == 0 {
				f.resolveVia(t, "full", nil, nil)
				continue
			}
			f.resolveVia(t, "paths+names", st.paths, st.names)
		}
		got := f.binding(t, edge)
		if want == "" {
			want = got
			continue
		}
		if got != want {
			t.Fatalf("history %q produced %q; another history produced %q", hname, got, want)
		}
	}
	// Two `helper` declarations exist in the final state, so the honest answer
	// is unresolved -- and every history must reach it.
	if want != "<unresolved>" {
		t.Fatalf("final-state binding = %q, want <unresolved> (two declarations)", want)
	}
}

// Explicit cross-language links are not implicit name evidence: they are bound
// by their own pass from an import bridge, and no name-level resolver strategy
// ever reconsiders them. The invalidation pass shares the name predicate with
// the resolver, so without an explicit guard it would clear a link whose
// dst_name happens to be a name in the batch -- leaving a row the next
// cross-language run would delete as obsolete.
func TestInvalidationLeavesExplicitCrossLanguageLinksAlone(t *testing.T) {
	f := newGateFixture(t)
	spec := crossLangSpec{
		files: []crossLangFile{
			{path: "src/ts/client.ts", language: "typescript", symbols: []crossLangSymbol{{name: "Payload", qualified: "client.Payload"}}},
			{path: "src/shared/model.py", language: "python", symbols: []crossLangSymbol{{name: "Payload", qualified: "model.Payload"}}},
		},
		imports: []crossLangImport{{fromPath: "src/ts/client.ts", path: "src/shared/model.ts"}},
	}
	spec.build(t, f, 1)
	if created := f.resolveCrossLanguage(t); created != 1 {
		t.Fatalf("ResolveCrossLanguageLinks() created %d links, want 1", created)
	}
	before := crossLangLinks(t, f)
	if len(before) != 1 {
		t.Fatalf("expected exactly one cross-language link, got %v", before)
	}

	// `Payload` is exactly the kind of name an incremental batch reports.
	if _, err := f.store.ResolveEdgesForPathsAndNames(f.ctx, f.repoID,
		[]string{"src/ts/client.ts"}, []string{"Payload"}); err != nil {
		t.Fatalf("ResolveEdgesForPathsAndNames() error = %v", err)
	}
	after := crossLangLinks(t, f)
	if len(after) != 1 || after[0] != before[0] {
		t.Fatalf("cross-language link changed across an incremental resolve:\n before %v\n after  %v", before, after)
	}
}

// An incremental batch that merely MENTIONS a name must not destroy a binding.
// Whatever a fresh full index decided has to survive, byte-for-byte, for every
// strategy the repo-wide resolver can produce -- including the three
// (`receiver_method`, `slash_suffix`, `dot_suffix`) that only IT produces and
// that no incremental pass could rebuild if they were cleared.
//
// This is the case the real-repository harness cannot see: those three bind
// nothing on CodeGraph, google/pprof or mitmproxy, so an empty class there is
// not evidence of an absent class.
func TestIncrementalBatchPreservesEveryFullIndexStrategy(t *testing.T) {
	cases := []struct {
		name      string
		dstName   string
		qualified string
		kind      string
		container string
		want      string
	}{
		{"exact_qualified", "config.load", "config.load", "function", "", ResolutionStrategyExactQualified},
		{"exact_name", "load", "config.load", "function", "", ResolutionStrategyExactName},
		{"dot_tail2", "path.load", "some.other.path.load", "function", "", ResolutionStrategyDotTail2},
		{"dot_tail3", "y.path.load", "some.other.y.path.load", "function", "", ResolutionStrategyDotTail3},
		{"dot_suffix", "w.x.y.z", "v.w.x.y.z", "function", "", ResolutionStrategyDotSuffix},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newParityFixture(t, "")
			defs := f.file(t, "app/defs.py", "python")
			short := tc.qualified
			if i := len(short) - 1; i >= 0 {
				for j := i; j >= 0; j-- {
					if short[j] == '.' {
						short = short[j+1:]
						break
					}
				}
			}
			dst := f.symbolIn(t, defs, short, tc.qualified, tc.kind, tc.container, "python")
			// A same-named declaration in ANOTHER language. It cannot compete
			// for this edge (P2 gates candidates by the source's language), so
			// the full index still binds exactly as before -- but the name is
			// now declared more than once, which is what makes the batch reach
			// the invalidation pass instead of short-circuiting before it.
			// Without this the test cannot see the strategy predicate at all.
			decoy := f.file(t, "web/decoy.ts", "typescript")
			f.symbol(t, decoy, short, "decoy."+short, tc.kind, "typescript")

			callerFile := f.file(t, "app/main.py", "python")
			caller := f.symbol(t, callerFile, "run", "main.run", "function", "python")
			edge := f.edge(t, callerFile, caller, tc.dstName)

			f.resolveVia(t, "full", nil, nil)
			before := f.binding(t, edge)
			if want := f.qualifiedOf(t, dst) + "|" + tc.want + "|" + resolutionConfidenceFor(tc.want); before != want {
				t.Fatalf("full index produced %q, want %q -- fixture no longer exercises %s", before, want, tc.name)
			}

			// An unrelated incremental batch that happens to mention the name.
			f.resolveVia(t, "paths+names", []string{"app/main.py"}, []string{short})
			if after := f.binding(t, edge); after != before {
				t.Fatalf("an incremental batch changed a full-index binding: %q -> %q", before, after)
			}
		})
	}
}

func TestDotSuffixIncrementalUniqueAmbiguousRecovery(t *testing.T) {
	f := newParityFixture(t, "")
	defs := f.file(t, "app/defs.py", "python")
	f.symbol(t, defs, "run", "root.a.b.c.run", "function", "python")
	callerFile := f.file(t, "app/main.py", "python")
	caller := f.symbol(t, callerFile, "main", "main", "function", "python")
	edge := f.edge(t, callerFile, caller, "a.b.c.run")
	f.resolveVia(t, "full", nil, nil)
	if got, want := f.binding(t, edge), "root.a.b.c.run|dot_suffix|low"; got != want {
		t.Fatalf("unique: got %q, want %q", got, want)
	}

	competitorFile := f.file(t, "app/other.py", "python")
	competitor := f.symbol(t, competitorFile, "run", "other.a.b.c.run", "function", "python")
	f.resolveVia(t, "paths+names", []string{"app/other.py"}, []string{"run"})
	if got := f.binding(t, edge); got != "<unresolved>" {
		t.Fatalf("competitor added: got %q, want unresolved", got)
	}

	if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM symbols WHERE id = ?`, competitor); err != nil {
		t.Fatal(err)
	}
	f.resolveVia(t, "paths+names", []string{"app/other.py"}, []string{"run"})
	if got, want := f.binding(t, edge), "root.a.b.c.run|dot_suffix|low"; got != want {
		t.Fatalf("competitor removed: got %q, want %q", got, want)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET qualified_name = 'renamed.other' WHERE qualified_name = 'root.a.b.c.run'`); err != nil {
		t.Fatal(err)
	}
	f.resolveVia(t, "paths+names", []string{"app/defs.py"}, []string{"run"})
	if got := f.binding(t, edge); got != "<unresolved>" {
		t.Fatalf("target renamed: got %q, want unresolved", got)
	}
	if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM symbols WHERE qualified_name = 'renamed.other'`); err != nil {
		t.Fatal(err)
	}
	f.resolveVia(t, "paths+names", []string{"app/defs.py"}, []string{"run"})
	if got := f.binding(t, edge); got != "<unresolved>" {
		t.Fatalf("target deleted: got %q, want unresolved", got)
	}
}

func TestInactiveLegacyStrategiesConvergeOnIncrementalEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, strategy, confidence, dstName, qualified, kind, container string
	}{
		{"receiver_method", ResolutionStrategyReceiverMethod, ResolutionConfidenceMedium, "Run", "T.Run", "value", "T"},
		{"slash_suffix", ResolutionStrategySlashSuffix, ResolutionConfidenceMedium, "pkg.Func", "github.com/acme/pkg.Func", "function", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newParityFixture(t, "")
			defs := f.file(t, "app/defs.py", "python")
			dst := f.symbolIn(t, defs, tc.dstName[strings.LastIndex(tc.dstName, ".")+1:], tc.qualified, tc.kind, tc.container, "python")
			callerFile := f.file(t, "app/main.py", "python")
			caller := f.symbol(t, callerFile, "main", "main", "function", "python")
			edge := f.edge(t, callerFile, caller, tc.dstName)
			if _, err := f.store.db.ExecContext(f.ctx, `UPDATE edges SET dst_symbol_id = ?, resolution_strategy = ?, resolution_confidence = ? WHERE id = ?`, dst, tc.strategy, tc.confidence, edge); err != nil {
				t.Fatal(err)
			}
			f.resolveVia(t, "paths+names", []string{"app/defs.py"}, []string{tc.dstName})
			if got := f.binding(t, edge); strings.Contains(got, "|"+tc.strategy+"|") {
				t.Fatalf("legacy %s remained sticky: %q", tc.strategy, got)
			}
		})
	}
}

func TestDotSuffixIncrementalRedecisionIsRepositoryScoped(t *testing.T) {
	f := newParityFixture(t, "")
	otherRoot := t.TempDir()
	other, err := f.store.UpsertRepo(f.ctx, otherRoot)
	if err != nil {
		t.Fatal(err)
	}

	add := func(repoID int64, path, qualified string) (int64, int64) {
		file, err := insertTestFileLang(f.ctx, f.store, repoID, path, "python")
		if err != nil {
			t.Fatal(err)
		}
		symbol, err := insertTestSymbolKind(f.ctx, f.store, repoID, file, "run", qualified, "function", "", "python")
		if err != nil {
			t.Fatal(err)
		}
		return file, symbol
	}
	add(f.repoID, "a/defs.py", "root.a.b.c.run")
	callerAFile, callerA := add(f.repoID, "a/main.py", "main")
	edgeA := f.edge(t, callerAFile, callerA, "a.b.c.run")
	add(other.ID, "b/defs.py", "other.a.b.c.run")
	callerBFile, callerB := add(other.ID, "b/main.py", "main")
	edgeB, err := insertTestEdge(f.ctx, f.store, other.ID, callerBFile, callerB, "a.b.c.run")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ResolveEdges(f.ctx, f.repoID); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ResolveEdges(f.ctx, other.ID); err != nil {
		t.Fatal(err)
	}
	beforeB := legacyBinding(t, f.store, f.ctx, edgeB)

	competitorFile, err := insertTestFileLang(f.ctx, f.store, f.repoID, "a/other.py", "python")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := insertTestSymbolKind(f.ctx, f.store, f.repoID, competitorFile, "run", "other.a.b.c.run", "function", "", "python"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.ResolveEdgesForPathsAndNames(f.ctx, f.repoID, []string{"a/other.py"}, []string{"run"}); err != nil {
		t.Fatal(err)
	}
	if got := f.binding(t, edgeA); got != "<unresolved>" {
		t.Fatalf("repo A competitor did not invalidate dot_suffix: %q", got)
	}
	if got := legacyBinding(t, f.store, f.ctx, edgeB); got != beforeB {
		t.Fatalf("repo B changed after repo A mutation: %q -> %q", beforeB, got)
	}
	var dstRepo int64
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT COALESCE(d.repo_id, 0) FROM edges e LEFT JOIN symbols d ON d.id = e.dst_symbol_id WHERE e.id = ?`, edgeA).Scan(&dstRepo); err != nil {
		t.Fatal(err)
	}
	if dstRepo != 0 && dstRepo != f.repoID {
		t.Fatalf("repo A edge crossed into repo %d", dstRepo)
	}
}

func TestInactiveSlashSuffixIsNotProduced(t *testing.T) {
	f := newParityFixture(t, "")
	defs := f.file(t, "pkg/format.go", "go")
	f.symbol(t, defs, "Format", "github.com/acme/pkg.Format", "function", "go")
	callerFile := f.file(t, "cmd/main.go", "go")
	caller := f.symbol(t, callerFile, "main", "main", "function", "go")
	edge := f.edge(t, callerFile, caller, "pkg.Format")
	f.resolveVia(t, "full", nil, nil)
	if got := f.binding(t, edge); strings.Contains(got, "|"+ResolutionStrategySlashSuffix+"|") {
		t.Fatalf("inactive slash_suffix was produced: %q", got)
	}
}

func legacyBinding(t *testing.T, s *Store, ctx context.Context, edgeID int64) string {
	t.Helper()
	var qname sql.NullString
	var strategy, confidence string
	if err := s.db.QueryRowContext(ctx, `SELECT d.qualified_name, e.resolution_strategy, e.resolution_confidence FROM edges e LEFT JOIN symbols d ON d.id = e.dst_symbol_id WHERE e.id = ?`, edgeID).Scan(&qname, &strategy, &confidence); err != nil {
		t.Fatal(err)
	}
	if !qname.Valid {
		return "<unresolved>"
	}
	return qname.String + "|" + strategy + "|" + confidence
}

// qualifiedOf renders a symbol's identity for an expectation string.
func (f *parityFixture) qualifiedOf(t *testing.T, symbolID int64) string {
	t.Helper()
	var qname string
	if err := f.store.db.QueryRowContext(f.ctx,
		`SELECT qualified_name FROM symbols WHERE id = ?`, symbolID).Scan(&qname); err != nil {
		t.Fatalf("qualified name of %d: %v", symbolID, err)
	}
	return qname
}
