//go:build cgo

package treesitter

import (
	"context"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	clang "github.com/smacker/go-tree-sitter/c"
	cpp "github.com/smacker/go-tree-sitter/cpp"

	"github.com/isink17/codegraph/internal/graph"
)

// CppAdapter parses C and C++ source files using tree-sitter.
type CppAdapter struct{}

func NewCpp() *CppAdapter { return &CppAdapter{} }

func (a *CppAdapter) Language() string { return "cpp" }
func (a *CppAdapter) Extensions() []string {
	return []string{".c", ".h", ".cc", ".cpp", ".cxx", ".hpp", ".hh", ".hxx", ".ipp"}
}

func (a *CppAdapter) Supports(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".c", ".h", ".cc", ".cpp", ".cxx", ".hpp", ".hh", ".hxx", ".ipp":
		return true
	}
	return false
}

func (a *CppAdapter) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	ext := strings.ToLower(filepath.Ext(path))
	lang := cpp.GetLanguage()
	if ext == ".c" {
		lang = clang.GetLanguage()
	}

	root, err := parse(ctx, lang, content)
	if err != nil {
		return graph.ParsedFile{}, err
	}

	module := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	pf := graph.ParsedFile{
		Language:   "cpp",
		FileTokens: computeFileTokens(content),
	}

	cppExtractImports(root, content, &pf)
	cppExtractSymbols(root, module, "", "", filepath.ToSlash(path), content, &pf)
	cppExtractCalls(root, content, &pf)
	linkTestsGeneric(module, &pf, func(target string) string {
		return cppStableKey("func", testTargetModule(module, "_test"), target)
	})
	return pf, nil
}

func cppExtractImports(root *sitter.Node, content []byte, pf *graph.ParsedFile) {
	for _, inc := range findDescendants(root, "preproc_include") {
		pathNode := firstChild(inc, "string_literal")
		if pathNode == nil {
			pathNode = firstChild(inc, "system_lib_string")
		}
		if pathNode != nil {
			val := nodeText(pathNode, content)
			val = strings.Trim(val, `"<>`)
			if val != "" {
				pf.Imports = append(pf.Imports, val)
			}
		}
	}
}

var cppSkipFuncs = map[string]bool{
	"if": true, "for": true, "while": true, "switch": true,
	"catch": true, "sizeof": true, "return": true,
}

