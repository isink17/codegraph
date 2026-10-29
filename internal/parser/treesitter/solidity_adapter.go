//go:build cgo

package treesitter

import (
	"context"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/parser/treesitter/soliditygrammar"
	sitter "github.com/smacker/go-tree-sitter"
)

type SolidityAdapter struct{}

func NewSolidity() *SolidityAdapter { return &SolidityAdapter{} }

func (a *SolidityAdapter) Language() string { return "solidity" }

func (a *SolidityAdapter) Extensions() []string { return []string{".sol"} }

func (a *SolidityAdapter) Supports(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".sol")
}

func (a *SolidityAdapter) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	parser := sitter.NewParser()
	defer parser.Close()
	parser.SetLanguage(soliditygrammar.GetLanguage())
	tree, err := parser.ParseCtx(ctx, nil, content)
	if err != nil {
		return graph.ParsedFile{}, err
	}
	defer tree.Close()
	root := tree.RootNode()

	parsed := graph.ParsedFile{Language: "solidity", FileTokens: computeFileTokens(content)}
	var candidates []graph.Symbol
	solidityCollectDeclarations(root, "", content, &candidates)
	parsed.Symbols = solidityUniqueSymbols(candidates)
	parsed.References = solidityReferences(root, content)
	return parsed, nil
}

func solidityCollectDeclarations(scope *sitter.Node, container string, content []byte, out *[]graph.Symbol) {
	for i := range int(scope.NamedChildCount()) {
		node := scope.NamedChild(i)
		switch node.Type() {
		case "contract_declaration", "interface_declaration", "library_declaration":
			name := nodeText(childByFieldName(node, "name"), content)
			if name == "" {
				continue
			}
			kind, signature := "class", "contract"
			if node.Type() == "interface_declaration" {
				kind, signature = "interface", "interface"
			} else if node.Type() == "library_declaration" {
				signature = "library"
			} else if node.Type() == "contract_declaration" && solidityHasChild(node, "abstract") {
				signature = "abstract contract"
			}
			qualified := solidityQualified(container, name)
			*out = append(*out, graph.Symbol{
				Language: "solidity", Kind: kind, Name: name, QualifiedName: qualified,
				ContainerName: container, Signature: signature,
				Range: nodeRange(node), StableKey: "solidity:" + kind + ":" + qualified + ":" + signature,
			})
			if body := childByFieldName(node, "body"); body != nil {
				solidityCollectDeclarations(body, qualified, content, out)
			}
		case "function_definition", "constructor_definition", "modifier_definition", "event_definition", "error_declaration", "struct_declaration", "enum_declaration", "user_defined_type_definition":
			if symbol, ok := solidityDeclaration(node, container, content); ok {
				*out = append(*out, symbol)
			}
		}
	}
}

