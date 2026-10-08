//go:build cgo

package treesitter

import (
	"context"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	scalagrammar "github.com/smacker/go-tree-sitter/scala"

	"github.com/isink17/codegraph/internal/graph"
)

// ScalaAdapter extracts declarations, imports and call references from Scala 2
// and Scala 3 sources. It builds no call graph: which declaration a Scala call
// runs depends on implicit and given scope, extension methods, inheritance,
// overload resolution and apply/unapply desugaring, none of which source
// syntax alone proves. Every call stays an unresolved reference.
//
// Declarations are recorded only where they are members: the top level, a
// package body, and the bodies of classes, objects, traits and enums. Local
// definitions inside a method or block are not addressable and are skipped.
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
	if at := scalaLayoutErrorByte(root, true, true, -1, content); at != ^uint32(0) {
		limit = min(limit, at+1)
	}
	scalaMembers(root, "", nil, limit, content, &pf)
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
// body, an unbraced extension, the top level) every member that starts a
// line must start at the column of the first one, and in a `:` body deeper
// than the line its owner starts on. A member sharing its line with code
// before it is not checked, nor is a braced body: braces delimit it. End
// markers are checked in every scope, see scalaEndMarkerError.
func scalaLayoutErrorByte(parent *sitter.Node, pkgLevel, indented bool, ownerIndent int, content []byte) uint32 {
	errAt := ^uint32(0)
	first := -1
	var seen []*sitter.Node
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
		if col, ok := scalaLineColumn(child.StartByte(), content); ok && indented {
			if first < 0 {
				first = col
			}
			if col != first || col <= ownerIndent {
				errAt = min(errAt, child.StartByte())
			}
		}
		if body == nil {
			continue
		}
		colon := firstChild(body, "{") == nil
		if typ != "extension_definition" && body.ChildCount() > 0 {
			colon = body.Child(0).Type() == ":"
		}
		errAt = min(errAt, scalaLayoutErrorByte(body, typ == "package_clause", colon, scalaLineIndent(child.StartByte(), content), content))
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
			got = scalaPackageName(name, content)
		default:
			got = scalaName(name, content)
		}
		if got != want {
			continue
		}
		if j < len(seen)-1 {
			return seen[j+1].StartByte()
		}
		if col, ok := scalaLineColumn(end.StartByte(), content); !ok || col != scalaLineIndent(def.StartByte(), content) {
			return def.StartByte()
		}
		return ^uint32(0)
	}
	if len(seen) > 0 {
		return seen[0].StartByte()
	}
	return end.StartByte()
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
