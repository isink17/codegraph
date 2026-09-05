//go:build cgo

package treesitter

import (
	sitter "github.com/smacker/go-tree-sitter"

	"github.com/isink17/codegraph/internal/graph"
)

// goScopeNodes are the constructs that open a Go lexical scope. A binding is
// recorded against the innermost one that encloses it, which is what keeps one
// function's locals out of another's and a closure's parameters out of the
// function around it.
var goScopeNodes = map[string]bool{
	"function_declaration":        true,
	"method_declaration":          true,
	"func_literal":                true,
	"block":                       true,
	"if_statement":                true,
	"for_statement":               true,
	"expression_switch_statement": true,
	"type_switch_statement":       true,
	"select_statement":            true,
	"expression_case":             true,
	"default_case":                true,
	"type_case":                   true,
	"communication_case":          true,
}

// goCollectLocals is the tree-sitter twin of the go/ast collector in
// internal/parser/golang. Both must record the same names over the same line
// ranges with the same proven types: the receiver-scope resolver is a
// high-confidence strategy, and a strategy that decides differently depending
// on which parser built the database is not one.
func goCollectLocals(root *sitter.Node, content []byte, imports map[string]string) []graph.GoLocalBinding {
	var out []graph.GoLocalBinding

	emit := func(name string, scope *sitter.Node, typeNode *sitter.Node, forcePointer bool) {
		if name == "" || name == "_" || scope == nil {
			return
		}
		b := graph.GoLocalBinding{
			Name:           name,
			ScopeStartLine: int(scope.StartPoint().Row) + 1,
			ScopeEndLine:   int(scope.EndPoint().Row) + 1,
		}
		if typeNode != nil {
			b.TypeName, b.TypePackage, b.Pointer = goTypeSpellingNode(typeNode, content)
			if forcePointer && b.TypeName != "" {
				b.Pointer = true
			}
			if b.TypePackage != "" {
				b.TypeImportPath = imports[b.TypePackage]
			}
		}
		out = append(out, b)
	}

	var walk func(n *sitter.Node, scope *sitter.Node)
	walk = func(n *sitter.Node, scope *sitter.Node) {
		if n == nil {
			return
		}
		switch n.Type() {
		case "parameter_declaration", "variadic_parameter_declaration":
			// Receivers, parameters and named results all take this shape. The
			// enclosing scope is the declaration or literal, never its body,
			// so the name covers the signature too.
			//
			// The same shape also occurs inside a *type*: `var fn func(json
			// []byte)` and `interface{ Do(json []byte) }` both carry a
			// parameter_declaration whose name binds nothing at all. Recording
			// it would invent a local that suppresses the import rewrite for a
			// genuine `json.Marshal()` later in the block, which go/ast never
			// does.
			if !goBindsParameters(n) {
				break
			}
			typeNode := childByFieldName(n, "type")
			for _, name := range goFieldChildren(n, "name") {
				emit(nodeText(name, content), scope, typeNode, false)
			}
		case "var_spec", "const_spec":
			typeNode := childByFieldName(n, "type")
			names := goFieldChildren(n, "name")
			if typeNode == nil {
				// `var x = Store{}` proves a type the same way `x := Store{}`
				// does; every other initializer proves nothing.
				values := goFieldChildren(n, "value")
				if len(values) == len(names) {
					for i, name := range names {
						lit, ptr := goCompositeLiteralType(values[i], content)
						emit(nodeText(name, content), scope, lit, ptr)
					}
					break
				}
			}
			for _, name := range names {
				emit(nodeText(name, content), scope, typeNode, false)
			}
		case "short_var_declaration":
			left := childByFieldName(n, "left")
			right := childByFieldName(n, "right")
			lhs := goExpressionListItems(left)
			rhs := goExpressionListItems(right)
			paired := len(lhs) == len(rhs)
			for i, item := range lhs {
				if item.Type() != "identifier" {
					continue
				}
				var typeNode *sitter.Node
				var ptr bool
				if paired {
					typeNode, ptr = goCompositeLiteralType(rhs[i], content)
				}
				emit(nodeText(item, content), scope, typeNode, ptr)
			}
		case "range_clause":
			// The element type comes from the ranged expression. That is
			// inference, not syntax, so range variables are recorded untyped.
			for _, item := range goExpressionListItems(childByFieldName(n, "left")) {
				if item.Type() == "identifier" {
					emit(nodeText(item, content), scope, nil, false)
				}
			}
		case "type_switch_statement":
			// `switch v := x.(type)` rebinds v per clause, so it is untyped
			// here even where one clause would prove a type.
			for i := range int(n.ChildCount()) {
				child := n.Child(i)
				if child.Type() == "expression_list" {
					for _, item := range goExpressionListItems(child) {
						if item.Type() == "identifier" {
							emit(nodeText(item, content), n, nil, false)
						}
					}
					break
				}
			}
		}

		next := scope
		if goScopeNodes[n.Type()] {
			next = n
		}
		for i := range int(n.ChildCount()) {
			walk(n.Child(i), next)
		}
	}

	walk(root, nil)
	return out
}

