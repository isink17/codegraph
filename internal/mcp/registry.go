package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
)

// ToolMode selects how much of the tool surface `tools/list` advertises.
//
// The mode is a property of the running server, chosen at startup. It is never a
// tool argument and never session state: a client that was started against the
// gateway surface cannot talk the server into the full one, because there is
// nothing to talk to.
type ToolMode string

const (
	// ToolModeFull advertises every tool. It is the default, and it is what a
	// client that predates gateway mode sees.
	ToolModeFull ToolMode = "full"
	// ToolModeGateway advertises a small core plus tool_search/tool_call, which
	// reach every remaining tool on demand.
	ToolModeGateway ToolMode = "gateway"
)

// ParseToolMode reads a tool mode from a flag or config value. An empty value is
// the default. An unknown value is an error rather than a fallback: a client
// that asked for a surface and silently got another has no way to tell.
func ParseToolMode(value string) (ToolMode, error) {
	switch strings.TrimSpace(value) {
	case "":
		return ToolModeFull, nil
	case string(ToolModeFull):
		return ToolModeFull, nil
	case string(ToolModeGateway):
		return ToolModeGateway, nil
	default:
		return "", fmt.Errorf("invalid tool mode %q: want %q or %q", value, ToolModeFull, ToolModeGateway)
	}
}

// ToolModeNames lists the valid modes, for help text and error messages.
func ToolModeNames() []string { return []string{string(ToolModeFull), string(ToolModeGateway)} }

// toolHandler answers one tool call and returns the JSON envelope. Every tool
// has the same shape here so that dispatch is a registry lookup rather than a
// switch that can fall out of step with the advertised tool list.
type toolHandler func(*Server, context.Context, json.RawMessage) (map[string]any, error)

// toolDescriptor is the one canonical description of a tool.
//
// `tools/list` in both modes, the argument validator, the gateway's discovery
// catalog, and dispatch all read this struct. There is no second list of names,
// no second copy of a schema, and no way for a tool to be advertised without a
// handler: the fields that used to live in three parallel tables are now one row.
type toolDescriptor struct {
	name        string
	description string
	// properties is the ordered argument list. It drives both the JSON Schema in
	// tools/list and the accepted-argument set in the validator, so a schema and
	// its validation cannot disagree.
	properties []string
	required   []string
	// category is a single word used only by tool_search ranking and output. It is
	// deliberately not part of the advertised schema: tools/list is charged to
	// every session's context budget, and a category helps only the agent that is
	// already searching.
	category string
	// mutates marks a tool that writes -- to the graph, or to session state. Read
	// tools leave it false. tool_search reports it so an agent reaching for an
	// unfamiliar tool is not surprised by a write.
	mutates bool
	// gatewayCore exposes the tool directly in gateway mode.
	gatewayCore bool
	// gatewayMeta marks the discovery/invocation tools themselves. They exist only
	// in gateway mode, are absent from full mode, and carry no handler: they are
	// answered at the text layer, above the JSON envelope, and cannot be reached
	// through tool_call or through the in-process agent loop.
	gatewayMeta bool
	// hidden keeps a tool out of BOTH `tools/list` payloads while leaving it fully
	// callable by exact name and fully discoverable through tool_search.
	//
	// This is how a diagnostic earns a place on the surface without charging every
	// session for its schema. A tool an agent needs on a normal task belongs in the
	// list; a tool an agent reaches for when it is specifically asking about the
	// tooling does not.
	hidden  bool
	handler toolHandler

	// acceptsFormat and acceptsDetail are derived from properties in init(). The
	// usage meter reads them so a tool is only bucketed by an argument it actually
	// accepts.
	acceptsFormat bool
	acceptsDetail bool
}