// cppTransparentDeclWrappers are the C/C++ nodes that *hold* declarations
// without owning any part of a declaration's identity (P22.15).
//
// cppExtractSymbols walks declaration scopes, not whole subtrees: it inspects
// the direct children of a scope and recurses only where recursion is
// structurally intentional. That is deliberate -- descending into every
// descendant would file-scope the locals of every function body -- but before
// P22.15 the only scopes it could enter were a namespace body and a class or
// struct body, so any declaration sitting one syntax wrapper deeper was
// invisible to the graph. Measured on `googletest/test/gtest_unittest.cc`
// (7870 lines): 15 symbols, none after line 314.
//
// Each node below is *transparent*: the declarations inside it are declarations
// of the enclosing scope, so recursion passes `module` and `container` through
// unchanged and the wrapper itself never becomes a symbol or a container.
//
//	preproc_if / preproc_ifdef / preproc_elif / preproc_else
//	    A conditional arm. CodeGraph is source-graph tooling with no build
//	    configuration, so it cannot know which arm a compiler would take -- and
//	    picking one by source order would be a guess. Every arm's declarations
//	    are therefore emitted; when two arms declare the same name the resolver's
//	    bare-name ambiguity veto (resolver_ambiguity.go) already refuses the
//	    call, which is the fail-closed answer, independent of branch order.
//	    `#ifndef` is also a preproc_ifdef, and an `#else`/`#elif` arm is a child
//	    of the arm it follows, so nesting is handled by the recursion itself.
//	template_declaration
//	    `template <...> <declaration>`. The wrapper carries the parameter list;
//	    the declaration inside is the symbol. No instantiation or type
//	    resolution is implied -- the declaration merely becomes visible.
//	linkage_specification
//	    `extern "C" { ... }` and `extern "C" void f();`. `"C"` is a linkage
//	    label, never a container, so nothing about identity changes.
//	declaration_list
//	    The braced body of a namespace or of a linkage specification. The
//	    namespace case reaches it through its own `body` field; listing it here
//	    is what lets `extern "C" { ... }` compose.
//	ERROR
//	    Tree-sitter's recovery node, and the reason gtest_unittest.cc stopped at
//	    line 314: the file's anonymous namespace is reparented under a single
//	    ERROR spanning lines 317-7870. Recovery is unavoidable in real C/C++
//	    because the grammar parses preprocessor directives structurally --
//	    `#if __has_warning("...")` is not a preprocessor expression it accepts,
//	    a macro invocation used as a statement is a declaration missing its `;`,
//	    and `googlemock/src/gmock_main.cc` opens a brace inside one `#ifdef` and
//	    closes it inside another. The declarations under such a node are real
//	    source declarations; only the *shape* around them is unparsed. Entering
//	    it acts only on the node types the switch below names, and each still
//	    has to supply the fields the emitters require. It is not free of cost:
//	    an unmatched `{` inside the region is a plain child of the ERROR node,
//	    sibling to the declarations after it, so a declaration the source wrote
//	    inside that brace can be emitted at file scope. That is accepted --
//	    the alternative is losing every declaration in the region -- and it is
//	    bounded to files the grammar could not parse at all.
//
// Deliberately NOT transparent, with the reason recorded rather than
// rediscovered later (the full parent/child census of fmt + googletest is the
// source of this list):
//
//	compound_statement, function_definition, for_statement, init_statement,
//	condition_clause, case_statement
//	    Executable bodies. A declaration inside one is a local, and emitting it
//	    at file or class scope would claim an ownership the source does not
//	    have.
//	declaration, type_definition, parameter_declaration
//	    Leaf declarations. They can *embed* a named type (`struct S { ... } s;`,
//	    `typedef struct { ... } S;`), which is a separate extraction gap from the
//	    wrapper class this phase owns, and one whose bodiless forms
//	    (`class Foo* p;`) are references rather than definitions.
//	field_declaration
//	    The node a *bodiless* class member takes: `class C { void m(); };`
//	    parses to `field_declaration`, while `void m() {}` parses to
//	    `function_definition`. So the wrapper traversal below restores a
//	    guarded member *definition* to its class, but a guarded member
//	    *declaration* is still not emitted at all -- the same header-declaration
//	    /`.cc`-definition gap P22.17 owns, reached through a different node.
//	    TestCppClassMemberDeclarationStaysOut pins the current answer so it is a
//	    recorded gap rather than a surprise.
//	alias_declaration
//	    `using Alias = T;`. A named type CodeGraph does not model at all yet
//	    (500+ occurrences in fmt and googletest), so emitting it is a new symbol
//	    class rather than a wrapper fix. Its `type_descriptor` child is listed
//	    below only because the census found it; the parent is the real decision.
//	friend_declaration
//	    A friend function is declared at the enclosing namespace scope, not on
//	    the lexical class. It is handled explicitly below with that owner.
//	template_instantiation
//	    `template class Foo<int>;` instantiates an existing template. It
//	    declares no new name, so entering it would duplicate the template.
//	namespace_alias_definition
//	    `namespace n = m;` declares an alias for a namespace, and CodeGraph does
//	    not model namespaces as symbols at all yet (P22.16).
//	dependent_type, placeholder_type_specifier, type_descriptor
//	    Type expressions, not declaration scopes.
var cppTransparentDeclWrappers = map[string]bool{
	"preproc_if":            true,
	"preproc_ifdef":         true,
	"preproc_elif":          true,
	"preproc_else":          true,
	"template_declaration":  true,
	"linkage_specification": true,
	"declaration_list":      true,
	"ERROR":                 true,
}

