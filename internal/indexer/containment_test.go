package indexer

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/store"
)

// TestContainRelPathMatrix is the lexical half of P22.31: every shape a caller
// can hand Options.Paths, and the canonical repo-relative form it must collapse
// to -- or the refusal it must produce.
func TestContainRelPathMatrix(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "repo")
	abs := func(parts ...string) string { return filepath.Join(append([]string{root}, parts...)...) }

	accepted := []struct {
		name string
		in   string
		want string
	}{
		{"relative", filepath.Join("src", "a.go"), filepath.Join("src", "a.go")},
		{"dot prefixed", "." + string(filepath.Separator) + filepath.Join("src", "a.go"), filepath.Join("src", "a.go")},
		{"interior traversal", filepath.Join("src", "x", "..", "a.go"), filepath.Join("src", "a.go")},
		{"absolute inside root", abs("src", "a.go"), filepath.Join("src", "a.go")},
		{"absolute root itself", root, "."},
		{"missing file kept for deletion", filepath.Join("src", "deleted.go"), filepath.Join("src", "deleted.go")},
		{"dotfile is not traversal", ".golangci.yml", ".golangci.yml"},
		// Prefix confusion: /repo2 is a different repository, but /repo/foobar
		// is genuinely inside /repo. A naive HasPrefix check conflates them.
		{"sibling-looking name inside root", abs("foobar", "a.go"), filepath.Join("foobar", "a.go")},
	}
	for _, tc := range accepted {
		t.Run("accept/"+tc.name, func(t *testing.T) {
			got, err := containRelPath(root, tc.in)
			if err != nil {
				t.Fatalf("containRelPath(%q) error = %v, want accept", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("containRelPath(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}

	rejected := []struct {
		name string
		in   string
	}{
		{"bare parent", ".."},
		{"parent traversal", filepath.Join("..", "x.go")},
		{"traversal through interior", filepath.Join("src", "..", "..", "x.go")},
		{"outside sibling repo", filepath.Join("..", "repo-b", "secret.go")},
		{"absolute outside", filepath.Join(string(filepath.Separator), "repo-b", "secret.go")},
		// Prefix confusion in the other direction: /repo2 must never be
		// accepted just because "/repo2" starts with "/repo".
		{"absolute prefix-confusable sibling", filepath.Join(string(filepath.Separator), "repo2", "a.go")},
		{"missing file outside root", filepath.Join("..", "repo-b", "deleted.go")},
		{"empty", ""},
		{"blank", "   "},
	}
	for _, tc := range rejected {
		t.Run("reject/"+tc.name, func(t *testing.T) {
			got, err := containRelPath(root, tc.in)
			if err == nil {
				t.Fatalf("containRelPath(%q) = %q, want rejection", tc.in, got)
			}
			if !errors.Is(err, ErrPathOutsideRepo) {
				t.Fatalf("containRelPath(%q) error = %v, want ErrPathOutsideRepo", tc.in, err)
			}
			var scopeErr *PathScopeError
			if !errors.As(err, &scopeErr) {
				t.Fatalf("containRelPath(%q) error is not *PathScopeError: %v", tc.in, err)
			}
			if scopeErr.RepoRoot != root {
				t.Fatalf("PathScopeError.RepoRoot = %q, want %q", scopeErr.RepoRoot, root)
			}
		})
	}
}

// TestContainRelPathWindowsShapes pins the platform-native traversal and volume
// shapes that a POSIX-only reading of the algorithm would miss.
func TestContainRelPathWindowsShapes(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows path semantics")
	}
	root := `C:\repo`
	for _, in := range []string{
		`..\repo-b\secret.go`,
		`src\..\..\x.go`,
		`D:\repo-b\secret.go`, // different volume: Rel must fail, not fall through
		`C:a.go`,              // volume-relative, neither absolute nor repo-relative
		`C:\repo2\a.go`,       // prefix-confusable sibling
	} {
		if got, err := containRelPath(root, in); err == nil {
			t.Errorf("containRelPath(%q) = %q, want rejection", in, got)
		}
	}
	// Case difference must not falsely identify a path as contained; rejecting
	// is the safe direction.
	if got, err := containRelPath(root, `C:\repo\src\a.go`); err != nil || got != `src\a.go` {
		t.Errorf("containRelPath(exact case) = %q, %v; want src\\a.go", got, err)
	}
}

// TestContainCandidatesDedupes pins P22.31 item 15: the shapes that name one
// repository path collapse to one candidate, in first-seen order.
func TestContainCandidatesDedupes(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "repo")
	got, err := containCandidates(root, []string{
		filepath.Join("src", "a.go"),
		"." + string(filepath.Separator) + filepath.Join("src", "a.go"),
		filepath.Join("src", "x", "..", "a.go"),
		filepath.Join(root, "src", "a.go"),
		filepath.Join("src", "b.go"),
	})
	if err != nil {
		t.Fatalf("containCandidates() error = %v", err)
	}
	want := []string{filepath.Join("src", "a.go"), filepath.Join("src", "b.go")}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("containCandidates() = %v, want %v", got, want)
	}
}

func TestContainCandidatesRefusesWholeRun(t *testing.T) {
	root := filepath.Join(string(filepath.Separator), "repo")
	if _, err := containCandidates(root, []string{
		filepath.Join("src", "a.go"),
		filepath.Join("..", "repo-b", "secret.go"),
	}); !errors.Is(err, ErrPathOutsideRepo) {
		t.Fatalf("containCandidates() error = %v, want ErrPathOutsideRepo", err)
	}
}

// escapeEnv builds sibling repositories A (active) and B (must stay untouched),
// with A already indexed.
func escapeEnv(t *testing.T) (*store.Store, *Indexer, int64, string, string) {
	t.Helper()
	ctx := context.Background()
	parent := t.TempDir()
	repoA := filepath.Join(parent, "repo-a")
	repoB := filepath.Join(parent, "repo-b")
	for _, dir := range []string{repoA, repoB} {
		if err := os.MkdirAll(filepath.Join(dir, "src"), 0o755); err != nil {
			t.Fatalf("MkdirAll(%q) error = %v", dir, err)
		}
	}
	writeFile(t, filepath.Join(repoA, "src", "a.go"), "package src\n\nfunc InsideA() {}\n")
	writeFile(t, filepath.Join(repoB, "secret.go"), "package repob\n\nfunc OutsideBSecret() {}\n")

	s, err := store.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() { s.Close() })
	idx := New(s, parser.NewRegistry(goparser.New()), nil)
	repo, err := s.UpsertRepo(ctx, repoA)
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	if _, err := idx.Index(ctx, Options{RepoRoot: repoA}); err != nil {
		t.Fatalf("Index(A) error = %v", err)
	}
	return s, idx, repo.ID, repoA, repoB
}

func graphState(t *testing.T, s *store.Store, repoID int64) ([]string, int) {
	t.Helper()
	ctx := context.Background()
	paths, err := s.AllFilePaths(ctx, repoID)
	if err != nil {
		t.Fatalf("AllFilePaths() error = %v", err)
	}
	scans, err := s.ListScans(ctx, repoID, 1000, 0)
	if err != nil {
		t.Fatalf("ListScans() error = %v", err)
	}
	return paths, len(scans)
}

// TestPathScopedEscapeRejectedZeroMutation is the regression for the original
// vulnerability: before P22.31 an Options.Paths candidate of
// "../repo-b/secret.go" was read, parsed, and persisted into repo A's graph
// under that literal escaping path.
//
// The exclude negation is what made it reachable in practice. Traversal used to
// be blocked only incidentally, by shouldSkipDir treating ".." as a hidden
// directory, and a repository whose ignore rules negate ".." switched that
// accidental guard off.
func TestPathScopedEscapeRejectedZeroMutation(t *testing.T) {
	ctx := context.Background()
	s, idx, repoID, repoA, repoB := escapeEnv(t)

	pathsBefore, scansBefore := graphState(t, s, repoID)

	cases := []struct {
		name string
		path string
	}{
		{"relative traversal", filepath.Join("..", "repo-b", "secret.go")},
		{"traversal through interior", filepath.Join("src", "..", "..", "repo-b", "secret.go")},
		{"absolute outside", filepath.Join(repoB, "secret.go")},
		{"missing outside", filepath.Join("..", "repo-b", "deleted.go")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := idx.Update(ctx, Options{
				RepoRoot: repoA,
				Exclude:  []string{"!.."},
				Paths:    []string{tc.path},
			})
			if !errors.Is(err, ErrPathOutsideRepo) {
				t.Fatalf("Update(paths=%q) error = %v, want ErrPathOutsideRepo", tc.path, err)
			}
			pathsAfter, scansAfter := graphState(t, s, repoID)
			if scansAfter != scansBefore {
				t.Fatalf("rejected run recorded a scan: %d -> %d", scansBefore, scansAfter)
			}
			if len(pathsAfter) != len(pathsBefore) {
				t.Fatalf("rejected run mutated files: %v -> %v", pathsBefore, pathsAfter)
			}
			for _, p := range pathsAfter {
				if p == ".." || filepath.IsAbs(p) || len(p) >= 2 && p[:2] == ".." {
					t.Fatalf("persisted escaping path %q", p)
				}
			}
		})
	}

	// Repo B must not have been opened as a repository of its own either.
	repos, err := s.ListRepos(ctx, 100, 0)
	if err != nil {
		t.Fatalf("ListRepos() error = %v", err)
	}
	if len(repos) != 1 || repos[0].RootPath != repoA {
		t.Fatalf("repos = %v, want only the active repository %q", repos, repoA)
	}
	if _, err := os.Stat(filepath.Join(repoB, "secret.go")); err != nil {
		t.Fatalf("repo B was disturbed: %v", err)
	}
}

