package treesitter

import (
	"context"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	ruby "github.com/smacker/go-tree-sitter/ruby"

	"github.com/isink17/codegraph/internal/graph"
)

// RubyAdapter parses Ruby source files using tree-sitter.
type RubyAdapter struct{}

func NewRuby() *RubyAdapter { return &RubyAdapter{} }

func (a *RubyAdapter) Language() string     { return "ruby" }
func (a *RubyAdapter) Extensions() []string { return []string{".rb"} }

func (a *RubyAdapter) Supports(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".rb")
}

func (a *RubyAdapter) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	root, err := parse(ctx, ruby.GetLanguage(), content)
	if err != nil {
		return graph.ParsedFile{}, err
	}

	module := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	pf := graph.ParsedFile{
		Language:   "ruby",
		FileTokens: computeFileTokens(content),
	}

	rubyExtractImports(root, content, &pf)
	rubyExtractSymbols(root, module, "", content, &pf)
	rubyExtractCalls(root, content, &pf)
	linkTestsGeneric(module, &pf)
	return pf, nil
}

func rubyExtractImports(root *sitter.Node, content []byte, pf *graph.ParsedFile) {
	for _, call := range findDescendants(root, "call") {
		fnNode := childByFieldName(call, "method")
		if fnNode == nil {
			continue
		}
		method := nodeText(fnNode, content)
		if method != "require" && method != "require_relative" {
			continue
		}
		args := childByFieldName(call, "arguments")
		if args == nil {
			continue
		}
		for _, strNode := range findDescendants(args, "string") {
			val := strings.Trim(nodeText(strNode, content), `"'`)
			if val != "" {
				pf.Imports = append(pf.Imports, val)
			}
		}
	}
}

func rubyExtractSymbols(node *sitter.Node, module, container string, content []byte, pf *graph.ParsedFile) {
	for i := range int(node.ChildCount()) {
		child := node.Child(i)
		switch child.Type() {
		case "class":
			rubyAddClass(child, module, content, pf)
		case "module":
			rubyAddModule(child, module, content, pf)
		case "method":
			rubyAddMethod(child, module, container, content, pf)
		case "singleton_method":
			rubyAddMethod(child, module, container, content, pf)
		}
	}
}

func rubyAddClass(node *sitter.Node, module string, content []byte, pf *graph.ParsedFile) {
	nameNode := childByFieldName(node, "name")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, content)

	pf.Symbols = append(pf.Symbols, graph.Symbol{
		Language:      "ruby",
		Kind:          "class",
		Name:          name,
		QualifiedName: module + "." + name,
		ContainerName: module,
		Visibility:    heuristicVisibility(name),
		Range:         nodeRange(node),
		DocSummary:    prevCommentText(node, content),
		StableKey:     "type:ruby:" + module + ":" + name,
	})

	body := childByFieldName(node, "body")
	if body != nil {
		rubyExtractSymbols(body, module, name, content, pf)
	}
}

func rubyAddModule(node *sitter.Node, module string, content []byte, pf *graph.ParsedFile) {
	nameNode := childByFieldName(node, "name")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, content)

	pf.Symbols = append(pf.Symbols, graph.Symbol{
		Language:      "ruby",
		Kind:          "type",
		Name:          name,
		QualifiedName: module + "." + name,
		ContainerName: module,
		Visibility:    heuristicVisibility(name),
		Range:         nodeRange(node),
		DocSummary:    prevCommentText(node, content),
		StableKey:     "type:ruby:" + module + ":" + name,
	})

	body := childByFieldName(node, "body")
	if body != nil {
		rubyExtractSymbols(body, module, name, content, pf)
	}
}

func rubyAddMethod(node *sitter.Node, module, container string, content []byte, pf *graph.ParsedFile) {
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
		Language:      "ruby",
		Kind:          "function",
		Name:          name,
		QualifiedName: qualified,
		ContainerName: effectiveContainer,
		Visibility:    heuristicVisibility(name),
		Range:         nodeRange(node),
		DocSummary:    prevCommentText(node, content),
		StableKey:     "func:ruby:" + module + ":" + name,
	})
}

func rubyExtractCalls(root *sitter.Node, content []byte, pf *graph.ParsedFile) {
	for _, call := range findDescendants(root, "call") {
		methodNode := childByFieldName(call, "method")
		if methodNode == nil {
			continue
		}
		name := nodeText(methodNode, content)
		if name == "require" || name == "require_relative" {
			continue
		}
		receiver := childByFieldName(call, "receiver")
		fullName := name
		if receiver != nil {
			fullName = nodeText(receiver, content) + "." + name
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
