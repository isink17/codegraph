//go:build cgo

package treesitter

import (
	"context"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	javascript "github.com/smacker/go-tree-sitter/javascript"
	typescript "github.com/smacker/go-tree-sitter/typescript/typescript"

	"github.com/isink17/codegraph/internal/graph"
)

// TypeScriptAdapter parses TypeScript and JavaScript files using tree-sitter.
type TypeScriptAdapter struct{}

func NewTypeScript() *TypeScriptAdapter { return &TypeScriptAdapter{} }

func (a *TypeScriptAdapter) Language() string { return "typescript" }
func (a *TypeScriptAdapter) Extensions() []string {
	return []string{".ts", ".tsx", ".js", ".jsx", ".mjs"}
}

func (a *TypeScriptAdapter) Supports(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".ts", ".tsx", ".js", ".jsx", ".mjs":
		return true
	}
	return false
}

func (a *TypeScriptAdapter) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	ext := strings.ToLower(filepath.Ext(path))
	lang := typescript.GetLanguage()
	if ext == ".js" || ext == ".jsx" || ext == ".mjs" {
		lang = javascript.GetLanguage()
	}

	root, err := parse(ctx, lang, content)
	if err != nil {
		return graph.ParsedFile{}, err
	}

	module := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	pf := graph.ParsedFile{
		Language:   "typescript",
		FileTokens: computeFileTokens(content),
	}

	tsExtractImports(root, content, &pf)
	tsExtractSymbols(root, module, "", content, &pf)
	tsExtractCalls(root, content, &pf)
	linkTestsGeneric(module, &pf)
	return pf, nil
}

func tsExtractImports(root *sitter.Node, content []byte, pf *graph.ParsedFile) {
	for _, imp := range findDescendants(root, "import_statement") {
		src := childByFieldName(imp, "source")
		if src == nil {
			src = firstChild(imp, "string")
		}
		if src != nil {
			val := strings.Trim(nodeText(src, content), `"'`)
			if val != "" {
				pf.Imports = append(pf.Imports, val)
			}
		}
	}
	// require() calls
	for _, call := range findDescendants(root, "call_expression") {
		fnNode := childByFieldName(call, "function")
		if fnNode != nil && nodeText(fnNode, content) == "require" {
			args := childByFieldName(call, "arguments")
			if args != nil && args.ChildCount() > 1 {
				arg := args.Child(1) // skip '('
				if arg != nil {
					val := strings.Trim(nodeText(arg, content), `"'`)
					if val != "" {
						pf.Imports = append(pf.Imports, val)
					}
				}
			}
		}
	}
}

func tsExtractSymbols(node *sitter.Node, module, container string, content []byte, pf *graph.ParsedFile) {
	for i := range int(node.ChildCount()) {
		child := node.Child(i)
		switch child.Type() {
		case "function_declaration":
			tsAddFunction(child, module, container, false, content, pf)
		case "class_declaration":
			tsAddClass(child, module, content, pf)
		case "interface_declaration":
			tsAddType(child, module, "interface", content, pf)
		case "type_alias_declaration":
			tsAddType(child, module, "type", content, pf)
		case "export_statement":
			tsExtractExportedSymbol(child, module, container, content, pf)
		case "lexical_declaration", "variable_declaration":
			tsExtractArrowFunctions(child, module, container, false, content, pf)
		case "method_definition":
			tsAddFunction(child, module, container, false, content, pf)
		}
	}
}

func tsExtractExportedSymbol(node *sitter.Node, module, container string, content []byte, pf *graph.ParsedFile) {
	for i := range int(node.ChildCount()) {
		child := node.Child(i)
		switch child.Type() {
		case "function_declaration":
			tsAddFunction(child, module, container, true, content, pf)
		case "class_declaration":
			tsAddClass(child, module, content, pf)
		case "interface_declaration":
			tsAddType(child, module, "interface", content, pf)
		case "type_alias_declaration":
			tsAddType(child, module, "type", content, pf)
		case "lexical_declaration", "variable_declaration":
			tsExtractArrowFunctions(child, module, container, true, content, pf)
		}
	}
}

