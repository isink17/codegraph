package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/query"
	"github.com/isink17/codegraph/internal/store"
)

// newContextServer indexes a small repository and returns an MCP server over it.
func newContextServer(t *testing.T) *Server {
	t.Helper()
	ctx := context.Background()
	repoRoot := t.TempDir()
	writeRepoFile(t, repoRoot, "paymentsvc/service.go", `package paymentsvc

// ProcessPayment charges a payment and retries the charge on failure.
func ProcessPayment(id string) error {
	return chargeCard(id)
}

// chargeCard performs the payment charge attempt.
func chargeCard(id string) error { return nil }
`)
	writeRepoFile(t, repoRoot, "handler/handler.go", `package handler

import "example.test/fixture/paymentsvc"

// HandleCheckout is the HTTP entry point for a checkout request.
func HandleCheckout(id string) error { return paymentsvc.ProcessPayment(id) }
`)
	writeRepoFile(t, repoRoot, "paymentsvc/service_test.go", `package paymentsvc

import "testing"

func TestProcessPayment(t *testing.T) {
	if err := ProcessPayment("id"); err != nil {
		t.Fatal(err)
	}
}
`)
	writeRepoFile(t, repoRoot, "go.mod", "module example.test/fixture\n\ngo 1.22\n")

	s, err := store.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	idx := indexer.New(s, parser.NewRegistry(goparser.New()), nil)
	repo, err := s.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	if _, err := idx.Index(ctx, indexer.Options{RepoRoot: repoRoot}); err != nil {
		t.Fatalf("Index() error = %v", err)
	}
	return NewServer(repoRoot, repoRoot, repo.ID, s, idx, query.New(s, nil), io.Discard)
}

// callToolViaServe drives one tools/call through the production stdio path and
// returns the decoded tool payload.
func callToolViaServe(t *testing.T, server *Server, name string, args map[string]any) map[string]any {
	t.Helper()
	var input bytes.Buffer
	writeFrameToBuffer(t, &input, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  map[string]any{"name": name, "arguments": args},
	})
	var output bytes.Buffer
	if err := server.Serve(context.Background(), &input, &output, io.Discard); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
	responses := readAllFrames(t, &output)
	if len(responses) != 1 {
		t.Fatalf("response count = %d, want 1", len(responses))
	}
	result, ok := responses[0]["result"].(map[string]any)
	if !ok {
		t.Fatalf("no result in response: %v", responses[0])
	}
	text := result["content"].([]any)[0].(map[string]any)["text"].(string)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(text), &parsed); err != nil {
		t.Fatalf("json.Unmarshal(%q) error = %v", text, err)
	}
	if result["isError"] == true {
		parsed["_isError"] = true
	}
	return parsed
}

func contextData(t *testing.T, payload map[string]any) map[string]any {
	t.Helper()
	if payload["_isError"] == true {
		t.Fatalf("tool returned an error: %v", payload["error"])
	}
	data, ok := payload["data"].(map[string]any)
	if !ok {
		t.Fatalf("no data object in payload: %v", payload)
	}
	return data
}

// The regression that started P14: the production handler used to answer
// {"files":null} for an indexed repository and call it a success.
func TestContextForTaskThroughHandlerReturnsRealContext(t *testing.T) {
	server := newContextServer(t)
	data := contextData(t, callToolViaServe(t, server, "context_for_task", map[string]any{
		"task": "process payment retry",
	}))

	files, ok := data["files"].([]any)
	if !ok || len(files) == 0 {
		t.Fatalf("files = %v, want a non-empty array", data["files"])
	}
	if got := int(data["max_tokens"].(float64)); got != query.DefaultContextMaxTokens {
		t.Fatalf("max_tokens = %d, want %d", got, query.DefaultContextMaxTokens)
	}
	estimated := int(data["estimated_tokens"].(float64))
	if estimated <= 0 || estimated > query.DefaultContextMaxTokens {
		t.Fatalf("estimated_tokens = %d, want 0 < n <= %d", estimated, query.DefaultContextMaxTokens)
	}

	sawDirect := false
	for _, raw := range files {
		for _, rawSym := range raw.(map[string]any)["symbols"].([]any) {
			sym := rawSym.(map[string]any)
			if sym["relevance"] == "direct_match" {
				sawDirect = true
			}
			if sym["symbol_id"] == nil || sym["stable_key"] == nil {
				t.Fatalf("symbol lacks drill-down identity: %v", sym)
			}
		}
	}
	if !sawDirect {
		t.Fatalf("no direct_match symbol in %v", files)
	}
}

