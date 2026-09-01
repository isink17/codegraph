package store

import (
	"fmt"
	"testing"
)

// P22.1 regression coverage: member-call false positives.
//
// A dotted (or ::-scoped), slash-free dst_name is member/scope-qualified
// syntax: its qualifier is evidence. Discarding the qualifier and binding (or
// claiming, at query time) by the bare tail turns `rows.Close()` on an
// external receiver into an edge to an unrelated project method. The rule
// under test everywhere: one spelling must extend the other at a separator
// boundary ('.', '::', '/'), or nothing binds.
//
// Import-path spellings (containing '/') deliberately keep the legacy
// tail fallback: distinguishing own-module paths from external ones is the
// deferred own-module import-mapping work, not this slice.

// TestBinderMemberCallUnknownReceiverAbstains reproduces the P21 defect at the
// persistence layer: a unique project method named Close must NOT be bound by
// `rows.Close` where `rows` names nothing CodeGraph knows. Both scoped
// entrypoints are covered, and both insertion orders, so no outcome can hinge
// on row order.
func TestBinderMemberCallUnknownReceiverAbstains(t *testing.T) {
	for _, entrypoint := range []string{"paths", "names"} {
		for _, order := range []string{"dst_first", "src_first"} {
			t.Run(entrypoint+"_"+order, func(t *testing.T) {
				f := newGateFixture(t)
				buildDst := func() {
					defs := f.file(t, "src/go/app.go", "go")
					if _, err := insertTestSymbolKind(f.ctx, f.store, f.repoID, defs, "Close", "app.App.Close", "function", "App", "go"); err != nil {
						t.Fatalf("insert method: %v", err)
					}
				}
				var edgeID int64
				buildSrc := func() {
					caller := f.file(t, "src/go/db.go", "go")
					src := f.symbol(t, caller, "Query", "db.Query", "go")
					edgeID = f.edge(t, caller, src, "rows.Close")
				}
				if order == "dst_first" {
					buildDst()
					buildSrc()
				} else {
					buildSrc()
					buildDst()
				}

				var err error
				switch entrypoint {
				case "paths":
					err = f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"src/go/db.go"})
				case "names":
					_, err = f.store.ResolveEdgesForNames(f.ctx, f.repoID, []string{"Close"})
				}
				if err != nil {
					t.Fatalf("resolve via %s error = %v", entrypoint, err)
				}
				assertUnresolvedNoMetadata(t, f, edgeID)
			})
		}
	}
}

// TestBinderDotTailFallbackMatchesFullIndex is the recall guard and the
// full-vs-update parity check in one: a member spelling whose qualifier is
// verified against the destination's own identity must bind identically
// (target and strategy) through the repo-wide resolver and through the
// Go-side binder.
func TestBinderDotTailFallbackMatchesFullIndex(t *testing.T) {
	cases := []struct {
		name         string
		dstName      string
		qualified    string
		wantStrategy string
	}{
		// One dot: the destination's last two segments confirm the qualifier.
		{"dot_tail2", "App.Close", "app.App.Close", ResolutionStrategyDotTail2},
		// Two dots: last three segments.
		{"dot_tail3", "pkg.mod.run", "a.pkg.mod.run", ResolutionStrategyDotTail3},
	}
	for _, tc := range cases {
		for _, entrypoint := range []string{"repo", "paths", "names"} {
			t.Run(tc.name+"_"+entrypoint, func(t *testing.T) {
				f := newGateFixture(t)
				defs := f.file(t, "src/py/defs.py", "python")
				want := f.symbol(t, defs, "irrelevant_bare_name", tc.qualified, "python")
				caller := f.file(t, "src/py/caller.py", "python")
				src := f.symbol(t, caller, "caller", "caller", "python")
				edgeID := f.edge(t, caller, src, tc.dstName)

				var err error
				switch entrypoint {
				case "repo":
					_, err = f.store.ResolveEdges(f.ctx, f.repoID)
				case "paths":
					err = f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"src/py/caller.py"})
				case "names":
					_, err = f.store.ResolveEdgesForNames(f.ctx, f.repoID, []string{"run", "Close"})
				}
				if err != nil {
					t.Fatalf("resolve via %s error = %v", entrypoint, err)
				}
				assertResolvedWithStrategy(t, f, edgeID, want, tc.wantStrategy)
			})
		}
	}
}

