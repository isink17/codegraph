package indexer

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/store"
)

// Pause after parsing captured bytes, before they can reach a write batch.
type scanBarrierParser struct {
	parser.Adapter
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (p *scanBarrierParser) Extensions() []string { return []string{".go"} }
func (p *scanBarrierParser) Profile() parser.Profile {
	return p.Adapter.(parser.ProfileProvider).Profile()
}
func (p *scanBarrierParser) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	out, err := p.Adapter.Parse(ctx, path, content)
	p.once.Do(func() { close(p.entered) })
	select {
	case <-p.release:
	case <-ctx.Done():
		return out, ctx.Err()
	}
	return out, err
}

type committedScanEmbedder struct{ afterCommit func() }

func (e *committedScanEmbedder) Dimensions() int                                  { return 1 }
func (e *committedScanEmbedder) Embed(context.Context, string) ([]float32, error) { return nil, nil }
func (e *committedScanEmbedder) EmbedBatch(context.Context, []string) ([][]float32, error) {
	if e.afterCommit != nil {
		e.afterCommit()
	}
	return nil, nil
}

func concurrentStore(t *testing.T, path string) *store.Store {
	t.Helper()
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func scanGraph(t *testing.T, s *store.Store, root string) store.SemanticGraph {
	t.Helper()
	repo, found, err := s.FindRepo(context.Background(), root)
	if err != nil || !found {
		t.Fatalf("repo: found=%v err=%v", found, err)
	}
	g, err := s.SemanticGraph(context.Background(), repo.ID)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func assertScanParity(t *testing.T, s *store.Store, dbPath, root string) {
	t.Helper()
	freshPath := filepath.Join(t.TempDir(), "fresh.sqlite")
	fresh := concurrentStore(t, freshPath)
	indexSemanticTokenFixture(t, fresh, root, Options{NoHistory: true})
	got, want := scanGraph(t, s, root), scanGraph(t, fresh, root)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("graph differs from clean rebuild:\n got %+v\nwant %+v", got, want)
	}
	gotTokens, gotVersion := semanticTokenDump(t, dbPath)
	wantTokens, wantVersion := semanticTokenDump(t, freshPath)
	if gotTokens != wantTokens || gotVersion != wantVersion {
		t.Error("symbol tokens/version differ from clean rebuild")
	}
}

func TestConcurrentScansCannotOverwriteNewerBytes(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(t.TempDir(), "graph.sqlite")
	a, b := concurrentStore(t, path), concurrentStore(t, path)
	writeFile(t, filepath.Join(root, "main.go"), "package main\nfunc Seed() {}\n")
	indexSemanticTokenFixture(t, a, root, Options{NoHistory: true})
	writeFile(t, filepath.Join(root, "main.go"), "package main\nfunc OlderCaptured() {}\n")
	barrier := &scanBarrierParser{Adapter: goparser.New(), entered: make(chan struct{}), release: make(chan struct{})}
	var release sync.Once
	unblock := func() { release.Do(func() { close(barrier.release) }) }
	defer unblock()
	first := New(a, parser.NewRegistry(barrier), nil)
	second := New(b, parser.NewRegistry(goparser.New()), nil)
	done := make(chan error, 1)
	go func() { _, err := first.Update(ctx, Options{RepoRoot: root, NoHistory: true}); done <- err }()
	<-barrier.entered
	writeFile(t, filepath.Join(root, "main.go"), "package main\nfunc NewerAuthoritative() {}\n")
	_, secondErr := second.Update(ctx, Options{RepoRoot: root, NoHistory: true})
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// A busy refusal must be actionable: retry only after the owner finishes.
	if errors.Is(secondErr, store.ErrScanActive) {
		if _, err := second.Update(ctx, Options{RepoRoot: root, NoHistory: true}); err != nil {
			t.Fatal(err)
		}
	} else if secondErr != nil {
		t.Fatal(secondErr)
	}
	t.Logf("second scan while first paused: %v", secondErr)
	assertScanParity(t, b, path, root)
}

func TestFailedScanRetainsCommittedWritesAndRecovers(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(t.TempDir(), "graph.sqlite")
	s := concurrentStore(t, path)
	writeFile(t, filepath.Join(root, "caller.go"), "package main\nfunc Entry() { Target() }\n")
	writeFile(t, filepath.Join(root, "target.go"), "package main\nfunc Target() {}\n")
	indexSemanticTokenFixture(t, s, root, Options{NoHistory: true})
	writeFile(t, filepath.Join(root, "caller.go"), "package main\nfunc Entry() { Target(); Target() }\n")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	idx := New(s, parser.NewRegistry(goparser.New()), &committedScanEmbedder{afterCommit: cancel})
	summary, err := idx.Update(ctx, Options{RepoRoot: root, NoHistory: true})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error=%v", err)
	}
	db := openIndexedDatabase(t, path, true)
	var status string
	var edges int
	if err := db.QueryRow("SELECT status FROM scans WHERE id=?", summary.ScanID).Scan(&status); err != nil {
		t.Fatal(err)
	}
	if err := db.QueryRow("SELECT COUNT(*) FROM edges WHERE dst_name='Target'").Scan(&edges); err != nil {
		t.Fatal(err)
	}
	if status != "failed" || edges != 2 {
		t.Fatalf("status=%s retained edges=%d, want failed and 2 committed edges", status, edges)
	}
	t.Logf("failed scan retained %d new edges", edges)
	if _, err := New(s, parser.NewRegistry(goparser.New()), nil).Update(context.Background(), Options{RepoRoot: root, NoHistory: true}); err != nil {
		t.Fatal(err)
	}
	assertScanParity(t, s, path, root)
}

func TestTokenMigrationOverlapsCommittedScan(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	path := filepath.Join(t.TempDir(), "graph.sqlite")
	a, b := concurrentStore(t, path), concurrentStore(t, path)
	writeFile(t, filepath.Join(root, "main.go"), semanticTokenFixture)
	indexSemanticTokenFixture(t, a, root, Options{NoHistory: true})
	makeSemanticTokensLegacy(t, path)
	writeFile(t, filepath.Join(root, "main.go"), semanticTokenFixture+"\nfunc NewConnectionPool() {}\n")
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	defer unblock()
	first := New(a, parser.NewRegistry(goparser.New()), &committedScanEmbedder{afterCommit: func() { close(entered); <-release }})
	done := make(chan error, 1)
	go func() { _, err := first.Update(ctx, Options{RepoRoot: root, NoHistory: true}); done <- err }()
	<-entered
	repo, _, err := b.FindRepo(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := b.SemanticSearch(ctx, repo.ID, "connection pool", 10, 0); !errors.Is(err, store.ErrSemanticTokensStale) {
		t.Fatalf("partially migrated index was not refused: %v", err)
	}
	second := New(b, parser.NewRegistry(goparser.New()), nil)
	_, secondErr := second.Update(ctx, Options{RepoRoot: root, NoHistory: true})
	unblock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if errors.Is(secondErr, store.ErrScanActive) {
		if _, err := second.Update(ctx, Options{RepoRoot: root, NoHistory: true}); err != nil {
			t.Fatal(err)
		}
	} else if secondErr != nil {
		t.Fatal(secondErr)
	}
	assertScanParity(t, b, path, root)
}
