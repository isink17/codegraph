//go:build cgo

package treesitter

import (
	"context"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	swift "github.com/smacker/go-tree-sitter/swift"

	"github.com/isink17/codegraph/internal/graph"
)

// SwiftAdapter parses Swift source files using tree-sitter.
type SwiftAdapter struct{}

func NewSwift() *SwiftAdapter { return &SwiftAdapter{} }

func (a *SwiftAdapter) Language() string     { return "swift" }
func (a *SwiftAdapter) Extensions() []string { return []string{".swift"} }

func (a *SwiftAdapter) Supports(path string) bool {
	return strings.EqualFold(filepath.Ext(path), ".swift")
}

func (a *SwiftAdapter) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	root, err := parse(ctx, swift.GetLanguage(), content)
	if err != nil {
		return graph.ParsedFile{}, err
	}
	p := graph.ParsedFile{Language: "swift", FileTokens: computeFileTokens(content)}
	swiftExtractImports(root, content, &p)
	swiftExtractSymbols(root, "", "internal", content, &p)
	swiftExtractInheritanceRelations(root, content, &p)
	swiftExtractLexicalBindings(root, content, &p)
	swiftExtractCalls(root, content, &p)
	return p, nil
}

func swiftExtractImports(root *sitter.Node, content []byte, pf *graph.ParsedFile) {
	for _, imp := range findDescendants(root, "import_declaration") {
		name := childByFieldName(imp, "name")
		if name == nil {
			name = firstChild(imp, "identifier")
		}
		if name == nil {
			continue
		}
		source := nodeText(name, content)
		if source == "" {
			continue
		}
		pf.Imports = append(pf.Imports, source)
		pf.Scope.Imports = append(pf.Scope.Imports, swiftImportEvidence(imp, source, content))
	}
}

func swiftImportEvidence(node *sitter.Node, raw string, content []byte) graph.ScopeImport {
	full := strings.TrimSpace(nodeText(node, content))
	text := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(full, "@_exported"), "@testable"))
	text = strings.TrimSpace(strings.TrimPrefix(text, "import"))
	parts := strings.Fields(text)
	kind := graph.ScopeImportNamed
	if len(parts) > 1 {
		switch parts[0] {
		case "struct", "class", "func", "enum", "typealias", "var", "let":
			kind, parts = parts[0], parts[1:]
		}
	}
	module, imported := raw, ""
	if len(parts) > 0 {
		path := parts[0]
		if dot := strings.IndexByte(path, '.'); dot >= 0 {
			module, imported = path[:dot], path[dot+1:]
		} else {
			module = path
		}
	}
	return graph.ScopeImport{SourceSpecifier: module, ImportedName: imported, LocalName: imported, Kind: kind, ReExport: strings.HasPrefix(full, "@_exported")}
}

func swiftExtractSymbols(node *sitter.Node, container, visibility string, content []byte, pf *graph.ParsedFile) {
	if node == nil {
		return
	}
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		switch child.Type() {
		case "class_declaration":
			if swiftIsExtension(child, content) {
				if target := swiftExtensionTarget(child, content); target != "" {
					swiftExtractSymbols(childByFieldName(child, "body"), target, swiftVisibility(child, visibility, content), content, pf)
				}
				continue
			}
			kind := swiftNominalKind(child, content)
			if kind != "" {
				swiftAddType(child, container, kind, visibility, content, pf)
			}
		case "protocol_declaration":
			swiftAddType(child, container, "protocol", visibility, content, pf)
		case "function_declaration":
			swiftAddCallable(child, "function", container, visibility, content, pf)
		case "init_declaration":
			swiftAddCallable(child, "function", container, visibility, content, pf)
		case "protocol_function_declaration":
			swiftAddCallable(child, "protocol_requirement", container, visibility, content, pf)
		case "property_declaration":
			swiftAddMemberValue(child, container, content, pf)
		case "protocol_property_declaration":
			swiftAddMemberValue(child, container, content, pf)
		case "enum_entry":
			swiftAddEnumCase(child, container, content, pf)
		case "ERROR":
			swiftExtractSymbols(child, container, visibility, content, pf)
		}
	}
}

