//go:build cgo

package treesitter

import (
	"context"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	java "github.com/smacker/go-tree-sitter/java"

	"github.com/isink17/codegraph/internal/graph"
)

// JavaAdapter parses Java source files using tree-sitter.
type JavaAdapter struct {
	// legacyPackage reproduces treesitter:java:v2, which read the package
	// from raw text, for profile-transition tests. Every other fact follows
	// the current parser.
	legacyPackage bool
	// legacyArity reproduces treesitter:java:v3, which recorded no AST
	// argument count on constructor calls and no constructor parameter
	// count, for profile-transition tests.
	legacyArity bool
	// legacyNestedScope reproduces treesitter:java:v4, which did not mark
	// calls inside anonymous, local or enum-constant class bodies.
	legacyNestedScope bool
	// legacyGenericConstruction reproduces treesitter:java:v5, which spelled
	// a generic construction's class with its type arguments (`Box<>`).
	legacyGenericConstruction bool
	// legacyOwnMember reproduces treesitter:java:v6, which did not mark
	// constructions of a member type the calling class declares.
	legacyOwnMember bool
	// legacyLineOnly reproduces treesitter:java:v7, whose edges carried no
	// column, so methods sharing a line could not be told apart.
	legacyLineOnly bool
	// legacyNoInherited reproduces treesitter:java:v8, which did not mark
	// constructions in classes that spell no supertype.
	legacyNoInherited bool
	// legacyImportText reproduces treesitter:java:v9, which read imports from
	// raw text (legacyJavaScope).
	legacyImportText bool
	// legacyCallSupertype reproduces treesitter:java:v10, which did not mark
	// bare calls in classes that spell no supertype.
	legacyCallSupertype bool
}

func NewJava() *JavaAdapter { return &JavaAdapter{} }

// NewJavaV3 returns a parser that reports treesitter:java:v3 and omits the
// constructor arity facts v4 records. It exists only to reproduce v3
// databases in profile-transition tests.
func NewJavaV3() *JavaAdapter {
	return &JavaAdapter{legacyArity: true, legacyNestedScope: true, legacyGenericConstruction: true, legacyOwnMember: true, legacyLineOnly: true, legacyNoInherited: true, legacyImportText: true, legacyCallSupertype: true}
}

// NewJavaV4 returns a parser that reports treesitter:java:v4 and does not mark
// calls inside nested class bodies. It exists only to reproduce v4 databases
// in profile-transition tests.
func NewJavaV4() *JavaAdapter {
	return &JavaAdapter{legacyNestedScope: true, legacyGenericConstruction: true, legacyOwnMember: true, legacyLineOnly: true, legacyNoInherited: true, legacyImportText: true, legacyCallSupertype: true}
}

// NewJavaV5 returns a parser that reports treesitter:java:v5 and spells a
// generic construction's class with its type arguments. It exists only to
// reproduce v5 databases in profile-transition tests.
func NewJavaV5() *JavaAdapter {
	return &JavaAdapter{legacyGenericConstruction: true, legacyOwnMember: true, legacyLineOnly: true, legacyNoInherited: true, legacyImportText: true, legacyCallSupertype: true}
}

// NewJavaV6 returns a parser that reports treesitter:java:v6 and does not mark
// constructions of a member type the calling class declares. It exists only
// to reproduce v6 databases in profile-transition tests.
func NewJavaV6() *JavaAdapter {
	return &JavaAdapter{legacyOwnMember: true, legacyLineOnly: true, legacyNoInherited: true, legacyImportText: true, legacyCallSupertype: true}
}

// NewJavaV7 returns a parser that reports treesitter:java:v7 and records no
// column on edges. It exists only to reproduce v7 databases in
// profile-transition tests.
func NewJavaV7() *JavaAdapter {
	return &JavaAdapter{legacyLineOnly: true, legacyNoInherited: true, legacyImportText: true, legacyCallSupertype: true}
}

// NewJavaV8 returns a parser that reports treesitter:java:v8 and does not mark
// constructions in classes that spell no supertype. It exists only to
// reproduce v8 databases in profile-transition tests.
func NewJavaV8() *JavaAdapter {
	return &JavaAdapter{legacyNoInherited: true, legacyImportText: true, legacyCallSupertype: true}
}

