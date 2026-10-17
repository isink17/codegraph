//go:build cgo

package treesitter

import (
	sitter "github.com/smacker/go-tree-sitter"

	"github.com/isink17/codegraph/internal/classify"
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
	type initRef struct {
		index                         int
		callee, qualifier, importPath string
		line                          int
	}
	var inits []initRef

	// declared, when non-nil, is the declaring var_spec, const_spec or short
	// variable declaration; the binding starts on its line, as in the go/ast
	// adapter's declare.
	emit := func(name string, scope, declared *sitter.Node, typeNode *sitter.Node, forcePointer bool) bool {
		if name == "" || name == "_" || scope == nil {
			return false
		}
		start := int(scope.StartPoint().Row) + 1
		if declared != nil {
			start = max(start, int(declared.StartPoint().Row)+1)
		}
		b := graph.GoLocalBinding{
			Name:           name,
			ScopeStartLine: start,
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
		return true
	}
	// initFrom mirrors goLocalCollector.initFrom in the go/ast adapter.
	initFrom := func(value *sitter.Node) {
		if items := goExpressionListItems(value); len(items) == 1 {
			value = items[0]
		}
		if value == nil || value.Type() != "call_expression" || childByFieldName(value, "type_arguments") != nil {
			return
		}
		ref := initRef{index: len(out) - 1, line: int(value.StartPoint().Row) + 1}
		fn := childByFieldName(value, "function")
		switch {
		case fn == nil:
			return
		case fn.Type() == "identifier":
			ref.callee = nodeText(fn, content)
		case fn.Type() == "selector_expression":
			operand := childByFieldName(fn, "operand")
			field := childByFieldName(fn, "field")
			if operand == nil || field == nil || operand.Type() != "identifier" {
				return
			}
			ref.qualifier = nodeText(operand, content)
			if ref.importPath = imports[ref.qualifier]; ref.importPath == "" {
				return
			}
			ref.callee = nodeText(field, content)
		default:
			return
		}
		inits = append(inits, ref)
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
				emit(nodeText(name, content), scope, nil, typeNode, false)
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
						if emit(nodeText(name, content), scope, n, lit, ptr) && lit == nil && len(names) == 1 && n.Type() == "var_spec" {
							initFrom(values[i])
						}
					}
					break
				}
			}
			for _, name := range names {
				emit(nodeText(name, content), scope, n, typeNode, false)
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
				if emit(nodeText(item, content), scope, n, typeNode, ptr) && typeNode == nil && len(lhs) == 1 && len(rhs) == 1 {
					initFrom(rhs[0])
				}
			}
		case "range_clause":
			// The element type comes from the ranged expression. That is
			// inference, not syntax, so range variables are recorded untyped.
			for _, item := range goExpressionListItems(childByFieldName(n, "left")) {
				if item.Type() == "identifier" {
					emit(nodeText(item, content), scope, nil, nil, false)
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
							emit(nodeText(item, content), n, nil, nil, false)
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

	idx := newGoLocalIndex(out)
	for _, ref := range inits {
		shadow := ref.callee
		if ref.qualifier != "" {
			shadow = ref.qualifier
		}
		if idx.boundAt(shadow, ref.line) {
			continue
		}
		out[ref.index].InitFunction = ref.callee
		out[ref.index].InitImportPath = ref.importPath
	}
	for _, call := range findDescendants(root, "call_expression") {
		callee, inner, ok := goChainedCallNode(childByFieldName(call, "function"), content)
		if !ok || idx.boundAt(callee, int(inner.StartPoint().Row)+1) {
			continue
		}
		out = append(out, graph.GoLocalBinding{
			Name:           callee + "()",
			ScopeStartLine: int(inner.StartPoint().Row) + 1,
			ScopeEndLine:   int(inner.EndPoint().Row) + 1,
			InitFunction:   callee,
		})
	}
	return out
}

// goChainedCallNode is the tree-sitter twin of goChainedCall in the go/ast
// adapter: a selector made directly on a call of a bare function name.
func goChainedCallNode(fn *sitter.Node, content []byte) (string, *sitter.Node, bool) {
	if fn == nil || fn.Type() != "selector_expression" {
		return "", nil, false
	}
	inner := childByFieldName(fn, "operand")
	if inner == nil || inner.Type() != "call_expression" || childByFieldName(inner, "type_arguments") != nil {
		return "", nil, false
	}
	callee := childByFieldName(inner, "function")
	if callee == nil || callee.Type() != "identifier" {
		return "", nil, false
	}
	name := nodeText(callee, content)
	if _, builtin := classify.BuiltinCandidate("go", name); builtin {
		return "", nil, false
	}
	return name, inner, true
}

// goCollectFunctionResults mirrors collectGoFunctionResults in the go/ast
// adapter.
func goCollectFunctionResults(root *sitter.Node, content []byte, imports map[string]string) []graph.GoFunctionResult {
	var out []graph.GoFunctionResult
	for _, fn := range findChildren(root, "function_declaration") {
		if childByFieldName(fn, "type_parameters") != nil {
			continue
		}
		typeNode := childByFieldName(fn, "result")
		if typeNode == nil {
			continue
		}
		if typeNode.Type() == "parameter_list" {
			var params []*sitter.Node
			for i := range int(typeNode.NamedChildCount()) {
				if child := typeNode.NamedChild(i); child.Type() != "comment" {
					params = append(params, child)
				}
			}
			if len(params) != 1 || params[0].Type() != "parameter_declaration" || len(goFieldChildren(params[0], "name")) > 1 {
				continue
			}
			typeNode = childByFieldName(params[0], "type")
		}
		name, pkg, _ := goTypeSpellingNode(typeNode, content)
		if name == "" {
			continue
		}
		r := graph.GoFunctionResult{Function: nodeText(childByFieldName(fn, "name"), content), TypeName: name}
		if pkg != "" {
			if r.TypeImportPath = imports[pkg]; r.TypeImportPath == "" {
				continue
			}
		}
		out = append(out, r)
	}
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
