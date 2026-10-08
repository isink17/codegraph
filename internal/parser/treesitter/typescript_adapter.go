//go:build cgo

package treesitter

import (
	"context"
	"maps"
	"path"
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

func (a *TypeScriptAdapter) Parse(ctx context.Context, logicalPath string, content []byte) (graph.ParsedFile, error) {
	ext := strings.ToLower(path.Ext(logicalPath))
	lang := typescript.GetLanguage()
	if ext == ".js" || ext == ".jsx" || ext == ".mjs" {
		lang = javascript.GetLanguage()
	}

	root, err := parse(ctx, lang, content)
	if err != nil {
		return graph.ParsedFile{}, err
	}

	module := strings.TrimSuffix(logicalPath, path.Ext(logicalPath))
	pf := graph.ParsedFile{
		Language:   "typescript",
		FileTokens: computeFileTokens(content),
	}

	tsExtractImports(root, content, &pf)
	tsExtractSymbols(root, module, "", content, &pf)
	imported := map[string]bool{}
	for _, imp := range pf.Scope.Imports {
		if !imp.ReExport {
			imported[imp.LocalName] = true
		}
	}
	// A call can only bind through an import or a declaration of this file, so
	// only those names need the scope walk that proves a nearer binding.
	bindable := maps.Clone(imported)
	for _, sym := range pf.Symbols {
		bindable[sym.Name] = true
		// A dotted call can name a declaration of this file by its qualified
		// name, whose first segment is the module.
		head, _, _ := strings.Cut(sym.QualifiedName, ".")
		bindable[head] = true
	}
	tsExtractCalls(root, content, imported, bindable, &pf)
	linkTestsGeneric(module, &pf, func(target string) string {
		return "func:typescript:" + testTargetModule(module, ".test", ".spec") + ":" + target
	})
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
				addTypeScriptImport(nodeText(imp, content), &pf.Scope.Imports)
			}
		}
	}
	for _, export := range findDescendants(root, "export_statement") {
		src := childByFieldName(export, "source")
		if src == nil {
			src = firstChild(export, "string")
		}
		if src == nil {
			if clause := childByFieldName(export, "export_clause"); clause != nil {
				for _, spec := range findDescendants(clause, "export_specifier") {
					name := nodeText(childByFieldName(spec, "name"), content)
					alias := nodeText(childByFieldName(spec, "alias"), content)
					if name != "" {
						if alias == "" {
							alias = name
						}
						pf.Scope.Imports = append(pf.Scope.Imports, graph.ScopeImport{ImportedName: name, LocalName: alias, Kind: graph.ScopeImportNamed, ReExport: true})
					}
				}
			}
			continue
		}
		source := strings.Trim(nodeText(src, content), `"'`)
		if source == "" {
			continue
		}
		clause := childByFieldName(export, "export_clause")
		if clause == nil {
			clause = firstChild(export, "export_clause")
		}
		if clause != nil {
			for _, spec := range findDescendants(clause, "export_specifier") {
				name := nodeText(childByFieldName(spec, "name"), content)
				alias := nodeText(childByFieldName(spec, "alias"), content)
				if name == "" {
					continue
				}
				if alias == "" {
					alias = name
				}
				pf.ReExports = append(pf.ReExports, graph.ReExport{Source: source, Name: name, ExportedName: alias})
			}
		}
		addTypeScriptReExport(nodeText(export, content), &pf.Scope.Imports)
		if wildcard := firstChild(export, "*"); wildcard != nil {
			pf.ReExports = append(pf.ReExports, graph.ReExport{Source: source, Wildcard: true})
		} else if wildcard := firstChild(export, "namespace_export"); wildcard != nil {
			alias := nodeText(childByFieldName(wildcard, "name"), content)
			if alias == "" {
				alias = nodeText(firstChild(wildcard, "identifier"), content)
			}
			pf.ReExports = append(pf.ReExports, graph.ReExport{Source: source, ExportedName: alias, Wildcard: true})
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
			tsAddClass(child, module, false, content, pf)
		case "interface_declaration":
			tsAddType(child, module, "interface", false, content, pf)
		case "type_alias_declaration":
			tsAddType(child, module, "type", false, content, pf)
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
	isDefault := strings.Contains(nodeText(node, content), "export default")
	for i := range int(node.ChildCount()) {
		child := node.Child(i)
		switch child.Type() {
		case "function_declaration":
			tsAddFunction(child, module, container, true, content, pf)
			if isDefault {
				if name := nodeText(childByFieldName(child, "name"), content); name != "" {
					pf.Scope.Imports = append(pf.Scope.Imports, graph.ScopeImport{ImportedName: "default", LocalName: name, Kind: graph.ScopeImportDefault, ReExport: true})
				}
			}
		case "class_declaration":
			tsAddClass(child, module, true, content, pf)
			if isDefault {
				if name := nodeText(childByFieldName(child, "name"), content); name != "" {
					pf.Scope.Imports = append(pf.Scope.Imports, graph.ScopeImport{ImportedName: "default", LocalName: name, Kind: graph.ScopeImportDefault, ReExport: true})
				}
			}
		case "interface_declaration":
			tsAddType(child, module, "interface", true, content, pf)
		case "type_alias_declaration":
			tsAddType(child, module, "type", true, content, pf)
		case "lexical_declaration", "variable_declaration":
			tsExtractArrowFunctions(child, module, container, true, content, pf)
		}
	}
}

func tsVisibility(exported bool) string {
	if exported {
		return "public"
	}
	return "private"
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
	vis := "private"
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

func tsAddClass(node *sitter.Node, module string, exported bool, content []byte, pf *graph.ParsedFile) {
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
		Visibility:    tsVisibility(exported),
		Range:         nodeRange(node),
		DocSummary:    prevCommentText(node, content),
		StableKey:     "type:typescript:" + module + ":" + name,
	})

	body := childByFieldName(node, "body")
	if body != nil {
		tsExtractSymbols(body, module, name, content, pf)
	}
}