// NewJavaV9 returns a parser that reports treesitter:java:v9 and reads
// imports from raw text, so `import staticpkg.Bag;` names pkg.Bag. It exists
// only to reproduce v9 databases in profile-transition tests.
func NewJavaV9() *JavaAdapter { return &JavaAdapter{legacyImportText: true, legacyCallSupertype: true} }

// NewJavaV10 returns a parser that reports treesitter:java:v10 and does not
// mark bare calls in classes that spell no supertype. It exists only to
// reproduce v10 databases in profile-transition tests.
func NewJavaV10() *JavaAdapter { return &JavaAdapter{legacyCallSupertype: true} }

// NewJavaV2 returns a parser that reports treesitter:java:v2 and takes the
// first `package x;` spelled anywhere in the file, comments and strings
// included, as its package. It exists only to reproduce v2 databases in
// profile-transition tests.
func NewJavaV2() *JavaAdapter {
	return &JavaAdapter{legacyPackage: true, legacyArity: true, legacyNestedScope: true, legacyGenericConstruction: true, legacyOwnMember: true, legacyLineOnly: true, legacyNoInherited: true, legacyImportText: true, legacyCallSupertype: true}
}

func (a *JavaAdapter) Language() string     { return "java" }
func (a *JavaAdapter) Extensions() []string { return []string{".java"} }

func (a *JavaAdapter) Supports(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".java")
}

func (a *JavaAdapter) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	root, err := parse(ctx, java.GetLanguage(), content)
	if err != nil {
		return graph.ParsedFile{}, err
	}

	pf := graph.ParsedFile{
		Language:   "java",
		Scope:      graph.ScopeEvidence{Package: javaPackage(root, content)},
		FileTokens: computeFileTokens(content),
	}
	if a.legacyPackage {
		pf.Scope.Package = legacyPackageEvidence(content, "java")
	}

	javaExtractImports(root, content, &pf, a.legacyImportText)
	javaExtractSymbols(root, pf.Scope.Package, "", "module", content, &pf)
	javaExtractCalls(root, content, &pf, !a.legacyNestedScope, !a.legacyGenericConstruction, !a.legacyOwnMember, !a.legacyLineOnly, !a.legacyNoInherited, !a.legacyCallSupertype)
	if a.legacyArity {
		for i := range pf.Edges {
			if pf.Edges[i].Kind == "constructs" {
				pf.Edges[i].CallArity = nil
			}
		}
		for i := range pf.Symbols {
			if pf.Symbols[i].Kind == "constructor" {
				pf.Symbols[i].ArityMin, pf.Symbols[i].ArityMax = nil, nil
			}
		}
	}
	linkTestsGeneric(pf.Scope.Package, &pf, func(target string) string {
		return "func:java:" + testTargetModule(pf.Scope.Package, "Test", "Tests") + ":" + target
	})
	if jvmImportConflict(pf.Scope.Imports) {
		for i := range pf.JVMTypeEvidence {
			pf.JVMTypeEvidence[i].SyntaxState = "unknown"
		}
	}
	return pf, nil
}

// javaPackage reads the package from the file's one package_declaration, a
// direct child of the program. Its name is the identifier leaves of the name
// node, so neither a comment between them nor an annotation on the
// declaration (package-info.java) is part of it. No declaration, more than
// one, or a declaration with a syntax error is no package rather than a
// guessed one.
func javaPackage(root *sitter.Node, content []byte) string {
	var decl *sitter.Node
	for i := range int(root.NamedChildCount()) {
		if child := root.NamedChild(i); child.Type() == "package_declaration" {
			if decl != nil {
				return ""
			}
			decl = child
		}
	}
	if decl == nil || decl.HasError() {
		return ""
	}
	for i := range int(decl.NamedChildCount()) {
		name := decl.NamedChild(i)
		if name.Type() != "identifier" && name.Type() != "scoped_identifier" {
			continue
		}
		var parts []string
		for _, id := range findDescendants(name, "identifier") {
			parts = append(parts, nodeText(id, content))
		}
		return strings.Join(parts, ".")
	}
	return ""
}

