package store

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
)

func TestPythonModuleCandidatePaths(t *testing.T) {
	tests := []struct {
		name, source, specifier string
		want                    []string
	}{
		{
			// Absolute imports are anchored at the repository root and nowhere
			// else -- never the caller's own directory.
			name:   "absolute against the repository root",
			source: "src/app/main.py", specifier: "pkg.helpers",
			want: []string{"pkg/helpers.py", "pkg/helpers/__init__.py"},
		},
		{
			// A src layout is not a proven import root, so the only candidates
			// are the repository-root ones -- which do not exist in such a tree.
			name:   "src layout gets no extra root",
			source: "src/app/main.py", specifier: "app.helpers",
			want: []string{"app/helpers.py", "app/helpers/__init__.py"},
		},
		{
			name:   "relative one level is anchored at the importing package",
			source: "pkg/app.py", specifier: ".helpers",
			want: []string{"pkg/helpers.py", "pkg/helpers/__init__.py"},
		},
		{
			name:   "relative two levels climbs once",
			source: "pkg/deep/app.py", specifier: "..common.helpers",
			want: []string{"pkg/common/helpers.py", "pkg/common/helpers/__init__.py"},
		},
		{
			name:   "bare relative names a package, never a module file",
			source: "pkg/app.py", specifier: ".",
			want: []string{"pkg/__init__.py"},
		},
		{
			name:   "relative above the repository root has no candidate",
			source: "app.py", specifier: "..helpers",
			want: nil,
		},
		{
			name:   "malformed specifier has no candidate",
			source: "app.py", specifier: "pkg..helpers",
			want: nil,
		},
		{name: "empty specifier", source: "app.py", specifier: "", want: nil},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := pythonModuleCandidatePaths(tc.source, tc.specifier); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("pythonModuleCandidatePaths(%q, %q) = %v, want %v", tc.source, tc.specifier, got, tc.want)
			}
		})
	}
}

// Module identity is a path. Two modules sharing a basename must never share a
// candidate, whichever direction the import is written in.
func TestPythonModuleCandidatePathsSeparateSameBasename(t *testing.T) {
	foo := pythonModuleCandidatePaths("app.py", "foo.utils")
	bar := pythonModuleCandidatePaths("app.py", "bar.utils")
	for _, a := range foo {
		for _, b := range bar {
			if a == b {
				t.Fatalf("foo.utils and bar.utils share candidate %q", a)
			}
		}
	}
}

// `import a.b` binds `a`, not `a.b`, and the resolver derives both spellings
// that reach a module from that truthful record rather than from a corrupted
// LocalName.
func TestPythonBindingsOfNamespaceImports(t *testing.T) {
	tests := []struct {
		name string
		imp  pyScopeImport
		want []pyBinding
	}{
		{
			name: "unaliased dotted import binds the package and reaches the module",
			imp:  pyScopeImport{source: "a.b", imported: "a", local: "a", kind: "namespace"},
			want: []pyBinding{
				{prefix: "a.b", kind: "namespace", source: "a.b"},
				{prefix: "a", kind: "namespace", source: "a"},
			},
		},
		{
			name: "aliased dotted import binds only the alias",
			imp:  pyScopeImport{source: "a.b", imported: "a.b", local: "x", kind: "namespace"},
			want: []pyBinding{{prefix: "x", kind: "namespace", source: "a.b"}},
		},
		{
			name: "alias equal to the package is still an alias",
			imp:  pyScopeImport{source: "a.b", imported: "a.b", local: "a", kind: "namespace"},
			want: []pyBinding{{prefix: "a", kind: "namespace", source: "a.b"}},
		},
		{
			name: "plain import binds one spelling",
			imp:  pyScopeImport{source: "a", imported: "a", local: "a", kind: "namespace"},
			want: []pyBinding{{prefix: "a", kind: "namespace", source: "a"}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := pythonBindingsOf([]pyScopeImport{tc.imp}); !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("pythonBindingsOf(%+v) = %+v, want %+v", tc.imp, got, tc.want)
			}
		})
	}
}

func TestPythonBindingFor(t *testing.T) {
	bindings := pythonBindingsOf([]pyScopeImport{
		{source: "pkg.helpers", imported: "load", local: "read_config", kind: "named"},
		{source: "pkg.helpers", imported: "pkg", local: "pkg", kind: "namespace"},
		{source: "other", imported: "other", local: "other", kind: "namespace"},
		{source: "local.mod", imported: "local.mod", local: "read_config", kind: "named", owner: "run"},
	})
	tests := []struct {
		name, at, call, wantSource, wantMember string
		wantClaimed                            bool
	}{
		{"alias binds exactly", "", "read_config", "pkg.helpers", "", true},
		{"dotted module binds its whole path", "", "pkg.helpers.load", "pkg.helpers", "load", true},
		{"dotted module also binds its package", "", "pkg.other", "pkg", "other", true},
		{"module member", "", "other.run", "other", "run", true},
		{"unclaimed name", "", "sorted", "", "", false},
		{"prefix that is not a binding", "", "pkg_helpers.load", "", "", false},
		// A function-local import claims the name -- so no weaker strategy may
		// answer it -- but binds nothing itself, since nothing proves the
		// import statement ran before the call.
		{"a nearer import claims without binding", "run", "read_config", "", "", true},
		{"a function-local import is invisible elsewhere", "other_fn", "read_config", "pkg.helpers", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, member, claimed := pythonBindingFor(bindings, tc.at, tc.call)
			if claimed != tc.wantClaimed {
				t.Fatalf("claimed = %v, want %v", claimed, tc.wantClaimed)
			}
			if claimed && (got.source != tc.wantSource || member != tc.wantMember) {
				t.Fatalf("binding = (%q, %q), want (%q, %q)", got.source, member, tc.wantSource, tc.wantMember)
			}
		})
	}
}

