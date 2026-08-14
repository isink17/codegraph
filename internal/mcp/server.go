package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/isink17/codegraph/internal/agent"
	"github.com/isink17/codegraph/internal/compactfmt"
	"github.com/isink17/codegraph/internal/config"
	"github.com/isink17/codegraph/internal/detail"
	"github.com/isink17/codegraph/internal/framework"
	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/graphaudit"
	"github.com/isink17/codegraph/internal/indexer"
	"github.com/isink17/codegraph/internal/query"
	"github.com/isink17/codegraph/internal/store"
	"github.com/isink17/codegraph/internal/version"
)

type Server struct {
	cliRepoRoot string
	repoRoot    string
	repoID      int64
	store       *store.Store
	indexer     *indexer.Indexer
	query       *query.Service
	errOut      io.Writer
	agentCfg    agent.OllamaLLMConfig
	toolMode    ToolMode
}

// NewServer builds an MCP server. cliRepoRoot is the raw `--repo-root` flag value
// provided to `codegraph serve` (empty when omitted). repoRoot is the resolved
// active repository root for tools that operate on the initially opened repo.
//
// The tool surface defaults to ToolModeFull, so a server built without an
// explicit mode advertises exactly what it advertised before gateway mode existed.
func NewServer(cliRepoRoot, repoRoot string, repoID int64, s *store.Store, idx *indexer.Indexer, q *query.Service, errOut io.Writer) *Server {
	return &Server{cliRepoRoot: cliRepoRoot, repoRoot: repoRoot, repoID: repoID, store: s, indexer: idx, query: q, errOut: errOut, toolMode: ToolModeFull}
}

// SetAgentConfig configures the LLM backend for the agentic_query tool.
func (s *Server) SetAgentConfig(baseURL, model string) {
	s.agentCfg = agent.OllamaLLMConfig{BaseURL: baseURL, Model: model}
}

// SetToolMode selects which tool surface `tools/list` advertises. An empty mode
// is the default.
//
// Call it during setup, before Serve: the field is a plain one, so changing the
// mode while Serve is reading from another goroutine is a data race. Nothing on
// the tool surface calls this, which is the property that matters -- a gateway
// client cannot promote itself to the full surface, because no tool can reach
// this method.
func (s *Server) SetToolMode(mode ToolMode) error {
	parsed, err := ParseToolMode(string(mode))
	if err != nil {
		return err
	}
	s.toolMode = parsed
	return nil
}

// ToolMode reports the surface this server advertises.
func (s *Server) ToolMode() ToolMode {
	if s.toolMode == "" {
		return ToolModeFull
	}
	return s.toolMode
}

// toolDefinitions is the `tools/list` payload for this server's mode.
func (s *Server) toolDefinitions() []map[string]any {
	if s.ToolMode() == ToolModeGateway {
		return gatewayToolDefinitions
	}
	return staticToolDefinitions
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

type toolArgSpec struct {
	properties map[string]string
	required   []string
}

type pageRequest struct {
	Limit  int `json:"limit"`
	Offset int `json:"offset"`
}

// Serve reads JSON-RPC requests from `in` and writes responses to `out`,
// framed per the MCP stdio transport: one JSON object per line, terminated
// by '\n', with no embedded newlines in the JSON itself.
func (s *Server) Serve(ctx context.Context, in io.Reader, out, errOut io.Writer) error {
	reader := bufio.NewReader(in)
	for {
		msg, err := readFrame(reader)
		if err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if len(msg) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(msg, &req); err != nil {
			writeResponse(out, rpcResponse{JSONRPC: "2.0", Error: &rpcError{Code: -32700, Message: err.Error()}})
			continue
		}
		resp, shouldRespond := s.handle(ctx, req)
		if !shouldRespond {
			continue
		}
		if err := writeResponse(out, resp); err != nil {
			return err
		}
	}
}

func (s *Server) handle(ctx context.Context, req rpcRequest) (rpcResponse, bool) {
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
		}, true
	case "notifications/initialized":
		return rpcResponse{}, false
	case "tools/list":
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"tools": s.toolDefinitions()}}, true
	case "tools/call":
		var params struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &params); err != nil {
			return rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32602, Message: err.Error()}}, true
		}
		text, err := s.callToolText(ctx, params.Name, params.Arguments)
		if err != nil {
			// Marshal the error document rather than splicing the message into a
			// JSON literal: a message containing a quote (an argument name, a
			// symbol, a cursor) used to produce a payload the client could not
			// parse at all, turning a clear tool error into a protocol error.
			errPayload, marshalErr := json.Marshal(map[string]any{"ok": false, "error": err.Error()})
			if marshalErr != nil {
				errPayload = []byte(`{"ok":false,"error":"tool call failed"}`)
			}
			return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
				"content": []map[string]any{{"type": "text", "text": string(errPayload)}},
				"isError": true,
			}}, true
		}
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
			"content": []map[string]any{{"type": "text", "text": text}},
			"isError": false,
		}}, true
	default:
		if strings.HasPrefix(req.Method, "notifications/") {
			return rpcResponse{}, false
		}
		fmt.Fprintf(s.errOut, "codegraph: unhandled MCP method: %s\n", req.Method)
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &rpcError{Code: -32601, Message: "method not found"}}, true
	}
}

