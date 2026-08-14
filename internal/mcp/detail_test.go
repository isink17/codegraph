package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/detail"
	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/query"
)

// detailFixture indexes a small Go repository and returns a server over it.
// The repository is deliberately ordinary: the point of these tests is what the
// production serialization path emits, so nothing here is synthesized.
func detailFixture(t *testing.T) (*Server, context.Context) {
	t.Helper()
	ctx := context.Background()
	repoRoot := t.TempDir()

	var body strings.Builder
	for i := 1; i <= 120; i++ {
		fmt.Fprintf(&body, "\ttotal += %d\n", i)
	}
	writeRepoFile(t, repoRoot, "lib/widget.go", `package lib

// Widget is a thing with a size.
type Widget struct {
	Width  int
	Height int
}

// Area returns the widget's area.
func (w *Widget) Area() int {
	return w.Width * w.Height
}

// Accumulate sums a long series of constants.
func Accumulate() int {
	total := 0
`+body.String()+`	return total
}
`)
	writeRepoFile(t, repoRoot, "lib/caller.go", `package lib

// Report prints an area.
func Report(w *Widget) int {
	return w.Area()
}

// Twice calls Area twice.
func Twice(w *Widget) int {
	return w.Area() + w.Area()
}

// Helper is a plain package-level function, so calls to it resolve to a symbol
// edge rather than to an unresolved name.
func Helper(n int) int {
	return n * 2
}

// UseHelper calls Helper.
func UseHelper() int {
	return Helper(3)
}

// AlsoUseHelper calls Helper too.
func AlsoUseHelper() int {
	return Helper(4) + Accumulate()
}
`)

	s := openTestStore(t)
	t.Cleanup(func() { s.Close() })

	idx := indexer.New(s, parser.NewRegistry(goparser.New()), nil)
	repo, err := s.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	if _, err := idx.Index(ctx, indexer.Options{RepoRoot: repoRoot}); err != nil {
		t.Fatalf("Index() error = %v", err)
	}
	return NewServer(repoRoot, repoRoot, repo.ID, s, idx, query.New(s, nil), io.Discard), ctx
}

// callTooolJSON invokes a tool through Serve -- the same framing, dispatch, and
// json.Marshal a real client drives -- and returns both the decoded payload and
// its serialized size. Measuring the string the server actually writes is the
// point: a benchmark over a helper struct would not notice a projection that
// never reached the wire.
func callToolJSON(t *testing.T, s *Server, ctx context.Context, name string, args map[string]any) (map[string]any, int) {
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
	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	if result["isError"] == true {
		t.Fatalf("%s(%v) returned an error: %s", name, args, text)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		t.Fatalf("json.Unmarshal(%s) error = %v", name, err)
	}
	return parsed["data"].(map[string]any), len(text)
}

func recordsOf(t *testing.T, data map[string]any, key string) []map[string]any {
	t.Helper()
	raw, ok := data[key].([]any)
	if !ok {
		t.Fatalf("data[%q] is not a list: %#v", key, data[key])
	}
	out := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		out = append(out, item.(map[string]any))
	}
	return out
}

func findRecord(t *testing.T, records []map[string]any, name string) map[string]any {
	t.Helper()
	for _, r := range records {
		if r["name"] == name {
			return r
		}
	}
	t.Fatalf("no record named %q in %v", name, records)
	return nil
}

func TestOmittedDetailMatchesExplicitCard(t *testing.T) {
	s, ctx := detailFixture(t)
	omitted, omittedBytes := callToolJSON(t, s, ctx, "find_symbol", map[string]any{"query": "Area"})
	explicit, explicitBytes := callToolJSON(t, s, ctx, "find_symbol", map[string]any{"query": "Area", "detail": "card"})

	if omittedBytes != explicitBytes {
		t.Fatalf("omitted detail = %d bytes, explicit card = %d bytes", omittedBytes, explicitBytes)
	}
	if fmt.Sprint(omitted) != fmt.Sprint(explicit) {
		t.Fatalf("omitted detail differs from explicit card:\n%v\n%v", omitted, explicit)
	}
}

