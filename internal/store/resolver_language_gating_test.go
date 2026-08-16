package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
)

// Regression coverage for P2 resolver language gating.
//
// Every implicit resolver strategy must refuse to bind a destination symbol
// whose persisted language differs from the persisted language of the calling
// edge's own source file, and must keep resolving legitimate same-language
// calls. The scenarios below are run against all three production entrypoints
// (repo-wide, path-scoped, name-targeted) so the three paths cannot drift.

type gateFixture struct {
	ctx    context.Context
	store  *Store
	repoID int64
}

func newGateFixture(t *testing.T) *gateFixture {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "graph.sqlite")
	s, err := Open(dbPath)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()
	repo, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	return &gateFixture{ctx: ctx, store: s, repoID: repo.ID}
}

func (f *gateFixture) file(t *testing.T, path, language string) int64 {
	t.Helper()
	id, err := insertTestFileLang(f.ctx, f.store, f.repoID, path, language)
	if err != nil {
		t.Fatalf("insertTestFileLang(%s, %q) error = %v", path, language, err)
	}
	return id
}

func (f *gateFixture) symbol(t *testing.T, fileID int64, name, qualified, language string) int64 {
	t.Helper()
	id, err := insertTestSymbolLang(f.ctx, f.store, f.repoID, fileID, name, qualified, language)
	if err != nil {
		t.Fatalf("insertTestSymbolLang(%s, %q) error = %v", qualified, language, err)
	}
	return id
}

func (f *gateFixture) edge(t *testing.T, fileID, srcSymbolID int64, dstName string) int64 {
	t.Helper()
	id, err := insertTestEdge(f.ctx, f.store, f.repoID, fileID, srcSymbolID, dstName)
	if err != nil {
		t.Fatalf("insertTestEdge(%s) error = %v", dstName, err)
	}
	return id
}

