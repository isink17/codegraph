package mcp

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"path/filepath"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/editsafety"
	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/query"
	"github.com/isink17/codegraph/internal/store"
)

// toolData calls name and returns its `data` re-encoded, or fails on a tool
// error.
func toolData(t *testing.T, server *Server, name string, args any) string {
	t.Helper()
	isError, text := callResult(t, server, name, args)
	if isError {
		t.Fatalf("%s(%v) failed: %s", name, args, text)
	}
	var envelope struct {
		OK   bool            `json:"ok"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(text), &envelope); err != nil || !envelope.OK {
		t.Fatalf("%s envelope: %v %s", name, err, text)
	}
	var v any
	if err := json.Unmarshal(envelope.Data, &v); err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(v)
	return string(out)
}

// The tool returns exactly the assessment the CLI encodes (both call
// editsafety.Assess), in full mode and through the gateway.
func TestSafeToEditToolParity(t *testing.T) {
	full := newGatewayTestServer(t, ToolModeFull)
	args := map[string]any{"symbol": "main.Helper"}
	got := toolData(t, full, "safe_to_edit", args)

	res, err := editsafety.Assess(context.Background(), full.store, full.repoID, editsafety.Options{RepoRoot: full.repoRoot, Symbol: "main.Helper"})
	if err != nil {
		t.Fatal(err)
	}
	direct, _ := json.Marshal(res)
	var v any
	_ = json.Unmarshal(direct, &v)
	want, _ := json.Marshal(v)
	if got != string(want) {
		t.Fatalf("tool data differs from Assess:\n%s\n%s", got, want)
	}
	if res.Schema != editsafety.SchemaID || res.Evidence.DependentsTotal != 2 || res.Verdict != nil && res.Status != editsafety.StatusAssessed {
		t.Fatalf("unexpected result %+v", res)
	}
	if again := toolData(t, full, "safe_to_edit", args); again != got {
		t.Fatalf("repeated call differs:\n%s\n%s", got, again)
	}

	gw := newGatewayTestServer(t, ToolModeGateway)
	if names := searchNames(t, searchTools(t, gw, map[string]any{"query": "safe to edit"})); !strings.Contains(strings.Join(names, ","), "safe_to_edit") {
		t.Fatalf("tool_search did not find safe_to_edit: %v", names)
	}
	viaGateway := toolData(t, gw, "tool_call", map[string]any{"name": "safe_to_edit", "arguments": args})
	if viaGateway != toolData(t, gw, "safe_to_edit", args) {
		t.Fatal("gateway tool_call differs from the direct call")
	}
}

func TestSafeToEditToolErrors(t *testing.T) {
	server := newGatewayTestServer(t, ToolModeFull)
	for _, args := range []string{`{}`, `{"symbol":"main.Helper","file":"main.go"}`, `{"symbol":"Helper"}`, `{"symbol":"main.Nope"}`,
		`{"file":"."}`, `{"file":"nope.go"}`, `{"symbol_id":-1}`, `{"symbol":3}`, `{"directory":"x"}`} {
		var raw any
		_ = json.Unmarshal([]byte(args), &raw)
		if isError, text := callResult(t, server, "safe_to_edit", raw); !isError {
			t.Errorf("safe_to_edit %s succeeded: %s", args, text)
		}
	}
}

// The server assesses on its read-write handle; read-only rests on the
// assessment issuing SELECT only, which data_version on an independent
// connection proves.
func TestSafeToEditToolDoesNotWrite(t *testing.T) {
	ctx := context.Background()
	repoRoot := t.TempDir()
	writeRepoFile(t, repoRoot, "go.mod", "module example.com/m\n\ngo 1.22\n")
	writeRepoFile(t, repoRoot, "a.go", "package m\n\nfunc A() { B() }\n\nfunc B() {}\n")
	dbPath := filepath.Join(t.TempDir(), "graph.sqlite")
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	idx := indexer.New(st, parser.NewRegistry(goparser.New()), nil)
	repo, err := st.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := idx.Index(ctx, indexer.Options{RepoRoot: repoRoot}); err != nil {
		t.Fatal(err)
	}
	server := NewServer(repoRoot, repo.ID, st, idx, query.New(st, nil), io.Discard)
	dsn, err := store.BuildSQLiteDSN(dbPath, store.OpenOptions{}, false, false)
	if err != nil {
		t.Fatal(err)
	}
	observer, err := sql.Open(store.SQLiteDriverName(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	conn, err := observer.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	probe := func() (dv int64) {
		if err := conn.QueryRowContext(ctx, `PRAGMA data_version`).Scan(&dv); err != nil {
			t.Fatal(err)
		}
		return
	}
	before := probe()
	for _, args := range []string{`{"symbol":"m.B"}`, `{"file":"a.go"}`, `{"symbol":"m.Nope"}`} {
		_, _ = server.handleSafeToEdit(ctx, json.RawMessage(args))
	}
	if probe() != before {
		t.Fatal("safe_to_edit wrote")
	}
	if err := st.QueueDirtyFiles(ctx, repo.ID, []string{"a.go"}, "modified"); err != nil {
		t.Fatal(err)
	}
	if probe() == before {
		t.Fatal("data_version did not move on a real write; the probe proves nothing")
	}
	res, err := server.handleSafeToEdit(ctx, json.RawMessage(`{"symbol":"m.B"}`))
	if err != nil {
		t.Fatal(err)
	}
	if r := res["data"].(editsafety.Result); r.Status != editsafety.StatusUnknown || r.Verdict != nil || r.Reason != editsafety.ReasonKnownStale {
		t.Fatalf("queued change: %+v", r)
	}
}
