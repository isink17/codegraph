package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/query"
	"github.com/isink17/codegraph/internal/tokenest"
)

// preGatewayToolNames is the tool surface as it was advertised before gateway
// mode existed, in the order it was advertised.
//
// It is written out rather than derived so that a registry edit which drops,
// renames, or reorders a tool fails here instead of silently changing what every
// existing full-mode client sees.
var preGatewayToolNames = []string{
	"index_repo", "update_graph", "find_symbol", "find_callers", "find_callees",
	"get_impact_radius", "find_related_tests", "search_symbols", "search_semantic",
	"graph_stats", "supported_languages", "list_repos", "list_scans",
	"latest_scan_errors", "context_for_task", "find_dead_code", "list_files",
	"architecture_overview", "trace_dependencies", "detect_frameworks",
	"benchmark_tokens", "cross_language_links", "session_log", "session_history",
	"session_hot_files", "session_context", "graph_analytics", "agentic_query",
	"audit",
}

// wantGatewayToolNames is the gateway surface, in order: the core workflow tools
// first, then discovery.
var wantGatewayToolNames = []string{
	"find_symbol", "find_callers", "find_callees", "context_for_task",
	"tool_search", "tool_call",
}

// newGatewayTestServer builds a server over a small indexed repository. Every
// test in this file drives it through Serve, the production stdio path, rather
// than calling handlers directly: the thing under test is a wire contract.
func newGatewayTestServer(t *testing.T, mode ToolMode) *Server {
	t.Helper()
	ctx := context.Background()
	repoRoot := t.TempDir()
	writeRepoFile(t, repoRoot, "main.go", `package main

func Helper() int { return 1 }

func Caller() int { return Helper() }

func main() { _ = Caller() }
`)
	writeRepoFile(t, repoRoot, "main_test.go", `package main

import "testing"

func TestHelper(t *testing.T) { _ = Helper() }
`)

	st := openTestStore(t)
	t.Cleanup(func() { st.Close() })
	idx := indexer.New(st, parser.NewRegistry(goparser.New()), nil)
	repo, err := st.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	if _, err := idx.Index(ctx, indexer.Options{RepoRoot: repoRoot}); err != nil {
		t.Fatalf("Index() error = %v", err)
	}
	server := NewServer(repoRoot, repoRoot, repo.ID, st, idx, query.New(st, nil), io.Discard)
	if mode != "" {
		if err := server.SetToolMode(mode); err != nil {
			t.Fatalf("SetToolMode(%q) error = %v", mode, err)
		}
	}
	return server
}

// rpcRoundTrip sends requests through Serve and returns the decoded responses.
func rpcRoundTrip(t *testing.T, server *Server, requests ...map[string]any) []map[string]any {
	t.Helper()
	input := bytes.NewBuffer(nil)
	for _, req := range requests {
		writeFrameToBuffer(t, input, req)
	}
	var output bytes.Buffer
	if err := server.Serve(context.Background(), input, &output, io.Discard); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
	return readAllFrames(t, &output)
}

func initializeRequest(id int) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "method": "initialize", "params": map[string]any{}}
}

func toolsListRequest(id int) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/list", "params": map[string]any{}}
}

func toolsCallRequest(id int, name string, args any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args}}
}

// listedTools drives initialize + tools/list and returns the advertised
// definitions as decoded JSON.
func listedTools(t *testing.T, server *Server) []map[string]any {
	t.Helper()
	responses := rpcRoundTrip(t, server, initializeRequest(1), toolsListRequest(2))
	if len(responses) != 2 {
		t.Fatalf("response count = %d, want 2", len(responses))
	}
	result, ok := responses[1]["result"].(map[string]any)
	if !ok {
		t.Fatalf("tools/list result = %v, want an object", responses[1])
	}
	raw, ok := result["tools"].([]any)
	if !ok {
		t.Fatalf("tools/list tools = %v, want an array", result["tools"])
	}
	tools := make([]map[string]any, 0, len(raw))
	for _, item := range raw {
		def, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("tool definition = %v, want an object", item)
		}
		tools = append(tools, def)
	}
	return tools
}

func toolNames(tools []map[string]any) []string {
	names := make([]string, 0, len(tools))
	for _, def := range tools {
		name, _ := def["name"].(string)
		names = append(names, name)
	}
	return names
}

// callResult drives one tools/call and returns (isError, model-visible text).
func callResult(t *testing.T, server *Server, name string, args any) (bool, string) {
	t.Helper()
	responses := rpcRoundTrip(t, server, initializeRequest(1), toolsCallRequest(2, name, args))
	if len(responses) != 2 {
		t.Fatalf("response count = %d, want 2", len(responses))
	}
	result, ok := responses[1]["result"].(map[string]any)
	if !ok {
		t.Fatalf("tools/call produced no result: %v", responses[1])
	}
	isError, ok := result["isError"].(bool)
	if !ok {
		t.Fatalf("isError = %v, want a bool", result["isError"])
	}
	content, ok := result["content"].([]any)
	if !ok || len(content) != 1 {
		t.Fatalf("content = %v, want exactly one part", result["content"])
	}
	part, ok := content[0].(map[string]any)
	if !ok || part["type"] != "text" {
		t.Fatalf("content part = %v, want a text part", content[0])
	}
	text, ok := part["text"].(string)
	if !ok {
		t.Fatalf("content text = %v, want a string", part["text"])
	}
	return isError, text
}

