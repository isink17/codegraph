package mcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/isink17/codegraph/internal/compactfmt"
	"github.com/isink17/codegraph/internal/detail"
	"github.com/isink17/codegraph/internal/store"
)

// defaultToolFormat is the encoding every tool uses when `format` is omitted.
// It is JSON, and P15 did not change that: an existing client must keep getting
// the bytes it already parses, and compact is an explicit opt-in.
const defaultToolFormat = compactfmt.FormatJSON

// compactCapableTools is the set of tools that accept `format`.
//
// Membership is a measurement result, not a preference. A tool is here when it
// returns many homogeneous rows whose JSON is dominated by repeated keys. Tools
// left out, and why:
//
//   - search_semantic: its row shape is not fixed -- the hybrid path adds a
//     `kind` the token-overlap fallback does not emit -- and `why` is a list.
//     A tabular schema would have to either lose a column or invent a nested
//     encoding inside a cell.
//   - graph_analytics: three different row shapes behind one argument, at a
//     default limit of 20 rows, one of which carries a path list.
//   - context_for_task: its `estimated_tokens`/`max_tokens` contract budgets the
//     exact JSON document it returns. Compacting after budgeting would make the
//     reported estimate describe a payload the caller never received.
//   - graph_stats, architecture_overview, audit, session_context,
//     detect_frameworks, benchmark_tokens: single nested reports, not row sets.
//   - list_repos, list_scans, latest_scan_errors, session_history,
//     session_hot_files: administrative, small, and not part of the discovery
//     loop compact exists to make cheap.
var compactCapableTools = map[string]bool{
	"find_symbol":        true,
	"search_symbols":     true,
	"find_callers":       true,
	"find_callees":       true,
	"get_impact_radius":  true,
	"find_related_tests": true,
	"find_dead_code":     true,
	"list_files":         true,
	"trace_dependencies": true,
}

// formatArgument reads the `format` argument of a tool call. The value has
// already been validated by validateToolArguments; parsing again here keeps this
// function honest if it is ever called first.
func formatArgument(raw json.RawMessage) (compactfmt.Format, error) {
	format, _, err := formatAndDetailArguments(raw)
	return format, err
}

// formatAndDetailArguments reads the two enum-valued arguments in one pass.
//
// They are decoded together because the usage meter wants both and the encoding
// choice wants one: reading them separately would add a second parse of the same
// arguments object to every tool call, which is the sort of cost a meter is
// supposed to expose, not create. The raw `detail` value is returned unresolved;
// the caller decides what an omitted or invalid level means in its context.
func formatAndDetailArguments(raw json.RawMessage) (compactfmt.Format, string, error) {
	if len(raw) == 0 {
		return defaultToolFormat, "", nil
	}
	var req struct {
		Format string `json:"format"`
		Detail string `json:"detail"`
	}
	if err := json.Unmarshal(raw, &req); err != nil {
		// A malformed arguments payload is reported by the tool's own decoding,
		// with the message that call already produced before P15.
		return defaultToolFormat, "", nil
	}
	format, err := compactfmt.ParseFormat(req.Format, defaultToolFormat)
	return format, req.Detail, err
}

// callToolContent produces the model-visible text of one tool call: the JSON
// success envelope, or a compact document when the caller asked for one.
//
// This is the only place the two encodings diverge. Both are fed by the same
// query and, for symbol results, the same detail projection -- compact is a
// second serializer, never a second query or a second projection.
func (s *Server) callToolContent(ctx context.Context, name string, raw json.RawMessage) (string, error) {
	// The one place every model-visible tool call passes through exactly once,
	// direct or forwarded. Attributing here is what makes a gateway call a single
	// row named for the capability it reached, rather than a tool_call row plus a
	// double-counted target.
	rec := callMeterFrom(ctx)
	desc := rec.observeTool(name)
	if err := validateToolArguments(name, raw); err != nil {
		return "", err
	}
	format, rawDetail, err := formatAndDetailArguments(raw)
	if err != nil {
		return "", err
	}
	rec.observeDimensions(meterDimensions(desc, string(format), rawDetail))
	if format == compactfmt.FormatCompact {
		return s.callToolCompact(ctx, name, raw)
	}
	result, err := s.dispatchTool(ctx, name, raw)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(result)
	if err != nil {
		return "", err
	}
	return string(payload), nil
}