// Two imports binding one name at one distance is not a tie to break: the call
// is claimed, and nothing about it is decidable.
func TestPythonBindingForRefusesCompetingBindings(t *testing.T) {
	bindings := pythonBindingsOf([]pyScopeImport{
		{source: "a.mod", imported: "load", local: "load", kind: "named"},
		{source: "b.mod", imported: "load", local: "load", kind: "named"},
	})
	got, _, claimed := pythonBindingFor(bindings, "", "load")
	if !claimed {
		t.Fatal("competing bindings must still claim the edge")
	}
	if got.source != "" {
		t.Fatalf("competing bindings resolved to %q; want no decidable target", got.source)
	}
}

func TestPythonNameIsShadowed(t *testing.T) {
	locals := []pyScopeLocal{
		{owner: "run", name: "h"},
		{owner: "", name: "cfg"},
		{owner: "outer", name: "p"},
	}
	tests := []struct {
		name, at, bound, importOwner string
		claimed                      bool
		want                         bool
	}{
		{"parameter shadows a module import", "run", "h", "", true, true},
		{"an unrelated scope does not shadow", "other", "h", "", true, false},
		{"an enclosing scope shadows a nested call", "outer.inner", "p", "", true, true},
		{"a nearer import beats a module-level binding", "run", "cfg", "run", true, false},
		{"a module binding shadows a module import", "run", "cfg", "", true, true},
		{"an unclaimed name is shadowed by any visible binding", "run", "h", "", false, true},
		{"nothing binds it", "run", "other_name", "", true, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := pythonNameIsShadowed(locals, tc.at, tc.bound, tc.importOwner, tc.claimed); got != tc.want {
				t.Fatalf("pythonNameIsShadowed(%q, %q, %q, %v) = %v, want %v", tc.at, tc.bound, tc.importOwner, tc.claimed, got, tc.want)
			}
		})
	}
}

// countingQuerier records how many statements a pass issues, so a future change
// that reintroduces a per-edge query fails here rather than in production.
type countingQuerier struct {
	inner   execQuerier
	queries atomic.Int64
	execs   atomic.Int64
}

func (c *countingQuerier) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	c.queries.Add(1)
	return c.inner.QueryContext(ctx, query, args...)
}

func (c *countingQuerier) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	c.execs.Add(1)
	return c.inner.ExecContext(ctx, query, args...)
}

