package treesitter

import (
	"context"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	swift "github.com/smacker/go-tree-sitter/swift"

	"github.com/isink17/codegraph/internal/graph"
)

// SwiftAdapter parses Swift source files using tree-sitter.
type SwiftAdapter struct{}

func NewSwift() *SwiftAdapter { return &SwiftAdapter{} }

func (a *SwiftAdapter) Language() string     { return "swift" }
func (a *SwiftAdapter) Extensions() []string { return []string{".swift"} }

func (a *SwiftAdapter) Supports(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".swift")
}

func (a *SwiftAdapter) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	root, err := parse(ctx, swift.GetLanguage(), content)
	if err != nil {
		return graph.ParsedFile{}, err
	}

	module := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	pf := graph.ParsedFile{
		Language:   "swift",
		FileTokens: computeFileTokens(content),
	}

	swiftExtractImports(root, content, &pf)
	swiftExtractSymbols(root, module, "", content, &pf)
	swiftExtractCalls(root, content, &pf)
	linkTestsGeneric(module, &pf)
	return pf, nil
}

func swiftExtractImports(root *sitter.Node, content []byte, pf *graph.ParsedFile) {
	for _, imp := range findDescendants(root, "import_declaration") {
		// The identifier child holds the module name.
		for i := range int(imp.ChildCount()) {
			child := imp.Child(i)
			if child.Type() == "identifier" {
				pf.Imports = append(pf.Imports, nodeText(child, content))
			}
		}
	}
}

func swiftExtractSymbols(node *sitter.Node, module, container string, content []byte, pf *graph.ParsedFile) {
	for i := range int(node.ChildCount()) {
		child := node.Child(i)
		switch child.Type() {
		case "class_declaration":
			swiftAddType(child, module, "class", content, pf)
		case "struct_declaration":
			swiftAddType(child, module, "struct", content, pf)
		case "enum_declaration":
			swiftAddType(child, module, "enum", content, pf)
		case "protocol_declaration":
			swiftAddType(child, module, "protocol", content, pf)
		case "actor_declaration":
			swiftAddType(child, module, "actor", content, pf)
		case "function_declaration":
			swiftAddFunction(child, module, container, content, pf)
		}
	}
}

func swiftAddType(node *sitter.Node, module, kind string, content []byte, pf *graph.ParsedFile) {
	nameNode := childByFieldName(node, "name")
	if nameNode == nil {
		// Fallback: look for a type_identifier child.
		nameNode = firstChild(node, "type_identifier")
	}
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, content)

	pf.Symbols = append(pf.Symbols, graph.Symbol{
		Language:      "swift",
		Kind:          kind,
		Name:          name,
		QualifiedName: module + "." + name,
		ContainerName: module,
		Visibility:    heuristicVisibility(name),
		Range:         nodeRange(node),
		DocSummary:    prevCommentText(node, content),
		StableKey:     "type:swift:" + module + ":" + name,
	})

	body := childByFieldName(node, "body")
	if body != nil {
		swiftExtractSymbols(body, module, name, content, pf)
	}
}

func swiftAddFunction(node *sitter.Node, module, container string, content []byte, pf *graph.ParsedFile) {
	nameNode := childByFieldName(node, "name")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, content)
	effectiveContainer := module
	if container != "" && container != module {
		effectiveContainer = container
	}
	qualified := module + "." + name
	if container != "" && container != module {
		qualified = module + "." + container + "." + name
	}

	pf.Symbols = append(pf.Symbols, graph.Symbol{
		Language:      "swift",
		Kind:          "function",
		Name:          name,
		QualifiedName: qualified,
		ContainerName: effectiveContainer,
		Visibility:    heuristicVisibility(name),
		Range:         nodeRange(node),
		DocSummary:    prevCommentText(node, content),
		StableKey:     "func:swift:" + module + ":" + name,
	})
}

func swiftExtractCalls(root *sitter.Node, content []byte, pf *graph.ParsedFile) {
	for _, call := range findDescendants(root, "call_expression") {
		fnNode := childByFieldName(call, "function")
		if fnNode == nil && call.ChildCount() > 0 {
			fnNode = call.Child(0)
		}
		if fnNode == nil {
			continue
		}
		name := nodeText(fnNode, content)
		if name == "" {
			continue
		}
		line := int(call.StartPoint().Row) + 1
		pf.Edges = append(pf.Edges, graph.Edge{
			SrcSymbolID: 0,
			DstName:     name,
			Kind:        "calls",
			Evidence:    name,
			Line:        line,
		})
		pf.References = append(pf.References, graph.Reference{
			Kind:          "call",
			Name:          name,
			QualifiedName: name,
			Range:         nodeRange(call),
		})
	}
}