func cppExtractSymbols(node *sitter.Node, module, container, namespaceContainer, fileScope string, content []byte, pf *graph.ParsedFile) {
	for i := range int(node.ChildCount()) {
		child := node.Child(i)
		switch child.Type() {
		case "function_definition":
			cppAddFunction(child, module, container, content, pf)
			if body := childByFieldName(child, "body"); body != nil {
				for _, class := range findDescendants(child, "class_specifier") {
					if cppBraceDepth(content, body.StartByte(), class.StartByte()) == 0 {
						cppAddType(class, module, container, namespaceContainer, "class", fileScope, content, pf)
					}
				}
			}
		case "declaration":
			// Could be a function declaration or variable. Declarations are
			// evidence rows, not definition targets; the resolver connects them
			// to a definition by language plus qualified identity.
			fnDecl := firstChild(child, "function_declarator")
			if fnDecl != nil {
				cppAddFunctionFromDeclarator(child, fnDecl, module, container, content, pf)
			}
			for j := range int(child.NamedChildCount()) {
				embedded := child.NamedChild(j)
				if childByFieldName(child, "declarator") != nil {
					continue
				}
				switch embedded.Type() {
				case "struct_specifier":
					cppAddType(embedded, module, container, namespaceContainer, "struct", fileScope, content, pf)
				case "class_specifier":
					cppAddType(embedded, module, container, namespaceContainer, "class", fileScope, content, pf)
				case "enum_specifier":
					cppAddType(embedded, module, container, namespaceContainer, "enum", fileScope, content, pf)
				case "union_specifier":
					cppAddType(embedded, module, container, namespaceContainer, "union", fileScope, content, pf)
				}
			}
		case "field_declaration":
			// A bodiless method declaration is a direct function_declarator.
			// Function-pointer fields are wrapped by pointer_declarator and are
			// intentionally not callable symbols.
			fnDecl := firstChild(child, "function_declarator")
			if fnDecl != nil {
				cppAddFunctionFromDeclarator(child, fnDecl, module, container, content, pf)
			}
			if childByFieldName(child, "declarator") == nil {
				for j := range int(child.NamedChildCount()) {
					embedded := child.NamedChild(j)
					switch embedded.Type() {
					case "struct_specifier":
						cppAddType(embedded, module, container, namespaceContainer, "struct", fileScope, content, pf)
					case "class_specifier":
						cppAddType(embedded, module, container, namespaceContainer, "class", fileScope, content, pf)
					case "union_specifier":
						cppAddType(embedded, module, container, namespaceContainer, "union", fileScope, content, pf)
					}
				}
			}
		case "struct_specifier":
			cppAddType(child, module, container, namespaceContainer, "struct", fileScope, content, pf)
		case "class_specifier":
			cppAddType(child, module, container, namespaceContainer, "class", fileScope, content, pf)
		case "enum_specifier":
			cppAddType(child, module, container, namespaceContainer, "enum", fileScope, content, pf)
		case "union_specifier":
			// Symmetric with the three above: a named union at declaration
			// scope is a named type. (Neither fmt nor googletest has one --
			// their unions are all anonymous members -- but the switch would
			// otherwise be silently missing one of C's four tagged types.)
			cppAddType(child, module, container, namespaceContainer, "union", fileScope, content, pf)
		case "namespace_definition":
			// The `body` field always attaches when the node exists at all:
			// probed `namespace n {` unterminated, `namespace n` with no
			// brace, and a brace opened inside an `#ifdef`, and the grammar
			// either attaches a `declaration_list` or degrades the `namespace`
			// keyword to loose ERROR children -- it never yields a
			// namespace_definition without a body. So there is no fallback
			// here; an unfalsifiable one would be untestable code.
			if body := childByFieldName(child, "body"); body != nil {
				namespace := cppNamespaceName(child, content)
				if namespace == "" {
					namespace = "(anonymous@" + fileScope + ")"
				}
				namespace = cppScopeName(container, namespace)
				cppExtractSymbols(body, module, namespace, namespace, fileScope, content, pf)
			}
		case "friend_declaration":
			// The lexical class is not the semantic owner of a friend. The
			// current container is the enclosing namespace (or global scope).
			friendContainer := namespaceContainer
			for i := range int(child.NamedChildCount()) {
				friend := child.NamedChild(i)
				switch friend.Type() {
				case "function_definition":
					cppAddFunction(friend, module, friendContainer, content, pf)
					cppMarkHiddenFriend(pf)
				case "function_declarator":
					cppAddFunctionFromDeclarator(child, friend, module, friendContainer, content, pf)
					cppMarkHiddenFriend(pf)
				}
			}
		default:
			// A transparent wrapper contributes no identity, so the enclosing
			// scope is passed through untouched -- a class member behind an
			// `#if` keeps the class as its container. Recursion is
			// compositional rather than one level deep: a wrapper may hold
			// another wrapper (`#if` > `namespace` > `extern "C"` > `template`),
			// and each node still has exactly one parent, so every declaration
			// is reached along exactly one path and emitted exactly once.
			if cppTransparentDeclWrappers[child.Type()] {
				cppExtractSymbols(child, module, container, namespaceContainer, fileScope, content, pf)
			}
		}
	}
}

func cppBraceDepth(content []byte, start, end uint32) int {
	depth := 0
	lineComment, blockComment, quoted, escaped := false, false, byte(0), false
	for i := int(start); i < int(end); i++ {
		b := content[i]
		if lineComment {
			if b == '\n' {
				lineComment = false
			}
			continue
		}
		if blockComment {
			if b == '*' && i+1 < int(end) && content[i+1] == '/' {
				blockComment = false
				i++
			}
			continue
		}
		if quoted != 0 {
			if escaped {
				escaped = false
				continue
			}
			if b == '\\' {
				escaped = true
				continue
			}
			if b == quoted {
				quoted = 0
			}
			continue
		}
		if b == '/' && i+1 < int(end) && content[i+1] == '/' {
			lineComment = true
			i++
			continue
		}
		if b == '/' && i+1 < int(end) && content[i+1] == '*' {
			blockComment = true
			i++
			continue
		}
		if b == '"' || b == '\'' {
			quoted = b
			continue
		}
		switch b {
		case '{':
			depth++
		case '}':
			if depth > 0 {
				depth--
			}
		}
	}
	return depth
}

