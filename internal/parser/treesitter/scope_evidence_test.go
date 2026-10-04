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

func TestRustNestedUseTree(t *testing.T) {
	p, err := NewRust().Parse(context.Background(), "lib.rs", []byte(`mod caller { pub use crate /* prefix */ :: {a::{self, f /* alias */ as renamed, nested::{g, *}}, b :: h}; }`))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"crate::a|a|a|false", "crate::a::f|f|renamed|false", "crate::a::nested::g|g|g|false", "crate::a::nested|nested||true", "crate::b::h|h|h|false"}
	if len(p.Scope.Imports) != len(want) {
		t.Fatalf("imports = %+v", p.Scope.Imports)
	}
	for i, imp := range p.Scope.Imports {
		got := fmt.Sprintf("%s|%s|%s|%t", imp.SourceSpecifier, imp.ImportedName, imp.LocalName, imp.Wildcard)
		if got != want[i] || !imp.ReExport || imp.OwnerModule != "crate::caller" {
			t.Fatalf("import %d = %+v, want %s", i, imp, want[i])
		}
	}
}

func TestRustUseTreeRelativePaths(t *testing.T) {
	p, err := NewRust().Parse(context.Background(), "lib.rs", []byte(`mod caller {
use self::{local::{helper as local_helper}};
use super::{a::{helper as sibling}};
use super::super::{root::{helper as root_helper}};
use {crate::a::{helper as global}, self::local::{helper as nested}};
}`))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"self::local::helper|local_helper",
		"super::a::helper|sibling",
		"super::super::root::helper|root_helper",
		"crate::a::helper|global",
		"self::local::helper|nested",
	}
	if len(p.Scope.Imports) != len(want) {
		t.Fatalf("imports = %+v", p.Scope.Imports)
	}
	for i, imp := range p.Scope.Imports {
		if got := imp.SourceSpecifier + "|" + imp.LocalName; got != want[i] || imp.ImportedName != "helper" || imp.OwnerModule != "crate::caller" {
			t.Fatalf("import %d = %+v, want %s", i, imp, want[i])
		}
	}
}

func TestRustUseTreeUnsupportedAndMalformedFailClosed(t *testing.T) {
	for _, src := range []string{
		"use crate::{a::{helper as}};",
		"use crate::{a::{helper, other};",
		"use crate::a::;",
		"use crate::a::helper as;",
		"use crate::a::helper",
		"use crate::{a::{helper as _}};",
		"use *;",
	} {
		t.Run(src, func(t *testing.T) {
			p, err := NewRust().Parse(context.Background(), "lib.rs", []byte(src))
			if err != nil {
				t.Fatal(err)
			}
			if len(p.Scope.Imports) != 0 {
				t.Fatalf("unsupported or malformed import emitted evidence: %+v", p.Scope.Imports)
			}
		})
	}
}

// Java import evidence comes from the declaration's tokens: `static` is a
// keyword token, never a prefix of the package name, and whitespace and
// comments between tokens are not part of the name. A declaration with a
// syntax error keeps its raw spelling, which the store refuses.
func TestJavaImportEvidenceFromSyntax(t *testing.T) {
	for _, c := range []struct {
		decl, source, local string
		static, wildcard    bool
	}{
		{"import staticpkg.Bag;", "staticpkg.Bag", "Bag", false, false},
		{"import staticpkg.*;", "staticpkg", "", false, true},
		{"import static staticpkg.Util.run;", "staticpkg.Util.run", "run", true, false},
		{"import static\ta.B.Bag;", "a.B.Bag", "Bag", true, false},
		{"import static\na.B.*;", "a.B", "", true, true},
		{"import /*c*/ static /*d*/ a . B /*e*/ .*;", "a.B", "", true, true},
		{"import a.B. Box;", "a.B.Box", "Box", false, false},
		{"import a.B /*c*/ .Box;", "a.B.Box", "Box", false, false},
		{"import Top;", "Top", "Top", false, false},
		{"import a.;", "import a.;", "", false, false},
	} {
		p, err := NewJava().Parse(context.Background(), "C.java", []byte("package p;\n"+c.decl+"\nclass C {}\n"))
		if err != nil {
			t.Fatal(err)
		}
		if len(p.Scope.Imports) != 1 {
			t.Fatalf("%s: imports = %+v", c.decl, p.Scope.Imports)
		}
		got := p.Scope.Imports[0]
		if got.SourceSpecifier != c.source || got.LocalName != c.local || got.ImportedName != c.local || got.Static != c.static || got.Wildcard != c.wildcard {
			t.Errorf("%s: import = %+v, want source %q local %q static %v wildcard %v", c.decl, got, c.source, c.local, c.static, c.wildcard)
		}
	}
	// The v9 text rule is reproduced only by the legacy adapter.
	p, err := NewJavaV9().Parse(context.Background(), "C.java", []byte("package p;\nimport staticpkg.Bag;\nclass C {}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Scope.Imports) != 1 || p.Scope.Imports[0].SourceSpecifier != "pkg.Bag" {
		t.Fatalf("v9 imports = %+v, want the legacy pkg.Bag", p.Scope.Imports)
	}
}