func (f *gateFixture) dstSymbolID(t *testing.T, edgeID int64) (int64, bool) {
	t.Helper()
	var dst sql.NullInt64
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT dst_symbol_id FROM edges WHERE id = ?`, edgeID).Scan(&dst); err != nil {
		t.Fatalf("QueryRow(dst_symbol_id) error = %v", err)
	}
	return dst.Int64, dst.Valid
}

// symbolKind inserts a definition with an explicit kind, so strategies that
// filter on kind can be exercised.
func (f *gateFixture) symbolKind(t *testing.T, fileID int64, name, qualified, kind, language string) int64 {
	t.Helper()
	id, err := insertTestSymbolKind(f.ctx, f.store, f.repoID, fileID, name, qualified, kind, "", language)
	if err != nil {
		t.Fatalf("insertTestSymbolKind(%s, %q, %q) error = %v", qualified, kind, language, err)
	}
	return id
}

// method inserts a definition that declares a receiver, which is what the
// repo-wide receiver strategy matches on.
func (f *gateFixture) method(t *testing.T, fileID int64, name, qualified, container, language string) int64 {
	t.Helper()
	id, err := insertTestSymbolKind(f.ctx, f.store, f.repoID, fileID, name, qualified, "method", container, language)
	if err != nil {
		t.Fatalf("insertTestSymbolKind(%s, method, %q) error = %v", qualified, language, err)
	}
	return id
}

// qualifiedNameOf renders a resolved destination so a diverging selection is
// reported by name rather than by an opaque symbol id.
func (f *gateFixture) qualifiedNameOf(t *testing.T, symbolID int64) string {
	t.Helper()
	var qualified string
	if err := f.store.db.QueryRowContext(f.ctx, `SELECT qualified_name FROM symbols WHERE id = ?`, symbolID).Scan(&qualified); err != nil {
		t.Fatalf("QueryRow(qualified_name) error = %v", err)
	}
	return qualified
}

// gateScenario builds a graph and states what the resolver is expected to do.
type gateScenario struct {
	name string
	// build returns the edge under test, the id the edge must resolve to (0 when
	// it must stay unresolved), the source file path and the short name that a
	// name-targeted incremental run would be given.
	build func(t *testing.T, f *gateFixture) (edgeID, wantDstID int64, srcPath, targetName string)
}

func gateScenarios() []gateScenario {
	return []gateScenario{
		{
			// Case A from the P1 audit: python `sorted` must never bind to Swift.
			name: "exact_cross_language_unresolved",
			build: func(t *testing.T, f *gateFixture) (int64, int64, string, string) {
				swiftFile := f.file(t, "src/swift/SortKit.swift", "swift")
				f.symbol(t, swiftFile, "sorted", "sorted", "swift")
				pyFile := f.file(t, "src/py/caller.py", "python")
				src := f.symbol(t, pyFile, "caller", "caller", "python")
				return f.edge(t, pyFile, src, "sorted"), 0, "src/py/caller.py", "sorted"
			},
		},
		{
			name: "exact_same_language_resolved",
			build: func(t *testing.T, f *gateFixture) (int64, int64, string, string) {
				helpers := f.file(t, "src/py/helpers.py", "python")
				dst := f.symbol(t, helpers, "normalize", "normalize", "python")
				pyFile := f.file(t, "src/py/caller.py", "python")
				src := f.symbol(t, pyFile, "caller", "caller", "python")
				return f.edge(t, pyFile, src, "normalize"), dst, "src/py/caller.py", "normalize"
			},
		},
		{
			// Case C shape: foreign same-named definitions are inserted first, so
			// an ungated MIN(id)/LIMIT 1 pick would choose one of them.
			name: "same_language_wins_over_foreign_collision",
			build: func(t *testing.T, f *gateFixture) (int64, int64, string, string) {
				javaFile := f.file(t, "src/java/Serializer.java", "java")
				f.symbol(t, javaFile, "serialize", "serialize", "java")
				kotlinFile := f.file(t, "src/kotlin/serializer.kt", "kotlin")
				f.symbol(t, kotlinFile, "serialize", "serialize", "kotlin")
				rustFile := f.file(t, "src/rust/serializer.rs", "rust")
				f.symbol(t, rustFile, "serialize", "serialize", "rust")

				pyDefFile := f.file(t, "src/py/serializer.py", "python")
				dst := f.symbol(t, pyDefFile, "serialize", "serialize", "python")
				pyFile := f.file(t, "src/py/caller.py", "python")
				src := f.symbol(t, pyFile, "caller", "caller", "python")
				return f.edge(t, pyFile, src, "serialize"), dst, "src/py/caller.py", "serialize"
			},
		},
		{
			// Case D shape: a dotted dst_name suffix-matching a foreign symbol.
			name: "dot_suffix_cross_language_unresolved",
			build: func(t *testing.T, f *gateFixture) (int64, int64, string, string) {
				javaFile := f.file(t, "src/java/Shapes.java", "java")
				f.symbol(t, javaFile, "parse", "Shapes.Sorter.parse", "java")
				goFile := f.file(t, "src/go/caller.go", "go")
				src := f.symbol(t, goFile, "Caller", "Caller", "go")
				return f.edge(t, goFile, src, "Sorter.parse"), 0, "src/go/caller.go", "parse"
			},
		},
		{
			name: "dot_suffix_same_language_resolved",
			build: func(t *testing.T, f *gateFixture) (int64, int64, string, string) {
				defFile := f.file(t, "src/go/shapes.go", "go")
				dst := f.symbol(t, defFile, "parse", "shapes.Sorter.parse", "go")
				goFile := f.file(t, "src/go/caller.go", "go")
				src := f.symbol(t, goFile, "Caller", "Caller", "go")
				return f.edge(t, goFile, src, "Sorter.parse"), dst, "src/go/caller.go", "parse"
			},
		},
		{
			// Slash-qualified suffix strategy (symbols.qualified_suffix).
			name: "slash_suffix_cross_language_unresolved",
			build: func(t *testing.T, f *gateFixture) (int64, int64, string, string) {
				tsFile := f.file(t, "src/ts/pkg.ts", "typescript")
				f.symbol(t, tsFile, "Format", "github.com/org/repo/pkg.Format", "typescript")
				goFile := f.file(t, "src/go/caller.go", "go")
				src := f.symbol(t, goFile, "Caller", "Caller", "go")
				return f.edge(t, goFile, src, "pkg.Format"), 0, "src/go/caller.go", "Format"
			},
		},
		{
			name: "slash_suffix_same_language_resolved",
			build: func(t *testing.T, f *gateFixture) (int64, int64, string, string) {
				defFile := f.file(t, "src/go/pkg/format.go", "go")
				dst := f.symbol(t, defFile, "Format", "github.com/org/repo/pkg.Format", "go")
				goFile := f.file(t, "src/go/caller.go", "go")
				src := f.symbol(t, goFile, "Caller", "Caller", "go")
				return f.edge(t, goFile, src, "pkg.Format"), dst, "src/go/caller.go", "Format"
			},
		},
		{
			// Receiver strategy: unqualified dst_name matching a contained symbol.
			name: "receiver_cross_language_unresolved",
			build: func(t *testing.T, f *gateFixture) (int64, int64, string, string) {
				phpFile := f.file(t, "src/php/box.php", "php")
				id := f.symbol(t, phpFile, "unwrap", "Box.unwrap", "php")
				if _, err := f.store.db.ExecContext(f.ctx, `UPDATE symbols SET container_name = 'Box', kind = 'other' WHERE id = ?`, id); err != nil {
					t.Fatalf("set container_name: %v", err)
				}
				rubyFile := f.file(t, "src/ruby/caller.rb", "ruby")
				src := f.symbol(t, rubyFile, "caller", "caller", "ruby")
				return f.edge(t, rubyFile, src, "unwrap"), 0, "src/ruby/caller.rb", "unwrap"
			},
		},
		{
			// Fail closed when the source file has no persisted language.
			name: "unknown_source_language_unresolved",
			build: func(t *testing.T, f *gateFixture) (int64, int64, string, string) {
				defFile := f.file(t, "src/go/helpers.go", "go")
				f.symbol(t, defFile, "Helper", "Helper", "go")
				unknownFile := f.file(t, "vendor/blob.bin", "")
				src := f.symbol(t, unknownFile, "caller", "caller", "go")
				return f.edge(t, unknownFile, src, "Helper"), 0, "vendor/blob.bin", "Helper"
			},
		},
		{
			// Fail closed when the destination symbol has no persisted language.
			name: "unknown_destination_language_unresolved",
			build: func(t *testing.T, f *gateFixture) (int64, int64, string, string) {
				defFile := f.file(t, "src/unknown/helpers.txt", "")
				f.symbol(t, defFile, "Helper", "Helper", "")
				goFile := f.file(t, "src/go/caller.go", "go")
				src := f.symbol(t, goFile, "Caller", "Caller", "go")
				return f.edge(t, goFile, src, "Helper"), 0, "src/go/caller.go", "Helper"
			},
		},
		{
			// Per-language uniqueness: a name that is globally ambiguous but has
			// exactly one definition in the caller's language now resolves. This
			// is a deliberate widening -- foreign definitions must not veto a
			// valid same-language target.
			name: "globally_ambiguous_but_unique_in_source_language_resolved",
			build: func(t *testing.T, f *gateFixture) (int64, int64, string, string) {
				pyOne := f.file(t, "src/py/a.py", "python")
				f.symbol(t, pyOne, "render", "render", "python")
				pyTwo := f.file(t, "src/py/b.py", "python")
				f.symbol(t, pyTwo, "render", "render", "python")

				goDef := f.file(t, "src/go/render.go", "go")
				dst := f.symbol(t, goDef, "render", "gopkg.render", "go")
				goFile := f.file(t, "src/go/caller.go", "go")
				src := f.symbol(t, goFile, "Caller", "gopkg.Caller", "go")
				return f.edge(t, goFile, src, "render"), dst, "src/go/caller.go", "render"
			},
		},
		{
			name: "unknown_symbol_unresolved",
			build: func(t *testing.T, f *gateFixture) (int64, int64, string, string) {
				goFile := f.file(t, "src/go/caller.go", "go")
				src := f.symbol(t, goFile, "Caller", "Caller", "go")
				return f.edge(t, goFile, src, "NoSuchSymbol"), 0, "src/go/caller.go", "NoSuchSymbol"
			},
		},
	}
}

// TestResolverLanguageGating_AllEntrypoints asserts that the repo-wide,
// path-scoped and name-targeted resolvers make the same decision for every
// scenario. Divergence here is the full/incremental parity bug from P1.
func TestResolverLanguageGating_AllEntrypoints(t *testing.T) {
	resolvers := []struct {
		name string
		run  func(t *testing.T, f *gateFixture, srcPath, targetName string)
	}{
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
	}

	for _, scenario := range gateScenarios() {
		for _, resolver := range resolvers {
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

// TestResolverLanguageGating_FullIncrementalParity is the focused parity test
// for P1 case C: a python call with only foreign-language definitions must be
// left unresolved by the full resolver and by a follow-up name-targeted
// incremental run alike.
func TestResolverLanguageGating_FullIncrementalParity(t *testing.T) {
	build := func(t *testing.T, f *gateFixture) int64 {
		t.Helper()
		javaFile := f.file(t, "src/java/Serializer.java", "java")
		f.symbol(t, javaFile, "serialize_c", "SerializerC.serialize_c", "java")
		kotlinFile := f.file(t, "src/kotlin/serializer.kt", "kotlin")
		f.symbol(t, kotlinFile, "serialize_c", "serializer_c.serialize_c", "kotlin")
		rustFile := f.file(t, "src/rust/serializer.rs", "rust")
		f.symbol(t, rustFile, "serialize_c", "serializer_c.serialize_c", "rust")
		phpFile := f.file(t, "src/php/serializer.php", "php")
		f.symbol(t, phpFile, "serialize_c", "serializer_c.serialize_c", "php")
		pyFile := f.file(t, "src/py/caller_c.py", "python")
		src := f.symbol(t, pyFile, "caller_c", "caller_c", "python")
		return f.edge(t, pyFile, src, "serialize_c")
	}

	full := newGateFixture(t)
	fullEdge := build(t, full)
	if _, err := full.store.ResolveEdges(full.ctx, full.repoID); err != nil {
		t.Fatalf("ResolveEdges() error = %v", err)
	}
	if dst, resolved := full.dstSymbolID(t, fullEdge); resolved {
		t.Fatalf("full resolver bound cross-language destination %d; want unresolved", dst)
	}

	incremental := newGateFixture(t)
	incrementalEdge := build(t, incremental)
	if err := incremental.store.ResolveEdgesForPaths(incremental.ctx, incremental.repoID, []string{"src/py/caller_c.py"}); err != nil {
		t.Fatalf("ResolveEdgesForPaths() error = %v", err)
	}
	stats, err := incremental.store.ResolveEdgesForNamesWithStats(incremental.ctx, incremental.repoID, []string{"serialize_c"})
	if err != nil {
		t.Fatalf("ResolveEdgesForNamesWithStats() error = %v", err)
	}
	if dst, resolved := incremental.dstSymbolID(t, incrementalEdge); resolved {
		t.Fatalf("incremental resolver bound cross-language destination %d; want unresolved", dst)
	}
	if stats.TargetsSelected != 1 {
		t.Fatalf("TargetsSelected = %d, want 1", stats.TargetsSelected)
	}
	if stats.TargetsResolved != 0 {
		t.Fatalf("TargetsResolved = %d, want 0", stats.TargetsResolved)
	}
	if stats.TargetsUnresolved != 1 {
		t.Fatalf("TargetsUnresolved = %d, want 1", stats.TargetsUnresolved)
	}
	if stats.LanguageBlocked != 1 {
		t.Fatalf("LanguageBlocked = %d, want 1", stats.LanguageBlocked)
	}
}

// TestResolveEdgesForNamesStats_CountOnlyRealResolutions asserts the reported
// counters describe what actually happened: selection is not resolution.
func TestResolveEdgesForNamesStats_CountOnlyRealResolutions(t *testing.T) {
	f := newGateFixture(t)

	goDef := f.file(t, "src/go/helpers.go", "go")
	wantDst := f.symbol(t, goDef, "Shared", "gopkg.Shared", "go")
	swiftDef := f.file(t, "src/swift/Helpers.swift", "swift")
	f.symbol(t, swiftDef, "Shared", "Shared", "swift")

	goCaller := f.file(t, "src/go/caller.go", "go")
	goSrc := f.symbol(t, goCaller, "GoCaller", "gopkg.GoCaller", "go")
	goEdge := f.edge(t, goCaller, goSrc, "Shared")

	pyCaller := f.file(t, "src/py/caller.py", "python")
	pySrc := f.symbol(t, pyCaller, "py_caller", "py_caller", "python")
	pyEdge := f.edge(t, pyCaller, pySrc, "Shared")

	unknownCaller := f.file(t, "vendor/blob.bin", "")
	unknownSrc := f.symbol(t, unknownCaller, "blob", "blob", "go")
	unknownEdge := f.edge(t, unknownCaller, unknownSrc, "Shared")

	stats, err := f.store.ResolveEdgesForNamesWithStats(f.ctx, f.repoID, []string{"Shared"})
	if err != nil {
		t.Fatalf("ResolveEdgesForNamesWithStats() error = %v", err)
	}
	if stats.TargetsSelected != 3 {
		t.Fatalf("TargetsSelected = %d, want 3", stats.TargetsSelected)
	}
	if stats.TargetsResolved != 1 {
		t.Fatalf("TargetsResolved = %d, want 1", stats.TargetsResolved)
	}
	if stats.LanguageBlocked != 1 {
		t.Fatalf("LanguageBlocked = %d, want 1", stats.LanguageBlocked)
	}
	if stats.UnknownSrcLanguage != 1 {
		t.Fatalf("UnknownSrcLanguage = %d, want 1", stats.UnknownSrcLanguage)
	}
	// The three outcome counters must account for every selected target.
	if sum := stats.TargetsResolved + stats.TargetsUnresolved + stats.UnknownSrcLanguage; sum != stats.TargetsSelected {
		t.Fatalf("resolved+unresolved+unknown = %d, want TargetsSelected = %d", sum, stats.TargetsSelected)
	}

	if dst, resolved := f.dstSymbolID(t, goEdge); !resolved || dst != wantDst {
		t.Fatalf("go edge dst = (%d, %v), want (%d, true)", dst, resolved, wantDst)
	}
	if dst, resolved := f.dstSymbolID(t, pyEdge); resolved {
		t.Fatalf("python edge bound foreign destination %d; want unresolved", dst)
	}
	if dst, resolved := f.dstSymbolID(t, unknownEdge); resolved {
		t.Fatalf("unknown-language edge bound destination %d; want unresolved", dst)
	}
}

func TestResolverLanguageCompatible(t *testing.T) {
	cases := []struct {
		src  string
		dst  string
		want bool
	}{
		{"go", "go", true},
		{"python", "python", true},
		{"java", "kotlin", false},
		{"kotlin", "java", false},
		{"cpp", "c", false},
		{"typescript", "javascript", false},
		{"python", "swift", false},
		{"go", "java", false},
		{"", "go", false},
		{"go", "", false},
		{"", "", false},
	}
	for _, tc := range cases {
		if got := resolverLanguageCompatible(tc.src, tc.dst); got != tc.want {
			t.Fatalf("resolverLanguageCompatible(%q, %q) = %v, want %v", tc.src, tc.dst, got, tc.want)
		}
	}
}
