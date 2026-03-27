//go:build cgo

package treesitter

import (
	"context"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	csharp "github.com/smacker/go-tree-sitter/csharp"

	"github.com/isink17/codegraph/internal/graph"
)

// CSharpAdapter parses C# source files using tree-sitter.
type CSharpAdapter struct{}

func NewCSharp() *CSharpAdapter { return &CSharpAdapter{} }

func (a *CSharpAdapter) Language() string     { return "csharp" }
func (a *CSharpAdapter) Extensions() []string { return []string{".cs"} }

func (a *CSharpAdapter) Supports(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".cs")
}

func (a *CSharpAdapter) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	root, err := parse(ctx, csharp.GetLanguage(), content)
	if err != nil {
		return graph.ParsedFile{}, err
	}

	module := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	pf := graph.ParsedFile{
		Language:   "csharp",
		FileTokens: computeFileTokens(content),
	}

	csExtractImports(root, content, &pf)
	csExtractSymbols(root, module, "", content, &pf)
	csExtractCalls(root, content, &pf)
	linkTestsGeneric(module, &pf)
	return pf, nil
}

func csExtractImports(root *sitter.Node, content []byte, pf *graph.ParsedFile) {
	for _, dir := range findDescendants(root, "using_directive") {
		nameNode := childByFieldName(dir, "name")
		if nameNode == nil {
			// Fallback: last child that looks like a qualified name
			nameNode = firstChild(dir, "qualified_name")
		}
		if nameNode == nil {
			nameNode = firstChild(dir, "identifier_name")
		}
		if nameNode != nil {
			pf.Imports = append(pf.Imports, nodeText(nameNode, content))
		}
	}
}

func csExtractSymbols(node *sitter.Node, module, container string, content []byte, pf *graph.ParsedFile) {
	for i := range int(node.ChildCount()) {
		child := node.Child(i)
		switch child.Type() {
		case "class_declaration", "interface_declaration", "struct_declaration",
			"enum_declaration", "record_declaration":
			csAddType(child, module, content, pf)
		case "method_declaration", "constructor_declaration":
			csAddMethod(child, module, container, content, pf)
		case "namespace_declaration":
			body := childByFieldName(child, "body")
			if body != nil {
				csExtractSymbols(body, module, container, content, pf)
			}
		case "declaration_list":
			csExtractSymbols(child, module, container, content, pf)
		}
	}
}

func csAddType(node *sitter.Node, module string, content []byte, pf *graph.ParsedFile) {
	nameNode := childByFieldName(node, "name")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, content)

	pf.Symbols = append(pf.Symbols, graph.Symbol{
		Language:      "csharp",
		Kind:          "type",
		Name:          name,
		QualifiedName: module + "." + name,
		ContainerName: module,
		Visibility:    heuristicVisibility(name),
		Range:         nodeRange(node),
		DocSummary:    prevCommentText(node, content),
		StableKey:     "type:csharp:" + module + ":" + name,
	})

	body := childByFieldName(node, "body")
	if body != nil {
		csExtractSymbols(body, module, name, content, pf)
	}
}

func csAddMethod(node *sitter.Node, module, container string, content []byte, pf *graph.ParsedFile) {
	nameNode := childByFieldName(node, "name")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, content)
	effectiveContainer := module
	if container != "" {
		effectiveContainer = container
	}
	qualified := module + "." + name
	if container != "" && container != module {
		qualified = module + "." + container + "." + name
	}

	vis := csMethodVisibility(node, content)

	pf.Symbols = append(pf.Symbols, graph.Symbol{
		Language:      "csharp",
		Kind:          "function",
		Name:          name,
		QualifiedName: qualified,
		ContainerName: effectiveContainer,
		Visibility:    vis,
		Range:         nodeRange(node),
		DocSummary:    prevCommentText(node, content),
		StableKey:     "func:csharp:" + module + ":" + name,
	})
}

func csMethodVisibility(node *sitter.Node, content []byte) string {
	for i := 0; i < int(node.ChildCount()); i++ {
		c := node.Child(i)
		if c.Type() == "modifier" || c.Type() == "access_modifier" {
			text := nodeText(c, content)
			switch text {
			case "public":
				return "public"
			case "private":
				return "private"
			case "protected":
				return "protected"
			case "internal":
				return "module"
			}
		}
	}
	return "private"
}

func csExtractCalls(root *sitter.Node, content []byte, pf *graph.ParsedFile) {
	for _, call := range findDescendants(root, "invocation_expression") {
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
