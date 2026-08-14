package detail

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/compactfmt"
)

// TestCardColumnsCoverEveryCardField is the drift guard between the two
// encodings of a card.
//
// Production code never reflects over Symbol -- the column order is written out
// in compact.go precisely so a struct refactor cannot reorder the wire. This
// test does reflect, in the opposite direction: it asserts that the set of JSON
// names a card populates is exactly the set of compact columns, so adding a card
// field without adding a column fails here instead of silently disappearing from
// compact output.
func TestCardColumnsCoverEveryCardField(t *testing.T) {
	card := Symbol{
		Name:          "n",
		QualifiedName: "q",
		Kind:          "k",
		Language:      "l",
		File:          "f",
		Line:          1,
		SymbolID:      2,
		StableKey:     "s",
	}
	encoded, err := json.Marshal(card)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var fields map[string]any
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	jsonNames := make([]string, 0, len(fields))
	for name := range fields {
		jsonNames = append(jsonNames, name)
	}
	columns := CardColumns()
	if len(jsonNames) != len(columns) {
		t.Fatalf("card JSON has %d fields %v, compact schema has %d columns %v",
			len(jsonNames), jsonNames, len(columns), columns)
	}
	columnSet := map[string]bool{}
	for _, col := range columns {
		columnSet[col] = true
	}
	for _, name := range jsonNames {
		if !columnSet[name] {
			t.Fatalf("card field %q has no compact column (columns: %v)", name, columns)
		}
	}
}

func TestCardColumnsAreStable(t *testing.T) {
	want := []string{"symbol_id", "stable_key", "name", "qualified_name", "kind", "language", "file", "line"}
	if got := CardColumns(); !reflect.DeepEqual(got, want) {
		t.Fatalf("CardColumns() = %v, want %v", got, want)
	}
	// The returned slice is a copy: a caller cannot rewrite the wire contract.
	CardColumns()[0] = "hacked"
	if got := CardColumns()[0]; got != "symbol_id" {
		t.Fatalf("CardColumns() is mutable: first column became %q", got)
	}
}

// TestWriteCardsMatchesJSONSemantics round-trips a page through the compact
// encoder and compares every cell with the JSON projection of the same cards.
func TestWriteCardsMatchesJSONSemantics(t *testing.T) {
	cards := []Symbol{
		{Name: "Foo", QualifiedName: "pkg.Foo", Kind: "func", Language: "go", File: "internal/pkg/foo.go", Line: 12, SymbolID: 7, StableKey: "func:pkg:Foo"},
		// Empty optionals: JSON omits them, compact emits empty cells, and both
		// decode to the same zero values.
		{Name: "Bare", QualifiedName: "Bare", Kind: "type"},
		// Data that could forge structure or be trimmed.
		{Name: "tab\there", QualifiedName: "a\\b", Kind: "@kind", Language: "go", File: `dir\literal.go`, Line: 3, SymbolID: 9, StableKey: "  padded  "},
		{Name: "🚀", QualifiedName: "emoji.🚀", Kind: "func", Language: "go", File: "a/b.go", Line: 4, SymbolID: 10, StableKey: "k\nnewline"},
	}
	doc := compactfmt.NewDocument("search_symbols")
	WriteCards(doc, "matches", cards)
	text, err := doc.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	decoded, err := compactfmt.Decode(text)
	if err != nil {
		t.Fatalf("Decode: %v (text %q)", err, text)
	}
	section := decoded.Section("matches")
	if section == nil {
		t.Fatal("no matches section")
	}
	if len(section.Rows) != len(cards) {
		t.Fatalf("%d rows, want %d", len(section.Rows), len(cards))
	}

	for i, card := range cards {
		// The JSON representation of the same card, read back as a map, is the
		// oracle: whatever JSON says the value is, compact must agree.
		encoded, err := json.Marshal(card)
		if err != nil {
			t.Fatalf("row %d: marshal: %v", i, err)
		}
		var fields map[string]any
		if err := json.Unmarshal(encoded, &fields); err != nil {
			t.Fatalf("row %d: unmarshal: %v", i, err)
		}
		for _, column := range CardColumns() {
			cell, ok := section.Cell(i, column)
			if !ok {
				t.Fatalf("row %d: no cell for column %q", i, column)
			}
			want := jsonCellString(t, fields[column], numericCardColumns[column])
			if cell != want {
				t.Fatalf("row %d column %q: compact %q, JSON %q", i, column, cell, want)
			}
		}
	}
}

