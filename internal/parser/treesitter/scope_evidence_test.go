//go:build cgo

package treesitter

import (
	"context"
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