// toolRegistry is the canonical tool table. Order is the order `tools/list`
// reports in full mode, and the first 29 entries are in exactly the order they
// were advertised before gateway mode existed.
//
// The gateway meta tools come last so that the gateway surface reads core tools
// first, discovery second.
var toolRegistry = []toolDescriptor{
	{
		name: "index_repo", description: "Index a repository into the local code graph",
		properties: []string{"repo_root", "repo_path", "force", "paths"},
		category:   "index", mutates: true,
		handler: (*Server).handleIndexRepo,
	},
	{
		name: "update_graph", description: "Update only changed repository files in the local graph",
		properties: []string{"repo_root", "repo_path", "force", "paths"},
		category:   "index", mutates: true,
		handler: (*Server).handleUpdateGraph,
	},
	{
		name: "find_symbol", description: "Find symbols by exact or substring query",
		properties: []string{"query", "limit", "offset", "detail", "format"},
		required:   []string{"query"},
		category:   "search", gatewayCore: true,
		handler: (*Server).handleFindSymbol,
	},
	{
		name: "find_callers", description: "Find resolved callers of an indexed symbol; unknown names may return unresolved hints separately",
		properties: []string{"symbol", "symbol_id", "limit", "offset", "detail", "format"},
		category:   "graph", gatewayCore: true,
		handler: (*Server).handleFindCallers,
	},
	{
		name: "find_callees", description: "Find resolved callees of an indexed symbol",
		properties: []string{"symbol", "symbol_id", "limit", "offset", "detail", "format"},
		category:   "graph", gatewayCore: true,
		handler: (*Server).handleFindCallees,
	},
	{
		name: "get_impact_radius", description: "Estimate resolved dependency impact; report unresolved uncertainty separately",
		properties: []string{"symbols", "files", "depth", "limit", "offset", "detail", "format"},
		category:   "graph",
		handler:    (*Server).handleImpactRadius,
	},
	{
		name: "find_related_tests", description: "Find likely related tests; missing symbol targets return target_found=false",
		properties: []string{"symbol", "file", "files", "limit", "offset", "format"},
		category:   "tests",
		handler:    (*Server).handleRelatedTests,
	},
	{
		name: "search_symbols", description: "Search symbol names, signatures, and docs",
		properties: []string{"query", "limit", "offset", "detail", "format"},
		required:   []string{"query"},
		category:   "search",
		handler:    (*Server).handleSearchSymbols,
	},
	{
		name: "search_semantic", description: "Hybrid semantic search (vector similarity + FTS when embeddings available, token-overlap fallback)",
		properties: []string{"query", "limit", "offset"},
		required:   []string{"query"},
		category:   "search",
		handler:    (*Server).handleSearchSemantic,
	},
	{
		name: "graph_stats", description: "Return repository graph statistics",
		category: "overview",
		handler:  (*Server).handleGraphStats,
	},
	{
		name: "supported_languages", description: "List supported languages and file extensions",
		category: "overview",
		handler:  (*Server).handleSupportedLanguages,
	},
	{
		name: "list_repos", description: "List repositories known to the local graph store",
		properties: []string{"limit", "offset"},
		category:   "inventory",
		handler:    (*Server).handleListRepos,
	},
	{
		name: "list_scans", description: "List recent scans for the active repository",
		properties: []string{"limit", "offset"},
		category:   "inventory",
		handler:    (*Server).handleListScans,
	},
	{
		name: "latest_scan_errors", description: "List latest failed scans and error details",
		properties: []string{"limit", "offset"},
		category:   "inventory",
		handler:    (*Server).handleLatestScanErrors,
	},
	{
		name: "context_for_task", description: "Return relevant files, symbols, and relationships for a natural-language task description, ranked and trimmed to a token budget",
		properties: []string{"task", "max_files", "max_symbols", "include_tests", "include_callers", "max_tokens", "cursor"},
		required:   []string{"task"},
		category:   "context", gatewayCore: true,
		handler: (*Server).handleContextForTask,
	},
	{
		name: "find_dead_code", description: "Find symbols with no callers or references (likely dead code)",
		properties: []string{"limit", "offset", "format"},
		category:   "graph",
		handler:    (*Server).handleDeadCode,
	},
	{
		name: "list_files", description: "List indexed files in the repository, optionally filtered by path prefix",
		properties: []string{"path_filter", "limit", "offset", "format"},
		category:   "inventory",
		handler:    (*Server).handleListFiles,
	},
	{
		name: "architecture_overview", description: "High-level repository architecture: languages, directories, key symbols, dependency patterns",
		category: "overview",
		handler:  (*Server).handleArchitectureOverview,
	},
	{
		name: "trace_dependencies", description: "Trace resolved dependency edges from an exact symbol (upstream callers or downstream callees)",
		properties: []string{"symbol", "direction", "depth", "limit", "offset", "format"},
		required:   []string{"symbol"},
		category:   "graph",
		handler:    (*Server).handleTraceDependencies,
	},
	{
		name: "detect_frameworks", description: "Detect frameworks and libraries used in the repository",
		category: "overview",
		handler:  (*Server).handleDetectFrameworks,
	},
	{
		name: "benchmark_tokens", description: "Estimate token savings from using codegraph vs reading raw files",
		properties: []string{"task"},
		category:   "overview",
		handler:    (*Server).handleBenchmarkTokens,
	},
	{
		name: "cross_language_links", description: "Find and create cross-language symbol references",
		category: "graph", mutates: true,
		handler: (*Server).handleCrossLanguageLinks,
	},
	{
		name: "session_log", description: "Log a session event (read, edit, decision, task, fact)",
		properties: []string{"event_type", "key", "value", "metadata", "session_id"},
		required:   []string{"event_type"},
		category:   "session", mutates: true,
		handler: (*Server).handleSessionLog,
	},
	{
		name: "session_history", description: "Get session event history",
		properties: []string{"session_id", "event_type", "limit", "offset"},
		category:   "session",
		handler:    (*Server).handleSessionHistory,
	},
	{
		name: "session_hot_files", description: "Get most frequently accessed files in sessions",
		properties: []string{"session_id", "limit"},
		category:   "session",
		handler:    (*Server).handleSessionHotFiles,
	},
	{
		name: "session_context", description: "Get aggregated session context for pre-loading",
		properties: []string{"session_id"},
		category:   "session",
		handler:    (*Server).handleSessionContext,
	},
	{
		name: "graph_analytics", description: "Run graph analytics: pagerank, coupling, or cycles",
		properties: []string{"analysis", "limit"},
		required:   []string{"analysis"},
		category:   "graph",
		handler:    (*Server).handleGraphAnalytics,
	},
	{
		name: "agentic_query", description: "Ask a question answered by an AI agent that reasons over the code graph (requires local Ollama)",
		properties: []string{"query", "max_steps"},
		required:   []string{"query"},
		// The agent loop calls other tools, including the mutating ones, so it is
		// reported as a writer even though it holds no write of its own.
		category: "agent", mutates: true,
		handler: (*Server).handleAgenticQuery,
	},
	{
		name: "audit", description: "Audit the indexed graph for integrity, resolver-correctness, and trust issues (read-only)",
		properties: []string{"examples"},
		category:   "audit",
		handler:    (*Server).handleAudit,
	},

	// Hidden diagnostics. Callable and searchable, never advertised: the whole
	// point of measuring context cost is not to add any.
	{
		name: "usage_stats",
		// Deliberately short and specific. A long description would cost every
		// tool_search result that mentions it, and generic words like graph, session,
		// or context would rank a diagnostic against real navigation tools.
		description: "Report this MCP session's own local token usage per tool. Diagnostic only.",
		properties:  []string{"reset", "limit"},
		category:    "overview",
		hidden:      true,
		handler:     (*Server).handleUsageStats,
	},

	// Gateway meta tools. Absent from full mode on purpose: adding them there
	// would charge every default session for a surface it does not need.
	{
		name:        "tool_search",
		description: "Find a CodeGraph tool that is not listed here. Returns name, description, and category; add include_schema with an exact name for its arguments.",
		properties:  []string{"query", "limit", "include_schema"},
		required:    []string{"query"},
		category:    "gateway",
		gatewayMeta: true,
	},
	{
		name:        "tool_call",
		description: "Call any CodeGraph tool by exact name, including tools not listed here. Find names with tool_search.",
		properties:  []string{"name", "arguments"},
		required:    []string{"name"},
		category:    "gateway",
		gatewayMeta: true,
	},
}

