//go:build cgo

package treesitter

import (
	"context"
	"fmt"
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

func TestSwiftProfileV3(t *testing.T) {
	if got := NewSwift().Profile(); got.ID != "treesitter:swift:v3" || !got.EmitsCallEdges {
		t.Fatalf("profile=%+v", got)
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
		"Service":     "swift:bare",
		"Box":         "swift:initializer",
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
