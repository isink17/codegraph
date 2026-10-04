//go:build cgo

package treesitter

import (
	"context"
	"path/filepath"
	"slices"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	rust "github.com/smacker/go-tree-sitter/rust"

	"github.com/isink17/codegraph/internal/graph"
)

// RustAdapter parses Rust source files using tree-sitter.
type RustAdapter struct{}

func NewRust() *RustAdapter { return &RustAdapter{} }

func (a *RustAdapter) Language() string     { return "rust" }
func (a *RustAdapter) Extensions() []string { return []string{".rs"} }

func (a *RustAdapter) Supports(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".rs")
}

func (a *RustAdapter) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	root, err := parse(ctx, rust.GetLanguage(), content)
	if err != nil {
		return graph.ParsedFile{}, err
	}

	module := rustModulePath(path)
	pf := graph.ParsedFile{
		Language:   "rust",
		Scope:      graph.ScopeEvidence{ModulePath: module},
		FileTokens: computeFileTokens(content),
	}

	rustExtractImports(root, module, content, &pf)
	rustExtractSymbols(root, module, "", path, content, &pf)
	rustExtractCalls(root, content, &pf)
	linkTestsGeneric(module, &pf, func(target string) string {
		return "func:rust:" + testTargetModule(module, "_test") + ":" + target
	})
	return pf, nil
}

func rustExtractImports(root *sitter.Node, module string, content []byte, pf *graph.ParsedFile) {
	for i := range int(root.ChildCount()) {
		child := root.Child(i)
		if child.Type() == "use_declaration" && !child.HasError() {
			if arg := childByFieldName(child, "argument"); arg != nil {
				pf.Imports = append(pf.Imports, nodeText(arg, content))
				rustUseTree(arg, "", rustVisibility(child, content) == "public", module, content, &pf.Scope.Imports)
			}
		}
		if child.Type() == "mod_item" {
			if name := childByFieldName(child, "name"); name != nil {
				if body := childByFieldName(child, "body"); body != nil {
					rustExtractImports(body, module+"::"+nodeText(name, content), content, pf)
				}
			}
		}
	}
}

// rustUseTree preserves syntax-tree grouping instead of splitting source on commas.
func rustUseTree(node *sitter.Node, prefix string, public bool, owner string, content []byte, out *[]graph.ScopeImport) {
	if node == nil || node.HasError() {
		return
	}
	join := func(path string) string {
		if path == "" {
			return ""
		}
		if prefix == "" {
			return path
		}
		if path == "self" {
			return prefix
		}
		return prefix + "::" + path
	}
	switch node.Type() {
	case "scoped_use_list":
		path := childByFieldName(node, "path")
		next := prefix
		if path != nil {
			next = join(rustUsePath(path, content))
			if next == "" {
				return
			}
		}
		rustUseTree(childByFieldName(node, "list"), next, public, owner, content, out)
		return
	case "use_list":
		for i := range int(node.NamedChildCount()) {
			rustUseTree(node.NamedChild(i), prefix, public, owner, content, out)
		}
		return
	}
	path, alias, wildcard := "", "", false
	switch node.Type() {
	case "use_as_clause":
		path = join(rustUsePath(childByFieldName(node, "path"), content))
		alias = nodeText(childByFieldName(node, "alias"), content)
		if alias == "" || alias == "_" {
			return
		}
	case "use_wildcard":
		wildcard = true
		path = prefix
		for i := range int(node.NamedChildCount()) {
			child := node.NamedChild(i)
			if child.Type() != "line_comment" && child.Type() != "block_comment" {
				path = join(rustUsePath(child, content))
				break
			}
		}
	case "identifier", "scoped_identifier", "self", "super", "crate":
		path = join(rustUsePath(node, content))
	default:
		return
	}
	if path == "" {
		return
	}
	imported := path
	if i := strings.LastIndex(path, "::"); i >= 0 {
		imported = path[i+2:]
	}
	local := imported
	if alias != "" {
		local = alias
	}
	if wildcard {
		local = ""
	}
	*out = append(*out, graph.ScopeImport{SourceSpecifier: path, ImportedName: imported, LocalName: local, Kind: graph.ScopeImportUse, Wildcard: wildcard, ReExport: public, OwnerModule: owner})
}

func rustUsePath(node *sitter.Node, content []byte) string {
	if node == nil || node.HasError() {
		return ""
	}
	switch node.Type() {
	case "scoped_identifier":
		path := rustUsePath(childByFieldName(node, "path"), content)
		name := rustUsePath(childByFieldName(node, "name"), content)
		if name == "" || (childByFieldName(node, "path") != nil && path == "") {
			return ""
		}
		return path + "::" + name
	case "identifier", "self", "super", "crate":
		return nodeText(node, content)
	default:
		return ""
	}
}