// callTool validates one tool call and answers it as the JSON success envelope.
//
// It is the in-process entry point for agentic_query's tool loop, which consumes
// the envelope as a map, so this path returns JSON and only JSON. Requesting
// compact here is refused rather than ignored: a caller that asked for an
// encoding and silently received another has no way to tell.
//
// The MCP surface goes through callToolContent instead, which validates once and
// then calls dispatchTool directly, so a tool call is never validated twice.
func (s *Server) callTool(ctx context.Context, name string, raw json.RawMessage) (map[string]any, error) {
	if err := validateToolArguments(name, raw); err != nil {
		return nil, err
	}
	format, err := formatArgument(raw)
	if err != nil {
		return nil, err
	}
	if format != compactfmt.FormatJSON {
		return nil, fmt.Errorf("format %q is available to MCP clients only; this call path returns %q",
			format, compactfmt.FormatJSON)
	}
	return s.dispatchTool(ctx, name, raw)
}

// dispatchTool routes a tool call to its handler and returns the JSON envelope.
// Arguments must already be validated: every caller validates first, so doing it
// here as well would parse the same payload twice on the hot path.
//
// Routing is a registry lookup, not a switch, so the advertised tool list and the
// set of tools that can actually answer are the same set by construction.
func (s *Server) dispatchTool(ctx context.Context, name string, raw json.RawMessage) (map[string]any, error) {
	desc, ok := dispatchableTool(name)
	if !ok {
		return nil, fmt.Errorf("unknown tool %q", name)
	}
	return desc.handler(s, ctx, raw)
}

func (s *Server) handleFindSymbol(ctx context.Context, raw json.RawMessage) (map[string]any, error) {
	cards, err := s.symbolMatches(ctx, raw, false)
	return wrapData("matches", cards, err)
}

func (s *Server) handleSearchSymbols(ctx context.Context, raw json.RawMessage) (map[string]any, error) {
	cards, err := s.symbolMatches(ctx, raw, true)
	return wrapData("matches", cards, err)
}

func (s *Server) handleFindCallers(ctx context.Context, raw json.RawMessage) (map[string]any, error) {
	return s.handleCallGraph(ctx, raw, true)
}

func (s *Server) handleFindCallees(ctx context.Context, raw json.RawMessage) (map[string]any, error) {
	return s.handleCallGraph(ctx, raw, false)
}

func (s *Server) handleIndexRepo(ctx context.Context, raw json.RawMessage) (map[string]any, error) {
	return s.handleIndex(ctx, raw, false)
}

func (s *Server) handleUpdateGraph(ctx context.Context, raw json.RawMessage) (map[string]any, error) {
	return s.handleIndex(ctx, raw, true)
}

func (s *Server) handleImpactRadius(ctx context.Context, raw json.RawMessage) (map[string]any, error) {
	data, err := s.impactRadiusData(ctx, raw)
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "data": data}, nil
}

func (s *Server) handleRelatedTests(ctx context.Context, raw json.RawMessage) (map[string]any, error) {
	items, err := s.relatedTests(ctx, raw)
	return wrapData("tests", items, err)
}

func (s *Server) handleSearchSemantic(ctx context.Context, raw json.RawMessage) (map[string]any, error) {
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
}

func (s *Server) handleGraphStats(ctx context.Context, _ json.RawMessage) (map[string]any, error) {
	stats, err := s.query.Stats(ctx, s.repoID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "data": stats}, nil
}

