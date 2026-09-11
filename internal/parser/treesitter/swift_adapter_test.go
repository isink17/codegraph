//go:build cgo

package treesitter

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/isink17/codegraph/internal/graph"
)

func TestSwiftSemanticFoundationSymbols(t *testing.T) {
	const source = `import Foundation
import struct Foundation.URL
public struct Outer {
    private struct Inner {
        func run() { helper() }
        func run(_ value: Int) {}
        func run(name: String) {}
    }
}
class Service {
    func instanceRun() {}
    static func staticRun() {}
    class func classRun() {}
    init() {}
    init(id: Int) {}
}
protocol P {
    func run()
    static func make()
}
extension Service { func extensionRun() {} }
extension Outer.Inner { static func make() {} }
extension Array where Element == Service { func ignored() {} }
func helper() {}
`
	p := mustSwiftParse(t, "Foo.swift", source)
	want := map[string]struct {
		kind, container, signature, key, visibility string
		static                                      *bool
	}{
		"Outer":                {"struct", "", "", "type:swift:Outer", "public", nil},
		"Outer.Inner":          {"struct", "Outer", "", "type:swift:Outer.Inner", "private", nil},
		"Outer.Inner.run":      {"function", "Outer.Inner", "run()", "func:swift:Outer.Inner:run()", "internal", boolRef(false)},
		"Service":              {"class", "", "", "type:swift:Service", "internal", nil},
		"Service.instanceRun":  {"function", "Service", "instanceRun()", "func:swift:Service:instanceRun()", "internal", boolRef(false)},
		"Service.staticRun":    {"function", "Service", "staticRun()", "func:swift:Service:staticRun()", "internal", boolRef(true)},
		"Service.classRun":     {"function", "Service", "classRun()", "func:swift:Service:classRun()", "internal", boolRef(true)},
		"Service.init":         {"function", "Service", "init()", "func:swift:Service:init()", "internal", boolRef(false)},
		"P":                    {"protocol", "", "", "type:swift:P", "internal", nil},
		"P.run":                {"protocol_requirement", "P", "run()", "func:swift:P:run()", "internal", boolRef(false)},
		"P.make":               {"protocol_requirement", "P", "make()", "func:swift:P:make()", "internal", boolRef(true)},
		"Service.extensionRun": {"function", "Service", "extensionRun()", "func:swift:Service:extensionRun()", "internal", boolRef(false)},
		"Outer.Inner.make":     {"function", "Outer.Inner", "make()", "func:swift:Outer.Inner:make()", "internal", boolRef(true)},
		"helper":               {"function", "", "helper()", "func:swift:__global__:helper()", "internal", boolRef(false)},
	}
	seen := map[string]int{}
	for _, sym := range p.Symbols {
		seen[sym.QualifiedName]++
		w, ok := want[sym.QualifiedName]
		if !ok {
			t.Errorf("unexpected symbol: %#v", sym)
			continue
		}
		duplicate := sym.QualifiedName == "Service.init" && sym.Signature == "init(id:)" || sym.QualifiedName == "Outer.Inner.run" && sym.Signature != "run()"
		if w.kind != sym.Kind || w.container != sym.ContainerName || !duplicate && w.signature != sym.Signature || !duplicate && w.key != "" && w.key != sym.StableKey || w.visibility != sym.Visibility || (sym.Static == nil) != (w.static == nil) || w.static != nil && *sym.Static != *w.static {
			t.Errorf("symbol=%#v want=%#v", sym, w)
		}
	}
	if seen["Outer.Inner.run"] != 3 {
		t.Fatalf("run overload count=%d, want 3", seen["Outer.Inner.run"])
	}
	var runSignatures = map[string]bool{}
	for _, sym := range p.Symbols {
		if sym.QualifiedName == "Outer.Inner.run" {
			runSignatures[sym.Signature] = true
		}
	}
	for _, signature := range []string{"run()", "run(_:)", "run(name:)"} {
		if !runSignatures[signature] {
			t.Errorf("missing overload signature %q", signature)
		}
	}
	if len(p.Symbols) != 17 {
		t.Fatalf("symbol count=%d, want 17", len(p.Symbols))
	}
	if len(p.Imports) != 2 || p.Imports[1] != "Foundation.URL" || len(p.Scope.Imports) != 2 || p.Scope.Imports[1].Kind != "struct" || p.Scope.Imports[1].SourceSpecifier != "Foundation" || p.Scope.Imports[1].ImportedName != "URL" {
		t.Fatalf("imports=%q scope=%#v", p.Imports, p.Scope.Imports)
	}
}

