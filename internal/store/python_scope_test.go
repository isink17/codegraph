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
			// An absolute import has no single anchor, so every ancestor of the
			// importing file is offered as a root. Two of them existing at once
			// is what the resolver refuses to decide.
			name:   "absolute offers every ancestor root",
			source: "src/app/main.py", specifier: "pkg.helpers",
			want: []string{
				"src/app/pkg/helpers.py", "src/app/pkg/helpers/__init__.py",
				"src/pkg/helpers.py", "src/pkg/helpers/__init__.py",
				"pkg/helpers.py", "pkg/helpers/__init__.py",
			},
		},
		{
			name:   "absolute at the repository root",
			source: "main.py", specifier: "pkg",
			want: []string{"pkg.py", "pkg/__init__.py"},
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

func TestPythonBindingFor(t *testing.T) {
	imports := []pyScopeImport{
		{source: "pkg.helpers", imported: "load", local: "read_config", kind: "named"},
		{source: "pkg.helpers", local: "pkg.helpers", kind: "namespace"},
		{source: "other", local: "other", kind: "namespace"},
	}
	tests := []struct {
		name, call, wantSource, wantMember string
		wantClaimed                        bool
	}{
		{"alias binds exactly", "read_config", "pkg.helpers", "", true},
		{"dotted module binds its whole path", "pkg.helpers.load", "pkg.helpers", "load", true},
		{"module member", "other.run", "other", "run", true},
		{"unclaimed name", "sorted", "", "", false},
		{"prefix that is not a binding", "pkg_helpers.load", "", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, member, claimed := pythonBindingFor(imports, tc.call)
			if claimed != tc.wantClaimed {
				t.Fatalf("claimed = %v, want %v", claimed, tc.wantClaimed)
			}
			if claimed && (got.source != tc.wantSource || member != tc.wantMember) {
				t.Fatalf("binding = (%q, %q), want (%q, %q)", got.source, member, tc.wantSource, tc.wantMember)
			}
		})
	}
}

// Two imports binding one name is not a tie to break: the call is claimed, and
// nothing about it is decidable.
func TestPythonBindingForRefusesCompetingBindings(t *testing.T) {
	imports := []pyScopeImport{
		{source: "a.mod", imported: "load", local: "load", kind: "named"},
		{source: "b.mod", imported: "load", local: "load", kind: "named"},
	}
	got, _, claimed := pythonBindingFor(imports, "load")
	if !claimed {
		t.Fatal("competing bindings must still claim the edge")
	}
	if got.source != "" {
		t.Fatalf("competing bindings resolved to %q; want no decidable target", got.source)
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
		}
		counter := &countingQuerier{inner: s.db}
		bound, _, err := resolvePythonScope(ctx, counter, repo.ID, nil)
		if err != nil {
			t.Fatal(err)
		}
		if bound != callers {
			t.Fatalf("bound %d edges, want %d", bound, callers)
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

// TestPythonScopeVetoMatchesBindingClaim pins the twin. recordPythonScopeVeto
// decides in SQL who the generic strategies may not answer for; pythonBindingFor
// decides in Go which edges this pass takes responsibility for. A divergence
// would not fail anywhere else -- it would show up as a silently lost or
// silently false edge in the transactions that run the veto without the pass.
func TestPythonScopeVetoMatchesBindingClaim(t *testing.T) {
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
	file, err := insertTestFileLang(ctx, s, repo.ID, "app.py", "python")
	if err != nil {
		t.Fatal(err)
	}
	src, err := insertTestSymbolLang(ctx, s, repo.ID, file, "run", "app.run", "python")
	if err != nil {
		t.Fatal(err)
	}
	evidence := []pyScopeImport{
		{source: "pkg.helpers", imported: "load", local: "load", kind: "named"},
		{source: "pkg.helpers", local: "pkg.helpers", kind: "namespace"},
		{source: "some_pkg", imported: "load_it", local: "load_it", kind: "named"},
		{source: "star", kind: "named"}, // wildcard binds no local name
		{source: "a.mod", imported: "dup", local: "dup", kind: "named"},
		{source: "b.mod", imported: "dup", local: "dup", kind: "named"}, // competing
	}
	for _, imp := range evidence {
		if _, err := s.db.ExecContext(ctx,
			`INSERT INTO scope_import_evidence(repo_id,file_id,language,source_specifier,imported_name,local_name,import_kind,wildcard) VALUES(?,?,?,?,?,?,?,?)`,
			repo.ID, file, "python", imp.source, imp.imported, imp.local, imp.kind, boolInt(imp.local == "")); err != nil {
			t.Fatal(err)
		}
	}
	// Spellings chosen for the ways a prefix test can go wrong: an underscore
	// (which LIKE would treat as a wildcard), a longer name sharing a prefix,
	// and a member on a dotted module binding.
	names := []string{
		"load", "load.attr", "load_it", "loadx", "pkg.helpers.load", "pkg.helpersx.load",
		"dup", "only_here", "sorted", "star",
	}
	edgeName := map[int64]string{}
	for _, name := range names {
		id, err := insertTestEdge(ctx, s, repo.ID, file, src, name)
		if err != nil {
			t.Fatal(err)
		}
		edgeName[id] = name
	}

	if _, err := s.db.ExecContext(ctx, `CREATE TEMP TABLE IF NOT EXISTS `+pyScopeVeto+`(edge_id INTEGER PRIMARY KEY) WITHOUT ROWID`); err != nil {
		t.Fatal(err)
	}
	defer func() { _, _ = s.db.ExecContext(ctx, `DROP TABLE IF EXISTS temp.`+pyScopeVeto) }()
	if err := recordPythonScopeVeto(ctx, s.db, repo.ID); err != nil {
		t.Fatal(err)
	}
	gotSQL := map[string]bool{}
	rows, err := s.db.QueryContext(ctx, `SELECT edge_id FROM `+pyScopeVeto)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			t.Fatal(err)
		}
		gotSQL[edgeName[id]] = true
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}

	wantGo := map[string]bool{}
	for _, name := range names {
		if _, _, claimed := pythonBindingFor(evidence, name); claimed {
			wantGo[name] = true
		}
	}
	if !reflect.DeepEqual(gotSQL, wantGo) {
		t.Fatalf("veto set = %v, binding claims = %v", gotSQL, wantGo)
	}
	// The twin is only interesting if it actually separates the spellings.
	for _, name := range []string{"load", "load.attr", "load_it", "pkg.helpers.load", "dup"} {
		if !wantGo[name] {
			t.Fatalf("%q should be claimed", name)
		}
	}
	for _, name := range []string{"loadx", "pkg.helpersx.load", "only_here", "sorted"} {
		if wantGo[name] {
			t.Fatalf("%q must not be claimed", name)
		}
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
	bound, handled, err := resolvePythonScope(ctx, s.db, repo.ID, nil)
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