func cppScopeName(container, name string) string {
	container = strings.TrimSpace(container)
	name = strings.TrimSpace(name)
	if container == "" {
		return name
	}
	if name == container || strings.HasPrefix(name, container+"::") {
		return name
	}
	return container + "::" + name
}

// cppDeclarationAnchor is the node a symbol's range and doc comment are taken
// from. It is the declaration itself, except under `template <...>`: there the
// wrapper carries the parameter list, the declaration's own previous sibling is
// always that parameter list, and the doc comment sits before the wrapper. So
// anchoring on the declaration would start the range after `template <...>` and
// silently drop every templated symbol's documentation.
func cppDeclarationAnchor(node *sitter.Node) *sitter.Node {
	if parent := node.Parent(); parent != nil && parent.Type() == "template_declaration" {
		return parent
	}
	return node
}

func cppAddFunction(node *sitter.Node, module, container string, content []byte, pf *graph.ParsedFile) {
	declNode := childByFieldName(node, "declarator")
	if declNode == nil {
		return
	}
	// The declarator might be a function_declarator or a pointer_declarator wrapping one.
	fnDecl := declNode
	if declNode.Type() != "function_declarator" {
		fnDecl = firstChild(declNode, "function_declarator")
		if fnDecl == nil {
			fnDecl = declNode
		}
	}
	nameNode := childByFieldName(fnDecl, "declarator")
	if nameNode == nil {
		return
	}
	// The declarator of the function_declarator might be a qualified_identifier or identifier.
	name := nodeText(nameNode, content)
	if cppSkipFuncs[name] {
		return
	}

	effectiveContainer := container
	qualified := cppQualifiedName(effectiveContainer, name)
	anchor := cppDeclarationAnchor(node)

	visibility := heuristicVisibility(name)
	if cppHasAncestor(node, "friend_declaration") {
		visibility = "hidden_friend"
	}
	pf.Symbols = append(pf.Symbols, graph.Symbol{
		Language:      "cpp",
		Kind:          "function",
		Name:          name,
		QualifiedName: qualified,
		ContainerName: effectiveContainer,
		Signature:     cppFunctionSignature(fnDecl, content),
		Visibility:    visibility,
		Range:         nodeRange(anchor),
		DocSummary:    prevCommentText(anchor, content),
		StableKey:     cppStableKey("func", module, qualified),
	})
}

func cppAddFunctionFromDeclarator(decl, fnDecl *sitter.Node, module, container string, content []byte, pf *graph.ParsedFile) {
	anchor := cppDeclarationAnchor(decl)
	nameNode := childByFieldName(fnDecl, "declarator")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, content)
	if cppSkipFuncs[name] || name == "" {
		return
	}

	effectiveContainer := container
	qualified := cppQualifiedName(effectiveContainer, name)

	visibility := heuristicVisibility(name)
	if cppHasAncestor(decl, "friend_declaration") {
		visibility = "hidden_friend"
	}
	pf.Symbols = append(pf.Symbols, graph.Symbol{
		Language:      "cpp",
		Kind:          "declaration",
		Name:          name,
		QualifiedName: qualified,
		ContainerName: effectiveContainer,
		Signature:     cppFunctionSignature(fnDecl, content),
		Visibility:    visibility,
		Range:         nodeRange(anchor),
		DocSummary:    prevCommentText(anchor, content),
		StableKey:     cppStableKey("decl", module, qualified),
	})
}

func cppFunctionSignature(fnDecl *sitter.Node, content []byte) string {
	linkage := ""
	inClassDeclaration := false
	for node := fnDecl; node != nil; node = node.Parent() {
		text := strings.TrimSpace(nodeText(node, content))
		if node.Type() == "field_declaration" {
			inClassDeclaration = true
		}
		if strings.HasPrefix(text, "static ") || strings.HasPrefix(text, "static\n") {
			if !inClassDeclaration {
				linkage = "static:"
			}
			break
		}
		if node.Type() == "linkage_specification" && strings.Contains(text, `"C"`) {
			linkage = "extern_c:"
			break
		}
	}
	parameters := childByFieldName(fnDecl, "parameters")
	if parameters == nil {
		return linkage
	}
	var b strings.Builder
	b.WriteString(linkage)
	b.WriteByte('(')
	first := true
	for i := range int(parameters.ChildCount()) {
		parameter := parameters.Child(i)
		if parameter.Type() == "..." {
			if !first {
				b.WriteByte(',')
			}
			first = false
			b.WriteString("...")
			continue
		}
		if parameter.Type() != "parameter_declaration" && parameter.Type() != "optional_parameter_declaration" && parameter.Type() != "variadic_parameter_declaration" {
			continue
		}
		if !first {
			b.WriteByte(',')
		}
		first = false
		b.WriteString(cppCanonicalParameter(parameter, content))
	}
	b.WriteByte(')')
	if parameters.EndByte() < fnDecl.EndByte() {
		b.WriteString(cppSignatureWithoutComments(fnDecl, content, parameters.EndByte(), fnDecl.EndByte()))
	}
	return b.String()
}

