package store

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

// namedRefFixture writes symbols through the production graph-write path, so
// the derived columns the lookup joins against (qualified_suffix, dot_tail2,
// dot_tail3) are computed exactly as indexing computes them.
type namedRefFixture struct {
	ctx    context.Context
	store  *Store
	repoID int64
	other  int64
}

func newNamedRefFixture(t *testing.T) *namedRefFixture {
	t.Helper()
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	repo, err := st.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	other, err := st.UpsertRepo(ctx, t.TempDir())
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	fx := &namedRefFixture{ctx: ctx, store: st, repoID: repo.ID, other: other.ID}

	// A slash-qualified name, the shape a module-path language produces: the
	// after-slash portion is the only part a task would ever write.
	fx.write(t, fx.repoID, "src/app/service.ts", "typescript",
		fx.symbol("typescript", "getUser", "src/app/service.UserService.getUser"))
	// A four-segment dotted name, which is the only shape dot_tail3 serves.
	fx.write(t, fx.repoID, "pylib/deep.py", "python",
		fx.symbol("python", "run", "pkg.module.Runner.run"))
	// Same qualified name in the other repository.
	fx.write(t, fx.other, "pylib/deep.py", "python",
		fx.symbol("python", "run", "pkg.module.Runner.run"))
	return fx
}

func (fx *namedRefFixture) symbol(lang, name, qname string) graph.Symbol {
	return graph.Symbol{
		Language: lang, Kind: "function", Name: name, QualifiedName: qname,
		Signature: "fn " + name, StableKey: "func:" + lang + ":" + qname,
		Range: graph.Position{StartLine: 1, StartCol: 1, EndLine: 2, EndCol: 1},
	}
}

func (fx *namedRefFixture) write(t *testing.T, repoID int64, path, lang string, syms ...graph.Symbol) {
	t.Helper()
	if err := fx.store.ReplaceFileGraph(fx.ctx, repoID, 1, path, lang, 1, 1, path+"-hash",
		graph.ParsedFile{Symbols: syms}); err != nil {
		t.Fatalf("ReplaceFileGraph(%q) error = %v", path, err)
	}
}

func (fx *namedRefFixture) resolve(t *testing.T, repoID int64, name string, perName int) []graph.Symbol {
	t.Helper()
	got, err := fx.store.SymbolsForNamedReferences(fx.ctx, repoID, []string{name}, perName)
	if err != nil {
		t.Fatalf("SymbolsForNamedReferences(%q) error = %v", name, err)
	}
	return got
}

func TestSymbolsForNamedReferencesLegs(t *testing.T) {
	fx := newNamedRefFixture(t)

	t.Run("a slash-qualified name is reachable by its after-slash portion", func(t *testing.T) {
		// Neither the qualified_name leg (the stored name carries the slash
		// prefix) nor dot_tail3 (empty for a three-segment after-slash portion)
		// can answer this; only qualified_suffix can.
		got := fx.resolve(t, fx.repoID, "service.UserService.getUser", 3)
		if len(got) != 1 || got[0].QualifiedName != "src/app/service.UserService.getUser" {
			t.Fatalf("resolved %+v, want the one slash-qualified symbol", got)
		}
	})

	t.Run("a four-segment name is reachable by its last three segments", func(t *testing.T) {
		got := fx.resolve(t, fx.repoID, "module.Runner.run", 3)
		if len(got) != 1 || got[0].QualifiedName != "pkg.module.Runner.run" {
			t.Fatalf("resolved %+v, want the one dot_tail3 symbol", got)
		}
	})

	t.Run("the whole qualified name still resolves", func(t *testing.T) {
		if got := fx.resolve(t, fx.repoID, "pkg.module.Runner.run", 3); len(got) != 1 {
			t.Fatalf("resolved %+v, want one symbol", got)
		}
	})

	t.Run("a reference does not cross repositories", func(t *testing.T) {
		got := fx.resolve(t, fx.repoID, "module.Runner.run", 3)
		if len(got) != 1 {
			t.Fatalf("resolved %d symbols, want only this repository's: %+v", len(got), got)
		}
		if other := fx.resolve(t, fx.other, "module.Runner.run", 3); len(other) != 1 ||
			other[0].ID == got[0].ID {
			t.Fatalf("the other repository resolved %+v, want its own distinct symbol", other)
		}
	})

	t.Run("a symbol whose file was reindexed away stops resolving", func(t *testing.T) {
		fx.write(t, fx.repoID, "pylib/deep.py", "python")
		if got := fx.resolve(t, fx.repoID, "module.Runner.run", 3); len(got) != 0 {
			t.Fatalf("resolved %+v after the declaration was removed, want nothing", got)
		}
	})
}

// TestSymbolsForNamedReferencesSkipsDeletedFilesBeforeTheCap pins the reason
// each leg's cap sits behind the live-file join rather than in front of it: a
// name declared in many since-deleted files must not fill the cap and hide the
// one live declaration behind it.
func TestSymbolsForNamedReferencesSkipsDeletedFilesBeforeTheCap(t *testing.T) {
	fx := newNamedRefFixture(t)
	dead := make([]string, 0, 12)
	for _, pkg := range []string{"d1", "d2", "d3", "d4", "d5", "d6", "d7", "d8"} {
		path := "gone/" + pkg + ".py"
		dead = append(dead, path)
		fx.write(t, fx.repoID, path, "python",
			fx.symbol("python", "onCreate", pkg+".Activity.onCreate"))
	}
	fx.write(t, fx.repoID, "live/activity.py", "python",
		fx.symbol("python", "onCreate", "live.Activity.onCreate"))
	if n, err := fx.store.MarkFilesDeletedBatch(fx.ctx, fx.repoID, 2, dead); err != nil || n != len(dead) {
		t.Fatalf("MarkFilesDeletedBatch() = %d, %v; want %d, nil", n, err, len(dead))
	}

	got := fx.resolve(t, fx.repoID, "onCreate", 3)
	if len(got) != 1 || got[0].QualifiedName != "live.Activity.onCreate" {
		t.Fatalf("resolved %+v, want only the live declaration", got)
	}
}

func TestSymbolsForNamedReferencesAmbiguityCapIsBounded(t *testing.T) {
	fx := newNamedRefFixture(t)
	// More declarations of one name than the cap, each in its own file.
	for _, pkg := range []string{"a", "b", "c", "d", "e", "f"} {
		fx.write(t, fx.repoID, "many/"+pkg+".py", "python",
			fx.symbol("python", "setUp", pkg+".Case.setUp"))
	}
	if got := fx.resolve(t, fx.repoID, "setUp", 3); len(got) != 0 {
		t.Fatalf("an ambiguous reference resolved to %d symbols, want 0", len(got))
	}
	// At the cap it still answers, which is what makes the previous case a
	// decision about ambiguity rather than a blanket refusal. The boundary is
	// what the perNameLimit+1 probe exists to get right, so pin both sides of
	// it and not just a comfortable margin.
	for _, tc := range []struct {
		cap  int
		want int
	}{{5, 0}, {6, 6}, {10, 6}} {
		if got := fx.resolve(t, fx.repoID, "setUp", tc.cap); len(got) != tc.want {
			t.Fatalf("resolved %d symbols under a cap of %d, want %d", len(got), tc.cap, tc.want)
		}
	}
}
