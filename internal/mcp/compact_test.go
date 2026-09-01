package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/compactfmt"
	"github.com/isink17/codegraph/internal/detail"
	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/query"
	"github.com/isink17/codegraph/internal/store"
	"github.com/isink17/codegraph/internal/tokenest"
)

// callToolRaw invokes a tool through Serve and returns the model-visible text
// verbatim, plus whether the call reported an error.
//
// Everything in this file measures and parses that string. It is what the client
// hands the model, and it is the only honest place to compare two encodings: the
// JSON-RPC frame around it is transport whose escaping the client decodes before
// the model ever sees the payload.
func callToolRaw(t *testing.T, s *Server, ctx context.Context, name string, args map[string]any) (string, bool) {
	t.Helper()
	input := bytes.NewBuffer(nil)
	writeFrameToBuffer(t, input, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args},
	})
	var output bytes.Buffer
	if err := s.Serve(ctx, input, &output, io.Discard); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
	frames := readAllFrames(t, &output)
	if len(frames) != 1 {
		t.Fatalf("frames = %d, want 1", len(frames))
	}
	result := frames[0]["result"].(map[string]any)
	content := result["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("content parts = %d, want 1", len(content))
	}
	part := content[0].(map[string]any)
	if part["type"] != "text" {
		t.Fatalf("content type = %v, want text", part["type"])
	}
	// P15 did not introduce structuredContent: the payload must be sent once.
	if _, ok := result["structuredContent"]; ok {
		t.Fatalf("result carries structuredContent as well as text: %v", result)
	}
	return part["text"].(string), result["isError"] == true
}

func callToolCompactDoc(t *testing.T, s *Server, ctx context.Context, name string, args map[string]any) *compactfmt.Decoded {
	t.Helper()
	withFormat := map[string]any{"format": "compact"}
	for k, v := range args {
		withFormat[k] = v
	}
	text, isErr := callToolRaw(t, s, ctx, name, withFormat)
	if isErr {
		t.Fatalf("%s(compact) returned an error: %s", name, text)
	}
	decoded, err := compactfmt.Decode(text)
	if err != nil {
		t.Fatalf("%s(compact): Decode: %v\n%s", name, err, text)
	}
	if decoded.Tool != name {
		t.Fatalf("@tool = %q, want %q", decoded.Tool, name)
	}
	if decoded.Version != compactfmt.Version {
		t.Fatalf("version = %q, want %q", decoded.Version, compactfmt.Version)
	}
	return decoded
}

// jsonRecords calls a tool in the default encoding and returns one keyed list of
// records from its envelope.
func jsonRecords(t *testing.T, s *Server, ctx context.Context, name, key string, args map[string]any) []map[string]any {
	t.Helper()
	text, isErr := callToolRaw(t, s, ctx, name, args)
	if isErr {
		t.Fatalf("%s(json) returned an error: %s", name, text)
	}
	var envelope struct {
		OK   bool                       `json:"ok"`
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(text), &envelope); err != nil {
		t.Fatalf("%s(json): %v\n%s", name, err, text)
	}
	if !envelope.OK {
		t.Fatalf("%s(json): ok=false: %s", name, text)
	}
	raw, ok := envelope.Data[key]
	if !ok {
		t.Fatalf("%s(json): no %q in data: %s", name, key, text)
	}
	var records []map[string]any
	// UseNumber keeps every number as the exact text encoding/json emitted, so a
	// float column can be compared to that text rather than to a re-formatted
	// float64.
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&records); err != nil {
		t.Fatalf("%s(json): %q is not a record list: %v", name, key, err)
	}
	return records
}

// assertSectionMatchesJSON is the parity oracle: for every row and every column
// the compact schema claims to carry, the decoded cell must equal the JSON value
// of the same field in the same record, in the same order.
func assertSectionMatchesJSON(t *testing.T, label string, section *compactfmt.DecodedSection, records []map[string]any, numeric map[string]bool) {
	t.Helper()
	if section == nil {
		t.Fatalf("%s: no section", label)
	}
	if len(section.Rows) != len(records) {
		t.Fatalf("%s: compact has %d rows, JSON has %d records", label, len(section.Rows), len(records))
	}
	for i, record := range records {
		for _, column := range section.Columns {
			cell, ok := section.Cell(i, column)
			if !ok {
				t.Fatalf("%s row %d: no cell for column %q", label, i, column)
			}
			want := jsonValueString(t, record[column], numeric[column])
			if cell != want {
				t.Fatalf("%s row %d column %q: compact %q, JSON %q", label, i, column, cell, want)
			}
		}
	}
}

