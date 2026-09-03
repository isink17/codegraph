//go:build cgo

package treesitter

import (
	"context"
	"fmt"
	"testing"
)

func TestTypedScopeEvidenceFixtures(t *testing.T) {
	tests := []struct {
		name, path, src   string
		wantPackage, want string
		parse             func(context.Context, string, []byte) ([]string, string, error)
	}{
		{"java", "A.java", "package a.b; import static x.Util.run; import x.Foo.*;", "a.b", "run", func(c context.Context, p string, b []byte) ([]string, string, error) {
			x, e := NewJava().Parse(c, p, b)
			if e != nil {
				return nil, "", e
			}
			var v []string
			for _, i := range x.Scope.Imports {
				v = append(v, i.LocalName+":"+i.Kind)
			}
			return v, x.Scope.Package, nil
		}},
		{"kotlin", "A.kt", "package a.b\nimport x.Foo as Bar\nimport x.y.*", "a.b", "Bar", func(c context.Context, p string, b []byte) ([]string, string, error) {
			x, e := NewKotlin().Parse(c, p, b)
			if e != nil {
				return nil, "", e
			}
			var v []string
			for _, i := range x.Scope.Imports {
				if i.LocalName == "Bar" && i.ImportedName != "Foo" {
					return nil, "", fmt.Errorf("alias imported name = %q", i.ImportedName)
				}
				v = append(v, i.LocalName)
			}
			return v, x.Scope.Package, nil
		}},
		{"rust", "lib.rs", "use crate::a::{f, g as h, *}; pub use self::x::y;", "", "h", func(c context.Context, p string, b []byte) ([]string, string, error) {
			x, e := NewRust().Parse(c, p, b)
			if e != nil {
				return nil, "", e
			}
			var v []string
			for _, i := range x.Scope.Imports {
				v = append(v, i.LocalName)
			}
			return v, x.Scope.Package, nil
		}},
		{"typescript", "a.ts", `import {foo as bar} from "./m"; import def from "./d"; import * as ns from "./n"; import "./side"; export {foo as pub} from "./m"; export * as api from "./n";`, "", "bar", func(c context.Context, p string, b []byte) ([]string, string, error) {
			x, e := NewTypeScript().Parse(c, p, b)
			if e != nil {
				return nil, "", e
			}
			var v []string
			for _, i := range x.Scope.Imports {
				v = append(v, i.LocalName)
			}
			return v, x.Scope.Package, nil
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v, p, e := tt.parse(context.Background(), tt.path, []byte(tt.src))
			if e != nil {
				t.Fatal(e)
			}
			if p != tt.wantPackage {
				t.Fatalf("package=%q", p)
			}
			found := false
			for _, x := range v {
				if x == tt.want || len(x) >= len(tt.want) && x[:len(tt.want)] == tt.want {
					found = true
				}
			}
			if !found {
				t.Fatalf("evidence=%v want %q", v, tt.want)
			}
		})
	}
}

func TestRustScopeEvidenceKeepsInlineOwnersAndAliasTargets(t *testing.T) {
	p, err := NewRust().Parse(context.Background(), "lib.rs", []byte(`
mod outer {
    use crate::a::f as local_f;
    mod inner { use crate::b::f; fn run() { f(); } }
}
`))
	if err != nil {
		t.Fatal(err)
	}
	if p.Scope.ModulePath != "crate" {
		t.Fatalf("module path = %q", p.Scope.ModulePath)
	}
	var owners, imported string
	for _, imp := range p.Scope.Imports {
		if imp.LocalName == "local_f" {
			owners, imported = imp.OwnerModule, imp.ImportedName
		}
	}
	if owners != "crate::outer" || imported != "f" {
		t.Fatalf("alias evidence = (%q, %q)", owners, imported)
	}
	for _, imp := range p.Scope.Imports {
		if imp.LocalName == "f" && imp.OwnerModule != "crate::outer::inner" {
			t.Fatalf("nested owner = %q", imp.OwnerModule)
		}
	}
}

func TestRustPubUseIsReexportEvidence(t *testing.T) {
	p, err := NewRust().Parse(context.Background(), "lib.rs", []byte("pub use crate::a::f as g;"))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Scope.Imports) != 1 || !p.Scope.Imports[0].ReExport {
		t.Fatalf("imports=%+v", p.Scope.Imports)
	}
}

func TestKotlinPackageOwnsDeclarationIdentity(t *testing.T) {
	p, err := NewKotlin().Parse(context.Background(), "Helper.kt", []byte("package a.b\nfun helper() {}"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Scope.Package != "a.b" || len(p.Symbols) != 1 || p.Symbols[0].QualifiedName != "a.b.helper" {
		t.Fatalf("package identity = package %q symbols %+v", p.Scope.Package, p.Symbols)
	}
	p, err = NewKotlin().Parse(context.Background(), "Default.kt", []byte("fun helper() {}"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Scope.Package != "" || len(p.Symbols) != 1 || p.Symbols[0].QualifiedName != "helper" {
		t.Fatalf("default package identity = package %q symbols %+v", p.Scope.Package, p.Symbols)
	}
}