func TestSwiftCallsNormalizeAndSuppressLocalFunctions(t *testing.T) {
	const source = `func outer() {
    func inner() { localHelper() }
    let f = { closureHelper() }
    service.run()
    self.run()
    Self.run()
    super.run()
    value?.run()
    value!.run()
    foo.bar.run()
    helper(id: 1)
}
`
	p := mustSwiftParse(t, "Calls.swift", source)
	if len(p.Edges) != 9 {
		t.Fatalf("edge count=%d, want 9", len(p.Edges))
	}
	want := map[string]string{
		"service.run":   "swift:member;type_path_unproven",
		"self.run":      "swift:self",
		"Self.run":      "swift:Self",
		"super.run":     "swift:super",
		"value?.run":    "swift:optional_member",
		"value!.run":    "swift:forced_member",
		"foo.bar.run":   "swift:chained",
		"helper":        "swift:bare;labels=id:",
		"closureHelper": "swift:bare",
	}
	for _, edge := range p.Edges {
		if got := want[edge.DstName]; got != edge.Evidence {
			t.Errorf("edge=%#v want evidence=%q", edge, got)
		}
	}
	for _, ref := range p.References {
		if ref.Name == "" || ref.Name == ref.QualifiedName && ref.Name == "helper(id: 1)" {
			t.Errorf("reference retained raw call text: %#v", ref)
		}
	}
}

func TestSwiftProfileV6(t *testing.T) {
	if got := NewSwift().Profile(); got.ID != "treesitter:swift:v6" || !got.EmitsCallEdges {
		t.Fatalf("profile=%+v", got)
	}
}

func TestSwiftExtensionConformanceFacts(t *testing.T) {
	p := mustSwiftParse(t, "Extension.swift", `protocol P {}
protocol Q {}
class NotAProtocol {}
class Base {}
class Service {}
extension Service: P, Q {}
extension External.Service: ExternalProtocol {}
extension Service: NotAProtocol {}
extension Service { func helper<T>(_ value: T) {} }
class Outer { class Nested: P {} }
extension Outer {
    class NestedExtension: Base {}
}`)
	got := map[string]graph.SwiftInheritanceRelation{}
	for _, fact := range p.SwiftInheritanceRelations {
		got[fact.Child+"->"+fact.Target] = fact
	}
	for _, want := range []struct {
		key, relation        string
		constrained, generic bool
	}{
		{"Service->P", "conformance", false, false},
		{"Service->Q", "conformance", false, false},
		{"External.Service->ExternalProtocol", "conformance", false, false},
		{"Service->NotAProtocol", "unproven", false, false},
		{"Outer.NestedExtension->Base", "superclass", false, false},
	} {
		fact, ok := got[want.key]
		if !ok || fact.Relation != want.relation || fact.Constrained != want.constrained || fact.Generic != want.generic {
			t.Errorf("fact[%q]=%+v, want relation=%q generic=%v constrained=%v", want.key, fact, want.relation, want.generic, want.constrained)
		}
	}
	if _, ok := got["Service->Service"]; ok {
		t.Fatal("plain extension emitted a synthetic self relation")
	}
	if fact, ok := got["Outer.NestedExtension->Base"]; !ok || fact.Child == "Service" {
		t.Fatalf("nested extension relation=%+v", fact)
	}
}

func TestSwiftConstrainedExtensionConformanceFactExcludesBodyGenerics(t *testing.T) {
	p := mustSwiftParse(t, "GenericExtension.swift", `protocol P {}
protocol Q {}
struct Box<T> {}
extension Box: P where T: Q {
    func helper<U>(_ value: U) {}
    struct Nested<V> {}
}`)
	var found []graph.SwiftInheritanceRelation
	for _, fact := range p.SwiftInheritanceRelations {
		if fact.Child == "Box" && fact.Target == "P" {
			found = append(found, fact)
		}
	}
	if len(found) != 1 || !found[0].Constrained || found[0].Generic {
		t.Fatalf("Box conformance facts=%+v", found)
	}
}

