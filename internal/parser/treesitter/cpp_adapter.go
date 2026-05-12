//go:build cgo

package treesitter

import (
	"context"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	clang "github.com/smacker/go-tree-sitter/c"
	cpp "github.com/smacker/go-tree-sitter/cpp"

	"github.com/isink17/codegraph/internal/graph"
)

// CppAdapter parses C and C++ source files using tree-sitter.
type CppAdapter struct{}

func NewCpp() *CppAdapter { return &CppAdapter{} }

func (a *CppAdapter) Language() string { return "cpp" }
func (a *CppAdapter) Extensions() []string {
	return []string{".c", ".h", ".cc", ".cpp", ".cxx", ".hpp", ".hh", ".hxx", ".ipp"}
}

func (a *CppAdapter) Supports(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".c", ".h", ".cc", ".cpp", ".cxx", ".hpp", ".hh", ".hxx", ".ipp":
		return true
	}
	return false
}

func (a *CppAdapter) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	ext := strings.ToLower(filepath.Ext(path))
	lang := cpp.GetLanguage()
	if ext == ".c" {
		lang = clang.GetLanguage()
	}

	root, err := parse(ctx, lang, content)
	if err != nil {
		return graph.ParsedFile{}, err
	}

	module := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	pf := graph.ParsedFile{
		Language:   "cpp",
		FileTokens: computeFileTokens(content),
	}

	cppExtractImports(root, content, &pf)
	cppExtractSymbols(root, module, "", content, &pf)
	cppExtractCalls(root, content, &pf)
	linkTestsGeneric(module, &pf)
	return pf, nil
}

func cppExtractImports(root *sitter.Node, content []byte, pf *graph.ParsedFile) {
	for _, inc := range findDescendants(root, "preproc_include") {
		pathNode := firstChild(inc, "string_literal")
		if pathNode == nil {
			pathNode = firstChild(inc, "system_lib_string")
		}
		if pathNode != nil {
			val := nodeText(pathNode, content)
			val = strings.Trim(val, `"<>`)
			if val != "" {
				pf.Imports = append(pf.Imports, val)
			}
		}
	}
}

var cppSkipFuncs = map[string]bool{
	"if": true, "for": true, "while": true, "switch": true,
	"catch": true, "sizeof": true, "return": true,
}

func cppExtractSymbols(node *sitter.Node, module, container string, content []byte, pf *graph.ParsedFile) {
	for i := range int(node.ChildCount()) {
		child := node.Child(i)
		switch child.Type() {
		case "function_definition":
			cppAddFunction(child, module, container, content, pf)
		case "declaration":
			// Could be a function declaration or variable.
			fnDecl := firstChild(child, "function_declarator")
			if fnDecl != nil {
				cppAddFunctionFromDeclarator(child, fnDecl, module, container, content, pf)
			}
		case "struct_specifier":
			cppAddType(child, module, "struct", content, pf)
		case "class_specifier":
			cppAddType(child, module, "class", content, pf)
		case "enum_specifier":
			cppAddType(child, module, "enum", content, pf)
		case "namespace_definition":
			body := childByFieldName(child, "body")
			if body != nil {
				cppExtractSymbols(body, module, container, content, pf)
			}
		}
	}
}

func cppAddFunction(node *sitter.Node, module, container string, content []byte, pf *graph.ParsedFile) {
	declNode := childByFieldName(node, "declarator")
	if declNode == nil {
		return
	}
	// The declarator might be a function_declarator or a pointer_declarator wrapping one.
	fnDecl := declNode
	if declNode.Type() != "function_declarator" {
		fnDecl = firstChild(declNode, "function_declarator")
		if fnDecl == nil {
			fnDecl = declNode
		}
	}
	nameNode := childByFieldName(fnDecl, "declarator")
	if nameNode == nil {
		return
	}
	// The declarator of the function_declarator might be a qualified_identifier or identifier.
	name := nodeText(nameNode, content)
	if cppSkipFuncs[name] {
		return
	}

	effectiveContainer := module
	if container != "" && container != module {
		effectiveContainer = container
	}
	qualified := cppQualifiedName(effectiveContainer, name)

	pf.Symbols = append(pf.Symbols, graph.Symbol{
		Language:      "cpp",
		Kind:          "function",
		Name:          name,
		QualifiedName: qualified,
		ContainerName: effectiveContainer,
		Visibility:    heuristicVisibility(name),
		Range:         nodeRange(node),
		DocSummary:    prevCommentText(node, content),
		StableKey:     cppStableKey("func", module, effectiveContainer, name),
	})
}

