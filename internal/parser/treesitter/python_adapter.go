//go:build cgo

package treesitter

import (
	"context"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	python "github.com/smacker/go-tree-sitter/python"

	"github.com/isink17/codegraph/internal/graph"
)

// PythonAdapter parses Python source files using tree-sitter.
type PythonAdapter struct{}

func NewPython() *PythonAdapter { return &PythonAdapter{} }

func (a *PythonAdapter) Language() string     { return "python" }
func (a *PythonAdapter) Extensions() []string { return []string{".py"} }

func (a *PythonAdapter) Supports(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".py")
}

func (a *PythonAdapter) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	root, err := parse(ctx, python.GetLanguage(), content)
	if err != nil {
		return graph.ParsedFile{}, err
	}

	module := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	pf := graph.ParsedFile{
		Language:   "python",
		FileTokens: computeFileTokens(content),
	}

	// ---- imports ----
	pyExtractImports(root, content, &pf)

	// ---- top-level symbols ----
	pyExtractSymbols(root, module, "", content, &pf)

	// ---- call edges (all call nodes in the file) ----
	pyExtractCalls(root, content, &pf)

	linkTestsPython(module, &pf)
	return pf, nil
}

func pyExtractImports(root *sitter.Node, content []byte, pf *graph.ParsedFile) {
	for _, imp := range findDescendants(root, "import_statement") {
		// import foo, bar
		for _, nameNode := range findDescendants(imp, "dotted_name") {
			pf.Imports = append(pf.Imports, nodeText(nameNode, content))
		}
	}
	for _, imp := range findDescendants(root, "import_from_statement") {
		// from foo import bar
		nameNode := childByFieldName(imp, "module_name")
		if nameNode != nil {
			pf.Imports = append(pf.Imports, nodeText(nameNode, content))
		}
	}
}

func pyExtractSymbols(node *sitter.Node, module, container string, content []byte, pf *graph.ParsedFile) {
	for i := range int(node.ChildCount()) {
		child := node.Child(i)
		switch child.Type() {
		case "class_definition":
			pyExtractClass(child, module, content, pf)
		case "function_definition":
			pyExtractFunction(child, module, container, content, pf)
		case "decorated_definition":
			// The actual definition is a child of the decorated_definition.
			inner := firstChild(child, "class_definition")
			if inner != nil {
				pyExtractClass(inner, module, content, pf)
				continue
			}
			inner = firstChild(child, "function_definition")
			if inner != nil {
				pyExtractFunction(inner, module, container, content, pf)
			}
		}
	}
}

func pyExtractClass(node *sitter.Node, module string, content []byte, pf *graph.ParsedFile) {
	nameNode := childByFieldName(node, "name")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, content)
	qualified := module + "." + name
	doc := pyDocstring(node, content)

	pf.Symbols = append(pf.Symbols, graph.Symbol{
		Language:      "python",
		Kind:          "class",
		Name:          name,
		QualifiedName: qualified,
		ContainerName: module,
		Visibility:    pyVisibility(name),
		Range:         nodeRange(node),
		DocSummary:    doc,
		StableKey:     "class:" + module + ":" + name,
	})

	// Extract methods inside the class body.
	body := childByFieldName(node, "body")
	if body != nil {
		pyExtractSymbols(body, module, name, content, pf)
	}
}

func pyExtractFunction(node *sitter.Node, module, container string, content []byte, pf *graph.ParsedFile) {
	nameNode := childByFieldName(node, "name")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, content)
	kind := "function"
	if container != "" && container != module {
		kind = "method"
	}
	effectiveContainer := module
	if container != "" {
		effectiveContainer = container
	}
	qualified := module + "." + name
	stableKey := "func:" + module + "::" + name
	if container != "" && container != module {
		qualified = module + "." + container + "." + name
		stableKey = "func:" + module + ":" + container + ":" + name
	}

	// Build signature including decorators.
	sig := pySignature(node, content)
	doc := pyDocstring(node, content)

	pf.Symbols = append(pf.Symbols, graph.Symbol{
		Language:      "python",
		Kind:          kind,
		Name:          name,
		QualifiedName: qualified,
		ContainerName: effectiveContainer,
		Signature:     sig,
		Visibility:    pyVisibility(name),
		Range:         nodeRange(node),
		DocSummary:    doc,
		StableKey:     stableKey,
	})
}

func pySignature(node *sitter.Node, content []byte) string {
	// "def name(params):" or "@decorator\ndef name(params):"
	var b strings.Builder

	// Check for decorators on the parent decorated_definition.
	parent := node.Parent()
	if parent != nil && parent.Type() == "decorated_definition" {
		for _, dec := range findChildren(parent, "decorator") {
			b.WriteString(nodeText(dec, content))
			b.WriteByte('\n')
		}
	}

	b.WriteString("def ")
	nameNode := childByFieldName(node, "name")
	if nameNode != nil {
		b.WriteString(nodeText(nameNode, content))
	}
	params := childByFieldName(node, "parameters")
	if params != nil {
		b.WriteString(nodeText(params, content))
	}
	return b.String()
}

func pyDocstring(node *sitter.Node, content []byte) string {
	body := childByFieldName(node, "body")
	if body == nil || body.ChildCount() == 0 {
		return ""
	}
	first := body.Child(0)
	if first.Type() != "expression_statement" {
		return ""
	}
	if first.ChildCount() == 0 {
		return ""
	}
	strNode := first.Child(0)
	if strNode.Type() != "string" {
		return ""
	}
	raw := nodeText(strNode, content)
	raw = strings.Trim(raw, `"'`)
	raw = strings.TrimSpace(raw)
	// Take first line only for summary.
	if idx := strings.IndexByte(raw, '\n'); idx >= 0 {
		raw = strings.TrimSpace(raw[:idx])
	}
	return raw
}

var pyKeywords = map[string]bool{
	"if": true, "for": true, "while": true, "return": true, "print": true,
	"with": true, "class": true, "def": true, "try": true, "except": true,
	"elif": true, "raise": true, "assert": true, "del": true, "pass": true,
}

func pyExtractCalls(root *sitter.Node, content []byte, pf *graph.ParsedFile) {
	for _, call := range findDescendants(root, "call") {
		fnNode := childByFieldName(call, "function")
		if fnNode == nil {
			continue
		}
		name := pyCallName(fnNode, content)
		if name == "" || pyKeywords[name] {
			continue
		}
		line := int(call.StartPoint().Row) + 1
		pf.Edges = append(pf.Edges, graph.Edge{
			SrcSymbolID: 0,
			DstName:     name,
			Kind:        "calls",
			Evidence:    nodeText(fnNode, content),
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

func pyCallName(fnNode *sitter.Node, content []byte) string {
	switch fnNode.Type() {
	case "identifier":
		return nodeText(fnNode, content)
	case "attribute":
		return nodeText(fnNode, content)
	default:
		return nodeText(fnNode, content)
	}
}