func TestSwiftExtensionRecoveryAndNestedOwnershipFailClosed(t *testing.T) {
	generic := mustSwiftParse(t, "GenericExtensionRecovery.swift", `protocol P {}
class Service {}
extension Service: P<Int> {}`)
	var genericFacts []graph.SwiftInheritanceRelation
	for _, fact := range generic.SwiftInheritanceRelations {
		if fact.Child == "Service" && fact.Target == "P" {
			genericFacts = append(genericFacts, fact)
		}
	}
	if len(genericFacts) != 1 || genericFacts[0].Relation != "unproven" || !genericFacts[0].Generic || genericFacts[0].Constrained {
		t.Fatalf("generic recovery facts=%+v, want one unproven generic hazard", genericFacts)
	}

	p := mustSwiftParse(t, "ExtensionRecovery.swift", `protocol P {}
class Base {}
struct Box<T> {}
protocol Q {}
class Service {}
extension Box where T: Q {
    class Nested: Base {}
}
extension Service {
    class Nested: Base {}
}
extension Service: P {
    class NestedWithConformance: Base {}
}`)
	var ordinary, nestedWithConformance bool
	for _, fact := range p.SwiftInheritanceRelations {
		if fact.Child == "Nested" || fact.Child == "Box.Nested" || fact.Child == "Box" && fact.Target == "Base" {
			t.Fatalf("constrained extension leaked nested inheritance: %+v", fact)
		}
		ordinary = ordinary || fact.Child == "Service.Nested" && fact.Target == "Base" && fact.Relation == "superclass"
		nestedWithConformance = nestedWithConformance || fact.Child == "Service.NestedWithConformance" && fact.Target == "Base" && fact.Relation == "superclass"
	}
	if !ordinary || !nestedWithConformance {
		t.Fatalf("supported extension nested facts missing: ordinary=%v conformance=%v facts=%+v", ordinary, nestedWithConformance, p.SwiftInheritanceRelations)
	}
	var ordinaryConformance bool
	for _, fact := range p.SwiftInheritanceRelations {
		ordinaryConformance = ordinaryConformance || fact.Child == "Service" && fact.Target == "P" && !fact.Generic && fact.Relation == "conformance"
	}
	if !ordinaryConformance {
		t.Fatal("ordinary extension conformance missing")
	}
}

func TestSwiftExtensionDuplicateTargetProofFailsClosed(t *testing.T) {
	for _, source := range []string{
		"protocol P {}\nclass P {}\nclass Service {}\nextension Service: P {}\n",
		"class P {}\nprotocol P {}\nclass Service {}\nextension Service: P {}\n",
	} {
		p := mustSwiftParse(t, "Duplicate.swift", source)
		var facts []graph.SwiftInheritanceRelation
		for _, fact := range p.SwiftInheritanceRelations {
			if fact.Child == "Service" && fact.Target == "P" {
				facts = append(facts, fact)
			}
		}
		if len(facts) != 1 || facts[0].Relation != "unproven" || facts[0].Generic || facts[0].Constrained {
			t.Fatalf("duplicate target facts=%+v, want one unproven fact", facts)
		}
	}
}

func TestSwiftExtensionConformanceRange(t *testing.T) {
	p := mustSwiftParse(t, "ExtensionRange.swift", "protocol P {}\nclass Service {}\nextension Service: P {}\n")
	for _, fact := range p.SwiftInheritanceRelations {
		if fact.Child == "Service" && fact.Target == "P" {
			if fact.Range.StartLine != 3 || fact.Range.StartCol != 20 || fact.Range.EndLine != 3 || fact.Range.EndCol != 21 {
				t.Fatalf("range=%+v, want P token range", fact.Range)
			}
			return
		}
	}
	t.Fatal("ordinary extension conformance missing")
}