// TestPythonScopeQueryShapeIsBatched pins the pass's cost to the evidence it
// reads, not to the number of edges it decides: doubling the callers must not
// change the statement count.
func TestPythonScopeQueryShapeIsBatched(t *testing.T) {
	count := func(callers int) (int64, int64) {
		ctx := context.Background()
		s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		repo, err := s.UpsertRepo(ctx, t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		target, err := insertTestFileLang(ctx, s, repo.ID, "pkg/helpers.py", "python")
		if err != nil {
			t.Fatal(err)
		}
		// The package itself exists, so the submodule-arm callers below make the
		// pass read its module-level bindings -- that read is counted too.
		if _, err := insertTestFileLang(ctx, s, repo.ID, "pkg/__init__.py", "python"); err != nil {
			t.Fatal(err)
		}
		if _, err := insertTestSymbolLang(ctx, s, repo.ID, target, "load", "helpers.load", "python"); err != nil {
			t.Fatal(err)
		}
		for i := range callers {
			path := "caller" + string(rune('a'+i%26)) + string(rune('a'+i/26)) + ".py"
			file, err := insertTestFileLang(ctx, s, repo.ID, path, "python")
			if err != nil {
				t.Fatal(err)
			}
			src, err := insertTestSymbolLang(ctx, s, repo.ID, file, "run", "caller.run", "python")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := s.db.ExecContext(ctx,
				`INSERT INTO scope_import_evidence(repo_id,file_id,language,source_specifier,imported_name,local_name,import_kind,wildcard) VALUES(?,?,?,?,?,?,?,0)`,
				repo.ID, file, "python", "pkg.helpers", "load", "load", "named"); err != nil {
				t.Fatal(err)
			}
			if _, err := insertTestEdge(ctx, s, repo.ID, file, src, "load"); err != nil {
				t.Fatal(err)
			}
			// A submodule-arm caller as well, so the package-namespace check
			// this pass makes is counted too.
			if _, err := s.db.ExecContext(ctx,
				`INSERT INTO scope_import_evidence(repo_id,file_id,language,source_specifier,imported_name,local_name,import_kind,wildcard) VALUES(?,?,?,?,?,?,?,0)`,
				repo.ID, file, "python", "pkg", "helpers", "helpers", "named"); err != nil {
				t.Fatal(err)
			}
			if _, err := insertTestEdge(ctx, s, repo.ID, file, src, "helpers.load"); err != nil {
				t.Fatal(err)
			}
		}
		counter := &countingQuerier{inner: s.db}
		bound, _, err := resolvePythonScope(ctx, counter, repo.ID, nil, false)
		if err != nil {
			t.Fatal(err)
		}
		if bound != 2*callers {
			t.Fatalf("bound %d edges, want %d", bound, 2*callers)
		}
		return counter.queries.Load(), counter.execs.Load()
	}
	smallQ, smallE := count(4)
	largeQ, largeE := count(8)
	if smallQ != largeQ || smallE != largeE {
		t.Fatalf("statement count grew with edge count: 4 callers = %d queries/%d execs, 8 callers = %d/%d",
			smallQ, smallE, largeQ, largeE)
	}
}

// TestPythonScopeClaimsSurviveIntoTheWeakStrategy pins what the veto is for:
// resolveDotSuffixIncrementally runs a repo-wide strategy without running the
// Python pass, and must still refuse an edge the calling file's own import
// already gave a meaning.
func TestPythonScopeClaimsSurviveIntoTheWeakStrategy(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	repo, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// A decoy whose qualified name ends in the spelling the call uses. Nothing
	// imports it, and the caller's import names a module that has no `load`.
	decoy, err := insertTestFileLang(ctx, s, repo.ID, "decoy.py", "python")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := insertTestSymbolLang(ctx, s, repo.ID, decoy, "load", "decoy.helpers.load", "python"); err != nil {
		t.Fatal(err)
	}
	target, err := insertTestFileLang(ctx, s, repo.ID, "pkg/helpers.py", "python")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := insertTestSymbolLang(ctx, s, repo.ID, target, "other", "helpers.other", "python"); err != nil {
		t.Fatal(err)
	}
	caller, err := insertTestFileLang(ctx, s, repo.ID, "app.py", "python")
	if err != nil {
		t.Fatal(err)
	}
	src, err := insertTestSymbolLang(ctx, s, repo.ID, caller, "run", "app.run", "python")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO scope_import_evidence(repo_id,file_id,language,source_specifier,imported_name,local_name,import_kind,wildcard,owner_module) VALUES(?,?,?,?,?,?,?,0,'')`,
		repo.ID, caller, "python", "pkg.helpers", "pkg", "pkg", "namespace"); err != nil {
		t.Fatal(err)
	}
	edge, err := insertTestEdge(ctx, s, repo.ID, caller, src, "pkg.helpers.load")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.ResolveEdges(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.resolveDotSuffixIncrementally(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	var got sql.NullInt64
	if err := s.db.QueryRow(`SELECT dst_symbol_id FROM edges WHERE id=?`, edge).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if got.Valid {
		t.Fatalf("a claimed edge was bound by the weak strategy to symbol %d", got.Int64)
	}
}

// The generic strategies must keep every Python edge this pass did not take
// responsibility for: an unclaimed, undeclared bare name is not withheld.
func TestPythonScopeLeavesUnclaimedEdgesToGenericStrategies(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	repo, err := s.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target, err := insertTestFileLang(ctx, s, repo.ID, "helpers.py", "python")
	if err != nil {
		t.Fatal(err)
	}
	want, err := insertTestSymbolLang(ctx, s, repo.ID, target, "normalize", "helpers.normalize", "python")
	if err != nil {
		t.Fatal(err)
	}
	caller, err := insertTestFileLang(ctx, s, repo.ID, "app.py", "python")
	if err != nil {
		t.Fatal(err)
	}
	src, err := insertTestSymbolLang(ctx, s, repo.ID, caller, "run", "app.run", "python")
	if err != nil {
		t.Fatal(err)
	}
	edge, err := insertTestEdge(ctx, s, repo.ID, caller, src, "normalize")
	if err != nil {
		t.Fatal(err)
	}
	bound, handled, err := resolvePythonScope(ctx, s.db, repo.ID, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if bound != 0 {
		t.Fatalf("bound %d edges without import or module evidence, want 0", bound)
	}
	if _, withheld := handled[edge]; withheld {
		t.Fatal("an unclaimed bare call must stay available to the generic strategies")
	}
	if _, err := s.ResolveEdges(ctx, repo.ID); err != nil {
		t.Fatal(err)
	}
	var got sql.NullInt64
	if err := s.db.QueryRow(`SELECT dst_symbol_id FROM edges WHERE id=?`, edge).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if !got.Valid || got.Int64 != want {
		t.Fatalf("generic resolution = %v, want %d", got, want)
	}
}
