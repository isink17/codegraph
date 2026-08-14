// Package compactfmt is CodeGraph's tabular text encoding for bulk MCP results.
//
// JSON spends one copy of every key on every row. A hundred-row card page pays
// for a hundred copies of `"qualified_name"`, which is pure overhead for a
// reader that already knows the shape. compact states the shape once, in a
// header, and then emits values:
//
//	@codegraph.compact/v1
//	@tool search_symbols
//	@section matches
//	@columns symbol_id\tstable_key\tname\tqualified_name\tkind\tlanguage\tfile\tline
//	253\ttype:audit:CaseResult\tCaseResult\taudit.CaseResult\ttype\tgo\tinternal/audit/audit.go\t158
//
// The grammar is deliberately small -- a version line, a tool line, and one or
// more sections of fixed columns -- so that an agent can read it without a
// specification and a program can parse it without a library.
//
// # Grammar
//
//	document  = version-line tool-line section+
//	version-line = "@codegraph.compact/v1" LF
//	tool-line    = "@tool" SP name LF
//	section      = "@section" SP name LF "@columns" SP column (TAB column)* LF row*
//	row          = cell (TAB cell)* LF
//
// Lines are separated by LF on every platform, and the document ends with a
// single LF. There are exactly as many cells in a row as there are columns in
// its section. A line beginning with '@' is always a directive, never a row,
// because '@' inside a cell is escaped.
//
// # Escaping
//
// Five characters are escaped inside a cell, and nothing else is transformed --
// no trimming, no case folding, no path rewriting, no Unicode normalization:
//
//	\\  backslash
//	\t  tab
//	\n  line feed
//	\r  carriage return
//	\@  commercial at
//
// Escaping is byte-wise over UTF-8: every escaped byte is ASCII, so multi-byte
// sequences pass through untouched. A `\` followed by anything else is a decode
// error rather than a silently accepted literal, which is what makes the mapping
// reversible in both directions.
//
// # Values
//
// A cell is text, a decimal integer, a float in Go's shortest round-trip form,
// or `true`/`false`. An empty cell is the empty string. Sections have fixed
// columns, so there is no encoding for "key absent": a field that JSON would
// omit for being empty encodes as an empty cell, which decodes back to the same
// zero value the JSON reader would produce. A field whose absence means
// something other than its zero value does not belong in a compact schema.
package compactfmt

import (
	"fmt"
	"strconv"
	"strings"
)

// Version is the compact format identifier. It is versioned independently of
// the CodeGraph release version: the wire contract changes when the grammar
// changes, not when the binary ships.
const Version = "codegraph.compact/v1"

// Separator is the field separator inside a row.
const Separator = "\t"

// Format names the encoding a tool call asks for.
type Format string

const (
	// FormatJSON is the default encoding: the ordinary JSON success envelope.
	FormatJSON Format = "json"
	// FormatCompact is this package's tabular encoding.
	FormatCompact Format = "compact"
)

// Formats lists the encoding vocabulary. It is the single source of truth for
// MCP tool schemas, so the wire vocabulary cannot drift from the code.
var Formats = []Format{FormatJSON, FormatCompact}

// Names returns the encoding vocabulary as strings, for schemas and flag help.
func Names() []string {
	out := make([]string, 0, len(Formats))
	for _, f := range Formats {
		out = append(out, string(f))
	}
	return out
}

// ParseFormat resolves a wire value to an encoding. An empty or whitespace-only
// value selects fallback, which is how an omitted `format` argument becomes the
// default. Anything else that is not a defined encoding is an error: a
// misspelled encoding must not silently return a representation the caller
// cannot parse.
func ParseFormat(raw string, fallback Format) (Format, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return fallback, nil
	}
	for _, f := range Formats {
		if Format(trimmed) == f {
			return f, nil
		}
	}
	return "", fmt.Errorf("invalid format %q: want one of %s", raw, strings.Join(Names(), ", "))
}

// Schema is one section's explicit, ordered column contract.
//
// Column order is stated, never derived from struct field order or map
// iteration: the order is a public wire contract, and a future refactor of a Go
// type must not silently reorder it.
type Schema struct {
	Section string
	Columns []string
}

// Document is one compact result under construction.
//
// Rows are encoded as they are added, into one buffer, so building a page costs
// one allocation-amortized buffer rather than an intermediate row model. A
// Document is not safe for concurrent use; it is scoped to one response.
type Document struct {
	buf      strings.Builder
	sections map[string]bool
	// seq counts opened sections, so a Section handle can tell whether it is
	// still the open one. Comparing column counts would not: two sections of the
	// same width are indistinguishable that way, and a row appended to the
	// earlier one would land silently under the later header.
	seq int
	err error
}

// NewDocument starts a compact document for one tool.
func NewDocument(tool string) *Document {
	d := &Document{sections: map[string]bool{}}
	if strings.TrimSpace(tool) == "" {
		d.err = fmt.Errorf("compactfmt: empty tool name")
		return d
	}
	d.buf.WriteString("@" + Version + "\n@tool ")
	d.writeDirectiveValue(tool)
	d.buf.WriteString("\n")
	return d
}

// Grow reserves capacity for n more bytes of document text. It is a hint: a
// caller that knows its row count avoids the repeated doubling a growing buffer
// would otherwise pay for a hundred-row page.
func (d *Document) Grow(n int) {
	if n > 0 {
		d.buf.Grow(n)
	}
}

