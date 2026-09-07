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
	for _, i := range p.Scope.Imports {
		if i.Kind == "local_binding" {
			local[i.LocalName] = true
		}
	}
	for _, want := range []string{"service", "value"} {
		if !local[want] {
			t.Errorf("missing local binding %q", want)
		}
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
	for _, name := range []string{"Run", "error"} {
		if !got[name] {
			t.Fatalf("missing local binding %q: %#v", name, p.Scope.Imports)
		}
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