func TestDetailLevelsExposeTheDocumentedFields(t *testing.T) {
	s, ctx := detailFixture(t)

	card, _ := callToolJSON(t, s, ctx, "find_symbol", map[string]any{"query": "Area", "detail": "card"})
	area := findRecord(t, recordsOf(t, card, "matches"), "Area")
	for _, absent := range []string{"signature", "doc_summary", "visibility", "source", "range", "file_id", "end_line"} {
		if _, ok := area[absent]; ok {
			t.Fatalf("card carries %q: %v", absent, area)
		}
	}
	for _, present := range []string{"name", "qualified_name", "kind", "file", "line", "symbol_id", "stable_key"} {
		if _, ok := area[present]; !ok {
			t.Fatalf("card is missing %q: %v", present, area)
		}
	}

	skeleton, _ := callToolJSON(t, s, ctx, "find_symbol", map[string]any{"query": "Area", "detail": "skeleton"})
	area = findRecord(t, recordsOf(t, skeleton, "matches"), "Area")
	if area["signature"] == nil || area["end_line"] == nil {
		t.Fatalf("skeleton is missing declaration fields: %v", area)
	}
	if _, ok := area["source"]; ok {
		t.Fatalf("skeleton carries source: %v", area)
	}

	excerpt, _ := callToolJSON(t, s, ctx, "find_symbol", map[string]any{"query": "Area", "detail": "excerpt"})
	area = findRecord(t, recordsOf(t, excerpt, "matches"), "Area")
	src, ok := area["source"].(map[string]any)
	if !ok {
		t.Fatalf("excerpt is missing source: %v", area)
	}
	if !strings.Contains(src["text"].(string), "w.Width * w.Height") {
		t.Fatalf("excerpt did not include the symbol's body: %v", src["text"])
	}

	full, _ := callToolJSON(t, s, ctx, "find_symbol", map[string]any{"query": "Area", "detail": "full"})
	area = findRecord(t, recordsOf(t, full, "matches"), "Area")
	if area["range"] == nil || area["file_id"] == nil || area["source"] == nil {
		t.Fatalf("full is missing legacy or source fields: %v", area)
	}
}

// TestFullPreservesEveryPreP13Field pins the compatibility promise: whatever the
// pre-P13 response carried is still reachable, at detail=full.
func TestFullPreservesEveryPreP13Field(t *testing.T) {
	s, ctx := detailFixture(t)
	full, _ := callToolJSON(t, s, ctx, "find_symbol", map[string]any{"query": "Area", "detail": "full"})
	area := findRecord(t, recordsOf(t, full, "matches"), "Area")

	// These are exactly the JSON names graph.Symbol serialized before P13.
	for _, field := range []string{
		"symbol_id", "file_id", "language", "kind", "name", "qualified_name",
		"container_name", "signature", "visibility", "range", "doc_summary",
		"stable_key", "file",
	} {
		if _, ok := area[field]; !ok {
			t.Fatalf("detail=full dropped the pre-P13 field %q: %v", field, area)
		}
	}
	rng := area["range"].(map[string]any)
	for _, field := range []string{"start_line", "start_col", "end_line", "end_col"} {
		if _, ok := rng[field]; !ok {
			t.Fatalf("range lost %q: %v", field, rng)
		}
	}
}

func TestInvalidDetailIsRejected(t *testing.T) {
	s, ctx := detailFixture(t)
	input := bytes.NewBuffer(nil)
	writeFrameToBuffer(t, input, map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": "find_symbol", "arguments": map[string]any{"query": "Area", "detail": "verbose"}},
	})
	var output bytes.Buffer
	if err := s.Serve(ctx, input, &output, io.Discard); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
	result := readAllFrames(t, &output)[0]["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatalf("invalid detail was accepted: %v", result)
	}
	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, "invalid detail") {
		t.Fatalf("unhelpful rejection message: %s", text)
	}
}