// Section opens a section. Sections appear in the order they are opened, and a
// name may be used only once per document.
func (d *Document) Section(schema Schema) *Section {
	if d.err != nil {
		return &Section{doc: d}
	}
	switch {
	case strings.TrimSpace(schema.Section) == "":
		d.err = fmt.Errorf("compactfmt: empty section name")
		return &Section{doc: d}
	case len(schema.Columns) == 0:
		d.err = fmt.Errorf("compactfmt: section %q has no columns", schema.Section)
		return &Section{doc: d}
	case d.sections[schema.Section]:
		d.err = fmt.Errorf("compactfmt: duplicate section %q", schema.Section)
		return &Section{doc: d}
	}
	// A repeated column name would make one of the two unreachable by name for a
	// consumer that looks columns up rather than counting positions.
	seen := make(map[string]bool, len(schema.Columns))
	for _, col := range schema.Columns {
		if strings.TrimSpace(col) == "" {
			d.err = fmt.Errorf("compactfmt: section %q has an empty column name", schema.Section)
			return &Section{doc: d}
		}
		if seen[col] {
			d.err = fmt.Errorf("compactfmt: section %q has duplicate column %q", schema.Section, col)
			return &Section{doc: d}
		}
		seen[col] = true
	}
	d.sections[schema.Section] = true
	d.buf.WriteString("@section ")
	d.writeDirectiveValue(schema.Section)
	d.buf.WriteString("\n@columns ")
	for i, col := range schema.Columns {
		if i > 0 {
			d.buf.WriteString(Separator)
		}
		d.writeDirectiveValue(col)
	}
	d.buf.WriteString("\n")
	d.seq++
	return &Section{doc: d, columns: len(schema.Columns), seq: d.seq}
}

// Section is a handle for appending rows to the section that opened it.
type Section struct {
	doc     *Document
	columns int
	seq     int
}

// Row appends one row. The cell count must equal the section's column count; a
// mismatch is recorded and surfaced by Encode rather than emitting a document
// whose rows do not line up with its header.
func (s *Section) Row(cells []Cell) {
	d := s.doc
	if d.err != nil {
		return
	}
	if s.seq != d.seq {
		d.err = fmt.Errorf("compactfmt: row written to a closed section")
		return
	}
	if len(cells) != s.columns {
		d.err = fmt.Errorf("compactfmt: row has %d cells, section has %d columns", len(cells), s.columns)
		return
	}
	for i := range cells {
		if i > 0 {
			d.buf.WriteString(Separator)
		}
		cells[i].writeTo(&d.buf)
	}
	d.buf.WriteString("\n")
}

// Encode returns the document text, or the first error recorded while building
// it. An error is never a partial document: the caller reports a tool error
// instead of emitting a table a client cannot trust.
func (d *Document) Encode() (string, error) {
	if d.err != nil {
		return "", d.err
	}
	if len(d.sections) == 0 {
		return "", fmt.Errorf("compactfmt: document has no sections")
	}
	return d.buf.String(), nil
}

// writeDirectiveValue writes a header value with cell escaping, so a tool,
// section, or column name containing a separator or a newline cannot forge
// structure.
func (d *Document) writeDirectiveValue(v string) {
	writeEscaped(&d.buf, v)
}

type cellKind uint8

const (
	cellString cellKind = iota
	cellInt
	cellFloat
	cellBool
)

// Cell is one encoded value. Numbers and booleans are held in their own form
// rather than pre-formatted, so a row of integers costs no intermediate strings.
type Cell struct {
	str  string
	num  int64
	flt  float64
	bit  bool
	kind cellKind
}

// Str is a text cell. The empty string is a legal value and encodes as an empty
// cell.
func Str(v string) Cell { return Cell{kind: cellString, str: v} }

// Int is an integer cell.
func Int(v int) Cell { return Cell{kind: cellInt, num: int64(v)} }

// Int64 is an integer cell.
func Int64(v int64) Cell { return Cell{kind: cellInt, num: v} }

// Float is a floating-point cell, encoded in Go's shortest form that parses
// back to the same float64.
func Float(v float64) Cell { return Cell{kind: cellFloat, flt: v} }

// Bool is a boolean cell, encoded as `true` or `false`.
func Bool(v bool) Cell { return Cell{kind: cellBool, bit: v} }

func (c Cell) writeTo(b *strings.Builder) {
	switch c.kind {
	case cellString:
		writeEscaped(b, c.str)
	case cellInt:
		var scratch [20]byte
		b.Write(strconv.AppendInt(scratch[:0], c.num, 10))
	case cellFloat:
		var scratch [32]byte
		b.Write(strconv.AppendFloat(scratch[:0], c.flt, 'g', -1, 64))
	case cellBool:
		if c.bit {
			b.WriteString("true")
			return
		}
		b.WriteString("false")
	}
}

// writeEscaped writes s with the five escapes applied. It walks bytes rather
// than runes: every escaped byte is ASCII, so a multi-byte UTF-8 sequence -- or
// a byte sequence that is not valid UTF-8 at all -- is copied through
// unchanged instead of being replaced.
func writeEscaped(b *strings.Builder, s string) {
	start := 0
	for i := 0; i < len(s); i++ {
		var esc string
		switch s[i] {
		case '\\':
			esc = `\\`
		case '\t':
			esc = `\t`
		case '\n':
			esc = `\n`
		case '\r':
			esc = `\r`
		case '@':
			esc = `\@`
		default:
			continue
		}
		b.WriteString(s[start:i])
		b.WriteString(esc)
		start = i + 1
	}
	b.WriteString(s[start:])
}