// toolByName indexes the registry.
//
// It is filled in init() rather than by a var initializer because a descriptor
// holds a method value on *Server, and one of those handlers reaches dispatch,
// which reads this map: as package-level var initialization that is a cycle the
// compiler rejects, and init() runs after every var is already built.
//
// The population loop also fails loudly on a duplicate name, on a dispatchable
// tool with no handler, and on a meta tool that carries one -- so a registry edit
// cannot ship a tool that is advertised and then answers "unknown tool".
var toolByName = map[string]*toolDescriptor{}

// toolArgumentSpecs is the validator's view of the registry, derived from the
// same rows that produce the advertised schemas.
var toolArgumentSpecs = map[string]toolArgSpec{}

func init() {
	for i := range toolRegistry {
		desc := &toolRegistry[i]
		if _, dup := toolByName[desc.name]; dup {
			panic("mcp: duplicate tool " + desc.name)
		}
		if desc.gatewayMeta == (desc.handler != nil) {
			panic("mcp: tool " + desc.name + " must have exactly one of a handler or gatewayMeta")
		}
		if desc.gatewayMeta && desc.gatewayCore {
			panic("mcp: tool " + desc.name + " cannot be both a meta tool and a core tool")
		}
		if desc.hidden && (desc.gatewayCore || desc.gatewayMeta) {
			panic("mcp: tool " + desc.name + " cannot be hidden and advertised at once")
		}
		toolByName[desc.name] = desc

		props := make(map[string]string, len(desc.properties))
		for _, prop := range desc.properties {
			props[prop] = argType(prop)
			switch prop {
			case "format":
				desc.acceptsFormat = true
			case "detail":
				desc.acceptsDetail = true
			}
		}
		toolArgumentSpecs[desc.name] = toolArgSpec{properties: props, required: desc.required}
	}
}