func javaExtractImports(root *sitter.Node, content []byte, pf *graph.ParsedFile, legacyText bool) {
	for _, imp := range findDescendants(root, "import_declaration") {
		// The scoped_identifier child holds the full import path.
		scoped := firstChild(imp, "scoped_identifier")
		importText := nodeText(imp, content)
		if scoped != nil {
			importText = nodeText(scoped, content)
		}
		if importText != "" {
			pf.Imports = append(pf.Imports, importText)
		}
		if legacyText {
			legacyJavaScope(nodeText(imp, content), &pf.Scope.Imports)
			continue
		}
		addJavaScope(imp, content, &pf.Scope.Imports)
	}
}

func javaExtractSymbols(node *sitter.Node, module, container, ownerKind string, content []byte, pf *graph.ParsedFile) {
	for i := range int(node.ChildCount()) {
		child := node.Child(i)
		switch child.Type() {
		case "class_declaration", "interface_declaration", "enum_declaration", "record_declaration":
			javaAddType(child, module, container, content, pf)
		case "method_declaration", "constructor_declaration":
			if ownerKind == "type" {
				javaAddMethod(child, module, container, content, pf)
			}
		case "class_body", "interface_body", "enum_body":
			// Recurse into bodies — the container is set by the parent type.
			javaExtractSymbols(child, module, container, ownerKind, content, pf)
		}
	}
}

func javaAddType(node *sitter.Node, module, parent string, content []byte, pf *graph.ParsedFile) {
	nameNode := childByFieldName(node, "name")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, content)

	qualified := javaQName(module, name)
	if parent != "" {
		qualified = javaQName(module, parent+"."+name)
	}
	container := module
	if parent != "" {
		container = parent
	}
	pf.Symbols = append(pf.Symbols, graph.Symbol{
		Language:      "java",
		Kind:          "type",
		Name:          name,
		QualifiedName: qualified,
		ContainerName: container,
		Visibility:    javaTypeVisibility(node, content),
		Range:         nodeRange(node),
		DocSummary:    prevCommentText(node, content),
		StableKey:     "type:java:" + javaStablePrefix(module) + strings.TrimPrefix(qualified, javaPrefix(module)),
	})
	mods := firstChild(node, "modifiers")
	modText := ""
	if mods != nil {
		modText = nodeText(mods, content)
	}
	pf.JVMTypeEvidence = append(pf.JVMTypeEvidence, graph.JVMTypeEvidence{SymbolIndex: len(pf.Symbols) - 1, Kind: node.Type(), Modifiers: modText, TypeParams: firstChild(node, "type_parameters") != nil, OwnerName: container, SyntaxState: "known", Provenance: "java:tree-sitter:type-declaration"})

	// Recurse into the body with this type as container.
	body := childByFieldName(node, "body")
	if body != nil {
		nextContainer := name
		if parent != "" {
			nextContainer = parent + "." + name
		}
		javaExtractSymbols(body, module, nextContainer, "type", content, pf)
	}
}

func javaAddMethod(node *sitter.Node, module, container string, content []byte, pf *graph.ParsedFile) {
	nameNode := childByFieldName(node, "name")
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, content)
	effectiveContainer := module
	if container != "" {
		effectiveContainer = container
	}
	qualified := javaQName(module, name)
	if container != "" && container != module {
		qualified = javaQName(module, container+"."+name)
	}
	stableKey := "func:java:" + javaStablePrefix(module) + name
	if container != "" && container != module {
		stableKey = "func:java:" + javaStablePrefix(module) + container + ":" + name
	}

	vis := javaMethodVisibility(node, content)

	kind := "function"
	var arityMin, arityMax *int
	if node.Type() == "constructor_declaration" {
		kind = "constructor"
		arityMin, arityMax = javaDeclarationArity(node)
	}
	pf.Symbols = append(pf.Symbols, graph.Symbol{
		ArityMin:      arityMin,
		ArityMax:      arityMax,
		Language:      "java",
		Kind:          kind,
		Name:          name,
		QualifiedName: qualified,
		ContainerName: effectiveContainer,
		Visibility:    vis,
		Static:        javaStatic(node, content),
		Signature:     javaDeclarationSignature(node, content),
		Range:         nodeRange(node),
		DocSummary:    prevCommentText(node, content),
		StableKey:     stableKey,
	})
	fact := graph.JVMTypeEvidence{SymbolIndex: len(pf.Symbols) - 1, Kind: kind, TypeParams: firstChild(node, "type_parameters") != nil, OwnerName: effectiveContainer, SyntaxState: "known", Provenance: "java:tree-sitter:method-signature"}
	if params := firstChild(node, "formal_parameters"); params != nil {
		for i := range int(params.NamedChildCount()) {
			param := params.NamedChild(i)
			if param.Type() != "formal_parameter" && param.Type() != "spread_parameter" && param.Type() != "receiver_parameter" {
				continue
			}
			typ := childByFieldName(param, "type")
			if typ == nil {
				typ = firstChild(param, "type")
			}
			state, syntax := javaTypeSyntax(typ, content)
			fact.Params = append(fact.Params, graph.JVMCallableTypeEvidence{Position: "parameter", Syntax: syntax, SyntaxState: state})
		}
	}
	if kind == "function" {
		if result := childByFieldName(node, "type"); result != nil {
			state, syntax := javaTypeSyntax(result, content)
			fact.Params = append(fact.Params, graph.JVMCallableTypeEvidence{Position: "result", Syntax: syntax, SyntaxState: state})
		}
	}
	pf.JVMTypeEvidence = append(pf.JVMTypeEvidence, fact)
}

