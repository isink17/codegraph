//go:build cgo

package treesitter

import (
	"context"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	php "github.com/smacker/go-tree-sitter/php"

	"github.com/isink17/codegraph/internal/graph"
)

type PHPAdapter struct{}

func NewPHP() *PHPAdapter                       { return &PHPAdapter{} }
func (a *PHPAdapter) Language() string          { return "php" }
func (a *PHPAdapter) Extensions() []string      { return []string{".php"} }
func (a *PHPAdapter) Supports(path string) bool { return strings.EqualFold(filepath.Ext(path), ".php") }

func (a *PHPAdapter) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	root, err := parse(ctx, php.GetLanguage(), content)
	if err != nil {
		return graph.ParsedFile{}, err
	}
	p := graph.ParsedFile{Language: "php", FileTokens: computeFileTokens(content)}
	phpExtractImports(root, content, &p)
	namespaces, globalDecl := 0, false
	var onlyNamespace string
	phpExtractSymbols(root, "", "", content, &p, &namespaces, &globalDecl, &onlyNamespace)
	if namespaces == 1 && !globalDecl {
		p.Scope.Package = onlyNamespace
	}
	phpExtractCalls(root, content, &p)
	phpLinkTests(&p)
	return p, nil
}

// namespace_definition has name/body fields for braced namespaces, but no
// body for semicolon namespaces; following program siblings keep that scope.
// namespace_use_declaration contains namespace_use_clause or namespace_use_group.
// scoped_call_expression uses scope/name; member calls use object/name.
func phpExtractImports(root *sitter.Node, content []byte, pf *graph.ParsedFile) {
	var walk func(*sitter.Node, string)
	walk = func(node *sitter.Node, current string) {
		for i := range int(node.ChildCount()) {
			child := node.Child(i)
			switch child.Type() {
			case "namespace_definition":
				ns := phpSemanticQName(nodeText(childByFieldName(child, "name"), content))
				if body := childByFieldName(child, "body"); body != nil {
					walk(body, ns)
				} else {
					current = ns
				}
			case "namespace_use_declaration":
				phpAddNamespaceUses(child, current, content, pf)
			case "program", "compound_statement":
				walk(child, current)
			}
		}
	}
	walk(root, "")
	for _, call := range findDescendants(root, "include_expression") {
		arg := call.Child(int(call.ChildCount()) - 1)
		if arg != nil {
			if val := strings.Trim(nodeText(arg, content), `"'`); val != "" {
				pf.Imports = append(pf.Imports, val)
			}
		}
	}
}

func phpAddNamespaceUses(node *sitter.Node, owner string, content []byte, pf *graph.ParsedFile) {
	kind := phpUseKind(node, "php_type")
	prefix := ""
	if n := firstChild(node, "namespace_name"); n != nil {
		prefix = nodeText(n, content)
	}
	for i := range int(node.ChildCount()) {
		child := node.Child(i)
		switch child.Type() {
		case "namespace_use_clause":
			phpAddUseClause(child, "", phpUseKind(child, kind), owner, content, pf)
		case "namespace_use_group":
			for j := range int(child.ChildCount()) {
				if clause := child.Child(j); clause.Type() == "namespace_use_group_clause" {
					phpAddUseClause(clause, prefix, phpUseKind(clause, kind), owner, content, pf)
				}
			}
		}
	}
}

// The pinned PHP grammar exposes anonymous `function` and `const` tokens as
// direct children. Declaration-level tokens provide inheritance; mixed group
// clauses carry their own child token. Comments and whitespace have no effect.
func phpUseKind(node *sitter.Node, inherited string) string {
	for i := range int(node.ChildCount()) {
		switch node.Child(i).Type() {
		case "function":
			return "php_function"
		case "const":
			return "php_const"
		}
	}
	return inherited
}

