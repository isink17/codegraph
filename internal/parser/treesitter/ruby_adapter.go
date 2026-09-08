//go:build cgo

package treesitter

import (
	"context"
	"path/filepath"
	"strings"

	sitter "github.com/smacker/go-tree-sitter"
	ruby "github.com/smacker/go-tree-sitter/ruby"

	"github.com/isink17/codegraph/internal/graph"
)

type RubyAdapter struct{}

func NewRuby() *RubyAdapter                      { return &RubyAdapter{} }
func (a *RubyAdapter) Language() string          { return "ruby" }
func (a *RubyAdapter) Extensions() []string      { return []string{".rb"} }
func (a *RubyAdapter) Supports(path string) bool { return strings.EqualFold(filepath.Ext(path), ".rb") }

func (a *RubyAdapter) Parse(ctx context.Context, path string, content []byte) (graph.ParsedFile, error) {
	root, err := parse(ctx, ruby.GetLanguage(), content)
	if err != nil {
		return graph.ParsedFile{}, err
	}
	p := graph.ParsedFile{Language: "ruby", FileTokens: computeFileTokens(content)}
	rubyExtractImports(root, content, &p)
	rubyExtractSymbols(root, "", content, &p)
	rubyExtractCalls(root, content, &p)
	rubyLinkTests(&p)
	return p, nil
}

func rubyExtractImports(root *sitter.Node, content []byte, pf *graph.ParsedFile) {
	for _, call := range findDescendants(root, "call") {
		method := nodeText(childByFieldName(call, "method"), content)
		if method != "require" && method != "require_relative" {
			continue
		}
		for _, strNode := range findDescendants(childByFieldName(call, "arguments"), "string") {
			if value := strings.Trim(nodeText(strNode, content), `"'`); value != "" {
				pf.Imports = append(pf.Imports, value)
			}
		}
	}
}

// Ruby declaration constants use dots internally. Runtime receiver strings do not.
func rubySemanticConstantQName(node *sitter.Node, content []byte) (string, bool) {
	if node == nil || (node.Type() != "constant" && node.Type() != "scope_resolution") {
		return "", false
	}
	text := strings.TrimPrefix(nodeText(node, content), "::")
	if text == "" || strings.ContainsAny(text, " \t\n") {
		return "", false
	}
	return strings.ReplaceAll(text, "::", "."), true
}

func rubyJoinQName(parent, name string) string {
	if parent == "" {
		return name
	}
	if name == "" {
		return parent
	}
	return parent + "." + name
}

func rubyBool(value bool) *bool { return &value }

func rubyExtractSymbols(node *sitter.Node, container string, content []byte, pf *graph.ParsedFile) {
	rubyExtractSymbolsIn(node, container, false, content, pf)
}

func rubyExtractSymbolsIn(node *sitter.Node, container string, singleton bool, content []byte, pf *graph.ParsedFile) {
	for i := range int(node.ChildCount()) {
		child := node.Child(i)
		switch child.Type() {
		case "class":
			rubyAddType(child, "class", container, content, pf)
		case "module":
			rubyAddType(child, "type", container, content, pf)
		case "method":
			rubyAddMethod(child, container, singleton, content, pf)
		case "singleton_method":
			if !singleton && nodeText(childByFieldName(child, "object"), content) == "self" {
				rubyAddMethod(child, container, true, content, pf)
			}
		case "singleton_class":
			if !singleton && nodeText(childByFieldName(child, "value"), content) == "self" {
				if body := childByFieldName(child, "body"); body != nil {
					rubyExtractSymbolsIn(body, container, true, content, pf)
				}
			}
		}
	}
}