// jsonValueString renders a decoded JSON value the way the compact encoder does.
// An absent key is the column's zero value: `omitempty` and an empty cell encode
// the same fact.
func jsonValueString(t *testing.T, value any, numeric bool) string {
	t.Helper()
	switch v := value.(type) {
	case nil:
		if numeric {
			return "0"
		}
		return ""
	case string:
		return v
	case bool:
		return strconv.FormatBool(v)
	case float64:
		// Only integral card fields reach here (the impact-radius oracle decodes
		// into typed structs rather than json.Number).
		if v != float64(int64(v)) {
			t.Fatalf("unexpected non-integer float64 %v; decode with UseNumber to compare it", v)
		}
		return strconv.FormatInt(int64(v), 10)
	case json.Number:
		// Floats are compared against the digits encoding/json actually wrote,
		// not against a re-render through the same formatter the encoder uses:
		// re-rendering would assert the encoder against itself.
		if numeric {
			return v.String()
		}
		f, err := v.Float64()
		if err != nil {
			t.Fatalf("bad number %q: %v", v.String(), err)
		}
		return strconv.FormatFloat(f, 'g', -1, 64)
	}
	t.Fatalf("unexpected JSON value %T", value)
	return ""
}

var numericCardColumns = map[string]bool{"symbol_id": true, "line": true}

// TestOmittedFormatEqualsExplicitJSON is the compatibility floor: a call without
// `format` must produce exactly the bytes an explicit `format=json` produces, on
// every tool that gained the argument.
func TestOmittedFormatEqualsExplicitJSON(t *testing.T) {
	s, ctx := detailFixture(t)
	cases := []struct {
		tool string
		args map[string]any
	}{
		{"search_symbols", map[string]any{"query": "Helper"}},
		{"find_symbol", map[string]any{"query": "Area"}},
		{"find_callers", map[string]any{"symbol": "Helper"}},
		{"find_callees", map[string]any{"symbol": "lib.AlsoUseHelper"}},
		{"get_impact_radius", map[string]any{"symbols": []any{"Helper"}, "depth": 2}},
		{"find_related_tests", map[string]any{"file": "lib/widget.go"}},
		{"find_dead_code", map[string]any{"limit": 10}},
		{"list_files", map[string]any{"limit": 10}},
		{"trace_dependencies", map[string]any{"symbol": "Helper"}},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			omitted, isErr := callToolRaw(t, s, ctx, tc.tool, tc.args)
			if isErr {
				t.Fatalf("%s: error: %s", tc.tool, omitted)
			}
			explicit := map[string]any{"format": "json"}
			for k, v := range tc.args {
				explicit[k] = v
			}
			withJSON, isErr := callToolRaw(t, s, ctx, tc.tool, explicit)
			if isErr {
				t.Fatalf("%s: error: %s", tc.tool, withJSON)
			}
			if omitted != withJSON {
				t.Fatalf("%s: omitted format and format=json differ:\n%s\n%s", tc.tool, omitted, withJSON)
			}
			if !strings.HasPrefix(omitted, "{") {
				t.Fatalf("%s: default encoding is not JSON: %s", tc.tool, omitted)
			}
		})
	}
}

// TestFormatDoesNotChangeUnmigratedTools guards the tools deliberately left
// JSON-only: `format` is not a property they accept, and passing it is a clear
// argument error rather than a silently ignored option.
func TestFormatDoesNotChangeUnmigratedTools(t *testing.T) {
	s, ctx := detailFixture(t)
	for _, tool := range []string{"search_semantic", "context_for_task", "graph_stats"} {
		args := map[string]any{"format": "compact"}
		switch tool {
		case "search_semantic", "context_for_task":
			args["query"] = "widget area"
			args["task"] = "widget area"
		}
		text, isErr := callToolRaw(t, s, ctx, tool, args)
		if !isErr {
			t.Fatalf("%s accepted format: %s", tool, text)
		}
		if !strings.Contains(text, "unknown argument") {
			t.Fatalf("%s: unexpected rejection: %s", tool, text)
		}
	}
}

