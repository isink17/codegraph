//go:build cgo

package treesitter

import (
	"context"
	"path/filepath"
	"regexp"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	kotlin "github.com/smacker/go-tree-sitter/kotlin"

	"github.com/isink17/codegraph/internal/graph"
)

// KotlinAdapter parses Kotlin source files using tree-sitter.
type KotlinAdapter struct{}

func NewKotlin() *KotlinAdapter { return &KotlinAdapter{} }

func (a *KotlinAdapter) Language() string     { return "kotlin" }
func (a *KotlinAdapter) Extensions() []string { return []string{".kt", ".kts"} }

func (a *KotlinAdapter) Supports(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".kt" || ext == ".kts"
}

func (a *KotlinAdapter) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	root, err := parse(ctx, kotlin.GetLanguage(), content)
	if err != nil {
		return graph.ParsedFile{}, err
	}

	module := packageEvidence(content, "kotlin")
	pf := graph.ParsedFile{
		Language:   "kotlin",
		Scope:      graph.ScopeEvidence{Package: packageEvidence(content, "kotlin")},
		FileTokens: computeFileTokens(content),
	}

	kotlinExtractImports(root, content, &pf)
	pf.Scope.JVMFacade = kotlinJVMFacade(root, path, content, pf.Scope.Imports)
	kotlinExtractSymbols(root, module, "", "module", content, &pf)
	kotlinExtractCalls(root, content, &pf)
	linkTestsGeneric(module, &pf, func(target string) string {
		return "func:kotlin:" + testTargetModule(module, "Test", "Tests") + ":" + target
	})
	return pf, nil
}

func kotlinExtractImports(root *sitter.Node, content []byte, pf *graph.ParsedFile) {
	for _, imp := range findDescendants(root, "import_header") {
		ident := childByFieldName(imp, "identifier")
		if ident != nil {
			pf.Imports = append(pf.Imports, nodeText(ident, content))
		}
		addKotlinScope(nodeText(imp, content), &pf.Scope.Imports)
	}
}

// kotlinJVMFacade derives the JVM class holding this file's top-level
// declarations from the file name and the structured @file: annotations. The
// preamble must parse cleanly, an annotation of ours must have the one form
// proven here, and every name must fall in the plain Java identifier subset;
// anything else yields no facade rather than a guessed one.
func kotlinJVMFacade(root *sitter.Node, path string, content []byte, imports []graph.ScopeImport) graph.JVMFileFacade {
	if filepath.Ext(path) != ".kt" {
		return graph.JVMFileFacade{} // scripts compile to a script class
	}
	for _, imp := range imports {
		for _, simple := range []string{"JvmName", "JvmMultifileClass"} {
			// An alias of ours, or another annotation under our name, makes the
			// annotation spellings below unreliable.
			if !imp.Wildcard && (imp.SourceSpecifier == "kotlin.jvm."+simple) != (imp.LocalName == simple) {
				return graph.JVMFileFacade{}
			}
		}
	}
	var facade graph.JVMFileFacade
	topLevel, preamble := false, true
	for i := range int(root.ChildCount()) {
		child := root.Child(i)
		switch child.Type() {
		case "function_declaration", "property_declaration":
			topLevel = true
		}
		if !preamble {
			continue
		}
		if child.IsError() || child.HasError() {
			return graph.JVMFileFacade{}
		}
		switch child.Type() {
		case "file_annotation":
			for j := range int(child.NamedChildCount()) {
				if !kotlinFileAnnotation(child.NamedChild(j), content, &facade) {
					return graph.JVMFileFacade{}
				}
			}
		case "package_header", "import_list", "import_header", "shebang_line", "line_comment", "multiline_comment":
		default:
			preamble = false // annotations after this point are not file-targeted
		}
	}
	if !topLevel {
		return graph.JVMFileFacade{}
	}
	if !facade.Explicit {
		stem := strings.TrimSuffix(filepath.Base(path), ".kt")
		if !kotlinPlainJavaIdentifier(stem) {
			return graph.JVMFileFacade{}
		}
		facade.Class = strings.ToUpper(stem[:1]) + stem[1:] + "Kt"
	}
	return facade
}

// kotlinFileAnnotation records one annotation inside `@file:`. It reports
// false for a JvmName/JvmMultifileClass spelling it cannot prove.
func kotlinFileAnnotation(node *sitter.Node, content []byte, facade *graph.JVMFileFacade) bool {
	switch node.Type() {
	case "user_type":
		switch kotlinJVMAnnotationName(nodeText(node, content)) {
		case "JvmMultifileClass":
			if facade.Multifile {
				return false
			}
			facade.Multifile = true
		case "JvmName":
			return false
		}
	case "constructor_invocation":
		typ := firstChild(node, "user_type")
		if typ == nil {
			return false
		}
		switch kotlinJVMAnnotationName(nodeText(typ, content)) {
		case "JvmName":
			name, ok := kotlinSingleStringArgument(node, content)
			if !ok || facade.Explicit || !kotlinPlainJavaIdentifier(name) {
				return false
			}
			facade.Class, facade.Explicit = name, true
		case "JvmMultifileClass":
			return false
		}
	}
	return true
}

func kotlinJVMAnnotationName(spelling string) string {
	return strings.TrimPrefix(spelling, "kotlin.jvm.")
}