func rustModulePath(path string) string {
	p := filepath.ToSlash(path)
	parts := strings.Split(strings.Trim(p, "/"), "/")
	if len(parts) == 0 {
		return ""
	}
	base := strings.TrimSuffix(parts[len(parts)-1], filepath.Ext(parts[len(parts)-1]))
	if (base == "lib" || base == "main") && !strings.Contains(p, "/src/") {
		return "crate"
	}
	if filepath.IsAbs(path) && !strings.Contains(p, "/src/") && len(parts) > 1 {
		parts = parts[len(parts)-1:]
	}
	if base == "lib" || base == "main" || base == "mod" {
		parts = parts[:len(parts)-1]
	} else {
		parts[len(parts)-1] = base
	}
	for i, part := range parts {
		if part == "src" {
			parts = parts[i+1:]
			break
		}
	}
	if len(parts) > 0 && parts[0] == "bin" {
		parts = parts[1:]
	}
	if len(parts) == 0 {
		return "crate"
	}
	return "crate::" + strings.Join(parts, "::")
}

func rustExtractSymbols(node *sitter.Node, module, container, path string, content []byte, pf *graph.ParsedFile) {
	for i := range int(node.ChildCount()) {
		child := node.Child(i)
		switch child.Type() {
		case "function_item":
			rustAddFunction(child, module, container, content, pf)
		case "struct_item":
			rustAddType(child, module, "struct", content, pf)
		case "enum_item":
			rustAddType(child, module, "enum", content, pf)
		case "trait_item":
			rustAddType(child, module, "trait", content, pf)
		case "impl_item":
			rustExtractImpl(child, module, path, content, pf)
		case "const_item", "static_item", "function_signature_item", "foreign_mod_item", "macro_invocation", "expression_statement", "ERROR":
			// An impl body is walked with its type as container; its consts
			// and macros are the type's, not the module's.
			if container == "" {
				rustAddValueItems(child, module, content, pf)
			}
		case "mod_item":
			nameNode := childByFieldName(child, "name")
			body := childByFieldName(child, "body")
			if nameNode != nil {
				name := nodeText(nameNode, content)
				m := graph.RustModule{Name: name, OwnerModule: module, Inline: body != nil, Visibility: rustVisibility(child, content)}
				if body == nil {
					m.ExternalPath = name
					if base := rustModuleSourceBase(path); base != "" {
						m.ExternalPath = base + "/" + name
					}
				}
				pf.Scope.Modules = append(pf.Scope.Modules, m)
				if body != nil {
					rustExtractSymbols(body, module+"::"+name, "", path, content, pf)
				}
			}
		}
	}
}

// rustAddValueItems records what a module-level node declares in the value
// namespace without a symbol: a const, a static or an extern-block function or
// static, by name. A macro invocation, in the module or in an extern block, is
// recorded as an unknown expansion: its output is never read, so it may declare
// any item, a function of any name included. So is a node the grammar could
// not parse, which may hide a declaration. Invocations inside a function
// block are the block scope's concern (rustItemMacro), not the module's.
func rustAddValueItems(node *sitter.Node, module string, content []byte, pf *graph.ParsedFile) {
	add := func(name, kind string) {
		item := graph.RustValueItem{OwnerModule: module, Name: name, Kind: kind}
		if !slices.Contains(pf.Scope.RustValueItems, item) {
			pf.Scope.RustValueItems = append(pf.Scope.RustValueItems, item)
		}
	}
	switch node.Type() {
	case "const_item", "static_item", "function_signature_item":
		if name := childByFieldName(node, "name"); name != nil {
			add(nodeText(name, content), graph.RustValueItemDecl)
		}
	case "foreign_mod_item":
		if body := childByFieldName(node, "body"); body != nil {
			for i := range int(body.ChildCount()) {
				rustAddValueItems(body.Child(i), module, content, pf)
			}
		}
	case "expression_statement":
		if node.NamedChildCount() > 0 && node.NamedChild(0).Type() == "macro_invocation" {
			rustAddValueItems(node.NamedChild(0), module, content, pf)
		}
	case "macro_invocation":
		name := ""
		if macro := childByFieldName(node, "macro"); macro != nil {
			name = nodeText(macro, content)
		}
		add(name, graph.RustValueItemMacro)
	case "ERROR":
		add("", graph.RustValueItemMacro)
	}
}

