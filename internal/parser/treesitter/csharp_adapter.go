//go:build cgo

package treesitter

import (
	"context"
	"path/filepath"
	"sort"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	csharp "github.com/smacker/go-tree-sitter/csharp"

	"github.com/isink17/codegraph/internal/graph"
)

// CSharpAdapter parses C# source files using tree-sitter.
type CSharpAdapter struct{}

func NewCSharp() *CSharpAdapter { return &CSharpAdapter{} }

func (a *CSharpAdapter) Language() string     { return "csharp" }
func (a *CSharpAdapter) Extensions() []string { return []string{".cs"} }

func (a *CSharpAdapter) Supports(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".cs")
}

func (a *CSharpAdapter) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	root, err := parse(ctx, csharp.GetLanguage(), content)
	if err != nil {
		return graph.ParsedFile{}, err
	}

	module := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	pf := graph.ParsedFile{
		Language:   "csharp",
		FileTokens: computeFileTokens(content),
	}

	csExtractImports(root, "", content, &pf)
	csExtractSymbols(root, "", "", content, &pf)
	if namespaces := csNamespaces(root, content); len(namespaces) == 1 && csTruthfulFileNamespace(pf.Symbols, namespaces[0]) {
		pf.Scope.Package = namespaces[0]
	}
	csExtractCalls(root, content, &pf)
	linkTestsGeneric(module, &pf, func(target string) string {
		return "func:csharp:" + testTargetModule(module, "Test", "Tests") + ":" + target
	})
	return pf, nil
}

func csExtractImports(node *sitter.Node, owner string, content []byte, pf *graph.ParsedFile) {
	for i := range int(node.ChildCount()) {
		child := node.Child(i)
		switch child.Type() {
		case "using_directive":
			text := strings.TrimSpace(strings.TrimSuffix(nodeText(child, content), ";"))
			global := strings.HasPrefix(text, "global ")
			text = strings.TrimSpace(strings.TrimPrefix(text, "global"))
			text = strings.TrimSpace(strings.TrimPrefix(text, "using"))
			static := strings.HasPrefix(text, "static ")
			text = strings.TrimSpace(strings.TrimPrefix(text, "static"))
			local, source := "", text
			kind := graph.ScopeImportNamed
			if eq := strings.Index(source, "="); eq >= 0 {
				local = strings.TrimSpace(source[:eq])
				source = strings.TrimSpace(source[eq+1:])
				kind = "alias"
			} else {
				kind = "namespace"
				if dot := strings.LastIndexByte(source, '.'); dot >= 0 {
					local = source[dot+1:]
				} else {
					local = source
				}
			}
			if static {
				kind = "static"
			}
			if global {
				kind = "global_" + kind
			}
			if source != "" {
				pf.Imports = append(pf.Imports, source)
				pf.Scope.Imports = append(pf.Scope.Imports, graph.ScopeImport{SourceSpecifier: source, ImportedName: source, LocalName: local, Kind: kind, Static: static, OwnerModule: owner})
			}
		case "namespace_declaration":
			name := nodeText(childByFieldName(child, "name"), content)
			if body := childByFieldName(child, "body"); body != nil {
				csExtractImports(body, csJoinQName(owner, name), content, pf)
			}
		case "ERROR":
			csExtractImports(child, owner, content, pf)
		}
	}
}

func csExtractSymbols(node *sitter.Node, module, container string, content []byte, pf *graph.ParsedFile) {
	for i := range int(node.ChildCount()) {
		child := node.Child(i)
		switch child.Type() {
		case "class_declaration", "interface_declaration", "struct_declaration",
			"enum_declaration", "record_declaration":
			csAddType(child, module, container, content, pf)
		case "method_declaration", "constructor_declaration":
			csAddMethod(child, module, container, content, pf)
		case "namespace_declaration":
			name := nodeText(childByFieldName(child, "name"), content)
			body := childByFieldName(child, "body")
			if body != nil {
				csExtractSymbols(body, csJoinQName(module, name), container, content, pf)
			}
		case "file_scoped_namespace_declaration":
			module = nodeText(childByFieldName(child, "name"), content)
		case "declaration_list":
			csExtractSymbols(child, module, container, content, pf)
		case "ERROR":
			next := module
			if prefix := csErrorNamespacePrefix(child, content); prefix != "" {
				next = csJoinQName(module, prefix)
			}
			csExtractSymbols(child, next, container, content, pf)
		}
	}
}