type cppSignatureRemoval struct{ start, end uint32 }

// cppCanonicalParameter removes only AST-identified parameter names and the
// default-value subtree. The remaining source preserves the complete
// declarator shape, including nested function-pointer parameters.
func cppCanonicalParameter(parameter *sitter.Node, content []byte) string {
	var removals []cppSignatureRemoval
	var walk func(*sitter.Node)
	walk = func(node *sitter.Node) {
		if node.Type() == "comment" {
			removals = append(removals, cppSignatureRemoval{node.StartByte(), node.EndByte()})
			return
		}
		if node.Type() == "optional_parameter_declaration" {
			if def := childByFieldName(node, "default_value"); def != nil {
				removals = append(removals, cppSignatureRemoval{def.StartByte(), def.EndByte()})
			}
			for i := range int(node.ChildCount()) {
				if equal := node.Child(i); equal.Type() == "=" {
					removals = append(removals, cppSignatureRemoval{equal.StartByte(), equal.EndByte()})
				}
			}
		}
		if node.Type() == "parameter_declaration" || node.Type() == "optional_parameter_declaration" || node.Type() == "variadic_parameter_declaration" {
			if name := cppDeclaratorName(childByFieldName(node, "declarator")); name != nil {
				removals = append(removals, cppSignatureRemoval{name.StartByte(), name.EndByte()})
			}
		}
		defaultNode := childByFieldName(node, "default_value")
		for i := range int(node.NamedChildCount()) {
			child := node.NamedChild(i)
			if child == defaultNode {
				continue
			}
			walk(child)
		}
	}
	walk(parameter)
	return cppRemoveSignatureRanges(nodeText(parameter, content), parameter.StartByte(), removals)
}

func cppSignatureWithoutComments(node *sitter.Node, content []byte, start, end uint32) string {
	var removals []cppSignatureRemoval
	var walk func(*sitter.Node)
	walk = func(current *sitter.Node) {
		if current.Type() == "comment" {
			removals = append(removals, cppSignatureRemoval{current.StartByte(), current.EndByte()})
			return
		}
		for i := range int(current.NamedChildCount()) {
			walk(current.NamedChild(i))
		}
	}
	walk(node)
	return cppRemoveSignatureRanges(string(content[start:end]), start, removals)
}

func cppDeclaratorName(node *sitter.Node) *sitter.Node {
	if node == nil {
		return nil
	}
	if node.Type() == "identifier" || node.Type() == "field_identifier" {
		return node
	}
	declarator := childByFieldName(node, "declarator")
	if declarator != nil {
		if declarator.Type() == "type_identifier" && node.Type() == "pointer_type_declarator" {
			return declarator
		}
		if name := cppDeclaratorName(declarator); name != nil {
			return name
		}
	}
	if name := childByFieldName(node, "name"); name != nil {
		if name := cppDeclaratorName(name); name != nil {
			return name
		}
	}
	for i := range int(node.NamedChildCount()) {
		child := node.NamedChild(i)
		if child.Type() == "parameter_list" {
			continue
		}
		if name := cppDeclaratorName(child); name != nil {
			return name
		}
	}
	return nil
}

func cppRemoveSignatureRanges(text string, base uint32, removals []cppSignatureRemoval) string {
	if len(removals) == 0 {
		return cppCompactSignatureText([]byte(text))
	}
	sort.Slice(removals, func(i, j int) bool { return removals[i].start < removals[j].start })
	var b strings.Builder
	var cursor = base
	for _, removal := range removals {
		if removal.start < cursor || removal.end > base+uint32(len(text)) {
			continue
		}
		b.WriteString(text[cursor-base : removal.start-base])
		if removal.start > base && removal.end < base+uint32(len(text)) &&
			isCppSignatureWord(text[removal.start-base-1]) &&
			isCppSignatureWord(text[removal.end-base]) {
			b.WriteByte(' ')
		}
		cursor = removal.end
	}
	b.WriteString(text[cursor-base:])
	return cppCompactSignatureText([]byte(b.String()))
}