// The intended workflow: budgeted discovery, then explicit source drill-down
// with the identity the discovery returned.
func TestContextForTaskIdentityDrivesP13DrillDown(t *testing.T) {
	server := newContextServer(t)
	data := contextData(t, callToolViaServe(t, server, "context_for_task", map[string]any{
		"task": "process payment retry",
	}))

	var target map[string]any
	for _, raw := range data["files"].([]any) {
		for _, rawSym := range raw.(map[string]any)["symbols"].([]any) {
			sym := rawSym.(map[string]any)
			if sym["qualified_name"] == "paymentsvc.ProcessPayment" {
				target = sym
			}
		}
	}
	if target == nil {
		t.Fatalf("paymentsvc.ProcessPayment missing from context: %v", data)
	}
	wantID := int64(target["symbol_id"].(float64))

	for _, level := range []string{"excerpt", "full"} {
		payload := contextData(t, callToolViaServe(t, server, "find_symbol", map[string]any{
			"query":  target["qualified_name"],
			"detail": level,
		}))
		matches := payload["matches"].([]any)
		if len(matches) == 0 {
			t.Fatalf("find_symbol(detail=%s) returned no matches", level)
		}
		match := matches[0].(map[string]any)
		if int64(match["symbol_id"].(float64)) != wantID {
			t.Fatalf("find_symbol(detail=%s) resolved symbol_id %v, want %d", level, match["symbol_id"], wantID)
		}
		source, ok := match["source"].(map[string]any)
		if !ok {
			t.Fatalf("find_symbol(detail=%s) returned no source: %v", level, match)
		}
		if text, _ := source["text"].(string); !strings.Contains(text, "func ProcessPayment") {
			t.Fatalf("find_symbol(detail=%s) source does not hold the symbol: %v", level, source)
		}
	}

	// The same identity drives graph traversal.
	callers := contextData(t, callToolViaServe(t, server, "find_callers", map[string]any{
		"symbol_id": wantID,
	}))
	if len(callers["callers"].([]any)) == 0 {
		t.Fatalf("find_callers(symbol_id=%d) returned nothing: %v", wantID, callers)
	}
}

// Continuation through the production handler, including the has_more contract
// and a budget change between pages.
func TestContextForTaskCursorThroughHandler(t *testing.T) {
	server := newContextServer(t)
	whole := contextData(t, callToolViaServe(t, server, "context_for_task", map[string]any{
		"task":       "process payment retry",
		"max_tokens": query.MaxContextMaxTokens,
	}))
	wantSymbols := int(whole["returned_symbols"].(float64))
	if wantSymbols < 3 {
		t.Fatalf("fixture yields %d symbols; the cursor test needs more", wantSymbols)
	}

	page := contextData(t, callToolViaServe(t, server, "context_for_task", map[string]any{
		"task":       "process payment retry",
		"max_tokens": 200,
	}))
	if page["has_more"] != true {
		t.Fatalf("expected has_more with a 200-token budget: %v", page)
	}
	cursor, ok := page["next_cursor"].(string)
	if !ok || cursor == "" {
		t.Fatalf("no next_cursor: %v", page)
	}
	seen := int(page["returned_symbols"].(float64))

	for i := 0; i < 20 && cursor != ""; i++ {
		next := contextData(t, callToolViaServe(t, server, "context_for_task", map[string]any{
			"task":       "process payment retry",
			"max_tokens": 400, // a different budget on continuation is allowed
			"cursor":     cursor,
		}))
		seen += int(next["returned_symbols"].(float64))
		if next["has_more"] == true {
			cursor = next["next_cursor"].(string)
			continue
		}
		if _, ok := next["next_cursor"]; ok {
			t.Fatalf("final page carries next_cursor: %v", next)
		}
		cursor = ""
	}
	if cursor != "" {
		t.Fatal("cursor did not terminate")
	}
	if seen != wantSymbols {
		t.Fatalf("paged walk saw %d symbols, unbudgeted call returned %d", seen, wantSymbols)
	}
}

func TestContextForTaskHandlerRejectsBadArguments(t *testing.T) {
	server := newContextServer(t)
	cases := map[string]map[string]any{
		"negative max_tokens": {"task": "payment", "max_tokens": -1},
		"malformed cursor":    {"task": "payment", "cursor": "!!!not base64!!!"},
		"unknown argument":    {"task": "payment", "nonsense": 1},
	}
	for name, args := range cases {
		payload := callToolViaServe(t, server, "context_for_task", args)
		if payload["_isError"] != true {
			t.Fatalf("%s: expected a tool error, got %v", name, payload)
		}
	}
}

// Schema, argument validation, and handler must agree on the argument set.
func TestContextForTaskSchemaParity(t *testing.T) {
	var schemaProps map[string]any
	for _, def := range buildToolDefinitions() {
		if def["name"] == "context_for_task" {
			schemaProps = def["inputSchema"].(map[string]any)["properties"].(map[string]any)
		}
	}
	if schemaProps == nil {
		t.Fatal("context_for_task missing from tools/list")
	}
	spec := toolArgumentSpecs["context_for_task"]
	for name := range spec.properties {
		if _, ok := schemaProps[name]; !ok {
			t.Fatalf("argument %q is validated but absent from the schema", name)
		}
	}
	for name := range schemaProps {
		if _, ok := spec.properties[name]; !ok {
			t.Fatalf("argument %q is in the schema but not validated", name)
		}
	}
	for _, name := range []string{"max_tokens", "cursor"} {
		prop := schemaProps[name].(map[string]any)
		if desc, _ := prop["description"].(string); desc == "" {
			t.Fatalf("argument %q has no description", name)
		}
	}
	if got := schemaProps["max_tokens"].(map[string]any)["default"]; got != query.DefaultContextMaxTokens {
		t.Fatalf("max_tokens default = %v, want %d", got, query.DefaultContextMaxTokens)
	}
}
