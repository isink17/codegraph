package treesitter

import (
	"context"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	php "github.com/smacker/go-tree-sitter/php"

	"github.com/isink17/codegraph/internal/graph"
)

// PHPAdapter parses PHP source files using tree-sitter.
type PHPAdapter struct{}

func NewPHP() *PHPAdapter { return &PHPAdapter{} }

func (a *PHPAdapter) Language() string     { return "php" }
func (a *PHPAdapter) Extensions() []string { return []string{".php"} }

func (a *PHPAdapter) Supports(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".php")
}

func (a *PHPAdapter) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	root, err := parse(ctx, php.GetLanguage(), content)
	if err != nil {
		return graph.ParsedFile{}, err
	}

	module := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	pf := graph.ParsedFile{
		Language:   "php",
		FileTokens: computeFileTokens(content),
	}

	phpExtractImports(root, content, &pf)
	phpExtractSymbols(root, module, "", content, &pf)
	phpExtractCalls(root, content, &pf)
	linkTestsGeneric(module, &pf)
	return pf, nil
}

func phpExtractImports(root *sitter.Node, content []byte, pf *graph.ParsedFile) {
	// use statements
	for _, use := range findDescendants(root, "namespace_use_declaration") {
		for _, clause := range findDescendants(use, "namespace_use_clause") {
			nameNode := childByFieldName(clause, "name")
			if nameNode == nil {
				nameNode = firstChild(clause, "qualified_name")
			}
			if nameNode != nil {
				pf.Imports = append(pf.Imports, nodeText(nameNode, content))
			}
		}
	}
	// require/require_once/include/include_once
	for _, call := range findDescendants(root, "include_expression") {
		arg := call.Child(int(call.ChildCount()) - 1)
		if arg != nil {
			val := strings.Trim(nodeText(arg, content), `"'`)
			if val != "" {
				pf.Imports = append(pf.Imports, val)
			}
		}
	}
}

func phpExtractSymbols(node *sitter.Node, module, container string, content []byte, pf *graph.ParsedFile) {
	for i := range int(node.ChildCount()) {
		child := node.Child(i)
		switch child.Type() {
		case "class_declaration", "interface_declaration", "trait_declaration", "enum_declaration":
			phpAddType(child, module, content, pf)
		case "function_definition":
			phpAddFunction(child, module, container, content, pf)
		case "method_declaration":
			phpAddFunction(child, module, container, content, pf)
		case "program":
			phpExtractSymbols(child, module, container, content, pf)
		case "namespace_definition":
			body := childByFieldName(child, "body")
			if body != nil {
				phpExtractSymbols(body, module, container, content, pf)
			}
		case "declaration_list":
			phpExtractSymbols(child, module, container, content, pf)
		}
	}
}

func phpAddType(node *sitter.Node, module string, content []byte, pf *graph.ParsedFile) {
	nameNode := childByFieldName(node, "name")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, content)

	pf.Symbols = append(pf.Symbols, graph.Symbol{
		Language:      "php",
		Kind:          "type",
		Name:          name,
		QualifiedName: module + "." + name,
		ContainerName: module,
		Visibility:    heuristicVisibility(name),
		Range:         nodeRange(node),
		DocSummary:    prevCommentText(node, content),
		StableKey:     "type:php:" + module + ":" + name,
	})

	body := childByFieldName(node, "body")
	if body != nil {
		phpExtractSymbols(body, module, name, content, pf)
	}
}

func phpAddFunction(node *sitter.Node, module, container string, content []byte, pf *graph.ParsedFile) {
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

	vis := phpMethodVisibility(node, content)

	pf.Symbols = append(pf.Symbols, graph.Symbol{
		Language:      "php",
		Kind:          "function",
		Name:          name,
		QualifiedName: qualified,
		ContainerName: effectiveContainer,
		Visibility:    vis,
		Range:         nodeRange(node),
		DocSummary:    prevCommentText(node, content),
		StableKey:     "func:php:" + module + ":" + name,
	})
}

func phpMethodVisibility(node *sitter.Node, content []byte) string {
	for i := 0; i < int(node.ChildCount()); i++ {
		c := node.Child(i)
		if c.Type() == "visibility_modifier" {
			text := nodeText(c, content)
			switch text {
			case "public":
				return "public"
			case "private":
				return "private"
			case "protected":
				return "protected"
			}
		}
	}
	return "public" // PHP defaults to public
}

func phpExtractCalls(root *sitter.Node, content []byte, pf *graph.ParsedFile) {
	for _, call := range findDescendants(root, "function_call_expression") {
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
	// Member call expressions
	for _, call := range findDescendants(root, "member_call_expression") {
		nameNode := childByFieldName(call, "name")
		if nameNode == nil {
			continue
		}
		name := nodeText(nameNode, content)
		obj := childByFieldName(call, "object")
		fullName := name
		if obj != nil {
			fullName = nodeText(obj, content) + "->" + name
		}
		line := int(call.StartPoint().Row) + 1
		pf.Edges = append(pf.Edges, graph.Edge{
			SrcSymbolID: 0,
			DstName:     fullName,
			Kind:        "calls",
			Evidence:    fullName,
			Line:        line,
		})
		pf.References = append(pf.References, graph.Reference{
			Kind:          "call",
			Name:          fullName,
			QualifiedName: fullName,
			Range:         nodeRange(call),
		})
	}
}