func (s *Server) handleSupportedLanguages(_ context.Context, _ json.RawMessage) (map[string]any, error) {
	return map[string]any{
		"ok": true,
		"data": map[string]any{
			"languages": s.indexer.SupportedLanguages(),
		},
	}, nil
}

func (s *Server) handleListRepos(ctx context.Context, raw json.RawMessage) (map[string]any, error) {
	req, err := decodePageRequest(raw)
	if err != nil {
		return nil, err
	}
	items, err := s.store.ListRepos(ctx, req.Limit, req.Offset)
	return wrapData("repos", items, err)
}

func (s *Server) handleListScans(ctx context.Context, raw json.RawMessage) (map[string]any, error) {
	req, err := decodePageRequest(raw)
	if err != nil {
		return nil, err
	}
	items, err := s.store.ListScans(ctx, s.repoID, req.Limit, req.Offset)
	return wrapData("scans", items, err)
}

func (s *Server) handleLatestScanErrors(ctx context.Context, raw json.RawMessage) (map[string]any, error) {
	req, err := decodePageRequest(raw)
	if err != nil {
		return nil, err
	}
	items, err := s.store.LatestScanErrors(ctx, s.repoID, req.Limit, req.Offset)
	return wrapData("errors", items, err)
}

func (s *Server) handleDeadCode(ctx context.Context, raw json.RawMessage) (map[string]any, error) {
	items, err := s.deadCode(ctx, raw)
	return wrapData("dead_code", items, err)
}

func (s *Server) handleListFiles(ctx context.Context, raw json.RawMessage) (map[string]any, error) {
	items, err := s.listFiles(ctx, raw)
	return wrapData("files", items, err)
}

func (s *Server) handleContextForTask(ctx context.Context, raw json.RawMessage) (map[string]any, error) {
	var req struct {
		Task           string `json:"task"`
		MaxFiles       int    `json:"max_files"`
		MaxSymbols     int    `json:"max_symbols"`
		IncludeTests   *bool  `json:"include_tests"`
		IncludeCallers *bool  `json:"include_callers"`
		MaxTokens      int    `json:"max_tokens"`
		Cursor         string `json:"cursor"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	opts := query.ContextForTaskOptions{
		MaxFiles:       req.MaxFiles,
		MaxSymbols:     req.MaxSymbols,
		IncludeTests:   true,
		IncludeCallers: true,
		MaxTokens:      req.MaxTokens,
		Cursor:         req.Cursor,
	}
	if req.IncludeTests != nil {
		opts.IncludeTests = *req.IncludeTests
	}
	if req.IncludeCallers != nil {
		opts.IncludeCallers = *req.IncludeCallers
	}
	result, err := s.query.ContextForTask(ctx, s.repoID, req.Task, opts)
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "data": result}, nil
}

func (s *Server) handleArchitectureOverview(ctx context.Context, _ json.RawMessage) (map[string]any, error) {
	data, err := s.query.ArchitectureOverview(ctx, s.repoID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "data": data}, nil
}

func (s *Server) handleTraceDependencies(ctx context.Context, raw json.RawMessage) (map[string]any, error) {
	items, err := s.traceDependencies(ctx, raw)
	return wrapData("dependencies", items, err)
}

func (s *Server) handleDetectFrameworks(ctx context.Context, _ json.RawMessage) (map[string]any, error) {
	imports, err := s.query.AllImports(ctx, s.repoID)
	if err != nil {
		return nil, err
	}
	files, err := s.query.AllFilePaths(ctx, s.repoID)
	if err != nil {
		return nil, err
	}
	detections := framework.Detect(files, imports)
	return map[string]any{"ok": true, "data": map[string]any{"frameworks": detections}}, nil
}

func (s *Server) handleBenchmarkTokens(ctx context.Context, raw json.RawMessage) (map[string]any, error) {
	var req struct {
		Task string `json:"task"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	data, err := s.query.BenchmarkTokens(ctx, s.repoID, req.Task)
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "data": data}, nil
}

func (s *Server) handleCrossLanguageLinks(ctx context.Context, _ json.RawMessage) (map[string]any, error) {
	count, err := s.query.ResolveCrossLanguageLinks(ctx, s.repoID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "data": map[string]any{"links_created": count}}, nil
}

