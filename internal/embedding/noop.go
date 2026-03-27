package embedding

import "context"

type noopEmbedder struct{}

// NewNoop returns an Embedder that produces no embeddings.
// Used when embedding support is disabled.
func NewNoop() Embedder {
	return noopEmbedder{}
}

func (noopEmbedder) Embed(context.Context, string) ([]float32, error) {
	return nil, nil
}

func (noopEmbedder) EmbedBatch(_ context.Context, texts []string) ([][]float32, error) {
	result := make([][]float32, len(texts))
	return result, nil
}

func (noopEmbedder) Dimensions() int {
	return 0
}

// IsNoop reports whether the given Embedder is the noop implementation.
func IsNoop(e Embedder) bool {
	_, ok := e.(noopEmbedder)
	return ok
}