func javaTypeSyntax(node *sitter.Node, content []byte) (string, string) {
	if node == nil || node.HasError() {
		return "unknown", ""
	}
	syntax := strings.Join(strings.Fields(nodeText(node, content)), " ")
	if syntax == "" {
		return "unknown", ""
	}
	if strings.Contains(syntax, ".") || len(findDescendants(node, "generic_type")) > 0 || len(findDescendants(node, "type_arguments")) > 0 || node.Type() == "array_type" {
		return "incomplete", syntax
	}
	return "known", syntax
}

func javaPrefix(pkg string) string {
	if pkg == "" {
		return ""
	}
	return pkg + "."
}

func javaQName(pkg, name string) string { return javaPrefix(pkg) + name }

func javaStablePrefix(pkg string) string {
	if pkg == "" {
		return ""
	}
	return pkg + ":"
}

func javaDeclarationSignature(node *sitter.Node, content []byte) string {
	end := node.EndByte()
	if body := childByFieldName(node, "body"); body != nil {
		end = body.StartByte()
	}
	return strings.TrimSpace(string(content[node.StartByte():end]))
}

func javaMethodVisibility(node *sitter.Node, content []byte) string {
	for _, mod := range findChildren(node, "modifiers") {
		text := nodeText(mod, content)
		if strings.Contains(text, "public") {
			return "public"
		}
		if strings.Contains(text, "private") {
			return "private"
		}
		if strings.Contains(text, "protected") {
			return "protected"
		}
	}
	return "package"
}

func javaStatic(node *sitter.Node, content []byte) *bool {
	if node.Type() != "method_declaration" && node.Type() != "constructor_declaration" {
		return nil
	}
	static := false
	for _, mod := range findChildren(node, "modifiers") {
		for _, word := range strings.Fields(nodeText(mod, content)) {
			if word == "static" {
				static = true
				break
			}
		}
	}
	return &static
}

func javaTypeVisibility(node *sitter.Node, content []byte) string {
	return javaVisibility(node, content)
}

func javaVisibility(node *sitter.Node, content []byte) string {
	for _, mod := range findChildren(node, "modifiers") {
		text := nodeText(mod, content)
		for _, visibility := range []string{"public", "private", "protected"} {
			if strings.Contains(text, visibility) {
				return visibility
			}
		}
	}
	return "package"
}

