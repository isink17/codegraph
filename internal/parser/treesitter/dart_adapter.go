//go:build cgo

package treesitter

import (
	"context"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"

	"github.com/isink17/codegraph/internal/graph"
	"github.com/isink17/codegraph/internal/parser/treesitter/dartgrammar"
)

// DartAdapter extracts declarations, directives and call references from Dart
// sources. What a qualified Dart call runs depends on the receiver's static
// type (extension methods, cascades), on mixin linearization, on `package:`
// resolution through package_config.json and on dynamic dispatch, none of
// which syntax alone proves, so those calls stay unresolved references. Only a
// bare call that Dart's lexical scoping provably binds to a function of the
// same file becomes a call edge; see dartLexicalCalls.
//
// Declarations are recorded at the top level and in the bodies of classes,
// mixins, named extensions, extension types and enums. In a file with a parse
// error only the declarations that end before the first error are recorded;
// see dartMembers.
//
// Qualified names are `Owner.member`; a library has no name prefix because a
// Dart library is its file. Constructors are named the way Dart spells their
// member name: the unnamed constructor of `Point` is `Point.Point` (as in the
// Java adapter) and `Point.new` is the same constructor; a named or factory
// constructor is `Point.origin`. A setter's stable key ends in `=`, Dart's
// own setter name, so a getter and setter pair keep distinct keys.
type DartAdapter struct{}

func NewDart() *DartAdapter                 { return &DartAdapter{} }
func (a *DartAdapter) Language() string     { return "dart" }
func (a *DartAdapter) Extensions() []string { return []string{".dart"} }
func (a *DartAdapter) Supports(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".dart")
}

func (a *DartAdapter) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	root, err := parse(ctx, dartgrammar.GetLanguage(), content)
	if err != nil {
		return graph.ParsedFile{}, err
	}
	pf := graph.ParsedFile{Language: "dart", FileTokens: computeFileTokens(content)}
	broken := root.HasError()
	limit := ^uint32(0)
	if broken {
		limit = dartFirstErrorByte(root)
	}
	dartDirectives(root, limit, content, &pf)
	if broken && dartExcessCloseBrace(root) {
		limit = 0
	}
	dartMembers(root, "", limit, content, &pf)
	if !broken {
		pf.Edges = dartLexicalCalls(root, content, &pf)
	}
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if ref, ok := dartCallReference(n, content); ok && (!broken || !dartInError(n)) {
			pf.References = append(pf.References, ref)
		}
		for i := range int(n.ChildCount()) {
			walk(n.Child(i))
		}
	}
	walk(root)
	return pf, nil
}

// dartDirectives records library-level directives. Every URI is recorded as
// written; a `package:` URI is never mapped to a file, since that needs
// pubspec.yaml and package_config.json.
//
// Like declarations, only directives that end before limit, the first parse
// error, are read, so none is cut short by recovery.
func dartDirectives(root *sitter.Node, limit uint32, content []byte, pf *graph.ParsedFile) {
	for i := range int(root.ChildCount()) {
		child := root.Child(i)
		if child.EndByte() >= limit {
			return
		}
		switch child.Type() {
		case "import_or_export":
			if spec := firstChild(child, "library_import"); spec != nil {
				dartImport(firstChild(spec, "import_specification"), false, content, pf)
			} else if spec := firstChild(child, "library_export"); spec != nil {
				dartImport(spec, true, content, pf)
			}
		case "part_directive", "part_of_directive":
			if uri := firstChild(child, "uri"); uri != nil {
				if s := dartURI(uri, content); s != "" {
					pf.Imports = append(pf.Imports, s)
				}
			}
			// `part of a.b;` names the owning library, not a URI: not recorded.
		}
	}
}

