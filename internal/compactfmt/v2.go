package compactfmt

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/isink17/codegraph/internal/limits"
)

// ColumnType declares interpretation, never inferred from a cell's contents.
type ColumnType string

const (
	Scalar      ColumnType = "scalar"
	StringArray ColumnType = "string[]"
	// MaxV2Bytes bounds a typed document before splitting or JSON decoding.
	MaxV2Bytes    = 16 << 20
	maxV2Rows     = limits.StoreMaxRows
	maxV2Sections = limits.MaxPage
)

// Strings is a v2-only array cell. Nil normalizes to the empty array.
func Strings(values []string) Cell { return Cell{kind: cellStrings, list: values} }

func columnTypes(s Schema) ([]ColumnType, error) {
	if len(s.Columns) > limits.MaxPage {
		return nil, fmt.Errorf("compactfmt: too many columns")
	}
	if len(s.Types) != 0 && len(s.Types) != len(s.Columns) {
		return nil, fmt.Errorf("compactfmt: type count does not match columns")
	}
	types := append([]ColumnType(nil), s.Types...)
	if len(types) == 0 {
		types = make([]ColumnType, len(s.Columns))
		for i := range types {
			types[i] = Scalar
		}
	}
	if len(types) != len(s.Columns) {
		return nil, fmt.Errorf("compactfmt: type count does not match columns")
	}
	namesBytes := 0
	for _, name := range append([]string{s.Section}, s.Columns...) {
		if !utf8.ValidString(name) {
			return nil, fmt.Errorf("compactfmt: invalid UTF-8 name")
		}
		namesBytes += len(name)
	}
	if namesBytes > MaxV2Bytes/6 {
		return nil, fmt.Errorf("compactfmt: schema exceeds byte limit")
	}
	for _, kind := range types {
		if kind != Scalar && kind != StringArray {
			return nil, fmt.Errorf("compactfmt: invalid column type %q", kind)
		}
	}
	return types, nil
}

func validateTypedCell(c Cell, kind ColumnType) error {
	if (kind == StringArray) != (c.kind == cellStrings) {
		return fmt.Errorf("compactfmt: cell does not match declared type %s", kind)
	}
	values := []string{c.str}
	if kind == StringArray {
		values = c.list
		if len(values) > limits.MaxBatchItems {
			return fmt.Errorf("compactfmt: array exceeds item limit")
		}
	}
	size := 0
	for _, v := range values {
		if !utf8.ValidString(v) {
			return fmt.Errorf("compactfmt: invalid UTF-8 cell")
		}
		size += len(v)
		if size > MaxV2Bytes/6 {
			return fmt.Errorf("compactfmt: cell exceeds byte limit")
		}
	}
	return nil
}

func arrayJSON(values []string) string {
	if values == nil {
		values = []string{}
	}
	data, _ := json.Marshal(values) // Strings are validated before encoding.
	return string(data)
}

// TypedCell contains exactly one scalar or string array, as declared by Type.
// Scalar text is already unescaped; Array elements are already JSON-decoded.
// Consumers must not unescape either a second time.
type TypedCell struct {
	Type   ColumnType
	Scalar string
	Array  []string
}
type TypedSection struct {
	Name    string
	Columns []string
	Types   []ColumnType
	Rows    [][]TypedCell
}
type TypedDocument struct {
	Version  string
	Tool     string
	Sections []TypedSection
}

func (d *TypedDocument) Section(name string) *TypedSection {
	for i := range d.Sections {
		if d.Sections[i].Name == name {
			return &d.Sections[i]
		}
	}
	return nil
}
func (s *TypedSection) Cell(row int, column string) (TypedCell, bool) {
	if row < 0 || row >= len(s.Rows) {
		return TypedCell{}, false
	}
	for i, c := range s.Columns {
		if c == column {
			return s.Rows[row][i], true
		}
	}
	return TypedCell{}, false
}

