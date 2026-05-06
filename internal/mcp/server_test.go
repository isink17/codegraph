package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/parser"
	goparser "github.com/isink17/codegraph/internal/parser/golang"
	"github.com/isink17/codegraph/internal/query"
	"github.com/isink17/codegraph/internal/store"
)

func TestServeInitializeAndGraphStats(t *testing.T) {
	ctx := context.Background()
	repoRoot := t.TempDir()
	writeRepoFile(t, repoRoot, "main.go", `package main

func main() {}
`)

	s := openTestStore(t)
	defer s.Close()

	idx := indexer.New(s, parser.NewRegistry(goparser.New()), nil)
	repo, err := s.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	if _, err := idx.Index(ctx, indexer.Options{RepoRoot: repoRoot}); err != nil {
		t.Fatalf("Index() error = %v", err)
	}

	server := NewServer(repoRoot, repoRoot, repo.ID, s, idx, query.New(s, nil), io.Discard)
	input := bytes.NewBuffer(nil)
	writeFrameToBuffer(t, input, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params":  map[string]any{},
	})
	writeFrameToBuffer(t, input, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "graph_stats",
			"arguments": map[string]any{},
		},
	})

	var output bytes.Buffer
	if err := server.Serve(ctx, input, &output, io.Discard); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}

	responses := readAllFrames(t, &output)
	if len(responses) != 2 {
		t.Fatalf("response count = %d, want 2", len(responses))
	}

	initResult := responses[0]["result"].(map[string]any)
	if initResult["protocolVersion"] != "2024-11-05" {
		t.Fatalf("protocolVersion = %v, want 2024-11-05", initResult["protocolVersion"])
	}

	toolResult := responses[1]["result"].(map[string]any)
	if got := toolResult["isError"]; got != false {
		t.Fatalf("isError = %v, want false", got)
	}
	contentArr := toolResult["content"].([]any)
	if len(contentArr) == 0 {
		t.Fatal("expected content array to be non-empty")
	}
	textEntry := contentArr[0].(map[string]any)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(textEntry["text"].(string)), &parsed); err != nil {
		t.Fatalf("json.Unmarshal(content text) error = %v", err)
	}
	data := parsed["data"].(map[string]any)
	if got := int(data["files"].(float64)); got != 1 {
		t.Fatalf("files = %d, want 1", got)
	}
}

func TestSupportedLanguagesTool(t *testing.T) {
	ctx := context.Background()
	repoRoot := t.TempDir()
	s := openTestStore(t)
	defer s.Close()

	idx := indexer.New(s, parser.NewRegistry(goparser.New()), nil)
	repo, err := s.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	server := NewServer(repoRoot, repoRoot, repo.ID, s, idx, query.New(s, nil), io.Discard)

	input := bytes.NewBuffer(nil)
	writeFrameToBuffer(t, input, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "supported_languages",
			"arguments": map[string]any{},
		},
	})
	var output bytes.Buffer
	if err := server.Serve(ctx, input, &output, io.Discard); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
	responses := readAllFrames(t, &output)
	if len(responses) != 1 {
		t.Fatalf("response count = %d, want 1", len(responses))
	}
	result := responses[0]["result"].(map[string]any)
	if got := result["isError"]; got != false {
		t.Fatalf("isError = %v, want false", got)
	}
	contentArr := result["content"].([]any)
	if len(contentArr) == 0 {
		t.Fatal("expected content array to be non-empty")
	}
	textEntry := contentArr[0].(map[string]any)
	var parsed map[string]any
	if err := json.Unmarshal([]byte(textEntry["text"].(string)), &parsed); err != nil {
		t.Fatalf("json.Unmarshal(content text) error = %v", err)
	}
	langData := parsed["data"].(map[string]any)
	languages := langData["languages"].([]any)
	if len(languages) == 0 {
		t.Fatalf("languages = %v, want non-empty", languages)
	}
}

func TestServeMalformedJSONReturnsParseErrorAndContinues(t *testing.T) {
	ctx := context.Background()
	repoRoot := t.TempDir()
	s := openTestStore(t)
	defer s.Close()
	idx := indexer.New(s, parser.NewRegistry(goparser.New()), nil)
	repo, err := s.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	server := NewServer(repoRoot, repoRoot, repo.ID, s, idx, query.New(s, nil), io.Discard)

	var input bytes.Buffer
	input.WriteString("not-json\n")
	input.WriteString("\n")
	writeFrameToBuffer(t, &input, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params":  map[string]any{},
	})
	var output bytes.Buffer
	if err := server.Serve(ctx, &input, &output, io.Discard); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
	responses := readAllFrames(t, &output)
	if len(responses) != 2 {
		t.Fatalf("response count = %d, want 2", len(responses))
	}
	if _, ok := responses[0]["error"]; !ok {
		t.Fatalf("first response should be parse error, got: %v", responses[0])
	}
	if _, ok := responses[1]["result"]; !ok {
		t.Fatalf("second response should be initialize result, got: %v", responses[1])
	}
}

func TestToolValidationRejectsMissingQuery(t *testing.T) {
	ctx := context.Background()
	repoRoot := t.TempDir()
	s := openTestStore(t)
	defer s.Close()
	idx := indexer.New(s, parser.NewRegistry(goparser.New()), nil)
	repo, err := s.UpsertRepo(ctx, repoRoot)
	if err != nil {
		t.Fatalf("UpsertRepo() error = %v", err)
	}
	server := NewServer(repoRoot, repoRoot, repo.ID, s, idx, query.New(s, nil), io.Discard)

	var input bytes.Buffer
	writeFrameToBuffer(t, &input, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "find_symbol",
			"arguments": map[string]any{},
		},
	})
	var output bytes.Buffer
	if err := server.Serve(ctx, &input, &output, io.Discard); err != nil {
		t.Fatalf("Serve() error = %v", err)
	}
	responses := readAllFrames(t, &output)
	if len(responses) != 1 {
		t.Fatalf("response count = %d, want 1", len(responses))
	}
	result := responses[0]["result"].(map[string]any)
	if result["isError"] != true {
		t.Fatalf("expected tool error response, got %v", result)
	}
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "graph.sqlite"))
	if err != nil {
		t.Fatalf("store.Open() error = %v", err)
	}
	return s
}

func writeRepoFile(t *testing.T, repoRoot, relativePath, content string) {
	t.Helper()
	path := filepath.Join(repoRoot, relativePath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("MkdirAll(%q) error = %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
}

func writeFrameToBuffer(t *testing.T, buf *bytes.Buffer, payload any) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	buf.Write(body)
	buf.WriteByte('\n')
}

func readAllFrames(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	reader := bufio.NewReader(buf)
	var responses []map[string]any
	for {
		body, err := readFrame(reader)
		if err == io.EOF {
			return responses
		}
		if err != nil {
			t.Fatalf("readFrame() error = %v", err)
		}
		if len(body) == 0 {
			continue
		}
		var decoded map[string]any
		if err := json.Unmarshal(body, &decoded); err != nil {
			t.Fatalf("json.Unmarshal() error = %v", err)
		}
		responses = append(responses, decoded)
	}
}