func phpAddUseClause(node *sitter.Node, prefix, kind, owner string, content []byte, pf *graph.ParsedFile) {
	nameNode := firstChild(node, "qualified_name")
	if nameNode == nil {
		nameNode = firstChild(node, "namespace_name")
	}
	if nameNode == nil {
		return
	}
	imported := nodeText(nameNode, content)
	if prefix != "" {
		imported = strings.TrimSuffix(prefix, `\`) + `\` + imported
	}
	semantic := phpSemanticQName(imported)
	parts := strings.Split(semantic, ".")
	if len(parts) == 0 || parts[len(parts)-1] == "" {
		return
	}
	local := parts[len(parts)-1]
	if alias := firstChild(node, "namespace_aliasing_clause"); alias != nil {
		if n := firstChild(alias, "name"); n != nil {
			local = nodeText(n, content)
		}
	}
	pf.Imports = append(pf.Imports, imported)
	pf.Scope.Imports = append(pf.Scope.Imports, graph.ScopeImport{
		SourceSpecifier: semantic, ImportedName: parts[len(parts)-1], LocalName: local,
		Kind: kind, OwnerModule: owner,
	})
}

func phpExtractSymbols(node *sitter.Node, namespace, container string, content []byte, pf *graph.ParsedFile, namespaces *int, globalDecl *bool, onlyNamespace *string) {
	for i := range int(node.ChildCount()) {
		child := node.Child(i)
		switch child.Type() {
		case "namespace_definition":
			ns := phpSemanticQName(nodeText(childByFieldName(child, "name"), content))
			(*namespaces)++
			*onlyNamespace = ns
			if body := childByFieldName(child, "body"); body != nil {
				phpExtractSymbols(body, ns, "", content, pf, namespaces, globalDecl, onlyNamespace)
			} else {
				namespace = ns
			}
		case "class_declaration", "interface_declaration", "trait_declaration", "enum_declaration":
			if namespace == "" {
				*globalDecl = true
			}
			phpAddType(child, namespace, content, pf, namespaces, globalDecl, onlyNamespace)
		case "method_declaration":
			phpAddFunction(child, namespace, container, true, content, pf)
		case "function_definition":
			if namespace == "" && container == "" {
				*globalDecl = true
			}
			phpAddFunction(child, namespace, container, container != "", content, pf)
		case "program", "compound_statement", "declaration_list":
			phpExtractSymbols(child, namespace, container, content, pf, namespaces, globalDecl, onlyNamespace)
		}
	}
}

func phpAddType(node *sitter.Node, namespace string, content []byte, pf *graph.ParsedFile, namespaces *int, globalDecl *bool, onlyNamespace *string) {
	nameNode := childByFieldName(node, "name")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, content)
	qualified := phpJoinQName(namespace, name)
	pf.Symbols = append(pf.Symbols, graph.Symbol{Language: "php", Kind: "type", Name: name, QualifiedName: qualified,
		ContainerName: namespace, Visibility: "public", Range: nodeRange(node), DocSummary: prevCommentText(node, content), StableKey: "type:php:" + qualified})
	if node.Type() == "trait_declaration" {
		pf.Scope.Imports = append(pf.Scope.Imports, graph.ScopeImport{Kind: graph.ScopeImportPHPTraitScope, OwnerModule: qualified})
	}
	if body := childByFieldName(node, "body"); body != nil {
		phpExtractPropertyFacts(body, qualified, content, pf)
		phpExtractSymbols(body, namespace, qualified, content, pf, namespaces, globalDecl, onlyNamespace)
	}
}

func phpAddFunction(node *sitter.Node, namespace, container string, method bool, content []byte, pf *graph.ParsedFile) {
	nameNode := childByFieldName(node, "name")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, content)
	qualified := phpJoinQName(namespace, name)
	if container != "" {
		qualified = phpJoinQName(container, name)
	}
	p := graph.Symbol{Language: "php", Kind: "function", Name: name, QualifiedName: qualified,
		ContainerName: containerOrNamespace(container, namespace), Visibility: "public", Range: nodeRange(node),
		DocSummary: prevCommentText(node, content), StableKey: "func:php:" + qualified}
	if method {
		p.Visibility, p.Static = phpMethodVisibility(node, content), phpStatic(node)
	}
	pf.Symbols = append(pf.Symbols, p)
	if method {
		phpExtractPromotedPropertyFacts(childByFieldName(node, "parameters"), container, content, pf)
	}
}

func phpExtractPropertyFacts(body *sitter.Node, owner string, content []byte, pf *graph.ParsedFile) {
	for i := range int(body.ChildCount()) {
		child := body.Child(i)
		if child.Type() == "property_declaration" {
			phpAddPropertyFacts(child, owner, content, pf)
		}
	}
}

func phpExtractPromotedPropertyFacts(parameters *sitter.Node, owner string, content []byte, pf *graph.ParsedFile) {
	if parameters == nil {
		return
	}
	for i := range int(parameters.ChildCount()) {
		child := parameters.Child(i)
		if child.Type() == "property_promotion_parameter" {
			phpAddPropertyFact(child, owner, content, pf)
		}
	}
}

func phpAddPropertyFacts(node *sitter.Node, owner string, content []byte, pf *graph.ParsedFile) {
	static := false
	for i := range int(node.ChildCount()) {
		if node.Child(i).Type() == "static_modifier" {
			static = true
		}
	}
	for i := range int(node.ChildCount()) {
		child := node.Child(i)
		if child.Type() != "property_element" {
			continue
		}
		phpAddPropertyFactWithType(child, node, owner, static, content, pf)
	}
}

func phpAddPropertyFact(node *sitter.Node, owner string, content []byte, pf *graph.ParsedFile) {
	phpAddPropertyFactWithType(node, node, owner, false, content, pf)
}

func phpAddPropertyFactWithType(element, declaration *sitter.Node, owner string, static bool, content []byte, pf *graph.ParsedFile) {
	variable := childByFieldName(element, "name")
	if variable == nil {
		variable = childByFieldName(element, "variable_name")
	}
	if variable == nil {
		variable = firstChild(element, "variable_name")
	}
	if variable == nil {
		return
	}
	name := strings.TrimPrefix(nodeText(variable, content), "$")
	if name == "" {
		return
	}
	typeNode := childByFieldName(declaration, "type")
	typeName := ""
	if typeNode != nil && typeNode.Type() == "named_type" && !static {
		typeName = strings.TrimSpace(nodeText(typeNode, content))
	}
	kind := graph.ScopeImportLocalBinding
	if typeName != "" {
		kind = graph.ScopeImportTypedBinding
	}
	pf.Scope.Imports = append(pf.Scope.Imports, graph.ScopeImport{LocalName: name, SourceSpecifier: typeName, Kind: kind, OwnerModule: owner})
}

func containerOrNamespace(container, namespace string) string {
	if container != "" {
		return container
	}
	return namespace
}

func phpMethodVisibility(node *sitter.Node, content []byte) string {
	for i := range int(node.ChildCount()) {
		if c := node.Child(i); c.Type() == "visibility_modifier" {
			return nodeText(c, content)
		}
	}
	return "public"
}

func phpStatic(node *sitter.Node) *bool {
	static := false
	for i := range int(node.ChildCount()) {
		if node.Child(i).Type() == "static_modifier" {
			static = true
			break
		}
	}
	return &static
}

func phpLinkTests(pf *graph.ParsedFile) {
	for i, sym := range pf.Symbols {
		if sym.Kind != "function" || sym.ContainerName != "" {
			continue
		}
		target := ""
		switch {
		case strings.HasPrefix(sym.Name, "test_"):
			target = strings.TrimPrefix(sym.Name, "test_")
		case strings.HasPrefix(sym.Name, "Test") && testNameUpperBoundary(sym.Name[4:]):
			target = sym.Name[4:]
		case strings.HasPrefix(sym.Name, "test") && testNameLowerBoundary(sym.Name[4:]):
			target = sym.Name[4:]
		}
		if target == "" {
			continue
		}
		pf.TestLinks = append(pf.TestLinks, graph.TestLink{
			TestName: sym.QualifiedName, TargetName: target, Reason: "test_name_match", Score: 0.7,
			TestSymbolKey: sym.StableKey, TargetStableKey: "func:php:" + target, TestSymbolIndex: intRef(i),
		})
	}
}

func phpExtractCalls(root *sitter.Node, content []byte, pf *graph.ParsedFile) {
	add := func(call *sitter.Node, name string, nested bool) {
		if name == "" {
			return
		}
		evidence := name
		if nested && strings.HasPrefix(name, "$this->") {
			evidence = graph.PHPMemberCallNestedScopeEvidence
		}
		pf.Edges = append(pf.Edges, graph.Edge{DstName: name, Kind: "calls", Evidence: evidence, Line: int(call.StartPoint().Row) + 1})
		pf.References = append(pf.References, graph.Reference{Kind: "call", Name: name, QualifiedName: name, Range: nodeRange(call)})
	}
	var walk func(*sitter.Node, bool)
	walk = func(node *sitter.Node, nested bool) {
		if node == nil {
			return
		}
		switch node.Type() {
		case "anonymous_function_creation_expression", "arrow_function", "function_definition":
			nested = true
		case "function_call_expression":
			fn := childByFieldName(node, "function")
			if fn == nil && node.ChildCount() > 0 {
				fn = node.Child(0)
			}
			if fn != nil {
				add(node, nodeText(fn, content), nested)
			}
		case "scoped_call_expression":
			scope, name := childByFieldName(node, "scope"), childByFieldName(node, "name")
			if scope != nil && name != nil {
				add(node, nodeText(scope, content)+"::"+nodeText(name, content), nested)
			}
		case "member_call_expression", "nullsafe_member_call_expression":
			name, object := childByFieldName(node, "name"), childByFieldName(node, "object")
			if name != nil && object != nil {
				op := "->"
				if node.Type() == "nullsafe_member_call_expression" {
					op = "?->"
				}
				add(node, nodeText(object, content)+op+nodeText(name, content), nested)
			}
		}
		for i := range int(node.ChildCount()) {
			walk(node.Child(i), nested)
		}
	}
	walk(root, false)
}

// Normalize only syntax-proven PHP names; arbitrary runtime strings stay raw.
func phpSemanticQName(source string) string {
	return strings.Trim(strings.ReplaceAll(strings.TrimSpace(source), `\`, "."), ".")
}
func phpJoinQName(container, name string) string {
	return phpSemanticQName(strings.Trim(container, ".") + "." + strings.Trim(name, "."))
}