func javaExtractCalls(root *sitter.Node, content []byte, pf *graph.ParsedFile, markNested, rawConstruction, markOwnMember, markColumn, markNoInherited, markNoSupertype bool) {
	for _, creation := range findDescendants(root, "object_creation_expression") {
		typeNode := childByFieldName(creation, "type")
		if typeNode == nil {
			continue
		}
		name := nodeText(typeNode, content)
		if rawConstruction {
			name = javaRawTypeName(creation, typeNode, content)
		}
		if name == "" {
			continue
		}
		evidence := nodeText(creation, content)
		if rawConstruction && javaLocalTypeShadows(creation, name, content) {
			evidence = graph.JavaLocalTypeScopeEvidence
		} else if markOwnMember && javaOwnMemberCreation(root, creation, name, content) {
			evidence = graph.JavaOwnMemberTypeEvidence
		} else if markNoInherited && javaNoInheritedTypeCreation(root, creation, name, content) {
			evidence = graph.JavaNoInheritedTypeEvidence
		}
		pf.Edges = append(pf.Edges, graph.Edge{DstName: name, Kind: "constructs", Evidence: evidence, Line: int(creation.StartPoint().Row) + 1, Col: javaEdgeCol(creation, markColumn), CallArity: javaMethodCallArity(creation)})
	}
	for _, call := range findDescendants(root, "method_invocation") {
		nameNode := childByFieldName(call, "name")
		if nameNode == nil {
			continue
		}
		name := nodeText(nameNode, content)
		fullName := javaCallName(call, name, content)
		if fullName == "" {
			continue
		}
		line := int(call.StartPoint().Row) + 1
		evidence := nodeText(call, content)
		if obj := childByFieldName(call, "object"); markNested && (obj == nil || obj.Type() == "this") && javaCallInNestedClassBody(call) {
			evidence = graph.JavaCallNestedClassScopeEvidence
		} else if obj != nil && rawConstruction && javaLocalTypeShadows(call, fullName, content) {
			evidence = graph.JavaLocalTypeScopeEvidence
		} else if obj == nil && markNoSupertype && javaCallNoSupertype(call) {
			evidence = graph.JavaNoSupertypeCallEvidence
		}
		pf.Edges = append(pf.Edges, graph.Edge{
			SrcSymbolID: 0,
			DstName:     fullName,
			Kind:        "calls",
			Evidence:    evidence,
			Line:        line,
			Col:         javaEdgeCol(call, markColumn),
			CallArity:   javaMethodCallArity(call),
		})
		pf.References = append(pf.References, graph.Reference{
			Kind:          "call",
			Name:          fullName,
			QualifiedName: fullName,
			Range:         nodeRange(call),
		})
	}
}

// javaDeclarationArity counts a declaration's parameters from the AST, so
// generic types and annotation arguments never add to the count. A trailing
// varargs parameter leaves the maximum unbounded (nil); a recovery error
// leaves both unknown (nil, nil).
func javaDeclarationArity(node *sitter.Node) (*int, *int) {
	params := childByFieldName(node, "parameters")
	if params == nil || params.HasError() {
		return nil, nil
	}
	fixed, varargs := 0, false
	for i := range int(params.NamedChildCount()) {
		switch params.NamedChild(i).Type() {
		case "formal_parameter":
			if varargs {
				return nil, nil
			}
			fixed++
		case "spread_parameter":
			if varargs {
				return nil, nil
			}
			varargs = true
		case "receiver_parameter", "line_comment", "block_comment":
		default:
			return nil, nil
		}
	}
	if varargs {
		return &fixed, nil
	}
	return &fixed, &fixed
}

// javaCallInNestedClassBody reports whether call sits in the body of a class
// the adapter extracts no members of: an anonymous class, an enum constant
// body, or a class, interface, enum or record declared inside a method,
// constructor, initializer or lambda (or inside such a class). Lambdas
// themselves are not class bodies: `this` and bare names in a lambda belong
// to the enclosing class.
func javaCallInNestedClassBody(call *sitter.Node) bool {
	for n := call.Parent(); n != nil; n = n.Parent() {
		switch n.Type() {
		case "class_body", "interface_body", "enum_body", "annotation_type_body":
		default:
			continue
		}
		owner := n.Parent()
		if owner == nil {
			return false
		}
		switch owner.Type() {
		case "object_creation_expression", "enum_constant":
			return true
		}
		if outer := owner.Parent(); outer != nil {
			switch outer.Type() {
			case "program", "class_body", "interface_body", "enum_body", "enum_body_declarations", "annotation_type_body":
			default:
				return true
			}
		}
	}
	return false
}