// callError asserts a call failed and returns the decoded error message.
//
// The message is read out of the envelope rather than matched against the raw
// text: the payload is JSON, so a message containing a quoted tool name is
// escaped on the wire. Decoding it is also the assertion that the P14 escaping fix
// still holds through the gateway -- a spliced blob would not parse.
func callError(t *testing.T, server *Server, name string, args any) string {
	t.Helper()
	isError, text := callResult(t, server, name, args)
	if !isError {
		t.Fatalf("%s(%v) succeeded, want an error: %s", name, args, text)
	}
	var envelope struct {
		OK    bool   `json:"ok"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(text), &envelope); err != nil {
		t.Fatalf("%s error payload is not JSON: %v (%s)", name, err, text)
	}
	if envelope.OK {
		t.Errorf("%s error envelope ok = true, want false: %s", name, text)
	}
	return envelope.Error
}

// searchTools runs tool_search and returns its decoded data object.
func searchTools(t *testing.T, server *Server, args any) map[string]any {
	t.Helper()
	isError, text := callResult(t, server, "tool_search", args)
	if isError {
		t.Fatalf("tool_search(%v) failed: %s", args, text)
	}
	var envelope struct {
		OK   bool           `json:"ok"`
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal([]byte(text), &envelope); err != nil {
		t.Fatalf("tool_search result is not JSON: %v (%s)", err, text)
	}
	if !envelope.OK {
		t.Fatalf("tool_search ok = false: %s", text)
	}
	return envelope.Data
}

func searchNames(t *testing.T, data map[string]any) []string {
	t.Helper()
	tools, ok := data["tools"].([]any)
	if !ok {
		t.Fatalf("tool_search tools = %v, want an array", data["tools"])
	}
	names := make([]string, 0, len(tools))
	for _, item := range tools {
		entry, ok := item.(map[string]any)
		if !ok {
			t.Fatalf("tool_search entry = %v, want an object", item)
		}
		name, _ := entry["name"].(string)
		names = append(names, name)
	}
	return names
}

func TestParseToolMode(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    ToolMode
		wantErr bool
	}{
		{in: "", want: ToolModeFull},
		{in: "full", want: ToolModeFull},
		{in: "gateway", want: ToolModeGateway},
		{in: " gateway ", want: ToolModeGateway},
		{in: "minimal", wantErr: true},
		{in: "GATEWAY", wantErr: true},
		{in: "auto", wantErr: true},
	} {
		got, err := ParseToolMode(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseToolMode(%q) error = nil, want an error", tc.in)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseToolMode(%q) error = %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseToolMode(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestDefaultToolModeIsFull is the backward-compatibility guarantee: a server
// built the way it was built before gateway mode existed advertises the same
// surface it did then.
func TestDefaultToolModeIsFull(t *testing.T) {
	server := newGatewayTestServer(t, "")
	if got := server.ToolMode(); got != ToolModeFull {
		t.Fatalf("default ToolMode() = %q, want %q", got, ToolModeFull)
	}
	if got := toolNames(listedTools(t, server)); !reflect.DeepEqual(got, preGatewayToolNames) {
		t.Fatalf("default tools/list = %v,\nwant %v", got, preGatewayToolNames)
	}
}

// fullToolsListBytes and fullToolsListTokens pin the exact size of the default
// `tools/list` payload as measured on the commit that introduced gateway mode.
//
// This is the number every existing session pays before it asks a question, so a
// change to it is a change to every user's context cost. The constant is not a
// style rule: if a deliberate edit moves it, update it here and say so in the
// commit, which is exactly the review the number deserves.
//
// P18 moved these deliberately. The public size policy is now machine-readable:
// every advertised `limit`, `offset`, and `depth` carries the minimum and
// maximum the validator actually enforces, and get_impact_radius and
// trace_dependencies gained the limit/offset pair that bounds their traversal.
// The current contract edit grew full tools/list 11870 -> 12000 bytes (+130)
// and gateway 3968 -> 4062 (+94). These are deliberate per-session costs for
// more precise relationship and trace descriptions; schemas and arguments are
// unchanged. A client that can read a bound does not have to discover it by
// being rejected.
const (
	fullToolsListBytes  = 12000
	fullToolsListTokens = 3000
	// The gateway payload is pinned for the same reason, and became load-bearing
	// once a registry row could be hidden from a list: a hidden tool that leaked
	// into the gateway surface would show up here as a byte count, not as a name
	// nobody happened to assert on.
	gatewayToolsListBytes  = 4062
	gatewayToolsListTokens = 1016
)

// TestGatewayToolsListIsByteIdentical pins the reduced surface's exact size.
func TestGatewayToolsListIsByteIdentical(t *testing.T) {
	payload, err := json.Marshal(listedTools(t, newGatewayTestServer(t, ToolModeGateway)))
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if len(payload) != gatewayToolsListBytes {
		t.Errorf("gateway tools/list = %d bytes, want %d.\n"+
			"If the change is deliberate, update gatewayToolsListBytes/gatewayToolsListTokens "+
			"and note the new per-session cost in the commit message.\n%s",
			len(payload), gatewayToolsListBytes, payload)
	}
	if got := tokenest.FromBytes(len(payload)); got != gatewayToolsListTokens {
		t.Errorf("gateway tools/list = %d estimated tokens, want %d", got, gatewayToolsListTokens)
	}
}

// TestFullToolsListIsByteIdentical is the backward-compatibility guard the
// name-only and ratio-only assertions cannot give: it fails on a changed
// description, a changed property type, or a new argument, any of which silently
// alters the default payload.
func TestFullToolsListIsByteIdentical(t *testing.T) {
	payload, err := json.Marshal(listedTools(t, newGatewayTestServer(t, "")))
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if len(payload) != fullToolsListBytes {
		t.Errorf("full tools/list = %d bytes, want %d.\n"+
			"This is the default surface every existing client sees. If the change is "+
			"deliberate, update fullToolsListBytes/fullToolsListTokens and note the new "+
			"per-session cost in the commit message.\n%s",
			len(payload), fullToolsListBytes, payload)
	}
	if got := tokenest.FromBytes(len(payload)); got != fullToolsListTokens {
		t.Errorf("full tools/list = %d estimated tokens, want %d", got, fullToolsListTokens)
	}
}

// TestExplicitFullModeMatchesDefault asserts the two spellings of the default are
// byte-identical, schemas and all -- not merely the same set of names.
func TestExplicitFullModeMatchesDefault(t *testing.T) {
	def := newGatewayTestServer(t, "")
	explicit := newGatewayTestServer(t, ToolModeFull)
	defBytes, err := json.Marshal(listedTools(t, def))
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	explicitBytes, err := json.Marshal(listedTools(t, explicit))
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if !bytes.Equal(defBytes, explicitBytes) {
		t.Fatalf("--tool-mode full differs from the default surface:\n%s\n%s", defBytes, explicitBytes)
	}
}

// TestGatewayToolsListIsSmallAndDeterministic covers the surface contract: which
// tools, in which order, and how many.
func TestGatewayToolsListIsSmallAndDeterministic(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeGateway)
	first := toolNames(listedTools(t, server))
	if !reflect.DeepEqual(first, wantGatewayToolNames) {
		t.Fatalf("gateway tools/list = %v,\nwant %v", first, wantGatewayToolNames)
	}
	if len(first) > 8 {
		t.Fatalf("gateway tools/list has %d tools, want at most 8", len(first))
	}
	// Same server, second call, and a second server: no map iteration order.
	if second := toolNames(listedTools(t, server)); !reflect.DeepEqual(first, second) {
		t.Fatalf("gateway tools/list is not deterministic: %v then %v", first, second)
	}
	other := toolNames(listedTools(t, newGatewayTestServer(t, ToolModeGateway)))
	if !reflect.DeepEqual(first, other) {
		t.Fatalf("gateway tools/list differs between servers: %v vs %v", first, other)
	}
}

// TestGatewayHidesTheSpecializedToolsButKeepsTheirSchemas asserts both halves of
// the trade: hidden tools are absent from the list, and the schema a gateway core
// tool advertises is the one full mode advertises.
func TestGatewayHidesTheSpecializedToolsButKeepsTheirSchemas(t *testing.T) {
	full := listedTools(t, newGatewayTestServer(t, ToolModeFull))
	gateway := listedTools(t, newGatewayTestServer(t, ToolModeGateway))

	fullByName := map[string]map[string]any{}
	for _, def := range full {
		fullByName[def["name"].(string)] = def
	}
	gatewayByName := map[string]map[string]any{}
	for _, def := range gateway {
		gatewayByName[def["name"].(string)] = def
	}

	for _, name := range preGatewayToolNames {
		def, listed := gatewayByName[name]
		core := false
		for _, want := range wantGatewayToolNames {
			if want == name {
				core = true
			}
		}
		if core != listed {
			t.Errorf("tool %q listed in gateway mode = %v, want %v", name, listed, core)
			continue
		}
		if !core {
			continue
		}
		if !reflect.DeepEqual(def, fullByName[name]) {
			t.Errorf("gateway definition of %q differs from full mode:\n%v\n%v", name, def, fullByName[name])
		}
	}

	// The meta tools exist only in gateway mode: adding them to the default
	// surface would charge every existing session for them.
	for _, name := range []string{"tool_search", "tool_call"} {
		if _, listed := fullByName[name]; listed {
			t.Errorf("meta tool %q is advertised in full mode", name)
		}
		if _, listed := gatewayByName[name]; !listed {
			t.Errorf("meta tool %q is missing from the gateway surface", name)
		}
	}
}

// TestGatewayToolsListSavesTokens is the measurement P16 exists for. The floor is
// asserted, not the exact number, so an honest description edit does not fail the
// build -- but a gateway that stops being small does.
func TestGatewayToolsListSavesTokens(t *testing.T) {
	fullBytes, err := json.Marshal(listedTools(t, newGatewayTestServer(t, ToolModeFull)))
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	gatewayBytes, err := json.Marshal(listedTools(t, newGatewayTestServer(t, ToolModeGateway)))
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	fullTokens := tokenest.FromBytes(len(fullBytes))
	gatewayTokens := tokenest.FromBytes(len(gatewayBytes))
	saved := 100 * (1 - float64(gatewayTokens)/float64(fullTokens))
	t.Logf("tools/list: full %d bytes / %d tokens, gateway %d bytes / %d tokens, saved %.1f%%",
		len(fullBytes), fullTokens, len(gatewayBytes), gatewayTokens, saved)
	if saved < 60 {
		t.Errorf("gateway tools/list saves %.1f%% of estimated tokens, want at least 60%%", saved)
	}
}

// TestToolSearchFindsHiddenToolsByIntent checks discovery against the queries an
// agent would actually type. The expected answers are not hardcoded anywhere in
// the implementation: they fall out of names, categories, and descriptions.
func TestToolSearchFindsHiddenToolsByIntent(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeGateway)
	for _, tc := range []struct{ query, want string }{
		{"tests", "find_related_tests"},
		{"audit integrity", "audit"},
		{"dependency trace", "trace_dependencies"},
		{"index repository", "index_repo"},
		{"token benchmark", "benchmark_tokens"},
		{"dead code", "find_dead_code"},
		{"impact radius", "get_impact_radius"},
		{"session history", "session_history"},
		{"semantic search", "search_semantic"},
		{"architecture", "architecture_overview"},
	} {
		names := searchNames(t, searchTools(t, server, map[string]any{"query": tc.query}))
		if len(names) == 0 {
			t.Errorf("tool_search(%q) found nothing", tc.query)
			continue
		}
		if names[0] != tc.want {
			t.Errorf("tool_search(%q) top match = %q, want %q (got %v)", tc.query, names[0], tc.want, names)
		}
	}
}

// TestToolSearchIsBoundedAndDeterministic covers the output bound. A new API is
// bounded at its own source rather than inheriting an unbounded limit.
func TestToolSearchIsBoundedAndDeterministic(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeGateway)

	data := searchTools(t, server, map[string]any{"query": "find"})
	if got := len(searchNames(t, data)); got > defaultToolSearchLimit {
		t.Errorf("tool_search default returned %d tools, want at most %d", got, defaultToolSearchLimit)
	}

	huge := searchTools(t, server, map[string]any{"query": "s", "limit": 100000000})
	names := searchNames(t, huge)
	if len(names) > maxToolSearchLimit {
		t.Errorf("tool_search(limit=1e8) returned %d tools, want at most %d", len(names), maxToolSearchLimit)
	}
	if got := huge["limit"]; got != float64(maxToolSearchLimit) {
		t.Errorf("reported limit = %v, want %d", got, maxToolSearchLimit)
	}
	// A truncated result still reports how many matched, so an agent knows it is
	// looking at a page rather than the whole answer.
	if total, _ := huge["total_matches"].(float64); int(total) < len(names) {
		t.Errorf("total_matches = %v, want at least the %d returned", huge["total_matches"], len(names))
	}
	if again := searchNames(t, searchTools(t, server, map[string]any{"query": "s", "limit": 100000000})); !reflect.DeepEqual(names, again) {
		t.Errorf("tool_search is not deterministic: %v then %v", names, again)
	}

	// Discovery never returns the meta tools: they are already in tools/list.
	for _, name := range searchNames(t, searchTools(t, server, map[string]any{"query": "tool", "limit": maxToolSearchLimit})) {
		if name == "tool_search" || name == "tool_call" {
			t.Errorf("tool_search returned the meta tool %q", name)
		}
	}

	// A miss is an empty result, not an error.
	miss := searchTools(t, server, map[string]any{"query": "zzzznotatool"})
	if got := len(searchNames(t, miss)); got != 0 {
		t.Errorf("tool_search(no match) returned %d tools, want 0", got)
	}
}

// TestToolSearchExactNameReturnsTheCanonicalSchema is the schema-discovery
// contract: what an agent reads before calling tool_call is the schema the
// validator enforces, not a gateway paraphrase of it.
func TestToolSearchExactNameReturnsTheCanonicalSchema(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeGateway)
	full := listedTools(t, newGatewayTestServer(t, ToolModeFull))
	canonical := map[string]any{}
	for _, def := range full {
		canonical[def["name"].(string)] = def["inputSchema"]
	}

	for _, name := range []string{"trace_dependencies", "audit", "get_impact_radius", "session_log"} {
		data := searchTools(t, server, map[string]any{"query": name, "include_schema": true})
		tools, _ := data["tools"].([]any)
		if len(tools) != 1 {
			t.Fatalf("tool_search(%q, include_schema) returned %d tools, want 1", name, len(tools))
		}
		entry := tools[0].(map[string]any)
		if entry["name"] != name {
			t.Fatalf("tool_search(%q, include_schema) returned %q", name, entry["name"])
		}
		if !reflect.DeepEqual(entry["input_schema"], canonical[name]) {
			t.Errorf("tool_search schema for %q differs from tools/list:\n%v\n%v",
				name, entry["input_schema"], canonical[name])
		}
	}

	// Schemas are on demand only. A keyword search stays card-sized however many
	// tools it matches.
	data := searchTools(t, server, map[string]any{"query": "graph", "limit": maxToolSearchLimit})
	if strings.Contains(mustJSON(t, data), "input_schema") {
		t.Errorf("keyword tool_search returned schemas: %s", mustJSON(t, data))
	}
	if _, ok := data["note"]; ok {
		t.Errorf("keyword search without include_schema should carry no note: %v", data["note"])
	}
	// Asking for a schema without an exact name says so rather than dumping every
	// matching schema.
	ambiguous := searchTools(t, server, map[string]any{"query": "graph", "include_schema": true})
	if strings.Contains(mustJSON(t, ambiguous), "input_schema") {
		t.Errorf("ambiguous include_schema returned schemas: %s", mustJSON(t, ambiguous))
	}
	if note, _ := ambiguous["note"].(string); !strings.Contains(note, "exact tool name") {
		t.Errorf("ambiguous include_schema note = %q, want an explanation", note)
	}

	// tool_search reports the two facts an agent needs before calling a tool it
	// has not seen: whether it is already direct, and whether it writes.
	writes := searchTools(t, server, map[string]any{"query": "session_log", "include_schema": true})
	entry := writes["tools"].([]any)[0].(map[string]any)
	if entry["mutates"] != true {
		t.Errorf("session_log mutates = %v, want true", entry["mutates"])
	}
	if entry["listed"] != false {
		t.Errorf("session_log listed = %v, want false", entry["listed"])
	}
	core := searchTools(t, server, map[string]any{"query": "find_symbol", "include_schema": true})
	if got := core["tools"].([]any)[0].(map[string]any)["listed"]; got != true {
		t.Errorf("find_symbol listed = %v, want true", got)
	}
}

// TestToolCallMatchesDirectCallExactly is the parity requirement. Each case is
// answered by a full-mode server directly and by a gateway server through
// tool_call; the model-visible text and the error flag must be identical.
//
// The cases cover a flat query tool, a P13 detail tool, two P15 compact tools, a
// P14 budgeted context call, an admin read, a mutating tool, and five kinds of
// invalid argument -- the places a second validation path or a second serializer
// would show up.
func TestToolCallMatchesDirectCallExactly(t *testing.T) {
	// One server, two modes. A second server would sit on a different temp
	// repository root, and every path in the payload would differ for reasons that
	// have nothing to do with the gateway.
	server := newGatewayTestServer(t, ToolModeFull)

	// Every case is held to byte equality. There used to be an `unstable` escape
	// hatch here for graph_analytics pagerank, whose own output was not stable
	// across two identical calls; P19 gave the analytics a semantic total order,
	// so the waiver is gone and the tool is compared like any other.
	//
	// pagerank is the case that carries that: this fixture has enough symbols
	// and edges to produce a real ranked page. The coupling and cycles cases
	// below are close to vacuous here -- one row and none respectively -- and
	// are present for shape, not for tie-breaking. Their tie-breaking is proved
	// in internal/store/analytics_determinism_test.go, which builds graphs
	// specifically to tie.
	cases := []struct {
		name string
		args map[string]any
	}{
		{name: "audit", args: map[string]any{"examples": 2}},
		{name: "architecture_overview", args: map[string]any{}},
		{name: "search_symbols", args: map[string]any{"query": "Helper", "detail": "card"}},
		{name: "search_symbols", args: map[string]any{"query": "Helper", "detail": "skeleton"}},
		{name: "search_symbols", args: map[string]any{"query": "Helper", "detail": "excerpt"}},
		{name: "search_symbols", args: map[string]any{"query": "Helper", "format": "compact"}},
		{name: "find_dead_code", args: map[string]any{"limit": 5, "format": "compact"}},
		{name: "list_files", args: map[string]any{"format": "compact"}},
		{name: "get_impact_radius", args: map[string]any{"symbols": []any{"Helper"}, "depth": 2}},
		{name: "find_related_tests", args: map[string]any{"symbol": "Helper"}},
		{name: "trace_dependencies", args: map[string]any{"symbol": "Helper", "depth": 2}},
		{name: "context_for_task", args: map[string]any{"task": "change Helper", "max_tokens": 1200}},
		{name: "graph_stats", args: map[string]any{}},
		{name: "graph_analytics", args: map[string]any{"analysis": "pagerank", "limit": 3}},
		{name: "graph_analytics", args: map[string]any{"analysis": "coupling", "limit": 5}},
		{name: "graph_analytics", args: map[string]any{"analysis": "cycles", "limit": 5}},
		{name: "session_log", args: map[string]any{"event_type": "read", "key": "main.go", "session_id": "parity"}},
		{name: "session_history", args: map[string]any{"session_id": "parity", "limit": 5}},
		{name: "list_scans", args: map[string]any{"limit": 2}},
		// Invalid arguments: the canonical validator must be the one that answers.
		{name: "find_callers", args: map[string]any{}},
		{name: "search_symbols", args: map[string]any{"query": ""}},
		{name: "search_symbols", args: map[string]any{"query": "Helper", "bogus": 1}},
		{name: "search_symbols", args: map[string]any{"query": "Helper", "detail": "everything"}},
		{name: "search_symbols", args: map[string]any{"query": "Helper", "format": "yaml"}},
		{name: "search_symbols", args: map[string]any{"query": "Helper", "detail": "full", "format": "compact"}},
		{name: "find_symbol", args: map[string]any{"query": "Helper", "limit": "ten"}},
		{name: "audit", args: map[string]any{"examples": -1}},
		{name: "get_impact_radius", args: map[string]any{"depth": 2}},
		{name: "graph_analytics", args: map[string]any{"analysis": "nope"}},
	}

	for _, tc := range cases {
		label := tc.name + " " + mustJSON(t, tc.args)
		if err := server.SetToolMode(ToolModeFull); err != nil {
			t.Fatalf("SetToolMode(full) error = %v", err)
		}
		directErr, directText := callResult(t, server, tc.name, tc.args)
		if err := server.SetToolMode(ToolModeGateway); err != nil {
			t.Fatalf("SetToolMode(gateway) error = %v", err)
		}
		gatewayErr, gatewayText := callResult(t, server, "tool_call",
			map[string]any{"name": tc.name, "arguments": tc.args})

		if directErr != gatewayErr {
			t.Errorf("%s: isError direct = %v, gateway = %v", label, directErr, gatewayErr)
			continue
		}
		if directText != gatewayText {
			t.Errorf("%s: payload differs\ndirect : %s\ngateway: %s", label, directText, gatewayText)
		}
	}
}

// Direct/gateway parity is only meaningful if the tool is stable against
// itself: two identical direct calls that disagree would make the parity
// assertion above a coin flip rather than a statement about the gateway. This
// is the assumption the P16 waiver was hiding.
func TestGraphAnalyticsRepeatedDirectCallsAreIdentical(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeFull)
	for _, analysis := range []string{"pagerank", "coupling", "cycles"} {
		args := map[string]any{"analysis": analysis, "limit": 5}
		wantErr, want := callResult(t, server, "graph_analytics", args)
		if wantErr {
			t.Fatalf("graph_analytics %s returned an error: %s", analysis, want)
		}
		for i := range 20 {
			gotErr, got := callResult(t, server, "graph_analytics", args)
			if gotErr || got != want {
				t.Fatalf("graph_analytics %s call %d differs\nfirst: %s\nnow  : %s", analysis, i+2, want, got)
			}
		}
	}
}

// TestToolCallDoesNotDoubleEncode asserts the shape of what comes back, not just
// that it matches: a compact document must arrive as a compact document, and a
// JSON envelope must not be a JSON string holding JSON.
func TestToolCallDoesNotDoubleEncode(t *testing.T) {
	gateway := newGatewayTestServer(t, ToolModeGateway)

	isError, text := callResult(t, gateway, "tool_call", map[string]any{
		"name":      "search_symbols",
		"arguments": map[string]any{"query": "Helper", "format": "compact"},
	})
	if isError {
		t.Fatalf("compact tool_call failed: %s", text)
	}
	if !strings.HasPrefix(text, "@codegraph.compact/v1") {
		t.Fatalf("compact payload through the gateway = %q, want a compact document", text[:min(60, len(text))])
	}

	isError, text = callResult(t, gateway, "tool_call", map[string]any{
		"name":      "graph_stats",
		"arguments": map[string]any{},
	})
	if isError {
		t.Fatalf("graph_stats tool_call failed: %s", text)
	}
	var envelope map[string]any
	if err := json.Unmarshal([]byte(text), &envelope); err != nil {
		t.Fatalf("gateway result is not JSON: %v", err)
	}
	if _, wrapped := envelope["data"].(string); wrapped {
		t.Fatalf("gateway wrapped the result payload in a JSON string: %s", text)
	}
	if envelope["ok"] != true {
		t.Fatalf("gateway envelope ok = %v, want true", envelope["ok"])
	}
}

// TestContextForTaskThroughTheGatewayIsNotRebudgeted checks that the P14 budget
// contract survives both the direct core call and a cursor continuation. The
// gateway must not trim, re-rank, or re-budget anything.
func TestContextForTaskThroughTheGatewayIsNotRebudgeted(t *testing.T) {
	gateway := newGatewayTestServer(t, ToolModeGateway)
	isError, text := callResult(t, gateway, "context_for_task",
		map[string]any{"task": "change Helper", "max_tokens": 600})
	if isError {
		t.Fatalf("context_for_task failed: %s", text)
	}
	var envelope struct {
		Data struct {
			EstimatedTokens int    `json:"estimated_tokens"`
			MaxTokens       int    `json:"max_tokens"`
			HasMore         bool   `json:"has_more"`
			NextCursor      string `json:"next_cursor"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(text), &envelope); err != nil {
		t.Fatalf("context_for_task result is not JSON: %v", err)
	}
	if envelope.Data.MaxTokens != 600 {
		t.Errorf("max_tokens = %d, want 600", envelope.Data.MaxTokens)
	}
	if envelope.Data.EstimatedTokens > envelope.Data.MaxTokens {
		t.Errorf("estimated_tokens %d exceeds max_tokens %d",
			envelope.Data.EstimatedTokens, envelope.Data.MaxTokens)
	}
	if envelope.Data.HasMore && envelope.Data.NextCursor == "" {
		t.Error("has_more is true but next_cursor is empty")
	}
	if envelope.Data.NextCursor != "" {
		// A continuation through tool_call must reach the same paging machinery.
		isError, more := callResult(t, gateway, "tool_call", map[string]any{
			"name": "context_for_task",
			"arguments": map[string]any{
				"task": "change Helper", "max_tokens": 600, "cursor": envelope.Data.NextCursor,
			},
		})
		if isError {
			t.Fatalf("cursor continuation through tool_call failed: %s", more)
		}
	}
	// A stale cursor is still the target tool's error, not a gateway error.
	isError, stale := callResult(t, gateway, "tool_call", map[string]any{
		"name":      "context_for_task",
		"arguments": map[string]any{"task": "change Helper", "cursor": "not-a-cursor"},
	})
	if !isError {
		t.Errorf("a malformed cursor was accepted: %s", stale)
	}
	if !strings.Contains(stale, "cursor") {
		t.Errorf("malformed-cursor error = %s, want the target tool's wording", stale)
	}
}