// dartImport reads one import or export. The library itself is a namespace
// row (LocalName is the `as` prefix; Wildcard when no `show` narrows it);
// each `show` name is a named row and each `hide` name a ScopeImportDartHide
// row. A deferred import's library row is ScopeImportDartDeferred.
func dartImport(node *sitter.Node, export bool, content []byte, pf *graph.ParsedFile) {
	if node == nil {
		return
	}
	var uris []string
	if cfg := firstChild(node, "configurable_uri"); cfg != nil {
		uris = append(uris, dartURI(firstChild(cfg, "uri"), content))
		// `if (dart.library.io) 'b.dart'` alternatives are imports too.
		for _, alt := range findChildren(cfg, "configuration_uri") {
			uris = append(uris, dartURI(firstChild(alt, "uri"), content))
		}
	} else {
		uris = append(uris, dartURI(firstChild(node, "uri"), content))
	}
	prefix, deferred := "", false
	var show, hide []string
	for i := range int(node.ChildCount()) {
		child := node.Child(i)
		switch child.Type() {
		case "deferred":
			deferred = true
		case "identifier":
			prefix = nodeText(child, content)
		case "combinator":
			var names []string
			for _, id := range findChildren(child, "identifier") {
				names = append(names, nodeText(id, content))
			}
			if first := child.Child(0); first != nil && first.Type() == "hide" {
				hide = append(hide, names...)
			} else {
				show = append(show, names...)
			}
		}
	}
	kind := graph.ScopeImportNamespace
	if deferred {
		kind = graph.ScopeImportDartDeferred
	}
	for _, uri := range uris {
		if uri == "" {
			continue
		}
		pf.Imports = append(pf.Imports, uri)
		pf.Scope.Imports = append(pf.Scope.Imports, graph.ScopeImport{SourceSpecifier: uri, LocalName: prefix, Kind: kind, Wildcard: len(show) == 0, ReExport: export})
		for _, name := range show {
			pf.Scope.Imports = append(pf.Scope.Imports, graph.ScopeImport{SourceSpecifier: uri, ImportedName: name, LocalName: name, Kind: graph.ScopeImportNamed, ReExport: export})
		}
		for _, name := range hide {
			pf.Scope.Imports = append(pf.Scope.Imports, graph.ScopeImport{SourceSpecifier: uri, ImportedName: name, Kind: graph.ScopeImportDartHide, ReExport: export})
		}
	}
}

// dartURI is a directive's string literal without its quotes. An
// interpolated string is not a constant URI and yields "".
func dartURI(uri *sitter.Node, content []byte) string {
	lit := firstChild(uri, "string_literal")
	if lit == nil || firstChild(lit, "template_substitution") != nil {
		return ""
	}
	text := nodeText(lit, content)
	text = strings.TrimPrefix(text, "r")
	for _, q := range []string{`"""`, `'''`, `"`, `'`} {
		if len(text) >= 2*len(q) && strings.HasPrefix(text, q) && strings.HasSuffix(text, q) {
			return text[len(q) : len(text)-len(q)]
		}
	}
	return ""
}

// dartFirstErrorByte is the start of the first ERROR or MISSING node in
// document order.
func dartFirstErrorByte(n *sitter.Node) uint32 {
	if n.Type() == "ERROR" || n.IsMissing() {
		return n.StartByte()
	}
	for i := range int(n.ChildCount()) {
		if child := n.Child(i); child.HasError() {
			return dartFirstErrorByte(child)
		}
	}
	return n.EndByte()
}

// dartExcessCloseBrace reports more `}` than `{` tokens, which means an
// opening brace is missing. Recovery then closes a body at an earlier `}` and
// moves the members after it out to the enclosing scope, all before the first
// error, which is only the excess `}` at the end. Where the missing brace was
// is unknown, so no declaration's owner is. Tokens are counted, so braces in
// strings and comments do not; an interpolation `${` is a `$` and a `{`.
func dartExcessCloseBrace(root *sitter.Node) bool {
	depth := 0
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n.ChildCount() == 0 {
			if !n.IsMissing() {
				switch n.Type() {
				case "{":
					depth++
				case "}":
					depth--
				}
			}
			return
		}
		for i := range int(n.ChildCount()) {
			walk(n.Child(i))
		}
	}
	walk(root)
	return depth < 0
}