// TestDrillDownChainFromSearchToFull walks the progression an agent is expected
// to make: search cheaply, then use only what the card gave back to ask for
// more. If a card ever stops carrying a usable selector, this breaks.
func TestDrillDownChainFromSearchToFull(t *testing.T) {
	s, ctx := detailFixture(t)

	found, _ := callToolJSON(t, s, ctx, "search_symbols", map[string]any{"query": "Helper"})
	card := findRecord(t, recordsOf(t, found, "matches"), "Helper")

	qualified, _ := card["qualified_name"].(string)
	if qualified == "" {
		t.Fatalf("card has no qualified name: %v", card)
	}
	excerpt, _ := callToolJSON(t, s, ctx, "find_symbol", map[string]any{"query": qualified, "detail": "excerpt"})
	if len(recordsOf(t, excerpt, "matches")) == 0 {
		t.Fatalf("qualified name %q from a card did not resolve", qualified)
	}

	symbolID, ok := card["symbol_id"].(float64)
	if !ok || symbolID == 0 {
		t.Fatalf("card has no symbol id: %v", card)
	}
	callers, _ := callToolJSON(t, s, ctx, "find_callers", map[string]any{
		"symbol_id": int64(symbolID), "detail": "full",
	})
	names := map[string]bool{}
	for _, r := range recordsOf(t, callers, "callers") {
		names[r["name"].(string)] = true
	}
	if !names["UseHelper"] || !names["AlsoUseHelper"] {
		t.Fatalf("drill-down by symbol_id lost callers: %v", names)
	}
}

// TestImpactRadiusKeepsTraversalMetadataAtCardDetail guards the split between
// node detail and the traversal's own reporting: shrinking the symbols must not
// shrink the graph result around them.
func TestImpactRadiusKeepsTraversalMetadataAtCardDetail(t *testing.T) {
	s, ctx := detailFixture(t)

	full, _ := callToolJSON(t, s, ctx, "get_impact_radius", map[string]any{
		"symbols": []any{"Helper"}, "depth": 2, "detail": "full",
	})
	card, _ := callToolJSON(t, s, ctx, "get_impact_radius", map[string]any{
		"symbols": []any{"Helper"}, "depth": 2, "detail": "card",
	})

	if fmt.Sprint(card["files"]) != fmt.Sprint(full["files"]) {
		t.Fatalf("file list changed with node detail: %v vs %v", card["files"], full["files"])
	}
	if fmt.Sprint(card["summary"]) != fmt.Sprint(full["summary"]) {
		t.Fatalf("summary changed with node detail: %v vs %v", card["summary"], full["summary"])
	}
	if len(recordsOf(t, card, "symbols")) != len(recordsOf(t, full, "symbols")) {
		t.Fatal("node detail changed how many symbols the traversal reported")
	}
}

func TestExcerptTruncationIsExplicit(t *testing.T) {
	s, ctx := detailFixture(t)
	data, _ := callToolJSON(t, s, ctx, "find_symbol", map[string]any{"query": "Accumulate", "detail": "excerpt"})
	acc := findRecord(t, recordsOf(t, data, "matches"), "Accumulate")
	src := acc["source"].(map[string]any)

	if src["truncated"] != true {
		t.Fatalf("a 120-line body was returned without a truncation marker: %v", src)
	}
	if src["available_end_line"] == nil || src["available_start_line"] == nil {
		t.Fatalf("truncated excerpt did not report the full range: %v", src)
	}
	lines := int(src["end_line"].(float64)) - int(src["start_line"].(float64)) + 1
	if lines > detail.ExcerptMaxLines {
		t.Fatalf("excerpt spans %d lines, over the %d ceiling", lines, detail.ExcerptMaxLines)
	}

	full, _ := callToolJSON(t, s, ctx, "find_symbol", map[string]any{"query": "Accumulate", "detail": "full"})
	fullSrc := findRecord(t, recordsOf(t, full, "matches"), "Accumulate")["source"].(map[string]any)
	if fullSrc["truncated"] == true {
		t.Fatalf("detail=full truncated the body: %v", fullSrc)
	}
}

