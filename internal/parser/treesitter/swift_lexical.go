//go:build cgo

package treesitter

import (
	"sort"

	sitter "github.com/smacker/go-tree-sitter"

	"github.com/isink17/codegraph/internal/graph"
)

// swiftExtractLexicalBindings records only blockers. It intentionally does not
// model local declarations as graph symbols or resolve aliases.
func swiftExtractLexicalBindings(root *sitter.Node, content []byte, pf *graph.ParsedFile) {
	if root == nil {
		return
	}
	fileEnd := 1
	if len(content) > 0 {
		fileEnd = 1
		for _, b := range content {
			if b == '\n' {
				fileEnd++
			}
		}
	}
	var walk func(*sitter.Node, *sitter.Node, string, int)
	walk = func(node, scope *sitter.Node, owner string, callableDepth int) {
		if node == nil {
			return
		}
		switch node.Type() {
		case "function_declaration":
			body := childByFieldName(node, "body")
			if body == nil {
				body = node
			}
			if callableDepth > 0 {
				if name := childByFieldName(node, "name"); name != nil {
					swiftAddLexicalBinding(pf, nodeText(name, content), graph.SwiftLexicalLocalFunction, owner, scope, fileEnd)
				}
			}
			for _, p := range findChildren(node, "parameter") {
				if name := childByFieldName(p, "name"); name != nil {
					swiftAddLexicalBinding(pf, nodeText(name, content), graph.SwiftLexicalParameter, owner, body, fileEnd)
				}
			}
			for _, p := range swiftTypeParameters(node) {
				swiftAddLexicalBinding(pf, nodeText(p, content), graph.SwiftLexicalGenericParameter, owner, body, fileEnd)
			}
			for i := 0; i < int(node.ChildCount()); i++ {
				walk(node.Child(i), body, owner, callableDepth+1)
			}
			return

		case "lambda_literal":
			for _, p := range swiftDirectLambdaParameters(node) {
				if name := childByFieldName(p, "name"); name != nil {
					swiftAddLexicalBinding(pf, nodeText(name, content), graph.SwiftLexicalParameter, owner, node, fileEnd)
				}
			}
			for i := 0; i < int(node.ChildCount()); i++ {
				walk(node.Child(i), node, owner, callableDepth+1)
			}
			return

		case "class_declaration":
			name := swiftDeclarationName(node, content)
			body := childByFieldName(node, "body")
			if body == nil {
				body = node
			}
			if callableDepth > 0 {
				swiftAddLexicalBinding(pf, name, graph.SwiftLexicalLocalNominal, owner, scope, fileEnd)
			}
			for _, p := range swiftTypeParameters(node) {
				swiftAddLexicalBinding(pf, nodeText(p, content), graph.SwiftLexicalGenericParameter, swiftJoinQName(owner, name), body, fileEnd)
			}
			for i := 0; i < int(node.ChildCount()); i++ {
				walk(node.Child(i), body, swiftJoinQName(owner, name), callableDepth)
			}
			return

		case "typealias_declaration":
			if name := childByFieldName(node, "name"); name != nil {
				scopeNode := scope
				if scopeNode == nil {
					scopeNode = root
				}
				swiftAddLexicalBinding(pf, nodeText(name, content), graph.SwiftLexicalTypealias, owner, scopeNode, fileEnd)
			}
			for _, p := range swiftTypeParameters(node) {
				swiftAddLexicalBinding(pf, nodeText(p, content), graph.SwiftLexicalGenericParameter, owner, node, fileEnd)
			}

		case "if_statement", "guard_statement", "while_statement":
			scopeNode := firstChild(node, "statements")
			if node.Type() == "guard_statement" {
				scopeNode = scope
			}
			for _, name := range swiftConditionPatternNames(node, content) {
				swiftAddLexicalBinding(pf, name, graph.SwiftLexicalValue, owner, scopeNode, fileEnd)
			}

		case "for_statement":
			for _, pattern := range findChildren(node, "pattern") {
				for _, name := range swiftPatternBindingNames(pattern, content) {
					swiftAddLexicalBinding(pf, name, graph.SwiftLexicalValue, owner, firstChild(node, "statements"), fileEnd)
				}
			}

		case "switch_entry":
			for _, pattern := range findChildren(node, "switch_pattern") {
				for _, name := range swiftPatternBindingNames(pattern, content) {
					swiftAddLexicalBinding(pf, name, graph.SwiftLexicalValue, owner, firstChild(node, "statements"), fileEnd)
				}
			}

		case "catch_block":
			for _, pattern := range findChildren(node, "pattern") {
				for _, name := range swiftPatternBindingNames(pattern, content) {
					swiftAddLexicalBinding(pf, name, graph.SwiftLexicalValue, owner, firstChild(node, "statements"), fileEnd)
				}
			}

		case "property_declaration":
			if callableDepth > 0 || scope != nil && scope.Type() != "source_file" {
				for _, name := range swiftBindingNames(node, content) {
					swiftAddLexicalBinding(pf, name, graph.SwiftLexicalValue, owner, scope, fileEnd)
				}
			}
		}
		for i := 0; i < int(node.ChildCount()); i++ {
			walk(node.Child(i), scope, owner, callableDepth)
		}
	}
	walk(root, nil, "", 0)
	sort.Slice(pf.Scope.SwiftLexicalBindings, func(i, j int) bool {
		a, b := pf.Scope.SwiftLexicalBindings[i], pf.Scope.SwiftLexicalBindings[j]
		if a.ScopeStartLine != b.ScopeStartLine {
			return a.ScopeStartLine < b.ScopeStartLine
		}
		if a.ScopeEndLine != b.ScopeEndLine {
			return a.ScopeEndLine < b.ScopeEndLine
		}
		if a.Name != b.Name {
			return a.Name < b.Name
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.OwnerModule < b.OwnerModule
	})
}

func swiftDirectLambdaParameters(node *sitter.Node) []*sitter.Node {
	var out []*sitter.Node
	for _, functionType := range findChildren(node, "lambda_function_type") {
		for _, parameters := range findChildren(functionType, "lambda_function_type_parameters") {
			out = append(out, findChildren(parameters, "lambda_parameter")...)
		}
	}
	return out
}

func swiftConditionPatternNames(node *sitter.Node, content []byte) []string {
	var names []string
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		switch child.Type() {
		case "value_binding_pattern":
			for j := i + 1; j < int(node.ChildCount()); j++ {
				next := node.Child(j)
				if next.Type() == "simple_identifier" {
					names = append(names, nodeText(next, content))
					break
				}
				if next.Type() == "value_binding_pattern" || next.Type() == "statements" {
					break
				}
			}
		case "pattern", "switch_pattern":
			names = append(names, swiftPatternBindingNames(child, content)...)
		}
	}
	return names
}