func rustAddFunction(node *sitter.Node, module, container string, content []byte, pf *graph.ParsedFile) {
	nameNode := childByFieldName(node, "name")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, content)
	effectiveContainer := module
	if container != "" && container != module {
		effectiveContainer = container
	}
	qualified := module + "::" + name
	if container != "" && container != module {
		qualified = module + "::" + container + "::" + name
	}
	stableKey := "func:rust:" + module + ":" + name

	vis := rustVisibility(node, content)

	pf.Symbols = append(pf.Symbols, graph.Symbol{
		Language:      "rust",
		Kind:          "function",
		Name:          name,
		QualifiedName: qualified,
		ContainerName: effectiveContainer,
		Visibility:    vis,
		Range:         nodeRange(node),
		DocSummary:    prevCommentText(node, content),
		StableKey:     stableKey,
	})
}

func rustAddType(node *sitter.Node, module, kind string, content []byte, pf *graph.ParsedFile) {
	nameNode := childByFieldName(node, "name")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, content)

	vis := rustVisibility(node, content)

	pf.Symbols = append(pf.Symbols, graph.Symbol{
		Language:      "rust",
		Kind:          kind,
		Name:          name,
		QualifiedName: module + "::" + name,
		ContainerName: module,
		Visibility:    vis,
		Range:         nodeRange(node),
		DocSummary:    prevCommentText(node, content),
		StableKey:     "type:rust:" + module + ":" + name,
	})
}

func rustExtractImpl(node *sitter.Node, module, path string, content []byte, pf *graph.ParsedFile) {
	typeNode := childByFieldName(node, "type")
	if typeNode == nil {
		return
	}
	typeName := nodeText(typeNode, content)
	body := childByFieldName(node, "body")
	if body != nil {
		first := len(pf.Symbols)
		rustExtractSymbols(body, module, typeName, path, content, pf)
		// A trait impl's items have the trait's visibility, not their own,
		// and the parser does not know the trait's.
		if childByFieldName(node, "trait") != nil {
			for i := first; i < len(pf.Symbols); i++ {
				pf.Symbols[i].Visibility = graph.RustTraitImplVisibility
			}
		}
	}
}

// rustModuleSourceBase returns the directory, relative to the declaring file's
// own directory, that holds the file's out-of-line `mod name;` sources: "" for
// a directory owner (lib.rs, main.rs, mod.rs), whose modules are siblings, and
// the file's stem otherwise. It is the whole of what the parser knows about
// where a module lives; the store joins it with the declaring file's logical
// repository path, so nothing above that file -- least of all the native
// checkout root -- reaches rust_module_evidence.external_path. Only the file
// name is read from the native path, so a backslash in it stays filename data
// on POSIX.
func rustModuleSourceBase(path string) string {
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	if base == "lib" || base == "main" || base == "mod" {
		return ""
	}
	return base
}
func rustVisibility(node *sitter.Node, content []byte) string {
	vis := firstChild(node, "visibility_modifier")
	if vis != nil {
		text := strings.TrimSpace(nodeText(vis, content))
		if text == "pub" {
			return "public"
		}
		return "restricted:" + strings.TrimSuffix(strings.TrimPrefix(text, "pub("), ")")
	}
	return "private"
}

