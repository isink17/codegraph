# Compact v2 MCP contract

`format=compact-v2` explicitly selects `codegraph.compact/v2`. JSON remains the
omitted/default format; `format=compact` remains byte-compatible v1. There is no
automatic upgrade or fallback. V1's comma-joined list cells are ambiguous (for
example `["a,b","c"]` and `["a","b,c"]` collide); use JSON or v2 for exact lists.

## Grammar

Lines end with LF on every platform; fields use literal TAB. A document is:

```text
@codegraph.compact/v2
@tool TOOL
@section SECTION
@columns COLUMN<TAB>COLUMN...
@types TYPE<TAB>TYPE...
CELL<TAB>CELL...
```

One or more sections follow `@tool`. Every section requires exactly one
`@columns` followed immediately by exactly one `@types`, including empty
sections. Types and cells must have the column count. Names are nonblank and
unique within their scope. Section/column order and row order are deterministic.
The document ends with LF; a single empty scalar cell is an empty row line.

The only column types are:

- `scalar`: unescaped text, including decimal numbers and `true`/`false` using
  the existing column's semantics. JSON-looking text remains scalar text.
- `string[]`: a canonical JSON array of UTF-8 strings, with the ordinary compact
  cell escaping applied **after** JSON serialization. Empty array is `[]`;
  one empty string is `[""]`. Order and duplicates are retained.

Names and scalar cells use the same five v1 escapes: backslash `\\`, tab `\t`,
LF `\n`, CR `\r`, and at sign `\@`. Nothing is trimmed or normalized. Unknown
escapes fail. Array JSON uses Go `encoding/json.Marshal([]string)`'s canonical
spelling: no insignificant whitespace; quotes, backslashes and controls escaped;
`<`, `>` and `&` use lower-case Unicode escapes, as do U+2028/U+2029. Other valid
Unicode is preserved. Array `null`, null/non-string elements, invalid UTF-8,
unpaired surrogates and noncanonical JSON spellings are rejected. Scalar empty
cells remain legal; an empty array cell is invalid, never an implicit `[]`.

Presence-section excerpt (a full impact response also has symbols, files and
summary sections). Replace `→` by TAB; backslashes are literal wire bytes:

```text
@codegraph.compact/v2
@tool get_impact_radius
@section presence
@columns requested→found→missing_json
@types scalar→scalar→string[]
2→0→["a,b","c"]
```

An array containing a newline and a directive-looking string is written as
`["a\\nb","\@section forged"]`. Decode outer cell escaping **once**, then JSON
once: the values are `a` + newline + `b`, and `@section forged`. A literal
backslash followed by `n` is a different value and survives both layers.

Invalid examples:

```text
@types scalar→inferred        # unknown type
@types scalar                # wrong width for a two-column section
null                         # not a string[] payload
[null]                       # null element
["a", "b"]                   # noncanonical array whitespace
```

Comments in these examples are explanatory, not valid wire syntax. A row starting
with an unescaped `@` is a directive and must not be accepted as data. Producers
escape `@`, preventing directive injection even inside array elements.

## Supported MCP schemas

The same nine tools that support v1 support v2, with the same card projection,
rows, pagination, presence and limitations. `detail=skeleton|excerpt|full` is
rejected for both compact versions. Unsupported tools reject the format argument;
unknown format selectors are errors.

| Tool | Sections in wire order |
|---|---|
| `find_symbol`, `search_symbols` | `matches`, `presence` |
| `find_callers`, `find_callees` | `callers`/`callees`, `presence`, optional `unresolved_hints`, optional `limitations` |
| `get_impact_radius` | `symbols`, `files`, `summary`, `presence`, optional `limitations` |
| `find_related_tests` | `tests`, `presence`, optional `limitations` |
| `find_dead_code` | `dead_code`, optional `limitations` |
| `list_files` | `files` |
| `trace_dependencies` | `dependencies`, `presence`, optional `limitations` |

All columns are declared `scalar` except the `missing_json` presence column:

- General presence columns: `matched`, `target_found`, `requested`, `found`,
  `missing_json`, `total`, `offset`, `truncated`.
- Impact presence columns: `requested`, `found`, `missing_json`.

The existing `missing` JSON/v1 field is represented as `missing_json` with type
`string[]` only in v2. Missing, nil and empty Go slices normalize to `[]` in this
column, including single-target presence shapes with no applicable missing list.
A missing scalar follows its existing fixed-column zero/empty semantics. No
other array is flattened into a cell: result lists have rows in their respective
sections. Array-bearing variable-shape reports such as `search_semantic` and
`graph_analytics` remain JSON-only. A successful compact document represents the
JSON envelope's data and applicable limitations; errors use the existing MCP
error path and never return a partial document.

Direct calls and gateway `tool_call` use the same validator and serializer.
`tools/list` and gateway discovery advertise the same format vocabulary. Usage
tracks `compact-v2` separately and counts the actual model-visible wire bytes
before transport escaping. Token figures are the existing ceil(bytes/4)
estimate, not provider token measurements or an assumed saving.

## Decoder and bounds

`compactfmt.Decode` remains v1-only and rejects v2. Use `DecodeV2` for typed
results: each `TypedCell` has its declared `Type` and either `Scalar` or `Array`.
The returned values are fully decoded; do not unescape or JSON-decode them again.
Unknown versions, extra/missing type directives, duplicate/empty identifiers,
wrong-width rows, malformed escapes/arrays and invalid type declarations fail
closed. The v2 decoder reuses v1 structural validation without changing its
public acceptance semantics.

V2 encoding and decoding reject documents over 16 MiB, over 2,000 total rows,
over 500 sections or columns per section, or arrays over 500 elements. The
array bound is the public batch-item limit; the row bound is the store's existing
query backstop. For bounded escape expansion, each cell's decoded string bytes
(or summed array-element bytes) and each section's summed name bytes are capped
at `MaxV2Bytes/6`; tool names have the same cap. Line count is checked before
splitting. Array parsing checks the element limit while streaming tokens. These
limits add defensive format bounds; existing MCP page/depth/batch validation
still runs before querying and is never bypassed by selecting an encoding.
Invalid data yields an error, never truncation or JSON/v1 fallback. V1's decoder
and resource behavior are unchanged.

## Measured size tradeoff

On the existing in-tree MCP fixtures (model-visible bytes, not JSON-RPC frame
bytes), `go test ./internal/mcp -run TestCompactV2UsageAndSizes -v` reports:

| Fixture | JSON bytes | V1 bytes | V2 bytes | V2 increase over v1 |
|---|---:|---:|---:|---:|
| 40-symbol search | 7,804 | 4,198 | 4,333 | 135 |
| Impact, one found and one missing seed | 989 | 726 | 905 | 179 |
| Related tests, one found and two missing files | 91 | 220 | 331 | 111 |
| Three-file inventory | 147 | 131 | 159 | 28 |

V2 adds schema typing and exact array boundaries. Small tables can exceed JSON;
there is no universal saving claim. The format advertisement adds 180 bytes
(45 estimated tokens) to full tools/list and 60 bytes (15 estimated tokens) to
gateway tools/list. These estimates use ceil(bytes/4), not provider tokenization.