func swiftAddMemberValue(node *sitter.Node, container string, content []byte, pf *graph.ParsedFile) {
	if container == "" {
		return
	}
	for _, name := range swiftBindingNames(node, content) {
		pf.Scope.Imports = append(pf.Scope.Imports, graph.ScopeImport{
			Kind: graph.ScopeImportSwiftMemberValue, OwnerModule: container,
			LocalName: name, Static: swiftMemberStatic(node, content),
		})
	}
}

func swiftAddEnumCase(node *sitter.Node, container string, content []byte, pf *graph.ParsedFile) {
	if container == "" {
		return
	}
	for _, name := range swiftBindingNames(node, content) {
		pf.Scope.Imports = append(pf.Scope.Imports, graph.ScopeImport{
			Kind: graph.ScopeImportSwiftEnumCase, OwnerModule: container,
			LocalName: name, Static: true,
		})
	}
}

func swiftBindingNames(node *sitter.Node, content []byte) []string {
	var names []string
	for i := 0; i < int(node.ChildCount()); i++ {
		if node.FieldNameForChild(i) != "name" {
			continue
		}
		names = append(names, swiftPatternBindingNames(node.Child(i), content)...)
	}
	return names
}

func swiftPatternBindingNames(node *sitter.Node, content []byte) []string {
	if node == nil {
		return nil
	}
	if bound := node.ChildByFieldName("bound_identifier"); bound != nil {
		return []string{nodeText(bound, content)}
	}
	if node.Type() == "simple_identifier" {
		return []string{nodeText(node, content)}
	}
	var names []string
	for i := 0; i < int(node.NamedChildCount()); i++ {
		child := node.NamedChild(i)
		if child.Type() == "pattern" || child.Type() == "simple_identifier" {
			names = append(names, swiftPatternBindingNames(child, content)...)
		}
	}
	return names
}

func swiftMemberStatic(node *sitter.Node, content []byte) bool {
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == "modifiers" {
			for j := 0; j < int(child.ChildCount()); j++ {
				modifier := child.Child(j)
				if modifier.Type() != "property_modifier" {
					continue
				}
				text := strings.TrimSpace(nodeText(modifier, content))
				if text == "static" || text == "class" {
					return true
				}
			}
		}
	}
	return false
}

func swiftAddType(node *sitter.Node, container, kind, inheritedVisibility string, content []byte, pf *graph.ParsedFile) {
	nameNode := childByFieldName(node, "name")
	if nameNode == nil || nameNode.Type() != "type_identifier" {
		nameNode = firstChild(node, "type_identifier")
	}
	if nameNode == nil {
		return
	}
	name := nodeText(nameNode, content)
	qualified := swiftJoinQName(container, name)
	visibility := swiftVisibility(node, inheritedVisibility, content)
	pf.Symbols = append(pf.Symbols, graph.Symbol{
		Language: "swift", Kind: kind, Name: name,
		QualifiedName: qualified, ContainerName: container,
		Visibility: visibility, Range: nodeRange(node),
		DocSummary: prevCommentText(node, content),
		StableKey:  "type:swift:" + qualified,
	})
	pf.SwiftDeclarationFacts = append(pf.SwiftDeclarationFacts, graph.SwiftDeclarationFact{SymbolIndex: len(pf.Symbols) - 1, Final: swiftHasModifier(node, "final", content)})
	if body := childByFieldName(node, "body"); body != nil {
		memberVisibility := "internal"
		if kind == "protocol" {
			memberVisibility = visibility
		}
		swiftExtractSymbols(body, qualified, memberVisibility, content, pf)
	}
}

func swiftAddCallable(node *sitter.Node, kind, container, inheritedVisibility string, content []byte, pf *graph.ParsedFile) {
	name := "init"
	if node.Type() != "init_declaration" {
		nameNode := childByFieldName(node, "name")
		if nameNode == nil {
			return
		}
		name = nodeText(nameNode, content)
	}
	selector := swiftSelector(node, name, content)
	qualified := swiftJoinQName(container, name)
	owner := container
	if owner == "" {
		owner = "__global__"
	}
	static := swiftCallableStatic(node, content)
	pf.Symbols = append(pf.Symbols, graph.Symbol{
		Language: "swift", Kind: kind, Name: name,
		QualifiedName: qualified, ContainerName: container,
		Signature: selector, Visibility: swiftVisibility(node, inheritedVisibility, content),
		Static: &static, Range: nodeRange(node),
		DocSummary: prevCommentText(node, content),
		StableKey:  "func:swift:" + owner + ":" + selector,
	})
	pf.SwiftDeclarationFacts = append(pf.SwiftDeclarationFacts, graph.SwiftDeclarationFact{
		SymbolIndex: len(pf.Symbols) - 1, Final: swiftHasModifier(node, "final", content),
		Override: swiftHasModifier(node, "override", content), Dispatch: swiftCallableDispatch(node, content),
	})
	minArity, maxArity := swiftArity(node, content)
	pf.Symbols[len(pf.Symbols)-1].ArityMin = minArity
	pf.Symbols[len(pf.Symbols)-1].ArityMax = maxArity
}

