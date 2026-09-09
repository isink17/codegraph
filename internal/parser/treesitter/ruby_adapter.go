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
	// Root-level statements are their own lexical owner. A bare `Service = X`
	// there names a root constant, which is never a single-segment candidate
	// and records nothing; a qualified `App::Service = X` is absolute and names
	// its constant exactly.
	rubyScanConstantAssignments(root, rubyConstantScope{cref: true}, content, &p)
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

// rubyVisibilitySpecifiers are the three bare toggles a body may carry.
var rubyVisibilitySpecifiers = map[string]bool{"public": true, "private": true, "protected": true}

func rubyExtractSymbolsIn(node *sitter.Node, container string, singleton bool, content []byte, pf *graph.ParsedFile) {
	// Default singleton visibility is public, and a bare toggle moves it only
	// for the definitions that follow it in the same body. The state is tracked
	// for a `class << self` body only: a bare `private` in an ordinary class
	// body sets the default for that class's INSTANCE methods and leaves
	// `def self.run` public, so applying it to singleton definitions would
	// invent a private method Ruby never made.
	state := "public"
	for i := range int(node.ChildCount()) {
		child := node.Child(i)
		switch child.Type() {
		case "class":
			rubyAddType(child, "class", container, content, pf)
		case "module":
			rubyAddType(child, "type", container, content, pf)
		case "method":
			rubyAddMethod(child, container, singleton, state, content, pf)
		case "singleton_method":
			if !singleton && nodeText(childByFieldName(child, "object"), content) == "self" {
				rubyAddMethod(child, container, true, "public", content, pf)
			}
		case "singleton_class":
			if !singleton && nodeText(childByFieldName(child, "value"), content) == "self" {
				if body := childByFieldName(child, "body"); body != nil {
					rubyExtractSymbolsIn(body, container, true, content, pf)
				}
			}
		case "identifier":
			// A bare toggle parses as a plain identifier, not a call.
			name := nodeText(child, content)
			if singleton && rubyVisibilitySpecifiers[name] {
				state = name
			}
			// Argument-less `module_function` turns every following `def` into
			// a module-singleton method as well. Synthesising those would be a
			// guess, so the owner's singleton surface is unknown. An
			// argument-less `private_class_method` names nothing this parser
			// can see either, and it is not the shape a no-op takes.
			if name == "module_function" || name == "private_class_method" || name == "public_class_method" {
				rubyAddVisibilityHazard(container, pf)
			}
		case "call":
			rubyVisibilityCall(child, container, singleton, content, pf)
		}
	}
}

// rubyAddVisibilityHazard records that an owner's singleton visibility cannot
// be concluded from syntax. It carries no method name on purpose: the point is
// that the affected names are exactly what could not be proven.
func rubyAddVisibilityHazard(container string, pf *graph.ParsedFile) {
	if container == "" {
		return
	}
	pf.Scope.Imports = append(pf.Scope.Imports, graph.ScopeImport{
		Kind: graph.ScopeImportRubySingletonVisibilityUnknown, OwnerModule: container, Static: true})
}

// rubyVisibilityAPIs are the receiver-taking method names that change singleton
// visibility or the module singleton surface. A call to one of them through a
// receiver this parser cannot resolve to the current cref proves that SOMETHING
// changed, which is exactly enough to withdraw the owner.
var rubyVisibilityAPIs = map[string]bool{
	"private_class_method": true, "public_class_method": true, "module_function": true,
	"private": true, "public": true, "protected": true,
}