func (s *Server) handleSessionLog(ctx context.Context, raw json.RawMessage) (map[string]any, error) {
	var req struct {
		EventType string `json:"event_type"`
		Key       string `json:"key"`
		Value     string `json:"value"`
		Metadata  string `json:"metadata"`
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	if err := s.store.SessionLogEvent(ctx, s.repoID, req.SessionID, req.EventType, req.Key, req.Value, req.Metadata); err != nil {
		return nil, err
	}
	return map[string]any{"ok": true}, nil
}

func (s *Server) handleSessionHistory(ctx context.Context, raw json.RawMessage) (map[string]any, error) {
	var req struct {
		SessionID string `json:"session_id"`
		EventType string `json:"event_type"`
		Limit     int    `json:"limit"`
		Offset    int    `json:"offset"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	items, err := s.store.SessionGetHistory(ctx, s.repoID, req.SessionID, req.EventType, req.Limit, req.Offset)
	return wrapData("events", items, err)
}

func (s *Server) handleSessionHotFiles(ctx context.Context, raw json.RawMessage) (map[string]any, error) {
	var req struct {
		SessionID string `json:"session_id"`
		Limit     int    `json:"limit"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	items, err := s.store.SessionGetHotFiles(ctx, s.repoID, req.SessionID, req.Limit)
	return wrapData("hot_files", items, err)
}

func (s *Server) handleSessionContext(ctx context.Context, raw json.RawMessage) (map[string]any, error) {
	var req struct {
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	data, err := s.store.SessionGetContext(ctx, s.repoID, req.SessionID)
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "data": data}, nil
}

func (s *Server) handleGraphAnalytics(ctx context.Context, raw json.RawMessage) (map[string]any, error) {
	var req struct {
		Analysis string `json:"analysis"`
		Limit    int    `json:"limit"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	if req.Limit <= 0 {
		req.Limit = 20
	}
	switch req.Analysis {
	case "pagerank":
		items, err := s.query.PageRank(ctx, s.repoID, req.Limit)
		return wrapData("pagerank", items, err)
	case "coupling":
		items, err := s.query.CouplingMetrics(ctx, s.repoID, req.Limit)
		return wrapData("coupling", items, err)
	case "cycles":
		items, err := s.query.DetectCycles(ctx, s.repoID, req.Limit)
		return wrapData("cycles", items, err)
	default:
		return nil, fmt.Errorf("unknown analysis type %q, expected: pagerank, coupling, cycles", req.Analysis)
	}
}

func (s *Server) handleAgenticQuery(ctx context.Context, raw json.RawMessage) (map[string]any, error) {
	var req struct {
		Query    string `json:"query"`
		MaxSteps int    `json:"max_steps"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	llmFn := agent.NewOllamaLLM(s.agentCfg)
	toolFn := func(ctx context.Context, name string, args json.RawMessage) (map[string]any, error) {
		return s.callTool(ctx, name, args)
	}
	ag := agent.New(llmFn, toolFn, req.MaxSteps)
	result, err := ag.Run(ctx, req.Query)
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "data": result}, nil
}

func (s *Server) handleIndex(ctx context.Context, raw json.RawMessage, update bool) (map[string]any, error) {
	var req struct {
		RepoRoot string   `json:"repo_root"`
		RepoPath string   `json:"repo_path"`
		Force    bool     `json:"force"`
		Paths    []string `json:"paths"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	toolParam := strings.TrimSpace(req.RepoRoot)
	if toolParam == "" {
		toolParam = strings.TrimSpace(req.RepoPath)
	}
	resolved, err := config.ResolveRepoRoot(s.cliRepoRoot, toolParam)
	if err != nil {
		return nil, err
	}
	opts := indexer.Options{
		RepoRoot: resolved,
		Force:    req.Force,
		Paths:    req.Paths,
	}
	var summary store.ScanSummary
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
	key, cards, err := s.callGraphCards(ctx, raw, callers)
	return wrapData(key, cards, err)
}

// The helpers below answer one tool each and return the tool's typed result.
// They exist so that the JSON envelope above and the compact encoder in
// compact.go are two serializers of one query and one projection: neither
// encoding can drift into a different result set, a different detail level, or a
// different order than the other.

// symbolMatches answers find_symbol (search=false) and search_symbols
// (search=true).
func (s *Server) symbolMatches(ctx context.Context, raw json.RawMessage, search bool) ([]detail.Symbol, error) {
	var req struct {
		Query  string `json:"query"`
		Limit  int    `json:"limit"`
		Offset int    `json:"offset"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	level, err := detail.Parse(req.Detail, defaultToolDetail)
	if err != nil {
		return nil, err
	}
	var items []graph.Symbol
	if search {
		items, err = s.query.SearchSymbols(ctx, s.repoID, req.Query, req.Limit, req.Offset)
	} else {
		items, err = s.query.FindSymbol(ctx, s.repoID, req.Query, req.Limit, req.Offset)
	}
	if err != nil {
		return nil, err
	}
	return s.projector(level).Symbols(ctx, items), nil
}

// callGraphCards answers find_callers (callers=true) and find_callees, returning
// the response key alongside the projected page.
func (s *Server) callGraphCards(ctx context.Context, raw json.RawMessage, callers bool) (string, []detail.Symbol, error) {
	var req struct {
		Symbol   string `json:"symbol"`
		SymbolID int64  `json:"symbol_id"`
		Limit    int    `json:"limit"`
		Offset   int    `json:"offset"`
		Detail   string `json:"detail"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return "", nil, err
	}
	level, err := detail.Parse(req.Detail, defaultToolDetail)
	if err != nil {
		return "", nil, err
	}
	var (
		items []graph.Symbol
		key   = "callees"
	)
	if callers {
		key = "callers"
		items, err = s.query.FindCallers(ctx, s.repoID, req.Symbol, req.SymbolID, req.Limit, req.Offset)
	} else {
		items, err = s.query.FindCallees(ctx, s.repoID, req.Symbol, req.SymbolID, req.Limit, req.Offset)
	}
	if err != nil {
		return "", nil, err
	}
	return key, s.projector(level).Symbols(ctx, items), nil
}

// impactRadiusData answers get_impact_radius, returning the traversal result
// with its symbol nodes already projected.
func (s *Server) impactRadiusData(ctx context.Context, raw json.RawMessage) (map[string]any, error) {
	var req struct {
		Symbols []string `json:"symbols"`
		Files   []string `json:"files"`
		Depth   int      `json:"depth"`
		Detail  string   `json:"detail"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	level, err := detail.Parse(req.Detail, defaultToolDetail)
	if err != nil {
		return nil, err
	}
	data, err := s.query.ImpactRadius(ctx, s.repoID, req.Symbols, req.Files, req.Depth)
	if err != nil {
		return nil, err
	}
	return s.projectImpact(ctx, level, data), nil
}

// relatedTests answers find_related_tests.
func (s *Server) relatedTests(ctx context.Context, raw json.RawMessage) ([]store.RelatedTest, error) {
	var req struct {
		Symbol string   `json:"symbol"`
		File   string   `json:"file"`
		Files  []string `json:"files"`
		Limit  int      `json:"limit"`
		Offset int      `json:"offset"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	// If multiple files provided, aggregate tests from all of them.
	if len(req.Files) > 0 {
		return s.query.RelatedTestsForFiles(ctx, s.repoID, req.Files, req.Limit, req.Offset)
	}
	return s.query.RelatedTests(ctx, s.repoID, req.Symbol, req.File, req.Limit, req.Offset)
}

// deadCode answers find_dead_code.
func (s *Server) deadCode(ctx context.Context, raw json.RawMessage) ([]map[string]any, error) {
	req, err := decodePageRequest(raw)
	if err != nil {
		return nil, err
	}
	return s.query.FindDeadCode(ctx, s.repoID, req.Limit, req.Offset)
}

// listFiles answers list_files.
func (s *Server) listFiles(ctx context.Context, raw json.RawMessage) ([]map[string]any, error) {
	var req struct {
		PathFilter string `json:"path_filter"`
		Limit      int    `json:"limit"`
		Offset     int    `json:"offset"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	return s.query.ListFiles(ctx, s.repoID, req.PathFilter, req.Limit, req.Offset)
}

// traceDependencies answers trace_dependencies, including its depth clamp.
func (s *Server) traceDependencies(ctx context.Context, raw json.RawMessage) ([]map[string]any, error) {
	var req struct {
		Symbol    string `json:"symbol"`
		Direction string `json:"direction"`
		Depth     int    `json:"depth"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	if req.Depth <= 0 {
		req.Depth = 3
	}
	if req.Depth > 10 {
		req.Depth = 10
	}
	return s.query.TraceDependencies(ctx, s.repoID, req.Symbol, req.Direction, req.Depth)
}

// projector builds the response-scoped renderer for one tool call. s.repoRoot is
// the only filesystem root any rendered source can come from -- no tool argument
// contributes a path.
func (s *Server) projector(level detail.Level) *detail.Projector {
	adapter := storeProjection{store: s.store, repoID: s.repoID}
	return detail.NewProjector(level, s.repoRoot, adapter, adapter)
}

// storeProjection adapts the store to the two narrow contracts the rendering
// layer needs. It lives here rather than on the store so that persistence does
// not import presentation, and it binds the repository id once so the renderer
// never has to carry one.
type storeProjection struct {
	store  *store.Store
	repoID int64
}

func (a storeProjection) SymbolMembers(ctx context.Context, refs []detail.MemberRef) (map[int64][]graph.Symbol, error) {
	ranges := make([]store.MemberRange, 0, len(refs))
	for _, ref := range refs {
		ranges = append(ranges, store.MemberRange{
			SymbolID:  ref.SymbolID,
			FileID:    ref.FileID,
			StartLine: ref.StartLine,
			EndLine:   ref.EndLine,
		})
	}
	return a.store.SymbolMembers(ctx, a.repoID, ranges)
}

func (a storeProjection) FileStates(ctx context.Context, paths []string) (map[string]detail.FileState, error) {
	states, err := a.store.FileSourceStates(ctx, a.repoID, paths)
	if err != nil {
		return nil, err
	}
	out := make(map[string]detail.FileState, len(states))
	for path, state := range states {
		out[path] = detail.FileState{
			SizeBytes:     state.SizeBytes,
			MtimeUnixNs:   state.MtimeUnixNs,
			ContentSHA256: state.ContentSHA256,
		}
	}
	return out, nil
}

// projectImpact rewrites an impact-radius result so its symbol nodes carry the
// requested node detail.
//
// The traversal's own output -- the file list and the summary counts -- is
// copied through untouched: node detail says how much of each symbol to show,
// never how much of the graph to report. The input map is not modified, because
// the same map shape is consumed in-process by graph export.
func (s *Server) projectImpact(ctx context.Context, level detail.Level, data map[string]any) map[string]any {
	symbols, ok := data["symbols"].([]graph.Symbol)
	if !ok {
		return data
	}
	out := make(map[string]any, len(data))
	for key, value := range data {
		out[key] = value
	}
	out["symbols"] = s.projector(level).Symbols(ctx, symbols)
	return out
}

// handleAudit runs the production graph audit against the server's active
// repository and returns the same graphaudit.Report the CLI prints. There is
// one audit implementation; this is a second caller of it, not a second copy.
//
// Two differences from the CLI are deliberate:
//
//   - There is no --fail-on equivalent. Exit-code policy is a property of a
//     process, and an MCP tool call has no exit code; a client that wants to
//     gate on severity reads report.status, which is the same value --fail-on
//     consults.
//   - There is no repo argument. Every tool on this server operates on the
//     repository it was started for, and audit follows that convention rather
//     than introducing a way to reach outside it.
//
// The handle is the server's ordinary read-write store, so the driver-level
// guarantee store.OpenReadOnly gives the CLI does not apply here. graphaudit
// issues only SELECT either way -- see its package comment.
func (s *Server) handleAudit(ctx context.Context, raw json.RawMessage) (map[string]any, error) {
	var req struct {
		// Pointer so an omitted `examples` (use the default) is distinguishable
		// from an explicit 0 (counts only).
		Examples *int `json:"examples"`
	}
	if len(raw) > 0 && strings.TrimSpace(string(raw)) != "null" {
		if err := json.Unmarshal(raw, &req); err != nil {
			return nil, err
		}
	}
	exampleLimit := graphaudit.DefaultExampleLimit
	if req.Examples != nil {
		if *req.Examples < 0 {
			return nil, fmt.Errorf("invalid examples %d: want 0 or more", *req.Examples)
		}
		// graphaudit spells counts-only as a negative limit and treats 0 as
		// "use the default"; the wire spells counts-only as 0.
		if exampleLimit = *req.Examples; exampleLimit == 0 {
			exampleLimit = -1
		}
	}

	// A lookup failure here means the database itself is unreadable, which the
	// audit below would hit anyway with a less informative message. Surface it
	// rather than reporting an empty canonical path as if the repository simply
	// were not registered.
	repo, found, err := s.store.FindRepo(ctx, s.repoRoot)
	if err != nil {
		return nil, fmt.Errorf("look up repository %s: %w", s.repoRoot, err)
	}
	canonical := ""
	if found {
		canonical = repo.CanonicalPath
	}

	report, err := graphaudit.Run(ctx, s.store, graphaudit.Options{
		RepoID:       s.repoID,
		ExampleLimit: exampleLimit,
		Repository: graphaudit.Repository{
			Root:          s.repoRoot,
			CanonicalPath: canonical,
		},
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "data": report}, nil
}

func wrapData(key string, value any, err error) (map[string]any, error) {
	if err != nil {
		return nil, err
	}
	return map[string]any{"ok": true, "data": map[string]any{key: value}}, nil
}

// readFrame reads a single MCP stdio message: one line of JSON terminated
// by '\n' (CRLF tolerated). Empty lines yield an empty payload, which the
// caller skips. EOF is returned when the input is fully consumed.
func readFrame(r *bufio.Reader) ([]byte, error) {
	line, err := r.ReadBytes('\n')
	if err != nil {
		if err == io.EOF && len(line) == 0 {
			return nil, io.EOF
		}
		if err != io.EOF {
			return nil, err
		}
	}
	trimmed := strings.TrimRight(string(line), "\r\n")
	if strings.TrimSpace(trimmed) == "" {
		return nil, nil
	}
	return []byte(trimmed), nil
}

// writeResponse marshals resp as compact JSON and writes it followed by a
// single '\n', per MCP stdio framing.
func writeResponse(w io.Writer, resp rpcResponse) error {
	payload, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	payload = append(payload, '\n')
	_, err = w.Write(payload)
	return err
}

// detailProperty is the JSON Schema for the progressive-disclosure argument.
//
// It is built once and shared by every tool that accepts `detail`, so the four
// level names and the guidance an agent reads cannot drift apart between tools.
// The description is deliberately one line per level: `tools/list` is itself
// part of every session's token budget, and a paragraph per tool would spend
// more than the projection saves.
func detailProperty() map[string]any {
	return map[string]any{
		"type":    "string",
		"enum":    detail.Names(),
		"default": string(defaultToolDetail),
		"description": "How much of each symbol to return. card: identity and location, to choose and drill down. " +
			"skeleton: +signature and members. excerpt: +bounded source. full: +complete source.",
	}
}

// formatProperty is the JSON Schema for the output-encoding argument, shared by
// every tool that accepts `format`.
//
// The description is one line on purpose. tools/list is itself part of every
// session's token budget, and a per-tool copy of the compact grammar would cost
// far more than the encoding saves; the grammar is documented once, in README.
func formatProperty() map[string]any {
	return map[string]any{
		"type":        "string",
		"enum":        compactfmt.Names(),
		"default":     string(defaultToolFormat),
		"description": "Output encoding. compact: tabular, no repeated keys; best for bulk (detail=card).",
	}
}

// defaultToolDetail is the level every migrated MCP tool uses when `detail` is
// omitted. Agents overwhelmingly call these tools to decide where to look next,
// so the cheap representation is the right default and the expensive ones are
// the explicit request.
const defaultToolDetail = detail.Card

// contextMaxTokensProperty and contextCursorProperty document the two
// context_for_task budget arguments. tools/list is itself part of every
// session's token budget, so each is one sentence: what it does, what an agent
// must repeat, and what it may change.
func contextMaxTokensProperty() map[string]any {
	return map[string]any{
		"type":        "integer",
		"default":     query.DefaultContextMaxTokens,
		"description": "Estimated token budget for the response (bytes/4 estimate, not a provider token count). 0 uses the default; over " + strconv.Itoa(query.MaxContextMaxTokens) + " is clamped.",
	}
}

func contextCursorProperty() map[string]any {
	return map[string]any{
		"type":        "string",
		"description": "Opaque next_cursor from a previous call, to fetch context that did not fit. Omit for the first page; replay the same task and options, max_tokens may change.",
	}
}

// toolDef renders one tool's advertised definition. The four documented
// properties get their shared schema; everything else is typed by argType, the
// same function the validator consults, so a schema cannot advertise one type and
// the validator enforce another.
func toolDef(name, description string, properties, required []string) map[string]any {
	props := map[string]any{}
	for _, prop := range properties {
		switch prop {
		case "detail":
			props[prop] = detailProperty()
		case "format":
			props[prop] = formatProperty()
		case "max_tokens":
			props[prop] = contextMaxTokensProperty()
		case "cursor":
			props[prop] = contextCursorProperty()
		default:
			if argType(prop) == "array" {
				props[prop] = map[string]any{"type": "array", "items": map[string]any{"type": "string"}}
				continue
			}
			props[prop] = map[string]any{"type": argType(prop)}
		}
	}
	schema := map[string]any{
		"type":                 "object",
		"properties":           props,
		"additionalProperties": false,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return map[string]any{
		"name":        name,
		"description": description,
		"inputSchema": schema,
	}
}

func decodePageRequest(raw json.RawMessage) (pageRequest, error) {
	var req pageRequest
	if err := json.Unmarshal(raw, &req); err != nil && len(raw) > 0 {
		return pageRequest{}, err
	}
	return req, nil
}

func validateToolArguments(name string, raw json.RawMessage) error {
	spec, ok := toolArgumentSpecs[name]
	if !ok {
		return fmt.Errorf("unknown tool %q", name)
	}
	args := map[string]any{}
	if len(raw) > 0 && strings.TrimSpace(string(raw)) != "null" {
		if err := json.Unmarshal(raw, &args); err != nil {
			return fmt.Errorf("invalid arguments payload: %w", err)
		}
	}
	for key := range args {
		expected, ok := spec.properties[key]
		if !ok {
			return fmt.Errorf("unknown argument %q for tool %q", key, name)
		}
		if err := validateArgType(key, args[key], expected); err != nil {
			return err
		}
	}
	for _, req := range spec.required {
		val, ok := args[req]
		if !ok {
			return fmt.Errorf("missing required argument %q", req)
		}
		if s, ok := val.(string); ok && strings.TrimSpace(s) == "" {
			return fmt.Errorf("required argument %q must not be empty", req)
		}
	}
	// `detail` and `format` are the two enum-valued arguments on this surface.
	// Rejecting an unknown value here, rather than falling back to the default,
	// keeps a typo from silently returning a different amount of information, or a
	// different encoding, than the caller asked for.
	level := defaultToolDetail
	if raw, ok := args["detail"]; ok {
		value, _ := raw.(string)
		parsed, err := detail.Parse(value, defaultToolDetail)
		if err != nil {
			return err
		}
		level = parsed
	}
	if raw, ok := args["format"]; ok {
		value, _ := raw.(string)
		format, err := compactfmt.ParseFormat(value, defaultToolFormat)
		if err != nil {
			return err
		}
		// A detail level compact cannot represent is refused before the query runs,
		// so the caller gets one clear error instead of a document that quietly
		// answers a cheaper question than the one asked.
		if format == compactfmt.FormatCompact {
			if err := detail.CompactSupported(level); err != nil {
				return err
			}
		}
	}
	switch name {
	case "find_callers", "find_callees":
		symbol, _ := args["symbol"].(string)
		symbolID, _ := asInt64(args["symbol_id"])
		if strings.TrimSpace(symbol) == "" && symbolID == 0 {
			return fmt.Errorf("one of symbol or symbol_id is required")
		}
	case "find_related_tests":
		symbol, _ := args["symbol"].(string)
		file, _ := args["file"].(string)
		files, _ := args["files"].([]any)
		if strings.TrimSpace(symbol) == "" && strings.TrimSpace(file) == "" && len(files) == 0 {
			return fmt.Errorf("one of symbol, file, or files is required")
		}
	case "get_impact_radius":
		symbols, _ := args["symbols"].([]any)
		files, _ := args["files"].([]any)
		if len(symbols) == 0 && len(files) == 0 {
			return fmt.Errorf("one of symbols or files is required")
		}
	}
	return nil
}

func validateArgType(name string, value any, expected string) error {
	switch expected {
	case "string":
		if _, ok := value.(string); !ok {
			return fmt.Errorf("argument %q must be a string", name)
		}
	case "boolean":
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("argument %q must be a boolean", name)
		}
	case "integer":
		if _, ok := asInt64(value); !ok {
			return fmt.Errorf("argument %q must be an integer", name)
		}
	case "array":
		if _, ok := value.([]any); !ok {
			return fmt.Errorf("argument %q must be an array", name)
		}
	case "object":
		if _, ok := value.(map[string]any); !ok {
			return fmt.Errorf("argument %q must be an object", name)
		}
	}
	return nil
}

func asInt64(value any) (int64, bool) {
	switch v := value.(type) {
	case float64:
		if float64(int64(v)) == v {
			return int64(v), true
		}
		return 0, false
	case int64:
		return v, true
	case int:
		return int64(v), true
	default:
		return 0, false
	}
}
