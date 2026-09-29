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
type KotlinAdapter struct {
	// v6 disables detached-annotation recovery: the treesitter:kotlin:v6
	// parser, kept so profile-transition tests can write the databases it did.
	v6 bool
}

func NewKotlin() *KotlinAdapter { return &KotlinAdapter{} }

// NewKotlinV6 returns the previous treesitter:kotlin:v6 parser, which refuses
// a .kt facade on any detached root annotation instead of recovering it. It
// exists only to reproduce v6 databases in profile-transition tests.
func NewKotlinV6() *KotlinAdapter { return &KotlinAdapter{v6: true} }

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
	// Scripts may legally hold top-level expressions, so the split shape is
	// only proven, and only recovered, in .kt files.
	views, clean := kotlinRootDeclarations(root, content, !a.v6 && filepath.Ext(path) == ".kt")
	if clean {
		pf.Scope.JVMFacade = kotlinJVMFacade(root, path, content, pf.Scope.Imports)
	}
	kotlinExtractSymbols(root, module, "", "module", content, pf.Scope.Imports, views, &pf)
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
// anything else yields no facade rather than a guessed one. It is only called
// for a root kotlinRootDeclarations reports clean.
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
		if !preamble || !kotlinPreambleNode(child.Type()) {
			preamble = false // annotations after this point are not file-targeted
			continue
		}
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

// kotlinPreambleNode is the root child set that may precede the first
// top-level declaration: file annotations, package and imports.
func kotlinPreambleNode(kind string) bool {
	switch kind {
	case "file_annotation", "package_header", "import_list", "import_header", "shebang_line", "line_comment", "multiline_comment":
		return true
	}
	return false
}

// kotlinAnnotationEntry is one annotation application: its type and its
// argument list -- value_arguments, or for a recovered split annotation the
// parenthesized_expression standing in for exactly one positional argument.
type kotlinAnnotationEntry struct{ typ, args *sitter.Node }

// kotlinDeclarationView is a declaration as the compiler sees it. start is
// where its source begins (the first recovered detached annotation, else the
// declaration node), first anchors its documentation, and annotations are the
// recovered entries in source order that the declaration's own modifiers lack.
type kotlinDeclarationView struct {
	decl        *sitter.Node
	start       uint32
	first       *sitter.Node
	annotations []kotlinAnnotationEntry
}

func kotlinPlainView(node *sitter.Node) kotlinDeclarationView {
	return kotlinDeclarationView{decl: node, start: node.StartByte(), first: node}
}