func rubyAddType(node *sitter.Node, kind, parent string, content []byte, pf *graph.ParsedFile) {
	nameNode := childByFieldName(node, "name")
	name, ok := rubySemanticConstantQName(nameNode, content)
	if !ok {
		return
	}
	qualified, lexicalParent := name, ""
	if nameNode.Type() == "constant" {
		qualified, lexicalParent = rubyJoinQName(parent, name), parent
	} else if parent != "" {
		// Relative qualified-open target is not syntax-proven in nested scope.
		return
	}
	leaf := name[strings.LastIndexByte(name, '.')+1:]
	pf.Symbols = append(pf.Symbols, graph.Symbol{Language: "ruby", Kind: kind, Name: leaf,
		QualifiedName: qualified, ContainerName: lexicalParent, Range: nodeRange(node),
		DocSummary: prevCommentText(node, content), StableKey: "type:ruby:" + qualified})
	pf.Scope.Imports = append(pf.Scope.Imports, graph.ScopeImport{Kind: graph.ScopeImportRubyLexicalParent,
		OwnerModule: qualified, SourceSpecifier: lexicalParent})
	if body := childByFieldName(node, "body"); body != nil {
		rubyExtractSymbols(body, qualified, content, pf)
	}
}

func rubyAddMethod(node *sitter.Node, container string, static bool, content []byte, pf *graph.ParsedFile) {
	name := nodeText(childByFieldName(node, "name"), content)
	if name == "" {
		return
	}
	owner := container
	if owner == "" {
		owner = "top"
	}
	nature := "instance"
	if static {
		nature = "singleton"
	}
	pf.Symbols = append(pf.Symbols, graph.Symbol{Language: "ruby", Kind: "function", Name: name,
		QualifiedName: rubyJoinQName(container, name), ContainerName: container, Static: rubyBool(static),
		Range: nodeRange(node), DocSummary: prevCommentText(node, content),
		StableKey: "func:ruby:" + owner + ":" + nature + ":" + name})
}

func rubyCallOperator(receiver, method *sitter.Node, content []byte) string {
	if receiver == nil || method == nil || receiver.EndByte() > method.StartByte() {
		return ""
	}
	return strings.TrimSpace(string(content[receiver.EndByte():method.StartByte()]))
}

func rubyCallEvidence(receiver *sitter.Node, operator string) string {
	if receiver == nil {
		return "ruby:implicit_receiver"
	}
	if operator == "&." {
		return "ruby:safe_navigation"
	}
	if receiver.Type() == "self" {
		return "ruby:self_receiver"
	}
	if receiver.Type() == "constant" || receiver.Type() == "scope_resolution" {
		return "ruby:constant_receiver"
	}
	if receiver.Type() == "call" {
		return "ruby:chained_receiver"
	}
	return "ruby:value_receiver"
}

// rubyAssignmentTarget reports whether a `call` node is the left-hand side of
// an assignment: `self.name = v` parses as an assignment whose left IS a call
// node spelled `self.name`, but the method it invokes is the writer `name=`,
// not the reader `name`. The writer's spelling is not what the node carries, so
// the edge is dropped rather than pointed at the wrong method.
func rubyAssignmentTarget(call *sitter.Node) bool {
	node := call
	// A multiple assignment nests its targets one or two levels deeper:
	// `self.a, self.b = 1, 2` puts each call under a left_assignment_list.
	for _, wrapper := range []string{"left_assignment_list", "rest_assignment", "destructured_left_assignment"} {
		parent := node.Parent()
		if parent == nil {
			return false
		}
		if parent.Type() == wrapper {
			node = parent
		}
	}
	parent := node.Parent()
	if parent == nil {
		return false
	}
	switch parent.Type() {
	case "assignment", "operator_assignment":
		return childByFieldName(parent, "left") == node
	}
	return false
}

// rubyBlockLocalDefinition reports whether a call sits inside a `def` that is
// itself inside a block (`Class.new do def run; end end`). rubyExtractSymbols
// walks direct children only, so such a `def` is never a symbol and its body
// would otherwise be attributed to the enclosing method -- binding its calls to
// the enclosing container's members. Ruby's runtime owner there is the object
// the block builds, which is not syntax-proven, so fail closed.
func rubyBlockLocalDefinition(call *sitter.Node) bool {
	def := call.Parent()
	for def != nil && def.Type() != "method" && def.Type() != "singleton_method" {
		def = def.Parent()
	}
	for node := def; node != nil; node = node.Parent() {
		if node.Type() == "block" || node.Type() == "do_block" {
			return true
		}
	}
	return false
}

