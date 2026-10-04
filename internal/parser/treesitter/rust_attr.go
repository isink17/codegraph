//go:build cgo

package treesitter

import (
	"slices"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"

	"github.com/isink17/codegraph/internal/graph"
)

// Attribute evidence for Rust items. Procedural derives and attribute macros
// are never expanded, so an item carrying one is only partly known: a derive
// may emit sibling items (a function of any name, shadowing a glob import),
// and an attribute macro may also rewrite, rename or drop the item it sits on.
// Only attributes proven to be the compiler's built-in, non-generative ones
// leave an item as written; every other attribute, and every derive, is
// recorded as unproven.

type rustAttrKind int

const (
	rustAttrNeutral rustAttrKind = iota // built-in, adds and changes nothing
	rustAttrDerive                      // may emit sibling items; the item itself is kept
	rustAttrRewrite                     // may emit, rewrite, rename or drop items
)

// rustBuiltinAttrs never emit items and leave the item they sit on as written
// (or, for cfg, remove it whole). cfg_attr is absent: it may name any attribute.
// path is absent too: it moves a `mod` to a file the parser does not evaluate.
var rustBuiltinAttrs = map[string]bool{
	"allow": true, "warn": true, "deny": true, "forbid": true, "expect": true,
	"inline": true, "doc": true, "cfg": true, "link_name": true, "link": true,
	"link_section": true, "must_use": true, "deprecated": true, "repr": true,
	"non_exhaustive": true, "no_mangle": true, "export_name": true, "used": true,
	"track_caller": true, "cold": true, "automatically_derived": true,
	"test": true, "ignore": true, "should_panic": true,
}

// rustAttrs is the file-wide evidence that a bare built-in attribute name may
// mean something else: a name some `use` (an alias included) or macro
// definition binds, or evidence that cannot be read (a `use` with a syntax
// error, `#[macro_use]`). A glob import is not counted: it could bring in a
// macro of that name, but counting every glob would refuse every `use super::*`
// test module. That is the documented recall/soundness ceiling for attributes;
// derives are never proven (see rustAttrItemKind).
type rustAttrs struct {
	names map[string]bool
	any   bool
}

func (a rustAttrs) shadowed(name string) bool { return a.any || a.names[name] }

func rustAttrScope(root *sitter.Node, content []byte) rustAttrs {
	a := rustAttrs{names: map[string]bool{}}
	for _, use := range findDescendants(root, "use_declaration") {
		arg := childByFieldName(use, "argument")
		if arg == nil || use.HasError() {
			a.any = true // unreadable: it may import anything
			continue
		}
		var imports []graph.ScopeImport
		rustUseTree(arg, "", false, "", content, &imports)
		for _, im := range imports {
			root, _, _ := strings.Cut(im.SourceSpecifier, "::")
			switch {
			case root == "std" || root == "core" || root == "alloc":
			case im.Wildcard:
				// Residual gap, by design: a glob that brings in a macro named
				// like a built-in attribute or derive is not detected. Counting
				// every glob would refuse every `use super::*` test module.
			default:
				a.names[im.LocalName] = true
			}
		}
	}
	for _, def := range findDescendants(root, "macro_definition") {
		if n := childByFieldName(def, "name"); n != nil {
			a.names[nodeText(n, content)] = true
		}
	}
	for _, item := range findDescendants(root, "attribute_item") {
		if rustAttrName(item, content) == "macro_use" {
			a.any = true
		}
	}
	return a
}

func rustAttrName(item *sitter.Node, content []byte) string {
	if attr := firstChild(item, "attribute"); attr != nil && attr.ChildCount() > 0 {
		return nodeText(attr.Child(0), content)
	}
	return ""
}

func isComment(n *sitter.Node) bool {
	return n.Type() == "line_comment" || n.Type() == "block_comment"
}

func rustAttrItemKind(item *sitter.Node, content []byte, attrs rustAttrs) rustAttrKind {
	attr := firstChild(item, "attribute")
	if attr == nil || attr.ChildCount() == 0 || item.HasError() {
		return rustAttrRewrite
	}
	name := nodeText(attr.Child(0), content)
	if attrs.shadowed(name) {
		return rustAttrRewrite
	}
	if name == "derive" {
		// No derive is proven built-in: a crate-root `#[macro_use] extern
		// crate` in another file, or a glob, may put a procedural derive named
		// Clone or Debug in scope, and one file cannot see either. Every derive
		// may emit sibling items. Documented recall ceiling.
		return rustAttrDerive
	}
	if rustBuiltinAttrs[name] {
		return rustAttrNeutral
	}
	return rustAttrRewrite
}

// rustItemAttrKind is the worst kind among the attributes directly before
// item. Comments between them are doc comments and are skipped; a let binding
// or expression statement is not an item and has none.
func rustItemAttrKind(item *sitter.Node, content []byte, attrs rustAttrs) rustAttrKind {
	switch item.Type() {
	case "attribute_item", "inner_attribute_item", "let_declaration", "expression_statement":
		return rustAttrNeutral
	}
	kind := rustAttrNeutral
	for prev := item.PrevSibling(); prev != nil; prev = prev.PrevSibling() {
		switch {
		case isComment(prev):
		case prev.Type() == "attribute_item":
			kind = max(kind, rustAttrItemKind(prev, content, attrs))
		default:
			return kind
		}
	}
	return kind
}

// rustAddAttrEvidence records what an unproven attribute on item leaves
// unknown. Its output may declare anything beside the item, so the module
// holds an unknown expansion (the same fact as an item macro). A rewriting
// attribute may also rename or drop the item, so the item, and every symbol
// below it, names no proven declaration: kind RustValueItemUnproven, named by
// the qualified name the symbol is persisted under (or a prefix of it).
func rustAddAttrEvidence(item *sitter.Node, module, container string, content []byte, attrs rustAttrs, pf *graph.ParsedFile) {
	kind := rustItemAttrKind(item, content, attrs)
	if kind == rustAttrNeutral {
		return
	}
	add := func(name, kind string) {
		ev := graph.RustValueItem{OwnerModule: module, Name: name, Kind: kind}
		if !slices.Contains(pf.Scope.RustValueItems, ev) {
			pf.Scope.RustValueItems = append(pf.Scope.RustValueItems, ev)
		}
	}
	// An attribute inside an impl emits into the impl only, never the module.
	if container == "" || item.Type() != "function_item" {
		add("", graph.RustValueItemMacro)
	}
	if kind != rustAttrRewrite {
		return
	}
	field := "name"
	if item.Type() == "impl_item" {
		field = "type"
	}
	n := childByFieldName(item, field)
	switch item.Type() {
	case "function_item", "struct_item", "enum_item", "trait_item", "impl_item", "mod_item":
	default:
		return
	}
	if n == nil {
		return
	}
	qualified := module + "::" + nodeText(n, content)
	if item.Type() == "function_item" && container != "" && container != module {
		qualified = module + "::" + container + "::" + nodeText(n, content)
	}
	add(qualified, graph.RustValueItemUnproven)
}