func TestSwiftClassInheritanceFactsV5(t *testing.T) {
	p := mustSwiftParse(t, "Facts.swift", `class Base {}
protocol P {}
class Child: Base, P {}
class ExternalChild: ExternalBase {}
final class Service {
    final override class func make() {}
    static func build() {}
    func run() {}
}`)
	got := map[string]string{}
	for _, f := range p.SwiftInheritanceRelations {
		got[f.Child+"->"+f.Target] = f.Relation
	}
	if got["Child->Base"] != "superclass" || got["Child->P"] != "conformance" || got["ExternalChild->ExternalBase"] != "unproven" {
		t.Fatalf("inheritance facts=%v", got)
	}
	byName := map[string]graph.SwiftDeclarationFact{}
	for _, f := range p.SwiftDeclarationFacts {
		byName[p.Symbols[f.SymbolIndex].QualifiedName] = f
	}
	if !byName["Service"].Final || !byName["Service.make"].Final || !byName["Service.make"].Override || byName["Service.make"].Dispatch != "class" || byName["Service.build"].Dispatch != "static" || byName["Service.run"].Dispatch != "instance" {
		t.Fatalf("declaration facts=%v", byName)
	}
}

func TestSwiftMalformedCallableDispatchFailsClosed(t *testing.T) {
	p := mustSwiftParse(t, "Malformed.swift", `final class Service {
    class static func broken() {}
}`)
	for _, fact := range p.SwiftDeclarationFacts {
		if p.Symbols[fact.SymbolIndex].QualifiedName == "Service.broken" {
			if fact.Dispatch != "" {
				t.Fatalf("malformed dispatch=%q, want empty", fact.Dispatch)
			}
			return
		}
	}
	t.Fatal("malformed callable fact missing")
}

func TestSwiftInheritanceFactsRespectNestedNominalOwnership(t *testing.T) {
	p := mustSwiftParse(t, "Nested.swift", `protocol P {}
class Base {}
class Outer {
    struct Inner: P {}
    enum NestedEnum: P { case value }
    actor NestedActor: P {}
    class NestedClass: Base {}
}`)
	for _, fact := range p.SwiftInheritanceRelations {
		if fact.Child == "Outer" {
			t.Fatalf("nested inheritance leaked to Outer: %#v", fact)
		}
		if fact.Child == "Outer.NestedClass" && (fact.Target != "Base" || fact.Relation != "superclass") {
			t.Fatalf("nested class fact=%#v", fact)
		}
	}
	if !slices.ContainsFunc(p.SwiftInheritanceRelations, func(f graph.SwiftInheritanceRelation) bool {
		return f.Child == "Outer.NestedClass" && f.Target == "Base"
	}) {
		t.Fatal("nested class inheritance fact missing")
	}
}

func TestSwiftInheritanceConstraintsIgnoreNestedBodies(t *testing.T) {
	p := mustSwiftParse(t, "Constraints.swift", `class Base {}
class Child: Base {
    func helper<T>(_ value: T) {}
    struct Box<T> {}
}`)
	for _, fact := range p.SwiftInheritanceRelations {
		if fact.Child != "Child" || fact.Target != "Base" || fact.Relation != "superclass" {
			continue
		}
		if fact.Constrained || fact.Generic {
			t.Fatalf("nested generic syntax contaminated Child: %#v", fact)
		}
		return
	}
	t.Fatal("Child inheritance fact missing")
}

func TestSwiftInheritanceConstraintBoundaries(t *testing.T) {
	p := mustSwiftParse(t, "Generic.swift", `protocol P {}
class Base<T> {}
class Simple: Base<Int> {}
class GenericChild<T>: Base<Int> {}
class WhereChild<T>: Base<Int> where T: P {}`)
	byChild := map[string]graph.SwiftInheritanceRelation{}
	for _, fact := range p.SwiftInheritanceRelations {
		byChild[fact.Child] = fact
	}
	if fact := byChild["Simple"]; fact.Target != "Base" || fact.Relation != "superclass" || !fact.Generic || fact.Constrained {
		t.Fatalf("generic superclass fact=%#v", fact)
	}
	if fact := byChild["GenericChild"]; fact.Target != "Base" || !fact.Generic || !fact.Constrained {
		t.Fatalf("child constraint fact=%#v", fact)
	}
	if fact := byChild["WhereChild"]; fact.Target != "Base" || !fact.Generic || !fact.Constrained {
		t.Fatalf("where constraint fact=%#v", fact)
	}
}

