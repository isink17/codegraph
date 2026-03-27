package treesitter

import (
	"context"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	rust "github.com/smacker/go-tree-sitter/rust"

	"github.com/isink17/codegraph/internal/graph"
)

// RustAdapter parses Rust source files using tree-sitter.
type RustAdapter struct{}

func NewRust() *RustAdapter { return &RustAdapter{} }

func (a *RustAdapter) Language() string     { return "rust" }
func (a *RustAdapter) Extensions() []string { return []string{".rs"} }

func (a *RustAdapter) Supports(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".rs")
}

func (a *RustAdapter) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	root, err := parse(ctx, rust.GetLanguage(), content)
	if err != nil {
		return graph.ParsedFile{}, err
	}

	module := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	pf := graph.ParsedFile{
		Language:   "rust",
		FileTokens: computeFileTokens(content),
	}

	rustExtractImports(root, content, &pf)
	rustExtractSymbols(root, module, "", content, &pf)
	rustExtractCalls(root, content, &pf)
	linkTestsGeneric(module, &pf)
	return pf, nil
}

func rustExtractImports(root *sitter.Node, content []byte, pf *graph.ParsedFile) {
	for _, use := range findDescendants(root, "use_declaration") {
		arg := childByFieldName(use, "argument")
		if arg != nil {
			pf.Imports = append(pf.Imports, nodeText(arg, content))
		}
	}
}

func rustExtractSymbols(node *sitter.Node, module, container string, content []byte, pf *graph.ParsedFile) {
	for i := range int(node.ChildCount()) {
		child := node.Child(i)
		switch child.Type() {
		case "function_item":
			rustAddFunction(child, module, container, content, pf)
		case "struct_item":
			rustAddType(child, module, "struct", content, pf)
		case "enum_item":
			rustAddType(child, module, "enum", content, pf)
		case "trait_item":
			rustAddType(child, module, "trait", content, pf)
		case "impl_item":
			rustExtractImpl(child, module, content, pf)
		}
	}
}

func rustAddFunction(node *sitter.Node, module, container string, content []byte, pf *graph.ParsedFile) {
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
	stableKey := "func:rust:" + module + ":" + name

	vis := "module"
	if rustHasVisibilityModifier(node, content) {
		vis = "public"
	}

	pf.Symbols = append(pf.Symbols, graph.Symbol{
		Language:      "rust",
		Kind:          "function",
		Name:          name,
		QualifiedName: qualified,
		ContainerName: effectiveContainer,
		Visibility:    vis,
		Range:         nodeRange(node),
		DocSummary:    prevCommentText(node, content),
		StableKey:     stableKey,
	})
}

func rustAddType(node *sitter.Node, module, kind string, content []byte, pf *graph.ParsedFile) {
	nameNode := childByFieldName(node, "name")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, content)

	vis := "module"
	if rustHasVisibilityModifier(node, content) {
		vis = "public"
	}

	pf.Symbols = append(pf.Symbols, graph.Symbol{
		Language:      "rust",
		Kind:          kind,
		Name:          name,
		QualifiedName: module + "." + name,
		ContainerName: module,
		Visibility:    vis,
		Range:         nodeRange(node),
		DocSummary:    prevCommentText(node, content),
		StableKey:     "type:rust:" + module + ":" + name,
	})
}

func rustExtractImpl(node *sitter.Node, module string, content []byte, pf *graph.ParsedFile) {
	typeNode := childByFieldName(node, "type")
	if typeNode == nil {
		return
	}
	typeName := nodeText(typeNode, content)
	body := childByFieldName(node, "body")
	if body != nil {
		rustExtractSymbols(body, module, typeName, content, pf)
	}
}

func rustHasVisibilityModifier(node *sitter.Node, content []byte) bool {
	vis := firstChild(node, "visibility_modifier")
	if vis != nil {
		return strings.Contains(nodeText(vis, content), "pub")
	}
	return false
}

func rustExtractCalls(root *sitter.Node, content []byte, pf *graph.ParsedFile) {
	for _, call := range findDescendants(root, "call_expression") {
		fnNode := childByFieldName(call, "function")
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
