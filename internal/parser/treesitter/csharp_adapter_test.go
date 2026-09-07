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