// TestPathScopedContainedFormsStillWork is the same-repo regression: every
// legitimate shape must keep working, including a missing contained path, which
// still has to reach deletion retirement.
func TestPathScopedContainedFormsStillWork(t *testing.T) {
	ctx := context.Background()
	s, idx, repoID, repoA, _ := escapeEnv(t)

	for _, path := range []string{
		filepath.Join("src", "a.go"),
		"." + string(filepath.Separator) + filepath.Join("src", "a.go"),
		filepath.Join("src", "x", "..", "a.go"),
		filepath.Join(repoA, "src", "a.go"),
	} {
		if _, err := idx.Update(ctx, Options{RepoRoot: repoA, Paths: []string{path}}); err != nil {
			t.Fatalf("Update(paths=%q) error = %v, want success", path, err)
		}
		paths, _ := graphState(t, s, repoID)
		if len(paths) != 1 || paths[0] != filepath.Join("src", "a.go") {
			t.Fatalf("after Update(paths=%q) files = %v, want [src/a.go]", path, paths)
		}
	}

	// A contained path that no longer exists must still retire its row.
	if err := os.Remove(filepath.Join(repoA, "src", "a.go")); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	summary, err := idx.Update(ctx, Options{RepoRoot: repoA, Paths: []string{filepath.Join("src", "a.go")}})
	if err != nil {
		t.Fatalf("Update(missing contained path) error = %v", err)
	}
	if summary.FilesDeleted != 1 {
		t.Fatalf("FilesDeleted = %d, want 1 (deletion retirement regressed)", summary.FilesDeleted)
	}
}

