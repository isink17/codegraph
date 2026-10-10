//go:build cgo

package treesitter

import (
	"strings"

	sitter "github.com/smacker/go-tree-sitter"

	"github.com/isink17/codegraph/internal/graph"
)

// javaLocalReceiverEvidence reports whether a call's receiver is a bare
// identifier that a local variable or parameter of the calling method,
// constructor or lambda declares before the call, and if so the evidence:
// graph.JavaLocalReceiverEvidence plus ";Type" when that declaration states
// a usable class or interface type. A receiver no such declaration binds (a
// field, a type name, a variable of an enclosing class body) is not marked.
func javaLocalReceiverEvidence(call, obj *sitter.Node, content []byte) (string, bool) {
	if obj == nil || obj.Type() != "identifier" {
		return "", false
	}
	name := nodeText(obj, content)
	found, typ := javaFindLocal(call, name, content)
	if !found {
		return "", false
	}
	spelling := javaReceiverTypeSpelling(typ, content)
	if spelling == "" || javaTypeVariableInScope(call, spelling, content) || javaLocalTypeShadows(call, spelling, content) {
		return graph.JavaLocalReceiverEvidence, true
	}
	return graph.JavaLocalReceiverEvidence + ";" + spelling, true
}

// javaFindLocal walks from node to the enclosing method, constructor or
// lambda chain and returns the declaration of name visible at node. Java
// forbids a local or parameter from redeclaring another in scope, so the
// first one found is the only one. A class body ends the walk: a variable
// beyond it may be shadowed by a field the class inherits. typ is nil for a
// declaration that spells no type.
func javaFindLocal(node *sitter.Node, name string, content []byte) (bool, *sitter.Node) {
	prev := node
	for n := node.Parent(); n != nil; prev, n = n, n.Parent() {
		switch n.Type() {
		case "program", "class_body", "interface_body", "enum_body", "enum_body_declarations", "annotation_type_body":
			return false, nil
		case "local_variable_declaration":
			// A call in a later declarator's initializer sees earlier ones.
			if typ, ok := javaDeclaratorBefore(n, prev, name, content); ok {
				return true, typ
			}
		case "for_statement", "block", "constructor_body", "switch_block_statement_group":
			for i := range int(n.NamedChildCount()) {
				child := n.NamedChild(i)
				if child.StartByte() >= prev.StartByte() {
					break
				}
				if child.Type() == "local_variable_declaration" {
					if typ, ok := javaDeclaratorBefore(child, nil, name, content); ok {
						return true, typ
					}
				}
			}
		case "switch_block":
			for i := range int(n.NamedChildCount()) {
				group := n.NamedChild(i)
				if group.StartByte() >= prev.StartByte() {
					break
				}
				for j := range int(group.NamedChildCount()) {
					if decl := group.NamedChild(j); decl.Type() == "local_variable_declaration" {
						if typ, ok := javaDeclaratorBefore(decl, nil, name, content); ok {
							return true, typ
						}
					}
				}
			}
		case "enhanced_for_statement":
			if javaSameNode(childByFieldName(n, "body"), prev) && javaNameIs(childByFieldName(n, "name"), name, content) {
				if childByFieldName(n, "dimensions") != nil {
					return true, nil
				}
				return true, childByFieldName(n, "type")
			}
		case "try_with_resources_statement":
			if !javaSameNode(childByFieldName(n, "body"), prev) {
				continue
			}
			if spec := childByFieldName(n, "resources"); spec != nil {
				if found, typ := javaResourceBefore(spec, nil, name, content); found {
					return true, typ
				}
			}
		case "resource_specification":
			if found, typ := javaResourceBefore(n, prev, name, content); found {
				return true, typ
			}
		case "catch_clause":
			if !javaSameNode(childByFieldName(n, "body"), prev) {
				continue
			}
			if param := firstChild(n, "catch_formal_parameter"); param != nil && javaNameIs(childByFieldName(param, "name"), name, content) {
				if ct := firstChild(param, "catch_type"); ct != nil && ct.NamedChildCount() == 1 {
					return true, ct.NamedChild(0)
				}
				return true, nil
			}
		case "lambda_expression":
			params := childByFieldName(n, "parameters")
			if params == nil || javaSameNode(params, prev) {
				continue
			}
			if params.Type() == "identifier" {
				if javaNameIs(params, name, content) {
					return true, nil
				}
				continue
			}
			if found, typ := javaParameter(params, name, content); found {
				return true, typ
			}
		case "method_declaration", "constructor_declaration", "compact_constructor_declaration":
			if params := childByFieldName(n, "parameters"); params != nil {
				if found, typ := javaParameter(params, name, content); found {
					return true, typ
				}
			}
			// A pattern binding (`o instanceof T x`) is flow-scoped; any
			// such binding of name in the body makes the receiver a local
			// whose type is not modelled here.
			if body := childByFieldName(n, "body"); body != nil {
				for _, inst := range findDescendants(body, "instanceof_expression") {
					if javaNameIs(childByFieldName(inst, "name"), name, content) {
						return true, nil
					}
				}
			}
			return false, nil
		}
	}
	return false, nil
}