func swiftExtractCalls(root *sitter.Node, content []byte, pf *graph.ParsedFile) {
	var walk func(*sitter.Node, int, bool)
	walk = func(node *sitter.Node, localDepth int, modeledCallable bool) {
		if node.Type() == "function_declaration" || node.Type() == "init_declaration" {
			if modeledCallable {
				localDepth++
			}
			modeledCallable = true
		}
		if node.Type() == "call_expression" && localDepth == 0 {
			swiftAddCall(node, content, pf)
		}
		if node.Type() == "constructor_expression" && localDepth == 0 {
			swiftAddConstructorCall(node, content, pf)
		}
		for i := 0; i < int(node.ChildCount()); i++ {
			walk(node.Child(i), localDepth, modeledCallable)
		}
	}
	walk(root, 0, false)
}

func swiftAddCall(call *sitter.Node, content []byte, pf *graph.ParsedFile) {
	fn := childByFieldName(call, "function")
	if fn == nil && call.ChildCount() > 0 {
		fn = call.Child(0)
	}
	if fn == nil || nodeText(fn, content) == "" {
		return
	}
	name := nodeText(fn, content)
	evidence := swiftCallEvidence(fn, name) + swiftCallShapeSuffix(call, content)
	if swiftNominalCallProven(name, call, pf) {
		evidence = "swift:initializer" + swiftCallShapeSuffix(call, content)
	}
	swiftAppendCall(call, name, evidence, content, pf)
}

func swiftAddConstructorCall(call *sitter.Node, content []byte, pf *graph.ParsedFile) {
	typeNode := childByFieldName(call, "type")
	if typeNode == nil {
		typeNode = firstChild(call, "user_type")
	}
	if typeNode == nil {
		return
	}
	raw := nodeText(typeNode, content)
	name := swiftStripGeneric(raw)
	if name != "" {
		evidence := "swift:initializer_unproven"
		if swiftNominalCallProven(name, call, pf) {
			evidence = "swift:initializer"
		}
		if strings.Contains(raw, "<") {
			evidence += ";generic_specialization=true"
		}
		swiftAppendCall(call, name, evidence+swiftCallShapeSuffix(call, content), content, pf)
	}
}

func swiftNominalCallProven(name string, call *sitter.Node, pf *graph.ParsedFile) bool {
	root := name
	if dot := strings.IndexByte(root, '.'); dot >= 0 {
		root = root[:dot]
	}
	if !swiftTypePath.MatchString(name) || swiftLexicalBindingBlocks(pf.Scope.SwiftLexicalBindings, root, int(call.StartPoint().Row)+1) {
		return false
	}
	count := 0
	for _, symbol := range pf.Symbols {
		if symbol.Language == "swift" && symbol.QualifiedName == name {
			switch symbol.Kind {
			case "struct", "enum", "actor", "class":
				count++
			}
		}
	}
	return count == 1
}

func swiftAppendCall(call *sitter.Node, name, evidence string, content []byte, pf *graph.ParsedFile) {
	edge := graph.Edge{DstName: name, Kind: "calls", Evidence: evidence, Line: int(call.StartPoint().Row) + 1}
	if arity := swiftCallArity(call); arity != nil {
		edge.CallArity = arity
	}
	pf.Edges = append(pf.Edges, edge)
	pf.References = append(pf.References, graph.Reference{Kind: "call", Name: swiftCallBaseName(name), QualifiedName: name, Range: nodeRange(call)})
}