func tsAddType(node *sitter.Node, module, kind string, exported bool, content []byte, pf *graph.ParsedFile) {
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
		Visibility:    tsVisibility(exported),
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

func tsExtractCalls(root *sitter.Node, content []byte, imported, bindable map[string]bool, pf *graph.ParsedFile) {
	for _, call := range findDescendants(root, "call_expression") {
		fnNode := childByFieldName(call, "function")
		if fnNode == nil {
			continue
		}
		name, ok := tsCalleeName(fnNode, content)
		if !ok {
			continue
		}
		baseName := name
		if idx := strings.LastIndexByte(name, '.'); idx >= 0 {
			baseName = name[idx+1:]
		}
		if tsKeywords[baseName] {
			continue
		}
		line := int(call.StartPoint().Row) + 1
		evidence := name
		if head, _, _ := strings.Cut(name, "."); bindable[head] && tsCallShadowed(call, name, content, imported) {
			evidence = graph.TypeScriptCallLocalBindingEvidence
		}
		pf.Edges = append(pf.Edges, graph.Edge{
			SrcSymbolID: 0,
			DstName:     name,
			Kind:        "calls",
			Evidence:    evidence,
			Line:        line,
			Col:         int(fnNode.StartPoint().Column) + 1,
		})
		rng := nodeRange(call)
		rng.StartCol = int(fnNode.StartPoint().Column) + 1
		pf.References = append(pf.References, graph.Reference{
			Kind:          "call",
			Name:          name,
			QualifiedName: name,
			Range:         rng,
		})
	}
}

func tsCalleeName(node *sitter.Node, content []byte) (string, bool) {
	if node == nil {
		return "", false
	}
	switch node.Type() {
	case "identifier", "property_identifier", "private_property_identifier":
		name := nodeText(node, content)
		return name, name != ""
	case "member_expression":
		object := childByFieldName(node, "object")
		property := childByFieldName(node, "property")
		if object == nil || property == nil || node.ChildByFieldName("optional") != nil || node.ChildByFieldName("optional_chain") != nil {
			return "", false
		}
		left, ok := tsCalleeName(object, content)
		if !ok || property.Type() != "property_identifier" {
			return "", false
		}
		right := nodeText(property, content)
		if right == "" {
			return "", false
		}
		return left + "." + right, true
	default:
		return "", false
	}
}

// tsCallShadowed reports whether a scope between the call and the module
// binds the call's first name itself, so no import or module declaration of
// that name is proven to be the callee. It over-approximates: a scope binds a
// name wherever in it the declaration sits (let, const and class are in their
// temporal dead zone before it, var and function declarations are hoisted),
// and a var or function declaration anywhere in a function body counts for the
// whole function. A module-level declaration of an imported name is a
// redeclaration error, so it shadows too.
func tsCallShadowed(call *sitter.Node, name string, content []byte, imported map[string]bool) bool {
	head, _, _ := strings.Cut(name, ".")
	for node := call.Parent(); node != nil; node = node.Parent() {
		switch node.Type() {
		case "program":
			return imported[head] && tsScopeDeclares(node, head, content, true, true)
		case "statement_block", "switch_body":
			if tsScopeDeclares(node, head, content, false, true) {
				return true
			}
		case "for_statement", "for_in_statement":
			if tsPatternBinds(childByFieldName(node, "initializer"), head, content) || tsPatternBinds(childByFieldName(node, "left"), head, content) {
				return true
			}
		case "catch_clause":
			if tsPatternBinds(childByFieldName(node, "parameter"), head, content) {
				return true
			}
		case "class", "function", "function_expression", "generator_function":
			// A named class or function expression binds its own name inside.
			if nodeText(childByFieldName(node, "name"), content) == head {
				return true
			}
			fallthrough
		case "function_declaration", "generator_function_declaration", "arrow_function", "method_definition":
			if tsPatternBinds(childByFieldName(node, "parameters"), head, content) || tsPatternBinds(childByFieldName(node, "parameter"), head, content) {
				return true
			}
			if body := childByFieldName(node, "body"); body != nil && tsScopeDeclares(body, head, content, true, false) {
				return true
			}
		}
	}
	return false
}

// tsScopeDeclares reports whether node declares name. lexical covers the let,
// const, class and function declarations directly in node; hoisted covers var
// and function declarations anywhere below node outside nested functions.
func tsScopeDeclares(node *sitter.Node, name string, content []byte, hoisted, lexical bool) bool {
	var walk func(n *sitter.Node, top bool) bool
	walk = func(n *sitter.Node, top bool) bool {
		for i := range int(n.NamedChildCount()) {
			child := n.NamedChild(i)
			switch child.Type() {
			case "export_statement":
				if walk(child, top) {
					return true
				}
				continue
			case "lexical_declaration", "class_declaration", "abstract_class_declaration", "enum_declaration":
				if top && lexical && tsDeclarationBinds(child, name, content) {
					return true
				}
				continue
			case "function_declaration", "generator_function_declaration":
				if (top || hoisted) && nodeText(childByFieldName(child, "name"), content) == name {
					return true
				}
				continue
			case "variable_declaration":
				if hoisted && tsDeclarationBinds(child, name, content) {
					return true
				}
				continue
			case "switch_case", "switch_default":
				// A case clause's declarations belong to the switch body's scope.
				if walk(child, top) {
					return true
				}
				continue
			case "function", "function_expression", "arrow_function", "generator_function", "class", "class_body", "method_definition":
				continue
			}
			if hoisted && walk(child, false) {
				return true
			}
		}
		return false
	}
	return walk(node, true)
}

func tsDeclarationBinds(decl *sitter.Node, name string, content []byte) bool {
	if n := childByFieldName(decl, "name"); n != nil && decl.Type() != "lexical_declaration" && decl.Type() != "variable_declaration" {
		return nodeText(n, content) == name
	}
	for i := range int(decl.NamedChildCount()) {
		if d := decl.NamedChild(i); d.Type() == "variable_declarator" && tsPatternBinds(childByFieldName(d, "name"), name, content) {
			return true
		}
	}
	return false
}

// tsPatternBinds reports whether a binding pattern or parameter list binds
// name. Default values and type annotations bind nothing, so they are skipped.
func tsPatternBinds(node *sitter.Node, name string, content []byte) bool {
	if node == nil {
		return false
	}
	switch node.Type() {
	case "identifier", "shorthand_property_identifier_pattern":
		return nodeText(node, content) == name
	case "assignment_pattern", "object_assignment_pattern":
		return tsPatternBinds(childByFieldName(node, "left"), name, content)
	case "required_parameter", "optional_parameter":
		return tsPatternBinds(childByFieldName(node, "pattern"), name, content)
	case "pair_pattern":
		return tsPatternBinds(childByFieldName(node, "value"), name, content)
	case "type_annotation", "lexical_declaration", "variable_declaration":
		if node.Type() != "type_annotation" {
			return tsDeclarationBinds(node, name, content)
		}
		return false
	}
	for i := range int(node.NamedChildCount()) {
		if tsPatternBinds(node.NamedChild(i), name, content) {
			return true
		}
	}
	return false
}
