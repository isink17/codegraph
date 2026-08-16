package indexer

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
	"time"

	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/store"
)

// symlink creates a link, skipping the test when the platform refuses (native
// Windows without the developer-mode / SeCreateSymbolicLink privilege). The
// traversal-failure tests below are deliberately symlink-free so the fatal-scan
// rules stay covered on every platform.
func symlink(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		if runtime.GOOS == "windows" {
			t.Skipf("os.Symlink unavailable: %v", err)
		}
		t.Fatalf("os.Symlink(%q, %q) error = %v", target, link, err)
	}
}

// indexedPaths runs a full index and returns the repo-relative paths that
// survived in the graph.
func indexedPaths(t *testing.T, repoRoot string) []string {
	t.Helper()
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "graph.sqlite")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	defer s.Close()

	idx := New(s, parser.NewRegistry(goparser.New()), nil)
	if _, err := idx.Index(ctx, Options{RepoRoot: repoRoot}); err != nil {
		t.Fatalf("Index() error = %v", err)
	}
	repo, err := s.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	files, err := s.ExistingFiles(ctx, repo.ID)
	if err != nil {
		t.Fatalf("ExistingFiles() error = %v", err)
	}
	out := make([]string, 0, len(files))
	for path := range files {
		out = append(out, filepath.ToSlash(path))
	}
	sort.Strings(out)
	return out
}

func equalPaths(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// A directory symlink is skipped, and — the point of the phase — it does not
// truncate the rest of the walk. Its lexical position must not matter.
func TestIndexDirectorySymlinkDoesNotTruncateScan(t *testing.T) {
	for _, name := range []string{"aaa-link", "mmm-link", "zzz-link"} {
		t.Run(name, func(t *testing.T) {
			repoRoot := t.TempDir()
			writeFile(t, filepath.Join(repoRoot, "a.go"), "package a\n\nfunc A() {}\n")
			writeFile(t, filepath.Join(repoRoot, "sub", "inner.go"), "package sub\n\nfunc Inner() {}\n")
			writeFile(t, filepath.Join(repoRoot, "z.go"), "package a\n\nfunc Z() {}\n")
			symlink(t, filepath.Join(repoRoot, "sub"), filepath.Join(repoRoot, name))

			got := indexedPaths(t, repoRoot)
			want := []string{"a.go", "sub/inner.go", "z.go"}
			if !equalPaths(got, want) {
				t.Fatalf("indexed paths = %v, want %v", got, want)
			}
		})
	}
}

// The link target lives outside the repository: nothing from it may enter the
// graph, and the scan still completes.
func TestIndexOutOfRepoDirectorySymlinkIsNotFollowed(t *testing.T) {
	outside := t.TempDir()
	writeFile(t, filepath.Join(outside, "outside.go"), "package outside\n\nfunc Outside() {}\n")

	repoRoot := t.TempDir()
	writeFile(t, filepath.Join(repoRoot, "a.go"), "package a\n\nfunc A() {}\n")
	symlink(t, outside, filepath.Join(repoRoot, "vendor-link"))
	writeFile(t, filepath.Join(repoRoot, "z.go"), "package a\n\nfunc Z() {}\n")

	got := indexedPaths(t, repoRoot)
	want := []string{"a.go", "z.go"}
	if !equalPaths(got, want) {
		t.Fatalf("indexed paths = %v, want %v", got, want)
	}
}

// A self-referential link cannot recurse, because no directory link is
// followed at all.
func TestIndexDirectorySymlinkCycleTerminates(t *testing.T) {
	repoRoot := t.TempDir()
	writeFile(t, filepath.Join(repoRoot, "a", "inner.go"), "package a\n\nfunc Inner() {}\n")
	symlink(t, filepath.Join(repoRoot, "a"), filepath.Join(repoRoot, "a", "loop"))

	got := indexedPaths(t, repoRoot)
	want := []string{"a/inner.go"}
	if !equalPaths(got, want) {
		t.Fatalf("indexed paths = %v, want %v", got, want)
	}
}

func TestIndexBrokenSymlinkIsSkipped(t *testing.T) {
	repoRoot := t.TempDir()
	writeFile(t, filepath.Join(repoRoot, "a.go"), "package a\n\nfunc A() {}\n")
	symlink(t, filepath.Join(repoRoot, "nonexistent"), filepath.Join(repoRoot, "broken.go"))
	writeFile(t, filepath.Join(repoRoot, "z.go"), "package a\n\nfunc Z() {}\n")

	got := indexedPaths(t, repoRoot)
	want := []string{"a.go", "z.go"}
	if !equalPaths(got, want) {
		t.Fatalf("indexed paths = %v, want %v", got, want)
	}
}

// A link to a regular file stays indexed, under its own repo-relative path and
// never under the target's path.
func TestIndexFileSymlinkIndexedAtLogicalPath(t *testing.T) {
	repoRoot := t.TempDir()
	writeFile(t, filepath.Join(repoRoot, "real.go"), "package a\n\nfunc Real() {}\n")
	symlink(t, filepath.Join(repoRoot, "real.go"), filepath.Join(repoRoot, "alias.go"))

	got := indexedPaths(t, repoRoot)
	want := []string{"alias.go", "real.go"}
	if !equalPaths(got, want) {
		t.Fatalf("indexed paths = %v, want %v", got, want)
	}
}

// A link entry is described by its target's metadata, not the link's own: with
// the link's size and mtime the change check treats an edited target as
// unchanged and the graph goes stale behind the alias.
func TestUpdateFileSymlinkSeesTargetEdits(t *testing.T) {
	ctx := context.Background()
	repoRoot := t.TempDir()
	realPath := filepath.Join(repoRoot, "real.go")
	writeFile(t, realPath, "package a\n\nfunc Before() {}\n")
	symlink(t, realPath, filepath.Join(repoRoot, "alias.go"))

	dbPath := filepath.Join(t.TempDir(), "graph.sqlite")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	defer s.Close()
	idx := New(s, parser.NewRegistry(goparser.New()), nil)

	if _, err := idx.Index(ctx, Options{RepoRoot: repoRoot}); err != nil {
		t.Fatalf("Index() error = %v", err)
	}

	time.Sleep(2 * time.Millisecond)
	writeFile(t, realPath, "package a\n\nfunc After() {}\nfunc AfterTwo() {}\n")

	summary, err := idx.Update(ctx, Options{RepoRoot: repoRoot})
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if summary.FilesIndexed != 2 {
		t.Fatalf("FilesIndexed = %d, want 2 (real.go and alias.go both re-read)", summary.FilesIndexed)
	}
}

// A symlink must not become a back door into a hardcoded-skip directory.
func TestIndexSymlinkIntoIgnoredDirectoryIsNotFollowed(t *testing.T) {
	repoRoot := t.TempDir()
	writeFile(t, filepath.Join(repoRoot, "a.go"), "package a\n\nfunc A() {}\n")
	writeFile(t, filepath.Join(repoRoot, ".git", "hidden.go"), "package hidden\n\nfunc Hidden() {}\n")
	writeFile(t, filepath.Join(repoRoot, "node_modules", "dep.go"), "package dep\n\nfunc Dep() {}\n")
	symlink(t, filepath.Join(repoRoot, ".git"), filepath.Join(repoRoot, "git-link"))
	symlink(t, filepath.Join(repoRoot, "node_modules"), filepath.Join(repoRoot, "mods-link"))

	got := indexedPaths(t, repoRoot)
	want := []string{"a.go"}
	if !equalPaths(got, want) {
		t.Fatalf("indexed paths = %v, want %v", got, want)
	}
}

// failingWalk replays filepath.WalkDir but reports a filesystem error once it
// has visited failAfter entries, standing in for a mid-tree directory-read or
// permission failure. Deterministic and OS-independent, unlike chmod(000),
// which root and several CI containers ignore.
func failingWalk(failAfter int, failErr error) func(string, fs.WalkDirFunc) error {
	return func(root string, fn fs.WalkDirFunc) error {
		visited := 0
		return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return fn(path, d, err)
			}
			if !d.IsDir() {
				if visited >= failAfter {
					return failErr
				}
				visited++
			}
			return fn(path, d, nil)
		})
	}
}

