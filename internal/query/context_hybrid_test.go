package query

import (
	"context"
	"hash/fnv"
	"math"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/embedding"
)

// embedderForTest is the seam the fixture uses for the HybridSearch path. It is
// an embedding.Embedder, so no interface is widened for tests.
type embedderForTest interface {
	embedding.Embedder
}

// bagOfWordsEmbedder is a deterministic, offline stand-in for a real embedding
// model: it hashes lowercased word tokens into a fixed number of buckets and
// L2-normalises the result. Cosine similarity between two texts therefore
// tracks their word overlap, which is enough to make HybridSearch rank the
// task's target symbol first without Ollama or a network call.
type bagOfWordsEmbedder struct{}

const bagOfWordsDims = 32

func (bagOfWordsEmbedder) Dimensions() int { return bagOfWordsDims }

func (e bagOfWordsEmbedder) Embed(_ context.Context, text string) ([]float32, error) {
	vec := make([]float32, bagOfWordsDims)
	for _, tok := range strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9')
	}) {
		h := fnv.New32a()
		_, _ = h.Write([]byte(tok))
		vec[h.Sum32()%bagOfWordsDims] += 1
	}
	var norm float64
	for _, v := range vec {
		norm += float64(v) * float64(v)
	}
	if norm == 0 {
		return vec, nil
	}
	norm = math.Sqrt(norm)
	for i := range vec {
		vec[i] = float32(float64(vec[i]) / norm)
	}
	return vec, nil
}

func (e bagOfWordsEmbedder) EmbedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v, err := e.Embed(ctx, t)
		if err != nil {
			return nil, err
		}
		out[i] = v
	}
	return out, nil
}

// seedEmbeddings writes an embedding for every indexed symbol using emb, so the
// service takes the HybridSearch branch of SemanticSearch.
func (fx *contextFixture) seedEmbeddings(t testing.TB, emb embedderForTest) {
	t.Helper()
	ctx := context.Background()
	symbols, err := fx.store.ExportSymbolsPage(ctx, fx.repoID, 10_000, 0)
	if err != nil {
		t.Fatalf("ExportSymbolsPage() error = %v", err)
	}
	if len(symbols) == 0 {
		t.Fatal("fixture has no symbols to embed")
	}
	byFile := map[int64]map[string][]float32{}
	for _, sym := range symbols {
		text := embedding.FormatSymbolText(sym.Kind, sym.QualifiedName, sym.Signature, sym.DocSummary)
		vec, err := emb.Embed(ctx, text)
		if err != nil {
			t.Fatalf("Embed() error = %v", err)
		}
		if byFile[sym.FileID] == nil {
			byFile[sym.FileID] = map[string][]float32{}
		}
		byFile[sym.FileID][sym.StableKey] = vec
	}
	for fileID, m := range byFile {
		if err := fx.store.UpsertSymbolEmbeddings(ctx, fx.repoID, fileID, "test-bow", m); err != nil {
			t.Fatalf("UpsertSymbolEmbeddings() error = %v", err)
		}
	}
	has, err := fx.store.HasEmbeddings(ctx, fx.repoID)
	if err != nil || !has {
		t.Fatalf("HasEmbeddings() = %v, %v; want true, nil", has, err)
	}
}

// TestHybridSeedContract pins the HybridSearch producer contract and proves
// ContextForTask accepts seeds from that path too, not only from the
// token-overlap fallback.
func TestHybridSeedContract(t *testing.T) {
	ctx := context.Background()
	fx := newContextFixtureWithEmbedder(t, bagOfWordsEmbedder{})

	hits, err := fx.svc.SemanticSearch(ctx, fx.repoID, "process payment", 30, 0)
	if err != nil {
		t.Fatalf("SemanticSearch() error = %v", err)
	}
	if len(hits) == 0 {
		t.Fatal("hybrid SemanticSearch() returned no hits")
	}
	sawVector := false
	for _, h := range hits {
		if _, ok := h["file"].(string); !ok {
			t.Fatalf("hybrid hit missing %q: %v", "file", h)
		}
		if _, ok := h["symbol"].(string); !ok {
			t.Fatalf("hybrid hit missing %q: %v", "symbol", h)
		}
		if _, ok := h["name"]; ok {
			t.Fatalf("hybrid hit unexpectedly carries %q: %v", "name", h)
		}
		for _, why := range h["why"].([]string) {
			if why == "vector_similarity" {
				sawVector = true
			}
		}
	}
	if !sawVector {
		t.Fatal("no hybrid hit came from vector_similarity; the embedding path did not run")
	}

	res, err := fx.svc.ContextForTask(ctx, fx.repoID, "process payment", ContextForTaskOptions{})
	if err != nil {
		t.Fatalf("ContextForTask() error = %v", err)
	}
	if len(res.Files) == 0 {
		t.Fatalf("ContextForTask() dropped every hybrid seed: %+v", res)
	}
}
