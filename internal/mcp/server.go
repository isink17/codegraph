package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strconv"
	"strings"
	"sync"

	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/query"
	"github.com/isink17/codegraph/internal/store"
	"github.com/isink17/codegraph/internal/version"
)

type Server struct {
	repoRoot string
	repoID   int64
	store    *store.Store
	indexer  *indexer.Indexer
	query    *query.Service
}

func NewServer(repoRoot string, repoID int64, s *store.Store, idx *indexer.Indexer, q *query.Service) *Server {
	return &Server{repoRoot: repoRoot, repoID: repoID, store: s, indexer: idx, query: q}
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcResponse struct {
	JSONRPC string         `json:"jsonrpc"`
	ID      any            `json:"id,omitempty"`
	Result  map[string]any `json:"result,omitempty"`
	Error   *rpcError      `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

var frameBufferPool = sync.Pool{
	New: func() any {
		return &bytes.Buffer{}
	},
}

type frameProtocolError struct {
	msg string
}

func (e frameProtocolError) Error() string {
	return e.msg
}

func (s *Server) Serve(ctx context.Context, in io.Reader, out, errOut io.Writer) error {
	reader := bufio.NewReader(in)
	for {
		msg, err := readFrame(reader)
		if err != nil {
			if err == io.EOF {
				return nil
			}
			var protocolErr frameProtocolError
			if errors.As(err, &protocolErr) {
				if writeErr := writeResponse(out, rpcResponse{
					JSONRPC: "2.0",
					Error:   &rpcError{Code: -32700, Message: protocolErr.Error()},
				}); writeErr != nil {
					return writeErr
				}
				continue
			}
			return err
		}
		var req rpcRequest
		if err := json.Unmarshal(msg, &req); err != nil {
			writeResponse(out, rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: err.Error()}})
			continue
		}
		resp := s.handle(ctx, req)
		if err := writeResponse(out, resp); err != nil {
			return err
		}
	}
}

func (s *Server) handle(ctx context.Context, req rpcRequest) rpcResponse {
	switch req.Method {
	case "initialize":
		return rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Result: map[string]any{
				"protocolVersion": "2024-11-05",
				"serverInfo": map[string]any{
					"name":    "codegraph",
					"version": version.Current(),
				},
				"capabilities": map[string]any{
					"tools": map[string]any{},
				},
			},
		}
	case "notifications/initialized":
		return rpcResponse{JSONRPC: "2.0"}
	case "tools/list":
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"tools": toolDefinitions()}}
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32602, Message: err.Error()}}
		}
		result, err := s.callTool(ctx, params.Name, params.Arguments)
		if err != nil {
			return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
				"content": []map[string]any{{"type": "text", "text": `{"ok":false,"error":"` + escape(err.Error()) + `"}`}},
				"isError": true,
			}}
		}
		payload, _ := json.Marshal(result)
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
			"content":           []map[string]any{{"type": "text", "text": string(payload)}},
			"structuredContent": result,
			"isError":           false,
		}}
	default:
		log.Printf("unhandled MCP method: %s", req.Method)
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32601, Message: "method not found"}}
	}
}

func (s *Server) callTool(ctx context.Context, name string, raw json.RawMessage) (map[string]any, error) {
	switch name {
	case "index_repo":
		return s.handleIndex(ctx, raw, false)
	case "update_graph":
		return s.handleIndex(ctx, raw, true)
	case "find_symbol":
		var req struct {
			Query  string `json:"query"`
			Limit  int    `json:"limit"`
			Offset int    `json:"offset"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		items, err := s.query.FindSymbol(ctx, s.repoID, req.Query, req.Limit, req.Offset)
		return wrapData("matches", items, err)
	case "find_callers":
		return s.handleCallGraph(ctx, raw, true)
	case "find_callees":
		return s.handleCallGraph(ctx, raw, false)
	case "get_impact_radius":
		var req struct {
			Symbols []string `json:"symbols"`
			Files   []string `json:"files"`
			Depth   int      `json:"depth"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		data, err := s.query.ImpactRadius(ctx, s.repoID, req.Symbols, req.Files, req.Depth)
		if err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "data": data}, nil
	case "find_related_tests":
		var req struct {
			Symbol string `json:"symbol"`
			File   string `json:"file"`
			Limit  int    `json:"limit"`
			Offset int    `json:"offset"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		items, err := s.query.RelatedTests(ctx, s.repoID, req.Symbol, req.File, req.Limit, req.Offset)
		return wrapData("tests", items, err)
	case "search_symbols":
		var req struct {
			Query  string `json:"query"`
			Limit  int    `json:"limit"`
			Offset int    `json:"offset"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		items, err := s.query.SearchSymbols(ctx, s.repoID, req.Query, req.Limit, req.Offset)
		return wrapData("matches", items, err)
	case "search_semantic":
		var req struct {
			Query  string `json:"query"`
			Limit  int    `json:"limit"`
			Offset int    `json:"offset"`
		}
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
		items, err := s.query.SemanticSearch(ctx, s.repoID, req.Query, req.Limit, req.Offset)
		return wrapData("matches", items, err)
	case "graph_stats":
		stats, err := s.query.Stats(ctx, s.repoID)
		if err != nil {
			return nil, err
		}
		return map[string]any{"ok": true, "data": stats}, nil
	case "supported_languages":
		return map[string]any{
			"ok": true,
			"data": map[string]any{
				"languages": s.indexer.SupportedLanguages(),
			},
		}, nil
	case "list_repos":
		var req struct {
			Limit  int `json:"limit"`
			Offset int `json:"offset"`
		}
		if err := json.Unmarshal(raw, &req); err != nil && len(raw) > 0 {
			return nil, err
		}
		items, err := s.store.ListRepos(ctx, req.Limit, req.Offset)
		return wrapData("repos", items, err)
	case "list_scans":
		var req struct {
			Limit  int `json:"limit"`
			Offset int `json:"offset"`
		}
		if err := json.Unmarshal(raw, &req); err != nil && len(raw) > 0 {
			return nil, err
		}
		items, err := s.store.ListScans(ctx, s.repoID, req.Limit, req.Offset)
		return wrapData("scans", items, err)
	case "latest_scan_errors":
		var req struct {
			Limit  int `json:"limit"`
			Offset int `json:"offset"`
		}
		if err := json.Unmarshal(raw, &req); err != nil && len(raw) > 0 {
			return nil, err
		}
		items, err := s.store.LatestScanErrors(ctx, s.repoID, req.Limit, req.Offset)
		return wrapData("errors", items, err)
	default:
		return nil, fmt.Errorf("unknown tool %q", name)
	}
}

func (s *Server) handleIndex(ctx context.Context, raw json.RawMessage, update bool) (map[string]any, error) {
	var req struct {
		RepoPath string   `json:"repo_path"`
		Force    bool     `json:"force"`
		Paths    []string `json:"paths"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	opts := indexer.Options{
		RepoRoot: s.repoRoot,
		Force:    req.Force,
		Paths:    req.Paths,
	}
	if req.RepoPath != "" {
		opts.RepoRoot = req.RepoPath
	}
	var summary store.ScanSummary
	var err error
	if update {
		opts.ScanKind = "update"
		summary, err = s.indexer.Update(ctx, opts)
	} else {
		opts.ScanKind = "index"
		summary, err = s.indexer.Index(ctx, opts)
	}
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "data": summary}, nil
}

func (s *Server) handleCallGraph(ctx context.Context, raw json.RawMessage, callers bool) (map[string]any, error) {
	var req struct {
		Symbol   string `json:"symbol"`
		SymbolID int64  `json:"symbol_id"`
		Limit    int    `json:"limit"`
		Offset   int    `json:"offset"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	var (
		items any
		err   error
		key   = "callees"
	)
	if callers {
		key = "callers"
		items, err = s.query.FindCallers(ctx, s.repoID, req.Symbol, req.SymbolID, req.Limit, req.Offset)
	} else {
		items, err = s.query.FindCallees(ctx, s.repoID, req.Symbol, req.SymbolID, req.Limit, req.Offset)
	}
	return wrapData(key, items, err)
}

func wrapData(key string, value any, err error) (map[string]any, error) {
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "data": map[string]any{key: value}}, nil
}

func readFrame(r *bufio.Reader) ([]byte, error) {
	contentLength := 0
	sawHeader := false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			if err == io.EOF && !sawHeader {
				return nil, io.EOF
			}
			return nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if strings.TrimSpace(line) == "" {
			break
		}
		sawHeader = true
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(line)), "content-length:") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) != 2 {
				return nil, frameProtocolError{msg: "invalid Content-Length header"}
			}
			v := strings.TrimSpace(parts[1])
			n, err := strconv.Atoi(v)
			if err != nil {
				return nil, frameProtocolError{msg: "invalid Content-Length value"}
			}
			if n < 0 {
				return nil, frameProtocolError{msg: "negative Content-Length"}
			}
			contentLength = n
		}
	}
	if !sawHeader {
		return nil, frameProtocolError{msg: "missing frame headers"}
	}
	if contentLength <= 0 {
		return nil, frameProtocolError{msg: "missing Content-Length header"}
	}
	payload := make([]byte, contentLength)
	if _, err := io.ReadFull(r, payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func writeResponse(w io.Writer, resp rpcResponse) error {
	payload, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	frame := frameBufferPool.Get().(*bytes.Buffer)
	frame.Reset()
	defer frameBufferPool.Put(frame)
	fmt.Fprintf(frame, "Content-Length: %d\r\n\r\n", len(payload))
	frame.Write(payload)
	_, err = w.Write(frame.Bytes())
	return err
}

func toolDefinitions() []map[string]any {
	return []map[string]any{
		toolDef("index_repo", "Index a repository into the local code graph", []string{"repo_path", "force", "paths"}),
		toolDef("update_graph", "Update only changed repository files in the local graph", []string{"repo_path", "paths"}),
		toolDef("find_symbol", "Find symbols by exact or fuzzy query", []string{"query", "limit", "offset"}),
		toolDef("find_callers", "Find callers of a symbol", []string{"symbol", "symbol_id", "limit", "offset"}),
		toolDef("find_callees", "Find callees of a symbol", []string{"symbol", "symbol_id", "limit", "offset"}),
		toolDef("get_impact_radius", "Estimate affected symbols and files around a change", []string{"symbols", "files", "depth"}),
		toolDef("find_related_tests", "Find likely related tests for a symbol or file", []string{"symbol", "file", "limit", "offset"}),
		toolDef("search_symbols", "Search symbol names, signatures, and docs", []string{"query", "limit", "offset"}),
		toolDef("search_semantic", "Run lightweight local semantic search", []string{"query", "limit", "offset"}),
		toolDef("graph_stats", "Return repository graph statistics", nil),
		toolDef("supported_languages", "List supported languages and file extensions", nil),
		toolDef("list_repos", "List repositories known to the local graph store", []string{"limit", "offset"}),
		toolDef("list_scans", "List recent scans for the active repository", []string{"limit", "offset"}),
		toolDef("latest_scan_errors", "List latest failed scans and error details", []string{"limit", "offset"}),
	}
}

func toolDef(name, description string, properties []string) map[string]any {
	props := map[string]any{}
	for _, prop := range properties {
		props[prop] = map[string]any{"type": "string"}
		if prop == "limit" || prop == "offset" || prop == "symbol_id" || prop == "depth" {
			props[prop] = map[string]any{"type": "integer"}
		}
		if prop == "force" {
			props[prop] = map[string]any{"type": "boolean"}
		}
		if prop == "paths" || prop == "symbols" || prop == "files" {
			props[prop] = map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
		}
	}
	return map[string]any{
		"name":        name,
		"description": description,
		"inputSchema": map[string]any{
			"type":       "object",
			"properties": props,
		},
	}
}

func escape(s string) string {
	b, _ := json.Marshal(s)
	return strings.Trim(string(b), `"`)
}