// kotlinRootDeclarations scans the root once. clean reports that every root
// child is preamble, top-level declaration syntax, or -- when recovery is set
// -- a proven detached annotation run; views maps each declaration start byte
// to the annotations recovered for it.
//
// The Kotlin grammar can silently parse legal source such as an annotated
// function as a top-level expression, dropping the declaration or its
// annotations without an ERROR node; a facade built from the surviving
// declarations could then bind a Java call the compiler rejects or finds
// ambiguous. Recovery nodes nested inside a declaration (a MISSING automatic
// semicolon in a one-line body) do not change the root shape and are kept.
//
// Only one split is recovered, and only from structure: an own-line
// annotation with one argument before a later declaration becomes
// prefix_expression(annotation("@" user_type), parenthesized_expression)
// followed by a modifier-less function or object. A run is R1..Rn D where each
// Ri is a root annotation or, last and at most once, such a prefix_expression
// chain; only comments and whitespace separate them (checked on the bytes, as
// the grammar can swallow a `;`); D is the next function_declaration or
// object_declaration and starts with its `fun`/`object` keyword; nothing in
// the run has a parse error. Every other root expression -- including a
// declaration the grammar swallowed whole, which cannot be rebuilt from its
// tree -- leaves the root unclean, and an unclean root recovers nothing.
func kotlinRootDeclarations(root *sitter.Node, content []byte, recovery bool) (map[uint32]kotlinDeclarationView, bool) {
	var views map[uint32]kotlinDeclarationView
	clean, preamble := true, true
	var run *kotlinDeclarationView
	var runEnd uint32
	chained := false
	fail := func() { clean, run, chained = false, nil, false }
	extend := func(node *sitter.Node, entries []kotlinAnnotationEntry) {
		if run == nil {
			run = &kotlinDeclarationView{start: node.StartByte(), first: node}
		}
		run.annotations = append(run.annotations, entries...)
		runEnd = node.EndByte()
	}
	for i := range int(root.ChildCount()) {
		child := root.Child(i)
		kind := child.Type()
		if preamble && kotlinPreambleNode(kind) {
			continue
		}
		preamble = false
		if run != nil && !kotlinBlank(content[runEnd:child.StartByte()]) {
			fail()
		}
		switch kind {
		case "line_comment", "multiline_comment":
			if run != nil {
				runEnd = child.EndByte()
			}
			continue
		case "annotation", "prefix_expression":
			if !recovery || chained {
				fail()
				continue
			}
			var entries []kotlinAnnotationEntry
			ok := false
			if kind == "annotation" {
				entries, ok = kotlinPlainAnnotation(child)
			} else {
				entries, ok = kotlinDetachedAnnotationChain(child, content)
				chained = ok
			}
			if !ok {
				fail()
				continue
			}
			extend(child, entries)
			continue
		case "function_declaration", "object_declaration":
			if run == nil {
				continue
			}
			keyword := "fun"
			if kind == "object_declaration" {
				keyword = "object"
			}
			if !chained || child.HasError() || child.ChildCount() == 0 || child.Child(0).Type() != keyword {
				fail()
				continue
			}
			run.decl = child
			if views == nil {
				views = map[uint32]kotlinDeclarationView{}
			}
			views[child.StartByte()] = *run
			run, chained = nil, false
			continue
		}
		if run != nil {
			fail()
		}
		if child.IsError() || !kotlinTopLevelDeclarationNode(kind) {
			clean = false
		}
	}
	if run != nil {
		clean = false // a dangling annotation annotates nothing
	}
	if !clean {
		return nil, false // an unexplained root keeps every declaration unrecovered
	}
	return views, true
}

func kotlinBlank(gap []byte) bool {
	for _, c := range gap {
		if c != ' ' && c != '\t' && c != '\r' && c != '\n' && c != '\f' {
			return false
		}
	}
	return true
}

// kotlinAnnotationEntries lists the applications in one annotation node:
// `@T`, `@T(args)` or each entry of `@[...]`.
func kotlinAnnotationEntries(annotation *sitter.Node) []kotlinAnnotationEntry {
	var entries []kotlinAnnotationEntry
	for i := range int(annotation.NamedChildCount()) {
		switch entry := annotation.NamedChild(i); entry.Type() {
		case "user_type":
			entries = append(entries, kotlinAnnotationEntry{typ: entry})
		case "constructor_invocation":
			if typ := firstChild(entry, "user_type"); typ != nil {
				entries = append(entries, kotlinAnnotationEntry{typ: typ, args: firstChild(entry, "value_arguments")})
			}
		}
	}
	return entries
}

// kotlinPlainAnnotation accepts a detached annotation node made only of `@`,
// annotation types, constructor invocations and brackets, with `@` touching
// what follows it as kotlinc requires. A use-site target is refused: stripping
// it would change what the annotation applies to.
func kotlinPlainAnnotation(node *sitter.Node) ([]kotlinAnnotationEntry, bool) {
	if node.Type() != "annotation" || node.HasError() || node.ChildCount() < 2 || node.Child(0).Type() != "@" || node.Child(0).EndByte() != node.Child(1).StartByte() {
		return nil, false
	}
	for i := 1; i < int(node.ChildCount()); i++ {
		switch node.Child(i).Type() {
		case "user_type", "constructor_invocation", "[", "]":
		default:
			return nil, false
		}
	}
	entries := kotlinAnnotationEntries(node)
	return entries, len(entries) > 0
}