// TestContainCandidatesRelativeRoot pins the regression a purely lexical
// containRelPath had: `serve --repo-root .` leaves the active root relative, and
// filepath.Rel refuses to relate an absolute candidate to a relative base. A
// file plainly inside the repository must not be rejected because of how the
// root was spelled on the command line.
func TestContainCandidatesRelativeRoot(t *testing.T) {
	repoRoot := t.TempDir()
	writeFile(t, filepath.Join(repoRoot, "src", "a.go"), "package src\n")

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd() error = %v", err)
	}
	if err := os.Chdir(repoRoot); err != nil {
		t.Fatalf("Chdir() error = %v", err)
	}
	t.Cleanup(func() { os.Chdir(wd) })

	// Absolute candidate, relative root: must resolve, not reject.
	got, err := containCandidates(".", []string{filepath.Join(repoRoot, "src", "a.go")})
	if err != nil {
		t.Fatalf("containCandidates(relative root, absolute contained path) error = %v", err)
	}
	if len(got) != 1 || got[0] != filepath.Join("src", "a.go") {
		t.Fatalf("containCandidates() = %v, want [src/a.go]", got)
	}

	// An outside absolute path must still be refused under a relative root.
	if _, err := containCandidates(".", []string{filepath.Join(filepath.Dir(repoRoot), "elsewhere", "x.go")}); !errors.Is(err, ErrPathOutsideRepo) {
		t.Fatalf("containCandidates(outside, relative root) error = %v, want ErrPathOutsideRepo", err)
	}
}