// DecodeV2 is a dedicated, bounded typed decoder. Decode remains v1-only.
// Array payloads must be canonical encoding/json string arrays; null, null
// elements, invalid Unicode and noncanonical spellings are rejected.
func DecodeV2(text string) (*TypedDocument, error) {
	if len(text) > MaxV2Bytes {
		return nil, fmt.Errorf("compactfmt: document exceeds byte limit")
	}
	if !utf8.ValidString(text) {
		return nil, fmt.Errorf("compactfmt: invalid UTF-8 document")
	}
	if !strings.HasPrefix(text, "@"+VersionV2+"\n") {
		return nil, fmt.Errorf("compactfmt: expected %s", VersionV2)
	}
	if !strings.HasSuffix(text, "\n") {
		return nil, fmt.Errorf("compactfmt: missing final LF")
	}
	if strings.Count(text, "\n") > maxV2Rows+3*maxV2Sections+2 {
		return nil, fmt.Errorf("compactfmt: too many lines")
	}
	lines := strings.Split(text, "\n")
	// Validate/remove only the v2 type directives, then reuse the v1 structural
	// parser and cell unescaping once. No change to the v1 decoder's acceptance.
	plain := []string{"@" + Version}
	var types [][]ColumnType
	rows, sections := 0, 0
	for i := 1; i < len(lines); i++ {
		line := lines[i]
		if strings.HasPrefix(line, "@section ") {
			sections++
			if sections > maxV2Sections {
				return nil, fmt.Errorf("compactfmt: too many sections")
			}
		}
		plain = append(plain, line)
		if strings.HasPrefix(line, "@columns ") {
			if strings.Count(line, Separator) >= limits.MaxPage {
				return nil, fmt.Errorf("compactfmt: too many columns")
			}
			cols, err := splitRow(strings.TrimPrefix(line, "@columns "))
			if err != nil {
				return nil, err
			}
			if i+1 >= len(lines) || !strings.HasPrefix(lines[i+1], "@types ") {
				return nil, fmt.Errorf("compactfmt: missing @types after @columns")
			}
			i++
			if strings.Count(lines[i], Separator) >= limits.MaxPage {
				return nil, fmt.Errorf("compactfmt: too many types")
			}
			var declared []ColumnType
			for _, kind := range strings.Split(strings.TrimPrefix(lines[i], "@types "), Separator) {
				declared = append(declared, ColumnType(kind))
			}
			kinds, err := columnTypes(Schema{Section: "decoded", Columns: cols, Types: declared})
			if err != nil {
				return nil, err
			}
			types = append(types, kinds)
		} else if i > 1 && !strings.HasPrefix(line, "@") && i < len(lines)-1 {
			rows++
			if rows > maxV2Rows {
				return nil, fmt.Errorf("compactfmt: too many rows")
			}
		}
	}
	decoded, err := Decode(strings.Join(plain, "\n"))
	if err != nil {
		return nil, err
	}
	if len(types) != len(decoded.Sections) {
		return nil, fmt.Errorf("compactfmt: missing types")
	}
	if len(decoded.Tool) > MaxV2Bytes/6 {
		return nil, fmt.Errorf("compactfmt: tool name exceeds byte limit")
	}
	out := &TypedDocument{Version: VersionV2, Tool: decoded.Tool}
	for i, section := range decoded.Sections {
		if _, err := columnTypes(Schema{Section: section.Name, Columns: section.Columns, Types: types[i]}); err != nil {
			return nil, err
		}
		typed := TypedSection{Name: section.Name, Columns: section.Columns, Types: types[i]}
		for _, row := range section.Rows {
			cells := make([]TypedCell, len(row))
			for j, value := range row {
				cells[j].Type = types[i][j]
				if cells[j].Type == Scalar {
					if err := validateTypedCell(Str(value), Scalar); err != nil {
						return nil, err
					}
					cells[j].Scalar = value
					continue
				}
				if len(value) == 0 || value[0] != '[' {
					return nil, fmt.Errorf("compactfmt: expected string array")
				}
				dec := json.NewDecoder(strings.NewReader(value))
				if _, err := dec.Token(); err != nil {
					return nil, err
				}
				array := []string{}
				for dec.More() {
					if len(array) >= limits.MaxBatchItems {
						return nil, fmt.Errorf("compactfmt: array exceeds item limit")
					}
					token, err := dec.Token()
					if err != nil {
						return nil, err
					}
					element, ok := token.(string)
					if !ok {
						return nil, fmt.Errorf("compactfmt: array element is not a string")
					}
					array = append(array, element)
				}
				if token, err := dec.Token(); err != nil || token != json.Delim(']') {
					return nil, fmt.Errorf("compactfmt: invalid array terminator")
				}
				if arrayJSON(array) != value {
					return nil, fmt.Errorf("compactfmt: noncanonical string array")
				}

				if err := validateTypedCell(Strings(array), StringArray); err != nil {
					return nil, err
				}
				cells[j].Array = array
			}
			typed.Rows = append(typed.Rows, cells)
		}
		out.Sections = append(out.Sections, typed)
	}
	return out, nil
}
