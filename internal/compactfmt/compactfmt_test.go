package compactfmt

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseFormat(t *testing.T) {
	cases := []struct {
		raw     string
		want    Format
		wantErr bool
	}{
		{raw: "", want: FormatJSON},
		{raw: "   ", want: FormatJSON},
		{raw: "json", want: FormatJSON},
		{raw: "compact", want: FormatCompact},
		{raw: " compact ", want: FormatCompact},
		{raw: "Compact", wantErr: true},
		{raw: "toon", wantErr: true},
		{raw: "tsv", wantErr: true},
	}
	for _, tc := range cases {
		got, err := ParseFormat(tc.raw, FormatJSON)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("ParseFormat(%q) = %q, want error", tc.raw, got)
			}
			continue
		}
		if err != nil {
			t.Fatalf("ParseFormat(%q): %v", tc.raw, err)
		}
		if got != tc.want {
			t.Fatalf("ParseFormat(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestNamesIsTheWireVocabulary(t *testing.T) {
	if got, want := Names(), []string{"json", "compact"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("Names() = %v, want %v", got, want)
	}
}

// TestEncodeShape pins the grammar: version line, tool line, one section header
// per section, and one row per record.
func TestEncodeShape(t *testing.T) {
	doc := NewDocument("search_symbols")
	sec := doc.Section(Schema{Section: "matches", Columns: []string{"symbol_id", "name", "line"}})
	sec.Row([]Cell{Int64(7), Str("Foo"), Int(42)})
	sec.Row([]Cell{Int64(8), Str("Bar"), Int(43)})
	got, err := doc.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	want := "@codegraph.compact/v1\n" +
		"@tool search_symbols\n" +
		"@section matches\n" +
		"@columns symbol_id\tname\tline\n" +
		"7\tFoo\t42\n" +
		"8\tBar\t43\n"
	if got != want {
		t.Fatalf("Encode() =\n%q\nwant\n%q", got, want)
	}
	if !strings.HasSuffix(got, "\n") || strings.HasSuffix(got, "\n\n") {
		t.Fatalf("document must end with exactly one LF: %q", got)
	}
	if strings.Contains(got, "\r") {
		t.Fatalf("document must not contain CR: %q", got)
	}
}

func TestEncodeDeterministic(t *testing.T) {
	build := func() string {
		doc := NewDocument("list_files")
		sec := doc.Section(Schema{Section: "files", Columns: []string{"path", "size_bytes"}})
		for i := 0; i < 20; i++ {
			sec.Row([]Cell{Str("internal/a.go"), Int(i)})
		}
		text, err := doc.Encode()
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		return text
	}
	if a, b := build(), build(); a != b {
		t.Fatalf("encoding is not deterministic:\n%q\n%q", a, b)
	}
}

func TestSections(t *testing.T) {
	doc := NewDocument("get_impact_radius")
	doc.Section(Schema{Section: "symbols", Columns: []string{"symbol_id"}}).Row([]Cell{Int(1)})
	doc.Section(Schema{Section: "files", Columns: []string{"file"}}).Row([]Cell{Str("a.go")})
	doc.Section(Schema{Section: "summary", Columns: []string{"affected_symbols", "affected_files"}}).Row([]Cell{Int(1), Int(1)})
	text, err := doc.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	decoded, err := Decode(text)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	var names []string
	for _, s := range decoded.Sections {
		names = append(names, s.Name)
	}
	if want := []string{"symbols", "files", "summary"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("section order = %v, want %v", names, want)
	}
	if v, ok := decoded.Section("summary").Cell(0, "affected_files"); !ok || v != "1" {
		t.Fatalf("summary.affected_files = %q, %v", v, ok)
	}
}

func TestEmptySectionHasHeaderAndNoRows(t *testing.T) {
	doc := NewDocument("find_callers")
	doc.Section(Schema{Section: "callers", Columns: []string{"symbol_id", "name"}})
	text, err := doc.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	decoded, err := Decode(text)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	section := decoded.Section("callers")
	if section == nil || len(section.Rows) != 0 || len(section.Columns) != 2 {
		t.Fatalf("empty section decoded as %+v", section)
	}
}

// TestValueRoundTrip is the escaping contract. Every value here must come back
// byte for byte: this is the test that catches a silent trim, a case fold, a
// dropped byte, or a separator forged out of data.
func TestValueRoundTrip(t *testing.T) {
	values := map[string]string{
		"empty":             "",
		"literal backslash": `\`,
		"literal-t":         `\t`,
		"literal-n":         `\n`,
		"literal-at":        `\@`,
		"actual tab":        "a\tb",
		"actual lf":         "a\nb",
		"actual cr":         "a\rb",
		"crlf":              "a\r\nb",
		"lone cr":           "\r",
		"double backslash":  `\\`,
		"backslash then n":  `\` + "n",
		"emoji":             "🚀 ok",
		"cjk":               "识别符号",
		"combining":         "é",
		"leading space":     "  lead",
		"trailing space":    "trail  ",
		"only spaces":       "   ",
		"at sign":           "@section not really",
		"at inside":         "a@b",
		"windows path":      `dir\literal-name.go`,
		"posix path":        "dir/file.go",
		"quote":             `"quoted"`,
		"json blob":         `{"a":1}`,
		"nul-ish":           "a\x00b",
		"mixed":             "a\t\\@\r\n🚀",
		"upper/lower":       "MixedCase",
		"long":              strings.Repeat("x\t", 50),
	}
	for name, value := range values {
		doc := NewDocument("t")
		sec := doc.Section(Schema{Section: "s", Columns: []string{"a", "b", "c"}})
		// Put the value in every position, including first (where a leading '@'
		// would otherwise look like a directive) and last.
		sec.Row([]Cell{Str(value), Str("mid"), Str(value)})
		text, err := doc.Encode()
		if err != nil {
			t.Fatalf("%s: Encode: %v", name, err)
		}
		decoded, err := Decode(text)
		if err != nil {
			t.Fatalf("%s: Decode: %v (text %q)", name, err, text)
		}
		section := decoded.Section("s")
		if len(section.Rows) != 1 {
			t.Fatalf("%s: %d rows, want 1 (text %q)", name, len(section.Rows), text)
		}
		if got := section.Rows[0][0]; got != value {
			t.Fatalf("%s: first cell = %q, want %q", name, got, value)
		}
		if got := section.Rows[0][2]; got != value {
			t.Fatalf("%s: last cell = %q, want %q", name, got, value)
		}
		if got := section.Rows[0][1]; got != "mid" {
			t.Fatalf("%s: middle cell = %q, want %q", name, got, "mid")
		}
	}
}

func TestSeparatorAndDirectivesNeverForged(t *testing.T) {
	doc := NewDocument("t")
	sec := doc.Section(Schema{Section: "s", Columns: []string{"a", "b"}})
	sec.Row([]Cell{Str("@section evil\n@columns x"), Str("x\ty")})
	text, err := doc.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	if strings.Count(text, "\n") != 5 {
		t.Fatalf("data forged extra lines: %q", text)
	}
	decoded, err := Decode(text)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(decoded.Sections) != 1 || len(decoded.Sections[0].Rows) != 1 {
		t.Fatalf("data forged structure: %+v", decoded.Sections)
	}
}

func TestNumbersAndBools(t *testing.T) {
	doc := NewDocument("t")
	sec := doc.Section(Schema{Section: "s", Columns: []string{"i", "neg", "f", "small", "b"}})
	sec.Row([]Cell{Int64(9007199254740993), Int(-42), Float(0.30000000000000004), Float(0), Bool(true)})
	text, err := doc.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	decoded, err := Decode(text)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	want := []string{"9007199254740993", "-42", "0.30000000000000004", "0", "true"}
	if got := decoded.Sections[0].Rows[0]; !reflect.DeepEqual(got, want) {
		t.Fatalf("row = %v, want %v", got, want)
	}
}

func TestSingleColumnEmptyValueRoundTrips(t *testing.T) {
	doc := NewDocument("t")
	sec := doc.Section(Schema{Section: "files", Columns: []string{"file"}})
	sec.Row([]Cell{Str("")})
	sec.Row([]Cell{Str("a.go")})
	text, err := doc.Encode()
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	decoded, err := Decode(text)
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	rows := decoded.Section("files").Rows
	if len(rows) != 2 || rows[0][0] != "" || rows[1][0] != "a.go" {
		t.Fatalf("rows = %v", rows)
	}
}

func TestEncodeErrors(t *testing.T) {
	t.Run("cell count mismatch", func(t *testing.T) {
		doc := NewDocument("t")
		doc.Section(Schema{Section: "s", Columns: []string{"a", "b"}}).Row([]Cell{Str("only")})
		if _, err := doc.Encode(); err == nil {
			t.Fatal("want error for a short row")
		}
	})
	t.Run("duplicate section", func(t *testing.T) {
		doc := NewDocument("t")
		doc.Section(Schema{Section: "s", Columns: []string{"a"}})
		doc.Section(Schema{Section: "s", Columns: []string{"a"}})
		if _, err := doc.Encode(); err == nil {
			t.Fatal("want error for a duplicate section")
		}
	})
	t.Run("no sections", func(t *testing.T) {
		if _, err := NewDocument("t").Encode(); err == nil {
			t.Fatal("want error for a document with no sections")
		}
	})
	t.Run("no columns", func(t *testing.T) {
		doc := NewDocument("t")
		doc.Section(Schema{Section: "s"})
		if _, err := doc.Encode(); err == nil {
			t.Fatal("want error for a section with no columns")
		}
	})
	t.Run("duplicate column", func(t *testing.T) {
		doc := NewDocument("t")
		doc.Section(Schema{Section: "s", Columns: []string{"a", "a"}})
		if _, err := doc.Encode(); err == nil {
			t.Fatal("want error for a duplicate column name")
		}
	})
	t.Run("row into closed section", func(t *testing.T) {
		doc := NewDocument("t")
		first := doc.Section(Schema{Section: "a", Columns: []string{"x"}})
		doc.Section(Schema{Section: "b", Columns: []string{"y"}})
		first.Row([]Cell{Str("late")})
		if _, err := doc.Encode(); err == nil {
			t.Fatal("want error for a row appended to a closed section")
		}
	})
}

func TestDecodeErrors(t *testing.T) {
	cases := map[string]string{
		"empty":             "",
		"no header":         "@codegraph.compact/v1\n",
		"future version":    "@codegraph.compact/v2\n@tool t\n@section s\n@columns a\n1\n",
		"not a version":     "hello\n@tool t\n",
		"missing tool":      "@codegraph.compact/v1\n@section s\n@columns a\n",
		"row before header": "@codegraph.compact/v1\n@tool t\nrow\n",
		"columns first":     "@codegraph.compact/v1\n@tool t\n@columns a\n",
		"unknown directive": "@codegraph.compact/v1\n@tool t\n@section s\n@columns a\n@weird x\n",
		"two columns dirs":  "@codegraph.compact/v1\n@tool t\n@section s\n@columns a\n@columns b\n",
		"cell count":        "@codegraph.compact/v1\n@tool t\n@section s\n@columns a\tb\nonly\n",
		"unknown escape":    "@codegraph.compact/v1\n@tool t\n@section s\n@columns a\n\\q\n",
		"truncated escape":  "@codegraph.compact/v1\n@tool t\n@section s\n@columns a\nvalue\\\n",
		"no sections":       "@codegraph.compact/v1\n@tool t\n",
		"duplicate column":  "@codegraph.compact/v1\n@tool t\n@section s\n@columns a\ta\n1\t2\n",
	}
	for name, text := range cases {
		if _, err := Decode(text); err == nil {
			t.Fatalf("%s: want error, got none", name)
		}
	}
}

func TestDecodeRejectsFutureVersionByName(t *testing.T) {
	_, err := Decode("@codegraph.compact/v2\n@tool t\n@section s\n@columns a\n1\n")
	if err == nil || !strings.Contains(err.Error(), "unsupported format version") {
		t.Fatalf("err = %v, want an unsupported-version error", err)
	}
}

func TestDecodeCellByName(t *testing.T) {
	decoded, err := Decode("@codegraph.compact/v1\n@tool t\n@section s\n@columns a\tb\n1\t2\n")
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if v, ok := decoded.Section("s").Cell(0, "b"); !ok || v != "2" {
		t.Fatalf("Cell(0,\"b\") = %q, %v", v, ok)
	}
	if _, ok := decoded.Section("s").Cell(0, "missing"); ok {
		t.Fatal("Cell reported a column that does not exist")
	}
	if _, ok := decoded.Section("s").Cell(9, "a"); ok {
		t.Fatal("Cell reported a row that does not exist")
	}
	if decoded.Section("nope") != nil {
		t.Fatal("Section reported a section that does not exist")
	}
}

func BenchmarkEncodeCards(b *testing.B) {
	type row struct {
		id   int64
		key  string
		name string
	}
	rows := make([]row, 100)
	for i := range rows {
		rows[i] = row{id: int64(i), key: "func:pkg:Name", name: "Name"}
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		doc := NewDocument("search_symbols")
		sec := doc.Section(Schema{Section: "matches", Columns: []string{"symbol_id", "stable_key", "name"}})
		cells := make([]Cell, 0, 3)
		for _, r := range rows {
			sec.Row(append(cells[:0], Int64(r.id), Str(r.key), Str(r.name)))
		}
		if _, err := doc.Encode(); err != nil {
			b.Fatal(err)
		}
	}
}