// numericCardColumns is which card columns hold numbers, so an omitted key
// compares against the right zero: `""` for text, `0` for a number. Both encode
// the same fact -- JSON's `omitempty` dropped a zero value -- and a typed reader
// of either lands on the same Go zero.
var numericCardColumns = map[string]bool{"symbol_id": true, "line": true}

// jsonCellString renders a JSON value the way the compact encoder does, so the
// two can be compared cell by cell. An omitted key -- JSON's `omitempty` -- is
// the column's zero value.
func jsonCellString(t *testing.T, value any, numeric bool) string {
	t.Helper()
	switch v := value.(type) {
	case nil:
		if numeric {
			return "0"
		}
		return ""
	case string:
		return v
	case float64:
		if v != float64(int64(v)) {
			t.Fatalf("unexpected non-integer number %v in a card", v)
		}
		return strconv.FormatInt(int64(v), 10)
	}
	t.Fatalf("unexpected card value %T", value)
	return ""
}

// TestZeroLineAndMissingLineAgree pins the empty/zero semantics: JSON omits
// `line` when it is 0, compact emits `0`, and a reader of either ends up with 0.
func TestZeroLineAndMissingLineAgree(t *testing.T) {
	doc := compactfmt.NewDocument("t")
	WriteCards(doc, "matches", []Symbol{{Name: "n", QualifiedName: "n", Kind: "k"}})
	text, err := doc.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	decoded, err := compactfmt.Decode(text)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if cell, _ := decoded.Section("matches").Cell(0, "line"); cell != "0" {
		t.Fatalf("line cell = %q, want %q", cell, "0")
	}
	if cell, _ := decoded.Section("matches").Cell(0, "file"); cell != "" {
		t.Fatalf("file cell = %q, want empty", cell)
	}
	encoded, err := json.Marshal(Symbol{Name: "n", QualifiedName: "n", Kind: "k"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(encoded), `"line"`) || strings.Contains(string(encoded), `"file"`) {
		t.Fatalf("JSON unexpectedly carries empty card keys: %s", encoded)
	}
}

func TestCompactSupportedOnlyCard(t *testing.T) {
	if err := CompactSupported(Card); err != nil {
		t.Fatalf("card must be supported: %v", err)
	}
	for _, level := range []Level{Skeleton, Excerpt, Full} {
		err := CompactSupported(level)
		if err == nil {
			t.Fatalf("level %q must be rejected for compact", level)
		}
		if !strings.Contains(err.Error(), string(level)) || !strings.Contains(err.Error(), "json") {
			t.Fatalf("level %q: unhelpful error %v", level, err)
		}
	}
}

func BenchmarkWriteCards(b *testing.B) {
	cards := make([]Symbol, 100)
	for i := range cards {
		cards[i] = Symbol{
			Name: "Handler", QualifiedName: "internal/server.Handler", Kind: "func",
			Language: "go", File: "internal/server/handler.go", Line: i + 1,
			SymbolID: int64(i + 1), StableKey: "func:internal/server:Handler",
		}
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		doc := compactfmt.NewDocument("search_symbols")
		WriteCards(doc, "matches", cards)
		if _, err := doc.Encode(); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMarshalCardsJSON(b *testing.B) {
	cards := make([]Symbol, 100)
	for i := range cards {
		cards[i] = Symbol{
			Name: "Handler", QualifiedName: "internal/server.Handler", Kind: "func",
			Language: "go", File: "internal/server/handler.go", Line: i + 1,
			SymbolID: int64(i + 1), StableKey: "func:internal/server:Handler",
		}
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := json.Marshal(map[string]any{"ok": true, "data": map[string]any{"matches": cards}}); err != nil {
			b.Fatal(err)
		}
	}
}