// dartMembers records the declarations directly under parent: the program,
// or a class, mixin, extension, extension type or enum body. A function or
// method is a signature node followed by its function_body sibling.
//
// Only declarations that end before limit, the first parse error, are
// recorded. Text before the first error parsed without recovery, so its
// declarations and owners are the ones the source spells; after it,
// recovery may have closed a body early, swallowed a later declaration into
// an unclosed one, or turned a misspelt keyword into a different
// declaration. A type that does not end before the error is dropped with all
// of its members, since its extent is no longer known.
func dartMembers(parent *sitter.Node, owner string, limit uint32, content []byte, pf *graph.ParsedFile) {
	for i := 0; i < int(parent.ChildCount()); i++ {
		child := parent.Child(i)
		var body *sitter.Node
		if next := child.NextSibling(); next != nil && next.Type() == "function_body" {
			body = next
		}
		end := child
		if body != nil {
			end = body
		}
		if end.EndByte() >= limit {
			return
		}
		switch child.Type() {
		case "class_definition":
			dartType(child, "class", childByFieldName(child, "name"), childByFieldName(child, "body"), limit, content, pf)
		case "mixin_declaration":
			dartType(child, "mixin", firstChild(child, "identifier"), firstChild(child, "class_body"), limit, content, pf)
		case "extension_declaration":
			// An unnamed extension cannot be named anywhere; its members are
			// not recorded.
			dartType(child, "extension", childByFieldName(child, "name"), childByFieldName(child, "body"), limit, content, pf)
		case "extension_type_declaration":
			name := childByFieldName(child, "name")
			dartType(child, "extension_type", name, childByFieldName(child, "body"), limit, content, pf)
			if rep := childByFieldName(child, "representation"); rep != nil && name != nil {
				if field := childByFieldName(rep, "name"); field != nil {
					dartSymbol(rep, rep, "field", "value", nodeText(name, content), nodeText(field, content), "", content, pf)
				}
			}
		case "enum_declaration":
			dartType(child, "enum", childByFieldName(child, "name"), childByFieldName(child, "body"), limit, content, pf)
		case "type_alias":
			if owner == "" {
				dartSymbol(child, child, "typedef", "type", "", dartTypedefName(child, content), "", content, pf)
			}
		case "enum_constant":
			if name := childByFieldName(child, "name"); name != nil && owner != "" {
				dartSymbol(child, child, "enum_value", "value", owner, nodeText(name, content), "", content, pf)
			}
		case "function_signature", "getter_signature", "setter_signature":
			// A top-level signature is a declaration only with its body or,
			// for `external`, its `;`; recovery leaves bare signatures behind.
			if owner == "" && (body != nil || dartTerminated(child, limit)) {
				dartCallable(child, child, body, owner, content, pf)
			}
		case "method_signature", "declaration":
			if owner == "" && child.Type() == "declaration" {
				continue
			}
			if sig := dartSignature(child); sig != nil {
				dartCallable(sig, child, body, owner, content, pf)
			} else if child.Type() == "declaration" {
				dartVariables(child, child, "field", owner, content, pf)
			}
		case "initialized_identifier_list", "static_final_declaration_list":
			if start := dartTopLevelVariableStart(child); owner == "" && start != nil && dartTerminated(child, limit) {
				dartVariables(child, start, "variable", "", content, pf)
			}
		}
	}
}

func dartType(node *sitter.Node, kind string, nameNode, body *sitter.Node, limit uint32, content []byte, pf *graph.ParsedFile) {
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, content)
	dartSymbol(node, node, kind, "type", "", name, dartHeader(node, body, content), content, pf)
	if body != nil {
		dartMembers(body, name, limit, content, pf)
	}
}

