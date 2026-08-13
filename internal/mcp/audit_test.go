package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/isink17/codegraph/internal/graphaudit"
	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/query"
	"github.com/isink17/codegraph/internal/store"
)

// auditServer returns a server over a small indexed repository.
func auditServer(t *testing.T) (*Server, *store.Store, int64) {
	t.Helper()
	ctx := context.Background()
	repoRoot := t.TempDir()
	writeRepoFile(t, repoRoot, "main.go", `package main

func helper() {}

func main() { helper() }
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
	return NewServer(repoRoot, repoRoot, repo.ID, s, idx, query.New(s, nil), io.Discard), s, repo.ID
}

// callAudit invokes the tool over the wire and returns the decoded payload.
func callAudit(t *testing.T, server *Server, args map[string]any) map[string]any {
	t.Helper()
	input := bytes.NewBuffer(nil)
	writeFrameToBuffer(t, input, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params":  map[string]any{"name": "audit", "arguments": args},
	})
	var output bytes.Buffer
	if err := server.Serve(context.Background(), input, &output, io.Discard); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
	responses := readAllFrames(t, &output)
	if len(responses) != 1 {
		t.Fatalf("response count = %d, want 1", len(responses))
	}
	result, ok := responses[0]["result"].(map[string]any)
	if !ok {
		t.Fatalf("response has no result: %+v", responses[0])
	}
	if isErr, _ := result["isError"].(bool); isErr {
		t.Fatalf("audit tool call failed: %+v", result)
	}
	content := result["content"].([]any)
	text := content[0].(map[string]any)["text"].(string)

	var payload map[string]any
	if err := json.Unmarshal([]byte(text), &payload); err != nil {
		t.Fatalf("tool payload is not JSON: %v\n%s", err, text)
	}
	return payload
}

// TestAuditToolIsRegistered checks the three places a tool must appear stay in
// agreement: the advertised definitions, the argument specs, and the dispatch
// switch. A tool listed but not dispatchable is the drift this guards against.
func TestAuditToolIsRegistered(t *testing.T) {
	found := false
	for _, def := range buildToolDefinitions() {
		if def["name"] == "audit" {
			found = true
			schema := def["inputSchema"].(map[string]any)
			props := schema["properties"].(map[string]any)
			examples, ok := props["examples"].(map[string]any)
			if !ok || examples["type"] != "integer" {
				t.Errorf("audit examples property = %+v, want an integer", props["examples"])
			}
			if schema["additionalProperties"] != false {
				t.Error("audit input schema allows additional properties")
			}
		}
	}
	if !found {
		t.Fatal("audit is not in the advertised tool definitions")
	}
	if _, ok := toolArgumentSpecs["audit"]; !ok {
		t.Fatal("audit has no entry in toolArgumentSpecs, so every call would be rejected")
	}
	// Definitions and specs must cover exactly the same tool names.
	for _, def := range buildToolDefinitions() {
		name := def["name"].(string)
		if _, ok := toolArgumentSpecs[name]; !ok {
			t.Errorf("tool %q is advertised but has no argument spec", name)
		}
	}
	for name := range toolArgumentSpecs {
		matched := false
		for _, def := range buildToolDefinitions() {
			if def["name"] == name {
				matched = true
			}
		}
		if !matched {
			t.Errorf("tool %q has an argument spec but is not advertised", name)
		}
	}
}

// TestAuditToolReturnsTheProductionReport is the CLI/MCP parity check: the tool
// must return the same graphaudit.Report the command prints, not a reshaped
// summary of it.
func TestAuditToolReturnsTheProductionReport(t *testing.T) {
	server, s, repoID := auditServer(t)

	payload := callAudit(t, server, map[string]any{})
	if ok, _ := payload["ok"].(bool); !ok {
		t.Fatalf("payload ok = %v, want true: %+v", payload["ok"], payload)
	}
	data, err := json.Marshal(payload["data"])
	if err != nil {
		t.Fatalf("marshal data: %v", err)
	}

	direct, err := graphaudit.Run(context.Background(), s, graphaudit.Options{
		RepoID:       repoID,
		ExampleLimit: graphaudit.DefaultExampleLimit,
		Repository:   graphaudit.Repository{Root: server.repoRoot},
	})
	if err != nil {
		t.Fatalf("graphaudit.Run() error = %v", err)
	}
	// Compare the graph-derived parts; the repository block differs because the
	// CLI also records a database path.
	var viaTool, viaService map[string]any
	if err := json.Unmarshal(data, &viaTool); err != nil {
		t.Fatalf("decode tool data: %v", err)
	}
	directJSON, _ := json.Marshal(direct)
	if err := json.Unmarshal(directJSON, &viaService); err != nil {
		t.Fatalf("decode service report: %v", err)
	}
	// A graph the real indexer produced must audit clean. This is the only
	// assertion in the suite that runs the checks against genuine indexer
	// output, so it is what guards against a predicate false-positiving on
	// normal graphs -- every other fixture is hand-corrupted on purpose.
	if viaService["status"] != string(graphaudit.StatusOK) {
		t.Errorf("a freshly indexed repository audited to status %v, want ok; findings: %v",
			viaService["status"], viaService["findings"])
	}

	for _, field := range []string{"schema", "status", "graph", "findings", "unresolved_classification", "resolution_strategy", "resolution_confidence", "example_limit"} {
		a, _ := json.Marshal(viaTool[field])
		b, _ := json.Marshal(viaService[field])
		if string(a) != string(b) {
			t.Errorf("field %q differs between the MCP tool and the audit service:\ntool:    %s\nservice: %s", field, a, b)
		}
	}
}

// TestAuditToolExamplesArgument covers the one input the tool accepts,
// including the 0-means-counts-only convention it shares with the CLI.
func TestAuditToolExamplesArgument(t *testing.T) {
	server, _, _ := auditServer(t)

	byDefault := callAudit(t, server, map[string]any{})
	data := byDefault["data"].(map[string]any)
	if got := data["example_limit"]; got != float64(graphaudit.DefaultExampleLimit) {
		t.Errorf("default example_limit = %v, want %d", got, graphaudit.DefaultExampleLimit)
	}

	countsOnly := callAudit(t, server, map[string]any{"examples": 0})
	data = countsOnly["data"].(map[string]any)
	if got := data["example_limit"]; got != float64(0) {
		t.Errorf("example_limit with examples=0 = %v, want 0", got)
	}

	capped := callAudit(t, server, map[string]any{"examples": 2})
	data = capped["data"].(map[string]any)
	if got := data["example_limit"]; got != float64(2) {
		t.Errorf("example_limit with examples=2 = %v, want 2", got)
	}
}

// TestAuditToolRejectsUnknownArguments confirms the tool inherits the server's
// argument validation rather than silently ignoring typos.
func TestAuditToolRejectsUnknownArguments(t *testing.T) {
	if err := validateToolArguments("audit", json.RawMessage(`{"exmaples": 5}`)); err == nil {
		t.Error("audit accepted an unknown argument")
	}
	if err := validateToolArguments("audit", json.RawMessage(`{"examples": "five"}`)); err == nil {
		t.Error("audit accepted a string where an integer is required")
	}
	if err := validateToolArguments("audit", json.RawMessage(`{}`)); err != nil {
		t.Errorf("audit rejected an empty argument object: %v", err)
	}
}