// kotlinDetachedAnnotationChain reads prefix_expression(annotation, operand)
// where the operand is another such chain or, at the end, the parenthesized
// argument the grammar split from `@T(arg)`: the annotation is exactly `@` and
// a type, the `(` follows the type with no byte between, and the parentheses
// hold one expression.
func kotlinDetachedAnnotationChain(node *sitter.Node, content []byte) ([]kotlinAnnotationEntry, bool) {
	if node.Type() != "prefix_expression" || node.HasError() || node.ChildCount() != 2 {
		return nil, false
	}
	annotation, operand := node.Child(0), node.Child(1)
	if !kotlinBlank(content[annotation.EndByte():operand.StartByte()]) {
		return nil, false
	}
	entries, ok := kotlinPlainAnnotation(annotation)
	if !ok {
		return nil, false
	}
	switch operand.Type() {
	case "prefix_expression":
		rest, ok := kotlinDetachedAnnotationChain(operand, content)
		return append(entries, rest...), ok
	case "parenthesized_expression":
		if annotation.ChildCount() != 2 || annotation.Child(1).Type() != "user_type" || annotation.EndByte() != operand.StartByte() {
			return nil, false
		}
		if operand.ChildCount() != 3 || operand.Child(0).Type() != "(" || !operand.Child(1).IsNamed() || operand.Child(2).Type() != ")" {
			return nil, false
		}
		return []kotlinAnnotationEntry{{typ: annotation.Child(1), args: operand}}, true
	}
	return nil, false
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
		name, ok := kotlinSingleStringArgument(firstChild(node, "value_arguments"), content)
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
// escape. args is a value_arguments node, or the parenthesized_expression a
// recovered split annotation holds its one argument in.
func kotlinSingleStringArgument(args *sitter.Node, content []byte) (string, bool) {
	var lit *sitter.Node
	switch {
	case args == nil:
		return "", false
	case args.Type() == "parenthesized_expression":
		if args.ChildCount() != 3 {
			return "", false
		}
		lit = args.Child(1)
	default:
		if args.NamedChildCount() != 1 {
			return "", false
		}
		arg := args.NamedChild(0)
		if arg.Type() != "value_argument" || arg.NamedChildCount() != 1 {
			return "", false
		}
		lit = arg.NamedChild(0)
	}
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

// kotlinExtractSymbols walks one declaration container. views holds the root's
// recovered detached annotations and is nil for every nested body.
func kotlinExtractSymbols(node *sitter.Node, module, container, ownerKind string, content []byte, imports []graph.ScopeImport, views map[uint32]kotlinDeclarationView, pf *graph.ParsedFile) {
	for i := range int(node.ChildCount()) {
		child := node.Child(i)
		view, ok := views[child.StartByte()]
		if !ok || view.decl != child {
			view = kotlinPlainView(child)
		}
		switch child.Type() {
		case "class_declaration":
			kotlinAddType(view, module, container, "class", content, imports, pf)
		case "object_declaration":
			kotlinAddType(view, module, container, "object", content, imports, pf)
		case "interface_declaration":
			kotlinAddType(view, module, container, "interface", content, imports, pf)
		case "function_declaration":
			if ownerKind == "type" || ownerKind == "module" || ownerKind == "companion" {
				kotlinAddFunction(view, module, container, content, imports, pf)
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
		Visibility:    kotlinVisibility(kotlinPlainView(node), content),
		Range:         nodeRange(node),
		DocSummary:    prevCommentText(node, content),
		StableKey:     "companion:kotlin:" + qualified,
	})

	if body := firstChild(node, "class_body"); body != nil {
		kotlinExtractSymbols(body, module, container, "companion", content, imports, nil, pf)
	}
}

func kotlinAddType(view kotlinDeclarationView, module, parent, kind string, content []byte, imports []graph.ScopeImport, pf *graph.ParsedFile) {
	node := view.decl
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
		Visibility:    kotlinVisibility(view, content),
		Range:         nodeRange(node),
		DocSummary:    prevCommentText(view.first, content),
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
		kotlinExtractSymbols(body, module, nextContainer, "type", content, imports, nil, pf)
	}
}

func kotlinAddFunction(view kotlinDeclarationView, module, container string, content []byte, imports []graph.ScopeImport, pf *graph.ParsedFile) {
	node := view.decl
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
	sig := kotlinDeclarationSignature(view, content)
	var arityMin, arityMax *int
	jvmName, renamed, knownRename := kotlinDeclarationJVMName(view, content, imports)
	unknownRename := renamed && !knownRename
	if arity := kotlinFixedJavaCallableArity(view, content, imports, unknownRename); arity != nil {
		arityMin, arityMax = arity, arity
	}

	pf.Symbols = append(pf.Symbols, graph.Symbol{
		Language:      "kotlin",
		Kind:          "function",
		Name:          name,
		QualifiedName: qualified,
		ContainerName: effectiveContainer,
		Signature:     sig,
		Visibility:    kotlinVisibility(view, content),
		ArityMin:      arityMin,
		ArityMax:      arityMax,
		Range:         nodeRange(node),
		DocSummary:    prevCommentText(view.first, content),
		StableKey:     "func:kotlin:" + qualified,
	})
	if fact, ok := kotlinJVMCallableEvidence(view, content, imports, len(pf.Symbols)-1, unknownRename); ok {
		pf.KotlinJVMCallableEvidence = append(pf.KotlinJVMCallableEvidence, fact)
	}
	if renamed {
		pf.KotlinJVMNameEvidence = append(pf.KotlinJVMNameEvidence, graph.KotlinJVMNameEvidence{SymbolIndex: len(pf.Symbols) - 1, Known: knownRename, JVMName: jvmName})
	}
}

// kotlinDeclarationJVMName reads a function's own @JvmName from its structured
// modifier annotations and the detached ones recovered for it. present reports any annotation that is, or may be,
// kotlin.jvm.JvmName; known additionally proves the exact JVM method name: one
// such annotation with one positional plain string literal in the ASCII
// identifier subset. Named, constant, concatenated, raw, escaped or templated
// arguments, repeated annotations and shadowed spellings stay unknown -- the
// parser evaluates no Kotlin expressions.
func kotlinDeclarationJVMName(view kotlinDeclarationView, content []byte, imports []graph.ScopeImport) (name string, present, known bool) {
	entries := view.annotations
	if mods := firstChild(view.decl, "modifiers"); mods != nil {
		for i := range int(mods.NamedChildCount()) {
			if annotation := mods.NamedChild(i); annotation.Type() == "annotation" {
				entries = append(entries[:len(entries):len(entries)], kotlinAnnotationEntries(annotation)...)
			}
		}
	}
	count := 0
	for _, entry := range entries {
		ours, uncertain := kotlinJVMAnnotationSpelling(nodeText(entry.typ, content), "JvmName", imports)
		if !ours && !uncertain {
			continue
		}
		count++
		known = false
		if ours && entry.args != nil && !strings.Contains(nodeText(entry.args, content), `"""`) {
			if literal, ok := kotlinSingleStringArgument(entry.args, content); ok && kotlinPlainJavaIdentifier(literal) {
				name, known = literal, true
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
func kotlinJVMCallableEvidence(view kotlinDeclarationView, content []byte, imports []graph.ScopeImport, symbolIndex int, unknownRename bool) (graph.KotlinJVMCallableEvidence, bool) {
	node := view.decl
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
	prefix := string(content[view.start:fun.StartByte()])
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
func kotlinFixedJavaCallableArity(view kotlinDeclarationView, content []byte, imports []graph.ScopeImport, unknownRename bool) *int {
	node := view.decl
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
	prefix := string(content[view.start:fun.StartByte()])
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

func kotlinVisibility(view kotlinDeclarationView, content []byte) string {
	text := string(content[view.start:view.decl.EndByte()])
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

// kotlinDeclarationSignature starts at the view, so a recovered declaration
// persists its detached annotations exactly as an attached one would.
func kotlinDeclarationSignature(view kotlinDeclarationView, content []byte) string {
	end := view.decl.EndByte()
	if body := childByFieldName(view.decl, "body"); body != nil {
		end = body.StartByte()
	}
	return strings.TrimSpace(string(content[view.start:end]))
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