func swiftCallEvidence(fn *sitter.Node, name string) string {
	category := "swift:member"
	if fn.Type() == "simple_identifier" {
		category = "swift:bare"
	} else if strings.HasPrefix(name, "self.") {
		category = "swift:self"
	} else if strings.HasPrefix(name, "Self.") {
		category = "swift:Self"
	} else if strings.HasPrefix(name, "super.") {
		category = "swift:super"
	} else if strings.Contains(name, "?") {
		category = "swift:optional_member"
	} else if strings.Contains(name, "!") {
		category = "swift:forced_member"
	} else if strings.Count(name, ".") > 1 {
		category = "swift:chained"
	} else {
		category = "swift:member;type_path_unproven"
	}
	return category
}

func swiftLabelsSuffix(call *sitter.Node, content []byte) string {
	args := swiftCallArguments(call)
	if args == nil {
		return ""
	}
	var labels []string
	for i := 0; i < int(args.ChildCount()); i++ {
		arg := args.Child(i)
		if arg.Type() != "value_argument" {
			continue
		}
		label := firstChild(arg, "value_argument_label")
		if label == nil {
			labels = append(labels, "_")
		} else {
			labels = append(labels, strings.TrimSuffix(nodeText(label, content), ":")+":")
		}
	}
	if len(labels) == 0 {
		return ""
	}
	return ";labels=" + strings.Join(labels, ",")
}

func swiftCallShapeSuffix(call *sitter.Node, content []byte) string {
	regular := swiftLabelsSuffix(call, content)
	suffix := swiftCallSuffix(call)
	if suffix == nil {
		return regular
	}
	labels := swiftTrailingLabels(suffix, content)
	if len(labels) == 0 {
		return regular
	}
	return regular + ";trailing_labels=" + strings.Join(labels, ",")
}

func swiftCallSuffix(call *sitter.Node) *sitter.Node {
	if call == nil {
		return nil
	}
	for i := 0; i < int(call.ChildCount()); i++ {
		child := call.Child(i)
		if child.Type() == "call_suffix" || child.Type() == "constructor_suffix" {
			return child
		}
	}
	return nil
}

func swiftCallArguments(call *sitter.Node) *sitter.Node {
	suffix := swiftCallSuffix(call)
	if suffix == nil {
		return nil
	}
	for i := 0; i < int(suffix.ChildCount()); i++ {
		if child := suffix.Child(i); child.Type() == "value_arguments" {
			return child
		}
	}
	return nil
}

func swiftTrailingLabels(suffix *sitter.Node, content []byte) []string {
	labels := make([]string, 0, 1)
	next := "_"
	for i := 0; i < int(suffix.ChildCount()); i++ {
		child := suffix.Child(i)
		switch child.Type() {
		case "lambda_literal":
			labels = append(labels, next)
			next = "_"
		case "simple_identifier":
			next = nodeText(child, content) + ":"
		}
	}
	return labels
}

func swiftSelector(node *sitter.Node, name string, content []byte) string {
	var labels []string
	for _, p := range findChildren(node, "parameter") {
		label := firstChild(p, "simple_identifier")
		if label == nil {
			labels = append(labels, "_")
		} else {
			labelText := nodeText(label, content) + ":"
			labels = append(labels, labelText)
		}
	}
	return name + "(" + strings.Join(labels, ",") + ")"
}

func swiftArity(node *sitter.Node, content []byte) (*int, *int) {
	params := findChildren(node, "parameter")
	min, max := len(params), len(params)
	for _, param := range params {
		text := nodeText(param, content)
		if strings.Contains(text, "...") {
			max = -1
		}
		if strings.Contains(text, "=") {
			min--
		}
	}
	minPtr, maxPtr := min, max
	if max < 0 {
		return &minPtr, nil
	}
	return &minPtr, &maxPtr
}

func swiftCallArity(call *sitter.Node) *int {
	suffix := swiftCallSuffix(call)
	if suffix == nil {
		return nil
	}
	count := 0
	for i := 0; i < int(suffix.ChildCount()); i++ {
		child := suffix.Child(i)
		switch child.Type() {
		case "value_arguments":
			for j := 0; j < int(child.ChildCount()); j++ {
				if child.Child(j).Type() == "value_argument" {
					count++
				}
			}
		case "lambda_literal":
			count++
		}
	}
	return &count
}