// dartSignature is the declaration signature a method_signature or a bodyless
// declaration wraps, or nil for a field declaration.
func dartSignature(node *sitter.Node) *sitter.Node {
	for i := range int(node.NamedChildCount()) {
		switch child := node.NamedChild(i); child.Type() {
		case "function_signature", "getter_signature", "setter_signature", "operator_signature",
			"constructor_signature", "constant_constructor_signature",
			"factory_constructor_signature", "redirecting_factory_constructor_signature":
			return child
		}
	}
	return nil
}

// dartCallable records a function, method, accessor, operator or
// constructor. sig is the signature node, decl the node that spans it with
// its modifiers, and body the function_body that follows decl, if any.
func dartCallable(sig, decl, body *sitter.Node, owner string, content []byte, pf *graph.ParsedFile) {
	kind, keySuffix := "function", ""
	if owner != "" {
		kind = "method"
	}
	var name string
	switch sig.Type() {
	case "function_signature", "getter_signature":
		name = nodeText(childByFieldName(sig, "name"), content)
	case "setter_signature":
		name, keySuffix = nodeText(childByFieldName(sig, "name"), content), "="
	case "operator_signature":
		name = dartOperatorName(sig, content)
	default:
		if owner == "" {
			return
		}
		kind = "constructor"
		name = dartConstructorName(sig, owner, content)
	}
	if name == "" {
		return
	}
	end := decl
	if body != nil {
		end = body
	}
	rng := nodeRange(decl)
	endRange := nodeRange(end)
	rng.EndLine, rng.EndCol = endRange.EndLine, endRange.EndCol
	signature := strings.Join(strings.Fields(strings.TrimSuffix(strings.TrimSpace(nodeText(decl, content)), ";")), " ")
	pf.Symbols = append(pf.Symbols, graph.Symbol{
		Language:      "dart",
		Kind:          kind,
		Name:          name,
		QualifiedName: dartQualified(owner, name),
		ContainerName: owner,
		Signature:     signature,
		Visibility:    dartVisibility(name),
		Range:         rng,
		DocSummary:    dartDoc(decl, content),
		StableKey:     "func:dart:" + dartQualified(owner, name) + keySuffix,
	})
}

// dartConstructorName is the member part of a constructor's name, or "" when
// the class segment does not name the enclosing class.
func dartConstructorName(sig *sitter.Node, owner string, content []byte) string {
	var parts []string
	for i := range int(sig.ChildCount()) {
		child := sig.Child(i)
		if child.Type() == "formal_parameter_list" {
			break
		}
		if child.Type() == "identifier" {
			parts = append(parts, nodeText(child, content))
		}
	}
	if len(parts) == 0 || parts[0] != owner {
		return ""
	}
	if len(parts) == 1 || parts[1] == "new" {
		return owner
	}
	return parts[1]
}

func dartOperatorName(sig *sitter.Node, content []byte) string {
	var op strings.Builder
	seen := false
	for i := range int(sig.ChildCount()) {
		child := sig.Child(i)
		switch {
		case child.Type() == "operator":
			seen = true
		case child.Type() == "formal_parameter_list":
			if !seen || op.Len() == 0 {
				return ""
			}
			return "operator" + op.String()
		case seen:
			op.WriteString(strings.Join(strings.Fields(nodeText(child, content)), ""))
		}
	}
	return ""
}

// dartTypedefName names `typedef N<T> = ...` (the first type name) and the
// legacy `typedef R N(...)` (the last type name before the parameters).
func dartTypedefName(node *sitter.Node, content []byte) string {
	var names []string
	for i := range int(node.ChildCount()) {
		switch child := node.Child(i); child.Type() {
		case "type_identifier":
			names = append(names, nodeText(child, content))
		case "=":
			if len(names) > 0 {
				return names[0]
			}
			return ""
		case "formal_parameter_list":
			if len(names) > 0 {
				return names[len(names)-1]
			}
			return ""
		}
	}
	return ""
}