// substituteEntry replays filepath.WalkDir but hands fn a replacement DirEntry
// for one file, so the entry states that cannot be produced portably from a
// real filesystem (a file that vanished between readdir and its lazy lstat, a
// FIFO on Windows) are still covered on every platform.
func substituteEntry(name string, replace func(fs.DirEntry) fs.DirEntry) func(string, fs.WalkDirFunc) error {
	return func(root string, fn fs.WalkDirFunc) error {
		return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err == nil && d != nil && d.Name() == name {
				d = replace(d)
			}
			return fn(path, d, err)
		})
	}
}

type stubEntry struct {
	fs.DirEntry
	mode    fs.FileMode
	infoErr error
}

func (e stubEntry) Type() fs.FileMode { return e.mode }

func (e stubEntry) Info() (fs.FileInfo, error) {
	if e.infoErr != nil {
		return nil, e.infoErr
	}
	return e.DirEntry.Info()
}

// Entries that are not regular files are skipped by an explicit rule, and that
// skip must never truncate the rest of the walk. A vanished file is gone, so
// the scan is not missing anything it should have seen; a FIFO would block
// hashFile forever if it reached the pipeline.
func TestIndexNonIndexableEntriesAreSkippedNotFatal(t *testing.T) {
	cases := []struct {
		name  string
		entry stubEntry
	}{
		{"vanished before lstat", stubEntry{mode: 0, infoErr: fs.ErrNotExist}},
		{"named pipe", stubEntry{mode: fs.ModeNamedPipe}},
		{"device", stubEntry{mode: fs.ModeDevice}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repoRoot := t.TempDir()
			writeFile(t, filepath.Join(repoRoot, "a.go"), "package a\n\nfunc A() {}\n")
			writeFile(t, filepath.Join(repoRoot, "mid.go"), "package a\n\nfunc M() {}\n")
			writeFile(t, filepath.Join(repoRoot, "z.go"), "package a\n\nfunc Z() {}\n")

			withWalkDir(t, substituteEntry("mid.go", func(d fs.DirEntry) fs.DirEntry {
				stub := tc.entry
				stub.DirEntry = d
				return stub
			}))

			got := indexedPaths(t, repoRoot)
			want := []string{"a.go", "z.go"}
			if !equalPaths(got, want) {
				t.Fatalf("indexed paths = %v, want %v", got, want)
			}
		})
	}
}