func TestSwiftV3CallShapesAndMemberValues(t *testing.T) {
	p := mustSwiftParse(t, "Facts.swift", `struct Service {
    let handler: () -> Void = {}
    var callback: () -> Void = {}
    var first: () -> Void = {}, second: () -> Void = {}
    static var third: () -> Void = {}, fourth: () -> Void = {}
    var computed: () -> Void { work() }
    lazy var lazyHandler: () -> Void = {}
    @available(*, unavailable, message: "class static") var annotated: () -> Void = {}

    func caller() {
        self.run()
        self.run() { work() }
        self.run(id: 1)
        self.run(id: 1) { work() }
        self.run { first() } completion: { second() }
        self.run(id: 1) { first() } completion: { second() }
    }
}
protocol P { var required: () -> Void { get } }
enum E { case build(Int), second(String); case result(value: Int, error: Error) }
class ClassService { class var classFactory: () -> Void { {} } }
@propertyWrapper struct Wrapper {
    var wrappedValue: () -> Void
    init(wrappedValue: @escaping () -> Void) { self.wrappedValue = wrappedValue }
}
struct WrappedService { @Wrapper var wrapped: () -> Void = {} }
extension Service { var extensionHandler: () -> Void { {} }; var extensionFirst: () -> Void { {} }; var extensionSecond: () -> Void { {} } }
struct Contamination {
    var handler: () -> Void = makeHandler(named: "other")
    var computed: () -> Void { helper() }
}
struct TupleService { var (tupleFirst, tupleSecond) = (1, 2) }
func work() {}
func first() {}
func second() {}
`)
	want := map[string]string{
		"self.run|swift:self":                                          "0",
		"self.run|swift:self;trailing_labels=_":                        "1",
		"self.run|swift:self;labels=id:":                               "1",
		"self.run|swift:self;labels=id:;trailing_labels=_":             "2",
		"self.run|swift:self;trailing_labels=_,completion:":            "2",
		"self.run|swift:self;labels=id:;trailing_labels=_,completion:": "3",
	}
	got := map[string]int{}
	for _, edge := range p.Edges {
		if edge.DstName != "self.run" {
			continue
		}
		if edge.CallArity == nil {
			t.Fatalf("missing arity for %#v", edge)
		}
		got[edge.DstName+"|"+edge.Evidence]++
		if want[edge.DstName+"|"+edge.Evidence] != fmt.Sprint(*edge.CallArity) {
			t.Errorf("edge=%#v", edge)
		}
	}
	if len(got) != len(want) {
		t.Fatalf("call shapes=%v, want=%v", got, want)
	}
	for key, wantArity := range want {
		if got[key] != 1 {
			t.Errorf("%s count=%d, want 1", key, got[key])
		}
		_ = wantArity
	}

	facts := map[string]graph.ScopeImport{}
	for _, sym := range p.Symbols {
		if sym.Name == "handler" || sym.Name == "callback" || sym.Name == "third" || sym.Name == "fourth" || sym.Name == "classFactory" || sym.Name == "build" {
			t.Errorf("member value/case became symbol: %#v", sym)
		}
	}
	for _, fact := range p.Scope.Imports {
		if fact.Kind == graph.ScopeImportSwiftMemberValue || fact.Kind == graph.ScopeImportSwiftEnumCase {
			facts[fact.Kind+"|"+fact.OwnerModule+"|"+fact.LocalName] = fact
		}
	}
	for _, key := range []string{
		"swift_member_value|Service|handler",
		"swift_member_value|Service|callback",
		"swift_member_value|Service|first",
		"swift_member_value|Service|second",
		"swift_member_value|Service|computed",
		"swift_member_value|Service|lazyHandler",
		"swift_member_value|Service|annotated",
		"swift_member_value|ClassService|classFactory",
		"swift_member_value|WrappedService|wrapped",
		"swift_member_value|P|required",
		"swift_member_value|Service|extensionHandler",
		"swift_member_value|Service|extensionFirst",
		"swift_member_value|Service|extensionSecond",
		"swift_member_value|Contamination|handler",
		"swift_member_value|Contamination|computed",
		"swift_member_value|TupleService|tupleFirst",
		"swift_member_value|TupleService|tupleSecond",
		"swift_enum_case|E|build",
		"swift_enum_case|E|second",
		"swift_enum_case|E|result",
	} {
		if _, ok := facts[key]; !ok {
			t.Errorf("missing scope fact %s", key)
		}
	}
	for _, key := range []string{
		"swift_member_value|Service|handler",
		"swift_member_value|Service|callback",
		"swift_member_value|Service|first",
		"swift_member_value|Service|second",
		"swift_member_value|Service|computed",
		"swift_member_value|Service|lazyHandler",
		"swift_member_value|Service|annotated",
		"swift_member_value|P|required",
		"swift_member_value|Service|extensionHandler",
		"swift_member_value|Service|extensionFirst",
		"swift_member_value|Service|extensionSecond",
		"swift_member_value|Contamination|handler",
		"swift_member_value|Contamination|computed",
		"swift_member_value|TupleService|tupleFirst",
		"swift_member_value|TupleService|tupleSecond",
	} {
		if facts[key].Static {
			t.Errorf("%s marked static", key)
		}
	}
	for _, key := range []string{"swift_member_value|ClassService|classFactory", "swift_member_value|Service|third", "swift_member_value|Service|fourth", "swift_enum_case|E|build", "swift_enum_case|E|second", "swift_enum_case|E|result"} {
		if !facts[key].Static {
			t.Errorf("%s not marked static", key)
		}
	}
	for _, name := range []string{"makeHandler", "named", "other", "helper", "value", "error", "Int", "Error"} {
		for key := range facts {
			if strings.HasSuffix(key, "|"+name) {
				t.Errorf("contaminating fact %s", key)
			}
		}
	}
	refs := 0
	for _, ref := range p.References {
		if ref.QualifiedName == "self.run" {
			refs++
			if ref.Name != "run" {
				t.Errorf("reference=%#v", ref)
			}
		}
	}
	if refs != len(want) {
		t.Fatalf("self.run references=%d, want %d", refs, len(want))
	}
}

