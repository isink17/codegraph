//go:build cgo

package treesitter

import (
	"context"
	"path/filepath"
	"strconv"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	scalagrammar "github.com/smacker/go-tree-sitter/scala"

	"github.com/isink17/codegraph/internal/graph"
)

// ScalaAdapter extracts declarations, imports and call references from Scala 2
// and Scala 3 sources. Which declaration a Scala call runs generally depends
// on implicit and given scope, extension methods, inheritance, overload
// resolution and apply/unapply desugaring, none of which source syntax alone
// proves, so calls stay unresolved references. The one exception is a bare
// call to a local def of the same file; see scalaLocalFunctions.
//
// Members are recorded where they are declared: the top level, a package
// body, and the bodies of classes, objects, traits and enums. Local defs are
// recorded only in clean files; see scalaLocalFunctions.
// In a file with a parse error only the declarations that end before the
// first error are recorded, and none when an opening brace is missing; see
// scalaMembers and scalaExcessCloseBrace. Damaged indentation syntax that
// parses without an error is bounded the same way; see scalaLayoutErrorByte.
type ScalaAdapter struct{}

func NewScala() *ScalaAdapter            { return &ScalaAdapter{} }
func (a *ScalaAdapter) Language() string { return "scala" }

// `.sc` (scala-cli/Ammonite scripts) is not claimed: SuperCollider uses the
// same extension.
func (a *ScalaAdapter) Extensions() []string { return []string{".scala"} }
func (a *ScalaAdapter) Supports(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".scala")
}

func (a *ScalaAdapter) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	root, err := parse(ctx, scalagrammar.GetLanguage(), content)
	if err != nil {
		return graph.ParsedFile{}, err
	}
	pf := graph.ParsedFile{Language: "scala", FileTokens: computeFileTokens(content)}
	// Parent walks cost a cgo call per level; a clean tree needs none.
	broken := root.HasError()
	errAt, limit := ^uint32(0), ^uint32(0)
	if broken {
		errAt = scalaFirstErrorByte(root)
		limit = errAt
		if scalaExcessCloseBrace(root) {
			limit = 0
		}
	}
	// Damaged indentation syntax can parse without an error. The first
	// member laid out as no clean source lays it out bounds what is recorded
	// like an error, except that a body ending right where that member
	// starts, as an indented body does, is complete and kept.
	layoutErr := scalaLayoutErrorByte(root, true, false, -1, content)
	if layoutErr != ^uint32(0) {
		limit = min(limit, layoutErr+1)
	}
	scalaMembers(root, "", nil, limit, content, &pf)
	// Local defs and the calls proven to reach them come from clean files
	// only: in a damaged one no scope's extent is known.
	if !broken && layoutErr == ^uint32(0) && !scalaLocalDamage(root, content) {
		scalaLocalFunctions(root, content, &pf)
	}
	// Imports are file-scoped, so a stray brace cannot misplace one, but an
	// import running into an error may have lost a path segment or selector:
	// only imports ended by a newline or `;` before the first error count.
	for _, imp := range findDescendants(root, "import_declaration") {
		if !broken || imp.EndByte() < errAt && strings.ContainsAny(string(content[imp.EndByte():errAt]), "\n;") {
			scalaImports(imp, content, &pf)
		}
	}
	for _, call := range findDescendants(root, "call_expression") {
		if broken && scalaInError(call) {
			continue
		}
		name := scalaCallName(childByFieldName(call, "function"), content)
		if name == "" {
			continue
		}
		pf.References = append(pf.References, graph.Reference{Kind: "call", Name: name, QualifiedName: name, Range: nodeRange(call)})
	}
	return pf, nil
}

// scalaOwner is one enclosing type. Objects are spelled with a trailing `$`
// in stable keys only, so a class and its companion object, and their members
// of the same name, keep distinct identities while sharing a qualified name.
type scalaOwner struct {
	name   string
	object bool
}

func scalaPath(pkg string, owners []scalaOwner, name string, keyed bool) string {
	parts := make([]string, 0, len(owners)+2)
	if pkg != "" {
		parts = append(parts, pkg)
	}
	for _, o := range owners {
		if keyed && o.object {
			parts = append(parts, o.name+"$")
		} else {
			parts = append(parts, o.name)
		}
	}
	return strings.Join(append(parts, name), ".")
}