// kotlinSingleStringArgument returns the content of `("Name")`: exactly one
// positional argument that is a plain string literal with no interpolation or
// escape.
func kotlinSingleStringArgument(node *sitter.Node, content []byte) (string, bool) {
	args := firstChild(node, "value_arguments")
	if args == nil || args.NamedChildCount() != 1 {
		return "", false
	}
	arg := args.NamedChild(0)
	if arg.Type() != "value_argument" || arg.NamedChildCount() != 1 {
		return "", false
	}
	lit := arg.NamedChild(0)
	if lit.Type() != "string_literal" || lit.NamedChildCount() != 1 || lit.NamedChild(0).Type() != "string_content" {
		return "", false
	}
	return nodeText(lit.NamedChild(0), content), true
}

// kotlinPlainJavaIdentifier is the conservative name subset whose JVM class
// spelling is unambiguous: ASCII letters, digits and '_', not starting with a
// digit. Kotlin sanitizes anything else by rules this adapter does not model.
func kotlinPlainJavaIdentifier(name string) bool {
	if name == "" || name[0] >= '0' && name[0] <= '9' {
		return false
	}
	for _, r := range name {
		if !(r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func kotlinExtractSymbols(node *sitter.Node, module, container, ownerKind string, content []byte, pf *graph.ParsedFile) {
	for i := range int(node.ChildCount()) {
		child := node.Child(i)
		switch child.Type() {
		case "class_declaration":
			kotlinAddType(child, module, container, "class", content, pf)
		case "object_declaration":
			kotlinAddType(child, module, container, "object", content, pf)
		case "interface_declaration":
			kotlinAddType(child, module, container, "interface", content, pf)
		case "function_declaration":
			if ownerKind == "type" || ownerKind == "module" {
				kotlinAddFunction(child, module, container, content, pf)
			}
		}
	}
}

func kotlinAddType(node *sitter.Node, module, parent, kind string, content []byte, pf *graph.ParsedFile) {
	nameNode := childByFieldName(node, "name")
	if nameNode == nil {
		nameNode = firstChild(node, "type_identifier")
	}
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, content)

	qualified := kotlinQualified(module, name)
	container := module
	if parent != "" {
		qualified = kotlinQualified(module, parent+"."+name)
		container = parent
	}
	pf.Symbols = append(pf.Symbols, graph.Symbol{
		Language:      "kotlin",
		Kind:          kind,
		Name:          name,
		QualifiedName: qualified,
		ContainerName: container,
		Visibility:    kotlinVisibility(node, content),
		Range:         nodeRange(node),
		DocSummary:    prevCommentText(node, content),
		StableKey:     "type:kotlin:" + qualified,
	})

	body := childByFieldName(node, "body")
	if body == nil {
		body = firstChild(node, "class_body")
	}
	if body != nil {
		nextContainer := name
		if parent != "" {
			nextContainer = parent + "." + name
		}
		kotlinExtractSymbols(body, module, nextContainer, "type", content, pf)
	}
}

func kotlinAddFunction(node *sitter.Node, module, container string, content []byte, pf *graph.ParsedFile) {
	nameNode := childByFieldName(node, "name")
	if nameNode == nil {
		nameNode = firstChild(node, "simple_identifier")
	}
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, content)
	effectiveContainer := module
	if container != "" && container != module {
		effectiveContainer = container
	}
	qualified := kotlinQualified(module, name)
	if container != "" && container != module {
		qualified = kotlinQualified(module, container+"."+name)
	}
	sig := kotlinDeclarationSignature(node, content)

	pf.Symbols = append(pf.Symbols, graph.Symbol{
		Language:      "kotlin",
		Kind:          "function",
		Name:          name,
		QualifiedName: qualified,
		ContainerName: effectiveContainer,
		Signature:     sig,
		Visibility:    kotlinVisibility(node, content),
		Range:         nodeRange(node),
		DocSummary:    prevCommentText(node, content),
		StableKey:     "func:kotlin:" + qualified,
	})
}

func kotlinQualified(pkg, name string) string {
	return strings.Trim(strings.TrimSpace(pkg)+"."+strings.TrimSpace(name), ".")
}

func kotlinVisibility(node *sitter.Node, content []byte) string {
	text := string(content[node.StartByte():node.EndByte()])
	if brace := strings.IndexByte(text, '{'); brace >= 0 {
		text = text[:brace]
	}
	if eq := strings.IndexByte(text, '='); eq >= 0 {
		text = text[:eq]
	}
	if regexp.MustCompile(`\bprivate\b`).MatchString(text) {
		return "private"
	}
	if regexp.MustCompile(`\bprotected\b`).MatchString(text) {
		return "protected"
	}
	if regexp.MustCompile(`\binternal\b`).MatchString(text) {
		return "internal"
	}
	return "public"
}

func kotlinDeclarationSignature(node *sitter.Node, content []byte) string {
	end := node.EndByte()
	if body := childByFieldName(node, "body"); body != nil {
		end = body.StartByte()
	}
	return strings.TrimSpace(string(content[node.StartByte():end]))
}

func kotlinExtractCalls(root *sitter.Node, content []byte, pf *graph.ParsedFile) {
	for _, call := range findDescendants(root, "call_expression") {
		fnNode := childByFieldName(call, "function")
		if fnNode == nil && call.ChildCount() > 0 {
			fnNode = call.Child(0)
		}
		if fnNode == nil {
			continue
		}
		name := nodeText(fnNode, content)
		if name == "" {
			continue
		}
		line := int(call.StartPoint().Row) + 1
		pf.Edges = append(pf.Edges, graph.Edge{
			SrcSymbolID: 0,
			DstName:     name,
			Kind:        "calls",
			Evidence:    name,
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