func rustExtractCalls(root *sitter.Node, content []byte, pf *graph.ParsedFile) {
	for _, call := range findDescendants(root, "call_expression") {
		fnNode := childByFieldName(call, "function")
		if fnNode == nil {
			continue
		}
		name := nodeText(fnNode, content)
		if name == "" {
			continue
		}
		line := int(call.StartPoint().Row) + 1
		evidence := name
		if rustCallShadowed(call, name, content) {
			evidence = graph.RustCallBlockScopeEvidence
		}
		pf.Edges = append(pf.Edges, graph.Edge{
			SrcSymbolID: 0,
			DstName:     name,
			Kind:        "calls",
			Evidence:    evidence,
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

// rustPathHead is the first segment of a called path, the name a block's own
// items can shadow. A global path, a qualified-self path and the keywords
// `crate`, `self`, `super` and `Self` name nothing a block declares, so they
// give "".
func rustPathHead(name string) string {
	head, _, _ := strings.Cut(name, "::")
	head = strings.TrimSpace(head)
	switch head {
	case "", "crate", "self", "super", "Self":
		return ""
	}
	if strings.ContainsAny(head, "<(.") {
		return ""
	}
	return head
}

// rustCallShadowed reports whether something the parser does not record as
// a module item may answer the call instead of one: the call sits in a `mod`
// declared inside a block, whose scope is not the file's module; or an
// enclosing block declares the path's first name itself (an item, an extern
// crate, a `use` naming it or a glob), or holds a statement macro that may
// declare it; or, for a bare name, a parameter or pattern binding of an
// enclosing function, closure, `let`, `for`, `match` arm or `if let` /
// `while let` names it. Each shadows the module's item of that name.
// Over-approximating scopes only leaves more calls unresolved.
func rustCallShadowed(call *sitter.Node, name string, content []byte) bool {
	head := rustPathHead(name)
	bare := head != "" && !strings.Contains(name, "::")
	inMod := false
	for node := call.Parent(); node != nil; node = node.Parent() {
		switch node.Type() {
		case "mod_item":
			inMod = true
		case "block":
			if inMod {
				// Only a `crate::` path means the same thing everywhere.
				return !strings.HasPrefix(strings.TrimSpace(name), "crate::")
			}
			if head == "" {
				continue
			}
			for i := range int(node.ChildCount()) {
				child := node.Child(i)
				if rustDeclares(child, head, content) || rustItemMacro(child, content) {
					return true
				}
				if bare && child.Type() == "let_declaration" && rustPatternBinds(childByFieldName(child, "pattern"), head, content) {
					return true
				}
			}
		case "function_item", "closure_expression":
			if bare && rustPatternBinds(childByFieldName(node, "parameters"), head, content) {
				return true
			}
		case "for_expression", "match_arm":
			if bare && rustPatternBinds(childByFieldName(node, "pattern"), head, content) {
				return true
			}
		case "if_expression", "while_expression":
			if bare && rustPatternBinds(childByFieldName(node, "condition"), head, content) {
				return true
			}
		}
	}
	return false
}

// rustPatternBinds reports whether any named leaf in a pattern, parameter
// list or condition spells name: an identifier, a struct-pattern shorthand
// (`P { f }`, a shorthand_field_identifier), or a binding inside `x @ ..`,
// `ref`, `mut`, tuple, slice or or-patterns. Paths, field names and types
// inside the pattern count too, which only refuses more.
func rustPatternBinds(node *sitter.Node, name string, content []byte) bool {
	if node == nil {
		return false
	}
	if node.NamedChildCount() == 0 {
		return nodeText(node, content) == name
	}
	for i := range int(node.NamedChildCount()) {
		if rustPatternBinds(node.NamedChild(i), name, content) {
			return true
		}
	}
	return false
}

// rustExpressionMacros are std macros that expand to an expression and never
// declare an item, so a statement calling one shadows nothing.
var rustExpressionMacros = map[string]bool{
	"assert": true, "assert_eq": true, "assert_ne": true, "debug_assert": true,
	"debug_assert_eq": true, "debug_assert_ne": true, "dbg": true, "eprint": true,
	"eprintln": true, "format": true, "format_args": true, "matches": true,
	"panic": true, "print": true, "println": true, "todo": true,
	"unimplemented": true, "unreachable": true, "vec": true, "write": true,
	"writeln": true,
}

// rustItemMacro reports whether a block statement is a macro call that may
// expand to items. A std expression macro is exempt unless the file defines
// a macro of that name, which would shadow std's.
func rustItemMacro(stmt *sitter.Node, content []byte) bool {
	if stmt.Type() == "expression_statement" && stmt.NamedChildCount() > 0 {
		stmt = stmt.NamedChild(0)
	}
	if stmt.Type() != "macro_invocation" {
		return false
	}
	macro := childByFieldName(stmt, "macro")
	if macro == nil || macro.Type() != "identifier" {
		return true
	}
	name := nodeText(macro, content)
	if !rustExpressionMacros[name] {
		return true
	}
	root := stmt
	for root.Parent() != nil {
		root = root.Parent()
	}
	for _, def := range findDescendants(root, "macro_definition") {
		if n := childByFieldName(def, "name"); n != nil && nodeText(n, content) == name {
			return true
		}
	}
	return false
}

func rustDeclares(item *sitter.Node, head string, content []byte) bool {
	switch item.Type() {
	case "use_declaration":
		arg := childByFieldName(item, "argument")
		if arg == nil || item.HasError() {
			return true // unreadable: it may import anything
		}
		var imports []graph.ScopeImport
		rustUseTree(arg, "", false, "", content, &imports)
		for _, im := range imports {
			if im.Wildcard || im.LocalName == head {
				return true
			}
		}
		return false
	case "extern_crate_declaration":
		if alias := childByFieldName(item, "alias"); alias != nil {
			return nodeText(alias, content) == head
		}
	case "foreign_mod_item":
		if body := childByFieldName(item, "body"); body != nil {
			for i := range int(body.ChildCount()) {
				if rustDeclares(body.Child(i), head, content) {
					return true
				}
			}
		}
		return false
	}
	name := childByFieldName(item, "name")
	return name != nil && nodeText(name, content) == head
}
