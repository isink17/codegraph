package treesitter

import (
	"context"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	golang "github.com/smacker/go-tree-sitter/golang"

	"github.com/isink17/codegraph/internal/graph"
)

// GoAdapter parses Go source files using tree-sitter.
type GoAdapter struct{}

func NewGo() *GoAdapter { return &GoAdapter{} }

func (a *GoAdapter) Language() string     { return "go" }
func (a *GoAdapter) Extensions() []string { return []string{".go"} }

func (a *GoAdapter) Supports(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".go")
}

func (a *GoAdapter) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	root, err := parse(ctx, golang.GetLanguage(), content)
	if err != nil {
		return graph.ParsedFile{}, err
	}

	pf := graph.ParsedFile{
		Language:   "go",
		FileTokens: computeFileTokens(content),
	}

	pkgName := goPackageName(root, content)
	importAliases := map[string]string{}

	// ---- imports ----
	for _, impDecl := range findChildren(root, "import_declaration") {
		for _, spec := range findDescendants(impDecl, "import_spec") {
			pathNode := firstChild(spec, "interpreted_string_literal")
			if pathNode == nil {
				continue
			}
			importPath := strings.Trim(nodeText(pathNode, content), `"`)
			pf.Imports = append(pf.Imports, importPath)
			base := filepath.Base(importPath)
			nameNode := childByFieldName(spec, "name")
			if nameNode != nil {
				alias := nodeText(nameNode, content)
				if alias != "_" && alias != "." {
					base = alias
				}
			}
			importAliases[base] = importPath
		}
		// Single import (no import_spec_list)
		specs := findChildren(impDecl, "import_spec")
		if len(specs) == 0 {
			// The import_declaration itself may hold a single interpreted_string_literal
			pathNode := firstChild(impDecl, "interpreted_string_literal")
			if pathNode != nil {
				importPath := strings.Trim(nodeText(pathNode, content), `"`)
				pf.Imports = append(pf.Imports, importPath)
				base := filepath.Base(importPath)
				importAliases[base] = importPath
			}
		}
	}

	symbolByKey := map[string]*graph.Symbol{}

	// ---- functions and methods ----
	for _, fn := range findChildren(root, "function_declaration") {
		sym := goFuncSymbol(fn, pkgName, content)
		pf.Symbols = append(pf.Symbols, sym)
		symbolByKey[sym.StableKey] = &pf.Symbols[len(pf.Symbols)-1]
	}
	for _, fn := range findChildren(root, "method_declaration") {
		sym := goMethodSymbol(fn, pkgName, content)
		pf.Symbols = append(pf.Symbols, sym)
		symbolByKey[sym.StableKey] = &pf.Symbols[len(pf.Symbols)-1]
	}

	// ---- type declarations ----
	for _, genDecl := range findChildren(root, "type_declaration") {
		for _, typeSpec := range findDescendants(genDecl, "type_spec") {
			sym := goTypeSymbol(typeSpec, pkgName, content)
			pf.Symbols = append(pf.Symbols, sym)
			symbolByKey[sym.StableKey] = &pf.Symbols[len(pf.Symbols)-1]
		}
	}

	// ---- value declarations (var/const) ----
	for _, kind := range []string{"var_declaration", "const_declaration"} {
		for _, decl := range findChildren(root, kind) {
			for _, spec := range findDescendants(decl, "var_spec") {
				goAddValueSymbols(&pf, spec, pkgName, content)
			}
			for _, spec := range findDescendants(decl, "const_spec") {
				goAddValueSymbols(&pf, spec, pkgName, content)
			}
		}
	}

	// ---- call edges ----
	goExtractCalls(root, content, importAliases, &pf)

	linkTestsGo(pkgName, &pf)
	return pf, nil
}

// ---------- helpers ----------

func goPackageName(root *sitter.Node, content []byte) string {
	pkg := firstChild(root, "package_clause")
	if pkg == nil {
		return ""
	}
	nameNode := childByFieldName(pkg, "name")
	if nameNode != nil {
		return nodeText(nameNode, content)
	}
	// fallback: second child
	if pkg.ChildCount() >= 2 {
		return nodeText(pkg.Child(1), content)
	}
	return ""
}

func goFuncSymbol(fn *sitter.Node, pkg string, content []byte) graph.Symbol {
	nameNode := childByFieldName(fn, "name")
	name := nodeText(nameNode, content)
	qualified := pkg + "." + name
	stableKey := "func:" + pkg + "::" + name
	sig := goFuncSignature(fn, content)
	doc := prevCommentText(fn, content)

	return graph.Symbol{
		Language:      "go",
		Kind:          "function",
		Name:          name,
		QualifiedName: qualified,
		ContainerName: pkg,
		Signature:     sig,
		Visibility:    goVisibility(name),
		Range:         nodeRange(fn),
		DocSummary:    doc,
		StableKey:     stableKey,
	}
}

