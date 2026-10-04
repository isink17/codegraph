//go:build cgo

package treesitter

import (
	"context"
	"testing"
)

func TestCSharpV2NamespaceIdentity(t *testing.T) {
	p, err := NewCSharp().Parse(context.Background(), "Service.cs", []byte(`using App.Shared;
namespace App.Core;
public class Outer { public class Inner { public static void Run(int x) {} public static void Run(string x) {} } }`))
	if err != nil {
		t.Fatal(err)
	}
	if p.Scope.Package != "App.Core" {
		t.Fatalf("package = %q", p.Scope.Package)
	}
	want := map[string]string{"Outer": "App.Core.Outer", "Inner": "App.Core.Outer.Inner", "Run": "App.Core.Outer.Inner.Run"}
	seen := map[string]int{}
	keys := map[string]bool{}
	for _, s := range p.Symbols {
		if q, ok := want[s.Name]; ok && s.QualifiedName != q {
			t.Errorf("%s qname = %q, want %q", s.Name, s.QualifiedName, q)
		}
		if s.Name == "Run" {
			seen[s.Signature]++
			keys[s.StableKey] = true
		}
	}
	if len(seen) != 2 {
		t.Fatalf("overload signatures = %#v", seen)
	}
	if len(keys) != 2 {
		t.Fatalf("overload stable keys = %#v", keys)
	}
	imports := 0
	for _, i := range p.Scope.Imports {
		if i.Kind == "namespace" {
			imports++
			if i.SourceSpecifier != "App.Shared" {
				t.Errorf("import = %#v", i)
			}
		}
	}
	if imports != 1 {
		t.Fatalf("scope imports = %#v", p.Scope.Imports)
	}
}

func TestCSharpV4ArityFacts(t *testing.T) {
	p, err := NewCSharp().Parse(context.Background(), "Arity.cs", []byte(`class C {
 void Run() {}
 void Run(int x = 1) {}
 void Pack(int x, params int[] values) {}
 void Ref(ref int x) {}
	 void F(int x) { Run(); Run(x, Foo(1, 2)); Pack(x, x); Ref(ref x); Run<int>(x); }
 int Foo(int x, int y) { return x + y; }
}`))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string][][2]int{}
	for _, symbol := range p.Symbols {
		if symbol.Name != "Run" && symbol.Name != "Pack" {
			continue
		}
		if symbol.ArityMin == nil || symbol.ArityMax == nil {
			t.Fatalf("%s arity unknown", symbol.Signature)
		}
		got[symbol.Name] = append(got[symbol.Name], [2]int{*symbol.ArityMin, *symbol.ArityMax})
	}
	if len(got["Run"]) != 2 || !containsArity(got["Run"], [2]int{0, 0}) || !containsArity(got["Run"], [2]int{0, 1}) {
		t.Fatalf("Run arity = %v, want [0 0] and [0 1]", got["Run"])
	}
	if len(got["Pack"]) != 1 || got["Pack"][0] != [2]int{1, -1} {
		t.Fatalf("Pack arity = %v, want [1 -1]", got["Pack"])
	}
	for _, edge := range p.Edges {
		if edge.DstName == "Run" && edge.Line == 5 && edge.CallArity == nil {
			t.Fatal("simple Run() call missing arity")
		}
		if edge.DstName == "Ref" && edge.CallArity != nil {
			t.Fatal("ref call must not carry narrowing arity")
		}
		if edge.DstName == "Run<int>" && edge.CallArity != nil {
			t.Fatal("explicit generic call must not carry narrowing arity")
		}
	}
}

func containsArity(values [][2]int, want [2]int) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func TestCSharpV2CallSpellingAndNegativeBindings(t *testing.T) {
	p, err := NewCSharp().Parse(context.Background(), "Caller.cs", []byte(`namespace App.Core;
class Caller { void F(Service service, int value) { Run(); this.Run(); service.Run(); Service.Run(); App.Core.Service.Run(); } void Run() {} }`))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, e := range p.Edges {
		got[e.DstName] = true
	}
	for _, want := range []string{"Run", "this.Run", "service.Run", "Service.Run", "App.Core.Service.Run"} {
		if !got[want] {
			t.Errorf("missing call %q in %#v", want, got)
		}
	}
	local := map[string]bool{}
	typed := map[string]string{}
	for _, i := range p.Scope.Imports {
		if i.Kind == "local_binding" {
			local[i.LocalName] = true
		}
		if i.Kind == "typed_binding" {
			typed[i.LocalName] = i.SourceSpecifier
		}
	}
	if typed["service"] != "Service" || local["value"] {
		t.Fatalf("bindings local=%v typed=%v", local, typed)
	}
}

