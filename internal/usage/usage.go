// Package usage is CodeGraph's local, in-process MCP usage meter.
//
// It answers one question: where did this session's CodeGraph context actually
// go? Tool definitions advertised at startup, arguments the model wrote, and
// content the model read back are counted separately, so the savings from
// gateway mode, from progressive detail levels, and from the compact encoding
// are observable rather than asserted.
//
// # Scope
//
// The meter counts the MODEL-VISIBLE layer and nothing else:
//
//   - for `tools/list`, the serialized tool-definition array the client received
//   - for `tools/call`, the arguments object the client sent and the tool content
//     text the client received
//
// JSON-RPC ids, the result envelope, stdio framing, and transport escaping are
// deliberately excluded. They are real bytes on a pipe, but they are not the
// bytes a model reads, and mixing the two would make every number a lie.
//
// The line is drawn at the request: everything inside a `tools/list` or a
// `tools/call` is metered, including a rejection, and nothing outside one is.
// `initialize`, the notifications, and a `method not found` reply are protocol
// traffic that reaches for no capability, so none of them are metered. Nor is a
// tool loop that runs INSIDE one tool -- agentic_query is charged in full to its
// own response, and its inner calls are not rows of their own.
//
// # Estimator
//
// Token figures come from internal/tokenest: ceil(bytes/4). They are a
// deterministic size estimate, not a provider's tokenizer and not billing usage.
// A report's estimated_tokens is always the sum of its request and response
// estimates, each derived from that stream's aggregate byte count.
//
// # Retention
//
// Nothing but numbers and small enum labels is retained. No task text, no source,
// no arguments, no error strings, no payloads. There is no telemetry, no network,
// and no persistence: a meter lives and dies with one Server.
package usage

import (
	"sort"
	"sync"
	"time"

	"github.com/isink17/codegraph/internal/tokenest"
)

// Schema versions the report document.
const Schema = "codegraph.usage/v1"

// Via distinguishes how a canonical tool was reached.
type Via string

const (
	// ViaDirect is a `tools/call` naming the tool itself.
	ViaDirect Via = "direct"
	// ViaGateway is a `tools/call` on tool_call that forwarded to the tool.
	ViaGateway Via = "gateway"
)

// vias is the reported order of the invocation breakdown.
var vias = []Via{ViaDirect, ViaGateway}

// UnknownTool buckets a call whose canonical tool could not be resolved, such as
// a `tools/call` naming a tool that does not exist. Folding those into one label
// is what keeps an attacker-, or typo-, supplied name from becoming a metric
// dimension.
const UnknownTool = "<unknown>"

// OverflowTool buckets calls once the dimension table is full. It cannot be
// reached by the MCP surface, whose dimensions are all bounded enums over a
// fixed registry; it exists so that a future caller with a looser key cannot
// turn the meter into an unbounded map.
const OverflowTool = "<overflow>"

// maxTrackedKeys bounds the dimension table: once this many distinct keys are
// tracked, further new keys fold into a per-path OverflowTool bucket, so the
// table settles at maxTrackedKeys plus at most one overflow row per Via.
//
// The MCP surface's own ceiling is the sum over tools of via x the format and
// detail vocabularies that tool accepts, which is a few hundred rows. This
// constant is not that ceiling; it is the backstop that keeps a future caller
// with a looser key from turning the meter into an unbounded map.
const maxTrackedKeys = 512

// DefaultToolRows bounds the report's per-tool table.
//
// A meter whose own report is the most expensive response in the session has
// defeated its purpose, so the table is trimmed to the rows a reader actually
// wants -- the costliest ones -- and the trimmed count is reported rather than
// hidden. Session, discovery, and invocation totals always cover every call, so
// trimming loses detail, never accuracy.
const DefaultToolRows = 25

// Key is the bounded dimension tuple of one tool row.
//
// Every field must come from a small closed set: a canonical registry name (or
// UnknownTool), an invocation path, and the format/detail vocabularies. Nothing
// derived from an argument VALUE -- a symbol, a path, a task, a cursor -- may
// ever appear here.
type Key struct {
	Tool string
	Via  Via
	// Format and Detail are empty for tools that do not accept that argument, and
	// are otherwise the effective value after defaulting.
	Format string
	Detail string
}

type counters struct {
	calls     int
	errors    int
	reqBytes  int64
	respBytes int64
}