func solidityDeclaration(node *sitter.Node, container string, content []byte) (graph.Symbol, bool) {
	name := nodeText(childByFieldName(node, "name"), content)
	kind := node.Type()
	params := []string(nil)
	switch kind {
	case "function_definition", "modifier_definition", "event_definition", "error_declaration":
		if name == "" {
			return graph.Symbol{}, false
		}
		var ok bool
		params, ok = solidityParameterTypes(node, content)
		if !ok {
			return graph.Symbol{}, false
		}
	case "constructor_definition":
		name = "constructor"
		var ok bool
		params, ok = solidityParameterTypes(node, content)
		if !ok {
			return graph.Symbol{}, false
		}
	case "struct_declaration", "enum_declaration":
		if name == "" {
			return graph.Symbol{}, false
		}
	case "user_defined_type_definition":
		if name == "" {
			return graph.Symbol{}, false
		}
	}
	if name == "" {
		return graph.Symbol{}, false
	}
	qualified := solidityQualified(container, name)
	parameterList := strings.Join(params, ",")
	signature := ""
	if kind == "function_definition" || kind == "modifier_definition" || kind == "event_definition" || kind == "error_declaration" || kind == "constructor_definition" {
		signature = name + "(" + parameterList + ")"
	}
	symbolKind := map[string]string{
		"function_definition": "function", "constructor_definition": "constructor",
		"modifier_definition": "modifier", "event_definition": "event", "error_declaration": "error",
		"struct_declaration": "struct", "enum_declaration": "enum", "user_defined_type_definition": "type",
	}[kind]
	if symbolKind == "" {
		return graph.Symbol{}, false
	}
	if kind == "user_defined_type_definition" {
		if underlying := firstChild(node, "primitive_type"); underlying != nil && !underlying.HasError() {
			signature = "is " + solidityCanonicalType(underlying, content)
		}
	}
	var visibility string
	for i := range int(node.NamedChildCount()) {
		child := node.NamedChild(i)
		if child.Type() == "visibility" {
			visibility = nodeText(child, content)
			break
		}
	}
	stableKey := "solidity:" + symbolKind + ":" + qualified
	if signature != "" {
		stableKey += ":" + signature
	}
	return graph.Symbol{
		Language: "solidity", Kind: symbolKind, Name: name, QualifiedName: qualified,
		ContainerName: container, Signature: signature, Visibility: visibility,
		Range: nodeRange(node), StableKey: stableKey,
	}, true
}

func solidityParameterTypes(node *sitter.Node, content []byte) ([]string, bool) {
	var out []string
	for i := range int(node.NamedChildCount()) {
		parameter := node.NamedChild(i)
		switch parameter.Type() {
		case "parameter", "event_parameter", "error_parameter":
			typeNode := childByFieldName(parameter, "type")
			if typeNode == nil || typeNode.HasError() {
				return nil, false
			}
			typ := solidityCanonicalType(typeNode, content)
			if typ == "" {
				return nil, false
			}
			out = append(out, typ)
		}
	}
	return out, true
}

func solidityCanonicalType(node *sitter.Node, content []byte) string {
	var out strings.Builder
	var walk func(*sitter.Node)
	walk = func(current *sitter.Node) {
		if current == nil || current.IsExtra() || current.IsMissing() || current.IsError() {
			return
		}
		if current.ChildCount() == 0 {
			out.WriteString(nodeText(current, content))
			return
		}
		for i := range int(current.ChildCount()) {
			walk(current.Child(i))
		}
	}
	walk(node)
	return out.String()
}