// TestBinderMemberAmbiguityFailsClosed: two receivers legitimately claim the
// same member spelling; the binder must refuse rather than pick either, in
// both insertion orders.
func TestBinderMemberAmbiguityFailsClosed(t *testing.T) {
	for _, order := range []string{"ab", "ba"} {
		t.Run(order, func(t *testing.T) {
			f := newGateFixture(t)
			files := []struct{ path, qualified string }{
				{"src/py/a.py", "a.Conn.close"},
				{"src/py/b.py", "b.Conn.close"},
			}
			if order == "ba" {
				files[0], files[1] = files[1], files[0]
			}
			for _, fl := range files {
				id := f.file(t, fl.path, "python")
				f.symbol(t, id, "close", fl.qualified, "python")
			}
			caller := f.file(t, "src/py/caller.py", "python")
			src := f.symbol(t, caller, "caller", "caller", "python")
			edgeID := f.edge(t, caller, src, "Conn.close")

			if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"src/py/caller.py"}); err != nil {
				t.Fatalf("ResolveEdgesForPaths() error = %v", err)
			}
			assertUnresolvedNoMetadata(t, f, edgeID)
		})
	}
}

// TestBinderMemberCallRespectsTestShadow: the only qualifier-confirmed
// candidate lives in a test file, the caller is production code, so the
// binder must abstain (P7).
func TestBinderMemberCallRespectsTestShadow(t *testing.T) {
	f := newGateFixture(t)
	defs := f.file(t, "src/py/test_app.py", "python")
	f.symbol(t, defs, "close", "test_app.App.close", "python")
	caller := f.file(t, "src/py/caller.py", "python")
	src := f.symbol(t, caller, "caller", "caller", "python")
	edgeID := f.edge(t, caller, src, "App.close")

	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"src/py/caller.py"}); err != nil {
		t.Fatalf("ResolveEdgesForPaths() error = %v", err)
	}
	assertUnresolvedNoMetadata(t, f, edgeID)
}

// TestBinderKeepsBareFallbackAndDropsImportPathTail is the recall guard for the
// bare-name class the member rule must NOT touch (free-function calls), and the
// contract for the import-path class beside it.
//
// P22.1 left import-path spellings on the tail fallback because telling
// own-module paths from external ones was deferred. P22.5 built that evidence
// (module_import maps the path against the repository's own go.mod files), so
// P22.8 stopped degrading them: a path that does not map into the module names
// something outside the repository, and the project's own same-tailed symbol is
// not it. `TestOwnModuleImportResolvesOnEveryEntryPoint` is the recall guard for
// the half that does map.
func TestBinderKeepsBareFallbackAndDropsImportPathTail(t *testing.T) {
	cases := []struct {
		name      string
		dstName   string
		qualified string
		symName   string
		wantBound bool
	}{
		{"bare_free_function", "load_config", "config.load_config", "load_config", true},
		{"import_path_tail", "github.com/org/repo/internal/parser.NewRegistry", "parser.NewRegistry", "NewRegistry", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newGateFixture(t)
			defs := f.file(t, "src/py/defs.py", "python")
			want := f.symbol(t, defs, tc.symName, tc.qualified, "python")
			caller := f.file(t, "src/py/caller.py", "python")
			src := f.symbol(t, caller, "caller", "caller", "python")
			edgeID := f.edge(t, caller, src, tc.dstName)

			if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"src/py/caller.py"}); err != nil {
				t.Fatalf("ResolveEdgesForPaths() error = %v", err)
			}
			if !tc.wantBound {
				assertUnresolvedNoMetadata(t, f, edgeID)
				return
			}
			assertResolvedWithStrategy(t, f, edgeID, want, ResolutionStrategyExactName)
		})
	}
}