// TestDirectorySymlinkEscapeRejected is the second half of the path escape: a
// candidate can be lexically contained and still read an outside file, because
// filepath.Join is lexical while os.Stat and os.ReadFile follow symlinked
// directory components.
//
// The asserted invariant is consistency between the two scan modes. A full scan
// cannot reach this content -- filepath.WalkDir does not descend directory
// symlinks -- so a path-scoped run must not be able to index it either.
func TestDirectorySymlinkEscapeRejected(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on windows")
	}
	ctx := context.Background()
	parent := t.TempDir()
	repoA := filepath.Join(parent, "repo-a")
	outside := filepath.Join(parent, "outside")
	writeFile(t, filepath.Join(repoA, "src", "a.go"), "package src\n\nfunc InsideA() {}\n")
	writeFile(t, filepath.Join(outside, "secret.go"), "package outside\n\nfunc OutsideSecret() {}\n")
	if err := os.Symlink(outside, filepath.Join(repoA, "link")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	s, err := store.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	defer s.Close()
	idx := New(s, parser.NewRegistry(goparser.New()), nil)
	repo, err := s.UpsertRepo(ctx, repoA)
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}

	// A full scan does not descend the directory symlink.
	if _, err := idx.Index(ctx, Options{RepoRoot: repoA}); err != nil {
		t.Fatalf("Index() error = %v", err)
	}
	full, _ := graphState(t, s, repo.ID)
	if len(full) != 1 || full[0] != filepath.Join("src", "a.go") {
		t.Fatalf("full scan files = %v, want only src/a.go", full)
	}

	// So a path-scoped run must not reach it either.
	for _, path := range []string{
		filepath.Join("link", "secret.go"),
		filepath.Join(repoA, "link", "secret.go"),
	} {
		if _, err := idx.Update(ctx, Options{RepoRoot: repoA, Paths: []string{path}}); !errors.Is(err, ErrPathOutsideRepo) {
			t.Fatalf("Update(paths=[%q]) error = %v, want ErrPathOutsideRepo", path, err)
		}
	}
	after, _ := graphState(t, s, repo.ID)
	if len(after) != 1 || after[0] != filepath.Join("src", "a.go") {
		t.Fatalf("after rejected scoped runs files = %v, want only src/a.go", after)
	}
}

// TestSymlinkedFileStillIndexed guards the other direction: repository-contained
// symlinked *files* are repository content in both scan modes, and P22.31 must
// not quietly change that product behavior. Only the directory component is
// containment-checked.
func TestSymlinkedFileStillIndexed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on windows")
	}
	ctx := context.Background()
	parent := t.TempDir()
	repoA := filepath.Join(parent, "repo-a")
	outside := filepath.Join(parent, "outside")
	writeFile(t, filepath.Join(repoA, "src", "a.go"), "package src\n\nfunc InsideA() {}\n")
	writeFile(t, filepath.Join(outside, "linked.go"), "package src\n\nfunc LinkedTarget() {}\n")
	if err := os.Symlink(filepath.Join(outside, "linked.go"), filepath.Join(repoA, "src", "linked.go")); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	s, err := store.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	defer s.Close()
	idx := New(s, parser.NewRegistry(goparser.New()), nil)
	repo, err := s.UpsertRepo(ctx, repoA)
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	if _, err := idx.Index(ctx, Options{RepoRoot: repoA}); err != nil {
		t.Fatalf("Index() error = %v", err)
	}
	full, _ := graphState(t, s, repo.ID)
	if len(full) != 2 {
		t.Fatalf("full scan files = %v, want both src/a.go and src/linked.go", full)
	}
	// Path-scoped mode must agree with the full scan.
	if _, err := idx.Update(ctx, Options{RepoRoot: repoA, Paths: []string{filepath.Join("src", "linked.go")}}); err != nil {
		t.Fatalf("Update(symlinked file) error = %v, want success (existing contract)", err)
	}
}

// TestContainCandidatesSymlinkAliasedRoot covers the alias asymmetry: a client
// may hand back absolute paths spelled through a different alias of the same
// directory (on macOS "/tmp" is a symlink to "/private/tmp"). Those name files
// inside the repository and must be accepted, while an alias that really leaves
// the repository must still be refused.
func TestContainCandidatesSymlinkAliasedRoot(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on windows")
	}
	parent := t.TempDir()
	repoA := filepath.Join(parent, "repo-a")
	outside := filepath.Join(parent, "outside")
	writeFile(t, filepath.Join(repoA, "src", "a.go"), "package src\n")
	writeFile(t, filepath.Join(outside, "secret.go"), "package outside\n")

	aliasDir := filepath.Join(t.TempDir(), "alias")
	if err := os.Symlink(repoA, aliasDir); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}

	// Root spelled canonically, candidate spelled through the alias.
	got, err := containCandidates(repoA, []string{filepath.Join(aliasDir, "src", "a.go")})
	if err != nil {
		t.Fatalf("containCandidates(aliased candidate) error = %v, want accept", err)
	}
	if len(got) != 1 || got[0] != filepath.Join("src", "a.go") {
		t.Fatalf("containCandidates() = %v, want [src/a.go]", got)
	}

	// The alias must not become a way to reach outside the repository.
	if _, err := containCandidates(repoA, []string{filepath.Join(aliasDir, "..", "outside", "secret.go")}); !errors.Is(err, ErrPathOutsideRepo) {
		t.Fatalf("containCandidates(alias escaping) error = %v, want ErrPathOutsideRepo", err)
	}
}