func TestSwiftV3NestedTrailingClosureDoesNotContaminateOuterCall(t *testing.T) {
	p := mustSwiftParse(t, "Nested.swift", `struct Service {
    func caller() {
        self.run { helper(id: 1) }
    }
}
`)
	outer, inner := false, false
	for _, edge := range p.Edges {
		if edge.DstName == "self.run" {
			outer = true
			if edge.Evidence != "swift:self;trailing_labels=_" || edge.CallArity == nil || *edge.CallArity != 1 {
				t.Fatalf("outer edge=%#v", edge)
			}
		}
		if edge.DstName == "helper" {
			inner = true
			if edge.Evidence != "swift:bare;labels=id:" || edge.CallArity == nil || *edge.CallArity != 1 {
				t.Fatalf("inner edge=%#v", edge)
			}
		}
	}
	if !outer || !inner {
		t.Fatalf("outer=%v inner=%v edges=%#v", outer, inner, p.Edges)
	}
}

func TestSwiftConstructorAndTypeReceiverEvidence(t *testing.T) {
	p := mustSwiftParse(t, "Calls.swift", `struct Service {}
struct Box<T> {}
func caller() {
    Service()
    Box<Int>()
    Service.run()
    service.run()
}

`)
	want := map[string]string{
		"Service":     "swift:initializer",
		"Box":         "swift:initializer;generic_specialization=true",
		"Service.run": "swift:member;type_path_unproven",
		"service.run": "swift:member;type_path_unproven",
	}
	if len(p.Edges) != len(want) {
		t.Fatalf("edges=%#v", p.Edges)
	}
	for _, edge := range p.Edges {
		if got := want[edge.DstName]; got != edge.Evidence {
			t.Errorf("edge=%#v want evidence=%q", edge, got)
		}
	}
}