func withWalkDir(t *testing.T, walk func(string, fs.WalkDirFunc) error) {
	t.Helper()
	prev := walkDir
	walkDir = walk
	t.Cleanup(func() { walkDir = prev })
}

var errInjectedWalk = errors.New("injected traversal failure")

// A traversal failure means the scan cannot know what the repository contains,
// so it must not be reported as a successful (shorter) index.
func TestIndexTraversalFailureIsFatal(t *testing.T) {
	ctx := context.Background()
	repoRoot := t.TempDir()
	for _, name := range []string{"a.go", "mid.go", "z.go"} {
		writeFile(t, filepath.Join(repoRoot, name), "package a\n\nfunc F"+name[:1]+"() {}\n")
	}

	dbPath := filepath.Join(t.TempDir(), "graph.sqlite")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	defer s.Close()
	idx := New(s, parser.NewRegistry(goparser.New()), nil)

	withWalkDir(t, failingWalk(1, errInjectedWalk))
	if _, err := idx.Index(ctx, Options{RepoRoot: repoRoot}); !errors.Is(err, errInjectedWalk) {
		t.Fatalf("Index() error = %v, want %v", err, errInjectedWalk)
	}
}

// An update whose traversal fails after a prefix must not conclude that the
// files it never reached were deleted, and a later clean update must converge
// to the same graph as a fresh index.
func TestUpdateTraversalFailureKeepsUnvisitedFilesAndRecovers(t *testing.T) {
	ctx := context.Background()
	repoRoot := t.TempDir()
	for _, name := range []string{"a.go", "mid.go", "z.go"} {
		writeFile(t, filepath.Join(repoRoot, name), "package a\n\nfunc F"+name[:1]+"() {}\n")
	}

	dbPath := filepath.Join(t.TempDir(), "graph.sqlite")
	s, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	defer s.Close()
	idx := New(s, parser.NewRegistry(goparser.New()), nil)

	if _, err := idx.Index(ctx, Options{RepoRoot: repoRoot}); err != nil {
		t.Fatalf("Index() error = %v", err)
	}
	repo, err := s.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	before, err := s.Stats(ctx, repo.ID)
	if err != nil {
		t.Fatalf("Stats() error = %v", err)
	}
	if before.Files != 3 {
		t.Fatalf("stats.Files after index = %d, want 3", before.Files)
	}

	t.Run("traversal fails after a prefix", func(t *testing.T) {
		withWalkDir(t, failingWalk(1, errInjectedWalk))
		if _, err := idx.Update(ctx, Options{RepoRoot: repoRoot}); !errors.Is(err, errInjectedWalk) {
			t.Fatalf("Update() error = %v, want %v", err, errInjectedWalk)
		}
	})

	files, err := s.ExistingFiles(ctx, repo.ID)
	if err != nil {
		t.Fatalf("ExistingFiles() error = %v", err)
	}
	for _, name := range []string{"a.go", "mid.go", "z.go"} {
		if _, ok := files[name]; !ok {
			t.Fatalf("%s was dropped by a failed update; live files = %v", name, files)
		}
	}
	partial, err := s.Stats(ctx, repo.ID)
	if err != nil {
		t.Fatalf("Stats(after failure) error = %v", err)
	}
	if partial.Symbols != before.Symbols {
		t.Fatalf("symbols after failed update = %d, want %d", partial.Symbols, before.Symbols)
	}

	if _, err := idx.Update(ctx, Options{RepoRoot: repoRoot}); err != nil {
		t.Fatalf("Update(recovered) error = %v", err)
	}
	after, err := s.Stats(ctx, repo.ID)
	if err != nil {
		t.Fatalf("Stats(recovered) error = %v", err)
	}
	if after.Files != before.Files || after.Symbols != before.Symbols || after.Edges != before.Edges {
		t.Fatalf("recovered stats = %+v, want %+v", after, before)
	}
}