// TestBinderIdempotentOnMemberEdges: resolving an unchanged graph again must
// not rebind, oscillate, or duplicate anything.
func TestBinderIdempotentOnMemberEdges(t *testing.T) {
	f := newGateFixture(t)
	defs := f.file(t, "src/go/app.go", "go")
	want, err := insertTestSymbolKind(f.ctx, f.store, f.repoID, defs, "Close", "app.App.Close", "function", "App", "go")
	if err != nil {
		t.Fatalf("insert method: %v", err)
	}
	caller := f.file(t, "src/go/db.go", "go")
	src := f.symbol(t, caller, "Query", "db.Query", "go")
	external := f.edge(t, caller, src, "rows.Close")
	member := f.edge(t, caller, src, "App.Close")

	snapshot := func() string {
		rows, err := f.store.db.QueryContext(f.ctx, `
			SELECT id, IFNULL(dst_symbol_id, 0), resolution_strategy, resolution_confidence
			FROM edges WHERE repo_id = ? ORDER BY id`, f.repoID)
		if err != nil {
			t.Fatalf("snapshot query: %v", err)
		}
		defer rows.Close()
		out := ""
		for rows.Next() {
			var id, dst int64
			var strategy, confidence string
			if err := rows.Scan(&id, &dst, &strategy, &confidence); err != nil {
				t.Fatalf("snapshot scan: %v", err)
			}
			out += fmt.Sprintf("%d:%d:%s:%s\n", id, dst, strategy, confidence)
		}
		return out
	}

	for i := 0; i < 3; i++ {
		if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"src/go/db.go"}); err != nil {
			t.Fatalf("resolve pass %d: %v", i, err)
		}
		if _, err := f.store.ResolveEdgesForNames(f.ctx, f.repoID, []string{"Close"}); err != nil {
			t.Fatalf("resolve names pass %d: %v", i, err)
		}
		if i == 0 {
			continue
		}
	}
	first := snapshot()
	if err := f.store.ResolveEdgesForPaths(f.ctx, f.repoID, []string{"src/go/db.go"}); err != nil {
		t.Fatalf("resolve again: %v", err)
	}
	if got := snapshot(); got != first {
		t.Fatalf("resolution not idempotent:\nfirst:\n%s\nagain:\n%s", first, got)
	}
	assertUnresolvedNoMetadata(t, f, external)
	assertResolvedWithStrategy(t, f, member, want, ResolutionStrategyDotTail2)
}

// TestFindCallersMemberCallClaims pins the caller-side name evidence: an
// unresolved dst_name claims a target only when one spelling extends the
// other at a separator boundary.
func TestFindCallersMemberCallClaims(t *testing.T) {
	f := newGateFixture(t)
	appFile := f.file(t, "src/go/app.go", "go")
	if _, err := insertTestSymbolKind(f.ctx, f.store, f.repoID, appFile, "Close", "cli.App.Close", "function", "App", "go"); err != nil {
		t.Fatalf("insert method: %v", err)
	}

	callers := []struct {
		srcName string
		dstName string
		claimed bool
	}{
		{"ExternalReceiver", "rows.Close", false},    // foreign qualifier: never a caller of cli.App.Close
		{"ScopedForeign", "ns::Close", false},        // foreign scope qualifier
		{"MemberSpelling", "App.Close", false},       // unresolved suffix remains evidence
		{"FullSpelling", "cli.App.Close", false},     // unresolved exact spelling remains evidence
		{"DeeperSpelling", "x.cli.App.Close", false}, // unresolved extension remains evidence
		// Bare short name: Go spells a method call through a receiver, so a bare
		// `Close` in package `callers` names nothing in package `cli` (P22.6).
		{"BareSpelling", "Close", false},
	}
	srcFile := f.file(t, "src/go/callers.go", "go")
	bySrc := map[string]int64{}
	for _, c := range callers {
		src := f.symbol(t, srcFile, c.srcName, "callers."+c.srcName, "go")
		f.edge(t, srcFile, src, c.dstName)
		bySrc[c.srcName] = src
	}

	got, err := f.store.FindCallers(f.ctx, f.repoID, "cli.App.Close", 0, 100, 0)
	if err != nil {
		t.Fatalf("FindCallers: %v", err)
	}
	claimed := map[string]bool{}
	for _, sym := range got {
		claimed[sym.Name] = true
	}
	for _, c := range callers {
		if claimed[c.srcName] != c.claimed {
			t.Errorf("caller %s (dst %q): claimed = %v, want %v", c.srcName, c.dstName, claimed[c.srcName], c.claimed)
		}
	}
}