func javaSameNode(a, b *sitter.Node) bool {
	return a != nil && b != nil && a.StartByte() == b.StartByte() && a.EndByte() == b.EndByte() && a.Type() == b.Type()
}

func javaNameIs(n *sitter.Node, name string, content []byte) bool {
	return n != nil && nodeText(n, content) == name
}

// javaDeclaratorBefore finds name among decl's declarators that end before
// stop (all of them when stop is nil). A declarator with its own dimensions
// (`Foo x[]`) declares an array: found, but no type.
func javaDeclaratorBefore(decl, stop *sitter.Node, name string, content []byte) (*sitter.Node, bool) {
	for i := range int(decl.NamedChildCount()) {
		d := decl.NamedChild(i)
		if stop != nil && d.StartByte() >= stop.StartByte() {
			break
		}
		if d.Type() != "variable_declarator" || !javaNameIs(childByFieldName(d, "name"), name, content) {
			continue
		}
		if childByFieldName(d, "dimensions") != nil {
			return nil, true
		}
		return childByFieldName(decl, "type"), true
	}
	return nil, false
}

func javaResourceBefore(spec, stop *sitter.Node, name string, content []byte) (bool, *sitter.Node) {
	for i := range int(spec.NamedChildCount()) {
		r := spec.NamedChild(i)
		if stop != nil && r.StartByte() >= stop.StartByte() {
			break
		}
		if r.Type() == "resource" && javaNameIs(childByFieldName(r, "name"), name, content) {
			if childByFieldName(r, "dimensions") != nil {
				return true, nil
			}
			return true, childByFieldName(r, "type")
		}
	}
	return false, nil
}

// javaParameter finds name among formal parameters. A varargs parameter is
// an array, and an inferred (untyped) lambda parameter states no type.
func javaParameter(params *sitter.Node, name string, content []byte) (bool, *sitter.Node) {
	for i := range int(params.NamedChildCount()) {
		p := params.NamedChild(i)
		switch p.Type() {
		case "formal_parameter":
			if javaNameIs(childByFieldName(p, "name"), name, content) {
				if childByFieldName(p, "dimensions") != nil {
					return true, nil
				}
				return true, childByFieldName(p, "type")
			}
		case "spread_parameter":
			if d := firstChild(p, "variable_declarator"); d != nil && javaNameIs(childByFieldName(d, "name"), name, content) {
				return true, nil
			}
		case "identifier":
			if javaNameIs(p, name, content) {
				return true, nil
			}
		}
	}
	return false, nil
}

// javaReceiverTypeSpelling is a declared type's class or interface name with
// its type arguments dropped, or "" for anything else: `var`, a primitive, an
// array, an annotated or malformed type.
func javaReceiverTypeSpelling(typ *sitter.Node, content []byte) string {
	if typ == nil || typ.HasError() {
		return ""
	}
	if typ.Type() == "generic_type" {
		if typ.NamedChildCount() == 0 {
			return ""
		}
		typ = typ.NamedChild(0)
	}
	switch typ.Type() {
	case "type_identifier", "scoped_type_identifier":
	default:
		return ""
	}
	var parts []string
	for _, id := range findDescendants(typ, "type_identifier") {
		parts = append(parts, nodeText(id, content))
	}
	if len(parts) == 0 || len(findDescendants(typ, "annotation"))+len(findDescendants(typ, "marker_annotation"))+len(findDescendants(typ, "type_arguments")) > 0 {
		return ""
	}
	spelling := strings.Join(parts, ".")
	if spelling == "var" {
		return ""
	}
	return spelling
}

// javaTypeVariableInScope reports whether the type spelling's first name is
// a type parameter of a method, constructor, class, interface or record
// enclosing node.
func javaTypeVariableInScope(node *sitter.Node, spelling string, content []byte) bool {
	head, _, _ := strings.Cut(spelling, ".")
	for n := node.Parent(); n != nil; n = n.Parent() {
		tps := childByFieldName(n, "type_parameters")
		if tps == nil {
			tps = firstChild(n, "type_parameters")
		}
		if tps == nil {
			continue
		}
		for i := range int(tps.NamedChildCount()) {
			if id := firstChild(tps.NamedChild(i), "type_identifier"); javaNameIs(id, head, content) {
				return true
			}
		}
	}
	return false
}

// javaTypeSignature is a type declaration's keyword and supertype clauses
// (`class extends Base implements I, J`), whitespace-normalized, with type
// arguments kept as spelled.
func javaTypeSignature(node *sitter.Node, content []byte) string {
	parts := []string{strings.TrimSuffix(node.Type(), "_declaration")}
	for i := range int(node.NamedChildCount()) {
		switch c := node.NamedChild(i); c.Type() {
		case "superclass", "super_interfaces", "extends_interfaces", "permits":
			parts = append(parts, strings.Join(strings.Fields(nodeText(c, content)), " "))
		}
	}
	return strings.Join(parts, " ")
}
