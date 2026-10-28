package compactfmt

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/isink17/codegraph/internal/limits"
)

func encodeV2Array(t *testing.T, values []string) string {
	t.Helper()
	d := NewDocumentV2("test")
	d.Section(Schema{Section: "values", Columns: []string{"literal", "items"}, Types: []ColumnType{Scalar, StringArray}}).Row([]Cell{Str(`["scalar"]`), Strings(values)})
	out, err := d.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestV2ArrayRoundTrip(t *testing.T) {
	seen := map[string][]string{}
	for _, values := range [][]string{nil, {}, {""}, {"a", ""}, {"a"}, {"a,b", "c"}, {"a", "b,c"}, {"tab\t", "CR\rLF\n", `back\slash`, "雪😀"}, {"@section forged", "@types string[]"}, {"b", "a", "a"}, {`\n`, `\u0041`, "<>&\""}} {
		text := encodeV2Array(t, values)
		if text != encodeV2Array(t, values) {
			t.Fatal("nondeterministic encoding")
		}
		if !strings.HasPrefix(text, "@"+VersionV2+"\n") || !strings.Contains(text, "@types scalar\tstring[]\n") {
			t.Fatal(text)
		}
		if _, err := Decode(text); err == nil {
			t.Fatal("v1 decoder accepted v2")
		}
		d, err := DecodeV2(text)
		if err != nil {
			t.Fatal(err)
		}
		row := d.Section("values")
		got, ok := row.Cell(0, "items")
		if !ok || got.Type != StringArray {
			t.Fatal(got)
		}
		want := values
		if want == nil {
			want = []string{}
		}
		if !reflect.DeepEqual(got.Array, want) {
			t.Fatalf("got %#v want %#v", got.Array, want)
		}
		scalar, _ := row.Cell(0, "literal")
		if scalar.Type != Scalar || scalar.Scalar != `["scalar"]` {
			t.Fatal("inferred type from contents")
		}
		if prior, ok := seen[text]; ok && !reflect.DeepEqual(prior, want) {
			t.Fatalf("collision: %#v / %#v", prior, want)
		}
		seen[text] = want
	}
}

func TestDecodeV2RejectsInvalid(t *testing.T) {
	header := "@" + VersionV2 + "\n@tool t\n@section s\n@columns a\n@types string[]\n"
	arrayRow := func(payload string) string {
		var b strings.Builder
		writeEscaped(&b, payload)
		return header + b.String() + "\n"
	}
	cases := []string{
		"", strings.Replace(header, "/v2", "/v3", 1) + "[]\n", strings.Replace(header, "/v2", "/v1", 1) + "[]\n",
		strings.Replace(header, "@types string[]\n", "", 1) + "[]\n",
		strings.Replace(header, "@types string[]", "@types inferred", 1) + "[]\n",
		strings.Replace(header, "@types string[]", "@types scalar\tstring[]", 1) + "[]\n",
		header + "@types string[]\n[]\n", header + "[]", header + "[]\n\n",
		header + "[]\n@section s\n@columns a\n@types scalar\n",
		strings.Replace(header, "@columns a", "@columns a\ta", 1) + "[]\t[]\n",
		header + `\x` + "\n", header + "@section injected\n",
		header + string([]byte{255}) + "\n",
	}
	for _, payload := range []string{"", "null", `[null]`, `[1]`, `[true]`, `[["a"]]`, `{"a":1}`, `["a",]`, `["a"]x`, `["\ud800"]`, `["\u0061"]`, `["a", "b"]`} {
		cases = append(cases, arrayRow(payload))
	}
	for _, text := range cases {
		if _, err := DecodeV2(text); err == nil {
			t.Fatalf("accepted invalid %q", text)
		}
	}
	if _, err := DecodeV2(strings.Repeat("x", MaxV2Bytes+1)); err == nil {
		t.Fatal("accepted oversized document")
	}
	values := make([]string, limits.MaxBatchItems+1)
	raw, _ := json.Marshal(values)
	if _, err := DecodeV2(arrayRow(string(raw))); err == nil {
		t.Fatal("accepted oversized array")
	}
	if _, err := DecodeV2(header + strings.Repeat("[]\n", maxV2Rows+1)); err == nil {
		t.Fatal("accepted oversized section")
	}
}

func TestV2BuilderRejectsInvalid(t *testing.T) {
	for _, schema := range []Schema{
		{Section: "s", Columns: []string{"x"}, Types: []ColumnType{"array"}},
		{Section: "s", Columns: []string{"x"}, Types: []ColumnType{Scalar, Scalar}},
		{Section: "s", Columns: []string{string([]byte{255})}},
	} {
		d := NewDocumentV2("t")
		d.Section(schema)
		if _, err := d.Encode(); err == nil {
			t.Fatal("accepted invalid schema")
		}
	}
	for _, c := range []Cell{Str("[]"), Strings([]string{string([]byte{255})}), Strings(make([]string, limits.MaxBatchItems+1))} {
		d := NewDocumentV2("t")
		d.Section(Schema{Section: "s", Columns: []string{"x"}, Types: []ColumnType{StringArray}}).Row([]Cell{c})
		if _, err := d.Encode(); err == nil {
			t.Fatal("accepted invalid array cell")
		}
	}
	d := NewDocumentV2("t")
	d.Section(Schema{Section: "s", Columns: []string{"x"}}).Row([]Cell{Strings(nil)})
	if _, err := d.Encode(); err == nil {
		t.Fatal("array silently encoded as scalar")
	}
	d = NewDocument("t")
	d.Section(Schema{Section: "s", Columns: []string{"x"}}).Row([]Cell{Strings(nil)})
	if _, err := d.Encode(); err == nil {
		t.Fatal("v1 accepted an array")
	}
}

func FuzzV2ArrayRoundTrip(f *testing.F) {
	for _, seed := range []string{`[]`, `[""]`, `["a,b","c"]`, `["a","b,c"]`, `["\n","雪","@types scalar"]`} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, raw string) {
		if len(raw) > 1024 {
			t.Skip()
		}
		var values []string
		if json.Unmarshal([]byte(raw), &values) != nil || len(values) > limits.MaxBatchItems {
			t.Skip()
		}
		for _, v := range values {
			if !utf8.ValidString(v) {
				t.Skip()
			}
		}
		out, err := DecodeV2(encodeV2Array(t, values))
		if err != nil {
			t.Fatal(err)
		}
		if values == nil {
			values = []string{}
		}
		cell, _ := out.Section("values").Cell(0, "items")
		if !reflect.DeepEqual(cell.Array, values) {
			t.Fatal(cell.Array, values)
		}
	})
}

