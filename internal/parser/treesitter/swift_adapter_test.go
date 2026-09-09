//go:build cgo

package treesitter

import (
	"context"
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

func TestSwiftProfileV2(t *testing.T) {
	if got := NewSwift().Profile(); got.ID != "treesitter:swift:v2" || !got.EmitsCallEdges {
		t.Fatalf("profile=%+v", got)
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
