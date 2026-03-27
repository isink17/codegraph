package embedding

import "context"

// Embedder generates vector embeddings from text.
type Embedder interface {
	// Embed returns a vector embedding for the given text.
	Embed(ctx context.Context, text string) ([]float32, error)

	// EmbedBatch returns vector embeddings for multiple texts.
	EmbedBatch(ctx context.Context, texts []string) ([][]float32, error)

	// Dimensions returns the embedding vector dimension.
	Dimensions() int
}

// SymbolEmbedding pairs a symbol's stable key with its embedding vector.
type SymbolEmbedding struct {
	SymbolStableKey string
	Embedding       []float32
}

// FormatSymbolText builds the text string to embed for a symbol.
func FormatSymbolText(kind, qualifiedName, signature, docSummary string) string {
	text := kind + " " + qualifiedName
	if signature != "" {
		text += " " + signature
	}
	if docSummary != "" {
		text += " " + docSummary
	}
	return text
}