func TestSwiftV4InitializerFactsFailClosedOnLexicalBindings(t *testing.T) {
	p := mustSwiftParse(t, "Facts.swift", `
struct Service { init() {} }
struct Outer { struct Inner { init() {}; func make() { Inner(); Outer.Inner() } } }
struct Box<T> {}
typealias BoxAlias<T> = Box<T>
protocol Proto {}
class ClassService {}
enum Event { case value(Int) }
func clear() { Service() }
func shadow(_ Service: () -> Int) {
    let Local = { 1 }
    func Service() -> Int { return 1 }
    struct Local { init() {} }
    typealias Alias = Service
    Service()
    Local()
    Alias()
    { Service in Service() }(1)
}

func generic<T>() { T() }
func other() { BoxAlias<Int>(); Proto(); ClassService(); Event.value(1); Swift.Array<Int>() }
`)
	var byName = map[string][]graph.Edge{}
	for _, edge := range p.Edges {
		byName[edge.DstName] = append(byName[edge.DstName], edge)
	}
	if got := byName["Service"]; len(got) != 3 || got[0].Evidence != "swift:initializer" || got[1].Evidence != "swift:bare" || got[2].Evidence != "swift:bare" {
		t.Fatalf("Service facts=%#v", got)
	}
	for _, want := range []struct{ name, evidence string }{
		{"Outer.Inner", "swift:initializer"},
		{"BoxAlias", "swift:initializer_unproven;generic_specialization=true"},
		{"Proto", "swift:bare"},
		{"ClassService", "swift:initializer"},
		{"Swift.Array", "swift:initializer_unproven;generic_specialization=true"},
	} {
		edges := byName[want.name]
		if len(edges) != 1 || edges[0].Evidence != want.evidence {
			t.Fatalf("%s facts=%#v want %q", want.name, edges, want.evidence)
		}
	}
	if got := byName["Inner"]; len(got) != 1 || got[0].Evidence == "swift:initializer" {
		t.Fatalf("short nested path=%#v", got)
	}
	for _, name := range []string{"Local", "Alias"} {
		for _, edge := range byName[name] {
			if edge.Evidence == "swift:initializer" {
				t.Fatalf("shadowed %s became initializer: %#v", name, edge)
			}
		}
	}
	wantFacts := map[string][]string{
		"Service|parameter":      {"10|19", "18|18"},
		"Local|value":            {"10|19"},
		"Service|local_function": {"10|19"},
		"Local|local_nominal":    {"10|19"},
		"Alias|typealias":        {"10|19"},
		"T|generic_parameter":    {"4|4", "5|5", "21|21"},
	}
	seen := map[string]map[string]bool{}
	for _, fact := range p.Scope.SwiftLexicalBindings {
		key := fact.Name + "|" + fact.Kind
		if wants, ok := wantFacts[key]; ok {
			got := fmt.Sprintf("%d|%d", fact.ScopeStartLine, fact.ScopeEndLine)
			if seen[key] == nil {
				seen[key] = map[string]bool{}
			}
			if !slices.Contains(wants, got) {
				t.Errorf("fact %s range=%s want one of %v", key, got, wants)
			} else {
				seen[key][got] = true
			}
		}
	}
	for key, wants := range wantFacts {
		for _, want := range wants {
			if !seen[key][want] {
				t.Errorf("missing lexical fact %s range=%s", key, want)
			}
		}
	}
}

func TestSwiftV4QualifiedRootAndPatternShadowing(t *testing.T) {
	p := mustSwiftParse(t, "Shadow.swift", `
struct Outer { struct Inner { init() {} } }
struct OtherOuter { struct Inner { init() {} } }
struct Service { init() {} }
let maybe: (() -> Int)? = { 1 }
let factories: [() -> Int] = [{ 1 }]
func clear() { Outer.Inner() }
func outside() { Service() }
func shadowed() {
    typealias Outer = OtherOuter
    Outer.Inner()
}
func unrelated() {
    let Inner = { 1 }
    Outer.Inner()
}
func patterns() {
    if let Service = maybe { Service() }
    guard let Service = maybe else { return }
    Service()
    for Service in factories { Service() }
    while let Service = maybe { Service() }
    if case let Service? = maybe { Service() }
    switch maybe {
    case let Service?: Service()
    default: break
    }
    do { throw ServiceError() } catch let Service { _ = Service }
}
struct ServiceError: Error {}
`)
	byName := map[string][]graph.Edge{}
	for _, edge := range p.Edges {
		byName[edge.DstName] = append(byName[edge.DstName], edge)
	}
	outer := byName["Outer.Inner"]
	positive, blocked := 0, 0
	for _, edge := range outer {
		if edge.Evidence == "swift:initializer" {
			positive++
		} else {
			blocked++
		}
	}
	if len(outer) != 3 || positive != 2 || blocked != 1 {
		t.Fatalf("qualified facts=%#v", outer)
	}
	serviceInitializers := 0
	for _, edge := range byName["Service"] {
		if edge.Evidence == "swift:initializer" {
			serviceInitializers++
		}
	}
	if serviceInitializers != 1 {
		t.Fatalf("pattern-bound Service facts=%#v", byName["Service"])
	}
	facts := map[string]bool{}
	for _, fact := range p.Scope.SwiftLexicalBindings {
		if fact.Kind == graph.SwiftLexicalValue {
			facts[fmt.Sprintf("%s|%d|%d", fact.Name, fact.ScopeStartLine, fact.ScopeEndLine)] = true
		}
	}
	if !slices.ContainsFunc([]string{"Service|16|16", "Service|17|17", "Service|20|20", "Service|21|21", "Service|23|23", "Service|26|26"}, func(want string) bool { return facts[want] }) {
		t.Fatalf("missing pattern binding facts: %#v", facts)
	}
}