// TestGatewayRejectsUnsafeToolCalls covers the gateway-specific amplification
// risks: an unknown target, a meta tool calling a meta tool, and an `arguments`
// value that is not an object.
func TestGatewayRejectsUnsafeToolCalls(t *testing.T) {
	gateway := newGatewayTestServer(t, ToolModeGateway)
	for _, tc := range []struct {
		label    string
		args     any
		wantText string
	}{
		{"unknown tool", map[string]any{"name": "no_such_tool", "arguments": map[string]any{}}, `unknown tool "no_such_tool"`},
		{"recursive tool_call", map[string]any{"name": "tool_call", "arguments": map[string]any{"name": "audit"}}, `unknown tool "tool_call"`},
		{"recursive tool_search", map[string]any{"name": "tool_search", "arguments": map[string]any{"query": "x"}}, `unknown tool "tool_search"`},
		{"missing name", map[string]any{"arguments": map[string]any{}}, `missing required argument "name"`},
		{"blank name", map[string]any{"name": "   "}, `required argument "name" must not be empty`},
		{"scalar arguments", map[string]any{"name": "audit", "arguments": 5}, `argument "arguments" must be an object`},
		{"string arguments", map[string]any{"name": "audit", "arguments": "{}"}, `argument "arguments" must be an object`},
		{"array arguments", map[string]any{"name": "audit", "arguments": []any{1, 2}}, `argument "arguments" must be an object`},
		{"unknown gateway argument", map[string]any{"name": "audit", "nope": 1}, `unknown argument "nope"`},
	} {
		if got := callError(t, gateway, "tool_call", tc.args); !strings.Contains(got, tc.wantText) {
			t.Errorf("%s: error = %q, want it to mention %q", tc.label, got, tc.wantText)
		}
	}

	// tool_search validates through the same canonical path.
	for _, tc := range []struct {
		label    string
		args     any
		wantText string
	}{
		{"missing query", map[string]any{}, `missing required argument "query"`},
		{"unknown argument", map[string]any{"query": "x", "bogus": 1}, `unknown argument "bogus"`},
		{"wrong limit type", map[string]any{"query": "x", "limit": "five"}, `argument "limit" must be an integer`},
	} {
		if got := callError(t, gateway, "tool_search", tc.args); !strings.Contains(got, tc.wantText) {
			t.Errorf("tool_search %s: error = %q, want it to mention %q", tc.label, got, tc.wantText)
		}
	}
}

