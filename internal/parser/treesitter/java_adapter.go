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
}

func NewJava() *JavaAdapter { return &JavaAdapter{} }

// NewJavaV3 returns a parser that reports treesitter:java:v3 and omits the
// constructor arity facts v4 records. It exists only to reproduce v3
// databases in profile-transition tests.
func NewJavaV3() *JavaAdapter { return &JavaAdapter{legacyArity: true} }

// NewJavaV2 returns a parser that reports treesitter:java:v2 and takes the
// first `package x;` spelled anywhere in the file, comments and strings
// included, as its package. It exists only to reproduce v2 databases in
// profile-transition tests.
func NewJavaV2() *JavaAdapter { return &JavaAdapter{legacyPackage: true, legacyArity: true} }

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

	javaExtractImports(root, content, &pf)
	javaExtractSymbols(root, pf.Scope.Package, "", "module", content, &pf)
	javaExtractCalls(root, content, &pf)
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

func javaExtractImports(root *sitter.Node, content []byte, pf *graph.ParsedFile) {
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
		// Wildcard imports may expose only an identifier child; retain the full
		// declaration text so the scope evidence keeps the `.*` fact.
		addJavaScope(nodeText(imp, content), &pf.Scope.Imports)
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

func javaExtractCalls(root *sitter.Node, content []byte, pf *graph.ParsedFile) {
	for _, creation := range findDescendants(root, "object_creation_expression") {
		typeNode := childByFieldName(creation, "type")
		if typeNode == nil {
			continue
		}
		name := nodeText(typeNode, content)
		if name == "" {
			continue
		}
		pf.Edges = append(pf.Edges, graph.Edge{DstName: name, Kind: "constructs", Evidence: nodeText(creation, content), Line: int(creation.StartPoint().Row) + 1, CallArity: javaMethodCallArity(creation)})
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
		pf.Edges = append(pf.Edges, graph.Edge{
			SrcSymbolID: 0,
			DstName:     fullName,
			Kind:        "calls",
			Evidence:    nodeText(call, content),
			Line:        line,
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
			fixed++
		case "spread_parameter":
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