// scalaFirstErrorByte is where the first ERROR or MISSING node starts.
func scalaFirstErrorByte(n *sitter.Node) uint32 {
	if n.Type() == "ERROR" || n.IsMissing() {
		return n.StartByte()
	}
	for i := range int(n.ChildCount()) {
		if child := n.Child(i); child.HasError() {
			return scalaFirstErrorByte(child)
		}
	}
	return n.EndByte()
}

// scalaExcessCloseBrace reports more `}` than `{` tokens, which means an
// opening brace is missing. Recovery then closes a body at an earlier `}`
// and moves the members after it out to the enclosing scope, all before the
// first error, which is only the excess `}` at the end. Where the missing
// brace was is unknown, so no declaration's owner is.
func scalaExcessCloseBrace(root *sitter.Node) bool {
	depth := 0
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if n.ChildCount() == 0 && !n.IsMissing() {
			switch n.Type() {
			case "{":
				depth++
			case "}":
				depth--
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

// scalaPackageLevel lists what may appear at the top level or in a package
// body. Anything else there, such as an expression, means the source is
// damaged: a misspelt `package` keyword parses as one and leaves every
// declaration after it without the package prefix.
var scalaPackageLevel = map[string]bool{
	"package_clause": true, "package_object": true, "import_declaration": true, "export_declaration": true,
	"class_definition": true, "object_definition": true, "trait_definition": true, "enum_definition": true,
	"function_definition": true, "function_declaration": true, "val_definition": true, "val_declaration": true,
	"var_definition": true, "var_declaration": true, "given_definition": true, "type_definition": true,
	"extension_definition": true, "comment": true, "block_comment": true,
}

// scalaLayoutErrorByte is where the first member starts that clean,
// consistently indented source does not produce, or ^0 when there is none;
// Parse treats it as the first parse error. The grammar parses some damaged
// indentation syntax without an error: without the `:` that opens a body, or
// with the body's first member indented deeper than the rest, the later
// members move out to the enclosing scope. So in an indented scope (a `:`
// body, an unbraced extension) every member that starts a line must start
// at the column of the first one, deeper than the line its owner starts on;
// the top level has a narrower rule, below. Columns are taken from
// scalaAnchor. A member sharing its line with code before it is not
// checked, nor is a braced body: braces delimit it. End markers are checked
// in every scope, see scalaEndMarkerError.
func scalaLayoutErrorByte(parent *sitter.Node, pkgLevel, indented bool, ownerIndent int, content []byte) uint32 {
	errAt := ^uint32(0)
	first := -1
	var seen []*sitter.Node
	prevCol, prevType, prevBody, prevColon := -1, false, false, false
	for i := range int(parent.ChildCount()) {
		child := parent.Child(i)
		typ := child.Type()
		if typ == "end" && !child.IsNamed() {
			var ident *sitter.Node
			if i+1 < int(parent.ChildCount()) {
				ident = parent.Child(i + 1)
			}
			errAt = min(errAt, scalaEndMarkerError(seen, child, ident, content))
			continue
		}
		if !child.IsNamed() || typ == "comment" || typ == "block_comment" {
			continue
		}
		if pkgLevel && !scalaPackageLevel[typ] {
			return min(errAt, child.StartByte())
		}
		seen = append(seen, child)
		var body *sitter.Node
		switch typ {
		case "class_definition", "object_definition", "trait_definition", "enum_definition", "package_clause":
			body = childByFieldName(child, "body")
		case "extension_definition":
			body = child
		case "function_definition", "function_declaration", "val_definition", "val_declaration",
			"var_definition", "var_declaration", "given_definition", "type_definition":
		default:
			continue
		}
		isType := typ == "class_definition" || typ == "object_definition" || typ == "trait_definition" || typ == "enum_definition"
		colon := false
		if body != nil {
			colon = firstChild(body, "{") == nil
			if typ != "extension_definition" && body.ChildCount() > 0 {
				colon = body.Child(0).Type() == ":"
			}
		}
		anchor := scalaAnchor(child)
		col, ok := scalaLineColumn(anchor, content)
		if ok && indented {
			if first < 0 {
				first = col
			}
			if col != first || col <= ownerIndent {
				errAt = min(errAt, child.StartByte())
			}
		}
		// At the top level only a member indented under the type before it
		// is checked: under one with a `:` body it would have been a member
		// of that body, and a def or value under a bodyless type is that
		// type's body without its `:`. Braced code may indent freely there.
		if ok && ownerIndent < 0 && prevCol >= 0 && col > prevCol && (prevColon || prevType && !prevBody && !isType) {
			errAt = min(errAt, child.StartByte())
		}
		if ownerIndent < 0 && typ != "package_clause" {
			prevCol, prevType, prevBody, prevColon = -1, isType, body != nil, colon
			if ok {
				prevCol = col
			}
		}
		if body == nil {
			continue
		}
		errAt = min(errAt, scalaLayoutErrorByte(body, typ == "package_clause", colon, scalaLineIndent(anchor, content), content))
	}
	return errAt
}

// scalaEndMarkerError checks the end marker `end X` after the members seen
// so far in a scope, returning where the damage starts or ^0. The grammar
// attaches the marker to the scope holding X, right after X, and clean
// source aligns it with the line X starts on. Following other members, the
// marker shows they moved out of the body it closes, as a dedented line
// does, so the first of them is suspect; misaligned, X itself moved.
// Keyword markers (`end extension`, `end given`) name nothing and pass.
func scalaEndMarkerError(seen []*sitter.Node, end, ident *sitter.Node, content []byte) uint32 {
	if ident == nil || ident.Type() != "_end_ident" {
		return ^uint32(0)
	}
	want := scalaName(ident, content)
	for j := len(seen) - 1; j >= 0; j-- {
		def := seen[j]
		name := childByFieldName(def, "name")
		if name == nil {
			name = childByFieldName(def, "pattern")
		}
		got := ""
		switch {
		case name == nil:
		case def.Type() == "package_clause":
			// `package a.b:` closes with `end b`.
			got = scalaPackageName(name, content)
			got = got[strings.LastIndexByte(got, '.')+1:]
		default:
			got = scalaName(name, content)
		}
		if got != want {
			continue
		}
		if j < len(seen)-1 {
			return seen[j+1].StartByte()
		}
		if col, ok := scalaLineColumn(end.StartByte(), content); !ok || col != scalaLineIndent(scalaAnchor(def), content) {
			return def.StartByte()
		}
		return ^uint32(0)
	}
	if len(seen) > 0 {
		return seen[0].StartByte()
	}
	return end.StartByte()
}

// scalaAnchor is where a definition starts once annotations before it are
// skipped: an annotation may sit on its own line at any column.
func scalaAnchor(def *sitter.Node) uint32 {
	for i := range int(def.ChildCount()) {
		if c := def.Child(i); c.Type() != "annotation" && c.Type() != "comment" && c.Type() != "block_comment" {
			return c.StartByte()
		}
	}
	return def.StartByte()
}

// scalaLineColumn is the column of the byte at, when only spaces and tabs
// precede it on its line.
func scalaLineColumn(at uint32, content []byte) (int, bool) {
	for i := int(at) - 1; i >= 0; i-- {
		switch content[i] {
		case '\n':
			return int(at) - 1 - i, true
		case ' ', '\t':
		default:
			return 0, false
		}
	}
	return int(at), true
}

// scalaLineIndent is the indentation of the line holding the byte at.
func scalaLineIndent(at uint32, content []byte) int {
	start := int(at)
	for start > 0 && content[start-1] != '\n' {
		start--
	}
	end := start
	for end < len(content) && (content[end] == ' ' || content[end] == '\t') {
		end++
	}
	return end - start
}

// scalaMembers records the member declarations directly under parent. A
// bodyless package clause (`package a.b`) applies to every following sibling
// and chains with earlier ones; a package with a body applies to the body.
//
// Only declarations that end before limit, the first parse error, are
// recorded. After the error, recovery may have closed a body early, left a
// later declaration inside an unclosed one, or dropped a package clause, so
// owners there are guesses. A container that does not end before the error
// is skipped with all of its members, since its extent is no longer known.
func scalaMembers(parent *sitter.Node, pkg string, owners []scalaOwner, limit uint32, content []byte, pf *graph.ParsedFile) {
	for i := range int(parent.ChildCount()) {
		child := parent.Child(i)
		if child.EndByte() >= limit {
			return
		}
		switch child.Type() {
		case "package_clause":
			name := scalaPackageName(childByFieldName(child, "name"), content)
			if name == "" {
				return
			}
			full := name
			if pkg != "" {
				full = pkg + "." + name
			}
			if body := childByFieldName(child, "body"); body != nil {
				scalaMembers(body, full, owners, limit, content, pf)
			} else {
				pkg = full
			}
		case "class_definition":
			scalaType(child, "class", pkg, owners, limit, content, pf)
		case "object_definition":
			scalaType(child, "object", pkg, owners, limit, content, pf)
		case "trait_definition":
			scalaType(child, "trait", pkg, owners, limit, content, pf)
		case "enum_definition":
			scalaType(child, "enum", pkg, owners, limit, content, pf)
		case "function_definition", "function_declaration":
			scalaFunction(child, pkg, owners, content, pf)
		case "extension_definition":
			// Extension methods are members of the enclosing scope; the
			// receiver they extend is not modelled. Each method is its own
			// `body` child, in both the indented and the braced form.
			scalaMembers(child, pkg, owners, limit, content, pf)
		case "val_definition", "var_definition":
			pattern := childByFieldName(child, "pattern")
			switch {
			case pattern == nil:
			case pattern.Type() == "identifier":
				scalaValue(child, pattern, pkg, owners, content, pf)
			case pattern.Type() == "identifiers":
				for j := range int(pattern.NamedChildCount()) {
					scalaValue(child, pattern.NamedChild(j), pkg, owners, content, pf)
				}
			}
			// Destructuring patterns (`val (a, b) = ...`) are not recorded.
		case "val_declaration", "var_declaration", "given_definition":
			// An anonymous given has no name to record.
			if name := childByFieldName(child, "name"); name != nil {
				scalaValue(child, name, pkg, owners, content, pf)
			}
		case "type_definition":
			if name := childByFieldName(child, "name"); name != nil {
				scalaSymbol(child, "type", scalaName(name, content), "type", pkg, owners, content, pf)
			}
		}
	}
}

func scalaType(node *sitter.Node, kind, pkg string, owners []scalaOwner, limit uint32, content []byte, pf *graph.ParsedFile) {
	nameNode := childByFieldName(node, "name")
	if nameNode == nil {
		return
	}
	name := scalaName(nameNode, content)
	prefix := "type"
	if kind == "object" {
		prefix = "object"
	}
	scalaSymbol(node, kind, name, prefix, pkg, owners, content, pf)
	if body := childByFieldName(node, "body"); body != nil {
		inner := append(owners[:len(owners):len(owners)], scalaOwner{name: name, object: kind == "object"})
		scalaMembers(body, pkg, inner, limit, content, pf)
	}
}

func scalaFunction(node *sitter.Node, pkg string, owners []scalaOwner, content []byte, pf *graph.ParsedFile) {
	nameNode := childByFieldName(node, "name")
	if nameNode == nil {
		return
	}
	name := scalaName(nameNode, content)
	if name == "this" {
		return // auxiliary constructor
	}
	// Overloads share a stable key, as in the Java and Kotlin adapters; the
	// signature and the declaration position tell them apart.
	scalaSymbol(node, "function", name, "func", pkg, owners, content, pf)
}

func scalaValue(node, nameNode *sitter.Node, pkg string, owners []scalaOwner, content []byte, pf *graph.ParsedFile) {
	scalaSymbol(node, "value", scalaName(nameNode, content), "value", pkg, owners, content, pf)
}

func scalaSymbol(node *sitter.Node, kind, name, keyPrefix, pkg string, owners []scalaOwner, content []byte, pf *graph.ParsedFile) {
	if name == "" {
		return
	}
	container := pkg
	if len(owners) > 0 {
		container = scalaPath(pkg, owners[:len(owners)-1], owners[len(owners)-1].name, false)
	}
	pf.Symbols = append(pf.Symbols, graph.Symbol{
		Language:      "scala",
		Kind:          kind,
		Name:          name,
		QualifiedName: scalaPath(pkg, owners, name, false),
		ContainerName: container,
		Signature:     scalaSignature(node, content),
		Visibility:    scalaVisibility(node, content),
		Range:         nodeRange(node),
		DocSummary:    prevCommentText(node, content),
		StableKey:     keyPrefix + ":scala:" + scalaPath(pkg, owners, name, true),
	})
}

// scalaSignature is the declaration header: everything before its body or `=`,
// with whitespace collapsed.
func scalaSignature(node *sitter.Node, content []byte) string {
	end := node.EndByte()
	for _, field := range []string{"body", "value"} {
		if b := childByFieldName(node, field); b != nil {
			end = b.StartByte()
			break
		}
	}
	header := strings.TrimSpace(string(content[node.StartByte():end]))
	header = strings.TrimSpace(strings.TrimSuffix(header, "="))
	return strings.Join(strings.Fields(header), " ")
}

func scalaVisibility(node *sitter.Node, content []byte) string {
	mods := firstChild(node, "modifiers")
	if access := firstChild(mods, "access_modifier"); access != nil {
		text := nodeText(access, content)
		for _, v := range []string{"private", "protected"} {
			if strings.HasPrefix(text, v) {
				return v
			}
		}
	}
	return "public"
}

func scalaName(node *sitter.Node, content []byte) string {
	return strings.Trim(nodeText(node, content), "`")
}

func scalaPackageName(node *sitter.Node, content []byte) string {
	if node == nil {
		return ""
	}
	var parts []string
	for i := range int(node.NamedChildCount()) {
		if part := node.NamedChild(i); part.Type() == "identifier" {
			parts = append(parts, scalaName(part, content))
		}
	}
	return strings.Join(parts, ".")
}

// scalaImports reads one import statement, which may hold several
// comma-separated clauses (`import a.b, c.d`). Hidden selectors (`{X => _}`)
// and given-by-type selectors (`{given Ord[?]}`) import no name and are not
// recorded; `given` alone is recorded in Imports only, since it brings
// instances into implicit scope rather than binding a name.
func scalaImports(node *sitter.Node, content []byte, pf *graph.ParsedFile) {
	var path []string
	flush := func() {
		if len(path) > 0 {
			name := path[len(path)-1]
			pf.Imports = append(pf.Imports, strings.Join(path, "."))
			pf.Scope.Imports = append(pf.Scope.Imports, graph.ScopeImport{
				SourceSpecifier: strings.Join(path[:len(path)-1], "."), ImportedName: name, LocalName: name, Kind: graph.ScopeImportNamed,
			})
		}
		path = nil
	}
	// renamed records `q => r` or `q as r` under base; `q => _` hides q.
	renamed := func(sel *sitter.Node, base string) {
		name, alias := childByFieldName(sel, "name"), childByFieldName(sel, "alias")
		if base == "" || name == nil || alias == nil || alias.Type() != "identifier" {
			return
		}
		imported := scalaName(name, content)
		pf.Imports = append(pf.Imports, base+"."+imported)
		pf.Scope.Imports = append(pf.Scope.Imports, graph.ScopeImport{SourceSpecifier: base, ImportedName: imported, LocalName: scalaName(alias, content), Kind: graph.ScopeImportNamed})
	}
	for i := range int(node.ChildCount()) {
		child := node.Child(i)
		base := strings.Join(path, ".")
		switch child.Type() {
		case "identifier":
			path = append(path, scalaName(child, content))
		case ",":
			flush()
		case "namespace_wildcard":
			scalaWildcard(base, nodeText(child, content), pf)
			path = nil
		case "arrow_renamed_identifier", "as_renamed_identifier":
			renamed(child, base) // Scala 3 unbraced `import p.q as r`
			path = nil
		case "namespace_selectors":
			for j := range int(child.NamedChildCount()) {
				sel := child.NamedChild(j)
				switch sel.Type() {
				case "identifier":
					name := scalaName(sel, content)
					pf.Imports = append(pf.Imports, base+"."+name)
					pf.Scope.Imports = append(pf.Scope.Imports, graph.ScopeImport{SourceSpecifier: base, ImportedName: name, LocalName: name, Kind: graph.ScopeImportNamed})
				case "namespace_wildcard":
					scalaWildcard(base, nodeText(sel, content), pf)
				case "arrow_renamed_identifier", "as_renamed_identifier":
					renamed(sel, base)
				}
			}
			path = nil
		}
	}
	flush()
}

func scalaWildcard(base, spelling string, pf *graph.ParsedFile) {
	if base == "" {
		return
	}
	if spelling == "given" {
		pf.Imports = append(pf.Imports, base+".given")
		return
	}
	pf.Imports = append(pf.Imports, base+"._")
	pf.Scope.Imports = append(pf.Scope.Imports, graph.ScopeImport{SourceSpecifier: base, Kind: graph.ScopeImportNamespace, Wildcard: true})
}

// scalaCallName names the callee of `f(...)`, `a.b.f(...)` or `f[T](...)`. A
// member call on a computed receiver (`xs.map(g).foreach(h)`) is named by the
// member alone. Any other callee shape yields no reference.
func scalaCallName(fn *sitter.Node, content []byte) string {
	if fn == nil {
		return ""
	}
	switch fn.Type() {
	case "identifier", "operator_identifier":
		return scalaName(fn, content)
	case "generic_function":
		return scalaCallName(childByFieldName(fn, "function"), content)
	case "field_expression":
		field := childByFieldName(fn, "field")
		if field == nil {
			return ""
		}
		if receiver := scalaStablePath(childByFieldName(fn, "value"), content); receiver != "" {
			return receiver + "." + scalaName(field, content)
		}
		return scalaName(field, content)
	}
	return ""
}

func scalaStablePath(node *sitter.Node, content []byte) string {
	if node == nil {
		return ""
	}
	switch node.Type() {
	case "identifier":
		return scalaName(node, content)
	case "field_expression":
		value := scalaStablePath(childByFieldName(node, "value"), content)
		field := childByFieldName(node, "field")
		if value == "" || field == nil {
			return ""
		}
		return value + "." + scalaName(field, content)
	}
	return ""
}

func scalaInError(node *sitter.Node) bool {
	for n := node; n != nil; n = n.Parent() {
		if n.Type() == "ERROR" || n.IsMissing() {
			return true
		}
	}
	return false
}

// scalaLocalFunctions records the local defs of a clean file (a `def`
// statement in a block, an indented block or a case clause, outside any local
// type) and returns, in pf.Edges, a call edge for every bare `f(...)` that
// Scala's scoping provably binds to one of them. A local def is a binding of
// the highest precedence in the innermost scope declaring it, so it shadows
// every member, inherited member, import and package binding of an enclosing
// scope (SLS 2; Scala 3 keeps the rule); it cannot be overridden, and a bare
// call is not subject to implicit conversion or extension-method lookup,
// which apply to selections. What remains is shadowing between the call and
// the def, which is refused wholesale: the call binds only when, in the
// whole declaring scope, f is spelled nowhere but as that def's name, as a
// bare callee or as a selected member (`x.f`), and the scope holds no import
// or export. Any parameter, val, var, pattern, generator, given, nested def,
// type or other use of the name refuses every call in the scope. A call
// inside a type, template, given, extension, quote or splice between it and
// the def also stays unresolved, since a member there may shadow the def.
func scalaLocalFunctions(root *sitter.Node, content []byte, pf *graph.ParsedFile) {
	recorded := map[uint32]bool{} // start bytes of the recorded local defs
	var calls []*sitter.Node
	var walk func(n *sitter.Node, inExpr bool)
	walk = func(n *sitter.Node, inExpr bool) {
		typ := n.Type()
		if scalaOpaqueScopes[typ] || inExpr && scalaMemberScopes[typ] {
			return
		}
		switch typ {
		case "function_definition":
			if inExpr && scalaLocalScopes[n.Parent().Type()] {
				if scalaLocalSymbol(n, content, pf) {
					recorded[n.StartByte()] = true
				}
			}
		case "call_expression":
			calls = append(calls, n)
		}
		inner := inExpr || !scalaMemberScopes[typ]
		for i := range int(n.NamedChildCount()) {
			walk(n.NamedChild(i), inner)
		}
	}
	walk(root, false)
	proven := map[[2]uint32]bool{} // (scope start, def start) pairs that passed
	refused := map[[2]uint32]bool{}
	for _, call := range calls {
		callee := childByFieldName(call, "function")
		if callee == nil || callee.Type() != "identifier" {
			continue
		}
		name := scalaName(callee, content)
		scope, def := scalaLocalBinding(call, name, content)
		if def == nil || !recorded[def.StartByte()] {
			continue
		}
		key := [2]uint32{scope.StartByte(), def.StartByte()}
		if !proven[key] && !refused[key] {
			if scalaSoleSpelling(scope, def, name, content) {
				proven[key] = true
			} else {
				refused[key] = true
			}
		}
		if !proven[key] {
			continue
		}
		decl, at := nodeRange(def), nodeRange(call)
		pf.Edges = append(pf.Edges, graph.Edge{
			DstName:  name,
			Kind:     "calls",
			Evidence: graph.ScalaLocalFunctionEvidence + strconv.Itoa(decl.StartLine) + ":" + strconv.Itoa(decl.StartCol),
			Line:     at.StartLine,
			Col:      at.StartCol,
		})
	}
}

// scalaLocalScopes are the scopes whose statements may be local defs.
var scalaLocalScopes = map[string]bool{"block": true, "indented_block": true, "case_clause": true}

// scalaMemberScopes hold members, not statements. Met inside an expression
// they are a local type, whose members may shadow anything outside it.
var scalaMemberScopes = map[string]bool{
	"compilation_unit": true, "package_clause": true, "package_object": true,
	"class_definition": true, "object_definition": true, "trait_definition": true, "enum_definition": true,
	"template_body": true, "enum_body": true, "extension_definition": true,
}

// scalaOpaqueScopes are never searched: a given or anonymous class template
// has members of its own, and quoted code is not run where it is written.
var scalaOpaqueScopes = map[string]bool{
	"given_definition": true, "instance_expression": true, "with_template_body": true,
	"quote_expression": true, "splice_expression": true, "macro_body": true,
}

// scalaLocalBinding finds the innermost local scope around call that declares
// a def named name as one of its own statements, and that def. It gives up
// at a type, template or other scope that may hold a shadowing member, and
// when the scope declares the name more than once. A case clause's statements
// are in scope only in its body: a call in its pattern or guard looks
// further out.
func scalaLocalBinding(call *sitter.Node, name string, content []byte) (*sitter.Node, *sitter.Node) {
	child := call
	for n := call.Parent(); n != nil; child, n = n, n.Parent() {
		typ := n.Type()
		if scalaMemberScopes[typ] || scalaOpaqueScopes[typ] {
			return nil, nil
		}
		if !scalaLocalScopes[typ] || typ == "case_clause" && !scalaClauseBody(n, child) {
			continue
		}
		var def *sitter.Node
		count := 0
		for i := range int(n.NamedChildCount()) {
			c := n.NamedChild(i)
			if c.Type() == "function_definition" && scalaName(childByFieldName(c, "name"), content) == name {
				def = c
				count++
			}
		}
		switch count {
		case 0:
			continue
		case 1:
			return n, def
		}
		return nil, nil
	}
	return nil, nil
}

// scalaClauseBody reports whether child, a child of case clause n, is one of
// its body statements rather than its pattern or guard.
func scalaClauseBody(n, child *sitter.Node) bool {
	for i := range int(n.ChildCount()) {
		if scalaSameNode(n.Child(i), child) {
			return n.FieldNameForChild(i) == "body"
		}
	}
	return false
}

// scalaSoleSpelling reports whether name occurs in scope only as def's name,
// as a bare callee `name(...)` or as a selected member `x.name`, and the
// scope holds no import or export.
func scalaSoleSpelling(scope, def *sitter.Node, name string, content []byte) bool {
	defName := childByFieldName(def, "name")
	ok := true
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if !ok {
			return
		}
		switch n.Type() {
		case "import_declaration", "export_declaration":
			ok = false
			return
		case "identifier", "type_identifier", "operator_identifier":
			if scalaName(n, content) != name || scalaSameNode(n, defName) {
				return
			}
			parent := n.Parent()
			switch parent.Type() {
			case "call_expression":
				ok = scalaSameNode(childByFieldName(parent, "function"), n)
			case "field_expression":
				ok = scalaSameNode(childByFieldName(parent, "field"), n)
			default:
				ok = false
			}
			return
		}
		for i := range int(n.NamedChildCount()) {
			walk(n.NamedChild(i))
		}
	}
	walk(scope)
	return ok
}

func scalaSameNode(a, b *sitter.Node) bool {
	return a != nil && b != nil && a.StartByte() == b.StartByte() && a.EndByte() == b.EndByte() && a.Type() == b.Type()
}

// scalaLocalSymbol records a local def under the innermost symbol enclosing
// it. Its stable key carries its position: local defs of one name in
// different blocks are distinct declarations.
func scalaLocalSymbol(def *sitter.Node, content []byte, pf *graph.ParsedFile) bool {
	nameNode := childByFieldName(def, "name")
	if nameNode == nil {
		return false
	}
	name := scalaName(nameNode, content)
	if name == "" {
		return false
	}
	rng := nodeRange(def)
	container := ""
	var best graph.Position
	for _, s := range pf.Symbols {
		r := s.Range
		if scalaBefore(r.StartLine, r.StartCol, rng.StartLine, rng.StartCol) && scalaBefore(rng.EndLine, rng.EndCol, r.EndLine, r.EndCol) &&
			(container == "" || scalaBefore(best.StartLine, best.StartCol, r.StartLine, r.StartCol)) {
			container, best = s.QualifiedName, r
		}
	}
	qname := name
	if container != "" {
		qname = container + "." + name
	}
	pf.Symbols = append(pf.Symbols, graph.Symbol{
		Language:      "scala",
		Kind:          "function",
		Name:          name,
		QualifiedName: qname,
		ContainerName: container,
		Signature:     scalaSignature(def, content),
		Range:         rng,
		DocSummary:    prevCommentText(def, content),
		StableKey:     "func:scala:local:" + qname + ":" + strconv.Itoa(rng.StartLine) + ":" + strconv.Itoa(rng.StartCol),
	})
	return true
}

// scalaBefore reports whether (l1, c1) is at or before (l2, c2).
func scalaBefore(l1, c1, l2, c2 int) bool { return l1 < l2 || l1 == l2 && c1 <= c2 }

// scalaHardKeywords are reserved in Scala 2 and Scala 3 alike. The grammar
// accepts some of them as identifiers, which only damaged source spells:
// `def a = 1  class Deep:` parses cleanly as an infix call of `class`.
var scalaHardKeywords = map[string]bool{
	"abstract": true, "case": true, "catch": true, "class": true, "def": true, "do": true, "else": true,
	"extends": true, "final": true, "finally": true, "for": true, "if": true, "implicit": true, "import": true,
	"lazy": true, "match": true, "new": true, "object": true, "override": true, "package": true,
	"private": true, "protected": true, "return": true, "sealed": true, "throw": true, "trait": true,
	"try": true, "type": true, "val": true, "var": true, "while": true, "with": true, "yield": true,
}

// scalaLocalDamage reports layout that clean source does not produce inside
// expressions, where scalaLayoutErrorByte does not look: a hard keyword
// parsed as an identifier, or an indented block whose statements starting a
// line are not all at one column deeper than the line its owner starts on,
// or a def or value whose body starts a line no deeper than the definition.
// Each can move a statement into or out of a block without a parse error.
// The grammar also produces them for some valid code, such as a multi-line
// `||` chain parsed as postfix statements; that tree is wrong too.
func scalaLocalDamage(root *sitter.Node, content []byte) bool {
	damaged := false
	var walk func(n *sitter.Node)
	walk = func(n *sitter.Node) {
		if damaged {
			return
		}
		switch n.Type() {
		case "identifier":
			damaged = scalaHardKeywords[nodeText(n, content)]
			return
		case "function_definition", "val_definition", "var_definition":
			body := childByFieldName(n, "body")
			if body == nil {
				body = childByFieldName(n, "value")
			}
			if body != nil {
				if col, ok := scalaLineColumn(body.StartByte(), content); ok && col <= scalaLineIndent(scalaAnchor(n), content) {
					damaged = true
					return
				}
			}
		case "indented_block":
			owner := scalaLineIndent(n.Parent().StartByte(), content)
			first := -1
			for i := range int(n.NamedChildCount()) {
				c := n.NamedChild(i)
				col, ok := scalaLineColumn(scalaAnchor(c), content)
				if !ok || c.Type() == "comment" || c.Type() == "block_comment" {
					continue
				}
				if first < 0 {
					first = col
				}
				if col != first || col <= owner {
					damaged = true
					return
				}
			}
		}
		for i := range int(n.NamedChildCount()) {
			walk(n.NamedChild(i))
		}
	}
	walk(root)
	return damaged
}