// TestMetaToolsDoNotExistInFullMode is the other half of full-mode parity: the
// gateway tools are not merely unlisted there, they are not callable either.
func TestMetaToolsDoNotExistInFullMode(t *testing.T) {
	full := newGatewayTestServer(t, ToolModeFull)
	for _, name := range []string{"tool_search", "tool_call"} {
		got := callError(t, full, name, map[string]any{"query": "audit", "name": "audit"})
		if want := `unknown tool "` + name + `"`; !strings.Contains(got, want) {
			t.Errorf("%s in full mode = %q, want %q", name, got, want)
		}
	}
	// Nothing on the tool surface can change the mode.
	if got := full.ToolMode(); got != ToolModeFull {
		t.Fatalf("ToolMode() = %q after meta calls, want %q", got, ToolModeFull)
	}
}

// TestGatewayKeepsEveryCapabilityReachable is the no-lost-capability check: every
// tool full mode advertises is reachable from a gateway server, either directly or
// through tool_call, and tool_search can name it.
func TestGatewayKeepsEveryCapabilityReachable(t *testing.T) {
	gateway := newGatewayTestServer(t, ToolModeGateway)
	listed := map[string]bool{}
	for _, name := range toolNames(listedTools(t, gateway)) {
		listed[name] = true
	}
	for _, name := range preGatewayToolNames {
		if listed[name] {
			continue
		}
		// tool_search names it, with its canonical schema on request.
		data := searchTools(t, gateway, map[string]any{"query": name, "include_schema": true})
		found := searchNames(t, data)
		if len(found) != 1 || found[0] != name {
			t.Errorf("tool_search(%q) = %v, want exactly [%q]", name, found, name)
		}
		// And tool_call reaches it: an empty argument set is answered by the target's
		// own validator, never by "unknown tool".
		_, text := callResult(t, gateway, "tool_call", map[string]any{
			"name": name, "arguments": map[string]any{},
		})
		if strings.Contains(text, "unknown tool") {
			t.Errorf("tool_call(%q) is not reachable: %s", name, text)
		}
	}
}