func swiftCallableStatic(node *sitter.Node, content []byte) bool {
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() == "static" || child.Type() == "class" || (child.Type() == "modifiers" && strings.Contains(nodeText(child, content), "static")) {
			return true
		}
	}
	return false
}

func swiftHasModifier(node *sitter.Node, want string, content []byte) bool {
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() != want && child.Type() != "modifiers" {
			continue
		}
		for _, token := range strings.Fields(nodeText(child, content)) {
			if strings.TrimSpace(token) == want {
				return true
			}
		}
	}
	return false
}

func swiftCallableDispatch(node *sitter.Node, content []byte) string {
	hasClass, hasStatic := swiftHasModifier(node, "class", content), swiftHasModifier(node, "static", content)
	if hasClass && hasStatic {
		return ""
	}
	if hasClass {
		return "class"
	}
	if hasStatic {
		return "static"
	}
	return "instance"
}

func swiftExtractInheritanceRelations(root *sitter.Node, content []byte, pf *graph.ParsedFile) {
	kinds := make(map[string][]string)
	for _, symbol := range pf.Symbols {
		switch symbol.Kind {
		case "class", "protocol", "struct", "enum", "actor":
			kinds[symbol.QualifiedName] = append(kinds[symbol.QualifiedName], symbol.Kind)
		}
	}
	var walk func(*sitter.Node, string)
	walk = func(node *sitter.Node, container string) {
		if node == nil {
			return
		}
		nextContainer := container
		if node.Type() == "class_declaration" && swiftIsExtension(node, content) {
			if target := swiftExtensionTarget(node, content); target != "" {
				nextContainer = target
			}
			if target := swiftExtensionFactTarget(node, content); target != "" {
				specifiers, constrained := swiftInheritanceHeaderFacts(node)
				for _, spec := range specifiers {
					targetNode := childByFieldName(spec, "inherits_from")
					if targetNode == nil {
						targetNode = firstChild(spec, "user_type")
					}
					if targetNode == nil {
						continue
					}
					raw := strings.TrimSpace(nodeText(targetNode, content))
					targetName := swiftStripGeneric(raw)
					if targetName == "" {
						continue
					}
					relation := "conformance"
					if matches := kinds[targetName]; len(matches) > 0 && (len(matches) != 1 || matches[0] != "protocol") {
						relation = "unproven"
					}
					pf.SwiftInheritanceRelations = append(pf.SwiftInheritanceRelations, graph.SwiftInheritanceRelation{
						Child: target, Target: targetName, Relation: relation, Range: nodeRange(targetNode), Generic: raw != targetName, Constrained: constrained,
					})
				}
			}
		} else if (node.Type() == "class_declaration" || node.Type() == "protocol_declaration") && !swiftIsExtension(node, content) {
			nameNode := childByFieldName(node, "name")
			if nameNode == nil {
				nameNode = firstChild(node, "type_identifier")
			}
			if nameNode != nil {
				child := swiftJoinQName(container, nodeText(nameNode, content))
				nextContainer = child
				specifiers, constrained := swiftInheritanceHeaderFacts(node)
				for _, spec := range specifiers {
					targetNode := childByFieldName(spec, "inherits_from")
					if targetNode == nil {
						targetNode = firstChild(spec, "user_type")
					}
					if targetNode == nil {
						continue
					}
					raw := strings.TrimSpace(nodeText(targetNode, content))
					target := swiftStripGeneric(raw)
					relation := "unproven"
					matches, childKinds := kinds[target], kinds[child]
					if target != child && len(matches) == 1 && len(childKinds) == 1 {
						switch {
						case matches[0] == "class" && childKinds[0] == "class":
							relation = "superclass"
						case matches[0] == "protocol" && childKinds[0] != "protocol":
							relation = "conformance"
						}
					}
					pf.SwiftInheritanceRelations = append(pf.SwiftInheritanceRelations, graph.SwiftInheritanceRelation{
						Child: child, Target: target, Relation: relation, Range: nodeRange(targetNode), Generic: raw != target,
						Constrained: constrained,
					})
				}
			}
		}
		for i := 0; i < int(node.ChildCount()); i++ {
			walk(node.Child(i), nextContainer)
		}
	}
	walk(root, "")
	sort.Slice(pf.SwiftInheritanceRelations, func(i, j int) bool {
		a, b := pf.SwiftInheritanceRelations[i], pf.SwiftInheritanceRelations[j]
		if a.Child != b.Child {
			return a.Child < b.Child
		}
		if a.Target != b.Target {
			return a.Target < b.Target
		}
		return a.Range.StartLine < b.Range.StartLine || a.Range.StartLine == b.Range.StartLine && a.Range.StartCol < b.Range.StartCol
	})
}