// TestFindCalleesMemberCallFallback pins the callee-side name evidence for
// unresolved member spellings.
func TestFindCalleesMemberCallFallback(t *testing.T) {
	f := newGateFixture(t)
	appFile := f.file(t, "src/go/app.go", "go")
	if _, err := insertTestSymbolKind(f.ctx, f.store, f.repoID, appFile, "Close", "cli.App.Close", "function", "App", "go"); err != nil {
		t.Fatalf("insert cli method: %v", err)
	}
	repoFile := f.file(t, "src/go/repo.go", "go")
	if _, err := insertTestSymbolKind(f.ctx, f.store, f.repoID, repoFile, "Close", "store.Repo.Close", "function", "Repo", "go"); err != nil {
		t.Fatalf("insert repo method: %v", err)
	}

	srcFile := f.file(t, "src/go/db.go", "go")
	src := f.symbol(t, srcFile, "Query", "db.Query", "go")
	f.edge(t, srcFile, src, "rows.Close") // unknown receiver: no callee may be fabricated
	f.edge(t, srcFile, src, "App.Close")  // qualifier confirmed by cli.App.Close only
	f.edge(t, srcFile, src, "d.Query")    // unknown receiver, tail spells the caller itself

	got, err := f.store.FindCallees(f.ctx, f.repoID, "db.Query", 0, 100, 0)
	if err != nil {
		t.Fatalf("FindCallees: %v", err)
	}
	var names []string
	for _, sym := range got {
		names = append(names, sym.QualifiedName)
	}
	if want := "[]"; fmt.Sprint(names) != want {
		t.Fatalf("FindCallees = %v, want %s", names, want)
	}
}

// TestFindCalleesResolvedOnlyLegDeduplicates: when every name-evidence leg is
// empty, the resolved-edge branch is the only branch in the candidate CTE and
// no UNION runs -- two resolved edges to the same destination must still yield
// one row.
func TestFindCalleesResolvedOnlyLegDeduplicates(t *testing.T) {
	f := newGateFixture(t)
	defs := f.file(t, "src/go/util.go", "go")
	dst := f.symbol(t, defs, "safeLimit", "util.safeLimit", "go")
	srcFile := f.file(t, "src/go/db.go", "go")
	src := f.symbol(t, srcFile, "Query", "db.Query", "go")
	for line := 1; line <= 2; line++ {
		if _, err := f.store.db.ExecContext(f.ctx, `
			INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line)
			VALUES(?, ?, ?, 'safeLimit', 'call', '', ?, ?)`, f.repoID, src, dst, srcFile, line); err != nil {
			t.Fatalf("insert resolved edge: %v", err)
		}
	}
	got, err := f.store.FindCallees(f.ctx, f.repoID, "db.Query", 0, 100, 0)
	if err != nil {
		t.Fatalf("FindCallees: %v", err)
	}
	if len(got) != 1 || got[0].QualifiedName != "util.safeLimit" {
		names := make([]string, 0, len(got))
		for _, sym := range got {
			names = append(names, sym.QualifiedName)
		}
		t.Fatalf("FindCallees = %v, want exactly one util.safeLimit", names)
	}
}

// TestFindCalleesMemberSpellingIsLiteral: a member spelling's '_' and '%'
// bytes are evidence, not LIKE wildcards -- `config.load_config` must not
// claim an identity spelled `x.config.loadXconfig`, which the wildcard
// reading would accept and the caller-side legs would never claim back.
func TestFindCalleesMemberSpellingIsLiteral(t *testing.T) {
	f := newGateFixture(t)
	defs := f.file(t, "src/py/defs.py", "python")
	f.symbol(t, defs, "loadXconfig", "x.config.loadXconfig", "python")
	f.symbol(t, defs, "load_config", "x.config.load_config", "python")

	caller := f.file(t, "src/py/caller.py", "python")
	src := f.symbol(t, caller, "caller", "caller", "python")
	f.edge(t, caller, src, "config.load_config")

	got, err := f.store.FindCallees(f.ctx, f.repoID, "caller", 0, 100, 0)
	if err != nil {
		t.Fatalf("FindCallees: %v", err)
	}
	if len(got) != 0 {
		names := make([]string, 0, len(got))
		for _, sym := range got {
			names = append(names, sym.QualifiedName)
		}
		t.Fatalf("FindCallees = %v, want no unresolved relationship", names)
	}
}

