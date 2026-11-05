package query

import (
	"context"
	"errors"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/store"
)

func TestContextForTaskRefusesRunningScan(t *testing.T) {
	fx := newContextFixture(t)
	ctx := context.Background()
	id, started, err := fx.store.BeginScan(ctx, fx.repoID, "update")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fx.store.CompleteScan(ctx, id, store.ScanSummary{}, started, "failed", "test cleanup") }()
	result, err := fx.svc.ContextForTask(ctx, fx.repoID, taskPayment, fullOpts())
	if !errors.Is(err, store.ErrScanActive) || result != nil {
		t.Fatalf("context served while scan is running: result present=%v err=%v", result != nil, err)
	}
}

type interleavedContextStore struct {
	contextStoreOps
	commit func()
}

func (s *interleavedContextStore) SymbolsForNamedReferences(ctx context.Context, repo int64, refs []string, limit int) ([]graph.Symbol, error) {
	before, err := s.contextStoreOps.SymbolsForNamedReferences(ctx, repo, refs, limit)
	if err == nil {
		s.commit()
	}
	return before, err
}

func TestContextForTaskCannotMixBatchesOfOneRunningScan(t *testing.T) {
	fx := newContextFixture(t)
	ctx := context.Background()
	id, started, err := fx.store.BeginScan(ctx, fx.repoID, "update")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fx.store.CompleteScan(ctx, id, store.ScanSummary{}, started, "failed", "test cleanup") }()
	fx.svc.ctxStore = &interleavedContextStore{contextStoreOps: fx.store, commit: func() {
		p := goparser.New()
		parsed, err := p.Parse(ctx, "paymentsvc/service.go", []byte("package paymentsvc\n// payment retry replacement\nfunc NewPayment() {}\n"))
		if err != nil {
			t.Fatal(err)
		}
		profile := p.Profile()
		_, err = fx.store.ReplaceFileGraphsBatch(ctx, fx.repoID, id, []store.ReplaceFileGraphInput{{Path: "paymentsvc/service.go", Language: "go", ParserProfile: profile.ID, ParserCallEdges: profile.EmitsCallEdges, ParserSemanticEpoch: profile.SemanticEpoch, Parsed: parsed}})
		if err != nil {
			t.Fatal(err)
		}
	}}
	result, err := fx.svc.ContextForTask(ctx, fx.repoID, "ProcessPayment payment retry", ContextForTaskOptions{})
	if errors.Is(err, store.ErrScanActive) && result == nil {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	var ids []int64
	for _, sym := range allSymbols(result) {
		ids = append(ids, sym.SymbolID)
	}
	live, err := fx.store.SymbolsForIDs(ctx, fx.repoID, ids)
	if err != nil {
		t.Fatal(err)
	}
	for _, sym := range allSymbols(result) {
		if _, ok := live[sym.SymbolID]; !ok {
			t.Fatalf("served symbol %s (id %d) from before a committed replacement alongside post-replacement retrieval", sym.QualifiedName, sym.SymbolID)
		}
	}
	t.Fatal("expected an active-scan refusal")
}