func TestCompactCardParityAcrossTools(t *testing.T) {
	s, ctx := detailFixture(t)
	cases := []struct {
		tool    string
		section string
		args    map[string]any
	}{
		{"search_symbols", "matches", map[string]any{"query": "Helper"}},
		{"find_symbol", "matches", map[string]any{"query": "Area"}},
		{"find_callers", "callers", map[string]any{"symbol": "Helper"}},
		{"find_callees", "callees", map[string]any{"symbol": "lib.AlsoUseHelper"}},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			records := jsonRecords(t, s, ctx, tc.tool, tc.section, tc.args)
			if len(records) == 0 {
				t.Fatalf("%s returned no records; the fixture cannot prove parity", tc.tool)
			}
			decoded := callToolCompactDoc(t, s, ctx, tc.tool, tc.args)
			section := decoded.Section(tc.section)
			if !reflect.DeepEqual(section.Columns, detail.CardColumns()) {
				t.Fatalf("%s columns = %v, want %v", tc.tool, section.Columns, detail.CardColumns())
			}
			assertSectionMatchesJSON(t, tc.tool, section, records, numericCardColumns)
		})
	}
}

// TestCompactPreservesQueryOrderAndPaging checks that compact rows arrive in the
// query's order, and that a page boundary falls in the same place in both
// encodings.
func TestCompactPreservesQueryOrderAndPaging(t *testing.T) {
	s, ctx := bulkFixture(t)
	args := map[string]any{"query": "Handle", "limit": 4, "offset": 2}
	records := jsonRecords(t, s, ctx, "search_symbols", "matches", args)
	if len(records) < 2 {
		t.Fatalf("fixture page too small to prove ordering: %d rows", len(records))
	}
	decoded := callToolCompactDoc(t, s, ctx, "search_symbols", args)
	section := decoded.Section("matches")
	var jsonIDs, compactIDs []string
	for i, record := range records {
		jsonIDs = append(jsonIDs, jsonValueString(t, record["symbol_id"], true))
		cell, _ := section.Cell(i, "symbol_id")
		compactIDs = append(compactIDs, cell)
	}
	if !reflect.DeepEqual(jsonIDs, compactIDs) {
		t.Fatalf("row order differs: json %v, compact %v", jsonIDs, compactIDs)
	}
}