// dartTerminated reports a loose top-level node followed by a real `;` that
// ends before limit.
func dartTerminated(n *sitter.Node, limit uint32) bool {
	next := n.NextSibling()
	return next != nil && next.Type() == ";" && !next.IsMissing() && next.EndByte() < limit
}

// dartTopLevelVariableStart is the first modifier or type node of a top-level
// variable list (the grammar leaves them as loose siblings), or nil when the
// list does not follow a finished declaration: error recovery turns stray
// words into such lists.
func dartTopLevelVariableStart(list *sitter.Node) *sitter.Node {
	start := list
	for p := list.PrevSibling(); p != nil; p = p.PrevSibling() {
		if p.IsMissing() {
			return nil
		}
		switch p.Type() {
		case ";", "}", "function_body", "documentation_comment", "comment", "annotation",
			"library_name", "import_or_export", "part_directive", "part_of_directive",
			"class_definition", "mixin_declaration", "extension_declaration",
			"extension_type_declaration", "enum_declaration", "type_alias":
			return start
		case "ERROR", "initialized_identifier_list", "static_final_declaration_list",
			"function_signature", "getter_signature", "setter_signature":
			return nil
		}
		start = p
	}
	return start
}

// dartVariables records each name a field declaration or top-level variable
// list declares. decl is the node the declaration starts at.
func dartVariables(node, decl *sitter.Node, kind, owner string, content []byte, pf *graph.ParsedFile) {
	lists := []*sitter.Node{node}
	if node.Type() == "declaration" {
		lists = append(findChildren(node, "initialized_identifier_list"), findChildren(node, "static_final_declaration_list")...)
	}
	signature := strings.Join(strings.Fields(string(content[decl.StartByte():node.EndByte()])), " ")
	for _, list := range lists {
		for i := range int(list.NamedChildCount()) {
			entry := list.NamedChild(i)
			if entry.Type() != "initialized_identifier" && entry.Type() != "static_final_declaration" {
				continue
			}
			if id := firstChild(entry, "identifier"); id != nil {
				dartSymbol(entry, decl, kind, "value", owner, nodeText(id, content), strings.TrimSuffix(signature, ";"), content, pf)
			}
		}
	}
}

func dartSymbol(node, docNode *sitter.Node, kind, keyPrefix, owner, name, signature string, content []byte, pf *graph.ParsedFile) {
	if name == "" {
		return
	}
	if signature == "" {
		signature = strings.Join(strings.Fields(strings.TrimSuffix(strings.TrimSpace(nodeText(node, content)), ";")), " ")
	}
	pf.Symbols = append(pf.Symbols, graph.Symbol{
		Language:      "dart",
		Kind:          kind,
		Name:          name,
		QualifiedName: dartQualified(owner, name),
		ContainerName: owner,
		Signature:     signature,
		Visibility:    dartVisibility(name),
		Range:         nodeRange(node),
		DocSummary:    dartDoc(docNode, content),
		StableKey:     keyPrefix + ":dart:" + dartQualified(owner, name),
	})
}

// dartHeader is a type declaration up to its body.
func dartHeader(node, body *sitter.Node, content []byte) string {
	end := node.EndByte()
	if body != nil {
		end = body.StartByte()
	}
	return strings.Join(strings.Fields(string(content[node.StartByte():end])), " ")
}

func dartQualified(owner, name string) string {
	if owner == "" {
		return name
	}
	return owner + "." + name
}

// dartVisibility: a leading underscore makes a name private to its library.
func dartVisibility(name string) string {
	if strings.HasPrefix(name, "_") {
		return "private"
	}
	return "public"
}