// callToolCompact answers one tool call as a compact document.
func (s *Server) callToolCompact(ctx context.Context, name string, raw json.RawMessage) (string, error) {
	if !compactCapableTools[name] {
		// Unreachable through the MCP surface: `format` is only a property of the
		// tools above, and validateToolArguments rejects unknown arguments.
		return "", fmt.Errorf("tool %q does not support format %q", name, compactfmt.FormatCompact)
	}
	doc := compactfmt.NewDocument(name)
	switch name {
	case "find_symbol", "search_symbols":
		cards, err := s.symbolMatches(ctx, raw, name == "search_symbols")
		if err != nil {
			return "", err
		}
		detail.WriteCards(doc, "matches", cards)
	case "find_callers", "find_callees":
		key, cards, err := s.callGraphCards(ctx, raw, name == "find_callers")
		if err != nil {
			return "", err
		}
		detail.WriteCards(doc, key, cards)
	case "get_impact_radius":
		data, err := s.impactRadiusData(ctx, raw)
		if err != nil {
			return "", err
		}
		if err := writeImpactSections(doc, data); err != nil {
			return "", err
		}
	case "find_related_tests":
		tests, err := s.relatedTests(ctx, raw)
		if err != nil {
			return "", err
		}
		writeRelatedTests(doc, tests)
	case "find_dead_code":
		items, err := s.deadCode(ctx, raw)
		if err != nil {
			return "", err
		}
		if err := writeMapRows(doc, "dead_code", deadCodeColumns, items); err != nil {
			return "", err
		}
	case "list_files":
		items, err := s.listFiles(ctx, raw)
		if err != nil {
			return "", err
		}
		if err := writeMapRows(doc, "files", listFilesColumns, items); err != nil {
			return "", err
		}
	case "trace_dependencies":
		items, err := s.traceDependencies(ctx, raw)
		if err != nil {
			return "", err
		}
		if err := writeMapRows(doc, "dependencies", traceColumns, items); err != nil {
			return "", err
		}
	default:
		// compactCapableTools said yes and this switch has no arm: a tool was
		// advertised with `format` and never given an encoder. Saying so beats
		// returning the encoder's "document has no sections".
		return "", fmt.Errorf("tool %q is registered for compact but has no encoder", name)
	}
	return doc.Encode()
}

// relatedTestColumns mirrors store.RelatedTest field for field. A test asserts
// that against the struct's JSON names, so a new field cannot appear on the JSON
// encoding and quietly miss the compact one.
var relatedTestColumns = compactfmt.Schema{
	Section: "tests",
	Columns: []string{"file", "symbol", "reason", "score"},
}

func writeRelatedTests(doc *compactfmt.Document, tests []store.RelatedTest) {
	sec := doc.Section(relatedTestColumns)
	cells := make([]compactfmt.Cell, 0, len(relatedTestColumns.Columns))
	for _, t := range tests {
		sec.Row(append(cells[:0],
			compactfmt.Str(t.File),
			compactfmt.Str(t.Symbol),
			compactfmt.Str(t.Reason),
			compactfmt.Float(t.Score),
		))
	}
}

// impactFilesSchema and impactSummarySchema are the two non-symbol sections of
// an impact radius. They are separate sections rather than extra columns because
// they are different things: a file list is not a symbol, and the summary counts
// describe the traversal, not any row in it.
var (
	impactFilesSchema   = compactfmt.Schema{Section: "files", Columns: []string{"file"}}
	impactSummarySchema = compactfmt.Schema{Section: "summary", Columns: []string{"affected_symbols", "affected_files"}}
)

// writeImpactSections encodes a projected impact radius without flattening it.
// Section order is fixed -- symbols, files, summary -- so a consumer can read
// positionally if it wants to.
func writeImpactSections(doc *compactfmt.Document, data map[string]any) error {
	cards, ok := data["symbols"].([]detail.Symbol)
	if !ok {
		return fmt.Errorf("impact radius: unexpected symbols shape %T", data["symbols"])
	}
	files, ok := data["files"].([]string)
	if !ok {
		return fmt.Errorf("impact radius: unexpected files shape %T", data["files"])
	}
	summary, ok := data["summary"].(map[string]any)
	if !ok {
		return fmt.Errorf("impact radius: unexpected summary shape %T", data["summary"])
	}

	detail.WriteCards(doc, "symbols", cards)

	fileSection := doc.Section(impactFilesSchema)
	row := make([]compactfmt.Cell, 1)
	for _, file := range files {
		row[0] = compactfmt.Str(file)
		fileSection.Row(row)
	}

	affectedSymbols, err := intFromAny(summary["affected_symbols"])
	if err != nil {
		return fmt.Errorf("impact radius summary affected_symbols: %w", err)
	}
	affectedFiles, err := intFromAny(summary["affected_files"])
	if err != nil {
		return fmt.Errorf("impact radius summary affected_files: %w", err)
	}
	doc.Section(impactSummarySchema).Row([]compactfmt.Cell{
		compactfmt.Int64(affectedSymbols),
		compactfmt.Int64(affectedFiles),
	})
	return nil
}