// TestRegistryIsTheOnlySourceOfTruth guards the invariant the P16 refactor bought:
// one row per tool feeding the definitions, the validator, and dispatch.
func TestRegistryIsTheOnlySourceOfTruth(t *testing.T) {
	if len(toolByName) != len(toolRegistry) {
		t.Fatalf("toolByName has %d entries, registry has %d", len(toolByName), len(toolRegistry))
	}
	// `unlisted` is a tool the gateway surface reaches through tool_call rather
	// than advertising; `diagnostic` is a tool no surface advertises at all.
	var core, meta, unlisted, diagnostic []string
	for _, desc := range toolRegistry {
		switch {
		case desc.gatewayMeta:
			meta = append(meta, desc.name)
		case desc.hidden:
			diagnostic = append(diagnostic, desc.name)
		case desc.gatewayCore:
			core = append(core, desc.name)
		default:
			unlisted = append(unlisted, desc.name)
		}
		spec, ok := toolArgumentSpecs[desc.name]
		if !ok {
			t.Errorf("tool %q has no argument spec", desc.name)
			continue
		}
		if len(spec.properties) != len(desc.properties) {
			t.Errorf("tool %q: %d spec properties, %d registry properties", desc.name, len(spec.properties), len(desc.properties))
		}
		for _, prop := range desc.properties {
			if spec.properties[prop] != argType(prop) {
				t.Errorf("tool %q property %q: spec type %q, argType %q", desc.name, prop, spec.properties[prop], argType(prop))
			}
		}
		if !reflect.DeepEqual(spec.required, desc.required) {
			t.Errorf("tool %q required: spec %v, registry %v", desc.name, spec.required, desc.required)
		}
		// Advertised schemas and the validator must agree on the accepted set.
		schema := desc.definition()["inputSchema"].(map[string]any)
		props := schema["properties"].(map[string]any)
		if len(props) != len(desc.properties) {
			t.Errorf("tool %q advertises %d properties, registry has %d", desc.name, len(props), len(desc.properties))
		}
		for _, prop := range desc.properties {
			if _, ok := props[prop]; !ok {
				t.Errorf("tool %q property %q is not advertised", desc.name, prop)
			}
		}
		if _, ok := dispatchableTool(desc.name); ok == desc.gatewayMeta {
			t.Errorf("tool %q: dispatchable = %v, gatewayMeta = %v", desc.name, ok, desc.gatewayMeta)
		}
	}
	if len(core) != 4 {
		t.Errorf("gateway core tools = %v, want 4", core)
	}
	if !reflect.DeepEqual(meta, []string{"tool_search", "tool_call"}) {
		t.Errorf("gateway meta tools = %v", meta)
	}
	if len(unlisted) != len(preGatewayToolNames)-len(core) {
		t.Errorf("gateway-unlisted tools = %d, want %d", len(unlisted), len(preGatewayToolNames)-len(core))
	}
	// Diagnostics are advertised nowhere, which is the property that keeps both
	// tools/list payloads the size P16 left them.
	if !reflect.DeepEqual(diagnostic, []string{"usage_stats"}) {
		t.Errorf("hidden diagnostics = %v, want [usage_stats]", diagnostic)
	}

	// Every registry name has a category, so tool_search can rank it.
	for _, desc := range toolRegistry {
		if strings.TrimSpace(desc.category) == "" {
			t.Errorf("tool %q has no category", desc.name)
		}
	}

	// The full definitions are exactly the non-meta registry rows, in order.
	var wantFull []string
	for _, desc := range toolRegistry {
		if !desc.gatewayMeta && !desc.hidden {
			wantFull = append(wantFull, desc.name)
		}
	}
	if got := toolNames(buildToolDefinitions()); !reflect.DeepEqual(got, wantFull) {
		t.Errorf("buildToolDefinitions() = %v, want %v", got, wantFull)
	}
	sorted := append([]string(nil), preGatewayToolNames...)
	sort.Strings(sorted)
	gotSorted := append([]string(nil), wantFull...)
	sort.Strings(gotSorted)
	if !reflect.DeepEqual(gotSorted, sorted) {
		t.Errorf("registry tool set drifted from the pre-gateway surface:\n%v\n%v", gotSorted, sorted)
	}
}