// javaLocalTypeShadows reports whether the first segment of a constructed
// class or a call's owner names a class, interface, enum or record declared
// in a block enclosing node -- a method, constructor, initializer or lambda
// body, or the body of a class the adapter does not model. Such a local type
// is not a recorded symbol and hides every package, member and imported type
// of that name (JLS 6.4.1), so no recorded type may answer. A declaration
// later in the block counts too, which only refuses more. A member type of a
// modeled class is a recorded symbol and is left to the store.
func javaLocalTypeShadows(node *sitter.Node, name string, content []byte) bool {
	head, _, _ := strings.Cut(name, ".")
	if head == "" || head == "this" || head == "super" {
		return false
	}
	for n := node.Parent(); n != nil; n = n.Parent() {
		for i := range int(n.NamedChildCount()) {
			decl := n.NamedChild(i)
			switch decl.Type() {
			case "class_declaration", "interface_declaration", "enum_declaration", "record_declaration", "annotation_type_declaration":
			default:
				continue
			}
			if id := childByFieldName(decl, "name"); id == nil || nodeText(id, content) != head {
				continue
			}
			switch n.Type() {
			case "program", "class_body", "interface_body", "enum_body_declarations", "annotation_type_body":
				if !javaCallInNestedClassBody(decl) {
					continue
				}
			}
			return true
		}
	}
	return false
}

// javaEdgeCol is the 1-based column node starts at, or 0 for a parser
// reproducing a profile that recorded none.
func javaEdgeCol(node *sitter.Node, mark bool) int {
	if !mark {
		return 0
	}
	return int(node.StartPoint().Column) + 1
}

// javaOwnMemberCreation reports whether creation is an unqualified `new` of
// the simple name that a member type of the calling class declares, made
// directly in a method, constructor or lambda body of a class the adapter
// models. A field initializer or initializer block is not credited to a
// member of that class, and a construction inside an anonymous, local or
// enum-constant class body may name a type that body inherits, so neither
// is marked. Own-member marking remains conservatively line-based: no other
// method or constructor declaration may cover the construction's line.
func javaOwnMemberCreation(root, creation *sitter.Node, name string, content []byte) bool {
	body := javaCreationClassBody(root, creation, name)
	return body != nil && javaBodyDeclaresType(body, name, content)
}

// javaNoInheritedTypeCreation reports whether creation is an unqualified `new`
// of a simple name made directly in a method, constructor or lambda body of a
// class that, with every class enclosing it, spells no supertype clause and
// declares no member type of that name. Only java.lang.Object, Enum or Record
// is then inherited, and none declares a member type, so the name means the
// type an import or the package gives it (graph.JavaNoInheritedTypeEvidence).
// A declaration nested in anything but a class or interface body (an enum's
// members, a method) is not walked and marks nothing.
func javaNoInheritedTypeCreation(root, creation *sitter.Node, name string, content []byte) bool {
	body := javaCreationClassBody(root, creation, name)
	for body != nil {
		if javaBodyDeclaresType(body, name, content) {
			return false
		}
		decl := body.Parent()
		if decl == nil {
			return false
		}
		switch decl.Type() {
		case "class_declaration", "interface_declaration", "record_declaration":
		default:
			return false
		}
		for i := range int(decl.NamedChildCount()) {
			switch decl.NamedChild(i).Type() {
			case "superclass", "super_interfaces", "extends_interfaces":
				return false
			}
		}
		switch outer := decl.Parent(); {
		case outer == nil:
			return false
		case outer.Type() == "program":
			return true
		case outer.Type() == "class_body" || outer.Type() == "interface_body":
			body = outer
		default:
			return false
		}
	}
	return false
}

// javaCallNoSupertype reports whether call sits in a class or interface body
// whose declaration, with every class or interface enclosing it, spells no
// extends, implements or interface-extends clause, up to the compilation unit
// (graph.JavaNoSupertypeCallEvidence). Enum, record and annotation bodies,
// and declarations nested in anything but a class or interface body, mark
// nothing: their implicit supertypes or members are not modelled.
func javaCallNoSupertype(call *sitter.Node) bool {
	var body *sitter.Node
	for n := call.Parent(); n != nil && body == nil; n = n.Parent() {
		switch n.Type() {
		case "class_body", "interface_body":
			body = n
		case "program", "enum_body", "enum_body_declarations", "annotation_type_body":
			return false
		}
	}
	for body != nil {
		decl := body.Parent()
		if decl == nil || decl.Type() != "class_declaration" && decl.Type() != "interface_declaration" {
			return false
		}
		for i := range int(decl.NamedChildCount()) {
			switch decl.NamedChild(i).Type() {
			case "superclass", "super_interfaces", "extends_interfaces":
				return false
			}
		}
		switch outer := decl.Parent(); {
		case outer == nil:
			return false
		case outer.Type() == "program":
			return true
		case outer.Type() == "class_body" || outer.Type() == "interface_body":
			body = outer
		default:
			return false
		}
	}
	return false
}