// dartDoc is the `///` or `/** */` documentation directly above node,
// skipping its annotations.
func dartDoc(node *sitter.Node, content []byte) string {
	var lines []string
	for prev := node.PrevSibling(); prev != nil; prev = prev.PrevSibling() {
		if prev.Type() == "annotation" {
			continue
		}
		if prev.Type() != "documentation_comment" {
			break
		}
		var text []string
		for _, line := range strings.Split(nodeText(prev, content), "\n") {
			line = strings.TrimSpace(line)
			line = strings.TrimPrefix(line, "///")
			line = strings.TrimPrefix(line, "/**")
			line = strings.TrimSuffix(line, "*/")
			line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "*"))
			if line != "" {
				text = append(text, line)
			}
		}
		lines = append(text, lines...)
	}
	return strings.Join(lines, " ")
}

// dartCallReference names the call that n completes. The grammar spells a
// call as a primary followed by selector siblings, the last of which holds
// the argument_part, so a call is recognised at that selector (or at the
// argument_part of a cascade section). `f()` is `f`, `a.b.f()` is `a.b.f`,
// and a member call on anything but a plain dotted path (`this.f()`,
// `x?.f()`, `g().f()`, `'s'.f()`, `..f()`) is the member name alone.
// `new C()` and `const C.n()` name the constructor. A call of a call result
// (`f()()`) or an index (`a[0]()`) has no name and yields no reference.
func dartCallReference(n *sitter.Node, content []byte) (graph.Reference, bool) {
	var callee, end *sitter.Node
	name := ""
	switch n.Type() {
	case "selector":
		if first := n.NamedChild(0); first == nil || first.Type() != "argument_part" {
			return graph.Reference{}, false
		}
		callee, end = n.PrevSibling(), n
		name = dartCallee(callee, content)
	case "argument_part":
		if p := n.Parent(); p == nil || p.Type() != "cascade_section" {
			return graph.Reference{}, false
		}
		callee, end = n.PrevSibling(), n
		name = dartMemberName(callee, content)
	case "new_expression", "const_object_expression":
		var parts []string
		for i := range int(n.ChildCount()) {
			switch child := n.Child(i); child.Type() {
			case "type_identifier", "identifier":
				parts = append(parts, nodeText(child, content))
			}
		}
		callee, end = n, n
		name = strings.Join(parts, ".")
	default:
		return graph.Reference{}, false
	}
	if name == "" || callee == nil {
		return graph.Reference{}, false
	}
	rng := nodeRange(callee)
	endRange := nodeRange(end)
	rng.EndLine, rng.EndCol = endRange.EndLine, endRange.EndCol
	return graph.Reference{Kind: "call", Name: name, QualifiedName: name, Range: rng}, true
}

func dartCallee(callee *sitter.Node, content []byte) string {
	if callee == nil {
		return ""
	}
	if callee.Type() == "identifier" {
		return nodeText(callee, content)
	}
	member := dartMemberName(callee, content)
	if member == "" || !dartPlainMember(callee) {
		return member
	}
	path := []string{member}
	for q := callee.PrevSibling(); q != nil; q = q.PrevSibling() {
		if q.Type() == "identifier" {
			return strings.Join(append([]string{nodeText(q, content)}, path...), ".")
		}
		m := dartMemberName(q, content)
		if m == "" || !dartPlainMember(q) {
			break
		}
		path = append([]string{m}, path...)
	}
	return member
}

// dartMemberName is the member a `.m`, `?.m` or cascade `..m` selector names.
func dartMemberName(n *sitter.Node, content []byte) string {
	if n == nil {
		return ""
	}
	sel := n
	if n.Type() == "selector" {
		sel = n.NamedChild(0)
		if sel == nil {
			return ""
		}
	}
	switch sel.Type() {
	case "unconditional_assignable_selector", "conditional_assignable_selector", "cascade_selector":
		if id := firstChild(sel, "identifier"); id != nil {
			return nodeText(id, content)
		}
	}
	return ""
}

// dartPlainMember reports a `.m` selector, the only one a dotted path may
// pass through.
func dartPlainMember(n *sitter.Node) bool {
	if n.Type() == "selector" {
		n = n.NamedChild(0)
	}
	return n != nil && n.Type() == "unconditional_assignable_selector"
}