func cppAddFunctionFromDeclarator(decl, fnDecl *sitter.Node, module, container string, content []byte, pf *graph.ParsedFile) {
	nameNode := childByFieldName(fnDecl, "declarator")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, content)
	if cppSkipFuncs[name] || name == "" {
		return
	}

	effectiveContainer := module
	if container != "" && container != module {
		effectiveContainer = container
	}
	qualified := cppQualifiedName(effectiveContainer, name)

	pf.Symbols = append(pf.Symbols, graph.Symbol{
		Language:      "cpp",
		Kind:          "function",
		Name:          name,
		QualifiedName: qualified,
		ContainerName: effectiveContainer,
		Visibility:    heuristicVisibility(name),
		Range:         nodeRange(decl),
		DocSummary:    prevCommentText(decl, content),
		StableKey:     cppStableKey("func", module, effectiveContainer, name),
	})
}

func cppAddType(node *sitter.Node, module, kind string, content []byte, pf *graph.ParsedFile) {
	nameNode := childByFieldName(node, "name")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, content)
	if name == "" {
		return
	}

	pf.Symbols = append(pf.Symbols, graph.Symbol{
		Language:      "cpp",
		Kind:          kind,
		Name:          name,
		QualifiedName: cppQualifiedName(module, name),
		ContainerName: module,
		Visibility:    heuristicVisibility(name),
		Range:         nodeRange(node),
		DocSummary:    prevCommentText(node, content),
		StableKey:     cppStableKey("type", module, module, name),
	})

	body := childByFieldName(node, "body")
	if body != nil {
		cppExtractSymbols(body, module, name, content, pf)
	}
}

func cppExtractCalls(root *sitter.Node, content []byte, pf *graph.ParsedFile) {
	for _, call := range findDescendants(root, "call_expression") {
		fnNode := childByFieldName(call, "function")
		if fnNode == nil {
			continue
		}
		callName, callKind, receiver := cppCallTarget(fnNode, content)
		if callName == "" || cppSkipFuncs[callName] {
			continue
		}
		line := int(call.StartPoint().Row) + 1
		evidence := callKind + ":" + nodeText(fnNode, content)
		if receiver != "" {
			evidence = callKind + ":" + receiver + ":" + nodeText(fnNode, content)
		}
		pf.Edges = append(pf.Edges, graph.Edge{
			SrcSymbolID: 0,
			DstName:     callName,
			Kind:        "calls",
			Evidence:    evidence,
			Line:        line,
		})
		pf.References = append(pf.References, graph.Reference{
			Kind:          "call",
			Name:          callName,
			QualifiedName: callName,
			Range:         nodeRange(call),
		})
	}
}

func cppQualifiedName(container, name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if strings.Contains(name, "::") {
		return name
	}
	container = strings.TrimSpace(container)
	if container == "" {
		return name
	}
	return container + "::" + name
}

func cppStableKey(prefix, module, container, name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	key := name
	if !strings.Contains(name, "::") {
		container = strings.TrimSpace(container)
		if container != "" {
			key = container + "::" + name
		}
	}
	module = strings.TrimSpace(module)
	if module == "" {
		return prefix + ":cpp:" + key
	}
	if strings.HasPrefix(key, module+"::") {
		key = strings.TrimPrefix(key, module+"::")
	}
	return prefix + ":cpp:" + module + ":" + key
}

func cppCallTarget(fnNode *sitter.Node, content []byte) (name, kind, receiver string) {
	raw := strings.TrimSpace(nodeText(fnNode, content))
	if raw == "" {
		return "", "", ""
	}
	switch {
	case strings.HasPrefix(raw, "::"):
		return strings.TrimPrefix(raw, "::"), "static_qualified", ""
	case strings.Contains(raw, "::"):
		return raw, "static_qualified", ""
	case strings.Contains(raw, "->"):
		receiver, name = splitCallTarget(raw, "->")
		return strings.TrimSpace(name), "member_pointer", strings.TrimSpace(receiver)
	case strings.Contains(raw, "."):
		receiver, name = splitCallTarget(raw, ".")
		return strings.TrimSpace(name), "member_value", strings.TrimSpace(receiver)
	default:
		return raw, "direct", ""
	}
}

func splitCallTarget(text, sep string) (string, string) {
	idx := strings.LastIndex(text, sep)
	if idx < 0 {
		return "", text
	}
	return text[:idx], text[idx+len(sep):]
}