// javaCreationClassBody returns the class or interface body whose method,
// constructor or lambda body holds creation directly, for an unqualified `new`
// of a simple name outside any anonymous, local or enum-constant class body,
// when no other method or constructor declaration covers the creation's line.
// Otherwise it returns nil.
func javaCreationClassBody(root, creation *sitter.Node, name string) *sitter.Node {
	if first := creation.Child(0); first == nil || first.Type() != "new" || strings.Contains(name, ".") || javaCallInNestedClassBody(creation) {
		return nil
	}
	var method *sitter.Node
	for n := creation.Parent(); n != nil && method == nil; n = n.Parent() {
		switch n.Type() {
		case "method_declaration", "constructor_declaration":
			method = n
		case "program", "class_body", "interface_body", "enum_body", "enum_body_declarations", "annotation_type_body":
			return nil
		}
	}
	if method == nil {
		return nil
	}
	body := method.Parent()
	if body == nil || body.Type() != "class_body" && body.Type() != "interface_body" {
		return nil
	}
	row := creation.StartPoint().Row
	for _, kind := range []string{"method_declaration", "constructor_declaration"} {
		for _, other := range findDescendants(root, kind) {
			if !other.Equal(method) && other.StartPoint().Row <= row && row <= other.EndPoint().Row {
				return nil
			}
		}
	}
	return body
}

// javaBodyDeclaresType reports whether a class or interface body declares a
// member type named name.
func javaBodyDeclaresType(body *sitter.Node, name string, content []byte) bool {
	for i := range int(body.NamedChildCount()) {
		decl := body.NamedChild(i)
		switch decl.Type() {
		case "class_declaration", "interface_declaration", "enum_declaration", "record_declaration", "annotation_type_declaration":
			if id := childByFieldName(decl, "name"); id != nil && nodeText(id, content) == name {
				return true
			}
		}
	}
	return false
}

// javaRawTypeName spells a constructed class without its type arguments:
// `Box<>`, `Box<String>` and `a.b.Box<>` construct `Box` and `a.b.Box`. Java
// chooses the class and the constructor without them, so the call carries the
// same identity as a raw construction. Every other spelling keeps its source
// text, which no class lookup matches: type arguments on an outer segment
// (`Outer<A>.Inner<B>`, which javac rejects), and a qualified creation
// (`outer.new Inner<>()`), whose class is a member of outer's type that a bare
// lookup of `Inner` does not find.
func javaRawTypeName(creation, typeNode *sitter.Node, content []byte) string {
	text := nodeText(typeNode, content)
	if typeNode.Type() != "generic_type" || creation.Child(0).Type() != "new" {
		return text
	}
	raw := typeNode.NamedChild(0)
	if raw == nil || raw.Type() == "type_arguments" || len(findDescendants(raw, "type_arguments")) > 0 {
		return text
	}
	return nodeText(raw, content)
}

func javaMethodCallArity(call *sitter.Node) *int {
	args := childByFieldName(call, "arguments")
	if args == nil || args.HasError() {
		return nil
	}
	count := 0
	for i := range int(args.NamedChildCount()) {
		child := args.NamedChild(i)
		switch child.Type() {
		case "line_comment", "block_comment":
			continue
		}
		if child.IsError() {
			return nil
		}
		count++
	}
	return &count
}

func javaCallName(call *sitter.Node, name string, content []byte) string {
	obj := childByFieldName(call, "object")
	if obj == nil {
		return name
	}
	switch obj.Type() {
	case "identifier", "field_access", "scoped_identifier", "this", "super":
		return nodeText(obj, content) + "." + name
	default:
		return ""
	}
}