// column is one column of a schema over a store result that is a map rather
// than a struct. The kind is declared, so a value of the wrong type is an error
// instead of an empty cell that reads as legitimately empty data.
type column struct {
	name string
	kind columnKind
}

type columnKind uint8

const (
	columnString columnKind = iota
	columnInt
	columnFloat
)

// mapSchema is an explicit column contract over a []map[string]any result.
//
// These stores return maps, not structs, so there is no typed model to reuse and
// no field order to inherit. The column list here is the contract; a test asserts
// that the store's own key set still matches it, so a query that gains or renames
// a key fails loudly rather than silently dropping a column from compact output.
type mapSchema struct {
	section string
	columns []column
}

func (m mapSchema) schema() compactfmt.Schema {
	names := make([]string, 0, len(m.columns))
	for _, col := range m.columns {
		names = append(names, col.name)
	}
	return compactfmt.Schema{Section: m.section, Columns: names}
}

var (
	deadCodeColumns = mapSchema{
		section: "dead_code",
		columns: []column{
			{name: "symbol"},
			{name: "kind"},
			{name: "name"},
			{name: "file"},
			{name: "language"},
			{name: "start_line", kind: columnInt},
			{name: "end_line", kind: columnInt},
		},
	}
	listFilesColumns = mapSchema{
		section: "files",
		columns: []column{
			{name: "path"},
			{name: "language"},
			{name: "size_bytes", kind: columnInt},
		},
	}
	traceColumns = mapSchema{
		section: "dependencies",
		columns: []column{
			{name: "symbol"},
			{name: "kind"},
			{name: "name"},
			{name: "file"},
			{name: "depth", kind: columnInt},
			{name: "direction"},
		},
	}
)

func writeMapRows(doc *compactfmt.Document, section string, schema mapSchema, items []map[string]any) error {
	if schema.section != section {
		return fmt.Errorf("compact: schema %q used for section %q", schema.section, section)
	}
	sec := doc.Section(schema.schema())
	cells := make([]compactfmt.Cell, len(schema.columns))
	for _, item := range items {
		for i, col := range schema.columns {
			cell, err := cellFromMap(item, col)
			if err != nil {
				return err
			}
			cells[i] = cell
		}
		sec.Row(cells)
	}
	return nil
}

// cellFromMap reads one declared column out of a store row. An absent key is the
// column's zero value -- a fixed-column format has no way to say "key omitted",
// and the JSON encoding of these tools always emits every key anyway. A present
// value of the wrong type is an error.
func cellFromMap(item map[string]any, col column) (compactfmt.Cell, error) {
	value, present := item[col.name]
	switch col.kind {
	case columnString:
		if !present || value == nil {
			return compactfmt.Str(""), nil
		}
		text, ok := value.(string)
		if !ok {
			return compactfmt.Cell{}, fmt.Errorf("column %q: want string, got %T", col.name, value)
		}
		return compactfmt.Str(text), nil
	case columnInt:
		if !present || value == nil {
			return compactfmt.Int(0), nil
		}
		n, err := intFromAny(value)
		if err != nil {
			return compactfmt.Cell{}, fmt.Errorf("column %q: %w", col.name, err)
		}
		return compactfmt.Int64(n), nil
	case columnFloat:
		if !present || value == nil {
			return compactfmt.Float(0), nil
		}
		f, err := floatFromAny(value)
		if err != nil {
			return compactfmt.Cell{}, fmt.Errorf("column %q: %w", col.name, err)
		}
		return compactfmt.Float(f), nil
	}
	return compactfmt.Cell{}, fmt.Errorf("column %q: unknown column kind", col.name)
}

func intFromAny(value any) (int64, error) {
	switch v := value.(type) {
	case int:
		return int64(v), nil
	case int32:
		return int64(v), nil
	case int64:
		return v, nil
	case float64:
		if float64(int64(v)) != v {
			return 0, fmt.Errorf("want integer, got %v", v)
		}
		return int64(v), nil
	case nil:
		return 0, nil
	default:
		return 0, fmt.Errorf("want integer, got %T", value)
	}
}

func floatFromAny(value any) (float64, error) {
	switch v := value.(type) {
	case float64:
		return v, nil
	case float32:
		return float64(v), nil
	case int:
		return float64(v), nil
	case int64:
		return float64(v), nil
	case nil:
		return 0, nil
	default:
		return 0, fmt.Errorf("want number, got %T", value)
	}
}