func tsAddFunction(node *sitter.Node, module, container string, exported bool, content []byte, pf *graph.ParsedFile) {
	nameNode := childByFieldName(node, "name")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, content)
	kind := "function"
	effectiveContainer := module
	if container != "" && container != module {
		kind = "method"
		effectiveContainer = container
	}
	qualified := module + "." + name
	stableKey := "func:" + "typescript:" + module + ":" + name
	if container != "" && container != module {
		qualified = module + "." + container + "." + name
	}
	vis := heuristicVisibility(name)
	if exported {
		vis = "public"
	}

	pf.Symbols = append(pf.Symbols, graph.Symbol{
		Language:      "typescript",
		Kind:          kind,
		Name:          name,
		QualifiedName: qualified,
		ContainerName: effectiveContainer,
		Visibility:    vis,
		Range:         nodeRange(node),
		DocSummary:    prevCommentText(node, content),
		StableKey:     stableKey,
	})
}

func tsAddClass(node *sitter.Node, module string, content []byte, pf *graph.ParsedFile) {
	nameNode := childByFieldName(node, "name")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, content)

	pf.Symbols = append(pf.Symbols, graph.Symbol{
		Language:      "typescript",
		Kind:          "class",
		Name:          name,
		QualifiedName: module + "." + name,
		ContainerName: module,
		Visibility:    "public",
		Range:         nodeRange(node),
		DocSummary:    prevCommentText(node, content),
		StableKey:     "type:typescript:" + module + ":" + name,
	})

	body := childByFieldName(node, "body")
	if body != nil {
		tsExtractSymbols(body, module, name, content, pf)
	}
}

func tsAddType(node *sitter.Node, module, kind string, content []byte, pf *graph.ParsedFile) {
	nameNode := childByFieldName(node, "name")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, content)

	pf.Symbols = append(pf.Symbols, graph.Symbol{
		Language:      "typescript",
		Kind:          kind,
		Name:          name,
		QualifiedName: module + "." + name,
		ContainerName: module,
		Visibility:    "public",
		Range:         nodeRange(node),
		DocSummary:    prevCommentText(node, content),
		StableKey:     "type:typescript:" + module + ":" + name,
	})
}

func tsExtractArrowFunctions(node *sitter.Node, module, container string, exported bool, content []byte, pf *graph.ParsedFile) {
	// Look for patterns like: const foo = (...) => { ... }
	for _, decl := range findChildren(node, "variable_declarator") {
		nameNode := childByFieldName(decl, "name")
		valueNode := childByFieldName(decl, "value")
		if nameNode == nil || valueNode == nil {
			continue
		}
		t := valueNode.Type()
		if t != "arrow_function" && t != "function" && t != "function_expression" {
			continue
		}
		name := nodeText(nameNode, content)
		effectiveContainer := module
		if container != "" && container != module {
			effectiveContainer = container
		}
		qualified := module + "." + name
		stableKey := "func:" + "typescript:" + module + ":" + name
		vis := heuristicVisibility(name)
		if exported {
			vis = "public"
		}

		pf.Symbols = append(pf.Symbols, graph.Symbol{
			Language:      "typescript",
			Kind:          "function",
			Name:          name,
			QualifiedName: qualified,
			ContainerName: effectiveContainer,
			Visibility:    vis,
			Range:         nodeRange(decl),
			DocSummary:    prevCommentText(node, content),
			StableKey:     stableKey,
		})
	}
}

var tsKeywords = map[string]bool{
	"if": true, "for": true, "while": true, "return": true, "switch": true,
	"new": true, "typeof": true, "instanceof": true, "throw": true,
	"require": true,
}

func tsExtractCalls(root *sitter.Node, content []byte, pf *graph.ParsedFile) {
	for _, call := range findDescendants(root, "call_expression") {
		fnNode := childByFieldName(call, "function")
		if fnNode == nil {
			continue
		}
		name := nodeText(fnNode, content)
		baseName := name
		if idx := strings.LastIndexByte(name, '.'); idx >= 0 {
			baseName = name[idx+1:]
		}
		if tsKeywords[baseName] {
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
