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
// A declaration whose subtree holds a parse error is skipped with everything
// inside it, because tree-sitter's recovery can nest the declarations that
// follow it under the wrong owner.
type ScalaAdapter struct{}

func NewScala() *ScalaAdapter                { return &ScalaAdapter{} }
func (a *ScalaAdapter) Language() string     { return "scala" }
func (a *ScalaAdapter) Extensions() []string { return []string{".scala", ".sc"} }
func (a *ScalaAdapter) Supports(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".scala" || ext == ".sc"
}

func (a *ScalaAdapter) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	root, err := parse(ctx, scalagrammar.GetLanguage(), content)
	if err != nil {
		return graph.ParsedFile{}, err
	}
	pf := graph.ParsedFile{Language: "scala", FileTokens: computeFileTokens(content)}
	scalaMembers(root, "", nil, content, &pf)
	// Parent walks cost a cgo call per level; a clean tree needs none.
	broken := root.HasError()
	for _, imp := range findDescendants(root, "import_declaration") {
		if !broken || !scalaInError(imp) {
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

// scalaMembers records the member declarations directly under parent. A
// bodyless package clause (`package a.b`) applies to every following sibling
// and chains with earlier ones; a package with a body applies to the body.
func scalaMembers(parent *sitter.Node, pkg string, owners []scalaOwner, content []byte, pf *graph.ParsedFile) {
	for i := range int(parent.ChildCount()) {
		child := parent.Child(i)
		if child.HasError() {
			if child.Type() == "package_clause" {
				return // every following declaration's package is unknown
			}
			continue
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
				scalaMembers(body, full, owners, content, pf)
			} else {
				pkg = full
			}
		case "class_definition":
			scalaType(child, "class", pkg, owners, content, pf)
		case "object_definition":
			scalaType(child, "object", pkg, owners, content, pf)
		case "trait_definition":
			scalaType(child, "trait", pkg, owners, content, pf)
		case "enum_definition":
			scalaType(child, "enum", pkg, owners, content, pf)
		case "function_definition", "function_declaration":
			scalaFunction(child, pkg, owners, content, pf)
		case "extension_definition":
			// Extension methods are members of the enclosing scope; the
			// receiver they extend is not modelled.
			if body := childByFieldName(child, "body"); body != nil {
				if body.Type() == "function_definition" || body.Type() == "function_declaration" {
					scalaFunction(body, pkg, owners, content, pf)
				} else {
					scalaMembers(body, pkg, owners, content, pf)
				}
			}
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

func scalaType(node *sitter.Node, kind, pkg string, owners []scalaOwner, content []byte, pf *graph.ParsedFile) {
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
		scalaMembers(body, pkg, inner, content, pf)
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
					name, alias := childByFieldName(sel, "name"), childByFieldName(sel, "alias")
					if name == nil || alias == nil || alias.Type() != "identifier" {
						continue
					}
					imported := scalaName(name, content)
					pf.Imports = append(pf.Imports, base+"."+imported)
					pf.Scope.Imports = append(pf.Scope.Imports, graph.ScopeImport{SourceSpecifier: base, ImportedName: imported, LocalName: scalaName(alias, content), Kind: graph.ScopeImportNamed})
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