// TestMutatingToolsAreLabelled keeps the write set honest: tool_search reports it,
// and an agent reaching for an unfamiliar tool reads it there.
func TestMutatingToolsAreLabelled(t *testing.T) {
	want := map[string]bool{
		"index_repo": true, "update_graph": true, "cross_language_links": true,
		"session_log": true, "agentic_query": true,
	}
	for _, desc := range toolRegistry {
		if desc.gatewayMeta {
			if desc.mutates {
				t.Errorf("meta tool %q is marked mutating", desc.name)
			}
			continue
		}
		if desc.mutates != want[desc.name] {
			t.Errorf("tool %q mutates = %v, want %v", desc.name, desc.mutates, want[desc.name])
		}
	}
}

// TestRepoIsolationIsUnchangedThroughTheGateway checks that going through
// tool_call does not widen what a tool can reach: the repo-scoped tools answer for
// the served repository in both modes, and the two tools that accept a repo
// argument are the same two they were.
func TestRepoIsolationIsUnchangedThroughTheGateway(t *testing.T) {
	repoArgTools := map[string]bool{}
	for _, desc := range toolRegistry {
		for _, prop := range desc.properties {
			if prop == "repo_root" || prop == "repo_path" {
				repoArgTools[desc.name] = true
			}
		}
	}
	if !reflect.DeepEqual(repoArgTools, map[string]bool{"index_repo": true, "update_graph": true}) {
		t.Fatalf("tools accepting a repository argument = %v, want only index_repo and update_graph", repoArgTools)
	}

	gateway := newGatewayTestServer(t, ToolModeGateway)
	isError, text := callResult(t, gateway, "tool_call", map[string]any{
		"name": "list_files", "arguments": map[string]any{"path_filter": "../"},
	})
	if isError {
		t.Fatalf("list_files through the gateway failed: %s", text)
	}
	var envelope struct {
		Data struct {
			Files []map[string]any `json:"files"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(text), &envelope); err != nil {
		t.Fatalf("list_files result is not JSON: %v", err)
	}
	if len(envelope.Data.Files) != 0 {
		t.Errorf("a traversal path filter through the gateway returned %d files, want 0", len(envelope.Data.Files))
	}
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	payload, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	return string(payload)
}

// BenchmarkGatewayDispatchOverhead measures what the indirection costs: the same
// call answered directly and through tool_call.
func BenchmarkGatewayDispatchOverhead(b *testing.B) {
	ctx := context.Background()
	server := setupMCPBenchServer(b, ctx)
	direct, err := json.Marshal(map[string]any{"query": "BenchFn", "limit": 20})
	if err != nil {
		b.Fatalf("json.Marshal() error = %v", err)
	}
	viaGateway, err := json.Marshal(map[string]any{
		"name": "search_symbols", "arguments": map[string]any{"query": "BenchFn", "limit": 20},
	})
	if err != nil {
		b.Fatalf("json.Marshal() error = %v", err)
	}
	if err := server.SetToolMode(ToolModeGateway); err != nil {
		b.Fatalf("SetToolMode() error = %v", err)
	}

	b.Run("direct", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := server.callToolText(ctx, "search_symbols", direct); err != nil {
				b.Fatalf("callToolText() error = %v", err)
			}
		}
	})
	b.Run("tool_call", func(b *testing.B) {
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if _, err := server.callToolText(ctx, "tool_call", viaGateway); err != nil {
				b.Fatalf("callToolText() error = %v", err)
			}
		}
	})
}