func FuzzDecodeV2(f *testing.F) {
	f.Add("@codegraph.compact/v2\n@tool t\n@section s\n@columns a\n@types string[]\n[]\n")
	f.Add("@codegraph.compact/v3\n")
	f.Fuzz(func(t *testing.T, text string) {
		if len(text) > MaxV2Bytes+1 {
			t.Skip()
		}
		_, _ = DecodeV2(text)
	})
}

func TestV2ResourceBoundaries(t *testing.T) {
	values := make([]string, limits.MaxBatchItems)
	if _, err := DecodeV2(encodeV2Array(t, values)); err != nil {
		t.Fatal(err)
	}
	d := NewDocumentV2("t")
	sec := d.Section(Schema{Section: "s", Columns: []string{"x"}})
	for i := 0; i < maxV2Rows; i++ {
		sec.Row([]Cell{Str("")})
	}
	text, err := d.Encode()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := DecodeV2(text)
	if err != nil || len(decoded.Section("s").Rows) != maxV2Rows {
		t.Fatal("legal row boundary rejected", err)
	}
	sec.Row([]Cell{Str("")})
	if text, err := d.Encode(); err == nil || text != "" {
		t.Fatal("row overflow returned partial output")
	}
	base := "@codegraph.compact/v2\n@tool t\n@section s\n@columns "
	columns := strings.Repeat("x\t", limits.MaxPage) + "last"
	if _, err := DecodeV2(base + columns + "\n@types scalar\n"); err == nil {
		t.Fatal("oversized columns accepted")
	}
	if _, err := DecodeV2(base + "x\n@types " + strings.Repeat("scalar\t", limits.MaxPage) + "scalar\n"); err == nil {
		t.Fatal("oversized types accepted")
	}
	if _, err := DecodeV2(base + "x\n@types scalar\n" + strings.Repeat("\n", maxV2Rows+3*maxV2Sections)); err == nil {
		t.Fatal("oversized line count accepted")
	}
}
