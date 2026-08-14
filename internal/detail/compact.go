package detail

import (
	"fmt"

	"github.com/isink17/codegraph/internal/compactfmt"
)

// cardColumns is the compact column contract for a projected card.
//
// It lives beside the Symbol struct because a card has one definition: the
// projection decides what a card contains, and both encodings render that same
// projection. The order is written out rather than derived from struct fields --
// it is a public wire contract, and reordering Symbol's fields must not reorder
// it.
//
// Identity first, then location: an agent reading a page top to bottom sees the
// selectors it needs for the follow-up call before the prose.
var cardColumns = []string{
	"symbol_id",
	"stable_key",
	"name",
	"qualified_name",
	"kind",
	"language",
	"file",
	"line",
}

// CardColumns returns the compact card column order.
func CardColumns() []string {
	out := make([]string, len(cardColumns))
	copy(out, cardColumns)
	return out
}

// CardSchema returns the compact schema for a section of cards. section is the
// name the JSON encoding uses for the same list -- `matches`, `callers`,
// `callees`, `symbols` -- so one result has one vocabulary in both encodings.
func CardSchema(section string) compactfmt.Schema {
	return compactfmt.Schema{Section: section, Columns: cardColumns}
}

// AppendCardCells appends this symbol's card cells to dst in schema order and
// returns the extended slice, so a page can encode through one reused buffer.
//
// Only card fields are read. A Symbol projected at a richer level has more
// fields populated, which is exactly why CompactSupported rejects those levels
// rather than letting this method quietly drop them.
func (s Symbol) AppendCardCells(dst []compactfmt.Cell) []compactfmt.Cell {
	return append(dst,
		compactfmt.Int64(s.SymbolID),
		compactfmt.Str(s.StableKey),
		compactfmt.Str(s.Name),
		compactfmt.Str(s.QualifiedName),
		compactfmt.Str(s.Kind),
		compactfmt.Str(s.Language),
		compactfmt.Str(s.File),
		compactfmt.Int(s.Line),
	)
}

// approxCardBytes is a rough encoded size of one card row, used only to size the
// output buffer once instead of growing it by doubling. It is deliberately a
// little generous: over-reserving a page costs one allocation, under-reserving
// costs several.
const approxCardBytes = 128

// WriteCards writes a page of projected symbols as one compact section.
func WriteCards(doc *compactfmt.Document, section string, symbols []Symbol) {
	doc.Grow(len(symbols) * approxCardBytes)
	sec := doc.Section(CardSchema(section))
	cells := make([]compactfmt.Cell, 0, len(cardColumns))
	for _, sym := range symbols {
		sec.Row(sym.AppendCardCells(cells[:0]))
	}
}

// CompactSupported reports whether a level can be encoded as compact, and
// explains the rejection when it cannot.
//
// Only card is supported, and the reason is measurement rather than effort:
// compact's whole saving is the repeated key, and beyond card the payload is
// dominated by per-symbol prose and source text, where the keys are a few
// percent. Encoding a body as escaped cells would trade a large readability
// cost for a small one. The combination is refused rather than silently served
// as cards, because a caller who asked for source and received identity would
// have no way to tell.
func CompactSupported(level Level) error {
	if level == Card {
		return nil
	}
	return fmt.Errorf("format %q supports only detail %q, got %q: request %q for %s detail",
		compactfmt.FormatCompact, Card, level, compactfmt.FormatJSON, level)
}