// rubyVisibilityCall records a syntax-proven singleton-visibility override, or
// an owner-level hazard when a recognised visibility API is spelled in a form
// whose targets are not literal method names.
//
// `private_class_method` is an ordinary public method of Module, so a body may
// spell it bare or with a literal `self` receiver; both are the current cref
// and are treated identically. Any other receiver -- `Other.private_class_method
// :x`, `Service.private_class_method :x` inside `Service` itself, or a
// `singleton_class` expression -- names an owner this parser has not proven, so
// the current owner is withdrawn rather than either trusted or ignored.
func rubyVisibilityCall(call *sitter.Node, container string, singleton bool, content []byte, pf *graph.ParsedFile) {
	if container == "" {
		return
	}
	method := nodeText(childByFieldName(call, "method"), content)
	if receiver := childByFieldName(call, "receiver"); receiver != nil && nodeText(receiver, content) != "self" {
		// An eigenclass expression reaches the singleton class through methods
		// with no visibility name of their own (`singleton_class.send(:private,
		// name)`, `singleton_class.class_eval { ... }`).
		if rubyVisibilityAPIs[method] || strings.HasPrefix(nodeText(receiver, content), "singleton_class") {
			rubyAddVisibilityHazard(container, pf)
		}
		return
	}
	specifier := ""
	switch method {
	case "private_class_method", "public_class_method":
		// The cref inside `class << self` is the singleton class, so these
		// would name a method of the singleton's singleton. Nothing this phase
		// resolves lives there, and guessing the outer owner would invent one.
		if singleton {
			rubyAddVisibilityHazard(container, pf)
			return
		}
		specifier = "private"
		if method == "public_class_method" {
			specifier = "public"
		}
	case "module_function":
		rubyAddVisibilityHazard(container, pf)
		return
	case "public", "private", "protected":
		// With arguments these name methods of the current cref. In an ordinary
		// class body that cref holds the instance methods; only inside
		// `class << self` does one of them name a singleton method.
		if !singleton {
			return
		}
		specifier = method
	default:
		return
	}
	names := rubySymbolArguments(childByFieldName(call, "arguments"), content)
	if len(names) == 0 {
		rubyAddVisibilityHazard(container, pf)
		return
	}
	for _, name := range names {
		pf.Scope.Imports = append(pf.Scope.Imports, graph.ScopeImport{
			Kind: graph.ScopeImportRubySingletonVisibility, OwnerModule: container,
			LocalName: name, SourceSpecifier: specifier, Static: true})
	}
}

// rubySymbolArguments returns the literal method names an argument list spells,
// or nil when the list is empty or any argument is anything else -- a splat, a
// variable, an inline `def`, an interpolated string. Nil is not "no targets":
// it is "recognised operation, unprovable targets", which the caller turns into
// an owner hazard.
func rubySymbolArguments(args *sitter.Node, content []byte) []string {
	if args == nil || args.NamedChildCount() == 0 {
		return nil
	}
	names := make([]string, 0, args.NamedChildCount())
	for i := range int(args.NamedChildCount()) {
		name, ok := rubyLiteralName(args.NamedChild(i), content)
		if !ok {
			return nil
		}
		names = append(names, name)
	}
	return names
}

