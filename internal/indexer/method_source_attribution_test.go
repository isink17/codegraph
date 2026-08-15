//go:build cgo

package indexer

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/isink17/codegraph/internal/parser"
	tsparser "github.com/isink17/codegraph/internal/parser/treesitter"
	"github.com/isink17/codegraph/internal/store"
)

// A call in a method body belongs to that method. Before P20b the chooser only
// considered symbols of kind "function", so a method-body call fell through to
// the file's first function: an empty method inherited calls it never made,
// and calls in class-only files were dropped for having no source at all.
//
// TypeScript and tree-sitter Python are the two adapters that emit methods as
// kind "method"; every other adapter emits "function". Both are covered here.
func TestMethodOwnsItsBodyEdges_TypeScript(t *testing.T) {
	const src = `export function noop(): void {}

export function target(): number {
  return 1;
}

export class Service {
  idle(): void {}

  run(): number {
    return target();
  }

  oneLiner(): number { return target(); }
}
`
	s, repoID := indexSource(t, tsparser.NewTypeScript(), "svc.ts", src)

	// The empty top-level function must not absorb the class's calls.
	assertCallees(t, s, repoID, "svc.noop")
	// Nor must the empty method.
	assertCallees(t, s, repoID, "svc.Service.idle")
	// Each method owns exactly the call in its own body.
	assertCallees(t, s, repoID, "svc.Service.run", "svc.target")
	assertCallees(t, s, repoID, "svc.Service.oneLiner", "svc.target")
	// And the target names its real callers, not an arbitrary first function.
	assertCallers(t, s, repoID, "svc.target", "svc.Service.oneLiner", "svc.Service.run")
}

func TestMethodOwnsItsBodyEdges_Python(t *testing.T) {
	const src = `def target():
    return 1


class Service:
    def idle(self):
        pass

    def run(self):
        return target()
`
	s, repoID := indexSource(t, tsparser.NewPython(), "svc.py", src)

	assertCallees(t, s, repoID, "svc.Service.idle")
	assertCallees(t, s, repoID, "svc.Service.run", "svc.target")
	assertCallers(t, s, repoID, "svc.target", "svc.Service.run")
}

// A file containing only a class still produces edges: before P20b it had no
// symbol of kind "function", so the chooser returned no source at all and
// every call in it was silently discarded.
func TestClassOnlyFileStillProducesEdges(t *testing.T) {
	const src = `export class OnlyClass {
  go(): number {
    return helper();
  }
}

export function helper(): number { return 1; }
`
	s, repoID := indexSource(t, tsparser.NewTypeScript(), "only.ts", src)

	assertCallees(t, s, repoID, "only.OnlyClass.go", "only.helper")
	assertCallers(t, s, repoID, "only.helper", "only.OnlyClass.go")
}

// Go emits methods as kind "function", so it was never affected. Its
// attribution must be unchanged: this is the positive control.
func TestGoMethodAttributionUnchanged(t *testing.T) {
	const src = `package main

func noop() {}

func target() int { return 1 }

type Service struct{}

func (s *Service) Run() int {
	return target()
}

func (s *Service) OneLine() int { return target() }

func Outer() int {
	return target()
}
`
	s, repoID := indexSource(t, tsparser.NewGo(), "main.go", src)

	assertCallees(t, s, repoID, "main.noop")
	assertCallees(t, s, repoID, "main.Service.Run", "main.target")
	assertCallees(t, s, repoID, "main.Service.OneLine", "main.target")
	assertCallees(t, s, repoID, "main.Outer", "main.target")
	assertCallers(t, s, repoID, "main.target", "main.Outer", "main.Service.OneLine", "main.Service.Run")
}

