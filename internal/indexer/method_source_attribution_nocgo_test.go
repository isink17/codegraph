package indexer

// The cgo-free Python adapter is the release path for Python when
// CGO_ENABLED=0. It folds methods into kind "function", and before P20c it
// emitted every symbol with EndLine == StartLine, so the P20b chooser had no
// body span to match: every method-body call fell back to the file's first
// function. These tests run under both build modes because the adapter is
// build-tag free.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/isink17/codegraph/internal/parser"
	pyparser "github.com/isink17/codegraph/internal/parser/python"
	"github.com/isink17/codegraph/internal/store"
)

func TestPythonNoCgoMethodOwnsItsBodyEdges(t *testing.T) {
	const src = `class Service:
    def noop(self):
        pass

    def process(self):
        helper()


def helper():
    return 1
`
	s, repoID := indexSource(t, pyparser.New(), "service.py", src)

	// The empty method must not absorb its sibling's call.
	assertCallees(t, s, repoID, "service.Service.noop")
	assertCallees(t, s, repoID, "service.Service.process", "service.helper")
	assertCallers(t, s, repoID, "service.helper", "service.Service.process")
	assertNoSelfEdges(t, s, repoID, "service.helper")
}

func TestPythonNoCgoAdjacentMethodsKeepTheirOwnCalls(t *testing.T) {
	const src = `class A:
    def first(self):
        helper_a()

    def second(self):
        helper_b()


def helper_a():
    pass


def helper_b():
    pass
`
	s, repoID := indexSource(t, pyparser.New(), "a.py", src)

	assertCallees(t, s, repoID, "a.A.first", "a.helper_a")
	assertCallees(t, s, repoID, "a.A.second", "a.helper_b")
	assertCallers(t, s, repoID, "a.helper_a", "a.A.first")
	assertCallers(t, s, repoID, "a.helper_b", "a.A.second")
}

// Decorator lines and comments sit between two methods; the earlier method's
// span must not swallow them, and the final method's body runs to EOF.
func TestPythonNoCgoDecoratorsCommentsAndEOF(t *testing.T) {
	const src = `def helper():
    return 1


class S:
    def a(self):
        helper()

    # comment between methods

    @staticmethod
    def b():
        helper()
`
	s, repoID := indexSource(t, pyparser.New(), "s.py", src)

	assertCallees(t, s, repoID, "s.S.a", "s.helper")
	assertCallees(t, s, repoID, "s.S.b", "s.helper")
	assertCallers(t, s, repoID, "s.helper", "s.S.a", "s.S.b")
}

// A nested def owns its own body; the enclosing function keeps what is outside
// the nested range.
func TestPythonNoCgoNestedDefOwnsItsBody(t *testing.T) {
	const src = `def outer():
    def inner():
        helper_inner()
    helper_outer()


def helper_inner():
    pass


def helper_outer():
    pass
`
	s, repoID := indexSource(t, pyparser.New(), "n.py", src)

	assertCallers(t, s, repoID, "n.helper_inner", "n.outer.inner")
	assertCallers(t, s, repoID, "n.helper_outer", "n.outer")
}

// Full index and incremental update must agree on source attribution, and a
// changed method body must not leave the old edge behind.
func TestPythonNoCgoUpdateMatchesFullIndex(t *testing.T) {
	ctx := context.Background()
	repoRoot := t.TempDir()
	path := filepath.Join(repoRoot, "svc.py")

	const before = `def helper_a():
    return 1


def helper_b():
    return 2


class Service:
    def idle(self):
        pass

    def run(self):
        return helper_a()
`
	const after = `def helper_a():
    return 1


def helper_b():
    return 2


class Service:
    def idle(self):
        pass

    def run(self):
        return helper_b()
`
	if err := os.WriteFile(path, []byte(before), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	s, err := store.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	defer s.Close()

	idx := New(s, parser.NewRegistry(pyparser.New()), nil)
	if _, err := idx.Index(ctx, Options{RepoRoot: repoRoot}); err != nil {
		t.Fatalf("Index() error = %v", err)
	}
	repo, err := s.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	assertCallers(t, s, repo.ID, "svc.helper_a", "svc.Service.run")
	assertCallers(t, s, repo.ID, "svc.helper_b")

	if err := os.WriteFile(path, []byte(after), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, err := idx.Update(ctx, Options{RepoRoot: repoRoot}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	// The stale edge is gone and the new one belongs to the same method.
	assertCallers(t, s, repo.ID, "svc.helper_a")
	assertCallers(t, s, repo.ID, "svc.helper_b", "svc.Service.run")
	assertCallees(t, s, repo.ID, "svc.Service.run", "svc.helper_b")
	assertCallees(t, s, repo.ID, "svc.Service.idle")

	// A fresh full index of the same content must agree exactly.
	fresh, freshRepo := indexSource(t, pyparser.New(), "svc.py", after)
	assertCallers(t, fresh, freshRepo, "svc.helper_b", "svc.Service.run")
	assertCallers(t, fresh, freshRepo, "svc.helper_a")
	assertCallees(t, fresh, freshRepo, "svc.Service.run", "svc.helper_b")

	updatedStats, err := s.Stats(ctx, repo.ID)
	if err != nil {
		t.Fatalf("Stats(updated) error = %v", err)
	}
	freshStats, err := fresh.Stats(ctx, freshRepo)
	if err != nil {
		t.Fatalf("Stats(fresh) error = %v", err)
	}
	if updatedStats.Edges != freshStats.Edges {
		t.Fatalf("edges after update = %d, fresh full index = %d; the superseded edge was not cleaned up",
			updatedStats.Edges, freshStats.Edges)
	}
	if updatedStats.Symbols != freshStats.Symbols {
		t.Fatalf("symbols after update = %d, fresh full index = %d", updatedStats.Symbols, freshStats.Symbols)
	}
}