// goBindsParameters reports whether a parameter declaration belongs to a
// construct that actually binds its parameter names -- a function or method
// declaration, or a function literal -- rather than to a function type or an
// interface method signature, which name parameters without binding anything.
func goBindsParameters(n *sitter.Node) bool {
	list := n.Parent()
	if list == nil {
		return false
	}
	owner := list.Parent()
	if owner == nil {
		return false
	}
	switch owner.Type() {
	case "function_declaration", "method_declaration", "func_literal":
		return true
	default:
		return false
	}
}

// goFieldChildren returns every child carrying the given field name. Go's
// grammar repeats `name` for `a, b int` and `value` for `var a, b = x, y`, so
// ChildByFieldName alone would see only the first.
func goFieldChildren(n *sitter.Node, field string) []*sitter.Node {
	if n == nil {
		return nil
	}
	var out []*sitter.Node
	for i := range int(n.ChildCount()) {
		if n.FieldNameForChild(i) == field {
			out = append(out, n.Child(i))
		}
	}
	return out
}

func goExpressionListItems(n *sitter.Node) []*sitter.Node {
	if n == nil {
		return nil
	}
	if n.Type() != "expression_list" {
		return []*sitter.Node{n}
	}
	var out []*sitter.Node
	for i := range int(n.NamedChildCount()) {
		out = append(out, n.NamedChild(i))
	}
	return out
}

// goCompositeLiteralType mirrors compositeLiteralType in the go/ast adapter:
// `Store{}` and `&Store{}` name their own type, and nothing else does.
func goCompositeLiteralType(n *sitter.Node, content []byte) (*sitter.Node, bool) {
	if n == nil {
		return nil, false
	}
	switch n.Type() {
	case "composite_literal":
		return childByFieldName(n, "type"), false
	case "unary_expression":
		operand := childByFieldName(n, "operand")
		if operand == nil || operand.Type() != "composite_literal" {
			return nil, false
		}
		if nodeText(childByFieldName(n, "operator"), content) != "&" {
			return nil, false
		}
		return childByFieldName(operand, "type"), true
	}
	return nil, false
}

// goTypeSpellingNode reduces a type expression to its bare name, written package
// qualifier, and pointerness. Shapes with no named type identity -- slices,
// maps, channels, funcs, interfaces, anonymous structs, arrays -- yield an
// empty name and prove nothing.
func goTypeSpellingNode(n *sitter.Node, content []byte) (name, pkg string, pointer bool) {
	if n == nil {
		return "", "", false
	}
	switch n.Type() {
	case "pointer_type":
		inner := n.NamedChild(0)
		name, pkg, _ = goTypeSpellingNode(inner, content)
		return name, pkg, true
	case "parenthesized_type":
		return goTypeSpellingNode(n.NamedChild(0), content)
	case "type_identifier":
		return nodeText(n, content), "", false
	case "qualified_type":
		pkgNode := childByFieldName(n, "package")
		nameNode := childByFieldName(n, "name")
		if pkgNode == nil || nameNode == nil {
			return "", "", false
		}
		return nodeText(nameNode, content), nodeText(pkgNode, content), false
	case "generic_type":
		return goTypeSpellingNode(childByFieldName(n, "type"), content)
	}
	return "", "", false
}