// staticToolDefinitions is the full-mode `tools/list` payload, built once.
var staticToolDefinitions = buildToolDefinitions()

// gatewayToolDefinitions is the gateway-mode `tools/list` payload, built once:
// the core workflow tools in registry order, then the discovery tools.
var gatewayToolDefinitions = buildGatewayToolDefinitions()

// buildToolDefinitions renders the full tool surface. The gateway meta tools and
// the hidden diagnostics are excluded, so this is byte for byte the list every
// pre-gateway client received.
func buildToolDefinitions() []map[string]any {
	defs := make([]map[string]any, 0, len(toolRegistry))
	for _, desc := range toolRegistry {
		if desc.gatewayMeta || desc.hidden {
			continue
		}
		defs = append(defs, desc.definition())
	}
	return defs
}

// buildGatewayToolDefinitions renders the reduced surface. Core tools carry the
// same name, description, and schema they have in full mode -- a gateway client
// and a full client are calling the same tool, described the same way.
func buildGatewayToolDefinitions() []map[string]any {
	defs := make([]map[string]any, 0, 8)
	for _, desc := range toolRegistry {
		if desc.gatewayCore {
			defs = append(defs, desc.definition())
		}
	}
	for _, desc := range toolRegistry {
		if desc.gatewayMeta {
			defs = append(defs, desc.definition())
		}
	}
	return defs
}

// staticToolDefinitionsBytes and gatewayToolDefinitionsBytes are the serialized
// sizes of the two advertised payloads, measured once at initialization.
//
// The usage meter reads these instead of marshalling a `tools/list` response a
// second time to size it. The definitions are immutable after init, so the
// measurement can never drift from what a client actually receives.
var (
	staticToolDefinitionsBytes  = definitionsBytes(staticToolDefinitions)
	gatewayToolDefinitionsBytes = definitionsBytes(gatewayToolDefinitions)
)

// definitionsBytes measures a rendered tool list exactly as the byte-identity
// guard measures it: the marshalled definition array, without the JSON-RPC
// result envelope that carries it.
func definitionsBytes(defs []map[string]any) int {
	payload, err := json.Marshal(defs)
	if err != nil {
		panic("mcp: tool definitions are not serializable: " + err.Error())
	}
	return len(payload)
}

func (d toolDescriptor) definition() map[string]any {
	return toolDef(d.name, d.description, d.properties, d.required)
}

// argType is the one mapping from argument name to JSON type. Both the advertised
// schema and the validator read it, so a property cannot be documented as one
// type and checked as another.
func argType(prop string) string {
	switch prop {
	case "limit", "offset", "symbol_id", "depth", "max_files", "max_symbols", "max_steps", "examples", "max_tokens":
		return "integer"
	case "force", "include_tests", "include_callers", "include_schema", "reset":
		return "boolean"
	case "paths", "symbols", "files":
		return "array"
	case "arguments":
		return "object"
	default:
		return "string"
	}
}

// dispatchableTool resolves a name to a tool that can actually be called. The
// gateway meta tools are answered above this layer and are deliberately not
// resolvable here, which is what makes a meta tool calling itself impossible
// rather than merely rejected.
func dispatchableTool(name string) (*toolDescriptor, bool) {
	desc, ok := toolByName[name]
	if !ok || desc.handler == nil {
		return nil, false
	}
	return desc, true
}