func cppCompactSignatureText(text []byte) string {
	s := string(text)
	var b strings.Builder
	var last byte
	for i := 0; i < len(s); {
		if s[i] != ' ' && s[i] != '\t' && s[i] != '\n' && s[i] != '\r' {
			b.WriteByte(s[i])
			last = s[i]
			i++
			continue
		}
		for i < len(s) && (s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r') {
			i++
		}
		if b.Len() > 0 && i < len(s) && isCppSignatureWord(last) && isCppSignatureWord(s[i]) {
			b.WriteByte(' ')
		}
	}
	return b.String()
}

func isCppSignatureWord(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9'
}

func cppAddType(node *sitter.Node, module, container, namespaceContainer, kind, fileScope string, content []byte, pf *graph.ParsedFile) {
	nameNode := childByFieldName(node, "name")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, content)
	if name == "" {
		return
	}
	anchor := cppDeclarationAnchor(node)

	pf.Symbols = append(pf.Symbols, graph.Symbol{
		Language:      "cpp",
		Kind:          kind,
		Name:          name,
		QualifiedName: cppQualifiedName(container, name),
		ContainerName: container,
		Visibility:    heuristicVisibility(name),
		Range:         nodeRange(anchor),
		DocSummary:    prevCommentText(anchor, content),
		StableKey:     cppStableKey("type", module, cppQualifiedName(container, name)),
	})

	body := childByFieldName(node, "body")
	if body == nil {
		body = firstChild(node, "field_declaration_list")
	}
	if body != nil {
		cppExtractSymbols(body, module, cppQualifiedName(container, name), namespaceContainer, fileScope, content, pf)
	}
}

func cppExtractCalls(root *sitter.Node, content []byte, pf *graph.ParsedFile) {
	enumValues, enumTypes := cppEnumNames(root, content)
	typeAliases := cppTypeAliasNames(root, content)
	macros := cppMacroNames(root, content)
	for _, call := range findDescendants(root, "call_expression") {
		fnNode := childByFieldName(call, "function")
		if fnNode == nil {
			continue
		}
		callName, callKind, receiver := cppCallTarget(fnNode, content)
		if callName == "" || cppSkipFuncs[callName] {
			continue
		}
		if cppCallInRecoveredDeclaration(call, content) || cppCallNamesNonCallableType(call, fnNode, content, enumValues, enumTypes, typeAliases) || cppCallInHiddenFriendScope(call) {
			continue
		}
		dstName := cppMemberDstName(receiver, callName)
		line := int(call.StartPoint().Row) + 1
		evidence := callKind + ":" + nodeText(fnNode, content)
		if receiver != "" {
			evidence = callKind + ":" + receiver + ":" + nodeText(fnNode, content)
		}
		if cppCallInsideMacro(call, content, macros) {
			evidence = "macro_unexpanded:" + evidence
		}
		pf.Edges = append(pf.Edges, graph.Edge{
			SrcSymbolID: 0,
			DstName:     dstName,
			Kind:        "calls",
			Evidence:    evidence,
			Line:        line,
		})
		pf.References = append(pf.References, graph.Reference{
			Kind:          "call",
			Name:          callName,
			QualifiedName: callName,
			Range:         nodeRange(call),
		})
	}
}

func cppCallInHiddenFriendScope(call *sitter.Node) bool {
	return cppHasAncestor(call, "friend_declaration")
}

func cppMarkHiddenFriend(pf *graph.ParsedFile) {
	if len(pf.Symbols) > 0 {
		pf.Symbols[len(pf.Symbols)-1].Visibility = "hidden_friend"
	}
}

// cppCallInRecoveredDeclaration rejects call_expression nodes that tree-sitter
// recovers from a declaration-shaped class body (notably `Message();` behind a
// declaration macro). A real function body has a body field; the recovery node
// does not, so this remains syntax-based and does not guess from symbol names.
func cppCallInRecoveredDeclaration(call *sitter.Node, content []byte) bool {
	for node := call.Parent(); node != nil; node = node.Parent() {
		if node.Type() == "field_declaration" || node.Type() == "declaration" {
			// C++ permits a declaration as a control-flow condition. Its value
			// initializer is executable code, not a recovered declarator.
			if condition := node.Parent(); condition != nil && condition.Type() == "condition_clause" && childByFieldName(condition, "value") == node {
				return false
			}
			// An initializer is a declaration syntactically, but its call is a
			// real expression. Only reject calls recovered as declarators.
			if decl := cppAncestor(call, "init_declarator"); decl != nil {
				value := childByFieldName(decl, "value")
				if value != nil && call.StartByte() >= value.StartByte() && call.EndByte() <= value.EndByte() {
					return false
				}
			}
			if cppHasAncestor(call, "parameter_declaration") || cppHasAncestor(call, "optional_parameter_declaration") {
				return false
			}
			return true
		}
		if node.Type() == "labeled_statement" {
			label := childByFieldName(node, "label")
			if label != nil {
				switch strings.TrimSpace(nodeText(label, content)) {
				case "public", "private", "protected":
					return true
				}
			}
		}
		if node.Type() == "function_definition" {
			return childByFieldName(node, "body") == nil
		}
	}
	return false
}