// rubySelfRebindingMethods take a block whose `self` is some other object, so a
// receiver-less call inside one is not the lexical self the parser can see.
var rubySelfRebindingMethods = map[string]struct{}{
	"instance_eval": {}, "instance_exec": {},
	"class_eval": {}, "class_exec": {},
	"module_eval": {}, "module_exec": {},
	"define_method": {}, "define_singleton_method": {},
}

// rubySelfRebindingConstants build an anonymous class or module and evaluate
// the block against it, so `self` inside is that new object.
var rubySelfRebindingConstants = map[string]struct{}{
	"Class": {}, "Module": {}, "Struct": {}, "Data": {},
}

// rubyRebindsSelf reports whether a call sits inside a block that rebinds
// `self`. `other.instance_eval { run() }` calls `run` on `other`, not on the
// enclosing method's receiver, and which object that is cannot be proven from
// syntax -- so no receiver evidence is emitted for it at all. Ordinary blocks
// (`each`, `map`, a lambda) keep the enclosing lexical self and are unaffected.
func rubyRebindsSelf(call *sitter.Node, content []byte) bool {
	for node := call.Parent(); node != nil; node = node.Parent() {
		if node.Type() != "block" && node.Type() != "do_block" {
			continue
		}
		parent := node.Parent()
		if parent == nil || parent.Type() != "call" {
			continue
		}
		method := nodeText(childByFieldName(parent, "method"), content)
		if _, ok := rubySelfRebindingMethods[method]; ok {
			return true
		}
		// `Class.new do ... end` class_evals its block, so `self` there is the
		// anonymous class. `new` alone is far too broad a name to key on, so
		// this is restricted to the constants that actually do it.
		if method != "new" && method != "define" {
			continue
		}
		receiver := childByFieldName(parent, "receiver")
		if receiver == nil || receiver.Type() != "constant" {
			continue
		}
		if _, ok := rubySelfRebindingConstants[nodeText(receiver, content)]; ok {
			return true
		}
	}
	return false
}

func rubyExtractCalls(root *sitter.Node, content []byte, pf *graph.ParsedFile) {
	for _, call := range findDescendants(root, "call") {
		methodNode := childByFieldName(call, "method")
		method := nodeText(methodNode, content)
		if method == "" || method == "require" || method == "require_relative" {
			continue
		}
		receiver := childByFieldName(call, "receiver")
		name := method
		if receiver != nil {
			name = nodeText(receiver, content) + rubyCallOperator(receiver, methodNode, content) + method
		}
		// The reference is a real occurrence either way; only the call edge
		// needs a receiver the parser can vouch for.
		pf.References = append(pf.References, graph.Reference{Kind: "call", Name: name, QualifiedName: name, Range: nodeRange(call)})
		if rubyAssignmentTarget(call) || rubyBlockLocalDefinition(call) || rubyRebindsSelf(call, content) {
			continue
		}
		evidence := rubyCallEvidence(receiver, rubyCallOperator(receiver, methodNode, content))
		pf.Edges = append(pf.Edges, graph.Edge{DstName: name, Kind: "calls", Evidence: evidence, Line: int(call.StartPoint().Row) + 1})
	}
}

func rubyLinkTests(pf *graph.ParsedFile) {
	for i, sym := range pf.Symbols {
		if sym.Kind != "function" || sym.ContainerName != "" {
			continue
		}
		var target string
		if strings.HasPrefix(sym.Name, "test_") {
			target = strings.TrimPrefix(sym.Name, "test_")
		} else if strings.HasPrefix(sym.Name, "Test") && testNameUpperBoundary(sym.Name[4:]) {
			target = sym.Name[4:]
		}
		if target == "" {
			continue
		}
		pf.TestLinks = append(pf.TestLinks, graph.TestLink{TestName: sym.QualifiedName, TargetName: target,
			Reason: "test_name_match", Score: 0.7, TestSymbolKey: sym.StableKey,
			TargetStableKey: "func:ruby:top:instance:" + target, TestSymbolIndex: intRef(i)})
	}
}