func (c *counters) add(reqBytes, respBytes int, failed bool) {
	c.calls++
	if failed {
		c.errors++
	}
	c.reqBytes += int64(max0(reqBytes))
	c.respBytes += int64(max0(respBytes))
}

func max0(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

// Meter aggregates one server's usage. The zero value is not usable; call New.
//
// All state is guarded by one mutex held only for the arithmetic. Callers
// measure sizes first and record after, so no tool execution, query, or
// serialization ever runs under the meter lock.
type Meter struct {
	mu        sync.Mutex
	now       func() time.Time
	startedAt time.Time
	toolMode  string

	tools map[Key]*counters
	list  counters
}

// New builds a meter for a server running in toolMode. A nil clock means
// time.Now; tests inject a deterministic one.
func New(toolMode string, now func() time.Time) *Meter {
	if now == nil {
		now = time.Now
	}
	return &Meter{
		now:       now,
		startedAt: now(),
		toolMode:  toolMode,
		tools:     make(map[Key]*counters),
	}
}

// SetToolMode records the surface this server advertises. It is called during
// setup, before any request is served.
func (m *Meter) SetToolMode(mode string) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.toolMode = mode
	m.mu.Unlock()
}

// RecordToolCall books one completed `tools/call`, successful or not.
//
// requestBytes is the model-authored arguments object; responseBytes is the tool
// content text the model reads back, which for an error is the error document
// that took its place. A failed call still cost context, so it is counted, not
// skipped.
func (m *Meter) RecordToolCall(k Key, requestBytes, responseBytes int, failed bool) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.tools[k]
	if !ok {
		if len(m.tools) >= maxTrackedKeys {
			k = Key{Tool: OverflowTool, Via: k.Via}
			c, ok = m.tools[k]
		}
		if !ok {
			c = &counters{}
			m.tools[k] = c
		}
	}
	c.add(requestBytes, responseBytes, failed)
}

// RecordToolsList books one `tools/list`, which is where gateway mode's saving
// is realized. Every call is counted: a client that asks twice paid twice.
func (m *Meter) RecordToolsList(requestBytes, responseBytes int) {
	if m == nil {
		return
	}
	m.mu.Lock()
	m.list.add(requestBytes, responseBytes, false)
	m.mu.Unlock()
}

// Cost is the byte and estimated-token pair for one bucket. EstimatedTokens is
// exactly RequestEstimatedTokens + ResponseEstimatedTokens.
type Cost struct {
	RequestBytes            int64 `json:"request_bytes"`
	RequestEstimatedTokens  int   `json:"request_estimated_tokens"`
	ResponseBytes           int64 `json:"response_bytes"`
	ResponseEstimatedTokens int   `json:"response_estimated_tokens"`
	EstimatedTokens         int   `json:"estimated_tokens"`
}

func cost(c counters) Cost {
	req := tokenest.FromBytes(int(c.reqBytes))
	resp := tokenest.FromBytes(int(c.respBytes))
	return Cost{
		RequestBytes:            c.reqBytes,
		RequestEstimatedTokens:  req,
		ResponseBytes:           c.respBytes,
		ResponseEstimatedTokens: resp,
		EstimatedTokens:         req + resp,
	}
}

// Session is the whole-server total. It covers every metered MCP call, so its
// figures include the Discovery block rather than excluding it.
type Session struct {
	ToolMode       string `json:"tool_mode"`
	ElapsedMS      int64  `json:"elapsed_ms"`
	MCPCalls       int    `json:"mcp_calls"`
	ToolCalls      int    `json:"tool_calls"`
	ToolsListCalls int    `json:"tools_list_calls"`
	Errors         int    `json:"errors"`
	Cost
}

// Discovery is the `tools/list` subtotal: what this session paid to be told
// which tools exist, before asking anything. It is a breakout of Session, not an
// addition to it.
type Discovery struct {
	Calls int `json:"calls"`
	Cost
}

// InvocationRow is the direct-versus-gateway subtotal. Both rows are always
// present so the report's shape does not depend on what a session happened to
// call.
type InvocationRow struct {
	Via    Via `json:"via"`
	Calls  int `json:"calls"`
	Errors int `json:"errors"`
	Cost
}

// ToolRow is one bounded dimension tuple's usage.
type ToolRow struct {
	Name   string `json:"name"`
	Via    Via    `json:"via"`
	Format string `json:"format,omitempty"`
	Detail string `json:"detail,omitempty"`
	Calls  int    `json:"calls"`
	Errors int    `json:"errors"`
	Cost
}