func solidityReferences(root *sitter.Node, content []byte) []graph.Reference {
	var references []graph.Reference
	for _, node := range findDescendants(root, "import_directive") {
		if node.HasError() {
			continue
		}
		source := childByFieldName(node, "source")
		if source == nil {
			continue
		}
		if path, ok := solidityStringLiteral(nodeText(source, content)); ok && path != "" {
			references = append(references, graph.Reference{Kind: "import", Name: path, QualifiedName: path, Range: nodeRange(source)})
		}
	}
	for _, node := range findDescendants(root, "call_expression") {
		if node.HasError() {
			continue
		}
		function := childByFieldName(node, "function")
		name := solidityCallName(function, content)
		qualified := strings.TrimSpace(nodeText(function, content))
		if name != "" && qualified != "" {
			references = append(references, graph.Reference{Kind: "call", Name: name, QualifiedName: qualified, Range: nodeRange(node)})
		}
	}
	for _, node := range findDescendants(root, "modifier_invocation") {
		if node.HasError() {
			continue
		}
		nameNode := firstChild(node, "identifier")
		name := nodeText(nameNode, content)
		if name != "" {
			references = append(references, graph.Reference{Kind: "modifier_invocation", Name: name, QualifiedName: nodeText(node, content), Range: nodeRange(node)})
		}
	}
	sort.Slice(references, func(i, j int) bool {
		a, b := references[i], references[j]
		if a.Range.StartLine != b.Range.StartLine {
			return a.Range.StartLine < b.Range.StartLine
		}
		if a.Range.StartCol != b.Range.StartCol {
			return a.Range.StartCol < b.Range.StartCol
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.QualifiedName < b.QualifiedName
	})
	return references
}

func solidityStringLiteral(literal string) (string, bool) {
	if len(literal) < 2 || (literal[0] != '\'' && literal[0] != '"') || literal[len(literal)-1] != literal[0] {
		return "", false
	}
	var decoded strings.Builder
	for i := 1; i < len(literal)-1; i++ {
		c := literal[i]
		if c == literal[0] || c == '\n' || c == '\r' {
			return "", false
		}
		if c != '\\' {
			if c < 0x20 {
				return "", false
			}
			decoded.WriteByte(c)
			continue
		}
		i++
		if i >= len(literal)-1 {
			return "", false
		}
		switch literal[i] {
		case 'n':
			decoded.WriteByte('\n')
		case 'r':
			decoded.WriteByte('\r')
		case 't':
			decoded.WriteByte('\t')
		case '\\', '\'', '"':
			decoded.WriteByte(literal[i])
		case 'x', 'u':
			n := 2
			if literal[i] == 'u' {
				n = 4
			}
			if i+n >= len(literal) {
				return "", false
			}
			value, err := strconv.ParseUint(literal[i+1:i+1+n], 16, 32)
			if err != nil || (n == 4 && value >= 0xd800 && value <= 0xdfff) {
				return "", false
			}
			if literal[i] == 'x' {
				decoded.WriteByte(byte(value))
			} else {
				decoded.WriteRune(rune(value))
			}
			i += n
		default:
			return "", false
		}
	}
	value := decoded.String()
	return value, utf8.ValidString(value)
}

func solidityCallName(node *sitter.Node, content []byte) string {
	if node == nil || node.IsMissing() || node.IsError() {
		return ""
	}
	switch node.Type() {
	case "identifier":
		return nodeText(node, content)
	case "user_defined_type":
		children := findDescendants(node, "identifier")
		if len(children) > 0 {
			return nodeText(children[len(children)-1], content)
		}
	case "expression":
		if node.NamedChildCount() == 1 {
			return solidityCallName(node.NamedChild(0), content)
		}
	case "member_expression":
		return nodeText(childByFieldName(node, "property"), content)
	case "call_expression":
		return solidityCallName(childByFieldName(node, "function"), content)
	case "array_access":
		return solidityCallName(childByFieldName(node, "base"), content)
	case "parenthesized_expression":
		return solidityCallName(node.NamedChild(0), content)
	case "new_expression":
		return solidityCallName(childByFieldName(node, "name"), content)
	case "type_name":
		children := findDescendants(node, "identifier")
		if len(children) > 0 {
			return nodeText(children[len(children)-1], content)
		}
	}
	return ""
}

func solidityQualified(container, name string) string {
	if container == "" {
		return name
	}
	return container + "." + name
}

func solidityHasChild(node *sitter.Node, kind string) bool {
	for i := range int(node.ChildCount()) {
		if node.Child(i).Type() == kind {
			return true
		}
	}
	return false
}

func solidityUniqueSymbols(candidates []graph.Symbol) []graph.Symbol {
	counts := make(map[string]int, len(candidates))
	for _, symbol := range candidates {
		counts[symbol.StableKey]++
	}
	out := make([]graph.Symbol, 0, len(candidates))
	for _, symbol := range candidates {
		if counts[symbol.StableKey] == 1 {
			out = append(out, symbol)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].StableKey != out[j].StableKey {
			return out[i].StableKey < out[j].StableKey
		}
		if out[i].Range.StartLine != out[j].Range.StartLine {
			return out[i].Range.StartLine < out[j].Range.StartLine
		}
		return out[i].Range.StartCol < out[j].Range.StartCol
	})
	return out
}