func csAddType(node *sitter.Node, module, parent string, content []byte, pf *graph.ParsedFile) {
	nameNode := childByFieldName(node, "name")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, content)

	qualified := csJoinQName(parent, name)
	container := parent
	if parent == "" {
		qualified = csJoinQName(module, name)
		container = module
	}
	if container == "" {
		qualified, container = name, ""
	}
	pfsym := graph.Symbol{
		Language:      "csharp",
		Kind:          "type",
		Name:          name,
		QualifiedName: qualified,
		ContainerName: container,
		Visibility:    csTypeVisibility(node, content),
		Range:         nodeRange(node),
		DocSummary:    prevCommentText(node, content),
		StableKey:     "type:csharp:" + qualified,
	}
	pf.Symbols = append(pf.Symbols, pfsym)

	body := childByFieldName(node, "body")
	if body != nil {
		csCollectMemberBindings(body, qualified, content, pf)
		csExtractSymbols(body, module, qualified, content, pf)
	}
}

func csAddMethod(node *sitter.Node, module, container string, content []byte, pf *graph.ParsedFile) {
	nameNode := childByFieldName(node, "name")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, content)
	effectiveContainer := container
	qualified := csJoinQName(container, name)
	if qualified == "" {
		qualified = name
	}

	vis := csMethodVisibility(node, content)

	signature := csDeclarationSignature(node, content)
	stableKey := "func:csharp:" + qualified + ":" + signature
	p := graph.Symbol{
		Language:      "csharp",
		Kind:          "function",
		Name:          name,
		QualifiedName: qualified,
		ContainerName: effectiveContainer,
		Visibility:    vis,
		Range:         nodeRange(node),
		DocSummary:    prevCommentText(node, content),
		StableKey:     stableKey,
		Signature:     signature,
		Static:        csStatic(node, content),
	}
	p.ArityMin, p.ArityMax = csDeclarationArity(node, content)
	pf.Symbols = append(pf.Symbols, p)
	csCollectBindings(node, stableKey, content, pf)
}

func csDeclarationArity(node *sitter.Node, content []byte) (*int, *int) {
	params := childByFieldName(node, "parameters")
	if params == nil {
		return nil, nil
	}
	var items []*sitter.Node
	paramsIndex := -1
	for i := range int(params.ChildCount()) {
		child := params.Child(i)
		if child.Type() == "params" {
			paramsIndex = i
		}
		if child.Type() == "parameter" {
			items = append(items, child)
		}
	}
	if paramsIndex >= 0 {
		for i := paramsIndex + 1; i < int(params.ChildCount()); i++ {
			if params.Child(i).Type() == "," || params.Child(i).Type() == "parameter" {
				return nil, nil
			}
		}
		if paramsIndex+2 >= int(params.ChildCount()) || params.Child(paramsIndex+1).Type() != "array_type" {
			return nil, nil
		}
		minPtr, maxPtr := len(items), -1
		return &minPtr, &maxPtr
	}
	min, max := 0, 0
	optionalSeen := false
	for i, parameter := range items {
		isParams := false
		for j := range int(parameter.ChildCount()) {
			child := parameter.Child(j)
			text := nodeText(child, content)
			if text == "params" {
				isParams = true
			}
			if (child.Type() == "modifier" || child.Type() == "ref" || child.Type() == "out" || child.Type() == "in") && (text == "ref" || text == "out" || text == "in") {
				return nil, nil
			}
		}
		if isParams {
			if i != len(items)-1 {
				return nil, nil
			}
			minPtr, maxPtr := min, -1
			return &minPtr, &maxPtr
		}
		optional := childByFieldName(parameter, "default_value") != nil
		if !optional {
			for j := range int(parameter.ChildCount()) {
				if parameter.Child(j).Type() == "=" {
					optional = true
					break
				}
			}
		}
		if optional {
			optionalSeen = true
		} else if optionalSeen {
			return nil, nil
		}
		max++
		if !optional {
			min++
		}
	}
	minPtr, maxPtr := min, max
	return &minPtr, &maxPtr
}