// TestCompactImpactRadiusKeepsGraphSemantics checks the nested result: three
// named sections in a fixed order, the file list intact, and the summary counts
// carried rather than recomputed away.
func TestCompactImpactRadiusKeepsGraphSemantics(t *testing.T) {
	s, ctx := detailFixture(t)
	args := map[string]any{"symbols": []any{"Helper"}, "depth": 2}

	text, isErr := callToolRaw(t, s, ctx, "get_impact_radius", args)
	if isErr {
		t.Fatalf("json error: %s", text)
	}
	var envelope struct {
		Data struct {
			Symbols []map[string]any `json:"symbols"`
			Files   []string         `json:"files"`
			Summary struct {
				AffectedSymbols int `json:"affected_symbols"`
				AffectedFiles   int `json:"affected_files"`
			} `json:"summary"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(text), &envelope); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(envelope.Data.Symbols) == 0 || len(envelope.Data.Files) == 0 {
		t.Fatalf("fixture impact radius is empty: %s", text)
	}

	decoded := callToolCompactDoc(t, s, ctx, "get_impact_radius", args)
	var names []string
	for _, section := range decoded.Sections {
		names = append(names, section.Name)
	}
	if want := []string{"symbols", "files", "summary", "presence"}; !reflect.DeepEqual(names, want) {
		t.Fatalf("sections = %v, want %v", names, want)
	}
	assertSectionMatchesJSON(t, "impact symbols", decoded.Section("symbols"), envelope.Data.Symbols, numericCardColumns)

	files := decoded.Section("files")
	if len(files.Rows) != len(envelope.Data.Files) {
		t.Fatalf("files rows = %d, want %d", len(files.Rows), len(envelope.Data.Files))
	}
	for i, want := range envelope.Data.Files {
		if got, _ := files.Cell(i, "file"); got != want {
			t.Fatalf("file %d = %q, want %q", i, got, want)
		}
	}
	summary := decoded.Section("summary")
	if len(summary.Rows) != 1 {
		t.Fatalf("summary rows = %d, want 1", len(summary.Rows))
	}
	gotSymbols, _ := summary.Cell(0, "affected_symbols")
	gotFiles, _ := summary.Cell(0, "affected_files")
	if gotSymbols != strconv.Itoa(envelope.Data.Summary.AffectedSymbols) || gotFiles != strconv.Itoa(envelope.Data.Summary.AffectedFiles) {
		t.Fatalf("summary = (%s, %s), want (%d, %d)", gotSymbols, gotFiles,
			envelope.Data.Summary.AffectedSymbols, envelope.Data.Summary.AffectedFiles)
	}
}

// TestCompactRowToolParity covers the tools whose records are store maps rather
// than projected cards, including their relationship-specific fields.
func TestCompactRowToolParity(t *testing.T) {
	s, ctx := detailFixture(t)
	cases := []struct {
		tool    string
		section string
		args    map[string]any
		numeric map[string]bool
	}{
		{"list_files", "files", map[string]any{"limit": 20}, map[string]bool{"size_bytes": true}},
		{"find_dead_code", "dead_code", map[string]any{"limit": 20}, map[string]bool{"start_line": true, "end_line": true}},
		{"trace_dependencies", "dependencies", map[string]any{"symbol": "Helper", "depth": 3}, map[string]bool{"depth": true}},
	}
	for _, tc := range cases {
		t.Run(tc.tool, func(t *testing.T) {
			records := jsonRecords(t, s, ctx, tc.tool, tc.section, tc.args)
			if len(records) == 0 {
				t.Fatalf("%s returned no records; the fixture cannot prove parity", tc.tool)
			}
			decoded := callToolCompactDoc(t, s, ctx, tc.tool, tc.args)
			section := decoded.Section(tc.section)
			assertSectionMatchesJSON(t, tc.tool, section, records, tc.numeric)
			// Every key the JSON record carries must have a column: this is what
			// catches a store query that gains a field the schema does not know.
			for _, record := range records {
				var keys, columns []string
				for key := range record {
					keys = append(keys, key)
				}
				columns = append(columns, section.Columns...)
				sort.Strings(keys)
				sort.Strings(columns)
				if !reflect.DeepEqual(keys, columns) {
					t.Fatalf("%s: JSON keys %v, compact columns %v", tc.tool, keys, columns)
				}
			}
		})
	}
}

// TestCompactRelatedTestsParity uses a fixture with a real test file so the
// tests list is non-empty and its `reason` and `score` survive.
func TestCompactRelatedTestsParity(t *testing.T) {
	s, ctx := relatedTestFixture(t)
	args := map[string]any{"symbol": "lib.Report", "limit": 20}
	records := jsonRecords(t, s, ctx, "find_related_tests", "tests", args)
	if len(records) == 0 {
		t.Fatalf("fixture produced no related tests")
	}
	decoded := callToolCompactDoc(t, s, ctx, "find_related_tests", args)
	section := decoded.Section("tests")
	if want := []string{"file", "symbol", "reason", "score"}; !reflect.DeepEqual(section.Columns, want) {
		t.Fatalf("columns = %v, want %v", section.Columns, want)
	}
	assertSectionMatchesJSON(t, "find_related_tests", section, records, map[string]bool{})
}

// TestCompactRejectsRicherDetail pins the one restriction of the encoding, and
// pins that it is a rejection rather than a silent downgrade to cards.
func TestCompactRejectsRicherDetail(t *testing.T) {
	s, ctx := detailFixture(t)
	for _, level := range []string{"skeleton", "excerpt", "full"} {
		text, isErr := callToolRaw(t, s, ctx, "search_symbols", map[string]any{
			"query": "Helper", "detail": level, "format": "compact",
		})
		if !isErr {
			t.Fatalf("detail=%s with format=compact was accepted: %s", level, text)
		}
		if !strings.Contains(text, "supports only detail") {
			t.Fatalf("detail=%s: unhelpful rejection: %s", level, text)
		}
		// An error is an ordinary MCP tool error, not a half-written table.
		if strings.Contains(text, "@codegraph.compact") {
			t.Fatalf("detail=%s: error leaked a partial compact document: %s", level, text)
		}
		var envelope map[string]any
		if err := json.Unmarshal([]byte(text), &envelope); err != nil {
			t.Fatalf("detail=%s: error payload is not JSON: %s", level, text)
		}
		if envelope["ok"] != false {
			t.Fatalf("detail=%s: error payload is not the error envelope: %s", level, text)
		}
	}
	// detail=card is explicitly allowed, and equals the omitted default.
	explicit := callToolCompactDoc(t, s, ctx, "search_symbols", map[string]any{"query": "Helper", "detail": "card"})
	implicit := callToolCompactDoc(t, s, ctx, "search_symbols", map[string]any{"query": "Helper"})
	if !reflect.DeepEqual(explicit, implicit) {
		t.Fatalf("detail=card and omitted detail differ under compact")
	}
}

// TestCallToolRefusesCompact pins that the in-process envelope path -- the one
// agentic_query's tool loop drives -- refuses compact rather than ignoring it.
func TestCallToolRefusesCompact(t *testing.T) {
	s, ctx := detailFixture(t)
	if _, err := s.callTool(ctx, "search_symbols", json.RawMessage(`{"query":"Helper","format":"compact"}`)); err == nil {
		t.Fatal("callTool silently ignored format=compact")
	}
	if _, err := s.callTool(ctx, "search_symbols", json.RawMessage(`{"query":"Helper","format":"json"}`)); err != nil {
		t.Fatalf("callTool rejected format=json: %v", err)
	}
	if _, err := s.callTool(ctx, "search_symbols", json.RawMessage(`{"query":"Helper"}`)); err != nil {
		t.Fatalf("callTool rejected an omitted format: %v", err)
	}
}

func TestInvalidFormatIsRejected(t *testing.T) {
	s, ctx := detailFixture(t)
	for _, value := range []string{"toon", "tsv", "COMPACT", "compact "} {
		args := map[string]any{"query": "Helper", "format": value}
		text, isErr := callToolRaw(t, s, ctx, "search_symbols", args)
		if value == "compact " {
			// Surrounding whitespace is tolerated, exactly as `detail` tolerates it.
			if isErr {
				t.Fatalf("format=%q was rejected: %s", value, text)
			}
			continue
		}
		if !isErr {
			t.Fatalf("format=%q was accepted: %s", value, text)
		}
		if !strings.Contains(text, "invalid format") {
			t.Fatalf("format=%q: unhelpful rejection: %s", value, text)
		}
	}
}

func TestNonStringFormatIsRejected(t *testing.T) {
	s, ctx := detailFixture(t)
	text, isErr := callToolRaw(t, s, ctx, "search_symbols", map[string]any{"query": "Helper", "format": 7})
	if !isErr {
		t.Fatalf("numeric format was accepted: %s", text)
	}
	if !strings.Contains(text, "must be a string") {
		t.Fatalf("unhelpful rejection: %s", text)
	}
}

// TestCompactIsDeterministic runs the same call twice and compares bytes.
func TestCompactIsDeterministic(t *testing.T) {
	s, ctx := detailFixture(t)
	args := map[string]any{"query": "e", "limit": 20, "format": "compact"}
	first, _ := callToolRaw(t, s, ctx, "search_symbols", args)
	second, _ := callToolRaw(t, s, ctx, "search_symbols", args)
	if first != second {
		t.Fatalf("compact output is not deterministic:\n%s\n%s", first, second)
	}
	if strings.Contains(first, "\r\n") {
		t.Fatalf("compact output contains CRLF: %q", first)
	}
}

// TestCompactEmptyResult checks the degenerate page: headers, no rows, still a
// parseable document.
func TestCompactEmptyResult(t *testing.T) {
	s, ctx := detailFixture(t)
	decoded := callToolCompactDoc(t, s, ctx, "search_symbols", map[string]any{"query": "NoSuchSymbolAnywhere"})
	section := decoded.Section("matches")
	if section == nil || len(section.Rows) != 0 {
		t.Fatalf("empty result decoded as %+v", section)
	}
	if !reflect.DeepEqual(section.Columns, detail.CardColumns()) {
		t.Fatalf("columns = %v, want %v", section.Columns, detail.CardColumns())
	}
}

// TestCompactDoesNotMaterializeSource is the eager-work guard: a compact card
// page must not read any file, so its output can never contain a source body and
// must stay the same size no matter how large the symbol's body is.
func TestCompactDoesNotMaterializeSource(t *testing.T) {
	s, ctx := detailFixture(t)
	text, isErr := callToolRaw(t, s, ctx, "search_symbols", map[string]any{
		"query": "Accumulate", "format": "compact",
	})
	if isErr {
		t.Fatalf("error: %s", text)
	}
	// Accumulate's body is 120 generated lines; none of them may appear.
	if strings.Contains(text, "total +=") {
		t.Fatalf("compact card output carries source: %s", text)
	}
	if lines := strings.Count(text, "\n"); lines > 8 {
		t.Fatalf("compact card page has %d lines, so it is not card-shaped:\n%s", lines, text)
	}
}

// TestCompactSchemaMatchesHandlers keeps the three descriptions of the surface in
// agreement: the advertised schema, the argument validator, and the encoder.
func TestCompactSchemaMatchesHandlers(t *testing.T) {
	advertised := map[string]bool{}
	for _, def := range staticToolDefinitions {
		name := def["name"].(string)
		schema := def["inputSchema"].(map[string]any)
		props := schema["properties"].(map[string]any)
		prop, ok := props["format"]
		if !ok {
			continue
		}
		advertised[name] = true
		property := prop.(map[string]any)
		if property["type"] != "string" {
			t.Fatalf("%s: format type = %v", name, property["type"])
		}
		if !reflect.DeepEqual(property["enum"], compactfmt.Names()) {
			t.Fatalf("%s: format enum = %v, want %v", name, property["enum"], compactfmt.Names())
		}
		if property["default"] != string(compactfmt.FormatJSON) {
			t.Fatalf("%s: format default = %v, want json", name, property["default"])
		}
		if spec := toolArgumentSpecs[name]; spec.properties["format"] != "string" {
			t.Fatalf("%s advertises format but does not validate it", name)
		}
	}
	if !reflect.DeepEqual(advertised, compactCapableTools) {
		t.Fatalf("advertised format tools %v, encoder supports %v", keysOf(advertised), keysOf(compactCapableTools))
	}
	for name := range toolArgumentSpecs {
		_, validates := toolArgumentSpecs[name].properties["format"]
		if validates && !advertised[name] {
			t.Fatalf("%s validates format but does not advertise it", name)
		}
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestEveryRegisteredCompactToolEncodes calls every tool in compactCapableTools
// with format=compact and checks it produces a real document. A tool advertised
// with `format` but missing an encoder arm would otherwise only fail at runtime.
func TestEveryRegisteredCompactToolEncodes(t *testing.T) {
	s, ctx := detailFixture(t)
	args := map[string]map[string]any{
		"find_symbol":        {"query": "Area"},
		"search_symbols":     {"query": "Helper"},
		"find_callers":       {"symbol": "Helper"},
		"find_callees":       {"symbol": "lib.AlsoUseHelper"},
		"get_impact_radius":  {"symbols": []any{"Helper"}},
		"find_related_tests": {"file": "lib/widget.go"},
		"find_dead_code":     {"limit": 5},
		"list_files":         {"limit": 5},
		"trace_dependencies": {"symbol": "Helper"},
	}
	for tool := range compactCapableTools {
		call, ok := args[tool]
		if !ok {
			t.Fatalf("%s is compact-capable but this test has no arguments for it", tool)
		}
		decoded := callToolCompactDoc(t, s, ctx, tool, call)
		if len(decoded.Sections) == 0 {
			t.Fatalf("%s produced a document with no sections", tool)
		}
		for _, section := range decoded.Sections {
			if len(section.Columns) == 0 {
				t.Fatalf("%s section %q has no columns", tool, section.Name)
			}
		}
	}
}

// TestRelatedTestColumnsCoverStoreStruct is the drift guard for the one compact
// schema built over a store struct: a new field on store.RelatedTest must either
// gain a column or fail here.
func TestRelatedTestColumnsCoverStoreStruct(t *testing.T) {
	typ := reflect.TypeOf(store.RelatedTest{})
	var jsonNames []string
	for i := 0; i < typ.NumField(); i++ {
		tag := typ.Field(i).Tag.Get("json")
		name := strings.Split(tag, ",")[0]
		if name == "" || name == "-" {
			t.Fatalf("store.RelatedTest field %q has no JSON name", typ.Field(i).Name)
		}
		jsonNames = append(jsonNames, name)
	}
	columns := append([]string(nil), relatedTestColumns.Columns...)
	sort.Strings(jsonNames)
	sort.Strings(columns)
	if !reflect.DeepEqual(jsonNames, columns) {
		t.Fatalf("store.RelatedTest JSON names %v, compact columns %v", jsonNames, columns)
	}
}

// TestImpactSectionsCoverEveryTraversalKey is the drift guard for the nested
// result: a new top-level key or summary counter in an impact radius must gain a
// section or a column rather than disappearing from compact output.
func TestImpactSectionsCoverEveryTraversalKey(t *testing.T) {
	s, ctx := detailFixture(t)
	args := map[string]any{"symbols": []any{"Helper"}, "depth": 2}
	text, isErr := callToolRaw(t, s, ctx, "get_impact_radius", args)
	if isErr {
		t.Fatalf("json error: %s", text)
	}
	var envelope struct {
		Data map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(text), &envelope); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	var dataKeys []string
	for key := range envelope.Data {
		dataKeys = append(dataKeys, key)
	}
	decoded := callToolCompactDoc(t, s, ctx, "get_impact_radius", args)
	var sectionNames []string
	for _, section := range decoded.Sections {
		sectionNames = append(sectionNames, section.Name)
	}
	sort.Strings(dataKeys)
	for i, key := range dataKeys {
		if key == "seed_presence" {
			dataKeys[i] = "presence"
		}
	}
	sorted := append([]string(nil), sectionNames...)
	sort.Strings(sorted)
	if !reflect.DeepEqual(dataKeys, sorted) {
		t.Fatalf("impact JSON keys %v, compact sections %v", dataKeys, sorted)
	}

	var summary map[string]json.RawMessage
	if err := json.Unmarshal(envelope.Data["summary"], &summary); err != nil {
		t.Fatalf("summary: %v", err)
	}
	var summaryKeys []string
	for key := range summary {
		summaryKeys = append(summaryKeys, key)
	}
	columns := append([]string(nil), impactSummarySchema.Columns...)
	sort.Strings(summaryKeys)
	sort.Strings(columns)
	if !reflect.DeepEqual(summaryKeys, columns) {
		t.Fatalf("impact summary keys %v, compact columns %v", summaryKeys, columns)
	}
}

// TestToolsListGrowthIsModest measures the schema cost of the new argument. The
// budget is deliberately tight: tools/list is paid once per session by every
// client, and P13 and P14 already grew it.
func TestToolsListGrowthIsModest(t *testing.T) {
	payload, err := json.Marshal(staticToolDefinitions)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var withoutFormat []map[string]any
	if err := json.Unmarshal(payload, &withoutFormat); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, def := range withoutFormat {
		props := def["inputSchema"].(map[string]any)["properties"].(map[string]any)
		delete(props, "format")
	}
	stripped, err := json.Marshal(withoutFormat)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	growth := len(payload) - len(stripped)
	perTool := growth / len(compactCapableTools)
	if perTool > 200 {
		t.Fatalf("format costs %d bytes per tool (%d total); keep the description short", perTool, growth)
	}
	t.Logf("tools/list: %d bytes with format, %d without (+%d, %d per tool)", len(payload), len(stripped), growth, perTool)
}

// TestCompactSavesTokens is the reason the format exists. The threshold is the
// phase target for bulk card pages, measured on model-visible content through
// the production path.
func TestCompactSavesTokens(t *testing.T) {
	s, ctx := bulkFixture(t)
	cases := []struct {
		tool    string
		args    map[string]any
		minSave float64
	}{
		{"search_symbols", map[string]any{"query": "Handle", "limit": 100}, 0.35},
		{"search_symbols", map[string]any{"query": "Handle", "limit": 20}, 0.35},
		{"find_callers", map[string]any{"symbol": "service.Emit", "limit": 50}, 0.35},
		{"find_callees", map[string]any{"symbol": "service.Dispatch", "limit": 50}, 0.35},
		{"get_impact_radius", map[string]any{"symbols": []any{"service.Dispatch"}, "depth": 2}, 0.25},
		{"list_files", map[string]any{"limit": 100}, 0.25},
		{"find_dead_code", map[string]any{"limit": 100}, 0.35},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s/%v", tc.tool, tc.args["limit"]), func(t *testing.T) {
			jsonText, isErr := callToolRaw(t, s, ctx, tc.tool, tc.args)
			if isErr {
				t.Fatalf("json error: %s", jsonText)
			}
			compactArgs := map[string]any{"format": "compact"}
			for k, v := range tc.args {
				compactArgs[k] = v
			}
			compactText, isErr := callToolRaw(t, s, ctx, tc.tool, compactArgs)
			if isErr {
				t.Fatalf("compact error: %s", compactText)
			}
			saved := 1 - float64(tokenest.OfString(compactText))/float64(tokenest.OfString(jsonText))
			t.Logf("%s%v: json %d B / %d tokens, compact %d B / %d tokens, saved %.1f%%",
				tc.tool, tc.args, len(jsonText), tokenest.OfString(jsonText),
				len(compactText), tokenest.OfString(compactText), saved*100)
			if saved < tc.minSave {
				t.Fatalf("%s saved only %.1f%%, want at least %.0f%%", tc.tool, saved*100, tc.minSave*100)
			}
		})
	}
}

// relatedTestFixture indexes a repository whose test names match package-level
// functions, which is what the Go adapter turns into test links.
func relatedTestFixture(t *testing.T) (*Server, context.Context) {
	t.Helper()
	s, ctx := detailFixture(t)
	writeRepoFile(t, s.repoRoot, "lib/widget_test.go", `package lib

import "testing"

func TestReport(t *testing.T) {
	if got := Report(&Widget{Width: 1, Height: 1}); got != 1 {
		t.Fatalf("Report() = %d", got)
	}
}

func TestHelper(t *testing.T) {
	if got := Helper(2); got != 4 {
		t.Fatalf("Helper() = %d", got)
	}
}
`)
	if _, err := s.indexer.Index(ctx, indexer.Options{RepoRoot: s.repoRoot, Force: true}); err != nil {
		t.Fatalf("reindex: %v", err)
	}
	return s, ctx
}

// bulkFixture indexes a repository large enough for a hundred-row page, with
// ordinary Go names and nested paths. The point is a page that looks like a real
// one: cell values of realistic length, so the measured saving is the repeated
// key and not an artifact of one-character identifiers.
func bulkFixture(tb testing.TB) (*Server, context.Context) {
	tb.Helper()
	ctx := context.Background()
	repoRoot := tb.TempDir()

	var dispatch strings.Builder
	dispatch.WriteString("package service\n\n// Dispatch fans a request out to every handler.\nfunc Dispatch(id int) int {\n\ttotal := 0\n")
	for i := 0; i < 120; i++ {
		writeRepoFileTB(tb, repoRoot, fmt.Sprintf("internal/service/handler_%03d.go", i), fmt.Sprintf(`package service

// RequestContext%03d carries one handler's request state.
type RequestContext%03d struct {
	RequestID int
}

// HandleRequest%03d handles one request and returns its status code.
func HandleRequest%03d(ctx *RequestContext%03d) int {
	return Emit(ctx.RequestID + %d)
}
`, i, i, i, i, i, i))
		fmt.Fprintf(&dispatch, "\ttotal += HandleRequest%03d(&RequestContext%03d{RequestID: id})\n", i, i)
	}
	dispatch.WriteString("\treturn total\n}\n")
	writeRepoFileTB(tb, repoRoot, "internal/service/dispatch.go", dispatch.String())
	// Emit is the fan-in hub: every handler calls it, so find_callers has a page
	// worth measuring.
	writeRepoFileTB(tb, repoRoot, "internal/service/emit.go", `package service

// Emit records one status code and returns it.
func Emit(code int) int {
	return code
}
`)

	st := openTestStoreTB(tb)
	tb.Cleanup(func() { st.Close() })
	idx := indexer.New(st, parser.NewRegistry(goparser.New()), nil)
	repo, err := st.UpsertRepo(ctx, repoRoot)
	if err != nil {
		tb.Fatalf("UpsertRepo() error = %v", err)
	}
	if _, err := idx.Index(ctx, indexer.Options{RepoRoot: repoRoot}); err != nil {
		tb.Fatalf("Index() error = %v", err)
	}
	return NewServer(repoRoot, repoRoot, repo.ID, st, idx, query.New(st, nil), io.Discard), ctx
}

func writeRepoFileTB(tb testing.TB, repoRoot, relativePath, content string) {
	tb.Helper()
	path := filepath.Join(repoRoot, relativePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		tb.Fatalf("MkdirAll(%q) error = %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		tb.Fatalf("WriteFile(%q) error = %v", path, err)
	}
}

func openTestStoreTB(tb testing.TB) *store.Store {
	tb.Helper()
	st, err := store.Open(filepath.Join(tb.TempDir(), "graph.sqlite"))
	if err != nil {
		tb.Fatalf("store.Open() error = %v", err)
	}
	return st
}

func BenchmarkSearchSymbolsCompactVsJSON(b *testing.B) {
	ctx := context.Background()
	server := setupMCPBenchServer(b, ctx)
	for _, format := range []string{"json", "compact"} {
		b.Run(format, func(b *testing.B) {
			raw, err := json.Marshal(map[string]any{"query": "BenchFn", "limit": 100, "format": format})
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			var total int
			for i := 0; i < b.N; i++ {
				text, err := server.callToolContent(ctx, "search_symbols", raw)
				if err != nil {
					b.Fatal(err)
				}
				total += len(text)
			}
			b.ReportMetric(float64(total)/float64(b.N), "bytes/op")
		})
	}
}

// BenchmarkHubPageEncoding measures a 100-row page off P12's hub fixture -- one
// symbol with thousands of callers -- through the production content path, so the
// two encodings are compared where a per-row cost would actually show up.
func BenchmarkHubPageEncoding(b *testing.B) {
	ctx := context.Background()
	server, _, _ := setupMCPHubServer(b)
	for _, format := range []string{"json", "compact"} {
		b.Run(format, func(b *testing.B) {
			raw := json.RawMessage(fmt.Sprintf(`{"symbol":"pkg.Hub","limit":100,"offset":0,"format":%q}`, format))
			warm, err := server.callToolContent(ctx, "find_callers", raw)
			if err != nil {
				b.Fatalf("callToolContent: %v", err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := server.callToolContent(ctx, "find_callers", raw); err != nil {
					b.Fatalf("callToolContent: %v", err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(len(warm)), "payload_bytes")
			b.ReportMetric(float64(tokenest.OfString(warm)), "est_tokens")
		})
	}
}
