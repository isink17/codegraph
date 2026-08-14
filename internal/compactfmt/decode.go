package compactfmt

import (
	"fmt"
	"strings"
)

// Decoded is a parsed compact document. Cells are returned as decoded text; a
// consumer that knows a column is numeric parses it, exactly as it would from a
// CSV or a database driver.
type Decoded struct {
	Version  string
	Tool     string
	Sections []DecodedSection
}

// DecodedSection is one section of a parsed document.
type DecodedSection struct {
	Name    string
	Columns []string
	Rows    [][]string
}

// Section returns the named section, or nil when the document has none.
func (d *Decoded) Section(name string) *DecodedSection {
	for i := range d.Sections {
		if d.Sections[i].Name == name {
			return &d.Sections[i]
		}
	}
	return nil
}

// Cell returns the value of one column of one row by name, and whether the
// column exists. It keeps consumers from hard-coding column positions.
func (s *DecodedSection) Cell(row int, column string) (string, bool) {
	if row < 0 || row >= len(s.Rows) {
		return "", false
	}
	for i, col := range s.Columns {
		if col == column && i < len(s.Rows[row]) {
			return s.Rows[row][i], true
		}
	}
	return "", false
}

// Decode parses a compact document.
//
// An unknown version is rejected rather than parsed optimistically: a future
// grammar may reuse the same line shapes with different meaning, and a decoder
// that guessed would hand its caller plausible nonsense.
func Decode(text string) (*Decoded, error) {
	if text == "" {
		return nil, fmt.Errorf("compactfmt: empty document")
	}
	lines := strings.Split(text, "\n")
	// The document ends with exactly one LF; that terminator is not a row.
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) < 2 {
		return nil, fmt.Errorf("compactfmt: document has no header")
	}
	if !strings.HasPrefix(lines[0], "@") {
		return nil, fmt.Errorf("compactfmt: first line is not a version directive")
	}
	version := lines[0][1:]
	if version != Version {
		return nil, fmt.Errorf("compactfmt: unsupported format version %q: want %q", version, Version)
	}
	tool, ok := directiveValue(lines[1], "@tool ")
	if !ok {
		return nil, fmt.Errorf("compactfmt: second line is not a @tool directive")
	}
	toolName, err := unescape(tool)
	if err != nil {
		return nil, fmt.Errorf("compactfmt: @tool: %w", err)
	}

	out := &Decoded{Version: version, Tool: toolName}
	var current *DecodedSection
	for i := 2; i < len(lines); i++ {
		line := lines[i]
		switch {
		case strings.HasPrefix(line, "@section "):
			name, err := unescape(strings.TrimPrefix(line, "@section "))
			if err != nil {
				return nil, fmt.Errorf("compactfmt: line %d: @section: %w", i+1, err)
			}
			out.Sections = append(out.Sections, DecodedSection{Name: name})
			current = &out.Sections[len(out.Sections)-1]
		case strings.HasPrefix(line, "@columns "):
			if current == nil {
				return nil, fmt.Errorf("compactfmt: line %d: @columns outside a section", i+1)
			}
			if current.Columns != nil {
				return nil, fmt.Errorf("compactfmt: line %d: section %q has two @columns directives", i+1, current.Name)
			}
			cols, err := splitRow(strings.TrimPrefix(line, "@columns "))
			if err != nil {
				return nil, fmt.Errorf("compactfmt: line %d: @columns: %w", i+1, err)
			}
			seen := make(map[string]bool, len(cols))
			for _, col := range cols {
				if seen[col] {
					return nil, fmt.Errorf("compactfmt: line %d: section %q has duplicate column %q", i+1, current.Name, col)
				}
				seen[col] = true
			}
			current.Columns = cols
		case strings.HasPrefix(line, "@"):
			return nil, fmt.Errorf("compactfmt: line %d: unknown directive %q", i+1, line)
		default:
			if current == nil || current.Columns == nil {
				return nil, fmt.Errorf("compactfmt: line %d: row before a section header", i+1)
			}
			cells, err := splitRow(line)
			if err != nil {
				return nil, fmt.Errorf("compactfmt: line %d: %w", i+1, err)
			}
			if len(cells) != len(current.Columns) {
				return nil, fmt.Errorf("compactfmt: line %d: row has %d cells, section %q has %d columns",
					i+1, len(cells), current.Name, len(current.Columns))
			}
			current.Rows = append(current.Rows, cells)
		}
	}
	for _, section := range out.Sections {
		if section.Columns == nil {
			return nil, fmt.Errorf("compactfmt: section %q has no @columns directive", section.Name)
		}
	}
	if len(out.Sections) == 0 {
		return nil, fmt.Errorf("compactfmt: document has no sections")
	}
	return out, nil
}

func directiveValue(line, prefix string) (string, bool) {
	if !strings.HasPrefix(line, prefix) {
		return "", false
	}
	return strings.TrimPrefix(line, prefix), true
}

func splitRow(line string) ([]string, error) {
	parts := strings.Split(line, Separator)
	out := make([]string, 0, len(parts))
	for _, part := range parts {
		value, err := unescape(part)
		if err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, nil
}

// unescape reverses writeEscaped. An unknown escape is an error: accepting it as
// a literal would make two different byte sequences decode to the same value and
// break the reversibility the format promises.
func unescape(s string) (string, error) {
	if !strings.ContainsRune(s, '\\') {
		return s, nil
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		if s[i] != '\\' {
			b.WriteByte(s[i])
			continue
		}
		i++
		if i >= len(s) {
			return "", fmt.Errorf("truncated escape at end of field")
		}
		switch s[i] {
		case '\\':
			b.WriteByte('\\')
		case 't':
			b.WriteByte('\t')
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case '@':
			b.WriteByte('@')
		default:
			return "", fmt.Errorf("unknown escape %q", `\`+string(s[i]))
		}
	}
	return b.String(), nil
}
