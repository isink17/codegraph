package mcp

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"path/filepath"
	"testing"

	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/query"
	"github.com/isink17/codegraph/internal/store"
)

// TestExplainEdgeToolDoesNotWrite brackets explain_edge calls -- a resolved
// edge, a refused one and an unknown one, which between them load every fact
// edgeRefusalReasons reads -- with PRAGMA data_version and total_changes() on
// an independent connection.
func TestExplainEdgeToolDoesNotWrite(t *testing.T) {
	ctx := context.Background()
	repoRoot := t.TempDir()
	writeRepoFile(t, repoRoot, "go.mod", "module example.com/p\n\ngo 1.22\n")
	writeRepoFile(t, repoRoot, "pkg/a.go", "package pkg\n\nfunc Open() {}\n")
	writeRepoFile(t, repoRoot, "cmd/main.go", "package main\n\nimport \"example.com/p/pkg\"\n\ntype T struct{}\n\nfunc main() {\n"+
		"\tpkg.Open()\n\tvar store T\n\tstore.Missing()\n\tfmt.Println()\n\tOpen()\n\tTwo.run()\n}\n")

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
	probe := func() (dv, changes int64) {
		if err := conn.QueryRowContext(ctx, `PRAGMA data_version`).Scan(&dv); err != nil {
			t.Fatal(err)
		}
		if err := conn.QueryRowContext(ctx, `SELECT total_changes()`).Scan(&changes); err != nil {
			t.Fatal(err)
		}
		return
	}

	input := bytes.NewBuffer(nil)
	for line := 8; line <= 13; line++ {
		writeFrameToBuffer(t, input, map[string]any{"jsonrpc": "2.0", "id": line, "method": "tools/call",
			"params": map[string]any{"name": "explain_edge", "arguments": map[string]any{"file": "cmd/main.go", "line": line}}})
	}
	beforeDV, beforeChanges := probe()
	var output bytes.Buffer
	if err := server.Serve(ctx, input, &output, io.Discard); err != nil {
		t.Fatal(err)
	}
	afterDV, afterChanges := probe()
	explanations := map[string]bool{}
	for _, resp := range readAllFrames(t, &output) {
		result := resp["result"].(map[string]any)
		if result["isError"] == true {
			continue // line 9 declares a variable and holds no edge
		}
		var payload struct {
			Data store.ExplainResult `json:"data"`
		}
		if err := json.Unmarshal([]byte(result["content"].([]any)[0].(map[string]any)["text"].(string)), &payload); err != nil {
			t.Fatal(err)
		}
		for _, e := range payload.Data.Edges {
			explanations[e.Explanation] = true
		}
	}
	for _, want := range []string{store.ExplainPersisted, store.ExplainRefused, store.ExplainUnknown} {
		if !explanations[want] {
			t.Fatalf("no %s explanation among the calls: %v", want, explanations)
		}
	}
	if beforeDV != afterDV || beforeChanges != afterChanges {
		t.Fatalf("data_version %d -> %d, total_changes %d -> %d: explain_edge wrote", beforeDV, afterDV, beforeChanges, afterChanges)
	}
	if err := st.QueueDirtyFiles(ctx, repo.ID, []string{"cmd/main.go"}, "modified"); err != nil {
		t.Fatal(err)
	}
	if dv, _ := probe(); dv == afterDV {
		t.Fatal("data_version did not move on a real write; the probe proves nothing")
	}
}

// TestExplainEdgeToolArguments: argument types are checked by the registry;
// selector shape and page bounds by the store.
func TestExplainEdgeToolArguments(t *testing.T) {
	for _, args := range []string{`{"edge_id":"1"}`, `{"line":"3"}`, `{"config":"x"}`} {
		if err := validateToolArguments("explain_edge", json.RawMessage(args)); err == nil {
			t.Errorf("validateToolArguments(%s) accepted", args)
		}
	}
	for _, sel := range []store.EdgeSelector{
		{}, {EdgeID: -1}, {File: "a.go"}, {Line: 3}, {EdgeID: 1, File: "a.go", Line: 1},
		{EdgeID: 1, Limit: 501}, {EdgeID: 1, Offset: -1}, {EdgeID: 1, Name: "x"},
	} {
		if err := sel.Validate(); err == nil {
			t.Errorf("Validate(%+v) accepted", sel)
		}
	}
	if err := validateToolArguments("explain_edge", json.RawMessage(`{"file":"a.go","line":3,"name":"f","limit":5,"offset":2}`)); err != nil {
		t.Errorf("valid arguments rejected: %v", err)
	}
}