// TestMigration023ClearsMemberBareTailBindings: an existing database carries
// bindings the retired member bare-tail fallback wrote; re-applying migration
// 023 must clear exactly that class -- dotted slash-free bare_tail rows --
// and leave bare, import-path, and dot_tail2 bindings untouched.
//
// P22.8 note: this resets version 23 only, so 027 stays recorded and does not
// re-run. On a real upgrade 027 runs after 023 and does clear `slashKept` (an
// import-path spelling degraded to its tail is exactly the class 027 retires)
// and relabels `bareKept`. What this test pins is narrower and still true: 023
// must not touch either of them for the MEMBER rule's reason.
func TestMigration023ClearsMemberBareTailBindings(t *testing.T) {
	f := newGateFixture(t)
	defs := f.file(t, "src/go/app.go", "go")
	dst := f.symbol(t, defs, "Close", "app.App.Close", "go")
	caller := f.file(t, "src/go/db.go", "go")
	src := f.symbol(t, caller, "Query", "db.Query", "go")

	insertBound := func(dstName, strategy string) int64 {
		res, err := f.store.db.ExecContext(f.ctx, `
			INSERT INTO edges(repo_id, src_symbol_id, dst_symbol_id, dst_name, edge_kind, evidence, file_id, line,
				resolution_strategy, resolution_confidence)
			VALUES(?, ?, ?, ?, 'call', '', ?, 1, ?, ?)`,
			f.repoID, src, dst, dstName, caller, strategy, resolutionConfidenceFor(strategy))
		if err != nil {
			t.Fatalf("insert bound edge %q: %v", dstName, err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			t.Fatalf("LastInsertId: %v", err)
		}
		return id
	}
	legacyMember := insertBound("rows.Close", ResolutionStrategyBareTail)
	bareKept := insertBound("Close", ResolutionStrategyBareTail)
	slashKept := insertBound("github.com/org/repo/app.Close", ResolutionStrategyBareTail)
	tail2Kept := insertBound("App.Close", ResolutionStrategyDotTail2)

	if _, err := f.store.db.ExecContext(f.ctx, `DELETE FROM schema_migrations WHERE version = 23`); err != nil {
		t.Fatalf("reset migration 23: %v", err)
	}
	if err := f.store.Migrate(); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	assertUnresolvedNoMetadata(t, f, legacyMember)
	assertResolvedWithStrategy(t, f, bareKept, dst, ResolutionStrategyBareTail)
	assertResolvedWithStrategy(t, f, slashKept, dst, ResolutionStrategyBareTail)
	assertResolvedWithStrategy(t, f, tail2Kept, dst, ResolutionStrategyDotTail2)
}

func assertUnresolvedNoMetadata(t *testing.T, f *gateFixture, edgeID int64) {
	t.Helper()
	var dst any
	var strategy, confidence string
	if err := f.store.db.QueryRowContext(f.ctx, `
		SELECT dst_symbol_id, resolution_strategy, resolution_confidence
		FROM edges WHERE id = ?`, edgeID).Scan(&dst, &strategy, &confidence); err != nil {
		t.Fatalf("read edge %d: %v", edgeID, err)
	}
	if dst != nil {
		t.Fatalf("edge %d bound to symbol %v, want unresolved", edgeID, dst)
	}
	if strategy != "" || confidence != "" {
		t.Fatalf("edge %d carries stale metadata (%q, %q), want none", edgeID, strategy, confidence)
	}
}

func assertResolvedWithStrategy(t *testing.T, f *gateFixture, edgeID, wantDst int64, wantStrategy string) {
	t.Helper()
	var dst any
	var strategy, confidence string
	if err := f.store.db.QueryRowContext(f.ctx, `
		SELECT dst_symbol_id, resolution_strategy, resolution_confidence
		FROM edges WHERE id = ?`, edgeID).Scan(&dst, &strategy, &confidence); err != nil {
		t.Fatalf("read edge %d: %v", edgeID, err)
	}
	got, ok := dst.(int64)
	if !ok || got != wantDst {
		t.Fatalf("edge %d dst = %v, want %d", edgeID, dst, wantDst)
	}
	if strategy != wantStrategy {
		t.Fatalf("edge %d strategy = %q, want %q", edgeID, strategy, wantStrategy)
	}
	if want := resolutionConfidenceFor(wantStrategy); confidence != want {
		t.Fatalf("edge %d confidence = %q, want %q", edgeID, confidence, want)
	}
}