func TestCSharpV2NamespaceFormsAndUsingKinds(t *testing.T) {
	p, err := NewCSharp().Parse(context.Background(), "Nested.cs", []byte(`namespace App { namespace Core { class Outer { class Inner { void Run() {} } } }`))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"Outer": "App.Core.Outer", "Inner": "App.Core.Outer.Inner", "Run": "App.Core.Outer.Inner.Run"}
	for _, s := range p.Symbols {
		if q, ok := want[s.Name]; ok && s.QualifiedName != q {
			t.Errorf("%s qname = %q, want %q", s.Name, s.QualifiedName, q)
		}
	}
	if p.Scope.Package != "App.Core" {
		t.Fatalf("nested package = %q symbols=%#v", p.Scope.Package, p.Symbols)
	}
	static, err := NewCSharp().Parse(context.Background(), "Static.cs", []byte(`using static App.Core.Service;`))
	if err != nil || len(static.Scope.Imports) != 1 || static.Scope.Imports[0].Kind != "static" {
		t.Fatalf("static using = %#v err=%v", static.Scope.Imports, err)
	}
	g, err := NewCSharp().Parse(context.Background(), "Global.cs", []byte(`global using G = App.Global.Service;`))
	if err != nil || len(g.Scope.Imports) != 1 || g.Scope.Imports[0].Kind != "global_alias" {
		t.Fatalf("global using = %#v err=%v", g.Scope.Imports, err)
	}
}

func TestCSharpV2LocalFunctionAndCatchBindings(t *testing.T) {
	p, err := NewCSharp().Parse(context.Background(), "Bindings.cs", []byte(`class Service {
 void Run() {}
 void F() { void Run() {} try {} catch (System.Exception error) { Run(); } }
}`))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, i := range p.Scope.Imports {
		if i.Kind == "local_binding" {
			got[i.LocalName] = true
		}
	}
	if !got["Run"] {
		t.Fatalf("missing local function binding: %#v", p.Scope.Imports)
	}
	if got["error"] {
		t.Fatalf("typed catch binding became unknown: %#v", p.Scope.Imports)
	}
}

func TestCSharpV3TypedBindingEvidence(t *testing.T) {
	// AST shape pin lives in assertions below; parser nodes are intentionally
	// inspected through the adapter's field accessors.
	p, err := NewCSharp().Parse(context.Background(), "Bindings.cs", []byte(`using S = App.Core.Service;
namespace App.Core;
class Caller {
 Service field;
 Service Property { get; }
 void F(Service parameter, S alias, global::App.Core.Service globalName) {
  Service local;
  var created = new Service();
  var qualified = new App.Core.Service();
  var unknown = GetService();
  foreach (Service item in values) { item.Run(); }
  parameter.Run(); alias.Run(); globalName.Run(); local.Run(); created.Run(); qualified.Run(); unknown.Run();
  try {} catch (System.Exception ex) { ex.Handle(); }
 }
}`))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	unknown := map[string]bool{}
	for _, i := range p.Scope.Imports {
		if i.Kind == "typed_binding" {
			got[i.LocalName] = i.SourceSpecifier
		}
		if i.Kind == "local_binding" {
			unknown[i.LocalName] = true
		}
	}
	for name, want := range map[string]string{"parameter": "Service", "alias": "S", "globalName": "global::App.Core.Service", "local": "Service", "created": "Service", "qualified": "App.Core.Service", "item": "Service", "ex": "System.Exception"} {
		if got[name] != want {
			t.Errorf("typed %s=%q, want %q; imports=%#v", name, got[name], want, p.Scope.Imports)
		}
	}
	if !unknown["unknown"] {
		t.Errorf("unknown var lost negative evidence: %#v", p.Scope.Imports)
	}
	if _, ok := got["unknown"]; ok {
		t.Errorf("method-call var inferred: %#v", p.Scope.Imports)
	}
}

