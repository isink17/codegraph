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
	kotlinExtractSymbols(root, module, "", "module", content, pf.Scope.Imports, &pf)
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
//
// Every root child must also be preamble syntax or a top-level declaration
// node. The Kotlin grammar can silently parse legal source such as an
// annotated function as a top-level expression, dropping the declaration or
// its annotations without an ERROR node; a facade built from the surviving
// declarations could then bind a Java call the compiler rejects or finds
// ambiguous. Recovery nodes nested inside a declaration (a MISSING automatic
// semicolon in a one-line body) do not change the root shape and are kept.
//
// The refusal is deliberate and costs recall: a top-level annotation with
// arguments on its own line before `fun` (@Deprecated("x"), @Throws(...),
// @Suppress("..."), @OptIn(...)) is split by the grammar into a root
// expression plus an unannotated function, so the whole file loses its
// facade. A missed Java binding is preferred to a false one; recovering the
// split shape is separate parser work.
func kotlinJVMFacade(root *sitter.Node, path string, content []byte, imports []graph.ScopeImport) graph.JVMFileFacade {
	if filepath.Ext(path) != ".kt" {
		return graph.JVMFileFacade{} // scripts compile to a script class
	}
	var facade graph.JVMFileFacade
	topLevel, preamble := false, true
	for i := range int(root.ChildCount()) {
		child := root.Child(i)
		switch child.Type() {
		case "function_declaration", "property_declaration":
			topLevel = true
		}
		if preamble {
			switch child.Type() {
			case "file_annotation", "package_header", "import_list", "import_header", "shebang_line", "line_comment", "multiline_comment":
				if child.HasError() {
					return graph.JVMFileFacade{}
				}
				if child.Type() == "file_annotation" {
					for j := range int(child.NamedChildCount()) {
						if !kotlinFileAnnotation(child.NamedChild(j), content, imports, &facade) {
							return graph.JVMFileFacade{}
						}
					}
				}
				continue
			default:
				preamble = false // annotations after this point are not file-targeted
			}
		}
		if child.IsError() || !kotlinTopLevelDeclarationNode(child.Type()) {
			return graph.JVMFileFacade{}
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

// kotlinTopLevelDeclarationNode is the tree-sitter-kotlin root child set a
// .kt file may hold after its preamble: the grammar's declaration kinds
// (enum, interface, annotation, data and value classes are all
// class_declaration; accessors of a top-level property are separate getter and
// setter children), comments, and statement separators.
func kotlinTopLevelDeclarationNode(kind string) bool {
	switch kind {
	case "class_declaration", "object_declaration", "function_declaration", "property_declaration",
		"getter", "setter", "type_alias", "line_comment", "multiline_comment", ";":
		return true
	}
	return false
}

// kotlinFileAnnotation records one annotation inside `@file:`. It reports
// false for a JvmName/JvmMultifileClass spelling it cannot prove: an aliased
// use of either (deferred), or a simple name an import shadows or hides.
func kotlinFileAnnotation(node *sitter.Node, content []byte, imports []graph.ScopeImport, facade *graph.JVMFileFacade) bool {
	var typ *sitter.Node
	switch node.Type() {
	case "user_type":
		typ = node
	case "constructor_invocation":
		if typ = firstChild(node, "user_type"); typ == nil {
			return false
		}
	default:
		return true
	}
	spelling := nodeText(typ, content)
	var simple string
	for _, candidate := range []string{"JvmName", "JvmMultifileClass"} {
		ours, uncertain := kotlinJVMAnnotationSpelling(spelling, candidate, imports)
		if uncertain || ours && spelling != candidate && spelling != "kotlin.jvm."+candidate {
			return false
		}
		if ours {
			simple = candidate
		}
	}
	switch simple {
	case "JvmMultifileClass":
		if node.Type() != "user_type" || facade.Multifile {
			return false
		}
		facade.Multifile = true
	case "JvmName":
		if node.Type() != "constructor_invocation" {
			return false
		}
		name, ok := kotlinSingleStringArgument(node, content)
		if !ok || facade.Explicit || !kotlinPlainJavaIdentifier(name) {
			return false
		}
		facade.Class, facade.Explicit = name, true
	}
	return true
}

// kotlinJVMAnnotationSpelling classifies an annotation type spelling against
// kotlin.jvm.<simple>. ours means the spelling names that annotation: its
// qualified form, an explicit import (aliased or not), or the default-imported
// simple name. uncertain means the spelling is the simple name but an explicit
// import rebinds it to another declaration, or an aliased import of ours hides
// the simple name, so it cannot be proven either way. Same-package and
// star-import shadowing of the default import are not modelled, as for file
// facades.
func kotlinJVMAnnotationSpelling(spelling, simple string, imports []graph.ScopeImport) (ours, uncertain bool) {
	qualified := "kotlin.jvm." + simple
	if spelling == qualified {
		return true, false
	}
	for _, imp := range imports {
		if !imp.Wildcard && imp.LocalName == spelling {
			if imp.SourceSpecifier == qualified {
				return true, false
			}
			return false, spelling == simple
		}
	}
	if spelling != simple {
		return false, false
	}
	for _, imp := range imports {
		if !imp.Wildcard && imp.SourceSpecifier == qualified && imp.LocalName != simple {
			return false, true
		}
	}
	return true, false
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

func kotlinExtractSymbols(node *sitter.Node, module, container, ownerKind string, content []byte, imports []graph.ScopeImport, pf *graph.ParsedFile) {
	for i := range int(node.ChildCount()) {
		child := node.Child(i)
		switch child.Type() {
		case "class_declaration":
			kotlinAddType(child, module, container, "class", content, imports, pf)
		case "object_declaration":
			kotlinAddType(child, module, container, "object", content, imports, pf)
		case "interface_declaration":
			kotlinAddType(child, module, container, "interface", content, imports, pf)
		case "function_declaration":
			if ownerKind == "type" || ownerKind == "module" || ownerKind == "companion" {
				kotlinAddFunction(child, module, container, content, imports, pf)
			}
		case "companion_object":
			if ownerKind == "type" {
				kotlinAddCompanion(child, module, container, content, imports, pf)
			}
		}
	}
}

func kotlinAddCompanion(node *sitter.Node, module, outerContainer string, content []byte, imports []graph.ScopeImport, pf *graph.ParsedFile) {
	if outerContainer == "" {
		return
	}
	nameNode := childByFieldName(node, "name")
	if nameNode == nil {
		nameNode = firstChild(node, "type_identifier")
	}
	name := "Companion"
	if nameNode != nil {
		name = nodeText(nameNode, content)
	}
	container := outerContainer + "." + name
	qualified := kotlinQualified(module, container)
	pf.Symbols = append(pf.Symbols, graph.Symbol{
		Language:      "kotlin",
		Kind:          "companion_object",
		Name:          name,
		QualifiedName: qualified,
		ContainerName: outerContainer,
		Visibility:    kotlinVisibility(node, content),
		Range:         nodeRange(node),
		DocSummary:    prevCommentText(node, content),
		StableKey:     "companion:kotlin:" + qualified,
	})

	if body := firstChild(node, "class_body"); body != nil {
		kotlinExtractSymbols(body, module, container, "companion", content, imports, pf)
	}
}

func kotlinAddType(node *sitter.Node, module, parent, kind string, content []byte, imports []graph.ScopeImport, pf *graph.ParsedFile) {
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
		kotlinExtractSymbols(body, module, nextContainer, "type", content, imports, pf)
	}
}

func kotlinAddFunction(node *sitter.Node, module, container string, content []byte, imports []graph.ScopeImport, pf *graph.ParsedFile) {
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
	var arityMin, arityMax *int
	jvmName, renamed, knownRename := kotlinDeclarationJVMName(node, content, imports)
	unknownRename := renamed && !knownRename
	if arity := kotlinFixedJavaCallableArity(node, content, imports, unknownRename); arity != nil {
		arityMin, arityMax = arity, arity
	}

	pf.Symbols = append(pf.Symbols, graph.Symbol{
		Language:      "kotlin",
		Kind:          "function",
		Name:          name,
		QualifiedName: qualified,
		ContainerName: effectiveContainer,
		Signature:     sig,
		Visibility:    kotlinVisibility(node, content),
		ArityMin:      arityMin,
		ArityMax:      arityMax,
		Range:         nodeRange(node),
		DocSummary:    prevCommentText(node, content),
		StableKey:     "func:kotlin:" + qualified,
	})
	if fact, ok := kotlinJVMCallableEvidence(node, content, imports, len(pf.Symbols)-1, unknownRename); ok {
		pf.KotlinJVMCallableEvidence = append(pf.KotlinJVMCallableEvidence, fact)
	}
	if renamed {
		pf.KotlinJVMNameEvidence = append(pf.KotlinJVMNameEvidence, graph.KotlinJVMNameEvidence{SymbolIndex: len(pf.Symbols) - 1, Known: knownRename, JVMName: jvmName})
	}
}

// kotlinDeclarationJVMName reads a function's own @JvmName from its structured
// modifier annotations. present reports any annotation that is, or may be,
// kotlin.jvm.JvmName; known additionally proves the exact JVM method name: one
// such annotation with one positional plain string literal in the ASCII
// identifier subset. Named, constant, concatenated, raw, escaped or templated
// arguments, repeated annotations and shadowed spellings stay unknown -- the
// parser evaluates no Kotlin expressions.
func kotlinDeclarationJVMName(node *sitter.Node, content []byte, imports []graph.ScopeImport) (name string, present, known bool) {
	mods := firstChild(node, "modifiers")
	if mods == nil {
		return "", false, false
	}
	count := 0
	for i := range int(mods.NamedChildCount()) {
		annotation := mods.NamedChild(i)
		if annotation.Type() != "annotation" {
			continue
		}
		for j := range int(annotation.NamedChildCount()) {
			entry := annotation.NamedChild(j)
			typ := entry
			if entry.Type() == "constructor_invocation" {
				typ = firstChild(entry, "user_type")
			} else if entry.Type() != "user_type" {
				continue
			}
			if typ == nil {
				continue
			}
			ours, uncertain := kotlinJVMAnnotationSpelling(nodeText(typ, content), "JvmName", imports)
			if !ours && !uncertain {
				continue
			}
			count++
			known = false
			if args := firstChild(entry, "value_arguments"); ours && args != nil && !strings.Contains(nodeText(args, content), `"""`) {
				if literal, ok := kotlinSingleStringArgument(entry, content); ok && kotlinPlainJavaIdentifier(literal) {
					name, known = literal, true
				}
			}
		}
	}
	if count == 0 {
		return "", false, false
	}
	if count > 1 || !known {
		return "", true, false
	}
	return name, true, true
}

// kotlinJVMCallableEvidence records Java arities only for defaulted functions
// or functions explicitly annotated with JvmOverloads. Parameter boundaries
// and defaults come from tree-sitter nodes; source text is used only to classify
// annotation spellings in the modifier region.
// unknownRename reports a declaration @JvmName whose JVM name is unprovable.
func kotlinJVMCallableEvidence(node *sitter.Node, content []byte, imports []graph.ScopeImport, symbolIndex int, unknownRename bool) (graph.KotlinJVMCallableEvidence, bool) {
	unknown := graph.KotlinJVMCallableEvidence{SymbolIndex: symbolIndex}
	if node == nil || node.HasError() {
		return unknown, false
	}
	fun := childByFieldName(node, "name")
	if fun == nil {
		fun = firstChild(node, "simple_identifier")
	}
	params := firstChild(node, "function_value_parameters")
	if fun == nil || params == nil || params.HasError() {
		return unknown, false
	}
	prefix := string(content[node.StartByte():fun.StartByte()])
	defaultCount, count, hasVararg, hasIntroducedParam := 0, 0, false, false
	paramTypes := make([]*sitter.Node, 0, params.NamedChildCount())
	for i := range int(params.ChildCount()) {
		param := params.Child(i)
		if param.Type() != "parameter" {
			continue // tree-sitter represents a default expression as a sibling
		}
		if param.HasError() {
			return unknown, true
		}
		count++
		var typ *sitter.Node
		nextParam := params.EndByte()
		for j := i + 1; j < int(params.ChildCount()); j++ {
			if params.Child(j).Type() == "parameter" {
				nextParam = params.Child(j).StartByte()
				break
			}
		}
		hasDefault := false
		for j := i + 1; j < int(params.ChildCount()) && params.Child(j).StartByte() < nextParam; j++ {
			hasDefault = hasDefault || params.Child(j).Type() == "="
		}
		for j := range int(param.ChildCount()) {
			child := param.Child(j)
			switch child.Type() {
			case "parameter_modifiers":
				if strings.Contains(nodeText(child, content), "vararg") {
					hasVararg = true
				}
			case ":":
				if j+1 < int(param.ChildCount()) {
					typ = param.Child(j + 1)
				}
			}
		}
		if hasDefault && kotlinHasAnnotation(nodeText(param, content), "IntroducedAt", "kotlin.IntroducedAt", imports) {
			hasIntroducedParam = true
		}
		if hasDefault {
			defaultCount++
		}
		paramTypes = append(paramTypes, typ)
	}
	parameterSource := nodeText(params, content)
	hasVararg = hasVararg || kotlinHasToken(parameterSource, "vararg")
	hasIntroducedParam = hasIntroducedParam || kotlinHasAnnotation(parameterSource, "IntroducedAt", "kotlin.IntroducedAt", imports)
	hasOverloads := kotlinHasAnnotation(prefix, "JvmOverloads", "kotlin.jvm.JvmOverloads", imports)
	hasIntroduced := kotlinHasAnnotation(prefix, "IntroducedAt", "kotlin.IntroducedAt", imports)
	if defaultCount == 0 && !hasOverloads && !hasIntroduced && !hasIntroducedParam {
		return unknown, false
	}
	unknown.Known = false
	if hasIntroduced || hasIntroducedParam || hasVararg || kotlinHasToken(prefix, "suspend") || kotlinHasToken(prefix, "internal") || kotlinHasToken(prefix, "external") || kotlinHasToken(prefix, "expect") || kotlinHasAnnotation(prefix, "JvmExposeBoxed", "kotlin.jvm.JvmExposeBoxed", imports) || unknownRename || kotlinHasUnknownAliasedJVMAnnotation(prefix+parameterSource, imports) {
		return unknown, true
	}
	name := childByFieldName(node, "name")
	if name == nil {
		name = firstChild(node, "simple_identifier")
	}
	if name == nil {
		return unknown, true
	}
	var funToken *sitter.Node
	for i := range int(node.ChildCount()) {
		child := node.Child(i)
		if child.Type() == "fun" {
			funToken = child
			continue
		}
		if child.Type() == "type_parameters" {
			return unknown, true
		}
		if funToken != nil && child.StartByte() >= funToken.EndByte() && child.EndByte() <= name.StartByte() && child.Type() != "type_parameters" {
			return unknown, true // extension receiver
		}
	}
	if funToken == nil || name == nil {
		return unknown, true
	}
	for _, typ := range paramTypes {
		if !kotlinPlainJavaType(typ, content) {
			return unknown, true
		}
	}
	if result, explicit := kotlinFunctionReturnType(node); explicit && !kotlinPlainJavaType(result, content) {
		return unknown, true
	}
	if _, explicit := kotlinFunctionReturnType(node); !explicit {
		body := firstChild(node, "function_body")
		if body == nil || strings.HasPrefix(strings.TrimSpace(nodeText(body, content)), "=") {
			return unknown, true
		}
	}
	if !hasOverloads {
		defaultCount = 0
	}
	unknown.Known = true
	unknown.ArityMin, unknown.ArityMax = count-defaultCount, count
	return unknown, true
}

func kotlinHasAnnotation(prefix, simple, qualified string, imports []graph.ScopeImport) bool {
	for _, spelling := range []string{"@" + simple, "@" + qualified} {
		if kotlinAnnotationPresent(prefix, spelling) {
			return true
		}
	}
	for _, imp := range imports {
		if imp.SourceSpecifier == qualified && !imp.Wildcard && imp.LocalName != "" && kotlinAnnotationPresent(prefix, "@"+imp.LocalName) {
			return true
		}
	}
	return false
}

func kotlinAnnotationPresent(source, spelling string) bool {
	for start := 0; ; {
		rel := strings.Index(source[start:], spelling)
		if rel < 0 {
			return false
		}
		end := start + rel + len(spelling)
		if end == len(source) || source[end] == '(' || source[end] == '[' || source[end] == ':' || source[end] == ' ' || source[end] == '\t' || source[end] == '\r' || source[end] == '\n' {
			return true
		}
		start = end
	}
}

func kotlinHasUnknownAliasedJVMAnnotation(source string, imports []graph.ScopeImport) bool {
	for _, imp := range imports {
		if strings.HasPrefix(imp.SourceSpecifier, "kotlin.jvm.") && !imp.Wildcard && imp.LocalName != "" && imp.LocalName != imp.ImportedName && kotlinAnnotationPresent(source, "@"+imp.LocalName) {
			// JvmName is classified structurally by kotlinDeclarationJVMName.
			if imp.SourceSpecifier == "kotlin.jvm.JvmOverloads" || imp.SourceSpecifier == "kotlin.jvm.JvmSynthetic" || imp.SourceSpecifier == "kotlin.jvm.JvmStatic" || imp.SourceSpecifier == "kotlin.jvm.JvmName" {
				continue
			}
			return true
		}
	}
	return false
}

// kotlinFixedJavaCallableArity persists exact arity only for declarations
// whose source parameter count is also a plain Java JVM call shape. Fully
// qualified built-in types avoid package declarations and aliases shadowing
// short names such as Int. Unknown types remain unknown; no type resolution is
// attempted here.
func kotlinFixedJavaCallableArity(node *sitter.Node, content []byte, imports []graph.ScopeImport, unknownRename bool) *int {
	if node == nil || node.HasError() {
		return nil
	}
	var fun, name *sitter.Node
	for i := range int(node.ChildCount()) {
		child := node.Child(i)
		if child.Type() == "fun" {
			fun = child
		}
	}
	name = childByFieldName(node, "name")
	if name == nil {
		name = firstChild(node, "simple_identifier")
	}
	if fun == nil || name == nil {
		return nil
	}
	prefix := string(content[node.StartByte():fun.StartByte()])
	// A proven declaration rename changes the JVM method name, not its
	// parameters; an unprovable one keeps the callable unknown.
	if unknownRename {
		return nil
	}
	for _, forbidden := range []string{"JvmSynthetic", "JvmOverloads", "JvmExposeBoxed"} {
		if strings.Contains(prefix, forbidden) || kotlinUsesAliasedJVMAnnotation(prefix, imports, forbidden) {
			return nil
		}
	}
	if kotlinUsesAliasedJVMAnnotation(prefix, imports, "JvmStatic") {
		return nil
	}
	for _, modifier := range []string{"suspend", "internal", "external", "expect"} {
		if kotlinHasToken(prefix, modifier) {
			return nil
		}
	}

	// Any receiver node between `fun` and the declared name is an extension
	// receiver. A generic type-parameter list is the only permitted node there.
	for i := range int(node.ChildCount()) {
		child := node.Child(i)
		if child.StartByte() < fun.EndByte() || child.EndByte() > name.StartByte() || child == fun {
			continue
		}
		if child.Type() != "type_parameters" {
			return nil
		}
		if strings.Contains(nodeText(child, content), "reified") {
			return nil
		}
	}

	params := firstChild(node, "function_value_parameters")
	if params == nil || params.HasError() {
		return nil
	}
	count := 0
	for i := range int(params.NamedChildCount()) {
		param := params.NamedChild(i)
		if param.Type() != "parameter" || param.HasError() {
			return nil
		}
		var typ *sitter.Node
		for j := range int(param.ChildCount()) {
			child := param.Child(j)
			if child.Type() == "=" || child.Type() == "parameter_modifiers" {
				return nil
			}
			if child.Type() == ":" && j+1 < int(param.ChildCount()) {
				typ = param.Child(j + 1)
			}
		}
		if !kotlinPlainJavaType(typ, content) {
			return nil
		}
		count++
	}

	if result, explicit := kotlinFunctionReturnType(node); explicit {
		if !kotlinPlainJavaType(result, content) {
			return nil
		}
	} else {
		body := firstChild(node, "function_body")
		if body == nil || strings.HasPrefix(strings.TrimSpace(nodeText(body, content)), "=") {
			return nil
		}
	}
	return &count
}

func kotlinFunctionReturnType(node *sitter.Node) (*sitter.Node, bool) {
	for i := range int(node.ChildCount()) {
		if node.Child(i).Type() == ":" && i+1 < int(node.ChildCount()) {
			return node.Child(i + 1), true
		}
	}
	return nil, false
}

func kotlinPlainJavaType(node *sitter.Node, content []byte) bool {
	if node == nil || node.HasError() || node.Type() != "user_type" && node.Type() != "nullable_type" {
		return false
	}
	typ := strings.Join(strings.Fields(nodeText(node, content)), "")
	switch typ {
	case "kotlin.Boolean", "kotlin.Boolean?", "kotlin.Byte", "kotlin.Byte?", "kotlin.Char", "kotlin.Char?",
		"kotlin.Double", "kotlin.Double?", "kotlin.Float", "kotlin.Float?", "kotlin.Int", "kotlin.Int?",
		"kotlin.Long", "kotlin.Long?", "kotlin.Short", "kotlin.Short?", "kotlin.String", "kotlin.String?",
		"kotlin.Unit", "kotlin.Unit?", "kotlin.Any", "kotlin.Any?", "java.lang.String", "java.lang.String?":
		return true
	default:
		return false
	}
}

func kotlinHasToken(source, token string) bool {
	for _, part := range strings.Fields(source) {
		part = strings.Trim(part, "@()[],:;")
		if part == token {
			return true
		}
	}
	return false
}

func kotlinUsesAliasedJVMAnnotation(prefix string, imports []graph.ScopeImport, name string) bool {
	for _, imp := range imports {
		if imp.SourceSpecifier == "kotlin.jvm."+name && imp.LocalName != name && strings.Contains(prefix, "@"+imp.LocalName) {
			return true
		}
	}
	return false
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
