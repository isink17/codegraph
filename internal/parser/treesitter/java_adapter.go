package treesitter

import (
	"context"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	java "github.com/smacker/go-tree-sitter/java"

	"github.com/isink17/codegraph/internal/graph"
)

// JavaAdapter parses Java source files using tree-sitter.
type JavaAdapter struct{}

func NewJava() *JavaAdapter { return &JavaAdapter{} }

func (a *JavaAdapter) Language() string     { return "java" }
func (a *JavaAdapter) Extensions() []string { return []string{".java"} }

func (a *JavaAdapter) Supports(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".java")
}

func (a *JavaAdapter) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	root, err := parse(ctx, java.GetLanguage(), content)
	if err != nil {
		return graph.ParsedFile{}, err
	}

	module := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	pf := graph.ParsedFile{
		Language:   "java",
		FileTokens: computeFileTokens(content),
	}

	javaExtractImports(root, content, &pf)
	javaExtractSymbols(root, module, "", content, &pf)
	javaExtractCalls(root, content, &pf)
	linkTestsGeneric(module, &pf)
	return pf, nil
}

func javaExtractImports(root *sitter.Node, content []byte, pf *graph.ParsedFile) {
	for _, imp := range findDescendants(root, "import_declaration") {
		// The scoped_identifier child holds the full import path.
		scoped := firstChild(imp, "scoped_identifier")
		if scoped != nil {
			pf.Imports = append(pf.Imports, nodeText(scoped, content))
		}
	}
}

func javaExtractSymbols(node *sitter.Node, module, container string, content []byte, pf *graph.ParsedFile) {
	for i := range int(node.ChildCount()) {
		child := node.Child(i)
		switch child.Type() {
		case "class_declaration", "interface_declaration", "enum_declaration", "record_declaration":
			javaAddType(child, module, content, pf)
		case "method_declaration", "constructor_declaration":
			javaAddMethod(child, module, container, content, pf)
		case "class_body", "interface_body", "enum_body":
			// Recurse into bodies — the container is set by the parent type.
			javaExtractSymbols(child, module, container, content, pf)
		}
	}
}

func javaAddType(node *sitter.Node, module string, content []byte, pf *graph.ParsedFile) {
	nameNode := childByFieldName(node, "name")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, content)

	pf.Symbols = append(pf.Symbols, graph.Symbol{
		Language:      "java",
		Kind:          "type",
		Name:          name,
		QualifiedName: module + "." + name,
		ContainerName: module,
		Visibility:    heuristicVisibility(name),
		Range:         nodeRange(node),
		DocSummary:    prevCommentText(node, content),
		StableKey:     "type:java:" + module + ":" + name,
	})

	// Recurse into the body with this type as container.
	body := childByFieldName(node, "body")
	if body != nil {
		javaExtractSymbols(body, module, name, content, pf)
	}
}

func javaAddMethod(node *sitter.Node, module, container string, content []byte, pf *graph.ParsedFile) {
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
	stableKey := "func:java:" + module + ":" + name

	vis := javaMethodVisibility(node, content)

	pf.Symbols = append(pf.Symbols, graph.Symbol{
		Language:      "java",
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

func javaMethodVisibility(node *sitter.Node, content []byte) string {
	for _, mod := range findChildren(node, "modifiers") {
		text := nodeText(mod, content)
		if strings.Contains(text, "public") {
			return "public"
		}
		if strings.Contains(text, "private") {
			return "private"
		}
		if strings.Contains(text, "protected") {
			return "protected"
		}
	}
	return "package"
}

func javaExtractCalls(root *sitter.Node, content []byte, pf *graph.ParsedFile) {
	for _, call := range findDescendants(root, "method_invocation") {
		nameNode := childByFieldName(call, "name")
		if nameNode == nil {
			continue
		}
		name := nodeText(nameNode, content)
		obj := childByFieldName(call, "object")
		fullName := name
		if obj != nil {
			fullName = nodeText(obj, content) + "." + name
		}
		line := int(call.StartPoint().Row) + 1
		pf.Edges = append(pf.Edges, graph.Edge{
			SrcSymbolID: 0,
			DstName:     fullName,
			Kind:        "calls",
			Evidence:    nodeText(call, content),
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