func swiftDeclarationName(node *sitter.Node, content []byte) string {
	if name := childByFieldName(node, "name"); name != nil {
		return nodeText(name, content)
	}
	return ""
}

func swiftTypeParameters(node *sitter.Node) []*sitter.Node {
	var out []*sitter.Node
	for _, group := range findChildren(node, "type_parameters") {
		out = append(out, findChildren(group, "type_parameter")...)
	}
	return out
}

func swiftAddLexicalBinding(pf *graph.ParsedFile, name, kind, owner string, scope *sitter.Node, fileEnd int) {
	if name == "" {
		return
	}
	start, end := 1, fileEnd
	if scope != nil {
		start = int(scope.StartPoint().Row) + 1
		end = int(scope.EndPoint().Row) + 1
	}
	pf.Scope.SwiftLexicalBindings = append(pf.Scope.SwiftLexicalBindings, graph.SwiftLexicalBinding{
		Name: name, Kind: kind, OwnerModule: owner, ScopeStartLine: start, ScopeEndLine: end,
	})
}

func swiftLexicalBindingBlocks(bindings []graph.SwiftLexicalBinding, name string, line int) bool {
	for _, binding := range bindings {
		if binding.Name == name && line >= binding.ScopeStartLine && line <= binding.ScopeEndLine {
			return true
		}
	}
	return false
}