// swiftExtensionFactTarget reads only the extension header. It intentionally
// differs from swiftExtensionTarget: constrained extensions may not own
// member symbols, but their conformance is still parser-owned hazard evidence.
func swiftExtensionFactTarget(node *sitter.Node, content []byte) string {
	if node == nil || !swiftIsExtension(node, content) {
		return ""
	}
	target := childByFieldName(node, "name")
	if target == nil {
		target = firstChild(node, "user_type")
	}
	if target == nil {
		return ""
	}
	text := strings.TrimSpace(nodeText(target, content))
	if !swiftTypePath.MatchString(text) {
		return ""
	}
	return text
}

func swiftInheritanceSpecifiers(root *sitter.Node) []*sitter.Node {
	specifiers, _ := swiftInheritanceHeaderFacts(root)
	return specifiers
}

func swiftInheritanceHeaderFacts(root *sitter.Node) ([]*sitter.Node, bool) {
	var out []*sitter.Node
	constrained := false
	body := childByFieldName(root, "body")
	var walk func(*sitter.Node)
	walk = func(node *sitter.Node) {
		for i := 0; i < int(node.ChildCount()); i++ {
			child := node.Child(i)
			if child == body {
				continue
			}
			if child.Type() == "inheritance_specifier" {
				out = append(out, child)
				continue
			}
			if child.Type() == "type_constraints" || child.Type() == "type_parameters" {
				constrained = true
				continue
			}
			if child != root && swiftNominalDeclarationNode(child.Type()) {
				continue
			}
			walk(child)
		}
	}
	walk(root)
	return out, constrained
}

func swiftNominalDeclarationNode(kind string) bool {
	switch kind {
	case "class_declaration", "protocol_declaration", "struct_declaration", "enum_declaration", "actor_declaration":
		return true
	default:
		return false
	}
}

func swiftVisibility(node *sitter.Node, inherited string, content []byte) string {
	for i := 0; i < int(node.ChildCount()); i++ {
		child := node.Child(i)
		if child.Type() != "modifiers" && child.Type() != "visibility_modifier" {
			continue
		}
		text := nodeText(child, content)
		for _, value := range []string{"fileprivate", "private", "internal", "package", "public", "open"} {
			if strings.Contains(text, value) {
				return value
			}
		}
	}
	if inherited != "" {
		return inherited
	}
	return "internal"
}

func swiftNominalKind(node *sitter.Node, content []byte) string {
	for i := 0; i < int(node.ChildCount()); i++ {
		switch node.Child(i).Type() {
		case "class", "struct", "enum", "protocol", "actor":
			return node.Child(i).Type()
		}
	}
	for _, word := range strings.Fields(nodeText(node, content)) {
		switch word {
		case "class", "struct", "enum", "protocol", "actor":
			return word
		}
	}
	return ""
}

func swiftIsExtension(node *sitter.Node, content []byte) bool {
	return firstChild(node, "extension") != nil || strings.HasPrefix(strings.TrimSpace(nodeText(node, content)), "extension ")
}

var swiftTypePath = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*)*$`)

func swiftExtensionTarget(node *sitter.Node, content []byte) string {
	if len(findDescendants(node, "type_constraints")) > 0 {
		return ""
	}
	target := childByFieldName(node, "name")
	if target == nil {
		target = firstChild(node, "user_type")
	}
	if target == nil {
		return ""
	}
	text := nodeText(target, content)
	if !swiftTypePath.MatchString(text) {
		return ""
	}
	return text
}

func swiftJoinQName(container, name string) string {
	if container == "" {
		return name
	}
	return container + "." + name
}

func swiftStripGeneric(name string) string {
	if i := strings.IndexByte(name, '<'); i >= 0 {
		return strings.TrimSpace(name[:i])
	}
	return name
}

func swiftCallBaseName(name string) string {
	if i := strings.LastIndexByte(name, '.'); i >= 0 {
		return name[i+1:]
	}
	return name
}