// Report is the usage document. It is fully determined by the recorded counters
// and the elapsed clock: no map iteration order reaches the output.
type Report struct {
	Schema     string          `json:"schema"`
	Session    Session         `json:"session"`
	Discovery  Discovery       `json:"discovery"`
	Invocation []InvocationRow `json:"invocation"`
	// ToolsTotal is how many dimension rows exist; Tools may hold fewer. The
	// difference is stated rather than silently dropped, because a truncated table
	// that looks complete is worse than no table.
	ToolsTotal int       `json:"tools_total"`
	Tools      []ToolRow `json:"tools"`
}

// Snapshot renders the report. When reset is true the counters are cleared under
// the same lock that read them, so a concurrent call is either wholly inside the
// returned snapshot or wholly inside the next one, never split across both.
//
// The snapshot describes usage UP TO the call that asked for it. The asking call
// cannot be in its own report -- its response does not exist yet -- so it is
// booked afterwards, by the same path every other call takes, and appears in the
// next snapshot.
//
// maxToolRows caps the per-tool table, most expensive first; zero or less
// selects DefaultToolRows. Trimming never touches the session, discovery, or
// invocation totals, which always cover every recorded call.
func (m *Meter) Snapshot(reset bool, maxToolRows int) Report {
	if m == nil {
		return Report{Schema: Schema, Invocation: invocationRows(nil), Tools: []ToolRow{}}
	}
	limit := maxToolRows
	if limit <= 0 {
		limit = DefaultToolRows
	}
	m.mu.Lock()
	toolMode := m.toolMode
	elapsed := m.now().Sub(m.startedAt)
	list := m.list
	rows := make([]ToolRow, 0, len(m.tools))
	byVia := map[Via]counters{}
	var tools counters
	for k, c := range m.tools {
		rows = append(rows, ToolRow{
			Name:   k.Tool,
			Via:    k.Via,
			Format: k.Format,
			Detail: k.Detail,
			Calls:  c.calls,
			Errors: c.errors,
			Cost:   cost(*c),
		})
		agg := byVia[k.Via]
		agg.calls += c.calls
		agg.errors += c.errors
		agg.reqBytes += c.reqBytes
		agg.respBytes += c.respBytes
		byVia[k.Via] = agg

		tools.calls += c.calls
		tools.errors += c.errors
		tools.reqBytes += c.reqBytes
		tools.respBytes += c.respBytes
	}
	if reset {
		m.tools = make(map[Key]*counters)
		m.list = counters{}
		m.startedAt = m.now()
	}
	m.mu.Unlock()

	sortToolRows(rows)
	toolsTotal := len(rows)
	if len(rows) > limit {
		rows = rows[:limit]
	}

	total := counters{
		calls:     tools.calls + list.calls,
		errors:    tools.errors,
		reqBytes:  tools.reqBytes + list.reqBytes,
		respBytes: tools.respBytes + list.respBytes,
	}
	return Report{
		Schema: Schema,
		Session: Session{
			ToolMode:       toolMode,
			ElapsedMS:      elapsed.Milliseconds(),
			MCPCalls:       total.calls,
			ToolCalls:      tools.calls,
			ToolsListCalls: list.calls,
			Errors:         total.errors,
			Cost:           cost(total),
		},
		Discovery:  Discovery{Calls: list.calls, Cost: cost(list)},
		Invocation: invocationRows(byVia),
		ToolsTotal: toolsTotal,
		Tools:      rows,
	}
}

func invocationRows(byVia map[Via]counters) []InvocationRow {
	out := make([]InvocationRow, 0, len(vias))
	for _, via := range vias {
		c := byVia[via]
		out = append(out, InvocationRow{Via: via, Calls: c.calls, Errors: c.errors, Cost: cost(c)})
	}
	return out
}

// sortToolRows orders rows by what a reader is looking for -- the most expensive
// responses first -- and breaks every tie on the dimension tuple, so two runs of
// the same session produce byte-identical reports.
func sortToolRows(rows []ToolRow) {
	sort.Slice(rows, func(a, b int) bool {
		x, y := rows[a], rows[b]
		if x.ResponseBytes != y.ResponseBytes {
			return x.ResponseBytes > y.ResponseBytes
		}
		if x.Name != y.Name {
			return x.Name < y.Name
		}
		if x.Via != y.Via {
			return x.Via < y.Via
		}
		if x.Format != y.Format {
			return x.Format < y.Format
		}
		return x.Detail < y.Detail
	})
}
