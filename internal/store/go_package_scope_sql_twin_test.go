package store

import (
	"context"
	"path/filepath"
	"testing"
)

// The package-scope rule is enforced twice -- once in SQL by the repo-wide
// resolver and the public queries, once in Go by the binder and the query-side
// helpers. Drift between the two is exactly the full-vs-incremental divergence
// this phase exists to remove, so the derivations are evaluated side by side on
// the inputs most likely to separate them: literal backslashes in filenames,
// dotted directory names, repository-root files, receivers, and package names
// that are a prefix of a directory name.
//
// files.path is the logical repository identity (P23): '/' is its only
// separator and '\' is filename data. `internal\store\store.go` is therefore
// one repository-root file whose directory prefix is "", and `b/x\y.go` sits in
// `b/` beside `b/z.go`, not in `b/x/` beside `b/x/y.go`.
func TestGoPackageScopeSQLMatchesGo(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	ctx := context.Background()

	cases := []struct {
		path          string
		qualifiedName string
		name          string
	}{
		{"a.go", "main.run", "run"},
		{"internal/store/store.go", "store.Open", "Open"},
		{`internal\store\store.go`, "store.Open", "Open"},
		{`b/x\y.go`, "b.Run", "Run"},
		{"b/z.go", "b.Run", "Run"},
		{"b/x/y.go", "x.Run", "Run"},
		{"main.go", "main.run", "run"},
		{"a.b/c.d/e.go", "e.Helper", "Helper"},
		{"internal/store/store.go", "store.Store.Ping", "Ping"},
		{"internal/store/store.go", "store.*Store.Ping", "Ping"},
		{"internal/store/store_test.go", "store_test.TestX", "TestX"},
		{"mai/n.go", "main.X", "X"},
		{"", "pkg.X", "X"},
		{"pkg/x.go", "nodot", "nodot"},
		{"pkg/x.go", ".leading", "leading"},
	}

	// The logical contract the twins must agree on, pinned on the Go side so a
	// change that keeps them equal but wrong is still caught.
	wantDir := map[string]string{
		"a.go":                    "",
		"internal/store/store.go": "internal/store/",
		`internal\store\store.go`: "",
		`b/x\y.go`:                "b/",
		"b/z.go":                  "b/",
		"b/x/y.go":                "b/x/",
	}

	for _, tc := range cases {
		if want, pinned := wantDir[tc.path]; pinned {
			if got := storedPathDir(tc.path); got != want {
				t.Errorf("storedPathDir(%q) = %q, want %q", tc.path, got, want)
			}
		}
		var sqlKey, sqlDir, sqlPkg string
		var sqlPackageLevel int
		// The builders repeat their column expression, so the inputs arrive as
		// columns of a one-row subquery rather than as positional parameters.
		query := `SELECT ` +
			sqlGoPackageScopeKey("s.path", "s.qualified_name") + `, ` +
			sqlStoredPathDir("s.path") + `, ` +
			sqlGoPackageNameOf("s.qualified_name") + `, ` +
			`(CASE WHEN ` + sqlGoPackageLevelDeclaration("s") + ` THEN 1 ELSE 0 END)
			 FROM (SELECT ? AS path, ? AS qualified_name, ? AS name) s`
		if err := s.db.QueryRowContext(ctx, query,
			tc.path, tc.qualifiedName, tc.name,
		).Scan(&sqlKey, &sqlDir, &sqlPkg, &sqlPackageLevel); err != nil {
			t.Fatalf("%+v: query: %v", tc, err)
		}

		if got := goPackageScopeKey(tc.path, tc.qualifiedName); got != sqlKey {
			t.Errorf("%+v: goPackageScopeKey = %q, SQL = %q", tc, got, sqlKey)
		}
		if got := storedPathDir(tc.path); got != sqlDir {
			t.Errorf("%+v: storedPathDir = %q, SQL = %q", tc, got, sqlDir)
		}
		if got := goPackageNameOf(tc.qualifiedName); got != sqlPkg {
			t.Errorf("%+v: goPackageNameOf = %q, SQL = %q", tc, got, sqlPkg)
		}
		if got := goPackageLevelDeclaration(tc.qualifiedName, tc.name); got != (sqlPackageLevel == 1) {
			t.Errorf("%+v: goPackageLevelDeclaration = %v, SQL = %v", tc, got, sqlPackageLevel == 1)
		}
	}
}

// The scope key concatenates a directory prefix and a package name with no
// separator, which is only unambiguous because a non-empty directory prefix
// always ends in '/' and a Go package name never contains one. These are the
// pairs that would collide if that reasoning were wrong.
func TestGoPackageScopeKeysDoNotCollide(t *testing.T) {
	cases := []struct{ aPath, aQname, bPath, bQname string }{
		{"main.go", "main.X", "mai/n.go", "n.X"},
		{"a/b.go", "b.X", "a/b/c.go", "c.X"},
		{"internal/store/x.go", "store.X", "internal/store/x_test.go", "store_test.X"},
		{"internal/x.go", "store.X", "internal/store/x.go", "X.Y"},
	}
	for _, tc := range cases {
		a := goPackageScopeKey(tc.aPath, tc.aQname)
		b := goPackageScopeKey(tc.bPath, tc.bQname)
		if a == b {
			t.Errorf("scope keys collide: (%s,%s) and (%s,%s) both -> %q",
				tc.aPath, tc.aQname, tc.bPath, tc.bQname, a)
		}
	}
}

// goBareCallName governs which spellings the rule applies to at all. The
// qualifier-bearing forms belong to P22.1 (dot/scope tails) and P22.5
// (import paths) and must not be pulled into this one.
func TestGoBareCallNameClassification(t *testing.T) {
	bare := []string{"Helper", "countTags", "len", "_x", "X9"}
	notBare := []string{"", "pkg.Helper", "rows.Close", "ns::Close", "github.com/org/pkg.Helper", "a/b"}
	for _, name := range bare {
		if !goBareCallName(name) {
			t.Errorf("goBareCallName(%q) = false, want true", name)
		}
	}
	for _, name := range notBare {
		if goBareCallName(name) {
			t.Errorf("goBareCallName(%q) = true, want false", name)
		}
	}
}
