package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/githistory/gittest"
	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/mcp"
	"github.com/isink17/codegraph/internal/query"
	"github.com/isink17/codegraph/internal/store"
)

func TestDiffCLIAndMCPReturnSameDocument(t *testing.T) {
	r := gittest.Init(t)
	r.Write("main.go", "package main\nfunc A() {}\n")
	base := r.Commit("", "base")
	r.Write("main.go", "package main\nfunc A() { B() }\nfunc B() {}\n")
	head := r.Commit("", "head")

	var cliOut, cliErr bytes.Buffer
	if err := Run(context.Background(), []string{"diff", base, head, "--repo-root", r.Dir}, &cliOut, &cliErr); err != nil {
		t.Fatalf("CLI diff: %v (%s)", err, cliErr.String())
	}

	dbPath := t.TempDir() + "/graph.sqlite"
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	idx := indexer.New(st, newDefaultRegistry(), nil)
	server := mcp.NewServer(r.Dir, 1, st, idx, query.New(st, nil), io.Discard)
	activeDB, err := os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	activeHash := sha256.Sum256(activeDB)
	arguments, _ := json.Marshal(map[string]any{"base": base, "head": head})
	request, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "diff", "arguments": json.RawMessage(arguments)}})
	var mcpOut bytes.Buffer
	if err := server.Serve(context.Background(), strings.NewReader(string(request)+"\n"), &mcpOut, io.Discard); err != nil {
		t.Fatalf("MCP diff: %v", err)
	}
	activeDB, err = os.ReadFile(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if got := sha256.Sum256(activeDB); got != activeHash {
		t.Fatal("diff tool modified the active repository database")
	}
	var response struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal(mcpOut.Bytes(), &response); err != nil {
		t.Fatalf("decode MCP response: %v\n%s", err, mcpOut.String())
	}
	if len(response.Result.Content) != 1 {
		t.Fatalf("MCP content = %+v", response.Result.Content)
	}
	var viaCLI any
	if err := json.Unmarshal(cliOut.Bytes(), &viaCLI); err != nil {
		t.Fatal(err)
	}
	var viaMCP struct {
		OK   bool            `json:"ok"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(response.Result.Content[0].Text), &viaMCP); err != nil {
		t.Fatalf("decode MCP diff document: %v", err)
	}
	if !viaMCP.OK {
		t.Fatalf("MCP diff payload not ok: %s", response.Result.Content[0].Text)
	}
	cliJSON, _ := json.Marshal(viaCLI)
	var mcpDocument any
	if err := json.Unmarshal(viaMCP.Data, &mcpDocument); err != nil {
		t.Fatal(err)
	}
	mcpJSON, _ := json.Marshal(mcpDocument)
	if !bytes.Equal(cliJSON, mcpJSON) {
		t.Fatalf("CLI/MCP documents differ:\nCLI %s\nMCP %s", cliJSON, mcpJSON)
	}
}