func goMethodSymbol(fn *sitter.Node, pkg string, content []byte) graph.Symbol {
	nameNode := childByFieldName(fn, "name")
	name := nodeText(nameNode, content)
	recv := goReceiverType(fn, content)
	qualified := pkg + "." + recv + "." + name
	stableKey := "func:" + pkg + ":" + recv + ":" + name
	sig := goFuncSignature(fn, content)
	doc := prevCommentText(fn, content)

	return graph.Symbol{
		Language:      "go",
		Kind:          "function",
		Name:          name,
		QualifiedName: qualified,
		ContainerName: recv,
		Signature:     sig,
		Visibility:    goVisibility(name),
		Range:         nodeRange(fn),
		DocSummary:    doc,
		StableKey:     stableKey,
	}
}

func goReceiverType(fn *sitter.Node, content []byte) string {
	recvNode := childByFieldName(fn, "receiver")
	if recvNode == nil {
		return ""
	}
	// The receiver is a parameter_list; grab its first parameter_declaration.
	params := findChildren(recvNode, "parameter_declaration")
	if len(params) == 0 {
		return ""
	}
	typeNode := childByFieldName(params[0], "type")
	if typeNode == nil {
		// Fallback: last child
		if params[0].ChildCount() > 0 {
			typeNode = params[0].Child(int(params[0].ChildCount()) - 1)
		}
	}
	text := nodeText(typeNode, content)
	text = strings.TrimPrefix(text, "*")
	return text
}

func goTypeSymbol(typeSpec *sitter.Node, pkg string, content []byte) graph.Symbol {
	nameNode := childByFieldName(typeSpec, "name")
	name := nodeText(nameNode, content)
	doc := prevCommentText(typeSpec.Parent(), content)

	// Determine the underlying kind.
	typeNode := childByFieldName(typeSpec, "type")
	sig := ""
	if typeNode != nil {
		t := typeNode.Type()
		switch t {
		case "struct_type":
			sig = "struct"
		case "interface_type":
			sig = "interface"
		default:
			sig = nodeText(typeNode, content)
		}
	}

	return graph.Symbol{
		Language:      "go",
		Kind:          "type",
		Name:          name,
		QualifiedName: pkg + "." + name,
		ContainerName: pkg,
		Signature:     sig,
		Visibility:    goVisibility(name),
		Range:         nodeRange(typeSpec),
		DocSummary:    doc,
		StableKey:     "type:" + pkg + ":" + name,
	}
}

func goAddValueSymbols(pf *graph.ParsedFile, spec *sitter.Node, pkg string, content []byte) {
	nameNode := childByFieldName(spec, "name")
	if nameNode == nil {
		// Multiple names: iterate children looking for identifiers
		for i := 0; i < int(spec.ChildCount()); i++ {
			c := spec.Child(i)
			if c.Type() == "identifier" {
				name := nodeText(c, content)
				pf.Symbols = append(pf.Symbols, graph.Symbol{
					Language:      "go",
					Kind:          "value",
					Name:          name,
					QualifiedName: pkg + "." + name,
					ContainerName: pkg,
					Visibility:    goVisibility(name),
					Range:         nodeRange(c),
					StableKey:     "value:" + pkg + ":" + name,
				})
			}
		}
		return
	}
	name := nodeText(nameNode, content)
	pf.Symbols = append(pf.Symbols, graph.Symbol{
		Language:      "go",
		Kind:          "value",
		Name:          name,
		QualifiedName: pkg + "." + name,
		ContainerName: pkg,
		Visibility:    goVisibility(name),
		Range:         nodeRange(nameNode),
		StableKey:     "value:" + pkg + ":" + name,
	})
}

func goFuncSignature(fn *sitter.Node, content []byte) string {
	// Build "func (recv) Name(params) results"
	var b strings.Builder
	b.WriteString("func ")
	if recv := childByFieldName(fn, "receiver"); recv != nil {
		b.WriteString(nodeText(recv, content))
		b.WriteByte(' ')
	}
	if nameNode := childByFieldName(fn, "name"); nameNode != nil {
		b.WriteString(nodeText(nameNode, content))
	}
	if params := childByFieldName(fn, "parameters"); params != nil {
		b.WriteString(nodeText(params, content))
	}
	if result := childByFieldName(fn, "result"); result != nil {
		b.WriteByte(' ')
		b.WriteString(nodeText(result, content))
	}
	return b.String()
}

func goExtractCalls(root *sitter.Node, content []byte, imports map[string]string, pf *graph.ParsedFile) {
	for _, call := range findDescendants(root, "call_expression") {
		fnNode := childByFieldName(call, "function")
		if fnNode == nil {
			continue
		}
		name := goCallName(fnNode, content, imports)
		if name == "" {
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

func goCallName(fnNode *sitter.Node, content []byte, imports map[string]string) string {
	switch fnNode.Type() {
	case "identifier":
		return nodeText(fnNode, content)
	case "selector_expression":
		left := childByFieldName(fnNode, "operand")
		right := childByFieldName(fnNode, "field")
		if left == nil || right == nil {
			return nodeText(fnNode, content)
		}
		leftText := nodeText(left, content)
		rightText := nodeText(right, content)
		if importPath, ok := imports[leftText]; ok {
			return importPath + "." + rightText
		}
		return leftText + "." + rightText
	default:
		return nodeText(fnNode, content)
	}
}