func dartInError(node *sitter.Node) bool {
	for n := node; n != nil; n = n.Parent() {
		if n.Type() == "ERROR" || n.IsMissing() {
			return true
		}
	}
	return false
}

// dartUnit is one program-level declaration: a top-level function, getter or
// setter is its signature and the function_body that follows it.
type dartUnit struct {
	nodes []*sitter.Node
	// name is the declared name of a top-level function, getter or setter;
	// function is set for a function with a body, the only kind a bare call
	// runs directly (a getter's call invokes the value it returns).
	name     *sitter.Node
	function bool
	// body is a type's member list. Its leaves, like everything in a
	// function unit but its name, are scoped to the unit; a type's header
	// (name, type parameters) and every other unit are library-level.
	body *sitter.Node
}

// dartLeaf is one occurrence of a name inside a unit.
type dartLeaf struct {
	node   *sitter.Node
	unit   int
	inBody bool
	callee bool
}

// dartLexicalCalls returns a call edge for each bare `name(...)` call Dart's
// lexical scoping provably binds to a function declared in this file, and
// records each local function declaration as a symbol so such a call has a
// target. Lexical lookup runs from the innermost block out through the
// enclosing class body (declared members only: an inherited member never
// shadows a lexical name), the library and then the imports, which the
// library's own declarations shadow.
//
// The proof is by spelling: every declaration spells its name as a token, so
// a name whose only tokens are one declaration and bare calls has no other
// binding to shadow it. A call binds
//
//   - to a local function, when every other token of the name in the enclosing
//     program-level declaration is a bare call, the call lies in the block that
//     declares the function and follows its declaration;
//   - to a top-level function, when the file has no part directives (so the
//     library is this file), it is the file's only top-level declaration of the
//     name, the name appears in no directive, top-level variable, typedef or
//     type header, and every other token of the name in the enclosing
//     program-level declaration is a bare call.
//
// Anything else stays unresolved: a method, field or accessor of the same
// name, a parameter, variable or pattern of that name, a closure held in a
// variable, an import prefix, and every qualified, cascade, conditional or
// constructor call. A file with a parse error proves nothing; the caller does
// not call this for one.
func dartLexicalCalls(root *sitter.Node, content []byte, pf *graph.ParsedFile) []graph.Edge {
	var units []dartUnit
	parts := false
	for i := 0; i < int(root.ChildCount()); i++ {
		child := root.Child(i)
		u := dartUnit{nodes: []*sitter.Node{child}}
		switch child.Type() {
		case "part_directive", "part_of_directive":
			parts = true
		case "function_signature", "getter_signature", "setter_signature":
			u.name = childByFieldName(child, "name")
			if next := child.NextSibling(); next != nil && next.Type() == "function_body" {
				u.nodes = append(u.nodes, next)
				u.function = child.Type() == "function_signature"
				i++
			}
		case "class_definition", "mixin_declaration", "extension_declaration", "extension_type_declaration", "enum_declaration":
			u.body = childByFieldName(child, "body")
			if u.body == nil {
				u.body = firstChild(child, "class_body")
			}
		}
		units = append(units, u)
	}
	leaves := map[string][]dartLeaf{}
	for ui, u := range units {
		var walk func(n *sitter.Node, inBody bool)
		walk = func(n *sitter.Node, inBody bool) {
			inBody = inBody || u.name != nil || u.body != nil && n.StartByte() == u.body.StartByte() && n.EndByte() == u.body.EndByte() && n.Type() == u.body.Type()
			if n.ChildCount() == 0 {
				if text := nodeText(n, content); dartNameLike(text) {
					next := n.NextSibling()
					callee := n.Type() == "identifier" && next != nil && next.Type() == "selector" &&
						next.NamedChild(0) != nil && next.NamedChild(0).Type() == "argument_part"
					leaves[text] = append(leaves[text], dartLeaf{node: n, unit: ui, inBody: inBody, callee: callee})
				}
				return
			}
			for i := range int(n.ChildCount()) {
				walk(n.Child(i), inBody)
			}
		}
		for _, n := range u.nodes {
			walk(n, false)
		}
	}
	// Local functions are declarations of this file; record them all so a
	// proven call has a symbol to bind to.
	locals := map[uint32]*sitter.Node{} // name token start -> declaration
	for _, decl := range findDescendants(root, "local_function_declaration") {
		lambda := firstChild(decl, "lambda_expression")
		if lambda == nil {
			continue
		}
		sig := childByFieldName(lambda, "parameters")
		if sig == nil || sig.Type() != "function_signature" {
			continue
		}
		name := childByFieldName(sig, "name")
		if name == nil {
			continue
		}
		locals[name.StartByte()] = decl
		text := nodeText(name, content)
		rng := nodeRange(decl)
		pf.Symbols = append(pf.Symbols, graph.Symbol{
			Language:      "dart",
			Kind:          "function",
			Name:          text,
			QualifiedName: text,
			Signature:     strings.Join(strings.Fields(nodeText(sig, content)), " "),
			Visibility:    dartVisibility(text),
			Range:         rng,
			StableKey:     "func:dart:local:" + text + ":" + strconv.Itoa(rng.StartLine) + ":" + strconv.Itoa(rng.StartCol),
		})
	}
	same := func(a, b *sitter.Node) bool {
		return a != nil && b != nil && a.StartByte() == b.StartByte() && a.EndByte() == b.EndByte()
	}
	var edges []graph.Edge
	edge := func(call *sitter.Node, name string, target graph.Position) {
		at := nodeRange(call)
		edges = append(edges, graph.Edge{
			DstName:  name,
			Kind:     "calls",
			Evidence: graph.DartLexicalFunctionEvidence + strconv.Itoa(target.StartLine) + ":" + strconv.Itoa(target.StartCol),
			Line:     at.StartLine,
			Col:      at.StartCol,
		})
	}
	for _, name := range slices.Sorted(maps.Keys(leaves)) {
		occ := leaves[name]
		// The top-level target and whether anything library-level besides it
		// declares or mentions the name.
		top, libraryClash := -1, parts
		for _, l := range occ {
			u := units[l.unit]
			switch {
			case l.callee:
			case u.name != nil && same(l.node, u.name):
				if u.function && top == -1 {
					top = l.unit
				} else {
					libraryClash = true
				}
			case !l.inBody:
				libraryClash = true
			}
		}
		for _, call := range occ {
			if !call.callee {
				continue
			}
			var own []dartLeaf
			for _, l := range occ {
				if l.unit != call.unit || l.callee || top == l.unit && same(l.node, units[top].name) {
					continue
				}
				own = append(own, l)
			}
			switch {
			case len(own) == 0 && top != -1 && !libraryClash:
				edge(call.node, name, nodeRange(units[top].nodes[0]))
			case len(own) == 1:
				// A block is the declaration's whole scope, and Dart rejects a
				// reference that precedes the declaration.
				decl := locals[own[0].node.StartByte()]
				if decl == nil {
					continue
				}
				if block := decl.Parent(); block != nil && block.Type() == "block" && dartWithin(call.node, block) && call.node.StartByte() > decl.StartByte() {
					edge(call.node, name, nodeRange(decl))
				}
			}
		}
	}
	return edges
}

func dartWithin(n, outer *sitter.Node) bool {
	return n.StartByte() >= outer.StartByte() && n.EndByte() <= outer.EndByte()
}

// dartNameLike reports text that can spell an identifier or a built-in
// identifier used as a name.
func dartNameLike(text string) bool {
	if text == "" {
		return false
	}
	c := text[0]
	return c == '_' || c == '$' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= 0x80
}