// A container symbol spans all of its members but owns no code. It must never
// become the source of a call made inside a member.
func TestContainerIsNeverAnEdgeSource(t *testing.T) {
	const src = `export class Wrapper {
  a(): number { return target(); }
  b(): number { return target(); }
}

export function target(): number { return 1; }
`
	s, repoID := indexSource(t, tsparser.NewTypeScript(), "wrap.ts", src)

	assertCallers(t, s, repoID, "wrap.target", "wrap.Wrapper.a", "wrap.Wrapper.b")
	// The class itself has no outgoing calls.
	assertCallees(t, s, repoID, "wrap.Wrapper")
}

// A nested function inside a method owns its own body; the method owns what is
// outside the nested range. Go closures are not emitted as symbols, so the
// enclosing function keeps those calls.
func TestNestedExecutableOwnership(t *testing.T) {
	const src = `package main

func target() int { return 1 }

func Outer() int {
	inner := func() int {
		return target()
	}
	return inner() + target()
}
`
	s, repoID := indexSource(t, tsparser.NewGo(), "main.go", src)

	// Both the closure body call and the trailing call stay with Outer,
	// because the closure has no symbol of its own.
	assertCallers(t, s, repoID, "main.target", "main.Outer")
}

// Full index and incremental update must agree on source attribution, and a
// changed method body must not leave the old edge behind.
func TestIncrementalUpdateMatchesFullIndex(t *testing.T) {
	ctx := context.Background()
	repoRoot := t.TempDir()
	path := filepath.Join(repoRoot, "svc.ts")

	const before = `export function helperA(): number { return 1; }
export function helperB(): number { return 2; }

export class Service {
  idle(): void {}

  run(): number {
    return helperA();
  }
}
`
	const after = `export function helperA(): number { return 1; }
export function helperB(): number { return 2; }

export class Service {
  idle(): void {}

  run(): number {
    return helperB();
  }
}
`
	if err := os.WriteFile(path, []byte(before), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	s, err := store.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	defer s.Close()

	idx := New(s, parser.NewRegistry(tsparser.NewTypeScript()), nil)
	if _, err := idx.Index(ctx, Options{RepoRoot: repoRoot}); err != nil {
		t.Fatalf("Index() error = %v", err)
	}
	repo, err := s.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	assertCallers(t, s, repo.ID, "svc.helperA", "svc.Service.run")
	assertCallers(t, s, repo.ID, "svc.helperB")

	if err := os.WriteFile(path, []byte(after), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, err := idx.Update(ctx, Options{RepoRoot: repoRoot}); err != nil {
		t.Fatalf("Update() error = %v", err)
	}

	// The stale edge is gone and the new one belongs to the same method.
	assertCallers(t, s, repo.ID, "svc.helperA")
	assertCallers(t, s, repo.ID, "svc.helperB", "svc.Service.run")
	assertCallees(t, s, repo.ID, "svc.Service.run", "svc.helperB")
	assertCallees(t, s, repo.ID, "svc.Service.idle")

	// A fresh full index of the same content must agree exactly.
	fresh, freshRepo := indexSource(t, tsparser.NewTypeScript(), "svc.ts", after)
	assertCallers(t, fresh, freshRepo, "svc.helperB", "svc.Service.run")
	assertCallers(t, fresh, freshRepo, "svc.helperA")
	assertCallees(t, fresh, freshRepo, "svc.Service.run", "svc.helperB")

	// Edge totals must match too. A replaced file re-inserts its symbols under
	// new ids, so a superseded edge left behind would be orphaned rather than
	// returned by FindCallers: only the count catches it.
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

// Java's stable key carries neither the signature nor the container, so every
// overload of a method shares one. They are still separate rows in `symbols`,
// so each overload owns its own body and neither is bounced to the fallback —
// which would make the callee appear to call itself.
func TestOverloadsSharingAStableKeyKeepTheirCaller(t *testing.T) {
	const src = `class Calc {
  int helper() { return 1; }
  int add(int a) { return helper(); }
  int add(String a) { return helper(); }
}
`
	s, repoID := indexSource(t, tsparser.NewJava(), "Calc.java", src)

	// Two rows, both named Calc.add: each overload owns the call in its own
	// body, instead of one of them swallowing both.
	assertCallers(t, s, repoID, "Calc.helper", "Calc.add", "Calc.add")
	assertNoSelfEdges(t, s, repoID, "Calc.helper", "Calc.add")
}

// The TypeScript stable key is scoped to the module, so a top-level function
// and a same-named method share one. They are separate rows, so each must be
// named as the caller of the call in its own body.
func TestSameNamedFunctionAndMethodKeepDistinctCallers(t *testing.T) {
	const src = `export function request(): number { return target(); }

export class Agent {
  request(): number { return target(); }
}

export function target(): number { return 1; }
`
	s, repoID := indexSource(t, tsparser.NewTypeScript(), "http.ts", src)

	assertCallers(t, s, repoID, "http.target", "http.Agent.request", "http.request")
	assertNoSelfEdges(t, s, repoID, "http.target", "http.request")
}

// Two classes in one file each declaring a same-named method: the realistic
// shape of a container-less stable key. Every body keeps its own caller.
func TestSameNamedMethodsInTwoClasses(t *testing.T) {
	const src = `export class A {
  run(): number { return target(); }
}

export class B {
  run(): number { return target(); }
}

export function target(): number { return 1; }
`
	s, repoID := indexSource(t, tsparser.NewTypeScript(), "two.ts", src)

	assertCallers(t, s, repoID, "two.target", "two.A.run", "two.B.run")
}

// A statement at module scope belongs to no body. It must not be credited to
// an arbitrary method: master only ever used a top-level function as the
// fallback, and widening that pool to methods would invent a caller.
func TestModuleLevelCallIsNeverCreditedToAMethod(t *testing.T) {
	t.Run("class-only file", func(t *testing.T) {
		const src = `export class App {
  helper(): number { return 1; }
  other(): number { return 2; }
}

const app = new App();
app.helper();
`
		s, repoID := indexSource(t, tsparser.NewTypeScript(), "boot.ts", src)

		assertCallers(t, s, repoID, "boot.App.helper")
		assertCallees(t, s, repoID, "boot.App.helper")
		assertCallees(t, s, repoID, "boot.App.other")
	})

	t.Run("file with a top-level function", func(t *testing.T) {
		const src = `export class A {
  m(): number { return 1; }
}

export function f(): number { return 2; }

f();
`
		s, repoID := indexSource(t, tsparser.NewTypeScript(), "mix.ts", src)

		// The module-level call falls back to the top-level function, exactly
		// as it did before P20b — never to A.m.
		assertCallers(t, s, repoID, "mix.f", "mix.f")
		assertCallees(t, s, repoID, "mix.A.m")
	})
}

// Test-file symbols must not be pulled into a production file's edges: source
// attribution is decided within the file being indexed.
func TestSourceAttributionStaysWithinTheFile(t *testing.T) {
	ctx := context.Background()
	repoRoot := t.TempDir()

	const prod = `export function target(): number { return 1; }

export class Service {
  run(): number { return target(); }
}
`
	const spec = `import { Service } from './svc';

export class ServiceTest {
  run(): number { return new Service().run(); }
}
`
	if err := os.WriteFile(filepath.Join(repoRoot, "svc.ts"), []byte(prod), 0o644); err != nil {
		t.Fatalf("WriteFile(svc.ts) error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "svc.test.ts"), []byte(spec), 0o644); err != nil {
		t.Fatalf("WriteFile(svc.test.ts) error = %v", err)
	}

	s, err := store.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	defer s.Close()

	idx := New(s, parser.NewRegistry(tsparser.NewTypeScript()), nil)
	if _, err := idx.Index(ctx, Options{RepoRoot: repoRoot}); err != nil {
		t.Fatalf("Index() error = %v", err)
	}
	repo, err := s.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}

	// Both classes declare run(); each call stays with the method in its own
	// file rather than crossing over to the same-named one.
	assertCallers(t, s, repo.ID, "svc.target", "svc.Service.run")
	assertCallees(t, s, repo.ID, "svc.Service.run", "svc.target")
}