func TestCSharpV2TypeVisibility(t *testing.T) {
	p, err := NewCSharp().Parse(context.Background(), `Hidden.cs`, []byte(`class Outer {
 public class PublicType {}
 private class Hidden {}
 protected class ProtectedType {}
 internal class InternalType {}
}`))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, s := range p.Symbols {
		got[s.Name] = s.Visibility
	}
	want := map[string]string{"Outer": "module", "PublicType": "public", "Hidden": "private", "ProtectedType": "protected", "InternalType": "module"}
	for name, visibility := range want {
		if got[name] != visibility {
			t.Errorf("%s visibility = %q, want %q", name, got[name], visibility)
		}
	}
}

func TestCSharpV2BlockNamespacePackageIsNotFileWide(t *testing.T) {
	for name, source := range map[string]string{
		"mixed": `class GlobalCaller {}
namespace App.Core { class NamespacedCaller {} }`,
		"multiple": `namespace A { class CallerA {} }
namespace B { class CallerB {} }`,
	} {
		p, err := NewCSharp().Parse(context.Background(), name+".cs", []byte(source))
		if err != nil {
			t.Fatal(err)
		}
		if p.Scope.Package != "" {
			t.Fatalf("%s package = %q, want empty", name, p.Scope.Package)
		}
	}
}

// C# using-directive evidence comes from the declaration's tokens. Spec: C#
// 12 §14.5 using_directive (using_alias_directive, using_namespace_directive,
// using_static_directive); `global` and `static` are keyword tokens, never a
// prefix of the name that follows. No C# compiler was available.
func TestCSharpUsingEvidenceFromSyntax(t *testing.T) {
	for _, c := range []struct {
		decl, source, local, kind string
		static                    bool
		raw                       bool // untrusted: raw spelling, kind named
	}{
		{"using staticns;", "staticns", "staticns", "namespace", false, false},
		{"using globalns;", "globalns", "globalns", "namespace", false, false},
		{"using static staticns.Util;", "staticns.Util", "Util", "static", true, false},
		{"using static\tns.Util;", "ns.Util", "Util", "static", true, false},
		{"using static\nns.Util;", "ns.Util", "Util", "static", true, false},
		{"using /*c*/ static /*d*/ ns . /*e*/ Util;", "ns.Util", "Util", "static", true, false},
		{"global using staticns;", "staticns", "staticns", "global_namespace", false, false},
		{"global\tusing\tstatic\tns.Util;", "ns.Util", "Util", "global_static", true, false},
		{"using A = staticns.Util;", "staticns.Util", "A", "alias", false, false},
		{"global using A = ns.Util;", "ns.Util", "A", "global_alias", false, false},
		{"using ns\t.\tUtil;", "ns.Util", "Util", "namespace", false, false},
		{"using a.;", "using a.;", "", "named", false, true},
		{"using static;", "using static;", "", "named", false, true},
		{"using ns.*;", "using ns.*;", "", "named", false, true},
		{"using unsafe ns.Util;", "using unsafe ns.Util;", "", "named", false, true},
		{"using static A = ns.Util;", "using static A = ns.Util;", "", "named", false, true},
	} {
		p, err := NewCSharp().Parse(context.Background(), "C.cs", []byte(c.decl+"\nclass C {}\n"))
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Scope.Imports) != 1 {
			t.Fatalf("%q: imports = %+v", c.decl, p.Scope.Imports)
		}
		got := p.Scope.Imports[0]
		if got.SourceSpecifier != c.source || got.LocalName != c.local || got.Kind != c.kind || got.Static != c.static {
			t.Errorf("%q: import = %+v, want source %q local %q kind %q static %v", c.decl, got, c.source, c.local, c.kind, c.static)
		}
		if c.raw && len(p.Imports) != 0 {
			t.Errorf("%q: untrusted import leaked into Imports %v", c.decl, p.Imports)
		}
	}
	// The v4 text rule survives only in the legacy adapter.
	p, err := NewCSharpV4().Parse(context.Background(), "C.cs", []byte("using staticns;\nclass C {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Scope.Imports) != 1 || p.Scope.Imports[0].SourceSpecifier != "ns" {
		t.Fatalf("v4 imports = %+v, want the legacy ns", p.Scope.Imports)
	}
}
