//go:build cgo

package treesitter

import (
	"context"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	kotlin "github.com/smacker/go-tree-sitter/kotlin"

	"github.com/isink17/codegraph/internal/graph"
)

// KotlinAdapter parses Kotlin source files using tree-sitter.
type KotlinAdapter struct{}

func NewKotlin() *KotlinAdapter { return &KotlinAdapter{} }

func (a *KotlinAdapter) Language() string     { return "kotlin" }
func (a *KotlinAdapter) Extensions() []string { return []string{".kt", ".kts"} }

func (a *KotlinAdapter) Supports(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".kt" || ext == ".kts"
}

func (a *KotlinAdapter) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	root, err := parse(ctx, kotlin.GetLanguage(), content)
	if err != nil {
		return graph.ParsedFile{}, err
	}

	module := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	pf := graph.ParsedFile{
		Language:   "kotlin",
		FileTokens: computeFileTokens(content),
	}

	kotlinExtractImports(root, content, &pf)
	kotlinExtractSymbols(root, module, "", content, &pf)
	kotlinExtractCalls(root, content, &pf)
	linkTestsGeneric(module, &pf)
	return pf, nil
}

func kotlinExtractImports(root *sitter.Node, content []byte, pf *graph.ParsedFile) {
	for _, imp := range findDescendants(root, "import_header") {
		ident := childByFieldName(imp, "identifier")
		if ident != nil {
			pf.Imports = append(pf.Imports, nodeText(ident, content))
		}
	}
}

func kotlinExtractSymbols(node *sitter.Node, module, container string, content []byte, pf *graph.ParsedFile) {
	for i := range int(node.ChildCount()) {
		child := node.Child(i)
		switch child.Type() {
		case "class_declaration":
			kotlinAddType(child, module, "class", content, pf)
		case "object_declaration":
			kotlinAddType(child, module, "object", content, pf)
		case "interface_declaration":
			kotlinAddType(child, module, "interface", content, pf)
		case "function_declaration":
			kotlinAddFunction(child, module, container, content, pf)
		}
	}
}

func kotlinAddType(node *sitter.Node, module, kind string, content []byte, pf *graph.ParsedFile) {
	nameNode := childByFieldName(node, "name")
	if nameNode == nil {
		nameNode = firstChild(node, "type_identifier")
	}
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, content)

	pf.Symbols = append(pf.Symbols, graph.Symbol{
		Language:      "kotlin",
		Kind:          kind,
		Name:          name,
		QualifiedName: module + "." + name,
		ContainerName: module,
		Visibility:    heuristicVisibility(name),
		Range:         nodeRange(node),
		DocSummary:    prevCommentText(node, content),
		StableKey:     "type:kotlin:" + module + ":" + name,
	})

	body := childByFieldName(node, "body")
	if body == nil {
		body = firstChild(node, "class_body")
	}
	if body != nil {
		kotlinExtractSymbols(body, module, name, content, pf)
	}
}

func kotlinAddFunction(node *sitter.Node, module, container string, content []byte, pf *graph.ParsedFile) {
	nameNode := childByFieldName(node, "name")
	if nameNode == nil {
		nameNode = firstChild(node, "simple_identifier")
	}
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
		Language:      "kotlin",
		Kind:          "function",
		Name:          name,
		QualifiedName: qualified,
		ContainerName: effectiveContainer,
		Visibility:    heuristicVisibility(name),
		Range:         nodeRange(node),
		DocSummary:    prevCommentText(node, content),
		StableKey:     "func:kotlin:" + module + ":" + name,
	})
}

func kotlinExtractCalls(root *sitter.Node, content []byte, pf *graph.ParsedFile) {
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