// TestPayloadShrinksAtLowerDetail is the token-savings harness. It measures the
// bytes the server writes, not a stand-in, and asserts ratios rather than exact
// sizes so ordinary code changes to the fixture do not make it brittle.
func TestPayloadShrinksAtLowerDetail(t *testing.T) {
	s, ctx := detailFixture(t)

	scenarios := []struct {
		name string
		tool string
		args map[string]any
	}{
		{"search_symbols", "search_symbols", map[string]any{"query": "Widget"}},
		{"find_callers", "find_callers", map[string]any{"symbol": "Helper"}},
		{"get_impact_radius", "get_impact_radius", map[string]any{"symbols": []any{"Helper"}, "depth": 2}},
	}

	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			sizes := map[detail.Level]int{}
			for _, level := range detail.Levels {
				args := map[string]any{"detail": string(level)}
				for k, v := range sc.args {
					args[k] = v
				}
				_, size := callToolJSON(t, s, ctx, sc.tool, args)
				sizes[level] = size
				t.Logf("%s detail=%s: %d bytes (~%d tokens)", sc.tool, level, size, size/4)
			}

			if sizes[detail.Card] > sizes[detail.Skeleton] {
				t.Fatalf("card (%d) is larger than skeleton (%d)", sizes[detail.Card], sizes[detail.Skeleton])
			}
			if sizes[detail.Card] > sizes[detail.Excerpt] {
				t.Fatalf("card (%d) is larger than excerpt (%d)", sizes[detail.Card], sizes[detail.Excerpt])
			}
			// The headline target: the default an agent gets is at most half of
			// what the same call costs at full detail.
			if sizes[detail.Card]*2 > sizes[detail.Full] {
				t.Fatalf("card (%d) is not at least 50%% smaller than full (%d)", sizes[detail.Card], sizes[detail.Full])
			}
		})
	}
}

// TestDetailSchemaMatchesTheCodeVocabulary keeps tools/list, the argument
// validator, and the Level type from drifting apart.
func TestDetailSchemaMatchesTheCodeVocabulary(t *testing.T) {
	want := map[string]bool{
		"find_symbol": true, "search_symbols": true, "find_callers": true,
		"find_callees": true, "get_impact_radius": true,
	}
	seen := map[string]bool{}

	for _, def := range buildToolDefinitions() {
		name := def["name"].(string)
		props := def["inputSchema"].(map[string]any)["properties"].(map[string]any)
		prop, ok := props["detail"]
		if !ok {
			if want[name] {
				t.Fatalf("%s has no detail property", name)
			}
			continue
		}
		seen[name] = true
		if !want[name] {
			t.Fatalf("%s advertises detail but is not a migrated tool", name)
		}
		schema := prop.(map[string]any)
		enum := schema["enum"].([]string)
		if fmt.Sprint(enum) != fmt.Sprint(detail.Names()) {
			t.Fatalf("%s enum = %v, want %v", name, enum, detail.Names())
		}
		if schema["default"] != string(defaultToolDetail) {
			t.Fatalf("%s default = %v, want %v", name, schema["default"], defaultToolDetail)
		}
		if spec := toolArgumentSpecs[name]; spec.properties["detail"] != "string" {
			t.Fatalf("%s accepts detail in tools/list but not in the validator", name)
		}
	}
	for name := range want {
		if !seen[name] {
			t.Fatalf("%s was expected to advertise detail", name)
		}
	}
}