func cppAncestor(node *sitter.Node, typ string) *sitter.Node {
	for node = node.Parent(); node != nil; node = node.Parent() {
		if node.Type() == typ {
			return node
		}
	}
	return nil
}

func cppCallNamesNonCallableType(call, fnNode *sitter.Node, content []byte, enumValues, enumTypes, typeAliases map[string][]*sitter.Node) bool {
	if fnNode.Type() != "identifier" {
		return false
	}
	name := strings.TrimSpace(nodeText(fnNode, content))
	if name == "" {
		return false
	}
	callScope := cppLexicalScope(call, content)
	return cppHasVisibleNonCallableScope(callScope, enumValues[name], content) ||
		cppHasVisibleNonCallableScope(callScope, enumTypes[name], content) ||
		cppHasVisibleNonCallableScope(callScope, typeAliases[name], content)
}

func cppHasVisibleNonCallableScope(callScope []string, declarations []*sitter.Node, content []byte) bool {
	for _, declaration := range declarations {
		declScope := cppLexicalScope(declaration, content)
		if len(declScope) <= len(callScope) {
			match := true
			for i := range declScope {
				if declScope[i] != callScope[i] {
					match = false
					break
				}
			}
			if match {
				return true
			}
		}
	}
	return false
}

// cppLexicalScope returns semantic scope owners from outermost to innermost.
// Declaration names alone are insufficient: a type in namespace a must not
// veto a callable with the same spelling in namespace b.
func cppLexicalScope(node *sitter.Node, content []byte) []string {
	var reversed []string
	for current := node; current != nil; current = current.Parent() {
		var key string
		switch current.Type() {
		case "namespace_definition":
			name := childByFieldName(current, "name")
			if name != nil {
				key = current.Type() + ":" + strings.Join(strings.Fields(nodeText(name, content)), "")
			}
			if key == "" {
				key = current.Type() + ":" + strconv.FormatUint(uint64(current.StartByte()), 10)
			}
		case "class_specifier", "struct_specifier", "union_specifier", "function_definition", "compound_statement":
			key = current.Type() + ":" + strconv.FormatUint(uint64(current.StartByte()), 10)
		}
		if key != "" {
			reversed = append(reversed, key)
		}
	}
	for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
		reversed[left], reversed[right] = reversed[right], reversed[left]
	}
	return reversed
}

func cppHasAncestor(node *sitter.Node, typ string) bool {
	for node != nil {
		if node.Type() == typ {
			return true
		}
		node = node.Parent()
	}
	return false
}

func cppEnumNames(root *sitter.Node, content []byte) (map[string][]*sitter.Node, map[string][]*sitter.Node) {
	values := map[string][]*sitter.Node{}
	types := map[string][]*sitter.Node{}
	for _, enum := range findDescendants(root, "enum_specifier") {
		name := childByFieldName(enum, "name")
		if name != nil {
			types[strings.TrimSpace(nodeText(name, content))] = append(types[strings.TrimSpace(nodeText(name, content))], enum)
		}
	}
	for _, enumerator := range findDescendants(root, "enumerator") {
		name := childByFieldName(enumerator, "name")
		if name != nil {
			key := strings.TrimSpace(nodeText(name, content))
			values[key] = append(values[key], enumerator)
		}
	}
	return values, types
}

func cppTypeAliasNames(root *sitter.Node, content []byte) map[string][]*sitter.Node {
	aliases := map[string][]*sitter.Node{}
	for _, node := range findDescendants(root, "alias_declaration") {
		name := childByFieldName(node, "name")
		if name != nil {
			key := strings.TrimSpace(nodeText(name, content))
			aliases[key] = append(aliases[key], node)
		}
	}
	return aliases
}

func cppMacroNames(root *sitter.Node, content []byte) map[string]struct{} {
	macros := map[string]struct{}{}
	nodes := append(findDescendants(root, "preproc_def"), findDescendants(root, "preproc_function_def")...)
	for _, node := range nodes {
		text := strings.TrimSpace(nodeText(node, content))
		fields := strings.Fields(strings.TrimPrefix(text, "#define"))
		if len(fields) == 0 {
			continue
		}
		name := strings.TrimSpace(strings.SplitN(fields[0], "(", 2)[0])
		if name != "" {
			macros[name] = struct{}{}
		}
	}
	return macros
}