func csJoinQName(parts ...string) string {
	var out []string
	for _, part := range parts {
		if part = strings.Trim(part, ". "); part != "" {
			out = append(out, part)
		}
	}
	return strings.Join(out, ".")
}

func csNamespaces(root *sitter.Node, content []byte) []string {
	seen := map[string]struct{}{}
	var walk func(*sitter.Node, string)
	walk = func(node *sitter.Node, parent string) {
		for i := range int(node.ChildCount()) {
			child := node.Child(i)
			switch child.Type() {
			case "namespace_declaration":
				name := csJoinQName(parent, nodeText(childByFieldName(child, "name"), content))
				if name != "" {
					seen[name] = struct{}{}
				}
				if body := childByFieldName(child, "body"); body != nil {
					walk(body, name)
				}
			case "file_scoped_namespace_declaration":
				if name := csJoinQName(parent, nodeText(childByFieldName(child, "name"), content)); name != "" {
					seen[name] = struct{}{}
				}
			case "ERROR":
				next := parent
				if prefix := csErrorNamespacePrefix(child, content); prefix != "" {
					next = csJoinQName(parent, prefix)
				}
				walk(child, next)
			}
		}
	}
	walk(root, "")
	for name := range seen {
		for other := range seen {
			if name != other && strings.HasPrefix(other, name+".") {
				delete(seen, name)
				break
			}
		}
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func csErrorNamespacePrefix(node *sitter.Node, content []byte) string {
	if len(findDescendants(node, "namespace_declaration")) == 0 {
		return ""
	}
	for i := range int(node.ChildCount()) {
		child := node.Child(i)
		if child.Type() == "identifier" {
			return nodeText(child, content)
		}
	}
	return ""
}

func csDeclarationSignature(node *sitter.Node, content []byte) string {
	end := node.EndByte()
	if body := childByFieldName(node, "body"); body != nil {
		end = body.StartByte()
	}
	return strings.Join(strings.Fields(string(content[node.StartByte():end])), " ")
}

func csStatic(node *sitter.Node, content []byte) *bool {
	static := false
	for i := range int(node.ChildCount()) {
		if strings.TrimSpace(nodeText(node.Child(i), content)) == "static" {
			static = true
		}
	}
	return &static
}

func csAddBinding(pf *graph.ParsedFile, name, owner string) {
	if name == "" {
		return
	}
	pf.Scope.Imports = append(pf.Scope.Imports, graph.ScopeImport{LocalName: name, Kind: graph.ScopeImportLocalBinding, OwnerModule: owner})
}

func csAddTypedBinding(pf *graph.ParsedFile, name, typeName, owner string) {
	if name == "" {
		return
	}
	if typeName == "" {
		csAddBinding(pf, name, owner)
		return
	}
	pf.Scope.Imports = append(pf.Scope.Imports, graph.ScopeImport{LocalName: name, SourceSpecifier: typeName, ImportedName: typeName, Kind: graph.ScopeImportTypedBinding, OwnerModule: owner})
}

func csSupportedType(node *sitter.Node, content []byte) string {
	if node == nil {
		return ""
	}
	text := strings.TrimSpace(nodeText(node, content))
	if text == "" || text == "var" || text == "dynamic" {
		return ""
	}
	if strings.ContainsAny(text, "[]?*<>(){},") || strings.Contains(text, "=>") {
		return ""
	}
	if strings.HasPrefix(text, "global::") {
		text = strings.TrimPrefix(text, "global::")
	}
	if text == "" {
		return ""
	}
	for _, part := range strings.Split(text, ".") {
		if part == "" {
			return ""
		}
		for i, r := range part {
			if !(r == '_' || r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || i > 0 && r >= '0' && r <= '9') {
				return ""
			}
		}
	}
	return strings.TrimSpace(nodeText(node, content))
}

func csTypeParameterNames(node *sitter.Node, content []byte) map[string]struct{} {
	set := map[string]struct{}{}
	for n := node; n != nil; n = n.Parent() {
		for _, p := range findDescendants(n, "type_parameter") {
			if parent := p.Parent(); parent != nil && (parent == n || parent.Type() == "type_parameter_list") {
				set[nodeText(p, content)] = struct{}{}
			}
		}
		if n.Type() == "class_declaration" || n.Type() == "struct_declaration" || n.Type() == "interface_declaration" || n.Type() == "record_declaration" {
			break
		}
	}
	return set
}

func csBindingType(node, valueNode *sitter.Node, content []byte, typeParams map[string]struct{}) string {
	typeNode := childByFieldName(node, "type")
	if typeNode == nil {
		return ""
	}
	typeName := csSupportedType(typeNode, content)
	if _, blocked := typeParams[typeName]; blocked {
		return ""
	}
	if typeName != "" {
		return typeName
	}
	if typeNode.Type() != "implicit_type" {
		return ""
	}
	value := childByFieldName(valueNode, "value")
	if value == nil {
		for i := range int(valueNode.ChildCount()) {
			candidate := valueNode.Child(i)
			if candidate.Type() == "object_creation_expression" {
				value = candidate
				break
			}
		}
	}
	if value == nil || value.Type() != "object_creation_expression" {
		return ""
	}
	return csSupportedType(childByFieldName(value, "type"), content)
}

func csOwnedByMethod(root, node *sitter.Node) bool {
	for parent := node.Parent(); parent != nil; parent = parent.Parent() {
		if parent == root {
			return true
		}
		if parent.Type() == "method_declaration" || parent.Type() == "constructor_declaration" {
			return false
		}
	}
	return false
}

func csCollectBindings(node *sitter.Node, owner string, content []byte, pf *graph.ParsedFile) {
	typeParams := csTypeParameterNames(node, content)
	for _, p := range findDescendants(node, "parameter") {
		if !csOwnedByMethod(node, p) {
			continue
		}
		csAddTypedBinding(pf, nodeText(childByFieldName(p, "name"), content), csBindingType(p, p, content, typeParams), owner)
	}
	for _, declaration := range findDescendants(node, "variable_declaration") {
		if !csOwnedByMethod(node, declaration) {
			continue
		}
		for _, v := range findDescendants(declaration, "variable_declarator") {
			csAddTypedBinding(pf, nodeText(childByFieldName(v, "name"), content), csBindingType(declaration, v, content, typeParams), owner)
		}
	}
	for _, v := range findDescendants(node, "foreach_statement") {
		if !csOwnedByMethod(node, v) {
			continue
		}
		csAddTypedBinding(pf, nodeText(childByFieldName(v, "left"), content), csSupportedType(childByFieldName(v, "type"), content), owner)
	}
	for _, v := range findDescendants(node, "catch_declaration") {
		if !csOwnedByMethod(node, v) {
			continue
		}
		csAddTypedBinding(pf, nodeText(childByFieldName(v, "name"), content), csSupportedType(childByFieldName(v, "type"), content), owner)
	}
	for _, v := range findDescendants(node, "local_function_statement") {
		csAddBinding(pf, nodeText(childByFieldName(v, "name"), content), owner)
	}
}

func csCollectMemberBindings(body *sitter.Node, owner string, content []byte, pf *graph.ParsedFile) {
	for i := range int(body.ChildCount()) {
		child := body.Child(i)
		switch child.Type() {
		case "field_declaration", "event_field_declaration":
			declaration := childByFieldName(child, "declaration")
			if declaration == nil {
				for i := range int(child.ChildCount()) {
					if child.Child(i).Type() == "variable_declaration" {
						declaration = child.Child(i)
						break
					}
				}
			}
			typeName := csSupportedType(childByFieldName(declaration, "type"), content)
			for _, v := range findDescendants(child, "variable_declarator") {
				csAddTypedBinding(pf, nodeText(childByFieldName(v, "name"), content), typeName, owner)
			}
		case "property_declaration":
			csAddTypedBinding(pf, nodeText(childByFieldName(child, "name"), content), csSupportedType(childByFieldName(child, "type"), content), owner)
		}
	}
}

func csMethodVisibility(node *sitter.Node, content []byte) string {
	for i := 0; i < int(node.ChildCount()); i++ {
		c := node.Child(i)
		if c.Type() == "modifier" || c.Type() == "access_modifier" {
			text := nodeText(c, content)
			switch text {
			case "public":
				return "public"
			case "private":
				return "private"
			case "protected":
				return "protected"
			case "internal":
				return "module"
			}
		}
	}
	return "private"
}

func csTypeVisibility(node *sitter.Node, content []byte) string {
	for i := 0; i < int(node.ChildCount()); i++ {
		c := node.Child(i)
		if c.Type() != "modifier" && c.Type() != "access_modifier" {
			continue
		}
		switch nodeText(c, content) {
		case "public":
			return "public"
		case "private":
			return "private"
		case "protected":
			return "protected"
		case "internal":
			return "module"
		}
	}
	for parent := node.Parent(); parent != nil; parent = parent.Parent() {
		switch parent.Type() {
		case "class_declaration", "interface_declaration", "struct_declaration", "enum_declaration", "record_declaration":
			return "private"
		}
	}
	return "module"
}

func csTruthfulFileNamespace(symbols []graph.Symbol, namespace string) bool {
	if namespace == "" || len(symbols) == 0 {
		return false
	}
	for _, symbol := range symbols {
		if symbol.QualifiedName != namespace && !strings.HasPrefix(symbol.QualifiedName, namespace+".") {
			return false
		}
	}
	return true
}

func csExtractCalls(root *sitter.Node, content []byte, pf *graph.ParsedFile) {
	for _, call := range findDescendants(root, "invocation_expression") {
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
		arity, safe := csCallArity(call, fnNode)
		pf.Edges = append(pf.Edges, graph.Edge{
			SrcSymbolID: 0,
			DstName:     name,
			Kind:        "calls",
			Evidence:    name,
			Line:        line,
			CallArity:   callArityIfSafe(arity, safe),
		})
		pf.References = append(pf.References, graph.Reference{
			Kind:          "call",
			Name:          name,
			QualifiedName: name,
			Range:         nodeRange(call),
		})
	}
}

func callArityIfSafe(arity int, safe bool) *int {
	if !safe {
		return nil
	}
	return &arity
}

func csCallArity(call, function *sitter.Node) (int, bool) {
	arguments := childByFieldName(call, "arguments")
	if arguments == nil {
		return 0, false
	}
	if len(findDescendants(function, "type_argument_list")) > 0 || len(findDescendants(function, "type_arguments")) > 0 {
		return 0, false
	}
	count := 0
	safe := true
	for _, kind := range []string{"named_argument", "ref_argument", "out_argument", "in_argument", "ref", "out", "in", "name_colon", "name_equals", "argument_name"} {
		if len(findDescendants(arguments, kind)) > 0 {
			safe = false
		}
	}
	for i := range int(arguments.ChildCount()) {
		child := arguments.Child(i)
		switch child.Type() {
		case ",", "(", ")":
			continue
		case "named_argument", "ref_argument", "out_argument", "in_argument", "name_colon", "name_equals", "argument_name":
			safe = false
		}
		count++
	}
	return count, safe
}