func TestSwiftV4NestedLambdaAndTypealiasGenericRanges(t *testing.T) {
	p := mustSwiftParse(t, "Ranges.swift", `
struct Service { init() {} }
struct T { init() {} }
typealias Alias<T> = Service
func f() {
    let outer = {
        let inner = { Service in Service() }
        Service()
    }
    T()
}
`)
	serviceEdges := 0
	serviceInitializers := 0
	for _, edge := range p.Edges {
		if edge.DstName == "Service" {
			serviceEdges++
			if edge.Evidence == "swift:initializer" {
				serviceInitializers++
			}
		}
	}
	if serviceEdges != 2 || serviceInitializers != 1 {
		t.Fatalf("nested lambda facts edges=%#v bindings=%#v", p.Edges, p.Scope.SwiftLexicalBindings)
	}
	for _, fact := range p.Scope.SwiftLexicalBindings {
		if fact.Name == "T" && fact.Kind == graph.SwiftLexicalGenericParameter && (fact.ScopeStartLine != 4 || fact.ScopeEndLine != 4) {
			t.Fatalf("typealias generic range=%#v", fact)
		}
	}
}

func TestSwiftNominalKinds(t *testing.T) {
	p := mustSwiftParse(t, "Kinds.swift", `class C {}
struct S {}
enum E {}
protocol P {}
actor A {}
`)
	want := map[string]string{"C": "class", "S": "struct", "E": "enum", "P": "protocol", "A": "actor"}
	if len(p.Symbols) != len(want) {
		t.Fatalf("symbols=%#v", p.Symbols)
	}
	for _, sym := range p.Symbols {
		if sym.Kind != want[sym.Name] {
			t.Errorf("symbol=%#v", sym)
		}
	}
}

func TestSwiftProtocolRequirementVisibility(t *testing.T) {
	p := mustSwiftParse(t, "Visibility.swift", `public protocol PublicP { func run(); static func make() }
private protocol PrivateP { func run() }
fileprivate protocol FilePrivateP { func run() }
internal protocol InternalP { func run() }
package protocol PackageP { func run() }
public struct S { func run() {} }
`)
	want := map[string]string{
		"PublicP.run": "public", "PublicP.make": "public",
		"PrivateP.run": "private", "FilePrivateP.run": "fileprivate",
		"InternalP.run": "internal", "PackageP.run": "package",
		"S.run": "internal",
	}
	seen := make(map[string]bool, len(want))
	for _, sym := range p.Symbols {
		if want, ok := want[sym.QualifiedName]; ok && sym.Visibility != want {
			t.Errorf("%s visibility=%q want %q", sym.QualifiedName, sym.Visibility, want)
		}
		if _, ok := want[sym.QualifiedName]; ok {
			seen[sym.QualifiedName] = true
		}
	}
	for name := range want {
		if !seen[name] {
			t.Errorf("missing visibility symbol %s", name)
		}
	}
}

func boolRef(v bool) *bool { return &v }

func mustSwiftParse(t *testing.T, path, source string) (p graph.ParsedFile) {
	t.Helper()
	var err error
	p, err = NewSwift().Parse(context.Background(), path, []byte(source))
	if err != nil {
		t.Fatal(err)
	}
	return p
}