func cppCallInsideMacro(call *sitter.Node, content []byte, macros map[string]struct{}) bool {
	for node := call.Parent(); node != nil; node = node.Parent() {
		if node.Type() != "call_expression" {
			continue
		}
		fn := childByFieldName(node, "function")
		if fn != nil {
			name := strings.TrimSpace(nodeText(fn, content))
			if _, ok := macros[name]; ok || (name != "" && strings.ToUpper(name) == name) {
				return true
			}
		}
	}
	return false
}

// cppMemberDstName is the persisted destination identity of a call. For an
// instance call it keeps the receiver: `p.foo()` and `p->foo()` both spell
// `p.foo`, so the qualifier the source actually wrote survives into
// `edges.dst_name` instead of living only in `evidence`.
//
// Why the qualifier has to survive: without it the destination is the bare tail
// `foo`, and the repo-wide resolver answers a bare name with whichever project
// symbol named `foo` happens to be unique -- so `v.size()` on a std::vector
// bound a project `size` method (measured: 281 such bindings in fmt, 207 in
// googletest, roughly half of a hand sample wrong and the rest correct only by
// coincidence). Keeping the receiver moves the spelling into the member-
// qualified class P22.1 already defines, where a qualifier may only bind when
// the destination's own identity confirms it.
//
// The receiver is a *variable* spelling, never a type, so this composition can
// only ever make a destination harder to bind -- deliberately. C++ symbol
// identities are `::`-separated (`A::parse`), so `a.parse` matches no identity
// at any evidence level and the edge stays honestly unresolved. Recall for
// calls CodeGraph can actually prove -- `Type::foo()`, `ns::Type::foo()` -- is
// untouched: those never had a receiver and keep their full `::` spelling.
//
// `->` is normalized to '.' rather than persisted raw, so no new separator
// grammar enters the identity space and the existing '.'-keyed member rules in
// the resolver and the query surfaces apply unchanged. All whitespace is
// dropped, so a member call split across lines cannot produce a newline-bearing
// identity.
//
// Scope: this covers the two receiver branches of cppCallTarget only. A call
// whose spelling contains "::" takes the scope-qualified branch and is
// persisted verbatim, so `this->Base::foo()` keeps its arrow and a multi-line
// `d\n  .Base::foo()` keeps its newline. That predates P22.11 and is left
// alone: those spellings already carry a "::" qualifier, which is what both the
// resolver's member rule and the query surfaces key on, so they fail closed
// exactly as intended -- and rewriting them would change identities this phase
// has no evidence about. TestCppCallDstNameKeepsReceiver pins the shape.
func cppMemberDstName(receiver, name string) string {
	if strings.TrimSpace(receiver) == "" {
		return name
	}
	return strings.Join(strings.Fields(strings.ReplaceAll(receiver, "->", ".")+"."+name), "")
}

func cppQualifiedName(container, name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	container = strings.TrimSpace(container)
	if container == "" {
		return name
	}
	if strings.HasPrefix(name, "::") || name == container || strings.HasPrefix(name, container+"::") {
		return name
	}
	return container + "::" + name
}

func cppStableKey(prefix, module, qualified string) string {
	qualified = strings.TrimSpace(qualified)
	if qualified == "" {
		return ""
	}
	module = strings.TrimSpace(module)
	if module == "" {
		return prefix + ":cpp:" + qualified
	}
	return prefix + ":cpp:" + module + ":" + qualified
}

func cppCallTarget(fnNode *sitter.Node, content []byte) (name, kind, receiver string) {
	raw := strings.TrimSpace(nodeText(fnNode, content))
	if raw == "" {
		return "", "", ""
	}
	switch {
	case strings.HasPrefix(raw, "::"):
		return raw, "static_qualified", ""
	case strings.Contains(raw, "::"):
		return raw, "static_qualified", ""
	case strings.Contains(raw, "->"):
		receiver, name = splitCallTarget(raw, "->")
		return strings.TrimSpace(name), "member_pointer", strings.TrimSpace(receiver)
	case strings.Contains(raw, "."):
		receiver, name = splitCallTarget(raw, ".")
		return strings.TrimSpace(name), "member_value", strings.TrimSpace(receiver)
	default:
		return raw, "direct", ""
	}
}

func cppNamespaceName(node *sitter.Node, content []byte) string {
	name := childByFieldName(node, "name")
	if name == nil {
		return ""
	}
	return strings.Join(strings.Fields(strings.TrimSpace(nodeText(name, content))), "")
}

func splitCallTarget(text, sep string) (string, string) {
	idx := strings.LastIndex(text, sep)
	if idx < 0 {
		return "", text
	}
	return text[:idx], text[idx+len(sep):]
}
