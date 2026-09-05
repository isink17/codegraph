//go:build cgo

package treesitter

import (
	"context"
	"path/filepath"
	"sort"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	sitterpython "github.com/smacker/go-tree-sitter/python"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/parser/python"
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
	root, err := parse(ctx, sitterpython.GetLanguage(), content)
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
	pyExtractSymbols(root, module, pyScope{}, content, &pf)

	// ---- lexical bindings that shadow an import ----
	pyExtractLocalBindings(root, module, content, &pf)

	// ---- call edges (all call nodes in the file) ----
	pyExtractCalls(root, content, &pf)

	linkTestsPython(module, &pf)
	return pf, nil
}

// pyExtractImports hands each import statement's own source text to the shared
// binding parser, so this adapter and the regex adapter record the same module
// spelling, imported name and local binding for the same statement.
func pyExtractImports(root *sitter.Node, content []byte, pf *graph.ParsedFile) {
	nodes := append(findDescendants(root, "import_statement"), findDescendants(root, "import_from_statement")...)
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].StartByte() < nodes[j].StartByte() })
	seen := make(map[string]struct{}, len(nodes))
	for _, imp := range nodes {
		if pyInClassBody(imp) {
			// A class body is not a scope Python name lookup passes through, so
			// an import written in one reaches nothing this resolver can see.
			continue
		}
		owner := pyLexicalOwner(imp, content)
		for _, binding := range python.ImportBindings(nodeText(imp, content)) {
			binding.OwnerModule = owner
			pf.Scope.Imports = append(pf.Scope.Imports, binding)
			if _, ok := seen[binding.SourceSpecifier]; !ok {
				seen[binding.SourceSpecifier] = struct{}{}
				pf.Imports = append(pf.Imports, binding.SourceSpecifier)
			}
		}
	}
}

// pyInClassBody reports whether the nearest enclosing declaration is a class.
func pyInClassBody(node *sitter.Node) bool {
	for parent := node.Parent(); parent != nil; parent = parent.Parent() {
		switch parent.Type() {
		case "class_definition":
			return true
		case "function_definition":
			return false
		}
	}
	return false
}

// pyLexicalOwner names the scope a node was written in, as the dotted path of
// enclosing declarations -- the same spelling a symbol's qualified name carries
// after its module. A node at module level is owned by "".
func pyLexicalOwner(node *sitter.Node, content []byte) string {
	var parts []string
	for parent := node.Parent(); parent != nil; parent = parent.Parent() {
		switch parent.Type() {
		case "function_definition", "class_definition":
			if name := childByFieldName(parent, "name"); name != nil {
				parts = append(parts, nodeText(name, content))
			}
		}
	}
	for i, j := 0, len(parts)-1; i < j; i, j = i+1, j-1 {
		parts[i], parts[j] = parts[j], parts[i]
	}
	return strings.Join(parts, ".")
}

// pyExtractLocalBindings records what each lexical scope binds itself, using
// the same text scan the regex adapter uses so the two adapters cannot disagree
// about a shadow. Class bodies are not scopes a Python name lookup passes
// through, so only the module and each function body are scanned.
func pyExtractLocalBindings(root *sitter.Node, module string, content []byte, pf *graph.ParsedFile) {
	emit := func(owner, source string) {
		for _, name := range python.LocalBindings(source, owner != "") {
			pf.Scope.Imports = append(pf.Scope.Imports, graph.ScopeImport{
				LocalName:   name,
				Kind:        graph.ScopeImportLocalBinding,
				OwnerModule: owner,
			})
		}
	}
	emit("", string(content))
	for _, fn := range findDescendants(root, "function_definition") {
		name := childByFieldName(fn, "name")
		if name == nil {
			continue
		}
		owner := pyLexicalOwner(fn, content)
		if owner != "" {
			owner += "."
		}
		emit(owner+nodeText(name, content), nodeText(fn, content))
	}
}

type pyScope struct {
	path      []string
	ownerKind string
}

func pyExtractSymbols(node *sitter.Node, module string, scope pyScope, content []byte, pf *graph.ParsedFile) {
	for i := range int(node.ChildCount()) {
		child := node.Child(i)
		switch child.Type() {
		case "class_definition":
			pyExtractClass(child, module, scope, content, pf)
		case "function_definition":
			pyExtractFunction(child, module, scope, content, pf)
		case "decorated_definition":
			// The actual definition is a child of the decorated_definition.
			inner := firstChild(child, "class_definition")
			if inner != nil {
				pyExtractClass(inner, module, scope, content, pf)
				continue
			}
			inner = firstChild(child, "function_definition")
			if inner != nil {
				pyExtractFunction(inner, module, scope, content, pf)
			}
		}
	}
}

func pyExtractClass(node *sitter.Node, module string, scope pyScope, content []byte, pf *graph.ParsedFile) {
	nameNode := childByFieldName(node, "name")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, content)
	path := append(append([]string(nil), scope.path...), name)
	qualified := module + "." + strings.Join(path, ".")
	container := module
	if len(scope.path) > 0 {
		container = strings.Join(scope.path, ".")
	}
	doc := pyDocstring(node, content)

	pf.Symbols = append(pf.Symbols, graph.Symbol{
		Language:      "python",
		Kind:          "class",
		Name:          name,
		QualifiedName: qualified,
		ContainerName: container,
		Visibility:    pyVisibility(name),
		Range:         nodeRange(node),
		DocSummary:    doc,
		StableKey:     "class:" + module + ":" + name,
	})

	// Extract methods inside the class body.
	body := childByFieldName(node, "body")
	if body != nil {
		pyExtractSymbols(body, module, pyScope{path: path, ownerKind: "class"}, content, pf)
	}
}

func pyExtractFunction(node *sitter.Node, module string, scope pyScope, content []byte, pf *graph.ParsedFile) {
	nameNode := childByFieldName(node, "name")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, content)
	kind := "function"
	if scope.ownerKind == "class" {
		kind = "method"
	}
	effectiveContainer := module
	if len(scope.path) > 0 {
		effectiveContainer = strings.Join(scope.path, ".")
	}
	path := append(append([]string(nil), scope.path...), name)
	qualified := module + "." + strings.Join(path, ".")
	stableKey := "func:" + module + "::" + name
	if len(scope.path) > 0 {
		stableKey = "func:" + module + ":" + strings.Join(scope.path, ".") + ":" + name
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

	body := childByFieldName(node, "body")
	if body != nil {
		pyExtractSymbols(body, module, pyScope{path: path, ownerKind: "function"}, content, pf)
	}
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

	if strings.HasPrefix(strings.TrimSpace(nodeText(node, content)), "async ") {
		b.WriteString("async ")
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

func pyCallName(fnNode *sitter.Node, content []byte) string {
	switch fnNode.Type() {
	case "identifier":
		return nodeText(fnNode, content)
	case "attribute":
		object := childByFieldName(fnNode, "object")
		attribute := childByFieldName(fnNode, "attribute")
		if object == nil || attribute == nil {
			return ""
		}
		prefix := pyCallName(object, content)
		if prefix == "" {
			return ""
		}
		return prefix + "." + nodeText(attribute, content)
	default:
		return ""
	}
}
