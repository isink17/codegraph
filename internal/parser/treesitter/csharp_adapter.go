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
	if namespaces := csNamespaces(root, content); len(namespaces) == 1 {
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
		Visibility:    heuristicVisibility(name),
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
	pf.Symbols = append(pf.Symbols, p)
	csCollectBindings(node, stableKey, content, pf)
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

func csCollectBindings(node *sitter.Node, owner string, content []byte, pf *graph.ParsedFile) {
	for _, p := range findDescendants(node, "parameter") {
		csAddBinding(pf, nodeText(childByFieldName(p, "name"), content), owner)
	}
	for _, v := range findDescendants(node, "variable_declarator") {
		csAddBinding(pf, nodeText(childByFieldName(v, "name"), content), owner)
	}
	for _, v := range findDescendants(node, "foreach_variable") {
		csAddBinding(pf, nodeText(v, content), owner)
	}
	for _, v := range findDescendants(node, "catch_declaration") {
		csAddBinding(pf, nodeText(v, content), owner)
	}
}

func csCollectMemberBindings(body *sitter.Node, owner string, content []byte, pf *graph.ParsedFile) {
	for i := range int(body.ChildCount()) {
		child := body.Child(i)
		switch child.Type() {
		case "field_declaration", "event_field_declaration":
			for _, v := range findDescendants(child, "variable_declarator") {
				csAddBinding(pf, nodeText(childByFieldName(v, "name"), content), owner)
			}
		case "property_declaration":
			csAddBinding(pf, nodeText(childByFieldName(child, "name"), content), owner)
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
