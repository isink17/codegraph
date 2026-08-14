// Package limits holds CodeGraph's public size policy: how many rows one
// request may ask for, how deep a traversal may go, and how many items a batch
// argument may carry.
//
// The numbers live here rather than next to each tool so that the MCP schema,
// the MCP validator, the CLI flags, and the store's defensive backstop cannot
// drift apart. Every public surface reads the same constant.
//
// Two layers enforce them, deliberately:
//
//   - the public validator rejects an out-of-range value with a clear message,
//     so a caller is never told a request was honoured when it was trimmed;
//   - the store clamps to StoreMaxRows regardless, so an internal caller or a
//     future tool cannot hand SQLite an unbounded LIMIT by accident.
//
// The maxima are derived from measured response cost on an indexed repository,
// not chosen for roundness. At card detail a symbol row serialises to roughly
// 250 bytes, so MaxPage rows cost about 125KB, or ~31k estimated tokens -- the
// most a single tool response can plausibly be worth to a model. The heavier
// detail levels carry source text, so their ceilings are scaled down to land at
// the same response cost:
//
//	detail=card      ~250 B/row  * 500 = ~31k tokens
//	detail=skeleton  ~580 B/row  * 200 = ~29k tokens
//	detail=excerpt  ~1850 B/row  *  50 = ~23k tokens
//	detail=full     ~2025 B/row  *  50 = ~25k tokens
//
// The ceiling belongs to the semantic query, not to its encoder: `format` picks
// how rows are written, never how many exist, so a compact response obeys the
// same row ceiling a JSON one does.
package limits

// Page bounds one page of result rows from a public query -- symbol searches,
// caller/callee neighbours, file and scan inventories, dead code, analytics,
// related tests, and the graph traversals.
const (
	// DefaultPage is the page size a tool returns when `limit` is omitted or 0.
	DefaultPage = 20

	// MaxPage is the hard ceiling for one page of card-detail rows. Above it a
	// public request is rejected rather than trimmed.
	MaxPage = 500

	// MaxPageSkeleton and MaxPageSource lower the ceiling for the detail levels
	// that carry declaration shape and source text, so that the worst legal
	// request costs about what MaxPage cards cost.
	MaxPageSkeleton = 200
	MaxPageSource   = 50
)

// StoreMaxRows is the store's defensive ceiling on any single page query. It is
// deliberately above MaxPage: internal fan-out legitimately over-fetches (hybrid
// search reads three times the requested window before fusing), and the backstop
// must never trim a request the public policy already accepted. Its only job is
// to keep an absurd value -- an internal bug, a test, a surface added later
// without validation -- from reaching SQLite as a real LIMIT.
const StoreMaxRows = 2000

// Bulk export is a different family from a query page: it streams to a file or
// to stdout rather than into a model's context, and `codegraph graph export
// --limit 0` deliberately means "stream the whole repository".
const (
	// DefaultExportPage is the page size the streaming exporters read with.
	DefaultExportPage = 500

	// MaxExportPage bounds the non-streaming paged export, which materialises
	// its page in memory. Callers who want everything should use --limit 0,
	// which streams and stays O(page) regardless of repository size.
	MaxExportPage = 100_000
)

// MaxDepth bounds recursive graph expansion (`get_impact_radius`,
// `trace_dependencies`). Ten hops already reaches the connected closure of any
// real repository; beyond that the traversal only re-walks what it has seen.
const MaxDepth = 10

// MaxBatchItems bounds an array argument that costs one query per element --
// the symbol and file lists on `get_impact_radius` and `find_related_tests`. It
// is a work bound, not an output bound: without it a single call can issue
// unbounded round trips.
//
// Repository-scoped indexing arguments (`paths` on index_repo/update_graph) are
// deliberately not bounded by this: indexing is repo-scale work by definition.
const MaxBatchItems = 500

// MaxContextSelection bounds `max_files` and `max_symbols` on context_for_task.
// The response is already trimmed to `max_tokens`, which stays P14's policy and
// is untouched here; these two widen the candidate set the ranker considers, so
// they bound work rather than output.
const MaxContextSelection = 500

// MaxAuditExamples bounds `examples` on the audit surface. The argument
// multiplies per-finding example queries; 0 still means "counts only" and a
// negative value is still an error, both unchanged.
const MaxAuditExamples = 100

// MaxAgentSteps bounds `max_steps` on agentic_query. Each step is a model call
// plus tool calls, so this is the one argument here that bounds wall-clock and
// spend rather than bytes.
const MaxAgentSteps = 20

// MaxPageForDetail returns the row ceiling that applies at a detail level. The
// level is taken as a string so this package stays dependency-free and can be
// read by the store, the CLI, and the MCP layer alike.
func MaxPageForDetail(level string) int {
	switch level {
	case "skeleton":
		return MaxPageSkeleton
	case "excerpt", "full":
		return MaxPageSource
	default:
		return MaxPage
	}
}

// PageRows normalizes a requested page size for a store query: 0 or negative
// selects DefaultPage, and anything above StoreMaxRows is clamped. Public
// callers are validated before they reach here, so in practice this only ever
// changes a value that never went through validation.
func PageRows(limit int) int {
	if limit <= 0 {
		return DefaultPage
	}
	if limit > StoreMaxRows {
		return StoreMaxRows
	}
	return limit
}

// ExportRows normalizes a page size for the bulk exporters, which page with a
// larger window than a query does and are bounded by MaxExportPage.
func ExportRows(limit int) int {
	if limit <= 0 {
		return DefaultExportPage
	}
	if limit > MaxExportPage {
		return MaxExportPage
	}
	return limit
}

// Offset normalizes a row offset. Offsets are not capped: SQLite stops at the
// end of the result set, so a large offset costs no more than a scan of the
// rows that exist, and paging deep into a large repository is legitimate.
func Offset(offset int) int {
	if offset < 0 {
		return 0
	}
	return offset
}