// rubyLiteralName decodes one argument node as a literal method or constant
// name, or reports ok=false when it is a splat, a variable, an inline `def`, an
// interpolated string, or anything else whose value is not in the source.
func rubyLiteralName(arg *sitter.Node, content []byte) (string, bool) {
	if arg == nil {
		return "", false
	}
	text := nodeText(arg, content)
	switch arg.Type() {
	case "simple_symbol":
		text = strings.TrimPrefix(text, ":")
	case "string":
		text = strings.Trim(text, `"'`)
	default:
		return "", false
	}
	if text == "" || strings.ContainsAny(text, " \t\n#{}:.'\"") {
		return "", false
	}
	return text, true
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

func rubyAddMethod(node *sitter.Node, container string, static bool, visibility string, content []byte, pf *graph.ParsedFile) {
	name := nodeText(childByFieldName(node, "name"), content)
	if name == "" {
		return
	}
	// Only singleton visibility is modelled (P22.48): an explicit constant
	// receiver may not reach a private or protected singleton method, so that
	// nature needs a proven answer. Instance visibility stays unstated rather
	// than guessed -- `self` receivers reach private methods anyway.
	if !static {
		visibility = ""
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
		Visibility: visibility, Range: nodeRange(node), DocSummary: prevCommentText(node, content),
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

// rubyScanConstantAssignments records every syntax-proven constant identity
// mutation inside one lexical owner's body, walking the whole file from the
// root so that a class or module nested under control flow is reached too --
// the symbol walk only inspects direct children, so it would miss one.
//
// A `class`/`module` declaration proves what a constant denoted at that point,
// not what it denotes: `Service = Other` rebinds `App::Service` to another
// object, so `Service.run` calls `Other.run` and the declaration's own
// singleton method is not the target. There is no load order here to say which
// assignment or declaration wins, so the constant's identity is simply
// unprovable and every constant-receiver call through it fails closed.
//
// Everything that is not an owner boundary is descended into, so an assignment
// under any control-flow shape the grammar spells -- `if`, `unless`, a
// modifier, `begin`/`rescue`, a block, a lambda -- still counts against the
// owner whose body it sits in. That is the safe default: an unrecognised
// wrapper produces a hazard rather than silence.
// rubyConstantScope is where in the file the assignment scan currently stands.
// A mutation's target depends on two different things, and only the first one
// moves with the lexical body: which constants a relative path can resolve
// through, and which object a receiver-less mutation is called on.
type rubyConstantScope struct {
	// chain is the nameable lexical nesting, innermost first. A body whose own
	// cref cannot be named (`class A::B` inside another scope) keeps its
	// enclosing chain: Ruby's nesting there really is that unnameable frame
	// plus the outer levels, so the outer levels are still candidates.
	chain []string
	// cref reports that a receiver-less or literal-`self` mutation names a
	// constant directly under chain's innermost level -- true in a class or
	// module body, and inside a singleton method, where `self` is that
	// class/module object.
	cref bool
	// eigenclass reports that the walk is directly inside `class << self`,
	// where a `def` is a singleton method of the enclosing container but a
	// bare constant lands on the singleton class instead.
	eigenclass bool
	// inMethod reports that the walk is inside a `def`. Every constant
	// assignment there -- bare `X = 1` and qualified `Foo::Bar = 1` alike --
	// is a Ruby SyntaxError, so none of them is evidence.
	inMethod bool
}

// owner is the semantic qname of the innermost nameable lexical level, or ""
// at the root.
func (s rubyConstantScope) owner() string {
	if len(s.chain) == 0 {
		return ""
	}
	return s.chain[0]
}

func rubyScanConstantAssignments(node *sitter.Node, scope rubyConstantScope, content []byte, pf *graph.ParsedFile) {
	for i := range int(node.ChildCount()) {
		child := node.Child(i)
		inner := scope
		switch child.Type() {
		case "class", "module":
			nested, ok := rubyNestedScope(child, scope, content)
			if !ok {
				continue
			}
			if body := childByFieldName(child, "body"); body != nil {
				rubyScanConstantAssignments(body, nested, content, pf)
			}
			continue
		case "singleton_class":
			// A bare constant here lands on the singleton class, so the
			// container's own constants are untouched -- but a `def` inside is
			// a singleton method of the container, and a receiver-proven
			// mutation is not about the cref at all, so the body is still
			// walked.
			inner.cref, inner.eigenclass = false, true
		case "method":
			// `def x` in an eigenclass body IS a singleton method of the
			// container, so `self` there is that class/module object.
			inner.cref, inner.eigenclass, inner.inMethod = scope.eigenclass, false, true
		case "singleton_method":
			inner.cref, inner.eigenclass, inner.inMethod = !scope.eigenclass, false, true
		case "assignment", "operator_assignment":
			if !scope.inMethod {
				rubyConstantAssignmentTargets(childByFieldName(child, "left"), scope, content, pf)
			}
		case "call":
			rubyConstantIdentityCall(child, scope, content, pf)
		}
		rubyScanConstantAssignments(child, inner, content, pf)
	}
}

// rubyNestedScope is the scope a nested `class`/`module` body opens.
//
// A bare-constant opening pushes one frame onto the enclosing chain. A
// qualified opening (`class A::B`) pushes exactly one frame, which at the root
// is nameable; inside another scope that frame is whatever `A` resolves to,
// which this parser cannot name -- so the body keeps only the enclosing chain
// (Ruby's nesting there is the unnameable frame plus those outer levels) and
// loses its cref, exactly as rubyAddType refuses to declare such a type.
func rubyNestedScope(node *sitter.Node, scope rubyConstantScope, content []byte) (rubyConstantScope, bool) {
	nameNode := childByFieldName(node, "name")
	name, ok := rubySemanticConstantQName(nameNode, content)
	if !ok {
		return scope, false
	}
	nested := rubyConstantScope{cref: true}
	if nameNode.Type() != "constant" {
		if len(scope.chain) != 0 {
			return rubyConstantScope{chain: scope.chain}, true
		}
		nested.chain = []string{name}
		return nested, true
	}
	chain := make([]string, 0, len(scope.chain)+1)
	chain = append(chain, rubyJoinQName(scope.owner(), name))
	nested.chain = append(chain, scope.chain...)
	return nested, true
}

// rubyRelativeConstantHazards records one relative constant path against every
// lexical level it could resolve through, innermost first, plus the root. Which
// level Ruby picks is the multi-segment constant lookup this phase does not
// model, and a hazard can only WITHHOLD an edge -- it never creates a target --
// so recording every syntax-derived candidate is the fail-closed choice, and
// picking one would be a guess.
func rubyRelativeConstantHazards(chain []string, path string, pf *graph.ParsedFile) {
	for _, level := range chain {
		rubyAddConstantIdentityHazard(rubyJoinQName(level, path), pf)
	}
	rubyAddConstantIdentityHazard(path, pf)
}

// rubyConstantReceiverPath returns the semantic qname path a call's receiver
// spells, whether that path is absolute, and whether the receiver is the
// current cref (absent, or a literal `self`). A value receiver -- an
// identifier, a chain, an instance variable -- names an object this parser has
// not proven and is reported as unusable.
func rubyConstantReceiverPath(call *sitter.Node, content []byte) (path string, absolute, cref, ok bool) {
	receiver := childByFieldName(call, "receiver")
	if receiver == nil || nodeText(receiver, content) == "self" {
		return "", false, true, true
	}
	if receiver.Type() != "constant" && receiver.Type() != "scope_resolution" {
		return "", false, false, false
	}
	path, ok = rubySemanticConstantQName(receiver, content)
	if !ok {
		return "", false, false, false
	}
	// `Object.const_set(:Service, X)` moves the ROOT `Service`, not
	// `Object::Service`. Root constants are never single-segment lexical
	// candidates in this phase, so there is nothing to withhold and naming
	// `Object.Service` would state a fact about a different constant.
	// ponytail: root-constant mutations unmodelled; needs root candidates first.
	if rubyRootObjectReceivers[path] {
		return "", false, false, false
	}
	return path, strings.HasPrefix(nodeText(receiver, content), "::"), false, true
}

// rubyRootObjectReceivers hold the root constant table itself.
var rubyRootObjectReceivers = map[string]bool{"Object": true, "Kernel": true, "BasicObject": true}

// rubyConstantAssignmentTargets turns one assignment's left-hand side into the
// exact semantic constant qnames whose identity it moves. A non-constant target
// (`local`, `@ivar`, `$global`, an element reference) moves no constant.
func rubyConstantAssignmentTargets(left *sitter.Node, scope rubyConstantScope, content []byte, pf *graph.ParsedFile) {
	if left == nil {
		return
	}
	switch left.Type() {
	case "left_assignment_list", "rest_assignment", "destructured_left_assignment":
		for i := range int(left.NamedChildCount()) {
			rubyConstantAssignmentTargets(left.NamedChild(i), scope, content, pf)
		}
	case "constant":
		// A bare constant assignment always defines the constant in the current
		// cref, never in an outer one, so this qname is exact -- and it is only
		// evidence when that cref is nameable.
		if scope.cref {
			rubyAddConstantIdentityHazard(rubyJoinQName(scope.owner(), nodeText(left, content)), pf)
		}
	case "scope_resolution":
		path, ok := rubySemanticConstantQName(left, content)
		if !ok {
			return
		}
		// An absolute path names its constant outright; a relative one names it
		// through whatever its first segment resolves to.
		if strings.HasPrefix(nodeText(left, content), "::") {
			rubyAddConstantIdentityHazard(path, pf)
			return
		}
		rubyRelativeConstantHazards(scope.chain, path, pf)
	}
}

// rubyConstantIdentityAPIs move a constant's identity without an assignment
// operator. `autoload` is one of them: it makes a constant appear from a file
// this graph cannot connect to the name, so a declaration of the same qname is
// no longer proof of what the name denotes.
var rubyConstantIdentityAPIs = map[string]bool{
	"const_set": true, "remove_const": true, "autoload": true,
}

// rubyConstantIdentityCall records the hazard for the literal-name forms of
// those APIs.
//
// The receiver matters, and a constant one is not a dynamic one:
// `App.const_set(:Service, Other)` at the top of a file names its target as
// exactly as a bare `Service = Other` inside `module App` does, and ignoring it
// leaves the same confidently wrong edge in place. So the cref forms (no
// receiver, or a literal `self`) keep naming the current owner, a constant or
// constant-path receiver names its own path -- exactly when absolute, against
// every lexical level it could resolve through when relative -- and everything
// else is a value this parser has not proven.
//
// A dynamic name (`const_set(name, x)`), a value receiver
// (`target.const_set(...)`), and the indirect spellings -- `send(:const_set,
// ...)`, `eval`, `class_eval`, `const_missing`, runtime load order -- are
// deliberately not modelled: none of them names a constant from syntax, and
// inventing an owner would be worse than the gap.
func rubyConstantIdentityCall(call *sitter.Node, scope rubyConstantScope, content []byte, pf *graph.ParsedFile) {
	if !rubyConstantIdentityAPIs[nodeText(childByFieldName(call, "method"), content)] {
		return
	}
	path, absolute, cref, ok := rubyConstantReceiverPath(call, content)
	if !ok {
		return
	}
	name, ok := rubyFirstSymbolArgument(childByFieldName(call, "arguments"), content)
	if !ok {
		return
	}
	switch {
	case cref:
		// Only when the current cref is a nameable class or module: inside an
		// instance method `self` is an instance and has no const_set at all,
		// and directly inside `class << self` the target is the singleton
		// class, not the container.
		if scope.cref {
			rubyAddConstantIdentityHazard(rubyJoinQName(scope.owner(), name), pf)
		}
	case absolute:
		rubyAddConstantIdentityHazard(rubyJoinQName(path, name), pf)
	default:
		rubyRelativeConstantHazards(scope.chain, rubyJoinQName(path, name), pf)
	}
}

// rubyFirstSymbolArgument returns the literal name an argument list names
// first, or ok=false when that argument is anything else.
func rubyFirstSymbolArgument(args *sitter.Node, content []byte) (string, bool) {
	if args == nil || args.NamedChildCount() == 0 {
		return "", false
	}
	names := rubySymbolArguments(args, content)
	if len(names) > 0 {
		return names[0], true
	}
	// A mixed list (`const_set(:Service, Other)`) is still exact in its first
	// argument, which is the only one that names the constant.
	first := args.NamedChild(0)
	name, ok := rubyLiteralName(first, content)
	return name, ok
}

// rubyAddConstantIdentityHazard records one exact semantic constant qname whose
// identity moved. A root-level constant carries no owner prefix and so can
// never be a single-segment lexical candidate; recording it would state a fact
// nothing reads.
func rubyAddConstantIdentityHazard(qname string, pf *graph.ParsedFile) {
	dot := strings.LastIndexByte(qname, '.')
	if dot <= 0 || dot+1 >= len(qname) {
		return
	}
	// One row per constant per file: the fact is that the identity is
	// unprovable, and repeating it says nothing more. Two mutations of one
	// constant, or one relative path whose candidate levels collide, would
	// otherwise persist duplicate evidence rows.
	for _, fact := range pf.Scope.Imports {
		if fact.Kind == graph.ScopeImportRubyConstantIdentityUnknown && fact.OwnerModule == qname {
			return
		}
	}
	pf.Scope.Imports = append(pf.Scope.Imports, graph.ScopeImport{
		Kind: graph.ScopeImportRubyConstantIdentityUnknown, OwnerModule: qname, LocalName: qname[dot+1:]})
}
